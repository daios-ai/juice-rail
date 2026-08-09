package rail

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/daios-ai/juice-rail/go/erc4337"
)

// fakeBundler records what was presented to it.
type fakeBundler struct {
	sent [][]byte
	err  error
}

func (b *fakeBundler) SendUserOperation(_ context.Context, op *erc4337.UserOperation, _ common.Address) (common.Hash, error) {
	if b.err != nil {
		return common.Hash{}, b.err
	}
	b.sent = append(b.sent, op.CallData)
	return op.Hash(common.Address{}, big.NewInt(1)), nil
}

func testKey(t *testing.T, n int64) *ecdsa.PrivateKey {
	t.Helper()
	key, err := crypto.ToECDSA(common.BigToHash(big.NewInt(n)).Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func newTestRail(t *testing.T) (*Rail, *fakeChain, *fakeBundler) {
	t.Helper()
	chain, bundler := newFakeChain(), &fakeBundler{}
	d := testDomain()
	sponsor := LocalSponsor{
		Paymaster:  d.Paymaster,
		EntryPoint: d.Contracts.EntryPoint,
		ChainID:    d.ChainID,
		Key:        testKey(t, 2),
	}
	r, err := New(d, newMemStore(), chain, bundler, sponsor, testKey(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	return r, chain, bundler
}

func TestDomainRejectsIncompleteOrUnsafeConfiguration(t *testing.T) {
	base := testDomain()

	missing := base
	missing.Rail = common.Address{}
	if err := missing.Validate(); err == nil {
		t.Error("a missing rail address must be rejected")
	}

	// Only true finality is admissible: a confirmed fact must never revert.
	for _, mechanism := range []string{"", "latest", "safe", "12-confirmations"} {
		weak := base
		weak.Finality = mechanism
		if err := weak.Validate(); err == nil {
			t.Errorf("finality %q must be rejected", mechanism)
		}
	}
}

func TestPrepareIsWriteAheadOnly(t *testing.T) {
	r, _, bundler := newTestRail(t)
	ctx := context.Background()
	id := testID(1)

	if err := r.Prepare(ctx, id, depositTerms(100)); err != nil {
		t.Fatal(err)
	}
	if ops, _ := r.store.SignedOps(id); len(ops) != 0 {
		t.Fatalf("nothing may be signed yet, got %d attempts", len(ops))
	}
	if len(bundler.sent) != 0 {
		t.Fatal("nothing may be submitted yet")
	}
	in, ok, _ := r.store.Intent(id)
	if !ok || in.FromBlock == 0 {
		t.Fatalf("the intent must record where to start looking, got %+v", in)
	}
}

func TestPrepareRejectsDifferentTermsForTheSameIdentifier(t *testing.T) {
	r, _, _ := newTestRail(t)
	ctx := context.Background()
	id := testID(2)

	if err := r.Prepare(ctx, id, depositTerms(100)); err != nil {
		t.Fatal(err)
	}
	if err := r.Prepare(ctx, id, depositTerms(100)); err != nil {
		t.Fatalf("identical terms must be accepted: %v", err)
	}
	if err := r.Prepare(ctx, id, depositTerms(200)); !errors.Is(err, ErrIntentConflict) {
		t.Fatalf("want ErrIntentConflict, got %v", err)
	}
}

// Signing is guarded: while an attempt may still execute, no second one is
// created. Only expiry at finality releases the guard.
func TestSignsAgainOnlyOnceTheAttemptIsProvablyDead(t *testing.T) {
	r, chain, _ := newTestRail(t)
	ctx := context.Background()
	id := testID(3)

	if err := r.Prepare(ctx, id, depositTerms(100)); err != nil {
		t.Fatal(err)
	}
	if err := r.Sign(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := r.Sign(ctx, id); err != nil {
		t.Fatal(err)
	}
	ops, _ := r.store.SignedOps(id)
	if len(ops) != 1 {
		t.Fatalf("a live attempt must not be replaced, got %d", len(ops))
	}

	// Time moves past the sponsorship expiry, at the head and at finality.
	chain.finalizedTime = ops[0].ValidUntil + 1
	chain.headTime = chain.finalizedTime
	if err := r.Sign(ctx, id); err != nil {
		t.Fatal(err)
	}
	ops, _ = r.store.SignedOps(id)
	if len(ops) != 2 {
		t.Fatalf("a dead attempt must be replaced, got %d", len(ops))
	}
	if bytes.Equal(ops[0].Op, ops[1].Op) {
		t.Fatal("the replacement must be a fresh operation")
	}
}

func TestSubmitReSendsTheSameSignedOperation(t *testing.T) {
	r, _, bundler := newTestRail(t)
	ctx := context.Background()
	id := testID(4)

	if err := r.Prepare(ctx, id, depositTerms(100)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := r.Submit(ctx, id); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	if ops, _ := r.store.SignedOps(id); len(ops) != 1 {
		t.Fatalf("resubmission must not sign again, got %d attempts", len(ops))
	}
	if len(bundler.sent) != 3 {
		t.Fatalf("want 3 submissions, got %d", len(bundler.sent))
	}
	for i := 1; i < len(bundler.sent); i++ {
		if !bytes.Equal(bundler.sent[0], bundler.sent[i]) {
			t.Fatal("every resubmission must present identical bytes")
		}
	}
}

// The attempt is durable before it is presented: if submission fails, the
// record of what we signed survives.
func TestAttemptIsDurableBeforeSubmission(t *testing.T) {
	r, _, bundler := newTestRail(t)
	ctx := context.Background()
	id := testID(5)
	bundler.err = errors.New("bundler unreachable")

	if err := r.Prepare(ctx, id, depositTerms(100)); err != nil {
		t.Fatal(err)
	}
	if err := r.Submit(ctx, id); err == nil {
		t.Fatal("want a submission error")
	}
	if ops, _ := r.store.SignedOps(id); len(ops) != 1 {
		t.Fatalf("the signed attempt must survive a failed submission, got %d", len(ops))
	}
}

func TestSigningIsRefusedAfterAbandonment(t *testing.T) {
	r, _, _ := newTestRail(t)
	ctx := context.Background()
	id := testID(6)

	if err := r.Prepare(ctx, id, depositTerms(100)); err != nil {
		t.Fatal(err)
	}
	if err := r.Abandon(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := r.Sign(ctx, id); !errors.Is(err, ErrAbandoned) {
		t.Fatalf("want ErrAbandoned, got %v", err)
	}
}

func TestSubmitStopsOnceSettled(t *testing.T) {
	r, chain, bundler := newTestRail(t)
	ctx := context.Background()
	id, terms := testID(7), depositTerms(100)

	if err := r.Prepare(ctx, id, terms); err != nil {
		t.Fatal(err)
	}
	chain.logs = append(chain.logs, railLog(r.domain.Rail, id, terms, chain.head+1))
	chain.finalized = chain.head + 2

	if err := r.Submit(ctx, id); err != nil {
		t.Fatal(err)
	}
	if len(bundler.sent) != 0 {
		t.Fatal("a confirmed intent must not be submitted again")
	}
}

func TestSubmitWithoutAnIntentIsRefused(t *testing.T) {
	r, _, _ := newTestRail(t)
	if err := r.Submit(context.Background(), testID(8)); !errors.Is(err, ErrNoIntent) {
		t.Fatalf("want ErrNoIntent, got %v", err)
	}
}

// Each intent owns a nonce key, so one stuck attempt never blocks another.
func TestNonceKeysAreDistinctPerIdentifier(t *testing.T) {
	if nonceKey(testID(1)).Cmp(nonceKey(testID(2))) == 0 {
		t.Fatal("different identifiers must use different nonce keys")
	}
	if got := nonceKey(testID(1)).BitLen(); got > 192 {
		t.Fatalf("nonce key must fit uint192, got %d bits", got)
	}
}

func TestNamedOperationsRejectTheWrongKind(t *testing.T) {
	r, _, _ := newTestRail(t)
	ctx := context.Background()
	id := testID(9)

	if err := r.Prepare(ctx, id, depositTerms(100)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SettlementStatus(ctx, id); err == nil {
		t.Fatal("a deposit must not report as a settlement")
	}
	if _, err := r.DepositStatus(ctx, id); err != nil {
		t.Fatalf("a deposit must report as a deposit: %v", err)
	}
}

func TestAccountAddressIsStable(t *testing.T) {
	r, _, _ := newTestRail(t)
	ctx := context.Background()

	first, err := r.Account(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Account(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first == (common.Address{}) {
		t.Fatalf("account address must be stable and non-zero: %s %s", first, second)
	}
}

func TestSettleAndWithdrawBindTheRailAsDebtor(t *testing.T) {
	r, _, _ := newTestRail(t)
	ctx := context.Background()
	self, err := r.Account(ctx)
	if err != nil {
		t.Fatal(err)
	}
	creditor := common.BigToAddress(big.NewInt(0xC1))

	if err := r.Settle(ctx, testID(10), creditor, big.NewInt(40)); err != nil {
		t.Fatal(err)
	}
	in, _, _ := r.store.Intent(testID(10))
	if in.Terms.Account != self || in.Terms.Party != creditor || in.Terms.Kind != KindSettle {
		t.Fatalf("settle terms = %s, want debtor %s", in.Terms, self)
	}

	if err := r.Withdraw(ctx, testID(11), creditor, big.NewInt(25)); err != nil {
		t.Fatal(err)
	}
	in, _, _ = r.store.Intent(testID(11))
	if in.Terms.Account != self || in.Terms.Party != creditor || in.Terms.Kind != KindWithdraw {
		t.Fatalf("withdraw terms = %s, want debtor %s", in.Terms, self)
	}
}
