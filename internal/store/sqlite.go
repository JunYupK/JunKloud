// Package store persists JunKloud sessions, submissions, grading runs,
// recoverable operations and owned-resource inventories in SQLite.
//
// Only model types cross the package boundary; database/sql types stay
// inside. Kubernetes and Docker changes are never part of a database
// transaction: callers record an Operation first, change the outside world,
// then record the new phase, and recovery re-checks the real state.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
)

// Sentinel errors. Callers match them with errors.Is.
var (
	// ErrNotFound: the requested record (or a record it must reference) does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrAlreadyExists: a record with the same identity was already stored. Stored records are never overwritten.
	ErrAlreadyExists = errors.New("store: already exists")
	// ErrStateConflict: a compare-and-swap session transition found a state other than the expected one.
	ErrStateConflict = errors.New("store: session state conflict")
	// ErrInvalidState: a session state name is not one of model.SessionStates.
	ErrInvalidState = errors.New("store: invalid session state")
	// ErrOperationMismatch: PutOperation tried to change an existing operation's identity fields.
	ErrOperationMismatch = errors.New("store: operation identity changed")
	// ErrCorrupt: the database file is damaged. The file is left untouched.
	ErrCorrupt = errors.New("store: database is corrupt")
	// ErrSchemaTooNew: the database was written by a newer JunKloud.
	ErrSchemaTooNew = errors.New("store: database schema is newer than this program")
)

// busyTimeout bounds how long a statement waits for another process's lock.
const busyTimeout = 5 * time.Second

// Store is a handle on the SQLite database. It is safe for concurrent use,
// and several processes may use the same file.
type Store struct {
	// w serves every mutation. Its transactions start with BEGIN IMMEDIATE
	// and a single connection serializes writers inside this process, so a
	// read-then-write transaction never fails on lock upgrade.
	w *sql.DB
	// r serves reads and takes only read snapshots, so readers never wait
	// for the writer (WAL mode).
	r *sql.DB
}

// Open opens (creating if missing) the database at path, checks it for
// damage, switches it to WAL mode and applies pending migrations.
//
// A damaged file is reported as ErrCorrupt and is never modified or replaced;
// the same holds for a database from a newer program (ErrSchemaTooNew).
func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve %q: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, fmt.Errorf("store: create state directory: %w", err)
	}

	w, err := sql.Open("sqlite", dsn(abs, true))
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", abs, err)
	}
	w.SetMaxOpenConns(1)
	r, err := sql.Open("sqlite", dsn(abs, false))
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("store: open %q: %w", abs, err)
	}
	s := &Store{w: w, r: r}
	if err := s.init(context.Background()); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init(ctx context.Context) error {
	// Check before changing anything: journal mode and migrations write to
	// the file, and a damaged file must stay byte-identical.
	if err := checkIntegrity(ctx, s.w); err != nil {
		return err
	}
	if _, err := s.w.ExecContext(ctx, `PRAGMA journal_mode = WAL`); err != nil {
		return classify(fmt.Errorf("store: enable WAL: %w", err))
	}
	return migrate(ctx, s.w)
}

// Close releases the database handles.
func (s *Store) Close() error {
	return errors.Join(s.r.Close(), s.w.Close())
}

// dsn builds a SQLite URI. Foreign keys and the bounded busy timeout are set
// per connection; immediate transactions are used for the writer only.
func dsn(abs string, immediate bool) string {
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	if immediate {
		q.Set("_txlock", "immediate")
	}
	u := url.URL{Scheme: "file", Path: abs, RawQuery: q.Encode()}
	return u.String()
}

// checkIntegrity runs a quick consistency check and reports damage as ErrCorrupt.
func checkIntegrity(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA quick_check`)
	if err != nil {
		return classify(fmt.Errorf("store: check database: %w", err))
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return classify(fmt.Errorf("store: check database: %w", err))
		}
		if line != "ok" {
			problems = append(problems, line)
		}
	}
	if err := rows.Err(); err != nil {
		return classify(fmt.Errorf("store: check database: %w", err))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s (the file was left unchanged)", ErrCorrupt, problems[0])
	}
	return nil
}

// SQLite primary result codes and extended codes this package cares about.
const (
	codeCorrupt = 11
	codeNotADB  = 26

	codeForeignKey = 787  // SQLITE_CONSTRAINT_FOREIGNKEY
	codePrimaryKey = 1555 // SQLITE_CONSTRAINT_PRIMARYKEY
	codeUnique     = 2067 // SQLITE_CONSTRAINT_UNIQUE
)

func sqliteCode(err error) (int, bool) {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code(), true
	}
	return 0, false
}

// classify turns SQLite damage errors into ErrCorrupt and leaves others alone.
func classify(err error) error {
	if code, ok := sqliteCode(err); ok && (code&0xff == codeCorrupt || code&0xff == codeNotADB) {
		return fmt.Errorf("%w: %v (the file was left unchanged)", ErrCorrupt, err)
	}
	return err
}

// mapWriteErr translates constraint failures into sentinels:
// duplicates become ErrAlreadyExists and dangling references ErrNotFound.
func mapWriteErr(err error) error {
	if err == nil {
		return nil
	}
	if code, ok := sqliteCode(err); ok {
		switch code {
		case codePrimaryKey, codeUnique:
			return fmt.Errorf("%w: %v", ErrAlreadyExists, err)
		case codeForeignKey:
			return fmt.Errorf("%w: referenced record is missing: %v", ErrNotFound, err)
		}
	}
	return err
}

// inTx runs fn in a write transaction, committing on success and rolling
// back on any error.
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return mapWriteErr(fmt.Errorf("store: commit: %w", err))
	}
	return nil
}

// timeLayout is fixed-width UTC so stored times sort as text.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

func encodeTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func decodeTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: bad stored time %q: %w", s, err)
	}
	return t, nil
}

func decodeNullTime(s sql.NullString) (*time.Time, error) {
	if !s.Valid {
		return nil, nil
	}
	t, err := decodeTime(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
