//go:build linux

package author_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

var (
	nsGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	crGVR = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
)

func janitorMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Version: "v1"}, {Group: "rbac.authorization.k8s.io", Version: "v1"}})
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, meta.RESTScopeRoot)
	m.Add(schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}, meta.RESTScopeRoot)
	return m
}

func owned(kind, apiVersion, name, uid string, labels map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": map[string]any{"name": name}}}
	if uid != "" {
		u.SetUID(types.UID(uid))
	}
	u.SetLabels(labels)
	return u
}

func ownerLabels(session, problem string) map[string]string {
	return map[string]string{
		environment.LabelEnvironmentID: "0123456789ab", environment.LabelSessionID: session,
		environment.LabelProblemID: problem, environment.LabelManagedBy: environment.ManagedByValue,
	}
}

func janitorOwner(extra ...string) environment.Owner {
	return environment.Owner{
		Inventory: model.Inventory{EnvironmentID: "0123456789ab", SessionID: author.SessionID, ProblemID: "scale-web"},
		Namespace: "ns-a",
		Scope: model.ResourceScope{
			Namespaces:       extra,
			ClusterResources: []model.ObjectRef{{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: "viewer"}},
		},
	}
}

type janitorEnv struct {
	client  *dynamicfake.FakeDynamicClient
	janitor *author.ClusterJanitor
}

// newJanitorEnv builds a janitor over a fake dynamic client. Namespace and
// ClusterRole deletions behave like a cluster: the object turns Terminating
// and disappears after `termination` (never, when it is negative).
func newJanitorEnv(t *testing.T, termination time.Duration, objs ...runtime.Object) *janitorEnv {
	t.Helper()
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		nsGVR: "NamespaceList", crGVR: "ClusterRoleList",
	}, objs...)
	e := &janitorEnv{client: client}
	client.PrependReactor("delete", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		del := action.(clienttesting.DeleteAction)
		gvr, name := del.GetResource(), del.GetName()
		obj, err := client.Tracker().Get(gvr, "", name)
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		now := metav1.Now()
		u.SetDeletionTimestamp(&now)
		if err := client.Tracker().Update(gvr, u, ""); err != nil {
			return true, nil, err
		}
		if termination >= 0 {
			time.AfterFunc(termination, func() { _ = client.Tracker().Delete(gvr, "", name) })
		}
		return true, nil, nil
	})
	e.janitor = author.NewJanitor(client, janitorMapper(), author.WithCleanupTimeout(300*time.Millisecond), author.WithCleanupPoll(5*time.Millisecond))
	return e
}

func (e *janitorEnv) exists(gvr schema.GroupVersionResource, name string) bool {
	_, err := e.client.Tracker().Get(gvr, "", name)
	return err == nil
}

func (e *janitorEnv) verbs() []string {
	var out []string
	for _, a := range e.client.Actions() {
		if !slices.Contains(out, a.GetVerb()) {
			out = append(out, a.GetVerb())
		}
	}
	slices.Sort(out)
	return out
}

func TestJanitorRemovesOnlyTheOwnersObjects(t *testing.T) {
	mine := ownerLabels(author.SessionID, "scale-web")
	e := newJanitorEnv(t, 40*time.Millisecond,
		owned("Namespace", "v1", "ns-a", "u1", mine),
		owned("Namespace", "v1", "ns-extra", "u2", mine),
		owned("Namespace", "v1", "ns-other-problem", "u3", ownerLabels(author.SessionID, "other-problem")),
		owned("Namespace", "v1", "ns-learner", "u4", ownerLabels("sess-1", "scale-web")),
		owned("Namespace", "v1", "kube-system", "u5", nil),
		owned("ClusterRole", "rbac.authorization.k8s.io/v1", "viewer", "u6", mine),
		owned("ClusterRole", "rbac.authorization.k8s.io/v1", "bystander", "u7", mine),
	)
	start := time.Now()
	testutil.NoError(t, e.janitor.Cleanup(ctx, janitorOwner("ns-extra")))
	if time.Since(start) < 30*time.Millisecond {
		t.Error("Cleanup returned before the namespace deletion completed")
	}

	testutil.Equal(t, e.exists(nsGVR, "ns-a"), false)
	testutil.Equal(t, e.exists(nsGVR, "ns-extra"), false)
	testutil.Equal(t, e.exists(crGVR, "viewer"), false)
	// Everything that is not the owner's bundle stays, whoever else's it is.
	for _, name := range []string{"ns-other-problem", "ns-learner", "kube-system"} {
		testutil.Equal(t, e.exists(nsGVR, name), true)
	}
	testutil.Equal(t, e.exists(crGVR, "bystander"), true) // not declared in the scope
}

