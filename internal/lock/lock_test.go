package lock_test

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/JunYupK/JunKloud/internal/lock"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

const (
	helperEnv     = "JUNKLOUD_LOCK_HELPER"
	helperPathEnv = "JUNKLOUD_LOCK_PATH"
)

// TestMain turns the test binary into a helper process when helperEnv is
// set. The helper tries to take the lock, reports "acquired" or "locked" on
// stdout, and (when it holds the lock) keeps it until stdin closes.
func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		os.Exit(runHelper())
	}
	os.Exit(m.Run())
}

func runHelper() int {
	release, err := lock.Acquire(os.Getenv(helperPathEnv))
	switch {
	case err == nil:
		os.Stdout.WriteString("acquired\n")
		io.Copy(io.Discard, os.Stdin) // hold until the parent closes stdin
		release()
		return 0
	case isLocked(err):
		os.Stdout.WriteString("locked\n")
		return 0
	default:
		os.Stdout.WriteString("error: " + err.Error() + "\n")
		return 1
	}
}

func isLocked(err error) bool { return errors.Is(err, lock.ErrLocked) }

type helper struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *bufio.Reader
}

func startHelper(t *testing.T, path string) *helper {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"=1", helperPathEnv+"="+path)
	in, err := cmd.StdinPipe()
	testutil.NoError(t, err)
	out, err := cmd.StdoutPipe()
	testutil.NoError(t, err)
	testutil.NoError(t, cmd.Start())
	h := &helper{cmd: cmd, stdin: in, out: bufio.NewReader(out)}
	t.Cleanup(func() {
		in.Close()
		cmd.Process.Kill()
		cmd.Wait()
	})
	return h
}

// result reads the helper's one-line report.
func (h *helper) result(t *testing.T) string {
	t.Helper()
	line, err := h.out.ReadString('\n')
	testutil.NoError(t, err)
	return strings.TrimSpace(line)
}

func TestLockAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env.lock")

	// A lock held by this process blocks another process.
	release, err := lock.Acquire(path)
	testutil.NoError(t, err)
	h := startHelper(t, path)
	testutil.Equal(t, h.result(t), "locked")
	testutil.NoError(t, h.cmd.Wait())

	// After release the other process can take it, and then we are locked out.
	testutil.NoError(t, release())
	h = startHelper(t, path)
	testutil.Equal(t, h.result(t), "acquired")
	_, err = lock.Acquire(path)
	testutil.ErrorIs(t, err, lock.ErrLocked)
	h.stdin.Close()
	testutil.NoError(t, h.cmd.Wait())

	// Once that process has exited the lock is free again.
	release, err = lock.Acquire(path)
	testutil.NoError(t, err)
	testutil.NoError(t, release())
}

func TestLockReclaimedAfterKill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env.lock")

	h := startHelper(t, path)
	testutil.Equal(t, h.result(t), "acquired")
	_, err := lock.Acquire(path)
	testutil.ErrorIs(t, err, lock.ErrLocked)

	// SIGKILL gives the process no chance to run release; the kernel must
	// drop the lock anyway.
	testutil.NoError(t, h.cmd.Process.Kill())
	h.cmd.Wait()

	release, err := lock.Acquire(path)
	testutil.NoError(t, err)
	testutil.NoError(t, release())
}

func TestLockExactlyOneWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env.lock")

	const n = 5
	hs := make([]*helper, n)
	for i := range hs {
		hs[i] = startHelper(t, path)
	}
	// Every helper reports before any releases: the winners keep the lock
	// until their stdin closes, so the losers' answer is "locked".
	acquired, locked := 0, 0
	for _, h := range hs {
		switch got := h.result(t); got {
		case "acquired":
			acquired++
		case "locked":
			locked++
		default:
			t.Fatalf("unexpected helper output %q", got)
		}
	}
	testutil.Equal(t, acquired, 1)
	testutil.Equal(t, locked, n-1)
}

func TestLockInProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "env.lock")
	// The parent directory does not exist: Acquire must not silently succeed.
	if _, err := lock.Acquire(path); err == nil {
		t.Fatal("Acquire in a missing directory succeeded")
	}

	path = filepath.Join(t.TempDir(), "env.lock")
	release, err := lock.Acquire(path)
	testutil.NoError(t, err)
	_, err = lock.Acquire(path)
	testutil.ErrorIs(t, err, lock.ErrLocked)

	testutil.NoError(t, release())
	testutil.NoError(t, release()) // release is idempotent

	release, err = lock.Acquire(path)
	testutil.NoError(t, err)
	testutil.NoError(t, release())
}

func TestLockFileRecordsOwnerPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env.lock")
	release, err := lock.Acquire(path)
	testutil.NoError(t, err)
	defer release()
	b, err := os.ReadFile(path)
	testutil.NoError(t, err)
	testutil.Equal(t, strings.TrimSpace(string(b)), strconv.Itoa(os.Getpid()))
}
