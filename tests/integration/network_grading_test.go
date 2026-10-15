//go:build integration

package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

const (
	netNamespace      = "jk-net"
	netOtherNamespace = "jk-net-other"
	netWebMarker      = "JK-NET-WEB"
	netOtherMarker    = "JK-NET-OTHER"
)

// netFixture loads the objects of tests/fixtures/network/<name>.
func netFixture(t *testing.T, name string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "fixtures", "network", name))
	testutil.NoError(t, err)
	var out []map[string]any
	for _, doc := range strings.Split(string(raw), "\n---\n") {
		var obj map[string]any
		testutil.NoError(t, yaml.Unmarshal([]byte(doc), &obj))
		if len(obj) > 0 {
			out = append(out, obj)
		}
	}
	return out
}

func netRef(kind, name string) model.ObjectRef {
	apiVersion := map[string]string{"Service": "v1", "Deployment": "apps/v1", "NetworkPolicy": "networking.k8s.io/v1", "Ingress": "networking.k8s.io/v1"}[kind]
	return model.ObjectRef{APIVersion: apiVersion, Kind: kind, Namespace: netNamespace, Name: name}
}

func netSource(role string) map[string]any {
	return map[string]any{"labels": map[string]any{"role": role}}
}

// netCriteria are the problem's network criteria. The frontend (same
// namespace, role=frontend) must reach web; an intruder from another
// namespace (role=intruder) must not.
func netCriteria() (policy, httpC, tcpC model.Criterion) {
	svc := netRef("Service", "web")
	intruder := map[string]any{"namespace": netOtherNamespace, "labels": map[string]any{"role": "intruder"}}
	policy = model.Criterion{ID: "policy", Kind: model.CriterionNetworkPolicy, Target: svc, Points: 5, Timeout: 40 * time.Second,
		Args: map[string]any{"expectations": []any{
			map[string]any{"name": "frontend-allowed", "expect": "allow", "port": 80, "source": netSource("frontend"),
				"http": map[string]any{"body_marker": netWebMarker}},
			map[string]any{"name": "intruder-denied", "expect": "deny", "port": 80, "source": intruder},
		}}}
	httpC = model.Criterion{ID: "http", Kind: model.CriterionHTTP, Target: svc, Points: 3, Timeout: 40 * time.Second,
		Args: map[string]any{"source": netSource("frontend"), "port": 80, "body_marker": netWebMarker}}
	tcpC = model.Criterion{ID: "tcp", Kind: model.CriterionTCP, Target: svc, Points: 2, Timeout: 40 * time.Second,
		Args: map[string]any{"source": netSource("frontend"), "port": 80}}
	return
}

func waitUntil(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(time.Second)
	}
}

func readyEndpoints(t *testing.T, cs kubernetes.Interface, ns, svc string) int {
	t.Helper()
	list, err := cs.DiscoveryV1().EndpointSlices(ns).List(context.Background(), metav1.ListOptions{LabelSelector: "kubernetes.io/service-name=" + svc})
	testutil.NoError(t, err)
	n := 0
	for _, s := range list.Items {
		for _, ep := range s.Endpoints {
			if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
				n++
			}
		}
	}
	return n
}

func rolledOut(d *appsv1.Deployment) bool {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	return s.ObservedGeneration >= d.Generation && s.UpdatedReplicas == want && s.ReadyReplicas == want && s.Replicas == want
}

// probePods lists every grading probe pod of the cluster.
func probePods(t *testing.T, cs kubernetes.Interface) []string {
	t.Helper()
	list, err := cs.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{LabelSelector: grader.LabelProbeComponent + "=" + grader.ProbeComponent})
	testutil.NoError(t, err)
	var out []string
	for _, p := range list.Items {
		out = append(out, p.Namespace+"/"+p.Name)
	}
	return out
}

