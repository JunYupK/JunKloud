//go:build linux

package environment

import (
	"context"
	"github.com/JunYupK/JunKloud/internal/model"
)

// PreflightActions performs Apply's complete read-only scope/namespace
// checks before a mixed author list can write files or build images.
func (m *Manager) PreflightActions(ctx context.Context, id model.Identity, owner Owner, actions []Action) error {
	if owner.Inventory.EnvironmentID != id.ID {
		return ErrIdentityMismatch
	}
	if err := validateOwner(owner); err != nil {
		return err
	}
	_, cl, err := m.verify(ctx, id)
	if err != nil {
		return err
	}
	steps, err := planApply(owner, actions, cl.mapper)
	if err != nil {
		return err
	}
	_, err = checkNamespaces(ctx, cl, owner, steps)
	return err
}
