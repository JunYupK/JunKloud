//go:build linux

package environment

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func testMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "Service"}, meta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, meta.RESTScopeRoot)
	m.Add(schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}, meta.RESTScopeRoot)
	return m
}

func configMap(namespace, name string) map[string]any {
	meta := map[string]any{"name": name}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	return map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta, "data": map[string]any{"k": "v"}}
}

func clusterRole(name string) map[string]any {
	return map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "metadata": map[string]any{"name": name}}
}

func namespaceObj(name string) map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": name}}
}

func testOwner() Owner {
	return Owner{
		Inventory: model.Inventory{EnvironmentID: "0123456789ab", SessionID: "s1", ProblemID: "service-test"},
		Namespace: "svc-alpha",
		Scope: model.ResourceScope{
			Namespaces: []string{"svc-alpha-client"},
			ClusterResources: []model.ObjectRef{
				{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: "svc-alpha-reader"}},
		},
	}
}

func TestPlanApplyWithinScope(t *testing.T) {
	steps, err := planApply(testOwner(), []Action{{Kind: model.ActionApply, Objects: []map[string]any{
		configMap("", "defaulted"),
		configMap("svc-alpha", "explicit"),
		configMap("svc-alpha-client", "client"),
		clusterRole("svc-alpha-reader"),
		namespaceObj("svc-alpha-client"),
	}}}, testMapper())
	testutil.NoError(t, err)
	testutil.Equal(t, len(steps), 1)
	var refs []model.ObjectRef
	for _, o := range steps[0].objects {
		refs = append(refs, o.ref)
	}
	testutil.Equal(t, refs, []model.ObjectRef{
		{APIVersion: "v1", Kind: "ConfigMap", Namespace: "svc-alpha", Name: "defaulted"},
		{APIVersion: "v1", Kind: "ConfigMap", Namespace: "svc-alpha", Name: "explicit"},
		{APIVersion: "v1", Kind: "ConfigMap", Namespace: "svc-alpha-client", Name: "client"},
		{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: "svc-alpha-reader"},
		{APIVersion: "v1", Kind: "Namespace", Name: "svc-alpha-client"},
	})
	// Owner marks are applied to every object, and the input is not changed.
	obj := steps[0].objects[0].obj
	testutil.Equal(t, obj.GetNamespace(), "svc-alpha")
	testutil.Equal(t, obj.GetLabels(), map[string]string{
		LabelEnvironmentID: "0123456789ab", LabelSessionID: "s1", LabelProblemID: "service-test",
		LabelManagedBy: ManagedByValue,
	})
	testutil.Equal(t, obj.GetAnnotations()[AnnotationOwner], "s1/service-test")
}

func TestPlanApplyRejectsOutOfScope(t *testing.T) {
	cases := map[string]map[string]any{
		"other-namespace":       configMap("default", "x"),
		"system-namespace":      configMap("kube-system", "x"),
		"undeclared-cluster":    clusterRole("cluster-admin"),
		"undeclared-namespace":  namespaceObj("default"),
		"scope-name-other-kind": {"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "svc-alpha-reader"}},
	}
	for name, obj := range cases {
		t.Run(name, func(t *testing.T) {
			// A valid object before the bad one: nothing may be planned.
			steps, err := planApply(testOwner(), []Action{{Kind: model.ActionApply,
				Objects: []map[string]any{configMap("", "ok"), obj}}}, testMapper())
			testutil.ErrorIs(t, err, ErrOutOfScope)
			testutil.Equal(t, len(steps), 0)
		})
	}
}

func TestPlanApplyRejectsInvalidActions(t *testing.T) {
	cases := map[string]Action{
		"no-name":      {Kind: model.ActionApply, Objects: []map[string]any{{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"generateName": "x-"}}}},
		"no-kind":      {Kind: model.ActionApply, Objects: []map[string]any{{"apiVersion": "v1", "metadata": map[string]any{"name": "x"}}}},
		"unknown-kind": {Kind: model.ActionApply, Objects: []map[string]any{{"apiVersion": "example.com/v1", "kind": "Widget", "metadata": map[string]any{"name": "x"}}}},
		"empty-apply":  {Kind: model.ActionApply},
		"empty-delete": {Kind: model.ActionDeleteOwned},
		"unknown":      {Kind: "shell"},
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := planApply(testOwner(), []Action{a}, testMapper())
			testutil.ErrorIs(t, err, ErrInvalidAction)
		})
	}
}

