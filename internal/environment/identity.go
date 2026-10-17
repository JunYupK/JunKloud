//go:build linux

// Package environment owns JunKloud's single dedicated kind cluster: it
// creates it (recording the intent first), proves on every use that the
// cluster it is about to touch is the one it created, and applies problem
// setup actions inside the owning problem's declared resource scope.
//
// Ownership is proved by three independent facts recorded at creation and
// compared before any mutation: the cluster CA (hash), the UID of an
// in-cluster marker object, and the Docker container IDs of the nodes. A
// name or the "junkloud-" prefix alone never counts. The host's kubeconfig
// ($KUBECONFIG, ~/.kube/config) and its current-context are never read or
// changed; JunKloud keeps its own kubeconfig under the state directory.
//
// Concurrency: Ensure and Apply change the outside world. Callers must hold
// the environment lock (internal/lock) around them; the Manager does not take
// it itself, so a caller that already holds it does not deadlock. A command
// that needs the identity first (to compose the runtime) gets it through
// author.EnsureEnvironment, which takes the lock around Ensure and
// EnsurePlatform.
package environment

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/JunYupK/JunKloud/internal/executil"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/store"
)

// Sentinel errors. Match them with errors.Is.
var (
	// ErrIdentityMismatch: the target is not the environment this Manager
	// created (different ID, CA, marker UID, containers, or the real cluster
	// no longer matches the record). Nothing was changed.
	ErrIdentityMismatch = errors.New("environment: identity mismatch")
	// ErrNoEnvironment: no environment has been created in this state
	// directory yet.
	ErrNoEnvironment = errors.New("environment: no environment recorded")
	// ErrUnavailable: the recorded environment exists but is not usable
	// right now (a node container is stopped, or the API is unreachable).
	ErrUnavailable = errors.New("environment: environment unavailable")
	// ErrRecoveryNeeded: an interrupted creation left real resources whose
	// state cannot be completed automatically. Nothing is deleted
	// automatically; the error names what was found.
	ErrRecoveryNeeded = errors.New("environment: interrupted operation needs manual recovery")
)

// Operation kinds and phases recorded in the store.
const (
	OperationCreate = "environment-create"
	OperationApply  = "environment-apply"

	PhasePlanned       = "planned"
	PhaseClusterCreate = "cluster-created"
	PhaseDone          = "done"
)

// Object labels and annotations. Every object JunKloud creates carries the
// environment ID; problem objects also carry the session and problem owner.
const (
	LabelEnvironmentID = "junkloud.io/environment-id"
	LabelSessionID     = "junkloud.io/session-id"
	LabelProblemID     = "junkloud.io/problem-id"
	LabelManagedBy     = "app.kubernetes.io/managed-by"
	ManagedByValue     = "junkloud"
	AnnotationOwner    = "junkloud.io/owner"
)

// The in-cluster marker whose UID is part of the identity.
const (
	MarkerNamespace = "junkloud-system"
	MarkerName      = "junkloud-environment"
	markerKeyID     = "environment-id"
	markerKeyName   = "cluster-name"
)

// Runner runs external commands; executil.Runner implements it.
type Runner interface {
	Run(ctx context.Context, exe string, args []string, stdin io.Reader) (executil.Result, error)
}

// Option configures a Manager.
type Option func(*Manager)

// WithRunner replaces the command runner used for kind and docker. The
// default is an executil.Runner whose child environment is the current one
// without KUBECONFIG.
func WithRunner(r Runner) Option { return func(m *Manager) { m.runner = r } }

// WithHostCompat enables host-compatibility patches in the kind cluster
// configuration. They are never applied by default; see HostCompat.
func WithHostCompat(c HostCompat) Option { return func(m *Manager) { m.hostCompat = c } }

// Manager owns one dedicated kind cluster for one state directory.
type Manager struct {
	stateDir   string
	dir        string
	store      *store.Store
	runner     Runner
	hostCompat HostCompat
	nodeImage  string
	now        func() time.Time
}

