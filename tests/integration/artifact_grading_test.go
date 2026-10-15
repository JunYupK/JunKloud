//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

const (
	artNamespace = "jk-art"
	// Secret values planted in the cluster and in Helm values. Neither may
	// appear in any result, reason, evidence or test output.
	artSecret     = "integration-secret-value-7f3a91"
	artHelmSecret = "helm-secret-value-9c1d52"
	fixtures      = "../fixtures/artifacts"
)

// run executes a program with the test environment (no ambient KUBECONFIG)
// and returns stdout. It fails the test on a non-zero exit.
func run(t *testing.T, stdin string, exe string, args ...string) string {
	t.Helper()
	var in io.Reader
	if stdin != "" {
		in = strings.NewReader(stdin)
	}
	res, err := toolEnv.Run(context.Background(), exe, args, in)
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", exe, strings.Join(args, " "), err, res.Stderr)
	}
	return string(res.Stdout)
}

// artEnv bundles what the subtests need.
type artEnv struct {
	t        *testing.T
	m        *environment.Manager
	id       model.Identity
	engine   *grader.Engine
	capturer *grader.ArtifactChecker
	evidence []string // every evaluation as JSON, for the final leak scan
	logged   []string // every verdict line shown with t.Logf
}

func (e *artEnv) kubectl(stdin string, args ...string) string {
	e.t.Helper()
	full := append([]string{"--kubeconfig", e.m.KubeconfigPath(), "--context", e.id.Context}, args...)
	return run(e.t, stdin, "kubectl", full...)
}

func (e *artEnv) helm(args ...string) string {
	e.t.Helper()
	dir := e.t.TempDir()
	full := append([]string{"--kubeconfig", e.m.KubeconfigPath(), "--kube-context", e.id.Context}, args...)
	cmd := append([]string{"env", "HELM_CACHE_HOME=" + dir, "HELM_CONFIG_HOME=" + dir, "HELM_DATA_HOME=" + dir, "helm"}, full...)
	return run(e.t, "", cmd[0], cmd[1:]...)
}

func (e *artEnv) target(ws string, assets map[string][]byte, criteria ...model.Criterion) grader.Target {
	return grader.Target{
		Identity:  e.id,
		Workspace: ws,
		Problem: model.FrozenProblem{
			Definition: model.Problem{ID: "grade-art", Namespace: artNamespace, Criteria: criteria},
			Namespace:  artNamespace, Assets: assets,
		},
	}
}

// grade evaluates criteria and returns results by criterion ID.
func (e *artEnv) grade(ws string, assets map[string][]byte, criteria ...model.Criterion) map[string]model.CheckResult {
	e.t.Helper()
	ev, err := e.engine.Evaluate(context.Background(), e.target(ws, assets, criteria...))
	testutil.NoError(e.t, err)
	raw, _ := json.Marshal(ev)
	e.evidence = append(e.evidence, string(raw))
	out := map[string]model.CheckResult{}
	for _, r := range ev.Results {
		line := fmt.Sprintf("%-12s %-5s %s", r.CriterionID, r.Status, r.Reason)
		e.logged = append(e.logged, line)
		e.t.Log(line)
		out[r.CriterionID] = r
	}
	return out
}

func (e *artEnv) expect(res map[string]model.CheckResult, id, status string, reason ...string) {
	e.t.Helper()
	r, ok := res[id]
	if !ok {
		e.t.Fatalf("no result for %s", id)
	}
	if r.Status != status {
		e.t.Fatalf("%s: status %s (%s), want %s", id, r.Status, r.Reason, status)
	}
	for _, f := range reason {
		if !strings.Contains(r.Reason, f) {
			e.t.Fatalf("%s: reason %q lacks %q", id, r.Reason, f)
		}
	}
}

func (e *artEnv) captureInputs(ws string, assets map[string][]byte, criteria ...model.Criterion) map[string]string {
	e.t.Helper()
	m, err := e.capturer.CaptureInputs(context.Background(), e.target(ws, assets, criteria...))
	testutil.NoError(e.t, err)
	return m
}

func crit(id, kind string, points int, ref model.ObjectRef, args map[string]any, timeout time.Duration) model.Criterion {
	return model.Criterion{ID: id, Kind: kind, Target: ref, Points: points, Args: args, Timeout: timeout}
}

// withDeps makes c wait for criteria that must have passed first: here the
// log lines exist before anything is asserted about their absence.
func withDeps(c model.Criterion, deps ...string) model.Criterion {
	c.DependsOn = deps
	return c
}

func nsRef(apiVersion, kind, name string) model.ObjectRef {
	return model.ObjectRef{APIVersion: apiVersion, Kind: kind, Namespace: artNamespace, Name: name}
}

func (e *artEnv) waitPod(name string) {
	e.t.Helper()
	e.kubectl("", "wait", "--for=condition=Ready", "pod/"+name, "-n", artNamespace, "--timeout=120s")
}

