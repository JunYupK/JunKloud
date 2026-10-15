package grader

import (
	"context"
	"errors"
)

// External tools (docker, helm, kubectl, kind) are reached through a
// ToolRunner so the checkers can be unit-tested without them. Arguments are
// always an array; nothing here goes through a shell, and no problem
// definition supplies an executable name or a free-form argument vector.

// ErrToolUnavailable: the program could not be started (not installed) or
// ran past its limits. It is an environment fault: graded ERROR.
var ErrToolUnavailable = errors.New("external tool unavailable")

// ToolCall is one program invocation.
type ToolCall struct {
	Exe   string
	Args  []string
	Stdin []byte
	// Env entries are added to the runner's base environment (the process
	// environment without any KUBECONFIG-style variable).
	Env []string
	Dir string
}

// ToolResult is what a program that ran produced. A non-zero ExitCode is not
// an error: callers interpret it (for example "No such image" is the
// learner's missing image, a dead daemon is not).
type ToolResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// ToolRunner runs programs. An error means the program could not run to an
// exit (not found, timed out, output limit, cancelled).
type ToolRunner interface {
	Run(ctx context.Context, call ToolCall) (ToolResult, error)
}
