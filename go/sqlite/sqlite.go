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

// Records are keyed by account, so one file may serve several profiles on one
// domain without aliasing them. Amounts are decimal text: exact, and no
// floating point can creep in.
const schema = `
CREATE TABLE IF NOT EXISTS intents (
  account    BLOB    NOT NULL,
  id         BLOB    NOT NULL,
  kind       INTEGER NOT NULL,
  dest       BLOB    NOT NULL,
  amount     TEXT    NOT NULL,
  delta      TEXT    NOT NULL,
  nonce      INTEGER NOT NULL,
  calldata   BLOB    NOT NULL,
  from_block INTEGER NOT NULL,
  PRIMARY KEY (account, id)
);
CREATE UNIQUE INDEX IF NOT EXISTS intents_nonce ON intents (account, nonce);
CREATE TABLE IF NOT EXISTS submissions (
  account BLOB    NOT NULL,
  id      BLOB    NOT NULL,
  seq     INTEGER NOT NULL,
  tx_hash BLOB    NOT NULL,
  gas     INTEGER NOT NULL,
  tip     TEXT    NOT NULL,
  fee_cap TEXT    NOT NULL,
  PRIMARY KEY (account, id, seq)
);
CREATE TABLE IF NOT EXISTS facts (
  account      BLOB    NOT NULL,
  id           BLOB    NOT NULL,
  tx_hash      BLOB    NOT NULL,
  block_number INTEGER NOT NULL,
  executed     INTEGER NOT NULL,
  PRIMARY KEY (account, id)
);
CREATE TABLE IF NOT EXISTS deposits (
  account      BLOB    NOT NULL,
  tx_hash      BLOB    NOT NULL,
  log_index    INTEGER NOT NULL,
  sender       BLOB    NOT NULL,
  amount       TEXT    NOT NULL,
  block_number INTEGER NOT NULL,
  PRIMARY KEY (account, tx_hash, log_index)
);
CREATE TABLE IF NOT EXISTS cursors (
  account        BLOB PRIMARY KEY,
  deposit_block  INTEGER,
  nonce_floor    INTEGER
);
CREATE TABLE IF NOT EXISTS domain (
  only_row INTEGER PRIMARY KEY CHECK (only_row = 1),
  key      TEXT NOT NULL
);
`

// Store keeps the rail's durable records. Every row is written once or
// appended, never edited, so a crash can only lose the very last write.
type Store struct {
	db *sql.DB
}

