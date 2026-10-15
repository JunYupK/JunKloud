package grader

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func testMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, meta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, meta.RESTScopeRoot)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace)
	return m
}

func u(doc map[string]any) *unstructured.Unstructured { return &unstructured.Unstructured{Object: doc} }

func TestDynamicReader(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	deploy := u(map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "web", "namespace": "jk-p1", "uid": "uid-1"}})
	ns := u(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "jk-p1", "uid": "ns-uid"}})
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		{Group: "apps", Version: "v1", Resource: "deployments"}: "DeploymentList",
		{Version: "v1", Resource: "namespaces"}:                 "NamespaceList",
		{Version: "v1", Resource: "pods"}:                       "PodList",
	}, deploy, ns,
		u(map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "a", "namespace": "jk-p1", "labels": map[string]any{"app": "web"}}}),
		u(map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "b", "namespace": "jk-p1", "labels": map[string]any{"app": "other"}}}))
	r := newDynamicReader(client, testMapper())

	got, err := r.Get(ctx, deployRef2)
	testutil.NoError(t, err)
	testutil.Equal(t, uidOf(got), "uid-1")

	pods, err := r.List(ctx, ListQuery{APIVersion: "v1", Kind: "Pod", Namespace: "jk-p1", LabelSelector: "app=web"})
	testutil.NoError(t, err)
	testutil.Equal(t, len(pods), 1)
	testutil.Equal(t, nameOf(pods[0]), "a")
	_, err = r.List(ctx, ListQuery{APIVersion: "v1", Kind: "Pod"})
	testutil.ErrorIs(t, err, ErrInvalidCriterion)

	cluster, err := r.Get(ctx, refOf2("v1", "Namespace", "ignored", "jk-p1"))
	testutil.NoError(t, err)
	testutil.Equal(t, uidOf(cluster), "ns-uid")

	_, err = r.Get(ctx, refOf2("apps/v1", "Deployment", "jk-p1", "ghost"))
	testutil.ErrorIs(t, err, ErrNotFound)

	_, err = r.Get(ctx, refOf2("example.com/v1", "Widget", "jk-p1", "w"))
	testutil.ErrorIs(t, err, ErrNotFound) // CRD not installed: the learner did not create it

	_, err = r.Get(ctx, refOf2("apps/v1", "Deployment", "", "web"))
	testutil.ErrorIs(t, err, ErrInvalidCriterion) // namespaced kind without a namespace

	_, err = r.Get(ctx, refOf2("a/b/c", "Deployment", "jk-p1", "web"))
	testutil.ErrorIs(t, err, ErrInvalidCriterion)

	boom := errors.New("etcd down")
	client.PrependReactor("get", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, boom
	})
	_, err = r.Get(ctx, deployRef2)
	testutil.ErrorIs(t, err, boom)
	if errors.Is(err, ErrNotFound) {
		t.Fatal("an API failure must not look like a missing object")
	}
}

var deployRef2 = refOf2("apps/v1", "Deployment", "jk-p1", "web")

func refOf2(apiVersion, kind, ns, name string) model.ObjectRef {
	return model.ObjectRef{APIVersion: apiVersion, Kind: kind, Namespace: ns, Name: name}
}
