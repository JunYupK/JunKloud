//go:build linux

package environment

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	certv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/JunYupK/JunKloud/runtime/platform"
)

// Capability names: what a problem may require and the doctor verifies.
const (
	CapabilityDNS           = "dns"
	CapabilityService       = "service"
	CapabilityNetworkPolicy = "network-policy"
	CapabilityIngress       = "ingress"
	CapabilityStorage       = "storage"
	CapabilityMetrics       = "metrics"
	CapabilityImageBuild    = "image-build"
)

// Capabilities lists every capability, in the order the doctor reports them.
var Capabilities = []string{
	CapabilityDNS, CapabilityService, CapabilityNetworkPolicy, CapabilityIngress,
	CapabilityStorage, CapabilityMetrics, CapabilityImageBuild,
}

// Capability check statuses.
const (
	StatusPass = "PASS"
	StatusFail = "FAIL"
)

// CapabilityCheck is the verified state of one capability. A FAIL carries a
// reason a learner can act on; a capability that is not PASS is unavailable
// and problems requiring it must not be served.
type CapabilityCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// Platform component versions (runtime/versions.lock.yaml, platform.*).
const (
	CloudProviderKindVersion = "v0.12.0"
	MetricsServerVersion     = "v0.9.0"
)

// Pinned platform images. They must equal runtime/versions.lock.yaml
// (unit-tested). Node images are preloaded into the kind node; host images
// run on the shared Docker engine.
var (
	// CloudProviderKindImage provides Ingress (host).
	CloudProviderKindImage = PinnedImage{
		Name:   "registry.k8s.io/cloud-provider-kind/cloud-controller-manager:v0.12.0",
		Digest: "sha256:1b97632e5dfec8f83c7e970cf18cedb9e6b707e272011d712cabf6ab59d323c9",
	}
	// EnvoyProxyImage is the proxy cloud-provider-kind starts per Ingress
	// gateway (host); pre-pulled so cloud-provider-kind never pulls itself.
	EnvoyProxyImage = PinnedImage{
		Name:   "docker.io/envoyproxy/envoy:v1.33.2",
		Digest: "sha256:e2baf0b155d0f54881b54621fdd997e3ec81dc6454f299d4e590afe18256a176",
	}
	// MetricsServerImage serves the metrics API (node).
	MetricsServerImage = PinnedImage{
		Name:   "registry.k8s.io/metrics-server/metrics-server:v0.9.0",
		Digest: "sha256:d9862115e7c7881280d3d75ca26bda8ffc0fc213315979575bf23ce9826205c0",
	}
	// ProbeImage runs every doctor probe pod and is the image-build probe's
	// base (node).
	ProbeImage = PinnedImage{
		Name:   "docker.io/library/busybox:1.37.0",
		Digest: "sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e",
	}
)

// Platform objects, labels and names.
const (
	// PlatformNamespace holds the doctor's probe objects and nothing else.
	// It is reserved: never a problem namespace.
	PlatformNamespace = "junkloud-platform"
	// LabelComponent marks platform objects and containers
	// (ComponentPlatform, ComponentCloudProvider).
	LabelComponent         = "junkloud.io/component"
	ComponentPlatform      = "platform"
	ComponentCloudProvider = "cloud-provider-kind"
	// LabelProbeRun marks every object one doctor run creates.
	LabelProbeRun = "junkloud.io/probe-run"
	labelProbeRun = LabelProbeRun
	// labelConfig on the cloud-provider-kind container identifies its run
	// configuration, so a changed configuration recreates it.
	labelConfig = "junkloud.io/config"
	// ImageBuildProbeRepository is the repository of the image-build probe's
	// throwaway images (removed after each run).
	ImageBuildProbeRepository = "junkloud.local/probe/image-build"

	// OperationPlatform records a platform installation (Planned holds the
	// platform inventory as it will be).
	OperationPlatform = "environment-platform"
	// platformProblemID keys the platform inventory: (environment, "",
	// "platform"). Session "" is environment-level; problem inventories
	// always have a session.
	platformProblemID = "platform"

	metricsNamespace  = "kube-system"
	metricsName       = "metrics-server"
	metricsTLSSecret  = "junkloud-metrics-server-tls"
	metricsTLSDir     = "/junkloud-tls"
	metricsAPIService = "v1beta1.metrics.k8s.io"
	// cloudProviderLBLabel marks the load-balancer containers
	// cloud-provider-kind creates for a cluster (value: cluster name).
	cloudProviderLBLabel = "io.x-k8s.cloud-provider-kind.cluster"
	cloudProviderClass   = "cloud-provider-kind"
	dockerSocket         = "/var/run/docker.sock"
)

// Time limits.
const (
	pullTimeout        = 10 * time.Minute
	loadTimeout        = 5 * time.Minute
	kubeletTLSTimeout  = 2 * time.Minute
	metricsReadyTimout = 3 * time.Minute
	cloudProviderWait  = time.Minute
	pollInterval       = time.Second
)

// ErrInvalidImage: an image reference is not a tagged name plus a sha256
// digest. Nothing was run.
var ErrInvalidImage = errors.New("environment: invalid image reference")

// PinnedImage is an image pinned by tag and by the digest of its
// (multi-arch) index. Name is "<repository>:<tag>"; pods and containers
// refer to Name, whose content JunKloud has pulled by Digest.
type PinnedImage struct {
	Name   string
	Digest string
}

