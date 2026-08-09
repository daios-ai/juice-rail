package rail

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// fakeChain is a chain whose head, finality and logs the test controls
// directly, so every finality edge is reachable without timing.
type fakeChain struct {
	head          uint64
	headTime      uint64
	finalized     uint64
	finalizedTime uint64
	logs          []types.Log
	nonce         *big.Int
	balances      map[common.Address]*big.Int
	code          map[common.Address][]byte
}

func newFakeChain() *fakeChain {
	return &fakeChain{
		head: 100, headTime: 1_000,
		finalized: 90, finalizedTime: 900,
		nonce:    new(big.Int),
		balances: map[common.Address]*big.Int{},
		code:     map[common.Address][]byte{},
	}
}

func (c *fakeChain) HeaderByNumber(_ context.Context, number *big.Int) (*types.Header, error) {
	if number != nil && number.Sign() < 0 {
		return &types.Header{Number: new(big.Int).SetUint64(c.finalized), Time: c.finalizedTime}, nil
	}
	return &types.Header{Number: new(big.Int).SetUint64(c.head), Time: c.headTime}, nil
}

func (c *fakeChain) BlockNumber(context.Context) (uint64, error) { return c.head, nil }

func (c *fakeChain) SuggestGasPrice(context.Context) (*big.Int, error) {
	return big.NewInt(1_000_000_000), nil
}

func (c *fakeChain) CodeAt(_ context.Context, addr common.Address, _ *big.Int) ([]byte, error) {
	return c.code[addr], nil
}

func (c *fakeChain) FilterLogs(_ context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	var out []types.Log
	for _, lg := range c.logs {
		if q.FromBlock != nil && lg.BlockNumber < q.FromBlock.Uint64() {
			continue
		}
		if q.ToBlock != nil && lg.BlockNumber > q.ToBlock.Uint64() {
			continue
		}
		if len(q.Addresses) > 0 && lg.Address != q.Addresses[0] {
			continue
		}
		if matchTopics(q.Topics, lg.Topics) {
			out = append(out, lg)
		}
	}
	return out, nil
}

