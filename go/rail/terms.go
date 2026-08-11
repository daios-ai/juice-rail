// Package rail is the domain-bound money rail: intents, the signed variants
// that may carry them, and status derived from finalized chain facts.
package rail

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// Kind is the operation kind. It is part of the terms hash, so one identifier
// can never mean two different intents.
type Kind uint8

const (
	KindDeposit  Kind = 1
	KindTransfer Kind = 2
	KindWithdraw Kind = 3
)

func (k Kind) String() string {
	switch k {
	case KindDeposit:
		return "deposit"
	case KindTransfer:
		return "transfer"
	case KindWithdraw:
		return "withdraw"
	default:
		return fmt.Sprintf("kind(%d)", uint8(k))
	}
}

// ParseKind reads a kind by name.
func ParseKind(s string) (Kind, error) {
	switch s {
	case "deposit":
		return KindDeposit, nil
	case "transfer":
		return KindTransfer, nil
	case "withdraw":
		return KindWithdraw, nil
	default:
		return 0, fmt.Errorf("%w: unknown kind %q", errBadTerms, s)
	}
}

// ID identifies one intent of one account. It is the host's idempotency key.
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

// Ref identifies one intent: the authorising account and its identifier.
// Identifiers are scoped to the account that binds them, on chain and here, so
// no account can burn another's.
type Ref struct {
	Account common.Address
	ID      ID
}

func (r Ref) String() string { return r.Account.Hex() + "/" + r.ID.String() }

// Terms are the core of an intent: what money moves, and between whom. They
// are fixed by the write-ahead record and shared by every variant of the
// intent, which differ only in relayer, fee and deadline.
//
//	deposit   Account pays;    Party is credited
//	transfer  Account pays;    Party is credited
//	withdraw  Account pays;    Party receives the tokens outside the rail
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
	case KindDeposit, KindTransfer, KindWithdraw:
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
	if t.Party == (common.Address{}) {
		return fmt.Errorf("%w: party must be set", errBadTerms)
	}
	return nil
}

// Equal reports whether two terms denote the same intent.
func (t Terms) Equal(o Terms) bool {
	return t.Kind == o.Kind && t.Account == o.Account && t.Party == o.Party &&
		t.Amount != nil && o.Amount != nil && t.Amount.Cmp(o.Amount) == 0
}

func (t Terms) String() string {
	return fmt.Sprintf("%s %s %s -> %s", t.Kind, t.Amount, t.Account.Hex(), t.Party.Hex())
}

// Variant is one signed way to carry an intent: its core terms plus the
// relayer that may submit it, the fee that relayer earns, and the deadline
// past which it can never execute.
//
// An intent may have several variants. The contract executes at most one, so
// replacing an unresponsive relayer needs no waiting: sign another.
type Variant struct {
	Terms
	ID          ID
	Fee         *big.Int
	Relayer     common.Address
	ValidBefore uint64
	// TermsSig is the account holder's EIP-712 signature over the terms.
	TermsSig []byte
	// AuthSig is the payer's EIP-3009 authorisation, deposits only.
	AuthSig []byte
}

// Ref is the intent this variant carries.
func (v Variant) Ref() Ref { return Ref{Account: v.Account, ID: v.ID} }

// Total is what the account pays: the amount plus the relay fee.
func (v Variant) Total() *big.Int { return new(big.Int).Add(v.Amount, v.Fee) }

// Validate reports whether the variant is well formed and signed.
func (v Variant) Validate() error {
	if err := v.Terms.Validate(); err != nil {
		return err
	}
	if v.Fee == nil || v.Fee.Sign() < 0 {
		return fmt.Errorf("%w: fee must be non-negative", errBadTerms)
	}
	if v.Relayer == (common.Address{}) {
		return fmt.Errorf("%w: relayer must be set", errBadTerms)
	}
	if v.ValidBefore == 0 {
		return fmt.Errorf("%w: deadline must be set", errBadTerms)
	}
	if len(v.TermsSig) != 65 {
		return fmt.Errorf("%w: terms signature is %d bytes, want 65", errBadTerms, len(v.TermsSig))
	}
	if v.Kind == KindDeposit && len(v.AuthSig) != 65 {
		return fmt.Errorf("%w: deposit authorisation is %d bytes, want 65", errBadTerms, len(v.AuthSig))
	}
	if v.Kind != KindDeposit && len(v.AuthSig) != 0 {
		return fmt.Errorf("%w: only a deposit carries a token authorisation", errBadTerms)
	}
	return nil
}

