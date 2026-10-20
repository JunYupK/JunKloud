//go:build linux

package author

import (
	"context"
	"errors"
	"fmt"

	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/store"
)

func hasToolActions(p model.Problem) bool {
	groups := [][]model.Action{p.Setup, p.Solution}
	for _, c := range p.Cases {
		groups = append(groups, c.Actions)
	}
	for _, list := range groups {
		for _, a := range list {
			if a.Kind == model.ActionHelm || a.Kind == model.ActionKustomize {
				return true
			}
		}
	}
	return false
}

// Recovery uses the persisted parent identity, never matching marks alone.
// Records remains source-compatible for old validators and their stores.
func (s *seedRun) restoreToolInventory(ctx context.Context) error {
	records, ok := s.v.cfg.Store.(interface {
		GetInventory(context.Context, string, string, string) (model.Inventory, error)
	})
	if !ok {
		return fmt.Errorf("%w: tool recovery needs persisted inventory access", environment.ErrUnsupportedAction)
	}
	inv, err := records.GetInventory(ctx, s.v.cfg.Identity.ID, SessionID, s.p.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err == nil {
		var parents []string
		for _, r := range inv.Resources {
			if r.Ref.APIVersion == "v1" && r.Ref.Kind == "Namespace" && r.Ref.Namespace == "" {
				if r.UID == "" {
					return fmt.Errorf("%w: recovery namespace UID missing", environment.ErrNotOwned)
				}
				parents = append(parents, r.Ref.Name)
			}
		}
		if len(inv.Resources) > 0 {
			if len(parents) == 0 {
				return fmt.Errorf("%w: persisted tool inventory has no namespace identity", environment.ErrNotOwned)
			}
			s.owner.Inventory = inv
			// The interrupted seed may have a different namespace bundle.
			// Recovery can claim only the parents whose UIDs were persisted.
			s.owner.Namespace = parents[0]
			s.owner.Scope = model.ResourceScope{Namespaces: parents[1:]}
			return nil
		}
	}
	if !s.v.cfg.ToolActions.configured() {
		return environment.ErrUnsupportedAction
	}
	for _, ns := range append([]string{s.owner.Namespace}, s.owner.Scope.Namespaces...) {
		_, err := s.v.cfg.ToolActions.Reader.Get(ctx, model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: ns})
		if err == nil {
			return fmt.Errorf("%w: namespace has no persisted recovery identity", environment.ErrNotOwned)
		}
		if !errors.Is(err, grader.ErrNotFound) {
			return err
		}
	}
	s.clean = true // no target to reclaim; namespace creation will reject races
	return nil
}
