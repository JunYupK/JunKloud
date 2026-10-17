//go:build linux

// Package executil runs external programs (kind, docker) safely: arguments
// are passed as an array and never through a shell, output is bounded,
// context cancellation stops the program and every process it started, and
// the child environment can be set explicitly.
//
// Kubernetes API work does not belong here; it goes through client-go. Use
// this package only where an external tool's own semantics are needed, and
// parse only machine-readable output (IDs, JSON).
package executil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Sentinel errors. Match them with errors.Is. A cancelled or timed-out run
// returns an error matching context.Canceled or context.DeadlineExceeded.
var (
	// ErrStart: the program could not be started (for example not found).
	ErrStart = errors.New("executil: command could not start")
	// ErrExit: the program ran and exited with a non-zero status.
	ErrExit = errors.New("executil: command failed")
	// ErrOutputLimit: stdout or stderr exceeded the output limit; the
	// program was stopped and the captured output is incomplete.
	ErrOutputLimit = errors.New("executil: command output exceeded the limit")
)

// DefaultMaxOutput is the per-stream capture limit when Runner.MaxOutput is 0.
const DefaultMaxOutput = 4 << 20

// waitDelay bounds how long Run waits for output pipes after the program has
// exited or been killed, in case a process escaped the process group.
const waitDelay = 2 * time.Second

// Result is what a finished command produced. ExitCode is -1 when the
// program did not start or was killed by a signal.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Runner holds execution limits. The zero value is ready to use: no timeout
// beyond the context, DefaultMaxOutput per stream, and the current process
// environment.
type Runner struct {
	// Timeout, when positive, bounds each run in addition to the context.
	Timeout time.Duration
	// MaxOutput bounds each of stdout and stderr in bytes (0 = DefaultMaxOutput).
	MaxOutput int
	// Env is the complete child environment. nil inherits os.Environ().
	Env []string
	// Dir is the working directory ("" = the current directory).
	Dir string
}

// Run runs exe with args using a zero Runner. See Runner.Run.
func Run(ctx context.Context, exe string, args []string, stdin io.Reader) (Result, error) {
	return Runner{}.Run(ctx, exe, args, stdin)
}

// Run starts exe (looked up in PATH when it has no slash) with args, feeds it
// stdin (nil = empty), and waits for it. The program runs in its own process
// group; when ctx is done, the timeout passes or the output limit is hit, the
// whole group is killed and Run returns promptly. After a normal exit nothing
// is signalled (the group ID could already be reused); a straggler that keeps
// the output pipes open is cut off after a short grace period.
func (r Runner) Run(ctx context.Context, exe string, args []string, stdin io.Reader) (Result, error) {
	res := Result{ExitCode: -1}
	desc := describe(exe, args)
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return res, fmt.Errorf("%s: %w", desc, err)
	}
	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)

	limit := r.MaxOutput
	if limit <= 0 {
		limit = DefaultMaxOutput
	}
	stdout := &cappedBuffer{limit: limit, overflow: func() { stop(ErrOutputLimit) }}
	stderr := &cappedBuffer{limit: limit, overflow: func() { stop(ErrOutputLimit) }}

	cmd := exec.CommandContext(runCtx, exe, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Dir = r.Dir
	cmd.Env = r.Env
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Negative PID: signal the whole process group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = waitDelay

	if err := cmd.Start(); err != nil {
		return res, fmt.Errorf("%w: %s: %v", ErrStart, desc, err)
	}
	waitErr := cmd.Wait()
	res.Stdout, res.Stderr = stdout.buf.Bytes(), stderr.buf.Bytes()
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}

	if cause := context.Cause(runCtx); cause != nil && runCtx.Err() != nil {
		if errors.Is(cause, ErrOutputLimit) {
			return res, fmt.Errorf("%w: %s (limit %d bytes per stream)", ErrOutputLimit, desc, limit)
		}
		return res, fmt.Errorf("%s: %w", desc, ctx.Err())
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) && res.ExitCode > 0 {
			return res, fmt.Errorf("%w: %s: exit status %d: %s", ErrExit, desc, res.ExitCode, tail(res.Stderr))
		}
		return res, fmt.Errorf("%w: %s: %v: %s", ErrExit, desc, waitErr, tail(res.Stderr))
	}
	return res, nil
}

// EnvWithout returns env without the entries for the named variables.
func EnvWithout(env []string, names ...string) []string {
	out := make([]string, 0, len(env))
next:
	for _, kv := range env {
		for _, n := range names {
			if strings.HasPrefix(kv, n+"=") {
				continue next
			}
		}
		out = append(out, kv)
	}
	return out
}

// cappedBuffer keeps at most limit bytes and reports the first overflow.
type cappedBuffer struct {
	buf      bytes.Buffer
	limit    int
	full     bool
	overflow func()
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.full {
		return 0, ErrOutputLimit
	}
	room := c.limit - c.buf.Len()
	if len(p) <= room {
		return c.buf.Write(p)
	}
	c.buf.Write(p[:room])
	c.full = true
	c.overflow()
	return room, ErrOutputLimit
}

// describe names a command for error messages: the program and up to two
// arguments (enough for "kind create cluster", never long payloads).
func describe(exe string, args []string) string {
	parts := []string{exe}
	for i, a := range args {
		if i == 2 {
			parts = append(parts, "...")
			break
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// tail returns the last part of stderr for an error message.
func tail(b []byte) string {
	const max = 1024
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		s = "..." + s[len(s)-max:]
	}
	return s
}
