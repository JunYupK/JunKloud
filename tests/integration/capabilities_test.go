//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"

	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// unsupportedCapabilities reads JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES, a
// comma-separated list of capabilities the test host is known not to
// provide (for example "network-policy" on a kernel without nft_queue).
// Those must be reported FAIL with a reason (the doctor detects the gap);
// every other capability must PASS. Unset means every capability must PASS.
func unsupportedCapabilities(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, name := range strings.Split(os.Getenv("JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES"), ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !slices.Contains(environment.Capabilities, name) {
			t.Fatalf("JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES names unknown capability %q", name)
		}
		out[name] = true
	}
	return out
}

// expectReport checks that the report covers every capability exactly once,
// in order, and that exactly the capabilities in failing are FAIL (each with
// a reason).
func expectReport(t *testing.T, report environment.DoctorReport, id model.Identity, failing map[string]bool) {
	t.Helper()
	for _, c := range report.Checks {
		t.Logf("doctor: %-15s %s %s", c.Name, c.Status, c.Reason)
	}
	testutil.Equal(t, report.EnvironmentID, id.ID)
	var names []string
	for _, c := range report.Checks {
		names = append(names, c.Name)
		if failing[c.Name] {
			testutil.Equal(t, c.Status, environment.StatusFail)
			if c.Reason == "" {
				t.Errorf("%s: FAIL without a reason", c.Name)
			}
			continue
		}
		testutil.Equal(t, c.Status, environment.StatusPass)
	}
	testutil.Equal(t, names, environment.Capabilities)
}

func union(a map[string]bool, names ...string) map[string]bool {
	out := map[string]bool{}
	for k, v := range a {
		out[k] = v
	}
	for _, n := range names {
		out[n] = true
	}
	return out
}

// bootEnvironment creates a JunKloud environment. Cleanup (registered in
// this order, run in reverse) stops the platform's host-side containers and
// then deletes the cluster.
func bootEnvironment(t *testing.T) (*environment.Manager, model.Identity) {
	t.Helper()
	dir, st := newEnvironmentDir(t)
	m := environment.New(dir, st, hostCompatOptions(t)...)
	id, err := m.Ensure(context.Background())
	testutil.NoError(t, err)
	t.Cleanup(func() {
		if err := m.StopPlatform(context.Background(), id); err != nil {
			t.Errorf("cleanup: stop platform: %v", err)
		}
		if ids := cloudProviderContainers(t, id); len(ids) != 0 {
			t.Errorf("cleanup: platform containers left: %v", ids)
		}
	})
	return m, id
}

// cloudProviderContainers lists the JunKloud cloud-provider-kind container
// and every load-balancer container cloud-provider-kind made for this
// environment's cluster.
func cloudProviderContainers(t *testing.T, id model.Identity) []string {
	t.Helper()
	var ids []string
	for _, filter := range []string{
		"label=" + environment.LabelEnvironmentID + "=" + id.ID,
		"label=io.x-k8s.cloud-provider-kind.cluster=" + id.ClusterName,
	} {
		res, err := toolEnv.Run(context.Background(), "docker", []string{"ps", "--all", "--quiet", "--filter", filter}, nil)
		testutil.NoError(t, err)
		ids = append(ids, strings.Fields(string(res.Stdout))...)
	}
	return ids
}

type containerState struct {
	ID     string
	Image  string
	Labels map[string]string
	Status string
}

func inspectContainer(t *testing.T, name string) (containerState, bool) {
	t.Helper()
	res, err := toolEnv.Run(context.Background(), "docker", []string{"container", "inspect", "--format",
		`{"ID":{{json .Id}},"Image":{{json .Config.Image}},"Labels":{{json .Config.Labels}},"Status":{{json .State.Status}}}`, name}, nil)
	if err != nil {
		return containerState{}, false
	}
	var c containerState
	testutil.NoError(t, json.Unmarshal(bytes.TrimSpace(res.Stdout), &c))
	return c, true
}

