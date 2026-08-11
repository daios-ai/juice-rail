package rail

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"reflect"
)

// fakeChain answers exactly what the rail asks a node, and records what it
// submits. Nothing here reaches a network.
type fakeChain struct {
	headTime      uint64
	headNumber    uint64
	finalizedTime uint64
	finalizedNum  uint64
	baseFee       *big.Int

	railBalances  map[common.Address]*big.Int
	tokenBalances map[common.Address]*big.Int
	bindings      map[Ref]common.Hash
	tokenDomain   common.Hash
	noAuthState   bool

	mu        sync.Mutex
	logs      []types.Log
	sent      []*types.Transaction
	simulate  error
	estimated uint64
	nonce     uint64
}

func newFakeChain() *fakeChain {
	return &fakeChain{
		headTime:      1_000_000,
		headNumber:    100,
		finalizedTime: 1_000_000,
		finalizedNum:  98,
		baseFee:       big.NewInt(1_000_000_000),
		railBalances:  map[common.Address]*big.Int{},
		tokenBalances: map[common.Address]*big.Int{},
		bindings:      map[Ref]common.Hash{},
		tokenDomain:   common.HexToHash("0xd0d0"),
		estimated:     120_000,
	}
}

func sel(sig string) string { return string(crypto.Keccak256([]byte(sig))[:4]) }

var (
	selBalanceOf   = sel("balanceOf(address)")
	selOperations  = sel("operations(address,bytes32)")
	selTokenDomain = sel("DOMAIN_SEPARATOR()")
	selAuthState   = sel("authorizationState(address,bytes32)")
)

func (f *fakeChain) HeaderByNumber(_ context.Context, number *big.Int) (*types.Header, error) {
	if number != nil && number.Cmp(finalizedBlockArg) == 0 {
		return &types.Header{Number: new(big.Int).SetUint64(f.finalizedNum), Time: f.finalizedTime}, nil
	}
	return &types.Header{Number: new(big.Int).SetUint64(f.headNumber), Time: f.headTime, BaseFee: f.baseFee}, nil
}

func (f *fakeChain) BlockNumber(context.Context) (uint64, error) { return f.headNumber, nil }

func (f *fakeChain) FilterLogs(context.Context, ethereum.FilterQuery) ([]types.Log, error) {
	return f.logs, nil
}

func (f *fakeChain) CallContract(_ context.Context, call ethereum.CallMsg, _ *big.Int) ([]byte, error) {
	if len(call.Data) < 4 {
		return nil, errors.New("short call")
	}
	switch string(call.Data[:4]) {
	case selBalanceOf:
		who := common.BytesToAddress(call.Data[4:36])
		set := f.railBalances
		if call.To != nil && *call.To == testDomain().Token {
			set = f.tokenBalances
		}
		v, ok := set[who]
		if !ok {
			v = new(big.Int)
		}
		return common.LeftPadBytes(v.Bytes(), 32), nil
	case selOperations:
		ref := Ref{
			Account: common.BytesToAddress(call.Data[4:36]),
			ID:      ID(common.BytesToHash(call.Data[36:68])),
		}
		return f.bindings[ref].Bytes(), nil
	case selTokenDomain:
		return f.tokenDomain.Bytes(), nil
	case selAuthState:
		if f.noAuthState {
			return nil, errors.New("execution reverted")
		}
		return make([]byte, 32), nil
	default:
		// A money call: this is the relayer's simulation.
		return nil, f.simulate
	}
}

// PendingNonceAt counts what this fake has already accepted, the way a node
// counts its pending pool. Two relays that select a nonce without serialising
// therefore collide, exactly as they would on a real chain.
func (f *fakeChain) PendingNonceAt(context.Context, common.Address) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nonce + uint64(len(f.sent)), nil
}

func (f *fakeChain) SuggestGasTipCap(context.Context) (*big.Int, error) { return big.NewInt(0), nil }

