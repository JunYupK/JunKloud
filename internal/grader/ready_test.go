package grader_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// deployment builds a live-looking Deployment (uid-1, selector app=web).
// gen/observed are metadata.generation and status.observedGeneration; the
// status counters are replicas, updated, ready, available.
func deployment(t testing.TB, replicas, gen, observed int, status [4]int, extraSpec string) map[string]any {
	t.Helper()
	spec := `"selector":{"matchLabels":{"app":"web"}}`
	if replicas >= 0 { // negative: omitted, the API defaults it to 1
		spec += fmt.Sprintf(`,"replicas":%d`, replicas)
	}
	if extraSpec != "" {
		spec += "," + extraSpec
	}
	return obj(t, fmt.Sprintf(`{"apiVersion":"apps/v1","kind":"Deployment",
	  "metadata":{"name":"web","namespace":"jk-p1","uid":"uid-1","generation":%d},
	  "spec":{%s},
	  "status":{"observedGeneration":%d,"replicas":%d,"updatedReplicas":%d,"readyReplicas":%d,"availableReplicas":%d}}`,
		gen, spec, observed, status[0], status[1], status[2], status[3]))
}

func readyCrit(timeout time.Duration, args map[string]any) model.Criterion {
	c := crit("ready", model.CriterionReady, 4, deployRef, args)
	c.Timeout = timeout
	return c
}

// withPods serves the Deployment's ReplicaSet and the given pods (owned by
// that ReplicaSet) for the cause analysis.
func withPods(t testing.TB, r *fakeReader, pods ...map[string]any) *fakeReader {
	t.Helper()
	rs := obj(t, `{"apiVersion":"apps/v1","kind":"ReplicaSet","metadata":{"name":"web-1","namespace":"jk-p1","uid":"rs-1",
	  "ownerReferences":[{"kind":"Deployment","name":"web","uid":"uid-1","controller":true}]}}`)
	other := obj(t, `{"apiVersion":"apps/v1","kind":"ReplicaSet","metadata":{"name":"stranger","namespace":"jk-p1","uid":"rs-other",
	  "ownerReferences":[{"kind":"Deployment","name":"stranger","uid":"uid-other","controller":true}]}}`)
	return r.putList("ReplicaSet", "jk-p1", rs, other).putList("Pod", "jk-p1", pods...)
}

// pod builds a pod owned by the Deployment's ReplicaSet; status is the JSON
// of its status object, spec the containers' images by name.
func pod(t testing.TB, name, status string, images ...string) map[string]any {
	t.Helper()
	var containers []string
	for i := 0; i+1 < len(images); i += 2 {
		containers = append(containers, fmt.Sprintf(`{"name":%q,"image":%q}`, images[i], images[i+1]))
	}
	return obj(t, fmt.Sprintf(`{"apiVersion":"v1","kind":"Pod",
	  "metadata":{"name":%q,"namespace":"jk-p1","uid":"pod-%s","ownerReferences":[{"kind":"ReplicaSet","name":"web-1","uid":"rs-1","controller":true}]},
	  "spec":{"containers":[%s]},"status":%s}`, name, name, strings.Join(containers, ","), status))
}

const crashStatus = `{"phase":"Running","conditions":[{"type":"Ready","status":"False"}],
  "containerStatuses":[{"name":"web","ready":false,"state":{"waiting":{"reason":"CrashLoopBackOff"}},"lastState":{"terminated":{"exitCode":1}}}]}`

func crashPod(t testing.TB) map[string]any {
	return pod(t, "web-1-a", crashStatus, "web", "nginx:1.27.0")
}

func runReady(t *testing.T, r *fakeReader, clk *fakeClock, c model.Criterion, opts ...grader.Option) (model.CheckResult, error) {
	t.Helper()
	return grader.NewReadyChecker(r, append([]grader.Option{grader.WithClock(clk)}, opts...)...).Check(context.Background(), target(c), c)
}

