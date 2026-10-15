package grader_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

const deployDoc = `{
 "apiVersion":"apps/v1","kind":"Deployment",
 "metadata":{"name":"web","namespace":"jk-p1","uid":"uid-1","generation":3,
   "labels":{"app":"web","tier":"frontend"},
   "annotations":{"app.kubernetes.io/note":"hello"}},
 "spec":{"replicas":3,"selector":{"matchLabels":{"app":"web","tier":"frontend"}},
   "strategy":{"type":"RollingUpdate"},
   "template":{"spec":{"containers":[
     {"name":"sidecar","image":"busybox:1.37.0","resources":{"requests":{"cpu":"100m"}}},
     {"name":"web","image":"nginx:1.27.0","args":["--a","--b"],"env":[{"name":"A","value":"1"},{"name":"B","value":"2"}],"imagePullPolicy":"IfNotPresent",
      "ports":[{"name":"http","containerPort":8080}],
      "resources":{"limits":{"cpu":"1","memory":"128Mi"},"requests":{"cpu":"500m","memory":"64Mi"}}}
   ]}}}
}`

func fieldCrit(fields ...map[string]any) model.Criterion {
	var list []any
	for _, f := range fields {
		list = append(list, f)
	}
	return crit("f", model.CriterionField, 2, deployRef, map[string]any{"fields": list})
}

func fieldEq(path string, value any, extra ...any) map[string]any {
	m := map[string]any{"path": path, "equals": value}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i].(string)] = extra[i+1]
	}
	return m
}

func checkField(t *testing.T, doc string, c model.Criterion) model.CheckResult {
	t.Helper()
	r := newReader().put(deployRef, obj(t, doc))
	res, err := grader.NewFieldChecker(r, grader.WithClock(newClock())).Check(context.Background(), target(c), c)
	testutil.NoError(t, err)
	return res
}

func TestQuantityEquivalence(t *testing.T) {
	cases := []struct {
		name  string
		field map[string]any
		want  string
	}{
		{"1 cpu equals 1000m", fieldEq("spec.template.spec.containers[name=web].resources.limits.cpu", "1000m"), "PASS"},
		{"integer 1 equals 1", fieldEq("spec.template.spec.containers[name=web].resources.limits.cpu", 1), "PASS"},
		{"float 0.5 equals 500m", fieldEq("spec.template.spec.containers[name=web].resources.requests.cpu", 0.5), "PASS"},
		{"128Mi equals 134217728", fieldEq("spec.template.spec.containers[name=web].resources.limits.memory", 134217728), "PASS"},
		{"128Mi equals 131072Ki", fieldEq("spec.template.spec.containers[name=web].resources.limits.memory", "131072Ki"), "PASS"},
		{"256Mi is not 128Mi", fieldEq("spec.template.spec.containers[name=web].resources.limits.memory", "256Mi"), "FAIL"},
		{"2 cpu is not 1", fieldEq("spec.template.spec.containers[name=web].resources.limits.cpu", "2"), "FAIL"},
		{"whole map of quantities", fieldEq("spec.template.spec.containers[name=web].resources.limits", map[string]any{"cpu": "1000m", "memory": "128Mi"}), "PASS"},
		{"explicit quantity mode", fieldEq("spec.template.spec.containers[name=web].resources.limits.cpu", "1000m", "compare", "quantity"), "PASS"},
		{"explicit exact mode keeps strings strict", fieldEq("spec.template.spec.containers[name=web].resources.limits.cpu", "1000m", "compare", "exact"), "FAIL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := checkField(t, deployDoc, fieldCrit(tc.field))
			if res.Status != tc.want {
				t.Fatalf("status %s, want %s (reason %q)", res.Status, tc.want, res.Reason)
			}
		})
	}
}

