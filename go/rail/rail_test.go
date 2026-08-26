package rail

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// fakeChain is a node small enough to reason about: blocks are made by the
// test, and nothing finalizes until the test says so.
type fakeChain struct {
	mu sync.Mutex

	domain     Domain
	head       uint64
	finalized  uint64
	baseFee    *big.Int
	tip        *big.Int
	gas        map[common.Address]*big.Int
	token      map[common.Address]*big.Int
	minedNonce map[common.Address]uint64
	finalNonce map[common.Address]uint64
	nextNonce  map[common.Address]uint64
	receipts   map[common.Hash]*types.Receipt
	sent       map[common.Hash]*types.Transaction
	order      []common.Hash
	logs       []types.Log
	// price is token base units per 1e18 of native currency.
	price    *big.Int
	decimals byte
	chainID  *big.Int
	// tokenFinal and gasFinal, when set, are the balances at the finalized
	// block, distinct from the latest ones above. A settled read must see
	// these, never the latest.
	tokenFinal map[common.Address]*big.Int
	gasFinal   map[common.Address]*big.Int
	sendErr    error
	callErr    error
	// receiptErr stands in for a node that cannot answer, as distinct from one
	// answering "no such transaction".
	receiptErr error
	// gasFloor is what a call really needs; a bound below it runs out, exactly
	// as the transaction would.
	gasFloor uint64
	filtered []string
	// filterErrAfter makes the nth log query onwards fail, standing in for a
	// node that gives up part way through a wide scan.
	filterErrAfter int
}

func newFakeChain(d Domain) *fakeChain {
	return &fakeChain{
		domain:     d,
		head:       10,
		finalized:  8,
		baseFee:    big.NewInt(1_000_000_000),
		tip:        big.NewInt(1),
		gas:        map[common.Address]*big.Int{},
		token:      map[common.Address]*big.Int{},
		minedNonce: map[common.Address]uint64{},
		finalNonce: map[common.Address]uint64{},
		nextNonce:  map[common.Address]uint64{},
		receipts:   map[common.Hash]*types.Receipt{},
		sent:       map[common.Hash]*types.Transaction{},
		price:      big.NewInt(3_000_000_000), // 3000 token units per unit of gas currency
		decimals:   6,
	}
}

func (c *fakeChain) ChainID(context.Context) (*big.Int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.chainID != nil {
		return c.chainID, nil
	}
	return c.domain.ChainID, nil
}

func (c *fakeChain) HeaderByNumber(_ context.Context, number *big.Int) (*types.Header, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.head
	if number != nil && number.Sign() < 0 {
		n = c.finalized
	} else if number != nil {
		n = number.Uint64()
	}
	return &types.Header{
		Number:  new(big.Int).SetUint64(n),
		Time:    1_700_000_000 + n*12,
		BaseFee: new(big.Int).Set(c.baseFee),
	}, nil
}

func (c *fakeChain) BlockNumber(context.Context) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.head, nil
}

func (c *fakeChain) BalanceAt(_ context.Context, account common.Address, block *big.Int) (*big.Int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if block != nil && block.Uint64() == c.finalized && c.gasFinal != nil {
		return orZero(c.gasFinal[account]), nil
	}
	return orZero(c.gas[account]), nil
}

func (c *fakeChain) NonceAt(_ context.Context, account common.Address, block *big.Int) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if block == nil {
		return c.nextNonce[account], nil
	}
	// A nonce is only spent, as far as anyone may rely on it, once the block
	// that spent it has finalized.
	return c.finalNonce[account], nil
}

func (c *fakeChain) PendingNonceAt(_ context.Context, account common.Address) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nextNonce[account], nil
}

func (c *fakeChain) SuggestGasTipCap(context.Context) (*big.Int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return new(big.Int).Set(c.tip), nil
}

func (c *fakeChain) SendTransaction(_ context.Context, tx *types.Transaction) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sendErr != nil {
		return c.sendErr
	}
	from, err := types.Sender(types.LatestSignerForChainID(c.domain.ChainID), tx)
	if err != nil {
		return err
	}
	if _, seen := c.sent[tx.Hash()]; seen {
		return errors.New("already known")
	}
	c.sent[tx.Hash()] = tx
	c.order = append(c.order, tx.Hash())
	if tx.Nonce() >= c.nextNonce[from] {
		c.nextNonce[from] = tx.Nonce() + 1
	}
	return nil
}

func (c *fakeChain) TransactionReceipt(_ context.Context, hash common.Hash) (*types.Receipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.receiptErr != nil {
		return nil, c.receiptErr
	}
	r, ok := c.receipts[hash]
	if !ok {
		return nil, ethereum.NotFound
	}
	return r, nil
}

func (c *fakeChain) FilterLogs(_ context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.filtered = append(c.filtered, fmt.Sprintf("%s-%s", q.FromBlock, q.ToBlock))
	if c.filterErrAfter > 0 && len(c.filtered) > c.filterErrAfter {
		return nil, errors.New("query returned more than 10000 results")
	}
	var out []types.Log
	for _, lg := range c.logs {
		if q.FromBlock != nil && lg.BlockNumber < q.FromBlock.Uint64() {
			continue
		}
		if q.ToBlock != nil && lg.BlockNumber > q.ToBlock.Uint64() {
			continue
		}
		if len(q.Topics) > 2 && len(q.Topics[2]) > 0 && lg.Topics[2] != q.Topics[2][0] {
			continue
		}
		out = append(out, lg)
	}
	return out, nil
}

