package rail

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// Status is a property of the intent, never of one submission.
type Status string

const (
	// StatusUnknown means no record of the intent.
	StatusUnknown Status = "unknown"
	// StatusPending means the intent may still execute. A revert is not
	// failure: a reverted intent can be retried.
	StatusPending Status = "pending"
	// StatusConfirmed means a finalized rail event matching the core terms
	// exists.
	StatusConfirmed Status = "confirmed"
	// StatusFailed means the intent can never execute: the identifier is
	// finalized under different terms, or it was abandoned and every signed
	// variant is finalized-dead.
	StatusFailed Status = "failed"
)

// Event signatures emitted by JuiceRail. All three carry the debited account
// as topic 1 and the identifier as topic 2, so one query by (account,
// identifier) covers every kind.
var (
	topicDeposited   = crypto.Keccak256Hash([]byte("Deposited(address,bytes32,address,uint256,uint256,address,uint256)"))
	topicTransferred = crypto.Keccak256Hash([]byte("Transferred(address,bytes32,address,uint256,uint256,address,uint256)"))
	topicWithdrawn   = crypto.Keccak256Hash([]byte("Withdrawn(address,bytes32,address,uint256,uint256,address,uint256)"))
)

// finalizedBlockArg selects the "finalized" tag (go-ethereum encodes the RPC
// block tags as negative numbers).
var finalizedBlockArg = big.NewInt(-3)

// classify is the whole decision procedure, over facts the caller has already
// gathered. Keeping it separate from how they are gathered is what lets the
// state machine be checked exhaustively; see model_test.go.
func classify(known bool, fact *Fact, abandoned, everyVariantDead bool) Status {
	switch {
	case !known:
		return StatusUnknown
	case fact != nil && fact.Executed:
		return StatusConfirmed
	case fact != nil:
		return StatusFailed
	case abandoned && everyVariantDead:
		return StatusFailed
	default:
		return StatusPending
	}
}

// variantDead reports whether a signed variant can never execute. The deadline
// is the whole test: block timestamps only increase, so once the head is at or
// past validBefore no block can ever include it. The contract's own check is
// `block.timestamp < validBefore`, which is this test's mirror image.
//
// Callers judging death durably must pass a finalized head: an unfinalized one
// can still be reorganised away.
func variantDead(v Variant, headTime uint64) bool {
	return headTime >= v.ValidBefore
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

// Status derives the status of an intent from finalized chain facts.
func (r *Rail) Status(ctx context.Context, id ID) (Status, error) {
	ref := r.ref(id)
	in, ok, err := r.store.Intent(ref)
	if err != nil {
		return StatusUnknown, err
	}
	if !ok {
		return StatusUnknown, nil
	}
	if f, cached, err := r.store.Fact(ref); err != nil {
		return StatusUnknown, err
	} else if cached {
		return classify(true, &f, false, false), nil
	}

	head, err := r.finalizedHeader(ctx)
	if err != nil {
		return StatusUnknown, err
	}
	f, found, err := r.finalizedOutcome(ctx, ref, in, head.Number)
	if err != nil {
		return StatusUnknown, err
	}
	if found {
		if err := r.store.PutFact(ref, f); err != nil {
			return StatusUnknown, err
		}
		return classify(true, &f, false, false), nil
	}

	abandoned, err := r.store.Abandoned(ref)
	if err != nil {
		return StatusUnknown, err
	}
	dead, err := r.everyVariantDead(ref, head)
	if err != nil {
		return StatusUnknown, err
	}
	return classify(true, nil, abandoned, dead), nil
}

// finalizedOutcome looks for a finalized event under this (account,
// identifier), regardless of which transaction carried it: a replay executes
// nothing and emits nothing, and any variant of the intent confirms it.
func (r *Rail) finalizedOutcome(ctx context.Context, ref Ref, in Intent, finalized *big.Int) (Fact, bool, error) {
	logs, err := r.chain.FilterLogs(ctx, ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(in.FromBlock),
		ToBlock:   finalized,
		Addresses: []common.Address{r.domain.Rail},
		Topics: [][]common.Hash{
			{topicDeposited, topicTransferred, topicWithdrawn},
			{common.BytesToHash(ref.Account.Bytes())},
			{ref.ID.Hash()},
		},
	})
	if err != nil {
		return Fact{}, false, fmt.Errorf("filter rail logs: %w", err)
	}
	for _, lg := range logs {
		if lg.Removed {
			continue
		}
		got, err := decodeTerms(lg)
		if err != nil {
			return Fact{}, false, err
		}
		// Variants differ in relayer, fee and deadline; only the core terms
		// decide whether this is the intent the host is waiting for.
		return Fact{
			TxHash:      lg.TxHash,
			BlockNumber: lg.BlockNumber,
			Executed:    got.Equal(in.Terms),
		}, true, nil
	}
	return Fact{}, false, nil
}

// everyVariantDead reports whether no signed variant can execute any more.
func (r *Rail) everyVariantDead(ref Ref, head *types.Header) (bool, error) {
	variants, err := r.store.Variants(ref)
	if err != nil {
		return false, err
	}
	for _, v := range variants {
		if !variantDead(v, head.Time) {
			return false, nil
		}
	}
	return true, nil
}

func decodeTerms(lg types.Log) (Terms, error) {
	if len(lg.Topics) != 3 || len(lg.Data) != 160 {
		return Terms{}, fmt.Errorf("rail log: malformed event")
	}
	var kind Kind
	switch lg.Topics[0] {
	case topicDeposited:
		kind = KindDeposit
	case topicTransferred:
		kind = KindTransfer
	case topicWithdrawn:
		kind = KindWithdraw
	default:
		return Terms{}, fmt.Errorf("rail log: unknown event %s", lg.Topics[0])
	}
	return Terms{
		Kind:    kind,
		Account: common.BytesToAddress(lg.Topics[1].Bytes()),
		Party:   common.BytesToAddress(lg.Data[:32]),
		Amount:  new(big.Int).SetBytes(lg.Data[32:64]),
	}, nil
}
