//go:build linux && (e2e || acceptance)

package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/store"
)

const (
	foreignNamespace = "foreign-acceptance-sentinel"
	foreignImage     = "foreign.local/sentinel:keep"
	imageBuildLabel  = "junkloud.io/image-build"
	provisionerRoot  = "/var/local-path-provisioner"
)

// operationKinds are every operation kind the product journals.
var operationKinds = []string{
	environment.OperationCreate, environment.OperationApply, environment.OperationCleanup,
	environment.OperationRecreate, environment.OperationPlatform, environment.OperationImageBuild,
	"session-prepare", "session-reset", "session-grade",
}

// ownedLeftovers is what a finished reset must not leave behind.
//
//   - Namespaces: Namespaces carrying the session label.
//   - ClusterObjects: every other listable object (namespaced or cluster
//     scoped) carrying the session label, as "Kind [namespace/]name".
//   - VolumePaths: local-path data directories (or PV backing paths) that
//     still exist in the node container.
//   - Images: Docker images labelled with a session ID or an image build.
//   - OpenOperations: journaled operations whose phase is not done.
type ownedLeftovers struct {
	Namespaces, ClusterObjects, VolumePaths, Images, OpenOperations []string
}

// Count is the number of leftovers of every kind.
func (o ownedLeftovers) Count() int {
	return len(o.Namespaces) + len(o.ClusterObjects) + len(o.VolumePaths) + len(o.Images) + len(o.OpenOperations)
}

type kubeClients struct {
	cs   kubernetes.Interface
	dyn  dynamic.Interface
	disc discovery.DiscoveryInterface
	id   model.Identity
}

func (h *cliHarness) kube(ctx context.Context) kubeClients {
	t := h.t
	t.Helper()
	id := h.identity(ctx)
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: h.kubeconfig()}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: id.Context}).ClientConfig()
	if err != nil {
		t.Fatalf("load the environment kubeconfig: %v", err)
	}
	cfg.QPS, cfg.Burst, cfg.Timeout = 50, 100, 30*time.Second
	return kubeClients{cs: mustKube(t, cfg), dyn: mustDyn(t, cfg), disc: mustDisc(t, cfg), id: id}
}

func mustKube(t *testing.T, cfg *rest.Config) kubernetes.Interface {
	c, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustDyn(t *testing.T, cfg *rest.Config) dynamic.Interface {
	c, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustDisc(t *testing.T, cfg *rest.Config) discovery.DiscoveryInterface {
	c, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out, diag bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &diag
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(diag.String()))
	}
	return out.String(), nil
}

// clusterCounts is the number of namespaces and PersistentVolumes.
func clusterCounts(ctx context.Context, t *testing.T, h *cliHarness) (namespaces, volumes int) {
	t.Helper()
	k := h.kube(ctx)
	ns, err := k.cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	pvs, err := k.cs.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list persistent volumes: %v", err)
	}
	return len(ns.Items), len(pvs.Items)
}