func TestObservedGeneration(t *testing.T) {
	t.Run("stale status is not ready even when counters look ready", func(t *testing.T) {
		// generation 4 was just applied; the controller still reports the
		// old, fully ready generation 3. A controller that does not catch up
		// is cluster trouble: ERROR, never a guessed FAIL.
		stale := deployment(t, 3, 4, 3, [4]int{3, 3, 3, 3}, "")
		clk := newClock()
		_, err := runReady(t, newReader().put(deployRef, stale), clk, readyCrit(5*time.Second, nil))
		testutil.ErrorIs(t, err, grader.ErrInsufficientEvidence)
		if !strings.Contains(err.Error(), "observedGeneration") {
			t.Fatalf("error must explain the stale status: %v", err)
		}
		if clk.slept() < 5*time.Second {
			t.Fatalf("must wait out the limit first, waited %v", clk.slept())
		}
	})
	t.Run("passes once the controller observes the generation", func(t *testing.T) {
		stale := deployment(t, 3, 4, 3, [4]int{3, 3, 3, 3}, "")
		rolling := deployment(t, 3, 4, 4, [4]int{4, 2, 3, 3}, "")
		done := deployment(t, 3, 4, 4, [4]int{3, 3, 3, 3}, "")
		clk := newClock()
		r := newReader().put(deployRef, stale, rolling, done)
		res, err := runReady(t, r, clk, readyCrit(30*time.Second, nil))
		testutil.NoError(t, err)
		testutil.Equal(t, res.Status, "PASS")
		testutil.Equal(t, res.Evidence.Generation, int64(4))
		if clk.slept() >= 30*time.Second {
			t.Fatalf("should not wait out the timeout: %v", clk.slept())
		}
	})
	t.Run("observedGeneration ahead of generation is fine", func(t *testing.T) {
		ok := deployment(t, 2, 1, 2, [4]int{2, 2, 2, 2}, "")
		res, err := runReady(t, newReader().put(deployRef, ok), newClock(), readyCrit(time.Second, nil))
		testutil.NoError(t, err)
		testutil.Equal(t, res.Status, "PASS")
	})
}

func TestReadyDeployment(t *testing.T) {
	cases := []struct {
		name string
		d    map[string]any
		args map[string]any
		want string
	}{
		{"all replicas ready", deployment(t, 3, 2, 2, [4]int{3, 3, 3, 3}, ""), nil, "PASS"},
		{"defaulted replicas (omitted means 1)", deployment(t, -1, 1, 1, [4]int{1, 1, 1, 1}, ""), nil, "PASS"},
		{"scaled to zero", deployment(t, 0, 2, 2, [4]int{0, 0, 0, 0}, ""), nil, "PASS"},
		{"not enough ready", deployment(t, 3, 2, 2, [4]int{3, 3, 2, 2}, ""), nil, "FAIL"},
		{"ready but not available", deployment(t, 3, 2, 2, [4]int{3, 3, 3, 2}, ""), nil, "FAIL"},
		{"old replicas still around", deployment(t, 3, 2, 2, [4]int{4, 3, 3, 3}, ""), nil, "FAIL"},
		{"not all updated", deployment(t, 3, 2, 2, [4]int{3, 2, 3, 3}, ""), nil, "FAIL"},
		{"declared rollout target matches", deployment(t, 3, 2, 2, [4]int{3, 3, 3, 3}, ""), map[string]any{"replicas": 3}, "PASS"},
		{"declared rollout target differs", deployment(t, 2, 2, 2, [4]int{2, 2, 2, 2}, ""), map[string]any{"replicas": 3}, "FAIL"},
		{"paused rollout", deployment(t, 3, 2, 2, [4]int{3, 3, 3, 3}, `"paused":true`), nil, "FAIL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clk := newClock()
			r := withPods(t, newReader().put(deployRef, tc.d), crashPod(t))
			res, err := runReady(t, r, clk, readyCrit(3*time.Second, tc.args))
			testutil.NoError(t, err)
			if res.Status != tc.want {
				t.Fatalf("status %s, want %s (reason %q)", res.Status, tc.want, res.Reason)
			}
			if tc.want == "FAIL" && res.Reason == "" {
				t.Fatal("FAIL needs a reason")
			}
		})
	}
}

