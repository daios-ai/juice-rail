package rail

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	// The three money calls, plus the two reads a relayer needs. The parameter
	// names are ours; the selector depends only on the types, which are the
	// contract's.
	railABIJSON = `[
      {"type":"function","name":"deposit","inputs":[
        {"name":"t","type":"tuple","components":[
          {"name":"id","type":"bytes32"},{"name":"account","type":"address"},{"name":"party","type":"address"},
          {"name":"amount","type":"uint256"},{"name":"fee","type":"uint256"},{"name":"relayer","type":"address"},
          {"name":"validBefore","type":"uint256"}]},
        {"name":"termsSig","type":"bytes"},{"name":"authSig","type":"bytes"}]},
      {"type":"function","name":"transfer","inputs":[
        {"name":"t","type":"tuple","components":[
          {"name":"id","type":"bytes32"},{"name":"account","type":"address"},{"name":"party","type":"address"},
          {"name":"amount","type":"uint256"},{"name":"fee","type":"uint256"},{"name":"relayer","type":"address"},
          {"name":"validBefore","type":"uint256"}]},
        {"name":"sig","type":"bytes"}]},
      {"type":"function","name":"withdraw","inputs":[
        {"name":"t","type":"tuple","components":[
          {"name":"id","type":"bytes32"},{"name":"account","type":"address"},{"name":"party","type":"address"},
          {"name":"amount","type":"uint256"},{"name":"fee","type":"uint256"},{"name":"relayer","type":"address"},
          {"name":"validBefore","type":"uint256"}]},
        {"name":"sig","type":"bytes"}]},
      {"type":"function","name":"balanceOf","inputs":[{"name":"account","type":"address"}],
        "outputs":[{"type":"uint256"}],"stateMutability":"view"},
      {"type":"function","name":"operations","inputs":[
        {"name":"account","type":"address"},{"name":"id","type":"bytes32"}],
        "outputs":[{"type":"bytes32"}],"stateMutability":"view"}]`

	tokenABIJSON = `[
      {"type":"function","name":"balanceOf","inputs":[{"name":"account","type":"address"}],
        "outputs":[{"type":"uint256"}],"stateMutability":"view"},
      {"type":"function","name":"DOMAIN_SEPARATOR","inputs":[],
        "outputs":[{"type":"bytes32"}],"stateMutability":"view"},
      {"type":"function","name":"authorizationState","inputs":[
        {"name":"authorizer","type":"address"},{"name":"nonce","type":"bytes32"}],
        "outputs":[{"type":"bool"}],"stateMutability":"view"}]`
)

var (
	railABI  = mustABI(railABIJSON)
	tokenABI = mustABI(tokenABIJSON)
)

func mustABI(s string) abi.ABI {
	a, err := abi.JSON(strings.NewReader(s))
	if err != nil {
		panic(fmt.Sprintf("rail: bad ABI: %v", err))
	}
	return a
}

// termsTuple is the contract's terms struct. The three kinds share this shape.
type termsTuple struct {
	Id          [32]byte
	Account     common.Address
	Party       common.Address
	Amount      *big.Int
	Fee         *big.Int
	Relayer     common.Address
	ValidBefore *big.Int
}

// DefaultValidFor is how long a signed variant lives unless told otherwise. It
// bounds how long a stalled relayer can hold an intent, and nothing else: a
// replacement may be signed at once.
const DefaultValidFor = time.Hour

var (
	// ErrExecuted is returned when the intent has already executed under these
	// exact terms. Nothing is wrong: the money moved once.
	ErrExecuted = errors.New("rail: operation already executed")
	// ErrConflict is returned when the identifier is bound to other terms.
	ErrConflict = errors.New("rail: identifier bound to different terms")
	// ErrExpired is returned when a variant's deadline has passed.
	ErrExpired = errors.New("rail: variant deadline has passed")
	// ErrNotRelayer is returned when asked to carry a variant naming someone
	// else. Only the named relayer may submit.
	ErrNotRelayer = errors.New("rail: the variant names a different relayer")
	// ErrForeignDomain is returned for a variant signed for another domain.
	ErrForeignDomain = errors.New("rail: variant belongs to a different domain")
	// ErrBadSignature is returned when a variant is not signed by the account
	// it debits.
	ErrBadSignature = errors.New("rail: variant is not signed by its account")
)