func TestFieldSemanticComparison(t *testing.T) {
	cases := []struct {
		name  string
		field map[string]any
		want  string
	}{
		{"named container regardless of index", fieldEq("spec.template.spec.containers[name=web].image", "nginx:1.27.0"), "PASS"},
		{"wrong image is exact", fieldEq("spec.template.spec.containers[name=web].image", "nginx:1.27"), "FAIL"},
		// Spec 6.2: equivalent image spellings are the same image by default.
		{"fully qualified image equals the short one", fieldEq("spec.template.spec.containers[name=web].image", "docker.io/library/nginx:1.27.0"), "PASS"},
		{"short image equals the fully qualified live one", fieldEq("spec.template.spec.containers[name=sidecar].image", "docker.io/library/busybox:1.37.0"), "PASS"},
		{"index.docker.io is docker.io", fieldEq("spec.template.spec.containers[name=sidecar].image", "index.docker.io/library/busybox:1.37.0"), "PASS"},
		{"another tag is another image", fieldEq("spec.template.spec.containers[name=web].image", "docker.io/library/nginx:1.27.1"), "FAIL"},
		{"implicit latest is not 1.37.0", fieldEq("spec.template.spec.containers[name=sidecar].image", "busybox"), "FAIL"},
		{"another registry is another image", fieldEq("spec.template.spec.containers[name=web].image", "ghcr.io/library/nginx:1.27.0"), "FAIL"},
		{"index addressing finds the image too", fieldEq("spec.template.spec.containers[1].image", "docker.io/library/nginx:1.27.0"), "PASS"},
		{"exact keeps the literal string", fieldEq("spec.template.spec.containers[name=web].image", "docker.io/library/nginx:1.27.0", "compare", "exact"), "FAIL"},
		{"exact accepts the literal string", fieldEq("spec.template.spec.containers[name=web].image", "nginx:1.27.0", "compare", "exact"), "PASS"},
		{"a non-image field is never normalized", fieldEq("spec.template.spec.containers[name=web].imagePullPolicy", "ifnotpresent"), "FAIL"},
		{"image inside a whole container", fieldEq("spec.template.spec.containers[name=web]", map[string]any{"name": "web", "image": "docker.io/library/nginx:1.27.0"}, "match", "subset"), "PASS"},
		{"image inside the container list", fieldEq("spec.template.spec.containers", []any{map[string]any{"name": "sidecar", "image": "docker.io/library/busybox:1.37.0"}}, "match", "subset"), "PASS"},
		{"wrong image inside the container list", fieldEq("spec.template.spec.containers", []any{map[string]any{"name": "sidecar", "image": "busybox:1.36.0"}}, "match", "subset"), "FAIL"},
		// Quantities nested inside a compared parent map (auto mode).
		{"quantities inside a whole resources block", fieldEq("spec.template.spec.containers[name=web].resources", map[string]any{
			"limits": map[string]any{"cpu": "1000m", "memory": 134217728}, "requests": map[string]any{"cpu": 0.5, "memory": "64Mi"}}), "PASS"},
		{"wrong quantity inside a whole resources block", fieldEq("spec.template.spec.containers[name=web].resources", map[string]any{
			"limits": map[string]any{"cpu": "2", "memory": "128Mi"}, "requests": map[string]any{"cpu": "500m", "memory": "64Mi"}}), "FAIL"},
		{"quantities inside a whole container", fieldEq("spec.template.spec.containers[name=web]", map[string]any{
			"name": "web", "resources": map[string]any{"limits": map[string]any{"cpu": "1000m"}}}, "match", "subset"), "PASS"},
		{"quantities inside the container list", fieldEq("spec.template.spec.containers", []any{map[string]any{
			"name": "web", "resources": map[string]any{"limits": map[string]any{"cpu": "1000m"}}}}, "match", "subset"), "PASS"},
		{"exact mode keeps quantity strings literal in a parent map", fieldEq("spec.template.spec.containers[name=web].resources", map[string]any{
			"limits": map[string]any{"cpu": "1000m", "memory": "128Mi"}, "requests": map[string]any{"cpu": "500m", "memory": "64Mi"}}, "compare", "exact"), "FAIL"},
		{"non-quantity strings stay exact next to quantities", fieldEq("spec.template.spec.containers[name=web]", map[string]any{
			"name": "web", "imagePullPolicy": "ifnotpresent", "resources": map[string]any{"limits": map[string]any{"cpu": "1000m"}}}, "match", "subset"), "FAIL"},
		{"missing named container", fieldEq("spec.template.spec.containers[name=db].image", "x"), "FAIL"},
		{"index addressing", fieldEq("spec.template.spec.containers[1].name", "web"), "PASS"},
		{"integer from JSON float", fieldEq("spec.replicas", 3.0), "PASS"},
		{"integer from int", fieldEq("spec.replicas", 3), "PASS"},
		{"wrong integer", fieldEq("spec.replicas", 2), "FAIL"},
		{"number is not string", fieldEq("spec.replicas", "3"), "FAIL"},
		{"defaulted field", fieldEq("spec.strategy.type", "RollingUpdate"), "PASS"},
		{"unordered selector map", fieldEq("spec.selector.matchLabels", map[string]any{"tier": "frontend", "app": "web"}), "PASS"},
		{"selector with missing key fails equal", fieldEq("spec.selector.matchLabels", map[string]any{"app": "web"}), "FAIL"},
		{"selector subset", fieldEq("spec.selector.matchLabels", map[string]any{"app": "web"}, "match", "subset"), "PASS"},
		{"annotation key with dots and slash", fieldEq("metadata.annotations['app.kubernetes.io/note']", "hello"), "PASS"},
		{"label via quoted key", fieldEq("metadata.labels['app']", "web"), "PASS"},
		{"args order matters", fieldEq("spec.template.spec.containers[name=web].args", []any{"--b", "--a"}), "FAIL"},
		{"args unordered on request", fieldEq("spec.template.spec.containers[name=web].args", []any{"--b", "--a"}, "match", "unordered"), "PASS"},
		{"env list matches by name, not position", fieldEq("spec.template.spec.containers[name=web].env", []any{map[string]any{"name": "B", "value": "2"}, map[string]any{"name": "A", "value": "1"}}), "PASS"},
		{"env list wrong value under a name", fieldEq("spec.template.spec.containers[name=web].env", []any{map[string]any{"name": "B", "value": "1"}, map[string]any{"name": "A", "value": "1"}}), "FAIL"},
		{"port by name", fieldEq("spec.template.spec.containers[name=web].ports[name=http].containerPort", 8080), "PASS"},
		{"subset requires every expected key of a list element", fieldEq("spec.template.spec.containers[name=web].ports", []any{map[string]any{"name": "http", "containerPort": 8080, "protocol": "TCP"}}, "match", "subset"), "FAIL"},
		{"subset list of named objects", fieldEq("spec.template.spec.containers", []any{map[string]any{"name": "web", "image": "nginx:1.27.0"}, map[string]any{"name": "sidecar"}}, "match", "subset"), "PASS"},
		{"equal list of named objects needs same members", fieldEq("spec.template.spec.containers", []any{map[string]any{"name": "web"}}), "FAIL"},
		{"exists true", map[string]any{"path": "spec.strategy", "exists": true}, "PASS"},
		{"exists false on absent", map[string]any{"path": "spec.paused", "exists": false}, "PASS"},
		{"exists false on present", map[string]any{"path": "spec.replicas", "exists": false}, "FAIL"},
		{"exists true on absent", map[string]any{"path": "spec.paused", "exists": true}, "FAIL"},
		{"absent path fails equals", fieldEq("spec.paused", true), "FAIL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := checkField(t, deployDoc, fieldCrit(tc.field))
			if res.Status != tc.want {
				t.Fatalf("status %s, want %s (reason %q)", res.Status, tc.want, res.Reason)
			}
		})
	}
}

