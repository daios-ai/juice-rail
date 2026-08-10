package rail

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/daios-ai/juice-rail/go/erc4337"
)

const (
	railABIJSON = `[
      {"type":"function","name":"deposit","inputs":[
        {"name":"id","type":"bytes32"},{"name":"account","type":"address"},{"name":"amount","type":"uint256"}]},
      {"type":"function","name":"settle","inputs":[
        {"name":"id","type":"bytes32"},{"name":"creditor","type":"address"},{"name":"amount","type":"uint256"}]},
      {"type":"function","name":"withdraw","inputs":[
        {"name":"id","type":"bytes32"},{"name":"to","type":"address"},{"name":"amount","type":"uint256"}]},
      {"type":"function","name":"balanceOf","inputs":[{"name":"account","type":"address"}],
        "outputs":[{"type":"uint256"}],"stateMutability":"view"}]`

	erc20ABIJSON = `[
      {"type":"function","name":"approve","inputs":[
        {"name":"spender","type":"address"},{"name":"amount","type":"uint256"}],"outputs":[{"type":"bool"}]},
      {"type":"function","name":"balanceOf","inputs":[{"name":"account","type":"address"}],
        "outputs":[{"type":"uint256"}],"stateMutability":"view"}]`

	multiSendABIJSON = `[
      {"type":"function","name":"multiSend","inputs":[{"name":"transactions","type":"bytes"}],"stateMutability":"payable"}]`

	moduleABIJSON = `[
      {"type":"function","name":"executeUserOp","inputs":[
        {"name":"to","type":"address"},{"name":"value","type":"uint256"},
        {"name":"data","type":"bytes"},{"name":"operation","type":"uint8"}]}]`
)

var (
	railABI      = mustABI(railABIJSON)
	erc20ABI     = mustABI(erc20ABIJSON)
	multiSendABI = mustABI(multiSendABIJSON)
	moduleABI    = mustABI(moduleABIJSON)
)

func mustABI(s string) abi.ABI {
	a, err := abi.JSON(strings.NewReader(s))
	if err != nil {
		panic(fmt.Sprintf("rail: bad ABI: %v", err))
	}
	return a
}

// Gas is the sponsorship policy. The three sponsored shapes have bounded,
// predictable cost, so static limits are exact rather than a guess.
type Gas struct {
	CallGasLimit                  uint64
	VerificationGasLimit          uint64
	PreVerificationGas            uint64
	PaymasterVerificationGasLimit uint64
	PaymasterPostOpGasLimit       uint64
	// DeploymentGas is added to the verification limit when the account is
	// still counterfactual.
	DeploymentGas uint64
	// ValidFor is how long a sponsorship authorisation lives.
	ValidFor time.Duration
}

// DefaultGas is sized for the three shapes with room for account deployment.
var DefaultGas = Gas{
	CallGasLimit:                  400_000,
	VerificationGasLimit:          200_000,
	PreVerificationGas:            100_000,
	PaymasterVerificationGasLimit: 100_000,
	PaymasterPostOpGasLimit:       1,
	DeploymentGas:                 400_000,
	ValidFor:                      5 * time.Minute,
}

// Domain is one JuiceRail deployment: (chain id, contract address), with the
// addresses and policy that surround it. One Rail instance serves one domain,
// so no method takes a network argument and domains cannot be mixed.
type Domain struct {
	Name      string
	ChainID   *big.Int
	Rail      common.Address
	Token     common.Address
	Paymaster common.Address
	Contracts erc4337.Contracts
	// Finality names the mechanism that makes a fact permanent. Only true
	// finality is supported, because a confirmed fact must never revert.
	Finality string
	Gas      Gas
}