func TestReadyTimeoutAndTerminalStates(t *testing.T) {
	notReady := deployment(t, 1, 1, 1, [4]int{1, 1, 0, 0}, "")
	t.Run("waits the whole timeout then FAILs with the proven cause", func(t *testing.T) {
		clk := newClock()
		r := withPods(t, newReader().put(deployRef, notReady), crashPod(t))
		res, err := runReady(t, r, clk, readyCrit(8*time.Second, nil))
		testutil.NoError(t, err)
		testutil.Equal(t, res.Status, "FAIL")
		if clk.slept() < 8*time.Second || clk.slept() > 9*time.Second {
			t.Fatalf("waited %v on the fake clock, want 8s", clk.slept())
		}
		for _, want := range []string{"8s", "availableReplicas", "CrashLoopBackOff"} {
			if !strings.Contains(res.Reason, want) {
				t.Fatalf("reason lacks %q: %q", want, res.Reason)
			}
		}
		act := res.Evidence.Actual.(map[string]any)
		if pods, _ := act["pods"].([]any); len(pods) != 1 {
			t.Fatalf("evidence should list the failing pod: %v", act["pods"])
		}
		// Only pods of this Deployment's ReplicaSets are considered, selected by its selector.
		testutil.Equal(t, r.listQuery[0].LabelSelector, "app=web")
	})
	t.Run("default limit is 30s", func(t *testing.T) {
		clk := newClock()
		r := withPods(t, newReader().put(deployRef, notReady), crashPod(t))
		res, err := runReady(t, r, clk, readyCrit(0, nil))
		testutil.NoError(t, err)
		testutil.Equal(t, res.Status, "FAIL")
		if clk.slept() < 30*time.Second || clk.slept() > 31*time.Second {
			t.Fatalf("waited %v, want 30s", clk.slept())
		}
	})
	t.Run("progress deadline exceeded does not wait", func(t *testing.T) {
		d := deployment(t, 1, 1, 1, [4]int{1, 1, 0, 0}, "")
		d["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Progressing", "status": "False", "reason": "ProgressDeadlineExceeded"}}
		clk := newClock()
		res, err := runReady(t, withPods(t, newReader().put(deployRef, d), crashPod(t)), clk, readyCrit(30*time.Second, nil))
		testutil.NoError(t, err)
		testutil.Equal(t, res.Status, "FAIL")
		testutil.Equal(t, clk.slept(), time.Duration(0))
		// The controller's verdict is not trusted blindly: if the pods say
		// the cluster is at fault, it is ERROR.
		stuck := pod(t, "web-1-b", unschedulable("0/1 nodes are available: 1 Insufficient cpu."), "web", "nginx:1.27.0")
		_, err = runReady(t, withPods(t, newReader().put(deployRef, d), stuck), newClock(), readyCrit(30*time.Second, nil))
		testutil.ErrorIs(t, err, grader.ErrInsufficientEvidence)
	})
	t.Run("missing workload fails immediately", func(t *testing.T) {
		clk := newClock()
		res, err := runReady(t, newReader(), clk, readyCrit(30*time.Second, nil))
		testutil.NoError(t, err)
		testutil.Equal(t, res.Status, "FAIL")
		testutil.Equal(t, clk.slept(), time.Duration(0))
	})
	t.Run("api outage is an error", func(t *testing.T) {
		_, err := runReady(t, newReader().put(deployRef, fmt.Errorf("etcdserver: request timed out")), newClock(), readyCrit(time.Second, nil))
		if err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("pod listing failure is an error", func(t *testing.T) {
		r := withPods(t, newReader().put(deployRef, notReady), crashPod(t))
		r.listErr = errors.New("apiserver busy")
		_, err := runReady(t, r, newClock(), readyCrit(2*time.Second, nil))
		testutil.ErrorIs(t, err, r.listErr)
	})
}

func unschedulable(msg string) string {
	return fmt.Sprintf(`{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable","message":%q}]}`, msg)
}

func waiting(reason, msg string) string {
	return fmt.Sprintf(`{"phase":"Pending","conditions":[{"type":"Ready","status":"False"}],
	  "containerStatuses":[{"name":"web","ready":false,"state":{"waiting":{"reason":%q,"message":%q}}}]}`, reason, msg)
}

// TestReadyFaultClassification is spec 8.1: a rollout that does not finish is
// FAIL only when the pods prove the learner's doing; environment trouble and
// unprovable causes are ERROR.
func TestReadyFaultClassification(t *testing.T) {
	const preloaded = "docker.io/library/busybox:1.37.0"
	notReady := deployment(t, 1, 1, 1, [4]int{1, 1, 0, 0}, "")
	cases := []struct {
		name   string
		pods   []map[string]any
		want   string // FAIL or ERROR
		reason string // substring of the FAIL reason / ERROR text
	}{
		// Learner's doing: FAIL with the reason as evidence.
		{"pull failure of the learner's own image", []map[string]any{pod(t, "p", waiting("ErrImagePull", "manifest unknown"), "web", "nginx:no-such-tag")}, "FAIL", "ErrImagePull"},
		{"image pull back-off of the learner's own image", []map[string]any{pod(t, "p", waiting("ImagePullBackOff", "pull access denied"), "web", "ghcr.io/x/y:1")}, "FAIL", "ghcr.io/x/y:1"},
		{"invalid image name", []map[string]any{pod(t, "p", waiting("InvalidImageName", "couldn't parse image reference"), "web", "NGINX::bad")}, "FAIL", "InvalidImageName"},
		{"crash loop", []map[string]any{crashPod(t)}, "FAIL", "CrashLoopBackOff"},
		{"missing config reference", []map[string]any{pod(t, "p", waiting("CreateContainerConfigError", `secret "nope" not found`), "web", "nginx:1")}, "FAIL", "CreateContainerConfigError"},
		{"container exits non-zero", []map[string]any{pod(t, "p", `{"phase":"Running","containerStatuses":[{"name":"web","ready":false,"state":{"terminated":{"exitCode":2,"reason":"Error"}}}]}`, "web", "nginx:1")}, "FAIL", "exit code 2"},
		{"readiness probe failing", []map[string]any{pod(t, "p", `{"phase":"Running","conditions":[{"type":"Ready","status":"False"}],"containerStatuses":[{"name":"web","ready":false,"state":{"running":{}}}]}`, "web", "nginx:1")}, "FAIL", "readiness probe"},
		{"node selector nothing matches", []map[string]any{pod(t, "p", unschedulable("0/1 nodes are available: 1 node(s) didn't match Pod's node affinity/selector."), "web", "nginx:1")}, "FAIL", "constraints"},
		// A learner fault wins over a simultaneous environment problem.
		{"crash loop beside an unschedulable replica", []map[string]any{crashPod(t), pod(t, "q", unschedulable("0/1 nodes are available: 1 Insufficient cpu."), "web", "nginx:1")}, "FAIL", "CrashLoopBackOff"},
		// Environment trouble: ERROR.
		{"insufficient cpu", []map[string]any{pod(t, "p", unschedulable("0/1 nodes are available: 1 Insufficient cpu. preemption: 0/1 nodes are available"), "web", "nginx:1")}, "ERROR", "Insufficient cpu"},
		{"insufficient memory", []map[string]any{pod(t, "p", unschedulable("0/2 nodes are available: 2 Insufficient memory."), "web", "nginx:1")}, "ERROR", "Insufficient memory"},
		{"node not ready", []map[string]any{pod(t, "p", unschedulable("0/1 nodes are available: 1 node(s) had untolerated taint {node.kubernetes.io/not-ready: }."), "web", "nginx:1")}, "ERROR", "not-ready"},
		{"pull failure of an image the environment provides", []map[string]any{pod(t, "p", waiting("ErrImagePull", "connection refused"), "web", "busybox:1.37.0")}, "ERROR", "environment itself provides"},
		{"back-off of an image the environment provides", []map[string]any{pod(t, "p", waiting("ImagePullBackOff", "x"), "web", preloaded)}, "ERROR", "image preparation failed"},
		{"evicted", []map[string]any{pod(t, "p", `{"phase":"Failed","reason":"Evicted","message":"The node was low on resource: memory."}`, "web", "nginx:1")}, "ERROR", "evicted"},
		// Cause not provable: ERROR.
		{"no pods were created", nil, "ERROR", "created no pods"},
		{"still creating containers", []map[string]any{pod(t, "p", waiting("ContainerCreating", ""), "web", "nginx:1")}, "ERROR", "ContainerCreating"},
		{"pending without any explanation", []map[string]any{pod(t, "p", `{"phase":"Pending"}`, "web", "nginx:1")}, "ERROR", "without a container state"},
		{"every pod ready yet counters short", []map[string]any{pod(t, "p", `{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}`, "web", "nginx:1")}, "ERROR", "every pod is ready"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := withPods(t, newReader().put(deployRef, notReady), tc.pods...)
			res, err := runReady(t, r, newClock(), readyCrit(2*time.Second, nil), grader.WithPreloadedImages("docker.io/library/busybox:1.37.0"))
			switch tc.want {
			case "FAIL":
				testutil.NoError(t, err)
				testutil.Equal(t, res.Status, "FAIL")
				if !strings.Contains(res.Reason, tc.reason) {
					t.Fatalf("reason lacks %q: %q", tc.reason, res.Reason)
				}
			default:
				testutil.ErrorIs(t, err, grader.ErrInsufficientEvidence)
				if !strings.Contains(err.Error(), tc.reason) {
					t.Fatalf("error lacks %q: %v", tc.reason, err)
				}
			}
		})
	}
	t.Run("an image only counts as provided after Docker defaulting", func(t *testing.T) {
		p := pod(t, "p", waiting("ErrImagePull", "manifest unknown"), "web", "busybox:1.37.0")
		r := withPods(t, newReader().put(deployRef, notReady), p)
		// Not in the provided set: the learner's own wrong image.
		res, err := runReady(t, r, newClock(), readyCrit(time.Second, nil), grader.WithPreloadedImages("alpine:3.20"))
		testutil.NoError(t, err)
		testutil.Equal(t, res.Status, "FAIL")
	})
	t.Run("terminating pods are ignored", func(t *testing.T) {
		old := crashPod(t)
		old["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-15T00:00:00Z"
		r := withPods(t, newReader().put(deployRef, notReady), old, pod(t, "q", unschedulable("0/1 nodes are available: 1 Insufficient cpu."), "web", "nginx:1"))
		_, err := runReady(t, r, newClock(), readyCrit(time.Second, nil))
		testutil.ErrorIs(t, err, grader.ErrInsufficientEvidence)
	})
	t.Run("engine grades the unproven cause ERROR and keeps the score pending", func(t *testing.T) {
		r := withPods(t, newReader().put(deployRef, notReady), pod(t, "p", unschedulable("0/1 nodes are available: 1 Insufficient cpu."), "web", "nginx:1"))
		e := grader.NewEngine(map[string]grader.Checker{model.CriterionReady: grader.NewReadyChecker(r, grader.WithClock(newClock()))}, grader.WithClock(newClock()))
		ev, err := e.Evaluate(context.Background(), target(readyCrit(time.Second, nil)))
		testutil.NoError(t, err)
		testutil.Equal(t, ev.Results[0].Status, "ERROR")
		testutil.Equal(t, ev.Score, (*int)(nil))
		if !strings.Contains(ev.Results[0].Reason, "Insufficient cpu") {
			t.Fatalf("reason: %q", ev.Results[0].Reason)
		}
	})
}

func TestReadyOtherWorkloads(t *testing.T) {
	sts := refOf("apps/v1", "StatefulSet", "jk-p1", "db")
	ds := refOf("apps/v1", "DaemonSet", "jk-p1", "agent")
	rs := refOf("apps/v1", "ReplicaSet", "jk-p1", "rs")
	podRef := refOf("v1", "Pod", "jk-p1", "p")
	sel := `"selector":{"matchLabels":{"app":"x"}}`
	owned := func(kind, uid string) map[string]any {
		return obj(t, fmt.Sprintf(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"x-0","namespace":"jk-p1","uid":"px",
		  "ownerReferences":[{"kind":%q,"name":"n","uid":%q,"controller":true}]},"spec":{"containers":[{"name":"web","image":"nginx:1"}]},"status":%s}`, kind, uid, crashStatus))
	}
	cases := []struct {
		name string
		ref  model.ObjectRef
		doc  string
		pods []map[string]any
		want string
	}{
		{"statefulset ready", sts, `{"apiVersion":"apps/v1","kind":"StatefulSet","metadata":{"name":"db","namespace":"jk-p1","uid":"u","generation":2},
			"spec":{"replicas":2,` + sel + `},"status":{"observedGeneration":2,"replicas":2,"readyReplicas":2,"updatedReplicas":2,"availableReplicas":2}}`, nil, "PASS"},
		{"statefulset one ready, crashing pod", sts, `{"apiVersion":"apps/v1","kind":"StatefulSet","metadata":{"name":"db","namespace":"jk-p1","uid":"u","generation":2},
			"spec":{"replicas":2,` + sel + `},"status":{"observedGeneration":2,"replicas":2,"readyReplicas":1,"updatedReplicas":2,"availableReplicas":1}}`, []map[string]any{owned("StatefulSet", "u")}, "FAIL"},
		{"statefulset stale controller", sts, `{"apiVersion":"apps/v1","kind":"StatefulSet","metadata":{"name":"db","namespace":"jk-p1","uid":"u","generation":3},
			"spec":{"replicas":2,` + sel + `},"status":{"observedGeneration":2,"replicas":2,"readyReplicas":2,"updatedReplicas":2,"availableReplicas":2}}`, nil, "ERROR"},
		{"daemonset ready", ds, `{"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"agent","namespace":"jk-p1","uid":"u","generation":1},
			"spec":{` + sel + `},"status":{"observedGeneration":1,"desiredNumberScheduled":1,"currentNumberScheduled":1,"updatedNumberScheduled":1,"numberReady":1,"numberAvailable":1}}`, nil, "PASS"},
		{"daemonset none scheduled yet", ds, `{"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"agent","namespace":"jk-p1","uid":"u","generation":1},
			"spec":{` + sel + `},"status":{"observedGeneration":1,"desiredNumberScheduled":0}}`, nil, "ERROR"},
		{"daemonset not ready, crashing pod", ds, `{"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"agent","namespace":"jk-p1","uid":"u","generation":1},
			"spec":{` + sel + `},"status":{"observedGeneration":1,"desiredNumberScheduled":1,"updatedNumberScheduled":1,"numberReady":0,"numberAvailable":0}}`, []map[string]any{owned("DaemonSet", "u")}, "FAIL"},
		{"replicaset ready", rs, `{"apiVersion":"apps/v1","kind":"ReplicaSet","metadata":{"name":"rs","namespace":"jk-p1","uid":"u","generation":1},
			"spec":{"replicas":2,` + sel + `},"status":{"observedGeneration":1,"replicas":2,"readyReplicas":2,"availableReplicas":2}}`, nil, "PASS"},
		{"pod ready", podRef, `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p","namespace":"jk-p1","uid":"u"},
			"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}`, nil, "PASS"},
		{"pod running but not ready", podRef, `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p","namespace":"jk-p1","uid":"u"},
			"spec":{"containers":[{"name":"web","image":"nginx:1"}]},
			"status":{"phase":"Running","conditions":[{"type":"Ready","status":"False"}],"containerStatuses":[{"name":"web","ready":false,"state":{"running":{}}}]}}`, nil, "FAIL"},
		{"pod pending for lack of cpu", podRef, `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p","namespace":"jk-p1","uid":"u"},
			"status":` + unschedulable("0/1 nodes are available: 1 Insufficient cpu.") + `}`, nil, "ERROR"},
		{"pod ran to completion", podRef, `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p","namespace":"jk-p1","uid":"u"},
			"status":{"phase":"Succeeded"}}`, nil, "FAIL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newReader().put(tc.ref, obj(t, tc.doc)).putList("Pod", "jk-p1", tc.pods...)
			c := crit("ready", model.CriterionReady, 1, tc.ref, nil)
			c.Timeout = 2 * time.Second
			res, err := grader.NewReadyChecker(r, grader.WithClock(newClock())).Check(context.Background(), target(c), c)
			if tc.want == "ERROR" {
				testutil.ErrorIs(t, err, grader.ErrInsufficientEvidence)
				return
			}
			testutil.NoError(t, err)
			if res.Status != tc.want {
				t.Fatalf("status %s, want %s (%s)", res.Status, tc.want, res.Reason)
			}
		})
	}
	t.Run("unsupported kind is an author error", func(t *testing.T) {
		cm := refOf("v1", "ConfigMap", "jk-p1", "c")
		c := crit("ready", model.CriterionReady, 1, cm, nil)
		_, err := grader.NewReadyChecker(newReader()).Check(context.Background(), target(c), c)
		testutil.ErrorIs(t, err, grader.ErrInvalidCriterion)
	})
	t.Run("replicas arg on daemonset is an author error", func(t *testing.T) {
		c := crit("ready", model.CriterionReady, 1, ds, map[string]any{"replicas": 1})
		_, err := grader.NewReadyChecker(newReader()).Check(context.Background(), target(c), c)
		testutil.ErrorIs(t, err, grader.ErrInvalidCriterion)
	})
}

