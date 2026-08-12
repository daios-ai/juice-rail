package rail

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Status is a property of the intent, never of one submission.
type Status string

const (
	// StatusUnknown means no record of the intent.
	StatusUnknown Status = "unknown"
	// StatusPending means the intent may still execute: its nonce is not yet
	// spent at finality. A transaction that stalls is pending, not failed.
	StatusPending Status = "pending"
	// StatusConfirmed means a finalized transaction carried out the intent.
	StatusConfirmed Status = "confirmed"
	// StatusFailed means the intent can never execute: its nonce is spent at
	// finality by a reverted transaction, or by one this account never
	// recorded.
	StatusFailed Status = "failed"
)

// finalizedBlockArg selects the "finalized" tag; go-ethereum encodes the RPC
// block tags as negative numbers.
var finalizedBlockArg = big.NewInt(-3)

// maxScanSpan is how many blocks one deposit query may cover. Nodes commonly
// refuse wider ranges, and the limit is theirs rather than any one provider's.
const maxScanSpan = 10_000

// classify is the whole decision, over a fact the caller has already gathered.
// A fact only exists once the chain has finalized, so confirmed and failed are
// both permanent.
func classify(f *Fact) Status {
	switch {
	case f == nil:
		return StatusPending
	case f.Executed:
		return StatusConfirmed
	default:
		return StatusFailed
	}
}

// Status reports where an intent stands, from finalized chain facts only.
func (r *Rail) Status(ctx context.Context, id ID) (Status, error) {
	in, ok, err := r.store.Intent(r.address, id)
	if err != nil {
		return StatusUnknown, err
	}
	if !ok {
		return StatusUnknown, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status(ctx, in)
}

func (r *Rail) status(ctx context.Context, in Intent) (Status, error) {
	if f, cached, err := r.store.Fact(r.address, in.ID); err != nil {
		return StatusUnknown, err
	} else if cached {
		return classify(&f), nil
	}
	head, err := r.finalizedHeader(ctx)
	if err != nil {
		return StatusUnknown, err
	}
	spent, err := r.chain.NonceAt(ctx, r.address, head.Number)
	if err != nil {
		return StatusUnknown, fmt.Errorf("read finalized nonce: %w", err)
	}
	// While the nonce is unspent at finality the intent may still execute, and
	// nothing about it is durable yet.
	if in.Nonce >= spent {
		return StatusPending, nil
	}
	f, err := r.outcome(ctx, in, head.Number)
	if err != nil {
		return StatusUnknown, err
	}
	if err := r.store.PutFact(r.address, in.ID, f); err != nil {
		return StatusUnknown, err
	}
	return classify(&f), nil
}

// outcome finds what finalized under an intent's nonce. Only this account's
// own recorded attempts are considered: a transaction hash commits to its
// nonce, its destination and its calldata, so a receipt for a recorded hash is
// proof that this exact intent was mined.
//
// A zero transaction hash means the nonce was spent by something this account
// never recorded, which the intent can never recover from.
func (r *Rail) outcome(ctx context.Context, in Intent, finalized *big.Int) (Fact, error) {
	subs, err := r.store.Submissions(r.address, in.ID)
	if err != nil {
		return Fact{}, err
	}
	for i := len(subs) - 1; i >= 0; i-- {
		receipt, err := r.chain.TransactionReceipt(ctx, subs[i].TxHash)
		switch {
		case errors.Is(err, ethereum.NotFound) || (err == nil && receipt == nil):
			continue // this attempt was never mined; another may have been
		case err != nil:
			// The node could not answer. That is not evidence of anything, and
			// a fact written from it would be permanent. Ask again later.
			return Fact{}, fmt.Errorf("read receipt %s: %w", subs[i].TxHash, err)
		}
		if receipt.BlockNumber == nil || receipt.BlockNumber.Cmp(finalized) > 0 {
			continue // included but not yet final
		}
		return Fact{
			TxHash:      subs[i].TxHash,
			BlockNumber: receipt.BlockNumber.Uint64(),
			Executed:    r.executed(in, receipt),
		}, nil
	}
	return Fact{}, nil
}

// executed reports whether a finalized receipt really carried the intent out.
// A payment must also show its transfer: a token that reports failure by
// returning false instead of reverting would otherwise look like success.
func (r *Rail) executed(in Intent, receipt *types.Receipt) bool {
	if receipt.Status != types.ReceiptStatusSuccessful {
		return false
	}
	if in.Kind == KindRefill {
		return true
	}
	for _, lg := range receipt.Logs {
		if lg == nil || lg.Removed {
			continue
		}
		if transferLog(lg.Address, lg.Topics, lg.Data, r.domain.Token, r.address, in.To, in.Amount) {
			return true
		}
	}
	return false
}

// settle brings every unresolved intent up to date against finality, so the
// records agree with the chain before anything new is decided.
func (r *Rail) settle(ctx context.Context) error {
	pending, err := r.store.Pending(r.address)
	if err != nil {
		return err
	}
	for _, in := range pending {
		if _, err := r.status(ctx, in); err != nil {
			return err
		}
	}
	return nil
}

// Reconcile matches every spent nonce back to a recorded intent. It is the
// restart procedure: a crash may have left a signed transaction that this
// process never saw finalize, and the chain is what says which one won.
//
// A spent nonce with no recorded attempt behind it means something else signed
// with this key. That cannot be repaired here, so it is reported rather than
// papered over.
func (r *Rail) Reconcile(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reconcile(ctx)
}

func (r *Rail) reconcile(ctx context.Context) error {
	head, err := r.finalizedHeader(ctx)
	if err != nil {
		return err
	}
	spent, err := r.chain.NonceAt(ctx, r.address, head.Number)
	if err != nil {
		return fmt.Errorf("read finalized nonce: %w", err)
	}
	floor, ok, err := r.store.NonceFloor(r.address)
	if err != nil {
		return err
	}
	if !ok {
		// First use: whatever this account did before it had records is not
		// ours to explain.
		return r.store.PutNonceFloor(r.address, spent)
	}
	for nonce := floor; nonce < spent; nonce++ {
		in, found, err := r.store.IntentByNonce(r.address, nonce)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: nonce %d", ErrUnreconciled, nonce)
		}
		f, cached, err := r.store.Fact(r.address, in.ID)
		if err != nil {
			return err
		}
		if !cached {
			if f, err = r.outcome(ctx, in, head.Number); err != nil {
				return err
			}
			if err := r.store.PutFact(r.address, in.ID, f); err != nil {
				return err
			}
		}
		if f.TxHash == (common.Hash{}) {
			return fmt.Errorf("%w: nonce %d was spent by a transaction %s does not record", ErrUnreconciled, nonce, in.ID)
		}
	}
	return r.store.PutNonceFloor(r.address, spent)
}