// Ref is the pull reference "<Name>@<Digest>".
func (p PinnedImage) Ref() string { return p.Name + "@" + p.Digest }

var (
	imageNamePattern   = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?::[0-9]+)?(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)+:[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	imageDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	archPattern        = regexp.MustCompile(`^[a-z0-9]+$`)
	// envIDPattern: environment IDs are generated as 12 hex digits; they end
	// up in container names and in the cloud-provider-kind wrapper script.
	envIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)
)

func (p PinnedImage) validate() error {
	if !imageNamePattern.MatchString(p.Name) || strings.HasSuffix(p.Name, ":latest") {
		return fmt.Errorf("%w: %q is not <registry>/<repository>:<tag> with a fixed tag", ErrInvalidImage, p.Name)
	}
	if !imageDigestPattern.MatchString(p.Digest) {
		return fmt.Errorf("%w: %q is not a sha256 digest", ErrInvalidImage, p.Digest)
	}
	return nil
}

// PreloadImages verifies id and loads each image into the environment's
// nodes, which never pull from a registry: the exact digest is pulled for
// the node's platform, tagged as Name, saved for that platform only and
// loaded with `kind load image-archive`. Pods must then refer to Name with
// imagePullPolicy IfNotPresent or Never.
func (m *Manager) PreloadImages(ctx context.Context, id model.Identity, images ...PinnedImage) error {
	_, cl, err := m.verify(ctx, id)
	if err != nil {
		return err
	}
	arch, err := nodeArch(ctx, cl)
	if err != nil {
		return err
	}
	for _, img := range images {
		if err := m.preload(ctx, id.ClusterName, arch, img, true); err != nil {
			return err
		}
	}
	return nil
}

// preload pulls img by digest for linux/<arch> and tags it as img.Name; with
// intoNode it also loads it into cluster's nodes through a temporary archive.
func (m *Manager) preload(ctx context.Context, cluster, arch string, img PinnedImage, intoNode bool) error {
	if err := img.validate(); err != nil {
		return err
	}
	if !archPattern.MatchString(arch) {
		return fmt.Errorf("environment: invalid node architecture %q", arch)
	}
	platformFlag := "linux/" + arch
	// A failed pull (offline, registry rate limit) is tolerated only when
	// the pinned digest's content for this platform is complete in the
	// local store, which `docker save` of it proves below.
	pullErr := m.command(ctx, pullTimeout, "docker", "pull", "--quiet", "--platform", platformFlag, img.Ref())
	if pullErr != nil {
		pullErr = fmt.Errorf("environment: pull %s: %w", img.Ref(), pullErr)
	}
	notLocal := func(err error) error {
		if pullErr != nil {
			return fmt.Errorf("%w (and it is not complete in the local image store: %v)", pullErr, err)
		}
		return err
	}
	// Re-point the tag on the shared engine at the pinned content. This
	// overwrites a local tag of the same name (as `docker pull <tag>` of an
	// unchanged upstream tag would); pods and containers refer to the tag
	// because a digest reference cannot match after `docker save
	// --platform` (it writes a reduced index). Accepted by ruling R19.
	if err := m.command(ctx, dockerTimeout, "docker", "tag", img.Ref(), img.Name); err != nil {
		return notLocal(fmt.Errorf("environment: tag %s: %w", img.Name, err))
	}
	if !intoNode && pullErr == nil {
		return nil
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return fmt.Errorf("environment: create %s: %w", m.dir, err)
	}
	tmp, err := os.MkdirTemp(m.dir, ".preload-*")
	if err != nil {
		return fmt.Errorf("environment: preload %s: %w", img.Name, err)
	}
	defer os.RemoveAll(tmp)
	archive := filepath.Join(tmp, "image.tar")
	if err := m.command(ctx, loadTimeout, "docker", "save", "--platform", platformFlag, "--output", archive, img.Name); err != nil {
		return notLocal(fmt.Errorf("environment: save %s: %w", img.Name, err))
	}
	if !intoNode {
		return nil
	}
	if err := m.command(ctx, loadTimeout, "kind", "load", "image-archive", archive, "--name", cluster); err != nil {
		return fmt.Errorf("environment: load %s into %s: %w", img.Name, cluster, err)
	}
	return nil
}

// command runs one external command with a time limit.
func (m *Manager) command(ctx context.Context, limit time.Duration, exe string, args ...string) error {
	_, err := m.output(ctx, limit, exe, args...)
	return err
}

func (m *Manager) output(ctx context.Context, limit time.Duration, exe string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	res, err := m.runner.Run(ctx, exe, args, nil)
	return res.Stdout, err
}

// nodeArch is the CPU architecture shared by every node.
func nodeArch(ctx context.Context, cl *clients) (string, error) {
	nodes, err := cl.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", apiError("list nodes", err)
	}
	arch := ""
	for _, n := range nodes.Items {
		a := n.Status.NodeInfo.Architecture
		if arch != "" && a != arch {
			return "", fmt.Errorf("environment: nodes have mixed architectures %q and %q", arch, a)
		}
		arch = a
	}
	if arch == "" {
		return "", fmt.Errorf("%w: no node reports its architecture", ErrUnavailable)
	}
	return arch, nil
}

