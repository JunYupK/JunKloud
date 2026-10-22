//go:build linux

package environment

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/JunYupK/JunKloud/internal/model"
)

const OperationCleanup = "environment-cleanup"

var ErrStorageRetained = errors.New("environment: storage cleanup cannot be verified; inspect retained PVC/PV data and retry reset")

// ManagedJanitor gives authors the same verified, journaled cleanup as sessions.
type ManagedJanitor struct {
	Manager  *Manager
	Identity model.Identity
	Options  []JanitorOption
}

func (j ManagedJanitor) Cleanup(ctx context.Context, owner Owner) error {
	inv := owner.Inventory
	for _, ref := range cleanupRefs(owner) {
		if !slices.ContainsFunc(inv.Resources, func(r model.OwnedResource) bool { return r.Ref == ref }) {
			inv.Resources = append(inv.Resources, model.OwnedResource{Ref: ref, OwnerID: OwnerID(inv)})
		}
	}
	return j.Manager.cleanup(ctx, j.Identity, inv, j.Options...)
}

// Cleanup verifies the dedicated cluster, snapshots ownership/storage before
// deletion, journals every phase, and retains the inventory on every failure.
// Callers hold the environment lock. Personal workspace paths are never removed.
func (m *Manager) Cleanup(ctx context.Context, id model.Identity, inv model.Inventory) error {
	return m.cleanup(ctx, id, inv)
}
func (m *Manager) cleanup(ctx context.Context, id model.Identity, inv model.Inventory, opts ...JanitorOption) error {
	if inv.EnvironmentID != id.ID || inv.SessionID == "" || inv.ProblemID == "" {
		return ErrInvalidOwner
	}
	_, cl, err := m.verify(ctx, id)
	if err != nil {
		return err
	}
	ops, err := m.store.ListOperations(ctx, OperationCleanup)
	if err != nil {
		return err
	}
	var op model.Operation
	for _, o := range ops {
		if o.EnvironmentID == id.ID && o.SessionID == inv.SessionID && o.Planned.ProblemID == inv.ProblemID && o.Phase != PhaseDone {
			op = o
		}
	}
	if op.ID == "" {
		suffix, err := newID()
		if err != nil {
			return err
		}
		op = model.Operation{ID: "cleanup-" + suffix, EnvironmentID: id.ID, SessionID: inv.SessionID, Kind: OperationCleanup, Phase: PhasePlanned, Planned: inv, CreatedAt: m.now()}
		if err = m.store.PutOperation(ctx, op); err != nil {
			return err
		}
	}
	j := NewJanitor(cl.dyn, cl.mapper, opts...)
	owner := Owner{Inventory: op.Planned}
	for _, r := range op.Planned.Resources {
		if r.Ref.Kind == "Namespace" && r.Ref.APIVersion == "v1" {
			owner.Scope.Namespaces = append(owner.Scope.Namespaces, r.Ref.Name)
		}
	}
	if op.Phase == PhasePlanned {
		// Claim every target before storage reads or any delete.
		for _, r := range cleanupRefs(owner) {
			v, claimErr := j.claim(ctx, op.Planned, r)
			if claimErr != nil {
				return claimErr
			}
			if v != nil {
				op.Planned = upsertResource(op.Planned, model.OwnedResource{Ref: r, UID: string(v.uid), OwnerID: OwnerID(inv)})
			}
			if r.Kind == "Namespace" && slices.Contains(reservedNamespaces, r.Name) {
				return ErrInvalidOwner
			}
			if err != nil {
				return err
			}
		}
		volumes, err := snapshotVolumes(ctx, cl, owner)
		op.Planned.Volumes = volumes
		if err != nil {
			// Pending/absent claims remain durable proof to reconcile on retry.
			if saveErr := m.store.PutOperation(ctx, op); saveErr != nil {
				return errors.Join(err, saveErr)
			}
			return err
		}
		op.Phase = "snapshotted"
		if err = m.store.PutOperation(ctx, op); err != nil {
			return err
		}
	}
	owner.Inventory = op.Planned
	for _, v := range op.Planned.Volumes {
		if err = validateVolume(v); err != nil {
			return err
		}
	}
	if len(op.Planned.DockerIDs) > 0 || len(op.Planned.ArtifactPaths) > 0 {
		return fmt.Errorf("%w: untyped Docker/file inventory cannot be safely deleted", ErrNotOwned)
	}
	if op.Phase == "snapshotted" {
		if err = j.Cleanup(ctx, owner); err != nil {
			return err
		}
		op.Phase = "objects-deleted"
		if err = m.store.PutOperation(ctx, op); err != nil {
			return err
		}
	}
	if err = m.verifyVolumesGone(ctx, id, cl, op.Planned.Volumes, j.timeout, j.poll); err != nil {
		return err
	}
	if err = m.cleanupImages(ctx, op.Planned); err != nil {
		return err
	}
	// Keep original inventory in the immutable operation; clear the live slot
	// only after all data is gone, so later Apply cannot reuse stale UIDs.
	if err = m.store.PutInventory(ctx, model.Inventory{EnvironmentID: id.ID, SessionID: inv.SessionID, ProblemID: inv.ProblemID}); err != nil {
		return err
	}
	op.Phase = PhaseDone
	return m.store.PutOperation(ctx, op)
}