func TestReadyRejectsBadArgs(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"unknown key":       {"replica": 1},
		"negative replicas": {"replicas": -1},
		"string replicas":   {"replicas": "three"},
		"fractional":        {"replicas": 1.5},
	} {
		t.Run(name, func(t *testing.T) {
			c := readyCrit(time.Second, args)
			r := newReader().put(deployRef, deployment(t, 1, 1, 1, [4]int{1, 1, 1, 1}, ""))
			_, err := grader.NewReadyChecker(r).Check(context.Background(), target(c), c)
			testutil.ErrorIs(t, err, grader.ErrInvalidCriterion)
			testutil.Equal(t, r.callsFor(deployRef), 0)
		})
	}
}

func TestReadyTargetReplacedDuringGrading(t *testing.T) {
	a := deployment(t, 1, 1, 1, [4]int{1, 1, 0, 0}, "")
	b := deployment(t, 1, 1, 1, [4]int{1, 1, 1, 1}, "")
	b["metadata"].(map[string]any)["uid"] = "uid-other"
	_, err := runReady(t, newReader().put(deployRef, a, b), newClock(), readyCrit(10*time.Second, nil))
	testutil.ErrorIs(t, err, grader.ErrTargetChanged)

	// A PASS is re-confirmed: the object that became ready must still be
	// the same one on the final read.
	ok := deployment(t, 1, 1, 1, [4]int{1, 1, 1, 1}, "")
	swapped := deployment(t, 1, 1, 1, [4]int{1, 1, 1, 1}, "")
	swapped["metadata"].(map[string]any)["uid"] = "uid-other"
	_, err = runReady(t, newReader().put(deployRef, ok, swapped), newClock(), readyCrit(10*time.Second, nil))
	testutil.ErrorIs(t, err, grader.ErrTargetChanged)
}

