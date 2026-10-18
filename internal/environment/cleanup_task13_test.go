//go:build linux

package environment

import (
	"context"
	"errors"
	"github.com/JunYupK/JunKloud/internal/executil"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func cleanupTest(t *testing.T, names ...string) (*ClusterJanitor, Owner, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	inv := model.Inventory{EnvironmentID: "env", SessionID: "session", ProblemID: "problem"}
	var objs []runtime.Object
	for _, name := range names {
		u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": name}}}
		u.SetLabels(ownerLabels(inv))
		u.SetUID(types.UID(name + "-uid"))
		objs = append(objs, u)
		inv.Resources = append(inv.Resources, model.OwnedResource{Ref: model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: name}, UID: name + "-uid", OwnerID: OwnerID(inv)})
	}
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objs...)
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Version: "v1"}})
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, meta.RESTScopeRoot)
	return NewJanitor(dyn, mapper, WithCleanupTimeout(20*time.Millisecond), WithCleanupPoll(time.Millisecond)), Owner{Inventory: inv, Scope: model.ResourceScope{Namespaces: names}}, dyn
}
func TestResetNamespaceBundle(t *testing.T) {
	j, o, d := cleanupTest(t, "one", "two")
	testutil.NoError(t, j.Cleanup(bg, o))
	for _, n := range []string{"one", "two"} {
		if _, err := d.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).Get(bg, n, metav1.GetOptions{}); err == nil {
			t.Fatal("namespace survived", n)
		}
	}
}
func TestForeignResourcesPreserved(t *testing.T) {
	j, o, d := cleanupTest(t, "one")
	o.Inventory.Resources[0].UID = "different"
	testutil.ErrorIs(t, j.Cleanup(bg, o), ErrNotOwned)
	u, err := d.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).Get(bg, "one", metav1.GetOptions{})
	testutil.NoError(t, err)
	testutil.Equal(t, string(u.GetUID()), "one-uid")
}
func TestFinalizerIsNotForced(t *testing.T) {
	j, o, d := cleanupTest(t, "one")
	d.PrependReactor("delete", "namespaces", func(a clienttesting.Action) (bool, runtime.Object, error) { return true, nil, nil })
	testutil.ErrorIs(t, j.Cleanup(bg, o), ErrCleanupTimeout)
	for _, a := range d.Actions() {
		if a.GetVerb() == "patch" || a.GetVerb() == "update" {
			t.Fatal("forced finalizer")
		}
	}
}
func TestResetReservedNamespaceRefused(t *testing.T) {
	j, o, d := cleanupTest(t, "kube-system")
	testutil.ErrorIs(t, j.Cleanup(bg, o), ErrInvalidOwner)
	for _, a := range d.Actions() {
		if a.GetVerb() == "delete" {
			t.Fatal("system namespace delete")
		}
	}
}
func TestPVCDataRemoved(t *testing.T) {
	v := model.VolumeCleanup{Name: "pv", UID: "pv-uid", ClaimNamespace: "ns", ClaimName: "data", ClaimUID: "claim-uid", ReclaimPolicy: "Delete", Provisioner: "rancher.io/local-path", Path: "/var/local-path-provisioner/pvc-claim-uid_ns_data", NodeName: "node"}
	testutil.NoError(t, validateVolume(v))
	for _, mutate := range []func(*model.VolumeCleanup){func(v *model.VolumeCleanup) { v.ReclaimPolicy = "Retain" }, func(v *model.VolumeCleanup) { v.Path = "/etc" }, func(v *model.VolumeCleanup) { v.Provisioner = "unknown" }, func(v *model.VolumeCleanup) { v.ClaimUID = "replacement" }} {
		bad := v
		mutate(&bad)
		if !errors.Is(validateVolume(bad), ErrStorageRetained) {
			t.Fatal("unverified backing accepted", bad)
		}
	}
}

var _ = context.Background

