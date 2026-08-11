package rail

import (
	"errors"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// Intent is the write-ahead record: the decision to pursue an operation,
// durable before any signature exists. It fixes the core terms, so every
// variant of the intent moves the same money between the same parties.
type Intent struct {
	Terms Terms
	// FromBlock bounds the log search for this intent's event. It is the chain
	// head observed when the intent was recorded.
	FromBlock uint64
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
	// with different core terms.
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

// Store holds the rail's durable state: four records per intent, each
// write-once or append-only. Nothing is ever edited in place.
//
// The rail's own decisions — intent, signed variants, abandonment — are
// authoritative. Facts are cached observations of a chain that has finalized.
//
// Records are keyed by (account, identifier), exactly as the contract keys its
// bindings, so one store may serve several accounts on one domain.
//
// A store serves exactly one domain. Identifiers are only unique within a
// domain, so a store shared between two would alias their intents and their
// finalized facts. An implementation must bind itself to a domain and refuse
// to be reopened under another; see DomainKey.
type Store interface {
	// PutIntent records an intent. Recording the same core terms twice
	// succeeds; different terms return ErrIntentConflict.
	PutIntent(ref Ref, in Intent) error
	Intent(ref Ref) (Intent, bool, error)

	// AppendVariant appends a signed variant. It must fail with ErrAbandoned if
	// the intent is abandoned, atomically with respect to Abandon, so that a
	// release can never race a fresh signature.
	AppendVariant(ref Ref, v Variant) error
	Variants(ref Ref) ([]Variant, error)

	// Abandon records "never sign this intent again". It is idempotent and
	// stops future signing; it kills nothing already signed.
	Abandon(ref Ref) error
	Abandoned(ref Ref) (bool, error)

	// PutFact caches a finalized outcome. Idempotent: facts cannot change.
	PutFact(ref Ref, f Fact) error
	Fact(ref Ref) (Fact, bool, error)

	// Pending lists intents with no cached fact, so a restart can resume
	// watching them.
	Pending() ([]Ref, error)

	Close() error
}
