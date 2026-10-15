package grader_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

const kustomizeFixtures = "../../tests/fixtures/artifacts/kustomize"

// What kustomize renders for the fixture overlay.
const renderedOverlay = `apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    app: web
    env: prod
  name: web
  namespace: jk-art
spec:
  replicas: 2
  selector:
    matchLabels:
      app: web
  template:
    metadata:
      labels:
        app: web
    spec:
      containers:
      - command:
        - sleep
        - "3600"
        image: docker.io/library/busybox:1.37.0
        name: web
`

// The same objects as another overlay would render them: other key order,
// flow style, quoted values, a leading comment, an extra ConfigMap first.
const renderedOverlayReordered = `# rendered by another overlay
---
apiVersion: v1
kind: ConfigMap
metadata: {name: unrelated, namespace: jk-art}
data: {k: v}
---
spec:
  template:
    spec:
      containers:
      - {name: web, image: "docker.io/library/busybox:1.37.0", command: ["sleep", "3600"]}
    metadata: {labels: {app: web}}
  selector: {matchLabels: {app: web}}
  replicas: 2
kind: Deployment
metadata: {namespace: jk-art, name: web, labels: {env: prod, app: web}}
apiVersion: apps/v1
`

const renderedWrongReplicas = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: jk-art
  labels: {app: web, env: prod}
spec:
  replicas: 5
  template:
    spec:
      containers:
      - name: web
        image: docker.io/library/busybox:1.37.0
