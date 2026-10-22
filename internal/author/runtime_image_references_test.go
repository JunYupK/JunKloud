//go:build linux

package author

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

type runtimeReferenceTools struct{ t *testing.T }

func (r runtimeReferenceTools) Run(_ context.Context, call grader.ToolCall) (grader.ToolResult, error) {
	var out string
	switch call.Exe {
	case "helm":
		out = "v4.3.0+gbec5b06"
	case "kubectl":
		out = `{"clientVersion":{"gitVersion":"v1.37.0"}}`
	case "docker":
		out = `{"Client":{"Version":"29.8.2"},"Server":{"Version":"24.0.6"}}`
	default:
		r.t.Fatalf("unexpected tool before static preflight boundary: %+v", call)
	}
	return grader.ToolResult{Stdout: []byte(out)}, nil
}

// Exercise NewRuntime's real Validator composition, ending at Prepare before
// platform mutations. A pinned FROM must be accepted just like the actual
// executor permits it; an unprovided digest must never reach that boundary.
func TestRuntimeValidatorProvidedImageReferences(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "sessions.db"))
	testutil.NoError(t, err)
	defer st.Close()
	m := environment.New(dir, st)
	testutil.NoError(t, os.MkdirAll(filepath.Dir(m.KubeconfigPath()), 0700))
	testutil.NoError(t, os.WriteFile(m.KubeconfigPath(), []byte(`apiVersion: v1
kind: Config
clusters:
- name: static
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: static
  context:
    cluster: static
    user: static
users:
- name: static
  user:
    token: static-test-token
current-context: static
`), 0600))
	id := model.Identity{ID: "0123456789ab", Context: "static", Versions: map[string]string{"kubernetes": "v1.37.0"}}
	rt, err := NewRuntime(ctx, m, st, id, GraderConfig{StateDir: dir, Tools: runtimeReferenceTools{t}})
	testutil.NoError(t, err)
	defer rt.Close()
	stop := errors.New("static preflight complete before runtime effects")
	rt.Validator.cfg.Prepare = func(context.Context) error { return stop }
	original, err := problem.Load(filepath.Join("..", "..", "problems", "image-001"))
	testutil.NoError(t, err)
	for _, tc := range []struct {
		name, from string
		allowed    bool
	}{
		{"configured pinned reference", environment.ProbeImage.Ref(), true},
		{"configured native alias", environment.ProbeImage.Name, true},
		{"foreign digest", environment.ProbeImage.Name + "@sha256:" + strings.Repeat("f", 64), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := original
			p.Files = maps.Clone(original.Files)
			for path, contents := range p.Files {
				if strings.HasSuffix(path, "Dockerfile") {
					p.Files[path] = []byte(strings.ReplaceAll(string(contents), environment.ProbeImage.Ref(), tc.from))
				}
			}
			seeds, err := CoveringSeeds(p)
			testutil.NoError(t, err)
			_, err = rt.Validator.Validate(ctx, p, seeds)
			if tc.allowed {
				if !errors.Is(err, stop) {
					t.Fatalf("configured FROM did not reach runtime boundary: %v", err)
				}
			} else if !errors.Is(err, ErrNotValid) || errors.Is(err, stop) {
				t.Fatalf("unprovided FROM was not refused statically: %v", err)
			}
		})
	}
}
