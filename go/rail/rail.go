package rail

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// refillWindow is how long a signed refill stays valid.
const refillWindow = time.Hour

// Chain is everything the rail needs from a node: the reads that decide
// status, and plain transaction machinery. *ethclient.Client satisfies it.
type Chain interface {
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error)
	CallContract(ctx context.Context, call ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)
	BalanceAt(ctx context.Context, account common.Address, block *big.Int) (*big.Int, error)
	NonceAt(ctx context.Context, account common.Address, block *big.Int) (uint64, error)
	PendingNonceAt(ctx context.Context, account common.Address) (uint64, error)
	SuggestGasTipCap(ctx context.Context) (*big.Int, error)
	SendTransaction(ctx context.Context, tx *types.Transaction) error
	TransactionReceipt(ctx context.Context, hash common.Hash) (*types.Receipt, error)
}

// Rail is one account on one domain. It holds the stablecoin as money and the
// chain's native currency as an operating reserve, and needs no other party to
// move either.
//
// A Rail may be used from several goroutines: everything from reconciliation
// through nonce assignment to submission serialises on one instance. One thing
// it cannot do for you, because it does not live inside this process: a key
// must be driven by exactly one signer. Nonces are chain state, and two
// signers would race for them.
type Rail struct {
	domain  Domain
	store   Store
	chain   Chain
	key     *ecdsa.PrivateKey
	address common.Address

	// mu serialises reconciliation, the reserve decision, nonce assignment and
	// submission: the sequence that must not interleave with itself.
	mu sync.Mutex
}

// New binds a rail to one domain. The key both spends the money and pays the
// gas: they are the same account.
func New(d Domain, s Store, chain Chain, key *ecdsa.PrivateKey) (*Rail, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	if s == nil || chain == nil || key == nil {
		return nil, fmt.Errorf("%w: store, chain and key are required", ErrBadInput)
	}
	return &Rail{
		domain:  d,
		store:   s,
		chain:   chain,
		key:     key,
		address: crypto.PubkeyToAddress(key.PublicKey),
	}, nil
}

// Domain returns the domain this rail is bound to.
func (r *Rail) Domain() Domain { return r.domain }

// Account is the rail's address: an ordinary Ethereum address, which is also
// where anyone may send it money.
func (r *Rail) Account() common.Address { return r.address }

// Balances reports the account's money and its operating reserve.
func (r *Rail) Balances(ctx context.Context) (token, gas *big.Int, err error) {
	if token, err = tokenUint(ctx, r.chain, r.domain.Token, "balanceOf", r.address); err != nil {
		return nil, nil, err
	}
	if gas, err = r.chain.BalanceAt(ctx, r.address, nil); err != nil {
		return nil, nil, fmt.Errorf("read reserve: %w", err)
	}
	return token, gas, nil
}

// CheckDomain proves the domain is usable before any money depends on it: the
// token answers, it carries EIP-2612 permits, and the venue can price a
// refill. A domain that fails this can never keep an account in gas.
func (r *Rail) CheckDomain(ctx context.Context) error {
	if _, err := tokenUint(ctx, r.chain, r.domain.Token, "balanceOf", r.address); err != nil {
		return fmt.Errorf("token %s is not an ERC-20 here: %w", r.domain.Token, err)
	}
	if _, err := callWord(ctx, r.chain, r.domain.Token, tokenABI, "DOMAIN_SEPARATOR"); err != nil {
		return fmt.Errorf("token %s does not implement EIP-2612: %w", r.domain.Token, err)
	}
	if _, err := tokenUint(ctx, r.chain, r.domain.Token, "nonces", r.address); err != nil {
		return fmt.Errorf("token %s does not implement EIP-2612: %w", r.domain.Token, err)
	}
	if _, err := quoteRefill(ctx, r.chain, r.domain, r.domain.Gas.Min); err != nil {
		return fmt.Errorf("swap venue %s cannot price a refill: %w", r.domain.Venue.Router, err)
	}
	return nil
}

