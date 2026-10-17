package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
)

// MarkStarted starts a prepared session's clock: it stores the start time
// (UTC) and moves the session from PREPARING to RUNNING in one statement, so
// a session is never RUNNING without a clock or timed while still PREPARING.
// It is a compare-and-swap like Transition: only a PREPARING session that has
// no start time yet can start, so two terminals racing to start it cannot
// both win and a second start can never move the clock.
//
// It returns ErrNotFound for an unknown session and ErrStateConflict, naming
// the actual state, for any session that is not PREPARING (a failed
// preparation, an already running or finished session).
func (s *Store) MarkStarted(ctx context.Context, id string, at time.Time) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE sessions SET state = ?, started_at = ?
			WHERE id = ? AND state = ? AND started_at IS NULL`,
			model.StateRunning, encodeTime(at), id, model.StatePreparing)
		if err != nil {
			return fmt.Errorf("store: start session %q: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: start session %q: %w", id, err)
		}
		if n == 1 {
			return nil
		}
		var actual string
		var started sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT state, started_at FROM sessions WHERE id = ?`, id).Scan(&actual, &started)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: session %q", ErrNotFound, id)
		}
		if err != nil {
			return fmt.Errorf("store: start session %q: %w", id, err)
		}
		if started.Valid {
			return fmt.Errorf("%w: session %q already has a start time (state %s)", ErrStateConflict, id, actual)
		}
		return fmt.Errorf("%w: session %q is %s, expected %s", ErrStateConflict, id, actual, model.StatePreparing)
	})
}
