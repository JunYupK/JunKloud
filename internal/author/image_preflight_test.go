//go:build linux

package author_test

import (
	"context"
	"errors"
	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
	"os"
	"path/filepath"
	"testing"
)

type preflightImages struct {
	builds, saves, checks int
	saveError             error
}

func (i *preflightImages) BuildImage(context.Context, model.Inventory, string, string, []environment.PinnedImage) (model.OwnedImage, error) {
	i.builds++
	return model.OwnedImage{Reference: "app:v1"}, nil
}
func (i *preflightImages) SaveImage(_ context.Context, _ string, p string) error {
	i.saves++
	return os.WriteFile(p, []byte("archive"), 0600)
}
func (i *preflightImages) PreflightSaveReference(context.Context, string) error {
	i.checks++
	return i.saveError
}

func TestImageSavePreflightRejectsConflictsBeforeAnyEffects(t *testing.T) {
	file := func(dest string) model.Action {
		return model.Action{Kind: model.ActionFile, Args: map[string]any{"source": "solution/answer", "path": dest}}
	}
	save := func(dest string) model.Action {
		return model.Action{Kind: model.ActionImage, Args: map[string]any{"operation": "save", "reference": "app:v1", "path": dest}}
	}
	for _, tc := range []struct {
		name        string
		actions     []model.Action
		directory   bool
		unavailable bool
	}{
		{"save then child file", []model.Action{save("out"), file("out/answer.txt")}, false, false},
		{"file then save directory", []model.Action{file("answer"), save("out")}, true, false},
		{"file then unavailable save", []model.Action{file("answer"), save("out")}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			ws := t.TempDir()
			testutil.NoError(t, os.WriteFile(filepath.Join(ws, "answer"), []byte("original"), 0600))
			if tc.directory {
				testutil.NoError(t, os.Mkdir(filepath.Join(ws, "out"), 0700))
			}
			images := &preflightImages{}
			if tc.unavailable {
				images.saveError = errors.New("missing or non-native image")
			}
			_, err := author.NewActionExecutor(h.cluster, images).Execute(ctx, h.id, environment.Owner{}, model.Problem{Files: map[string][]byte{"solution/answer": []byte("private replacement")}}, model.FrozenProblem{}, ws, tc.actions)
			if err == nil {
				t.Fatal("invalid save sequence accepted")
			}
			b, _ := os.ReadFile(filepath.Join(ws, "answer"))
			testutil.Equal(t, string(b), "original")
			testutil.Equal(t, images.builds, 0)
			testutil.Equal(t, images.saves, 0)
			testutil.Equal(t, len(h.cluster.applies), 0)
			if !tc.directory {
				if _, err := os.Stat(filepath.Join(ws, "out")); !os.IsNotExist(err) {
					t.Fatal("save output appeared before rejection")
				}
			}
		})
	}
}
