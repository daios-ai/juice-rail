package rail

import (
	"context"
	"fmt"
	"math/big"
	"testing"
)

// An intent's whole life is: record it, attempt it one or more times, and see
// what finalizes. The chain admits one transaction per nonce, so at most one
// attempt can ever win. This enumerates every shape that history can take and
// checks the reported status against it, including that a settled status never
// moves again.

func TestIntentLifecycle(t *testing.T) {
	for attempts := 1; attempts <= 3; attempts++ {
		for winner := -1; winner < attempts; winner++ {
			for _, success := range []bool{true, false} {
				for _, finalized := range []bool{true, false} {
					name := fmt.Sprintf("attempts=%d winner=%d success=%v finalized=%v",
						attempts, winner, success, finalized)
					t.Run(name, func(t *testing.T) {
						checkLifecycle(t, attempts, winner, success, finalized)
					})
				}
			}
		}
	}
}

func checkLifecycle(t *testing.T, attempts, winner int, success, finalized bool) {
	t.Helper()
	ctx := context.Background()
	r, chain, store := newTestRail(t)
	amount := big.NewInt(10_000_000)

	if err := r.Prepare(ctx, id(1), KindTransfer, bob, amount); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// Before anything is sent, the intent is pending: it may still execute.
	if status, _ := r.Status(ctx, id(1)); status != StatusPending {
		t.Fatalf("a recorded but unsent intent is %s, want pending", status)
	}

	hashes := make([]string, 0, attempts)
	if _, err := r.Send(ctx, id(1)); err != nil {
		t.Fatalf("send: %v", err)
	}
	for i := 1; i < attempts; i++ {
		if _, err := r.Retry(ctx, id(1)); err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
	}
	subs, _ := store.Submissions(r.Account(), id(1))
	if len(subs) != attempts {
		t.Fatalf("%d attempts recorded, want %d", len(subs), attempts)
	}
	// Every attempt is the same operation under the same nonce. Only the fees
	// differ, which is the whole reason a retry cannot become a second payment.
	in, _, _ := store.Intent(r.Account(), id(1))
	for _, sub := range subs {
		tx := chain.sent[sub.TxHash]
		if tx.Nonce() != in.Nonce {
			t.Fatalf("an attempt sits on nonce %d, the intent owns %d", tx.Nonce(), in.Nonce)
		}
		if string(tx.Data()) != string(in.Calldata) {
			t.Fatal("an attempt changed the operation")
		}
		hashes = append(hashes, sub.TxHash.Hex())
	}

	want := StatusPending
	if winner >= 0 {
		if success {
			chain.include(t, subs[winner].TxHash, true,
				transferLogOf(r.Domain(), r.Account(), bob, amount))
		} else {
			chain.include(t, subs[winner].TxHash, false)
		}
		if finalized {
			chain.finalize()
			want = StatusConfirmed
			if !success {
				want = StatusFailed
			}
		}
	}

	got, err := r.Status(ctx, id(1))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if got != want {
		t.Fatalf("status is %s, want %s", got, want)
	}

	// A settled status is settled: more blocks, more finality and more asking
	// change nothing.
	chain.finalize()
	for i := 0; i < 3; i++ {
		again, err := r.Status(ctx, id(1))
		if err != nil {
			t.Fatalf("status again: %v", err)
		}
		if want != StatusPending && again != want {
			t.Fatalf("a settled status moved from %s to %s", want, again)
		}
		if want == StatusPending && again != StatusPending && winner < 0 {
			t.Fatalf("an intent nothing carried became %s", again)
		}
	}
	if len(hashes) != attempts {
		t.Fatalf("%d attempt hashes, want %d", len(hashes), attempts)
	}
}