// New returns a Manager for stateDir. Its files live in
// <stateDir>/environment: identity.json (the recorded Identity),
// kubeconfig (JunKloud's own, 0600) and kind-config.yaml.
func New(stateDir string, s *store.Store, opts ...Option) *Manager {
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		abs = filepath.Clean(stateDir)
	}
	m := &Manager{
		stateDir:  abs,
		dir:       filepath.Join(abs, "environment"),
		store:     s,
		runner:    executil.Runner{Env: executil.EnvWithout(os.Environ(), "KUBECONFIG")},
		nodeImage: DefaultNodeImage,
		now:       func() time.Time { return time.Now().UTC() },
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// KubeconfigPath is JunKloud's own kubeconfig for this environment. Its
// single context is Identity.Context and its server is the node's address
// on the kind Docker network.
func (m *Manager) KubeconfigPath() string { return filepath.Join(m.dir, "kubeconfig") }

// IdentityPath is the file holding the recorded Identity (JSON).
func (m *Manager) IdentityPath() string { return filepath.Join(m.dir, "identity.json") }

// ClusterName is the kind cluster (and kubeconfig context) name for an
// environment ID.
func ClusterName(environmentID string) string { return "junkloud-" + environmentID }

// Ensure returns the identity of this state directory's environment,
// creating the cluster when none exists. With a recorded environment it only
// verifies it (and refreshes the endpoint); it never replaces a mismatching
// or missing cluster. An unfinished creation recorded by an interrupted run
// is resumed from its recorded intent after re-checking the real state.
func (m *Manager) Ensure(ctx context.Context) (model.Identity, error) {
	recorded, err := m.readIdentity()
	switch {
	case err == nil:
		return m.ensureRecorded(ctx, recorded)
	case !errors.Is(err, ErrNoEnvironment):
		return model.Identity{}, err
	}

	pending, err := m.pendingCreates(ctx)
	if err != nil {
		return model.Identity{}, err
	}
	var op model.Operation
	switch len(pending) {
	case 0:
		id, err := newID()
		if err != nil {
			return model.Identity{}, err
		}
		op = m.plannedCreate(id)
		// Intent first: nothing real exists before this is stored.
		if err := m.store.PutOperation(ctx, op); err != nil {
			return model.Identity{}, fmt.Errorf("environment: record create intent: %w", err)
		}
	case 1:
		op = pending[0]
	default:
		ids := make([]string, len(pending))
		for i, p := range pending {
			ids[i] = p.EnvironmentID
		}
		return model.Identity{}, fmt.Errorf("%w: several unfinished environment creations %v", ErrRecoveryNeeded, ids)
	}
	return m.create(ctx, op)
}

// Check verifies that id is this Manager's environment and that the real
// cluster still matches it. The recorded identity is compared first, without
// any external call; then the node containers (Docker IDs and cluster
// label), the cluster CA (TLS verification and kube-root-ca.crt) and the
// marker object UID are checked with read-only calls.
func (m *Manager) Check(ctx context.Context, id model.Identity) error {
	_, _, err := m.verify(ctx, id)
	return err
}

func (m *Manager) ensureRecorded(ctx context.Context, recorded model.Identity) (model.Identity, error) {
	live, _, err := m.verify(ctx, recorded)
	if err != nil {
		return model.Identity{}, err
	}
	if live.Endpoint != recorded.Endpoint {
		access, err := loadAccess(m.KubeconfigPath(), recorded.Context)
		if err != nil {
			return model.Identity{}, err
		}
		if err := writeKubeconfig(m.KubeconfigPath(), recorded.Context, live.Endpoint, access); err != nil {
			return model.Identity{}, err
		}
	}
	if !identityEqual(live, recorded) {
		if err := m.writeIdentity(live); err != nil {
			return model.Identity{}, err
		}
	}
	// A run interrupted after writing the identity leaves its operation open.
	pending, err := m.pendingCreates(ctx)
	if err != nil {
		return model.Identity{}, err
	}
	for _, op := range pending {
		if op.EnvironmentID == live.ID {
			op.Phase = PhaseDone
			if err := m.store.PutOperation(ctx, op); err != nil {
				return model.Identity{}, err
			}
		}
	}
	return live, nil
}

// plannedCreate is the intent recorded before a cluster is created. The
// kubeconfig path ties it to this state directory.
func (m *Manager) plannedCreate(environmentID string) model.Operation {
	return model.Operation{
		ID:            "create-" + environmentID,
		EnvironmentID: environmentID,
		Kind:          OperationCreate,
		Phase:         PhasePlanned,
		Planned: model.Inventory{
			EnvironmentID: environmentID,
			ArtifactPaths: []string{m.KubeconfigPath(), m.IdentityPath()},
		},
		CreatedAt: m.now(),
	}
}

// pendingCreates lists this state directory's unfinished create intents.
func (m *Manager) pendingCreates(ctx context.Context) ([]model.Operation, error) {
	ops, err := m.store.ListOperations(ctx, OperationCreate)
	if err != nil {
		return nil, err
	}
	var out []model.Operation
	for _, op := range ops {
		if op.Phase != PhaseDone && slices.Contains(op.Planned.ArtifactPaths, m.KubeconfigPath()) {
			out = append(out, op)
		}
	}
	return out, nil
}

// create carries a recorded create intent to completion, adopting nodes that
// an interrupted run already created once they are proved to be ours.
func (m *Manager) create(ctx context.Context, op model.Operation) (model.Identity, error) {
	envID := op.EnvironmentID
	name := ClusterName(envID)
	ids, err := m.clusterContainerIDs(ctx, name)
	if err != nil {
		return model.Identity{}, err
	}
	if len(ids) == 0 {
		if err := m.kindCreate(ctx, envID); err != nil {
			return model.Identity{}, err
		}
		if ids, err = m.clusterContainerIDs(ctx, name); err != nil {
			return model.Identity{}, err
		}
		if len(ids) == 0 {
			return model.Identity{}, fmt.Errorf("environment: kind reported success but cluster %s has no node containers", name)
		}
	}
	op.Phase = PhaseClusterCreate
	op.Planned.DockerIDs = ids
	if err := m.store.PutOperation(ctx, op); err != nil {
		return model.Identity{}, err
	}

	internal, err := m.kindInternalKubeconfig(ctx, name)
	if err != nil {
		return model.Identity{}, fmt.Errorf("%w: cluster %s has node containers %v but its kubeconfig cannot be read (%v); inspect them and delete them yourself if they are leftovers",
			ErrRecoveryNeeded, name, ids, err)
	}
	access, err := buildAccess(internal)
	if err != nil {
		return model.Identity{}, err
	}
	cp, err := m.controlPlane(ctx, name, ids)
	if err != nil {
		return model.Identity{}, err
	}
	endpoint, err := endpointFor(access.Server, cp.IP)
	if err != nil {
		return model.Identity{}, err
	}
	cl, err := newClients(restConfig(access, endpoint))
	if err != nil {
		return model.Identity{}, err
	}
	// Nodes carry the environment ID from the kind config: this proves the
	// cluster was created for this intent and not by someone else under the
	// same name.
	if err := checkNodeLabels(ctx, cl, envID); err != nil {
		return model.Identity{}, err
	}
	if err := writeKubeconfig(m.KubeconfigPath(), name, endpoint, access); err != nil {
		return model.Identity{}, err
	}
	marker, ns, err := ensureMarker(ctx, cl, envID, name)
	if err != nil {
		return model.Identity{}, err
	}
	caFromAccess, err := caHash(access.CAData)
	if err != nil {
		return model.Identity{}, err
	}
	id := model.Identity{
		ID:           envID,
		ClusterName:  name,
		Context:      name,
		Endpoint:     endpoint,
		CAHash:       caFromAccess,
		MarkerUID:    string(marker.UID),
		ContainerIDs: ids,
		Versions:     m.versions(cl),
	}
	op.Planned.Resources = []model.OwnedResource{
		{Ref: model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: MarkerNamespace}, UID: string(ns.UID), OwnerID: envID},
		{Ref: model.ObjectRef{APIVersion: "v1", Kind: "ConfigMap", Namespace: MarkerNamespace, Name: MarkerName}, UID: string(marker.UID), OwnerID: envID},
	}
	if err := m.writeIdentity(id); err != nil {
		return model.Identity{}, err
	}
	if err := m.store.PutInventory(ctx, op.Planned); err != nil {
		return model.Identity{}, err
	}
	op.Phase = PhaseDone
	if err := m.store.PutOperation(ctx, op); err != nil {
		return model.Identity{}, err
	}
	return id, nil
}

