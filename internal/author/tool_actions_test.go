//go:build linux

package author_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

type actionReader struct {
	uid       string
	collision bool
}

func (r actionReader) Get(_ context.Context, ref model.ObjectRef) (map[string]any, error) {
	if ref.Kind == "Namespace" {
		return map[string]any{"metadata": map[string]any{"uid": r.uid, "labels": map[string]any{environment.LabelEnvironmentID: "0123456789ab", environment.LabelSessionID: "validation", environment.LabelProblemID: "tools", environment.LabelManagedBy: environment.ManagedByValue}, "annotations": map[string]any{environment.AnnotationOwner: "validation/tools"}}}, nil
	}
	if r.collision {
		return map[string]any{"metadata": map[string]any{"uid": "foreign"}}, nil
	}
	return nil, grader.ErrNotFound
}
func (r actionReader) List(context.Context, grader.ListQuery) ([]map[string]any, error) {
	return nil, nil
}

type actionTools struct {
	calls        []grader.ToolCall
	installBytes []string
	failInstall  bool
	installExit  int
	output       string
	helmOutput   string
	renderBytes  []string
}

func (r *actionTools) Run(ctx context.Context, c grader.ToolCall) (grader.ToolResult, error) {
	r.calls = append(r.calls, c)
	if err := ctx.Err(); err != nil {
		return grader.ToolResult{}, err
	}
	if c.Exe == "helm" && strings.Contains(strings.Join(c.Args, " "), " install ") {
		for i, a := range c.Args {
			if a == "install" {
				b, e := os.ReadFile(filepath.Join(c.Args[i+2], "templates", "cm.yaml"))
				if e != nil {
					return grader.ToolResult{}, e
				}
				r.installBytes = append(r.installBytes, string(b))
			}
		}
		if r.failInstall {
			return grader.ToolResult{}, context.Canceled
		}
		if r.installExit != 0 {
			return grader.ToolResult{ExitCode: r.installExit}, nil
		}
		return grader.ToolResult{}, nil
	}
	out := r.output
	if c.Exe == "helm" && r.helmOutput != "" {
		out = r.helmOutput
	}
	if c.Exe == "helm" {
		for i, a := range c.Args {
			if a == "template" {
				b, err := os.ReadFile(filepath.Join(c.Args[i+2], "values.yaml"))
				if err != nil {
					return grader.ToolResult{}, err
				}
				r.renderBytes = append(r.renderBytes, string(b))
			}
		}
	}
	if out == "" {
		out = "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: answer}\ndata: {value: rendered}\n"
	}
	return grader.ToolResult{Stdout: []byte(out)}, nil
}

func toolFixture(t *testing.T) (*harness, string, environment.Owner, model.Problem, model.FrozenProblem) {
	t.Helper()
	h := newHarness(t)
	ws := t.TempDir()
	files := map[string]string{"chart/Chart.yaml": "apiVersion: v2\nname: local\nversion: 0.1.0\n", "chart/values.yaml": "message: original\n", "chart/templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\n  namespace: {{ .Release.Namespace }}\ndata:\n  message: {{ .Values.message | quote }}\n", "overlay/kustomization.yaml": "resources: [cm.yaml]\n", "overlay/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: answer}\n"}
	for p, b := range files {
		testutil.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(ws, p)), 0700))
		testutil.NoError(t, os.WriteFile(filepath.Join(ws, p), []byte(b), 0600))
	}
	inv := model.Inventory{EnvironmentID: h.id.ID, SessionID: "validation", ProblemID: "tools", Resources: []model.OwnedResource{{Ref: model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: "jk-tools"}, UID: "ns-uid"}}}
	h.cluster.namespaces["jk-tools"] = true
	return h, ws, environment.Owner{Inventory: inv, Namespace: "jk-tools"}, model.Problem{Files: map[string][]byte{}}, model.FrozenProblem{Namespace: "jk-tools"}
}
func helmAction() model.Action {
	return model.Action{Kind: model.ActionHelm, Args: map[string]any{"operation": "install", "release": "answer", "chart": "chart", "values": []any{}}}
}
func kzAction() model.Action {
	return model.Action{Kind: "kustomize", Args: map[string]any{"path": "overlay"}}
}