// Prepare runs the reserve policy and records the write-ahead intent. Nothing
// is signed, so after it returns the identifier durably owns one account nonce
// and no transaction exists yet.
//
// If the reserve is too low it returns ErrNeedRefill and records nothing: call
// Refill, wait for it to finalize, then Prepare again. If money is short it
// returns a shortage error, also having recorded nothing. Blocked is not lost.
//
// Preparing the same identifier twice with the same terms is a no-op, so a
// repeated command never pays twice.
func (r *Rail) Prepare(ctx context.Context, id ID, kind Kind, to common.Address, amount *big.Int) error {
	if kind != KindTransfer && kind != KindWithdraw {
		return fmt.Errorf("%w: %s is not a payment", ErrBadInput, kind)
	}
	if to == (common.Address{}) {
		return fmt.Errorf("%w: destination must be set", ErrBadInput)
	}
	if amount == nil || amount.Sign() <= 0 {
		return fmt.Errorf("%w: amount must be positive", ErrBadInput)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if in, ok, err := r.store.Intent(r.address, id); err != nil {
		return err
	} else if ok {
		if in.Kind != kind || in.To != to || in.Amount.Cmp(amount) != 0 {
			return fmt.Errorf("%w: %s", ErrIntentConflict, id)
		}
		return nil
	}
	if err := r.ready(ctx); err != nil {
		return err
	}

	head, _, feeCap, err := r.fees(ctx)
	if err != nil {
		return err
	}
	in, err := r.policyInput(ctx, feeCap, amount)
	if err != nil {
		return err
	}
	switch d := Decide(r.domain.Gas, in); d.Action {
	case ActionWait:
		return r.shortage(d.Reason, in, amount)
	case ActionQuote, ActionRefill:
		return fmt.Errorf("%w: reserve is %s, minimum is %s", ErrNeedRefill, in.Gas, r.domain.Gas.Min)
	}

	data, err := transferCalldata(to, amount)
	if err != nil {
		return err
	}
	if err := r.simulate(ctx, r.domain.Token, data, r.gasLimit(kind)); err != nil {
		return err
	}
	// Only the nonce is bound here. Fees belong to a submission, not to the
	// intent, because a retry may raise them and nothing else.
	nonce, err := r.nextNonce(ctx)
	if err != nil {
		return err
	}
	return r.store.PutIntent(r.address, Intent{
		ID: id, Kind: kind, To: to, Amount: new(big.Int).Set(amount),
		Nonce: nonce, Calldata: data, FromBlock: head.Number.Uint64(),
	})
}

// Refill buys native currency with the account's own stablecoin, topping the
// reserve up towards its maximum. reserve is the payment the refill must leave
// affordable; pass zero when topping up for its own sake.
//
// It records and submits in one call because the identifier is derived from
// the nonce, not chosen by the caller: maintenance is not a banking verb.
func (r *Rail) Refill(ctx context.Context, reserve *big.Int) (ID, common.Hash, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.ready(ctx); err != nil {
		return ID{}, common.Hash{}, err
	}
	head, tip, feeCap, err := r.fees(ctx)
	if err != nil {
		return ID{}, common.Hash{}, err
	}
	in, err := r.policyInput(ctx, feeCap, reserve)
	if err != nil {
		return ID{}, common.Hash{}, err
	}

	d := Decide(r.domain.Gas, in)
	if d.Action == ActionExecute {
		return ID{}, common.Hash{}, fmt.Errorf("%w: %s held, minimum is %s", ErrNoRefillNeeded, in.Gas, r.domain.Gas.Min)
	}
	if d.Action == ActionQuote {
		quote, err := quoteRefill(ctx, r.chain, r.domain, d.Delta)
		if err != nil {
			return ID{}, common.Hash{}, err
		}
		in.Quote = quote
		d = Decide(r.domain.Gas, in)
	}
	if d.Action != ActionRefill {
		return ID{}, common.Hash{}, r.shortage(d.Reason, in, reserve)
	}

	deadline := head.Time + uint64(refillWindow.Seconds())
	data, err := refillCalldata(ctx, r.chain, r.domain, r.key, r.address, d.Delta, d.MaxInput, deadline)
	if err != nil {
		return ID{}, common.Hash{}, err
	}
	if err := r.simulate(ctx, r.domain.Venue.Router, data, r.gasLimit(KindRefill)); err != nil {
		return ID{}, common.Hash{}, err
	}
	nonce, err := r.nextNonce(ctx)
	if err != nil {
		return ID{}, common.Hash{}, err
	}
	intent := Intent{
		ID: refillID(r.address, nonce), Kind: KindRefill, To: r.domain.Venue.Router,
		Amount: d.MaxInput, Delta: d.Delta, Nonce: nonce, Calldata: data,
		FromBlock: head.Number.Uint64(),
	}
	if err := r.store.PutIntent(r.address, intent); err != nil {
		return ID{}, common.Hash{}, err
	}
	hash, err := r.submit(ctx, intent, Submission{Gas: r.gasLimit(KindRefill), Tip: tip, FeeCap: feeCap})
	return intent.ID, hash, err
}

// Send signs a recorded intent at its recorded nonce and broadcasts it.
//
// It is idempotent: signing is deterministic and the fees of an existing
// attempt are reused, so calling it again reproduces the same transaction
// rather than making a second one.
func (r *Rail) Send(ctx context.Context, id ID) (common.Hash, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	in, live, err := r.unresolved(id)
	if err != nil || !live {
		return common.Hash{}, err
	}
	subs, err := r.store.Submissions(r.address, id)
	if err != nil {
		return common.Hash{}, err
	}
	if len(subs) > 0 {
		return r.submit(ctx, in, subs[len(subs)-1])
	}
	_, tip, feeCap, err := r.fees(ctx)
	if err != nil {
		return common.Hash{}, err
	}
	return r.submit(ctx, in, Submission{Gas: r.gasLimit(in.Kind), Tip: tip, FeeCap: feeCap})
}

// Retry re-sends a recorded intent under the same nonce and the same economic
// terms, raising only the transaction fees. That is Ethereum's own replacement
// rule, and it is why a stuck payment never becomes two payments.
func (r *Rail) Retry(ctx context.Context, id ID) (common.Hash, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	in, live, err := r.unresolved(id)
	if err != nil || !live {
		return common.Hash{}, err
	}
	subs, err := r.store.Submissions(r.address, id)
	if err != nil {
		return common.Hash{}, err
	}
	_, tip, feeCap, err := r.fees(ctx)
	if err != nil {
		return common.Hash{}, err
	}
	next := Submission{Gas: r.gasLimit(in.Kind), Tip: tip, FeeCap: feeCap}
	if len(subs) > 0 {
		last := subs[len(subs)-1]
		next.Gas = last.Gas
		next.Tip = maxInt(next.Tip, bump(last.Tip))
		next.FeeCap = maxInt(next.FeeCap, bump(last.FeeCap))
	}
	return r.submit(ctx, in, next)
}

// Pending lists intents with no finalized outcome, oldest first.
func (r *Rail) Pending() ([]Intent, error) { return r.store.Pending(r.address) }

// Intent returns a recorded intent.
func (r *Rail) Intent(id ID) (Intent, bool, error) { return r.store.Intent(r.address, id) }

// --- internals ---

// ready brings the rail up to date and refuses to start anything while
// something is still in flight. One outgoing transaction at a time is what
// keeps two operations from projecting the same reserve twice.
func (r *Rail) ready(ctx context.Context) error {
	if err := r.reconcile(ctx); err != nil {
		return err
	}
	if err := r.settle(ctx); err != nil {
		return err
	}
	pending, err := r.store.Pending(r.address)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		return fmt.Errorf("%w: %s %s", ErrInFlight, pending[0].Kind, pending[0].ID)
	}
	return nil
}