// Domain is one JuiceRail deployment: (chain id, contract address), plus how
// to reach it. One Rail instance serves one domain, so no method takes a
// network argument and domains cannot be mixed.
type Domain struct {
	Name    string
	ChainID *big.Int
	Rail    common.Address
	Token   common.Address
	// Finality names the mechanism that makes a fact permanent. Only true
	// finality is supported, because a confirmed fact must never revert.
	Finality string
}

// Validate checks that the domain is completely and safely configured.
func (d Domain) Validate() error {
	if d.ChainID == nil || d.ChainID.Sign() <= 0 {
		return fmt.Errorf("domain %q: chain id must be set", d.Name)
	}
	if d.Rail == (common.Address{}) {
		return fmt.Errorf("domain %q: rail address must be set", d.Name)
	}
	if d.Token == (common.Address{}) {
		return fmt.Errorf("domain %q: token address must be set", d.Name)
	}
	if d.Finality != "finalized" {
		return fmt.Errorf("domain %q: finality %q unsupported: only true finality (\"finalized\") is safe here", d.Name, d.Finality)
	}
	return nil
}

// Chain is everything the rail needs from a node: reads to decide status, and
// the plain transaction machinery a relayer uses. *ethclient.Client satisfies
// it.
type Chain interface {
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error)
	CallContract(ctx context.Context, call ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)
	BlockNumber(ctx context.Context) (uint64, error)
	PendingNonceAt(ctx context.Context, account common.Address) (uint64, error)
	SuggestGasTipCap(ctx context.Context) (*big.Int, error)
	EstimateGas(ctx context.Context, call ethereum.CallMsg) (uint64, error)
	SendTransaction(ctx context.Context, tx *types.Transaction) error
}

// Rail moves money on one domain and reports finalized facts. The host stays
// authoritative for its own ledger.
//
// A Rail may be used from several goroutines: signing and relaying serialise
// on one instance, which is what keeps two concurrent operations from picking
// the same transaction nonce. Two things it cannot do for you, because neither
// lives inside this process: one relayer key must be driven by one process,
// since nonces are chain state; and a transaction that sticks or is replaced
// needs handling above the library. For parallelism, hold one Rail per key,
// which is the natural shape anyway since each already holds exactly one.
type Rail struct {
	domain Domain
	store  Store
	chain  Chain
	key    *ecdsa.PrivateKey

	address   common.Address
	separator common.Hash

	// mu serialises this rail's own operations: nonce selection through
	// submission, the token domain cache, and the read-then-append that
	// decides whether a fresh variant is needed.
	mu          sync.Mutex
	tokenDomain common.Hash // read once from the token, then fixed
}

// New binds a rail to one domain. The key authorises this account's money and
// pays gas when the rail relays.
func New(d Domain, s Store, chain Chain, key *ecdsa.PrivateKey) (*Rail, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	if s == nil || chain == nil || key == nil {
		return nil, errors.New("rail: store, chain and key are required")
	}
	return &Rail{
		domain:    d,
		store:     s,
		chain:     chain,
		key:       key,
		address:   crypto.PubkeyToAddress(key.PublicKey),
		separator: DomainSeparator(d.ChainID, d.Rail),
	}, nil
}

// Domain returns the domain this rail is bound to.
func (r *Rail) Domain() Domain { return r.domain }

// Account is the rail's address on this domain: an ordinary Ethereum address,
// which is also where its withdrawals may be sent.
func (r *Rail) Account() common.Address { return r.address }