// Realistic containerd/kubelet pull errors. Only an answer that proves the
// reference itself wrong is the learner's FAIL; transport, registry and
// rate-limit trouble leaves the image unproven (ERROR), and so does anything
// unrecognised.
const (
	msgNotFound = `rpc error: code = NotFound desc = failed to pull and unpack image "docker.io/library/busybox:jk-does-not-exist": failed to resolve reference "docker.io/library/busybox:jk-does-not-exist": docker.io/library/busybox:jk-does-not-exist: not found`
	msgManifest = `rpc error: code = Unknown desc = failed to pull and unpack image "registry.example.com/team/app:9": failed to resolve reference "registry.example.com/team/app:9": failed to authorize: failed to fetch anonymous token: unexpected status: 404 manifest unknown`
	msgDenied   = `rpc error: code = Unknown desc = failed to pull and unpack image "docker.io/library/nope-xyz:1": failed to resolve reference "docker.io/library/nope-xyz:1": pull access denied, repository does not exist or may require authorization: server message: insufficient_scope: authorization failed`
	msgInvalid  = `Failed to apply default image tag "nginx::1": couldn't parse image reference "nginx::1": invalid reference format`
	msgNameUnk  = `Error response from daemon: pull access denied for foo, name unknown: repository name not known to registry`
	// Digest text that contains transport-looking numbers is not transport trouble.
	msgDigest = `failed to resolve reference "docker.io/library/app@sha256:8a4291f1e5039b5d6429ab1ec0f2cf3b5e502c9eed86a2d5ba0fd6c1e5035e3f": not found`

	msgDial    = `rpc error: code = Unknown desc = failed to pull and unpack image "docker.io/library/busybox:1.37.0": failed to resolve reference "docker.io/library/busybox:1.37.0": failed to do request: Head "https://registry-1.docker.io/v2/library/busybox/manifests/1.37.0": dial tcp 52.1.2.3:443: i/o timeout`
	msgProxy   = `failed to resolve reference "docker.io/library/busybox:jk-does-not-exist": failed to do request: Head "https://registry-1.docker.io/v2/library/busybox/manifests/jk-does-not-exist": proxyconnect tcp: dial tcp 127.0.0.1:45073: connect: connection refused`
	msgDNS     = `failed to resolve reference "ghcr.io/x/y:1": failed to do request: Head "https://ghcr.io/v2/x/y/manifests/1": dial tcp: lookup ghcr.io on 10.96.0.10:53: no such host`
	msgTLS     = `failed to resolve reference "registry.local/app:1": failed to do request: Head "https://registry.local/v2/app/manifests/1": tls: failed to verify certificate: x509: certificate signed by unknown authority`
	msgHandshk = `failed to do request: Head "https://registry-1.docker.io/v2/library/nginx/manifests/1.27.0": net/http: TLS handshake timeout`
	msgRate    = `rpc error: code = Unknown desc = failed to pull and unpack image "docker.io/library/nginx:1.27.0": failed to copy: httpReadSeeker: failed open: unexpected status code https://registry-1.docker.io/v2/library/nginx/manifests/sha256:abc: 429 Too Many Requests - Server message: toomanyrequests: You have reached your unauthenticated pull rate limit. https://www.docker.com/increase-rate-limit`
	msg503     = `failed to copy: httpReadSeeker: failed open: unexpected status code: 503 Service Unavailable`
	msgEOF     = `failed to pull and unpack image "nginx:1.27.0": failed to copy: read tcp 10.244.0.2:41522->104.16.1.1:443: unexpected EOF`
	msgCtx     = `rpc error: code = DeadlineExceeded desc = context deadline exceeded`
	// A "not found" that came with transport trouble (a proxy answering for the registry) proves nothing.
	msgMixed   = `failed to do request: Head "https://registry-1.docker.io/v2/library/x/manifests/1": proxyconnect tcp: dial tcp 10.0.0.1:3128: connect: no route to host (upstream: 404 not found)`
	msgNovel   = `something the kubelet has never said before`
	msgBackoff = `Back-off pulling image "nginx:1.27.0"`
)

