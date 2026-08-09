package rail

import (
	"errors"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// Intent is the write-ahead record: the decision to pursue an operation,
// durable before any signature exists.
type Intent struct {
	Terms Terms
	// FromBlock bounds the log search for this intent's event. It is the chain
	// head observed when the intent was recorded.
	FromBlock uint64
}

// SignedOp is one signed attempt. Attempts are appended, never edited: a new
// one may be signed only once the previous can never execute.
type SignedOp struct {
	// Op is the marshalled user operation, opaque to the store.
	Op []byte
	// Hash identifies the operation to the bundler.
	Hash common.Hash
	// Nonce is the EntryPoint nonce this attempt consumes.
	Nonce *big.Int
	// ValidUntil is when its gas sponsorship expires.
	ValidUntil uint64
}

// Fact records a finalized chain outcome. Finalized facts cannot change, so
// this is a cache rather than authority.
type Fact struct {
	TxHash      common.Hash
	BlockNumber uint64
	// Executed is false when the identifier was finalized under other terms.
	Executed bool
}

var (
	// ErrIntentConflict is returned when an identifier is already recorded
	// with different terms.
	ErrIntentConflict = errors.New("rail: identifier already recorded with different terms")
	// ErrAbandoned is returned when signing is attempted after abandonment.
	ErrAbandoned = errors.New("rail: identifier abandoned")
	// ErrNoIntent is returned when an operation is referenced before its
	// write-ahead record exists.
	ErrNoIntent = errors.New("rail: no intent recorded")
	// ErrWrongDomain is returned when a store bound to one domain is opened
	// for another.
	ErrWrongDomain = errors.New("rail: store belongs to a different domain")
)

// DomainKey names the domain a store serves: the chain and the deployment on
// it. Identifiers are unique only within one of these.
func DomainKey(chainID *big.Int, railAddr common.Address) string {
	chain := "0"
	if chainID != nil {
		chain = chainID.String()
	}
	return chain + ":" + strings.ToLower(railAddr.Hex())
}

// Store holds the rail's durable state: four records per identifier, each
// write-once or append-only. Nothing is ever edited in place.
//
// The rail's own decisions — intent, signed ops, abandonment — are
// authoritative. Facts are cached observations of a chain that has finalized.
//
// A store serves exactly one domain. Identifiers are only unique within a
// domain, so a store shared between two would alias their intents and their
// finalized facts. An implementation must bind itself to a domain and refuse
// to be reopened under another; see DomainKey.
type Store interface {
	// PutIntent records an intent. Recording the same terms twice succeeds;
	// different terms return ErrIntentConflict.
	PutIntent(id ID, in Intent) error
	Intent(id ID) (Intent, bool, error)

	// AppendSignedOp appends an attempt. It must fail with ErrAbandoned if the
	// identifier is abandoned, atomically with respect to Abandon, so that a
	// release can never race a fresh signature.
	AppendSignedOp(id ID, op SignedOp) error
	SignedOps(id ID) ([]SignedOp, error)

	// Abandon records "never sign this identifier again". It is idempotent and
	// stops future signing; it kills nothing already signed.
	Abandon(id ID) error
	Abandoned(id ID) (bool, error)

	// PutFact caches a finalized outcome. Idempotent: facts cannot change.
	PutFact(id ID, f Fact) error
	Fact(id ID) (Fact, bool, error)

	// PendingIDs lists identifiers with no cached fact, so a restart can
	// resume watching them.
	PendingIDs() ([]ID, error)

	Close() error
}