// observeLeftovers reports everything the session left behind. Every listable
// resource (dynamic discovery) is searched for the session label.
func observeLeftovers(ctx context.Context, t *testing.T, h *cliHarness) ownedLeftovers {
	t.Helper()
	k := h.kube(ctx)
	var left ownedLeftovers

	// Labelled Kubernetes objects of every listable resource.
	_, lists, err := k.disc.ServerGroupsAndResources()
	if err != nil {
		// A partial discovery (an unavailable aggregated API) is reported but
		// the reachable resources are still searched; a total failure is fatal.
		if len(lists) == 0 {
			t.Fatalf("discover the API resources: %v", err)
		}
		t.Logf("partial API discovery: %v", err)
	}
	seen := map[string]bool{}
	for _, list := range lists {
		gv, perr := schema.ParseGroupVersion(list.GroupVersion)
		if perr != nil {
			t.Fatal(perr)
		}
		for _, r := range list.APIResources {
			if strings.Contains(r.Name, "/") || !slices.Contains(r.Verbs, "list") {
				continue
			}
			key := gv.Group + "/" + r.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			items, lerr := k.dyn.Resource(gv.WithResource(r.Name)).List(ctx, metav1.ListOptions{LabelSelector: environment.LabelSessionID})
			if lerr != nil {
				t.Fatalf("cannot list %s while looking for leftovers: %v", key, lerr)
			}
			for _, item := range items.Items {
				switch {
				case r.Kind == "Namespace" && gv.Group == "":
					left.Namespaces = append(left.Namespaces, item.GetName())
				case item.GetNamespace() != "":
					left.ClusterObjects = append(left.ClusterObjects, fmt.Sprintf("%s %s/%s", r.Kind, item.GetNamespace(), item.GetName()))
				default:
					left.ClusterObjects = append(left.ClusterObjects, fmt.Sprintf("%s %s", r.Kind, item.GetName()))
				}
			}
		}
	}

	// Volume data that still exists in the node container.
	node := k.id.ClusterName + "-control-plane"
	pvs, err := k.cs.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list persistent volumes: %v", err)
	}
	for _, pv := range pvs.Items {
		var p string
		switch {
		case pv.Spec.HostPath != nil:
			p = pv.Spec.HostPath.Path
		case pv.Spec.Local != nil:
			p = pv.Spec.Local.Path
		}
		if p == "" {
			continue
		}
		if _, err := docker(ctx, "exec", node, "test", "-e", p); err == nil {
			left.VolumePaths = append(left.VolumePaths, p)
		}
	}
	// The directory appears with the first volume, so its absence is normal.
	// What must be right is the path itself: it has to be the one the
	// provisioner is configured with, or the check would pass vacuously.
	cfgMap, err := k.cs.CoreV1().ConfigMaps("local-path-storage").Get(ctx, "local-path-config", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the local-path provisioner configuration: %v", err)
	}
	if !strings.Contains(cfgMap.Data["config.json"], `"`+provisionerRoot+`"`) {
		t.Fatalf("the provisioner is not configured with %s: %s", provisionerRoot, cfgMap.Data["config.json"])
	}
	if _, err := docker(ctx, "exec", node, "test", "-d", provisionerRoot); err != nil {
		return finishLeftovers(ctx, t, h, left)
	}
	entries, err := docker(ctx, "exec", node, "ls", "-A", provisionerRoot)
	if err != nil {
		t.Fatalf("list the node volume directory: %v", err)
	}
	for _, name := range strings.Fields(entries) {
		p := path.Join(provisionerRoot, name)
		if !slices.Contains(left.VolumePaths, p) {
			left.VolumePaths = append(left.VolumePaths, p)
		}
	}

	return finishLeftovers(ctx, t, h, left)
}

