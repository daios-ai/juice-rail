package rail

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// memStore is the in-memory Store the other tests run against. It is also the
// second implementation of the interface, which keeps the contract honest.
type memStore struct {
	intents   map[Ref]Intent
	variants  map[Ref][]Variant
	abandoned map[Ref]bool
	facts     map[Ref]Fact
	order     []Ref
}

func newMemStore() *memStore {
	return &memStore{
		intents:   map[Ref]Intent{},
		variants:  map[Ref][]Variant{},
		abandoned: map[Ref]bool{},
		facts:     map[Ref]Fact{},
	}
}

func (m *memStore) PutIntent(ref Ref, in Intent) error {
	if existing, ok := m.intents[ref]; ok {
		if !existing.Terms.Equal(in.Terms) {
			return ErrIntentConflict
		}
		return nil
	}
	m.intents[ref] = in
	m.order = append(m.order, ref)
	return nil
}

func (m *memStore) Intent(ref Ref) (Intent, bool, error) {
	in, ok := m.intents[ref]
	return in, ok, nil
}

func (m *memStore) AppendVariant(ref Ref, v Variant) error {
	if m.abandoned[ref] {
		return ErrAbandoned
	}
	if _, err := MarshalVariant(v); err != nil {
		return err
	}
	m.variants[ref] = append(m.variants[ref], v)
	return nil
}

func (m *memStore) Variants(ref Ref) ([]Variant, error) { return m.variants[ref], nil }

func (m *memStore) Abandon(ref Ref) error { m.abandoned[ref] = true; return nil }

func (m *memStore) Abandoned(ref Ref) (bool, error) { return m.abandoned[ref], nil }

func (m *memStore) PutFact(ref Ref, f Fact) error {
	if _, ok := m.facts[ref]; ok {
		return nil
	}
	m.facts[ref] = f
	return nil
}

func (m *memStore) Fact(ref Ref) (Fact, bool, error) {
	f, ok := m.facts[ref]
	return f, ok, nil
}

func (m *memStore) Pending() ([]Ref, error) {
	var out []Ref
	for _, ref := range m.order {
		if _, done := m.facts[ref]; !done {
			out = append(out, ref)
		}
	}
	return out, nil
}

func (m *memStore) Close() error { return nil }

// --- helpers shared by the package's tests ---

func testID(n byte) ID {
	var id ID
	id[31] = n
	return id
}

func addr(n byte) common.Address {
	var a common.Address
	a[19] = n
	return a
}

func testTerms(kind Kind, account, party common.Address, amount int64) Terms {
	return Terms{Kind: kind, Account: account, Party: party, Amount: big.NewInt(amount)}
}

func testVariant(kind Kind, account, party common.Address, amount, fee int64, relayer common.Address, validBefore uint64) Variant {
	v := Variant{
		Terms:       testTerms(kind, account, party, amount),
		ID:          testID(1),
		Fee:         big.NewInt(fee),
		Relayer:     relayer,
		ValidBefore: validBefore,
		TermsSig:    make([]byte, 65),
	}
	if kind == KindDeposit {
		v.AuthSig = make([]byte, 65)
	}
	return v
}

// --- the interface contract ---

func TestStoreRecordsAnIntentOnce(t *testing.T) {
	s := newMemStore()
	ref := Ref{Account: addr(1), ID: testID(1)}
	in := Intent{Terms: testTerms(KindTransfer, addr(1), addr(2), 10), FromBlock: 7}

	if err := s.PutIntent(ref, in); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIntent(ref, in); err != nil {
		t.Fatalf("re-recording identical terms must succeed: %v", err)
	}
	other := in
	other.Terms.Amount = big.NewInt(11)
	if err := s.PutIntent(ref, other); !errors.Is(err, ErrIntentConflict) {
		t.Fatalf("want ErrIntentConflict, got %v", err)
	}

	got, ok, err := s.Intent(ref)
	if err != nil || !ok {
		t.Fatalf("intent missing: %v %v", ok, err)
	}
	if !got.Terms.Equal(in.Terms) || got.FromBlock != 7 {
		t.Fatalf("stored %+v, want %+v", got, in)
	}
}