func TestFieldEvidence(t *testing.T) {
	c := fieldCrit(fieldEq("spec.replicas", 2), fieldEq("spec.strategy.type", "RollingUpdate"))
	res := checkField(t, deployDoc, c)
	testutil.Equal(t, res.Status, "FAIL")
	if !strings.Contains(res.Reason, "spec.replicas") {
		t.Fatalf("reason must name the field: %q", res.Reason)
	}
	ev := res.Evidence
	testutil.Equal(t, ev.UID, "uid-1")
	testutil.Equal(t, ev.Generation, int64(3))
	if !strings.HasPrefix(ev.InputHash, "sha256:") {
		t.Fatalf("input hash %q", ev.InputHash)
	}
	testutil.Equal(t, ev.ObservedAt.Location().String(), "UTC")
	exp := ev.Expected.(map[string]any)
	act := ev.Actual.(map[string]any)
	testutil.Equal(t, exp["spec.replicas"], json.Number("2"))
	testutil.Equal(t, act["spec.replicas"], int64(3))
	testutil.Equal(t, act["spec.strategy.type"], "RollingUpdate")
}

func TestFieldRejectsBadArgs(t *testing.T) {
	bad := map[string]map[string]any{
		"unknown top-level key":     {"fields": []any{fieldEq("spec.replicas", 1)}, "feild": 1},
		"unknown requirement key":   {"fields": []any{map[string]any{"path": "spec.replicas", "equals": 1, "equal": 1}}},
		"no fields":                 {"fields": []any{}},
		"missing fields":            {},
		"missing path":              {"fields": []any{map[string]any{"equals": 1}}},
		"neither equals nor exists": {"fields": []any{map[string]any{"path": "spec.replicas"}}},
		"both equals and exists":    {"fields": []any{map[string]any{"path": "spec.replicas", "equals": 1, "exists": true}}},
		"null equals":               {"fields": []any{map[string]any{"path": "spec.replicas", "equals": nil}}},
		"bad match":                 {"fields": []any{fieldEq("spec.replicas", 1, "match", "fuzzy")}},
		"bad compare":               {"fields": []any{fieldEq("spec.replicas", 1, "compare", "loose")}},
		"bad path":                  {"fields": []any{fieldEq("spec..replicas", 1)}},
		"unclosed bracket":          {"fields": []any{fieldEq("spec.containers[name=web", 1)}},
		"wait wrong type":           {"fields": []any{fieldEq("spec.replicas", 1)}, "wait": "yes"},
	}
	for name, args := range bad {
		t.Run(name, func(t *testing.T) {
			c := crit("f", model.CriterionField, 1, deployRef, args)
			r := newReader().put(deployRef, obj(t, deployDoc))
			_, err := grader.NewFieldChecker(r).Check(context.Background(), target(c), c)
			testutil.ErrorIs(t, err, grader.ErrInvalidCriterion)
			testutil.Equal(t, r.callsFor(deployRef), 0) // an author error never touches the cluster
		})
	}
}