// finishLeftovers adds the Docker images and open operations.
func finishLeftovers(ctx context.Context, t *testing.T, h *cliHarness, left ownedLeftovers) ownedLeftovers {
	t.Helper()
	// Docker images the sessions built.
	for _, label := range []string{environment.LabelSessionID, imageBuildLabel} {
		out, err := docker(ctx, "image", "ls", "--all", "--filter", "label="+label, "--format", "{{.Repository}}:{{.Tag}}@{{.ID}}")
		if err != nil {
			t.Fatalf("list labelled images: %v", err)
		}
		for _, line := range strings.Fields(out) {
			if !slices.Contains(left.Images, line) {
				left.Images = append(left.Images, line)
			}
		}
	}

	// Operations that never reached done.
	st, err := store.Open(filepath.Join(h.state, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, kind := range operationKinds {
		ops, err := st.ListOperations(ctx, kind)
		if err != nil {
			t.Fatalf("list %s operations: %v", kind, err)
		}
		for _, op := range ops {
			if !terminalPhase(op.Phase) {
				left.OpenOperations = append(left.OpenOperations, fmt.Sprintf("%s %s phase=%s", op.Kind, op.ID, op.Phase))
			}
		}
	}
	return left
}

// ownedObjectCount is the number of session-labelled objects now in the
// cluster. After `start` it must be positive, which proves the label selector
// of observeLeftovers matches what the product creates.
func ownedObjectCount(ctx context.Context, t *testing.T, h *cliHarness) int {
	t.Helper()
	left := observeLeftovers(ctx, t, h)
	return len(left.Namespaces) + len(left.ClusterObjects)
}

// foreignSentinel is the state of the objects the product must never touch.
type foreignSentinel struct{ NamespaceUID, ConfigMapData, ImageID string }

// plantForeign creates a namespace with a ConfigMap and a Docker tag that no
// session owns, and registers their removal.
func plantForeign(ctx context.Context, t *testing.T, h *cliHarness) foreignSentinel {
	t.Helper()
	k := h.kube(ctx)
	_, err := k.cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: foreignNamespace}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("plant the foreign namespace: %v", err)
	}
	_, err = k.cs.CoreV1().ConfigMaps(foreignNamespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "keep", Namespace: foreignNamespace},
		Data:       map[string]string{"note": "not owned by any session"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("plant the foreign ConfigMap: %v", err)
	}
	// A local scratch image with a unique label: no network needed.
	build := exec.CommandContext(ctx, "docker", "build", "--quiet", "--tag", foreignImage, "-")
	build.Stdin = strings.NewReader("FROM scratch\nLABEL junkloud.test=foreign-sentinel\n")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("plant the foreign image: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if out, err := docker(context.Background(), "image", "rm", foreignImage); err != nil {
			t.Logf("remove the foreign image: %v %s", err, out)
		}
	})
	return readForeign(ctx, t, h)
}

// readForeign reads the planted objects back.
func readForeign(ctx context.Context, t *testing.T, h *cliHarness) foreignSentinel {
	t.Helper()
	k := h.kube(ctx)
	var s foreignSentinel
	ns, err := k.cs.CoreV1().Namespaces().Get(ctx, foreignNamespace, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the foreign namespace is gone: %v", err)
	}
	s.NamespaceUID = string(ns.UID)
	cm, err := k.cs.CoreV1().ConfigMaps(foreignNamespace).Get(ctx, "keep", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the foreign ConfigMap is gone: %v", err)
	}
	data, err := json.Marshal(cm.Data)
	if err != nil {
		t.Fatal(err)
	}
	s.ConfigMapData = string(data)
	out, err := docker(ctx, "image", "inspect", "--format", "{{.Id}}", foreignImage)
	if err != nil {
		t.Fatalf("the foreign image is gone: %v", err)
	}
	s.ImageID = strings.TrimSpace(out)
	return s
}

// personalFiles hashes the learner's own files under root: .bashrc and
// notes.txt at the top, and every answer-notes.txt in an attempt directory.
// The key is the file path, the value its sha256.
func personalFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	hashFile := func(p string) {
		sum, err := sha256File(p)
		if err != nil {
			t.Fatalf("hash %s: %v", p, err)
		}
		out[p] = sum
	}
	for _, name := range []string{".bashrc", "notes.txt"} {
		p := filepath.Join(root, name)
		if fileExists(p) {
			hashFile(p)
		}
	}
	matches, err := filepath.Glob(filepath.Join(root, "*", "*", "attempt-*", "answer-notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range matches {
		hashFile(p)
	}
	return out
}

// memorySampler records the peak memory of the environment's containers.
type memorySampler struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	total  int
	byName map[string]int
}

var memUsage = regexp.MustCompile(`^([0-9.]+)\s*([A-Za-z]+)`)

