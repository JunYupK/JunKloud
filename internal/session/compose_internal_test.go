//go:build linux

package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

type versionTools struct{}

func (versionTools) Run(_ context.Context, call grader.ToolCall) (grader.ToolResult, error) {
	switch call.Exe + " " + strings.Join(call.Args, " ") {
	case "helm version --short":
		return grader.ToolResult{Stdout: []byte("v4.3.0+gbec5b06\n")}, nil
	case "kubectl version --client -o json":
		return grader.ToolResult{Stdout: []byte(`{"clientVersion":{"gitVersion":"v1.37.0"}}`)}, nil
	case "docker version --format {{json .}}":
		return grader.ToolResult{Stdout: []byte(`{"Client":{"Version":"29.8.2"},"Server":{"Version":"29.8.2"}}`)}, nil
	}
	return grader.ToolResult{}, errors.New("unexpected call")
}

// TestNewTakesTheGraderConfigFromTheRuntime: there is exactly one
// GraderConfig, the runtime's. The environment manifest a session freezes
// (and compares certificates with) is computed from the runtime's own Images,
// so serving can never use another provided-image set than validation.
func TestNewTakesTheGraderConfigFromTheRuntime(t *testing.T) {
	img := environment.PinnedImage{Name: "docker.io/library/nginx:1.27.0", Digest: "sha256:" + strings.Repeat("a", 64)}
	rt := &author.Runtime{
		Config:   author.GraderConfig{StateDir: t.TempDir(), Images: []environment.PinnedImage{img}, Tools: versionTools{}},
		Grader:   &author.Grader{},
		Identity: model.Identity{ID: "env-1", Versions: map[string]string{"kind": "v0.33.0"}},
	}
	svc := New((*store.Store)(nil), (*environment.Manager)(nil), rt, t.TempDir(), rt.Config.StateDir)
	manifest, err := svc.cfg.EnvironmentManifest(context.Background(), rt.Identity)
	testutil.NoError(t, err)
	testutil.Equal(t, manifest["image:"+img.Name], img.Digest)
	want, err := rt.Config.EnvironmentManifest(context.Background(), rt.Identity)
	testutil.NoError(t, err)
	testutil.Equal(t, manifest, want)
}