// Validate checks that the domain is completely and safely configured.
func (d Domain) Validate() error {
	if d.ChainID == nil || d.ChainID.Sign() <= 0 {
		return fmt.Errorf("domain %q: chain id must be set", d.Name)
	}
	for name, addr := range map[string]common.Address{
		"rail":              d.Rail,
		"token":             d.Token,
		"paymaster":         d.Paymaster,
		"entryPoint":        d.Contracts.EntryPoint,
		"safeSingleton":     d.Contracts.SafeSingleton,
		"safeProxyFactory":  d.Contracts.SafeProxyFactory,
		"safeModule":        d.Contracts.SafeModule,
		"safeModuleSetup":   d.Contracts.SafeModuleSetup,
		"multiSendCallOnly": d.Contracts.MultiSendCallOnly,
	} {
		if addr == (common.Address{}) {
			return fmt.Errorf("domain %q: %s address must be set", d.Name, name)
		}
	}
	if d.Finality != "finalized" {
		return fmt.Errorf("domain %q: finality %q unsupported: only true finality (\"finalized\") is safe here", d.Name, d.Finality)
	}
	if d.Gas.ValidFor <= 0 {
		return fmt.Errorf("domain %q: gas.validFor must be positive", d.Name)
	}
	return nil
}

// Sponsor authorises gas for an operation. Sponsorship is operator
// infrastructure, so a host may replace this with a remote service.
type Sponsor interface {
	Sponsor(ctx context.Context, op *erc4337.UserOperation, validUntil uint64, maxGasCost *big.Int) error
}

// LocalSponsor signs authorisations with the paymaster's verifying key.
type LocalSponsor struct {
	Paymaster  common.Address
	EntryPoint common.Address
	ChainID    *big.Int
	Key        *ecdsa.PrivateKey
}

func (s LocalSponsor) Sponsor(_ context.Context, op *erc4337.UserOperation, validUntil uint64, maxGasCost *big.Int) error {
	return erc4337.SignPaymasterData(op, s.Paymaster, s.EntryPoint, s.ChainID, validUntil, maxGasCost, s.Key)
}

// Chain is everything the rail reads from a node.
type Chain interface {
	ChainReader
	erc4337.ContractCaller
	SuggestGasPrice(ctx context.Context) (*big.Int, error)
	BlockNumber(ctx context.Context) (uint64, error)
}

// Rail moves money on one domain and reports finalized facts. The host stays
// authoritative for its own ledger.
type Rail struct {
	domain  Domain
	store   Store
	chain   Chain
	bundler erc4337.Bundler
	sponsor Sponsor
	key     *ecdsa.PrivateKey

	account erc4337.Account
	address common.Address // resolved lazily, then fixed
}

// New binds a rail to one domain. key owns the rail's Safe account.
func New(d Domain, s Store, chain Chain, b erc4337.Bundler, sponsor Sponsor, key *ecdsa.PrivateKey) (*Rail, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	if s == nil || chain == nil || b == nil || sponsor == nil || key == nil {
		return nil, fmt.Errorf("rail: store, chain, bundler, sponsor and key are required")
	}
	return &Rail{
		domain:  d,
		store:   s,
		chain:   chain,
		bundler: b,
		sponsor: sponsor,
		key:     key,
		account: erc4337.Account{
			Owner:     crypto.PubkeyToAddress(key.PublicKey),
			Contracts: d.Contracts,
		},
	}, nil
}

// Domain returns the domain this rail is bound to.
func (r *Rail) Domain() Domain { return r.domain }

// Account is the rail's own address on this domain: a counterfactual Safe,
// derivable and fundable before it is deployed.
func (r *Rail) Account(ctx context.Context) (common.Address, error) {
	if r.address != (common.Address{}) {
		return r.address, nil
	}
	addr, err := r.account.Address(ctx, r.chain)
	if err != nil {
		return common.Address{}, err
	}
	r.address = addr
	return addr, nil
}

// Balance reads an account's rail balance.
func (r *Rail) Balance(ctx context.Context, account common.Address) (*big.Int, error) {
	return callUint256(ctx, r.chain, r.domain.Rail, railABI, "balanceOf", account)
}

// TokenBalance reads a plain token balance, for funding and verification.
func (r *Rail) TokenBalance(ctx context.Context, account common.Address) (*big.Int, error) {
	return callUint256(ctx, r.chain, r.domain.Token, erc20ABI, "balanceOf", account)
}