// New local actions must reach native render/install instead of returning unsupported.
func TestToolActionsExecuteReviewedLocalInputs(t *testing.T) {
	for _, a := range []model.Action{helmAction(), kzAction()} {
		t.Run(a.Kind, func(t *testing.T) {
			h, ws, o, p, f := toolFixture(t)
			r := &actionTools{}
			ex := configuredActions(h, r, actionReader{uid: "ns-uid"})
			_, err := ex.Execute(context.Background(), h.id, o, p, f, ws, []model.Action{a})
			testutil.NoError(t, err)
			if a.Kind == model.ActionHelm {
				testutil.Equal(t, len(r.installBytes), 1)
			} else {
				testutil.Equal(t, len(h.cluster.applies), 1)
			}
		})
	}
}
func TestToolActionLaterInvalidHasNoEffects(t *testing.T) {
	h, ws, o, p, f := toolFixture(t)
	p.Files["solution/a"] = []byte("changed")
	r := &actionTools{}
	ex := configuredActions(h, r, actionReader{uid: "ns-uid"})
	_, err := ex.Execute(context.Background(), h.id, o, p, f, ws, []model.Action{{Kind: model.ActionFile, Args: map[string]any{"source": "solution/a", "path": "chart/values.yaml"}}, helmAction(), {Kind: "unknown"}})
	if err == nil {
		t.Fatal("accepted later invalid action")
	}
	b, _ := os.ReadFile(filepath.Join(ws, "chart/values.yaml"))
	if string(b) != "message: original\n" || len(h.cluster.applies) != 0 {
		t.Fatal("effects before complete preflight")
	}
	if len(r.installBytes) != 0 {
		t.Fatal("installed before complete preflight")
	}
}
func configuredActions(h *harness, r grader.ToolRunner, reader grader.ObjectReader) author.ActionExecutor {
	return author.NewActionExecutorWithTools(h.cluster, nil, author.ToolActionConfig{Tools: r, Reader: reader, KubeconfigPath: "/private/config", Context: h.id.Context})
}

func TestToolActionOrderedSnapshot(t *testing.T) {
	h, ws, o, p, f := toolFixture(t)
	r := &actionTools{}
	original, _ := os.ReadFile(filepath.Join(ws, "chart/templates/cm.yaml"))
	p.Files["solution/values"] = []byte("message: before\n")
	p.Files["solution/template"] = []byte("invalid after the install\n")
	actions := []model.Action{{Kind: model.ActionFile, Args: map[string]any{"source": "solution/values", "path": "chart/values.yaml"}}, helmAction(), {Kind: model.ActionFile, Args: map[string]any{"source": "solution/template", "path": "chart/templates/cm.yaml"}}}
	_, err := configuredActions(h, r, actionReader{uid: "ns-uid"}).Execute(context.Background(), h.id, o, p, f, ws, actions)
	testutil.NoError(t, err)
	testutil.Equal(t, r.renderBytes, []string{"message: before\n"})
	testutil.Equal(t, r.installBytes, []string{string(original)})
	b, _ := os.ReadFile(filepath.Join(ws, "chart/templates/cm.yaml"))
	testutil.Equal(t, string(b), "invalid after the install\n")
	// Native install consumes the same reviewed snapshot even though the list's
	// final virtual workspace has already received later file edits.
}

