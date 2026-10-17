//go:build linux

package author

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/model"
)

// ErrCleanupTimeout: owned objects were deleted but did not disappear in
// time, typically a namespace stuck in Terminating because of a finalizer.
// The janitor never removes finalizers or forces a deletion.
var ErrCleanupTimeout = errors.New("author: cleanup did not finish")

// Janitor resets the temporary environment a validation uses: it removes
// everything the owner owns and returns only when it is gone. It is the
// validation's own cleanup of its temporary inventory; Task 13 unifies it
// with the common reset.
type Janitor interface {
	Cleanup(ctx context.Context, owner environment.Owner) error
}

const (
	defaultCleanupTimeout = 3 * time.Minute
	defaultCleanupPoll    = 500 * time.Millisecond
)

// ClusterJanitor is the Janitor on a real cluster's dynamic client.
type ClusterJanitor struct {
	dyn     dynamic.Interface
	mapper  meta.RESTMapper
	timeout time.Duration
	poll    time.Duration
}

// JanitorOption tunes a ClusterJanitor.
type JanitorOption func(*ClusterJanitor)

// WithCleanupTimeout bounds how long Cleanup waits for deletions to finish
// (default 3 minutes).
func WithCleanupTimeout(d time.Duration) JanitorOption {
	return func(j *ClusterJanitor) {
		if d > 0 {
			j.timeout = d
		}
	}
}

// WithCleanupPoll sets the gap between two checks while waiting (default
// 500ms).
func WithCleanupPoll(d time.Duration) JanitorOption {
	return func(j *ClusterJanitor) {
		if d > 0 {
			j.poll = d
		}
	}
}

// NewJanitor is the seam unit tests use with a fake dynamic client.
func NewJanitor(dyn dynamic.Interface, mapper meta.RESTMapper, opts ...JanitorOption) *ClusterJanitor {
	j := &ClusterJanitor{dyn: dyn, mapper: mapper, timeout: defaultCleanupTimeout, poll: defaultCleanupPoll}
	for _, o := range opts {
		o(j)
	}
	return j
}

// NewClusterJanitor builds the Janitor for the cluster that one explicit
// kubeconfig file and context point at (the environment's kubeconfig and
// Identity.Context). It never consults $KUBECONFIG or the current context.
func NewClusterJanitor(kubeconfigPath, contextName string, opts ...JanitorOption) (*ClusterJanitor, error) {
	if kubeconfigPath == "" || contextName == "" {
		return nil, errors.New("author: kubeconfig path and context are both required")
	}
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: contextName}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("author: load kubeconfig: %w", err)
	}
	cfg.Timeout = 15 * time.Second
	cfg.UserAgent = "junkloud-author"
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("author: dynamic client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("author: discovery client: %w", err)
	}
	return NewJanitor(dyn, restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disc)), opts...), nil
}

// victim is one object of the owner that Cleanup deleted and now waits for.
type victim struct {
	gvr  schema.GroupVersionResource
	name string
	uid  types.UID
	what string
}

func (v victim) resource(j *ClusterJanitor) dynamic.ResourceInterface { return j.dyn.Resource(v.gvr) }

// Cleanup deletes what the owner owns in the cluster and waits until it is
// gone:
//
//   - every namespace of the owner's bundle (Owner.Namespace plus
//     Scope.Namespaces), which takes all objects inside with it;
//   - the cluster-scoped objects the problem scope declares or the owner's
//     inventory records.
//
// An object is the owner's only if it carries the owner's environment,
// session and problem labels, and, when the inventory records a UID, still
// has that UID. Anything else, however it is named, is left untouched and is
// not an error (a later apply of the problem reports the conflict). Deletes
// carry the UID as a precondition. Cleanup is idempotent and does nothing on
// a clean cluster. It never edits finalizers: a namespace that stays in
// Terminating ends in ErrCleanupTimeout naming it and its finalizers.
func (j *ClusterJanitor) Cleanup(ctx context.Context, owner environment.Owner) error {
	inv := owner.Inventory
	var victims []victim
	for _, ref := range cleanupRefs(owner) {
		v, err := j.claim(ctx, inv, ref)
		if err != nil {
			return err
		}
		if v != nil {
			victims = append(victims, *v)
		}
	}
	policy := metav1.DeletePropagationBackground
	for _, v := range victims {
		uid := v.uid
		err := v.resource(j).Delete(ctx, v.name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: &policy,
		})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return fmt.Errorf("author: cleanup: delete %s: %w", v.what, err)
		}
	}
	return j.wait(ctx, victims)
}

