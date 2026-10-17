//go:build linux

package environment

import (
	"fmt"
	"strings"
	"testing"

	"github.com/JunYupK/JunKloud/internal/executil"
)

func TestPreloadOlderDaemonSaveRequiresMatchingPinnedNativeImage(t *testing.T) {
	imageID := "sha256:" + strings.Repeat("a", 64)
	unsupported := fmt.Errorf("%w: \"--platform\" requires API version 1.48, but the Docker daemon API version is 1.43", executil.ErrExit)
	for _, tc := range []struct {
		name, inspection         string
		saveErr                  error
		wantOK                   bool
		legacySaves, inspections int
	}{
		{"native", imageID + " linux/amd64\n" + imageID + " linux/amd64\n", unsupported, true, 1, 1},
		{"wrong-architecture", imageID + " linux/arm64\n" + imageID + " linux/arm64\n", unsupported, false, 0, 1},
		{"tag-changed", imageID + " linux/amd64\nsha256:" + strings.Repeat("b", 64) + " linux/amd64\n", unsupported, false, 0, 1},
		{"unrelated-error", "", fmt.Errorf("%w: permission denied", executil.ErrExit), false, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{respond: func(exe string, args []string) (executil.Result, error) {
				if exe == "docker" && args[0] == "save" && args[1] == "--platform" {
					return executil.Result{}, tc.saveErr
				}
				if exe == "docker" && args[0] == "image" && args[1] == "inspect" {
					return executil.Result{Stdout: []byte(tc.inspection)}, nil
				}
				return executil.Result{}, nil
			}}
			dir := t.TempDir()
			m := New(dir, openTestStore(t, dir), WithRunner(runner))
			img := PinnedImage{Name: "docker.io/library/busybox:1.37.0", Digest: "sha256:" + strings.Repeat("c", 64)}
			err := m.preload(bg, "junkloud-0123456789ab", "amd64", img, true)
			if (err == nil) != tc.wantOK {
				t.Fatal(err)
			}
			if got := runner.count("docker save --output "); got != tc.legacySaves {
				t.Fatalf("legacy saves: %d calls=%v", got, runner.Calls())
			}
			if got := runner.count("docker image inspect "); got != tc.inspections {
				t.Fatalf("inspections: %d calls=%v", got, runner.Calls())
			}
			if tc.wantOK && runner.count("kind load image-archive ") != 1 {
				t.Fatal("image was not loaded")
			}
		})
	}
}
