//go:build linux

package environment

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/JunYupK/JunKloud/internal/model"
)

// Occupied is the read-only check a new owner's first setup needs before
// anything is recorded: it verifies id (as Check does) and reports every
// namespace of namespaces and every cluster-scoped object of cluster that
// already exists. A new session owns nothing yet, so any of them would make
// its setup fail with ErrNotOwned half-way; the caller refuses up front
// instead. Each entry names the object and who owns it (the JunKloud session
// and problem from its owner labels, "not created by JunKloud" otherwise, and
// whether it is terminating). A kind the cluster does not serve has no
// objects. Nothing is changed.
func (m *Manager) Occupied(ctx context.Context, id model.Identity, namespaces []string, cluster []model.ObjectRef) ([]string, error) {
	_, cl, err := m.verify(ctx, id)
	if err != nil {
		return nil, err
	}
	return occupied(ctx, cl.dyn, cl.mapper, namespaces, cluster)
}

func occupied(ctx context.Context, dyn dynamic.Interface, mapper meta.RESTMapper, namespaces []string, cluster []model.ObjectRef) ([]string, error) {
	var out []string
	nsGVR := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	for _, name := range namespaces {
		obj, err := dyn.Resource(nsGVR).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return nil, apiError("read namespace "+name, err)
		}
		out = append(out, fmt.Sprintf("namespace %q (%s)", name, describeOwner(obj.GetLabels(), obj.GetDeletionTimestamp() != nil)))
	}
	for _, ref := range cluster {
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrInvalidAction, ref.APIVersion, err)
		}
		mapping, err := mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: ref.Kind}, gv.Version)
		if meta.IsNoMatchError(err) {
			continue // not served: no such object can exist
		}
		if err != nil {
			return nil, apiError("resolve "+ref.Kind, err)
		}
		obj, err := dyn.Resource(mapping.Resource).Get(ctx, ref.Name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return nil, apiError(fmt.Sprintf("read %s %s", ref.Kind, ref.Name), err)
		}
		out = append(out, fmt.Sprintf("%s %q (%s)", ref.Kind, ref.Name, describeOwner(obj.GetLabels(), obj.GetDeletionTimestamp() != nil)))
	}
	return out, nil
}

func describeOwner(labels map[string]string, terminating bool) string {
	who := "not created by JunKloud"
	if labels[LabelManagedBy] == ManagedByValue && labels[LabelEnvironmentID] != "" {
		who = fmt.Sprintf("session %s, problem %s", orDash(labels[LabelSessionID]), orDash(labels[LabelProblemID]))
	}
	if terminating {
		who += ", terminating"
	}
	return who
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
