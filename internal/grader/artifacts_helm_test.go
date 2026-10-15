package grader_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// helmStatusJSON is `helm list -o json` for the release "web".
func helmStatusJSON(status, chart, version string, revision int) string {
	raw, _ := json.Marshal([]any{map[string]any{
		"name": "web", "namespace": "jk-p1", "revision": fmt.Sprint(revision), "updated": "2026-10-15 09:00:00 +0000 UTC",
		"status": status, "chart": chart + "-" + version, "app_version": "1.27.0",
	}})
	return string(raw)
}

const helmManifest = `---
# Source: web/templates/service.yaml
apiVersion: v1
kind: Service
metadata:
  name: web
spec:
  ports:
  - port: 80
---
# Source: web/templates/deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  replicas: 2
`

// helmTools plays the helm CLI for one release.
type helmFixture struct {
	status   func() grader.ToolResult
	values   string
	manifest string
	fail     map[string]grader.ToolResult
}

func (h helmFixture) tools() *fakeTools {
	return &fakeTools{handler: func(call grader.ToolCall) (grader.ToolResult, error) {
		line := joined(call)
		for sub, r := range h.fail {
			if strings.Contains(line, sub) {
				return r, nil
			}
		}
		switch {
		case strings.Contains(line, " list "):
			return h.status(), nil
		case strings.Contains(line, " get values web"):
			return out(h.values), nil
		case strings.Contains(line, " get manifest web"):
			return out(h.manifest), nil
		}
		return grader.ToolResult{}, fmt.Errorf("unexpected helm call %s", line)
	}}
}

func deployedFixture() helmFixture {
	return helmFixture{
		status:   func() grader.ToolResult { return out(helmStatusJSON("deployed", "web", "0.1.0", 2)) },
		values:   `{"replicaCount":2,"image":{"tag":"1.27.0"},"dbPassword":"hunter2-very-secret"}`,
		manifest: helmManifest,
	}
}

func helmCrit(args map[string]any) model.Criterion {
	a := map[string]any{"release": "web"}
	for k, v := range args {
		a[k] = v
	}
	return model.Criterion{ID: "c", Kind: model.CriterionHelm, Points: 4, Args: a}
}

func helmOwned(kind, apiVersion, name string) map[string]any {
	return map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": map[string]any{
		"name": name, "namespace": "jk-p1", "uid": "uid-" + name,
		"labels":      map[string]any{"app.kubernetes.io/managed-by": "Helm"},
		"annotations": map[string]any{"meta.helm.sh/release-name": "web", "meta.helm.sh/release-namespace": "jk-p1"}},
		"spec": map[string]any{}}
}

func helmReader(t testing.TB) *fakeReader {
	return newReader().
		put(refOf("v1", "Service", "jk-p1", "web"), helmOwned("Service", "v1", "web")).
		put(deployRef, helmOwned("Deployment", "apps/v1", "web"))
}

// TestHelmRequiredNotRawApply: the problem requires a Helm release. The
// original fixture is a real release with its objects; the wrong answer is
// the chart's manifests applied with kubectl and no release record.
func TestHelmRequiredNotRawApply(t *testing.T) {
	cfg := func(h helmFixture, r *fakeReader) grader.ArtifactConfig {
		return grader.ArtifactConfig{Reader: r, Tools: h.tools(), KubeconfigPath: "/state/kubeconfig", Context: "junkloud-0123456789ab"}
	}
	t.Run("release with its objects", func(t *testing.T) {
		res := gradeOne(t, cfg(deployedFixture(), helmReader(t)), "", helmCrit(map[string]any{"chart": map[string]any{"name": "web", "version": "0.1.0"}}))
		testutil.Equal(t, res.Status, "PASS")
		testutil.Equal(t, res.Points, 4)
	})
	t.Run("objects applied without a release", func(t *testing.T) {
		// The Deployment and the Service exist (and even look right) but helm
		// has no record of a release.
		raw := helmReader(t)
		raw.put(deployRef, map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "web", "namespace": "jk-p1", "uid": "u"}, "spec": map[string]any{}})
		h := deployedFixture()
		h.fail = map[string]grader.ToolResult{" list ": out("[]")}
		res := gradeOne(t, cfg(h, raw), "", helmCrit(nil))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "no Helm release", "web")
	})
	t.Run("a release record whose objects were applied by hand is not enough either", func(t *testing.T) {
		raw := helmReader(t)
		hand := helmOwned("Deployment", "apps/v1", "web")
		meta := hand["metadata"].(map[string]any)
		meta["annotations"] = map[string]any{}
		meta["labels"] = map[string]any{}
		raw.put(deployRef, hand)
		res := gradeOne(t, cfg(deployedFixture(), raw), "", helmCrit(nil))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "Deployment", "not managed by release web")
	})
	t.Run("an object another release owns", func(t *testing.T) {
		raw := helmReader(t)
		other := helmOwned("Deployment", "apps/v1", "web")
		other["metadata"].(map[string]any)["annotations"].(map[string]any)["meta.helm.sh/release-name"] = "other"
		raw.put(deployRef, other)
		res := gradeOne(t, cfg(deployedFixture(), raw), "", helmCrit(nil))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "not managed by release web")
	})
	t.Run("an object of the release that was deleted", func(t *testing.T) {
		raw := helmReader(t)
		raw.put(refOf("v1", "Service", "jk-p1", "web"), nil)
		res := gradeOne(t, cfg(deployedFixture(), raw), "", helmCrit(nil))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "Service", "not found")
	})
	t.Run("opting out of the object check", func(t *testing.T) {
		raw := newReader()
		res := gradeOne(t, cfg(deployedFixture(), raw), "", helmCrit(map[string]any{"objects": false}))
		testutil.Equal(t, res.Status, "PASS")
	})
}

