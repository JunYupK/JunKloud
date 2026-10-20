//go:build linux

package author

import (
	"context"
	"os"

	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/localoverlay"
	"github.com/JunYupK/JunKloud/internal/model"
)

func renderKustomize(ctx context.Context, c ToolActionConfig, a model.Action, t *toolAction) (grader.ToolResult, error) {
	if err := onlyArgs(a.Args, "path"); err != nil {
		return grader.ToolResult{}, err
	}
	rel, ok := a.Args["path"].(string)
	if !ok {
		return grader.ToolResult{}, invalid("overlay path must be a string")
	}
	var err error
	t.path, err = scopedPath(t.root, rel, false)
	if err != nil {
		return grader.ToolResult{}, err
	}
	st, err := os.Stat(t.path)
	if err != nil || !st.IsDir() {
		return grader.ToolResult{}, invalid("overlay directory does not exist")
	}
	if _, err = localoverlay.Inspect(t.root, t.path); err != nil {
		return grader.ToolResult{}, invalid("unsafe local overlay: %v", err)
	}
	return toolRun(ctx, c, grader.ToolCall{Exe: "kubectl", Args: []string{"kustomize", "--load-restrictor=LoadRestrictionsRootOnly", t.path}, Dir: t.root})
}
