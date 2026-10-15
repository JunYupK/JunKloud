//go:build linux

package author_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

var params = map[string]any{"namespace": "jk-val-a", "replicas": 3, "name": "web"}

func applyAction(path string) model.Action {
	return model.Action{Kind: model.ActionApply, Args: map[string]any{"path": path}}
}

func TestBuildActionsApply(t *testing.T) {
	files := map[string][]byte{
		"solution/two.yaml": []byte(`# a leading comment
apiVersion: apps/v1
kind: Deployment
metadata: {name: "${name}", namespace: "${namespace}"}
spec:
  replicas: "${replicas}"
  template:
    spec:
      containers:
        - name: web
          command: ["sh", "-c", "echo ${name} in ${namespace}"]
---
# nothing but a comment
---
apiVersion: v1
kind: ConfigMap
metadata: {name: cfg, namespace: "${namespace}"}
data:
  port: "8080"
  ${notAParam}: kept
`),
	}
	got, err := author.BuildActions(files, params, []model.Action{applyAction("solution/two.yaml")})
	testutil.NoError(t, err)
	testutil.Equal(t, len(got), 1)
	testutil.Equal(t, got[0].Kind, model.ActionApply)
	testutil.Equal(t, len(got[0].Objects), 2)

	deploy := got[0].Objects[0]
	meta := deploy["metadata"].(map[string]any)
	testutil.Equal(t, meta["name"], "web")
	testutil.Equal(t, meta["namespace"], "jk-val-a")
	// A value that is exactly one reference keeps the parameter's own type.
	testutil.Equal(t, deploy["spec"].(map[string]any)["replicas"], 3)
	container := deploy["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	testutil.Equal(t, container["command"].([]any)[2], "echo web in jk-val-a")
	// Map keys are never rendered.
	cfgData := got[0].Objects[1]["data"].(map[string]any)
	testutil.Equal(t, cfgData["${notAParam}"], "kept")
	testutil.Equal(t, cfgData["port"], "8080")
	// The package content is not modified by rendering.
	if !strings.Contains(string(files["solution/two.yaml"]), `"${name}"`) {
		t.Fatal("rendering modified the package file")
	}
}

func TestBuildActionsDeleteOwned(t *testing.T) {
	a := model.Action{Kind: model.ActionDeleteOwned, Args: map[string]any{"targets": []any{
		map[string]any{"api_version": "apps/v1", "kind": "Deployment", "name": "${name}", "namespace": "${namespace}"},
		map[string]any{"api_version": "v1", "kind": "ConfigMap", "name": "cfg"},
	}}}
	got, err := author.BuildActions(nil, params, []model.Action{a})
	testutil.NoError(t, err)
	testutil.Equal(t, got[0].Kind, model.ActionDeleteOwned)
	testutil.Equal(t, got[0].Targets, []model.ObjectRef{
		{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "jk-val-a", Name: "web"},
		{APIVersion: "v1", Kind: "ConfigMap", Name: "cfg"},
	})
}

func TestBuildActionsRejects(t *testing.T) {
	files := map[string][]byte{
		"setup/ok.yaml":       []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\n"),
		"setup/empty.yaml":    []byte("# only a comment\n"),
		"setup/list.yaml":     []byte("- a\n- b\n"),
		"setup/broken.yaml":   []byte("kind: [unclosed\n"),
		"setup/dup.yaml":      []byte("kind: A\nkind: B\n"),
		"setup/undef.yaml":    []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: \"${nope}\"}\n"),
		"question.en.md":      []byte("# not an action file\n"),
		"setup/provided/x.md": []byte("x"),
	}
	cases := []struct {
		name   string
		action model.Action
		is     error
		text   string
	}{
		{"missing path", model.Action{Kind: model.ActionApply}, environment.ErrInvalidAction, "path"},
		{"unknown arg", model.Action{Kind: model.ActionApply, Args: map[string]any{"path": "setup/ok.yaml", "paths": "x"}}, environment.ErrInvalidAction, "paths"},
		{"non-string path", model.Action{Kind: model.ActionApply, Args: map[string]any{"path": 5}}, environment.ErrInvalidAction, "path"},
		{"absolute", applyAction("/etc/passwd"), environment.ErrInvalidAction, "relative"},
		{"parent escape", applyAction("setup/../../x.yaml"), environment.ErrInvalidAction, "clean"},
		{"leading dotdot", applyAction("../setup/ok.yaml"), environment.ErrInvalidAction, "clean"},
		{"unclean", applyAction("setup//ok.yaml"), environment.ErrInvalidAction, "clean"},
		{"backslash", applyAction(`setup\ok.yaml`), environment.ErrInvalidAction, "slash"},
		{"outside action dirs", applyAction("question.en.md"), environment.ErrInvalidAction, "under"},
		{"missing file", applyAction("setup/absent.yaml"), environment.ErrInvalidAction, "does not exist"},
		{"no object", applyAction("setup/empty.yaml"), environment.ErrInvalidAction, "no object"},
		{"not an object", applyAction("setup/list.yaml"), environment.ErrInvalidAction, "document 1"},
		{"broken yaml", applyAction("setup/broken.yaml"), environment.ErrInvalidAction, "document 1"},
		{"duplicate key", applyAction("setup/dup.yaml"), environment.ErrInvalidAction, "document 1"},
		{"undefined parameter", applyAction("setup/undef.yaml"), problem.ErrUndefinedParameter, "nope"},
		{"shell", model.Action{Kind: "shell", Args: map[string]any{"command": "kubectl get pods"}}, environment.ErrInvalidAction, "unknown action kind"},
		{"eval", model.Action{Kind: "eval"}, environment.ErrInvalidAction, "unknown action kind"},
		{"empty targets", model.Action{Kind: model.ActionDeleteOwned, Args: map[string]any{"targets": []any{}}}, environment.ErrInvalidAction, "targets"},
		{"target without name", model.Action{Kind: model.ActionDeleteOwned, Args: map[string]any{"targets": []any{map[string]any{"api_version": "v1", "kind": "Pod"}}}}, environment.ErrInvalidAction, "targets[0]"},
		{"target extra key", model.Action{Kind: model.ActionDeleteOwned, Args: map[string]any{"targets": []any{map[string]any{"api_version": "v1", "kind": "Pod", "name": "p", "force": "yes"}}}}, environment.ErrInvalidAction, "force"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := author.BuildActions(files, params, []model.Action{applyAction("setup/ok.yaml"), c.action})
			if err == nil {
				t.Fatalf("accepted: %+v", got)
			}
			testutil.ErrorIs(t, err, c.is)
			if !strings.Contains(err.Error(), c.text) || !strings.Contains(err.Error(), "action 1") {
				t.Fatalf("error %q lacks %q or the action index", err, c.text)
			}
			if got != nil {
				t.Fatalf("returned partial actions: %+v", got)
			}
		})
	}
}

