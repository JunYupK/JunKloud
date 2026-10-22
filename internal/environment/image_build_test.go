//go:build linux

package environment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JunYupK/JunKloud/internal/executil"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func TestImageBuildPreflightRejectsRemoteAndSymlink(t *testing.T) {
	for _, text := range []string{"FROM ubuntu:latest\n", "# syntax=evil/frontend\nFROM scratch\n", "FROM scratch\nADD https://evil/a /a\n", "FROM scratch\nCOPY --from=evil/app /a /a\n", "FROM scratch\nRUN --network=host echo bad\n"} {
		dir := t.TempDir()
		testutil.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(text), 0600))
		if err := ValidateBuildContext(dir, []PinnedImage{ProbeImage}); err == nil {
			t.Fatalf("accepted remote input %q", text)
		}
	}
	dir := t.TempDir()
	testutil.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0600))
	testutil.NoError(t, os.Symlink(t.TempDir(), filepath.Join(dir, "escape")))
	if err := ValidateBuildContext(dir, nil); err == nil {
		t.Fatal("accepted symlink")
	}
}

func TestImageBuildRejectsSpacedFrontendBeforeEffects(t *testing.T) {
	for _, directive := range []string{"# syntax = docker/dockerfile:1", "#\tSyNtAx\t=\tevil/frontend:1"} {
		t.Run(directive, func(t *testing.T) {
			dir := t.TempDir()
			st := openTestStore(t, dir)
			runner := &fakeRunner{}
			m := New(dir, st, WithRunner(runner))
			ws := t.TempDir()
			testutil.NoError(t, os.WriteFile(filepath.Join(ws, "Dockerfile"), []byte(directive+"\nFROM scratch\n"), 0600))
			_, err := m.BuildImage(bg, model.Inventory{EnvironmentID: "env", SessionID: "session", ProblemID: "image"}, ws, "app:v1", nil)
			testutil.ErrorIs(t, err, ErrInvalidAction)
			testutil.Equal(t, len(runner.Calls()), 0)
		})
	}
}

func TestImageObservationMatchingLabelsCannotManufactureCleanupProof(t *testing.T) {
	dir := t.TempDir()
	st := openTestStore(t, dir)
	id := "sha256:" + strings.Repeat("b", 64)
	inv := model.Inventory{EnvironmentID: "env", SessionID: "session", ProblemID: "image"}
	raw, _ := json.Marshal([]buildImageRecord{{ID: id, RepoTags: []string{"app:v1"}, Config: buildImageConfig{Labels: map[string]string{LabelEnvironmentID: "env", LabelSessionID: "session", LabelProblemID: "image", labelImageBuild: "copied-operation"}}}})
	runner := &fakeRunner{respond: func(_ string, args []string) (executil.Result, error) {
		if args[1] == "rm" {
			return executil.Result{}, nil
		}
		return executil.Result{Stdout: raw}, nil
	}}
	m := New(dir, st, WithRunner(runner))
	observed, err := m.ObserveImages(bg, inv, []string{"app:v1"})
	testutil.NoError(t, err)
	testutil.ErrorIs(t, m.cleanupImages(bg, observed), ErrNotOwned)
	testutil.Equal(t, runner.count("docker image rm"), 0)
}