`

func copyTree(t testing.TB, from, to string) {
	t.Helper()
	testutil.NoError(t, filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	}))
}

// kzWorkspace is a session workspace holding the provided base and the
// learner's overlay(s), plus the frozen problem's assets (the provided base).
func kzWorkspace(t testing.TB) (string, map[string][]byte) {
	t.Helper()
	ws := t.TempDir()
	for _, d := range []string{"base", "overlay", "overlay-alt"} {
		testutil.NoError(t, os.MkdirAll(filepath.Join(ws, d), 0o755))
		copyTree(t, filepath.Join(kustomizeFixtures, d), filepath.Join(ws, d))
	}
	assets := map[string][]byte{}
	for _, f := range []string{"base/kustomization.yaml", "base/deployment.yaml"} {
		b, err := os.ReadFile(filepath.Join(kustomizeFixtures, f))
		testutil.NoError(t, err)
		assets[f] = b
	}
	return ws, assets
}

// kubectlRendering plays `kubectl kustomize <dir>`.
func kubectlRendering(renders map[string]string) *fakeTools {
	return &fakeTools{handler: func(call grader.ToolCall) (grader.ToolResult, error) {
		if call.Exe != "kubectl" || len(call.Args) < 2 || call.Args[0] != "kustomize" {
			return grader.ToolResult{}, fmt.Errorf("unexpected tool call %s", joined(call))
		}
		dir := call.Args[len(call.Args)-1]
		r, ok := renders[filepath.Base(dir)]
		if !ok {
			return exit(1, "Error: unable to find one of 'kustomization.yaml' in directory "+dir), nil
		}
		if strings.HasPrefix(r, "ERROR:") {
			return exit(1, strings.TrimPrefix(r, "ERROR:")), nil
		}
		return out(r), nil
	}}
}

func kzCrit(args map[string]any) model.Criterion {
	a := map[string]any{
		"path": "overlay",
		"base": map[string]any{"path": "base"},
		"expect": []any{map[string]any{"api_version": "apps/v1", "kind": "Deployment", "name": "web", "namespace": "jk-art",
			"fields": []any{
				map[string]any{"path": "spec.replicas", "equals": 2},
				map[string]any{"path": "metadata.labels.env", "equals": "prod"},
				map[string]any{"path": "spec.template.spec.containers[name=web].image", "equals": "busybox:1.37.0"}}}},
		"live": false,
	}
	for k, v := range args {
		a[k] = v
	}
	return model.Criterion{ID: "c", Kind: model.CriterionKustomize, Points: 4, Args: a}
}

func kzGrade(t testing.TB, cfg grader.ArtifactConfig, ws string, assets map[string][]byte, c model.Criterion) model.CheckResult {
	t.Helper()
	eng, _ := artifactEngine(cfg, newClock())
	tg := artifactTarget(ws, c)
	tg.Problem.Assets = assets
	ev, err := eng.Evaluate(context.Background(), tg)
	testutil.NoError(t, err)
	return ev.Results[0]
}

// TestOverlaySemantics: the rendered objects are compared as objects. The
// original fixture is the overlay's standard rendering; the same objects in
// another key order and style must PASS; a different value must FAIL.
func TestOverlaySemantics(t *testing.T) {
	cases := []struct {
		name   string
		render string
		want   string
		reason []string
	}{
		{name: "standard rendering", render: renderedOverlay, want: "PASS"},
		{name: "same objects, other key order and style", render: renderedOverlayReordered, want: "PASS"},
		{name: "different replicas", render: renderedWrongReplicas, want: "FAIL", reason: []string{"spec.replicas"}},
		{name: "object missing from the rendering", render: "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x, namespace: jk-art}\n", want: "FAIL", reason: []string{"Deployment", "web", "not rendered"}},
		{name: "overlay that does not build", render: "ERROR:Error: accumulating resources: accumulation err='accumulating resources from 'missing.yaml'': evalsymlink failure", want: "FAIL", reason: []string{"does not build", "accumulating resources"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws, assets := kzWorkspace(t)
			res := kzGrade(t, grader.ArtifactConfig{Tools: kubectlRendering(map[string]string{"overlay": tc.render})}, ws, assets, kzCrit(nil))
			testutil.Equal(t, res.Status, tc.want)
			reasonHas(t, res, tc.reason...)
		})
	}
	t.Run("kubectl missing is an error", func(t *testing.T) {
		ws, assets := kzWorkspace(t)
		res := kzGrade(t, grader.ArtifactConfig{Tools: &fakeTools{}}, ws, assets, kzCrit(nil))
		testutil.Equal(t, res.Status, "ERROR")
	})
	t.Run("absent objects", func(t *testing.T) {
		ws, assets := kzWorkspace(t)
		c := kzCrit(map[string]any{"expect": []any{map[string]any{"api_version": "v1", "kind": "ConfigMap", "name": "debug", "absent": true}}})
		res := kzGrade(t, grader.ArtifactConfig{Tools: kubectlRendering(map[string]string{"overlay": renderedOverlay})}, ws, assets, c)
		testutil.Equal(t, res.Status, "PASS")
		res = kzGrade(t, grader.ArtifactConfig{Tools: kubectlRendering(map[string]string{"overlay": renderedOverlayReordered})}, ws, assets,
			kzCrit(map[string]any{"expect": []any{map[string]any{"api_version": "v1", "kind": "ConfigMap", "name": "unrelated", "absent": true}}}))
		testutil.Equal(t, res.Status, "FAIL")
	})
	t.Run("secrets from generators never show", func(t *testing.T) {
		ws, assets := kzWorkspace(t)
		render := "apiVersion: v1\nkind: Secret\nmetadata: {name: db, namespace: jk-art}\ndata: {password: aHVudGVyMi12ZXJ5LXNlY3JldA==}\n"
		c := kzCrit(map[string]any{"expect": []any{map[string]any{"api_version": "v1", "kind": "Secret", "name": "db", "namespace": "jk-art",
			"fields": []any{map[string]any{"path": "data.password", "equals": "something-else"}}}}})
		res := kzGrade(t, grader.ArtifactConfig{Tools: kubectlRendering(map[string]string{"overlay": render})}, ws, assets, c)
		testutil.Equal(t, res.Status, "FAIL")
		for _, leak := range []string{"hunter2-very-secret", "aHVudGVyMi12ZXJ5LXNlY3JldA=="} {
			if strings.Contains(evidenceJSON(t, res), leak) {
				t.Errorf("evidence leaks %q: %s", leak, evidenceJSON(t, res))
			}
		}
	})
}

func TestKustomizeProtectsTheBase(t *testing.T) {
	cfg := func() grader.ArtifactConfig {
		return grader.ArtifactConfig{Tools: kubectlRendering(map[string]string{"overlay": renderedOverlay})}
	}
	t.Run("untouched base", func(t *testing.T) {
		ws, assets := kzWorkspace(t)
		testutil.Equal(t, kzGrade(t, cfg(), ws, assets, kzCrit(nil)).Status, "PASS")
	})
	t.Run("line endings alone are not a modification", func(t *testing.T) {
		ws, assets := kzWorkspace(t)
		p := filepath.Join(ws, "base", "deployment.yaml")
		b, _ := os.ReadFile(p)
		testutil.NoError(t, os.WriteFile(p, []byte(strings.ReplaceAll(string(b), "\n", "\r\n")), 0o644))
		testutil.Equal(t, kzGrade(t, cfg(), ws, assets, kzCrit(nil)).Status, "PASS")
	})
	mods := map[string]func(ws string){
		"edited base file": func(ws string) {
			p := filepath.Join(ws, "base", "deployment.yaml")
			b, _ := os.ReadFile(p)
			testutil.NoError(t, os.WriteFile(p, []byte(strings.Replace(string(b), "replicas: 1", "replicas: 2", 1)), 0o644))
		},
		"added base file": func(ws string) {
			testutil.NoError(t, os.WriteFile(filepath.Join(ws, "base", "patch.yaml"), []byte("x: y\n"), 0o644))
		},
		"removed base file": func(ws string) { testutil.NoError(t, os.Remove(filepath.Join(ws, "base", "deployment.yaml"))) },
	}
	for name, mod := range mods {
		t.Run(name, func(t *testing.T) {
			ws, assets := kzWorkspace(t)
			mod(ws)
			res := kzGrade(t, cfg(), ws, assets, kzCrit(nil))
			testutil.Equal(t, res.Status, "FAIL")
			reasonHas(t, res, "base", "modified")
		})
	}
	t.Run("overlay that copies the objects instead of using the base", func(t *testing.T) {
		ws, assets := kzWorkspace(t)
		testutil.NoError(t, os.WriteFile(filepath.Join(ws, "overlay", "kustomization.yaml"),
			[]byte("resources:\n  - deployment.yaml\nnamespace: jk-art\n"), 0o644))
		testutil.NoError(t, os.WriteFile(filepath.Join(ws, "overlay", "deployment.yaml"), []byte(renderedOverlay), 0o644))
		res := kzGrade(t, cfg(), ws, assets, kzCrit(nil))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "does not use the provided base")
	})
	t.Run("overlay that reaches the base through a nested overlay", func(t *testing.T) {
		ws, assets := kzWorkspace(t)
		testutil.NoError(t, os.MkdirAll(filepath.Join(ws, "overlay", "inner"), 0o755))
		testutil.NoError(t, os.WriteFile(filepath.Join(ws, "overlay", "inner", "kustomization.yaml"), []byte("resources:\n  - ../../base\n"), 0o644))
		testutil.NoError(t, os.WriteFile(filepath.Join(ws, "overlay", "kustomization.yaml"), []byte("resources:\n  - inner\nnamespace: jk-art\n"), 0o644))
		testutil.Equal(t, kzGrade(t, cfg(), ws, assets, kzCrit(nil)).Status, "PASS")
	})
	t.Run("no provided assets under the base is an author error", func(t *testing.T) {
		ws, _ := kzWorkspace(t)
		res := kzGrade(t, cfg(), ws, map[string][]byte{}, kzCrit(nil))
		testutil.Equal(t, res.Status, "ERROR")
	})
}

func TestKustomizeRefusesUnsafeOverlays(t *testing.T) {
	cfg := func() grader.ArtifactConfig {
		return grader.ArtifactConfig{Tools: kubectlRendering(map[string]string{"overlay": renderedOverlay})}
	}
	remote := []string{
		"resources:\n  - https://github.com/example/app//deploy?ref=v1\n",
		"resources:\n  - github.com/example/app\n",
		"resources:\n  - git@github.com:example/app.git\n",
		"bases:\n  - git::https://example.com/x.git\n",
	}
	for i, k := range remote {
		t.Run(fmt.Sprintf("remote resource %d", i), func(t *testing.T) {
			ws, assets := kzWorkspace(t)
			testutil.NoError(t, os.WriteFile(filepath.Join(ws, "overlay", "kustomization.yaml"), []byte(k+"  - ../base\n"), 0o644))
			tools := kubectlRendering(map[string]string{"overlay": renderedOverlay})
			res := kzGrade(t, grader.ArtifactConfig{Tools: tools}, ws, assets, kzCrit(nil))
			testutil.Equal(t, res.Status, "FAIL")
			reasonHas(t, res, "remote")
			testutil.Equal(t, len(tools.called()), 0)
		})
	}
	t.Run("symlink out of the workspace", func(t *testing.T) {
		ws, assets := kzWorkspace(t)
		outside := t.TempDir()
		testutil.NoError(t, os.WriteFile(filepath.Join(outside, "secret.yaml"), []byte("kind: Secret\n"), 0o644))
		testutil.NoError(t, os.Symlink(filepath.Join(outside, "secret.yaml"), filepath.Join(ws, "overlay", "extra.yaml")))
		tools := kubectlRendering(map[string]string{"overlay": renderedOverlay})
		res := kzGrade(t, grader.ArtifactConfig{Tools: tools}, ws, assets, kzCrit(nil))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "outside the session workspace")
		testutil.Equal(t, len(tools.called()), 0)
	})
	t.Run("overlay directory missing", func(t *testing.T) {
		ws, assets := kzWorkspace(t)
		testutil.NoError(t, os.RemoveAll(filepath.Join(ws, "overlay")))
		res := kzGrade(t, cfg(), ws, assets, kzCrit(nil))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "overlay", "does not exist")
	})
	t.Run("command line", func(t *testing.T) {
		ws, assets := kzWorkspace(t)
		tools := kubectlRendering(map[string]string{"overlay": renderedOverlay})
		kzGrade(t, grader.ArtifactConfig{Tools: tools}, ws, assets, kzCrit(nil))
		calls := tools.called()
		testutil.Equal(t, len(calls), 1)
		line := joined(calls[0])
		for _, bad := range []string{"--enable-helm", "--enable-alpha-plugins", "--enable-exec", "KUBECONFIG", "--kubeconfig"} {
			if strings.Contains(line, bad) || strings.Contains(strings.Join(calls[0].Env, " "), bad) {
				t.Errorf("render call carries %q: %s %v", bad, line, calls[0].Env)
			}
		}
		if !strings.Contains(line, "--load-restrictor=LoadRestrictionsRootOnly") {
			t.Errorf("load restriction not pinned: %s", line)
		}
	})
}

func liveDeployment(t testing.TB, replicas int, extra string) map[string]any {
	return obj(t, fmt.Sprintf(`{"apiVersion":"apps/v1","kind":"Deployment",
	 "metadata":{"name":"web","namespace":"jk-art","uid":"u1","generation":2,"labels":{"app":"web","env":"prod"}},
	 "spec":{"replicas":%d,"revisionHistoryLimit":10,"strategy":{"type":"RollingUpdate"},"selector":{"matchLabels":{"app":"web"}},
	  "template":{"metadata":{"labels":{"app":"web"}},"spec":{"containers":[{"name":"web","image":"docker.io/library/busybox:1.37.0","command":["sleep","3600"],"imagePullPolicy":"IfNotPresent"%s}]}}}}`, replicas, extra))
}

func TestKustomizeChecksTheDeployedState(t *testing.T) {
	live := func(r *fakeReader) grader.ArtifactConfig {
		return grader.ArtifactConfig{Reader: r, Tools: kubectlRendering(map[string]string{"overlay": renderedOverlay})}
	}
	crit := kzCrit(map[string]any{"live": true})
	t.Run("applied as rendered", func(t *testing.T) {
		ws, assets := kzWorkspace(t)
		r := newReader().put(refOf("apps/v1", "Deployment", "jk-art", "web"), liveDeployment(t, 2, ""))
		testutil.Equal(t, kzGrade(t, live(r), ws, assets, crit).Status, "PASS")
	})
	t.Run("rendered well but never applied", func(t *testing.T) {
		ws, assets := kzWorkspace(t)
		r := newReader().put(refOf("apps/v1", "Deployment", "jk-art", "web"), liveDeployment(t, 1, ""))
		res := kzGrade(t, live(r), ws, assets, crit)
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "deployed", "replicas")
	})
	t.Run("applied then edited by hand", func(t *testing.T) {
		ws, assets := kzWorkspace(t)
		d := liveDeployment(t, 2, "")
		d["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["image"] = "docker.io/library/busybox:1.36.0"
		r := newReader().put(refOf("apps/v1", "Deployment", "jk-art", "web"), d)
		res := kzGrade(t, live(r), ws, assets, crit)
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "deployed")
	})
	t.Run("not deployed at all", func(t *testing.T) {
		ws, assets := kzWorkspace(t)
		res := kzGrade(t, live(newReader()), ws, assets, crit)
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "not found")
	})
	t.Run("API failure is an error", func(t *testing.T) {
		ws, assets := kzWorkspace(t)
		r := newReader().put(refOf("apps/v1", "Deployment", "jk-art", "web"), fmt.Errorf("etcd timeout"))
		testutil.Equal(t, kzGrade(t, live(r), ws, assets, crit).Status, "ERROR")
	})
	t.Run("quantities are compared as quantities", func(t *testing.T) {
		ws, assets := kzWorkspace(t)
		render := strings.Replace(renderedOverlay, "        name: web\n", "        name: web\n        resources:\n          limits: {cpu: 1000m}\n", 1)
		r := newReader().put(refOf("apps/v1", "Deployment", "jk-art", "web"), liveDeployment(t, 2, `,"resources":{"limits":{"cpu":"1"}}`))
		cfg := grader.ArtifactConfig{Reader: r, Tools: kubectlRendering(map[string]string{"overlay": render})}
		testutil.Equal(t, kzGrade(t, cfg, ws, assets, crit).Status, "PASS")
	})
}

// TestOverlaySemanticsRealKustomize renders the reviewed fixtures with the
// kustomize built into the pinned kubectl: two differently written overlays
// that mean the same thing must both pass, and an overlay of other meaning
// must fail. It is skipped (NOT RUN) when kubectl is not installed.
func TestOverlaySemanticsRealKustomize(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is not installed: real kustomize rendering NOT RUN")
	}
	cfg := grader.ArtifactConfig{Tools: grader.NewToolRunner()}
	for _, overlay := range []string{"overlay", "overlay-alt"} {
		t.Run(overlay, func(t *testing.T) {
			ws, assets := kzWorkspace(t)
			res := kzGrade(t, cfg, ws, assets, kzCrit(map[string]any{"path": overlay}))
			testutil.Equal(t, res.Status, "PASS")
			if !strings.HasPrefix(res.Evidence.InputHash, "sha256:") {
				t.Errorf("no overlay tree hash: %q", res.Evidence.InputHash)
			}
		})
	}
	t.Run("different meaning", func(t *testing.T) {
		ws, assets := kzWorkspace(t)
		p := filepath.Join(ws, "overlay", "kustomization.yaml")
		b, _ := os.ReadFile(p)
		testutil.NoError(t, os.WriteFile(p, []byte(strings.Replace(string(b), "count: 2", "count: 4", 1)), 0o644))
		res := kzGrade(t, cfg, ws, assets, kzCrit(nil))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "spec.replicas")
	})
	t.Run("broken overlay", func(t *testing.T) {
		ws, assets := kzWorkspace(t)
		testutil.NoError(t, os.WriteFile(filepath.Join(ws, "overlay", "kustomization.yaml"), []byte("resources:\n  - ../base\n  - missing.yaml\n"), 0o644))
		res := kzGrade(t, cfg, ws, assets, kzCrit(nil))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "does not build")
	})
}

func TestKustomizeArgsAreStrict(t *testing.T) {
	ck := grader.NewArtifactChecker(grader.ArtifactConfig{})
	ok := kzCrit(nil)
	testutil.NoError(t, ck.ValidateCriterion(ok))
	mut := func(f func(a map[string]any)) model.Criterion {
		c := kzCrit(nil)
		f(c.Args)
		return c
	}
	exp := func(e map[string]any) []any { return []any{e} }
	bad := map[string]model.Criterion{
		"unknown key":       mut(func(a map[string]any) { a["bogus"] = 1 }),
		"no path":           mut(func(a map[string]any) { delete(a, "path") }),
		"path escapes":      mut(func(a map[string]any) { a["path"] = "../x" }),
		"base escapes":      mut(func(a map[string]any) { a["base"] = map[string]any{"path": "/etc"} }),
		"nothing graded":    mut(func(a map[string]any) { delete(a, "expect"); delete(a, "base") }),
		"expect needs kind": mut(func(a map[string]any) { a["expect"] = exp(map[string]any{"api_version": "v1", "name": "x"}) }),
		"absent with fields": mut(func(a map[string]any) {
			a["expect"] = exp(map[string]any{"api_version": "v1", "kind": "ConfigMap", "name": "x", "absent": true, "fields": []any{map[string]any{"path": "a", "exists": true}}})
		}),
		"absent with live": mut(func(a map[string]any) {
			a["live"] = true
			a["expect"] = exp(map[string]any{"api_version": "v1", "kind": "ConfigMap", "name": "x", "absent": true, "live": true})
		}),
		"bad field": mut(func(a map[string]any) {
			a["expect"] = exp(map[string]any{"api_version": "v1", "kind": "ConfigMap", "name": "x", "fields": []any{map[string]any{"path": ""}}})
		}),
	}
	for name, c := range bad {
		if err := ck.ValidateCriterion(c); !isInvalid(err) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	withTarget := kzCrit(nil)
	withTarget.Target = refOf("v1", "Pod", "ns", "x")
	if err := ck.ValidateCriterion(withTarget); !isInvalid(err) {
		t.Errorf("target given: %v", err)
	}
}
