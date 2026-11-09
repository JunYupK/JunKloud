// Package lock provides the per-environment mutation lock. One environment
// allows one mutating operation (start, reset, bank validation, grade) at a
// time; read-only commands never take it (they may only probe it with Held,
// which opens the file read-only and never creates or writes it).
//
// The lock is a Linux flock(2) on a file. The kernel drops it when the
// holding process exits for any reason, including SIGKILL, so a crashed
// command can never leave a stale lock behind that someone has to clean up.
package lock

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"syscall"
)

// ErrLocked means another holder, in this or any other process, has the lock.
var ErrLocked = errors.New("lock: environment is locked by another operation")

// Acquire takes the exclusive lock stored in the file at path, creating the
// file if needed (its directory must exist). It never waits: if the lock is
// held it returns ErrLocked at once, so the caller can tell the learner which
// operation to finish first.
//
// release unlocks and is safe to call more than once. The lock file is left
// in place on purpose (removing it would let two processes lock different
// files); it holds the owner's PID for diagnostics only.
func Acquire(path string) (release func() error, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock: open %q: %w", path, err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock: flock %q: %w", path, err)
	}
	// Best effort: the PID is informational and the lock is already ours.
	if f.Truncate(0) == nil {
		f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}

	var once sync.Once
	return func() error {
		var rerr error
		once.Do(func() {
			// Closing the descriptor releases the flock too; the explicit
			// unlock just makes the intent obvious.
			rerr = errors.Join(syscall.Flock(int(f.Fd()), syscall.LOCK_UN), f.Close())
		})
		return rerr
	}, nil
}

// Held reports whether another open file description holds the lock at path,
// without ever creating, truncating or writing the file, so a read-only
// command can call it on a lock file it has no write access to. A missing file
// is not held. The probe takes a shared non-blocking flock and drops it again
// at once. A mutating command that tries to Acquire in that very instant gets
// ErrLocked once; nothing is written, so the file is never left changed.
func Held(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock: open %q: %w", path, err)
	}
	defer f.Close()
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if err != syscall.EINTR {
			break
		}
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock: flock %q: %w", path, err)
	}
	return false, syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