func snapshotVolumes(ctx context.Context, cl *clients, owner Owner) ([]model.VolumeCleanup, error) {
	var out []model.VolumeCleanup
	claims := map[string]model.VolumeCleanup{}
	for _, v := range owner.Inventory.Volumes {
		claims[v.ClaimUID] = v
	}
	for _, r := range owner.Inventory.Resources {
		if r.Ref.APIVersion == "v1" && r.Ref.Kind == "PersistentVolumeClaim" && slices.Contains(owner.Scope.Namespaces, r.Ref.Namespace) {
			if r.UID == "" {
				return out, fmt.Errorf("%w: PVC %s/%s has no recorded UID", ErrStorageRetained, r.Ref.Namespace, r.Ref.Name)
			}
			if _, ok := claims[r.UID]; !ok {
				claims[r.UID] = model.VolumeCleanup{ClaimNamespace: r.Ref.Namespace, ClaimName: r.Ref.Name, ClaimUID: r.UID}
			}
		}
	}
	for _, ns := range owner.Scope.Namespaces {
		live, err := cl.kube.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, c := range live.Items {
			if c.UID == "" {
				return out, fmt.Errorf("%w: PVC %s/%s has no UID", ErrStorageRetained, ns, c.Name)
			}
			if _, ok := claims[string(c.UID)]; !ok {
				claims[string(c.UID)] = model.VolumeCleanup{ClaimNamespace: ns, ClaimName: c.Name, ClaimUID: string(c.UID)}
			}
		}
	}
	// A deleted claim's Released PV is still backing data. Reconcile all
	// claim references in the verified bundle, never PV names alone.
	pvs, err := cl.kube.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, err
	}
	matched := map[string]bool{}
	for _, pv := range pvs.Items {
		ref := pv.Spec.ClaimRef
		if ref == nil || !slices.Contains(owner.Scope.Namespaces, ref.Namespace) {
			continue
		}
		claim, ok := claims[string(ref.UID)]
		if !ok || claim.ClaimName != ref.Name || claim.ClaimNamespace != ref.Namespace {
			return out, fmt.Errorf("%w: PV %s claim identity cannot be proved", ErrStorageRetained, pv.Name)
		}
		v := model.VolumeCleanup{Name: pv.Name, UID: string(pv.UID), ClaimNamespace: ref.Namespace, ClaimName: ref.Name, ClaimUID: string(ref.UID), ReclaimPolicy: string(pv.Spec.PersistentVolumeReclaimPolicy), Provisioner: pv.Annotations["pv.kubernetes.io/provisioned-by"]}
		if pv.Spec.HostPath != nil {
			v.Path = pv.Spec.HostPath.Path
		}
		if pv.Spec.Local != nil {
			v.Path = pv.Spec.Local.Path
		}
		if pv.Spec.NodeAffinity != nil && pv.Spec.NodeAffinity.Required != nil {
			terms := pv.Spec.NodeAffinity.Required.NodeSelectorTerms
			if len(terms) == 1 && len(terms[0].MatchExpressions) == 1 {
				e := terms[0].MatchExpressions[0]
				if e.Key == "kubernetes.io/hostname" && string(e.Operator) == "In" && len(e.Values) == 1 {
					v.NodeName = e.Values[0]
				}
			}
		}
		out = append(out, v)
		matched[string(ref.UID)] = true
	}
	var unresolved []string
	for uid, claim := range claims {
		if matched[uid] {
			continue
		}
		out = append(out, claim)
		if claim.Name == "" {
			unresolved = append(unresolved, claim.ClaimNamespace+"/"+claim.ClaimName)
		}
	}
	if len(unresolved) > 0 {
		return out, fmt.Errorf("%w: PVC backing not established for %s; wait for binding or inspect prior data, then retry reset", ErrStorageRetained, strings.Join(unresolved, ", "))
	}
	return out, nil
}
func validateVolume(v model.VolumeCleanup) error {
	base := "pvc-" + v.ClaimUID + "_" + v.ClaimNamespace + "_" + v.ClaimName
	if v.UID == "" || v.ClaimUID == "" || v.ReclaimPolicy != "Delete" || v.Provisioner != "rancher.io/local-path" || path.Dir(v.Path) != "/var/local-path-provisioner" || path.Base(v.Path) != base || v.NodeName == "" {
		return fmt.Errorf("%w: PV %s (reclaim %s, provisioner %s, path %s)", ErrStorageRetained, v.Name, v.ReclaimPolicy, v.Provisioner, v.Path)
	}
	return nil
}
func (m *Manager) verifyVolumesGone(ctx context.Context, id model.Identity, cl *clients, vols []model.VolumeCleanup, timeout, poll time.Duration) error {
	deadline := time.Now().Add(timeout)
	for _, v := range vols {
		for {
			pv, err := cl.kube.CoreV1().PersistentVolumes().Get(ctx, v.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				break
			}
			if err != nil {
				return err
			}
			if string(pv.UID) != v.UID {
				return fmt.Errorf("%w: PV %s replaced", ErrNotOwned, v.Name)
			}
			if !time.Now().Before(deadline) {
				return fmt.Errorf("%w: PV %s remains; finalizers %v", ErrCleanupTimeout, v.Name, pv.Finalizers)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(poll):
			}
		}
		// Use a verified node ID and argv, never arbitrary host filesystem deletion.
		nodes, err := m.inspect(ctx, id.ContainerIDs)
		if err != nil {
			return err
		}
		container := ""
		for _, n := range nodes {
			if strings.TrimPrefix(n.Name, "/") == v.NodeName && n.Cluster == id.ClusterName {
				container = n.ID
			}
		}
		if container == "" {
			return fmt.Errorf("%w: PV node %s is not a recorded node", ErrNotOwned, v.NodeName)
		}
		bounded, cancel := context.WithTimeout(ctx, dockerTimeout)
		_, err = m.runner.Run(bounded, "docker", []string{"exec", container, "test", "!", "-e", v.Path}, nil)
		cancel()
		if err != nil {
			return fmt.Errorf("%w: backing path %s remains or cannot be inspected: %v", ErrStorageRetained, v.Path, err)
		}
	}
	return nil
}