// assertNoProbeLeftovers: a finished doctor run leaves none of its probe
// objects in the platform namespace and none of its build images behind.
func assertNoProbeLeftovers(t *testing.T, cs kubernetes.Interface, id model.Identity) {
	t.Helper()
	ctx := context.Background()
	sel := metav1.ListOptions{LabelSelector: environment.LabelProbeRun}
	pods, err := cs.CoreV1().Pods(environment.PlatformNamespace).List(ctx, sel)
	testutil.NoError(t, err)
	svcs, err := cs.CoreV1().Services(environment.PlatformNamespace).List(ctx, sel)
	testutil.NoError(t, err)
	pvcs, err := cs.CoreV1().PersistentVolumeClaims(environment.PlatformNamespace).List(ctx, sel)
	testutil.NoError(t, err)
	ings, err := cs.NetworkingV1().Ingresses(environment.PlatformNamespace).List(ctx, sel)
	testutil.NoError(t, err)
	nps, err := cs.NetworkingV1().NetworkPolicies(environment.PlatformNamespace).List(ctx, sel)
	testutil.NoError(t, err)
	var left []string
	for _, p := range pods.Items {
		if p.DeletionTimestamp == nil {
			left = append(left, "pod/"+p.Name)
		}
	}
	for _, s := range svcs.Items {
		left = append(left, "service/"+s.Name)
	}
	for _, p := range pvcs.Items {
		if p.DeletionTimestamp == nil {
			left = append(left, "pvc/"+p.Name)
		}
	}
	for _, i := range ings.Items {
		left = append(left, "ingress/"+i.Name)
	}
	for _, n := range nps.Items {
		left = append(left, "networkpolicy/"+n.Name)
	}
	if len(left) != 0 {
		t.Errorf("doctor left probe objects: %v", left)
	}
	res, err := toolEnv.Run(ctx, "docker", []string{"images", "--quiet", "--filter", "reference=" + environment.ImageBuildProbeRepository}, nil)
	testutil.NoError(t, err)
	if s := strings.TrimSpace(string(res.Stdout)); s != "" {
		t.Errorf("doctor left image-build probe images on the host: %s", s)
	}
	res, err = toolEnv.Run(ctx, "docker", []string{"exec", id.ContainerIDs[0], "crictl", "images"}, nil)
	testutil.NoError(t, err)
	if strings.Contains(string(res.Stdout), environment.ImageBuildProbeRepository) {
		t.Errorf("doctor left image-build probe images on the node:\n%s", res.Stdout)
	}
}

func TestPlatformCapabilities(t *testing.T) {
	requireTools(t, "docker", "kind")
	ctx := context.Background()
	unsupported := unsupportedCapabilities(t)
	m, id := bootEnvironment(t)
	cs := kubeClient(t, m, id)

	t.Run("ReportsMissingAddons", func(t *testing.T) {
		// Only the probe image is loaded and no add-on is installed: the
		// built-in capabilities work, Ingress and metrics are reported FAIL.
		testutil.NoError(t, m.PreloadImages(ctx, id, environment.ProbeImage))
		report, err := m.Doctor(ctx, id)
		testutil.NoError(t, err)
		expectReport(t, report, id, union(unsupported, environment.CapabilityIngress, environment.CapabilityMetrics))
		assertNoProbeLeftovers(t, cs, id)
	})

	t.Run("AllCapabilities", func(t *testing.T) {
		testutil.NoError(t, m.EnsurePlatform(ctx, id))
		report, err := m.Doctor(ctx, id)
		testutil.NoError(t, err)
		expectReport(t, report, id, unsupported)
		for _, key := range []string{"kubernetes", "node_image", "cloud-provider-kind", "envoy-proxy", "metrics-server", "probe-image"} {
			if report.Versions[key] == "" {
				t.Errorf("report versions lack %s: %v", key, report.Versions)
			}
		}
		testutil.Equal(t, report.Versions["cloud-provider-kind"], environment.CloudProviderKindImage.Ref())
		testutil.Equal(t, report.Versions["metrics-server"], environment.MetricsServerImage.Ref())
		assertNoProbeLeftovers(t, cs, id)
		// A capability failure disables exactly the problems that need it.
		testutil.Equal(t, len(report.Unavailable([]string{environment.CapabilityDNS, environment.CapabilityStorage})), 0)
	})

	t.Run("CloudProviderKindLifecycle", func(t *testing.T) {
		name := environment.CloudProviderContainerName(id.ID)
		c, ok := inspectContainer(t, name)
		if !ok {
			t.Fatalf("cloud-provider-kind container %s missing", name)
		}
		testutil.Equal(t, c.Status, "running")
		testutil.Equal(t, c.Image, environment.CloudProviderKindImage.Ref())
		testutil.Equal(t, c.Labels[environment.LabelEnvironmentID], id.ID)
		testutil.Equal(t, c.Labels[environment.LabelComponent], environment.ComponentCloudProvider)

		// Stopped behind JunKloud's back: the doctor reports Ingress FAIL and
		// EnsurePlatform restarts the same container.
		_, err := toolEnv.Run(ctx, "docker", []string{"stop", c.ID}, nil)
		testutil.NoError(t, err)
		report, err := m.Doctor(ctx, id)
		testutil.NoError(t, err)
		expectReport(t, report, id, union(unsupported, environment.CapabilityIngress))
		testutil.NoError(t, m.EnsurePlatform(ctx, id))
		again, ok := inspectContainer(t, name)
		testutil.Equal(t, ok, true)
		testutil.Equal(t, again.ID, c.ID)
		testutil.Equal(t, again.Status, "running")
		report, err = m.Doctor(ctx, id)
		testutil.NoError(t, err)
		expectReport(t, report, id, unsupported)

		// Stop removes the container and every load balancer it made for
		// this cluster; stopping twice is harmless; ensuring recreates it.
		testutil.NoError(t, m.StopPlatform(ctx, id))
		testutil.Equal(t, len(cloudProviderContainers(t, id)), 0)
		testutil.NoError(t, m.StopPlatform(ctx, id))
		testutil.NoError(t, m.EnsurePlatform(ctx, id))
		fresh, ok := inspectContainer(t, name)
		testutil.Equal(t, ok, true)
		testutil.Equal(t, fresh.Status, "running")
		if fresh.ID == c.ID {
			t.Fatal("cloud-provider-kind was not recreated after stop")
		}
	})
}