// Identifiers are scoped to their account, on chain and here.
func TestStoreSeparatesAccountsUnderOneIdentifier(t *testing.T) {
	s := newMemStore()
	id := testID(1)
	alice := Ref{Account: addr(1), ID: id}
	bob := Ref{Account: addr(2), ID: id}

	if err := s.PutIntent(alice, Intent{Terms: testTerms(KindTransfer, addr(1), addr(9), 10)}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIntent(bob, Intent{Terms: testTerms(KindWithdraw, addr(2), addr(9), 99)}); err != nil {
		t.Fatalf("another account's identifier must not conflict: %v", err)
	}
	got, _, _ := s.Intent(bob)
	if got.Terms.Amount.Int64() != 99 {
		t.Fatalf("accounts alias: got %v", got.Terms)
	}
	if err := s.Abandon(alice); err != nil {
		t.Fatal(err)
	}
	if abandoned, _ := s.Abandoned(bob); abandoned {
		t.Fatal("abandoning one account's intent abandoned another's")
	}
}

func TestStoreAppendsVariantsUntilAbandoned(t *testing.T) {
	s := newMemStore()
	ref := Ref{Account: addr(1), ID: testID(1)}
	v := testVariant(KindTransfer, addr(1), addr(2), 10, 1, addr(3), 100)

	if err := s.AppendVariant(ref, v); err != nil {
		t.Fatal(err)
	}
	v.ValidBefore = 200
	if err := s.AppendVariant(ref, v); err != nil {
		t.Fatal(err)
	}
	got, err := s.Variants(ref)
	if err != nil || len(got) != 2 {
		t.Fatalf("want 2 variants, got %d (%v)", len(got), err)
	}
	if got[0].ValidBefore != 100 || got[1].ValidBefore != 200 {
		t.Fatalf("variants out of order: %v", got)
	}

	if err := s.Abandon(ref); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendVariant(ref, v); !errors.Is(err, ErrAbandoned) {
		t.Fatalf("want ErrAbandoned, got %v", err)
	}
	// Abandonment kills nothing already signed.
	if got, _ := s.Variants(ref); len(got) != 2 {
		t.Fatalf("abandonment dropped variants: %d", len(got))
	}
}

func TestStoreCachesFinalizedFactsOnce(t *testing.T) {
	s := newMemStore()
	ref := Ref{Account: addr(1), ID: testID(1)}
	if err := s.PutFact(ref, Fact{BlockNumber: 5, Executed: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutFact(ref, Fact{BlockNumber: 6, Executed: false}); err != nil {
		t.Fatal(err)
	}
	f, ok, err := s.Fact(ref)
	if err != nil || !ok {
		t.Fatal("fact missing")
	}
	if f.BlockNumber != 5 || !f.Executed {
		t.Fatalf("a finalized fact changed: %+v", f)
	}
}

func TestStorePendingDropsSettledIntents(t *testing.T) {
	s := newMemStore()
	a := Ref{Account: addr(1), ID: testID(1)}
	b := Ref{Account: addr(1), ID: testID(2)}
	for _, ref := range []Ref{a, b} {
		if err := s.PutIntent(ref, Intent{Terms: testTerms(KindTransfer, addr(1), addr(2), 1)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutFact(a, Fact{Executed: true}); err != nil {
		t.Fatal(err)
	}
	pending, err := s.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0] != b {
		t.Fatalf("pending %v, want just %v", pending, b)
	}
}

func TestDomainKeyDistinguishesDeployments(t *testing.T) {
	one := DomainKey(big.NewInt(42161), addr(1))
	two := DomainKey(big.NewInt(421614), addr(1))
	three := DomainKey(big.NewInt(42161), addr(2))
	if one == two || one == three {
		t.Fatalf("domains collide: %s %s %s", one, two, three)
	}
	if DomainKey(big.NewInt(1), common.HexToAddress("0xAbC0000000000000000000000000000000000000")) !=
		DomainKey(big.NewInt(1), common.HexToAddress("0xabc0000000000000000000000000000000000000")) {
		t.Fatal("case changes the domain key")
	}
}
