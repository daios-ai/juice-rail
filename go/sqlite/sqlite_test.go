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

const testDomain = "31337:0x00000000000000000000000000000000000000a1"

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "rail.db"), testDomain)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func addr(n byte) common.Address {
	var a common.Address
	a[19] = n
	return a
}

func id(n byte) rail.ID {
	var v rail.ID
	v[31] = n
	return v
}

func ref(account, identifier byte) rail.Ref {
	return rail.Ref{Account: addr(account), ID: id(identifier)}
}

func terms(kind rail.Kind, account, party byte, amount int64) rail.Terms {
	return rail.Terms{Kind: kind, Account: addr(account), Party: addr(party), Amount: big.NewInt(amount)}
}

func variant(kind rail.Kind, account, party byte, amount, fee int64, validBefore uint64) rail.Variant {
	v := rail.Variant{
		Terms:       terms(kind, account, party, amount),
		ID:          id(1),
		Fee:         big.NewInt(fee),
		Relayer:     addr(9),
		ValidBefore: validBefore,
		TermsSig:    make([]byte, 65),
	}
	for i := range v.TermsSig {
		v.TermsSig[i] = byte(i)
	}
	if kind == rail.KindDeposit {
		v.AuthSig = make([]byte, 65)
	}
	return v
}

func TestOpenBindsOneDomain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rail.db")

	s, err := Open(path, testDomain)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	again, err := Open(path, testDomain)
	if err != nil {
		t.Fatalf("reopening the same domain must work: %v", err)
	}
	again.Close()

	if _, err := Open(path, "1:0x00000000000000000000000000000000000000ff"); !errors.Is(err, rail.ErrWrongDomain) {
		t.Fatalf("want ErrWrongDomain, got %v", err)
	}
	if _, err := Open(path, ""); err == nil {
		t.Fatal("a store must know its domain")
	}
}

func TestIntentsAreWriteOnce(t *testing.T) {
	s := open(t)
	r := ref(1, 1)
	in := rail.Intent{Terms: terms(rail.KindTransfer, 1, 2, 10), FromBlock: 42}

	if err := s.PutIntent(r, in); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIntent(r, in); err != nil {
		t.Fatalf("identical terms must be accepted again: %v", err)
	}
	other := in
	other.Terms.Party = addr(3)
	if err := s.PutIntent(r, other); !errors.Is(err, rail.ErrIntentConflict) {
		t.Fatalf("want ErrIntentConflict, got %v", err)
	}

	got, ok, err := s.Intent(r)
	if err != nil || !ok {
		t.Fatalf("intent missing: %v %v", ok, err)
	}
	if !got.Terms.Equal(in.Terms) || got.FromBlock != 42 {
		t.Fatalf("read back %+v, want %+v", got, in)
	}
	if _, ok, _ := s.Intent(ref(1, 2)); ok {
		t.Fatal("an unrecorded identifier must be unknown")
	}
}