func dockerHas(t *testing.T, kind, ref string) bool {
	t.Helper()
	switch kind {
	case "image":
		res, _ := toolEnv.Run(context.Background(), "docker", []string{"image", "ls", "--format", "{{.Repository}}:{{.Tag}}", ref}, nil)
		return strings.Contains(string(res.Stdout), ref)
	default:
		res, _ := toolEnv.Run(context.Background(), "docker", []string{"ps", "-a", "--format", "{{.Names}}", "--filter", "name=^" + ref + "$"}, nil)
		return strings.TrimSpace(string(res.Stdout)) == ref
	}
}

func buildImage(t *testing.T, dir, tag string) string {
	t.Helper()
	run(t, "", "docker", "build", "--quiet", "--tag", tag, dir)
	t.Cleanup(func() { _, _ = toolEnv.Run(context.Background(), "docker", []string{"image", "rm", tag}, nil) })
	return strings.TrimSpace(run(t, "", "docker", "image", "inspect", "--format", "{{.Id}}", tag))
}

func TestArtifactGrading(t *testing.T) {
	requireTools(t, "docker", "kind", "kubectl", "helm")
	ctx := context.Background()

	// A foreign Docker container and image tag that JunKloud did not create.
	// They must survive everything below.
	foreignContainer := "jk-foreign-sentinel-" + randomHex(t, 3)
	foreignImage := "jk-foreign-sentinel:keep-" + randomHex(t, 3)
	run(t, "", "docker", "tag", environment.ProbeImage.Name, foreignImage)
	run(t, "", "docker", "create", "--name", foreignContainer, environment.ProbeImage.Name, "true")
	t.Cleanup(func() {
		_, _ = toolEnv.Run(ctx, "docker", []string{"rm", "-f", foreignContainer}, nil)
		_, _ = toolEnv.Run(ctx, "docker", []string{"image", "rm", foreignImage}, nil)
	})

	stopCapture := captureProcessOutput(t)
	fmt.Fprintln(os.Stderr, "output-capture-marker") // proves the capture below sees process output
	tools := &recordingTools{inner: grader.NewToolRunner()}

	m, id := bootEnvironment(t)
	testutil.NoError(t, m.EnsurePlatform(ctx, id))
	testutil.NoError(t, m.PreloadImages(ctx, id, environment.ProbeImage))

	redactor := grader.NewRedactor()
	opts := []grader.Option{
		grader.WithRedactor(redactor),
		grader.WithPreloadedImages(environment.ProbeImage.Name),
		grader.WithProbeImage(environment.ProbeImage.Name),
		grader.WithProbeControlNamespace(environment.PlatformNamespace),
		grader.WithInputKey([]byte("integration-test-installation-key")),
		grader.WithToolRunner(tools),
	}
	probes, err := grader.NewPodProbeRunner(m.KubeconfigPath(), id.Context, opts...)
	testutil.NoError(t, err)
	art, err := grader.NewClusterArtifactChecker(m.KubeconfigPath(), id.Context, probes, opts...)
	testutil.NoError(t, err)
	t.Cleanup(func() { _ = art.Close() })
	sec, err := grader.NewClusterSecurityChecker(m.KubeconfigPath(), id.Context, opts...)
	testutil.NoError(t, err)
	reader, err := grader.NewClusterReader(m.KubeconfigPath(), id.Context)
	testutil.NoError(t, err)
	checkers := grader.DefaultCheckers(reader, opts...)
	grader.AddArtifactCheckers(checkers, art)
	grader.AddSecurityCheckers(checkers, sec)
	e := &artEnv{t: t, m: m, id: id, engine: grader.NewEngine(checkers, opts...), capturer: art}

	e.kubectl("", "create", "namespace", artNamespace)
	e.kubectl(fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata: {name: db, namespace: %s}
stringData: {password: %s}
`, artNamespace, artSecret), "apply", "-f", "-")

	t.Run("FilesAndSymlinks", func(t *testing.T) {
		e.t = t
		outside := t.TempDir()
		testutil.NoError(t, os.WriteFile(filepath.Join(outside, "answer.yaml"), []byte("kind: ConfigMap\n"), 0o644))
		ws := t.TempDir()
		testutil.NoError(t, os.WriteFile(filepath.Join(ws, "answer.yaml"), []byte("# mine\nkind: ConfigMap\napiVersion: v1\nmetadata: {name: cfg}\ndata: {mode: fast}\n"), 0o644))
		testutil.NoError(t, os.Symlink(filepath.Join(outside, "answer.yaml"), filepath.Join(ws, "link.yaml")))
		fields := []any{map[string]any{"path": "data.mode", "equals": "fast"}}
		res := e.grade(ws, nil,
			crit("file", model.CriterionFile, 2, model.ObjectRef{}, map[string]any{"path": "answer.yaml", "format": "yaml", "fields": fields}, 0),
			crit("escape", model.CriterionFile, 2, model.ObjectRef{}, map[string]any{"path": "link.yaml", "format": "yaml"}, 0))
		e.expect(res, "file", "PASS")
		e.expect(res, "escape", "FAIL", "outside the session workspace")
	})

	t.Run("Logs", func(t *testing.T) {
		e.t = t
		e.kubectl(fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: talker, namespace: %s}
spec:
  containers:
  - name: main
    image: %s
    command: ["sh", "-c", "echo '{\"level\":\"info\",\"msg\":\"ready\"}'; echo password is $DB_PASSWORD; sleep 3600"]
    env:
    - {name: DB_PASSWORD, valueFrom: {secretKeyRef: {name: db, key: password}}}
    resources: {limits: {cpu: 50m, memory: 32Mi}}
`, artNamespace, environment.ProbeImage.Name), "apply", "-f", "-")
		e.waitPod("talker")
		pod := nsRef("v1", "Pod", "talker")
		res := e.grade("", nil,
			crit("ready", model.CriterionLogs, 2, pod, map[string]any{"wait": true, "json_fields": []any{map[string]any{"path": "msg", "equals": "ready"}}, "contains": []any{"password is"}}, 30*time.Second),
			withDeps(crit("never", model.CriterionLogs, 2, pod, map[string]any{"contains": []any{"never printed"}}, 0), "ready"),
			withDeps(crit("leak", model.CriterionLogs, 2, pod, map[string]any{"not_contains": []any{artSecret}}, 0), "ready"))
		e.expect(res, "ready", "PASS")
		e.expect(res, "never", "FAIL", "never printed")
		// The log really contains the Secret value; the criterion that forbids it fails without echoing it.
		if strings.Contains(evidenceJSON(t, res["leak"]), artSecret) {
			t.Fatalf("the forbidden secret is echoed in the result: %s", evidenceJSON(t, res["leak"]))
		}
		e.expect(res, "leak", "FAIL")
	})

	t.Run("Image", func(t *testing.T) {
		e.t = t
		tag := "jk-art-shop:" + randomHex(t, 4)
		firstID := buildImage(t, filepath.Join(fixtures, "image"), tag)
		ws := t.TempDir()
		run(t, "", "docker", "save", "--platform", "linux/"+goarch(), "--output", filepath.Join(ws, "shop.tar"), tag)
		timeout := 120 * time.Second
		img := func(id string, args map[string]any) model.Criterion {
			args["reference"] = tag
			return crit(id, model.CriterionImage, 2, model.ObjectRef{}, args, timeout)
		}
		cfg := map[string]any{"user": "10001", "entrypoint": []any{"/bin/sh", "/app/entry.sh"}, "cmd": []any{"print"},
			"exposed_ports": []any{"8080"}, "labels": map[string]any{"team": "grading-fixture"}}
		criteria := []model.Criterion{
			img("exists", map[string]any{"config": cfg}),
			img("missing", map[string]any{}),
			img("runs", map[string]any{"run": map[string]any{"stdout_contains": []any{"order service v2"}}}),
			img("exit3", map[string]any{"run": map[string]any{"args": []any{"fail"}, "expect_exit_code": 3}}),
			img("badexit", map[string]any{"run": map[string]any{"args": []any{"fail"}}}),
			img("wrongout", map[string]any{"run": map[string]any{"stdout_contains": []any{"something else"}}}),
			img("serves", map[string]any{"run": map[string]any{"args": []any{"serve"}, "timeout": "30s",
				"serve": map[string]any{"port": 8080, "body_marker": "shop-v2"}}}),
			img("notserving", map[string]any{"run": map[string]any{"serve": map[string]any{"port": 8080, "body_marker": "shop-v2"}}}),
			{ID: "archive", Kind: model.CriterionImage, Points: 2, Args: map[string]any{"archive": map[string]any{"path": "shop.tar", "repo_tag": tag}}},
			{ID: "absent", Kind: model.CriterionImage, Points: 2, Args: map[string]any{"reference": "jk-art-nothing:1"}},
		}
		criteria[1].Args["reference"] = "jk-art-missing:1"
		res := e.grade(ws, nil, criteria...)
		e.expect(res, "exists", "PASS")
		testutil.Equal(t, res["exists"].Evidence.InputHash, firstID)
		e.expect(res, "missing", "FAIL", "does not exist")
		e.expect(res, "runs", "PASS")
		e.expect(res, "exit3", "PASS")
		e.expect(res, "badexit", "FAIL", "exited with code 3")
		e.expect(res, "wrongout", "FAIL", "something else")
		e.expect(res, "serves", "PASS")
		e.expect(res, "notserving", "FAIL", "instead of serving")
		e.expect(res, "archive", "PASS")
		e.expect(res, "absent", "FAIL", "does not exist")

		// A changed image changes the fingerprint (TestInputChange against the real engine).
		inputCrit := img("exists", map[string]any{})
		before := e.captureInputs(ws, nil, inputCrit)
		testutil.Equal(t, before["image:"+tag], "id="+firstID)
		changed := t.TempDir()
		for _, f := range []string{"Dockerfile", "entry.sh"} {
			b, err := os.ReadFile(filepath.Join(fixtures, "image", f))
			testutil.NoError(t, err)
			testutil.NoError(t, os.WriteFile(filepath.Join(changed, f), b, 0o644))
		}
		testutil.NoError(t, os.MkdirAll(filepath.Join(changed, "www"), 0o755))
		testutil.NoError(t, os.WriteFile(filepath.Join(changed, "www", "index.html"), []byte("shop-v3 storefront\n"), 0o644))
		secondID := buildImage(t, changed, tag)
		if secondID == firstID {
			t.Fatal("rebuilding with other content gave the same image ID")
		}
		after := e.captureInputs(ws, nil, inputCrit)
		testutil.Equal(t, reflect.DeepEqual(before, after), false)

		// Nothing JunKloud made for the runs is left, in the engine or in the nodes.
		leftover := run(t, "", "docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "junkloud.local/grade-image")
		testutil.Equal(t, strings.TrimSpace(leftover), "")
		for _, node := range nodeContainers(t, id.ClusterName) {
			images := run(t, "", "docker", "exec", node, "crictl", "images")
			if strings.Contains(images, "junkloud.local/grade-image") {
				t.Fatalf("per-run image left in node %s:\n%s", node[:12], images)
			}
		}
		pods := e.kubectl("", "get", "pods", "-n", environment.PlatformNamespace, "-l", "junkloud.io/component=grading-probe", "-o", "name")
		testutil.Equal(t, strings.TrimSpace(pods), "")
	})

	t.Run("Helm", func(t *testing.T) {
		e.t = t
		chart := filepath.Join(fixtures, "chart", "web")
		e.helm("install", "rel1", chart, "-n", artNamespace, "--wait", "--timeout", "120s", "--set", "message=first", "--set", "dbPassword="+artHelmSecret)
		rel := func(name string, extra map[string]any) map[string]any {
			a := map[string]any{"release": name, "chart": map[string]any{"name": "web", "version": "0.1.0"}, "workloads_ready": true}
			for k, v := range extra {
				a[k] = v
			}
			return a
		}
		h := func(id, release string, extra map[string]any) model.Criterion {
			return crit(id, model.CriterionHelm, 2, model.ObjectRef{}, rel(release, extra), 60*time.Second)
		}
		// Wrong answer: the same manifests applied with kubectl and no release.
		raw := e.helm("template", "raw", chart, "-n", artNamespace)
		e.kubectl(raw, "apply", "-n", artNamespace, "-f", "-")
		res := e.grade("", nil,
			h("release", "rel1", nil),
			h("raw", "raw", nil),
			h("values", "rel1", map[string]any{"values": []any{map[string]any{"path": "message", "equals": "first"}}}),
			h("secretvalue", "rel1", map[string]any{"values": []any{map[string]any{"path": "dbPassword", "equals": "not-the-password"}}}),
			h("upgraded", "rel1", map[string]any{"min_revision": 2}))
		e.expect(res, "release", "PASS")
		e.expect(res, "raw", "FAIL", "no Helm release")
		e.expect(res, "values", "PASS")
		e.expect(res, "secretvalue", "FAIL")
		e.expect(res, "upgraded", "FAIL", "revision 1")
		// The raw objects exist and are even ready: the release is what is missing.
		e.kubectl("", "wait", "--for=condition=Available", "deployment/raw-web", "-n", artNamespace, "--timeout=120s")
		res = e.grade("", nil, h("raw", "raw", nil))
		e.expect(res, "raw", "FAIL", "no Helm release")

		before := e.captureInputs("", nil, h("release", "rel1", nil))
		e.helm("upgrade", "rel1", chart, "-n", artNamespace, "--wait", "--timeout", "120s", "--set", "message=second", "--set", "dbPassword="+artHelmSecret)
		res = e.grade("", nil, h("upgraded", "rel1", map[string]any{"min_revision": 2}), h("values", "rel1", map[string]any{"values": []any{map[string]any{"path": "message", "equals": "second"}}}))
		e.expect(res, "upgraded", "PASS")
		e.expect(res, "values", "PASS")
		after := e.captureInputs("", nil, h("release", "rel1", nil))
		testutil.Equal(t, reflect.DeepEqual(before, after), false)
		for _, v := range after {
			if strings.Contains(v, artHelmSecret) {
				t.Fatalf("helm values leak into the fingerprint: %v", after)
			}
		}
	})

	t.Run("Kustomize", func(t *testing.T) {
		e.t = t
		ws := t.TempDir()
		assets := map[string][]byte{}
		for _, d := range []string{"base", "overlay", "overlay-alt"} {
			testutil.NoError(t, os.MkdirAll(filepath.Join(ws, d), 0o755))
			entries, err := os.ReadDir(filepath.Join(fixtures, "kustomize", d))
			testutil.NoError(t, err)
			for _, f := range entries {
				b, err := os.ReadFile(filepath.Join(fixtures, "kustomize", d, f.Name()))
				testutil.NoError(t, err)
				testutil.NoError(t, os.WriteFile(filepath.Join(ws, d, f.Name()), b, 0o644))
				if d == "base" {
					assets["base/"+f.Name()] = b
				}
			}
		}
		kz := func(id, overlay string) model.Criterion {
			return crit(id, model.CriterionKustomize, 3, model.ObjectRef{}, map[string]any{
				"path": overlay, "base": map[string]any{"path": "base"},
				"expect": []any{map[string]any{"api_version": "apps/v1", "kind": "Deployment", "name": "web", "namespace": artNamespace,
					"fields": []any{map[string]any{"path": "spec.replicas", "equals": 2}, map[string]any{"path": "metadata.labels.env", "equals": "prod"}}}},
			}, 60*time.Second)
		}
		// Rendered well, but nothing applied yet.
		res := e.grade(ws, assets, kz("overlay", "overlay"))
		e.expect(res, "overlay", "FAIL", "not found")
		e.kubectl("", "apply", "-k", filepath.Join(ws, "overlay"))
		res = e.grade(ws, assets, kz("overlay", "overlay"), kz("alt", "overlay-alt"))
		e.expect(res, "overlay", "PASS")
		e.expect(res, "alt", "PASS") // the same meaning, written another way (TestOverlaySemantics, live)
		testutil.Equal(t, strings.HasPrefix(res["overlay"].Evidence.InputHash, "sha256:"), true)

		// Applied, then changed by hand: the deployed state no longer matches.
		e.kubectl("", "scale", "deployment/web", "-n", artNamespace, "--replicas=3")
		res = e.grade(ws, assets, kz("overlay", "overlay"))
		e.expect(res, "overlay", "FAIL", "deployed", "replicas")
		e.kubectl("", "scale", "deployment/web", "-n", artNamespace, "--replicas=2")

		// Fingerprint: status churn does not change it, a spec change does.
		web := crit("ready", model.CriterionReady, 1, nsRef("apps/v1", "Deployment", "web"), nil, 60*time.Second)
		e.kubectl("", "rollout", "status", "deployment/web", "-n", artNamespace, "--timeout=120s")
		before := e.captureInputs(ws, assets, web, kz("overlay", "overlay"))
		pods := strings.Fields(e.kubectl("", "get", "pods", "-n", artNamespace, "-l", "app=web", "-o", "name"))
		e.kubectl("", append([]string{"delete", "-n", artNamespace, "--wait=true"}, pods[:1]...)...)
		e.kubectl("", "rollout", "status", "deployment/web", "-n", artNamespace, "--timeout=120s")
		churn := e.captureInputs(ws, assets, web, kz("overlay", "overlay"))
		testutil.Equal(t, reflect.DeepEqual(before, churn), true)
		// A criterion that names the Service sees the Deployment behind it.
		e.kubectl("", "create", "service", "clusterip", "web-svc", "-n", artNamespace, "--tcp=80:8080")
		e.kubectl("", "set", "selector", "service/web-svc", "-n", artNamespace, "app=web")
		tcp := crit("tcp", model.CriterionTCP, 1, nsRef("v1", "Service", "web-svc"), map[string]any{"port": 80, "source": map[string]any{}}, 60*time.Second)
		viaService := e.captureInputs(ws, assets, tcp)
		if _, ok := viaService["object:apps/v1/Deployment/"+artNamespace+"/web"]; !ok {
			t.Fatalf("the Deployment behind the Service is not fingerprinted: %v", viaService)
		}
		webBefore := e.captureInputs(ws, assets, web)
		e.kubectl("", "label", "deployment/web", "-n", artNamespace, "tier=frontend")
		testutil.Equal(t, reflect.DeepEqual(webBefore, e.captureInputs(ws, assets, web)), false) // a learner's label is an input
		e.kubectl("", "set", "env", "deployment/web", "-n", artNamespace, "MARK=changed")
		changed := e.captureInputs(ws, assets, web, kz("overlay", "overlay"))
		testutil.Equal(t, reflect.DeepEqual(before, changed), false)
		testutil.NoError(t, os.WriteFile(filepath.Join(ws, "base", "deployment.yaml"), []byte("# edited\n"), 0o644))
		res = e.grade(ws, assets, kz("overlay", "overlay"))
		e.expect(res, "overlay", "FAIL", "base")
	})

	t.Run("JobsAndCronJobs", func(t *testing.T) {
		e.t = t
		e.kubectl(fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata: {name: report, namespace: %[1]s}
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
      - {name: main, image: %[2]s, command: ["sh", "-c", "echo done"], resources: {limits: {cpu: 50m, memory: 32Mi}}}
---
apiVersion: batch/v1
kind: Job
metadata: {name: broken, namespace: %[1]s}
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
      - {name: main, image: %[2]s, command: ["sh", "-c", "exit 1"], resources: {limits: {cpu: 50m, memory: 32Mi}}}
---
apiVersion: batch/v1
kind: CronJob
metadata: {name: declared, namespace: %[1]s}
spec:
  schedule: "0,5,10,15,20,25,30,35,40,45,50,55 * * * *"
  concurrencyPolicy: Forbid
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: OnFailure
          containers:
          - {name: main, image: %[2]s, command: ["date"], resources: {limits: {cpu: 50m, memory: 32Mi}}}
---
apiVersion: batch/v1
kind: CronJob
metadata: {name: yearly, namespace: %[1]s}
spec:
  schedule: "0 0 1 1 *"
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: OnFailure
          containers:
          - {name: main, image: %[2]s, command: ["date"], resources: {limits: {cpu: 50m, memory: 32Mi}}}
---
apiVersion: batch/v1
kind: CronJob
metadata: {name: ticker, namespace: %[1]s}
spec:
  schedule: "* * * * *"
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: OnFailure
          containers:
          - {name: main, image: %[2]s, command: ["date"], resources: {limits: {cpu: 50m, memory: 32Mi}}}
`, artNamespace, environment.ProbeImage.Name), "apply", "-f", "-")
		// A Job made by hand from the CronJob: it exists and even completes.
		e.kubectl("", "create", "job", "manual-1", "--from=cronjob/yearly", "-n", artNamespace)
		e.kubectl("", "wait", "--for=condition=Complete", "job/manual-1", "-n", artNamespace, "--timeout=120s")

		res := e.grade("", nil,
			crit("job", model.CriterionJob, 2, nsRef("batch/v1", "Job", "report"), map[string]any{"succeeded": 1}, 90*time.Second),
			crit("broken", model.CriterionJob, 2, nsRef("batch/v1", "Job", "broken"), nil, 60*time.Second),
			crit("brokenexpected", model.CriterionJob, 2, nsRef("batch/v1", "Job", "broken"), map[string]any{"outcome": "failed", "reason": "BackoffLimitExceeded"}, 60*time.Second),
			crit("template", model.CriterionCronJobTemplate, 2, nsRef("batch/v1", "CronJob", "declared"), map[string]any{"schedule": "*/5 * * * *", "concurrency_policy": "Forbid"}, 0),
			crit("wrongtemplate", model.CriterionCronJobTemplate, 2, nsRef("batch/v1", "CronJob", "declared"), map[string]any{"schedule": "*/10 * * * *"}, 0),
			crit("manual", model.CriterionCronJobSchedule, 2, nsRef("batch/v1", "CronJob", "yearly"), map[string]any{"schedule": "* * * * *", "window": "75s"}, 100*time.Second),
			crit("ticks", model.CriterionCronJobSchedule, 2, nsRef("batch/v1", "CronJob", "ticker"), map[string]any{"schedule": "* * * * *", "window": "100s"}, 130*time.Second))
		e.expect(res, "job", "PASS")
		e.expect(res, "broken", "FAIL", "BackoffLimitExceeded")
		e.expect(res, "brokenexpected", "PASS")
		e.expect(res, "template", "PASS")
		e.expect(res, "wrongtemplate", "FAIL", "schedule")
		// TestCronScheduleNotProvedByManualJob against the real controller.
		// R23: the yearly schedule is not the declared per-minute one, whatever jobs exist.
		e.expect(res, "manual", "FAIL", "does not mean the expected schedule")
		e.expect(res, "ticks", "PASS")
	})

	t.Run("Authorization", func(t *testing.T) {
		e.t = t
		e.kubectl(fmt.Sprintf(`apiVersion: v1
kind: ServiceAccount
metadata: {name: reader, namespace: %[1]s}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: pod-reader, namespace: %[1]s}
rules: [{apiGroups: [""], resources: [pods], verbs: [get, list]}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: reader-pods, namespace: %[1]s}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: pod-reader}
subjects: [{kind: ServiceAccount, name: reader, namespace: %[1]s}]
---
apiVersion: v1
kind: Pod
metadata: {name: as-reader, namespace: %[1]s}
spec:
  serviceAccountName: reader
  containers: [{name: main, image: %[2]s, command: ["sleep", "3600"], resources: {limits: {cpu: 50m, memory: 32Mi}}}]
---
apiVersion: v1
kind: Pod
metadata: {name: as-default, namespace: %[1]s}
spec:
  containers: [{name: main, image: %[2]s, command: ["sleep", "3600"], resources: {limits: {cpu: 50m, memory: 32Mi}}}]
`, artNamespace, environment.ProbeImage.Name), "apply", "-f", "-")
		e.waitPod("as-reader")
		e.waitPod("as-default")
		sa := nsRef("v1", "ServiceAccount", "reader")
		checks := []any{
			map[string]any{"verb": "get", "resource": "pods", "allowed": true},
			map[string]any{"verb": "list", "resource": "pods", "allowed": true},
			map[string]any{"verb": "delete", "resource": "pods", "allowed": false},
			map[string]any{"verb": "get", "resource": "secrets", "allowed": false},
			map[string]any{"verb": "list", "resource": "pods", "cluster_scope": true, "allowed": false},
		}
		runsAs := func(pod string) map[string]any {
			return map[string]any{"api_version": "v1", "kind": "Pod", "name": pod, "identity": "token"}
		}
		res := e.grade("", nil,
			crit("least", model.CriterionAuthorization, 3, sa, map[string]any{"checks": checks, "runs_as": runsAs("as-reader")}, 60*time.Second),
			crit("spec-only", model.CriterionAuthorization, 3, sa, map[string]any{"checks": checks}, 0),
			crit("default", model.CriterionAuthorization, 3, sa, map[string]any{"checks": checks, "runs_as": runsAs("as-default")}, 60*time.Second))
		e.expect(res, "least", "PASS")
		e.expect(res, "spec-only", "PASS")
		// TestEffectiveIdentity: the SA has the right permissions but the pod does not run as it.
		e.expect(res, "default", "FAIL", "as-default", "default")
		// Overprivilege: bind the reader to cluster-wide edit rights.
		e.kubectl("", "create", "clusterrolebinding", "reader-edit", "--clusterrole=edit", "--serviceaccount="+artNamespace+":reader")
		res = e.grade("", nil, crit("least", model.CriterionAuthorization, 3, sa, map[string]any{"checks": checks}, 0))
		e.expect(res, "least", "FAIL", "must not be allowed")
		inputs := e.captureInputs("", nil, crit("least", model.CriterionAuthorization, 3, sa, map[string]any{"checks": checks}, 0))
		found := false
		for k := range inputs {
			found = found || strings.Contains(k, "ClusterRoleBinding") && strings.Contains(k, "reader-edit")
		}
		if !found {
			t.Fatalf("the binding that grants the service account is not in the fingerprint: %v", inputs)
		}
		e.kubectl("", "delete", "clusterrolebinding", "reader-edit")
	})

	t.Run("SecurityContext", func(t *testing.T) {
		e.t = t
		e.kubectl(fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: hardened, namespace: %[1]s}
spec:
  securityContext: {runAsUser: 10001, runAsGroup: 10001, runAsNonRoot: true, seccompProfile: {type: RuntimeDefault}}
  containers:
  - name: main
    image: %[2]s
    command: ["sleep", "3600"]
    securityContext:
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities: {drop: ["ALL"], add: ["NET_BIND_SERVICE"]}
    resources: {limits: {cpu: 50m, memory: 32Mi}}
---
apiVersion: v1
kind: Pod
metadata: {name: plain, namespace: %[1]s}
spec:
  containers: [{name: main, image: %[2]s, command: ["sleep", "3600"], resources: {limits: {cpu: 50m, memory: 32Mi}}}]
`, artNamespace, environment.ProbeImage.Name), "apply", "-f", "-")
		e.waitPod("hardened")
		e.waitPod("plain")
		hardened, plain := nsRef("v1", "Pod", "hardened"), nsRef("v1", "Pod", "plain")
		declared := []any{map[string]any{"run_as_user": 10001, "run_as_non_root": true, "read_only_root_filesystem": true,
			"allow_privilege_escalation": false, "seccomp": "RuntimeDefault", "capabilities": map[string]any{"drop": []any{"ALL"}, "add": []any{"NET_BIND_SERVICE"}}}}
		behavior := map[string]any{"uid": 10001, "gid": 10001, "caps_present": []any{"NET_BIND_SERVICE"}, "caps_absent": []any{"NET_RAW", "SETUID"}, "read_only_root": true}
		res := e.grade("", nil,
			crit("declared", model.CriterionSecurityContext, 3, hardened, map[string]any{"containers": declared}, 0),
			crit("behavior", model.CriterionSecurityContext, 3, hardened, map[string]any{"behavior": behavior}, 60*time.Second),
			crit("plain-declared", model.CriterionSecurityContext, 3, plain, map[string]any{"containers": declared}, 0),
			crit("plain-behavior", model.CriterionSecurityContext, 3, plain, map[string]any{"behavior": map[string]any{"non_root": true}}, 60*time.Second),
			crit("plain-caps", model.CriterionSecurityContext, 3, plain, map[string]any{"behavior": map[string]any{"caps_absent": []any{"NET_RAW"}, "read_only_root": true}}, 60*time.Second))
		e.expect(res, "declared", "PASS")
		e.expect(res, "behavior", "PASS")
		e.expect(res, "plain-declared", "FAIL")
		e.expect(res, "plain-behavior", "FAIL", "root")
		e.expect(res, "plain-caps", "FAIL")
	})

	t.Run("NothingLeaksAndNothingForeignIsTouched", func(t *testing.T) {
		e.t = t
		all := strings.Join(e.evidence, "\n")
		canaries := []string{artSecret, artHelmSecret, base64.StdEncoding.EncodeToString([]byte(artSecret)), base64.StdEncoding.EncodeToString([]byte(artHelmSecret))}
		for _, secret := range canaries {
			if strings.Contains(all, secret) {
				t.Fatalf("a secret value reached the stored evidence: %q", secret)
			}
		}
		// Nor does it reach what the grader prints or passes to other programs:
		// the process's stdout and stderr (the tests' logging, client-go's klog,
		// panics), the command lines of every tool invocation and their stderr.
		printed := stopCapture()
		if !strings.Contains(printed, "output-capture-marker") {
			t.Fatalf("the output capture does not see process output (%d bytes)", len(printed))
		}
		// t.Log lines (the verdicts and reasons shown to a person) are scanned too.
		printed += "\n" + strings.Join(e.logged, "\n")
		argLines, toolStderr := tools.seen()
		testutil.Equal(t, strings.Count(argLines, "\n") > 20, true) // the grader really ran many tools
		for _, secret := range canaries {
			for name, text := range map[string]string{"process stdout/stderr": printed, "tool command lines": argLines, "tool stderr": toolStderr} {
				if strings.Contains(text, secret) {
					t.Fatalf("a secret value reached the %s: %q", name, secret)
				}
			}
		}
		if !dockerHas(t, "container", foreignContainer) {
			t.Fatalf("the foreign container %s was removed", foreignContainer)
		}
		if !dockerHas(t, "image", foreignImage) {
			t.Fatalf("the foreign image %s was removed", foreignImage)
		}
		for _, leftover := range []string{"junkloud.local/grade-image"} {
			out := run(t, "", "docker", "image", "ls", "--format", "{{.Repository}}", leftover)
			testutil.Equal(t, strings.TrimSpace(out), "")
		}
	})
}

