package rail

import "github.com/ethereum/go-ethereum/common"

// Store holds the rail's durable state. Every record is written once or
// appended, never edited, so a crash can only lose the last write and never
// corrupt an earlier decision.
//
// The rail's own decisions — which intent owns which nonce — are
// authoritative. Facts, deposits and the two cursors are cached observations
// of a chain that has already finalized.
//
// A store serves exactly one domain. Identifiers are unique only within a
// domain, so a store shared between two would alias their intents. An
// implementation must bind itself to a domain and refuse to be reopened under
// another; see DomainKey.
type Store interface {
	// PutIntent records the write-ahead intent. Recording the same terms twice
	// succeeds; different terms return ErrIntentConflict. A second intent on
	// the same nonce is also a conflict: one nonce carries one operation.
	PutIntent(account common.Address, in Intent) error
	Intent(account common.Address, id ID) (Intent, bool, error)
	// IntentByNonce finds the intent that owns a nonce, which is how a
	// finalized nonce is matched back to a decision.
	IntentByNonce(account common.Address, nonce uint64) (Intent, bool, error)
	// Pending lists intents with no finalized fact, oldest first.
	Pending(account common.Address) ([]Intent, error)

	// AppendSubmission records a signed attempt before it is broadcast, so no
	// transaction can exist on chain that this account has no record of.
	AppendSubmission(account common.Address, id ID, s Submission) error
	Submissions(account common.Address, id ID) ([]Submission, error)

	// PutFact caches a finalized outcome. Idempotent: facts cannot change.
	PutFact(account common.Address, id ID, f Fact) error
	Fact(account common.Address, id ID) (Fact, bool, error)

	// PutDeposit records one finalized incoming transfer, identified by the log
	// that carried it. Idempotent.
	PutDeposit(account common.Address, d Deposit) error
	Deposits(account common.Address) ([]Deposit, error)

	// Cursor is how far deposit observation has reached. It only advances.
	Cursor(account common.Address) (uint64, bool, error)
	PutCursor(account common.Address, block uint64) error

	// NonceFloor is the nonce below which everything is reconciled. It only
	// advances.
	NonceFloor(account common.Address) (uint64, bool, error)
	PutNonceFloor(account common.Address, nonce uint64) error

	Close() error
}