func TestToolActionRejectsUnsafeInputsBeforeNativeRender(t *testing.T) {
	tests := []struct {
		name, path, content string
		action              model.Action
	}{
		{"remote resource", "overlay/kustomization.yaml", "resources: [https://example.invalid/base]\n", kzAction()},
		{"remote patch", "overlay/kustomization.yaml", "patches: [{path: 'https://example.invalid/p'}]\n", kzAction()},
		{"remote legacy patch", "overlay/kustomization.yaml", "patchesStrategicMerge: ['https://example.invalid/p']\n", kzAction()},
		{"remote generator file", "overlay/kustomization.yaml", "configMapGenerator: [{name: x, files: ['key=https://example.invalid/p']}]\n", kzAction()},
		{"remote generator env", "overlay/kustomization.yaml", "secretGenerator: [{name: x, envs: ['https://example.invalid/p']}]\n", kzAction()},
		{"plugin generator", "overlay/kustomization.yaml", "generators: [gen.yaml]\n", kzAction()},
		{"helm inflation", "overlay/kustomization.yaml", "helmCharts: [{name: web, repo: 'https://example.invalid'}]\n", kzAction()},
		{"remote dependencies", "chart/Chart.yaml", "apiVersion: v2\nname: local\nversion: 0.1.0\ndependencies: [{name: evil, version: '1', repository: 'https://example.invalid'}]\n", helmAction()},
		{"lookup", "chart/templates/cm.yaml", "{{ lookup \"v1\" \"ConfigMap\" \"\" \"x\" }}", helmAction()},
		{"capabilities", "chart/templates/cm.yaml", "{{ .Capabilities.KubeVersion.Version }}", helmAction()},
		{"conditional", "chart/templates/cm.yaml", "{{ if .Values.x }}x{{ end }}", helmAction()},
		{"template invocation", "chart/templates/cm.yaml", "{{ define \"x\" }}x{{ end }}{{ template \"x\" . }}", helmAction()},
		{"indirect function", "chart/templates/cm.yaml", "{{ .Values.x | printf \"%s\" }}", helmAction()},
		{"crds", "chart/crds/evil.yaml", "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n", helmAction()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, ws, o, p, f := toolFixture(t)
			r := &actionTools{}
			testutil.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(ws, tc.path)), 0700))
			testutil.NoError(t, os.WriteFile(filepath.Join(ws, tc.path), []byte(tc.content), 0600))
			_, err := configuredActions(h, r, actionReader{uid: "ns-uid"}).Execute(context.Background(), h.id, o, p, f, ws, []model.Action{tc.action})
			if err == nil {
				t.Fatal("unsafe input accepted")
			}
			testutil.Equal(t, len(r.calls), 0)
			testutil.Equal(t, len(h.cluster.applies), 0)
		})
	}
}
func TestToolActionRejectsRenderedScope(t *testing.T) {
	for _, output := range []string{"apiVersion: v1\nkind: Namespace\nmetadata: {name: escape}\n", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: escape, namespace: other}\n", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: hook, annotations: {helm.sh/hook: pre-install}}\n"} {
		t.Run(output, func(t *testing.T) {
			h, ws, o, p, f := toolFixture(t)
			r := &actionTools{output: output}
			_, err := configuredActions(h, r, actionReader{uid: "ns-uid"}).Execute(context.Background(), h.id, o, p, f, ws, []model.Action{helmAction()})
			if err == nil {
				t.Fatal("unsafe render accepted")
			}
			testutil.Equal(t, len(r.installBytes), 0)
		})
	}
}
func TestToolActionForeignIdentityAndCancellation(t *testing.T) {
	for _, reader := range []actionReader{{uid: "replaced"}, {uid: "ns-uid", collision: true}} {
		h, ws, o, p, f := toolFixture(t)
		r := &actionTools{}
		_, err := configuredActions(h, r, reader).Execute(context.Background(), h.id, o, p, f, ws, []model.Action{helmAction()})
		if !errors.Is(err, environment.ErrNotOwned) {
			t.Fatalf("expected ownership rejection: %v", err)
		}
		testutil.Equal(t, len(r.installBytes), 0)
	}
	h, ws, o, p, f := toolFixture(t)
	r := &actionTools{failInstall: true}
	inv, err := configuredActions(h, r, actionReader{uid: "ns-uid"}).Execute(context.Background(), h.id, o, p, f, ws, []model.Action{helmAction()})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	testutil.Equal(t, inv, o.Inventory)
}