// auditEvent is the part of an audit.k8s.io/v1 Event the test inspects.
type auditEvent struct {
	Kind                     string    `json:"kind"`
	Stage                    string    `json:"stage"`
	Verb                     string    `json:"verb"`
	RequestURI               string    `json:"requestURI"`
	UserAgent                string    `json:"userAgent"`
	RequestReceivedTimestamp time.Time `json:"requestReceivedTimestamp"`
	ObjectRef                *struct {
		Resource    string `json:"resource"`
		Namespace   string `json:"namespace"`
		Name        string `json:"name"`
		APIGroup    string `json:"apiGroup"`
		Subresource string `json:"subresource"`
	} `json:"objectRef"`
}

const auditPolicy = `apiVersion: audit.k8s.io/v1
kind: Policy
omitStages: ["RequestReceived"]
rules:
- level: Metadata
  users: ["kubernetes-admin"]
- level: None
`

// enableAudit turns on the API server audit log of the environment's
// control-plane node (requests by the admin user JunKloud authenticates as,
// at Metadata level, written to the API server's stdout) by editing its
// static pod manifest, and waits until the restarted API server logs events
// and the environment still verifies. This is test instrumentation outside
// JunKloud: the product is unchanged.
func enableAudit(t *testing.T, m *environment.Manager, id model.Identity) {
	t.Helper()
	ctx := context.Background()
	node := id.ContainerIDs[0]
	_, err := toolEnv.Run(ctx, "docker", []string{"exec", "-i", node, "sh", "-c", "cat > /etc/kubernetes/pki/junkloud-test-audit-policy.yaml"}, strings.NewReader(auditPolicy))
	testutil.NoError(t, err)
	res, err := toolEnv.Run(ctx, "docker", []string{"exec", node, "cat", "/etc/kubernetes/manifests/kube-apiserver.yaml"}, nil)
	testutil.NoError(t, err)
	manifest := string(res.Stdout)
	const anchor = "    - kube-apiserver\n"
	if !strings.Contains(manifest, anchor) {
		t.Fatalf("unexpected kube-apiserver manifest:\n%s", manifest)
	}
	manifest = strings.Replace(manifest, anchor, anchor+
		"    - --audit-policy-file=/etc/kubernetes/pki/junkloud-test-audit-policy.yaml\n"+
		"    - --audit-log-path=-\n", 1)
	_, err = toolEnv.Run(ctx, "docker", []string{"exec", "-i", node, "sh", "-c",
		"cat > /etc/kubernetes/junkloud-apiserver.yaml.tmp && mv /etc/kubernetes/junkloud-apiserver.yaml.tmp /etc/kubernetes/manifests/kube-apiserver.yaml"},
		strings.NewReader(manifest))
	testutil.NoError(t, err)

	deadline := time.Now().Add(4 * time.Minute)
	for {
		if len(auditEvents(t, id)) > 0 && m.Check(ctx, id) == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("API server did not come back with auditing enabled")
		}
		time.Sleep(3 * time.Second)
	}
}

// auditEvents reads every audit event the API server has written so far
// from its container logs on the node (CRI log format).
func auditEvents(t *testing.T, id model.Identity) []auditEvent {
	t.Helper()
	res, err := toolEnv.Run(context.Background(), "docker", []string{"exec", id.ContainerIDs[0], "sh", "-c",
		"for f in /var/log/pods/kube-system_kube-apiserver-*/kube-apiserver/*; do case $f in *.gz) ;; *) cat \"$f\" ;; esac; done 2>/dev/null; true"}, nil)
	testutil.NoError(t, err)
	var events []auditEvent
	var partial strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(res.Stdout))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		// <time> <stream> <P|F> <content>
		fields := strings.SplitN(sc.Text(), " ", 4)
		if len(fields) != 4 || fields[1] != "stdout" {
			continue
		}
		partial.WriteString(fields[3])
		if fields[2] == "P" {
			continue
		}
		line := partial.String()
		partial.Reset()
		var ev auditEvent
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Kind == "Event" {
			events = append(events, ev)
		}
	}
	testutil.NoError(t, sc.Err())
	return events
}

