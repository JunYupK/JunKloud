//go:build linux

package environment

import (
	"context"
	"encoding/base64"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/JunYupK/JunKloud/internal/executil"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

var bg = context.Background()

// fakeRunner records every external command and answers from respond. A nil
// respond fails every command, so a test that expects zero commands notices.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []string
	respond func(exe string, args []string) (executil.Result, error)
}

func (f *fakeRunner) Run(_ context.Context, exe string, args []string, _ io.Reader) (executil.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, exe+" "+strings.Join(args, " "))
	f.mu.Unlock()
	if f.respond == nil {
		return executil.Result{ExitCode: -1}, executil.ErrStart
	}
	return f.respond(exe, args)
}

func (f *fakeRunner) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeRunner) count(prefix string) int {
	n := 0
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func openTestStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(dir, "junkloud.db"))
	testutil.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
