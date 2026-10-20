//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func t15File(source, path string) model.Action {
	return model.Action{Kind: model.ActionFile, Args: map[string]any{"source": source, "path": path}}
}
func t15Apply(path string) model.Action {
	return model.Action{Kind: model.ActionApply, Args: map[string]any{"path": path}}
}
func t15Helm() model.Action {
	return model.Action{Kind: model.ActionHelm, Args: map[string]any{"operation": "install", "release": "answer", "chart": "chart", "values": []any{"values.yaml"}}}
}
func t15Kustomize() model.Action {
	return model.Action{Kind: model.ActionKustomize, Args: map[string]any{"path": "overlay"}}
}

func t15HelmProblem(ns string) model.Problem {
	chart := `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-details
  namespace: {{ .Release.Namespace }}
data:
  message: {{ .Values.message | quote }}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}-web
  namespace: {{ .Release.Namespace }}
spec:
  replicas: 1
  selector: {matchLabels: {app: task15}}
  template:
    metadata: {labels: {app: task15}}
    spec:
      containers:
      - name: web
        image: {{ .Values.image | quote }}
        imagePullPolicy: Never
        command: ["sh", "-c", "sleep 3600"]
        resources: {requests: {cpu: 10m, memory: 16Mi}, limits: {cpu: 100m, memory: 32Mi}}
`
	files := map[string][]byte{
		problem.QuestionFile:                          []byte("Install the provided local chart with the reviewed values.\n"),
		"setup/provided/chart/Chart.yaml":             []byte("apiVersion: v2\nname: tool-chart\nversion: 0.1.0\n"),
		"setup/provided/chart/values.yaml":            []byte("message: initial\nimage: " + environment.ProbeImage.Name + "\n"),
		"setup/provided/chart/templates/objects.yaml": []byte(chart),
		"setup/provided/values.yaml":                  []byte("message: initial\n"),
		"solution/values.yaml":                        []byte("message: solved\n"),
		"cases/wrong-values.yaml":                     []byte("message: wrong\n"),
	}
	// Hand-applied equivalent objects still cannot prove native Helm release state.
	raw := strings.ReplaceAll(chart, "{{ .Release.Name }}", "answer")
	raw = strings.ReplaceAll(raw, "{{ .Release.Namespace }}", ns)
	raw = strings.ReplaceAll(raw, "{{ .Values.message | quote }}", `"solved"`)
	raw = strings.ReplaceAll(raw, "{{ .Values.image | quote }}", `"`+environment.ProbeImage.Name+`"`)
	files["cases/raw.yaml"] = []byte(raw)
	return model.Problem{SchemaVersion: 1, ID: "task15-tools-helm", Version: "1.0.0", Namespace: ns, Files: files,
		Criteria:        []model.Criterion{{ID: "release", Kind: model.CriterionHelm, Points: 1, Timeout: 60 * time.Second, Args: map[string]any{"release": "answer", "chart": map[string]any{"name": "tool-chart", "version": "0.1.0"}, "values": []any{map[string]any{"path": "message", "equals": "solved"}}, "workloads_ready": true}}},
		InitialExpected: map[string]string{"release": model.StatusFail},
		Solution:        []model.Action{t15File("solution/values.yaml", "values.yaml"), t15Helm()},
		Cases:           []model.Case{{ID: "raw-bypass", Actions: []model.Action{t15Apply("cases/raw.yaml")}, Expected: map[string]string{"release": model.StatusFail}}, {ID: "wrong-values", Actions: []model.Action{t15File("cases/wrong-values.yaml", "values.yaml"), t15Helm()}, Expected: map[string]string{"release": model.StatusFail}}},
	}
}

