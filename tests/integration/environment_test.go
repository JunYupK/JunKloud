//go:build integration

// Package integration holds tests that drive real kind clusters. Each test
// creates uniquely named clusters and deletes them (and everything else it
// created) in t.Cleanup, also when it fails.
//
// On a cgroup v1 host that forbids lowering oom_score_adj (the CI sandbox),
// set JUNKLOUD_TEST_HOST_COMPAT=cgroupv1 so clusters get the explicit
// environment.WithHostCompat option; on an engine whose kernel has IPv6
// disabled add no-ipv6 (JUNKLOUD_TEST_HOST_COMPAT=cgroupv1,no-ipv6).
package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/executil"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// hostCompatOptions returns the environment options selected by
// JUNKLOUD_TEST_HOST_COMPAT (a comma-separated list of host-compat modes,
// see environment.ParseHostCompat).
func hostCompatOptions(t *testing.T) []environment.Option {
	t.Helper()
	compat, err := environment.ParseHostCompat(os.Getenv("JUNKLOUD_TEST_HOST_COMPAT"))
	testutil.NoError(t, err)
	if compat == environment.HostCompatNone {
		return nil
	}
	return []environment.Option{environment.WithHostCompat(compat)}
}

// requireTools fails (not skips) when a tool the integration suite needs is
// missing: these tests were asked for explicitly.
func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("integration tests need %s in PATH: %v", tool, err)
		}
	}
	if _, err := executil.Run(context.Background(), "docker", []string{"info", "--format", "{{.ID}}"}, nil); err != nil {
		t.Fatalf("integration tests need a running Docker daemon: %v", err)
	}
}

// toolEnv is the child environment for test-issued kind/docker commands:
// never the ambient KUBECONFIG.
var toolEnv = executil.Runner{Env: executil.EnvWithout(os.Environ(), "KUBECONFIG"), Timeout: 10 * time.Minute}

// newEnvironmentDir returns a fresh state directory and its store. Cleanup
// deletes every kind cluster any create intent in that store planned (so a
// failed or interrupted Ensure leaves nothing behind), then closes the store.
func newEnvironmentDir(t *testing.T) (string, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "junkloud.db"))
	testutil.NoError(t, err)
	t.Cleanup(func() {
		defer st.Close()
		ops, err := st.ListOperations(context.Background(), environment.OperationCreate)
		if err != nil {
			t.Errorf("cleanup: list create intents: %v", err)
			return
		}
		for _, op := range ops {
			kindDelete(t, environment.ClusterName(op.EnvironmentID), filepath.Join(dir, "cleanup.kubeconfig"))
		}
	})
	return dir, st
}

// kindDelete deletes one kind cluster by name. kind edits the given
// kubeconfig (a throwaway path), never $KUBECONFIG or ~/.kube/config.
func kindDelete(t *testing.T, name, kubeconfig string) {
	t.Helper()
	if _, err := toolEnv.Run(context.Background(), "kind", []string{"delete", "cluster", "--name", name, "--kubeconfig", kubeconfig}, nil); err != nil {
		t.Errorf("cleanup: delete kind cluster %s: %v", name, err)
	}
	ids := nodeContainers(t, name)
	if len(ids) != 0 {
		t.Errorf("cleanup: cluster %s still has containers %v", name, ids)
	}
}

// nodeContainers lists the containers kind labeled as nodes of a cluster.
func nodeContainers(t *testing.T, name string) []string {
	t.Helper()
	res, err := toolEnv.Run(context.Background(), "docker", []string{"ps", "--all", "--quiet", "--no-trunc", "--filter", "label=io.x-k8s.kind.cluster=" + name}, nil)
	testutil.NoError(t, err)
	ids := strings.Fields(string(res.Stdout))
	slices.Sort(ids)
	return ids
}

