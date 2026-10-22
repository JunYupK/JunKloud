package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// CurrentSessionID is the most recently created session in this installation.
// Persisted creation order also works before the clock starts and across
// independent terminals; there is no process-local current-session pointer.
func (s *Store) CurrentSessionID(ctx context.Context) (string, error) {
	var id string
	err := s.r.QueryRowContext(ctx, `SELECT id FROM sessions ORDER BY rowid DESC LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// SetMark changes only a learner bookmark. Its read and write share a write
// transaction so simultaneous marks cannot overwrite one another.
func (s *Store) SetMark(ctx context.Context, sessionID, problemID string, marked bool) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var raw sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT marks_json FROM sessions WHERE id=? AND EXISTS (SELECT 1 FROM session_problems WHERE session_id=? AND problem_id=?)`, sessionID, sessionID, problemID).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		marks := map[string]bool{}
		if raw.Valid {
			if err := json.Unmarshal([]byte(raw.String), &marks); err != nil {
				return fmt.Errorf("store: decode marks: %w", err)
			}
		}
		if marks == nil {
			marks = map[string]bool{}
		}
		if marked {
			marks[problemID] = true
		} else {
			delete(marks, problemID)
		}
		encoded, err := json.Marshal(marks)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET marks_json=? WHERE id=?`, string(encoded), sessionID)
		return err
	})
}
