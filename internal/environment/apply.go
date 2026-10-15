//go:build linux

package environment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"

	"github.com/JunYupK/JunKloud/internal/model"
)

// Apply errors. Match them with errors.Is.
var (
	// ErrOutOfScope: an object lies outside the owner's namespace bundle and
	// declared cluster resources. Checked for every object before any change.
	ErrOutOfScope = errors.New("environment: object outside the problem's resource scope")
	// ErrNotOwned: an existing object (or namespace) is not marked as this
	// owner's, or a delete target is not in the owner's inventory or has a
	// different UID than recorded. It is left untouched.
	ErrNotOwned = errors.New("environment: object not owned by this owner")
	// ErrUnsupportedAction: a valid action kind this build cannot run yet
	// (helm, image, file, patch).
	ErrUnsupportedAction = errors.New("environment: action kind not supported yet")
	// ErrInvalidAction: an action or object is malformed (unknown kind,
	// missing name, unknown resource type, nothing to do).
	ErrInvalidAction = errors.New("environment: invalid action")
	// ErrInvalidOwner: the owner cannot be marked on objects (bad IDs or
	// namespaces, or a reserved namespace).
	ErrInvalidOwner = errors.New("environment: invalid owner")
)

// reservedNamespaces are never a problem's namespace.
var reservedNamespaces = []string{"default", "kube-system", "kube-public", "kube-node-lease", "local-path-storage", MarkerNamespace}

// Owner is who Apply works for and where it may write.
//
// Inventory carries the owner (EnvironmentID, SessionID, ProblemID) and the
// resources it already owns; delete-owned may only remove those. Namespace is
// the problem namespace; it and Scope.Namespaces form the problem's namespace
// bundle, which Apply creates (marked as the owner's) when missing. Scope
// also lists the only cluster-scoped objects the problem may create.
type Owner struct {
	Inventory model.Inventory
	Namespace string
	Scope     model.ResourceScope
}

// Action is one rendered setup, solution or case step. Callers decode the
// manifests named by a model.Action and render them with
// problem.RenderValue before building an Action.
//
//   - model.ActionApply: create or update each of Objects (Kubernetes objects
//     as decoded JSON/YAML maps). Namespaced objects without a namespace go
//     into Owner.Namespace.
//   - model.ActionDeleteOwned: delete each of Targets; each must be in the
//     owner's inventory with the recorded UID. A target without a namespace
//     means Owner.Namespace.
//
// helm, image, file and patch return ErrUnsupportedAction for now.
type Action struct {
	Kind    string
	Objects []map[string]any
	Targets []model.ObjectRef
}

// OwnerID is the owner string recorded in OwnedResource.OwnerID and the
// owner annotation: "<session>/<problem>".
func OwnerID(inv model.Inventory) string { return inv.SessionID + "/" + inv.ProblemID }

