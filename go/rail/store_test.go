package rail

import (
	"errors"
	"math/big"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// memStore is an in-memory Store used by the rail tests. It enforces the same
// write-once, append-only, and append-after-abandon rules as the shipped one.
type memStore struct {
	mu        sync.Mutex
	intents   map[ID]Intent
	ops       map[ID][]SignedOp
	abandoned map[ID]bool
	facts     map[ID]Fact
	order     []ID
}

func newMemStore() *memStore {
	return &memStore{
		intents:   map[ID]Intent{},
		ops:       map[ID][]SignedOp{},
		abandoned: map[ID]bool{},
		facts:     map[ID]Fact{},
	}
}

func (m *memStore) PutIntent(id ID, in Intent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.intents[id]; ok {
		if !existing.Terms.Equal(in.Terms) {
			return ErrIntentConflict
		}
		return nil
	}
	m.intents[id] = in
	m.order = append(m.order, id)
	return nil
}

func (m *memStore) Intent(id ID) (Intent, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in, ok := m.intents[id]
	return in, ok, nil
}

func (m *memStore) AppendSignedOp(id ID, op SignedOp) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.abandoned[id] {
		return ErrAbandoned
	}
	m.ops[id] = append(m.ops[id], op)
	return nil
}

func (m *memStore) SignedOps(id ID) ([]SignedOp, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]SignedOp(nil), m.ops[id]...), nil
}

func (m *memStore) Abandon(id ID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.abandoned[id] = true
	return nil
}

func (m *memStore) Abandoned(id ID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.abandoned[id], nil
}

func (m *memStore) PutFact(id ID, f Fact) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.facts[id]; ok {
		return nil // finalized facts cannot change
	}
	m.facts[id] = f
	return nil
}

func (m *memStore) Fact(id ID) (Fact, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.facts[id]
	return f, ok, nil
}

func (m *memStore) PendingIDs() ([]ID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ID
	for _, id := range m.order {
		if _, done := m.facts[id]; !done {
			out = append(out, id)
		}
	}
	return out, nil
}

func (m *memStore) Close() error { return nil }

// --- the Store contract ---

func testID(n byte) ID {
	var id ID
	id[31] = n
	return id
}

func depositTerms(amount int64) Terms {
	return Terms{Kind: KindDeposit, Account: common.HexToAddress("0xaaaa"), Amount: big.NewInt(amount)}
}

func TestStoreIntentIsWriteOnce(t *testing.T) {
	s := newMemStore()
	id := testID(1)

	if err := s.PutIntent(id, Intent{Terms: depositTerms(100), FromBlock: 7}); err != nil {
		t.Fatalf("first record: %v", err)
	}
	if err := s.PutIntent(id, Intent{Terms: depositTerms(100), FromBlock: 9}); err != nil {
		t.Fatalf("identical terms must be accepted: %v", err)
	}
	if err := s.PutIntent(id, Intent{Terms: depositTerms(101)}); !errors.Is(err, ErrIntentConflict) {
		t.Fatalf("different terms: want ErrIntentConflict, got %v", err)
	}

	got, ok, _ := s.Intent(id)
	if !ok || got.Terms.Amount.Int64() != 100 || got.FromBlock != 7 {
		t.Fatalf("original record must survive, got %+v", got)
	}
}

func TestStoreAppendRefusedAfterAbandon(t *testing.T) {
	s := newMemStore()
	id := testID(2)
	if err := s.PutIntent(id, Intent{Terms: depositTerms(1)}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendSignedOp(id, SignedOp{ValidUntil: 10}); err != nil {
		t.Fatal(err)
	}
	if err := s.Abandon(id); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendSignedOp(id, SignedOp{ValidUntil: 20}); !errors.Is(err, ErrAbandoned) {
		t.Fatalf("signing after abandonment: want ErrAbandoned, got %v", err)
	}

	ops, _ := s.SignedOps(id)
	if len(ops) != 1 {
		t.Fatalf("the earlier attempt must survive, got %d", len(ops))
	}
}

func TestStoreAppendsInOrder(t *testing.T) {
	s := newMemStore()
	id := testID(3)
	for i := uint64(1); i <= 3; i++ {
		if err := s.AppendSignedOp(id, SignedOp{ValidUntil: i}); err != nil {
			t.Fatal(err)
		}
	}
	ops, _ := s.SignedOps(id)
	if len(ops) != 3 {
		t.Fatalf("want 3 attempts, got %d", len(ops))
	}
	for i, op := range ops {
		if op.ValidUntil != uint64(i+1) {
			t.Fatalf("attempt %d out of order: %+v", i, op)
		}
	}
}

func TestStoreFactsCannotChange(t *testing.T) {
	s := newMemStore()
	id := testID(4)
	first := Fact{TxHash: common.HexToHash("0x01"), BlockNumber: 5, Executed: true}
	if err := s.PutFact(id, first); err != nil {
		t.Fatal(err)
	}
	if err := s.PutFact(id, Fact{TxHash: common.HexToHash("0x02"), Executed: false}); err != nil {
		t.Fatal(err)
	}
	got, ok, _ := s.Fact(id)
	if !ok || got != first {
		t.Fatalf("a finalized fact must never change, got %+v", got)
	}
}

// The domain key separates chains and deployments, which is what keeps one
// identifier from meaning two things.
func TestDomainKeySeparatesChainsAndDeployments(t *testing.T) {
	one := common.HexToAddress("0x0000000000000000000000000000000000001001")
	two := common.HexToAddress("0x0000000000000000000000000000000000002002")

	base := DomainKey(big.NewInt(42161), one)
	if base == DomainKey(big.NewInt(421614), one) {
		t.Error("a different chain must be a different domain")
	}
	if base == DomainKey(big.NewInt(42161), two) {
		t.Error("a different deployment must be a different domain")
	}
	// Address case must not create two names for one domain.
	if base != DomainKey(big.NewInt(42161), common.HexToAddress(strings.ToUpper(one.Hex()[2:]))) {
		t.Error("the key must not depend on address casing")
	}
}

func TestStorePendingExcludesSettled(t *testing.T) {
	s := newMemStore()
	a, b := testID(5), testID(6)
	if err := s.PutIntent(a, Intent{Terms: depositTerms(1)}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIntent(b, Intent{Terms: depositTerms(2)}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutFact(a, Fact{Executed: true}); err != nil {
		t.Fatal(err)
	}

	pending, _ := s.PendingIDs()
	if len(pending) != 1 || pending[0] != b {
		t.Fatalf("want only the unsettled intent, got %v", pending)
	}
}
