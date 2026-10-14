package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/JunYupK/JunKloud/internal/model"
)

// SaveValidation appends a validation record. Records are never updated; the
// newest record for a (problem hash, grader version, environment hash)
// triple is the one GetValidation returns, and older ones stay as history.
func (s *Store) SaveValidation(ctx context.Context, rec model.ValidationRecord) error {
	seeds, err := json.Marshal(rec.Seeds)
	if err != nil {
		return fmt.Errorf("store: encode seeds: %w", err)
	}
	cases, err := json.Marshal(rec.Cases)
	if err != nil {
		return fmt.Errorf("store: encode validation cases: %w", err)
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO problem_validations
			(problem_hash, grader_version, environment_hash, seeds_json, passed, cases_json)
			VALUES (?, ?, ?, ?, ?, ?)`,
			rec.ProblemHash, rec.GraderVersion, rec.EnvironmentHash, string(seeds), boolInt(rec.Passed), string(cases))
		if err != nil {
			return fmt.Errorf("store: insert validation of %q: %w", rec.ProblemHash, err)
		}
		return nil
	})
}

// GetValidation returns the newest validation record for the triple, or
// ErrNotFound if none exists (for example because the grader version or
// environment changed).
func (s *Store) GetValidation(ctx context.Context, problemHash, graderVersion, environmentHash string) (model.ValidationRecord, error) {
	rec := model.ValidationRecord{ProblemHash: problemHash, GraderVersion: graderVersion, EnvironmentHash: environmentHash}
	var (
		seeds, cases string
		passed       int
	)
	err := s.r.QueryRowContext(ctx, `SELECT seeds_json, passed, cases_json FROM problem_validations
		WHERE problem_hash = ? AND grader_version = ? AND environment_hash = ?
		ORDER BY id DESC LIMIT 1`, problemHash, graderVersion, environmentHash).
		Scan(&seeds, &passed, &cases)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ValidationRecord{}, fmt.Errorf("%w: validation of %q", ErrNotFound, problemHash)
	}
	if err != nil {
		return model.ValidationRecord{}, fmt.Errorf("store: load validation: %w", err)
	}
	rec.Passed = passed == 1
	if err := json.Unmarshal([]byte(seeds), &rec.Seeds); err != nil {
		return model.ValidationRecord{}, fmt.Errorf("store: decode seeds: %w", err)
	}
	if err := json.Unmarshal([]byte(cases), &rec.Cases); err != nil {
		return model.ValidationRecord{}, fmt.Errorf("store: decode validation cases: %w", err)
	}
	return rec, nil
}