func (r *Rail) ref(id ID) Ref { return Ref{Account: r.address, ID: id} }

// Balance reads an account's rail balance.
func (r *Rail) Balance(ctx context.Context, account common.Address) (*big.Int, error) {
	return callUint256(ctx, r.chain, r.domain.Rail, railABI, "balanceOf", account)
}

// TokenBalance reads a plain token balance, for funding and verification.
func (r *Rail) TokenBalance(ctx context.Context, account common.Address) (*big.Int, error) {
	return callUint256(ctx, r.chain, r.domain.Token, tokenABI, "balanceOf", account)
}

// binding reads the terms an identifier is bound to, or zero if unbound.
func (r *Rail) binding(ctx context.Context, account common.Address, id ID) (common.Hash, error) {
	out, err := call(ctx, r.chain, r.domain.Rail, railABI, "operations", account, id.Hash())
	if err != nil {
		return common.Hash{}, err
	}
	if len(out) != 32 {
		return common.Hash{}, fmt.Errorf("operations: want 32 bytes, got %d", len(out))
	}
	return common.BytesToHash(out), nil
}

// tokenSeparator reads the token's EIP-712 domain, which the deposit
// authorisation is signed under. Reading it from the token means no name or
// version has to be configured, and no domain can be misconfigured.
func (r *Rail) tokenSeparator(ctx context.Context) (common.Hash, error) {
	if r.tokenDomain != (common.Hash{}) {
		return r.tokenDomain, nil
	}
	out, err := call(ctx, r.chain, r.domain.Token, tokenABI, "DOMAIN_SEPARATOR")
	if err != nil {
		return common.Hash{}, fmt.Errorf("read token domain: %w", err)
	}
	if len(out) != 32 {
		return common.Hash{}, fmt.Errorf("read token domain: want 32 bytes, got %d", len(out))
	}
	r.tokenDomain = common.BytesToHash(out)
	return r.tokenDomain, nil
}

// CheckToken verifies that the domain's token exposes the EIP-3009 surface a
// deposit depends on. It proves the read surface is there, not that the token
// verifies authorisations correctly, which stays a trust base assumption; a
// token that fails this can never take a deposit at all.
func (r *Rail) CheckToken(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	separator, err := r.tokenSeparator(ctx)
	if err != nil {
		return err
	}
	if separator == (common.Hash{}) {
		return fmt.Errorf("token %s reports an empty EIP-712 domain", r.domain.Token)
	}
	if _, err := call(ctx, r.chain, r.domain.Token, tokenABI, "authorizationState",
		common.Address{}, common.Hash{}); err != nil {
		return fmt.Errorf("token %s does not implement EIP-3009: %w", r.domain.Token, err)
	}
	return nil
}

func call(ctx context.Context, chain Chain, to common.Address, a abi.ABI, method string, args ...any) ([]byte, error) {
	in, err := a.Pack(method, args...)
	if err != nil {
		return nil, err
	}
	out, err := chain.CallContract(ctx, ethereum.CallMsg{To: &to, Data: in}, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	return out, nil
}

func callUint256(ctx context.Context, chain Chain, to common.Address, a abi.ABI, method string, args ...any) (*big.Int, error) {
	out, err := call(ctx, chain, to, a, method, args...)
	if err != nil {
		return nil, err
	}
	if len(out) != 32 {
		return nil, fmt.Errorf("%s: want 32 bytes, got %d", method, len(out))
	}
	return new(big.Int).SetBytes(out), nil
}

// --- intents ---

// Prepare records the intent. Nothing is signed and nothing is submitted, so
// after this returns the identifier is durably ours and its core terms are
// fixed.
func (r *Rail) Prepare(ctx context.Context, id ID, t Terms) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if t.Account != r.address {
		return fmt.Errorf("rail: intent debits %s, but this rail is %s", t.Account, r.address)
	}
	head, err := r.chain.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("read head: %w", err)
	}
	return r.store.PutIntent(r.ref(id), Intent{Terms: t, FromBlock: head})
}

