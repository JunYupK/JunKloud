package store

import (
	"context"
	"fmt"

	"github.com/JunYupK/JunKloud/internal/model"
)

// ListSubmissions returns the session's submissions in the order they were
// recorded (not ordered by SubmittedAt: the wall clock may have run
// backwards, the record order is what happened). An unknown session has none.
func (s *Store) ListSubmissions(ctx context.Context, sessionID string) ([]model.Submission, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+submissionColumns+` FROM submissions
		WHERE session_id = ? ORDER BY rowid`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: list submissions of %q: %w", sessionID, err)
	}
	defer rows.Close()
	var out []model.Submission
	for rows.Next() {
		sub, err := scanSubmission(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list submissions of %q: %w", sessionID, err)
	}
	return out, nil
}

// ListRuns returns every grading run of a submission with its criterion
// results, in the order they were recorded. An unknown submission has none.
func (s *Store) ListRuns(ctx context.Context, submissionID string) ([]model.GradingRun, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT id FROM grading_runs WHERE submission_id = ? ORDER BY rowid`, submissionID)
	if err != nil {
		return nil, fmt.Errorf("store: list runs of %q: %w", submissionID, err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: list runs of %q: %w", submissionID, err)
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, fmt.Errorf("store: list runs of %q: %w", submissionID, err)
	}
	out := make([]model.GradingRun, 0, len(ids))
	for _, id := range ids {
		run, err := s.GetRun(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, nil
}

// ListReviews returns the session's review events (every time a solution was
// opened), oldest first.
func (s *Store) ListReviews(ctx context.Context, sessionID string) ([]model.ReviewEvent, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT session_id, problem_id, reviewed_at FROM review_events
		WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: list reviews of %q: %w", sessionID, err)
	}
	defer rows.Close()
	var out []model.ReviewEvent
	for rows.Next() {
		var (
			ev model.ReviewEvent
			at string
		)
		if err := rows.Scan(&ev.SessionID, &ev.ProblemID, &at); err != nil {
			return nil, fmt.Errorf("store: list reviews of %q: %w", sessionID, err)
		}
		if ev.ReviewedAt, err = decodeTime(at); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list reviews of %q: %w", sessionID, err)
	}
	return out, nil
}
