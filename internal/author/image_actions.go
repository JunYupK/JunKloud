//go:build linux

package author

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
)

type actionStep struct {
	kind, operation, path, reference string
	data                             []byte
	cluster                          environment.Action
	checkSaveReference               bool
}

// Build the entire list against a private virtual workspace before effects.
// This also checks Dockerfiles supplied by an earlier private file action.
func preflightActions(p model.Problem, f model.FrozenProblem, ws string, actions []model.Action, provided []environment.PinnedImage) ([]actionStep, error) {
	virtual, err := os.MkdirTemp("", "junkloud-preflight-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(virtual)
	if err = environment.CopyBuildContext(ws, virtual); err != nil {
		return nil, invalid("unsafe workspace: %v", err)
	}
	var steps []actionStep
	builtReferences := map[string]bool{}
	var savedOutputs []string
	for _, a := range actions {
		rendered, err := problem.RenderValue(a.Args, f.Parameters)
		if err != nil {
			return nil, err
		}
		if rendered != nil {
			a.Args = rendered.(map[string]any)
		}
		step := actionStep{kind: a.Kind}
		switch a.Kind {
		case model.ActionFile:
			if err = onlyArgs(a.Args, "source", "path"); err != nil {
				return nil, err
			}
			source, ok := a.Args["source"].(string)
			if !ok {
				return nil, invalid("file source must name a private package file")
			}
			source, err = packagePath(source)
			if err != nil {
				return nil, err
			}
			data, ok := p.Files[source]
			if !ok {
				return nil, invalid("file source does not exist")
			}
			dest, ok := a.Args["path"].(string)
			if !ok {
				return nil, invalid("file path must be a string")
			}
			step.path, err = scopedPath(ws, dest, false)
			if err != nil {
				return nil, err
			}
			vp, err := scopedPath(virtual, dest, false)
			if err != nil {
				return nil, err
			}
			if err = os.MkdirAll(filepath.Dir(vp), 0700); err != nil {
				return nil, err
			}
			if err = os.WriteFile(vp, data, 0600); err != nil {
				return nil, err
			}
			step.data = data
		case model.ActionImage:
			if err = onlyArgs(a.Args, "operation", "reference", "path"); err != nil {
				return nil, err
			}
			step.operation, _ = a.Args["operation"].(string)
			step.reference, _ = a.Args["reference"].(string)
			rel, ok := a.Args["path"].(string)
			if !ok || !environment.ValidBuildReference(step.reference) {
				return nil, invalid("invalid image path/reference")
			}
			if step.operation != "build" && step.operation != "save" {
				return nil, invalid("unsupported image operation")
			}
			step.path, err = scopedPath(ws, rel, step.operation == "build")
			if err != nil {
				return nil, err
			}
			vp, err := scopedPath(virtual, rel, step.operation == "build")
			if err != nil {
				return nil, err
			}
			if step.operation == "build" {
				for _, saved := range savedOutputs {
					rel, err := filepath.Rel(vp, saved)
					if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
						return nil, invalid("a build context cannot depend on an earlier save output")
					}
				}
				if err = environment.ValidateBuildContext(vp, provided); err != nil {
					return nil, err
				}
				builtReferences[step.reference] = true
			} else {
				if info, e := os.Stat(vp); e == nil && !info.Mode().IsRegular() {
					return nil, invalid("image save output must be a regular file")
				}
				if err = os.MkdirAll(filepath.Dir(vp), 0700); err != nil {
					return nil, invalid("image save parent is not a directory")
				}
				if err = os.WriteFile(vp, nil, 0600); err != nil {
					return nil, invalid("image save output cannot be reserved")
				}
				savedOutputs = append(savedOutputs, vp)
				step.checkSaveReference = !builtReferences[step.reference]
			}
		default:
			step.cluster, err = buildAction(p.Files, f.Parameters, a)
			if err != nil {
				return nil, err
			}
		}
		steps = append(steps, step)
	}
	return steps, nil
}
