package grader_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// deployment builds a live-looking Deployment. gen/observed are
// metadata.generation and status.observedGeneration; the status counters are
// replicas, updated, ready, available.
func deployment(t testing.TB, replicas, gen, observed int, status [4]int, extraSpec string) map[string]any {
	t.Helper()
	spec := fmt.Sprintf(`"replicas":%d`, replicas)
	if replicas < 0 { // omitted: the API defaults it to 1
		spec = `"selector":{}`
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

func runReady(t *testing.T, r *fakeReader, clk *fakeClock, c model.Criterion) (model.CheckResult, error) {
	t.Helper()
	return grader.NewReadyChecker(r, grader.WithClock(clk)).Check(context.Background(), target(c), c)
}

func TestObservedGeneration(t *testing.T) {
	t.Run("stale status is not ready even when counters look ready", func(t *testing.T) {
		// generation 4 was just applied; the controller still reports the
		// old, fully ready generation 3.
		stale := deployment(t, 3, 4, 3, [4]int{3, 3, 3, 3}, "")
		clk := newClock()
		res, err := runReady(t, newReader().put(deployRef, stale), clk, readyCrit(5*time.Second, nil))
		testutil.NoError(t, err)
		testutil.Equal(t, res.Status, "FAIL")
		if !strings.Contains(res.Reason, "observedGeneration") {
			t.Fatalf("reason must explain the stale status: %q", res.Reason)
		}
		testutil.Equal(t, res.Evidence.Generation, int64(4))
		act := res.Evidence.Actual.(map[string]any)
		testutil.Equal(t, act["observedGeneration"], int64(3))
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
			res, err := runReady(t, newReader().put(deployRef, tc.d), clk, readyCrit(3*time.Second, tc.args))
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
	t.Run("waits the whole timeout then FAILs", func(t *testing.T) {
		clk := newClock()
		r := newReader().put(deployRef, notReady)
		res, err := runReady(t, r, clk, readyCrit(8*time.Second, nil))
		testutil.NoError(t, err)
		testutil.Equal(t, res.Status, "FAIL")
		if clk.slept() < 8*time.Second || clk.slept() > 9*time.Second {
			t.Fatalf("waited %v on the fake clock, want 8s", clk.slept())
		}
		if !strings.Contains(res.Reason, "8s") {
			t.Fatalf("reason must state the limit: %q", res.Reason)
		}
	})
	t.Run("default limit is 30s", func(t *testing.T) {
		clk := newClock()
		res, err := runReady(t, newReader().put(deployRef, notReady), clk, readyCrit(0, nil))
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
		res, err := runReady(t, newReader().put(deployRef, d), clk, readyCrit(30*time.Second, nil))
		testutil.NoError(t, err)
		testutil.Equal(t, res.Status, "FAIL")
		testutil.Equal(t, clk.slept(), time.Duration(0))
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
}

func TestReadyOtherWorkloads(t *testing.T) {
	sts := refOf("apps/v1", "StatefulSet", "jk-p1", "db")
	ds := refOf("apps/v1", "DaemonSet", "jk-p1", "agent")
	rs := refOf("apps/v1", "ReplicaSet", "jk-p1", "rs")
	pod := refOf("v1", "Pod", "jk-p1", "p")
	mk := func(ref model.ObjectRef, doc string) *fakeReader {
		return newReader().put(ref, obj(t, doc))
	}
	cases := []struct {
		name string
		ref  model.ObjectRef
		doc  string
		want string
	}{
		{"statefulset ready", sts, `{"apiVersion":"apps/v1","kind":"StatefulSet","metadata":{"name":"db","namespace":"jk-p1","uid":"u","generation":2},
			"spec":{"replicas":2},"status":{"observedGeneration":2,"replicas":2,"readyReplicas":2,"updatedReplicas":2,"availableReplicas":2}}`, "PASS"},
		{"statefulset one ready", sts, `{"apiVersion":"apps/v1","kind":"StatefulSet","metadata":{"name":"db","namespace":"jk-p1","uid":"u","generation":2},
			"spec":{"replicas":2},"status":{"observedGeneration":2,"replicas":2,"readyReplicas":1,"updatedReplicas":2,"availableReplicas":1}}`, "FAIL"},
		{"statefulset stale", sts, `{"apiVersion":"apps/v1","kind":"StatefulSet","metadata":{"name":"db","namespace":"jk-p1","uid":"u","generation":3},
			"spec":{"replicas":2},"status":{"observedGeneration":2,"replicas":2,"readyReplicas":2,"updatedReplicas":2,"availableReplicas":2}}`, "FAIL"},
		{"daemonset ready", ds, `{"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"agent","namespace":"jk-p1","uid":"u","generation":1},
			"spec":{},"status":{"observedGeneration":1,"desiredNumberScheduled":1,"currentNumberScheduled":1,"updatedNumberScheduled":1,"numberReady":1,"numberAvailable":1}}`, "PASS"},
		{"daemonset none scheduled yet", ds, `{"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"agent","namespace":"jk-p1","uid":"u","generation":1},
			"spec":{},"status":{"observedGeneration":1,"desiredNumberScheduled":0}}`, "FAIL"},
		{"daemonset not ready", ds, `{"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"agent","namespace":"jk-p1","uid":"u","generation":1},
			"spec":{},"status":{"observedGeneration":1,"desiredNumberScheduled":1,"updatedNumberScheduled":1,"numberReady":0,"numberAvailable":0}}`, "FAIL"},
		{"replicaset ready", rs, `{"apiVersion":"apps/v1","kind":"ReplicaSet","metadata":{"name":"rs","namespace":"jk-p1","uid":"u","generation":1},
			"spec":{"replicas":2},"status":{"observedGeneration":1,"replicas":2,"readyReplicas":2,"availableReplicas":2}}`, "PASS"},
		{"pod ready", pod, `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p","namespace":"jk-p1","uid":"u"},
			"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}`, "PASS"},
		{"pod not ready", pod, `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p","namespace":"jk-p1","uid":"u"},
			"status":{"phase":"Running","conditions":[{"type":"Ready","status":"False"}]}}`, "FAIL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := crit("ready", model.CriterionReady, 1, tc.ref, nil)
			c.Timeout = 2 * time.Second
			res, err := grader.NewReadyChecker(mk(tc.ref, tc.doc), grader.WithClock(newClock())).Check(context.Background(), target(c), c)
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
