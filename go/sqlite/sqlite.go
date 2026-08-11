// Package sqlite is the shipped Store implementation. A host that already has
// a database may implement rail.Store over it instead, so that rail records
// commit atomically with its own.
package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	_ "modernc.org/sqlite"

	"github.com/daios-ai/juice-rail/go/rail"
)

// Records are keyed by (account, identifier), exactly as the contract keys its
// bindings, so one store may serve several accounts on one domain.
const schema = `
CREATE TABLE IF NOT EXISTS intents (
  account    BLOB    NOT NULL,
  id         BLOB    NOT NULL,
  kind       INTEGER NOT NULL,
  party      BLOB    NOT NULL,
  amount     TEXT    NOT NULL,
  from_block INTEGER NOT NULL,
  PRIMARY KEY (account, id)
);
CREATE TABLE IF NOT EXISTS variants (
  account BLOB    NOT NULL,
  id      BLOB    NOT NULL,
  seq     INTEGER NOT NULL,
  blob    BLOB    NOT NULL,
  PRIMARY KEY (account, id, seq)
);
CREATE TABLE IF NOT EXISTS abandoned (
  account BLOB NOT NULL,
  id      BLOB NOT NULL,
  PRIMARY KEY (account, id)
);
CREATE TABLE IF NOT EXISTS facts (
  account      BLOB    NOT NULL,
  id           BLOB    NOT NULL,
  tx_hash      BLOB    NOT NULL,
  block_number INTEGER NOT NULL,
  executed     INTEGER NOT NULL,
  PRIMARY KEY (account, id)
);
CREATE TABLE IF NOT EXISTS domain (
  only_row INTEGER PRIMARY KEY CHECK (only_row = 1),
  key      TEXT NOT NULL
);
`

// Store keeps the rail's four record kinds. Every row is written once and
// never edited, so a crash can only lose the very last write, never corrupt an
// earlier decision.
type Store struct {
	db *sql.DB
}

// Open creates or opens a store for one domain. Writes are durable before they
// are reported, which is what makes "signed before submitted" hold across a
// kill.
//
// The domain is recorded on first use. Reopening the same file for a different
// domain fails: identifiers are unique only within a domain, so sharing a
// store between two would alias their intents and their finalized facts.
func Open(path string, domain string) (*Store, error) {
	if domain == "" {
		return nil, fmt.Errorf("open store: no domain given")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	// One writer at a time keeps the append/abandon race serialisable.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	if err := bindDomain(db, domain); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// bindDomain claims the store for a domain, or checks the claim already made.
func bindDomain(db *sql.DB, domain string) error {
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO domain (only_row, key) VALUES (1, ?)`, domain); err != nil {
		return fmt.Errorf("bind domain: %w", err)
	}
	var bound string
	if err := db.QueryRow(`SELECT key FROM domain WHERE only_row = 1`).Scan(&bound); err != nil {
		return fmt.Errorf("bind domain: %w", err)
	}
	if bound != domain {
		return fmt.Errorf("%w: store serves %s, opened for %s", rail.ErrWrongDomain, bound, domain)
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func key(ref rail.Ref) (account []byte, id []byte) {
	return ref.Account.Bytes(), ref.ID[:]
}

// PutIntent records the write-ahead intent. Re-recording identical core terms
// is a no-op; different terms are a conflict, never an overwrite.
//
// The insert comes first and the comparison second, inside one transaction:
// reading before writing would let two callers recording the same intent both
// find it absent, and the loser would be refused for terms it agreed with.
func (s *Store) PutIntent(ref rail.Ref, in rail.Intent) error {
	if in.Terms.Account != ref.Account {
		return fmt.Errorf("record intent: terms debit %s, reference is %s", in.Terms.Account, ref.Account)
	}
	account, id := key(ref)

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("record intent: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO intents (account, id, kind, party, amount, from_block) VALUES (?, ?, ?, ?, ?, ?)`,
		account, id, int64(in.Terms.Kind), in.Terms.Party.Bytes(), in.Terms.Amount.String(), int64(in.FromBlock),
	); err != nil {
		return fmt.Errorf("record intent: %w", err)
	}

	var (
		kind   int64
		party  []byte
		amount string
	)
	if err := tx.QueryRow(
		`SELECT kind, party, amount FROM intents WHERE account = ? AND id = ?`, account, id,
	).Scan(&kind, &party, &amount); err != nil {
		return fmt.Errorf("record intent: %w", err)
	}
	value, ok := new(big.Int).SetString(amount, 10)
	if !ok {
		return fmt.Errorf("record intent %s: bad amount %q", ref, amount)
	}
	recorded := rail.Terms{
		Kind:    rail.Kind(kind),
		Account: ref.Account,
		Party:   common.BytesToAddress(party),
		Amount:  value,
	}
	if !recorded.Equal(in.Terms) {
		return fmt.Errorf("%w: %s", rail.ErrIntentConflict, ref)
	}
	return tx.Commit()
}

func (s *Store) Intent(ref rail.Ref) (rail.Intent, bool, error) {
	account, id := key(ref)
	var (
		kind      int64
		party     []byte
		amount    string
		fromBlock int64
	)
	err := s.db.QueryRow(
		`SELECT kind, party, amount, from_block FROM intents WHERE account = ? AND id = ?`, account, id,
	).Scan(&kind, &party, &amount, &fromBlock)
	if errors.Is(err, sql.ErrNoRows) {
		return rail.Intent{}, false, nil
	}
	if err != nil {
		return rail.Intent{}, false, fmt.Errorf("read intent: %w", err)
	}
	value, ok := new(big.Int).SetString(amount, 10)
	if !ok {
		return rail.Intent{}, false, fmt.Errorf("read intent %s: bad amount %q", ref, amount)
	}
	return rail.Intent{
		Terms: rail.Terms{
			Kind:    rail.Kind(kind),
			Account: ref.Account,
			Party:   common.BytesToAddress(party),
			Amount:  value,
		},
		FromBlock: uint64(fromBlock),
	}, true, nil
}

// AppendVariant appends a signed variant, refusing once the intent is
// abandoned. The check and the insert share one transaction, so a release can
// never race a fresh signature: either the variant is recorded before
// abandonment, or it is refused.
func (s *Store) AppendVariant(ref rail.Ref, v rail.Variant) error {
	blob, err := rail.MarshalVariant(v)
	if err != nil {
		return err
	}
	account, id := key(ref)

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("append variant: %w", err)
	}
	defer tx.Rollback()

	var abandoned int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM abandoned WHERE account = ? AND id = ?`, account, id).Scan(&abandoned); err != nil {
		return fmt.Errorf("append variant: %w", err)
	}
	if abandoned != 0 {
		return fmt.Errorf("%w: %s", rail.ErrAbandoned, ref)
	}

	var next int64
	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(seq), -1) + 1 FROM variants WHERE account = ? AND id = ?`, account, id,
	).Scan(&next); err != nil {
		return fmt.Errorf("append variant: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO variants (account, id, seq, blob) VALUES (?, ?, ?, ?)`, account, id, next, blob,
	); err != nil {
		return fmt.Errorf("append variant: %w", err)
	}
	return tx.Commit()
}

