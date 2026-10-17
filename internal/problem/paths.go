package problem

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrAssetEscape reports a path that leaves its root, either lexically
	// (absolute, "..") or through a symlink whose real path is outside.
	ErrAssetEscape = errors.New("asset path escapes its root")
	// ErrInvalidAssetPath reports an empty path or one naming the root itself.
	ErrInvalidAssetPath = errors.New("invalid asset path")
)

// maxSymlinkHops bounds symlink resolution of dangling links, like the
// kernel's ELOOP limit.
const maxSymlinkHops = 40

// ResolveAsset resolves a slash- or OS-separated path relative to root and
// returns its real absolute path, guaranteeing it stays inside root's real
// path. Symlinks are followed (including dangling ones, so a later write
// through them cannot land outside root). The target need not exist, so the
// same check serves provided files and submission output paths.
func ResolveAsset(root, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%w: empty path", ErrInvalidAssetPath)
	}
	native := filepath.FromSlash(path)
	if filepath.IsAbs(native) || filepath.VolumeName(native) != "" {
		return "", fmt.Errorf("%w: %q is absolute", ErrAssetEscape, path)
	}
	clean := filepath.Clean(native)
	if clean == "." {
		return "", fmt.Errorf("%w: %q names the root", ErrInvalidAssetPath, path)
	}
	if !isLocal(clean) {
		return "", fmt.Errorf("%w: %q leaves its root", ErrAssetEscape, path)
	}

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve asset root: %w", err)
	}
	realRoot, err = filepath.Abs(realRoot)
	if err != nil {
		return "", fmt.Errorf("resolve asset root: %w", err)
	}
	real, err := realPath(filepath.Join(realRoot, clean), 0)
	if err != nil {
		return "", fmt.Errorf("resolve asset %q: %w", path, err)
	}
	rel, err := filepath.Rel(realRoot, real)
	if err != nil || rel == "." || !isLocal(rel) {
		return "", fmt.Errorf("%w: %q resolves outside its root", ErrAssetEscape, path)
	}
	return real, nil
}

// isLocal reports whether a cleaned relative path stays below its base.
func isLocal(clean string) bool {
	return clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

// realPath is filepath.EvalSymlinks extended to paths whose tail does not
// exist yet: the longest existing prefix is resolved and the rest appended.
// A dangling symlink is resolved through its link text.
func realPath(p string, hops int) (string, error) {
	if hops > maxSymlinkHops {
		return "", fmt.Errorf("too many symlinks at %q", p)
	}
	real, err := filepath.EvalSymlinks(p)
	if err == nil {
		return real, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if info, lerr := os.Lstat(p); lerr == nil && info.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(p)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(p), target)
		}
		return realPath(target, hops+1)
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p, nil
	}
	realParent, err := realPath(parent, hops)
	if err != nil {
		return "", err
	}
	return filepath.Join(realParent, filepath.Base(p)), nil
}