// unresolved loads an intent that still needs sending. An intent that has
// already finalized is reported as not live, so callers read its outcome
// instead of acting on it again.
func (r *Rail) unresolved(id ID) (Intent, bool, error) {
	in, ok, err := r.store.Intent(r.address, id)
	if err != nil {
		return Intent{}, false, err
	}
	if !ok {
		return Intent{}, false, fmt.Errorf("%w: %s", ErrNoIntent, id)
	}
	if _, done, err := r.store.Fact(r.address, id); err != nil {
		return Intent{}, false, err
	} else if done {
		return Intent{}, false, nil
	}
	return in, true, nil
}

// policyInput gathers what the reserve decision depends on. Costs are fee cap
// times a fixed gas limit: an upper bound, so the decision errs towards
// keeping the account able to act.
func (r *Rail) policyInput(ctx context.Context, feeCap, pending *big.Int) (PolicyInput, error) {
	token, gas, err := r.Balances(ctx)
	if err != nil {
		return PolicyInput{}, err
	}
	_, swap := r.domain.Gas.limits()
	return PolicyInput{
		Gas:           gas,
		Token:         token,
		SwapCost:      new(big.Int).Mul(feeCap, new(big.Int).SetUint64(swap)),
		PendingAmount: pending,
	}, nil
}