func t15OverlayProblem(ns string) model.Problem {
	base := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata: {name: content, namespace: %s}
data: {message: stable}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: %s}
spec:
  replicas: 1
  selector: {matchLabels: {app: overlay}}
  template:
    metadata: {labels: {app: overlay}}
    spec:
      volumes: [{name: data, emptyDir: {}}]
      containers:
      - name: web
        image: %s
        imagePullPolicy: Never
        command: ["sh", "-c", "sleep 3600"]
        volumeMounts: [{name: data, mountPath: /data}]
        resources: {requests: {cpu: 10m, memory: 16Mi}, limits: {cpu: 100m, memory: 32Mi}}
`, ns, ns, environment.ProbeImage.Name)
	union := strings.Replace(base, "volumes: [{name: data, emptyDir: {}}]", "volumes: [{name: data, emptyDir: null, configMap: {name: content}}]", 1)
	overlay := fmt.Sprintf("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamespace: %s\nresources: [../base]\n", ns)
	solution := overlay + `replicas: [{name: web, count: 2}]
patches:
- target: {kind: Deployment, name: web}
  patch: '[{"op":"replace","path":"/spec/template/spec/volumes/0","value":{"name":"data","configMap":{"name":"content"}}}]'
`
	alternative := overlay + `patches:
- patch: |-
    apiVersion: apps/v1
    kind: Deployment
    metadata: {name: web}
    spec:
      replicas: 2
      template:
        spec:
          volumes:
          - name: data
            emptyDir: null
            configMap: {name: content}
