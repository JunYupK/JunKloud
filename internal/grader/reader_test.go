package grader_test

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

	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func testMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, meta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, meta.RESTScopeRoot)
	return m
}

func u(doc map[string]any) *unstructured.Unstructured { return &unstructured.Unstructured{Object: doc} }

func TestDynamicReader(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	deploy := u(obj(t, deployDoc))
	ns := u(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "jk-p1", "uid": "ns-uid"}})
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		{Group: "apps", Version: "v1", Resource: "deployments"}: "DeploymentList",
		{Version: "v1", Resource: "namespaces"}:                 "NamespaceList",
	}, deploy, ns)
	r := grader.NewDynamicReader(client, testMapper())

	got, err := r.Get(ctx, deployRef)
	testutil.NoError(t, err)
	testutil.Equal(t, string(got.GetUID()), "uid-1")

	cluster, err := r.Get(ctx, refOf("v1", "Namespace", "ignored", "jk-p1"))
	testutil.NoError(t, err)
	testutil.Equal(t, string(cluster.GetUID()), "ns-uid")

	_, err = r.Get(ctx, refOf("apps/v1", "Deployment", "jk-p1", "ghost"))
	testutil.ErrorIs(t, err, grader.ErrNotFound)

	_, err = r.Get(ctx, refOf("example.com/v1", "Widget", "jk-p1", "w"))
	testutil.ErrorIs(t, err, grader.ErrNotFound) // CRD not installed: the learner did not create it

	_, err = r.Get(ctx, refOf("apps/v1", "Deployment", "", "web"))
	testutil.ErrorIs(t, err, grader.ErrInvalidCriterion) // namespaced kind without a namespace

	_, err = r.Get(ctx, refOf("a/b/c", "Deployment", "jk-p1", "web"))
	testutil.ErrorIs(t, err, grader.ErrInvalidCriterion)

	boom := errors.New("etcd down")
	client.PrependReactor("get", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, boom
	})
	_, err = r.Get(ctx, deployRef)
	testutil.ErrorIs(t, err, boom)
	if errors.Is(err, grader.ErrNotFound) {
		t.Fatal("an API failure must not look like a missing object")
	}
}