func evidenceJSON(t *testing.T, r model.CheckResult) string {
	raw, err := json.Marshal(r)
	testutil.NoError(t, err)
	return string(raw)
}

// recordingTools wraps the grader's real ToolRunner and keeps every
// invocation: the command lines (visible to any user via ps) and the tools'
// stderr (it ends up in error messages). stdout of tools such as
// `helm get values` is raw data the grader consumes in memory; what matters is
// what the grader emits, which captureProcessOutput watches.
type recordingTools struct {
	inner grader.ToolRunner
	mu    sync.Mutex
	args  []string
	errs  []string
}

func (r *recordingTools) Run(ctx context.Context, call grader.ToolCall) (grader.ToolResult, error) {
	res, err := r.inner.Run(ctx, call)
	r.mu.Lock()
	r.args = append(r.args, call.Exe+" "+strings.Join(call.Args, " ")+" "+strings.Join(call.Env, " ")+" "+string(call.Stdin))
	r.errs = append(r.errs, string(res.Stderr))
	r.mu.Unlock()
	return res, err
}

func (r *recordingTools) seen() (args, stderr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.args, "\n"), strings.Join(r.errs, "\n")
}

// captureProcessOutput tees this process's stdout and stderr (everything the
// grader, client-go and the test print) into a buffer until the returned
// function is called, which restores them and returns what was written.
func captureProcessOutput(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	origOut, origErr := os.Stdout, os.Stderr
	var readers sync.WaitGroup
	var writers []*os.File
	tee := func(orig *os.File) *os.File {
		r, w, err := os.Pipe()
		testutil.NoError(t, err)
		writers = append(writers, w)
		readers.Add(1)
		go func() {
			defer readers.Done()
			chunk := make([]byte, 4096)
			for {
				n, err := r.Read(chunk)
				if n > 0 {
					mu.Lock()
					buf.Write(chunk[:n])
					mu.Unlock()
					_, _ = orig.Write(chunk[:n])
				}
				if err != nil {
					return
				}
			}
		}()
		return w
	}
	os.Stdout, os.Stderr = tee(origOut), tee(origErr)
	stopped := false
	stop := func() string {
		if stopped {
			mu.Lock()
			defer mu.Unlock()
			return buf.String()
		}
		stopped = true
		os.Stdout, os.Stderr = origOut, origErr
		for _, w := range writers {
			_ = w.Close()
		}
		readers.Wait()
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
	t.Cleanup(func() { stop() })
	return stop
}

func goarch() string {
	out, _ := toolEnv.Run(context.Background(), "docker", []string{"version", "--format", "{{.Server.Arch}}"}, nil)
	return strings.TrimSpace(string(out.Stdout))
}