// kubeClient returns a client for the environment built only from
// JunKloud's own kubeconfig and the identity's explicit context.
func kubeClient(t *testing.T, m *environment.Manager, id model.Identity) kubernetes.Interface {
	t.Helper()
	cfg, err := clientcmd.LoadFromFile(m.KubeconfigPath())
	testutil.NoError(t, err)
	rc, err := clientcmd.NewNonInteractiveClientConfig(*cfg, id.Context, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	testutil.NoError(t, err)
	cs, err := kubernetes.NewForConfig(rc)
	testutil.NoError(t, err)
	return cs
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	testutil.NoError(t, err)
	return hex.EncodeToString(b)
}

// interruptAfterCreate passes commands through but reports an interruption
// right after `kind create cluster` succeeds, as if the process died there.
type interruptAfterCreate struct {
	inner   environment.Runner
	mu      sync.Mutex
	creates int
}

var errInterrupted = errors.New("injected interruption after kind create")

func (r *interruptAfterCreate) Run(ctx context.Context, exe string, args []string, stdin io.Reader) (executil.Result, error) {
	res, err := r.inner.Run(ctx, exe, args, stdin)
	if exe == "kind" && len(args) > 1 && args[0] == "create" {
		r.mu.Lock()
		r.creates++
		r.mu.Unlock()
		if err == nil {
			return res, errInterrupted
		}
	}
	return res, err
}

// countingRunner counts kind create calls.
type countingRunner struct {
	inner   environment.Runner
	mu      sync.Mutex
	creates int
}

func (r *countingRunner) Run(ctx context.Context, exe string, args []string, stdin io.Reader) (executil.Result, error) {
	if exe == "kind" && len(args) > 1 && args[0] == "create" {
		r.mu.Lock()
		r.creates++
		r.mu.Unlock()
	}
	return r.inner.Run(ctx, exe, args, stdin)
}

// foreignCluster is a kind cluster JunKloud did not create, holding a
// sentinel object and a squatter marker at JunKloud's marker location.
type foreignCluster struct {
	name       string
	client     kubernetes.Interface
	internal   []byte // `kind get kubeconfig --internal`
	containers []string
	markerUID  string
}

// startForeignCluster creates a non-JunKloud kind cluster in the background;
// the returned function waits for it. Cleanup waits, then deletes it.
func startForeignCluster(t *testing.T) func() foreignCluster {
	t.Helper()
	dir := t.TempDir()
	name := "jk-foreign-" + randomHex(t, 4)
	kubeconfig := filepath.Join(dir, "kubeconfig")
	cfg := "kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nnodes:\n- role: control-plane\n"
	compat, err := environment.ParseHostCompat(os.Getenv("JUNKLOUD_TEST_HOST_COMPAT"))
	testutil.NoError(t, err)
	if compat.Has(environment.HostCompatCgroupV1) {
		cfg = "kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\ncontainerdConfigPatches:\n- |-\n  [plugins.\"io.containerd.grpc.v1.cri\"]\n    restrict_oom_score_adj = true\nnodes:\n- role: control-plane\n  kubeadmConfigPatches:\n  - |\n    kind: KubeletConfiguration\n    failCgroupV1: false\n"
	}
	cfgPath := filepath.Join(dir, "kind.yaml")
	testutil.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o600))

	done := make(chan error, 1)
	go func() {
		_, err := toolEnv.Run(context.Background(), "kind", []string{"create", "cluster", "--name", name,
			"--image", environment.DefaultNodeImage, "--config", cfgPath, "--kubeconfig", kubeconfig, "--wait", "5m"}, nil)
		done <- err
	}()
	var once sync.Once
	var createErr error
	wait := func() error {
		once.Do(func() { createErr = <-done })
		return createErr
	}
	t.Cleanup(func() {
		_ = wait()
		kindDelete(t, name, kubeconfig)
	})

	return func() foreignCluster {
		t.Helper()
		testutil.NoError(t, wait())
		raw, err := os.ReadFile(kubeconfig)
		testutil.NoError(t, err)
		rc, err := clientcmd.RESTConfigFromKubeConfig(raw)
		testutil.NoError(t, err)
		cs, err := kubernetes.NewForConfig(rc)
		testutil.NoError(t, err)
		ctx := context.Background()
		for _, ns := range []string{environment.MarkerNamespace, "foreign-app"} {
			_, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
			testutil.NoError(t, err)
		}
		marker, err := cs.CoreV1().ConfigMaps(environment.MarkerNamespace).Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: environment.MarkerName},
			Data:       map[string]string{"environment-id": "someone-else"},
		}, metav1.CreateOptions{})
		testutil.NoError(t, err)
		_, err = cs.CoreV1().ConfigMaps("foreign-app").Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "sentinel"}, Data: map[string]string{"owner": "not-junkloud"},
		}, metav1.CreateOptions{})
		testutil.NoError(t, err)
		res, err := toolEnv.Run(ctx, "kind", []string{"get", "kubeconfig", "--internal", "--name", name}, nil)
		testutil.NoError(t, err)
		return foreignCluster{name: name, client: cs, internal: res.Stdout, containers: nodeContainers(t, name), markerUID: string(marker.UID)}
	}
}

