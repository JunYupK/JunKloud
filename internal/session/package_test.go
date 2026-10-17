//go:build linux

package session_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func TestStoreAndLoadPackage(t *testing.T) {
	p, err := problem.Load(filepath.Join(devBank, "problems", "dev-scale-001"))
	testutil.NoError(t, err)
	hash, err := problem.Hash(p)
	testutil.NoError(t, err)
	dir := t.TempDir()

	got, err := session.StorePackage(dir, p)
	testutil.NoError(t, err)
	testutil.Equal(t, got, hash)

	loaded, err := session.LoadPackage(dir, hash)
	testutil.NoError(t, err)
	testutil.Equal(t, loaded.Files, p.Files)
	loadedHash, err := problem.Hash(loaded)
	testutil.NoError(t, err)
	testutil.Equal(t, loadedHash, hash)
	testutil.Equal(t, loaded.ID, p.ID)
	testutil.Equal(t, loaded.Solution, p.Solution)

	// Private: owner-only files and directories, no staging leftovers.
	pkg, err := session.PackageDir(dir, hash)
	testutil.NoError(t, err)
	err = filepath.WalkDir(filepath.Dir(pkg), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is accessible to others: %v", path, info.Mode())
		}
		if strings.Contains(d.Name(), ".stage-") {
			t.Errorf("staging leftover %s", path)
		}
		return nil
	})
	testutil.NoError(t, err)

	// Storing the same package again is a no-op that verifies the first.
	again, err := session.StorePackage(dir, p)
	testutil.NoError(t, err)
	testutil.Equal(t, again, hash)
}

func TestLoadPackageDetectsDamageAndAbsence(t *testing.T) {
	p, err := problem.Load(filepath.Join(devBank, "problems", "dev-sa-001"))
	testutil.NoError(t, err)
	dir := t.TempDir()
	hash, err := session.StorePackage(dir, p)
	testutil.NoError(t, err)
	pkg, err := session.PackageDir(dir, hash)
	testutil.NoError(t, err)

	_, err = session.LoadPackage(dir, "sha256:"+strings.Repeat("a", 64))
	testutil.ErrorIs(t, err, session.ErrPackageMissing)
	_, err = session.LoadPackage(dir, "not-a-hash")
	if err == nil {
		t.Fatal("a malformed hash was accepted")
	}
	_, err = session.PackageDir(dir, "sha256:../../etc/passwd")
	if err == nil {
		t.Fatal("a path-like hash was accepted")
	}

	// A changed, a removed and an added file are each caught by the hash.
	solution := filepath.Join(pkg, "files", "solution", "builder.yaml")
	orig, err := os.ReadFile(solution)
	testutil.NoError(t, err)
	testutil.NoError(t, os.WriteFile(solution, append(orig, "# tampered\n"...), 0o600))
	_, err = session.LoadPackage(dir, hash)
	testutil.ErrorIs(t, err, session.ErrPackageCorrupt)
	testutil.NoError(t, os.Remove(solution))
	_, err = session.LoadPackage(dir, hash)
	testutil.ErrorIs(t, err, session.ErrPackageCorrupt)
	testutil.NoError(t, os.WriteFile(solution, orig, 0o600))
	_, err = session.LoadPackage(dir, hash)
	testutil.NoError(t, err)
	testutil.NoError(t, os.WriteFile(filepath.Join(pkg, "files", "extra.txt"), []byte("x"), 0o600))
	_, err = session.LoadPackage(dir, hash)
	testutil.ErrorIs(t, err, session.ErrPackageCorrupt)

	// StorePackage never papers over a damaged copy.
	_, err = session.StorePackage(dir, p)
	testutil.ErrorIs(t, err, session.ErrPackageCorrupt)

	// A damaged definition is damage as well.
	testutil.NoError(t, os.Remove(filepath.Join(pkg, "files", "extra.txt")))
	testutil.NoError(t, os.WriteFile(filepath.Join(pkg, "definition.json"), []byte("{"), 0o600))
	_, err = session.LoadPackage(dir, hash)
	testutil.ErrorIs(t, err, session.ErrPackageCorrupt)
}

// TestPackageRoundTripKeepsEveryFixtureHash: the JSON round trip of a stored
// definition must not change the content hash for any fixture, otherwise a
// healthy package would look damaged.
func TestPackageRoundTripKeepsEveryFixtureHash(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join(devBank, "problems", "*"))
	testutil.NoError(t, err)
	testutil.Equal(t, len(dirs), 5)
	state := t.TempDir()
	for _, d := range dirs {
		p, err := problem.Load(d)
		testutil.NoError(t, err)
		hash, err := session.StorePackage(state, p)
		testutil.NoError(t, err)
		_, err = session.LoadPackage(state, hash)
		testutil.NoError(t, err)
	}
}