func callUint256(ctx context.Context, chain erc4337.ContractCaller, to common.Address, a abi.ABI, method string, args ...any) (*big.Int, error) {
	in, err := a.Pack(method, args...)
	if err != nil {
		return nil, err
	}
	out, err := chain.CallContract(ctx, ethereum.CallMsg{To: &to, Data: in}, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	if len(out) != 32 {
		return nil, fmt.Errorf("%s: want 32 bytes, got %d", method, len(out))
	}
	return new(big.Int).SetBytes(out), nil
}

// --- the two phases ---

// Prepare records the intent. Nothing is signed and nothing is submitted, so
// after this returns the identifier is durably ours and its terms are fixed.
func (r *Rail) Prepare(ctx context.Context, id ID, t Terms) error {
	if err := t.Validate(); err != nil {
		return err
	}
	head, err := r.chain.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("read head: %w", err)
	}
	return r.store.PutIntent(id, Intent{Terms: t, FromBlock: head})
}

// Submit drives a recorded intent towards execution. Calling it repeatedly is
// always safe.
func (r *Rail) Submit(ctx context.Context, id ID) error {
	if err := r.Sign(ctx, id); err != nil {
		return err
	}
	return r.Send(ctx, id)
}

// Sign makes sure a live attempt exists, signing a fresh one only once every
// previous attempt is provably dead. The attempt is durable before it is
// returned, so a crash can never leave an operation we signed but hold no
// record of.
func (r *Rail) Sign(ctx context.Context, id ID) error {
	// Abandonment is checked here as well as in the store, so the refusal is
	// explicit at the boundary rather than incidental to the status.
	abandoned, err := r.store.Abandoned(id)
	if err != nil {
		return err
	}
	if abandoned {
		return fmt.Errorf("%w: %s", ErrAbandoned, id)
	}
	if settled, err := r.settled(ctx, id); err != nil || settled {
		return err
	}
	in, live, err := r.liveAttempt(ctx, id)
	if err != nil || live != nil {
		return err
	}
	signed, err := r.sign(ctx, id, in)
	if err != nil {
		return err
	}
	return r.store.AppendSignedOp(id, signed)
}

// Send presents the newest attempt that may still execute. Re-presenting the
// same signed operation is always safe.
func (r *Rail) Send(ctx context.Context, id ID) error {
	if settled, err := r.settled(ctx, id); err != nil || settled {
		return err
	}
	_, live, err := r.liveAttempt(ctx, id)
	if err != nil {
		return err
	}
	if live == nil {
		return fmt.Errorf("rail: %s has no live attempt to send", id)
	}
	var op erc4337.UserOperation
	if err := json.Unmarshal(live.Op, &op); err != nil {
		return fmt.Errorf("decode stored operation: %w", err)
	}
	if _, err := r.bundler.SendUserOperation(ctx, &op, r.domain.Contracts.EntryPoint); err != nil {
		// A rejection carries no information once the attempt's nonce is
		// consumed: that exact operation can never execute again, so no
		// submission of it could have succeeded either. The test runs after
		// the failure rather than before it, leaving no window in which
		// inclusion races the check.
		if spent, spentErr := r.nonceSpent(ctx, id, live); spentErr == nil && spent {
			return nil
		}
		return fmt.Errorf("submit operation: %w", err)
	}
	return nil
}

// nonceSpent reports whether the attempt's nonce has already been used. The
// read is transient and nothing durable derives from it: signing a replacement
// still requires finalized evidence.
func (r *Rail) nonceSpent(ctx context.Context, id ID, attempt *SignedOp) (bool, error) {
	if attempt.Nonce == nil {
		return false, nil
	}
	addr, err := r.Account(ctx)
	if err != nil {
		return false, err
	}
	nonce, err := r.account.Nonce(ctx, r.chain, addr, nonceKey(id))
	if err != nil {
		return false, err
	}
	return nonce.Cmp(attempt.Nonce) > 0, nil
}

