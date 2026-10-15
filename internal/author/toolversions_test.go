//go:build linux

package author_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// fakeTools answers the version commands from a table keyed by
// "exe arg arg ...".
type fakeTools struct {
	mu      sync.Mutex
	answers map[string]grader.ToolResult
	errs    map[string]error
	calls   []grader.ToolCall
	deadl   []bool
}

func (f *fakeTools) Run(ctx context.Context, call grader.ToolCall) (grader.ToolResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	_, hasDeadline := ctx.Deadline()
	f.deadl = append(f.deadl, hasDeadline)
	key := call.Exe + " " + strings.Join(call.Args, " ")
	if err, ok := f.errs[key]; ok {
		return grader.ToolResult{}, err
	}
	res, ok := f.answers[key]
	if !ok {
		return grader.ToolResult{}, errors.New("unexpected call: " + key)
	}
	return res, nil
}

const (
	helmCmd    = "helm version --short"
	kubectlCmd = "kubectl version --client -o json"
	dockerCmd  = "docker version --format {{json .}}"
)

func goodTools() *fakeTools {
	return &fakeTools{answers: map[string]grader.ToolResult{
		helmCmd:    {Stdout: []byte("v4.3.0+gbec5b06\n")},
		kubectlCmd: {Stdout: []byte(`{"clientVersion":{"major":"1","minor":"37","gitVersion":"v1.37.0"},"kustomizeVersion":"v5.7.1"}`)},
		dockerCmd:  {Stdout: []byte(`{"Client":{"Version":"29.8.2","ApiVersion":"1.56"},"Server":{"Version":"29.8.2","ApiVersion":"1.56"}}`)},
	}}
}

func TestDetectToolVersions(t *testing.T) {
	tools := goodTools()
	got, err := author.DetectToolVersions(context.Background(), tools)
	testutil.NoError(t, err)
	testutil.Equal(t, got, map[string]string{
		"helm": "v4.3.0+gbec5b06", "kubectl": "v1.37.0", "docker-client": "29.8.2", "docker-server": "29.8.2",
	})
	// Every call is bounded and goes through the injected runner, as an
	// argument vector.
	testutil.Equal(t, len(tools.calls), 3)
	for i, d := range tools.deadl {
		if !d {
			t.Errorf("call %d has no deadline", i)
		}
	}
	// Deterministic.
	again, err := author.DetectToolVersions(context.Background(), goodTools())
	testutil.NoError(t, err)
	testutil.Equal(t, again, got)
}

func TestDetectToolVersionsFailsInsteadOfGuessing(t *testing.T) {
	tests := []struct {
		name string
		edit func(f *fakeTools)
		want string
	}{
		{"helm missing", func(f *fakeTools) { f.errs = map[string]error{helmCmd: grader.ErrToolUnavailable} }, "helm"},
		{"kubectl missing", func(f *fakeTools) { f.errs = map[string]error{kubectlCmd: grader.ErrToolUnavailable} }, "kubectl"},
		{"docker missing", func(f *fakeTools) { f.errs = map[string]error{dockerCmd: grader.ErrToolUnavailable} }, "docker"},
		{"timeout", func(f *fakeTools) { f.errs = map[string]error{helmCmd: context.DeadlineExceeded} }, "helm"},
		{"helm exits non-zero", func(f *fakeTools) {
			f.answers[helmCmd] = grader.ToolResult{ExitCode: 1, Stdout: []byte("v4.3.0+gbec5b06\n"), Stderr: []byte("boom")}
		}, "helm"},
		{"helm output is not a version", func(f *fakeTools) {
			f.answers[helmCmd] = grader.ToolResult{Stdout: []byte("hello world\n")}
		}, "helm"},
		{"helm output is empty", func(f *fakeTools) { f.answers[helmCmd] = grader.ToolResult{} }, "helm"},
		{"kubectl output is not JSON", func(f *fakeTools) {
			f.answers[kubectlCmd] = grader.ToolResult{Stdout: []byte("Client Version: v1.37.0")}
		}, "kubectl"},
		{"kubectl without a client version", func(f *fakeTools) {
			f.answers[kubectlCmd] = grader.ToolResult{Stdout: []byte(`{"clientVersion":{}}`)}
		}, "kubectl"},
		{"docker daemon down", func(f *fakeTools) {
			f.answers[dockerCmd] = grader.ToolResult{Stdout: []byte(`{"Client":{"Version":"29.8.2"},"Server":null}`)}
		}, "docker"},
		{"docker exits non-zero", func(f *fakeTools) {
			f.answers[dockerCmd] = grader.ToolResult{ExitCode: 1, Stdout: []byte(`{"Client":{"Version":"29.8.2"},"Server":{"Version":"29.8.2"}}`)}
		}, "docker"},
		{"docker client without a version", func(f *fakeTools) {
			f.answers[dockerCmd] = grader.ToolResult{Stdout: []byte(`{"Client":{},"Server":{"Version":"29.8.2"}}`)}
		}, "docker"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := goodTools()
			tt.edit(f)
			got, err := author.DetectToolVersions(context.Background(), f)
			if err == nil {
				t.Fatalf("accepted: %v", got)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not name %s", err, tt.want)
			}
			if got != nil {
				t.Errorf("returned a partial result: %v", got)
			}
		})
	}
}

func TestDetectToolVersionsHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeTools{answers: goodTools().answers}
	f.errs = map[string]error{helmCmd: ctx.Err()}
	if _, err := author.DetectToolVersions(ctx, f); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	_ = time.Second
}
