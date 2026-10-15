package grader

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/JunYupK/JunKloud/internal/model"
)

// ListQuery selects objects of one kind. LabelSelector uses the Kubernetes
// selector syntax ("app=web,tier in (a,b)"); empty selects all.
type ListQuery struct {
	APIVersion    string
	Kind          string
	Namespace     string
	LabelSelector string
	// FieldSelector uses the Kubernetes field selector syntax
	// ("involvedObject.name=web-1"); empty selects all.
	FieldSelector string
}

// ObjectReader reads Kubernetes objects of any group/version/kind as decoded
// JSON objects. Get returns an error wrapping ErrNotFound when the object
// (or its resource type) does not exist; any other error means the API could
// not answer and is graded ERROR, never FAIL.
type ObjectReader interface {
	Get(ctx context.Context, ref model.ObjectRef) (map[string]any, error)
	List(ctx context.Context, q ListQuery) ([]map[string]any, error)
}

// dynamicReader is ObjectReader on the client-go dynamic client.
type dynamicReader struct {
	client dynamic.Interface
	mapper meta.RESTMapper
}

// newDynamicReader is the seam unit tests use with a fake dynamic client. If
// the mapper is resettable (discovery backed), a kind it does not know is
// retried once after a reset, so CRDs installed after startup are found.
func newDynamicReader(client dynamic.Interface, mapper meta.RESTMapper) ObjectReader {
	return &dynamicReader{client: client, mapper: mapper}
}

// NewClusterReader builds an ObjectReader for the cluster that one explicit
// kubeconfig file and context point at (typically the environment's
// kubeconfig and Identity.Context). It never consults $KUBECONFIG or the
// kubeconfig's current context.
func NewClusterReader(kubeconfigPath, contextName string) (ObjectReader, error) {
	cfg, err := restConfig(kubeconfigPath, contextName)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("grader: dynamic client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("grader: discovery client: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disc))
	return newDynamicReader(dyn, mapper), nil
}

func restConfig(kubeconfigPath, contextName string) (*rest.Config, error) {
	if kubeconfigPath == "" || contextName == "" {
		return nil, errors.New("grader: kubeconfig path and context are both required")
	}
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: contextName}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("grader: load kubeconfig: %w", err)
	}
	if cfg.QPS == 0 {
		cfg.QPS, cfg.Burst = 20, 40
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 15 * time.Second
	}
	cfg.UserAgent = "junkloud-grader"
	return cfg, nil
}

// resolve maps a kind to its REST resource, retrying once after a discovery
// reset. A kind the cluster does not serve is ErrNotFound.
func (r *dynamicReader) resolve(apiVersion, kind string) (*meta.RESTMapping, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil || gv.Version == "" {
		return nil, fmt.Errorf("%w: apiVersion %q", ErrInvalidCriterion, apiVersion)
	}
	gk := schema.GroupKind{Group: gv.Group, Kind: kind}
	mapping, err := r.mapper.RESTMapping(gk, gv.Version)
	if meta.IsNoMatchError(err) {
		if reset, ok := r.mapper.(meta.ResettableRESTMapper); ok {
			reset.Reset()
			mapping, err = r.mapper.RESTMapping(gk, gv.Version)
		}
	}
	if meta.IsNoMatchError(err) {
		return nil, fmt.Errorf("%w: %s %s is not served by the cluster", ErrNotFound, apiVersion, kind)
	}
	if err != nil {
		return nil, fmt.Errorf("map %s %s: %w", apiVersion, kind, err)
	}
	return mapping, nil
}

func (r *dynamicReader) resource(mapping *meta.RESTMapping, namespace, kind, name string) (dynamic.ResourceInterface, error) {
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		if namespace == "" {
			return nil, fmt.Errorf("%w: %s %s is namespaced and needs a namespace", ErrInvalidCriterion, kind, name)
		}
		return r.client.Resource(mapping.Resource).Namespace(namespace), nil
	}
	return r.client.Resource(mapping.Resource), nil
}

// Get implements ObjectReader.
func (r *dynamicReader) Get(ctx context.Context, ref model.ObjectRef) (map[string]any, error) {
	mapping, err := r.resolve(ref.APIVersion, ref.Kind)
	if err != nil {
		return nil, err
	}
	ri, err := r.resource(mapping, ref.Namespace, ref.Kind, ref.Name)
	if err != nil {
		return nil, err
	}
	obj, err := ri.Get(ctx, ref.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%w: %s %s", ErrNotFound, ref.Kind, ref.Name)
	}
	if err != nil {
		return nil, fmt.Errorf("get %s %s: %w", ref.Kind, ref.Name, err)
	}
	return obj.Object, nil
}

// List implements ObjectReader.
func (r *dynamicReader) List(ctx context.Context, q ListQuery) ([]map[string]any, error) {
	mapping, err := r.resolve(q.APIVersion, q.Kind)
	if err != nil {
		return nil, err
	}
	ri, err := r.resource(mapping, q.Namespace, q.Kind, "")
	if err != nil {
		return nil, err
	}
	list, err := ri.List(ctx, metav1.ListOptions{LabelSelector: q.LabelSelector, FieldSelector: q.FieldSelector})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", q.Kind, err)
	}
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, list.Items[i].Object)
	}
	return out, nil
}