// verify compares want with the recorded identity and then with the real
// cluster, using only reads. It returns the live identity (endpoint
// re-derived from Docker, versions refreshed) and API clients bound to it.
func (m *Manager) verify(ctx context.Context, want model.Identity) (model.Identity, *clients, error) {
	recorded, err := m.readIdentity()
	if err != nil {
		return model.Identity{}, nil, err
	}
	if diff := identityDiff(want, recorded); diff != "" {
		return model.Identity{}, nil, fmt.Errorf("%w: %s differs from environment %s recorded in %s",
			ErrIdentityMismatch, diff, recorded.ID, m.dir)
	}

	// Docker: the recorded containers exist, are exactly the cluster's
	// nodes, and run.
	nodes, err := m.inspect(ctx, recorded.ContainerIDs)
	if err != nil {
		return model.Identity{}, nil, err
	}
	current, err := m.clusterContainerIDs(ctx, recorded.ClusterName)
	if err != nil {
		return model.Identity{}, nil, err
	}
	if !slices.Equal(current, sortedCopy(recorded.ContainerIDs)) {
		return model.Identity{}, nil, fmt.Errorf("%w: cluster %s now has node containers %v, recorded %v",
			ErrIdentityMismatch, recorded.ClusterName, current, recorded.ContainerIDs)
	}
	var cp *node
	for i, n := range nodes {
		if n.Cluster != recorded.ClusterName {
			return model.Identity{}, nil, fmt.Errorf("%w: container %s belongs to cluster %q", ErrIdentityMismatch, n.ID, n.Cluster)
		}
		if !n.Running {
			return model.Identity{}, nil, fmt.Errorf("%w: node container %s is not running", ErrUnavailable, n.Name)
		}
		if n.Role == "control-plane" {
			cp = &nodes[i]
		}
	}
	if cp == nil || cp.IP == "" {
		return model.Identity{}, nil, fmt.Errorf("%w: no control-plane address on the %q network", ErrUnavailable, kindNetwork)
	}

	// CA: our kubeconfig still holds the recorded CA, and TLS to the live
	// endpoint is verified against it.
	access, err := loadAccess(m.KubeconfigPath(), recorded.Context)
	if err != nil {
		return model.Identity{}, nil, err
	}
	fileCA, err := caHash(access.CAData)
	if err != nil {
		return model.Identity{}, nil, err
	}
	if fileCA != recorded.CAHash {
		return model.Identity{}, nil, fmt.Errorf("%w: kubeconfig CA %s, recorded %s", ErrIdentityMismatch, fileCA, recorded.CAHash)
	}
	endpoint, err := endpointFor(access.Server, cp.IP)
	if err != nil {
		return model.Identity{}, nil, err
	}
	cl, err := newClients(restConfig(access, endpoint))
	if err != nil {
		return model.Identity{}, nil, err
	}
	rootCA, err := cl.kube.CoreV1().ConfigMaps("kube-system").Get(ctx, "kube-root-ca.crt", metav1.GetOptions{})
	if err != nil {
		return model.Identity{}, nil, apiError("read cluster CA", err)
	}
	liveCA, err := caHash([]byte(rootCA.Data["ca.crt"]))
	if err != nil {
		return model.Identity{}, nil, fmt.Errorf("%w: cluster CA unreadable: %v", ErrIdentityMismatch, err)
	}
	if liveCA != recorded.CAHash {
		return model.Identity{}, nil, fmt.Errorf("%w: cluster CA %s, recorded %s", ErrIdentityMismatch, liveCA, recorded.CAHash)
	}

	// Marker: the object created with the environment, by UID and content.
	marker, err := cl.kube.CoreV1().ConfigMaps(MarkerNamespace).Get(ctx, MarkerName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return model.Identity{}, nil, fmt.Errorf("%w: marker %s/%s not found", ErrIdentityMismatch, MarkerNamespace, MarkerName)
	}
	if err != nil {
		return model.Identity{}, nil, apiError("read marker", err)
	}
	if string(marker.UID) != recorded.MarkerUID || marker.Data[markerKeyID] != recorded.ID {
		return model.Identity{}, nil, fmt.Errorf("%w: marker uid %s environment %q, recorded uid %s environment %q",
			ErrIdentityMismatch, marker.UID, marker.Data[markerKeyID], recorded.MarkerUID, recorded.ID)
	}

	live := recorded
	live.Endpoint = endpoint
	live.ContainerIDs = sortedCopy(recorded.ContainerIDs)
	live.Versions = m.versions(cl)
	return live, cl, nil
}