// CallContract answers the reads the rail makes, decoding the real calldata so
// the encoding is under test too.
func (c *fakeChain) CallContract(_ context.Context, msg ethereum.CallMsg, block *big.Int) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.callErr != nil {
		return nil, c.callErr
	}
	if msg.To == nil || len(msg.Data) < 4 {
		return nil, errors.New("malformed call")
	}
	if msg.Gas != 0 && msg.Gas < c.gasFloor {
		return nil, errors.New("execution reverted: out of gas")
	}
	selector := string(msg.Data[:4])
	switch *msg.To {
	case c.domain.Token:
		switch selector {
		case string(tokenABI.Methods["balanceOf"].ID):
			args, err := tokenABI.Methods["balanceOf"].Inputs.Unpack(msg.Data[4:])
			if err != nil {
				return nil, err
			}
			who := args[0].(common.Address)
			if block != nil && block.Uint64() == c.finalized && c.tokenFinal != nil {
				return common.LeftPadBytes(orZero(c.tokenFinal[who]).Bytes(), 32), nil
			}
			return common.LeftPadBytes(orZero(c.token[who]).Bytes(), 32), nil
		case string(tokenABI.Methods["nonces"].ID):
			return common.LeftPadBytes(nil, 32), nil
		case string(tokenABI.Methods["decimals"].ID):
			return common.LeftPadBytes([]byte{c.decimals}, 32), nil
		case string(tokenABI.Methods["DOMAIN_SEPARATOR"].ID):
			return crypto.Keccak256([]byte("token domain")), nil
		case string(tokenABI.Methods["transfer"].ID):
			args, err := tokenABI.Methods["transfer"].Inputs.Unpack(msg.Data[4:])
			if err != nil {
				return nil, err
			}
			if orZero(c.token[msg.From]).Cmp(args[1].(*big.Int)) < 0 {
				return nil, errors.New("execution reverted: transfer amount exceeds balance")
			}
			return common.LeftPadBytes([]byte{1}, 32), nil
		}
	case c.domain.Venue.Quoter:
		if selector == string(quoterABI.Methods["quoteExactOutputSingle"].ID) {
			args, err := quoterABI.Methods["quoteExactOutputSingle"].Inputs.Unpack(msg.Data[4:])
			if err != nil {
				return nil, err
			}
			params := args[0].(struct {
				TokenIn           common.Address `json:"tokenIn"`
				TokenOut          common.Address `json:"tokenOut"`
				Amount            *big.Int       `json:"amount"`
				Fee               *big.Int       `json:"fee"`
				SqrtPriceLimitX96 *big.Int       `json:"sqrtPriceLimitX96"`
			})
			return quoterABI.Methods["quoteExactOutputSingle"].Outputs.Pack(
				c.quote(params.Amount), new(big.Int), uint32(0), new(big.Int))
		}
	case c.domain.Venue.Router:
		// A refill simulates cleanly as long as the account can pay for it.
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected call to %s", msg.To)
}

func (c *fakeChain) quote(amountOut *big.Int) *big.Int {
	q := new(big.Int).Mul(amountOut, c.price)
	q.Add(q, big.NewInt(1e18-1))
	return q.Div(q, big.NewInt(1e18))
}

// --- test-side chain control ---

// include mines the transaction into a new block.
func (c *fakeChain) include(t *testing.T, hash common.Hash, success bool, logs ...*types.Log) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	tx, ok := c.sent[hash]
	if !ok {
		t.Fatalf("no such transaction %s", hash)
	}
	from, err := types.Sender(types.LatestSignerForChainID(c.domain.ChainID), tx)
	if err != nil {
		t.Fatalf("sender: %v", err)
	}
	c.head++
	status := types.ReceiptStatusFailed
	if success {
		status = types.ReceiptStatusSuccessful
	}
	c.receipts[hash] = &types.Receipt{
		Status: status, TxHash: hash, BlockNumber: new(big.Int).SetUint64(c.head), Logs: logs,
	}
	if tx.Nonce() >= c.minedNonce[from] {
		c.minedNonce[from] = tx.Nonce() + 1
	}
}

func (c *fakeChain) finalize() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finalized = c.head
	for account, nonce := range c.minedNonce {
		c.finalNonce[account] = nonce
	}
}

// spendNonce mines a transaction this account never recorded.
func (c *fakeChain) spendNonce(account common.Address, upTo uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.head++
	c.minedNonce[account] = upTo
	if upTo > c.nextNonce[account] {
		c.nextNonce[account] = upTo
	}
}

func (c *fakeChain) addTransferLog(block uint64, index uint, token, from, to common.Address, amount *big.Int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logs = append(c.logs, types.Log{
		Address: token,
		Topics: []common.Hash{
			topicTransfer,
			common.BytesToHash(from.Bytes()),
			common.BytesToHash(to.Bytes()),
		},
		Data:        common.LeftPadBytes(amount.Bytes(), 32),
		BlockNumber: block,
		TxHash:      common.BigToHash(new(big.Int).SetUint64(block*100 + uint64(index))),
		Index:       index,
	})
}