// Open creates or opens a store for one domain. Writes are durable before they
// are reported, which is what makes "recorded before broadcast" hold across a
// kill.
//
// The domain is recorded on first use. Reopening the same file for a different
// domain fails: identifiers are unique only within a domain, so sharing a
// store between two would alias their intents.
func Open(path string, domain string) (*Store, error) {
	if domain == "" {
		return nil, fmt.Errorf("open store: no domain given")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	// One writer at a time keeps every read-then-write serialisable.
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

// PutIntent records the write-ahead intent. Re-recording the same operation is
// a no-op; different terms are a conflict, never an overwrite.
//
// The insert comes first and the comparison second, inside one transaction:
// reading before writing would let two callers recording the same intent both
// find it absent, and the loser would be refused for terms it agreed with.
func (s *Store) PutIntent(account common.Address, in rail.Intent) error {
	if err := in.Validate(); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("record intent: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO intents (account, id, kind, dest, amount, delta, nonce, calldata, from_block)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		account.Bytes(), in.ID[:], int64(in.Kind), in.To.Bytes(),
		in.Amount.String(), deltaText(in.Delta), int64(in.Nonce), in.Calldata, int64(in.FromBlock),
	); err != nil {
		return fmt.Errorf("record intent: %w", err)
	}
	recorded, err := scanIntent(tx.QueryRow(
		`SELECT id, kind, dest, amount, delta, nonce, calldata, from_block FROM intents
         WHERE account = ? AND id = ?`, account.Bytes(), in.ID[:]))
	if errors.Is(err, sql.ErrNoRows) {
		// The insert was ignored and the identifier is absent, so the unique
		// nonce index refused it: one nonce carries one operation.
		return fmt.Errorf("%w: nonce %d already carries another operation", rail.ErrIntentConflict, in.Nonce)
	}
	if err != nil {
		return fmt.Errorf("record intent: %w", err)
	}
	if !recorded.SameTerms(in) {
		return fmt.Errorf("%w: %s", rail.ErrIntentConflict, in.ID)
	}
	return tx.Commit()
}

func (s *Store) Intent(account common.Address, id rail.ID) (rail.Intent, bool, error) {
	in, err := scanIntent(s.db.QueryRow(
		`SELECT id, kind, dest, amount, delta, nonce, calldata, from_block FROM intents
         WHERE account = ? AND id = ?`, account.Bytes(), id[:]))
	if errors.Is(err, sql.ErrNoRows) {
		return rail.Intent{}, false, nil
	}
	if err != nil {
		return rail.Intent{}, false, fmt.Errorf("read intent: %w", err)
	}
	return in, true, nil
}

func (s *Store) IntentByNonce(account common.Address, nonce uint64) (rail.Intent, bool, error) {
	in, err := scanIntent(s.db.QueryRow(
		`SELECT id, kind, dest, amount, delta, nonce, calldata, from_block FROM intents
         WHERE account = ? AND nonce = ?`, account.Bytes(), int64(nonce)))
	if errors.Is(err, sql.ErrNoRows) {
		return rail.Intent{}, false, nil
	}
	if err != nil {
		return rail.Intent{}, false, fmt.Errorf("read intent: %w", err)
	}
	return in, true, nil
}

// Pending lists intents with no settled outcome yet, so a restart knows what
// it still has to resolve.
func (s *Store) Pending(account common.Address) ([]rail.Intent, error) {
	rows, err := s.db.Query(
		`SELECT i.id, i.kind, i.dest, i.amount, i.delta, i.nonce, i.calldata, i.from_block
         FROM intents i
         WHERE i.account = ?
           AND NOT EXISTS (SELECT 1 FROM facts f WHERE f.account = i.account AND f.id = i.id)
         ORDER BY i.nonce`, account.Bytes())
	if err != nil {
		return nil, fmt.Errorf("read pending: %w", err)
	}
	defer rows.Close()

	var out []rail.Intent
	for rows.Next() {
		in, err := scanIntent(rows)
		if err != nil {
			return nil, fmt.Errorf("read pending: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// AppendSubmission records a signed attempt. Attempts are appended, never
// replaced, so the record of what may exist on chain only ever grows.
func (s *Store) AppendSubmission(account common.Address, id rail.ID, sub rail.Submission) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("record submission: %w", err)
	}
	defer tx.Rollback()

	var next int64
	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(seq), -1) + 1 FROM submissions WHERE account = ? AND id = ?`,
		account.Bytes(), id[:]).Scan(&next); err != nil {
		return fmt.Errorf("record submission: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO submissions (account, id, seq, tx_hash, gas, tip, fee_cap) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		account.Bytes(), id[:], next, sub.TxHash.Bytes(), int64(sub.Gas),
		amountText(sub.Tip), amountText(sub.FeeCap),
	); err != nil {
		return fmt.Errorf("record submission: %w", err)
	}
	return tx.Commit()
}

func (s *Store) Submissions(account common.Address, id rail.ID) ([]rail.Submission, error) {
	rows, err := s.db.Query(
		`SELECT tx_hash, gas, tip, fee_cap FROM submissions WHERE account = ? AND id = ? ORDER BY seq`,
		account.Bytes(), id[:])
	if err != nil {
		return nil, fmt.Errorf("read submissions: %w", err)
	}
	defer rows.Close()

	var out []rail.Submission
	for rows.Next() {
		var (
			hash   []byte
			gas    int64
			tip    string
			feeCap string
		)
		if err := rows.Scan(&hash, &gas, &tip, &feeCap); err != nil {
			return nil, fmt.Errorf("read submissions: %w", err)
		}
		t, err := amountValue(tip)
		if err != nil {
			return nil, err
		}
		f, err := amountValue(feeCap)
		if err != nil {
			return nil, err
		}
		out = append(out, rail.Submission{
			TxHash: common.BytesToHash(hash), Gas: uint64(gas), Tip: t, FeeCap: f,
		})
	}
	return out, rows.Err()
}

// PutFact caches a settled outcome. Settled facts cannot change, so a
// repeat write is ignored rather than applied.
func (s *Store) PutFact(account common.Address, id rail.ID, f rail.Fact) error {
	executed := 0
	if f.Executed {
		executed = 1
	}
	if _, err := s.db.Exec(
		`INSERT OR IGNORE INTO facts (account, id, tx_hash, block_number, executed) VALUES (?, ?, ?, ?, ?)`,
		account.Bytes(), id[:], f.TxHash.Bytes(), int64(f.BlockNumber), executed,
	); err != nil {
		return fmt.Errorf("record fact: %w", err)
	}
	return nil
}

func (s *Store) Fact(account common.Address, id rail.ID) (rail.Fact, bool, error) {
	var (
		hash     []byte
		block    int64
		executed int64
	)
	err := s.db.QueryRow(
		`SELECT tx_hash, block_number, executed FROM facts WHERE account = ? AND id = ?`,
		account.Bytes(), id[:]).Scan(&hash, &block, &executed)
	if errors.Is(err, sql.ErrNoRows) {
		return rail.Fact{}, false, nil
	}
	if err != nil {
		return rail.Fact{}, false, fmt.Errorf("read fact: %w", err)
	}
	return rail.Fact{
		TxHash:      common.BytesToHash(hash),
		BlockNumber: uint64(block),
		Executed:    executed != 0,
	}, true, nil
}

// PutDeposit records one settled incoming transfer. The log that carried it
// is its identity, so recording it twice changes nothing.
func (s *Store) PutDeposit(account common.Address, d rail.Deposit) error {
	if _, err := s.db.Exec(
		`INSERT OR IGNORE INTO deposits (account, tx_hash, log_index, sender, amount, block_number)
         VALUES (?, ?, ?, ?, ?, ?)`,
		account.Bytes(), d.TxHash.Bytes(), int64(d.LogIndex), d.From.Bytes(),
		amountText(d.Amount), int64(d.BlockNumber),
	); err != nil {
		return fmt.Errorf("record deposit: %w", err)
	}
	return nil
}

func (s *Store) Deposits(account common.Address) ([]rail.Deposit, error) {
	rows, err := s.db.Query(
		`SELECT tx_hash, log_index, sender, amount, block_number FROM deposits
         WHERE account = ? ORDER BY block_number, log_index`, account.Bytes())
	if err != nil {
		return nil, fmt.Errorf("read deposits: %w", err)
	}
	defer rows.Close()

	var out []rail.Deposit
	for rows.Next() {
		var (
			hash   []byte
			index  int64
			sender []byte
			amount string
			block  int64
		)
		if err := rows.Scan(&hash, &index, &sender, &amount, &block); err != nil {
			return nil, fmt.Errorf("read deposits: %w", err)
		}
		value, err := amountValue(amount)
		if err != nil {
			return nil, err
		}
		out = append(out, rail.Deposit{
			TxHash:      common.BytesToHash(hash),
			LogIndex:    uint(index),
			From:        common.BytesToAddress(sender),
			Amount:      value,
			BlockNumber: uint64(block),
		})
	}
	return out, rows.Err()
}

func (s *Store) Cursor(account common.Address) (uint64, bool, error) {
	return s.readCursor(account, "deposit_block")
}

// PutCursor advances deposit observation. It only moves forward: a cursor that
// went backwards would re-scan blocks already accounted for.
func (s *Store) PutCursor(account common.Address, block uint64) error {
	return s.advance(account, "deposit_block", block)
}

func (s *Store) NonceFloor(account common.Address) (uint64, bool, error) {
	return s.readCursor(account, "nonce_floor")
}

func (s *Store) PutNonceFloor(account common.Address, nonce uint64) error {
	return s.advance(account, "nonce_floor", nonce)
}

func (s *Store) readCursor(account common.Address, column string) (uint64, bool, error) {
	var value sql.NullInt64
	err := s.db.QueryRow(
		`SELECT `+column+` FROM cursors WHERE account = ?`, account.Bytes()).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read %s: %w", column, err)
	}
	if !value.Valid {
		return 0, false, nil
	}
	return uint64(value.Int64), true, nil
}

// advance moves a cursor forward and never back.
func (s *Store) advance(account common.Address, column string, value uint64) error {
	if _, err := s.db.Exec(
		`INSERT INTO cursors (account, `+column+`) VALUES (?, ?)
         ON CONFLICT (account) DO UPDATE SET `+column+` = MAX(COALESCE(`+column+`, 0), excluded.`+column+`)`,
		account.Bytes(), int64(value),
	); err != nil {
		return fmt.Errorf("advance %s: %w", column, err)
	}
	return nil
}

// scanner is whatever a row can be read from: one row or many.
type scanner interface{ Scan(dest ...any) error }

func scanIntent(row scanner) (rail.Intent, error) {
	var (
		id        []byte
		kind      int64
		dest      []byte
		amount    string
		delta     string
		nonce     int64
		calldata  []byte
		fromBlock int64
	)
	if err := row.Scan(&id, &kind, &dest, &amount, &delta, &nonce, &calldata, &fromBlock); err != nil {
		return rail.Intent{}, err
	}
	if len(id) != 32 {
		return rail.Intent{}, fmt.Errorf("intent identifier is %d bytes", len(id))
	}
	value, err := amountValue(amount)
	if err != nil {
		return rail.Intent{}, err
	}
	bought, err := amountValue(delta)
	if err != nil {
		return rail.Intent{}, err
	}
	return rail.Intent{
		ID:        rail.ID(id),
		Kind:      rail.Kind(kind),
		To:        common.BytesToAddress(dest),
		Amount:    value,
		Delta:     bought,
		Nonce:     uint64(nonce),
		Calldata:  calldata,
		FromBlock: uint64(fromBlock),
	}, nil
}

func amountText(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return v.String()
}

func deltaText(v *big.Int) string { return amountText(v) }

func amountValue(s string) (*big.Int, error) {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("stored amount %q is not a number", s)
	}
	return v, nil
}
