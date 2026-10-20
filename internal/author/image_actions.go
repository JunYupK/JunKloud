//go:build linux

package author

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
)

type actionStep struct {
	kind, operation, path, reference string
	data                             []byte
	cluster                          environment.Action
	checkSaveReference               bool
	tool                             *toolAction
}

// Build the entire list against a private virtual workspace before effects.
// This also checks Dockerfiles supplied by an earlier private file action.
func preflightToolActions(ctx context.Context, p model.Problem, f model.FrozenProblem, ws string, actions []model.Action, provided []environment.PinnedImage, tools ToolActionConfig) (steps []actionStep, remove func(), resultErr error) {
	nativeCount := 0
	for _, a := range actions {
		if a.Kind == model.ActionHelm || a.Kind == model.ActionKustomize {
			nativeCount++
		}
	}
	if nativeCount > 8 {
		return nil, nil, invalid("at most8 native actions per list")
	}
	if nativeCount > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
	}
	virtual, err := os.MkdirTemp("", "junkloud-preflight-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := []string{virtual}
	remove = func() {
		for _, p := range cleanup {
			os.RemoveAll(p)
		}
	}
	defer func() {
		if resultErr != nil {
			remove()
		}
	}()
	if err = environment.CopyBuildContext(ws, virtual); err != nil {
		return nil, remove, invalid("unsafe workspace: %v", err)
	}
	builtReferences := map[string]bool{}
	var savedOutputs []string
	for _, a := range actions {
		if err := ctx.Err(); err != nil {
			return nil, remove, err
		}
		rendered, err := problem.RenderValue(a.Args, f.Parameters)
		if err != nil {
			return nil, remove, err
		}
		if rendered != nil {
			a.Args = rendered.(map[string]any)
		}
		step := actionStep{kind: a.Kind}
		switch a.Kind {
		case model.ActionFile:
			if err = onlyArgs(a.Args, "source", "path"); err != nil {
				return nil, remove, err
			}
			source, ok := a.Args["source"].(string)
			if !ok {
				return nil, remove, invalid("file source must name a private package file")
			}
			source, err = packagePath(source)
			if err != nil {
				return nil, remove, err
			}
			data, ok := p.Files[source]
			if !ok {
				return nil, remove, invalid("file source does not exist")
			}
			if nativeCount > 0 && len(data) > 32<<20 {
				return nil, remove, invalid("native action file exceeds 32 MiB")
			}
			dest, ok := a.Args["path"].(string)
			if !ok {
				return nil, remove, invalid("file path must be a string")
			}
			step.path, err = scopedPath(ws, dest, false)
			if err != nil {
				return nil, remove, err
			}
			vp, err := scopedPath(virtual, dest, false)
			if err != nil {
				return nil, remove, err
			}
			if err = os.MkdirAll(filepath.Dir(vp), 0700); err != nil {
				return nil, remove, err
			}
			if err = os.WriteFile(vp, data, 0600); err != nil {
				return nil, remove, err
			}
			step.data = data
		case model.ActionImage:
			if err = onlyArgs(a.Args, "operation", "reference", "path"); err != nil {
				return nil, remove, err
			}
			step.operation, _ = a.Args["operation"].(string)
			step.reference, _ = a.Args["reference"].(string)
			rel, ok := a.Args["path"].(string)
			if !ok || !environment.ValidBuildReference(step.reference) {
				return nil, remove, invalid("invalid image path/reference")
			}
			if step.operation != "build" && step.operation != "save" {
				return nil, remove, invalid("unsupported image operation")
			}
			step.path, err = scopedPath(ws, rel, step.operation == "build")
			if err != nil {
				return nil, remove, err
			}
			vp, err := scopedPath(virtual, rel, step.operation == "build")
			if err != nil {
				return nil, remove, err
			}
			if step.operation == "build" {
				for _, saved := range savedOutputs {
					rel, err := filepath.Rel(vp, saved)
					if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
						return nil, remove, invalid("a build context cannot depend on an earlier save output")
					}
				}
				if err = environment.ValidateBuildContext(vp, provided); err != nil {
					return nil, remove, err
				}
				builtReferences[step.reference] = true
			} else {
				if info, e := os.Stat(vp); e == nil && !info.Mode().IsRegular() {
					return nil, remove, invalid("image save output must be a regular file")
				}
				if err = os.MkdirAll(filepath.Dir(vp), 0700); err != nil {
					return nil, remove, invalid("image save parent is not a directory")
				}
				if err = os.WriteFile(vp, nil, 0600); err != nil {
					return nil, remove, invalid("image save output cannot be reserved")
				}
				savedOutputs = append(savedOutputs, vp)
				step.checkSaveReference = !builtReferences[step.reference]
			}
		case model.ActionHelm, model.ActionKustomize:
			if len(savedOutputs) > 0 {
				return nil, remove, invalid("tool inputs cannot depend on image save outputs")
			}
			step, err = renderToolAction(ctx, virtual, f.Namespace, a, tools)
			if step.tool != nil {
				cleanup = append(cleanup, step.tool.root)
			}
			if err != nil {
				return nil, remove, err
			}
		default:
			step.cluster, err = buildAction(p.Files, f.Parameters, a)
			if err != nil {
				return nil, remove, err
			}
		}
		steps = append(steps, step)
	}
	return steps, remove, nil
}