func TestPlanApplyUnsupportedYet(t *testing.T) {
	for _, kind := range []string{model.ActionHelm, model.ActionImage, model.ActionFile, model.ActionPatch} {
		_, err := planApply(testOwner(), []Action{{Kind: kind}}, testMapper())
		testutil.ErrorIs(t, err, ErrUnsupportedAction)
	}
}

func TestPlanDeleteOwnedOnlyInventory(t *testing.T) {
	owner := testOwner()
	owned := model.OwnedResource{
		Ref: model.ObjectRef{APIVersion: "v1", Kind: "ConfigMap", Namespace: "svc-alpha", Name: "mine"},
		UID: "uid-1", OwnerID: "s1/service-test",
	}
	owner.Inventory.Resources = []model.OwnedResource{owned}

	steps, err := planApply(owner, []Action{{Kind: model.ActionDeleteOwned, Targets: []model.ObjectRef{
		{APIVersion: "v1", Kind: "ConfigMap", Name: "mine"}, // namespace defaults to the problem's
	}}}, testMapper())
	testutil.NoError(t, err)
	testutil.Equal(t, steps[0].objects[0].ref, owned.Ref)
	testutil.Equal(t, steps[0].objects[0].uid, "uid-1")

	_, err = planApply(owner, []Action{{Kind: model.ActionDeleteOwned, Targets: []model.ObjectRef{
		{APIVersion: "v1", Kind: "ConfigMap", Namespace: "svc-alpha", Name: "not-mine"},
	}}}, testMapper())
	testutil.ErrorIs(t, err, ErrNotOwned)
}

func TestValidateOwner(t *testing.T) {
	testutil.NoError(t, validateOwner(testOwner()))
	bad := map[string]func(*Owner){
		"no-env":         func(o *Owner) { o.Inventory.EnvironmentID = "" },
		"bad-namespace":  func(o *Owner) { o.Namespace = "Svc_Alpha" },
		"no-namespace":   func(o *Owner) { o.Namespace = "" },
		"reserved":       func(o *Owner) { o.Namespace = "kube-system" },
		"marker":         func(o *Owner) { o.Namespace = MarkerNamespace },
		"bad-label":      func(o *Owner) { o.Inventory.SessionID = "has space" },
		"scope-reserved": func(o *Owner) { o.Scope.Namespaces = []string{"default"} },
		"scope-is-ns":    func(o *Owner) { o.Scope.Namespaces = []string{"svc-alpha"} },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			o := testOwner()
			mutate(&o)
			testutil.ErrorIs(t, validateOwner(o), ErrInvalidOwner)
		})
	}
}

func TestOwnedBy(t *testing.T) {
	inv := testOwner().Inventory
	labels := ownerLabels(inv)
	testutil.Equal(t, ownedBy(labels, inv), true)
	other := inv
	other.ProblemID = "other"
	testutil.Equal(t, ownedBy(labels, other), false)
	testutil.Equal(t, ownedBy(nil, inv), false)
}

func TestMergeInventory(t *testing.T) {
	inv := model.Inventory{EnvironmentID: "e"}
	a := model.OwnedResource{Ref: model.ObjectRef{APIVersion: "v1", Kind: "ConfigMap", Namespace: "n", Name: "a"}, UID: "1"}
	inv = upsertResource(inv, a)
	a2 := a
	a2.UID = "2"
	inv = upsertResource(inv, a2)
	testutil.Equal(t, inv.Resources, []model.OwnedResource{a2})
	inv = removeResource(inv, a.Ref)
	testutil.Equal(t, len(inv.Resources), 0)
}