// snapshot records every namespace and ConfigMap with its resourceVersion,
// and every object carrying a JunKloud label, so any mutation shows up.
func snapshot(t *testing.T, cs kubernetes.Interface) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	nss, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	testutil.NoError(t, err)
	for _, n := range nss.Items {
		out["ns/"+n.Name] = n.ResourceVersion
	}
	cms, err := cs.CoreV1().ConfigMaps("").List(ctx, metav1.ListOptions{})
	testutil.NoError(t, err)
	for _, c := range cms.Items {
		out["cm/"+c.Namespace+"/"+c.Name] = c.ResourceVersion
	}
	labeled, err := cs.CoreV1().ConfigMaps("").List(ctx, metav1.ListOptions{LabelSelector: environment.LabelEnvironmentID})
	testutil.NoError(t, err)
	out["junkloud-labeled-configmaps"] = strconv.Itoa(len(labeled.Items))
	return out
}

func mutationCount(before, after map[string]string) int {
	n := 0
	for k, v := range before {
		if after[k] != v {
			n++
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			n++
		}
	}
	return n
}

func caHashOf(t *testing.T, internal []byte) string {
	t.Helper()
	cfg, err := clientcmd.Load(internal)
	testutil.NoError(t, err)
	cluster := cfg.Clusters[cfg.Contexts[cfg.CurrentContext].Cluster]
	h := sha256.New()
	for rest := cluster.CertificateAuthorityData; ; {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		h.Write(b.Bytes)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func fileState(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "absent: " + err.Error()
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestEnvironment(t *testing.T) {
	requireTools(t, "docker", "kind")
	ctx := context.Background()
	opts := hostCompatOptions(t)

	// Ambient kubeconfigs that must be neither used nor changed.
	decoy := filepath.Join(t.TempDir(), "decoy-kubeconfig")
	decoyBytes := []byte("apiVersion: v1\nkind: Config\ncurrent-context: decoy\nclusters:\n- name: decoy\n  cluster: {server: 'https://127.0.0.1:1'}\ncontexts:\n- name: decoy\n  context: {cluster: decoy, user: decoy}\nusers:\n- name: decoy\n  user: {token: decoy}\n")
	testutil.NoError(t, os.WriteFile(decoy, decoyBytes, 0o600))
	t.Setenv("KUBECONFIG", decoy)
	home, err := os.UserHomeDir()
	testutil.NoError(t, err)
	homeKubeconfig := filepath.Join(home, ".kube", "config")
	homeBefore := fileState(homeKubeconfig)

	waitForeign := startForeignCluster(t)
	stateDir, st := newEnvironmentDir(t)
	real := executil.Runner{Env: executil.EnvWithout(os.Environ(), "KUBECONFIG")}

	var id model.Identity
	var m *environment.Manager
	t.Run("RecoverCreateIntent", func(t *testing.T) {
		crash := &interruptAfterCreate{inner: real}
		_, err := environment.New(stateDir, st, append(slices.Clone(opts), environment.WithRunner(crash))...).Ensure(ctx)
		testutil.ErrorIs(t, err, errInterrupted)
		ops, err := st.ListOperations(ctx, environment.OperationCreate)
		testutil.NoError(t, err)
		testutil.Equal(t, len(ops), 1)
		planned := ops[0]
		testutil.Equal(t, planned.Phase, environment.PhasePlanned)
		name := environment.ClusterName(planned.EnvironmentID)
		before := nodeContainers(t, name)
		if len(before) == 0 {
			t.Fatalf("interrupted create left no nodes for %s", name)
		}

		counter := &countingRunner{inner: real}
		m = environment.New(stateDir, st, append(slices.Clone(opts), environment.WithRunner(counter))...)
		id, err = m.Ensure(ctx)
		testutil.NoError(t, err)
		testutil.Equal(t, id.ID, planned.EnvironmentID)
		testutil.Equal(t, id.ClusterName, name)
		testutil.Equal(t, id.ContainerIDs, before)
		testutil.Equal(t, nodeContainers(t, name), before) // no second cluster
		testutil.Equal(t, crash.creates+counter.creates, 1)
		op, err := st.GetOperation(ctx, planned.ID)
		testutil.NoError(t, err)
		testutil.Equal(t, op.Phase, environment.PhaseDone)
		if id.CAHash == "" || id.MarkerUID == "" || !strings.HasPrefix(id.Endpoint, "https://") {
			t.Fatalf("incomplete identity %+v", id)
		}
		envInv, err := st.GetInventory(ctx, id.ID, "", "")
		testutil.NoError(t, err)
		testutil.Equal(t, envInv.DockerIDs, id.ContainerIDs)
	})
	if m == nil || id.ID == "" {
		t.Fatal("environment was not created; later subtests need it")
	}

	t.Run("EnsureIsIdempotent", func(t *testing.T) {
		testutil.NoError(t, m.Check(ctx, id))
		again, err := environment.New(stateDir, st, opts...).Ensure(ctx)
		testutil.NoError(t, err)
		testutil.Equal(t, again.ID, id.ID)
		testutil.Equal(t, again.MarkerUID, id.MarkerUID)
		testutil.Equal(t, again.CAHash, id.CAHash)
		testutil.Equal(t, again.ContainerIDs, id.ContainerIDs)
		testutil.Equal(t, again.Endpoint, id.Endpoint)
		testutil.Equal(t, nodeContainers(t, id.ClusterName), id.ContainerIDs)
	})

	t.Run("IgnoreAmbientKubeconfig", func(t *testing.T) {
		after, err := os.ReadFile(decoy)
		testutil.NoError(t, err)
		testutil.Equal(t, bytes.Equal(after, decoyBytes), true)
		testutil.Equal(t, fileState(homeKubeconfig), homeBefore)

		cfg, err := clientcmd.LoadFromFile(m.KubeconfigPath())
		testutil.NoError(t, err)
		testutil.Equal(t, len(cfg.Contexts), 1)
		testutil.Equal(t, cfg.CurrentContext, id.Context)
		cluster := cfg.Clusters[cfg.Contexts[id.Context].Cluster]
		testutil.Equal(t, cluster.Server, id.Endpoint)
		testutil.Equal(t, cluster.InsecureSkipTLSVerify, false)
		testutil.Equal(t, caHashOf(t, mustKubeconfigBytes(t, m.KubeconfigPath())), id.CAHash)
		info, err := os.Stat(m.KubeconfigPath())
		testutil.NoError(t, err)
		testutil.Equal(t, info.Mode().Perm(), os.FileMode(0o600))
		// The endpoint is the node's address on the kind network, reachable
		// with TLS verified against the cluster CA.
		_, err = kubeClient(t, m, id).Discovery().ServerVersion()
		testutil.NoError(t, err)
	})

	t.Run("Apply", func(t *testing.T) { testApply(t, m, st, id) })

	t.Run("RejectForeignIdentity", func(t *testing.T) {
		foreign := waitForeign()
		before := snapshot(t, foreign.client)

		// Same manager, identity pointing at the foreign nodes.
		forged := id
		forged.ContainerIDs = foreign.containers
		expectMismatch(t, m.Check(ctx, forged))
		_, err := m.Apply(ctx, forged, environment.Owner{
			Inventory:        model.Inventory{EnvironmentID: forged.ID, SessionID: "s1", ProblemID: "p1"},
			Namespace:        "jk-new", // absent in both clusters: a broken gate would create it
			CreateNamespaces: true,
		}, []environment.Action{{Kind: model.ActionApply, Objects: []map[string]any{cm("", "sentinel")}}})
		expectMismatch(t, err)

		// A state directory whose record claims the foreign cluster, with
		// its real CA, nodes, kubeconfig and the UID of the squatter marker:
		// the live marker check still refuses it.
		otherDir, otherStore := newEnvironmentDir(t)
		other := environment.New(otherDir, otherStore, opts...)
		testutil.NoError(t, os.MkdirAll(filepath.Dir(other.KubeconfigPath()), 0o700))
		testutil.NoError(t, os.WriteFile(other.KubeconfigPath(), foreign.internal, 0o600))
		claim := model.Identity{
			ID: "f0f0f0f0f0f0", ClusterName: foreign.name, Context: "kind-" + foreign.name,
			CAHash: caHashOf(t, foreign.internal), MarkerUID: foreign.markerUID, ContainerIDs: foreign.containers,
		}
		raw, err := json.Marshal(claim)
		testutil.NoError(t, err)
		testutil.NoError(t, os.WriteFile(other.IdentityPath(), raw, 0o600))

		err = other.Check(ctx, claim)
		t.Logf("foreign claim rejected: %v", err)
		expectMismatch(t, err)
		_, err = other.Ensure(ctx)
		expectMismatch(t, err)
		_, err = other.Apply(ctx, claim, environment.Owner{
			Inventory:        model.Inventory{EnvironmentID: claim.ID, SessionID: "s1", ProblemID: "p1"},
			Namespace:        "jk-new",
			CreateNamespaces: true,
		}, []environment.Action{{Kind: model.ActionApply, Objects: []map[string]any{cm("", "sentinel")}}})
		expectMismatch(t, err)
		ops, err := otherStore.ListOperations(ctx, environment.OperationCreate)
		testutil.NoError(t, err)
		testutil.Equal(t, len(ops), 0)

		after := snapshot(t, foreign.client)
		foreignClusterMutationCount := mutationCount(before, after)
		testutil.Equal(t, foreignClusterMutationCount, 0)
		sentinel, err := foreign.client.CoreV1().ConfigMaps("foreign-app").Get(ctx, "sentinel", metav1.GetOptions{})
		testutil.NoError(t, err)
		testutil.Equal(t, sentinel.Data, map[string]string{"owner": "not-junkloud"})
		testutil.Equal(t, len(sentinel.Labels), 0)
		for _, cs := range []kubernetes.Interface{foreign.client, kubeClient(t, m, id)} {
			_, err := cs.CoreV1().Namespaces().Get(ctx, "jk-new", metav1.GetOptions{})
			if !apierrors.IsNotFound(err) {
				t.Fatalf("namespace jk-new must not exist after rejected Apply calls: %v", err)
			}
		}
	})
}

// expectMismatch records (without stopping the test) a missing
// ErrIdentityMismatch, so the foreign-cluster mutation check below still runs
// and shows whether a weakened gate let a write through.
func expectMismatch(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, environment.ErrIdentityMismatch) {
		t.Errorf("got error %v, want error matching %v", err, environment.ErrIdentityMismatch)
	}
}

func mustKubeconfigBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	testutil.NoError(t, err)
	return b
}

func cm(namespace, name string) map[string]any {
	meta := map[string]any{"name": name}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	return map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta, "data": map[string]any{"k": "v"}}
}

