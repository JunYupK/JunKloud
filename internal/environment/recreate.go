//go:build linux

package environment

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/store"
)

const OperationRecreate = "environment-recreate"

// Recreate replaces only the verified dedicated cluster. The destructive
// intent stores exact node IDs; recovery may remove only their surviving
// subset, never a new same-name cluster. Callers hold the environment lock.
func (m *Manager) Recreate(ctx context.Context, id model.Identity, confirmed string) (model.Identity, error) {
	return m.RecreateReset(ctx, id, confirmed, "recreate-"+id.ID+"-"+id.MarkerUID)
}

// RecreateReset binds the replacement to a durable session-reset intent.
// Repeating the same reset ID after completion returns the existing identity.
func (m *Manager) RecreateReset(ctx context.Context, id model.Identity, confirmed, resetID string) (model.Identity, error) {
	if confirmed == "" || confirmed != id.ID {
		return model.Identity{}, ErrIdentityMismatch
	}
	// The session-reset intent has its own phase machine. Never read it as
	// this replacement's intent or overwrite it when replacement completes.
	key := "recreate-" + resetID
	op, err := m.store.GetOperation(ctx, key)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return model.Identity{}, err
	}
	if err == nil && op.Phase == PhaseDone {
		return m.Ensure(ctx)
	}
	if errors.Is(err, store.ErrNotFound) {
		if err = m.Check(ctx, id); err != nil {
			return model.Identity{}, err
		}
		op = model.Operation{ID: key, EnvironmentID: id.ID, Kind: OperationRecreate, Phase: PhasePlanned, Planned: model.Inventory{EnvironmentID: id.ID, DockerIDs: slices.Clone(id.ContainerIDs)}, CreatedAt: m.now()}
		if err = m.store.PutOperation(ctx, op); err != nil {
			return model.Identity{}, err
		}
	}
	if op.Phase == PhasePlanned {
		if err = m.StopPlatform(ctx, id); err != nil {
			return model.Identity{}, err
		}
		remaining, err := m.clusterContainerIDs(ctx, id.ClusterName)
		if err != nil {
			return model.Identity{}, err
		}
		for _, cid := range remaining {
			if !slices.Contains(op.Planned.DockerIDs, cid) {
				return model.Identity{}, fmt.Errorf("%w: unexpected node %s during recovery", ErrIdentityMismatch, cid)
			}
		}
		if len(remaining) > 0 {
			nodes, err := m.inspect(ctx, remaining)
			if err != nil {
				return model.Identity{}, err
			}
			for _, n := range nodes {
				if n.Cluster != id.ClusterName {
					return model.Identity{}, ErrIdentityMismatch
				}
			}
			for _, cid := range remaining {
				if err = m.command(ctx, dockerTimeout, "docker", "rm", "--force", "--volumes", cid); err != nil {
					return model.Identity{}, err
				}
			}
		}
		remaining, err = m.clusterContainerIDs(ctx, id.ClusterName)
		if err != nil {
			return model.Identity{}, err
		}
		if len(remaining) > 0 {
			return model.Identity{}, ErrRecoveryNeeded
		}
		op.Phase = "deleted"
		if err = m.store.PutOperation(ctx, op); err != nil {
			return model.Identity{}, err
		}
	}
	if op.Phase == "deleted" {
		// Preserve the identity throughout the gap so the recovery CLI can be
		// composed offline. create atomically replaces it with the new identity.
		archive := m.IdentityPath() + "." + id.MarkerUID + ".archive"
		if raw, err := os.ReadFile(m.IdentityPath()); err == nil {
			if err = writeFileAtomic(archive, raw); err != nil {
				return model.Identity{}, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return model.Identity{}, err
		}
		op.Phase = "creating"
		if err = m.store.PutOperation(ctx, op); err != nil {
			return model.Identity{}, err
		}
	}
	create := m.plannedCreate(id.ID)
	create.ID = "recreate-create-" + id.ID
	if err = m.store.PutOperation(ctx, create); err != nil {
		return model.Identity{}, err
	}
	fresh, err := m.create(ctx, create)
	if err != nil {
		return model.Identity{}, err
	}
	op.Phase = PhaseDone
	if err = m.store.PutOperation(ctx, op); err != nil {
		return model.Identity{}, err
	}
	return fresh, nil
}