func matchTopics(want [][]common.Hash, got []common.Hash) bool {
	for i, options := range want {
		if len(options) == 0 {
			continue
		}
		if i >= len(got) {
			return false
		}
		found := false
		for _, o := range options {
			if o == got[i] {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func selector(signature string) string {
	return string(crypto.Keccak256([]byte(signature))[:4])
}

// CallContract answers the three reads the rail makes.
func (c *fakeChain) CallContract(_ context.Context, call ethereum.CallMsg, _ *big.Int) ([]byte, error) {
	switch string(call.Data[:4]) {
	case selector("proxyCreationCode()"):
		code := []byte("proxy-creation-code")
		out := make([]byte, 0, 96)
		out = append(out, word(big.NewInt(32))...)
		out = append(out, word(big.NewInt(int64(len(code))))...)
		return append(out, common.RightPadBytes(code, 32)...), nil
	case selector("getNonce(address,uint192)"):
		return word(c.nonce), nil
	case selector("balanceOf(address)"):
		addr := common.BytesToAddress(call.Data[4:36])
		if v, ok := c.balances[addr]; ok {
			return word(v), nil
		}
		return word(new(big.Int)), nil
	default:
		return word(new(big.Int)), nil
	}
}

// railLog builds the event the contract emits for these terms.
func railLog(railAddr common.Address, id ID, t Terms, block uint64) types.Log {
	lg := types.Log{Address: railAddr, BlockNumber: block, TxHash: common.BigToHash(big.NewInt(int64(block)))}
	switch t.Kind {
	case KindDeposit:
		lg.Topics = []common.Hash{topicDeposited, id.Hash(), common.BytesToHash(t.Account.Bytes())}
		lg.Data = word(t.Amount)
	case KindSettle:
		lg.Topics = []common.Hash{topicSettled, id.Hash(),
			common.BytesToHash(t.Account.Bytes()), common.BytesToHash(t.Party.Bytes())}
		lg.Data = word(t.Amount)
	case KindWithdraw:
		lg.Topics = []common.Hash{topicWithdrawn, id.Hash(), common.BytesToHash(t.Account.Bytes())}
		lg.Data = append(word(new(big.Int).SetBytes(t.Party.Bytes())), word(t.Amount)...)
	}
	return lg
}

func TestStatusIsUnknownWithoutAnIntent(t *testing.T) {
	r, _, _ := newTestRail(t)
	got, err := r.Status(context.Background(), testID(1))
	if err != nil || got != StatusUnknown {
		t.Fatalf("status = %s, %v; want unknown", got, err)
	}
}

func TestStatusIsPendingUntilFinality(t *testing.T) {
	r, chain, _ := newTestRail(t)
	ctx := context.Background()
	id, terms := testID(2), depositTerms(100)

	if err := r.Prepare(ctx, id, terms); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Status(ctx, id); got != StatusPending {
		t.Fatalf("before execution: status = %s, want pending", got)
	}

	// Executed, but the block is not finalized yet.
	chain.logs = append(chain.logs, railLog(r.domain.Rail, id, terms, chain.head+5))
	if got, _ := r.Status(ctx, id); got != StatusPending {
		t.Fatalf("executed but unfinalized: status = %s, want pending", got)
	}

	// Finality catches up.
	chain.head += 20
	chain.finalized = chain.head
	if got, _ := r.Status(ctx, id); got != StatusConfirmed {
		t.Fatalf("finalized: status = %s, want confirmed", got)
	}
}

// The event is found by identifier, not by transaction, so an intent someone
// else's transaction executed still confirms.
func TestStatusConfirmsAnEventFromAnyTransaction(t *testing.T) {
	r, chain, _ := newTestRail(t)
	ctx := context.Background()
	id, terms := testID(3), depositTerms(50)

	if err := r.Prepare(ctx, id, terms); err != nil {
		t.Fatal(err)
	}
	lg := railLog(r.domain.Rail, id, terms, chain.head+1)
	lg.TxHash = common.HexToHash("0xfeed")
	chain.logs = append(chain.logs, lg)
	chain.finalized = chain.head + 2

	if got, _ := r.Status(ctx, id); got != StatusConfirmed {
		t.Fatalf("status = %s, want confirmed", got)
	}
	fact, ok, _ := r.store.Fact(id)
	if !ok || fact.TxHash != lg.TxHash {
		t.Fatalf("the transaction that carried it must be recorded, got %+v", fact)
	}
}

// An identifier finalized under other terms can never execute ours.
func TestStatusFailsOnForeignTerms(t *testing.T) {
	r, chain, _ := newTestRail(t)
	ctx := context.Background()
	id := testID(4)

	if err := r.Prepare(ctx, id, depositTerms(100)); err != nil {
		t.Fatal(err)
	}
	chain.logs = append(chain.logs, railLog(r.domain.Rail, id, depositTerms(101), chain.head+1))
	chain.finalized = chain.head + 2

	if got, _ := r.Status(ctx, id); got != StatusFailed {
		t.Fatalf("status = %s, want failed", got)
	}
}

// Abandonment alone is not failure: it stops future signing but kills nothing
// already signed.
func TestStatusFailsOnlyWhenAbandonedAndEveryAttemptIsDead(t *testing.T) {
	r, chain, _ := newTestRail(t)
	ctx := context.Background()
	id := testID(5)

	if err := r.Prepare(ctx, id, depositTerms(10)); err != nil {
		t.Fatal(err)
	}
	if err := r.store.AppendSignedOp(id, SignedOp{ValidUntil: chain.finalizedTime + 60}); err != nil {
		t.Fatal(err)
	}
	if err := r.Abandon(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Status(ctx, id); got != StatusPending {
		t.Fatalf("a live attempt keeps it pending, got %s", got)
	}

	// Finality moves past the attempt's expiry: it can never validate again.
	chain.finalizedTime += 120
	if got, _ := r.Status(ctx, id); got != StatusFailed {
		t.Fatalf("status = %s, want failed", got)
	}
}

func TestStatusIsPendingWhenAttemptsAreDeadButNotAbandoned(t *testing.T) {
	r, chain, _ := newTestRail(t)
	ctx := context.Background()
	id := testID(6)

	if err := r.Prepare(ctx, id, depositTerms(10)); err != nil {
		t.Fatal(err)
	}
	if err := r.store.AppendSignedOp(id, SignedOp{ValidUntil: chain.finalizedTime - 1}); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Status(ctx, id); got != StatusPending {
		t.Fatalf("a retryable intent must stay pending, got %s", got)
	}
}

func TestDecodeTermsRoundTripsEveryKind(t *testing.T) {
	railAddr := common.BigToAddress(big.NewInt(1))
	a := common.BigToAddress(big.NewInt(0xA))
	b := common.BigToAddress(big.NewInt(0xB))

	for _, want := range []Terms{
		{Kind: KindDeposit, Account: a, Amount: big.NewInt(1)},
		{Kind: KindSettle, Account: a, Party: b, Amount: big.NewInt(2)},
		{Kind: KindWithdraw, Account: a, Party: b, Amount: big.NewInt(3)},
	} {
		got, err := decodeTerms(railLog(railAddr, testID(1), want, 1))
		if err != nil {
			t.Fatalf("%s: %v", want.Kind, err)
		}
		if !got.Equal(want) {
			t.Errorf("%s: decoded %s, want %s", want.Kind, got, want)
		}
	}
}