func TestResourceChecker(t *testing.T) {
	clock := newClock()
	c := crit("r", model.CriterionResource, 1, deployRef, nil)

	t.Run("found", func(t *testing.T) {
		r := newReader().put(deployRef, obj(t, deployDoc))
		res, err := grader.NewResourceChecker(r, grader.WithClock(clock)).Check(context.Background(), target(c), c)
		testutil.NoError(t, err)
		testutil.Equal(t, res.Status, "PASS")
		testutil.Equal(t, res.Evidence.UID, "uid-1")
		testutil.Equal(t, r.callsFor(deployRef), 1)
	})
	t.Run("missing target is a learner FAIL", func(t *testing.T) {
		r := newReader()
		res, err := grader.NewResourceChecker(r, grader.WithClock(clock)).Check(context.Background(), target(c), c)
		testutil.NoError(t, err)
		testutil.Equal(t, res.Status, "FAIL")
		if !strings.Contains(res.Reason, "not found") {
			t.Fatalf("reason %q", res.Reason)
		}
		testutil.Equal(t, r.callsFor(deployRef), 1)
	})
	t.Run("api failure is an error, never a guessed FAIL", func(t *testing.T) {
		boom := errors.New("connection refused")
		r := newReader().put(deployRef, boom)
		_, err := grader.NewResourceChecker(r, grader.WithClock(clock)).Check(context.Background(), target(c), c)
		testutil.ErrorIs(t, err, boom)
		testutil.Equal(t, r.callsFor(deployRef), 3) // retried before giving up
	})
	t.Run("transient api failure is retried", func(t *testing.T) {
		r := newReader().put(deployRef, errors.New("timeout"), obj(t, deployDoc))
		res, err := grader.NewResourceChecker(r, grader.WithClock(newClock())).Check(context.Background(), target(c), c)
		testutil.NoError(t, err)
		testutil.Equal(t, res.Status, "PASS")
	})
	t.Run("namespace defaults to the problem namespace", func(t *testing.T) {
		noNS := crit("r", model.CriterionResource, 1, refOf("apps/v1", "Deployment", "", "web"), nil)
		r := newReader().put(deployRef, obj(t, deployDoc))
		res, err := grader.NewResourceChecker(r, grader.WithClock(clock)).Check(context.Background(), target(noNS), noNS)
		testutil.NoError(t, err)
		testutil.Equal(t, res.Status, "PASS")
	})
	t.Run("wait polls until the object appears", func(t *testing.T) {
		clk := newClock()
		w := crit("r", model.CriterionResource, 1, deployRef, map[string]any{"wait": true})
		w.Timeout = 10 * time.Second
		r := newReader().put(deployRef, nil, nil, obj(t, deployDoc))
		res, err := grader.NewResourceChecker(r, grader.WithClock(clk)).Check(context.Background(), target(w), w)
		testutil.NoError(t, err)
		testutil.Equal(t, res.Status, "PASS")
		testutil.Equal(t, r.callsFor(deployRef), 3)
	})
	t.Run("wait gives up at the timeout", func(t *testing.T) {
		clk := newClock()
		w := crit("r", model.CriterionResource, 1, deployRef, map[string]any{"wait": true})
		w.Timeout = 5 * time.Second
		r := newReader()
		res, err := grader.NewResourceChecker(r, grader.WithClock(clk)).Check(context.Background(), target(w), w)
		testutil.NoError(t, err)
		testutil.Equal(t, res.Status, "FAIL")
		if clk.slept() < 5*time.Second || clk.slept() > 6*time.Second {
			t.Fatalf("waited %v on the fake clock, want about 5s", clk.slept())
		}
	})
	t.Run("rejects unknown args", func(t *testing.T) {
		bad := crit("r", model.CriterionResource, 1, deployRef, map[string]any{"image": "x"})
		_, err := grader.NewResourceChecker(newReader()).Check(context.Background(), target(bad), bad)
		testutil.ErrorIs(t, err, grader.ErrInvalidCriterion)
	})
	t.Run("rejects an incomplete target", func(t *testing.T) {
		bad := crit("r", model.CriterionResource, 1, model.ObjectRef{Kind: "Deployment"}, nil)
		_, err := grader.NewResourceChecker(newReader()).Check(context.Background(), target(bad), bad)
		testutil.ErrorIs(t, err, grader.ErrInvalidCriterion)
	})
}

