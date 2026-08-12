// Package rail is the money rail: an ordinary account that holds a stablecoin
// as money and the chain's native currency as an operating reserve, pays with
// plain token transfers, keeps its own reserve topped up, and reports
// finalized facts.
//
// There is no rail contract, no relayer and no operator. The token's own
// ledger is the ledger; the account's own transaction nonce serialises its
// operations.
package rail

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// Kind is what an operation is for. Transfer and withdraw are the same token
// transfer and differ only in where the money goes; refill is maintenance.
type Kind uint8

const (
	KindTransfer Kind = 1
	KindWithdraw Kind = 2
	KindRefill   Kind = 3
)

func (k Kind) String() string {
	switch k {
	case KindTransfer:
		return "transfer"
	case KindWithdraw:
		return "withdraw"
	case KindRefill:
		return "refill"
	default:
		return fmt.Sprintf("kind(%d)", uint8(k))
	}
}

// ParseKind reads a kind by name.
func ParseKind(s string) (Kind, error) {
	switch s {
	case "transfer":
		return KindTransfer, nil
	case "withdraw":
		return KindWithdraw, nil
	case "refill":
		return KindRefill, nil
	default:
		return 0, fmt.Errorf("%w: unknown kind %q", ErrBadInput, s)
	}
}

// ID identifies one intent of one account: the host's idempotency key.
type ID [32]byte

func (id ID) String() string { return "0x" + hex.EncodeToString(id[:]) }

// ParseID reads a 32-byte hex identifier.
func ParseID(s string) (ID, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		return ID{}, fmt.Errorf("id: %w", err)
	}
	if len(b) != 32 {
		return ID{}, fmt.Errorf("id: want 32 bytes, got %d", len(b))
	}
	return ID(b), nil
}

// Intent is the write-ahead record: the decision to send one transaction,
// durable before any signature exists. It binds the operation to exactly one
// account nonce, which is what makes the chain enforce at-most-once: the chain
// admits one transaction per nonce.
//
// Calldata is stored rather than rebuilt, so a retry after a crash reproduces
// the same transaction byte for byte, and reconciliation can recognise the
// mined one.
type Intent struct {
	ID   ID
	Kind Kind
	// To is the payee for a transfer or withdrawal, and the swap venue for a
	// refill.
	To common.Address
	// Amount is the token amount paid; for a refill it is the maximum input the
	// swap may consume.
	Amount *big.Int
	// Delta is the exact amount of native currency a refill buys. Zero
	// otherwise.
	Delta *big.Int
	// Nonce is the account nonce this intent owns.
	Nonce uint64
	// Calldata is the exact transaction input.
	Calldata []byte
	// FromBlock is the chain head when the intent was recorded, and bounds any
	// search for its outcome.
	FromBlock uint64
}

// Validate reports whether the intent is well formed.
func (in Intent) Validate() error {
	switch in.Kind {
	case KindTransfer, KindWithdraw, KindRefill:
	default:
		return fmt.Errorf("%w: unknown kind %d", ErrBadInput, uint8(in.Kind))
	}
	if in.To == (common.Address{}) {
		return fmt.Errorf("%w: destination must be set", ErrBadInput)
	}
	if in.Amount == nil || in.Amount.Sign() <= 0 {
		return fmt.Errorf("%w: amount must be positive", ErrBadInput)
	}
	if in.Amount.BitLen() > 256 {
		return fmt.Errorf("%w: amount overflows uint256", ErrBadInput)
	}
	if in.Kind == KindRefill && (in.Delta == nil || in.Delta.Sign() <= 0) {
		return fmt.Errorf("%w: a refill must buy a positive amount", ErrBadInput)
	}
	if len(in.Calldata) == 0 {
		return fmt.Errorf("%w: calldata must be set", ErrBadInput)
	}
	return nil
}

// SameTerms reports whether two records describe the same operation. Only the
// economic terms count: a retry may change transaction fees and nothing else.
func (in Intent) SameTerms(o Intent) bool {
	return in.ID == o.ID && in.Kind == o.Kind && in.To == o.To && in.Nonce == o.Nonce &&
		in.Amount != nil && o.Amount != nil && in.Amount.Cmp(o.Amount) == 0
}