func TestHelmReleaseState(t *testing.T) {
	cfg := func(h helmFixture) grader.ArtifactConfig {
		return grader.ArtifactConfig{Reader: helmReader(t), Tools: h.tools(), KubeconfigPath: "/state/kubeconfig", Context: "junkloud-0123456789ab"}
	}
	status := func(s, chart, version string, rev int) helmFixture {
		h := deployedFixture()
		h.status = func() grader.ToolResult { return out(helmStatusJSON(s, chart, version, rev)) }
		return h
	}
	cases := []struct {
		name   string
		h      helmFixture
		args   map[string]any
		want   string
		reason []string
	}{
		{name: "failed release", h: status("failed", "web", "0.1.0", 1), args: nil, want: "FAIL", reason: []string{"failed", "deployed"}},
		{name: "uninstalled but kept", h: status("uninstalled", "web", "0.1.0", 1), args: nil, want: "FAIL", reason: []string{"uninstalled"}},
		{name: "another chart", h: status("deployed", "api", "0.1.0", 1), args: map[string]any{"chart": map[string]any{"name": "web"}}, want: "FAIL", reason: []string{"chart", "api"}},
		{name: "old chart version", h: status("deployed", "web", "0.1.0", 1), args: map[string]any{"chart": map[string]any{"version": "0.2.0"}}, want: "FAIL", reason: []string{"0.2.0"}},
		{name: "upgrade not done", h: status("deployed", "web", "0.1.0", 1), args: map[string]any{"min_revision": 2}, want: "FAIL", reason: []string{"revision 1"}},
		{name: "upgrade done", h: status("deployed", "web", "0.2.0", 2), args: map[string]any{"min_revision": 2, "chart": map[string]any{"version": "0.2.0"}}, want: "PASS"},
		{name: "app version", h: status("deployed", "web", "0.1.0", 1), args: map[string]any{"chart": map[string]any{"app_version": "1.27.0"}}, want: "PASS"},
		{name: "rollback expected status", h: status("superseded", "web", "0.1.0", 1), args: map[string]any{"status": "superseded"}, want: "PASS"},
		{name: "cluster unreachable is an error", h: func() helmFixture {
			h := deployedFixture()
			h.fail = map[string]grader.ToolResult{" list ": exit(1, "Error: Kubernetes cluster unreachable: dial tcp 127.0.0.1:1: connect: connection refused")}
			return h
		}(), want: "ERROR"},
		{name: "unreadable status is an error", h: func() helmFixture {
			h := deployedFixture()
			h.status = func() grader.ToolResult { return out("not json") }
			return h
		}(), want: "ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := gradeOne(t, cfg(tc.h), "", helmCrit(tc.args))
			testutil.Equal(t, res.Status, tc.want)
			reasonHas(t, res, tc.reason...)
		})
	}
	t.Run("helm not installed is an error", func(t *testing.T) {
		res := gradeOne(t, grader.ArtifactConfig{Reader: helmReader(t), Tools: &fakeTools{}, KubeconfigPath: "/k", Context: "c"}, "", helmCrit(nil))
		testutil.Equal(t, res.Status, "ERROR")
	})
	t.Run("no kubeconfig configured is an error", func(t *testing.T) {
		res := gradeOne(t, grader.ArtifactConfig{Reader: helmReader(t), Tools: deployedFixture().tools()}, "", helmCrit(nil))
		testutil.Equal(t, res.Status, "ERROR")
	})
	t.Run("wait polls a pending release", func(t *testing.T) {
		n := 0
		h := deployedFixture()
		h.status = func() grader.ToolResult {
			n++
			if n < 3 {
				return out(helmStatusJSON("pending-install", "web", "0.1.0", 1))
			}
			return out(helmStatusJSON("deployed", "web", "0.1.0", 1))
		}
		eng, _ := artifactEngine(cfg(h), newClock())
		c := helmCrit(map[string]any{"wait": true})
		c.Timeout = 30 * time.Second
		ev, err := eng.Evaluate(t.Context(), artifactTarget("", c))
		testutil.NoError(t, err)
		testutil.Equal(t, ev.Results[0].Status, "PASS")
		testutil.Equal(t, n, 3)
	})
}