// PrepareDeposit records a deposit intent crediting account with amount. The
// rail's own address pays; the credited account may be anyone.
func (r *Rail) PrepareDeposit(ctx context.Context, id ID, account common.Address, amount *big.Int) error {
	return r.Prepare(ctx, id, Terms{Kind: KindDeposit, Account: r.address, Party: account, Amount: amount})
}

// PrepareTransfer records a payment to another account on this domain.
func (r *Rail) PrepareTransfer(ctx context.Context, id ID, recipient common.Address, amount *big.Int) error {
	return r.Prepare(ctx, id, Terms{Kind: KindTransfer, Account: r.address, Party: recipient, Amount: amount})
}

// PrepareWithdrawal records a withdrawal to an address outside the rail.
func (r *Rail) PrepareWithdrawal(ctx context.Context, id ID, destination common.Address, amount *big.Int) error {
	return r.Prepare(ctx, id, Terms{Kind: KindWithdraw, Account: r.address, Party: destination, Amount: amount})
}

// Sign returns a signed variant of a recorded intent, signing a fresh one
// unless a live variant with the same relayer and fee already exists. The
// variant is durable before it is returned, so a crash can never leave an
// operation we signed but hold no record of.
//
// Replacing an unresponsive relayer needs no waiting: sign another variant.
// The contract executes at most one, and a loser reverts at its own relayer's
// cost.
func (r *Rail) Sign(ctx context.Context, id ID, fee *big.Int, relayer common.Address, validFor time.Duration) (Variant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sign(ctx, id, fee, relayer, validFor)
}

func (r *Rail) sign(ctx context.Context, id ID, fee *big.Int, relayer common.Address, validFor time.Duration) (Variant, error) {
	ref := r.ref(id)
	abandoned, err := r.store.Abandoned(ref)
	if err != nil {
		return Variant{}, err
	}
	if abandoned {
		return Variant{}, fmt.Errorf("%w: %s", ErrAbandoned, id)
	}
	in, ok, err := r.store.Intent(ref)
	if err != nil {
		return Variant{}, err
	}
	if !ok {
		return Variant{}, fmt.Errorf("%w: %s", ErrNoIntent, id)
	}
	if fee == nil {
		fee = new(big.Int)
	}
	if relayer == (common.Address{}) {
		relayer = r.address
	}
	if validFor <= 0 {
		validFor = DefaultValidFor
	}

	// Liveness here is measured against the latest head: that is what decides
	// whether a variant can still be included. Death, which is a durable
	// judgement, is measured against the finalized head instead.
	head, err := r.chain.HeaderByNumber(ctx, nil)
	if err != nil {
		return Variant{}, fmt.Errorf("read head: %w", err)
	}
	existing, err := r.store.Variants(ref)
	if err != nil {
		return Variant{}, err
	}
	for i := len(existing) - 1; i >= 0; i-- {
		v := existing[i]
		if !variantDead(v, head.Time) && v.Relayer == relayer && v.Fee.Cmp(fee) == 0 {
			return v, nil
		}
	}

	v := Variant{
		Terms:       in.Terms,
		ID:          id,
		Fee:         fee,
		Relayer:     relayer,
		ValidBefore: head.Time + uint64(validFor.Seconds()),
	}
	h, err := TermsHash(v)
	if err != nil {
		return Variant{}, err
	}
	if v.TermsSig, err = signDigest(r.key, digest(r.separator, h)); err != nil {
		return Variant{}, err
	}
	if v.Kind == KindDeposit {
		tokenDomain, err := r.tokenSeparator(ctx)
		if err != nil {
			return Variant{}, err
		}
		// The authorisation is payable only to the rail, expires with the
		// operation, and uses the terms hash as its nonce, which is what ties
		// the two signatures to one deposit.
		d, err := authDigest(tokenDomain, v.Account, r.domain.Rail, v.Total(), v.ValidBefore, h)
		if err != nil {
			return Variant{}, err
		}
		if v.AuthSig, err = signDigest(r.key, d); err != nil {
			return Variant{}, err
		}
	}
	if err := v.Validate(); err != nil {
		return Variant{}, err
	}
	if err := r.store.AppendVariant(ref, v); err != nil {
		return Variant{}, err
	}
	return v, nil
}

