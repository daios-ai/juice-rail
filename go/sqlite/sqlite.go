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

const schema = `
CREATE TABLE IF NOT EXISTS intents (
  id         BLOB PRIMARY KEY,
  kind       INTEGER NOT NULL,
  account    BLOB    NOT NULL,
  party      BLOB    NOT NULL,
  amount     TEXT    NOT NULL,
  from_block INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS signed_ops (
  id          BLOB    NOT NULL,
  seq         INTEGER NOT NULL,
  op          BLOB    NOT NULL,
  hash        BLOB    NOT NULL,
  nonce       TEXT    NOT NULL,
  valid_until INTEGER NOT NULL,
  PRIMARY KEY (id, seq)
);
CREATE TABLE IF NOT EXISTS abandoned (
  id BLOB PRIMARY KEY
);
CREATE TABLE IF NOT EXISTS facts (
  id           BLOB PRIMARY KEY,
  tx_hash      BLOB    NOT NULL,
  block_number INTEGER NOT NULL,
  executed     INTEGER NOT NULL
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

// PutIntent records the write-ahead intent. Re-recording identical terms is a
// no-op; different terms are a conflict, never an overwrite.
func (s *Store) PutIntent(id rail.ID, in rail.Intent) error {
	existing, ok, err := s.Intent(id)
	if err != nil {
		return err
	}
	if ok {
		if !existing.Terms.Equal(in.Terms) {
			return fmt.Errorf("%w: %s", rail.ErrIntentConflict, id)
		}
		return nil
	}
	_, err = s.db.Exec(
		`INSERT INTO intents (id, kind, account, party, amount, from_block) VALUES (?, ?, ?, ?, ?, ?)`,
		id[:], int64(in.Terms.Kind), in.Terms.Account.Bytes(), in.Terms.Party.Bytes(),
		in.Terms.Amount.String(), int64(in.FromBlock),
	)
	if err != nil {
		return fmt.Errorf("record intent: %w", err)
	}
	return nil
}

func (s *Store) Intent(id rail.ID) (rail.Intent, bool, error) {
	var (
		kind      int64
		account   []byte
		party     []byte
		amount    string
		fromBlock int64
	)
	err := s.db.QueryRow(
		`SELECT kind, account, party, amount, from_block FROM intents WHERE id = ?`, id[:],
	).Scan(&kind, &account, &party, &amount, &fromBlock)
	if errors.Is(err, sql.ErrNoRows) {
		return rail.Intent{}, false, nil
	}
	if err != nil {
		return rail.Intent{}, false, fmt.Errorf("read intent: %w", err)
	}
	value, ok := new(big.Int).SetString(amount, 10)
	if !ok {
		return rail.Intent{}, false, fmt.Errorf("read intent %s: bad amount %q", id, amount)
	}
	return rail.Intent{
		Terms: rail.Terms{
			Kind:    rail.Kind(kind),
			Account: common.BytesToAddress(account),
			Party:   common.BytesToAddress(party),
			Amount:  value,
		},
		FromBlock: uint64(fromBlock),
	}, true, nil
}

// AppendSignedOp appends an attempt, refusing once the identifier is
// abandoned. The check and the insert share one transaction, so a release can
// never race a fresh signature: either the attempt is recorded before
// abandonment, or it is refused.
func (s *Store) AppendSignedOp(id rail.ID, op rail.SignedOp) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("append attempt: %w", err)
	}
	defer tx.Rollback()

	var abandoned int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM abandoned WHERE id = ?`, id[:]).Scan(&abandoned); err != nil {
		return fmt.Errorf("append attempt: %w", err)
	}
	if abandoned != 0 {
		return fmt.Errorf("%w: %s", rail.ErrAbandoned, id)
	}

	var next int64
	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(seq), -1) + 1 FROM signed_ops WHERE id = ?`, id[:],
	).Scan(&next); err != nil {
		return fmt.Errorf("append attempt: %w", err)
	}
	nonce := "0"
	if op.Nonce != nil {
		nonce = op.Nonce.String()
	}
	if _, err := tx.Exec(
		`INSERT INTO signed_ops (id, seq, op, hash, nonce, valid_until) VALUES (?, ?, ?, ?, ?, ?)`,
		id[:], next, op.Op, op.Hash.Bytes(), nonce, int64(op.ValidUntil),
	); err != nil {
		return fmt.Errorf("append attempt: %w", err)
	}
	return tx.Commit()
}

func (s *Store) SignedOps(id rail.ID) ([]rail.SignedOp, error) {
	rows, err := s.db.Query(
		`SELECT op, hash, nonce, valid_until FROM signed_ops WHERE id = ? ORDER BY seq`, id[:])
	if err != nil {
		return nil, fmt.Errorf("read attempts: %w", err)
	}
	defer rows.Close()

	var ops []rail.SignedOp
	for rows.Next() {
		var (
			blob       []byte
			hash       []byte
			nonce      string
			validUntil int64
		)
		if err := rows.Scan(&blob, &hash, &nonce, &validUntil); err != nil {
			return nil, fmt.Errorf("read attempts: %w", err)
		}
		value, ok := new(big.Int).SetString(nonce, 10)
		if !ok {
			return nil, fmt.Errorf("read attempts: bad nonce %q", nonce)
		}
		ops = append(ops, rail.SignedOp{
			Op:         blob,
			Hash:       common.BytesToHash(hash),
			Nonce:      value,
			ValidUntil: uint64(validUntil),
		})
	}
	return ops, rows.Err()
}

func (s *Store) Abandon(id rail.ID) error {
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO abandoned (id) VALUES (?)`, id[:]); err != nil {
		return fmt.Errorf("abandon: %w", err)
	}
	return nil
}

func (s *Store) Abandoned(id rail.ID) (bool, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM abandoned WHERE id = ?`, id[:]).Scan(&n); err != nil {
		return false, fmt.Errorf("read abandonment: %w", err)
	}
	return n != 0, nil
}

// PutFact caches a finalized outcome. Finalized facts cannot change, so a
// repeat write is ignored rather than applied.
func (s *Store) PutFact(id rail.ID, f rail.Fact) error {
	executed := 0
	if f.Executed {
		executed = 1
	}
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO facts (id, tx_hash, block_number, executed) VALUES (?, ?, ?, ?)`,
		id[:], f.TxHash.Bytes(), int64(f.BlockNumber), executed,
	)
	if err != nil {
		return fmt.Errorf("record fact: %w", err)
	}
	return nil
}

func (s *Store) Fact(id rail.ID) (rail.Fact, bool, error) {
	var (
		txHash   []byte
		block    int64
		executed int64
	)
	err := s.db.QueryRow(
		`SELECT tx_hash, block_number, executed FROM facts WHERE id = ?`, id[:],
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

// PendingIDs lists intents with no finalized outcome yet, so a restart knows
// what to keep watching.
func (s *Store) PendingIDs() ([]rail.ID, error) {
	rows, err := s.db.Query(
		`SELECT id FROM intents WHERE id NOT IN (SELECT id FROM facts) ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("read pending: %w", err)
	}
	defer rows.Close()

	var ids []rail.ID
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("read pending: %w", err)
		}
		if len(raw) != 32 {
			return nil, fmt.Errorf("read pending: identifier is %d bytes", len(raw))
		}
		ids = append(ids, rail.ID(raw))
	}
	return ids, rows.Err()
}
