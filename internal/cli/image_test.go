//go:build linux

package cli

import (
	"context"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/model"
	"strings"
	"testing"
)

func (e cluster) BuildImage(_ context.Context, inv model.Inventory, dir, ref string, _ []environment.PinnedImage) (model.OwnedImage, error) {
	return model.OwnedImage{Reference: ref, ID: "sha256:cli"}, nil
}
func TestImageBuildCLI(t *testing.T) {
	h := newHarness(t)
	h.start(model.ModePractice)
	code, out, diag := h.run("", "image", "build", "--problem", "Q1", "--context", ".", "--tag", "local/practice:v1")
	if code != 0 || !strings.Contains(out, "local/practice:v1") || !strings.Contains(out, "sha256:cli") {
		t.Fatal(code, out, diag)
	}
	for _, args := range [][]string{{"image", "build"}, {"image", "build", "--problem", "Q1", "--tag", "-bad"}} {
		code, _, _ = h.run("", args...)
		if code != 2 {
			t.Fatal("invalid request accepted", code)
		}
	}
}