// Accept checks that an envelope was signed for this domain and returns its
// variant. The domain is inside the signature too, so this only turns a
// foreign variant into a clear refusal.
func (r *Rail) Accept(env Envelope) (Variant, error) {
	if env.ChainID == nil || env.ChainID.Cmp(r.domain.ChainID) != 0 || env.Rail != r.domain.Rail {
		return Variant{}, fmt.Errorf("%w: signed for (%s, %s), this rail is (%s, %s)",
			ErrForeignDomain, env.ChainID, env.Rail, r.domain.ChainID, r.domain.Rail)
	}
	return env.Variant, nil
}

// Relay verifies a signed variant and submits it, paying gas from the rail's
// own key. It touches no durable state, so it serves variants signed by anyone
// — being a relayer requires nothing but native gas.
func (r *Rail) Relay(ctx context.Context, v Variant) (common.Hash, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.relay(ctx, v)
}

func (r *Rail) relay(ctx context.Context, v Variant) (common.Hash, error) {
	if err := v.Validate(); err != nil {
		return common.Hash{}, err
	}
	if v.Relayer != r.address {
		return common.Hash{}, fmt.Errorf("%w: names %s, this rail is %s", ErrNotRelayer, v.Relayer, r.address)
	}
	h, err := TermsHash(v)
	if err != nil {
		return common.Hash{}, err
	}
	signer, err := RecoverSigner(digest(r.separator, h), v.TermsSig)
	if err != nil {
		return common.Hash{}, err
	}
	if signer != v.Account {
		return common.Hash{}, fmt.Errorf("%w: recovers to %s, not %s", ErrBadSignature, signer, v.Account)
	}
	if v.Kind == KindDeposit {
		tokenDomain, err := r.tokenSeparator(ctx)
		if err != nil {
			return common.Hash{}, err
		}
		d, err := authDigest(tokenDomain, v.Account, r.domain.Rail, v.Total(), v.ValidBefore, h)
		if err != nil {
			return common.Hash{}, err
		}
		authSigner, err := RecoverSigner(d, v.AuthSig)
		if err != nil {
			return common.Hash{}, err
		}
		if authSigner != v.Account {
			return common.Hash{}, fmt.Errorf("%w: the token authorisation recovers to %s, not %s",
				ErrBadSignature, authSigner, v.Account)
		}
	}

	// Reading the binding first separates "already executed" from "will
	// execute": a simulation succeeds for both, and only one is worth gas.
	bound, err := r.binding(ctx, v.Account, v.ID)
	if err != nil {
		return common.Hash{}, err
	}
	switch bound {
	case h:
		return common.Hash{}, fmt.Errorf("%w: %s", ErrExecuted, v.Ref())
	case common.Hash{}:
	default:
		return common.Hash{}, fmt.Errorf("%w: %s", ErrConflict, v.Ref())
	}

	head, err := r.chain.HeaderByNumber(ctx, nil)
	if err != nil {
		return common.Hash{}, fmt.Errorf("read head: %w", err)
	}
	if variantDead(v, head.Time) {
		return common.Hash{}, fmt.Errorf("%w: %s", ErrExpired, v.Ref())
	}
	if v.Kind != KindDeposit {
		balance, err := r.Balance(ctx, v.Account)
		if err != nil {
			return common.Hash{}, err
		}
		if balance.Cmp(v.Total()) < 0 {
			return common.Hash{}, fmt.Errorf("rail: %s holds %s, needs %s", v.Account, balance, v.Total())
		}
	}

	data, err := callData(v)
	if err != nil {
		return common.Hash{}, err
	}
	return r.send(ctx, data, head)
}

