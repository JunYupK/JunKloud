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

// SaveRun stores one grading run together with all of its criterion results
// in a single transaction: either every row is saved or none is, so a
// half-saved result can never read as a finished grade. Runs are
// append-only; a second SaveRun with the same run ID yields ErrAlreadyExists
// and leaves the first untouched, and an unknown submission yields
// ErrNotFound. Criterion IDs must be unique within the run.
func (s *Store) SaveRun(ctx context.Context, run model.GradingRun) error {
	ev := run.Evaluation
	var score any
	if ev.Score != nil {
		score = int64(*ev.Score)
	}
	evidence := make([][]byte, len(ev.Results))
	for i, r := range ev.Results {
		b, err := json.Marshal(r.Evidence)
		if err != nil {
			return fmt.Errorf("store: encode evidence of %q: %w", r.CriterionID, err)
		}
		evidence[i] = b
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO grading_runs
			(id, submission_id, started_at, finished_at, score, max_score, complete)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			run.ID, run.SubmissionID, encodeTime(run.StartedAt), encodeTime(run.FinishedAt),
			score, ev.MaxScore, boolInt(ev.Complete))
		if err != nil {
			return mapWriteErr(fmt.Errorf("store: insert run %q: %w", run.ID, err))
		}
		for i, r := range ev.Results {
			_, err := tx.ExecContext(ctx, `INSERT INTO criterion_results
				(run_id, ordinal, criterion_id, status, reason, points, evidence_json)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				run.ID, i, r.CriterionID, r.Status, r.Reason, r.Points, string(evidence[i]))
			if err != nil {
				return mapWriteErr(fmt.Errorf("store: insert result %q of run %q: %w", r.CriterionID, run.ID, err))
			}
		}
		return nil
	})
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