// EnsurePlatform verifies id and installs (or repairs) the shared platform
// components behind the capabilities, recording what it owns as the
// platform inventory (environment, "", "platform") under an intent
// operation:
//
//   - preloads the node images (metrics-server, probe image) and pulls the
//     host images (cloud-provider-kind, its Envoy proxy), all by digest;
//   - approves the kubelet-serving CSRs of this environment's own nodes
//     (nothing else) and waits until the kubelets serve TLS;
//   - creates the platform namespace for doctor probes;
//   - applies the vendored metrics-server manifest with verified TLS on both
//     hops, and waits for its API to be available;
//   - starts, restarts or recreates the cloud-provider-kind container.
//
// It runs every step it can and returns all failures joined; the doctor
// then reports which capabilities work. Callers hold the environment lock.
func (m *Manager) EnsurePlatform(ctx context.Context, id model.Identity) error {
	_, cl, err := m.verify(ctx, id)
	if err != nil {
		return err
	}
	inv, err := m.platformInventory(ctx, id.ID)
	if err != nil {
		return err
	}
	opID, err := newID()
	if err != nil {
		return err
	}
	op := model.Operation{
		ID: "platform-" + opID, EnvironmentID: id.ID, Kind: OperationPlatform, Phase: PhasePlanned,
		Planned: plannedPlatform(inv, id.ID), CreatedAt: m.now(),
	}
	if err := m.store.PutOperation(ctx, op); err != nil {
		return fmt.Errorf("environment: record platform intent: %w", err)
	}
	record := func(r model.OwnedResource) error {
		inv = upsertResource(inv, r)
		return m.store.PutInventory(ctx, inv)
	}

	var errs []error
	arch, err := nodeArch(ctx, cl)
	if err != nil {
		return err
	}
	for _, img := range []PinnedImage{ProbeImage, MetricsServerImage} {
		if err := m.preload(ctx, id.ClusterName, arch, img, true); err != nil {
			errs = append(errs, err)
		}
	}
	for _, img := range []PinnedImage{CloudProviderKindImage, EnvoyProxyImage} {
		if err := m.preload(ctx, id.ClusterName, arch, img, false); err != nil {
			errs = append(errs, err)
		}
	}
	if err := approveKubeletServing(ctx, cl, id.ID); err != nil {
		errs = append(errs, err)
	}
	ns, err := ensurePlatformNamespace(ctx, cl, id.ID)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if err := record(model.OwnedResource{Ref: model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: PlatformNamespace}, UID: string(ns.UID), OwnerID: platformProblemID}); err != nil {
		return err
	}
	if err := m.installMetricsServer(ctx, cl, id.ID, record); err != nil {
		errs = append(errs, err)
	}
	// A container that started but is not ready yet is still recorded.
	cid, err := m.ensureCloudProvider(ctx, cl, id)
	if err != nil {
		errs = append(errs, err)
	}
	if cid != "" && !slices.Equal(inv.DockerIDs, []string{cid}) {
		inv.DockerIDs = []string{cid}
		if err := m.store.PutInventory(ctx, inv); err != nil {
			return err
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	op.Phase = PhaseDone
	op.Planned = inv
	return m.store.PutOperation(ctx, op)
}

// platformInventory returns the recorded platform inventory, or an empty one.
func (m *Manager) platformInventory(ctx context.Context, envID string) (model.Inventory, error) {
	inv, err := m.store.GetInventory(ctx, envID, "", platformProblemID)
	if errors.Is(err, store.ErrNotFound) {
		return model.Inventory{EnvironmentID: envID, ProblemID: platformProblemID}, nil
	}
	return inv, err
}

// plannedPlatform is the platform inventory as EnsurePlatform will leave it;
// entries not created yet have no UID.
func plannedPlatform(inv model.Inventory, envID string) model.Inventory {
	out := cloneInventory(inv)
	refs := []model.ObjectRef{{APIVersion: "v1", Kind: "Namespace", Name: PlatformNamespace}}
	if objs, err := platformObjects(envID, metricsTLS{}); err == nil {
		for _, o := range objs {
			refs = append(refs, model.ObjectRef{APIVersion: o.GetAPIVersion(), Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName()})
		}
	}
	for _, ref := range refs {
		if !hasResource(out, ref) {
			out = upsertResource(out, model.OwnedResource{Ref: ref, OwnerID: platformProblemID})
		}
	}
	return out
}

// platformLabels mark an object or container as this environment's platform.
func platformLabels(envID, component string) map[string]string {
	return map[string]string{LabelEnvironmentID: envID, LabelManagedBy: ManagedByValue, LabelComponent: component}
}

func platformOwned(labels map[string]string, envID string) bool {
	return envID != "" && labels[LabelEnvironmentID] == envID && labels[LabelComponent] == ComponentPlatform
}

// ensurePlatformNamespace creates the probe namespace or checks that the
// existing one is this environment's.
func ensurePlatformNamespace(ctx context.Context, cl *clients, envID string) (*corev1.Namespace, error) {
	ns, err := cl.kube.CoreV1().Namespaces().Get(ctx, PlatformNamespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		ns, err = cl.kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: PlatformNamespace, Labels: platformLabels(envID, ComponentPlatform),
		}}, metav1.CreateOptions{FieldManager: fieldManager})
	}
	if err != nil {
		return nil, apiError("ensure namespace "+PlatformNamespace, err)
	}
	if !platformOwned(ns.Labels, envID) {
		return nil, fmt.Errorf("%w: namespace %s is not marked as environment %s's platform", ErrNotOwned, PlatformNamespace, envID)
	}
	if ns.DeletionTimestamp != nil {
		return nil, fmt.Errorf("%w: namespace %s is terminating; retry shortly", ErrUnavailable, PlatformNamespace)
	}
	return ns, nil
}