func pullPod(t testing.TB, reason, msg string) map[string]any {
	return pod(t, "p", waiting(reason, msg), "web", "nginx:1.27.0")
}

func event(t testing.TB, podName, ts, msg string) map[string]any {
	return obj(t, fmt.Sprintf(`{"apiVersion":"v1","kind":"Event","metadata":{"name":"e-%s","namespace":"jk-p1"},
	  "involvedObject":{"kind":"Pod","name":%q},"reason":"Failed","lastTimestamp":%q,"message":%q}`, ts, podName, ts, msg))
}

func TestPullFailureAttribution(t *testing.T) {
	notReady := deployment(t, 1, 1, 1, [4]int{1, 1, 0, 0}, "")
	run := func(t *testing.T, p map[string]any, events []map[string]any, opts ...grader.Option) (model.CheckResult, error) {
		t.Helper()
		r := withPods(t, newReader().put(deployRef, notReady), p).putList("Event", "jk-p1", events...)
		return runReady(t, r, newClock(), readyCrit(2*time.Second, nil), opts...)
	}
	configured := grader.WithPreloadedImages("docker.io/library/busybox:1.37.0")

	wrongRef := map[string]string{"not found": msgNotFound, "manifest unknown": msgManifest, "repository does not exist": msgDenied,
		"invalid reference format": msgInvalid, "name unknown": msgNameUnk, "digest with 429 inside": msgDigest}
	for name, msg := range wrongRef {
		t.Run("FAIL on a registry answer: "+name, func(t *testing.T) {
			for _, reason := range []string{"ErrImagePull", "ImagePullBackOff"} {
				res, err := run(t, pullPod(t, reason, msg), nil, configured)
				testutil.NoError(t, err)
				testutil.Equal(t, res.Status, "FAIL")
				if !strings.Contains(res.Reason, "registry says the reference is wrong") {
					t.Fatalf("reason: %q", res.Reason)
				}
			}
		})
	}
	transport := map[string]string{"i/o timeout": msgDial, "proxyconnect refused": msgProxy, "dns": msgDNS, "tls x509": msgTLS,
		"tls handshake timeout": msgHandshk, "429 toomanyrequests": msgRate, "503": msg503, "unexpected EOF": msgEOF,
		"context deadline exceeded": msgCtx, "not found beside a transport error": msgMixed}
	for name, msg := range transport {
		t.Run("ERROR on transport trouble: "+name, func(t *testing.T) {
			_, err := run(t, pullPod(t, "ErrImagePull", msg), nil, configured)
			testutil.ErrorIs(t, err, grader.ErrInsufficientEvidence)
			if !strings.Contains(err.Error(), "not proven wrong") {
				t.Fatalf("error: %v", err)
			}
		})
	}
	t.Run("ERROR on an unrecognised message", func(t *testing.T) {
		_, err := run(t, pullPod(t, "ErrImagePull", msgNovel), nil, configured)
		testutil.ErrorIs(t, err, grader.ErrInsufficientEvidence)
		if !strings.Contains(err.Error(), "without a recognisable registry answer") {
			t.Fatalf("error: %v", err)
		}
	})
	t.Run("ERROR on a bare back-off without any registry answer", func(t *testing.T) {
		_, err := run(t, pullPod(t, "ImagePullBackOff", msgBackoff), nil, configured)
		testutil.ErrorIs(t, err, grader.ErrInsufficientEvidence)
	})

	// In back-off the container status carries no cause: the kubelet's
	// events do. The newest recognised message decides.
	t.Run("back-off uses the pull events: wrong reference", func(t *testing.T) {
		res, err := run(t, pullPod(t, "ImagePullBackOff", msgBackoff), []map[string]any{
			event(t, "p", "2026-10-15T10:00:01Z", "Pulling image \"nginx:1.27.0\""),
			event(t, "p", "2026-10-15T10:00:02Z", "Failed to pull image \"nginx:1.27.0\": "+msgNotFound),
			event(t, "other-pod", "2026-10-15T10:00:03Z", "Failed to pull image: "+msgDial),
		}, configured)
		testutil.NoError(t, err)
		testutil.Equal(t, res.Status, "FAIL")
	})
	t.Run("back-off uses the pull events: transport trouble", func(t *testing.T) {
		_, err := run(t, pullPod(t, "ImagePullBackOff", msgBackoff), []map[string]any{
			event(t, "p", "2026-10-15T10:00:02Z", "Failed to pull image \"nginx:1.27.0\": "+msgProxy),
		}, configured)
		testutil.ErrorIs(t, err, grader.ErrInsufficientEvidence)
	})
	t.Run("the newest pull event wins over an older one", func(t *testing.T) {
		_, err := run(t, pullPod(t, "ImagePullBackOff", msgBackoff), []map[string]any{
			event(t, "p", "2026-10-15T10:00:09Z", "Failed to pull image: "+msgDial), // newest, listed first
			event(t, "p", "2026-10-15T10:00:01Z", "Failed to pull image: "+msgNotFound),
		}, configured)
		testutil.ErrorIs(t, err, grader.ErrInsufficientEvidence)
	})
	t.Run("event listing failure is an error", func(t *testing.T) {
		r := withPods(t, newReader().put(deployRef, notReady), pullPod(t, "ImagePullBackOff", msgBackoff))
		r.listErr = errors.New("apiserver busy")
		_, err := runReady(t, r, newClock(), readyCrit(time.Second, nil), configured)
		testutil.ErrorIs(t, err, r.listErr)
	})
}

