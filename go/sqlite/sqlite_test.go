package sqlite

import (
	"errors"
	"math/big"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/daios-ai/juice-rail/go/rail"
)

var (
	account = common.HexToAddress("0x1111111111111111111111111111111111111111")
	other   = common.HexToAddress("0x2222222222222222222222222222222222222222")
	payee   = common.HexToAddress("0x3333333333333333333333333333333333333333")
)

const testDomain = "31337:0x00000000000000000000000000000000000000a0"

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "rail.db"), testDomain)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func intent(id byte, nonce uint64, amount int64) rail.Intent {
	return rail.Intent{
		ID:        rail.ID{id},
		Kind:      rail.KindTransfer,
		To:        payee,
		Amount:    big.NewInt(amount),
		Nonce:     nonce,
		Calldata:  []byte{0xa9, 0x05, 0x9c, 0xbb, id},
		FromBlock: 12,
	}
}

func TestStoreServesOneDomain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rail.db")
	s, err := Open(path, testDomain)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s.Close()

	if _, err := Open(path, "1:0x00000000000000000000000000000000000000ff"); !errors.Is(err, rail.ErrWrongDomain) {
		t.Fatalf("reopened for another domain: %v, want a refusal", err)
	}
	again, err := Open(path, testDomain)
	if err != nil {
		t.Fatalf("reopen for its own domain: %v", err)
	}
	again.Close()
}

func TestIntentsAreWriteOnce(t *testing.T) {
	s := open(t)
	in := intent(1, 0, 1000)
	if err := s.PutIntent(account, in); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.PutIntent(account, in); err != nil {
		t.Fatalf("record the same decision again: %v", err)
	}

	changed := in
	changed.Amount = big.NewInt(2000)
	if err := s.PutIntent(account, changed); !errors.Is(err, rail.ErrIntentConflict) {
		t.Fatalf("different terms under one identifier: %v, want a conflict", err)
	}
	// The original survives the attempt.
	got, ok, err := s.Intent(account, in.ID)
	if err != nil || !ok {
		t.Fatalf("read back: %v %v", ok, err)
	}
	if got.Amount.Cmp(in.Amount) != 0 || got.Nonce != in.Nonce || string(got.Calldata) != string(in.Calldata) {
		t.Fatalf("the record changed: %+v", got)
	}
}

func TestOneNonceCarriesOneOperation(t *testing.T) {
	s := open(t)
	if err := s.PutIntent(account, intent(1, 7, 100)); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.PutIntent(account, intent(2, 7, 100)); !errors.Is(err, rail.ErrIntentConflict) {
		t.Fatalf("second operation on one nonce: %v, want a conflict", err)
	}
	// A different account may use the same nonce: nonces are per account.
	if err := s.PutIntent(other, intent(3, 7, 100)); err != nil {
		t.Fatalf("another account on the same nonce: %v", err)
	}
}

