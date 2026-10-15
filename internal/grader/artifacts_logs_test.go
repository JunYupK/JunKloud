package grader_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

var podRef = refOf("v1", "Pod", "jk-p1", "app")

func podDoc(t testing.TB, name, ownerUID, phase string, containers ...string) map[string]any {
	t.Helper()
	var cs []string
	for _, c := range containers {
		cs = append(cs, fmt.Sprintf(`{"name":%q,"image":"busybox:1.37.0"}`, c))
	}
	owner := ""
	if ownerUID != "" {
		owner = fmt.Sprintf(`,"ownerReferences":[{"uid":%q,"controller":true}]`, ownerUID)
	}
	return obj(t, fmt.Sprintf(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":%q,"namespace":"jk-p1","uid":"uid-%s","labels":{"app":"web"}%s},
	  "spec":{"containers":[%s]},"status":{"phase":%q}}`, name, name, owner, strings.Join(cs, ","), phase))
}

func logsCrit(ref model.ObjectRef, args map[string]any) model.Criterion {
	return model.Criterion{ID: "c", Kind: model.CriterionLogs, Target: ref, Points: 2, Args: args}
}

func TestLogsOnPod(t *testing.T) {
	const log = "starting\nlistening on :8080\n{\"level\":\"info\",\"msg\":\"ready\",\"port\":8080}\n{\"level\":\"error\",\"msg\":\"boom\"}\n"
	cases := []struct {
		name   string
		args   map[string]any
		want   string
		reason []string
	}{
		{name: "contains", args: map[string]any{"contains": []any{"listening on :8080"}}, want: "PASS"},
		{name: "missing text", args: map[string]any{"contains": []any{"connected to db"}}, want: "FAIL", reason: []string{"connected to db"}},
		{name: "forbidden text", args: map[string]any{"not_contains": []any{"boom"}}, want: "FAIL", reason: []string{"boom"}},
		{name: "pattern", args: map[string]any{"matches": []any{`port":\s*8080`}}, want: "PASS"},
		{name: "json lines", args: map[string]any{"json_fields": []any{
			map[string]any{"path": "level", "equals": "info"}, map[string]any{"path": "port", "equals": 8080}}}, want: "PASS"},
		{name: "json lines need a matching line", args: map[string]any{"json_fields": []any{
			map[string]any{"path": "level", "equals": "debug"}}}, want: "FAIL", reason: []string{"json_fields"}},
		{name: "json lines min matches", args: map[string]any{"min_matches": 2, "json_fields": []any{
			map[string]any{"path": "msg", "exists": true}}}, want: "PASS"},
		{name: "json lines min matches not reached", args: map[string]any{"min_matches": 3, "json_fields": []any{
			map[string]any{"path": "msg", "exists": true}}}, want: "FAIL", reason: []string{"2 of the 3"}},
		{name: "min lines", args: map[string]any{"min_lines": 10}, want: "FAIL", reason: []string{"4 lines"}},
		{name: "named container", args: map[string]any{"container": "app", "contains": []any{"starting"}}, want: "PASS"},
		{name: "container the learner's pod lacks", args: map[string]any{"container": "sidecar", "contains": []any{"starting"}}, want: "FAIL", reason: []string{"sidecar"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newReader().put(podRef, podDoc(t, "app", "", "Running", "app"))
			pods := newPods()
			pods.logs["jk-p1/app/app"] = log
			res := gradeOne(t, grader.ArtifactConfig{Reader: r, Pods: pods}, "", logsCrit(podRef, tc.args))
			testutil.Equal(t, res.Status, tc.want)
			reasonHas(t, res, tc.reason...)
		})
	}
}