func TestFieldWaitConverges(t *testing.T) {
	clk := newClock()
	before := obj(t, deployDoc)
	after := obj(t, deployDoc)
	after["spec"].(map[string]any)["replicas"] = int64(5)
	after["metadata"].(map[string]any)["generation"] = int64(3) // same spec generation: a controller-written field
	c := fieldCrit(fieldEq("spec.replicas", 5))
	c.Args["wait"] = true
	c.Timeout = 10 * time.Second
	r := newReader().put(deployRef, before, before, after)
	res, err := grader.NewFieldChecker(r, grader.WithClock(clk)).Check(context.Background(), target(c), c)
	testutil.NoError(t, err)
	if res.Status != "PASS" {
		t.Fatalf("status %s reason %q", res.Status, res.Reason)
	}
}

func TestTargetChangedDuringGrading(t *testing.T) {
	first := obj(t, deployDoc)
	recreated := obj(t, deployDoc)
	recreated["metadata"].(map[string]any)["uid"] = "uid-2"
	edited := obj(t, deployDoc)
	edited["metadata"].(map[string]any)["generation"] = int64(4)
	for name, second := range map[string]map[string]any{"recreated": recreated, "edited": edited} {
		t.Run(name, func(t *testing.T) {
			c := fieldCrit(fieldEq("spec.replicas", 99))
			c.Args["wait"] = true
			c.Timeout = 10 * time.Second
			r := newReader().put(deployRef, first, second)
			_, err := grader.NewFieldChecker(r, grader.WithClock(newClock())).Check(context.Background(), target(c), c)
			testutil.ErrorIs(t, err, grader.ErrTargetChanged)
		})
	}
	t.Run("deleted", func(t *testing.T) {
		c := fieldCrit(fieldEq("spec.replicas", 99))
		c.Args["wait"] = true
		c.Timeout = 10 * time.Second
		r := newReader().put(deployRef, first, nil)
		_, err := grader.NewFieldChecker(r, grader.WithClock(newClock())).Check(context.Background(), target(c), c)
		testutil.ErrorIs(t, err, grader.ErrTargetChanged)
	})
}