func TestJanitorLeavesLookAlikesAlone(t *testing.T) {
	e := newJanitorEnv(t, 0,
		// A namespace of the bundle that belongs to somebody else.
		owned("Namespace", "v1", "ns-a", "u1", ownerLabels("sess-1", "scale-web")),
		// A declared cluster resource without the owner's marks.
		owned("ClusterRole", "rbac.authorization.k8s.io/v1", "viewer", "u2", nil),
	)
	testutil.NoError(t, e.janitor.Cleanup(ctx, janitorOwner()))
	testutil.Equal(t, e.exists(nsGVR, "ns-a"), true)
	testutil.Equal(t, e.exists(crGVR, "viewer"), true)
	for _, verb := range e.verbs() {
		if verb == "delete" {
			t.Fatalf("deleted a foreign object; actions = %v", e.client.Actions())
		}
	}
}

func TestJanitorChecksTheRecordedUID(t *testing.T) {
	mine := ownerLabels(author.SessionID, "scale-web")
	e := newJanitorEnv(t, 0, owned("ClusterRole", "rbac.authorization.k8s.io/v1", "viewer", "recreated-uid", mine))
	owner := janitorOwner()
	owner.Inventory.Resources = []model.OwnedResource{{
		Ref: model.ObjectRef{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: "viewer"}, UID: "original-uid",
	}}
	testutil.NoError(t, e.janitor.Cleanup(ctx, owner))
	// The recorded object is gone; what carries its name now is not the one
	// this owner created.
	testutil.Equal(t, e.exists(crGVR, "viewer"), true)
}

func TestJanitorIsIdempotent(t *testing.T) {
	e := newJanitorEnv(t, 0)
	testutil.NoError(t, e.janitor.Cleanup(ctx, janitorOwner("ns-extra")))
	testutil.NoError(t, e.janitor.Cleanup(ctx, janitorOwner("ns-extra")))
	for _, verb := range e.verbs() {
		if verb != "get" {
			t.Fatalf("an empty cluster needed %q; actions = %v", verb, e.client.Actions())
		}
	}
}

// A namespace stuck in Terminating (finalizers) is reported, never forced.
func TestJanitorNeverForcesFinalizers(t *testing.T) {
	mine := ownerLabels(author.SessionID, "scale-web")
	stuck := owned("Namespace", "v1", "ns-a", "u1", mine)
	stuck.SetFinalizers([]string{"example.com/hold"})
	e := newJanitorEnv(t, -1, stuck)
	err := e.janitor.Cleanup(ctx, janitorOwner())
	testutil.ErrorIs(t, err, author.ErrCleanupTimeout)
	for _, want := range []string{"ns-a", "Terminating", "example.com/hold"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	for _, verb := range e.verbs() {
		if verb != "get" && verb != "delete" {
			t.Fatalf("unexpected %q call; actions = %v", verb, e.client.Actions())
		}
	}
	// The namespace is still there: nothing removed its finalizer.
	obj, getErr := e.client.Tracker().Get(nsGVR, "", "ns-a")
	testutil.NoError(t, getErr)
	testutil.Equal(t, obj.(*unstructured.Unstructured).GetFinalizers(), []string{"example.com/hold"})
}

func TestJanitorHonoursCancellation(t *testing.T) {
	mine := ownerLabels(author.SessionID, "scale-web")
	e := newJanitorEnv(t, -1, owned("Namespace", "v1", "ns-a", "u1", mine))
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	err := e.janitor.Cleanup(cctx, janitorOwner())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's deadline", err)
	}
}
