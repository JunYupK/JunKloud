package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/JunYupK/JunKloud/internal/model"
)

// PutOperation records an operation's intent and phase. The first call for an
// ID inserts it; later calls advance Phase and Planned. Kind, EnvironmentID,
// SessionID and CreatedAt are fixed at the first call, and a later call that
// differs in the first three yields ErrOperationMismatch. Operations have no
// foreign keys: the intent must be recordable before the session exists, and
// recovery must work whatever else is missing.
func (s *Store) PutOperation(ctx context.Context, op model.Operation) error {
	planned, err := json.Marshal(op.Planned)
	if err != nil {
		return fmt.Errorf("store: encode operation %q: %w", op.ID, err)
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var env, session, kind string
		err := tx.QueryRowContext(ctx, `SELECT environment_id, session_id, kind FROM operations WHERE id = ?`, op.ID).
			Scan(&env, &session, &kind)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err = tx.ExecContext(ctx, `INSERT INTO operations
				(id, environment_id, session_id, kind, phase, planned_json, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				op.ID, op.EnvironmentID, op.SessionID, op.Kind, op.Phase, string(planned), encodeTime(op.CreatedAt))
			if err != nil {
				return fmt.Errorf("store: insert operation %q: %w", op.ID, err)
			}
			return nil
		case err != nil:
			return fmt.Errorf("store: load operation %q: %w", op.ID, err)
		}
		if env != op.EnvironmentID || session != op.SessionID || kind != op.Kind {
			return fmt.Errorf("%w: operation %q is %s in environment %q session %q",
				ErrOperationMismatch, op.ID, kind, env, session)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE operations SET phase = ?, planned_json = ? WHERE id = ?`,
			op.Phase, string(planned), op.ID); err != nil {
			return fmt.Errorf("store: update operation %q: %w", op.ID, err)
		}
		return nil
	})
}

// GetOperation returns a stored operation or ErrNotFound.
func (s *Store) GetOperation(ctx context.Context, id string) (model.Operation, error) {
	row := s.r.QueryRowContext(ctx, `SELECT id, environment_id, session_id, kind, phase, planned_json, created_at
		FROM operations WHERE id = ?`, id)
	op, err := scanOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Operation{}, fmt.Errorf("%w: operation %q", ErrNotFound, id)
	}
	return op, err
}

// ListOperations returns every operation of the given kind, oldest first
// (ties broken by ID), whatever its phase. Recovery code uses it to find
// intents an interrupted run left unfinished; which phases count as finished
// is the caller's decision.
func (s *Store) ListOperations(ctx context.Context, kind string) ([]model.Operation, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT id, environment_id, session_id, kind, phase, planned_json, created_at
		FROM operations WHERE kind = ? ORDER BY created_at, id`, kind)
	if err != nil {
		return nil, fmt.Errorf("store: list operations %q: %w", kind, err)
	}
	defer rows.Close()
	var ops []model.Operation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list operations %q: %w", kind, err)
	}
	return ops, nil
}

// scanOperation decodes one operations row. sql.ErrNoRows is returned as is.
func scanOperation(row interface{ Scan(...any) error }) (model.Operation, error) {
	var (
		op      model.Operation
		planned string
		created string
	)
	err := row.Scan(&op.ID, &op.EnvironmentID, &op.SessionID, &op.Kind, &op.Phase, &planned, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Operation{}, err
	}
	if err != nil {
		return model.Operation{}, fmt.Errorf("store: load operation: %w", err)
	}
	if err := json.Unmarshal([]byte(planned), &op.Planned); err != nil {
		return model.Operation{}, fmt.Errorf("store: decode operation %q: %w", op.ID, err)
	}
	if op.CreatedAt, err = decodeTime(created); err != nil {
		return model.Operation{}, err
	}
	return op, nil
}

// PutInventory stores the inventory for its (environment, session, problem)
// key, replacing any earlier inventory under the same key. SessionID and
// ProblemID may be empty for environment-level or session-level entries.
func (s *Store) PutInventory(ctx context.Context, inv model.Inventory) error {
	b, err := json.Marshal(inv)
	if err != nil {
		return fmt.Errorf("store: encode inventory: %w", err)
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO inventories (environment_id, session_id, problem_id, inventory_json)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (environment_id, session_id, problem_id) DO UPDATE SET inventory_json = excluded.inventory_json`,
			inv.EnvironmentID, inv.SessionID, inv.ProblemID, string(b))
		if err != nil {
			return fmt.Errorf("store: put inventory: %w", err)
		}
		return nil
	})
}

// GetInventory returns the inventory stored under the key or ErrNotFound.
func (s *Store) GetInventory(ctx context.Context, environmentID, sessionID, problemID string) (model.Inventory, error) {
	var raw string
	err := s.r.QueryRowContext(ctx, `SELECT inventory_json FROM inventories
		WHERE environment_id = ? AND session_id = ? AND problem_id = ?`,
		environmentID, sessionID, problemID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Inventory{}, fmt.Errorf("%w: inventory %q/%q/%q", ErrNotFound, environmentID, sessionID, problemID)
	}
	if err != nil {
		return model.Inventory{}, fmt.Errorf("store: load inventory: %w", err)
	}
	var inv model.Inventory
	if err := json.Unmarshal([]byte(raw), &inv); err != nil {
		return model.Inventory{}, fmt.Errorf("store: decode inventory: %w", err)
	}
	return inv, nil
}
