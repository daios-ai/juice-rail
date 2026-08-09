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
	// StatusUnknown means no record of the identifier.
	StatusUnknown Status = "unknown"
	// StatusPending means the intent may still execute. A revert is not
	// failure: a reverted intent can be retried.
	StatusPending Status = "pending"
	// StatusConfirmed means a finalized rail event matching the terms exists.
	StatusConfirmed Status = "confirmed"
	// StatusFailed means the intent can never execute: the identifier is
	// finalized under different terms, or it was abandoned and every signed
	// attempt is finalized-dead.
	StatusFailed Status = "failed"
)

// ChainReader is the read side of a chain node. *ethclient.Client satisfies it.
type ChainReader interface {
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error)
	CallContract(ctx context.Context, call ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)
}

// Event signatures emitted by JuiceRail. The identifier is topic 1 on all
// three, so one query by identifier covers every kind.
var (
	topicDeposited = crypto.Keccak256Hash([]byte("Deposited(bytes32,address,uint256)"))
	topicSettled   = crypto.Keccak256Hash([]byte("Settled(bytes32,address,address,uint256)"))
	topicWithdrawn = crypto.Keccak256Hash([]byte("Withdrawn(bytes32,address,address,uint256)"))
)

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

// finalizedBlockArg selects the "finalized" tag (go-ethereum encodes the RPC
// block tags as negative numbers).
var finalizedBlockArg = big.NewInt(-3)

// Status derives the status of an intent from finalized chain facts.
func (r *Rail) Status(ctx context.Context, id ID) (Status, error) {
	in, ok, err := r.store.Intent(id)
	if err != nil {
		return StatusUnknown, err
	}
	if !ok {
		return StatusUnknown, nil
	}
	if f, ok, err := r.store.Fact(id); err != nil {
		return StatusUnknown, err
	} else if ok {
		return statusOfFact(f), nil
	}

	head, err := r.finalizedHeader(ctx)
	if err != nil {
		return StatusUnknown, err
	}

	f, found, err := r.finalizedOutcome(ctx, id, in, head.Number)
	if err != nil {
		return StatusUnknown, err
	}
	if found {
		if err := r.store.PutFact(id, f); err != nil {
			return StatusUnknown, err
		}
		return statusOfFact(f), nil
	}

	dead, err := r.terminallyDead(ctx, id, head)
	if err != nil {
		return StatusUnknown, err
	}
	if dead {
		return StatusFailed, nil
	}
	return StatusPending, nil
}

func statusOfFact(f Fact) Status {
	if f.Executed {
		return StatusConfirmed
	}
	return StatusFailed
}

// finalizedOutcome looks for a finalized event carrying this identifier,
// regardless of which transaction carried it: an exact replay executes
// nothing and emits nothing, so status must follow the identifier.
func (r *Rail) finalizedOutcome(ctx context.Context, id ID, in Intent, finalized *big.Int) (Fact, bool, error) {
	logs, err := r.chain.FilterLogs(ctx, ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(in.FromBlock),
		ToBlock:   finalized,
		Addresses: []common.Address{r.domain.Rail},
		Topics: [][]common.Hash{
			{topicDeposited, topicSettled, topicWithdrawn},
			{id.Hash()},
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
		return Fact{
			TxHash:      lg.TxHash,
			BlockNumber: lg.BlockNumber,
			Executed:    got.Equal(in.Terms),
		}, true, nil
	}
	return Fact{}, false, nil
}

// terminallyDead reports whether nothing can ever execute this intent: the
// rail will never sign again and every attempt already signed is finalized-dead.
func (r *Rail) terminallyDead(ctx context.Context, id ID, head *types.Header) (bool, error) {
	abandoned, err := r.store.Abandoned(id)
	if err != nil || !abandoned {
		return false, err
	}
	ops, err := r.store.SignedOps(id)
	if err != nil {
		return false, err
	}
	for _, op := range ops {
		if !opDead(op, head) {
			return false, nil
		}
	}
	return true, nil
}

// opDead reports whether a signed attempt can never execute. Sponsorship
// expiry is the whole test: block timestamps only increase, so once the
// finalized head is past validUntil no block can ever validate it again. The
// test reads a header and no state, so it holds on any node.
func opDead(op SignedOp, head *types.Header) bool {
	return head.Time > op.ValidUntil
}

func decodeTerms(lg types.Log) (Terms, error) {
	if len(lg.Topics) == 0 {
		return Terms{}, fmt.Errorf("rail log: no topics")
	}
	switch lg.Topics[0] {
	case topicDeposited:
		if len(lg.Topics) != 3 || len(lg.Data) != 32 {
			return Terms{}, fmt.Errorf("rail log: malformed Deposited")
		}
		return Terms{
			Kind:    KindDeposit,
			Account: common.BytesToAddress(lg.Topics[2].Bytes()),
			Amount:  new(big.Int).SetBytes(lg.Data),
		}, nil
	case topicSettled:
		if len(lg.Topics) != 4 || len(lg.Data) != 32 {
			return Terms{}, fmt.Errorf("rail log: malformed Settled")
		}
		return Terms{
			Kind:    KindSettle,
			Account: common.BytesToAddress(lg.Topics[2].Bytes()),
			Party:   common.BytesToAddress(lg.Topics[3].Bytes()),
			Amount:  new(big.Int).SetBytes(lg.Data),
		}, nil
	case topicWithdrawn:
		if len(lg.Topics) != 3 || len(lg.Data) != 64 {
			return Terms{}, fmt.Errorf("rail log: malformed Withdrawn")
		}
		return Terms{
			Kind:    KindWithdraw,
			Account: common.BytesToAddress(lg.Topics[2].Bytes()),
			Party:   common.BytesToAddress(lg.Data[:32]),
			Amount:  new(big.Int).SetBytes(lg.Data[32:]),
		}, nil
	default:
		return Terms{}, fmt.Errorf("rail log: unknown event %s", lg.Topics[0])
	}
}