// Envelope is the wire form of a signed variant: the variant and the domain it
// was signed for. The domain is already inside the signature, so this only
// lets a relayer refuse the wrong domain by name instead of reporting an
// unexplained bad signature.
type Envelope struct {
	ChainID *big.Int
	Rail    common.Address
	Variant Variant
}

// wireVariant is the canonical form of a signed variant: the same bytes go to
// a relayer and into the durable record, so there is one format to be right
// about.
type wireVariant struct {
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	Account     string `json:"account"`
	Party       string `json:"party"`
	Amount      string `json:"amount"`
	Fee         string `json:"fee"`
	Relayer     string `json:"relayer"`
	ValidBefore uint64 `json:"validBefore"`
	TermsSig    string `json:"termsSig"`
	AuthSig     string `json:"authSig,omitempty"`
}

type wireEnvelope struct {
	ChainID string `json:"chainId"`
	Rail    string `json:"rail"`
	wireVariant
}

func toWire(v Variant) wireVariant {
	w := wireVariant{
		Kind:        v.Kind.String(),
		ID:          v.ID.String(),
		Account:     v.Account.Hex(),
		Party:       v.Party.Hex(),
		Amount:      v.Amount.String(),
		Fee:         v.Fee.String(),
		Relayer:     v.Relayer.Hex(),
		ValidBefore: v.ValidBefore,
		TermsSig:    hexutilBytes(v.TermsSig),
	}
	if len(v.AuthSig) != 0 {
		w.AuthSig = hexutilBytes(v.AuthSig)
	}
	return w
}

// MarshalVariant writes the durable record of a signed variant. A Store keeps
// these bytes opaque.
func MarshalVariant(v Variant) ([]byte, error) {
	if err := v.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(toWire(v))
}

// UnmarshalVariant reads back a durable record.
func UnmarshalVariant(raw []byte) (Variant, error) {
	var w wireVariant
	if err := json.Unmarshal(raw, &w); err != nil {
		return Variant{}, fmt.Errorf("decode variant: %w", err)
	}
	return fromWire(w)
}

// EncodeVariant writes a signed variant for a relayer to carry.
func EncodeVariant(d Domain, v Variant) ([]byte, error) {
	if err := v.Validate(); err != nil {
		return nil, err
	}
	return json.MarshalIndent(wireEnvelope{
		ChainID:     d.ChainID.String(),
		Rail:        d.Rail.Hex(),
		wireVariant: toWire(v),
	}, "", "  ")
}

// DecodeVariant reads a signed variant. Nothing is trusted: the caller must
// still check the domain and recover the signature.
func DecodeVariant(raw []byte) (Envelope, error) {
	var w wireEnvelope
	if err := json.Unmarshal(raw, &w); err != nil {
		return Envelope{}, fmt.Errorf("decode variant: %w", err)
	}
	chainID, ok := new(big.Int).SetString(w.ChainID, 10)
	if !ok {
		return Envelope{}, fmt.Errorf("decode variant: bad chain id %q", w.ChainID)
	}
	v, err := fromWire(w.wireVariant)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{ChainID: chainID, Rail: common.HexToAddress(w.Rail), Variant: v}, nil
}

func fromWire(w wireVariant) (Variant, error) {
	kind, err := ParseKind(w.Kind)
	if err != nil {
		return Variant{}, err
	}
	id, err := ParseID(w.ID)
	if err != nil {
		return Variant{}, err
	}
	amount, ok := new(big.Int).SetString(w.Amount, 10)
	if !ok {
		return Variant{}, fmt.Errorf("decode variant: bad amount %q", w.Amount)
	}
	fee, ok := new(big.Int).SetString(w.Fee, 10)
	if !ok {
		return Variant{}, fmt.Errorf("decode variant: bad fee %q", w.Fee)
	}
	termsSig, err := parseHexBytes(w.TermsSig)
	if err != nil {
		return Variant{}, err
	}
	authSig, err := parseHexBytes(w.AuthSig)
	if err != nil {
		return Variant{}, err
	}
	v := Variant{
		Terms: Terms{
			Kind:    kind,
			Account: common.HexToAddress(w.Account),
			Party:   common.HexToAddress(w.Party),
			Amount:  amount,
		},
		ID:          id,
		Fee:         fee,
		Relayer:     common.HexToAddress(w.Relayer),
		ValidBefore: w.ValidBefore,
		TermsSig:    termsSig,
		AuthSig:     authSig,
	}
	if err := v.Validate(); err != nil {
		return Variant{}, err
	}
	return v, nil
}

func hexutilBytes(b []byte) string { return "0x" + hex.EncodeToString(b) }

func parseHexBytes(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		return nil, fmt.Errorf("decode variant: bad signature: %w", err)
	}
	return b, nil
}