// settled reports whether the intent has reached a terminal status, in which
// case there is nothing left to sign or send.
func (r *Rail) settled(ctx context.Context, id ID) (bool, error) {
	status, err := r.Status(ctx, id)
	if err != nil {
		return false, err
	}
	return status == StatusConfirmed || status == StatusFailed, nil
}

// liveAttempt returns the intent and its newest attempt that may still
// execute, or nil when every attempt is provably dead.
func (r *Rail) liveAttempt(ctx context.Context, id ID) (Intent, *SignedOp, error) {
	in, ok, err := r.store.Intent(id)
	if err != nil {
		return Intent{}, nil, err
	}
	if !ok {
		return Intent{}, nil, ErrNoIntent
	}
	head, err := r.finalizedHeader(ctx)
	if err != nil {
		return in, nil, err
	}
	ops, err := r.store.SignedOps(id)
	if err != nil {
		return in, nil, err
	}
	for i := len(ops) - 1; i >= 0; i-- {
		if !opDead(ops[i], head) {
			return in, &ops[i], nil
		}
	}
	return in, nil, nil
}

// Abandon records that the rail will never sign this identifier again. It does
// not kill attempts already signed, so the host must still wait for `failed`
// before releasing anything.
func (r *Rail) Abandon(_ context.Context, id ID) error {
	return r.store.Abandon(id)
}

func (r *Rail) sign(ctx context.Context, id ID, in Intent) (SignedOp, error) {
	addr, err := r.Account(ctx)
	if err != nil {
		return SignedOp{}, err
	}
	callData, err := in.Terms.CallData(id, r.domain)
	if err != nil {
		return SignedOp{}, err
	}

	// Each intent owns a nonce key, so one stuck attempt never blocks another
	// intent, while attempts on one intent stay strictly ordered.
	nonce, err := r.account.Nonce(ctx, r.chain, addr, nonceKey(id))
	if err != nil {
		return SignedOp{}, err
	}
	gasPrice, err := r.chain.SuggestGasPrice(ctx)
	if err != nil {
		return SignedOp{}, fmt.Errorf("suggest gas price: %w", err)
	}
	deployed, err := r.account.Deployed(ctx, r.chain, addr)
	if err != nil {
		return SignedOp{}, err
	}

	g := r.domain.Gas
	verificationGas := g.VerificationGasLimit
	if !deployed {
		verificationGas += g.DeploymentGas
	}

	op := &erc4337.UserOperation{
		Sender:                        addr,
		Nonce:                         nonce,
		CallData:                      callData,
		CallGasLimit:                  new(big.Int).SetUint64(g.CallGasLimit),
		VerificationGasLimit:          new(big.Int).SetUint64(verificationGas),
		PreVerificationGas:            new(big.Int).SetUint64(g.PreVerificationGas),
		MaxPriorityFeePerGas:          gasPrice,
		MaxFeePerGas:                  new(big.Int).Mul(gasPrice, big.NewInt(2)),
		PaymasterVerificationGasLimit: new(big.Int).SetUint64(g.PaymasterVerificationGasLimit),
		PaymasterPostOpGasLimit:       new(big.Int).SetUint64(g.PaymasterPostOpGasLimit),
	}
	if !deployed {
		factory := r.domain.Contracts.SafeProxyFactory
		data, err := r.account.FactoryData()
		if err != nil {
			return SignedOp{}, err
		}
		op.Factory = &factory
		op.FactoryData = data
	}

	// The sponsorship ceiling is exactly what the EntryPoint will require, so
	// the authorisation can never be short.
	totalGas := new(big.Int).SetUint64(
		verificationGas + g.CallGasLimit + g.PreVerificationGas +
			g.PaymasterVerificationGasLimit + g.PaymasterPostOpGasLimit)
	maxGasCost := new(big.Int).Mul(totalGas, op.MaxFeePerGas)

	// Expiry is measured in chain time, which is what validation compares
	// against, so the death test never depends on this host's clock.
	validUntil, err := r.expiry(ctx)
	if err != nil {
		return SignedOp{}, err
	}

	// Order matters: the paymaster signs the operation without either
	// signature, then the account signs the operation including the
	// paymaster's.
	if err := r.sponsor.Sponsor(ctx, op, validUntil, maxGasCost); err != nil {
		return SignedOp{}, err
	}
	if err := erc4337.SignSafeOp(op, r.domain.Contracts, r.domain.ChainID, 0, validUntil, r.key); err != nil {
		return SignedOp{}, err
	}

	blob, err := json.Marshal(op)
	if err != nil {
		return SignedOp{}, err
	}
	return SignedOp{
		Op:         blob,
		Hash:       op.Hash(r.domain.Contracts.EntryPoint, r.domain.ChainID),
		Nonce:      nonce,
		ValidUntil: validUntil,
	}, nil
}

