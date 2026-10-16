//go:build linux

package session

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/lock"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/store"
)

// Start confirms a prepared session: it publishes every problem's English
// question and provided files in a NEW attempt directory of the learner
// workspace (<workspace>/<session>/<problem>/attempt-<n>/, never over
// personal files or earlier attempts) and starts the wall clock at `at`
// (stored in UTC), moving PREPARING to RUNNING in one atomic step.
//
// The session must be fully prepared (its preparation operation is done) and
// not started. Start takes the environment lock and checks that the
// environment is still the verified one. If publishing or the state change
// fails, the attempt directories this call created are removed again, so the
// questions are not left visible without a running clock. A zero `at` means
// now.
func (s *Service) Start(ctx context.Context, sessionID string, at time.Time) error {
	if at.IsZero() {
		at = s.cfg.Now()
	}
	at = at.Round(0).UTC()

	release, err := lock.Acquire(author.LockPath(s.cfg.StateDir))
	if errors.Is(err, lock.ErrLocked) {
		return fmt.Errorf("session: start: another start, reset, validation or grade is running on this environment: %w", err)
	}
	if err != nil {
		return fmt.Errorf("session: start: %w", err)
	}
	defer release()

	rec, err := s.cfg.Store.LoadSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session: start %q: %w", sessionID, err)
	}
	if rec.EnvironmentID != s.cfg.Identity.ID {
		return fmt.Errorf("%w: session %q belongs to environment %q", ErrEnvironmentChanged, sessionID, rec.EnvironmentID)
	}
	switch rec.State {
	case model.StatePreparing:
	case model.StateRunning, model.StateGrading, model.StateGradingError, model.StateCompleted:
		return fmt.Errorf("%w: session %q is %s", ErrAlreadyStarted, sessionID, rec.State)
	default:
		return fmt.Errorf("%w: session %q is %s", ErrNotPrepared, sessionID, rec.State)
	}
	op, err := s.cfg.Store.GetOperation(ctx, OperationID(sessionID))
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: session %q has no recorded preparation", ErrNotPrepared, sessionID)
	}
	if err != nil {
		return fmt.Errorf("session: start %q: %w", sessionID, err)
	}
	if op.Phase != PhaseDone {
		return fmt.Errorf("%w: the preparation of session %q stopped at %q; recover or abandon it", ErrNotPrepared, sessionID, op.Phase)
	}
	if err := s.cfg.Env.Check(ctx, s.cfg.Identity); err != nil {
		return fmt.Errorf("session: start %q: the environment is not usable: %w", sessionID, err)
	}

	var created []string
	undo := func() {
		for _, dir := range created {
			os.RemoveAll(dir)
		}
	}
	for _, f := range rec.Problems {
		dir, err := newAttemptDir(s.cfg.Workspace, sessionID, f.Definition.ID)
		if err != nil {
			undo()
			return fmt.Errorf("session: start %q: %w", sessionID, err)
		}
		created = append(created, dir)
		if _, clash := f.Assets[QuestionFile]; clash {
			undo()
			return fmt.Errorf("session: start %q: problem %q provides a file named %s, which is reserved for the question", sessionID, f.Definition.ID, QuestionFile)
		}
		files := maps.Clone(f.Assets)
		if files == nil {
			files = map[string][]byte{}
		}
		files[QuestionFile] = []byte(f.Question)
		if err := writeFiles(dir, files, 0o644); err != nil {
			undo()
			return fmt.Errorf("session: start %q: publish %q: %w", sessionID, f.Definition.ID, err)
		}
	}
	if err := s.cfg.Store.MarkStarted(ctx, sessionID, at); err != nil {
		undo()
		if errors.Is(err, store.ErrStateConflict) {
			return fmt.Errorf("%w: %w", ErrAlreadyStarted, err)
		}
		return fmt.Errorf("session: start %q: %w", sessionID, err)
	}
	return nil
}

// Status reports a session and its wall clock at `now`. The elapsed time is
// plain wall-clock time since the stored start: closing the terminal, a
// sleeping host or a restart pauses nothing. Overtime is the time beyond the
// session's limit (mock exams: the exam's duration; practice sessions have no
// limit and never run overtime). If now is before the start time the clock
// went backwards: ClockAnomaly is set and Elapsed is zero, never negative.
// The session is a SessionView: never the grading definition, and before the
// start no question text or provided files (Elapsed and Overtime are zero).
func (s *Service) Status(ctx context.Context, sessionID string, now time.Time) (Status, error) {
	rec, err := s.cfg.Store.LoadSession(ctx, sessionID)
	if err != nil {
		return Status{}, fmt.Errorf("session: status of %q: %w", sessionID, err)
	}
	if rec.StartedAt == nil {
		return Status{Session: viewOf(rec)}, nil
	}
	elapsed, overtime, anomaly := wallClock(rec, now)
	st := Status{Session: viewOf(rec), Elapsed: elapsed, Overtime: overtime, ClockAnomaly: anomaly}
	hist, err := s.loadHistory(ctx, sessionID)
	if err != nil {
		return Status{}, err
	}
	st.Pending = s.pending(rec, hist)
	return st, nil
}

// wallClock is the session's clock at `now`: plain wall time since the stored
// start, with monotonic readings stripped (the monotonic clock may stand still
// while the host sleeps, wall time does not), the part beyond the session's
// limit, and whether now lies before the start (the clock ran backwards: the
// elapsed time is then zero, never negative).
func wallClock(rec model.SessionRecord, now time.Time) (elapsed, overtime time.Duration, anomaly bool) {
	if rec.StartedAt == nil {
		return 0, 0, false
	}
	elapsed = now.Round(0).Sub(rec.StartedAt.Round(0))
	if elapsed < 0 {
		return 0, 0, true
	}
	if limit := rec.DurationLimit; limit > 0 && elapsed > limit {
		overtime = elapsed - limit
	}
	return elapsed, overtime, false
}