// finalizedHeader reads the head under the domain's finality mechanism. Only
// true finality qualifies, so a confirmed fact can never revert.
func (r *Rail) finalizedHeader(ctx context.Context) (*types.Header, error) {
	h, err := r.chain.HeaderByNumber(ctx, finalizedBlockArg)
	if err != nil {
		return nil, fmt.Errorf("read finalized head: %w", err)
	}
	if h == nil {
		return nil, fmt.Errorf("read finalized head: no header")
	}
	return h, nil
}

// ScanDeposits records incoming money that has finalized since the last scan
// and returns what it found. A deposit needs no transaction from this account:
// somebody sent it a token transfer, and the log that carried it is its
// identity, so one transfer is recognised exactly once.
func (r *Rail) ScanDeposits(ctx context.Context) ([]Deposit, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	head, err := r.finalizedHeader(ctx)
	if err != nil {
		return nil, err
	}
	to := head.Number.Uint64()
	from := r.domain.FromBlock
	if cursor, ok, err := r.store.Cursor(r.address); err != nil {
		return nil, err
	} else if ok {
		from = cursor + 1
	}
	var found []Deposit
	// Nodes bound how many blocks one log query may cover, and a chain that
	// makes blocks quickly outruns that bound in hours. Walking in spans keeps
	// the query answerable, and moving the cursor after each one means an
	// interrupted scan resumes where it stopped rather than starting over.
	for from <= to {
		last := to
		if span := from + maxScanSpan - 1; span < last {
			last = span
		}
		logs, err := r.chain.FilterLogs(ctx, ethereum.FilterQuery{
			FromBlock: new(big.Int).SetUint64(from),
			ToBlock:   new(big.Int).SetUint64(last),
			Addresses: []common.Address{r.domain.Token},
			Topics: [][]common.Hash{
				{topicTransfer},
				nil,
				{common.BytesToHash(r.address.Bytes())},
			},
		})
		if err != nil {
			return found, fmt.Errorf("scan deposits %d to %d: %w", from, last, err)
		}
		for _, lg := range logs {
			if lg.Removed || len(lg.Topics) != 3 || len(lg.Data) < 32 {
				continue
			}
			sender := common.BytesToAddress(lg.Topics[1].Bytes())
			amount := new(big.Int).SetBytes(lg.Data[len(lg.Data)-32:])
			// Money the account sent itself is not incoming money.
			if sender == r.address || amount.Sign() == 0 {
				continue
			}
			d := Deposit{
				TxHash: lg.TxHash, LogIndex: lg.Index, From: sender,
				Amount: amount, BlockNumber: lg.BlockNumber,
			}
			if err := r.store.PutDeposit(r.address, d); err != nil {
				return found, err
			}
			found = append(found, d)
		}
		if err := r.store.PutCursor(r.address, last); err != nil {
			return found, err
		}
		from = last + 1
	}
	return found, nil
}

// Deposits lists every deposit recorded so far.
func (r *Rail) Deposits() ([]Deposit, error) { return r.store.Deposits(r.address) }