// shortage turns a policy refusal into an error that says what is missing.
func (r *Rail) shortage(reason Reason, in PolicyInput, pending *big.Int) error {
	switch reason {
	case ReasonNeedToken:
		return fmt.Errorf("%w: holding %s, need %s plus the cost of a refill — send stablecoin to %s",
			ErrInsufficientStablecoin, in.Token, orZero(pending), r.address)
	case ReasonNeedNative:
		return fmt.Errorf("%w: holding %s, a refill costs %s — send native currency to %s",
			ErrInsufficientNative, in.Gas, in.SwapCost, r.address)
	case ReasonFeesAboveBound:
		return fmt.Errorf("%w: a refill would cost %s, the bound is %s", ErrFeesAboveBound, in.SwapCost, r.domain.Gas.FeeBound)
	default:
		return fmt.Errorf("%w: refused with no reason", ErrBadInput)
	}
}

// fees reads the head and prices a transaction from it. The cap covers a few
// blocks of base fee growth, so a transaction stays includable while it waits.
func (r *Rail) fees(ctx context.Context) (*types.Header, *big.Int, *big.Int, error) {
	head, err := r.chain.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read head: %w", err)
	}
	tip, err := r.chain.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("suggest gas tip: %w", err)
	}
	if tip == nil || tip.Sign() == 0 {
		tip = big.NewInt(1)
	}
	feeCap := new(big.Int).Set(tip)
	if head.BaseFee != nil {
		feeCap.Add(feeCap, new(big.Int).Mul(head.BaseFee, big.NewInt(2)))
	}
	return head, tip, feeCap, nil
}

// nextNonce assigns the account's next nonce, refusing if the chain shows a
// transaction this rail has no record of. Nonces are the whole exactly-once
// mechanism, so a surprise there is reported rather than worked around.
func (r *Rail) nextNonce(ctx context.Context) (uint64, error) {
	pending, err := r.chain.PendingNonceAt(ctx, r.address)
	if err != nil {
		return 0, fmt.Errorf("read nonce: %w", err)
	}
	finalized, err := r.chain.NonceAt(ctx, r.address, finalizedBlockArg)
	if err != nil {
		return 0, fmt.Errorf("read finalized nonce: %w", err)
	}
	if pending != finalized {
		return 0, fmt.Errorf("%w: nonce %d is finalized but %d is already in use", ErrUnreconciled, finalized, pending)
	}
	return pending, nil
}