`
	files := map[string][]byte{problem.QuestionFile: []byte("Apply the overlay without changing the base.\n"), "setup/provided/base/kustomization.yaml": []byte("resources: [objects.yaml]\n"), "setup/provided/base/objects.yaml": []byte(base), "setup/provided/overlay/kustomization.yaml": []byte(overlay), "setup/initial.yaml": []byte(base), "solution/union.yaml": []byte(union), "solution/overlay.yaml": []byte(solution), "cases/alternative.yaml": []byte(alternative), "cases/changed-base.yaml": []byte(base + "\n# changed base\n")}
	correct := []model.Action{t15Apply("solution/union.yaml"), t15File("solution/overlay.yaml", "overlay/kustomization.yaml"), t15Kustomize()}
	alt := []model.Action{t15Apply("solution/union.yaml"), t15File("cases/alternative.yaml", "overlay/kustomization.yaml"), t15Kustomize()}
	wrong := []model.Action{t15Apply("solution/union.yaml"), t15File("solution/overlay.yaml", "overlay/kustomization.yaml"), t15File("cases/changed-base.yaml", "base/objects.yaml"), t15Kustomize()}
	return model.Problem{SchemaVersion: 1, ID: "task15-tools-overlay", Version: "1.0.0", Namespace: ns, Files: files,
		Setup: []model.Action{t15Apply("setup/initial.yaml")}, Solution: correct,
		Criteria:        []model.Criterion{{ID: "overlay", Kind: model.CriterionKustomize, Points: 1, Timeout: 60 * time.Second, Args: map[string]any{"path": "overlay", "base": map[string]any{"path": "base"}, "expect": []any{map[string]any{"api_version": "apps/v1", "kind": "Deployment", "name": "web", "fields": []any{map[string]any{"path": "spec.replicas", "equals": 2}, map[string]any{"path": "spec.template.spec.volumes[name=data].configMap.name", "equals": "content"}}}}}}},
		InitialExpected: map[string]string{"overlay": model.StatusFail}, Cases: []model.Case{{ID: "equivalent", Actions: alt, Expected: map[string]string{"overlay": model.StatusPass}, ExpectedScore: 1}, {ID: "base-modified", Actions: wrong, Expected: map[string]string{"overlay": model.StatusFail}}},
	}
}

// Fixture-only compile/render checks are safe to iterate before the frozen live run.
func TestTask15ToolFixtureRenders(t *testing.T) {
	for _, p := range []model.Problem{t15HelmProblem("jk-t15helm"), t15OverlayProblem("jk-t15overlay")} {
		f, err := problem.Freeze(p, 1)
		testutil.NoError(t, err)
		ws, remove, err := author.ScratchWorkspace(t.TempDir(), f)
		testutil.NoError(t, err)
		defer remove()
		for _, a := range p.Solution {
			if a.Kind == model.ActionFile {
				testutil.NoError(t, os.WriteFile(filepath.Join(ws, a.Args["path"].(string)), p.Files[a.Args["source"].(string)], 0600))
			}
		}
		var call grader.ToolCall
		if strings.HasSuffix(p.ID, "helm") {
			call = grader.ToolCall{Exe: "helm", Args: []string{"template", "answer", filepath.Join(ws, "chart"), "--namespace", f.Namespace, "--values", filepath.Join(ws, "values.yaml")}}
		} else {
			call = grader.ToolCall{Exe: "kubectl", Args: []string{"kustomize", "--load-restrictor=LoadRestrictionsRootOnly", filepath.Join(ws, "overlay")}}
		}
		out, err := grader.NewToolRunner().Run(context.Background(), call)
		testutil.NoError(t, err)
		if out.ExitCode != 0 {
			t.Fatalf("fixture render failed: %s", out.Stderr)
		}
		if !strings.Contains(string(out.Stdout), "kind: Deployment") {
			t.Fatal("missing workload")
		}
	}
}

type t15InterruptedInstall struct{ inner grader.ToolRunner }

func (r t15InterruptedInstall) Run(ctx context.Context, c grader.ToolCall) (grader.ToolResult, error) {
	out, err := r.inner.Run(ctx, c)
	if err == nil && out.ExitCode == 0 && c.Exe == "helm" && strings.Contains(" "+strings.Join(c.Args, " ")+" ", " install ") {
		return out, context.Canceled
	}
	return out, err
}

func TestTask15ToolActions(t *testing.T) {
	ctx := context.Background()
	phase := func(s string) { t.Logf("TASK15 %s %s", time.Now().UTC().Format(time.RFC3339), s) }
	phase("boot isolated no-ipv6 environment; compose actual NewRuntime/Validator")
	b := openBankRuntime(t)
	helmProblem := t15HelmProblem("jk-t15helm-" + b.id.ID)
	overlayProblem := t15OverlayProblem("jk-t15overlay-" + b.id.ID)
	certify := func(rt *author.Runtime, p model.Problem) {
		t.Helper()
		phase("actual runtime validation " + p.ID)
		rec, err := rt.Validator.Validate(ctx, p, []uint64{1})
		testutil.NoError(t, err)
		testutil.Equal(t, rec.Passed, true)
		for name, ev := range rec.Cases {
			for _, r := range ev.Results {
				t.Logf("TASK15 STAGE %s %s %s %s: %s", p.ID, name, r.CriterionID, r.Status, r.Reason)
			}
		}
		testutil.Equal(t, len(rec.Cases), 2+len(p.Cases)+1)
	}
	certify(b.rt, helmProblem)
	certify(b.rt, overlayProblem)
	phase("actual installed release interrupted after native effects; parent UID persists")
	f, err := problem.Freeze(helmProblem, 1)
	testutil.NoError(t, err)
	ws, remove, err := author.ScratchWorkspace(b.dir, f)
	testutil.NoError(t, err)
	defer remove()
	owner := environment.Owner{Inventory: model.Inventory{EnvironmentID: b.id.ID, SessionID: author.SessionID, ProblemID: helmProblem.ID}, Namespace: f.Namespace, CreateNamespaces: true}
	owner.Inventory, err = bankActionExecutor(t, b).Execute(ctx, b.id, owner, helmProblem, f, ws, nil)
	testutil.NoError(t, err)
	owner.CreateNamespaces = false
	reader, err := grader.NewClusterReader(b.m.KubeconfigPath(), b.id.Context)
	testutil.NoError(t, err)
	ex := author.NewActionExecutorWithTools(b.m, b.m, author.ToolActionConfig{Tools: t15InterruptedInstall{grader.NewToolRunner()}, Reader: reader, KubeconfigPath: b.m.KubeconfigPath(), Context: b.id.Context}, b.cfg.ProvidedImages()...)
	owner.Inventory, err = ex.Execute(ctx, b.id, owner, helmProblem, f, ws, helmProblem.Solution)
	testutil.ErrorIs(t, err, context.Canceled)
	persisted, err := b.st.GetInventory(ctx, b.id.ID, author.SessionID, helmProblem.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, persisted, owner.Inventory)
	secrets, err := reader.List(ctx, grader.ListQuery{APIVersion: "v1", Kind: "Secret", Namespace: f.Namespace, LabelSelector: "owner=helm,name=answer"})
	testutil.NoError(t, err)
	if len(secrets) != 1 {
		t.Fatalf("native interrupted release Secret count %d", len(secrets))
	}
	restarted, err := author.NewRuntime(ctx, b.m, b.st, b.id, b.cfg)
	testutil.NoError(t, err)
	defer restarted.Close()
	certify(restarted, helmProblem)
	_, err = reader.Get(ctx, model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: f.Namespace})
	testutil.ErrorIs(t, err, grader.ErrNotFound)
	phase("same-mark foreign namespace replacement is preserved; cleanup retains identity")
	owner.Inventory = model.Inventory{EnvironmentID: b.id.ID, SessionID: author.SessionID, ProblemID: helmProblem.ID}
	owner.CreateNamespaces = true
	owner.Inventory, err = bankActionExecutor(t, b).Execute(ctx, b.id, owner, helmProblem, f, ws, nil)
	testutil.NoError(t, err)
	client := kubeClient(t, b.m, b.id)
	old, err := client.CoreV1().Namespaces().Get(ctx, f.Namespace, metav1.GetOptions{})
	testutil.NoError(t, err)
	uid := old.UID
	testutil.NoError(t, client.CoreV1().Namespaces().Delete(ctx, f.Namespace, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}))
	deadline := time.Now().Add(90 * time.Second)
	for {
		_, e := client.CoreV1().Namespaces().Get(ctx, f.Namespace, metav1.GetOptions{})
		if apierrors.IsNotFound(e) {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if time.Now().After(deadline) {
			t.Fatal("namespace deletion timeout")
		}
		time.Sleep(250 * time.Millisecond)
	}
	foreign, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: f.Namespace, Labels: old.Labels, Annotations: old.Annotations}}, metav1.CreateOptions{})
	testutil.NoError(t, err)
	_, err = restarted.Validator.Validate(ctx, helmProblem, []uint64{1})
	if !errors.Is(err, environment.ErrNotOwned) {
		t.Fatalf("replacement must block recovery: %v", err)
	}
	got, err := client.CoreV1().Namespaces().Get(ctx, f.Namespace, metav1.GetOptions{})
	testutil.NoError(t, err)
	testutil.Equal(t, got.UID, foreign.UID)
	retained, err := b.st.GetInventory(ctx, b.id.ID, author.SessionID, helmProblem.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, retained, owner.Inventory)
	foreignUID := types.UID(foreign.UID)
	testutil.NoError(t, client.CoreV1().Namespaces().Delete(ctx, f.Namespace, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &foreignUID}}))
	// The authorized test-owned replacement is removed by its exact UID; the
	// retained old cleanup intent can now settle without adopting it.
	for {
		_, e := client.CoreV1().Namespaces().Get(ctx, f.Namespace, metav1.GetOptions{})
		if apierrors.IsNotFound(e) {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if time.Now().After(deadline) {
			t.Fatal("replacement deletion timeout")
		}
		time.Sleep(250 * time.Millisecond)
	}
	testutil.NoError(t, (environment.ManagedJanitor{Manager: b.m, Identity: b.id}).Cleanup(ctx, owner))
	phase("PASS native release/value state, raw bypass, equivalent overlays, base protection, SSA null transition, runtime composition and recovery")
}