// identityDiff names the first identity field in which a and b differ.
// Endpoint (re-derived on every check) and Versions are not identity.
func identityDiff(a, b model.Identity) string {
	switch {
	case a.ID == "" || a.ID != b.ID:
		return "id"
	case a.ClusterName != b.ClusterName:
		return "cluster_name"
	case a.Context != b.Context:
		return "context"
	case a.CAHash == "" || a.CAHash != b.CAHash:
		return "ca_hash"
	case a.MarkerUID == "" || a.MarkerUID != b.MarkerUID:
		return "marker_uid"
	case len(a.ContainerIDs) == 0 || !slices.Equal(sortedCopy(a.ContainerIDs), sortedCopy(b.ContainerIDs)):
		return "container_ids"
	}
	return ""
}

func identityEqual(a, b model.Identity) bool {
	if identityDiff(a, b) != "" || a.Endpoint != b.Endpoint || len(a.Versions) != len(b.Versions) {
		return false
	}
	for k, v := range a.Versions {
		if b.Versions[k] != v {
			return false
		}
	}
	return true
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// readIdentity loads the recorded identity, or ErrNoEnvironment.
func (m *Manager) readIdentity() (model.Identity, error) {
	raw, err := os.ReadFile(m.IdentityPath())
	if errors.Is(err, os.ErrNotExist) {
		return model.Identity{}, fmt.Errorf("%w in %s", ErrNoEnvironment, m.dir)
	}
	if err != nil {
		return model.Identity{}, fmt.Errorf("environment: read identity: %w", err)
	}
	var id model.Identity
	if err := json.Unmarshal(raw, &id); err != nil {
		return model.Identity{}, fmt.Errorf("environment: decode identity %s: %w", m.IdentityPath(), err)
	}
	return id, nil
}

// writeIdentity records id atomically (0600).
func (m *Manager) writeIdentity(id model.Identity) error {
	raw, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return fmt.Errorf("environment: encode identity: %w", err)
	}
	return writeFileAtomic(m.IdentityPath(), append(raw, '\n'))
}