// Apply verifies id (as Check does), plans every action against the owner's
// scope, and only then changes the cluster: it ensures the namespace bundle,
// then runs the actions in order. Every created object carries the owner
// labels and annotation and is recorded, with its UID, in the returned
// inventory. The intent is stored as an Operation before the first change
// and the inventory is stored after each change, so an interruption leaves
// a record of what was created. On error the returned inventory holds what
// was owned at that point.
func (m *Manager) Apply(ctx context.Context, id model.Identity, owner Owner, actions []Action) (model.Inventory, error) {
	inv := cloneInventory(owner.Inventory)
	if id.ID == "" || owner.Inventory.EnvironmentID != id.ID {
		return inv, fmt.Errorf("%w: owner environment %q, identity %q", ErrIdentityMismatch, owner.Inventory.EnvironmentID, id.ID)
	}
	if err := validateOwner(owner); err != nil {
		return inv, err
	}
	_, cl, err := m.verify(ctx, id)
	if err != nil {
		return inv, err
	}
	steps, err := planApply(owner, actions, cl.mapper)
	if err != nil {
		return inv, err
	}

	opID, err := newID()
	if err != nil {
		return inv, err
	}
	op := model.Operation{
		ID:            "apply-" + opID,
		EnvironmentID: id.ID,
		SessionID:     owner.Inventory.SessionID,
		Kind:          OperationApply,
		Phase:         PhasePlanned,
		Planned:       plannedInventory(inv, owner, steps),
		CreatedAt:     m.now(),
	}
	if err := m.store.PutOperation(ctx, op); err != nil {
		return inv, fmt.Errorf("environment: record apply intent: %w", err)
	}
	record := func(r model.OwnedResource, remove bool) error {
		if remove {
			inv = removeResource(inv, r.Ref)
		} else {
			inv = upsertResource(inv, r)
		}
		return m.store.PutInventory(ctx, inv)
	}

	for _, ns := range bundle(owner) {
		r, err := ensureNamespace(ctx, cl, owner.Inventory, ns)
		if err != nil {
			return inv, err
		}
		if err := record(r, false); err != nil {
			return inv, err
		}
	}
	for _, s := range steps {
		for _, p := range s.objects {
			switch s.kind {
			case model.ActionApply:
				r, err := applyObject(ctx, cl, owner.Inventory, p)
				if err != nil {
					return inv, err
				}
				if err := record(r, false); err != nil {
					return inv, err
				}
			case model.ActionDeleteOwned:
				if err := deleteObject(ctx, cl, owner.Inventory, p); err != nil {
					return inv, err
				}
				if err := record(model.OwnedResource{Ref: p.ref}, true); err != nil {
					return inv, err
				}
			}
		}
	}
	op.Phase = PhaseDone
	if err := m.store.PutOperation(ctx, op); err != nil {
		return inv, err
	}
	return inv, nil
}

// plannedObject is one object an action will create, update or delete.
type plannedObject struct {
	ref        model.ObjectRef
	gvr        schema.GroupVersionResource
	namespaced bool
	obj        *unstructured.Unstructured // apply: the object to send
	uid        string                     // delete-owned: the recorded UID
}

type step struct {
	kind    string
	objects []plannedObject
}

// planApply validates every action and object against the owner's scope
// without contacting the cluster beyond the type mapper. It returns nothing
// unless everything is acceptable.
func planApply(owner Owner, actions []Action, mapper meta.RESTMapper) ([]step, error) {
	var steps []step
	for i, a := range actions {
		s := step{kind: a.Kind}
		switch a.Kind {
		case model.ActionApply:
			if len(a.Objects) == 0 {
				return nil, fmt.Errorf("%w: action %d (apply) has no objects", ErrInvalidAction, i)
			}
			for j, raw := range a.Objects {
				p, err := planObject(owner, raw, mapper)
				if err != nil {
					return nil, fmt.Errorf("action %d object %d: %w", i, j, err)
				}
				s.objects = append(s.objects, p)
			}
		case model.ActionDeleteOwned:
			if len(a.Targets) == 0 {
				return nil, fmt.Errorf("%w: action %d (delete-owned) has no targets", ErrInvalidAction, i)
			}
			for j, t := range a.Targets {
				p, err := planDelete(owner, t, mapper)
				if err != nil {
					return nil, fmt.Errorf("action %d target %d: %w", i, j, err)
				}
				s.objects = append(s.objects, p)
			}
		case model.ActionHelm, model.ActionImage, model.ActionFile, model.ActionPatch:
			return nil, fmt.Errorf("%w: action %d (%s)", ErrUnsupportedAction, i, a.Kind)
		default:
			return nil, fmt.Errorf("%w: action %d has unknown kind %q", ErrInvalidAction, i, a.Kind)
		}
		steps = append(steps, s)
	}
	return steps, nil
}