func (f *fakeChain) EstimateGas(context.Context, ethereum.CallMsg) (uint64, error) {
	return f.estimated, nil
}

func (f *fakeChain) SendTransaction(_ context.Context, tx *types.Transaction) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, tx)
	return nil
}

func (f *fakeChain) submitted() []*types.Transaction {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*types.Transaction(nil), f.sent...)
}

// --- fixtures ---

func testKey(t *testing.T, n byte) *ecdsa.PrivateKey {
	t.Helper()
	b := make([]byte, 32)
	b[31] = n
	key, err := crypto.ToECDSA(b)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func testDomain() Domain {
	return Domain{
		Name:     "test",
		ChainID:  big.NewInt(31337),
		Rail:     common.HexToAddress("0x00000000000000000000000000000000000000A1"),
		Token:    common.HexToAddress("0x00000000000000000000000000000000000000B2"),
		Finality: "finalized",
	}
}

func newTestRail(t *testing.T) (*Rail, *memStore, *fakeChain) {
	t.Helper()
	store, chain := newMemStore(), newFakeChain()
	r, err := New(testDomain(), store, chain, testKey(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	return r, store, chain
}

// --- construction ---

func TestNewRefusesAnIncompleteDomain(t *testing.T) {
	store, chain := newMemStore(), newFakeChain()
	key := testKey(t, 1)

	bad := testDomain()
	bad.Finality = "12-confirmations"
	if _, err := New(bad, store, chain, key); err == nil {
		t.Fatal("a confirmation-count policy must be refused: confirmed may never revert")
	}
	bad = testDomain()
	bad.Token = common.Address{}
	if _, err := New(bad, store, chain, key); err == nil {
		t.Fatal("a domain without a token must be refused")
	}
	if _, err := New(testDomain(), nil, chain, key); err == nil {
		t.Fatal("a rail without a store must be refused")
	}
}

func TestAccountIsTheKeysOwnAddress(t *testing.T) {
	r, _, _ := newTestRail(t)
	if want := crypto.PubkeyToAddress(testKey(t, 1).PublicKey); r.Account() != want {
		t.Fatalf("account %s, want %s", r.Account(), want)
	}
}

// --- intents ---

func TestPrepareRefusesAnIntentDebitingAnotherAccount(t *testing.T) {
	r, _, _ := newTestRail(t)
	err := r.Prepare(context.Background(), testID(1),
		testTerms(KindTransfer, addr(9), addr(2), 10))
	if err == nil {
		t.Fatal("a rail must not record an intent that debits someone else")
	}
}

func TestSignIsDurableBeforeItReturns(t *testing.T) {
	ctx := context.Background()
	r, store, _ := newTestRail(t)
	if err := r.PrepareTransfer(ctx, testID(1), addr(2), big.NewInt(10)); err != nil {
		t.Fatal(err)
	}
	v, err := r.Sign(ctx, testID(1), big.NewInt(1), addr(3), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := store.Variants(r.ref(testID(1)))
	if len(stored) != 1 {
		t.Fatalf("the variant must be durable before it is returned, got %d records", len(stored))
	}
	if stored[0].ValidBefore != v.ValidBefore || stored[0].Relayer != v.Relayer {
		t.Fatalf("stored %+v, returned %+v", stored[0], v)
	}
}

func TestSignReusesALiveVariantAndReplacesADeadOne(t *testing.T) {
	ctx := context.Background()
	r, store, chain := newTestRail(t)
	id := testID(1)
	if err := r.PrepareTransfer(ctx, id, addr(2), big.NewInt(10)); err != nil {
		t.Fatal(err)
	}

	first, err := r.Sign(ctx, id, big.NewInt(1), addr(3), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.Sign(ctx, id, big.NewInt(1), addr(3), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if again.ValidBefore != first.ValidBefore {
		t.Fatal("a live variant with the same relayer and fee must be reused")
	}

	// A different relayer is a different variant, signed at once: replacing an
	// unresponsive relayer never waits.
	other, err := r.Sign(ctx, id, big.NewInt(2), addr(4), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if other.Relayer != addr(4) || other.Fee.Int64() != 2 {
		t.Fatalf("wanted a fresh variant, got %+v", other)
	}
	if stored, _ := store.Variants(r.ref(id)); len(stored) != 2 {
		t.Fatalf("want 2 recorded variants, got %d", len(stored))
	}

	// Once the deadline passes, the same request signs a fresh variant.
	chain.headTime = first.ValidBefore + 1
	fresh, err := r.Sign(ctx, id, big.NewInt(1), addr(3), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ValidBefore <= first.ValidBefore {
		t.Fatal("a dead variant must not be reused")
	}
}

func TestSignRefusesWithoutAnIntentOrAfterAbandonment(t *testing.T) {
	ctx := context.Background()
	r, _, _ := newTestRail(t)
	if _, err := r.Sign(ctx, testID(7), nil, addr(3), time.Hour); !errors.Is(err, ErrNoIntent) {
		t.Fatalf("want ErrNoIntent, got %v", err)
	}
	if err := r.PrepareTransfer(ctx, testID(1), addr(2), big.NewInt(10)); err != nil {
		t.Fatal(err)
	}
	if err := r.Abandon(ctx, testID(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Sign(ctx, testID(1), nil, addr(3), time.Hour); !errors.Is(err, ErrAbandoned) {
		t.Fatalf("want ErrAbandoned, got %v", err)
	}
}

func TestSignedVariantsRecoverToTheAccount(t *testing.T) {
	ctx := context.Background()
	r, _, _ := newTestRail(t)

	for _, tc := range []struct {
		name    string
		prepare func() error
	}{
		{"transfer", func() error { return r.PrepareTransfer(ctx, testID(1), addr(2), big.NewInt(10)) }},
		{"withdraw", func() error { return r.PrepareWithdrawal(ctx, testID(2), addr(5), big.NewInt(10)) }},
		{"deposit", func() error { return r.PrepareDeposit(ctx, testID(3), addr(6), big.NewInt(10)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.prepare(); err != nil {
				t.Fatal(err)
			}
			id := testID(map[string]byte{"transfer": 1, "withdraw": 2, "deposit": 3}[tc.name])
			v, err := r.Sign(ctx, id, big.NewInt(3), addr(4), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			h, err := TermsHash(v)
			if err != nil {
				t.Fatal(err)
			}
			signer, err := RecoverSigner(digest(DomainSeparator(testDomain().ChainID, testDomain().Rail), h), v.TermsSig)
			if err != nil {
				t.Fatal(err)
			}
			if signer != r.Account() {
				t.Fatalf("terms recover to %s, want %s", signer, r.Account())
			}
			if tc.name != "deposit" {
				if len(v.AuthSig) != 0 {
					t.Fatal("only a deposit carries a token authorisation")
				}
				return
			}
			// The token authorisation is for amount + fee, payable to the rail
			// only, and its nonce is the terms hash.
			d, err := authDigest(common.HexToHash("0xd0d0"), v.Account, testDomain().Rail, v.Total(), v.ValidBefore, h)
			if err != nil {
				t.Fatal(err)
			}
			authSigner, err := RecoverSigner(d, v.AuthSig)
			if err != nil {
				t.Fatal(err)
			}
			if authSigner != r.Account() {
				t.Fatalf("authorisation recovers to %s, want %s", authSigner, r.Account())
			}
		})
	}
}

// --- relaying ---

// signedFor builds a variant signed by one rail and naming another as relayer.
func signedFor(t *testing.T, signer *Rail, kind Kind, party common.Address, amount, fee int64, relayer common.Address) Variant {
	t.Helper()
	ctx := context.Background()
	id := testID(1)
	if err := signer.Prepare(ctx, id, Terms{
		Kind: kind, Account: signer.Account(), Party: party, Amount: big.NewInt(amount),
	}); err != nil {
		t.Fatal(err)
	}
	v, err := signer.Sign(ctx, id, big.NewInt(fee), relayer, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRelayRefusesAVariantNamingAnotherRelayer(t *testing.T) {
	r, _, _ := newTestRail(t)
	v := signedFor(t, r, KindTransfer, addr(2), 10, 1, addr(9))
	if _, err := r.Relay(context.Background(), v); !errors.Is(err, ErrNotRelayer) {
		t.Fatalf("want ErrNotRelayer, got %v", err)
	}
}

func TestRelayRefusesATamperedVariant(t *testing.T) {
	r, _, _ := newTestRail(t)
	v := signedFor(t, r, KindTransfer, addr(2), 10, 1, r.Account())
	v.Amount = big.NewInt(11) // the signature no longer covers the terms
	if _, err := r.Relay(context.Background(), v); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestRelayRefusesADepositAuthorisedByAnotherAccount(t *testing.T) {
	r, _, chain := newTestRail(t)
	v := signedFor(t, r, KindDeposit, addr(2), 10, 1, r.Account())

	// A well-formed authorisation for exactly these terms, signed by someone
	// else: the tokens are not the payer's to move.
	h, err := TermsHash(v)
	if err != nil {
		t.Fatal(err)
	}
	d, err := authDigest(chain.tokenDomain, v.Account, testDomain().Rail, v.Total(), v.ValidBefore, h)
	if err != nil {
		t.Fatal(err)
	}
	if v.AuthSig, err = signDigest(testKey(t, 2), d); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Relay(context.Background(), v); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestRelayTellsExecutedApartFromConflicting(t *testing.T) {
	ctx := context.Background()
	r, _, chain := newTestRail(t)
	chain.railBalances[r.Account()] = big.NewInt(1000)
	v := signedFor(t, r, KindTransfer, addr(2), 10, 1, r.Account())
	h, err := TermsHash(v)
	if err != nil {
		t.Fatal(err)
	}

	chain.bindings[v.Ref()] = h
	if _, err := r.Relay(ctx, v); !errors.Is(err, ErrExecuted) {
		t.Fatalf("want ErrExecuted, got %v", err)
	}
	if len(chain.submitted()) != 0 {
		t.Fatal("an executed operation must cost no gas")
	}

	chain.bindings[v.Ref()] = common.HexToHash("0xbeef")
	if _, err := r.Relay(ctx, v); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	if len(chain.submitted()) != 0 {
		t.Fatal("a conflicting binding must cost no gas")
	}
}

func TestRelayRefusesAnExpiredVariant(t *testing.T) {
	ctx := context.Background()
	r, _, chain := newTestRail(t)
	chain.railBalances[r.Account()] = big.NewInt(1000)
	v := signedFor(t, r, KindTransfer, addr(2), 10, 1, r.Account())

	chain.headTime = v.ValidBefore // the contract's test is strict: >= is dead
	if _, err := r.Relay(ctx, v); !errors.Is(err, ErrExpired) {
		t.Fatalf("want ErrExpired, got %v", err)
	}
}

func TestRelayRefusesAnUnfundedOperation(t *testing.T) {
	ctx := context.Background()
	r, _, chain := newTestRail(t)
	chain.railBalances[r.Account()] = big.NewInt(10) // the fee tips it over
	v := signedFor(t, r, KindTransfer, addr(2), 10, 1, r.Account())
	if _, err := r.Relay(ctx, v); err == nil {
		t.Fatal("the balance must cover amount + fee")
	}
	if len(chain.submitted()) != 0 {
		t.Fatal("an unfunded operation must cost no gas")
	}
}

func TestRelaySimulatesBeforeSpendingGas(t *testing.T) {
	ctx := context.Background()
	r, _, chain := newTestRail(t)
	chain.railBalances[r.Account()] = big.NewInt(1000)
	chain.simulate = errors.New("execution reverted")
	v := signedFor(t, r, KindTransfer, addr(2), 10, 1, r.Account())

	if _, err := r.Relay(ctx, v); err == nil {
		t.Fatal("a failing simulation must stop the submission")
	}
	if len(chain.submitted()) != 0 {
		t.Fatal("gas was spent on an operation that cannot execute")
	}
}

func TestRelaySubmitsTheExactSignedCall(t *testing.T) {
	ctx := context.Background()
	r, _, chain := newTestRail(t)
	chain.railBalances[r.Account()] = big.NewInt(1000)
	v := signedFor(t, r, KindTransfer, addr(2), 10, 1, r.Account())

	hash, err := r.Relay(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	if len(chain.submitted()) != 1 {
		t.Fatalf("want 1 transaction, got %d", len(chain.submitted()))
	}
	tx := chain.submitted()[0]
	if tx.Hash() != hash {
		t.Fatal("the reported hash is not the transaction's")
	}
	if *tx.To() != testDomain().Rail {
		t.Fatalf("submitted to %s, want the rail", tx.To())
	}
	method, err := railABI.MethodById(tx.Data()[:4])
	if err != nil || method.Name != "transfer" {
		t.Fatalf("wrong method %v (%v)", method, err)
	}
	args, err := method.Inputs.Unpack(tx.Data()[4:])
	if err != nil {
		t.Fatal(err)
	}
	terms := reflect.ValueOf(args[0])
	field := func(name string) any { return terms.FieldByName(name).Interface() }
	if field("Account") != v.Account || field("Party") != v.Party ||
		field("Relayer") != v.Relayer ||
		field("Amount").(*big.Int).Cmp(v.Amount) != 0 ||
		field("Fee").(*big.Int).Cmp(v.Fee) != 0 ||
		field("ValidBefore").(*big.Int).Uint64() != v.ValidBefore ||
		field("Id").([32]byte) != [32]byte(v.ID) {
		t.Fatalf("the submitted terms are not the signed terms: %+v", args[0])
	}
	if got := args[1].([]byte); string(got) != string(v.TermsSig) {
		t.Fatal("the submitted signature is not the signed one")
	}
	if tx.Gas() <= chain.estimated {
		t.Fatal("the gas limit must leave room above the estimate")
	}
	if tx.GasTipCap().Sign() == 0 {
		t.Fatal("a zero tip can stall on a busy chain")
	}
}

func TestSubmitSignsAndRelaysForItself(t *testing.T) {
	ctx := context.Background()
	r, _, chain := newTestRail(t)
	chain.railBalances[r.Account()] = big.NewInt(1000)
	if err := r.PrepareTransfer(ctx, testID(1), addr(2), big.NewInt(10)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Submit(ctx, testID(1), big.NewInt(1), time.Hour); err != nil {
		t.Fatal(err)
	}
	if len(chain.submitted()) != 1 {
		t.Fatalf("want 1 transaction, got %d", len(chain.submitted()))
	}
	variants, _ := r.Variants(testID(1))
	if len(variants) != 1 || variants[0].Relayer != r.Account() {
		t.Fatalf("submit must name itself as relayer: %+v", variants)
	}
}

// The selectors are the contract's, computed from the canonical signatures.
func TestCallDataSelectorsMatchTheContract(t *testing.T) {
	const tuple = "(bytes32,address,address,uint256,uint256,address,uint256)"
	for name, signature := range map[string]string{
		"deposit":  "deposit(" + tuple + ",bytes,bytes)",
		"transfer": "transfer(" + tuple + ",bytes)",
		"withdraw": "withdraw(" + tuple + ",bytes)",
	} {
		want := crypto.Keccak256([]byte(signature))[:4]
		method, ok := railABI.Methods[name]
		if !ok {
			t.Fatalf("no method %s", name)
		}
		if string(method.ID) != string(want) {
			t.Fatalf("%s selector %x, want %x (signature %s)", name, method.ID, want, signature)
		}
	}
}

func TestAcceptRefusesAForeignDomain(t *testing.T) {
	r, _, _ := newTestRail(t)
	v := signedFor(t, r, KindTransfer, addr(2), 10, 1, r.Account())

	other := testDomain()
	other.ChainID = big.NewInt(1)
	if _, err := r.Accept(Envelope{ChainID: other.ChainID, Rail: other.Rail, Variant: v}); !errors.Is(err, ErrForeignDomain) {
		t.Fatalf("want ErrForeignDomain for another chain, got %v", err)
	}
	if _, err := r.Accept(Envelope{ChainID: testDomain().ChainID, Rail: addr(8), Variant: v}); !errors.Is(err, ErrForeignDomain) {
		t.Fatalf("want ErrForeignDomain for another deployment, got %v", err)
	}
	if _, err := r.Accept(Envelope{ChainID: testDomain().ChainID, Rail: testDomain().Rail, Variant: v}); err != nil {
		t.Fatalf("own domain refused: %v", err)
	}
}

func TestCheckTokenRequiresTheDepositSurface(t *testing.T) {
	ctx := context.Background()
	r, _, _ := newTestRail(t)
	if err := r.CheckToken(ctx); err != nil {
		t.Fatalf("a token with the EIP-3009 surface must pass: %v", err)
	}

	r2, _, chain2 := newTestRail(t)
	chain2.noAuthState = true
	if err := r2.CheckToken(ctx); err == nil {
		t.Fatal("a token without EIP-3009 must be refused: it can never take a deposit")
	}

	r3, _, chain3 := newTestRail(t)
	chain3.tokenDomain = common.Hash{}
	if err := r3.CheckToken(ctx); err == nil {
		t.Fatal("a token with an empty EIP-712 domain must be refused")
	}
}

// A host embeds this library and drives it from goroutines, so two relays on
// one rail must never choose the same transaction nonce: the loser would be
// rejected by the node or, worse, silently replace the winner.
func TestConcurrentRelaysNeverShareANonce(t *testing.T) {
	ctx := context.Background()
	r, _, chain := newTestRail(t)
	chain.railBalances[r.Account()] = big.NewInt(1_000_000)

	const operations = 12
	variants := make([]Variant, operations)
	for i := range variants {
		id := testID(byte(i + 1))
		if err := r.PrepareTransfer(ctx, id, addr(2), big.NewInt(10)); err != nil {
			t.Fatal(err)
		}
		v, err := r.Sign(ctx, id, big.NewInt(1), r.Account(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		variants[i] = v
	}

	var wg sync.WaitGroup
	errs := make([]error, operations)
	for i, v := range variants {
		wg.Add(1)
		go func(i int, v Variant) {
			defer wg.Done()
			_, errs[i] = r.Relay(ctx, v)
		}(i, v)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("relay %d: %v", i, err)
		}
	}
	sent := chain.submitted()
	if len(sent) != operations {
		t.Fatalf("%d transactions submitted, want %d", len(sent), operations)
	}
	seen := map[uint64]bool{}
	for _, tx := range sent {
		if seen[tx.Nonce()] {
			t.Fatalf("nonce %d was used twice", tx.Nonce())
		}
		seen[tx.Nonce()] = true
	}
}

// Signing concurrently on one intent must not produce two variants where one
// would do: the loser would only burn its relayer's gas.
func TestConcurrentSigningReusesOneVariant(t *testing.T) {
	ctx := context.Background()
	r, store, _ := newTestRail(t)
	id := testID(1)
	if err := r.PrepareTransfer(ctx, id, addr(2), big.NewInt(10)); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Sign(ctx, id, big.NewInt(1), addr(3), time.Hour); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	stored, err := store.Variants(r.ref(id))
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("%d variants signed for one intent, want 1", len(stored))
	}
}