// The log text itself is never stored: an application may print tokens.
func TestLogsNeverStoreLogText(t *testing.T) {
	r := newReader().put(podRef, podDoc(t, "app", "", "Running", "app")).
		putList("Secret", "jk-p1", obj(t, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db"},"stringData":{"password":"hunter2-very-secret"}}`))
	pods := newPods()
	pods.logs["jk-p1/app/app"] = "connecting with password hunter2-very-secret and token eyJhbGciOiJSUzI1NiJ9.payload\n"
	res := gradeOne(t, grader.ArtifactConfig{Reader: r, Pods: pods}, "",
		logsCrit(podRef, map[string]any{"contains": []any{"never printed"}, "json_fields": []any{map[string]any{"path": "x", "exists": true}}}))
	testutil.Equal(t, res.Status, "FAIL")
	for _, leak := range []string{"hunter2-very-secret", "eyJhbGci"} {
		if strings.Contains(evidenceJSON(t, res), leak) {
			t.Errorf("evidence leaks %q: %s", leak, evidenceJSON(t, res))
		}
	}
	// The namespace's Secrets were registered with the redactor.
	found := false
	for _, q := range r.listQuery {
		found = found || q.Kind == "Secret"
	}
	if !found {
		t.Error("namespace Secrets were not listed for redaction")
	}
}

const logDeployDoc = `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"jk-p1","uid":"dep-1","generation":1},
  "spec":{"replicas":2,"selector":{"matchLabels":{"app":"web"}}}}`

func deployLogReader(t testing.TB, pods ...map[string]any) *fakeReader {
	rs := obj(t, `{"apiVersion":"apps/v1","kind":"ReplicaSet","metadata":{"name":"web-1","namespace":"jk-p1","uid":"rs-1","ownerReferences":[{"uid":"dep-1","controller":true}]}}`)
	return newReader().put(deployRef, obj(t, logDeployDoc)).putList("ReplicaSet", "jk-p1", rs).putList("Pod", "jk-p1", pods...)
}

func TestLogsOfWorkloadPods(t *testing.T) {
	mk := func(t *testing.T, a, b string) (*fakeReader, *fakePods) {
		r := deployLogReader(t, podDoc(t, "web-a", "rs-1", "Running", "web"), podDoc(t, "web-b", "rs-1", "Running", "web"))
		pods := newPods()
		pods.logs["jk-p1/web-a/web"], pods.logs["jk-p1/web-b/web"] = a, b
		return r, pods
	}
	args := func(mode string) map[string]any {
		return map[string]any{"contains": []any{"served request"}, "pods": mode}
	}
	t.Run("any pod is enough by default", func(t *testing.T) {
		r, pods := mk(t, "idle\n", "served request\n")
		res := gradeOne(t, grader.ArtifactConfig{Reader: r, Pods: pods}, "", logsCrit(deployRef, args("any")))
		testutil.Equal(t, res.Status, "PASS")
	})
	t.Run("all pods", func(t *testing.T) {
		r, pods := mk(t, "idle\n", "served request\n")
		res := gradeOne(t, grader.ArtifactConfig{Reader: r, Pods: pods}, "", logsCrit(deployRef, args("all")))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "web-a")
	})
	t.Run("all pods satisfied", func(t *testing.T) {
		r, pods := mk(t, "served request\n", "served request\n")
		res := gradeOne(t, grader.ArtifactConfig{Reader: r, Pods: pods}, "", logsCrit(deployRef, args("all")))
		testutil.Equal(t, res.Status, "PASS")
	})
	t.Run("pods of another controller are not read", func(t *testing.T) {
		r := deployLogReader(t, podDoc(t, "stranger", "rs-other", "Running", "web"))
		pods := newPods()
		pods.logs["jk-p1/stranger/web"] = "served request\n"
		res := gradeOne(t, grader.ArtifactConfig{Reader: r, Pods: pods}, "", logsCrit(deployRef, args("any")))
		testutil.Equal(t, res.Status, "ERROR")
		testutil.Equal(t, pods.readCount(), 0)
	})
}

func TestLogsUnreadableContainers(t *testing.T) {
	t.Run("never started for an unproven reason is an error", func(t *testing.T) {
		r := newReader().put(podRef, podDoc(t, "app", "", "Pending", "app"))
		res := gradeOne(t, grader.ArtifactConfig{Reader: r, Pods: newPods()}, "", logsCrit(podRef, map[string]any{"contains": []any{"x"}}))
		testutil.Equal(t, res.Status, "ERROR")
	})
	t.Run("a wrong image of the learner is a fail", func(t *testing.T) {
		pod := obj(t, `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"app","namespace":"jk-p1","uid":"u"},
		 "spec":{"containers":[{"name":"app","image":"BusyBox:1"}]},
		 "status":{"phase":"Pending","containerStatuses":[{"name":"app","state":{"waiting":{"reason":"InvalidImageName","message":"bad"}}}]}}`)
		r := newReader().put(podRef, pod)
		res := gradeOne(t, grader.ArtifactConfig{Reader: r, Pods: newPods()}, "", logsCrit(podRef, map[string]any{"contains": []any{"x"}}))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "InvalidImageName")
	})
	t.Run("log API failure is an error", func(t *testing.T) {
		r := newReader().put(podRef, podDoc(t, "app", "", "Running", "app"))
		pods := newPods()
		pods.logErr["jk-p1/app/app"] = fmt.Errorf("kubelet unreachable")
		res := gradeOne(t, grader.ArtifactConfig{Reader: r, Pods: pods}, "", logsCrit(podRef, map[string]any{"contains": []any{"x"}}))
		testutil.Equal(t, res.Status, "ERROR")
	})
	t.Run("missing target is a fail", func(t *testing.T) {
		res := gradeOne(t, grader.ArtifactConfig{Reader: newReader(), Pods: newPods()}, "", logsCrit(podRef, map[string]any{"contains": []any{"x"}}))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "not found")
	})
	t.Run("wait polls until the log shows the text", func(t *testing.T) {
		r := newReader().put(podRef, podDoc(t, "app", "", "Running", "app"))
		pods := newPods()
		calls := 0
		pods2 := &sequencedPods{fakePods: pods, seq: []string{"", "booting\n", "ready now\n"}, calls: &calls}
		eng, _ := artifactEngine(grader.ArtifactConfig{Reader: r, Pods: pods2}, newClock())
		c := logsCrit(podRef, map[string]any{"wait": true, "contains": []any{"ready now"}})
		c.Timeout = 30e9
		ev, err := eng.Evaluate(context.Background(), artifactTarget("", c))
		testutil.NoError(t, err)
		testutil.Equal(t, ev.Results[0].Status, "PASS")
		testutil.Equal(t, calls, 3)
	})
}