// ---- kubelet serving certificates ----

// nodeNames are the DNS names and IP addresses one node may put in its
// kubelet serving certificate.
type nodeNames struct {
	dns []string
	ips []string
}

// ownNodes lists this environment's nodes (every node must carry its label)
// with the names each may claim.
func ownNodes(ctx context.Context, cl *clients, envID string) (map[string]nodeNames, error) {
	if err := checkNodeLabels(ctx, cl, envID); err != nil {
		return nil, err
	}
	list, err := cl.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, apiError("list nodes", err)
	}
	out := map[string]nodeNames{}
	for _, n := range list.Items {
		names := nodeNames{dns: []string{n.Name}}
		for _, a := range n.Status.Addresses {
			switch a.Type {
			case corev1.NodeHostName, corev1.NodeInternalDNS, corev1.NodeExternalDNS:
				names.dns = append(names.dns, a.Address)
			case corev1.NodeInternalIP, corev1.NodeExternalIP:
				names.ips = append(names.ips, a.Address)
			}
		}
		out[n.Name] = names
	}
	return out, nil
}

// validKubeletServingCSR accepts only an undecided kubernetes.io/kubelet-
// serving request made by one of nodes (system:node:<name> in system:nodes)
// for a server certificate whose subject is that node and whose names are
// all that node's own names and addresses.
func validKubeletServingCSR(csr *certv1.CertificateSigningRequest, nodes map[string]nodeNames) error {
	if csr.Spec.SignerName != certv1.KubeletServingSignerName {
		return fmt.Errorf("signer %q", csr.Spec.SignerName)
	}
	for _, c := range csr.Status.Conditions {
		if c.Type == certv1.CertificateApproved || c.Type == certv1.CertificateDenied || c.Type == certv1.CertificateFailed {
			return fmt.Errorf("already %s", c.Type)
		}
	}
	nodeName, ok := strings.CutPrefix(csr.Spec.Username, "system:node:")
	if !ok {
		return fmt.Errorf("requested by %q, not a node", csr.Spec.Username)
	}
	names, ok := nodes[nodeName]
	if !ok {
		return fmt.Errorf("node %q is not one of this environment's nodes", nodeName)
	}
	if !slices.Contains(csr.Spec.Groups, "system:nodes") {
		return fmt.Errorf("requester is not in group system:nodes")
	}
	server := false
	for _, u := range csr.Spec.Usages {
		switch u {
		case certv1.UsageServerAuth:
			server = true
		case certv1.UsageDigitalSignature, certv1.UsageKeyEncipherment:
		default:
			return fmt.Errorf("usage %q", u)
		}
	}
	if !server {
		return fmt.Errorf("no server auth usage")
	}
	block, _ := pem.Decode(csr.Spec.Request)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return fmt.Errorf("request is not a PEM certificate request")
	}
	req, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse request: %v", err)
	}
	if err := req.CheckSignature(); err != nil {
		return fmt.Errorf("request signature: %v", err)
	}
	if req.Subject.CommonName != csr.Spec.Username || !slices.Equal(req.Subject.Organization, []string{"system:nodes"}) {
		return fmt.Errorf("subject %q does not match requester %q", req.Subject.String(), csr.Spec.Username)
	}
	if len(req.EmailAddresses) > 0 || len(req.URIs) > 0 || len(req.DNSNames)+len(req.IPAddresses) == 0 {
		return fmt.Errorf("request names are not DNS names and IP addresses")
	}
	for _, d := range req.DNSNames {
		if !slices.Contains(names.dns, d) {
			return fmt.Errorf("DNS name %q is not node %s's", d, nodeName)
		}
	}
	for _, ip := range req.IPAddresses {
		if !slices.ContainsFunc(names.ips, func(s string) bool { return net.ParseIP(s).Equal(ip) }) {
			return fmt.Errorf("IP address %s is not node %s's", ip, nodeName)
		}
	}
	return nil
}