func TestIntentsAreFoundByNonce(t *testing.T) {
	s := open(t)
	for i := uint64(0); i < 3; i++ {
		if err := s.PutIntent(account, intent(byte(i+1), i, 100)); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	in, ok, err := s.IntentByNonce(account, 2)
	if err != nil || !ok || in.ID != (rail.ID{3}) {
		t.Fatalf("nonce 2 belongs to %s (%v %v)", in.ID, ok, err)
	}
	if _, ok, _ := s.IntentByNonce(other, 2); ok {
		t.Fatal("one account's nonce was read as another's")
	}
}

func TestPendingIsWhatHasNoFinalizedFact(t *testing.T) {
	s := open(t)
	for i := uint64(0); i < 3; i++ {
		if err := s.PutIntent(account, intent(byte(i+1), i, 100)); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	if err := s.PutFact(account, rail.ID{2}, rail.Fact{Executed: true, BlockNumber: 3}); err != nil {
		t.Fatalf("fact: %v", err)
	}
	pending, err := s.Pending(account)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 2 || pending[0].Nonce != 0 || pending[1].Nonce != 2 {
		t.Fatalf("pending is %+v", pending)
	}
}

func TestFactsAreNeverRewritten(t *testing.T) {
	s := open(t)
	first := rail.Fact{TxHash: common.HexToHash("0xaa"), BlockNumber: 4, Executed: true}
	if err := s.PutFact(account, rail.ID{1}, first); err != nil {
		t.Fatalf("fact: %v", err)
	}
	if err := s.PutFact(account, rail.ID{1}, rail.Fact{BlockNumber: 9}); err != nil {
		t.Fatalf("fact again: %v", err)
	}
	got, ok, _ := s.Fact(account, rail.ID{1})
	if !ok || got != first {
		t.Fatalf("a finalized fact was overwritten: %+v", got)
	}
}

func TestSubmissionsAreAppendedInOrder(t *testing.T) {
	s := open(t)
	for i := 0; i < 3; i++ {
		sub := rail.Submission{
			TxHash: common.BigToHash(big.NewInt(int64(i))),
			Gas:    21000,
			Tip:    big.NewInt(int64(i + 1)),
			FeeCap: big.NewInt(int64(100 * (i + 1))),
		}
		if err := s.AppendSubmission(account, rail.ID{1}, sub); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	subs, err := s.Submissions(account, rail.ID{1})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(subs) != 3 {
		t.Fatalf("%d attempts, want 3", len(subs))
	}
	for i, sub := range subs {
		if sub.Tip.Int64() != int64(i+1) || sub.FeeCap.Int64() != int64(100*(i+1)) {
			t.Fatalf("attempt %d came back as %+v", i, sub)
		}
	}
}

func TestDepositsAreIdentifiedByTheirLog(t *testing.T) {
	s := open(t)
	d := rail.Deposit{
		TxHash: common.HexToHash("0xaa"), LogIndex: 2, From: other,
		Amount: big.NewInt(25_000_000), BlockNumber: 9,
	}
	for i := 0; i < 3; i++ {
		if err := s.PutDeposit(account, d); err != nil {
			t.Fatalf("deposit: %v", err)
		}
	}
	all, err := s.Deposits(account)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(all) != 1 || all[0].Amount.Cmp(d.Amount) != 0 || all[0].From != other {
		t.Fatalf("one transfer stored as %+v", all)
	}
	// Two logs in one transaction are two deposits.
	d.LogIndex = 3
	if err := s.PutDeposit(account, d); err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if all, _ = s.Deposits(account); len(all) != 2 {
		t.Fatalf("two logs stored as %d deposits", len(all))
	}
}

func TestCursorsOnlyAdvance(t *testing.T) {
	s := open(t)
	if _, ok, err := s.Cursor(account); ok || err != nil {
		t.Fatalf("a fresh store reported a cursor (%v)", err)
	}
	for _, step := range []struct{ set, want uint64 }{{10, 10}, {4, 10}, {12, 12}} {
		if err := s.PutCursor(account, step.set); err != nil {
			t.Fatalf("cursor: %v", err)
		}
		got, ok, _ := s.Cursor(account)
		if !ok || got != step.want {
			t.Fatalf("after %d the cursor is %d, want %d", step.set, got, step.want)
		}
		if err := s.PutNonceFloor(account, step.set); err != nil {
			t.Fatalf("floor: %v", err)
		}
		if got, ok, _ = s.NonceFloor(account); !ok || got != step.want {
			t.Fatalf("after %d the floor is %d, want %d", step.set, got, step.want)
		}
	}
}

func TestConcurrentWritersStaySerialisable(t *testing.T) {
	s := open(t)
	const writers = 8
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Every writer records the same decision on the same nonce.
			errs[i] = s.PutIntent(account, intent(1, 0, 100))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	if _, ok, _ := s.Intent(account, rail.ID{1}); !ok {
		t.Fatal("the record went missing under concurrency")
	}
}