type sequencedPods struct {
	*fakePods
	seq   []string
	calls *int
}

func (s *sequencedPods) Logs(_ context.Context, _, _, _ string, _ grader.LogOptions) (string, error) {
	i := *s.calls
	*s.calls++
	if i >= len(s.seq) {
		i = len(s.seq) - 1
	}
	if s.seq[i] == "" {
		return "", fmt.Errorf("%w: waiting", grader.ErrContainerNotStarted)
	}
	return s.seq[i], nil
}

func TestLogsArgsAreStrict(t *testing.T) {
	ck := grader.NewArtifactChecker(grader.ArtifactConfig{})
	bad := map[string]model.Criterion{
		"unknown key":      logsCrit(podRef, map[string]any{"bogus": 1}),
		"no rule":          logsCrit(podRef, map[string]any{"container": "a"}),
		"no target":        logsCrit(model.ObjectRef{}, map[string]any{"contains": []any{"x"}}),
		"unsupported kind": logsCrit(refOf("v1", "ConfigMap", "ns", "x"), map[string]any{"contains": []any{"x"}}),
		"bad pods mode":    logsCrit(podRef, map[string]any{"contains": []any{"x"}, "pods": "some"}),
		"bad tail":         logsCrit(podRef, map[string]any{"contains": []any{"x"}, "tail_lines": 100000}),
		"bad regexp":       logsCrit(podRef, map[string]any{"matches": []any{"("}}),
		"min without json": logsCrit(podRef, map[string]any{"contains": []any{"x"}, "min_matches": 2}),
	}
	for name, c := range bad {
		if err := ck.ValidateCriterion(c); !isInvalid(err) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