func TestDoctorDoesNotReadAnswers(t *testing.T) {
	requireTools(t, "docker", "kind")
	ctx := context.Background()
	unsupported := unsupportedCapabilities(t)
	m, id := bootEnvironment(t)
	enableAudit(t, m, id)

	// An exam problem with its hidden answer, set up the way problems are.
	answerNS := "jk-exam-" + randomHex(t, 3)
	_, err := m.Apply(ctx, id, environment.Owner{
		Inventory:        model.Inventory{EnvironmentID: id.ID, SessionID: "s-audit", ProblemID: "p-answer"},
		Namespace:        answerNS,
		CreateNamespaces: true,
	}, []environment.Action{{Kind: model.ActionApply, Objects: []map[string]any{
		{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "answer"}, "stringData": map[string]any{"expected": "hidden"}},
		cm("", "grading-notes"),
	}}})
	testutil.NoError(t, err)
	testutil.NoError(t, m.EnsurePlatform(ctx, id))

	start := time.Now()
	report, err := m.Doctor(ctx, id)
	end := time.Now()
	testutil.NoError(t, err)
	expectReport(t, report, id, unsupported)

	// Which resources are namespaced: a cluster-wide request for one of
	// them (namespace "") could return the answer namespace's objects.
	cs := kubeClient(t, m, id)
	_, lists, err := cs.Discovery().ServerGroupsAndResources()
	testutil.NoError(t, err)
	namespaced := map[schema.GroupResource]bool{}
	for _, l := range lists {
		gv, err := schema.ParseGroupVersion(l.GroupVersion)
		testutil.NoError(t, err)
		for _, r := range l.APIResources {
			if !strings.Contains(r.Name, "/") {
				namespaced[schema.GroupResource{Group: gv.Group, Resource: r.Name}] = r.Namespaced
			}
		}
	}

	allowed := []string{environment.PlatformNamespace, environment.MarkerNamespace, "kube-system"}
	answerNamespaceReadCount := 0
	doctorRequests, platformRequests := 0, 0
	touched := map[string]bool{}
	// The audit log can trail the response slightly; wait until it has
	// caught up with the end of the doctor run.
	var events []auditEvent
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
		events = auditEvents(t, id)
		if len(events) > 0 && !events[len(events)-1].RequestReceivedTimestamp.Before(end) || time.Now().After(deadline) {
			break
		}
	}
	for _, ev := range events {
		if ev.UserAgent != "junkloud" || ev.RequestReceivedTimestamp.Before(start.Add(-time.Second)) || ev.RequestReceivedTimestamp.After(end.Add(time.Second)) {
			continue
		}
		doctorRequests++
		if ev.ObjectRef == nil {
			continue // non-resource request (discovery, /version)
		}
		ref := ev.ObjectRef
		gr := schema.GroupResource{Group: ref.APIGroup, Resource: ref.Resource}
		switch {
		case ref.Namespace == answerNS:
			answerNamespaceReadCount++
		case ref.Resource == "namespaces" && ref.APIGroup == "" && (ref.Name == answerNS || ref.Name == ""):
			answerNamespaceReadCount++ // reading or listing the answer namespace itself
		case ref.Namespace == "" && namespaced[gr]:
			answerNamespaceReadCount++ // cluster-wide read across every namespace
			t.Logf("cluster-wide request: %s %s", ev.Verb, ev.RequestURI)
		}
		if ref.Namespace != "" {
			touched[ref.Namespace] = true
		}
		if ref.Namespace == environment.PlatformNamespace {
			platformRequests++
		}
	}
	t.Logf("audit: %d doctor requests, %d in %s, namespaces %v", doctorRequests, platformRequests, environment.PlatformNamespace, touched)
	// The audit log really captured the doctor's traffic, so zero below is
	// not vacuous.
	if platformRequests == 0 {
		t.Fatal("audit log captured no doctor requests in the platform namespace")
	}
	testutil.Equal(t, answerNamespaceReadCount, 0)
	for ns := range touched {
		if !slices.Contains(allowed, ns) {
			t.Errorf("doctor touched namespace %q", ns)
		}
	}
}