func planObject(owner Owner, raw map[string]any, mapper meta.RESTMapper) (plannedObject, error) {
	// A JSON round trip copies the object and normalizes rendered values
	// (for example int parameters) into the types unstructured expects.
	b, err := json.Marshal(raw)
	if err != nil {
		return plannedObject{}, fmt.Errorf("%w: %v", ErrInvalidAction, err)
	}
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(b); err != nil {
		return plannedObject{}, fmt.Errorf("%w: %v", ErrInvalidAction, err)
	}
	if u.GetName() == "" {
		return plannedObject{}, fmt.Errorf("%w: %s has no metadata.name", ErrInvalidAction, u.GetKind())
	}
	gvk := u.GroupVersionKind()
	mapping, namespaced, err := mapKind(mapper, gvk)
	if err != nil {
		return plannedObject{}, err
	}
	if namespaced {
		if u.GetNamespace() == "" {
			u.SetNamespace(owner.Namespace)
		}
		if !slices.Contains(bundle(owner), u.GetNamespace()) {
			return plannedObject{}, fmt.Errorf("%w: %s %s/%s", ErrOutOfScope, gvk.Kind, u.GetNamespace(), u.GetName())
		}
	} else {
		if u.GetNamespace() != "" {
			return plannedObject{}, fmt.Errorf("%w: cluster-scoped %s %q has a namespace", ErrInvalidAction, gvk.Kind, u.GetName())
		}
		if !clusterAllowed(owner, u.GetAPIVersion(), gvk, u.GetName()) {
			return plannedObject{}, fmt.Errorf("%w: cluster-scoped %s %q is not declared in the problem scope", ErrOutOfScope, gvk.Kind, u.GetName())
		}
	}
	for _, f := range [][]string{{"metadata", "uid"}, {"metadata", "resourceVersion"}, {"metadata", "creationTimestamp"},
		{"metadata", "managedFields"}, {"metadata", "generation"}, {"metadata", "selfLink"}} {
		unstructured.RemoveNestedField(u.Object, f...)
	}
	labels := u.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range ownerLabels(owner.Inventory) {
		labels[k] = v
	}
	u.SetLabels(labels)
	ann := u.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[AnnotationOwner] = OwnerID(owner.Inventory)
	u.SetAnnotations(ann)
	return plannedObject{
		ref:        model.ObjectRef{APIVersion: u.GetAPIVersion(), Kind: gvk.Kind, Namespace: u.GetNamespace(), Name: u.GetName()},
		gvr:        mapping.Resource,
		namespaced: namespaced,
		obj:        u,
	}, nil
}

func planDelete(owner Owner, t model.ObjectRef, mapper meta.RESTMapper) (plannedObject, error) {
	gv, err := schema.ParseGroupVersion(t.APIVersion)
	if err != nil || t.Kind == "" || t.Name == "" {
		return plannedObject{}, fmt.Errorf("%w: delete target %+v", ErrInvalidAction, t)
	}
	mapping, namespaced, err := mapKind(mapper, gv.WithKind(t.Kind))
	if err != nil {
		return plannedObject{}, err
	}
	if namespaced && t.Namespace == "" {
		t.Namespace = owner.Namespace
	}
	for _, r := range owner.Inventory.Resources {
		if r.Ref == t {
			return plannedObject{ref: t, gvr: mapping.Resource, namespaced: namespaced, uid: r.UID}, nil
		}
	}
	return plannedObject{}, fmt.Errorf("%w: %s %s/%s is not in the owner's inventory", ErrNotOwned, t.Kind, t.Namespace, t.Name)
}

func mapKind(mapper meta.RESTMapper, gvk schema.GroupVersionKind) (*meta.RESTMapping, bool, error) {
	if gvk.Kind == "" || gvk.Version == "" {
		return nil, false, fmt.Errorf("%w: object has no apiVersion or kind", ErrInvalidAction)
	}
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if meta.IsNoMatchError(err) {
		return nil, false, fmt.Errorf("%w: unknown resource type %s", ErrInvalidAction, gvk)
	}
	if err != nil {
		return nil, false, apiError("map "+gvk.String(), err)
	}
	return mapping, mapping.Scope.Name() == meta.RESTScopeNameNamespace, nil
}

// bundle is the owner's namespace bundle: the problem namespace first.
func bundle(owner Owner) []string {
	return append([]string{owner.Namespace}, owner.Scope.Namespaces...)
}

func clusterAllowed(owner Owner, apiVersion string, gvk schema.GroupVersionKind, name string) bool {
	if gvk.Group == "" && gvk.Kind == "Namespace" {
		return slices.Contains(bundle(owner), name)
	}
	for _, r := range owner.Scope.ClusterResources {
		if r.APIVersion == apiVersion && r.Kind == gvk.Kind && r.Name == name {
			return true
		}
	}
	return false
}

