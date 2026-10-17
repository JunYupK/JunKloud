package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
)

func validState(state string) error {
	for _, s := range model.SessionStates() {
		if s == state {
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrInvalidState, state)
}

// CreateSession stores a new session and its frozen problems in one
// transaction. Each problem's frozen definition and public content are saved
// as JSON (session_problems.frozen_json). Note: the comment on that table in
// migration 001 says "public content only", which is not accurate and is not
// edited (applied migrations stay as they are): frozen_json holds the whole
// FrozenProblem, including the grading definition (criteria with their
// expected values, the declared initial state, setup, solution and case
// actions). It is private; the session package hands callers only its
// learner-safe SessionView. Only the private package files
// (FrozenProblem.Definition.Files) are not persisted here; they are keyed
// elsewhere by FrozenProblem.Hash.
//
// It returns ErrAlreadyExists for a duplicate session ID or a repeated
// problem ID, ErrNotFound for an unknown ParentID, and ErrInvalidState for an
// unknown state.
func (s *Store) CreateSession(ctx context.Context, rec model.SessionRecord) error {
	if err := validState(rec.State); err != nil {
		return err
	}
	var marks any
	if rec.Marks != nil {
		b, err := json.Marshal(rec.Marks)
		if err != nil {
			return fmt.Errorf("store: encode marks: %w", err)
		}
		marks = string(b)
	}
	var manifest any
	if rec.EnvironmentManifest != nil {
		b, err := json.Marshal(rec.EnvironmentManifest)
		if err != nil {
			return fmt.Errorf("store: encode environment manifest: %w", err)
		}
		manifest = string(b)
	}
	var started, parent any
	if rec.StartedAt != nil {
		started = encodeTime(*rec.StartedAt)
	}
	if rec.ParentID != "" {
		parent = rec.ParentID
	}
	frozen := make([][]byte, len(rec.Problems))
	for i, p := range rec.Problems {
		b, err := json.Marshal(p)
		if err != nil {
			return fmt.Errorf("store: encode problem %q: %w", p.Definition.ID, err)
		}
		frozen[i] = b
	}

	return s.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO sessions
			(id, environment_id, mode, state, parent_id, seed, started_at, duration_limit_ns, marks_json,
			 grader_version, environment_hash, environment_manifest_json)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.ID, rec.EnvironmentID, rec.Mode, rec.State, parent,
			int64(rec.Seed), started, int64(rec.DurationLimit), marks,
			nullIfEmpty(rec.GraderVersion), nullIfEmpty(rec.EnvironmentHash), manifest)
		if err != nil {
			return mapWriteErr(fmt.Errorf("store: insert session %q: %w", rec.ID, err))
		}
		for i, p := range rec.Problems {
			_, err := tx.ExecContext(ctx, `INSERT INTO session_problems
				(session_id, problem_id, ordinal, hash, frozen_json) VALUES (?, ?, ?, ?, ?)`,
				rec.ID, p.Definition.ID, i, p.Hash, string(frozen[i]))
			if err != nil {
				return mapWriteErr(fmt.Errorf("store: insert problem %q of session %q: %w", p.Definition.ID, rec.ID, err))
			}
		}
		return nil
	})
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// LoadSession returns the session and its problems in their original order.
// It returns ErrNotFound if the session does not exist.
func (s *Store) LoadSession(ctx context.Context, id string) (model.SessionRecord, error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.SessionRecord{}, fmt.Errorf("store: begin read: %w", err)
	}
	defer tx.Rollback() // a read snapshot only; nothing to commit

	var (
		rec     model.SessionRecord
		parent  sql.NullString
		seed    int64
		started sql.NullString
		limit   int64
		marks   sql.NullString

		graderVersion, envHash, manifest sql.NullString
	)
	err = tx.QueryRowContext(ctx, `SELECT id, environment_id, mode, state, parent_id, seed,
		started_at, duration_limit_ns, marks_json, grader_version, environment_hash, environment_manifest_json
		FROM sessions WHERE id = ?`, id).
		Scan(&rec.ID, &rec.EnvironmentID, &rec.Mode, &rec.State, &parent, &seed, &started, &limit, &marks,
			&graderVersion, &envHash, &manifest)
	if errors.Is(err, sql.ErrNoRows) {
		return model.SessionRecord{}, fmt.Errorf("%w: session %q", ErrNotFound, id)
	}
	if err != nil {
		return model.SessionRecord{}, fmt.Errorf("store: load session %q: %w", id, err)
	}
	rec.ParentID = parent.String
	rec.GraderVersion, rec.EnvironmentHash = graderVersion.String, envHash.String
	if manifest.Valid {
		if err := json.Unmarshal([]byte(manifest.String), &rec.EnvironmentManifest); err != nil {
			return model.SessionRecord{}, fmt.Errorf("store: decode environment manifest of %q: %w", id, err)
		}
	}
	rec.Seed = uint64(seed)
	rec.DurationLimit = time.Duration(limit)
	if rec.StartedAt, err = decodeNullTime(started); err != nil {
		return model.SessionRecord{}, err
	}
	if marks.Valid {
		if err := json.Unmarshal([]byte(marks.String), &rec.Marks); err != nil {
			return model.SessionRecord{}, fmt.Errorf("store: decode marks of %q: %w", id, err)
		}
	}

	rows, err := tx.QueryContext(ctx, `SELECT frozen_json FROM session_problems
		WHERE session_id = ? ORDER BY ordinal`, id)
	if err != nil {
		return model.SessionRecord{}, fmt.Errorf("store: load problems of %q: %w", id, err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return model.SessionRecord{}, fmt.Errorf("store: load problems of %q: %w", id, err)
		}
		var p model.FrozenProblem
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return model.SessionRecord{}, fmt.Errorf("store: decode problem of %q: %w", id, err)
		}
		rec.Problems = append(rec.Problems, p)
	}
	if err := rows.Err(); err != nil {
		return model.SessionRecord{}, fmt.Errorf("store: load problems of %q: %w", id, err)
	}
	return rec, nil
}