func TestImageBuildJournalForeignRebuildAndRecovery(t *testing.T) {
	dir := t.TempDir()
	st := openTestStore(t, dir)
	ws := t.TempDir()
	testutil.NoError(t, os.WriteFile(filepath.Join(ws, "Dockerfile"), []byte("FROM scratch\n"), 0600))
	inv := model.Inventory{EnvironmentID: "env", SessionID: "session", ProblemID: "image"}
	ref := "local/practice:v1"
	id1 := "sha256:" + strings.Repeat("1", 64)
	id2 := "sha256:" + strings.Repeat("2", 64)
	var current *buildImageRecord
	buildCount := 0
	cancelBuild := false
	fake := &fakeRunner{respond: func(exe string, args []string) (executil.Result, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "image inspect"):
			if current == nil {
				return executil.Result{Stderr: []byte("No such image")}, fmtExit("No such image")
			}
			raw, _ := json.Marshal([]buildImageRecord{*current})
			return executil.Result{Stdout: raw}, nil
		case strings.HasPrefix(joined, "image rm"):
			current = nil
			return executil.Result{}, nil
		case strings.HasPrefix(joined, "build "):
			ops, err := st.ListOperations(bg, OperationImageBuild)
			testutil.NoError(t, err)
			op := ops[len(ops)-1]
			if op.Phase != "building" {
				t.Fatalf("effect without intent: %+v", op)
			}
			labels := map[string]string{}
			for i, a := range args {
				if a == "--label" {
					k, v, _ := strings.Cut(args[i+1], "=")
					labels[k] = v
				}
			}
			buildCount++
			id := id1
			if buildCount > 1 {
				id = id2
			}
			current = &buildImageRecord{ID: id, RepoTags: []string{ref}, OS: "linux", Architecture: buildArch(), Config: buildImageConfig{Labels: labels}}
			if cancelBuild {
				return executil.Result{}, context.Canceled
			}
			return executil.Result{}, nil
		}
		return executil.Result{}, errors.New("unexpected command " + joined)
	}}
	m := New(dir, st, WithRunner(fake))
	current = &buildImageRecord{ID: id1, RepoTags: []string{ref}}
	_, err := m.BuildImage(bg, inv, ws, ref, nil)
	if !errors.Is(err, ErrNotOwned) {
		t.Fatalf("foreign: %v", err)
	}
	testutil.Equal(t, buildCount, 0)
	current = nil
	image, err := m.BuildImage(bg, inv, ws, ref, nil)
	testutil.NoError(t, err)
	testutil.Equal(t, image.ID, id1)
	image, err = m.BuildImage(bg, inv, ws, ref, nil)
	testutil.NoError(t, err)
	testutil.Equal(t, image.ID, id2)
	if fake.count("docker image rm") != 1 {
		t.Fatal("old image leaked")
	}
	cancelBuild = true
	_, err = m.BuildImage(bg, inv, ws, ref, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	_, err = m.BuildImage(bg, inv, ws, ref, nil)
	if !errors.Is(err, ErrImageBuildPending) {
		t.Fatalf("pending build accepted: %v", err)
	}
	testutil.NoError(t, m.RecoverImageBuilds(bg, inv.EnvironmentID))
	if current != nil {
		t.Fatal("interrupted owned image leaked")
	}
	ops, err := st.ListOperations(bg, OperationImageBuild)
	testutil.NoError(t, err)
	for _, op := range ops {
		testutil.Equal(t, op.Phase, PhaseDone)
	}
}
func fmtExit(s string) error { return errors.Join(executil.ErrExit, errors.New(s)) }

func TestImageRecoveryRetainsLostReferenceAndForeignLabels(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprint(foreign), func(t *testing.T) {
			dir := t.TempDir()
			st := openTestStore(t, dir)
			inv := model.Inventory{EnvironmentID: "env", SessionID: "session", ProblemID: "image", Images: []model.OwnedImage{{Reference: "app:v1"}}}
			op := model.Operation{ID: "interrupted", EnvironmentID: "env", SessionID: "session", Kind: OperationImageBuild, Phase: "building", Planned: inv}
			testutil.NoError(t, st.PutOperation(bg, op))
			id := "sha256:" + strings.Repeat("a", 64)
			runner := &fakeRunner{respond: func(_ string, args []string) (executil.Result, error) {
				if args[1] == "ls" {
					return executil.Result{Stdout: []byte(id)}, nil
				}
				if foreign {
					raw, _ := json.Marshal([]buildImageRecord{{ID: id, RepoTags: []string{"app:v1"}}})
					return executil.Result{Stdout: raw}, nil
				}
				return executil.Result{}, fmtExit("No such image")
			}}
			m := New(dir, st, WithRunner(runner))
			testutil.ErrorIs(t, m.RecoverImageBuilds(bg, "env"), ErrNotOwned)
			saved, err := st.GetOperation(bg, "interrupted")
			testutil.NoError(t, err)
			testutil.Equal(t, saved.Phase, "building")
			testutil.Equal(t, runner.count("docker image rm"), 0)
		})
	}
}

func TestImageObservationDoesNotAuthorizeDeletion(t *testing.T) {
	dir := t.TempDir()
	st := openTestStore(t, dir)
	id := "sha256:" + strings.Repeat("a", 64)
	raw, _ := json.Marshal([]buildImageRecord{{ID: id, RepoTags: []string{"app:latest"}}})
	runner := &fakeRunner{respond: func(_ string, _ []string) (executil.Result, error) { return executil.Result{Stdout: raw}, nil }}
	m := New(dir, st, WithRunner(runner))
	inv := model.Inventory{EnvironmentID: "env", SessionID: "session", ProblemID: "image"}
	observed, err := m.ObserveImages(bg, inv, []string{"app"})
	testutil.NoError(t, err)
	testutil.Equal(t, len(observed.Images), 0)
	testutil.Equal(t, observed.ObservedImages[0].ID, id)
	testutil.ErrorIs(t, m.cleanupImages(bg, observed), ErrNotOwned)
	testutil.Equal(t, runner.count("docker image rm"), 0)
}
