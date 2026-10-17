//go:build !linux

package grader

import (
	"context"
	"fmt"
)

type noTools struct{}

// NewToolRunner is only implemented on Linux, where the dev and workspace
// containers run; elsewhere every call reports the tool as unavailable.
func NewToolRunner() ToolRunner { return noTools{} }

// Run implements ToolRunner.
func (noTools) Run(_ context.Context, call ToolCall) (ToolResult, error) {
	return ToolResult{}, fmt.Errorf("%w: %s (external tools are only run on Linux)", ErrToolUnavailable, call.Exe)
}
