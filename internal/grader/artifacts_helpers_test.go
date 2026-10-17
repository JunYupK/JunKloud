package grader_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// fakeTools answers tool calls by a handler and records them.
type fakeTools struct {
	mu      sync.Mutex
	calls   []grader.ToolCall
	handler func(call grader.ToolCall) (grader.ToolResult, error)
}

func (f *fakeTools) Run(_ context.Context, call grader.ToolCall) (grader.ToolResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	h := f.handler
	f.mu.Unlock()
	if h == nil {
		return grader.ToolResult{}, fmt.Errorf("%w: no handler", grader.ErrToolUnavailable)
	}
	return h(call)
}

func (f *fakeTools) called() []grader.ToolCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]grader.ToolCall(nil), f.calls...)
}

// out is a successful tool result with stdout.
func out(stdout string) grader.ToolResult { return grader.ToolResult{Stdout: []byte(stdout)} }

// exit is a tool that ran and exited non-zero.
func exit(code int, stderr string) grader.ToolResult {
	return grader.ToolResult{ExitCode: code, Stderr: []byte(stderr)}
}

// joined renders a call for matching in handlers.
func joined(call grader.ToolCall) string { return call.Exe + " " + strings.Join(call.Args, " ") }

// fakePods serves logs and exec results by "namespace/pod/container".
type fakePods struct {
	mu     sync.Mutex
	logs   map[string]string
	logErr map[string]error
	execs  map[string]grader.ExecResult
	execFn func(key string, command []string) (grader.ExecResult, error)
	reads  []string
	ran    [][]string
}

func newPods() *fakePods {
	return &fakePods{logs: map[string]string{}, logErr: map[string]error{}, execs: map[string]grader.ExecResult{}}
}

func (f *fakePods) Logs(_ context.Context, ns, pod, container string, opts grader.LogOptions) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := ns + "/" + pod + "/" + container
	f.reads = append(f.reads, k)
	if err := f.logErr[k]; err != nil {
		return "", err
	}
	if s, ok := f.logs[k]; ok {
		return s, nil
	}
	return "", fmt.Errorf("%w: no log for %s", grader.ErrContainerNotStarted, k)
}

func (f *fakePods) Exec(_ context.Context, ns, pod, container string, command []string) (grader.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := ns + "/" + pod + "/" + container
	f.ran = append(f.ran, command)
	if f.execFn != nil {
		return f.execFn(k, command)
	}
	if r, ok := f.execs[k+" "+strings.Join(command, " ")]; ok {
		return r, nil
	}
	return grader.ExecResult{}, fmt.Errorf("no exec result for %s %v", k, command)
}

func (f *fakePods) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reads)
}

// workspace creates a session workspace with the given files.
func workspace(t testing.TB, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		testutil.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		testutil.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
	return root
}

// artifactEngine grades criteria with one ArtifactChecker (under the real
// engine, so redaction and normalisation apply).
func artifactEngine(cfg grader.ArtifactConfig, clock grader.Clock) (*grader.Engine, *grader.ArtifactChecker) {
	opts := []grader.Option{grader.WithClock(clock), grader.WithRedactor(grader.NewRedactor())}
	ck := grader.NewArtifactChecker(cfg, opts...)
	checkers := grader.DefaultCheckers(cfg.Reader, opts...)
	grader.AddArtifactCheckers(checkers, ck)
	return grader.NewEngine(checkers, opts...), ck
}

func artifactTarget(ws string, criteria ...model.Criterion) grader.Target {
	t := target(criteria...)
	t.Workspace = ws
	return t
}

// gradeOne evaluates one criterion and returns its result.
func gradeOne(t testing.TB, cfg grader.ArtifactConfig, ws string, c model.Criterion) model.CheckResult {
	t.Helper()
	eng, _ := artifactEngine(cfg, newClock())
	ev, err := eng.Evaluate(context.Background(), artifactTarget(ws, c))
	testutil.NoError(t, err)
	return ev.Results[0]
}

func fileCrit(args map[string]any) model.Criterion {
	return model.Criterion{ID: "c", Kind: model.CriterionFile, Points: 3, Args: args}
}

// reasonHas fails unless the result's reason contains every fragment.
func reasonHas(t testing.TB, res model.CheckResult, fragments ...string) {
	t.Helper()
	for _, f := range fragments {
		if !strings.Contains(res.Reason, f) {
			t.Errorf("reason %q lacks %q", res.Reason, f)
		}
	}
}

// evidenceJSON is the whole result as stored JSON, for "never contains" checks.
func evidenceJSON(t testing.TB, res model.CheckResult) string {
	t.Helper()
	raw, err := json.Marshal(res)
	testutil.NoError(t, err)
	return string(raw)
}

func isInvalid(err error) bool { return errors.Is(err, grader.ErrInvalidCriterion) }