// --- fixtures ---

var (
	bob      = common.HexToAddress("0x2222222222222222222222222222222222222222")
	exchange = common.HexToAddress("0x3333333333333333333333333333333333333333")
)

func testDomain() Domain {
	return Domain{
		Name:     "test",
		ChainID:  big.NewInt(31337),
		Token:    common.HexToAddress("0x00000000000000000000000000000000000000a0"),
		Decimals: 6,
		Finality: "finalized",
		Venue: Venue{
			Router:  common.HexToAddress("0x00000000000000000000000000000000000000b0"),
			Quoter:  common.HexToAddress("0x00000000000000000000000000000000000000c0"),
			WETH:    common.HexToAddress("0x00000000000000000000000000000000000000d0"),
			FeeTier: 500,
		},
		Gas: GasPolicy{
			Min:         big.NewInt(20_000_000_000_000_000), // 0.02
			Max:         big.NewInt(50_000_000_000_000_000), // 0.05
			SlippageBps: 50,
			FeeBound:    big.NewInt(10_000_000_000_000_000), // 0.01
			PaymentGas:  120_000,
			SwapGas:     400_000,
		},
	}
}

func newTestRail(t *testing.T) (*Rail, *fakeChain, *memStore) {
	t.Helper()
	d := testDomain()
	chain := newFakeChain(d)
	store := newMemStore()
	key, err := crypto.HexToECDSA("4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	r, err := New(d, store, chain, key)
	if err != nil {
		t.Fatalf("new rail: %v", err)
	}
	// A funded, operational account: money and a full reserve.
	chain.token[r.Account()] = big.NewInt(1_000_000_000) // 1000 units
	chain.gas[r.Account()] = big.NewInt(100_000_000_000_000_000)
	return r, chain, store
}

func id(n byte) ID { return ID{n} }

func mustPrepare(t *testing.T, r *Rail, i ID, to common.Address, amount int64) {
	t.Helper()
	if err := r.Prepare(context.Background(), i, KindTransfer, to, big.NewInt(amount)); err != nil {
		t.Fatalf("prepare: %v", err)
	}
}

func transferLogOf(d Domain, from, to common.Address, amount *big.Int) *types.Log {
	return &types.Log{
		Address: d.Token,
		Topics: []common.Hash{
			topicTransfer,
			common.BytesToHash(from.Bytes()),
			common.BytesToHash(to.Bytes()),
		},
		Data: common.LeftPadBytes(amount.Bytes(), 32),
	}
}

// --- tests ---

func TestPreparingTwiceRecordsOneOperation(t *testing.T) {
	ctx := context.Background()
	r, chain, store := newTestRail(t)
	mustPrepare(t, r, id(1), bob, 10_000_000)
	mustPrepare(t, r, id(1), bob, 10_000_000)

	if _, ok, _ := store.IntentByNonce(r.Account(), 1); ok {
		t.Fatal("a repeated command took a second nonce")
	}
	// Sending twice reproduces the same transaction rather than making another.
	first, err := r.Send(ctx, id(1))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	second, err := r.Send(ctx, id(1))
	if err != nil {
		t.Fatalf("send again: %v", err)
	}
	if first != second {
		t.Fatalf("resending produced %s then %s", first, second)
	}
	subs, _ := store.Submissions(r.Account(), id(1))
	if len(subs) != 1 {
		t.Fatalf("%d attempts recorded, want 1", len(subs))
	}
	if len(chain.order) != 1 {
		t.Fatalf("%d transactions broadcast, want 1", len(chain.order))
	}
}

func TestDifferentTermsUnderOneIdentifierAreRefused(t *testing.T) {
	ctx := context.Background()
	r, _, _ := newTestRail(t)
	mustPrepare(t, r, id(1), bob, 10_000_000)
	err := r.Prepare(ctx, id(1), KindTransfer, exchange, big.NewInt(10_000_000))
	if !errors.Is(err, ErrIntentConflict) {
		t.Fatalf("reused identifier: %v, want a conflict", err)
	}
}

func TestOneOperationAtATime(t *testing.T) {
	ctx := context.Background()
	r, _, _ := newTestRail(t)
	mustPrepare(t, r, id(1), bob, 10_000_000)
	if _, err := r.Send(ctx, id(1)); err != nil {
		t.Fatalf("send: %v", err)
	}
	err := r.Prepare(ctx, id(2), KindTransfer, bob, big.NewInt(1_000_000))
	if !errors.Is(err, ErrInFlight) {
		t.Fatalf("second operation while one is unresolved: %v, want in flight", err)
	}
}

func TestShortOfMoneySignsNothing(t *testing.T) {
	ctx := context.Background()
	r, chain, store := newTestRail(t)
	chain.token[r.Account()] = big.NewInt(5)

	err := r.Prepare(ctx, id(1), KindTransfer, bob, big.NewInt(10_000_000))
	if !errors.Is(err, ErrInsufficientStablecoin) {
		t.Fatalf("paying more than it holds: %v, want a shortage", err)
	}
	if _, ok, _ := store.Intent(r.Account(), id(1)); ok {
		t.Fatal("a blocked payment left a record behind")
	}
	if len(chain.order) != 0 {
		t.Fatal("a blocked payment broadcast something")
	}
}

func TestLowReserveAsksForARefillAndRecordsNothing(t *testing.T) {
	ctx := context.Background()
	r, chain, store := newTestRail(t)
	chain.gas[r.Account()] = big.NewInt(10_000_000_000_000_000) // below the minimum

	err := r.Prepare(ctx, id(1), KindTransfer, bob, big.NewInt(10_000_000))
	if !errors.Is(err, ErrNeedRefill) {
		t.Fatalf("payment on a low reserve: %v, want a refill first", err)
	}
	if _, ok, _ := store.Intent(r.Account(), id(1)); ok {
		t.Fatal("a payment needing a refill left a record behind")
	}
}

func TestRefillBuysGasWithTheAccountsOwnMoney(t *testing.T) {
	ctx := context.Background()
	r, chain, store := newTestRail(t)
	chain.gas[r.Account()] = big.NewInt(10_000_000_000_000_000)

	refill, hash, err := r.Refill(ctx, big.NewInt(10_000_000))
	if err != nil {
		t.Fatalf("refill: %v", err)
	}
	in, ok, _ := store.Intent(r.Account(), refill)
	if !ok {
		t.Fatal("the refill was not recorded before it was sent")
	}
	if in.Kind != KindRefill || in.To != r.Domain().Venue.Router {
		t.Fatalf("refill went to %s as %s", in.To, in.Kind)
	}
	// It buys back up to the maximum, counting its own gas as spent.
	if in.Delta.Sign() <= 0 || in.Delta.Cmp(r.Domain().Gas.Max) > 0 {
		t.Fatalf("refill buys %s, outside (0, %s]", in.Delta, r.Domain().Gas.Max)
	}
	// The input bound is the quote plus the slippage margin, and no more.
	quote := chain.quote(in.Delta)
	if want := withSlippage(quote, r.Domain().Gas.SlippageBps); in.Amount.Cmp(want) != 0 {
		t.Fatalf("input bound is %s, want %s", in.Amount, want)
	}
	if hash == (common.Hash{}) {
		t.Fatal("the refill was not broadcast")
	}
	// A refill takes the caller's nonce but never the caller's identifier.
	if refill == id(1) {
		t.Fatal("the refill claimed a caller identifier")
	}
}

func TestRefillIsRefusedWhenTheReserveIsFull(t *testing.T) {
	r, _, _ := newTestRail(t)
	if _, _, err := r.Refill(context.Background(), big.NewInt(0)); !errors.Is(err, ErrNoRefillNeeded) {
		t.Fatalf("refill on a full reserve: %v, want a refusal", err)
	}
}

func TestRefillWillNotSpendThePaymentItEnables(t *testing.T) {
	ctx := context.Background()
	r, chain, store := newTestRail(t)
	chain.gas[r.Account()] = big.NewInt(10_000_000_000_000_000)
	// Enough for the payment, not enough for the payment and the gas it needs.
	chain.token[r.Account()] = big.NewInt(60_000_000)

	_, _, err := r.Refill(ctx, big.NewInt(59_000_000))
	if !errors.Is(err, ErrInsufficientStablecoin) {
		t.Fatalf("refill that would eat the payment: %v, want a shortage", err)
	}
	if pending, _ := store.Pending(r.Account()); len(pending) != 0 {
		t.Fatal("a blocked refill left a record behind")
	}
	if len(chain.order) != 0 {
		t.Fatal("a blocked refill broadcast something")
	}
}

func TestRefillWaitsWhenGasCostsMoreThanTheBound(t *testing.T) {
	ctx := context.Background()
	r, chain, _ := newTestRail(t)
	chain.gas[r.Account()] = big.NewInt(10_000_000_000_000_000)
	chain.baseFee = big.NewInt(1_000_000_000_000) // a very expensive chain

	_, _, err := r.Refill(ctx, big.NewInt(0))
	if !errors.Is(err, ErrFeesAboveBound) {
		t.Fatalf("refill above the fee bound: %v, want a wait", err)
	}
}

func TestReserveTooLowToRefillIsReportedNotHidden(t *testing.T) {
	ctx := context.Background()
	r, chain, _ := newTestRail(t)
	chain.gas[r.Account()] = big.NewInt(1) // cannot pay for anything

	_, _, err := r.Refill(ctx, big.NewInt(0))
	if !errors.Is(err, ErrInsufficientNative) {
		t.Fatalf("refill with no gas at all: %v, want an external top-up", err)
	}
}

func TestRetryKeepsTheNonceAndRaisesOnlyFees(t *testing.T) {
	ctx := context.Background()
	r, chain, store := newTestRail(t)
	mustPrepare(t, r, id(1), bob, 10_000_000)
	if _, err := r.Send(ctx, id(1)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := r.Retry(ctx, id(1)); err != nil {
		t.Fatalf("retry: %v", err)
	}
	subs, _ := store.Submissions(r.Account(), id(1))
	if len(subs) != 2 {
		t.Fatalf("%d attempts recorded, want 2", len(subs))
	}
	if subs[1].FeeCap.Cmp(subs[0].FeeCap) <= 0 || subs[1].Tip.Cmp(subs[0].Tip) <= 0 {
		t.Fatalf("a retry did not raise its fees: %s then %s", subs[0].FeeCap, subs[1].FeeCap)
	}
	first, second := chain.sent[subs[0].TxHash], chain.sent[subs[1].TxHash]
	if first.Nonce() != second.Nonce() {
		t.Fatalf("a retry moved to nonce %d from %d", second.Nonce(), first.Nonce())
	}
	if string(first.Data()) != string(second.Data()) || *first.To() != *second.To() {
		t.Fatal("a retry changed the operation, not only its fees")
	}
}

func TestRestartResumesAnUnsentIntent(t *testing.T) {
	ctx := context.Background()
	r, chain, store := newTestRail(t)
	mustPrepare(t, r, id(1), bob, 10_000_000)

	// A crash between recording and sending: the intent exists, nothing was
	// broadcast. A fresh instance over the same records finishes the job.
	restarted, err := New(r.Domain(), store, chain, testKey(t))
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	hash, err := restarted.Send(ctx, id(1))
	if err != nil {
		t.Fatalf("send after restart: %v", err)
	}
	in, _, _ := store.Intent(r.Account(), id(1))
	if chain.sent[hash].Nonce() != in.Nonce {
		t.Fatalf("resumed at nonce %d, recorded %d", chain.sent[hash].Nonce(), in.Nonce)
	}
	if len(chain.order) != 1 {
		t.Fatalf("%d transactions broadcast, want 1", len(chain.order))
	}
}

func TestBalancesReportMoneyAndReserveSeparately(t *testing.T) {
	r, chain, _ := newTestRail(t)
	token, gas, err := r.Balances(context.Background())
	if err != nil {
		t.Fatalf("balances: %v", err)
	}
	if token.Cmp(chain.token[r.Account()]) != 0 || gas.Cmp(chain.gas[r.Account()]) != 0 {
		t.Fatalf("balances are %s and %s", token, gas)
	}
}

func TestNewRefusesAnIncompleteDomain(t *testing.T) {
	store, chain := newMemStore(), newFakeChain(testDomain())
	for _, tc := range []struct {
		name  string
		spoil func(*Domain)
	}{
		{"no finality", func(d *Domain) { d.Finality = "12 confirmations" }},
		{"no token", func(d *Domain) { d.Token = common.Address{} }},
		{"no venue", func(d *Domain) { d.Venue.Router = common.Address{} }},
		{"reserve below its own fee bound", func(d *Domain) { d.Gas.Min = big.NewInt(1) }},
		{"maximum below minimum", func(d *Domain) { d.Gas.Max = big.NewInt(1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDomain()
			tc.spoil(&d)
			if _, err := New(d, store, chain, testKey(t)); err == nil {
				t.Fatal("an unsafe domain was accepted")
			}
		})
	}
}

func testKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := crypto.HexToECDSA("4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	return key
}

func TestANodeThatAlreadyHasTheWorkIsNotAFailure(t *testing.T) {
	for _, tc := range []struct {
		message string
		benign  bool
	}{
		{"already known", true},
		{"known transaction: 0xabc", true},
		{"nonce too low", true},
		{"replacement transaction underpriced", false},
		{"insufficient funds for gas * price + value", false},
	} {
		if got := alreadySettled(errors.New(tc.message)); got != tc.benign {
			t.Fatalf("%q treated as benign=%v", tc.message, got)
		}
	}
}

func TestSendingAfterSettlementDoesNothing(t *testing.T) {
	ctx := context.Background()
	r, chain, store := newTestRail(t)
	mustPrepare(t, r, id(1), bob, 10_000_000)
	hash, err := r.Send(ctx, id(1))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	chain.include(t, hash, true, transferLogOf(r.Domain(), r.Account(), bob, big.NewInt(10_000_000)))
	chain.finalize()
	if status, _ := r.Status(ctx, id(1)); status != StatusConfirmed {
		t.Fatal("the payment did not confirm")
	}

	// Sending or retrying a settled operation is a no-op, reported as a zero
	// hash rather than a second transaction.
	for _, again := range []func() (common.Hash, error){
		func() (common.Hash, error) { return r.Send(ctx, id(1)) },
		func() (common.Hash, error) { return r.Retry(ctx, id(1)) },
	} {
		got, err := again()
		if err != nil {
			t.Fatalf("after settlement: %v", err)
		}
		if got != (common.Hash{}) {
			t.Fatalf("a settled operation was sent again as %s", got)
		}
	}
	subs, _ := store.Submissions(r.Account(), id(1))
	if len(subs) != 1 {
		t.Fatalf("%d attempts recorded, want 1", len(subs))
	}
	if len(chain.order) != 1 {
		t.Fatalf("%d transactions broadcast, want 1", len(chain.order))
	}
}

func TestTheZeroIdentifierIsAnOrdinaryIdentifier(t *testing.T) {
	ctx := context.Background()
	r, chain, store := newTestRail(t)
	var zero ID

	// Nothing recorded under it yet.
	if _, err := r.Send(ctx, zero); !errors.Is(err, ErrNoIntent) {
		t.Fatalf("sending an unrecorded operation: %v, want no intent", err)
	}
	if err := r.Prepare(ctx, zero, KindTransfer, bob, big.NewInt(10_000_000)); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	hash, err := r.Send(ctx, zero)
	if err != nil || hash == (common.Hash{}) {
		t.Fatalf("send: %s %v", hash, err)
	}
	chain.include(t, hash, true, transferLogOf(r.Domain(), r.Account(), bob, big.NewInt(10_000_000)))
	chain.finalize()
	if status, _ := r.Status(ctx, zero); status != StatusConfirmed {
		t.Fatalf("status is %s, want confirmed", status)
	}
	// Settled: nothing more goes out, and no attempt is invented.
	if again, err := r.Send(ctx, zero); err != nil || again != (common.Hash{}) {
		t.Fatalf("resending a settled operation: %s %v", again, err)
	}
	subs, _ := store.Submissions(r.Account(), zero)
	if len(subs) != 1 || len(chain.order) != 1 {
		t.Fatalf("%d attempts and %d transactions, want 1 and 1", len(subs), len(chain.order))
	}
}

func TestANodeThatCannotAnswerIsNotAFinalizedFailure(t *testing.T) {
	ctx := context.Background()
	r, chain, store := newTestRail(t)
	mustPrepare(t, r, id(1), bob, 10_000_000)
	hash, err := r.Send(ctx, id(1))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	chain.include(t, hash, true, transferLogOf(r.Domain(), r.Account(), bob, big.NewInt(10_000_000)))
	chain.finalize()

	// The nonce is spent, but the node cannot say what spent it.
	chain.receiptErr = errors.New("context deadline exceeded")
	if _, err := r.Status(ctx, id(1)); err == nil {
		t.Fatal("an unanswerable node produced a status instead of an error")
	}
	if _, cached, _ := store.Fact(r.Account(), id(1)); cached {
		t.Fatal("a permanent fact was written from an unanswered question")
	}

	// Once it can answer, the truth is the truth.
	chain.receiptErr = nil
	if status, err := r.Status(ctx, id(1)); err != nil || status != StatusConfirmed {
		t.Fatalf("status is %s (%v), want confirmed", status, err)
	}
}

func TestAGasBoundTooSmallIsCaughtBeforeAnythingIsRecorded(t *testing.T) {
	ctx := context.Background()
	r, chain, store := newTestRail(t)
	// The chain needs more gas for a transfer than the policy allows for one.
	chain.gasFloor = 200_000

	err := r.Prepare(ctx, id(1), KindTransfer, bob, big.NewInt(10_000_000))
	if err == nil {
		t.Fatal("a gas bound below what the call needs was accepted")
	}
	if !strings.Contains(err.Error(), "gas bound") {
		t.Fatalf("refused with %v, which does not name the bound", err)
	}
	if _, ok, _ := store.Intent(r.Account(), id(1)); ok {
		t.Fatal("an operation that cannot run took a nonce")
	}
	if len(chain.order) != 0 {
		t.Fatal("an operation that cannot run was broadcast")
	}
}

func TestDepositScanningWalksTheChainInSpans(t *testing.T) {
	ctx := context.Background()
	r, chain, store := newTestRail(t)
	d := r.Domain()

	// A range far wider than one query may cover, with money at both ends.
	chain.head, chain.finalized = 25_000, 25_000
	chain.addTransferLog(1, 0, d.Token, bob, r.Account(), big.NewInt(1_000_000))
	chain.addTransferLog(24_999, 0, d.Token, exchange, r.Account(), big.NewInt(2_000_000))

	found, err := r.ScanDeposits(ctx)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("found %d deposits across the range, want 2", len(found))
	}
	if len(chain.filtered) < 3 {
		t.Fatalf("scanned 25000 blocks in %d queries; the span bound is %d", len(chain.filtered), maxScanSpan)
	}
	if cursor, ok, _ := store.Cursor(r.Account()); !ok || cursor != 25_000 {
		t.Fatalf("cursor ended at %d, want 25000", cursor)
	}
	// And nothing is found twice.
	if again, _ := r.ScanDeposits(ctx); len(again) != 0 {
		t.Fatalf("rescanning found %d deposits", len(again))
	}
}

func TestAnInterruptedScanResumesWhereItStopped(t *testing.T) {
	ctx := context.Background()
	r, chain, store := newTestRail(t)
	d := r.Domain()
	chain.head, chain.finalized = 25_000, 25_000
	chain.addTransferLog(5, 0, d.Token, bob, r.Account(), big.NewInt(1_000_000))

	// Fail after the first span has been recorded.
	chain.filterErrAfter = 1
	if _, err := r.ScanDeposits(ctx); err == nil {
		t.Fatal("a failing scan reported success")
	}
	cursor, ok, _ := store.Cursor(r.Account())
	if !ok || cursor != maxScanSpan-1 {
		t.Fatalf("cursor is %d after one span, want %d", cursor, maxScanSpan-1)
	}
	// The money already seen is not lost, and is not recorded twice later.
	if all, _ := r.Deposits(); len(all) != 1 {
		t.Fatalf("%d deposits recorded before the failure, want 1", len(all))
	}
	chain.filterErrAfter = 0
	if _, err := r.ScanDeposits(ctx); err != nil {
		t.Fatalf("resumed scan: %v", err)
	}
	if all, _ := r.Deposits(); len(all) != 1 {
		t.Fatalf("%d deposits after resuming, want 1", len(all))
	}
}

func TestARefillGoesWhereItWasRecordedToGo(t *testing.T) {
	ctx := context.Background()
	r, chain, store := newTestRail(t)
	chain.gas[r.Account()] = big.NewInt(10_000_000_000_000_000)

	refill, _, err := r.Refill(ctx, big.NewInt(0))
	if err != nil {
		t.Fatalf("refill: %v", err)
	}
	recorded, _, _ := store.Intent(r.Account(), refill)

	// Configuration moves to a different venue while the refill is unresolved.
	// The permit inside the recorded calldata names the old one as spender, so
	// the transaction must still go there.
	moved := r.Domain()
	moved.Venue.Router = common.HexToAddress("0x00000000000000000000000000000000000000ff")
	after, err := New(moved, store, chain, testKey(t))
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if got := after.target(recorded); got != recorded.To {
		t.Fatalf("a recorded refill would be sent to %s, not the venue %s it was signed for", got, recorded.To)
	}
}

func TestAPaymentMayLeaveTheReserveUnderMinimumAndTheNextOneRefills(t *testing.T) {
	ctx := context.Background()
	r, chain, _ := newTestRail(t)
	min := r.Domain().Gas.Min

	// Just above the reorder point: the payment goes, whatever it costs.
	chain.gas[r.Account()] = new(big.Int).Add(min, big.NewInt(1))
	if err := r.Prepare(ctx, id(1), KindTransfer, bob, big.NewInt(1_000_000)); err != nil {
		t.Fatalf("a reserve above the minimum refused a payment: %v", err)
	}
	hash, err := r.Send(ctx, id(1))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	chain.include(t, hash, true, transferLogOf(r.Domain(), r.Account(), bob, big.NewInt(1_000_000)))
	chain.finalize()

	// Paying for it dropped the reserve under the minimum. That is the reorder
	// point doing its job, not a fault.
	chain.gas[r.Account()] = new(big.Int).Sub(min, big.NewInt(1))
	if status, _ := r.Status(ctx, id(1)); status != StatusConfirmed {
		t.Fatalf("the payment is %s, want confirmed", status)
	}

	// The next one refills first.
	err = r.Prepare(ctx, id(2), KindTransfer, bob, big.NewInt(1_000_000))
	if !errors.Is(err, ErrNeedRefill) {
		t.Fatalf("below the minimum the next payment says %v, want a refill first", err)
	}
	refill, _, err := r.Refill(ctx, big.NewInt(1_000_000))
	if err != nil {
		t.Fatalf("refill: %v", err)
	}
	// It buys back to the maximum, counting its own gas as spent.
	in, _, _ := r.Intent(refill)
	landing := new(big.Int).Add(chain.gas[r.Account()], in.Delta)
	if landing.Cmp(r.Domain().Gas.Max) < 0 {
		t.Fatalf("a refill of %s lands the reserve at %s, short of the maximum %s",
			in.Delta, landing, r.Domain().Gas.Max)
	}
}

func TestPayIsTheWholeFlowAnAppWouldOtherwiseWrite(t *testing.T) {
	ctx := context.Background()
	r, chain, store := newTestRail(t)

	// Reserve is fine: the payment goes out, and nothing was refilled.
	out, err := r.Pay(ctx, id(1), KindTransfer, bob, big.NewInt(10_000_000))
	if err != nil {
		t.Fatalf("pay: %v", err)
	}
	if out.Refilled() || out.TxHash == (common.Hash{}) {
		t.Fatalf("a funded account paid as %+v", out)
	}
	chain.include(t, out.TxHash, true, transferLogOf(r.Domain(), r.Account(), bob, big.NewInt(10_000_000)))
	chain.finalize()

	// Reserve below the minimum: gas is bought instead, under an identifier of
	// its own, and the payment is left for next time.
	chain.gas[r.Account()] = big.NewInt(10_000_000_000_000_000)
	out, err = r.Pay(ctx, id(2), KindTransfer, bob, big.NewInt(10_000_000))
	if err != nil {
		t.Fatalf("pay on a low reserve: %v", err)
	}
	if !out.Refilled() || out.TxHash != (common.Hash{}) {
		t.Fatalf("a low reserve paid as %+v", out)
	}
	if out.RefillID == id(2) {
		t.Fatal("the refill took the caller's identifier")
	}
	if _, ok, _ := store.Intent(r.Account(), id(2)); ok {
		t.Fatal("a payment that never ran left a record behind")
	}

	// Once the refill has finalized, the same call pays.
	chain.include(t, out.RefillTx, true)
	chain.finalize()
	chain.gas[r.Account()] = big.NewInt(100_000_000_000_000_000)
	again, err := r.Pay(ctx, id(2), KindTransfer, bob, big.NewInt(10_000_000))
	if err != nil {
		t.Fatalf("pay after the refill: %v", err)
	}
	if again.Refilled() || again.TxHash == (common.Hash{}) {
		t.Fatalf("after refilling, paying gave %+v", again)
	}
}

func TestPayReportsAShortageInUnitsAPersonReads(t *testing.T) {
	ctx := context.Background()
	r, chain, _ := newTestRail(t)
	chain.gas[r.Account()] = big.NewInt(1) // cannot even pay for a refill

	_, err := r.Pay(ctx, id(1), KindTransfer, bob, big.NewInt(10_000_000))
	if !errors.Is(err, ErrInsufficientNative) {
		t.Fatalf("pay with no gas: %v", err)
	}
	// Every amount is written the way a person writes one. A long run of
	// digits that is not the tail of a decimal is wei leaking out.
	if m := regexp.MustCompile(`(^|[^.0-9])([0-9]{12,})`).FindString(err.Error()); m != "" {
		t.Fatalf("the message shows raw base units %q: %s", strings.TrimSpace(m), err)
	}
	if !strings.Contains(err.Error(), r.Account().Hex()) {
		t.Fatalf("the message does not say where to send: %s", err)
	}
}

func TestTheUnitsAreCheckedAgainstTheToken(t *testing.T) {
	ctx := context.Background()
	r, chain, _ := newTestRail(t)
	if err := r.CheckDomain(ctx); err != nil {
		t.Fatalf("a correctly configured domain was refused: %v", err)
	}
	// A token that counts in eighteen decimals while the domain says six would
	// misstate every amount by a factor of a trillion.
	chain.decimals = 18
	if err := r.CheckDomain(ctx); err == nil {
		t.Fatal("a domain that disagrees with its token about units was accepted")
	}
}

func TestARailWillNotActOnTheWrongChain(t *testing.T) {
	ctx := context.Background()
	r, chain, store := newTestRail(t)
	// The endpoint is for some other chain. Records are bound to a chain id,
	// and money sent on the wrong one is gone.
	chain.chainID = big.NewInt(1)

	if err := r.CheckDomain(ctx); !errors.Is(err, ErrWrongDomain) {
		t.Fatalf("checking a domain against the wrong endpoint: %v", err)
	}
	err := r.Prepare(ctx, id(1), KindTransfer, bob, big.NewInt(10_000_000))
	if !errors.Is(err, ErrWrongDomain) {
		t.Fatalf("paying through the wrong endpoint: %v, want a refusal", err)
	}
	if _, ok, _ := store.Intent(r.Account(), id(1)); ok {
		t.Fatal("an operation on the wrong chain was recorded")
	}
	if len(chain.order) != 0 {
		t.Fatal("an operation on the wrong chain was broadcast")
	}
}

func TestWithdrawAllSendsEverythingAndDoesNotBuyGasToLeave(t *testing.T) {
	ctx := context.Background()
	r, chain, store := newTestRail(t)
	// Below the reserve minimum, where an ordinary payment would refill first.
	chain.gas[r.Account()] = big.NewInt(10_000_000_000_000_000)
	if err := r.Prepare(ctx, id(9), KindTransfer, bob, big.NewInt(1_000_000)); !errors.Is(err, ErrNeedRefill) {
		t.Fatalf("a payment on this reserve says %v, want a refill first", err)
	}

	whole := new(big.Int).Set(chain.token[r.Account()])
	hash, err := r.WithdrawAll(ctx, id(1), exchange)
	if err != nil {
		t.Fatalf("withdraw all: %v", err)
	}
	if hash == (common.Hash{}) {
		t.Fatal("nothing was sent")
	}
	// It bought no gas on the way out, and it sent the lot.
	if len(chain.order) != 1 {
		t.Fatalf("%d transactions broadcast, want the withdrawal alone", len(chain.order))
	}
	in, _, _ := store.Intent(r.Account(), id(1))
	if in.Kind != KindWithdraw || in.To != exchange || in.Amount.Cmp(whole) != 0 {
		t.Fatalf("it recorded %+v, want the whole balance to the destination", in)
	}
	chain.include(t, hash, true, transferLogOf(r.Domain(), r.Account(), exchange, whole))
	chain.finalize()
	if status, _ := r.Status(ctx, id(1)); status != StatusConfirmed {
		t.Fatalf("leaving is %s, want confirmed", status)
	}
}

func TestWithdrawAllWillNotSignATransferItCannotPayFor(t *testing.T) {
	ctx := context.Background()
	r, chain, store := newTestRail(t)
	chain.gas[r.Account()] = big.NewInt(1)

	_, err := r.WithdrawAll(ctx, id(1), exchange)
	if !errors.Is(err, ErrInsufficientNative) {
		t.Fatalf("leaving with no gas: %v", err)
	}
	if _, ok, _ := store.Intent(r.Account(), id(1)); ok {
		t.Fatal("an unpayable transfer was recorded")
	}
	if len(chain.order) != 0 {
		t.Fatal("an unpayable transfer was broadcast")
	}
}

func TestWithdrawAllRefusesAnEmptyAccount(t *testing.T) {
	ctx := context.Background()
	r, chain, _ := newTestRail(t)
	chain.token[r.Account()] = new(big.Int)

	if _, err := r.WithdrawAll(ctx, id(1), exchange); !errors.Is(err, ErrBadInput) {
		t.Fatalf("leaving an account with no money: %v, want a refusal", err)
	}
}