// startMemorySampler samples `docker stats` for the kind node, the
// cloud-provider-kind container and the envoy containers every interval.
func startMemorySampler(ctx context.Context, interval time.Duration) *memorySampler {
	ctx, cancel := context.WithCancel(ctx)
	s := &memorySampler{cancel: cancel, done: make(chan struct{}), byName: map[string]int{}}
	go func() {
		defer close(s.done)
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			s.sample(ctx)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return s
}

func (s *memorySampler) sample(ctx context.Context) {
	out, err := docker(ctx, "stats", "--no-stream", "--format", "{{.Name}}\t{{.MemUsage}}")
	if err != nil {
		return
	}
	total := 0
	current := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		name, usage, ok := strings.Cut(line, "\t")
		if !ok || !(strings.HasPrefix(name, "junkloud-") || strings.HasPrefix(name, "kindccm-") || strings.Contains(name, "envoy")) {
			continue
		}
		mib, ok := parseMiB(strings.TrimSpace(strings.Split(usage, "/")[0]))
		if !ok {
			continue
		}
		current[name] = mib
		total += mib
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total = max(s.total, total)
	for name, mib := range current {
		s.byName[name] = max(s.byName[name], mib)
	}
}

func parseMiB(s string) (int, bool) {
	m := memUsage.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	switch strings.ToLower(m[2]) {
	case "b":
		v /= 1 << 20
	case "kib", "kb":
		v /= 1 << 10
	case "mib", "mb":
	case "gib", "gb":
		v *= 1 << 10
	default:
		return 0, false
	}
	return int(v + 0.5), true
}

// Saw reports whether any container was sampled. Callers must fail the test
// when it is false after Stop, since the peak would be a vacuous zero.
func (s *memorySampler) Saw() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byName) > 0
}

// Stop ends the sampling and returns the peak total (the maximum over the
// samples of the summed usage) and the peak of each container, in MiB.
func (s *memorySampler) Stop() (peakTotalMiB int, peakByContainer map[string]int) {
	s.cancel()
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.byName))
	for k, v := range s.byName {
		out[k] = v
	}
	return s.total, out
}

// oomOrPending lists environment-caused trouble: containers the kernel
// OOM-killed (the environment's containers and Pod containers) and Pods stuck
// Pending with a FailedScheduling event. It returns nil when there is none.
func oomOrPending(ctx context.Context, t *testing.T, h *cliHarness) []string {
	t.Helper()
	k := h.kube(ctx)
	var found []string

	names, err := docker(ctx, "ps", "--all", "--format", "{{.Names}}")
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	for _, name := range strings.Fields(names) {
		if !(strings.HasPrefix(name, "junkloud-") || strings.HasPrefix(name, "kindccm-")) {
			continue
		}
		out, err := docker(ctx, "inspect", "--format", "{{.State.OOMKilled}}", name)
		if err != nil {
			t.Fatalf("inspect %s: %v", name, err)
		}
		if strings.TrimSpace(out) == "true" {
			found = append(found, "container "+name+" OOMKilled")
		}
	}

	pods, err := k.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	for _, pod := range pods.Items {
		statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
		for _, cs := range statuses {
			for _, term := range []*corev1.ContainerStateTerminated{cs.LastTerminationState.Terminated, cs.State.Terminated} {
				if term != nil && term.Reason == "OOMKilled" {
					found = append(found, fmt.Sprintf("pod %s/%s container %s OOMKilled", pod.Namespace, pod.Name, cs.Name))
				}
			}
		}
		if pod.Status.Phase != corev1.PodPending {
			continue
		}
		events, err := k.cs.CoreV1().Events(pod.Namespace).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.name=" + pod.Name + ",reason=FailedScheduling"})
		if err != nil {
			t.Fatalf("list events of %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		for _, ev := range events.Items {
			found = append(found, fmt.Sprintf("pod %s/%s Pending: FailedScheduling: %s", pod.Namespace, pod.Name, ev.Message))
		}
	}
	return found
}

func sha256File(p string) (string, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// terminalPhase: done, plus the phases of an operation that ended without
// leaving work open: "superseded" (an interrupted grading that a retry or
// resubmission replaced) and "failed" (a preparation whose session is
// SETUP_ERROR).
func terminalPhase(phase string) bool {
	return phase == environment.PhaseDone || phase == "superseded" || phase == "failed"
}
