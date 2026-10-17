//go:build linux

package environment

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func liveObject(apiVersion, kind, name string, labels map[string]any) *unstructured.Unstructured {
	meta := map[string]any{"name": name}
	if labels != nil {
		meta["labels"] = labels
	}
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": meta}}
}

// TestOccupied: the read-only preflight of a new session names every asked
// namespace and cluster-scoped object that exists, with its owner, and
// nothing else; it changes nothing.
func TestOccupied(t *testing.T) {
	nsGVR := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	crGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	owned := map[string]any{LabelEnvironmentID: "0123456789ab", LabelSessionID: "practice-1", LabelProblemID: "p1", LabelManagedBy: ManagedByValue}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nsGVR: "NamespaceList", crGVR: "ClusterRoleList"},
		liveObject("v1", "Namespace", "svc-alpha", owned),
		liveObject("v1", "Namespace", "foreign", nil),
		liveObject("rbac.authorization.k8s.io/v1", "ClusterRole", "svc-alpha-reader", owned),
	)
	got, err := occupied(context.Background(), client, testMapper(),
		[]string{"svc-alpha", "free", "foreign"},
		[]model.ObjectRef{
			{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: "svc-alpha-reader"},
			{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: "free-reader"},
			{APIVersion: "example.com/v1", Kind: "Unserved", Name: "x"},
		})
	testutil.NoError(t, err)
	testutil.Equal(t, len(got), 3)
	for i, want := range []string{`namespace "svc-alpha" (session practice-1, problem p1)`, `namespace "foreign" (not created by JunKloud)`, `ClusterRole "svc-alpha-reader" (session practice-1`} {
		if !strings.HasPrefix(got[i], want) {
			t.Errorf("entry %d = %q, want prefix %q", i, got[i], want)
		}
	}
	for _, a := range client.Actions() {
		if a.GetVerb() != "get" {
			t.Errorf("the check changed something: %s %s", a.GetVerb(), a.GetResource())
		}
	}

	client.PrependReactor("get", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver busy")
	})
	_, err = occupied(context.Background(), client, testMapper(), []string{"svc-alpha"}, nil)
	if err == nil || !strings.Contains(err.Error(), "apiserver busy") {
		t.Fatalf("an API failure must be an error: %v", err)
	}
}