func TestHelmChartColumnWithDashes(t *testing.T) {
	h := deployedFixture()
	h.status = func() grader.ToolResult { return out(helmStatusJSON("deployed", "my-web", "1.0.0-rc.1", 1)) }
	cfg := grader.ArtifactConfig{Reader: helmReader(t), Tools: h.tools(), KubeconfigPath: "/k", Context: "c"}
	res := gradeOne(t, cfg, "", helmCrit(map[string]any{"chart": map[string]any{"name": "my-web", "version": "1.0.0-rc.1"}}))
	testutil.Equal(t, res.Status, "PASS")
}

func TestHelmCommandLine(t *testing.T) {
	h := deployedFixture()
	tools := h.tools()
	cfg := grader.ArtifactConfig{Reader: helmReader(t), Tools: tools, KubeconfigPath: "/state/kubeconfig", Context: "junkloud-0123456789ab"}
	res := gradeOne(t, cfg, "", helmCrit(map[string]any{"values": []any{map[string]any{"path": "replicaCount", "equals": 2}}}))
	testutil.Equal(t, res.Status, "PASS")
	for _, call := range tools.called() {
		line := joined(call)
		testutil.Equal(t, call.Exe, "helm")
		for _, want := range []string{"--kubeconfig /state/kubeconfig", "--kube-context junkloud-0123456789ab", "-n jk-p1", "-o json"} {
			if strings.Contains(line, " get manifest ") && want == "-o json" {
				continue
			}
			if !strings.Contains(line, want) {
				t.Errorf("%q lacks %q", line, want)
			}
		}
		env := strings.Join(call.Env, "\n")
		for _, want := range []string{"HELM_CACHE_HOME=", "HELM_CONFIG_HOME=", "HELM_DATA_HOME="} {
			if !strings.Contains(env, want) {
				t.Errorf("helm env lacks %s: %v", want, call.Env)
			}
		}
		if strings.Contains(env, "KUBECONFIG=") {
			t.Errorf("ambient KUBECONFIG passed: %v", call.Env)
		}
	}
}

