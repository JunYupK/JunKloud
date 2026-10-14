package problem_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

const validFixture = "testdata/valid-service"

// copyFixture copies the valid-service package into dst (created as needed)
// so a test can modify it without touching testdata.
func copyFixture(t *testing.T, dst string) string {
	t.Helper()
	err := filepath.WalkDir(validFixture, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(validFixture, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	return dst
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
