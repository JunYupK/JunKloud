//go:build linux

package author

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
)

// ScratchWorkspace makes the workspace a frozen problem is graded in before a
// learner touches it: a new private directory below parent holding exactly
// the problem's provided files (FrozenProblem.Assets, rendered), what Start
// later publishes. Validation evaluates every stage in a fresh one and a
// session's preparation evaluates the initial state in one, so file,
// kustomize and archive criteria judge the same files in the state that is
// certified and in the state that is served. parent is created if needed
// ("" means the system temporary directory). remove deletes the directory.
func ScratchWorkspace(parent string, f model.FrozenProblem) (dir string, remove func(), err error) {
	if parent != "" {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return "", nil, fmt.Errorf("author: scratch workspace of %q: %w", f.Definition.ID, err)
		}
	}
	dir, err = os.MkdirTemp(parent, f.Definition.ID+"-")
	if err != nil {
		return "", nil, fmt.Errorf("author: scratch workspace of %q: %w", f.Definition.ID, err)
	}
	remove = func() { os.RemoveAll(dir) }
	if err := WriteFiles(dir, f.Assets, 0o600); err != nil {
		remove()
		return "", nil, fmt.Errorf("author: scratch workspace of %q: %w", f.Definition.ID, err)
	}
	return dir, remove, nil
}

// WriteFiles writes files (slash paths relative to dir) below dir, never
// outside it and never over an existing file. Directories are created
// world-readable only when the files are.
func WriteFiles(dir string, files map[string][]byte, mode os.FileMode) error {
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
