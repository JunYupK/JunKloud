package grader

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSharedReadScriptConfinesActualReads(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("shared-file proof executes in the Linux runtime")
	}
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "private")
	if err := os.WriteFile(outside, []byte("must-not-leak"), 0600); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "message")
	if err := os.WriteFile(inside, []byte("seeded\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		target string
		code   int
		out    string
	}{{"regular", inside, 0, "seeded\n"}, {"inside-link", inside, 0, "seeded\n"}, {"outside-link", outside, 2, ""}, {"missing", filepath.Join(root, "absent"), 3, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			file := tc.target
			if tc.name == "inside-link" || tc.name == "outside-link" {
				file = filepath.Join(root, tc.name)
				if err := os.Symlink(tc.target, file); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("sh", "-c", sharedReadScript, "shared-read", root, file)
			out, err := cmd.Output()
			code := 0
			if err != nil {
				e, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatal(err)
				}
				code = e.ExitCode()
			}
			if code != tc.code || string(out) != tc.out {
				t.Fatalf("exit=%d output=%q; want exit=%d output=%q", code, out, tc.code, tc.out)
			}
		})
	}
}