// NamespaceInventory augments an inventory with intended bundle names after
// an interrupted Apply, allowing only matching owner labels to be adopted.
func NamespaceInventory(inv model.Inventory, names []string, cluster []model.ObjectRef) model.Inventory {
	for _, name := range names {
		if name != "" {
			cluster = append(cluster, model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: name})
		}
	}
	for _, ref := range cluster {
		if !slices.ContainsFunc(inv.Resources, func(r model.OwnedResource) bool { return r.Ref == ref }) {
			inv.Resources = append(inv.Resources, model.OwnedResource{Ref: ref, OwnerID: OwnerID(inv)})
		}
	}
	return inv
}

// RequireSettled prevents authoring from entering an environment whose reset
// or cleanup still needs recovery. It is read-only; the caller holds the lock.
func (m *Manager) RequireSettled(ctx context.Context) error {
	for _, kind := range []string{OperationCleanup, "session-reset"} {
		ops, err := m.store.ListOperations(ctx, kind)
		if err != nil {
			return err
		}
		for _, op := range ops {
			if op.Phase != PhaseDone {
				return fmt.Errorf("%w: %s stopped at %s; use ckad reset --recover or confirmed ckad reset --cluster", ErrRecoveryNeeded, op.ID, op.Phase)
			}
		}
	}
	return nil
}
