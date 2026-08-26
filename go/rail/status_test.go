package rail

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
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

// refillReceiptLogs is what a real refill emits, taken from an Arbitrum
// Sepolia receipt: the permit's approval, the wrapped-currency legs, the pool's
// own event, and — the one that matters — the stablecoin leaving the account
// for the pool, not for the router.
func refillReceiptLogs(d Domain, account, pool common.Address, spent *big.Int) []*types.Log {
	weth := d.Venue.WETH
	bought := big.NewInt(271_584_001_500_000)
	return []*types.Log{
		{Address: d.Token, Topics: []common.Hash{
			crypto.Keccak256Hash([]byte("Approval(address,address,uint256)")),
			common.BytesToHash(account.Bytes()),
			common.BytesToHash(d.Venue.Router.Bytes()),
		}, Data: common.LeftPadBytes(big.NewInt(9_999_999).Bytes(), 32)},
		{Address: weth, Topics: []common.Hash{
			topicTransfer,
			common.BytesToHash(pool.Bytes()),
			common.BytesToHash(d.Venue.Router.Bytes()),
		}, Data: common.LeftPadBytes(bought.Bytes(), 32)},
		{Address: d.Token, Topics: []common.Hash{
			topicTransfer,
			common.BytesToHash(account.Bytes()),
			common.BytesToHash(pool.Bytes()),
		}, Data: common.LeftPadBytes(spent.Bytes(), 32)},
		{Address: pool, Topics: []common.Hash{crypto.Keccak256Hash([]byte("Swap()"))}},
		{Address: weth, Topics: []common.Hash{
			topicTransfer,
			common.BytesToHash(d.Venue.Router.Bytes()),
			common.Hash{},
		}, Data: common.LeftPadBytes(bought.Bytes(), 32)},
	}
}

var testPool = common.HexToAddress("0x00000000000000000000000000000000000000e0")

// settledRefill drives a refill through to a finalized receipt carrying spent.
func settledRefill(t *testing.T, r *Rail, chain *fakeChain, spent *big.Int) ID {
	t.Helper()
	ctx := context.Background()
	chain.gas[r.Account()] = big.NewInt(10_000_000_000_000_000)
	refill, hash, err := r.Refill(ctx, new(big.Int))
	if err != nil {
		t.Fatalf("refill: %v", err)
	}
	chain.include(t, hash, true, refillReceiptLogs(r.Domain(), r.Account(), testPool, spent)...)
	chain.finalize()
	return refill
}

func TestRefillCostIsWhatWasSpentNotWhatWasAllowed(t *testing.T) {
	ctx := context.Background()
	r, chain, store := newTestRail(t)
	spent := big.NewInt(1_219_400)
	refill := settledRefill(t, r, chain, spent)

	// The bound and the spend must differ, or the test proves nothing.
	in, _, _ := store.Intent(r.Account(), refill)
	if in.Amount.Cmp(spent) <= 0 {
		t.Fatalf("the recorded bound %s is not above the spend %s", in.Amount, spent)
	}

	got, err := r.RefillCost(ctx, refill)
	if err != nil {
		t.Fatalf("refill cost: %v", err)
	}
	if got.Cmp(spent) != 0 {
		t.Fatalf("the refill cost %s, want %s", got, spent)
	}
	if got.Cmp(in.Amount) == 0 {
		t.Fatal("it reported the ceiling instead of the cost")
	}
}

func TestRefillCostAnswersEvenIfNobodyLookedBefore(t *testing.T) {
	ctx := context.Background()
	r, chain, store := newTestRail(t)
	spent := big.NewInt(1_219_400)
	refill := settledRefill(t, r, chain, spent)

	// A fresh instance over the same records, having never asked for a status:
	// no fact is cached. That the refill finalized long ago must still be found.
	restarted, err := New(r.Domain(), store, chain, testKey(t))
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if _, cached, _ := store.Fact(r.Account(), refill); cached {
		t.Fatal("the fixture cached a fact; this test needs none")
	}
	got, err := restarted.RefillCost(ctx, refill)
	if err != nil {
		t.Fatalf("refill cost with nothing cached: %v", err)
	}
	if got.Cmp(spent) != 0 {
		t.Fatalf("the refill cost %s, want %s", got, spent)
	}
}