func TestHelmValues(t *testing.T) {
	cfg := func() grader.ArtifactConfig {
		return grader.ArtifactConfig{Reader: helmReader(t), Tools: deployedFixture().tools(), KubeconfigPath: "/k", Context: "c"}
	}
	t.Run("user values", func(t *testing.T) {
		res := gradeOne(t, cfg(), "", helmCrit(map[string]any{"values": []any{
			map[string]any{"path": "replicaCount", "equals": 2}, map[string]any{"path": "image.tag", "equals": "1.27.0"}}}))
		testutil.Equal(t, res.Status, "PASS")
	})
	t.Run("wrong value", func(t *testing.T) {
		res := gradeOne(t, cfg(), "", helmCrit(map[string]any{"values": []any{map[string]any{"path": "replicaCount", "equals": 5}}}))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "replicaCount")
	})
	t.Run("sensitive values are never echoed", func(t *testing.T) {
		res := gradeOne(t, cfg(), "", helmCrit(map[string]any{"values": []any{
			map[string]any{"path": "dbPassword", "equals": "expected-other-secret"}}}))
		testutil.Equal(t, res.Status, "FAIL")
		js := evidenceJSON(t, res)
		for _, leak := range []string{"hunter2-very-secret", "expected-other-secret"} {
			if strings.Contains(js, leak) {
				t.Errorf("evidence leaks %q: %s", leak, js)
			}
		}
	})
	t.Run("sensitive value that matches passes", func(t *testing.T) {
		res := gradeOne(t, cfg(), "", helmCrit(map[string]any{"values": []any{
			map[string]any{"path": "dbPassword", "equals": "hunter2-very-secret"}}}))
		testutil.Equal(t, res.Status, "PASS")
		if strings.Contains(evidenceJSON(t, res), "hunter2") {
			t.Errorf("evidence leaks the value: %s", evidenceJSON(t, res))
		}
	})
	t.Run("no user values at all", func(t *testing.T) {
		h := deployedFixture()
		h.values = "null"
		res := gradeOne(t, grader.ArtifactConfig{Reader: helmReader(t), Tools: h.tools(), KubeconfigPath: "/k", Context: "c"}, "",
			helmCrit(map[string]any{"values": []any{map[string]any{"path": "replicaCount", "equals": 2}}}))
		testutil.Equal(t, res.Status, "FAIL")
	})
	t.Run("computed values with all", func(t *testing.T) {
		tools := deployedFixture().tools()
		res := gradeOne(t, grader.ArtifactConfig{Reader: helmReader(t), Tools: tools, KubeconfigPath: "/k", Context: "c"}, "",
			helmCrit(map[string]any{"all_values": true, "values": []any{map[string]any{"path": "replicaCount", "equals": 2}}}))
		testutil.Equal(t, res.Status, "PASS")
		found := false
		for _, c := range tools.called() {
			found = found || (strings.Contains(joined(c), "get values") && strings.Contains(joined(c), "--all"))
		}
		testutil.Equal(t, found, true)
	})
}

func TestHelmWorkloadsReady(t *testing.T) {
	ready := func(avail int) map[string]any {
		d := helmOwned("Deployment", "apps/v1", "web")
		d["metadata"].(map[string]any)["generation"] = int64(1)
		d["spec"] = map[string]any{"replicas": int64(2), "selector": map[string]any{"matchLabels": map[string]any{"app": "web"}}}
		d["status"] = map[string]any{"observedGeneration": int64(1), "replicas": int64(2), "updatedReplicas": int64(2),
			"readyReplicas": int64(avail), "availableReplicas": int64(avail)}
		return d
	}
	run := func(avail int) model.CheckResult {
		r := newReader().put(refOf("v1", "Service", "jk-p1", "web"), helmOwned("Service", "v1", "web")).put(deployRef, ready(avail)).
			putList("ReplicaSet", "jk-p1").putList("Pod", "jk-p1").putList("Event", "jk-p1")
		c := helmCrit(map[string]any{"workloads_ready": true})
		c.Timeout = 5 * time.Second
		return gradeOne(t, grader.ArtifactConfig{Reader: r, Tools: deployedFixture().tools(), KubeconfigPath: "/k", Context: "c"}, "", c)
	}
	testutil.Equal(t, run(2).Status, "PASS")
	// 0 ready replicas and no pods to explain it: undecided, never a guessed FAIL.
	testutil.Equal(t, run(0).Status, "ERROR")
}

func TestHelmArgsAreStrict(t *testing.T) {
	ck := grader.NewArtifactChecker(grader.ArtifactConfig{})
	bad := map[string]map[string]any{
		"unknown key":        {"release": "web", "bogus": 1},
		"no release":         {},
		"flag as release":    {"release": "--all-namespaces"},
		"uppercase":          {"release": "Web"},
		"bad namespace":      {"release": "web", "namespace": "-n"},
		"values need path":   {"release": "web", "values": []any{map[string]any{"equals": 1}}},
		"negative revision":  {"release": "web", "min_revision": -1},
		"empty chart":        {"release": "web", "chart": map[string]any{}},
		"all without values": {"release": "web", "all_values": true},
	}
	for name, args := range bad {
		if err := ck.ValidateCriterion(model.Criterion{ID: "c", Kind: model.CriterionHelm, Points: 1, Args: args}); !isInvalid(err) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	c := helmCrit(nil)
	c.Target = refOf("v1", "Pod", "ns", "x")
	if err := ck.ValidateCriterion(c); !isInvalid(err) {
		t.Errorf("target given: %v", err)
	}
}
