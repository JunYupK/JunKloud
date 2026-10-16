//go:build linux

package author_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/lock"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// firstRunEnv creates "the cluster" on the first Ensure that finds none,
// holding inside Ensure until the test lets it go.
type firstRunEnv struct {
	mu        sync.Mutex
	recorded  *model.Identity
	creates   int
	platforms int
	entered   chan struct{}
	proceed   chan struct{}
}

func (e *firstRunEnv) Ensure(context.Context) (model.Identity, error) {
	e.entered <- struct{}{}
	<-e.proceed
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.recorded == nil {
		e.creates++
		e.recorded = &model.Identity{ID: "env-1", Context: "junkloud-env-1"}
	}
	return *e.recorded, nil
}

func (e *firstRunEnv) EnsurePlatform(context.Context, model.Identity) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.platforms++
	return nil
}

// TestEnsureEnvironmentTakesTheLock: two first-run commands at once must not
// both create an environment. EnsureEnvironment holds the environment lock
// around Ensure and EnsurePlatform; the second caller is refused at once.
func TestEnsureEnvironmentTakesTheLock(t *testing.T) {
	dir := t.TempDir()
	env := &firstRunEnv{entered: make(chan struct{}, 2), proceed: make(chan struct{})}
	type result struct {
		id  model.Identity
		err error
	}
	first := make(chan result, 1)
	go func() {
		id, err := author.EnsureEnvironment(context.Background(), env, dir)
		first <- result{id, err}
	}()
	<-env.entered // the first one is inside Ensure, holding the lock
	_, err := author.EnsureEnvironment(context.Background(), env, dir)
	testutil.ErrorIs(t, err, lock.ErrLocked)
	close(env.proceed)
	r := <-first
	testutil.NoError(t, r.err)
	testutil.Equal(t, r.id.ID, "env-1")
	testutil.Equal(t, env.creates, 1)
	testutil.Equal(t, env.platforms, 1)

	// Afterwards the lock is free again and the environment is only verified.
	id, err := author.EnsureEnvironment(context.Background(), env, dir)
	testutil.NoError(t, err)
	testutil.Equal(t, id.ID, "env-1")
	testutil.Equal(t, env.creates, 1)

	// A failing Ensure is returned and the platform is not touched.
	boom := errors.New("docker is not running")
	failing := &failingEnv{err: boom}
	_, err = author.EnsureEnvironment(context.Background(), failing, dir)
	testutil.ErrorIs(t, err, boom)
	testutil.Equal(t, failing.platforms, 0)
}

type failingEnv struct {
	err       error
	platforms int
}

func (f *failingEnv) Ensure(context.Context) (model.Identity, error) { return model.Identity{}, f.err }
func (f *failingEnv) EnsurePlatform(context.Context, model.Identity) error {
	f.platforms++
	return nil
}

// TestLockPathIsAbsolute: a relative and an absolute spelling of one state
// directory name one lock file, as environment.New resolves the directory.
func TestLockPathIsAbsolute(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	testutil.NoError(t, os.Mkdir("state", 0o700))
	testutil.Equal(t, author.LockPath("state"), filepath.Join(dir, "state", "env.lock"))
	testutil.Equal(t, filepath.IsAbs(author.LockPath("state")), true)
}