// Transition moves a session from one state to another only if it is
// currently in the from state (compare-and-swap), so two terminals or
// processes racing for the same step cannot both win. It returns
// ErrStateConflict, naming the actual state, when the session is elsewhere,
// ErrNotFound for an unknown session and ErrInvalidState for an unknown
// state name.
//
// The store enforces atomicity and one rule: ABANDONED is terminal, so a
// transition from it returns ErrSessionAbandoned. COMPLETED is deliberately
// not terminal (a resubmit after completion is legal). Which other edges are
// legal is the session layer's rule. To store a final grading run and change
// the state together, use SaveRunAndTransition.
func (s *Store) Transition(ctx context.Context, id, from, to string) error {
	if err := checkTransition(from, to); err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) error { return transitionTx(ctx, tx, id, from, to) })
}

func checkTransition(from, to string) error {
	if err := validState(from); err != nil {
		return err
	}
	if err := validState(to); err != nil {
		return err
	}
	if from == model.StateAbandoned {
		return fmt.Errorf("%w: cannot leave %s", ErrSessionAbandoned, model.StateAbandoned)
	}
	return nil
}

func transitionTx(ctx context.Context, tx *sql.Tx, id, from, to string) error {
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET state = ? WHERE id = ? AND state = ?`, to, id, from)
	if err != nil {
		return fmt.Errorf("store: transition %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: transition %q: %w", id, err)
	}
	if n == 1 {
		return nil
	}
	var actual string
	err = tx.QueryRowContext(ctx, `SELECT state FROM sessions WHERE id = ?`, id).Scan(&actual)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: session %q", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("store: transition %q: %w", id, err)
	}
	return fmt.Errorf("%w: session %q is %s, expected %s", ErrStateConflict, id, actual, from)
}

// RecordReview appends one review event: the learner opened the solution or
// review of a problem at the given time. Events are never updated. It
// returns ErrNotFound if the session or the problem within it is unknown.
func (s *Store) RecordReview(ctx context.Context, sessionID, problemID string, at time.Time) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO review_events (session_id, problem_id, reviewed_at)
			VALUES (?, ?, ?)`, sessionID, problemID, encodeTime(at))
		if err != nil {
			return mapWriteErr(fmt.Errorf("store: record review of %q in %q: %w", problemID, sessionID, err))
		}
		return nil
	})
}

// SessionStatus identifies a session and where it is.
type SessionStatus struct {
	ID    string
	Mode  string
	State string
}

// ActiveSessions returns the sessions of an environment that still occupy it
// (model.ActiveSessionStates), ordered by ID. Reads never need the
// environment lock, but a caller that acts on the answer (bank validation,
// session start) holds the lock so no session can start in between.
func (s *Store) ActiveSessions(ctx context.Context, environmentID string) ([]SessionStatus, error) {
	if environmentID == "" {
		return nil, errors.New("store: environment ID is required")
	}
	states := model.ActiveSessionStates()
	marks := strings.TrimSuffix(strings.Repeat("?,", len(states)), ",")
	args := []any{environmentID}
	for _, st := range states {
		args = append(args, st)
	}
	rows, err := s.r.QueryContext(ctx, `SELECT id, mode, state FROM sessions
		WHERE environment_id = ? AND state IN (`+marks+`) ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list active sessions of %q: %w", environmentID, err)
	}
	defer rows.Close()
	var out []SessionStatus
	for rows.Next() {
		var st SessionStatus
		if err := rows.Scan(&st.ID, &st.Mode, &st.State); err != nil {
			return nil, fmt.Errorf("store: list active sessions of %q: %w", environmentID, err)
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list active sessions of %q: %w", environmentID, err)
	}
	return out, nil
}