func (r *Rail) expiry(ctx context.Context) (uint64, error) {
	head, err := r.chain.HeaderByNumber(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("read head: %w", err)
	}
	return head.Time + uint64(r.domain.Gas.ValidFor.Seconds()), nil
}

// nonceKey derives the EntryPoint nonce key from the identifier. It hashes the
// whole identifier, so two intents cannot share a key by agreeing on a prefix,
// and each intent's attempts stay strictly ordered under their own key.
func nonceKey(id ID) *big.Int {
	return new(big.Int).SetBytes(crypto.Keccak256(id[:])[:24])
}

// --- the named operations of the specification ---

// PrepareDeposit records a deposit intent crediting account with amount.
func (r *Rail) PrepareDeposit(ctx context.Context, id ID, account common.Address, amount *big.Int) error {
	return r.Prepare(ctx, id, Terms{Kind: KindDeposit, Account: account, Amount: amount})
}

// SubmitDeposit drives a recorded deposit.
func (r *Rail) SubmitDeposit(ctx context.Context, id ID) error {
	if err := r.expectKind(id, KindDeposit); err != nil {
		return err
	}
	return r.Submit(ctx, id)
}

func (r *Rail) DepositStatus(ctx context.Context, id ID) (Status, error) {
	return r.statusOfKind(ctx, id, KindDeposit)
}

// Settle moves the rail's own balance to a creditor on this domain. The domain
// is part of the agreement: a debt owed here cannot be paid anywhere else.
func (r *Rail) Settle(ctx context.Context, id ID, creditor common.Address, amount *big.Int) error {
	addr, err := r.Account(ctx)
	if err != nil {
		return err
	}
	t := Terms{Kind: KindSettle, Account: addr, Party: creditor, Amount: amount}
	if err := r.Prepare(ctx, id, t); err != nil {
		return err
	}
	return r.Submit(ctx, id)
}

func (r *Rail) SettlementStatus(ctx context.Context, id ID) (Status, error) {
	return r.statusOfKind(ctx, id, KindSettle)
}

// Withdraw sends the rail's own balance out of the contract.
func (r *Rail) Withdraw(ctx context.Context, id ID, to common.Address, amount *big.Int) error {
	addr, err := r.Account(ctx)
	if err != nil {
		return err
	}
	t := Terms{Kind: KindWithdraw, Account: addr, Party: to, Amount: amount}
	if err := r.Prepare(ctx, id, t); err != nil {
		return err
	}
	return r.Submit(ctx, id)
}

func (r *Rail) WithdrawalStatus(ctx context.Context, id ID) (Status, error) {
	return r.statusOfKind(ctx, id, KindWithdraw)
}

func (r *Rail) statusOfKind(ctx context.Context, id ID, k Kind) (Status, error) {
	if err := r.expectKind(id, k); err != nil {
		return StatusUnknown, err
	}
	return r.Status(ctx, id)
}

func (r *Rail) expectKind(id ID, k Kind) error {
	in, ok, err := r.store.Intent(id)
	if err != nil {
		return err
	}
	if !ok {
		return nil // Status reports unknown; other callers get ErrNoIntent.
	}
	if in.Terms.Kind != k {
		return fmt.Errorf("rail: %s is a %s, not a %s", id, in.Terms.Kind, k)
	}
	return nil
}