func TestToolActionMissingEscapingAndSymlinkSources(t *testing.T) {
	for _, kind := range []string{model.ActionHelm, model.ActionKustomize} {
		for _, variant := range []string{"missing", "escape", "symlink"} {
			t.Run(kind+variant, func(t *testing.T) {
				h, ws, o, p, f := toolFixture(t)
				r := &actionTools{}
				a := helmAction()
				key := "chart"
				if kind == model.ActionKustomize {
					a = kzAction()
					key = "path"
				}
				switch variant {
				case "missing":
					a.Args[key] = "missing"
				case "escape":
					a.Args[key] = "../outside"
				case "symlink":
					testutil.NoError(t, os.Symlink(t.TempDir(), filepath.Join(ws, "linked")))
					a.Args[key] = "linked"
				}
				_, err := configuredActions(h, r, actionReader{uid: "ns-uid"}).Execute(context.Background(), h.id, o, p, f, ws, []model.Action{a})
				if err == nil {
					t.Fatal("unsafe source accepted")
				}
				testutil.Equal(t, len(r.calls), 0)
				testutil.Equal(t, len(h.cluster.applies), 0)
			})
		}
	}
	for _, values := range []any{[]any{"missing.yaml"}, []any{"../escape"}, []any{"https://example.invalid/values"}, []any{"a", "b", "c", "d", "e"}, "not-a-list"} {
		t.Run(fmt.Sprint(values), func(t *testing.T) {
			h, ws, o, p, f := toolFixture(t)
			r := &actionTools{}
			a := helmAction()
			a.Args["values"] = values
			_, err := configuredActions(h, r, actionReader{uid: "ns-uid"}).Execute(context.Background(), h.id, o, p, f, ws, []model.Action{a})
			if err == nil {
				t.Fatal("invalid values accepted")
			}
			testutil.Equal(t, len(r.calls), 0)
		})
	}
}
func TestToolActionBoundsAndDefaultCompatibility(t *testing.T) {
	h, ws, o, p, f := toolFixture(t)
	for _, a := range []model.Action{helmAction(), kzAction()} {
		_, err := author.NewActionExecutor(h.cluster, nil).Execute(context.Background(), h.id, o, p, f, ws, []model.Action{a})
		testutil.ErrorIs(t, err, environment.ErrUnsupportedAction)
	}
	for _, output := range []string{strings.Repeat("x", (16<<20)+1), func() string {
		var b strings.Builder
		for i := 0; i < 129; i++ {
			fmt.Fprintf(&b, "---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: cm%d}\n", i)
		}
		return b.String()
	}()} {
		r := &actionTools{output: output}
		_, err := configuredActions(h, r, actionReader{uid: "ns-uid"}).Execute(context.Background(), h.id, o, p, f, ws, []model.Action{helmAction()})
		if err == nil {
			t.Fatal("unbounded render accepted")
		}
		testutil.Equal(t, len(r.installBytes), 0)
	}
	actions := make([]model.Action, 9)
	for i := range actions {
		actions[i] = kzAction()
	}
	r := &actionTools{}
	_, err := configuredActions(h, r, actionReader{uid: "ns-uid"}).Execute(context.Background(), h.id, o, p, f, ws, actions)
	if err == nil {
		t.Fatal("unbounded tool count accepted")
	}
	testutil.Equal(t, len(r.calls), 0)
}

type changingActionReader struct{ count int }

func (r *changingActionReader) Get(ctx context.Context, ref model.ObjectRef) (map[string]any, error) {
	uid := "ns-uid"
	if ref.Kind == "Namespace" {
		r.count++
		if r.count > 1 {
			uid = "replacement"
		}
	}
	return (actionReader{uid: uid}).Get(ctx, ref)
}
func (r *changingActionReader) List(ctx context.Context, q grader.ListQuery) ([]map[string]any, error) {
	return nil, nil
}
func TestToolActionRechecksParentBeforeEffect(t *testing.T) {
	h, ws, o, p, f := toolFixture(t)
	r := &actionTools{}
	_, err := configuredActions(h, r, &changingActionReader{}).Execute(context.Background(), h.id, o, p, f, ws, []model.Action{helmAction()})
	testutil.ErrorIs(t, err, environment.ErrNotOwned)
	testutil.Equal(t, len(r.installBytes), 0)
}

type deadlineActionTools struct{ t *testing.T }