// TestRequiredNamespaces: only setup (CreateNamespaces) asks for the whole
// bundle; later steps need just the namespaces they write into, and an
// explicit Namespace object counts as the author creating it.
func TestRequiredNamespaces(t *testing.T) {
	actions := []Action{
		{Kind: model.ActionApply, Objects: []map[string]any{configMap("", "a"), configMap("", "b"), clusterRole("svc-alpha-reader")}},
		{Kind: model.ActionDeleteOwned, Targets: []model.ObjectRef{{APIVersion: "v1", Kind: "ConfigMap", Namespace: "svc-alpha-client", Name: "c"}}},
	}
	owner := testOwner()
	owner.Inventory.Resources = []model.OwnedResource{{Ref: model.ObjectRef{APIVersion: "v1", Kind: "ConfigMap", Namespace: "svc-alpha-client", Name: "c"}, UID: "u"}}
	steps, err := planApply(owner, actions, testMapper())
	testutil.NoError(t, err)

	// Solution/case: the problem namespace only; delete-owned and
	// cluster-scoped objects need none.
	testutil.Equal(t, requiredNamespaces(owner, steps), []string{"svc-alpha"})
	// delete-owned alone needs no namespace at all.
	testutil.Equal(t, len(requiredNamespaces(owner, steps[1:])), 0)

	// Setup: the whole bundle, created when missing.
	owner.CreateNamespaces = true
	testutil.Equal(t, requiredNamespaces(owner, steps), []string{"svc-alpha", "svc-alpha-client"})

	// An explicit Namespace object in the actions is the author's creation.
	owner.CreateNamespaces = false
	explicit, err := planApply(owner, []Action{{Kind: model.ActionApply, Objects: []map[string]any{
		namespaceObj("svc-alpha-client"), configMap("svc-alpha-client", "d")}}}, testMapper())
	testutil.NoError(t, err)
	testutil.Equal(t, len(requiredNamespaces(owner, explicit)), 0)
}

func crdWithConditions(name string, conds ...map[string]any) *unstructured.Unstructured {
	list := make([]any, 0, len(conds))
	for _, c := range conds {
		list = append(list, c)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": name},
		"status":   map[string]any{"conditions": list},
	}}
}

func TestWaitCRDEstablished(t *testing.T) {
	const name = "widgets.example.com"
	cond := func(typ, status string) map[string]any {
		return map[string]any{"type": typ, "status": status}
	}
	cases := []struct {
		name    string
		crd     *unstructured.Unstructured // nil: the CRD does not exist
		wantErr bool
	}{
		{"established", crdWithConditions(name, cond("NamesAccepted", "True"), cond("Established", "True")), false},
		{"names not accepted", crdWithConditions(name, cond("NamesAccepted", "False"), cond("Established", "False")), true},
		{"established false", crdWithConditions(name, cond("NamesAccepted", "True"), cond("Established", "False")), true},
		{"no conditions", crdWithConditions(name), true},
		{"missing", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var objs []runtime.Object
			if tc.crd != nil {
				objs = append(objs, tc.crd)
			}
			dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objs...)
			err := waitCRDEstablished(context.Background(), dyn, name, 30*time.Millisecond, time.Millisecond)
			if !tc.wantErr {
				testutil.NoError(t, err)
				return
			}
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("got error %v, want one naming %s", err, name)
			}
		})
	}
}

func TestWaitCRDEstablishedBecomesTrue(t *testing.T) {
	const name = "widgets.example.com"
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), crdWithConditions(name))
	gvr := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = dyn.Resource(gvr).UpdateStatus(context.Background(), crdWithConditions(name,
			map[string]any{"type": "Established", "status": "True"}), metav1.UpdateOptions{})
	}()
	testutil.NoError(t, waitCRDEstablished(context.Background(), dyn, name, 5*time.Second, 2*time.Millisecond))
}

func TestWaitCRDEstablishedHonoursContext(t *testing.T) {
	const name = "widgets.example.com"
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), crdWithConditions(name))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := waitCRDEstablished(ctx, dyn, name, time.Minute, time.Millisecond)
	testutil.ErrorIs(t, err, context.Canceled)
	if !strings.Contains(err.Error(), name) {
		t.Fatalf("error %v does not name %s", err, name)
	}
}