// validateOwner checks that the owner can be written as labels and that its
// namespaces are usable, distinct and not reserved.
func validateOwner(o Owner) error {
	inv := o.Inventory
	if inv.EnvironmentID == "" || inv.ProblemID == "" {
		return fmt.Errorf("%w: environment and problem IDs are required", ErrInvalidOwner)
	}
	for name, v := range map[string]string{"environment": inv.EnvironmentID, "session": inv.SessionID, "problem": inv.ProblemID} {
		if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
			return fmt.Errorf("%w: %s ID %q: %v", ErrInvalidOwner, name, v, errs)
		}
	}
	seen := map[string]bool{}
	for _, ns := range bundle(o) {
		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			return fmt.Errorf("%w: namespace %q: %v", ErrInvalidOwner, ns, errs)
		}
		if slices.Contains(reservedNamespaces, ns) {
			return fmt.Errorf("%w: namespace %q is reserved", ErrInvalidOwner, ns)
		}
		if seen[ns] {
			return fmt.Errorf("%w: namespace %q listed twice", ErrInvalidOwner, ns)
		}
		seen[ns] = true
	}
	return nil
}

// ownerLabels are the labels marking an object as inv's owner's.
func ownerLabels(inv model.Inventory) map[string]string {
	l := map[string]string{LabelEnvironmentID: inv.EnvironmentID, LabelManagedBy: ManagedByValue}
	if inv.SessionID != "" {
		l[LabelSessionID] = inv.SessionID
	}
	if inv.ProblemID != "" {
		l[LabelProblemID] = inv.ProblemID
	}
	return l
}

// ownedBy reports whether labels mark an object as inv's owner's.
func ownedBy(labels map[string]string, inv model.Inventory) bool {
	return inv.EnvironmentID != "" &&
		labels[LabelEnvironmentID] == inv.EnvironmentID &&
		labels[LabelSessionID] == inv.SessionID &&
		labels[LabelProblemID] == inv.ProblemID
}

func resourceFor(cl *clients, p plannedObject) dynamic.ResourceInterface {
	if p.namespaced {
		return cl.dyn.Resource(p.gvr).Namespace(p.ref.Namespace)
	}
	return cl.dyn.Resource(p.gvr)
}

// ensureNamespace creates a bundle namespace marked as the owner's, or
// accepts an existing one only if it already carries the owner's marks.
func ensureNamespace(ctx context.Context, cl *clients, inv model.Inventory, name string) (model.OwnedResource, error) {
	ref := model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: name}
	ns, err := cl.kube.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		ns, err = cl.kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: name, Labels: ownerLabels(inv), Annotations: map[string]string{AnnotationOwner: OwnerID(inv)},
		}}, metav1.CreateOptions{FieldManager: fieldManager})
	} else if err == nil {
		if !ownedBy(ns.Labels, inv) {
			return model.OwnedResource{}, fmt.Errorf("%w: namespace %q exists and is not marked as %s's", ErrNotOwned, name, OwnerID(inv))
		}
		if ns.DeletionTimestamp != nil {
			return model.OwnedResource{}, fmt.Errorf("%w: namespace %q is terminating", ErrUnavailable, name)
		}
	}
	if err != nil {
		return model.OwnedResource{}, apiError("ensure namespace "+name, err)
	}
	return model.OwnedResource{Ref: ref, UID: string(ns.UID), OwnerID: OwnerID(inv)}, nil
}