// approveKubeletServing approves the valid pending kubelet-serving CSRs of
// this environment's nodes, leaving every other CSR untouched, until every
// node's kubelet serves TLS (checked through the API server's node proxy).
func approveKubeletServing(ctx context.Context, cl *clients, envID string) error {
	nodes, err := ownNodes(ctx, cl, envID)
	if err != nil {
		return err
	}
	var last error
	err = wait.PollUntilContextTimeout(ctx, 2*time.Second, kubeletTLSTimeout, true, func(ctx context.Context) (bool, error) {
		csrs, err := cl.kube.CertificatesV1().CertificateSigningRequests().List(ctx, metav1.ListOptions{})
		if err != nil {
			last = apiError("list certificate signing requests", err)
			return false, nil
		}
		for i := range csrs.Items {
			csr := &csrs.Items[i]
			if validKubeletServingCSR(csr, nodes) != nil {
				continue
			}
			csr.Status.Conditions = append(csr.Status.Conditions, certv1.CertificateSigningRequestCondition{
				Type: certv1.CertificateApproved, Status: corev1.ConditionTrue,
				Reason: "JunKloudKubeletServing", Message: "kubelet serving certificate of JunKloud environment " + envID,
			})
			if _, err := cl.kube.CertificatesV1().CertificateSigningRequests().UpdateApproval(ctx, csr.Name, csr, metav1.UpdateOptions{FieldManager: fieldManager}); err != nil && !apierrors.IsConflict(err) {
				last = apiError("approve "+csr.Name, err)
			}
		}
		for name := range nodes {
			err := cl.kube.CoreV1().RESTClient().Get().AbsPath("/api/v1/nodes", name, "proxy", "healthz").Do(ctx).Error()
			if err != nil {
				last = fmt.Errorf("kubelet on %s does not serve TLS yet: %v", name, err)
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("%w: kubelet serving certificates: %v (last: %v)", ErrUnavailable, err, last)
	}
	return nil
}

// ---- metrics-server ----

// metricsTLS is the serving certificate metrics-server presents to the API
// server and the CA the API server verifies it with.
type metricsTLS struct {
	caPEM   []byte
	certPEM []byte
	keyPEM  []byte
}

// metricsServerDNS is the name the API server dials for the APIService.
const metricsServerDNS = metricsName + "." + metricsNamespace + ".svc"

func newMetricsTLS(now time.Time) (metricsTLS, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return metricsTLS{}, err
	}
	serial := func() *big.Int {
		n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
		return n
	}
	caTmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "junkloud-metrics-server-ca"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return metricsTLS{}, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return metricsTLS{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return metricsTLS{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: metricsServerDNS},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{metricsServerDNS, metricsServerDNS + ".cluster.local", metricsName + "." + metricsNamespace},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return metricsTLS{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return metricsTLS{}, err
	}
	return metricsTLS{
		caPEM:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// usable reports whether t is a complete bundle whose certificate chains to
// its CA for the metrics-server name and stays valid for a month.
func (t metricsTLS) usable(now time.Time) bool {
	block, _ := pem.Decode(t.certPEM)
	if block == nil || len(t.keyPEM) == 0 {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(t.caPEM) {
		return false
	}
	_, err = cert.Verify(x509.VerifyOptions{DNSName: metricsServerDNS, Roots: pool, CurrentTime: now.AddDate(0, 1, 0)})
	return err == nil
}

// platformObjects renders the vendored metrics-server manifest for envID:
// every object gets the platform labels; metrics-server serves the given
// certificate (from a Secret) and the APIService verifies it against the
// given CA instead of skipping verification. The TLS Secret comes first.
func platformObjects(envID string, tlsBundle metricsTLS) ([]*unstructured.Unstructured, error) {
	raw, err := fs.ReadFile(platform.FS, platform.MetricsServerManifest)
	if err != nil {
		return nil, fmt.Errorf("environment: read vendored metrics-server manifest: %w", err)
	}
	secret := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": metricsTLSSecret, "namespace": metricsNamespace},
		"type":     string(corev1.SecretTypeTLS),
		"data": map[string]any{
			corev1.TLSCertKey:       base64.StdEncoding.EncodeToString(tlsBundle.certPEM),
			corev1.TLSPrivateKeyKey: base64.StdEncoding.EncodeToString(tlsBundle.keyPEM),
			"ca.crt":                base64.StdEncoding.EncodeToString(tlsBundle.caPEM),
		},
	}}
	objs := []*unstructured.Unstructured{secret}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for {
		var obj map[string]any
		if err := dec.Decode(&obj); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("environment: decode metrics-server manifest: %w", err)
		}
		if len(obj) == 0 {
			continue
		}
		objs = append(objs, &unstructured.Unstructured{Object: obj})
	}
	for _, u := range objs {
		labels := u.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		for k, v := range platformLabels(envID, ComponentPlatform) {
			labels[k] = v
		}
		u.SetLabels(labels)
		switch {
		case u.GetKind() == "Deployment" && u.GetName() == metricsName:
			if err := serveMetricsTLS(u); err != nil {
				return nil, err
			}
		case u.GetKind() == "APIService":
			unstructured.RemoveNestedField(u.Object, "spec", "insecureSkipTLSVerify")
			if err := unstructured.SetNestedField(u.Object, base64.StdEncoding.EncodeToString(tlsBundle.caPEM), "spec", "caBundle"); err != nil {
				return nil, err
			}
		}
	}
	return objs, nil
}

// serveMetricsTLS mounts the TLS Secret into the metrics-server container
// and points its serving flags at it.
func serveMetricsTLS(u *unstructured.Unstructured) error {
	containers, _, err := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "containers")
	if err != nil || len(containers) != 1 {
		return fmt.Errorf("environment: unexpected metrics-server deployment shape (%v)", err)
	}
	c := containers[0].(map[string]any)
	args, _ := c["args"].([]any)
	c["args"] = append(args, "--tls-cert-file="+metricsTLSDir+"/"+corev1.TLSCertKey, "--tls-private-key-file="+metricsTLSDir+"/"+corev1.TLSPrivateKeyKey)
	mounts, _ := c["volumeMounts"].([]any)
	c["volumeMounts"] = append(mounts, map[string]any{"name": "junkloud-tls", "mountPath": metricsTLSDir, "readOnly": true})
	if err := unstructured.SetNestedSlice(u.Object, containers, "spec", "template", "spec", "containers"); err != nil {
		return err
	}
	volumes, _, _ := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "volumes")
	volumes = append(volumes, map[string]any{"name": "junkloud-tls", "secret": map[string]any{"secretName": metricsTLSSecret}})
	return unstructured.SetNestedSlice(u.Object, volumes, "spec", "template", "spec", "volumes")
}

// installMetricsServer applies the metrics-server objects (reusing the
// recorded TLS bundle while it is valid) and waits until the metrics API is
// served.
func (m *Manager) installMetricsServer(ctx context.Context, cl *clients, envID string, record func(model.OwnedResource) error) error {
	tlsBundle, err := currentMetricsTLS(ctx, cl, envID, m.now())
	if err != nil {
		return err
	}
	objs, err := platformObjects(envID, tlsBundle)
	if err != nil {
		return err
	}
	for _, u := range objs {
		r, err := applyPlatformObject(ctx, cl, envID, u)
		if err != nil {
			return err
		}
		if err := record(r); err != nil {
			return err
		}
	}
	err = wait.PollUntilContextTimeout(ctx, 2*time.Second, metricsReadyTimout, true, func(ctx context.Context) (bool, error) {
		svc, err := cl.dyn.Resource(apiServiceGVR).Get(ctx, metricsAPIService, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		return apiServiceAvailable(svc), nil
	})
	if err != nil {
		return fmt.Errorf("%w: metrics API %s not available after %s", ErrUnavailable, metricsAPIService, metricsReadyTimout)
	}
	return nil
}

var apiServiceGVR = schema.GroupVersionResource{Group: "apiregistration.k8s.io", Version: "v1", Resource: "apiservices"}

func apiServiceAvailable(u *unstructured.Unstructured) bool {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		cm, _ := c.(map[string]any)
		if cm["type"] == "Available" && cm["status"] == "True" {
			return true
		}
	}
	return false
}

// currentMetricsTLS returns the TLS bundle recorded in the metrics-server
// Secret when it is ours and usable, otherwise a new one.
func currentMetricsTLS(ctx context.Context, cl *clients, envID string, now time.Time) (metricsTLS, error) {
	s, err := cl.kube.CoreV1().Secrets(metricsNamespace).Get(ctx, metricsTLSSecret, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return metricsTLS{}, apiError("read "+metricsTLSSecret, err)
	case platformOwned(s.Labels, envID):
		t := metricsTLS{caPEM: s.Data["ca.crt"], certPEM: s.Data[corev1.TLSCertKey], keyPEM: s.Data[corev1.TLSPrivateKeyKey]}
		if t.usable(now) {
			return t, nil
		}
	}
	return newMetricsTLS(now)
}

// applyPlatformObject creates u, or server-side applies it over an existing
// object carrying this environment's platform labels (UID precondition). A
// foreign object of the same name is left untouched.
func applyPlatformObject(ctx context.Context, cl *clients, envID string, u *unstructured.Unstructured) (model.OwnedResource, error) {
	mapping, namespaced, err := mapKind(cl.mapper, u.GroupVersionKind())
	if err != nil {
		return model.OwnedResource{}, err
	}
	ref := model.ObjectRef{APIVersion: u.GetAPIVersion(), Kind: u.GetKind(), Namespace: u.GetNamespace(), Name: u.GetName()}
	p := plannedObject{ref: ref, gvr: mapping.Resource, namespaced: namespaced}
	ri := resourceFor(cl, p)
	what := fmt.Sprintf("%s %s/%s", ref.Kind, ref.Namespace, ref.Name)
	existing, err := ri.Get(ctx, ref.Name, metav1.GetOptions{})
	var out *unstructured.Unstructured
	switch {
	case apierrors.IsNotFound(err):
		out, err = ri.Create(ctx, u, metav1.CreateOptions{FieldManager: fieldManager})
	case err != nil:
	case !platformOwned(existing.GetLabels(), envID):
		return model.OwnedResource{}, fmt.Errorf("%w: %s exists and is not environment %s's platform", ErrNotOwned, what, envID)
	default:
		obj := u.DeepCopy()
		obj.SetUID(existing.GetUID())
		out, err = ri.Apply(ctx, ref.Name, obj, metav1.ApplyOptions{FieldManager: fieldManager, Force: true})
	}
	if err != nil {
		return model.OwnedResource{}, apiError("apply "+what, err)
	}
	return model.OwnedResource{Ref: ref, UID: string(out.GetUID()), OwnerID: platformProblemID}, nil
}

// ---- cloud-provider-kind ----

// CloudProviderContainerName is the Docker container running
// cloud-provider-kind for an environment.
func CloudProviderContainerName(envID string) string { return "junkloud-cpk-" + envID }

// cloudProviderDockerCLI is the docker CLI inside the pinned
// cloud-provider-kind image (FROM docker:29-cli).
const cloudProviderDockerCLI = "/usr/local/bin/docker"

// Labels cloud-provider-kind lists across all clusters: kind's cluster
// discovery (sigs.k8s.io/kind v0.33.0 docker provider ListClusters: `docker
// ps -a --filter label=io.x-k8s.kind.cluster`) and its final cleanup of
// every load balancer (`docker ps -a --filter
// label=io.x-k8s.cloud-provider-kind.cluster`).
const kindClusterLabel = "io.x-k8s.kind.cluster"

// cloudProviderDockerWrapper is a docker CLI wrapper (POSIX sh) that
// cloud-provider-kind runs instead of the real CLI at realDocker. It scopes
// cloud-provider-kind to one cluster: a bare filter on the kind cluster
// label or on cloud-provider-kind's load-balancer label (any cluster) is
// narrowed to cluster, so cloud-provider-kind never discovers, configures
// or cleans up another kind cluster on the engine. Every other argument
// passes through unchanged. Under HostCompatNoIPv6 it also drops the
// net.ipv6.* sysctl flags cloud-provider-kind passes to its load-balancer
// containers (a kernel without IPv6 rejects them). cluster must be a valid
// cluster name (no quotes).
func cloudProviderDockerWrapper(cluster string, compat HostCompat, realDocker string) string {
	ipv6 := ""
	if compat.Has(HostCompatNoIPv6) {
		ipv6 = "    --sysctl=net.ipv6.*) continue ;;\n"
	}
	return "#!/bin/sh\n" +
		"# JunKloud: cloud-provider-kind may only see kind cluster $cluster.\n" +
		"cluster='" + cluster + "'\n" +
		"for arg do\n" +
		"  shift\n" +
		"  case \"$arg\" in\n" +
		"    label=" + kindClusterLabel + "|label=" + cloudProviderLBLabel + "|--filter=label=" + kindClusterLabel + "|--filter=label=" + cloudProviderLBLabel + ") arg=\"$arg=$cluster\" ;;\n" +
		ipv6 +
		"  esac\n" +
		"  set -- \"$@\" \"$arg\"\n" +
		"done\n" +
		"exec " + realDocker + " \"$@\"\n"
}

// cloudProviderEntrypoint installs the docker CLI wrapper first in PATH
// and then runs cloud-provider-kind with its arguments.
func cloudProviderEntrypoint(cluster string, compat HostCompat) string {
	return "set -eu\nmkdir -p /run/junkloud\ncat > /run/junkloud/docker <<'JUNKLOUD_EOF'\n" +
		cloudProviderDockerWrapper(cluster, compat, cloudProviderDockerCLI) +
		"JUNKLOUD_EOF\nchmod 0755 /run/junkloud/docker\nPATH=/run/junkloud:$PATH\nexport PATH\nexec /bin/cloud-provider-kind \"$@\""
}

// cloudProviderRunArgs is the `docker run` command line of the
// cloud-provider-kind container: the pinned image on the kind network with
// the Docker socket (it starts one proxy container per Ingress gateway),
// restarted by Docker unless JunKloud stops it, labeled as this
// environment's, and always started through cloudProviderEntrypoint so it
// is scoped to this environment's cluster. The config label hashes the run
// configuration.
func cloudProviderRunArgs(envID string, compat HostCompat) []string {
	tail := []string{
		"--entrypoint", "/bin/sh", CloudProviderKindImage.Ref(),
		"-c", cloudProviderEntrypoint(ClusterName(envID), compat), "cloud-provider-kind",
		"--gateway-channel=standard", "--enable-default-ingress=true",
	}
	sum := sha256.Sum256([]byte(strings.Join(tail, "\x00")))
	args := []string{"run", "--detach", "--name", CloudProviderContainerName(envID)}
	labels := platformLabels(envID, ComponentCloudProvider)
	labels[labelConfig] = hex.EncodeToString(sum[:8])
	for _, k := range []string{LabelEnvironmentID, LabelManagedBy, LabelComponent, labelConfig} {
		args = append(args, "--label", k+"="+labels[k])
	}
	args = append(args, "--network", kindNetwork, "--restart", "unless-stopped", "--volume", dockerSocket+":"+dockerSocket)
	return append(args, tail...)
}

// hostContainer is the part of `docker container inspect` the platform uses.
type hostContainer struct {
	ID      string
	Name    string
	Image   string
	Labels  map[string]string
	Running bool
	// Error is Docker's last start error (State.Error), if any.
	Error string
}

// inspectHostContainer inspects one container by name or ID; found is false
// when it does not exist.
func (m *Manager) inspectHostContainer(ctx context.Context, ref string) (hostContainer, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, dockerTimeout)
	defer cancel()
	res, err := m.runner.Run(ctx, "docker", []string{"container", "inspect", "--", ref}, nil)
	if err != nil {
		if strings.Contains(string(res.Stderr), "No such container") || strings.Contains(string(res.Stderr), "No such object") {
			return hostContainer{}, false, nil
		}
		return hostContainer{}, false, fmt.Errorf("%w: inspect %s: %v", ErrUnavailable, ref, err)
	}
	var out []struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		Config struct {
			Image  string            `json:"Image"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		State struct {
			Running bool   `json:"Running"`
			Error   string `json:"Error"`
		} `json:"State"`
	}
	if err := json.Unmarshal(res.Stdout, &out); err != nil || len(out) != 1 {
		return hostContainer{}, false, fmt.Errorf("environment: decode docker inspect %s: %v", ref, err)
	}
	c := out[0]
	return hostContainer{ID: c.ID, Name: strings.TrimPrefix(c.Name, "/"), Image: c.Config.Image, Labels: c.Config.Labels, Running: c.State.Running, Error: c.State.Error}, true, nil
}

// ensureCloudProvider makes this environment's cloud-provider-kind container
// run with the current configuration: it creates it, starts it when stopped,
// or recreates it when its configuration changed. A container of that name
// that is not this environment's is never touched. It waits until
// cloud-provider-kind has registered its IngressClass in the cluster.
func (m *Manager) ensureCloudProvider(ctx context.Context, cl *clients, id model.Identity) (string, error) {
	if !envIDPattern.MatchString(id.ID) {
		return "", fmt.Errorf("%w: environment ID %q", ErrIdentityMismatch, id.ID)
	}
	name := CloudProviderContainerName(id.ID)
	want := cloudProviderRunArgs(id.ID, m.hostCompat)
	c, found, err := m.inspectHostContainer(ctx, name)
	if err != nil {
		return "", err
	}
	if found && !cloudProviderOwned(c, id.ID) {
		return "", fmt.Errorf("%w: container %s is not environment %s's cloud-provider-kind", ErrNotOwned, name, id.ID)
	}
	if found && (c.Labels[labelConfig] != configOf(want) || c.Image != CloudProviderKindImage.Ref()) {
		if err := m.removeHostContainer(ctx, c.ID); err != nil {
			return "", err
		}
		found = false
	}
	switch {
	case !found:
		out, err := m.output(ctx, dockerTimeout, "docker", want...)
		if err != nil {
			return "", fmt.Errorf("environment: start cloud-provider-kind: %w", err)
		}
		c.ID = strings.TrimSpace(string(out))
	case !c.Running:
		if err := m.command(ctx, dockerTimeout, "docker", "start", c.ID); err != nil {
			return "", fmt.Errorf("environment: restart cloud-provider-kind: %w", err)
		}
	}
	if !containerIDPattern.MatchString(c.ID) {
		return "", fmt.Errorf("environment: unexpected cloud-provider-kind container ID %q", c.ID)
	}
	err = wait.PollUntilContextTimeout(ctx, pollInterval, cloudProviderWait, true, func(ctx context.Context) (bool, error) {
		cur, ok, err := m.inspectHostContainer(ctx, c.ID)
		if err != nil || !ok || !cur.Running {
			return false, nil
		}
		_, err = cl.kube.NetworkingV1().IngressClasses().Get(ctx, cloudProviderClass, metav1.GetOptions{})
		return err == nil, nil
	})
	if err != nil {
		return c.ID, fmt.Errorf("%w: cloud-provider-kind (container %s) did not register IngressClass %q within %s", ErrUnavailable, name, cloudProviderClass, cloudProviderWait)
	}
	return c.ID, nil
}

func cloudProviderOwned(c hostContainer, envID string) bool {
	return c.Labels[LabelEnvironmentID] == envID && c.Labels[LabelComponent] == ComponentCloudProvider && c.Labels[LabelManagedBy] == ManagedByValue
}

// configOf extracts the config label value from a run command line.
func configOf(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--label" {
			if v, ok := strings.CutPrefix(args[i+1], labelConfig+"="); ok {
				return v
			}
		}
	}
	return ""
}

// removeHostContainer stops a container gracefully (cloud-provider-kind
// removes its load balancers on SIGTERM) and removes it.
func (m *Manager) removeHostContainer(ctx context.Context, id string) error {
	if !containerIDPattern.MatchString(id) {
		return fmt.Errorf("environment: %q is not a container ID", id)
	}
	if err := m.command(ctx, dockerTimeout, "docker", "stop", "--time", "30", id); err != nil {
		return fmt.Errorf("environment: stop container %s: %w", id, err)
	}
	if err := m.command(ctx, dockerTimeout, "docker", "rm", id); err != nil {
		return fmt.Errorf("environment: remove container %s: %w", id, err)
	}
	return nil
}

// StopPlatform stops and removes the host-side platform processes of the
// environment id names: its cloud-provider-kind container and any load
// balancer container cloud-provider-kind left behind for this environment's
// cluster. Objects inside the cluster stay. It checks id against the
// recorded identity only (the cluster need not be running), never touches a
// container that is not this environment's, and does nothing when nothing
// runs. Callers hold the environment lock.
func (m *Manager) StopPlatform(ctx context.Context, id model.Identity) error {
	recorded, err := m.readIdentity()
	if err != nil {
		return err
	}
	if diff := identityDiff(id, recorded); diff != "" {
		return fmt.Errorf("%w: %s differs from environment %s recorded in %s", ErrIdentityMismatch, diff, recorded.ID, m.dir)
	}
	name := CloudProviderContainerName(id.ID)
	c, found, err := m.inspectHostContainer(ctx, name)
	if err != nil {
		return err
	}
	if found {
		if !cloudProviderOwned(c, id.ID) {
			return fmt.Errorf("%w: container %s is not environment %s's cloud-provider-kind", ErrNotOwned, name, id.ID)
		}
		if err := m.removeHostContainer(ctx, c.ID); err != nil {
			return err
		}
	}
	out, err := m.output(ctx, dockerTimeout, "docker", "ps", "--all", "--quiet", "--no-trunc", "--filter", "label="+cloudProviderLBLabel+"="+id.ClusterName)
	if err != nil {
		return fmt.Errorf("%w: list load balancer containers: %v", ErrUnavailable, err)
	}
	if left := parseIDs(out); len(left) > 0 {
		for _, l := range left {
			if !containerIDPattern.MatchString(l) {
				return fmt.Errorf("environment: %q is not a container ID", l)
			}
		}
		if err := m.command(ctx, dockerTimeout, "docker", append([]string{"rm", "--force"}, left...)...); err != nil {
			return fmt.Errorf("environment: remove load balancer containers %v: %w", left, err)
		}
	}
	inv, err := m.platformInventory(ctx, id.ID)
	if err != nil {
		return err
	}
	if len(inv.DockerIDs) > 0 {
		inv.DockerIDs = nil
		return m.store.PutInventory(ctx, inv)
	}
	return nil
}