func TestRefillCostRefusesWhatItCannotKnow(t *testing.T) {
	ctx := context.Background()

	t.Run("still pending", func(t *testing.T) {
		r, chain, _ := newTestRail(t)
		chain.gas[r.Account()] = big.NewInt(10_000_000_000_000_000)
		refill, _, err := r.Refill(ctx, new(big.Int))
		if err != nil {
			t.Fatalf("refill: %v", err)
		}
		if _, err := r.RefillCost(ctx, refill); !errors.Is(err, ErrInFlight) {
			t.Fatalf("cost of an unfinalized refill: %v, want a refusal", err)
		}
	})

	t.Run("reverted", func(t *testing.T) {
		r, chain, _ := newTestRail(t)
		chain.gas[r.Account()] = big.NewInt(10_000_000_000_000_000)
		refill, hash, err := r.Refill(ctx, new(big.Int))
		if err != nil {
			t.Fatalf("refill: %v", err)
		}
		chain.include(t, hash, false)
		chain.finalize()
		got, err := r.RefillCost(ctx, refill)
		if err != nil || got.Sign() != 0 {
			t.Fatalf("a reverted refill cost %v (%v), want nothing", got, err)
		}
	})

	t.Run("ambiguous receipt", func(t *testing.T) {
		r, chain, _ := newTestRail(t)
		chain.gas[r.Account()] = big.NewInt(10_000_000_000_000_000)
		refill, hash, err := r.Refill(ctx, new(big.Int))
		if err != nil {
			t.Fatalf("refill: %v", err)
		}
		// Two payments from the account in one receipt: which one was the cost?
		logs := refillReceiptLogs(r.Domain(), r.Account(), testPool, big.NewInt(1_219_400))
		logs = append(logs, logs[2])
		chain.include(t, hash, true, logs...)
		chain.finalize()
		if _, err := r.RefillCost(ctx, refill); !errors.Is(err, ErrBadInput) {
			t.Fatalf("an ambiguous receipt gave %v, want a refusal rather than a guess", err)
		}
	})

	t.Run("not a refill", func(t *testing.T) {
		r, _, _ := newTestRail(t)
		if err := r.Prepare(ctx, id(1), KindTransfer, bob, big.NewInt(10_000_000)); err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if _, err := r.RefillCost(ctx, id(1)); !errors.Is(err, ErrBadInput) {
			t.Fatalf("cost of a payment: %v, want a refusal", err)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		r, _, _ := newTestRail(t)
		if _, err := r.RefillCost(ctx, id(9)); !errors.Is(err, ErrNoIntent) {
			t.Fatalf("cost of nothing: %v, want a refusal", err)
		}
	})
}

func TestFinalizedBalancesReadTheSettledBlockNotTheLatest(t *testing.T) {
	ctx := context.Background()
	r, chain, _ := newTestRail(t)
	// The two heights disagree: a payment is mined but not finalized, so the
	// latest balance is already lower.
	chain.tokenFinal = map[common.Address]*big.Int{r.Account(): big.NewInt(1_000_000_000)}
	chain.gasFinal = map[common.Address]*big.Int{r.Account(): big.NewInt(77)}
	chain.token[r.Account()] = big.NewInt(990_000_000)
	chain.gas[r.Account()] = big.NewInt(55)

	token, gas, block, err := r.FinalizedBalances(ctx)
	if err != nil {
		t.Fatalf("finalized balances: %v", err)
	}
	if token.Cmp(big.NewInt(1_000_000_000)) != 0 || gas.Cmp(big.NewInt(77)) != 0 {
		t.Fatalf("read %s and %s, which is the latest state, not the settled one", token, gas)
	}
	if block != chain.finalized {
		t.Fatalf("reports block %d, the finalized head is %d", block, chain.finalized)
	}
	// The present-moment read is unchanged and still sees the latest.
	nowToken, nowGas, err := r.Balances(ctx)
	if err != nil {
		t.Fatalf("balances: %v", err)
	}
	if nowToken.Cmp(big.NewInt(990_000_000)) != 0 || nowGas.Cmp(big.NewInt(55)) != 0 {
		t.Fatalf("Balances read %s and %s, want the latest state", nowToken, nowGas)
	}
}

func TestOutcomePlacesASettledOperation(t *testing.T) {
	ctx := context.Background()
	r, chain, _ := newTestRail(t)
	mustPrepare(t, r, id(1), bob, 10_000_000)
	hash, err := r.Send(ctx, id(1))
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	// Unknown identifier: refused, as everywhere else.
	if _, _, err := r.Outcome(ctx, id(9)); !errors.Is(err, ErrNoIntent) {
		t.Fatalf("outcome of nothing: %v, want a refusal", err)
	}
	// Pending: no fact yet, and no error.
	if _, settled, err := r.Outcome(ctx, id(1)); err != nil || settled {
		t.Fatalf("outcome while pending: settled=%v err=%v", settled, err)
	}

	chain.include(t, hash, true, transferLogOf(r.Domain(), r.Account(), bob, big.NewInt(10_000_000)))
	chain.finalize()

	f, settled, err := r.Outcome(ctx, id(1))
	if err != nil || !settled {
		t.Fatalf("outcome after finality: settled=%v err=%v", settled, err)
	}
	if f.TxHash != hash {
		t.Fatalf("the outcome names %s, the payment was %s", f.TxHash, hash)
	}
	receipt, _ := chain.TransactionReceipt(ctx, hash)
	if f.BlockNumber != receipt.BlockNumber.Uint64() {
		t.Fatalf("the outcome places it at block %d, the receipt says %d", f.BlockNumber, receipt.BlockNumber)
	}
	if !f.Executed {
		t.Fatal("a confirmed payment reported as not executed")
	}
}

func TestOutcomeOfAFailureAndOfTheUnobserved(t *testing.T) {
	ctx := context.Background()
	r, chain, store := newTestRail(t)
	mustPrepare(t, r, id(1), bob, 10_000_000)
	hash, err := r.Send(ctx, id(1))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	chain.include(t, hash, false)
	chain.finalize()

	// A fresh instance over the same records, having never asked for a status:
	// no fact is cached, and the failure must still be found and reported.
	restarted, err := New(r.Domain(), store, chain, testKey(t))
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	f, settled, err := restarted.Outcome(ctx, id(1))
	if err != nil || !settled {
		t.Fatalf("outcome of a settled failure: settled=%v err=%v", settled, err)
	}
	if f.Executed {
		t.Fatal("a reverted payment reported as executed")
	}
	if f.TxHash != hash {
		t.Fatalf("the outcome names %s, the attempt was %s", f.TxHash, hash)
	}
}

func TestDepositsScannedToReportsTheCursor(t *testing.T) {
	ctx := context.Background()
	r, chain, _ := newTestRail(t)

	if _, ok, err := r.DepositsScannedTo(); ok || err != nil {
		t.Fatalf("a cursor before any scan: ok=%v err=%v", ok, err)
	}
	if _, err := r.ScanDeposits(ctx); err != nil {
		t.Fatalf("scan: %v", err)
	}
	scanned, ok, err := r.DepositsScannedTo()
	if err != nil || !ok {
		t.Fatalf("cursor after a scan: ok=%v err=%v", ok, err)
	}
	if scanned != chain.finalized {
		t.Fatalf("scanned to %d, the finalized head is %d", scanned, chain.finalized)
	}
}
