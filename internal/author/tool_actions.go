//go:build linux

package author

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// ToolActionConfig binds typed author actions to the verified local environment.
// A zero config deliberately leaves native actions unsupported.
type ToolActionConfig struct {
	Tools          grader.ToolRunner
	Reader         grader.ObjectReader
	KubeconfigPath string
	Context        string
}
type toolAction struct {
	root, path, release string
	namespace           string
	values              []string
}

const toolActionMaxOutput = 16 << 20

func (c ToolActionConfig) configured() bool {
	return c.Tools != nil && c.Reader != nil && c.KubeconfigPath != "" && c.Context != ""
}
func toolRun(ctx context.Context, c ToolActionConfig, call grader.ToolCall) (grader.ToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	r, err := c.Tools.Run(ctx, call)
	if err != nil {
		return r, err
	}
	if err = ctx.Err(); err != nil {
		return r, err
	}
	if len(r.Stdout) > toolActionMaxOutput || len(r.Stderr) > toolActionMaxOutput {
		return r, invalid("native tool output exceeds limit")
	}
	if r.ExitCode != 0 {
		return r, invalid("%s failed (exit %d)", call.Exe, r.ExitCode)
	}
	return r, nil
}

func renderToolAction(ctx context.Context, ws, ns string, a model.Action, c ToolActionConfig) (s actionStep, err error) {
	s.kind = a.Kind
	if !c.configured() {
		return s, fmt.Errorf("%w: local tools require explicit tools, reader, kubeconfig and context", environment.ErrUnsupportedAction)
	}
	root, err := os.MkdirTemp("", "junkloud-tool-action-*")
	if err != nil {
		return s, err
	}
	s.tool = &toolAction{root: root, namespace: ns}
	if err = environment.CopyBuildContext(ws, root); err != nil {
		return s, err
	}
	var out grader.ToolResult
	if a.Kind == model.ActionHelm {
		out, err = renderHelm(ctx, c, ns, a, s.tool)
	} else {
		out, err = renderKustomize(ctx, c, a, s.tool)
	}
	if err != nil {
		return s, err
	}
	docs, err := toolObjects(out.Stdout, ns)
	if err != nil {
		return s, err
	}
	s.cluster = environment.Action{Kind: model.ActionApply, Objects: docs}
	return s, nil
}

// Native author fixtures deliberately use a small built-in namespaced surface.
// Unknown/cluster resources cannot reach install even when problem Scope allows them.
func toolObjects(raw []byte, ns string) ([]map[string]any, error) {
	allowed := map[string]string{"ConfigMap": "v1", "Secret": "v1", "Service": "v1", "ServiceAccount": "v1", "Pod": "v1", "Deployment": "apps/v1", "StatefulSet": "apps/v1", "DaemonSet": "apps/v1", "Job": "batch/v1", "CronJob": "batch/v1"}
	d := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var docs []map[string]any
	seen := map[model.ObjectRef]bool{}
	for {
		var o map[string]any
		err := d.Decode(&o)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, invalid("unreadable native render: %v", err)
		}
		if len(o) == 0 {
			continue
		}
		kind, _ := o["kind"].(string)
		api, _ := o["apiVersion"].(string)
		if allowed[kind] == "" || allowed[kind] != api {
			return nil, invalid("unsupported rendered kind %s %s", api, kind)
		}
		m, ok := o["metadata"].(map[string]any)
		if !ok {
			return nil, invalid("rendered object needs metadata")
		}
		name, _ := m["name"].(string)
		ons, _ := m["namespace"].(string)
		if name == "" || m["generateName"] != nil || ons != "" && ons != ns {
			return nil, invalid("rendered object has invalid name/namespace")
		}
		if an, ok := m["annotations"].(map[string]any); ok {
			for _, key := range []string{"helm.sh/hook", "helm.sh/resource-policy"} {
				if _, present := an[key]; present {
					return nil, invalid("rendered object has unsupported Helm lifecycle annotation")
				}
			}
		}
		m["namespace"] = ns
		ref := model.ObjectRef{APIVersion: api, Kind: kind, Namespace: ns, Name: name}
		if seen[ref] {
			return nil, invalid("duplicate rendered object")
		}
		seen[ref] = true
		docs = append(docs, o)
		if len(docs) > 128 {
			return nil, invalid("too many rendered objects")
		}
	}
	if len(docs) == 0 {
		return nil, invalid("native action renders no objects")
	}
	if ns == "" {
		return nil, invalid("native action requires a frozen problem namespace")
	}
	return docs, nil
}