func (r *Rail) gasLimit(kind Kind) uint64 {
	payment, swap := r.domain.Gas.limits()
	if kind == KindRefill {
		return swap
	}
	return payment
}

// target is where a recorded operation's transaction goes. A refill uses the
// venue recorded with it: its permit names that venue as the spender, so
// sending the same calldata to a venue configuration has since moved to would
// authorise nothing and revert. The token cannot move the same way — it is half
// the domain's identity, so a different one is a different store.
func (r *Rail) target(in Intent) common.Address {
	if in.Kind == KindRefill {
		return in.To
	}
	return r.domain.Token
}

// simulate runs the exact call, under the exact gas bound the transaction will
// carry, before anything is recorded. A call that cannot execute never consumes
// a nonce, and a configured bound that is too small is caught here rather than
// by an out-of-gas transaction that spends one.
func (r *Rail) simulate(ctx context.Context, to common.Address, data []byte, gas uint64) error {
	msg := ethereum.CallMsg{From: r.address, To: &to, Data: data, Gas: gas}
	if _, err := r.chain.CallContract(ctx, msg, nil); err != nil {
		return fmt.Errorf("simulate under a gas bound of %d: %w", gas, err)
	}
	return nil
}

// submit signs and broadcasts one attempt. The attempt is durable before it is
// broadcast, so no transaction can exist on chain that this account holds no
// record of.
func (r *Rail) submit(ctx context.Context, in Intent, sub Submission) (common.Hash, error) {
	to := r.target(in)
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   r.domain.ChainID,
		Nonce:     in.Nonce,
		GasTipCap: sub.Tip,
		GasFeeCap: sub.FeeCap,
		Gas:       sub.Gas,
		To:        &to,
		Data:      in.Calldata,
	})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(r.domain.ChainID), r.key)
	if err != nil {
		return common.Hash{}, fmt.Errorf("sign transaction: %w", err)
	}
	// Signing is deterministic, so re-sending a recorded attempt reproduces it
	// exactly and appends nothing.
	if sub.TxHash != signed.Hash() {
		sub.TxHash = signed.Hash()
		if err := r.store.AppendSubmission(r.address, in.ID, sub); err != nil {
			return common.Hash{}, err
		}
	}
	if err := r.chain.SendTransaction(ctx, signed); err != nil && !alreadySettled(err) {
		return common.Hash{}, fmt.Errorf("submit: %w", err)
	}
	// The hash locates the transaction. It is never a status.
	return signed.Hash(), nil
}

// alreadySettled reports whether the node is refusing a transaction because the
// work is already done: it holds this very transaction, or the nonce has
// already been spent. Neither is a failure — both mean the outcome is on the
// chain rather than in this call, which is where status comes from anyway.
func alreadySettled(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already known") ||
		strings.Contains(s, "known transaction") ||
		strings.Contains(s, "nonce too low")
}

// bump raises a fee by the 12.5% a node requires before it will accept a
// replacement.
func bump(v *big.Int) *big.Int {
	if v == nil {
		return big.NewInt(1)
	}
	out := new(big.Int).Mul(v, big.NewInt(9))
	out.Div(out, big.NewInt(8))
	return out.Add(out, big.NewInt(1))
}

func maxInt(a, b *big.Int) *big.Int {
	if orZero(a).Cmp(orZero(b)) >= 0 {
		return orZero(a)
	}
	return orZero(b)
}

// refillID names a refill from the nonce it owns, so a refill interrupted
// before it was sent is resumed rather than duplicated, and a caller's own
// identifiers are never taken.
func refillID(account common.Address, nonce uint64) ID {
	var n [8]byte
	for i := 0; i < 8; i++ {
		n[7-i] = byte(nonce >> (8 * i))
	}
	return ID(crypto.Keccak256Hash([]byte("juice-rail/refill"), account.Bytes(), n[:]))
}
