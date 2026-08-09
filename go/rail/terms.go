// Package rail is the domain-bound money rail: intents, their durable
// records, and status derived from finalized chain facts.
package rail

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Kind is the operation kind. It is part of the terms hash, so one identifier
// can never mean two different intents.
type Kind uint8

const (
	KindDeposit  Kind = 1
	KindSettle   Kind = 2
	KindWithdraw Kind = 3
)

func (k Kind) String() string {
	switch k {
	case KindDeposit:
		return "deposit"
	case KindSettle:
		return "settle"
	case KindWithdraw:
		return "withdraw"
	default:
		return fmt.Sprintf("kind(%d)", uint8(k))
	}
}

// ID identifies one intent. It is unguessable until submission and is the
// host's idempotency key.
type ID [32]byte

func (id ID) String() string { return "0x" + hex.EncodeToString(id[:]) }

func (id ID) Hash() common.Hash { return common.Hash(id) }

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

// Terms are everything that identifies an intent besides its ID. Account and
// Party carry the parties of each kind:
//
//	deposit   Account is credited;      Party is unused
//	settle    Account is the debtor;    Party is the creditor
//	withdraw  Account is the debtor;    Party receives the tokens
type Terms struct {
	Kind    Kind
	Account common.Address
	Party   common.Address
	Amount  *big.Int
}

var errBadTerms = errors.New("terms: invalid")

// Validate reports whether the terms are well formed.
func (t Terms) Validate() error {
	switch t.Kind {
	case KindDeposit, KindSettle, KindWithdraw:
	default:
		return fmt.Errorf("%w: unknown kind %d", errBadTerms, uint8(t.Kind))
	}
	if t.Amount == nil || t.Amount.Sign() < 0 {
		return fmt.Errorf("%w: amount must be non-negative", errBadTerms)
	}
	if t.Amount.BitLen() > 256 {
		return fmt.Errorf("%w: amount overflows uint256", errBadTerms)
	}
	if t.Account == (common.Address{}) {
		return fmt.Errorf("%w: account must be set", errBadTerms)
	}
	if t.Kind != KindDeposit && t.Party == (common.Address{}) {
		return fmt.Errorf("%w: party must be set", errBadTerms)
	}
	if t.Kind == KindDeposit && t.Party != (common.Address{}) {
		return fmt.Errorf("%w: deposit has no party", errBadTerms)
	}
	return nil
}

// Equal reports whether two terms denote the same intent.
func (t Terms) Equal(o Terms) bool {
	return t.Kind == o.Kind && t.Account == o.Account && t.Party == o.Party &&
		t.Amount != nil && o.Amount != nil && t.Amount.Cmp(o.Amount) == 0
}

func (t Terms) String() string {
	if t.Kind == KindDeposit {
		return fmt.Sprintf("%s %s -> %s", t.Kind, t.Amount, t.Account.Hex())
	}
	return fmt.Sprintf("%s %s %s -> %s", t.Kind, t.Amount, t.Account.Hex(), t.Party.Hex())
}

// Hash is the terms hash the contract binds an identifier to. The preimages
// are fixed by the specification:
//
//	deposit   abi.encode(1, chainid, rail, account, amount)
//	settle    abi.encode(2, chainid, rail, debtor, creditor, amount)
//	withdraw  abi.encode(3, chainid, rail, account, to, amount)
func (t Terms) Hash(chainID *big.Int, railAddr common.Address) common.Hash {
	words := [][]byte{
		word(big.NewInt(int64(t.Kind))),
		word(chainID),
		word(new(big.Int).SetBytes(railAddr.Bytes())),
		word(new(big.Int).SetBytes(t.Account.Bytes())),
	}
	if t.Kind != KindDeposit {
		words = append(words, word(new(big.Int).SetBytes(t.Party.Bytes())))
	}
	words = append(words, word(t.Amount))

	var buf []byte
	for _, w := range words {
		buf = append(buf, w...)
	}
	return crypto.Keccak256Hash(buf)
}

// word left-pads a non-negative integer into one 32-byte ABI word.
func word(v *big.Int) []byte {
	var w [32]byte
	if v != nil {
		v.FillBytes(w[:])
	}
	return w[:]
}

// CallData builds the account call for this intent. There are exactly three
// shapes, and the paymaster sponsors exactly these: a plain call to the rail
// for settle and withdraw, and the approve+deposit pair delegatecalled into
// MultiSendCallOnly so the approval originates from the account.
func (t Terms) CallData(id ID, d Domain) ([]byte, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	switch t.Kind {
	case KindSettle:
		inner, err := railABI.Pack("settle", id, t.Party, t.Amount)
		if err != nil {
			return nil, err
		}
		return execute(d.Rail, inner, 0)

	case KindWithdraw:
		inner, err := railABI.Pack("withdraw", id, t.Party, t.Amount)
		if err != nil {
			return nil, err
		}
		return execute(d.Rail, inner, 0)

	case KindDeposit:
		approve, err := erc20ABI.Pack("approve", d.Rail, t.Amount)
		if err != nil {
			return nil, err
		}
		deposit, err := railABI.Pack("deposit", id, t.Account, t.Amount)
		if err != nil {
			return nil, err
		}
		batch, err := multiSendABI.Pack("multiSend", concat(
			subCall(d.Token, approve),
			subCall(d.Rail, deposit),
		))
		if err != nil {
			return nil, err
		}
		return execute(d.Contracts.MultiSendCallOnly, batch, 1)

	default:
		return nil, fmt.Errorf("%w: unknown kind", errBadTerms)
	}
}

func execute(to common.Address, data []byte, operation uint8) ([]byte, error) {
	return moduleABI.Pack("executeUserOp", to, big.NewInt(0), data, operation)
}

// subCall is one MultiSend record: plain call, no value.
func subCall(to common.Address, data []byte) []byte {
	out := make([]byte, 0, 85+len(data))
	out = append(out, 0) // operation: call
	out = append(out, to.Bytes()...)
	out = append(out, word(big.NewInt(0))...)
	out = append(out, word(big.NewInt(int64(len(data))))...)
	return append(out, data...)
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
