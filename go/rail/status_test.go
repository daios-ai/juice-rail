package rail

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// railLog builds the event the contract emits for an executed operation.
func railLog(kind Kind, ref Ref, party common.Address, amount, fee int64, relayer common.Address, validBefore uint64, block uint64) types.Log {
	topic := map[Kind]common.Hash{
		KindDeposit:  topicDeposited,
		KindTransfer: topicTransferred,
		KindWithdraw: topicWithdrawn,
	}[kind]
	data := make([]byte, 0, 160)
	data = append(data, common.LeftPadBytes(party.Bytes(), 32)...)
	data = append(data, common.LeftPadBytes(big.NewInt(amount).Bytes(), 32)...)
	data = append(data, common.LeftPadBytes(big.NewInt(fee).Bytes(), 32)...)
	data = append(data, common.LeftPadBytes(relayer.Bytes(), 32)...)
	data = append(data, common.LeftPadBytes(new(big.Int).SetUint64(validBefore).Bytes(), 32)...)
	return types.Log{
		Topics: []common.Hash{
			topic,
			common.BytesToHash(ref.Account.Bytes()),
			ref.ID.Hash(),
		},
		Data:        data,
		BlockNumber: block,
		TxHash:      common.HexToHash("0xfeed"),
	}
}

func TestClassifyIsTheWholeDecision(t *testing.T) {
	executed := &Fact{Executed: true}
	foreign := &Fact{Executed: false}
	for _, tc := range []struct {
		name      string
		known     bool
		fact      *Fact
		abandoned bool
		everyDead bool
		want      Status
	}{
		{"no record", false, nil, false, false, StatusUnknown},
		{"recorded only", true, nil, false, false, StatusPending},
		{"abandoned but a variant lives", true, nil, true, false, StatusPending},
		{"dead variants but not abandoned", true, nil, false, true, StatusPending},
		{"abandoned and every variant dead", true, nil, true, true, StatusFailed},
		{"finalized under our terms", true, executed, false, false, StatusConfirmed},
		{"finalized under other terms", true, foreign, false, false, StatusFailed},
		{"a fact outranks abandonment", true, executed, true, true, StatusConfirmed},
	} {
		if got := classify(tc.known, tc.fact, tc.abandoned, tc.everyDead); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

// The contract executes while block.timestamp < validBefore, so a head at the
// deadline is already too late.
func TestVariantDeathMatchesTheContractsTest(t *testing.T) {
	v := testVariant(KindTransfer, addr(1), addr(2), 10, 1, addr(3), 1000)
	if variantDead(v, 999) {
		t.Fatal("a variant is alive before its deadline")
	}
	if !variantDead(v, 1000) {
		t.Fatal("a variant at its deadline can never execute")
	}
	if !variantDead(v, 1001) {
		t.Fatal("a variant past its deadline can never execute")
	}
}

func TestStatusIsUnknownWithoutARecord(t *testing.T) {
	r, _, _ := newTestRail(t)
	got, err := r.Status(context.Background(), testID(1))
	if err != nil {
		t.Fatal(err)
	}
	if got != StatusUnknown {
		t.Fatalf("got %s, want unknown", got)
	}
}

func TestStatusIsPendingUntilTheEventIsFinalized(t *testing.T) {
	ctx := context.Background()
	r, _, chain := newTestRail(t)
	id := testID(1)
	if err := r.PrepareTransfer(ctx, id, addr(2), big.NewInt(10)); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Status(ctx, id); got != StatusPending {
		t.Fatalf("got %s, want pending", got)
	}

	// The rail asks for logs only up to the finalized head, so an event that
	// exists but is not final yet is simply not returned.
	chain.logs = []types.Log{railLog(KindTransfer, r.ref(id), addr(2), 10, 1, addr(3), 2_000_000, 99)}
	if got, _ := r.Status(ctx, id); got != StatusConfirmed {
		t.Fatalf("got %s, want confirmed once the event is finalized", got)
	}
}

func TestStatusConfirmsAnyVariantOfTheIntent(t *testing.T) {
	ctx := context.Background()
	r, _, chain := newTestRail(t)
	id := testID(1)
	if err := r.PrepareTransfer(ctx, id, addr(2), big.NewInt(10)); err != nil {
		t.Fatal(err)
	}
	// A different relayer, fee and deadline: still this intent.
	chain.logs = []types.Log{railLog(KindTransfer, r.ref(id), addr(2), 10, 99, addr(8), 5, 99)}
	if got, _ := r.Status(ctx, id); got != StatusConfirmed {
		t.Fatalf("got %s: a variant differing only in relayer and fee still confirms", got)
	}
}

func TestStatusFailsWhenTheIdentifierWentToOtherTerms(t *testing.T) {
	ctx := context.Background()
	r, _, chain := newTestRail(t)
	id := testID(1)
	if err := r.PrepareTransfer(ctx, id, addr(2), big.NewInt(10)); err != nil {
		t.Fatal(err)
	}
	// The same identifier, but a different recipient: this intent can never
	// execute now.
	chain.logs = []types.Log{railLog(KindTransfer, r.ref(id), addr(7), 10, 0, addr(3), 5, 99)}
	if got, _ := r.Status(ctx, id); got != StatusFailed {
		t.Fatalf("got %s, want failed", got)
	}
}

func TestStatusFailsOnlyWhenAbandonedAndEveryVariantIsDead(t *testing.T) {
	ctx := context.Background()
	r, _, chain := newTestRail(t)
	id := testID(1)
	if err := r.PrepareTransfer(ctx, id, addr(2), big.NewInt(10)); err != nil {
		t.Fatal(err)
	}
	v, err := r.Sign(ctx, id, big.NewInt(1), addr(3), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Abandon(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Status(ctx, id); got != StatusPending {
		t.Fatalf("got %s: abandonment kills nothing already signed", got)
	}

	// Death is judged against the finalized head, not the latest one: an
	// unfinalized head can still be reorganised away.
	chain.headTime = v.ValidBefore + 1
	if got, _ := r.Status(ctx, id); got != StatusPending {
		t.Fatalf("got %s: expiry is only terminal once finalized", got)
	}
	chain.finalizedTime = v.ValidBefore
	if got, _ := r.Status(ctx, id); got != StatusFailed {
		t.Fatalf("got %s, want failed", got)
	}
}

func TestStatusCachesTheFinalizedFact(t *testing.T) {
	ctx := context.Background()
	r, store, chain := newTestRail(t)
	id := testID(1)
	if err := r.PrepareTransfer(ctx, id, addr(2), big.NewInt(10)); err != nil {
		t.Fatal(err)
	}
	chain.logs = []types.Log{railLog(KindTransfer, r.ref(id), addr(2), 10, 1, addr(3), 5, 99)}
	if got, _ := r.Status(ctx, id); got != StatusConfirmed {
		t.Fatalf("got %s, want confirmed", got)
	}
	f, ok, _ := store.Fact(r.ref(id))
	if !ok || !f.Executed || f.BlockNumber != 99 {
		t.Fatalf("the finalized fact was not cached: %+v", f)
	}

	// Once cached, the chain is not consulted again: finalized facts cannot
	// change, so the cache is the answer.
	chain.logs = nil
	if got, _ := r.Status(ctx, id); got != StatusConfirmed {
		t.Fatalf("got %s: a cached fact must stand on its own", got)
	}
}

func TestDecodeTermsRejectsMalformedEvents(t *testing.T) {
	good := railLog(KindWithdraw, Ref{Account: addr(1), ID: testID(1)}, addr(2), 10, 1, addr(3), 5, 1)
	got, err := decodeTerms(good)
	if err != nil {
		t.Fatal(err)
	}
	want := testTerms(KindWithdraw, addr(1), addr(2), 10)
	if !got.Equal(want) {
		t.Fatalf("decoded %v, want %v", got, want)
	}

	short := good
	short.Data = good.Data[:128]
	if _, err := decodeTerms(short); err == nil {
		t.Fatal("a short event must be refused, not guessed at")
	}
	unknown := good
	unknown.Topics = []common.Hash{common.HexToHash("0x01"), unknown.Topics[1], unknown.Topics[2]}
	if _, err := decodeTerms(unknown); err == nil {
		t.Fatal("an unknown event must be refused")
	}
}
