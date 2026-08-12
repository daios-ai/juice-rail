package rail

import (
	"errors"
	"math/big"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// memStore is a second Store implementation. Keeping one in the tests is what
// stops the interface quietly meaning "whatever SQLite happens to do".
type memStore struct {
	mu       sync.Mutex
	intents  map[string]Intent
	byNonce  map[string]ID
	order    []string
	subs     map[string][]Submission
	facts    map[string]Fact
	deposits map[string][]Deposit
	seen     map[string]bool
	cursor   map[common.Address]uint64
	floor    map[common.Address]uint64
}

func newMemStore() *memStore {
	return &memStore{
		intents:  map[string]Intent{},
		byNonce:  map[string]ID{},
		subs:     map[string][]Submission{},
		facts:    map[string]Fact{},
		deposits: map[string][]Deposit{},
		seen:     map[string]bool{},
		cursor:   map[common.Address]uint64{},
		floor:    map[common.Address]uint64{},
	}
}

func mkey(a common.Address, id ID) string { return a.Hex() + "/" + id.String() }

func nkey(a common.Address, nonce uint64) string {
	return a.Hex() + "#" + new(big.Int).SetUint64(nonce).String()
}

func (s *memStore) PutIntent(account common.Address, in Intent) error {
	if err := in.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	k := mkey(account, in.ID)
	if existing, ok := s.intents[k]; ok {
		if !existing.SameTerms(in) {
			return ErrIntentConflict
		}
		return nil
	}
	if _, taken := s.byNonce[nkey(account, in.Nonce)]; taken {
		return ErrIntentConflict
	}
	s.intents[k] = in
	s.byNonce[nkey(account, in.Nonce)] = in.ID
	s.order = append(s.order, k)
	return nil
}

func (s *memStore) Intent(account common.Address, id ID) (Intent, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.intents[mkey(account, id)]
	return in, ok, nil
}

func (s *memStore) IntentByNonce(account common.Address, nonce uint64) (Intent, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byNonce[nkey(account, nonce)]
	if !ok {
		return Intent{}, false, nil
	}
	return s.intents[mkey(account, id)], true, nil
}

func (s *memStore) Pending(account common.Address) ([]Intent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Intent
	for _, k := range s.order {
		in := s.intents[k]
		if k != mkey(account, in.ID) {
			continue
		}
		if _, done := s.facts[k]; !done {
			out = append(out, in)
		}
	}
	return out, nil
}

func (s *memStore) AppendSubmission(account common.Address, id ID, sub Submission) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := mkey(account, id)
	s.subs[k] = append(s.subs[k], sub)
	return nil
}

func (s *memStore) Submissions(account common.Address, id ID) ([]Submission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Submission(nil), s.subs[mkey(account, id)]...), nil
}

func (s *memStore) PutFact(account common.Address, id ID, f Fact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := mkey(account, id)
	if _, ok := s.facts[k]; !ok {
		s.facts[k] = f
	}
	return nil
}

func (s *memStore) Fact(account common.Address, id ID) (Fact, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.facts[mkey(account, id)]
	return f, ok, nil
}

func (s *memStore) PutDeposit(account common.Address, d Deposit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := account.Hex() + "/" + d.TxHash.Hex() + "#" + new(big.Int).SetUint64(uint64(d.LogIndex)).String()
	if s.seen[k] {
		return nil
	}
	s.seen[k] = true
	s.deposits[account.Hex()] = append(s.deposits[account.Hex()], d)
	return nil
}

func (s *memStore) Deposits(account common.Address) ([]Deposit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Deposit(nil), s.deposits[account.Hex()]...), nil
}

func (s *memStore) Cursor(account common.Address) (uint64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.cursor[account]
	return v, ok, nil
}

func (s *memStore) PutCursor(account common.Address, block uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if block > s.cursor[account] {
		s.cursor[account] = block
	}
	return nil
}

func (s *memStore) NonceFloor(account common.Address) (uint64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.floor[account]
	return v, ok, nil
}

func (s *memStore) PutNonceFloor(account common.Address, nonce uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.floor[account]; !ok || nonce > cur {
		s.floor[account] = nonce
	}
	return nil
}

func (s *memStore) Close() error { return nil }

// --- the properties every Store must have ---

func testIntent(id byte, nonce uint64) Intent {
	return Intent{
		ID:        ID{id},
		Kind:      KindTransfer,
		To:        common.HexToAddress("0x2222222222222222222222222222222222222222"),
		Amount:    big.NewInt(1000),
		Nonce:     nonce,
		Calldata:  []byte{0xa9, 0x05, 0x9c, 0xbb, byte(nonce)},
		FromBlock: 7,
	}
}