// Submission is one signed attempt at an intent. Attempts differ only in
// transaction fees, which is Ethereum's own replacement rule.
type Submission struct {
	TxHash common.Hash
	Gas    uint64
	Tip    *big.Int
	FeeCap *big.Int
}

// Fact records a finalized outcome. Finalized facts cannot change, so this is
// a cache rather than authority.
type Fact struct {
	TxHash      common.Hash
	BlockNumber uint64
	// Executed is false when the intent's nonce was finalized without the
	// intent executing: a reverted transaction, or one this account never
	// recorded.
	Executed bool
}

// Deposit is one finalized incoming token transfer. Its identity is the log
// that carried it, so one transfer is recognised exactly once.
type Deposit struct {
	TxHash      common.Hash
	LogIndex    uint
	From        common.Address
	Amount      *big.Int
	BlockNumber uint64
}

// Venue is where an account buys native currency with its own stablecoin. It
// is configuration: the library pins no address of its own.
type Venue struct {
	// Router and Quoter are a Uniswap V3 SwapRouter and QuoterV2.
	Router, Quoter common.Address
	// WETH is the wrapped native currency the pool trades.
	WETH common.Address
	// FeeTier is the pool's fee in hundredths of a basis point (500 = 0.05%).
	FeeTier uint32
	// Router02 selects the SwapRouter02 encoding, which carries the deadline
	// in multicall instead of in the swap parameters.
	Router02 bool
}

// Gas limits are configuration rather than estimates, because estimating
// requires the account to already hold the gas the policy is deciding whether
// to buy. Unused gas is not charged, so the only cost of a generous bound is a
// more conservative reserve. Chains that charge for data — rollups posting to
// a parent chain — need larger bounds than these.
const (
	DefaultPaymentGas = 200_000
	DefaultSwapGas    = 600_000
)

// GasPolicy is the operating reserve: keep the native balance between Min and
// Max, buying more with the account's own stablecoin when a payment would take
// it below Min.
type GasPolicy struct {
	Min, Max *big.Int
	// SlippageBps bounds how much more than the quote a refill may spend, in
	// basis points. Integer, because there is no floating point here.
	SlippageBps uint32
	// FeeBound is the most a refill transaction may cost. Above it the account
	// waits: no constant reserve survives every fee level.
	FeeBound *big.Int
	// PaymentGas and SwapGas bound a payment and a refill. Zero means the
	// defaults above.
	PaymentGas, SwapGas uint64
}

// limits returns the gas bounds with defaults applied.
func (p GasPolicy) limits() (payment, swap uint64) {
	payment, swap = p.PaymentGas, p.SwapGas
	if payment == 0 {
		payment = DefaultPaymentGas
	}
	if swap == 0 {
		swap = DefaultSwapGas
	}
	return payment, swap
}

// Validate checks the policy is orderly. Min must cover a refill, or the
// reserve could fall to a level from which it cannot replenish itself.
func (p GasPolicy) Validate() error {
	if p.Min == nil || p.Min.Sign() <= 0 {
		return fmt.Errorf("%w: gas policy needs a positive minimum", ErrBadInput)
	}
	if p.Max == nil || p.Max.Cmp(p.Min) <= 0 {
		return fmt.Errorf("%w: gas policy maximum must exceed its minimum", ErrBadInput)
	}
	if p.FeeBound == nil || p.FeeBound.Sign() <= 0 {
		return fmt.Errorf("%w: gas policy needs a positive fee bound", ErrBadInput)
	}
	if p.Min.Cmp(p.FeeBound) < 0 {
		return fmt.Errorf("%w: gas policy minimum %s is below its fee bound %s: the reserve could not pay for its own refill",
			ErrBadInput, p.Min, p.FeeBound)
	}
	if p.SlippageBps > 10_000 {
		return fmt.Errorf("%w: slippage of %d basis points is more than the whole price", ErrBadInput, p.SlippageBps)
	}
	return nil
}

