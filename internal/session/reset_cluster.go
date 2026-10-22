//go:build linux

package session

import (
	"context"
	"fmt"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/model"
)

type recreator interface {
	RecreateReset(context.Context, model.Identity, string, string) (model.Identity, error)
}

func (s *Service) resetCluster(ctx context.Context, rec model.SessionRecord, opts ResetOptions) error {
	if _, ok := s.cfg.Env.(recreator); !ok {
		return fmt.Errorf("session: cluster recreation unavailable")
	}
	if err := s.cfg.Env.Check(ctx, s.cfg.Identity); err != nil {
		return err
	}
	ops, err := s.cfg.Store.ListOperations(ctx, ResetOperationKind)
	if err != nil {
		return err
	}
	op := model.Operation{ID: fmt.Sprintf("reset-%s-%d", rec.ID, len(ops)+1), Kind: ResetOperationKind, EnvironmentID: rec.EnvironmentID, SessionID: rec.ID, Phase: PhasePlanned, CreatedAt: s.cfg.Now().UTC(), Planned: model.Inventory{EnvironmentID: rec.EnvironmentID, SessionID: rec.ID, ProblemID: "!cluster"}}
	if err = s.cfg.Store.PutOperation(ctx, op); err != nil {
		return err
	}
	return s.resumeClusterReset(ctx, rec, op)
}
func (s *Service) resumeClusterReset(ctx context.Context, rec model.SessionRecord, op model.Operation) error {
	c, ok := s.cfg.Env.(recreator)
	if !ok {
		return fmt.Errorf("session: cluster recreation unavailable")
	}
	if op.Phase == PhasePlanned {
		id, err := c.RecreateReset(ctx, s.cfg.Identity, rec.EnvironmentID, op.ID)
		if err != nil {
			return err
		}
		s.cfg.Identity = id
		op.Phase = "recreated"
		if err = s.cfg.Store.PutOperation(ctx, op); err != nil {
			return err
		}
	}
	// A saved phase proves what succeeded before interruption, not the live
	// replacement's current identity. Keep the old journals until it verifies.
	if err := s.cfg.Env.Check(ctx, s.cfg.Identity); err != nil {
		return err
	}
	if rec.State != model.StateAbandoned {
		if err := s.cfg.Store.Transition(ctx, rec.ID, rec.State, model.StateAbandoned); err != nil {
			return err
		}
	}
	// A verified replacement physically removed all old cluster resources.
	// Retain every journal snapshot, closing prior cleanup intents explicitly.
	for _, kind := range []string{ResetOperationKind, environment.OperationCleanup} {
		ops, err := s.cfg.Store.ListOperations(ctx, kind)
		if err != nil {
			return err
		}
		for _, prior := range ops {
			if prior.EnvironmentID == rec.EnvironmentID && prior.ID != op.ID && prior.Phase != PhaseDone {
				prior.Phase = PhaseDone
				if err = s.cfg.Store.PutOperation(ctx, prior); err != nil {
					return err
				}
			}
		}
	}
	op.Phase = PhaseDone
	return s.cfg.Store.PutOperation(ctx, op)
}
