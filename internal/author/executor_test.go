//go:build linux

package author_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func TestScopedFilesMutateOnlyStageWorkspace(t *testing.T) {
	h := newHarness(t)
	f := model.FrozenProblem{Assets: map[string][]byte{"answer.txt": []byte("initial")}}
	p := model.Problem{Files: map[string][]byte{"solution/answer.txt": []byte("private solved")}}
	ws, remove, err := author.ScratchWorkspace(t.TempDir(), f)
	testutil.NoError(t, err)
	defer remove()
	ex := author.NewActionExecutor(h.cluster, nil)
	_, err = ex.Execute(ctx, h.id, environment.Owner{}, p, f, ws, []model.Action{{Kind: model.ActionFile, Args: map[string]any{"source": "solution/answer.txt", "path": "answer.txt"}}})
	testutil.NoError(t, err)
	got, err := os.ReadFile(filepath.Join(ws, "answer.txt"))
	testutil.NoError(t, err)
	testutil.Equal(t, string(got), "private solved")
	testutil.Equal(t, string(f.Assets["answer.txt"]), "initial")
	fresh, cleanup, err := author.ScratchWorkspace(t.TempDir(), f)
	testutil.NoError(t, err)
	defer cleanup()
	got, err = os.ReadFile(filepath.Join(fresh, "answer.txt"))
	testutil.NoError(t, err)
	testutil.Equal(t, string(got), "initial")
}

func TestScopedActionsPreflightBeforeWrites(t *testing.T) {
	for _, bad := range []model.Action{
		{Kind: "unknown"}, {Kind: model.ActionHelm},
		{Kind: model.ActionFile, Args: map[string]any{"source": "solution/a", "path": "../escape"}},
		{Kind: model.ActionImage, Args: map[string]any{"operation": "push", "reference": "local/app:v1", "path": "."}},
	} {
		t.Run(bad.Kind, func(t *testing.T) {
			h := newHarness(t)
			ws := t.TempDir()
			testutil.NoError(t, os.WriteFile(filepath.Join(ws, "answer"), []byte("old"), 0600))
			ex := author.NewActionExecutor(h.cluster, nil)
			_, err := ex.Execute(ctx, h.id, environment.Owner{}, model.Problem{Files: map[string][]byte{"solution/a": []byte("secret")}}, model.FrozenProblem{}, ws, []model.Action{{Kind: model.ActionFile, Args: map[string]any{"source": "solution/a", "path": "answer"}}, bad})
			if err == nil {
				t.Fatal("invalid action accepted")
			}
			got, _ := os.ReadFile(filepath.Join(ws, "answer"))
			testutil.Equal(t, string(got), "old")
			testutil.Equal(t, len(h.cluster.applies), 0)
		})
	}
}

func TestScopedFileRejectsDestinationSymlink(t *testing.T) {
	h := newHarness(t)
	ws := t.TempDir()
	outside := t.TempDir()
	testutil.NoError(t, os.Symlink(outside, filepath.Join(ws, "escape")))
	_, err := author.NewActionExecutor(h.cluster, nil).Execute(ctx, h.id, environment.Owner{}, model.Problem{Files: map[string][]byte{"solution/a": []byte("secret")}}, model.FrozenProblem{}, ws, []model.Action{{Kind: model.ActionFile, Args: map[string]any{"source": "solution/a", "path": "escape/a"}}})
	if !errors.Is(err, environment.ErrInvalidAction) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "a")); !os.IsNotExist(err) {
		t.Fatal("escaped workspace")
	}
}

func TestValidationStagesShareWorkspaceAndResetOriginalAssets(t *testing.T) {
	h := newHarness(t)
	p := loadFixture(t)
	p.Files["setup/provided/answer.txt"] = []byte("initial")
	p.Files["solution/answer.txt"] = []byte("solved")
	p.Solution = append(p.Solution, model.Action{Kind: model.ActionFile, Args: map[string]any{"source": "solution/answer.txt", "path": "answer.txt"}})
	var seen []string
	h.cluster.onEvaluate = func(target grader.Target) {
		testutil.Equal(t, target.SessionID, author.SessionID)
		b, _ := os.ReadFile(filepath.Join(target.Workspace, "answer.txt"))
		seen = append(seen, string(b))
		testutil.Equal(t, string(target.Problem.Assets["answer.txt"]), "initial")
	}
	_, err := h.validate(p, 1)
	testutil.NoError(t, err)
	if len(seen) < 3 || seen[0] != "initial" || seen[1] != "solved" || seen[len(seen)-1] != "initial" {
		t.Fatalf("stages %v", seen)
	}
}

func TestFileImageSetupRefusedBeforePreparation(t *testing.T) {
	for _, kind := range []string{model.ActionFile, model.ActionImage} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t)
			p := loadFixture(t)
			p.Setup = append(p.Setup, model.Action{Kind: kind, Args: map[string]any{}})
			_, err := h.validate(p, 1)
			if !errors.Is(err, author.ErrNotValid) {
				t.Fatalf("setup accepted: %v", err)
			}
			testutil.Equal(t, h.prepared, 0)
			testutil.Equal(t, len(h.cluster.applies), 0)
		})
	}
}