func (e *executor) verifyToolNamespace(ctx context.Context, id model.Identity, o environment.Owner) error {
	if !e.tools.configured() {
		return environment.ErrUnsupportedAction
	}
	if o.CreateNamespaces || o.Namespace == "" || o.Inventory.SessionID != SessionID || o.Inventory.EnvironmentID != id.ID || e.tools.Context != id.Context {
		return fmt.Errorf("%w: tool action needs an existing verified stage namespace", environment.ErrInvalidOwner)
	}
	ref := model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: o.Namespace}
	uid := ""
	for _, r := range o.Inventory.Resources {
		if r.Ref == ref {
			uid = r.UID
		}
	}
	if uid == "" {
		return fmt.Errorf("%w: tool namespace UID is not recorded", environment.ErrNotOwned)
	}
	obj, err := e.tools.Reader.Get(ctx, ref)
	if err != nil {
		return err
	}
	m, _ := obj["metadata"].(map[string]any)
	if m["uid"] != uid || m["deletionTimestamp"] != nil || !toolOwnerMarks(m, o.Inventory) {
		return fmt.Errorf("%w: tool namespace identity changed", environment.ErrNotOwned)
	}
	return nil
}
func toolOwnerMarks(m map[string]any, inv model.Inventory) bool {
	l, _ := m["labels"].(map[string]any)
	a, _ := m["annotations"].(map[string]any)
	return l[environment.LabelEnvironmentID] == inv.EnvironmentID && l[environment.LabelSessionID] == inv.SessionID && l[environment.LabelProblemID] == inv.ProblemID && l[environment.LabelManagedBy] == environment.ManagedByValue && a[environment.AnnotationOwner] == environment.OwnerID(inv)
}
func toolObjectRef(o map[string]any, ns string) model.ObjectRef {
	m, _ := o["metadata"].(map[string]any)
	n, _ := m["namespace"].(string)
	if n == "" {
		n = ns
	}
	api, _ := o["apiVersion"].(string)
	k, _ := o["kind"].(string)
	name, _ := m["name"].(string)
	return model.ObjectRef{APIVersion: api, Kind: k, Namespace: n, Name: name}
}
func (e *executor) preflightTools(ctx context.Context, id model.Identity, o environment.Owner, steps []actionStep) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	seen := map[model.ObjectRef]string{}
	releases := map[string]bool{}
	has := false
	for _, s := range steps {
		if s.tool != nil {
			if s.tool.namespace != o.Namespace {
				return environment.ErrInvalidOwner
			}
			has = true
			if err := e.verifyToolNamespace(ctx, id, o); err != nil {
				return err
			}
			if s.kind == model.ActionHelm {
				if releases[s.tool.release] {
					return invalid("duplicate Helm release")
				}
				releases[s.tool.release] = true
				secrets, err := e.tools.Reader.List(ctx, grader.ListQuery{APIVersion: "v1", Kind: "Secret", Namespace: o.Namespace, LabelSelector: "owner=helm,name=" + s.tool.release})
				if err != nil {
					return err
				}
				if len(secrets) > 0 {
					return fmt.Errorf("%w: release already exists", environment.ErrNotOwned)
				}
				storageRef := model.ObjectRef{APIVersion: "v1", Kind: "Secret", Namespace: o.Namespace, Name: "sh.helm.release.v1." + s.tool.release + ".v1"}
				for _, other := range steps {
					for _, obj := range other.cluster.Objects {
						if toolObjectRef(obj, o.Namespace) == storageRef {
							return invalid("action list collides with native Helm release storage")
						}
					}
				}
				if _, err := e.tools.Reader.Get(ctx, storageRef); err == nil {
					return fmt.Errorf("%w: Helm release storage collision", environment.ErrNotOwned)
				} else if !errors.Is(err, grader.ErrNotFound) {
					return err
				}
			}
		}
		if s.cluster.Kind == model.ActionDeleteOwned {
			for _, ref := range s.cluster.Targets {
				if ref.APIVersion == "v1" && ref.Kind == "Namespace" && ref.Name == o.Namespace {
					for _, other := range steps {
						if other.tool != nil {
							return invalid("tool list cannot delete its parent namespace")
						}
					}
				}
			}
		}
		for _, obj := range s.cluster.Objects {
			ref := toolObjectRef(obj, o.Namespace)
			if s.kind == model.ActionHelm && ref.Kind == "Secret" && ref.Name == "sh.helm.release.v1."+s.tool.release+".v1" {
				return invalid("chart collides with native Helm release storage")
			}
			if prior := seen[ref]; prior != "" && (prior == model.ActionHelm || s.kind == model.ActionHelm) {
				return invalid("action list collides with Helm object")
			}
			if s.tool != nil && seen[ref] == "" {
				live, err := e.tools.Reader.Get(ctx, ref)
				if err != nil && !errors.Is(err, grader.ErrNotFound) {
					return err
				}
				if err == nil {
					m, _ := live["metadata"].(map[string]any)
					owned := false
					for _, r := range o.Inventory.Resources {
						if r.Ref == ref && r.UID != "" && r.UID == m["uid"] && toolOwnerMarks(m, o.Inventory) {
							owned = true
						}
					}
					if s.kind == model.ActionHelm || !owned {
						return fmt.Errorf("%w: native object collision", environment.ErrNotOwned)
					}
				}
			}
			seen[ref] = s.kind
		}
	}
	if has {
		if pre, ok := e.env.(interface {
			PreflightActions(context.Context, model.Identity, environment.Owner, []environment.Action) error
		}); ok {
			var all []environment.Action
			for _, s := range steps {
				if s.cluster.Kind != "" {
					all = append(all, s.cluster)
				}
			}
			return pre.PreflightActions(ctx, id, o, all)
		}
	}
	return nil
}

func helmCall(c ToolActionConfig, t *toolAction, ns string, args ...string) grader.ToolCall {
	full := append([]string{"--kubeconfig", c.KubeconfigPath, "--kube-context", c.Context, "--namespace", ns}, args...)
	return grader.ToolCall{Exe: "helm", Args: full, Dir: t.root, Env: []string{"HELM_CACHE_HOME=" + filepath.Join(t.root, ".helm-cache"), "HELM_CONFIG_HOME=" + filepath.Join(t.root, ".helm-config"), "HELM_DATA_HOME=" + filepath.Join(t.root, ".helm-data")}}
}