func testApply(t *testing.T, m *environment.Manager, st *store.Store, id model.Identity) {
	ctx := context.Background()
	cs := kubeClient(t, m, id)
	owner := environment.Owner{
		Inventory:        model.Inventory{EnvironmentID: id.ID, SessionID: "s1", ProblemID: "service-test"},
		Namespace:        "jk-p1",
		CreateNamespaces: true, // setup
		Scope: model.ResourceScope{
			Namespaces: []string{"jk-p1-client"},
			ClusterResources: []model.ObjectRef{
				{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: "jk-p1-reader"}},
		},
	}
	service := map[string]any{"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": "web", "namespace": "jk-p1"},
		// A rendered parameter arrives as a Go int.
		"spec": map[string]any{"ports": []any{map[string]any{"port": 8080}}}}
	clusterRole := map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]any{"name": "jk-p1-reader"},
		"rules":    []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"pods"}, "verbs": []any{"get"}}}}
	setup := []environment.Action{{Kind: model.ActionApply, Objects: []map[string]any{
		cm("", "web-config"), service, cm("jk-p1-client", "client-config"), clusterRole,
	}}}

	inv, err := m.Apply(ctx, id, owner, setup)
	testutil.NoError(t, err)
	want := []model.ObjectRef{
		{APIVersion: "v1", Kind: "Namespace", Name: "jk-p1"},
		{APIVersion: "v1", Kind: "Namespace", Name: "jk-p1-client"},
		{APIVersion: "v1", Kind: "ConfigMap", Namespace: "jk-p1", Name: "web-config"},
		{APIVersion: "v1", Kind: "Service", Namespace: "jk-p1", Name: "web"},
		{APIVersion: "v1", Kind: "ConfigMap", Namespace: "jk-p1-client", Name: "client-config"},
		{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: "jk-p1-reader"},
	}
	var got []model.ObjectRef
	for _, r := range inv.Resources {
		got = append(got, r.Ref)
		testutil.Equal(t, r.OwnerID, "s1/service-test")
		meta := liveMeta(t, cs, r.Ref)
		testutil.Equal(t, string(meta.UID), r.UID)
		testutil.Equal(t, meta.Labels[environment.LabelEnvironmentID], id.ID)
		testutil.Equal(t, meta.Labels[environment.LabelSessionID], "s1")
		testutil.Equal(t, meta.Labels[environment.LabelProblemID], "service-test")
		testutil.Equal(t, meta.Annotations[environment.AnnotationOwner], "s1/service-test")
	}
	testutil.Equal(t, got, want)
	svc, err := cs.CoreV1().Services("jk-p1").Get(ctx, "web", metav1.GetOptions{})
	testutil.NoError(t, err)
	testutil.Equal(t, svc.Spec.Ports[0].Port, int32(8080))
	stored, err := st.GetInventory(ctx, id.ID, "s1", "service-test")
	testutil.NoError(t, err)
	testutil.Equal(t, stored, inv)

	// Re-applying owned objects updates them in place (same UIDs).
	owner.Inventory = inv
	again, err := m.Apply(ctx, id, owner, setup)
	testutil.NoError(t, err)
	testutil.Equal(t, again, inv)

	// Solution, case and later steps never create namespaces.
	later := owner
	later.CreateNamespaces = false

	notExists := func(ns, name string) {
		t.Helper()
		_, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("configmap %s/%s should not exist: %v", ns, name, err)
		}
	}
	// Out of scope: refused before anything (even the valid first object)
	// is created.
	_, err = m.Apply(ctx, id, later, []environment.Action{{Kind: model.ActionApply, Objects: []map[string]any{
		cm("", "should-not-exist"), cm("default", "escaped")}}})
	testutil.ErrorIs(t, err, environment.ErrOutOfScope)
	notExists("jk-p1", "should-not-exist")
	notExists("default", "escaped")
	_, err = m.Apply(ctx, id, later, []environment.Action{{Kind: model.ActionApply, Objects: []map[string]any{
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "metadata": map[string]any{"name": "jk-undeclared"}}}}})
	testutil.ErrorIs(t, err, environment.ErrOutOfScope)
	_, err = cs.RbacV1().ClusterRoles().Get(ctx, "jk-undeclared", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("undeclared ClusterRole was created: %v", err)
	}

	// Not owned: an object the learner made in the problem namespace, and a
	// namespace nobody marked, are left untouched.
	learner, err := cs.CoreV1().ConfigMaps("jk-p1").Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "learner-made"}, Data: map[string]string{"mine": "yes"}}, metav1.CreateOptions{})
	testutil.NoError(t, err)
	_, err = m.Apply(ctx, id, later, []environment.Action{{Kind: model.ActionApply, Objects: []map[string]any{cm("", "learner-made")}}})
	testutil.ErrorIs(t, err, environment.ErrNotOwned)
	now, err := cs.CoreV1().ConfigMaps("jk-p1").Get(ctx, "learner-made", metav1.GetOptions{})
	testutil.NoError(t, err)
	testutil.Equal(t, now.ResourceVersion, learner.ResourceVersion)

	unowned, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "jk-unowned"}}, metav1.CreateOptions{})
	testutil.NoError(t, err)
	_, err = m.Apply(ctx, id, environment.Owner{
		Inventory:        model.Inventory{EnvironmentID: id.ID, SessionID: "s1", ProblemID: "other"},
		Namespace:        "jk-unowned",
		CreateNamespaces: true, // not even setup may take over an unmarked namespace
	}, []environment.Action{{Kind: model.ActionApply, Objects: []map[string]any{cm("", "x")}}})
	testutil.ErrorIs(t, err, environment.ErrNotOwned)
	ns, err := cs.CoreV1().Namespaces().Get(ctx, "jk-unowned", metav1.GetOptions{})
	testutil.NoError(t, err)
	testutil.Equal(t, ns.ResourceVersion, unowned.ResourceVersion)
	notExists("jk-unowned", "x")

	// delete-owned removes only recorded objects.
	_, err = m.Apply(ctx, id, later, []environment.Action{{Kind: model.ActionDeleteOwned, Targets: []model.ObjectRef{
		{APIVersion: "v1", Kind: "ConfigMap", Name: "learner-made"}}}})
	testutil.ErrorIs(t, err, environment.ErrNotOwned)
	_, err = cs.CoreV1().ConfigMaps("jk-p1").Get(ctx, "learner-made", metav1.GetOptions{})
	testutil.NoError(t, err)

	after, err := m.Apply(ctx, id, later, []environment.Action{{Kind: model.ActionDeleteOwned, Targets: []model.ObjectRef{
		{APIVersion: "v1", Kind: "ConfigMap", Name: "web-config"}}}})
	testutil.NoError(t, err)
	if slices.ContainsFunc(after.Resources, func(r model.OwnedResource) bool { return r.Ref.Name == "web-config" }) {
		t.Fatalf("deleted object still in inventory: %+v", after.Resources)
	}
	testutil.Equal(t, len(after.Resources), len(inv.Resources)-1)
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := cs.CoreV1().ConfigMaps("jk-p1").Get(ctx, "web-config", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("web-config not deleted: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// A namespace the learner deleted is not recreated by later steps; only
	// an explicit setup (CreateNamespaces) creates it again.
	oldClientNS := liveMeta(t, cs, model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: "jk-p1-client"}).UID
	testutil.NoError(t, cs.CoreV1().Namespaces().Delete(ctx, "jk-p1-client", metav1.DeleteOptions{}))
	nsGone := func() bool {
		_, err := cs.CoreV1().Namespaces().Get(ctx, "jk-p1-client", metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	}
	deadline = time.Now().Add(2 * time.Minute)
	for !nsGone() {
		if time.Now().After(deadline) {
			t.Fatal("namespace jk-p1-client was not deleted")
		}
		time.Sleep(500 * time.Millisecond)
	}
	later.Inventory = after
	_, err = m.Apply(ctx, id, later, []environment.Action{{Kind: model.ActionApply, Objects: []map[string]any{cm("jk-p1-client", "client-config")}}})
	testutil.ErrorIs(t, err, environment.ErrNamespaceMissing)
	testutil.Equal(t, nsGone(), true)
	// delete-owned of an object that went with the namespace succeeds
	// without bringing the namespace back.
	afterDelete, err := m.Apply(ctx, id, later, []environment.Action{{Kind: model.ActionDeleteOwned, Targets: []model.ObjectRef{
		{APIVersion: "v1", Kind: "ConfigMap", Namespace: "jk-p1-client", Name: "client-config"}}}})
	testutil.NoError(t, err)
	testutil.Equal(t, nsGone(), true)
	testutil.Equal(t, len(afterDelete.Resources), len(after.Resources)-1)
	// Setup recreates it explicitly, as a new namespace.
	resetup := owner
	resetup.Inventory = afterDelete
	recreated, err := m.Apply(ctx, id, resetup, setup)
	testutil.NoError(t, err)
	newClientNS := liveMeta(t, cs, model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: "jk-p1-client"}).UID
	if newClientNS == oldClientNS {
		t.Fatal("namespace was not recreated")
	}
	for _, r := range recreated.Resources {
		if r.Ref.Kind == "Namespace" && r.Ref.Name == "jk-p1-client" {
			testutil.Equal(t, r.UID, string(newClientNS))
		}
	}
	later.Inventory = recreated

	// Unsupported kinds are explicit.
	_, err = m.Apply(ctx, id, later, []environment.Action{{Kind: model.ActionHelm}})
	testutil.ErrorIs(t, err, environment.ErrUnsupportedAction)
}

// liveMeta reads an object's metadata through typed clients.
func liveMeta(t *testing.T, cs kubernetes.Interface, ref model.ObjectRef) metav1.ObjectMeta {
	t.Helper()
	ctx := context.Background()
	var (
		meta metav1.ObjectMeta
		err  error
	)
	switch ref.Kind {
	case "Namespace":
		var o *corev1.Namespace
		o, err = cs.CoreV1().Namespaces().Get(ctx, ref.Name, metav1.GetOptions{})
		if err == nil {
			meta = o.ObjectMeta
		}
	case "ConfigMap":
		var o *corev1.ConfigMap
		o, err = cs.CoreV1().ConfigMaps(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err == nil {
			meta = o.ObjectMeta
		}
	case "Service":
		var o *corev1.Service
		o, err = cs.CoreV1().Services(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err == nil {
			meta = o.ObjectMeta
		}
	case "ClusterRole":
		o, gerr := cs.RbacV1().ClusterRoles().Get(ctx, ref.Name, metav1.GetOptions{})
		err = gerr
		if err == nil {
			meta = o.ObjectMeta
		}
	default:
		t.Fatalf("liveMeta: unsupported kind %s", ref.Kind)
	}
	testutil.NoError(t, err)
	return meta
}
