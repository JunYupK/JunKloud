package grader

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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

// kubernetesTools are the programs that talk to a Kubernetes API (or read a
// kubeconfig) and therefore get an allowlisted environment.
var kubernetesTools = map[string]bool{"helm": true, "kubectl": true}

// allowedEnv are the only variables a Kubernetes tool inherits: enough to
// find programs and render text. Nothing in the process environment can then
// redirect it to another API server or identity, change its namespace or
// driver, or relax TLS verification (HELM_KUBEAPISERVER, HELM_KUBETOKEN,
// HELM_KUBEASUSER, HELM_KUBECAFILE, HELM_KUBEINSECURE_SKIP_TLS_VERIFY,
// HELM_DRIVER, HELM_NAMESPACE, KUBECONFIG, KUBECTL_*, proxies, ...).
var allowedEnv = map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true, "TZ": true, "USER": true}

// childEnv builds the environment of one tool run from the process
// environment base and the call's own entries (extra, set by JunKloud, for
// example private HELM_*_HOME cache directories).
//
// helm and kubectl: base is reduced to allowedEnv (plus LC_*). Any other tool
// (docker, kind) keeps the process environment, which it needs to reach the
// shared engine (DOCKER_HOST, DOCKER_CONFIG, ...), minus every HELM_*,
// KUBE* and KUBECTL_* variable.
func childEnv(exe string, base, extra []string) []string {
	// Never nil: executil.Runner (and os/exec) inherit the whole process
	// environment from a nil Env, which would defeat the isolation whenever
	// nothing is allowed through.
	out := []string{}
	strict := kubernetesTools[filepath.Base(exe)]
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case strict && (allowedEnv[name] || strings.HasPrefix(name, "LC_")):
			out = append(out, kv)
		case !strict && !strings.HasPrefix(name, "HELM_") && !strings.HasPrefix(name, "KUBE"):
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}
