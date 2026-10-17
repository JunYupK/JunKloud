//go:build linux

package executil_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/executil"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func TestRunCapturesOutputAndStdin(t *testing.T) {
	res, err := executil.Run(context.Background(), "sh", []string{"-c", "cat; echo err >&2"}, strings.NewReader("hello"))
	testutil.NoError(t, err)
	testutil.Equal(t, string(res.Stdout), "hello")
	testutil.Equal(t, string(res.Stderr), "err\n")
	testutil.Equal(t, res.ExitCode, 0)
}

func TestRunArgumentsAreNotShellInterpreted(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	arg := "$(touch " + marker + "); `touch " + marker + "`"
	res, err := executil.Run(context.Background(), "echo", []string{arg}, nil)
	testutil.NoError(t, err)
	testutil.Equal(t, string(res.Stdout), arg+"\n")
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("argument was interpreted by a shell: %v", err)
	}
}

func TestRunNonZeroExit(t *testing.T) {
	res, err := executil.Run(context.Background(), "sh", []string{"-c", "echo out; echo bad >&2; exit 3"}, nil)
	testutil.ErrorIs(t, err, executil.ErrExit)
	testutil.Equal(t, res.ExitCode, 3)
	testutil.Equal(t, string(res.Stdout), "out\n")
	if !strings.Contains(err.Error(), "bad") {
		t.Fatalf("exit error should carry stderr, got %v", err)
	}
}

func TestRunMissingExecutable(t *testing.T) {
	res, err := executil.Run(context.Background(), "junkloud-no-such-binary", nil, nil)
	testutil.ErrorIs(t, err, executil.ErrStart)
	testutil.Equal(t, res.ExitCode, -1)
}

func TestRunBoundsOutput(t *testing.T) {
	r := executil.Runner{MaxOutput: 1024}
	start := time.Now()
	// yes(1) never stops on its own: the runner must stop it at the limit.
	res, err := r.Run(context.Background(), "yes", nil, nil)
	testutil.ErrorIs(t, err, executil.ErrOutputLimit)
	if len(res.Stdout) > 1024 {
		t.Fatalf("stdout kept %d bytes, limit 1024", len(res.Stdout))
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("output limit did not stop the process promptly")
	}
}

// TestExecCancellation: cancelling the context stops the command and every
// process it started, and Run returns promptly with the context's error.
func TestExecCancellation(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	// The child sleep inherits stdout; without process-group cleanup Run
	// would block until it exits 60s later.
	_, err := executil.Run(ctx, "sh", []string{"-c", `sleep 60 & echo $! > "$0"; wait`, pidFile}, nil)
	testutil.ErrorIs(t, err, context.DeadlineExceeded)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Run returned after %v; cancellation was not prompt", d)
	}
	raw, rerr := os.ReadFile(pidFile)
	testutil.NoError(t, rerr)
	pid, perr := strconv.Atoi(strings.TrimSpace(string(raw)))
	testutil.NoError(t, perr)
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("child process %d survived cancellation", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// alive reports whether pid is a running (not zombie) process. A killed
// child may stay a zombie briefly until its new parent reaps it.
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// The state field follows the parenthesised command name.
	i := strings.LastIndexByte(string(stat), ')')
	return i < 0 || i+2 >= len(stat) || stat[i+2] != 'Z'
}

func TestRunnerTimeout(t *testing.T) {
	r := executil.Runner{Timeout: 200 * time.Millisecond}
	_, err := r.Run(context.Background(), "sleep", []string{"30"}, nil)
	testutil.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestRunnerEnvIsExplicit(t *testing.T) {
	t.Setenv("JUNKLOUD_AMBIENT", "leaked")
	r := executil.Runner{Env: []string{"PATH=" + os.Getenv("PATH"), "ONLY=this"}}
	res, err := r.Run(context.Background(), "sh", []string{"-c", `printf '%s|%s' "$ONLY" "$JUNKLOUD_AMBIENT"`}, nil)
	testutil.NoError(t, err)
	testutil.Equal(t, string(res.Stdout), "this|")
}

func TestEnvWithout(t *testing.T) {
	got := executil.EnvWithout([]string{"A=1", "KUBECONFIG=/x", "B=2", "KUBECONFIGX=3"}, "KUBECONFIG")
	testutil.Equal(t, got, []string{"A=1", "B=2", "KUBECONFIGX=3"})
}
