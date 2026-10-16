//go:build linux

package session

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/JunYupK/JunKloud/internal/problem"
)

// QuestionFile is the file Start writes the English question into, beside the
// provided files, in each problem's attempt directory.
const QuestionFile = "question.md"

const attemptPrefix = "attempt-"

// maxAttempts bounds the search for a free attempt number.
const maxAttempts = 10000

// ProblemDir is a problem's directory of a session in the learner workspace:
// <workspace>/<session>/<problem>. Its attempt-<n> subdirectories hold the
// learner's work; a reset keeps the old ones and starts the next number.
func ProblemDir(workspace, sessionID, problemID string) string {
	return filepath.Join(workspace, sessionID, problemID)
}

// AttemptDir is the n-th attempt directory of a problem (n starts at 1). Its
// root is the grader's Target.Workspace for the problem: provided files sit
// at the paths their Assets keys name.
func AttemptDir(workspace, sessionID, problemID string, n int) string {
	return filepath.Join(ProblemDir(workspace, sessionID, problemID), attemptPrefix+strconv.Itoa(n))
}

// LatestAttempt returns the highest-numbered attempt directory of a problem,
// or an error wrapping fs.ErrNotExist when Start has not written one.
func LatestAttempt(workspace, sessionID, problemID string) (dir string, n int, err error) {
	entries, err := os.ReadDir(ProblemDir(workspace, sessionID, problemID))
	if err != nil {
		return "", 0, err
	}
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), attemptPrefix)
		if !ok || !e.IsDir() {
			continue
		}
		if v, err := strconv.Atoi(rest); err == nil && v > n {
			n = v
		}
	}
	if n == 0 {
		return "", 0, fmt.Errorf("no attempt directory for %s/%s: %w", sessionID, problemID, fs.ErrNotExist)
	}
	return AttemptDir(workspace, sessionID, problemID, n), n, nil
}

// newAttemptDir creates the next free attempt directory of a problem. It
// only ever creates: an existing attempt (earlier work) is skipped, never
// reused or overwritten.
func newAttemptDir(workspace, sessionID, problemID string) (string, error) {
	if err := os.MkdirAll(ProblemDir(workspace, sessionID, problemID), 0o755); err != nil {
		return "", fmt.Errorf("create the workspace of %s: %w", problemID, err)
	}
	for n := 1; n <= maxAttempts; n++ {
		dir := AttemptDir(workspace, sessionID, problemID, n)
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("create an attempt directory for %s: %w", problemID, err)
		}
	}
	return "", fmt.Errorf("create an attempt directory for %s: %d attempts already exist", problemID, maxAttempts)
}

// writeFiles writes files (slash paths relative to dir) below dir without
// replacing anything: a path that exists, or that would leave dir, is an
// error. Directories are created as needed.
func writeFiles(dir string, files map[string][]byte, mode os.FileMode) error {
	dirMode := os.FileMode(0o755)
	if mode&0o044 == 0 {
		dirMode = 0o700
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		target, err := problem.ResolveAsset(dir, name)
		if err != nil {
			return fmt.Errorf("file %q: %w", name, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), dirMode); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return fmt.Errorf("file %q: %w", name, err)
		}
		_, werr := f.Write(files[name])
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return fmt.Errorf("file %q: %w", name, werr)
		}
	}
	return nil
}
