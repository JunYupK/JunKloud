package grader

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/JunYupK/JunKloud/internal/model"
)

// ObjectReader reads one Kubernetes object of any group/version/kind. Get
// returns an error wrapping ErrNotFound when the object (or its resource
// type) does not exist; any other error means the API could not answer and
// is graded ERROR, never FAIL.
type ObjectReader interface {
	Get(ctx context.Context, ref model.ObjectRef) (*unstructured.Unstructured, error)
}

// dynamicReader is ObjectReader on the client-go dynamic client.
type dynamicReader struct {
	client dynamic.Interface
	mapper meta.RESTMapper
}

// NewDynamicReader returns an ObjectReader over a dynamic client and a
// RESTMapper. If the mapper is resettable (discovery backed), a kind it does
// not know is retried once after a reset, so CRDs installed after startup
// are found.
func NewDynamicReader(client dynamic.Interface, mapper meta.RESTMapper) ObjectReader {
	return &dynamicReader{client: client, mapper: mapper}
}

// NewClusterReader builds an ObjectReader for a cluster from its rest config,
// with a discovery-backed mapper. The caller derives cfg from the
// environment's kubeconfig (see RESTConfig).
func NewClusterReader(cfg *rest.Config) (ObjectReader, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("grader: dynamic client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("grader: discovery client: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disc))
	return NewDynamicReader(dyn, mapper), nil
}

// RESTConfig loads a rest.Config from exactly one kubeconfig file and one
// explicit context; it never consults $KUBECONFIG or the current context.
func RESTConfig(kubeconfigPath, contextName string) (*rest.Config, error) {
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

// Get implements ObjectReader.
func (r *dynamicReader) Get(ctx context.Context, ref model.ObjectRef) (*unstructured.Unstructured, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil || gv.Version == "" {
		return nil, fmt.Errorf("%w: apiVersion %q", ErrInvalidCriterion, ref.APIVersion)
	}
	gk := schema.GroupKind{Group: gv.Group, Kind: ref.Kind}
	mapping, err := r.mapper.RESTMapping(gk, gv.Version)
	if meta.IsNoMatchError(err) {
		if reset, ok := r.mapper.(meta.ResettableRESTMapper); ok {
			reset.Reset()
			mapping, err = r.mapper.RESTMapping(gk, gv.Version)
		}
	}
	if meta.IsNoMatchError(err) {
		return nil, fmt.Errorf("%w: %s %s is not served by the cluster", ErrNotFound, ref.APIVersion, ref.Kind)
	}
	if err != nil {
		return nil, fmt.Errorf("map %s %s: %w", ref.APIVersion, ref.Kind, err)
	}
	var ri dynamic.ResourceInterface = r.client.Resource(mapping.Resource)
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		if ref.Namespace == "" {
			return nil, fmt.Errorf("%w: %s %s is namespaced and needs a namespace", ErrInvalidCriterion, ref.Kind, ref.Name)
		}
		ri = r.client.Resource(mapping.Resource).Namespace(ref.Namespace)
	}
	obj, err := ri.Get(ctx, ref.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%w: %s %s", ErrNotFound, ref.Kind, ref.Name)
	}
	if err != nil {
		return nil, fmt.Errorf("get %s %s: %w", ref.Kind, ref.Name, err)
	}
	return obj, nil
}