// Kinds the validator cannot run yet are refused before anything is applied,
// with a clear error, instead of being skipped.
func TestBuildActionsUnsupportedKinds(t *testing.T) {
	for _, kind := range []string{model.ActionHelm, model.ActionImage, model.ActionFile, model.ActionPatch} {
		_, err := author.BuildActions(nil, params, []model.Action{{Kind: kind, Args: map[string]any{"path": "setup/x"}}})
		if !errors.Is(err, environment.ErrUnsupportedAction) || !strings.Contains(err.Error(), `"`+kind+`"`) {
			t.Errorf("%s: err = %v, want ErrUnsupportedAction naming the kind", kind, err)
		}
	}
}

func TestObjectImages(t *testing.T) {
	deployment := map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers":     []any{map[string]any{"name": "a", "image": "busybox:1.37.0"}, map[string]any{"name": "b", "image": "nginx"}},
			"initContainers": []any{map[string]any{"name": "i", "image": "docker.io/library/busybox:1.37.0"}},
		}}},
	}
	cron := map[string]any{
		"apiVersion": "batch/v1", "kind": "CronJob",
		"spec": map[string]any{"jobTemplate": map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers":          []any{map[string]any{"name": "c", "image": "registry.k8s.io/pause:3.10"}},
			"ephemeralContainers": []any{map[string]any{"name": "e", "image": "alpine:3.20"}},
		}}}}},
	}
	pod := map[string]any{"apiVersion": "v1", "kind": "Pod", "spec": map[string]any{"containers": []any{map[string]any{"name": "p", "image": "busybox:1.37.0"}}}}
	notImages := map[string]any{"kind": "ConfigMap", "data": map[string]any{"image": "not-an-image", "containers": "x"}}

	testutil.Equal(t, author.ObjectImages(deployment), []string{"busybox:1.37.0", "nginx", "docker.io/library/busybox:1.37.0"})
	testutil.Equal(t, author.ObjectImages(cron), []string{"registry.k8s.io/pause:3.10", "alpine:3.20"})
	testutil.Equal(t, author.ObjectImages(pod), []string{"busybox:1.37.0"})
	testutil.Equal(t, author.ObjectImages(notImages), []string(nil))
}
