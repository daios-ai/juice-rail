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

// testDomain is the domain every store in these tests serves.
var testDomain = rail.DomainKey(big.NewInt(31337),
	common.HexToAddress("0x0000000000000000000000000000000000001001"))

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "rail.db"), testDomain)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func id(n byte) rail.ID {
	var out rail.ID
	out[31] = n
	return out
}

func terms(amount int64) rail.Terms {
	return rail.Terms{
		Kind:    rail.KindDeposit,
		Account: common.HexToAddress("0x00000000000000000000000000000000000000a1"),
		Amount:  big.NewInt(amount),
	}
}

func TestIntentIsWriteOnce(t *testing.T) {
	s := open(t)

	if err := s.PutIntent(id(1), rail.Intent{Terms: terms(100), FromBlock: 7}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIntent(id(1), rail.Intent{Terms: terms(100), FromBlock: 99}); err != nil {
		t.Fatalf("identical terms must be accepted: %v", err)
	}
	if err := s.PutIntent(id(1), rail.Intent{Terms: terms(101)}); !errors.Is(err, rail.ErrIntentConflict) {
		t.Fatalf("want ErrIntentConflict, got %v", err)
	}

	got, ok, err := s.Intent(id(1))
	if err != nil || !ok {
		t.Fatalf("read back: %v %v", ok, err)
	}
	if !got.Terms.Equal(terms(100)) || got.FromBlock != 7 {
		t.Fatalf("the first record must survive, got %+v", got)
	}
}

func TestIntentRoundTripsEveryKind(t *testing.T) {
	s := open(t)
	party := common.HexToAddress("0x00000000000000000000000000000000000000b2")

	for i, want := range []rail.Terms{
		{Kind: rail.KindDeposit, Account: party, Amount: big.NewInt(1)},
		{Kind: rail.KindSettle, Account: party, Party: party, Amount: big.NewInt(2)},
		{Kind: rail.KindWithdraw, Account: party, Party: party,
			Amount: new(big.Int).Lsh(big.NewInt(1), 200)}, // large amounts survive
	} {
		key := id(byte(10 + i))
		if err := s.PutIntent(key, rail.Intent{Terms: want, FromBlock: uint64(i)}); err != nil {
			t.Fatal(err)
		}
		got, ok, err := s.Intent(key)
		if err != nil || !ok || !got.Terms.Equal(want) {
			t.Fatalf("%s round trip: %+v %v %v", want.Kind, got.Terms, ok, err)
		}
	}
}

func TestMissingRecordsReportAbsence(t *testing.T) {
	s := open(t)

	if _, ok, err := s.Intent(id(99)); ok || err != nil {
		t.Fatalf("absent intent: %v %v", ok, err)
	}
	if _, ok, err := s.Fact(id(99)); ok || err != nil {
		t.Fatalf("absent fact: %v %v", ok, err)
	}
	if ops, err := s.SignedOps(id(99)); err != nil || len(ops) != 0 {
		t.Fatalf("absent attempts: %v %v", ops, err)
	}
	if abandoned, err := s.Abandoned(id(99)); err != nil || abandoned {
		t.Fatalf("absent abandonment: %v %v", abandoned, err)
	}
}

func TestAttemptsAppendInOrderAndSurvive(t *testing.T) {
	s := open(t)

	for i := 1; i <= 3; i++ {
		op := rail.SignedOp{
			Op:         []byte{byte(i)},
			Hash:       common.BigToHash(big.NewInt(int64(i))),
			Nonce:      big.NewInt(int64(i)),
			ValidUntil: uint64(1000 + i),
		}
		if err := s.AppendSignedOp(id(2), op); err != nil {
			t.Fatal(err)
		}
	}
	ops, err := s.SignedOps(id(2))
	if err != nil || len(ops) != 3 {
		t.Fatalf("want 3 attempts, got %d (%v)", len(ops), err)
	}
	for i, op := range ops {
		if op.ValidUntil != uint64(1001+i) || op.Op[0] != byte(i+1) ||
			op.Nonce.Int64() != int64(i+1) || op.Hash != common.BigToHash(big.NewInt(int64(i+1))) {
			t.Fatalf("attempt %d round trip: %+v", i, op)
		}
	}
}

// Signing must be refused once the identifier is abandoned, so that releasing
// funds can never race a fresh signature.
func TestAppendIsRefusedAfterAbandon(t *testing.T) {
	s := open(t)

	if err := s.AppendSignedOp(id(3), rail.SignedOp{Op: []byte{1}, ValidUntil: 10}); err != nil {
		t.Fatal(err)
	}
	if err := s.Abandon(id(3)); err != nil {
		t.Fatal(err)
	}
	if err := s.Abandon(id(3)); err != nil {
		t.Fatalf("abandonment must be idempotent: %v", err)
	}
	if err := s.AppendSignedOp(id(3), rail.SignedOp{Op: []byte{2}, ValidUntil: 20}); !errors.Is(err, rail.ErrAbandoned) {
		t.Fatalf("want ErrAbandoned, got %v", err)
	}

	ops, _ := s.SignedOps(id(3))
	if len(ops) != 1 {
		t.Fatalf("the refused attempt must not be stored, got %d", len(ops))
	}
}

// Whichever of the two wins, the outcome is safe: either the attempt is
// recorded before abandonment, or it is refused.
func TestConcurrentAppendAndAbandonStaySerialisable(t *testing.T) {
	s := open(t)
	var wg sync.WaitGroup
	var appendErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		appendErr = s.AppendSignedOp(id(4), rail.SignedOp{Op: []byte{1}, ValidUntil: 10})
	}()
	go func() {
		defer wg.Done()
		if err := s.Abandon(id(4)); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()

	ops, err := s.SignedOps(id(4))
	if err != nil {
		t.Fatal(err)
	}
	abandoned, err := s.Abandoned(id(4))
	if err != nil || !abandoned {
		t.Fatalf("abandonment must hold: %v %v", abandoned, err)
	}
	if appendErr == nil && len(ops) != 1 {
		t.Fatalf("a successful append must be stored, got %d attempts", len(ops))
	}
	if appendErr != nil {
		if !errors.Is(appendErr, rail.ErrAbandoned) {
			t.Fatalf("a refused append must say why: %v", appendErr)
		}
		if len(ops) != 0 {
			t.Fatalf("a refused append must store nothing, got %d", len(ops))
		}
	}
}

func TestFactsCannotChange(t *testing.T) {
	s := open(t)
	first := rail.Fact{TxHash: common.BigToHash(big.NewInt(1)), BlockNumber: 5, Executed: true}

	if err := s.PutFact(id(5), first); err != nil {
		t.Fatal(err)
	}
	if err := s.PutFact(id(5), rail.Fact{TxHash: common.BigToHash(big.NewInt(2)), Executed: false}); err != nil {
		t.Fatalf("a repeat write must be ignored, not fail: %v", err)
	}
	got, ok, err := s.Fact(id(5))
	if err != nil || !ok || got != first {
		t.Fatalf("a finalized fact must never change, got %+v", got)
	}
}

func TestPendingListsOnlyUnsettledIntents(t *testing.T) {
	s := open(t)
	for i := byte(1); i <= 3; i++ {
		if err := s.PutIntent(id(20+i), rail.Intent{Terms: terms(int64(i))}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutFact(id(22), rail.Fact{Executed: true}); err != nil {
		t.Fatal(err)
	}

	pending, err := s.PendingIDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0] != id(21) || pending[1] != id(23) {
		t.Fatalf("pending = %v", pending)
	}
}

// A restart must find every decision exactly as it was left.
func TestRecordsSurviveReopening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rail.db")
	first, err := Open(path, testDomain)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.PutIntent(id(6), rail.Intent{Terms: terms(500), FromBlock: 42}); err != nil {
		t.Fatal(err)
	}
	if err := first.AppendSignedOp(id(6), rail.SignedOp{Op: []byte("signed"), Nonce: big.NewInt(3), ValidUntil: 77}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(path, testDomain)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	in, ok, err := second.Intent(id(6))
	if err != nil || !ok || in.FromBlock != 42 || !in.Terms.Equal(terms(500)) {
		t.Fatalf("intent did not survive: %+v %v %v", in, ok, err)
	}
	ops, err := second.SignedOps(id(6))
	if err != nil || len(ops) != 1 || string(ops[0].Op) != "signed" || ops[0].ValidUntil != 77 {
		t.Fatalf("attempt did not survive: %+v %v", ops, err)
	}
}

// A store serves one domain. Identifiers are unique only within a domain, so
// reopening a store for another must fail rather than alias two ledgers.
func TestStoreRefusesAnotherDomain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rail.db")

	first, err := Open(path, testDomain)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.PutIntent(id(1), rail.Intent{Terms: terms(100)}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// Same chain, a second deployment on it: a different domain.
	other := rail.DomainKey(big.NewInt(31337),
		common.HexToAddress("0x0000000000000000000000000000000000009999"))
	if _, err := Open(path, other); !errors.Is(err, rail.ErrWrongDomain) {
		t.Fatalf("a different deployment: want ErrWrongDomain, got %v", err)
	}

	// Same deployment address, a different chain: also a different domain.
	otherChain := rail.DomainKey(big.NewInt(42161),
		common.HexToAddress("0x0000000000000000000000000000000000001001"))
	if _, err := Open(path, otherChain); !errors.Is(err, rail.ErrWrongDomain) {
		t.Fatalf("a different chain: want ErrWrongDomain, got %v", err)
	}

	// The original domain still opens, with its records intact.
	again, err := Open(path, testDomain)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if _, ok, _ := again.Intent(id(1)); !ok {
		t.Fatal("the records must survive")
	}
}

func TestStoreRequiresADomain(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "rail.db"), ""); err == nil {
		t.Fatal("a store must name the domain it serves")
	}
}

// The store satisfies the interface the rail depends on.
var _ rail.Store = (*Store)(nil)