// writeFileAtomic writes data to path through a temporary file and rename,
// creating the parent directory (0700) and leaving the file mode 0600.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("environment: create %s: %w", filepath.Dir(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("environment: write %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("environment: write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("environment: write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("environment: write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("environment: write %s: %w", path, err)
	}
	return nil
}

// newID returns a random 12-hex-digit environment ID.
func newID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("environment: generate id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// checkNodeLabels proves every node was created for envID.
func checkNodeLabels(ctx context.Context, cl *clients, envID string) error {
	nodes, err := cl.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return apiError("list nodes", err)
	}
	if len(nodes.Items) == 0 {
		return fmt.Errorf("%w: cluster has no nodes", ErrRecoveryNeeded)
	}
	for _, n := range nodes.Items {
		if n.Labels[LabelEnvironmentID] != envID {
			return fmt.Errorf("%w: node %s is labeled for environment %q, not %q",
				ErrIdentityMismatch, n.Name, n.Labels[LabelEnvironmentID], envID)
		}
	}
	return nil
}

// ensureMarker creates (or, on recovery, adopts) the marker namespace and
// object. An existing marker for another environment is a mismatch.
func ensureMarker(ctx context.Context, cl *clients, envID, name string) (*corev1.ConfigMap, *corev1.Namespace, error) {
	labels := map[string]string{LabelEnvironmentID: envID, LabelManagedBy: ManagedByValue}
	ns, err := cl.kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: MarkerNamespace, Labels: labels},
	}, metav1.CreateOptions{FieldManager: fieldManager})
	if apierrors.IsAlreadyExists(err) {
		ns, err = cl.kube.CoreV1().Namespaces().Get(ctx, MarkerNamespace, metav1.GetOptions{})
		if err == nil && ns.Labels[LabelEnvironmentID] != envID {
			return nil, nil, fmt.Errorf("%w: namespace %s belongs to environment %q", ErrIdentityMismatch, MarkerNamespace, ns.Labels[LabelEnvironmentID])
		}
	}
	if err != nil {
		return nil, nil, apiError("create marker namespace", err)
	}
	cm, err := cl.kube.CoreV1().ConfigMaps(MarkerNamespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: MarkerName, Namespace: MarkerNamespace, Labels: labels},
		Data:       map[string]string{markerKeyID: envID, markerKeyName: name},
	}, metav1.CreateOptions{FieldManager: fieldManager})
	if apierrors.IsAlreadyExists(err) {
		cm, err = cl.kube.CoreV1().ConfigMaps(MarkerNamespace).Get(ctx, MarkerName, metav1.GetOptions{})
		if err == nil && cm.Data[markerKeyID] != envID {
			return nil, nil, fmt.Errorf("%w: marker belongs to environment %q", ErrIdentityMismatch, cm.Data[markerKeyID])
		}
	}
	if err != nil {
		return nil, nil, apiError("create marker", err)
	}
	return cm, ns, nil
}

// versions reports the pinned tool versions and the live server version.
func (m *Manager) versions(cl *clients) map[string]string {
	v := map[string]string{"kind": KindVersion, "node_image": m.nodeImage}
	if info, err := cl.kube.Discovery().ServerVersion(); err == nil {
		v["kubernetes"] = info.GitVersion
	}
	return v
}

// apiError classifies a Kubernetes API failure. A TLS verification failure
// means the endpoint is not served by the recorded CA: a different cluster.
func apiError(what string, err error) error {
	var verr *tls.CertificateVerificationError
	if errors.As(err, &verr) || strings.Contains(err.Error(), "x509:") {
		return fmt.Errorf("%w: %s: %w", ErrIdentityMismatch, what, err)
	}
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) || apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) || apierrors.IsInvalid(err) {
		return fmt.Errorf("environment: %s: %w", what, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, what, err)
}