// applyObject creates the object, or server-side applies it over an
// existing object that carries the owner's marks (with its UID as a
// precondition). A foreign object of the same name is left untouched.
func applyObject(ctx context.Context, cl *clients, inv model.Inventory, p plannedObject) (model.OwnedResource, error) {
	ri := resourceFor(cl, p)
	what := fmt.Sprintf("%s %s/%s", p.ref.Kind, p.ref.Namespace, p.ref.Name)
	existing, err := ri.Get(ctx, p.ref.Name, metav1.GetOptions{})
	var out *unstructured.Unstructured
	switch {
	case apierrors.IsNotFound(err):
		out, err = ri.Create(ctx, p.obj, metav1.CreateOptions{FieldManager: fieldManager})
		if apierrors.IsAlreadyExists(err) {
			return model.OwnedResource{}, fmt.Errorf("%w: %s appeared concurrently", ErrNotOwned, what)
		}
	case err != nil:
	default:
		if !ownedBy(existing.GetLabels(), inv) {
			return model.OwnedResource{}, fmt.Errorf("%w: %s exists and is not marked as %s's", ErrNotOwned, what, OwnerID(inv))
		}
		obj := p.obj.DeepCopy()
		obj.SetUID(existing.GetUID())
		out, err = ri.Apply(ctx, p.ref.Name, obj, metav1.ApplyOptions{FieldManager: fieldManager, Force: true})
	}
	if err != nil {
		return model.OwnedResource{}, apiError("apply "+what, err)
	}
	return model.OwnedResource{Ref: p.ref, UID: string(out.GetUID()), OwnerID: OwnerID(inv)}, nil
}

// deleteObject deletes an inventory object only if it is still the recorded
// one (same UID, owner marks). An already-deleted object is fine.
func deleteObject(ctx context.Context, cl *clients, inv model.Inventory, p plannedObject) error {
	ri := resourceFor(cl, p)
	what := fmt.Sprintf("%s %s/%s", p.ref.Kind, p.ref.Namespace, p.ref.Name)
	existing, err := ri.Get(ctx, p.ref.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return apiError("read "+what, err)
	}
	if string(existing.GetUID()) != p.uid || !ownedBy(existing.GetLabels(), inv) {
		return fmt.Errorf("%w: %s is not the recorded object (uid %s, recorded %s)", ErrNotOwned, what, existing.GetUID(), p.uid)
	}
	uid := types.UID(p.uid)
	policy := metav1.DeletePropagationBackground
	err = ri.Delete(ctx, p.ref.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: &policy})
	if err != nil && !apierrors.IsNotFound(err) {
		return apiError("delete "+what, err)
	}
	return nil
}

// plannedInventory is the inventory as it will be once every step succeeds;
// objects not created yet have no UID. Stored as the apply intent.
func plannedInventory(inv model.Inventory, owner Owner, steps []step) model.Inventory {
	out := cloneInventory(inv)
	for _, ns := range bundle(owner) {
		ref := model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: ns}
		if !hasResource(out, ref) {
			out = upsertResource(out, model.OwnedResource{Ref: ref, OwnerID: OwnerID(inv)})
		}
	}
	for _, s := range steps {
		if s.kind != model.ActionApply {
			continue
		}
		for _, p := range s.objects {
			if !hasResource(out, p.ref) {
				out = upsertResource(out, model.OwnedResource{Ref: p.ref, OwnerID: OwnerID(inv)})
			}
		}
	}
	return out
}

func hasResource(inv model.Inventory, ref model.ObjectRef) bool {
	return slices.ContainsFunc(inv.Resources, func(r model.OwnedResource) bool { return r.Ref == ref })
}

// upsertResource adds r, replacing an entry with the same ref.
func upsertResource(inv model.Inventory, r model.OwnedResource) model.Inventory {
	for i := range inv.Resources {
		if inv.Resources[i].Ref == r.Ref {
			inv.Resources[i] = r
			return inv
		}
	}
	inv.Resources = append(inv.Resources, r)
	return inv
}

// removeResource drops the entry for ref.
func removeResource(inv model.Inventory, ref model.ObjectRef) model.Inventory {
	inv.Resources = slices.DeleteFunc(inv.Resources, func(r model.OwnedResource) bool { return r.Ref == ref })
	return inv
}

func cloneInventory(inv model.Inventory) model.Inventory {
	inv.Resources = slices.Clone(inv.Resources)
	inv.DockerIDs = slices.Clone(inv.DockerIDs)
	inv.ArtifactPaths = slices.Clone(inv.ArtifactPaths)
	return inv
}
