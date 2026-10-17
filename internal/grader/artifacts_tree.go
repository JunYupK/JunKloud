package grader

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// maxTreeEntries bounds the directory trees the grader walks (an overlay, a
// base, a chart).
const maxTreeEntries = 5000

// hashTree fingerprints a directory tree by content: the sorted relative path
// of every entry with its type and, for regular files, a digest of the
// bytes; for symlinks the link text. Modification times and permissions do
// not count. The result is deterministic.
func hashTree(root string) (string, error) {
	h := sha256.New()
	n := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if n++; n > maxTreeEntries {
			return fmt.Errorf("directory has more than %d entries", maxTreeEntries)
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			fmt.Fprintf(h, "d %s\n", rel)
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "l %s %s\n", rel, target)
		case d.Type().IsRegular():
			b, err := readLimited(p, maxFileBytes)
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			sum := sha256.Sum256(b)
			fmt.Fprintf(h, "f %s %s\n", rel, hex.EncodeToString(sum[:]))
		default:
			fmt.Fprintf(h, "o %s\n", rel)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// normalizeEOL makes line endings comparable across editors.
func normalizeEOL(b []byte) []byte {
	return []byte(strings.ReplaceAll(string(b), "\r\n", "\n"))
}