// Domain is one settlement environment: a chain and the stablecoin accounted
// on it, plus how to reach it and how the account keeps itself in gas. One
// Rail serves one domain, so no method takes a network argument and domains
// cannot be mixed.
type Domain struct {
	Name    string
	ChainID *big.Int
	Token   common.Address
	// Finality names the mechanism that makes a fact permanent. Only true
	// finality is supported, because a confirmed fact must never revert.
	Finality string
	// FromBlock is where deposit observation starts: the block before which
	// this account had no history.
	FromBlock uint64
	Venue     Venue
	Gas       GasPolicy
}

// Validate checks that the domain is completely and safely configured.
func (d Domain) Validate() error {
	if d.ChainID == nil || d.ChainID.Sign() <= 0 {
		return fmt.Errorf("%w: domain %q: chain id must be set", ErrBadInput, d.Name)
	}
	if d.Token == (common.Address{}) {
		return fmt.Errorf("%w: domain %q: token address must be set", ErrBadInput, d.Name)
	}
	if d.Finality != "finalized" {
		return fmt.Errorf("%w: domain %q: finality %q unsupported: only true finality (\"finalized\") is safe here",
			ErrBadInput, d.Name, d.Finality)
	}
	if d.Venue.Router == (common.Address{}) || d.Venue.Quoter == (common.Address{}) || d.Venue.WETH == (common.Address{}) {
		return fmt.Errorf("%w: domain %q: the swap venue needs a router, a quoter and a wrapped native token", ErrBadInput, d.Name)
	}
	if d.Venue.FeeTier == 0 {
		return fmt.Errorf("%w: domain %q: the swap venue needs a pool fee tier", ErrBadInput, d.Name)
	}
	return d.Gas.Validate()
}

// DomainKey names the domain a store serves: the chain and the token accounted
// on it. Identifiers are unique only within one of these.
func DomainKey(chainID *big.Int, token common.Address) string {
	chain := "0"
	if chainID != nil {
		chain = chainID.String()
	}
	return chain + ":" + strings.ToLower(token.Hex())
}

var (
	// ErrBadInput is returned for malformed arguments or configuration.
	ErrBadInput = errors.New("rail: invalid input")
	// ErrIntentConflict is returned when an identifier is already recorded with
	// different terms.
	ErrIntentConflict = errors.New("rail: identifier already recorded with different terms")
	// ErrNoIntent is returned when an operation is referenced before its
	// write-ahead record exists.
	ErrNoIntent = errors.New("rail: no intent recorded")
	// ErrWrongDomain is returned when a store bound to one domain is opened for
	// another.
	ErrWrongDomain = errors.New("rail: store belongs to a different domain")
	// ErrInFlight is returned when an operation is still unresolved. One
	// outgoing transaction at a time is what keeps two operations from
	// projecting the same reserve twice.
	ErrInFlight = errors.New("rail: an operation is still in flight")
	// ErrNeedRefill is returned when the reserve is too low for the payment.
	// Nothing was signed: refill, wait for finality, then ask again.
	ErrNeedRefill = errors.New("rail: reserve too low, refill first")
	// ErrNoRefillNeeded is returned when a refill is asked for and the reserve
	// is already sufficient.
	ErrNoRefillNeeded = errors.New("rail: reserve is sufficient")
	// ErrInsufficientToken is returned when the account cannot afford the
	// payment, or the refill that would let it make the payment.
	ErrInsufficientToken = errors.New("rail: not enough stablecoin")
	// ErrInsufficientGas is returned when the reserve cannot even pay for its
	// own refill. Recovery is an external top-up.
	ErrInsufficientGas = errors.New("rail: not enough native currency to refill; top up externally")
	// ErrFeesAboveBound is returned when a refill would cost more than the
	// configured bound. The account waits for cheaper gas.
	ErrFeesAboveBound = errors.New("rail: gas costs more than the configured bound")
	// ErrUnreconciled is returned when a finalized account nonce cannot be
	// matched to a recorded intent: something else signed with this key.
	ErrUnreconciled = errors.New("rail: a finalized nonce does not match any recorded intent; one signer per key")
)