// Submit signs a variant naming this rail as its own relayer and submits it.
// It is the path for an account that holds gas; an account that does not signs
// and hands the variant to a relayer instead.
func (r *Rail) Submit(ctx context.Context, id ID, fee *big.Int, validFor time.Duration) (common.Hash, error) {
	// One lock across both halves: the nonce this picks must not be picked
	// again before the transaction is on its way.
	r.mu.Lock()
	defer r.mu.Unlock()

	v, err := r.sign(ctx, id, fee, r.address, validFor)
	if err != nil {
		return common.Hash{}, err
	}
	return r.relay(ctx, v)
}

// Abandon records that the rail will never sign this intent again. It does not
// kill variants already signed, so the host must still wait for `failed`
// before releasing anything.
func (r *Rail) Abandon(_ context.Context, id ID) error {
	return r.store.Abandon(r.ref(id))
}

// Variants lists the signed variants of an intent, newest last.
func (r *Rail) Variants(id ID) ([]Variant, error) {
	return r.store.Variants(r.ref(id))
}

// callData builds the exact contract call for a variant.
func callData(v Variant) ([]byte, error) {
	t := termsTuple{
		Id:          v.ID,
		Account:     v.Account,
		Party:       v.Party,
		Amount:      v.Amount,
		Fee:         v.Fee,
		Relayer:     v.Relayer,
		ValidBefore: new(big.Int).SetUint64(v.ValidBefore),
	}
	switch v.Kind {
	case KindDeposit:
		return railABI.Pack("deposit", t, v.TermsSig, v.AuthSig)
	case KindTransfer:
		return railABI.Pack("transfer", t, v.TermsSig)
	case KindWithdraw:
		return railABI.Pack("withdraw", t, v.TermsSig)
	default:
		return nil, fmt.Errorf("%w: unknown kind", errBadTerms)
	}
}

// send simulates the exact call, then submits it. Simulating first is what
// keeps a relayer from burning gas on an operation that cannot execute.
func (r *Rail) send(ctx context.Context, data []byte, head *types.Header) (common.Hash, error) {
	to := r.domain.Rail
	msg := ethereum.CallMsg{From: r.address, To: &to, Data: data}
	if _, err := r.chain.CallContract(ctx, msg, nil); err != nil {
		return common.Hash{}, fmt.Errorf("simulate: %w", err)
	}
	gas, err := r.chain.EstimateGas(ctx, msg)
	if err != nil {
		return common.Hash{}, fmt.Errorf("estimate gas: %w", err)
	}
	nonce, err := r.chain.PendingNonceAt(ctx, r.address)
	if err != nil {
		return common.Hash{}, fmt.Errorf("read nonce: %w", err)
	}
	tip, err := r.chain.SuggestGasTipCap(ctx)
	if err != nil {
		return common.Hash{}, fmt.Errorf("suggest gas tip: %w", err)
	}
	if tip == nil || tip.Sign() == 0 {
		tip = big.NewInt(1)
	}
	feeCap := new(big.Int).Set(tip)
	if head.BaseFee != nil {
		feeCap.Add(feeCap, new(big.Int).Mul(head.BaseFee, big.NewInt(2)))
	}

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   r.domain.ChainID,
		Nonce:     nonce,
		GasTipCap: tip,
		GasFeeCap: feeCap,
		Gas:       gas + gas/5,
		To:        &to,
		Data:      data,
	})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(r.domain.ChainID), r.key)
	if err != nil {
		return common.Hash{}, fmt.Errorf("sign transaction: %w", err)
	}
	if err := r.chain.SendTransaction(ctx, signed); err != nil {
		return common.Hash{}, fmt.Errorf("submit: %w", err)
	}
	// The hash is a hint for finding the event, never a status.
	return signed.Hash(), nil
}