func TestResetOwnedImageRefusesForeignTag(t *testing.T) {
	dir := t.TempDir()
	st := openTestStore(t, dir)
	runner := &fakeRunner{respond: func(exe string, args []string) (executil.Result, error) {
		return executil.Result{Stdout: []byte(`[{"Id":"sha256:abc","RepoTags":["foreign:keep"],"Config":{"Labels":{"junkloud.io/environment-id":"env","junkloud.io/session-id":"session","junkloud.io/problem-id":"problem"}}}]`)}, nil
	}}
	m := New(dir, st, WithRunner(runner))
	inv := model.Inventory{EnvironmentID: "env", SessionID: "session", ProblemID: "problem", Images: []model.OwnedImage{{ID: "sha256:abc", Reference: "owned:attempt"}}}
	testutil.ErrorIs(t, m.cleanupImages(bg, inv), ErrNotOwned)
	for _, call := range runner.Calls() {
		if strings.Contains(call, "image rm") {
			t.Fatal("foreign image tag deleted")
		}
	}
}

func TestResetClusterRejectsForeignIdentity(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{}
	m := New(dir, openTestStore(t, dir), WithRunner(r))
	id := recordedIdentity()
	testutil.NoError(t, m.writeIdentity(id))
	foreign := id
	foreign.CAHash = "foreign"
	_, err := m.RecreateReset(bg, foreign, id.ID, "reset-1")
	testutil.ErrorIs(t, err, ErrIdentityMismatch)
	testutil.Equal(t, len(r.Calls()), 0)
	_, err = m.RecreateReset(bg, id, "wrong", "reset-2")
	testutil.ErrorIs(t, err, ErrIdentityMismatch)
	testutil.Equal(t, len(r.Calls()), 0)
}

// A session reset and the cluster replacement it starts must not overwrite
// or accidentally consume one another's durable intent.
func TestResetClusterUsesDistinctRecreationIntent(t *testing.T) {
	m, id, _, _ := newLiveManager(t, func(model.Identity, *liveSetup) {})
	parent := model.Operation{ID: "reset-session-1", Kind: "session-reset", EnvironmentID: id.ID, Phase: PhasePlanned}
	testutil.NoError(t, m.store.PutOperation(bg, parent))
	_, _ = m.RecreateReset(bg, id, id.ID, parent.ID)
	ops, err := m.store.ListOperations(bg, OperationRecreate)
	testutil.NoError(t, err)
	if len(ops) != 1 {
		t.Fatal("replacement intent was not recorded independently", ops)
	}
	got, err := m.store.GetOperation(bg, parent.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got, parent)
	testutil.Equal(t, ops[0].Planned.DockerIDs, id.ContainerIDs)
}

func TestPVCDeletedClaimRetainedPVIncluded(t *testing.T) {
	inv := model.Inventory{EnvironmentID: "env", SessionID: "session", ProblemID: "problem", Resources: []model.OwnedResource{{Ref: model.ObjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Namespace: "owned", Name: "data"}, UID: "claim-old"}}}
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "retained", UID: "pv-old"}, Spec: corev1.PersistentVolumeSpec{PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain, ClaimRef: &corev1.ObjectReference{Namespace: "owned", Name: "data", UID: "claim-old"}}, Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeReleased}}
	cl := &clients{kube: kubefake.NewClientset(pv)}
	vols, err := snapshotVolumes(bg, cl, Owner{Inventory: inv, Scope: model.ResourceScope{Namespaces: []string{"owned"}}})
	testutil.NoError(t, err)
	if len(vols) != 1 {
		t.Fatal("deleted claim's retained backing was omitted", vols)
	}
	testutil.Equal(t, vols[0].ClaimUID, "claim-old")
	testutil.ErrorIs(t, validateVolume(vols[0]), ErrStorageRetained)
}

func TestPVCPendingClaimRefusesUnverifiedCleanup(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "owned", UID: "pending-uid"}}
	cl := &clients{kube: kubefake.NewClientset(pvc)}
	vols, err := snapshotVolumes(bg, cl, Owner{Scope: model.ResourceScope{Namespaces: []string{"owned"}}})
	testutil.ErrorIs(t, err, ErrStorageRetained)
	if len(vols) != 1 || vols[0].ClaimUID != "pending-uid" {
		t.Fatal("pending claim identity not retained", vols)
	}
}