// Fail-closed: the grader does not know which images the environment itself
// provides unless the caller says so.
func TestPullFailureFailsClosedWithoutProvidedSet(t *testing.T) {
	notReady := deployment(t, 1, 1, 1, [4]int{1, 1, 0, 0}, "")
	run := func(t *testing.T, p map[string]any, opts ...grader.Option) (model.CheckResult, error) {
		r := withPods(t, newReader().put(deployRef, notReady), p)
		return runReady(t, r, newClock(), readyCrit(time.Second, nil), opts...)
	}
	t.Run("unconfigured: even a registry-proven wrong reference is ERROR", func(t *testing.T) {
		_, err := run(t, pullPod(t, "ErrImagePull", msgNotFound))
		testutil.ErrorIs(t, err, grader.ErrInsufficientEvidence)
		if !strings.Contains(err.Error(), "not configured") {
			t.Fatalf("error should say why: %v", err)
		}
		_, err = run(t, pullPod(t, "ImagePullBackOff", msgBackoff))
		testutil.ErrorIs(t, err, grader.ErrInsufficientEvidence)
	})
	t.Run("unconfigured: a syntactically invalid name is still the learner's", func(t *testing.T) {
		res, err := run(t, pod(t, "p", waiting("InvalidImageName", msgInvalid), "web", "nginx::1"))
		testutil.NoError(t, err)
		testutil.Equal(t, res.Status, "FAIL")
	})
	t.Run("configured but empty is an explicit statement", func(t *testing.T) {
		res, err := run(t, pullPod(t, "ErrImagePull", msgNotFound), grader.WithPreloadedImages())
		testutil.NoError(t, err)
		testutil.Equal(t, res.Status, "FAIL")
	})
	t.Run("an engine built from DefaultCheckers without the option never FAILs a pull", func(t *testing.T) {
		r := withPods(t, newReader().put(deployRef, notReady), pullPod(t, "ErrImagePull", msgNotFound))
		opts := []grader.Option{grader.WithClock(newClock())}
		e := grader.NewEngine(grader.DefaultCheckers(r, opts...), opts...)
		ev, err := e.Evaluate(context.Background(), target(readyCrit(time.Second, nil)))
		testutil.NoError(t, err)
		testutil.Equal(t, ev.Results[0].Status, "ERROR")
		testutil.Equal(t, ev.Score, (*int)(nil))
	})
}