// cleanupRefs lists what Cleanup considers: the bundle's namespaces and the
// cluster-scoped objects declared by the scope or recorded in the inventory.
func cleanupRefs(owner environment.Owner) []model.ObjectRef {
	var refs []model.ObjectRef
	add := func(r model.ObjectRef) {
		if !slices.Contains(refs, r) {
			refs = append(refs, r)
		}
	}
	for _, r := range owner.Scope.ClusterResources {
		add(r)
	}
	for _, r := range owner.Inventory.Resources {
		if r.Ref.Namespace == "" && !(r.Ref.APIVersion == "v1" && r.Ref.Kind == "Namespace") {
			add(r.Ref)
		}
	}
	for _, ns := range append([]string{owner.Namespace}, owner.Scope.Namespaces...) {
		add(model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: ns})
	}
	return refs
}

// claim reads ref and returns it as a victim if it is the owner's, or nil if
// it is absent, not served, not the owner's or not the recorded object.
func (j *ClusterJanitor) claim(ctx context.Context, inv model.Inventory, ref model.ObjectRef) (*victim, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("author: cleanup: %w", err)
	}
	mapping, err := j.mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: ref.Kind}, gv.Version)
	if meta.IsNoMatchError(err) {
		return nil, nil // the cluster does not serve the kind, so nothing of it exists
	}
	if err != nil {
		return nil, fmt.Errorf("author: cleanup: map %s %s: %w", ref.APIVersion, ref.Kind, err)
	}
	if mapping.Scope.Name() != meta.RESTScopeNameRoot {
		return nil, fmt.Errorf("author: cleanup: %s %q is not cluster-scoped", ref.Kind, ref.Name)
	}
	obj, err := j.dyn.Resource(mapping.Resource).Get(ctx, ref.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("author: cleanup: read %s %s: %w", ref.Kind, ref.Name, err)
	}
	if !ownedBy(obj.GetLabels(), inv) {
		return nil, nil
	}
	for _, r := range inv.Resources {
		if r.Ref == ref && r.UID != "" && r.UID != string(obj.GetUID()) {
			return nil, nil
		}
	}
	return &victim{gvr: mapping.Resource, name: ref.Name, uid: obj.GetUID(), what: ref.Kind + " " + ref.Name}, nil
}

// ownedBy reports whether labels mark an object as inv's owner's: the same
// labels the Manager puts on everything it applies.
func ownedBy(labels map[string]string, inv model.Inventory) bool {
	return inv.EnvironmentID != "" &&
		labels[environment.LabelEnvironmentID] == inv.EnvironmentID &&
		labels[environment.LabelSessionID] == inv.SessionID &&
		labels[environment.LabelProblemID] == inv.ProblemID
}

// wait polls until every victim is gone (absent, or replaced by an object
// with another UID).
func (j *ClusterJanitor) wait(ctx context.Context, victims []victim) error {
	deadline := time.Now().Add(j.timeout)
	for {
		var left []string
		for _, v := range victims {
			obj, err := v.resource(j).Get(ctx, v.name, metav1.GetOptions{})
			switch {
			case apierrors.IsNotFound(err):
			case err != nil:
				return fmt.Errorf("author: cleanup: read %s: %w", v.what, err)
			case obj.GetUID() != v.uid:
			default:
				left = append(left, describePending(v, obj))
			}
		}
		if len(left) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w within %v: %s; JunKloud never removes finalizers or forces a deletion",
				ErrCleanupTimeout, j.timeout, strings.Join(left, "; "))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("author: cleanup interrupted: %w", ctx.Err())
		case <-time.After(j.poll):
		}
	}
}

func describePending(v victim, obj *unstructured.Unstructured) string {
	state := "still present"
	if obj.GetDeletionTimestamp() != nil {
		state = "Terminating"
	}
	if f := obj.GetFinalizers(); len(f) > 0 {
		state += fmt.Sprintf(" (finalizers: %v)", f)
	}
	return v.what + " is " + state
}