func (r deadlineActionTools) Run(ctx context.Context, c grader.ToolCall) (grader.ToolResult, error) {
	d, ok := ctx.Deadline()
	if !ok || time.Until(d) > 2*time.Minute {
		r.t.Fatal("tool call has no bounded deadline")
	}
	return grader.ToolResult{}, context.DeadlineExceeded
}
func TestToolActionCancellationPreventsStageWrites(t *testing.T) {
	h, ws, o, p, f := toolFixture(t)
	p.Files["solution/values"] = []byte("changed")
	_, err := configuredActions(h, deadlineActionTools{t}, actionReader{uid: "ns-uid"}).Execute(context.Background(), h.id, o, p, f, ws, []model.Action{t15UnitFile("solution/values", "chart/values.yaml"), helmAction()})
	testutil.ErrorIs(t, err, context.DeadlineExceeded)
	b, _ := os.ReadFile(filepath.Join(ws, "chart/values.yaml"))
	testutil.Equal(t, string(b), "message: original\n")
}
func t15UnitFile(source, path string) model.Action {
	return model.Action{Kind: model.ActionFile, Args: map[string]any{"source": source, "path": path}}
}
func TestToolValidatorRejectsSetupBeforeEffects(t *testing.T) {
	h := newHarness(t)
	p := loadFixture(t)
	p.Setup = append(p.Setup, kzAction())
	v, err := author.NewValidator(author.Config{Identity: h.id, Env: h.cluster, Janitor: h.cluster, Engine: h.cluster, Store: h.store, LockPath: h.lockPath, GraderVersion: grader.GraderVersion, EnvironmentHash: h.envHash, ProvidedImages: h.provided, Actions: configuredActions(h, &actionTools{}, actionReader{uid: "ns-uid"})})
	testutil.NoError(t, err)
	_, err = v.Validate(context.Background(), p, []uint64{1})
	testutil.ErrorIs(t, err, author.ErrNotValid)
	testutil.Equal(t, len(h.cluster.applies), 0)
}
func TestToolValidatorRestoresParentUIDBeforeRecovery(t *testing.T) {
	h := newHarness(t)
	p := loadFixture(t)
	p.Files["setup/provided/overlay/kustomization.yaml"] = []byte("resources: [cm.yaml]\n")
	p.Files["setup/provided/overlay/cm.yaml"] = []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: answer}\n")
	p.Solution = []model.Action{kzAction()}
	f, err := problem.Freeze(p, 1)
	testutil.NoError(t, err)
	inv := model.Inventory{EnvironmentID: h.id.ID, SessionID: author.SessionID, ProblemID: p.ID, Resources: []model.OwnedResource{{Ref: model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: f.Namespace}, UID: "durable-parent"}}}
	testutil.NoError(t, h.store.PutInventory(context.Background(), inv))
	h.cluster.cleanupErr = context.Canceled
	v, err := author.NewValidator(author.Config{Identity: h.id, Env: h.cluster, Janitor: h.cluster, Engine: h.cluster, Store: h.store, LockPath: h.lockPath, GraderVersion: grader.GraderVersion, EnvironmentHash: h.envHash, ProvidedImages: h.provided, Actions: configuredActions(h, &actionTools{}, actionReader{uid: "durable-parent"})})
	testutil.NoError(t, err)
	_, err = v.Validate(context.Background(), p, []uint64{1})
	testutil.ErrorIs(t, err, context.Canceled)
	if len(h.cluster.cleanups) == 0 {
		t.Fatal("no recovery attempted")
	}
	testutil.Equal(t, h.cluster.cleanups[0].Inventory, inv)
	retained, err := h.store.GetInventory(context.Background(), h.id.ID, author.SessionID, p.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, retained, inv)
}

func TestToolInstallFailureIsNotAnAuthorCertificateFinding(t *testing.T) {
	h, ws, o, p, f := toolFixture(t)
	r := &actionTools{installExit: 1}
	inv, err := configuredActions(h, r, actionReader{uid: "ns-uid"}).Execute(context.Background(), h.id, o, p, f, ws, []model.Action{helmAction()})
	if err == nil || errors.Is(err, environment.ErrInvalidAction) {
		t.Fatalf("native install fault must abort validation and retain parent identity: %v", err)
	}
	testutil.Equal(t, inv, o.Inventory)
}