var testAccount = common.HexToAddress("0x1111111111111111111111111111111111111111")

func TestStoreIntentsAreWriteOnce(t *testing.T) {
	s := newMemStore()
	in := testIntent(1, 0)
	if err := s.PutIntent(testAccount, in); err != nil {
		t.Fatalf("record: %v", err)
	}
	// The same decision recorded twice is the same decision.
	if err := s.PutIntent(testAccount, in); err != nil {
		t.Fatalf("re-record: %v", err)
	}
	changed := in
	changed.Amount = big.NewInt(2000)
	if err := s.PutIntent(testAccount, changed); !errors.Is(err, ErrIntentConflict) {
		t.Fatalf("different terms under one identifier: %v, want conflict", err)
	}
	// One nonce carries one operation, whatever it is called.
	other := testIntent(2, 0)
	if err := s.PutIntent(testAccount, other); !errors.Is(err, ErrIntentConflict) {
		t.Fatalf("second intent on one nonce: %v, want conflict", err)
	}
}

func TestStoreFindsIntentByNonce(t *testing.T) {
	s := newMemStore()
	for i := uint64(0); i < 3; i++ {
		if err := s.PutIntent(testAccount, testIntent(byte(i+1), i)); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	in, ok, err := s.IntentByNonce(testAccount, 1)
	if err != nil || !ok {
		t.Fatalf("by nonce: %v %v", ok, err)
	}
	if in.ID != (ID{2}) {
		t.Fatalf("nonce 1 belongs to %s", in.ID)
	}
	if _, ok, _ := s.IntentByNonce(testAccount, 9); ok {
		t.Fatal("an unused nonce reported an intent")
	}
}

func TestStorePendingIsWhatHasNoFact(t *testing.T) {
	s := newMemStore()
	for i := uint64(0); i < 3; i++ {
		if err := s.PutIntent(testAccount, testIntent(byte(i+1), i)); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	if err := s.PutFact(testAccount, ID{2}, Fact{Executed: true}); err != nil {
		t.Fatalf("fact: %v", err)
	}
	pending, err := s.Pending(testAccount)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending is %d intents, want 2", len(pending))
	}
}

func TestStoreFactsNeverChange(t *testing.T) {
	s := newMemStore()
	if err := s.PutFact(testAccount, ID{1}, Fact{Executed: true, BlockNumber: 4}); err != nil {
		t.Fatalf("fact: %v", err)
	}
	if err := s.PutFact(testAccount, ID{1}, Fact{Executed: false, BlockNumber: 9}); err != nil {
		t.Fatalf("fact again: %v", err)
	}
	f, ok, _ := s.Fact(testAccount, ID{1})
	if !ok || !f.Executed || f.BlockNumber != 4 {
		t.Fatalf("a finalized fact was overwritten: %+v", f)
	}
}

func TestStoreDepositsAreIdentifiedByTheirLog(t *testing.T) {
	s := newMemStore()
	d := Deposit{TxHash: common.HexToHash("0xaa"), LogIndex: 3, From: testAccount, Amount: big.NewInt(5)}
	for i := 0; i < 3; i++ {
		if err := s.PutDeposit(testAccount, d); err != nil {
			t.Fatalf("deposit: %v", err)
		}
	}
	all, _ := s.Deposits(testAccount)
	if len(all) != 1 {
		t.Fatalf("one transfer recorded %d times", len(all))
	}
	d.LogIndex = 4
	if err := s.PutDeposit(testAccount, d); err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if all, _ = s.Deposits(testAccount); len(all) != 2 {
		t.Fatalf("two different logs recorded as %d", len(all))
	}
}

func TestStoreCursorsOnlyAdvance(t *testing.T) {
	s := newMemStore()
	for _, pair := range []struct {
		set  uint64
		want uint64
	}{{10, 10}, {4, 10}, {12, 12}} {
		if err := s.PutCursor(testAccount, pair.set); err != nil {
			t.Fatalf("cursor: %v", err)
		}
		got, ok, _ := s.Cursor(testAccount)
		if !ok || got != pair.want {
			t.Fatalf("after setting %d the cursor is %d, want %d", pair.set, got, pair.want)
		}
		if err := s.PutNonceFloor(testAccount, pair.set); err != nil {
			t.Fatalf("floor: %v", err)
		}
		if got, ok, _ = s.NonceFloor(testAccount); !ok || got != pair.want {
			t.Fatalf("after setting %d the floor is %d, want %d", pair.set, got, pair.want)
		}
	}
}
