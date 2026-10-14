package problem_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func TestAssetEscape(t *testing.T) {
	pkg := copyFixture(t, t.TempDir())
	root := filepath.Join(pkg, "setup")

	_, err := problem.ResolveAsset(root, "../solution/answer.yaml")
	testutil.ErrorIs(t, err, problem.ErrAssetEscape)

	for _, path := range []string{"..", "provided/../../solution/answer.yaml", "/etc/passwd", filepath.Join(pkg, "solution", "answer.yaml")} {
		_, err = problem.ResolveAsset(root, path)
		testutil.ErrorIs(t, err, problem.ErrAssetEscape)
	}

	// A symlink inside root that points outside it escapes.
	testutil.NoError(t, os.Symlink(filepath.Join(pkg, "solution"), filepath.Join(root, "provided", "sol")))
	_, err = problem.ResolveAsset(root, "provided/sol/answer.yaml")
	testutil.ErrorIs(t, err, problem.ErrAssetEscape)

	// A dangling symlink pointing outside escapes too, so a later write
	// through it cannot land outside root.
	testutil.NoError(t, os.Symlink(filepath.Join(pkg, "nowhere.yaml"), filepath.Join(root, "provided", "dangling.yaml")))
	_, err = problem.ResolveAsset(root, "provided/dangling.yaml")
	testutil.ErrorIs(t, err, problem.ErrAssetEscape)
}

func TestResolveAssetInside(t *testing.T) {
	base := filepath.Join(t.TempDir(), "작업 디렉터리")
	root := filepath.Join(base, "세션 1")
	writeFile(t, filepath.Join(root, "제공 파일", "pod spec.yaml"), "kind: Pod\n")
	realRoot, err := filepath.EvalSymlinks(root)
	testutil.NoError(t, err)

	got, err := problem.ResolveAsset(root, "제공 파일/pod spec.yaml")
	testutil.NoError(t, err)
	testutil.Equal(t, got, filepath.Join(realRoot, "제공 파일", "pod spec.yaml"))

	// Paths that only pass through ".." inside root are fine.
	got, err = problem.ResolveAsset(root, "제공 파일/../제공 파일/pod spec.yaml")
	testutil.NoError(t, err)
	testutil.Equal(t, got, filepath.Join(realRoot, "제공 파일", "pod spec.yaml"))

	// A not-yet-existing output path inside root resolves (for submissions).
	got, err = problem.ResolveAsset(root, "output/답안.txt")
	testutil.NoError(t, err)
	testutil.Equal(t, got, filepath.Join(realRoot, "output", "답안.txt"))

	// An internal symlink resolves to its real path.
	testutil.NoError(t, os.Symlink("제공 파일", filepath.Join(root, "link")))
	got, err = problem.ResolveAsset(root, "link/pod spec.yaml")
	testutil.NoError(t, err)
	testutil.Equal(t, got, filepath.Join(realRoot, "제공 파일", "pod spec.yaml"))

	// The root itself is not an asset.
	_, err = problem.ResolveAsset(root, ".")
	testutil.ErrorIs(t, err, problem.ErrInvalidAssetPath)
	_, err = problem.ResolveAsset(root, "")
	testutil.ErrorIs(t, err, problem.ErrInvalidAssetPath)
}
