package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/JunYupK/JunKloud/internal/model"
)

// CreateSubmission stores a new submission. Submissions are append-only: an
// existing ID is never changed and yields ErrAlreadyExists. An unknown
// session or parent submission yields ErrNotFound.
func (s *Store) CreateSubmission(ctx context.Context, sub model.Submission) error {
	var hashes, parent any
	if sub.InputHashes != nil {
		b, err := json.Marshal(sub.InputHashes)
		if err != nil {
			return fmt.Errorf("store: encode input hashes: %w", err)
		}
		hashes = string(b)
	}
	if sub.ParentSubmissionID != "" {
		parent = sub.ParentSubmissionID
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO submissions
			(id, session_id, kind, parent_submission_id, submitted_at, input_hashes_json)
			VALUES (?, ?, ?, ?, ?, ?)`,
			sub.ID, sub.SessionID, sub.Kind, parent, encodeTime(sub.SubmittedAt), hashes)
		if err != nil {
			return mapWriteErr(fmt.Errorf("store: insert submission %q: %w", sub.ID, err))
		}
		return nil
	})
}

// GetSubmission returns a stored submission or ErrNotFound.
func (s *Store) GetSubmission(ctx context.Context, id string) (model.Submission, error) {
	var (
		sub       model.Submission
		parent    sql.NullString
		submitted string
		hashes    sql.NullString
	)
	err := s.r.QueryRowContext(ctx, `SELECT id, session_id, kind, parent_submission_id,
		submitted_at, input_hashes_json FROM submissions WHERE id = ?`, id).
		Scan(&sub.ID, &sub.SessionID, &sub.Kind, &parent, &submitted, &hashes)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Submission{}, fmt.Errorf("%w: submission %q", ErrNotFound, id)
	}
	if err != nil {
		return model.Submission{}, fmt.Errorf("store: load submission %q: %w", id, err)
	}
	sub.ParentSubmissionID = parent.String
	if sub.SubmittedAt, err = decodeTime(submitted); err != nil {
		return model.Submission{}, err
	}
	if hashes.Valid {
		if err := json.Unmarshal([]byte(hashes.String), &sub.InputHashes); err != nil {
			return model.Submission{}, fmt.Errorf("store: decode input hashes of %q: %w", id, err)
		}
	}
	return sub, nil
}

// validateEvaluation enforces the completed-grade invariant before anything
// is written. A complete evaluation has a score, at least one result and no
// ERROR result; an incomplete one has no score. Anything else would let a
// partial result read as a finished grade.
func validateEvaluation(ev model.Evaluation) error {
	if !ev.Complete {
		if ev.Score != nil {
			return fmt.Errorf("%w: an incomplete evaluation must not carry a score", ErrIncompleteEvaluation)
		}
		return nil
	}
	if ev.Score == nil {
		return fmt.Errorf("%w: a complete evaluation needs a score", ErrIncompleteEvaluation)
	}
	if len(ev.Results) == 0 {
		return fmt.Errorf("%w: a complete evaluation needs at least one result", ErrIncompleteEvaluation)
	}
	for _, r := range ev.Results {
		if r.Status == model.StatusError {
			return fmt.Errorf("%w: criterion %q is ERROR", ErrIncompleteEvaluation, r.CriterionID)
		}
	}
	return nil
}

// SaveRun stores one grading run together with all of its criterion results
// in a single transaction: either every row is saved or none is. Runs are
// append-only; a second SaveRun with the same run ID yields ErrAlreadyExists
// and leaves the first untouched, and an unknown submission yields
// ErrNotFound. Criterion IDs must be unique within the run. An evaluation
// that breaks the completed-grade invariant yields ErrIncompleteEvaluation.
//
// SaveRun does not change the session state. To record a final result and
// move the session in one atomic step use SaveRunAndTransition.
func (s *Store) SaveRun(ctx context.Context, run model.GradingRun) error {
	if err := validateEvaluation(run.Evaluation); err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) error { return insertRun(ctx, tx, run) })
}

// SaveRunAndTransition stores the run and moves the session from one state to
// another (compare-and-swap, as Transition) in ONE transaction. If either
// part fails, including ErrStateConflict, neither is stored, so a crash can
// never leave a final run on a session still in the old state, nor a
// COMPLETED session without its run. The run's submission must belong to the
// session. Errors are those of SaveRun and Transition.
func (s *Store) SaveRunAndTransition(ctx context.Context, run model.GradingRun, sessionID, from, to string) error {
	if err := validateEvaluation(run.Evaluation); err != nil {
		return err
	}
	if err := checkTransition(from, to); err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var owner string
		err := tx.QueryRowContext(ctx, `SELECT session_id FROM submissions WHERE id = ?`, run.SubmissionID).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: submission %q", ErrNotFound, run.SubmissionID)
		}
		if err != nil {
			return fmt.Errorf("store: load submission %q: %w", run.SubmissionID, err)
		}
		if owner != sessionID {
			return fmt.Errorf("%w: submission %q does not belong to session %q", ErrNotFound, run.SubmissionID, sessionID)
		}
		if err := insertRun(ctx, tx, run); err != nil {
			return err
		}
		return transitionTx(ctx, tx, sessionID, from, to)
	})
}

func insertRun(ctx context.Context, tx *sql.Tx, run model.GradingRun) error {
	ev := run.Evaluation
	var score any
	if ev.Score != nil {
		score = int64(*ev.Score)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO grading_runs
		(id, submission_id, started_at, finished_at, score, max_score, complete)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.SubmissionID, encodeTime(run.StartedAt), encodeTime(run.FinishedAt),
		score, ev.MaxScore, boolInt(ev.Complete))
	if err != nil {
		return mapWriteErr(fmt.Errorf("store: insert run %q: %w", run.ID, err))
	}
	for i, r := range ev.Results {
		evidence, err := json.Marshal(r.Evidence)
		if err != nil {
			return fmt.Errorf("store: encode evidence of %q: %w", r.CriterionID, err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO criterion_results
			(run_id, ordinal, criterion_id, status, reason, points, evidence_json)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			run.ID, i, r.CriterionID, r.Status, r.Reason, r.Points, string(evidence))
		if err != nil {
			return mapWriteErr(fmt.Errorf("store: insert result %q of run %q: %w", r.CriterionID, run.ID, err))
		}
	}
	return nil
}

// GetRun returns a stored grading run with its criterion results in their
// saved order, or ErrNotFound.
func (s *Store) GetRun(ctx context.Context, id string) (model.GradingRun, error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.GradingRun{}, fmt.Errorf("store: begin read: %w", err)
	}
	defer tx.Rollback()

	var (
		run               model.GradingRun
		started, finished string
		score             sql.NullInt64
		complete          int
	)
	err = tx.QueryRowContext(ctx, `SELECT id, submission_id, started_at, finished_at, score, max_score, complete
		FROM grading_runs WHERE id = ?`, id).
		Scan(&run.ID, &run.SubmissionID, &started, &finished, &score, &run.Evaluation.MaxScore, &complete)
	if errors.Is(err, sql.ErrNoRows) {
		return model.GradingRun{}, fmt.Errorf("%w: grading run %q", ErrNotFound, id)
	}
	if err != nil {
		return model.GradingRun{}, fmt.Errorf("store: load run %q: %w", id, err)
	}
	if run.StartedAt, err = decodeTime(started); err != nil {
		return model.GradingRun{}, err
	}
	if run.FinishedAt, err = decodeTime(finished); err != nil {
		return model.GradingRun{}, err
	}
	if score.Valid {
		n := int(score.Int64)
		run.Evaluation.Score = &n
	}
	run.Evaluation.Complete = complete == 1

	rows, err := tx.QueryContext(ctx, `SELECT criterion_id, status, reason, points, evidence_json
		FROM criterion_results WHERE run_id = ? ORDER BY ordinal`, id)
	if err != nil {
		return model.GradingRun{}, fmt.Errorf("store: load results of run %q: %w", id, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			r   model.CheckResult
			raw string
		)
		if err := rows.Scan(&r.CriterionID, &r.Status, &r.Reason, &r.Points, &raw); err != nil {
			return model.GradingRun{}, fmt.Errorf("store: load results of run %q: %w", id, err)
		}
		if err := json.Unmarshal([]byte(raw), &r.Evidence); err != nil {
			return model.GradingRun{}, fmt.Errorf("store: decode evidence of %q: %w", r.CriterionID, err)
		}
		run.Evaluation.Results = append(run.Evaluation.Results, r)
	}
	if err := rows.Err(); err != nil {
		return model.GradingRun{}, fmt.Errorf("store: load results of run %q: %w", id, err)
	}
	return run, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