func TestToolRecoveryDoesNotAddDuplicateNamespacesToNextSetup(t *testing.T) {
	h := newHarness(t)
	p := loadFixture(t)
	p.Files["setup/provided/overlay/kustomization.yaml"] = []byte("resources: [cm.yaml]\n")
	p.Files["setup/provided/overlay/cm.yaml"] = []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: answer}\n")
	p.Solution = []model.Action{kzAction()}
	f, err := problem.Freeze(p, 1)
	testutil.NoError(t, err)
	inv := model.Inventory{EnvironmentID: h.id.ID, SessionID: author.SessionID, ProblemID: p.ID, Resources: []model.OwnedResource{{Ref: model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: f.Namespace}, UID: "old-parent"}}}
	testutil.NoError(t, h.store.PutInventory(context.Background(), inv))
	h.cluster.applyErr = func(_ int, o environment.Owner) error {
		for _, ns := range o.Scope.Namespaces {
			if ns == o.Namespace {
				return environment.ErrInvalidOwner
			}
		}
		return nil
	}
	v, err := author.NewValidator(author.Config{Identity: h.id, Env: h.cluster, Janitor: h.cluster, Engine: h.cluster, Store: h.store, LockPath: h.lockPath, GraderVersion: grader.GraderVersion, EnvironmentHash: h.envHash, ProvidedImages: h.provided, Actions: configuredActions(h, &actionTools{}, actionReader{uid: "old-parent"})})
	testutil.NoError(t, err)
	_, err = v.Validate(context.Background(), p, []uint64{1})
	if errors.Is(err, environment.ErrInvalidOwner) {
		t.Fatalf("recovery parent leaked into fresh stage scope: %v", err)
	}
	if len(h.cluster.applies) == 0 {
		t.Fatalf("no fresh setup reached: %v", err)
	}
}

type mixedCollisionReader struct{ live map[string]any }

func (r mixedCollisionReader) Get(ctx context.Context, ref model.ObjectRef) (map[string]any, error) {
	if ref.Kind == "Namespace" {
		return (actionReader{uid: "ns-uid"}).Get(ctx, ref)
	}
	if ref.Kind == "ConfigMap" && ref.Name == "answer" && r.live != nil {
		return r.live, nil
	}
	return nil, grader.ErrNotFound
}
func (r mixedCollisionReader) List(context.Context, grader.ListQuery) ([]map[string]any, error) {
	return nil, nil
}

func TestToolMixedListChecksInitialIdentityBeforeAnyEffects(t *testing.T) {
	for _, state := range []string{"same-mark-replaced", "unmarked-collision", "initially-absent"} {
		t.Run(state, func(t *testing.T) {
			h, ws, o, p, f := toolFixture(t)
			p.Files["solution/stage"] = []byte("stage changed")
			p.Files["solution/answer.yaml"] = []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: answer}\ndata: {value: initial}\n")
			ref := model.ObjectRef{APIVersion: "v1", Kind: "ConfigMap", Namespace: o.Namespace, Name: "answer"}
			var live map[string]any
			if state != "initially-absent" {
				o.Inventory.Resources = append(o.Inventory.Resources, model.OwnedResource{Ref: ref, UID: "recorded-A"})
				if state == "same-mark-replaced" {
					live, _ = (actionReader{uid: "replacement-B"}).Get(context.Background(), model.ObjectRef{Kind: "Namespace"})
				} else {
					live = map[string]any{"metadata": map[string]any{"uid": "foreign-B"}}
				}
			}
			r := &actionTools{helmOutput: "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: unrelated-helm}\ndata: {value: helper}\n"}
			ex := configuredActions(h, r, mixedCollisionReader{live: live})
			actions := []model.Action{t15UnitFile("solution/stage", "stage.txt"), helmAction(), {Kind: model.ActionApply, Args: map[string]any{"path": "solution/answer.yaml"}}, kzAction()}
			_, err := ex.Execute(context.Background(), h.id, o, p, f, ws, actions)
			if state == "initially-absent" {
				testutil.NoError(t, err)
				testutil.Equal(t, len(h.cluster.applies), 2)
				testutil.Equal(t, len(r.installBytes), 1)
				return
			}
			if !errors.Is(err, environment.ErrNotOwned) {
				t.Fatalf("earlier apply bypassed initial %s identity: %v", state, err)
			}
			if _, err := os.Stat(filepath.Join(ws, "stage.txt")); !os.IsNotExist(err) {
				t.Fatal("stage file changed before identity rejection")
			}
			testutil.Equal(t, len(h.cluster.applies), 0)
			testutil.Equal(t, len(r.installBytes), 0)
		})
	}
}
