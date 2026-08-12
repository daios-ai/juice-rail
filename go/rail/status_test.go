package rail

import (
	"context"
	"errors"
	"math/big"
	"testing"
)

func TestClassifyReadsOnlyFinalizedFacts(t *testing.T) {
	for _, tc := range []struct {
		name string
		fact *Fact
		want Status
	}{
		{"nothing finalized yet", nil, StatusPending},
		{"finalized and executed", &Fact{Executed: true}, StatusConfirmed},
		{"finalized and not executed", &Fact{Executed: false}, StatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.fact); got != tc.want {
				t.Fatalf("classify is %s, want %s", got, tc.want)
			}
		})
	}
}

func TestUnknownIntentIsUnknown(t *testing.T) {
	r, _, _ := newTestRail(t)
	if status, err := r.Status(context.Background(), id(9)); err != nil || status != StatusUnknown {
		t.Fatalf("status %s (%v), want unknown", status, err)
	}
}

func TestPaymentIsPendingUntilItFinalizes(t *testing.T) {
	ctx := context.Background()
	r, chain, _ := newTestRail(t)
	mustPrepare(t, r, id(1), bob, 10_000_000)

	hash, err := r.Send(ctx, id(1))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if status, _ := r.Status(ctx, id(1)); status != StatusPending {
		t.Fatalf("submitted is %s, want pending", status)
	}
	chain.include(t, hash, true, transferLogOf(r.Domain(), r.Account(), bob, big.NewInt(10_000_000)))
	if status, _ := r.Status(ctx, id(1)); status != StatusPending {
		t.Fatalf("included but unfinalized is %s, want pending", status)
	}
	chain.finalize()
	if status, _ := r.Status(ctx, id(1)); status != StatusConfirmed {
		t.Fatalf("finalized is %s, want confirmed", status)
	}
}

func TestRevertedIsFailed(t *testing.T) {
	ctx := context.Background()
	r, chain, _ := newTestRail(t)
	mustPrepare(t, r, id(1), bob, 10_000_000)
	hash, err := r.Send(ctx, id(1))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	chain.include(t, hash, false)
	chain.finalize()
	if status, _ := r.Status(ctx, id(1)); status != StatusFailed {
		t.Fatalf("a finalized revert is %s, want failed", status)
	}
}

func TestSuccessWithoutTheTransferIsNotConfirmed(t *testing.T) {
	ctx := context.Background()
	r, chain, _ := newTestRail(t)
	mustPrepare(t, r, id(1), bob, 10_000_000)
	hash, err := r.Send(ctx, id(1))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	// A token that reports failure by returning false rather than reverting.
	chain.include(t, hash, true)
	chain.finalize()
	if status, _ := r.Status(ctx, id(1)); status != StatusFailed {
		t.Fatalf("success with no transfer is %s, want failed", status)
	}
}

func TestAForeignNonceIsReported(t *testing.T) {
	ctx := context.Background()
	r, chain, _ := newTestRail(t)
	// Bind the floor at the current nonce, then let another signer spend one.
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	chain.spendNonce(r.Account(), 1)
	chain.finalize()

	if err := r.Reconcile(ctx); !errors.Is(err, ErrUnreconciled) {
		t.Fatalf("a nonce spent by someone else: %v, want a report", err)
	}
	if err := r.Prepare(ctx, id(1), KindTransfer, bob, big.NewInt(1)); !errors.Is(err, ErrUnreconciled) {
		t.Fatalf("acting after an unexplained nonce: %v, want a refusal", err)
	}
}

func TestReconcileAcceptsAHistoryItRecorded(t *testing.T) {
	ctx := context.Background()
	r, chain, _ := newTestRail(t)
	mustPrepare(t, r, id(1), bob, 10_000_000)
	hash, err := r.Send(ctx, id(1))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	chain.include(t, hash, true, transferLogOf(r.Domain(), r.Account(), bob, big.NewInt(10_000_000)))
	chain.finalize()

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile over its own history: %v", err)
	}
	// And the account can act again afterwards.
	if err := r.Prepare(ctx, id(2), KindTransfer, bob, big.NewInt(1_000_000)); err != nil {
		t.Fatalf("prepare after reconciliation: %v", err)
	}
}

func TestScanDepositsRecordsIncomingMoneyOnce(t *testing.T) {
	ctx := context.Background()
	r, chain, _ := newTestRail(t)
	d := r.Domain()
	chain.addTransferLog(5, 0, d.Token, bob, r.Account(), big.NewInt(25_000_000))
	chain.addTransferLog(5, 1, d.Token, exchange, r.Account(), big.NewInt(1_000_000))
	// Money this account sent itself is not incoming money.
	chain.addTransferLog(6, 0, d.Token, r.Account(), r.Account(), big.NewInt(7))

	found, err := r.ScanDeposits(ctx)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("found %d deposits, want 2", len(found))
	}
	again, err := r.ScanDeposits(ctx)
	if err != nil || len(again) != 0 {
		t.Fatalf("rescanning found %d deposits (%v), want none", len(again), err)
	}
	all, _ := r.Deposits()
	if len(all) != 2 {
		t.Fatalf("recorded %d deposits, want 2", len(all))
	}
}

func TestUnfinalizedDepositsAreNotRecorded(t *testing.T) {
	ctx := context.Background()
	r, chain, _ := newTestRail(t)
	chain.addTransferLog(9, 0, r.Domain().Token, bob, r.Account(), big.NewInt(5))

	found, err := r.ScanDeposits(ctx)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(found) != 0 {
		t.Fatal("an unfinalized transfer was recorded as a deposit")
	}
	chain.finalize()
	if found, _ = r.ScanDeposits(ctx); len(found) != 1 {
		t.Fatalf("after finality found %d deposits, want 1", len(found))
	}
}
