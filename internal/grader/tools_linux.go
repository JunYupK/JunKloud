//go:build linux

package grader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/JunYupK/JunKloud/internal/executil"
)

const (
	toolTimeout   = 2 * time.Minute
	toolMaxOutput = 16 << 20
)

type execTools struct{ timeout time.Duration }

// NewToolRunner returns the ToolRunner backed by internal/executil: no shell,
// bounded time and output, and an environment without KUBECONFIG, so a tool
// can only reach a cluster that its arguments name explicitly.
func NewToolRunner() ToolRunner { return execTools{timeout: toolTimeout} }

// Run implements ToolRunner.
func (e execTools) Run(ctx context.Context, call ToolCall) (ToolResult, error) {
	env := executil.EnvWithout(os.Environ(), "KUBECONFIG", "HELM_KUBECONFIG", "HELM_KUBECONTEXT")
	env = append(env, call.Env...)
	r := executil.Runner{Timeout: e.timeout, MaxOutput: toolMaxOutput, Env: env, Dir: call.Dir}
	var stdin io.Reader
	if len(call.Stdin) > 0 {
		stdin = bytes.NewReader(call.Stdin)
	}
	res, err := r.Run(ctx, call.Exe, call.Args, stdin)
	out := ToolResult{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode}
	switch {
	case err == nil:
		return out, nil
	case errors.Is(err, executil.ErrExit) && res.ExitCode > 0:
		return out, nil
	case errors.Is(err, executil.ErrStart):
		return out, fmt.Errorf("%w: %v", ErrToolUnavailable, err)
	default:
		return out, err
	}
}