func (s *Store) Variants(ref rail.Ref) ([]rail.Variant, error) {
	account, id := key(ref)
	rows, err := s.db.Query(
		`SELECT blob FROM variants WHERE account = ? AND id = ? ORDER BY seq`, account, id)
	if err != nil {
		return nil, fmt.Errorf("read variants: %w", err)
	}
	defer rows.Close()

	var variants []rail.Variant
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return nil, fmt.Errorf("read variants: %w", err)
		}
		v, err := rail.UnmarshalVariant(blob)
		if err != nil {
			return nil, fmt.Errorf("read variants: %w", err)
		}
		variants = append(variants, v)
	}
	return variants, rows.Err()
}

func (s *Store) Abandon(ref rail.Ref) error {
	account, id := key(ref)
	if _, err := s.db.Exec(
		`INSERT OR IGNORE INTO abandoned (account, id) VALUES (?, ?)`, account, id); err != nil {
		return fmt.Errorf("abandon: %w", err)
	}
	return nil
}

func (s *Store) Abandoned(ref rail.Ref) (bool, error) {
	account, id := key(ref)
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM abandoned WHERE account = ? AND id = ?`, account, id).Scan(&n); err != nil {
		return false, fmt.Errorf("read abandonment: %w", err)
	}
	return n != 0, nil
}

// PutFact caches a finalized outcome. Finalized facts cannot change, so a
// repeat write is ignored rather than applied.
func (s *Store) PutFact(ref rail.Ref, f rail.Fact) error {
	account, id := key(ref)
	executed := 0
	if f.Executed {
		executed = 1
	}
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO facts (account, id, tx_hash, block_number, executed) VALUES (?, ?, ?, ?, ?)`,
		account, id, f.TxHash.Bytes(), int64(f.BlockNumber), executed,
	)
	if err != nil {
		return fmt.Errorf("record fact: %w", err)
	}
	return nil
}

func (s *Store) Fact(ref rail.Ref) (rail.Fact, bool, error) {
	account, id := key(ref)
	var (
		txHash   []byte
		block    int64
		executed int64
	)
	err := s.db.QueryRow(
		`SELECT tx_hash, block_number, executed FROM facts WHERE account = ? AND id = ?`, account, id,
	).Scan(&txHash, &block, &executed)
	if errors.Is(err, sql.ErrNoRows) {
		return rail.Fact{}, false, nil
	}
	if err != nil {
		return rail.Fact{}, false, fmt.Errorf("read fact: %w", err)
	}
	return rail.Fact{
		TxHash:      common.BytesToHash(txHash),
		BlockNumber: uint64(block),
		Executed:    executed != 0,
	}, true, nil
}

// Pending lists intents with no finalized outcome yet, so a restart knows what
// to keep watching.
func (s *Store) Pending() ([]rail.Ref, error) {
	rows, err := s.db.Query(
		`SELECT i.account, i.id FROM intents i
         WHERE NOT EXISTS (SELECT 1 FROM facts f WHERE f.account = i.account AND f.id = i.id)
         ORDER BY i.rowid`)
	if err != nil {
		return nil, fmt.Errorf("read pending: %w", err)
	}
	defer rows.Close()

	var refs []rail.Ref
	for rows.Next() {
		var account, id []byte
		if err := rows.Scan(&account, &id); err != nil {
			return nil, fmt.Errorf("read pending: %w", err)
		}
		if len(id) != 32 {
			return nil, fmt.Errorf("read pending: identifier is %d bytes", len(id))
		}
		refs = append(refs, rail.Ref{Account: common.BytesToAddress(account), ID: rail.ID(id)})
	}
	return refs, rows.Err()
}