func TestNetworkGrading(t *testing.T) {
	requireTools(t, "docker", "kind")
	ctx := context.Background()
	enforced := !unsupportedCapabilities(t)[environment.CapabilityNetworkPolicy]
	m, id := bootEnvironment(t)
	testutil.NoError(t, m.EnsurePlatform(ctx, id))
	testutil.NoError(t, m.PreloadImages(ctx, id, environment.ProbeImage))
	cs := kubeClient(t, m, id)

	reader, err := grader.NewClusterReader(m.KubeconfigPath(), id.Context)
	testutil.NoError(t, err)
	newEngine := func(image string) *grader.Engine {
		opts := []grader.Option{
			grader.WithPreloadedImages(environment.ProbeImage.Name),
			grader.WithProbeImage(image),
			grader.WithProbeControlNamespace(environment.PlatformNamespace),
		}
		runner, err := grader.NewPodProbeRunner(m.KubeconfigPath(), id.Context, opts...)
		testutil.NoError(t, err)
		checkers := grader.DefaultCheckers(reader, opts...)
		grader.AddNetworkCheckers(checkers, grader.NewNetworkChecker(reader, runner, opts...))
		return grader.NewEngine(checkers, opts...)
	}
	engine := newEngine(environment.ProbeImage.Name)

	owner := environment.Owner{
		Inventory:        model.Inventory{EnvironmentID: id.ID, SessionID: "s1", ProblemID: "net-1"},
		Namespace:        netNamespace,
		Scope:            model.ResourceScope{Namespaces: []string{netOtherNamespace}},
		CreateNamespaces: true,
	}
	apply := func(t *testing.T, objs ...map[string]any) {
		t.Helper()
		var err error
		owner.Inventory, err = m.Apply(ctx, id, owner, []environment.Action{{Kind: model.ActionApply, Objects: objs}})
		testutil.NoError(t, err)
		owner.CreateNamespaces = false
	}
	remove := func(t *testing.T, refs ...model.ObjectRef) {
		t.Helper()
		var err error
		owner.Inventory, err = m.Apply(ctx, id, owner, []environment.Action{{Kind: model.ActionDeleteOwned, Targets: refs}})
		testutil.NoError(t, err)
	}
	waitWebReady := func(t *testing.T) {
		t.Helper()
		deadline := time.Now().Add(90 * time.Second)
		for {
			d, err := cs.AppsV1().Deployments(netNamespace).Get(ctx, "web", metav1.GetOptions{})
			if err == nil && rolledOut(d) && readyEndpoints(t, cs, netNamespace, "web") == 1 {
				return
			}
			if time.Now().After(deadline) {
				pods, _ := cs.CoreV1().Pods(netNamespace).List(ctx, metav1.ListOptions{})
				for _, p := range pods.Items {
					t.Logf("pod %s phase=%s statuses=%+v", p.Name, p.Status.Phase, p.Status.ContainerStatuses)
				}
				if d != nil {
					t.Logf("deployment generation=%d status=%+v", d.Generation, d.Status)
				}
				t.Fatalf("web was not rolled out with one ready endpoint (ready endpoints: %d)", readyEndpoints(t, cs, netNamespace, "web"))
			}
			time.Sleep(time.Second)
		}
	}
	// The other namespace must exist for the intruder's probe pod. The first
	// apply creates both namespaces and the fixtures.
	apply(t, append(netFixture(t, "web.yaml"), netFixture(t, "other.yaml")...)...)
	waitWebReady(t)

	grade := func(t *testing.T, eng *grader.Engine, criteria ...model.Criterion) map[string]model.CheckResult {
		t.Helper()
		ev, err := eng.Evaluate(ctx, grader.Target{Identity: id, Problem: model.FrozenProblem{
			Definition: model.Problem{ID: "net-1", Namespace: netNamespace, Criteria: criteria}, Namespace: netNamespace}})
		testutil.NoError(t, err)
		out := map[string]model.CheckResult{}
		for _, r := range ev.Results {
			t.Logf("%-7s %-5s %s", r.CriterionID, r.Status, r.Reason)
			out[r.CriterionID] = r
		}
		// Whatever the verdict, no probe pod may be left behind.
		testutil.Equal(t, len(probePods(t, cs)), 0)
		return out
	}
	policy, httpC, tcpC := netCriteria()

	t.Run("HTTPAndTCPReachTheServer", func(t *testing.T) {
		res := grade(t, engine, httpC, tcpC)
		testutil.Equal(t, res["http"].Status, "PASS")
		testutil.Equal(t, res["http"].Points, 3)
		testutil.Equal(t, res["tcp"].Status, "PASS")
		testutil.Equal(t, res["http"].Evidence.UID != "", true)
	})

	t.Run("NoPolicy", func(t *testing.T) {
		// Nothing restricts web: the intruder connects, so the policy
		// criterion fails even though the allowed path works.
		res := grade(t, engine, policy)
		testutil.Equal(t, res["policy"].Status, "FAIL")
		if !strings.Contains(res["policy"].Reason, "intruder-denied") || !strings.Contains(res["policy"].Reason, "connected") {
			t.Fatalf("reason %q", res["policy"].Reason)
		}
	})

	t.Run("WrongBackend", func(t *testing.T) {
		// The Service selects the other application: it answers 200, with
		// the wrong application marker.
		svc := netFixture(t, "web.yaml")[1]
		svc["spec"].(map[string]any)["selector"] = map[string]any{"app": "other"}
		apply(t, svc)
		waitUntil(t, "the other application to back web", 60*time.Second, func() bool { return readyEndpoints(t, cs, netNamespace, "web") == 1 })
		res := grade(t, engine, httpC, tcpC)
		testutil.Equal(t, res["http"].Status, "FAIL")
		if !strings.Contains(res["http"].Reason, "http-mismatch") {
			t.Fatalf("reason %q", res["http"].Reason)
		}
		testutil.Equal(t, res["tcp"].Status, "PASS") // something listens
		apply(t, netFixture(t, "web.yaml")[1])
		waitWebReady(t)
		res = grade(t, engine, httpC)
		testutil.Equal(t, res["http"].Status, "PASS")
	})

	t.Run("CorrectPolicy", func(t *testing.T) {
		if !enforced {
			t.Skip("NOT RUN: this environment does not enforce NetworkPolicy (JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES lists network-policy); a correct policy can only PASS where it is enforced")
		}
		apply(t, netFixture(t, "policy-correct.yaml")...)
		defer remove(t, netRef("NetworkPolicy", "web-from-frontend"))
		res := grade(t, engine, policy)
		testutil.Equal(t, res["policy"].Status, "PASS")
		testutil.Equal(t, res["policy"].Points, 5)
	})

	t.Run("CorrectPolicyUnenforcedIsNotAPass", func(t *testing.T) {
		if enforced {
			t.Skip("covered by CorrectPolicy: this environment enforces NetworkPolicy")
		}
		// The learner's policy is right, but nothing enforces it here: the
		// intruder still connects. That must be a FAIL, never a pass.
		apply(t, netFixture(t, "policy-correct.yaml")...)
		defer remove(t, netRef("NetworkPolicy", "web-from-frontend"))
		res := grade(t, engine, policy)
		testutil.Equal(t, res["policy"].Status, "FAIL")
		if !strings.Contains(res["policy"].Reason, "intruder-denied") || !strings.Contains(res["policy"].Reason, "connected") {
			t.Fatalf("reason %q", res["policy"].Reason)
		}
	})

	t.Run("AllBlocked", func(t *testing.T) {
		if !enforced {
			t.Skip("NOT RUN: this environment does not enforce NetworkPolicy (JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES lists network-policy); a deny-all policy only blocks where it is enforced")
		}
		apply(t, netFixture(t, "policy-deny-all.yaml")...)
		defer remove(t, netRef("NetworkPolicy", "deny-all"))
		res := grade(t, engine, policy, httpC)
		// The intruder is blocked, but so is the frontend: the block proves
		// nothing and the policy is wrong.
		testutil.Equal(t, res["policy"].Status, "FAIL")
		testutil.Equal(t, res["http"].Status, "FAIL")
		if !strings.Contains(res["policy"].Reason, "frontend-allowed") {
			t.Fatalf("reason %q", res["policy"].Reason)
		}
	})

	t.Run("ServerDown", func(t *testing.T) {
		web := netFixture(t, "web.yaml")[0]
		down := netFixture(t, "web.yaml")[0]
		down["spec"].(map[string]any)["replicas"] = 0
		apply(t, down)
		waitUntil(t, "web to lose its endpoints", 60*time.Second, func() bool { return readyEndpoints(t, cs, netNamespace, "web") == 0 })
		res := grade(t, engine, policy, httpC)
		testutil.Equal(t, res["policy"].Status, "FAIL")
		testutil.Equal(t, res["http"].Status, "FAIL")
		if !strings.Contains(res["policy"].Reason, "no ready endpoints") {
			t.Fatalf("reason %q", res["policy"].Reason)
		}
		apply(t, web)
		waitWebReady(t)
	})

	t.Run("DeadServerBehindReadyEndpoint", func(t *testing.T) {
		// The pod is Ready for Kubernetes but serves nothing, so every
		// connection is refused: the intruder's refusal is not a policy.
		// Replace the Deployment (server-side apply keeps fields that the
		// environment's own create step owns, such as the readiness probe).
		remove(t, netRef("Deployment", "web"))
		apply(t, netFixture(t, "hung.yaml")...)
		waitWebReady(t)
		res := grade(t, engine, policy, httpC, tcpC)
		for _, cid := range []string{"policy", "http", "tcp"} {
			testutil.Equal(t, res[cid].Status, "FAIL")
		}
		if !strings.Contains(res["policy"].Reason, "frontend-allowed") {
			t.Fatalf("reason %q", res["policy"].Reason)
		}
		remove(t, netRef("Deployment", "web"))
		apply(t, netFixture(t, "web.yaml")[0])
		waitWebReady(t)
	})

	t.Run("IngressRouting", func(t *testing.T) {
		apply(t, netFixture(t, "ingress.yaml")...)
		ref := netRef("Ingress", "web")
		ing := model.Criterion{ID: "ing", Kind: model.CriterionHTTP, Target: ref, Points: 3, Timeout: 90 * time.Second,
			Args: map[string]any{"source": netSource("frontend"), "host": "web.jk.test", "body_marker": netWebMarker}}
		wrong := model.Criterion{ID: "wrong", Kind: model.CriterionHTTP, Target: ref, Points: 3, Timeout: 90 * time.Second,
			Args: map[string]any{"source": netSource("frontend"), "host": "web.jk.test", "body_marker": netOtherMarker}}
		res := grade(t, engine, ing, wrong)
		testutil.Equal(t, res["ing"].Status, "PASS")
		testutil.Equal(t, res["wrong"].Status, "FAIL")
		unknownHost := model.Criterion{ID: "nohost", Kind: model.CriterionHTTP, Target: ref, Points: 3, Timeout: 90 * time.Second,
			Args: map[string]any{"source": netSource("frontend"), "host": "nobody.jk.test", "body_marker": netWebMarker}}
		res = grade(t, engine, unknownHost)
		testutil.Equal(t, res["nohost"].Status, "FAIL")
	})

	t.Run("ProbeImageMissingIsError", func(t *testing.T) {
		// A node without the probe image cannot run the probe: ERROR, not a
		// learner FAIL, and nothing is pulled.
		missing := newEngine("docker.io/library/busybox:1.35.0")
		res := grade(t, missing, httpC, policy)
		testutil.Equal(t, res["http"].Status, "ERROR")
		testutil.Equal(t, res["policy"].Status, "ERROR")
		if !strings.Contains(res["http"].Reason, "preloaded") {
			t.Fatalf("reason %q", res["http"].Reason)
		}
	})

	t.Run("CancelledGradingLeavesNoProbePods", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := engine.Evaluate(cctx, grader.Target{Identity: id, Problem: model.FrozenProblem{
				Definition: model.Problem{ID: "net-1", Namespace: netNamespace, Criteria: []model.Criterion{policy}}, Namespace: netNamespace}})
			done <- err
		}()
		waitUntil(t, "a probe pod to exist", 30*time.Second, func() bool { return len(probePods(t, cs)) > 0 })
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Evaluate = %v, want a cancellation", err)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("Evaluate did not return after cancellation")
		}
		testutil.Equal(t, len(probePods(t, cs)), 0)
	})

	t.Run("DNSFailure", func(t *testing.T) {
		// Cluster DNS is down: no source can resolve web, so neither an
		// allow nor a deny can be judged. Never a PASS.
		scale := func(n int32) {
			sc, err := cs.AppsV1().Deployments("kube-system").GetScale(ctx, "coredns", metav1.GetOptions{})
			testutil.NoError(t, err)
			sc.Spec.Replicas = n
			_, err = cs.AppsV1().Deployments("kube-system").UpdateScale(ctx, "coredns", sc, metav1.UpdateOptions{})
			testutil.NoError(t, err)
		}
		scale(0)
		defer func() {
			scale(2)
			waitUntil(t, "coredns to return", 120*time.Second, func() bool { return readyEndpoints(t, cs, "kube-system", "kube-dns") >= 1 })
		}()
		// Terminating CoreDNS pods keep answering during their lameduck
		// period (kube-proxy still routes to terminating endpoints), so wait
		// until the pods are really gone.
		waitUntil(t, "coredns to be gone", 90*time.Second, func() bool {
			pods, err := cs.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{LabelSelector: "k8s-app=kube-dns"})
			testutil.NoError(t, err)
			return len(pods.Items) == 0 && readyEndpoints(t, cs, "kube-system", "kube-dns") == 0
		})
		res := grade(t, engine, policy, httpC, tcpC)
		for _, cid := range []string{"policy", "http", "tcp"} {
			testutil.Equal(t, res[cid].Status == "PASS", false)
			testutil.Equal(t, res[cid].Status, "ERROR")
			if !strings.Contains(res[cid].Reason, "resolve") {
				t.Fatalf("%s: reason %q", cid, res[cid].Reason)
			}
		}
	})
}