// The contract scopes identifiers to the account that binds them, and so does
// the store: one store may serve several accounts on one domain.
func TestRecordsAreScopedToTheirAccount(t *testing.T) {
	s := open(t)
	alice, bob := ref(1, 1), ref(2, 1)

	if err := s.PutIntent(alice, rail.Intent{Terms: terms(rail.KindTransfer, 1, 3, 10)}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIntent(bob, rail.Intent{Terms: terms(rail.KindWithdraw, 2, 4, 77)}); err != nil {
		t.Fatalf("the same identifier under another account must not conflict: %v", err)
	}

	got, _, _ := s.Intent(bob)
	if got.Terms.Amount.Int64() != 77 || got.Terms.Account != addr(2) {
		t.Fatalf("accounts alias: %+v", got.Terms)
	}
	if err := s.Abandon(alice); err != nil {
		t.Fatal(err)
	}
	if abandoned, _ := s.Abandoned(bob); abandoned {
		t.Fatal("abandonment crossed accounts")
	}
	if err := s.PutFact(alice, rail.Fact{Executed: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Fact(bob); ok {
		t.Fatal("facts crossed accounts")
	}
}

func TestVariantsAppendUntilAbandoned(t *testing.T) {
	s := open(t)
	r := ref(1, 1)

	for _, deadline := range []uint64{100, 200, 300} {
		if err := s.AppendVariant(r, variant(rail.KindTransfer, 1, 2, 10, 1, deadline)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Variants(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 variants, got %d", len(got))
	}
	for i, want := range []uint64{100, 200, 300} {
		if got[i].ValidBefore != want {
			t.Fatalf("variant %d has deadline %d, want %d: order is the record", i, got[i].ValidBefore, want)
		}
	}
	if string(got[0].TermsSig) != string(variant(rail.KindTransfer, 1, 2, 10, 1, 100).TermsSig) {
		t.Fatal("the signature did not survive the round trip")
	}

	if err := s.Abandon(r); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendVariant(r, variant(rail.KindTransfer, 1, 2, 10, 1, 400)); !errors.Is(err, rail.ErrAbandoned) {
		t.Fatalf("want ErrAbandoned, got %v", err)
	}
	if after, _ := s.Variants(r); len(after) != 3 {
		t.Fatalf("abandonment killed signed variants: %d remain", len(after))
	}
}

func TestDepositVariantsKeepTheirAuthorisation(t *testing.T) {
	s := open(t)
	r := ref(1, 1)
	v := variant(rail.KindDeposit, 1, 2, 10, 1, 100)
	for i := range v.AuthSig {
		v.AuthSig[i] = byte(255 - i)
	}
	if err := s.AppendVariant(r, v); err != nil {
		t.Fatal(err)
	}
	got, err := s.Variants(r)
	if err != nil || len(got) != 1 {
		t.Fatalf("read back %d variants (%v)", len(got), err)
	}
	if string(got[0].AuthSig) != string(v.AuthSig) {
		t.Fatal("a deposit without its token authorisation can pull nothing")
	}
}

func TestFactsAreCachedOnce(t *testing.T) {
	s := open(t)
	r := ref(1, 1)
	first := rail.Fact{TxHash: common.HexToHash("0xaa"), BlockNumber: 5, Executed: true}
	if err := s.PutFact(r, first); err != nil {
		t.Fatal(err)
	}
	if err := s.PutFact(r, rail.Fact{TxHash: common.HexToHash("0xbb"), BlockNumber: 9}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Fact(r)
	if err != nil || !ok {
		t.Fatal("fact missing")
	}
	if got != first {
		t.Fatalf("a finalized fact changed: %+v, want %+v", got, first)
	}
}

func TestPendingListsWhatIsStillWatched(t *testing.T) {
	s := open(t)
	a, b := ref(1, 1), ref(1, 2)
	for _, r := range []rail.Ref{a, b} {
		if err := s.PutIntent(r, rail.Intent{Terms: terms(rail.KindTransfer, 1, 2, 1)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutFact(a, rail.Fact{Executed: true}); err != nil {
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

// A release must never race a fresh signature: either the variant is recorded
// before abandonment, or it is refused.
func TestConcurrentAppendAndAbandonStaySerialisable(t *testing.T) {
	s := open(t)
	r := ref(1, 1)

	var wg sync.WaitGroup
	appended := make([]error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			appended[i] = s.AppendVariant(r, variant(rail.KindTransfer, 1, 2, 10, 1, uint64(100+i)))
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.Abandon(r); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()

	stored, err := s.Variants(r)
	if err != nil {
		t.Fatal(err)
	}
	refused := 0
	for _, err := range appended {
		if errors.Is(err, rail.ErrAbandoned) {
			refused++
		} else if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if len(stored)+refused != 20 {
		t.Fatalf("%d recorded and %d refused, want 20 in total", len(stored), refused)
	}
	if abandoned, _ := s.Abandoned(r); !abandoned {
		t.Fatal("abandonment was lost")
	}
	// Nothing may be recorded after the abandonment took effect.
	if err := s.AppendVariant(r, variant(rail.KindTransfer, 1, 2, 10, 1, 999)); !errors.Is(err, rail.ErrAbandoned) {
		t.Fatalf("want ErrAbandoned, got %v", err)
	}
}

// Recording the same intent twice must always succeed, including when two
// goroutines do it at once: retry is the recovery protocol, so a retry that
// agrees with what is already recorded can never be an error.
func TestConcurrentPutIntentIsIdempotent(t *testing.T) {
	s := open(t)
	r := ref(1, 1)
	in := rail.Intent{Terms: terms(rail.KindTransfer, 1, 2, 10), FromBlock: 42}

	var wg sync.WaitGroup
	errs := make([]error, 20)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.PutIntent(r, in)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("recording identical terms was refused (%d): %v", i, err)
		}
	}
	got, ok, err := s.Intent(r)
	if err != nil || !ok {
		t.Fatalf("intent missing: %v %v", ok, err)
	}
	if !got.Terms.Equal(in.Terms) || got.FromBlock != 42 {
		t.Fatalf("read back %+v, want %+v", got, in)
	}
	// Conflicting terms are still refused, whoever asks.
	other := in
	other.Terms.Amount = big.NewInt(11)
	if err := s.PutIntent(r, other); !errors.Is(err, rail.ErrIntentConflict) {
		t.Fatalf("want ErrIntentConflict, got %v", err)
	}
}
