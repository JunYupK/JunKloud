//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/cli"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// This is an engine fixture, generated in TempDir; it adds no product bank
// content. Four tiny file fixtures satisfy the existing practice minimum.
func task14Bank(t *testing.T, envID string) (string, string, string) {
	t.Helper()
	bank := t.TempDir()
	pid := "task14-image"
	ref := "junkloud.local/task14-" + envID + ":practice"
	base := environment.ProbeImage.Name
	header := func(id string) string {
		return fmt.Sprintf(`schema_version: 1
id: %s
version: 1.0.0
title: Build a local practice artifact
domain: design-build
difficulty: easy
type: build
tags: [task14]
competencies: ["Build and modify container images"]
capabilities: []
parameters: {}
namespace: jk-%s
budget: {cpu: 100m, memory: 64Mi}
setup: []
`, id, id)
	}
	image := header(pid) + fmt.Sprintf(`criteria:
  - id: behavior
    kind: image
    feedback: "이미지 실행 결과를 확인하세요."
    points: 60
    timeout: 60s
    args:
      reference: %s
      run: {timeout: 5s, stdout_contains: [TASK14_OK]}
  - id: archive
    kind: image
    feedback: "저장된 이미지 archive를 확인하세요."
    points: 40
    args: {archive: {path: image.tar, repo_tag: %s}}
solution:
  - kind: file
    args: {source: solution/Dockerfile, path: Dockerfile}
  - kind: image
    args: {operation: build, path: ., reference: %s}
  - kind: image
    args: {operation: save, path: image.tar, reference: %s}
initial_expected: {behavior: FAIL, archive: FAIL}
initial_score: 0
cases:
  - id: wrong-output
    actions:
      - kind: image
        args: {operation: build, path: ., reference: %s}
      - kind: image
        args: {operation: save, path: image.tar, reference: %s}
    expected: {behavior: FAIL, archive: PASS}
    expected_score: 40
  - id: missing-archive
    actions:
      - kind: file
        args: {source: solution/Dockerfile, path: Dockerfile}
      - kind: image
        args: {operation: build, path: ., reference: %s}
    expected: {behavior: PASS, archive: FAIL}
    expected_score: 60
`, ref, ref, ref, ref, ref, ref, ref)
	write := func(id string, files map[string]string) {
		for name, data := range files {
			p := filepath.Join(bank, "problems", id, filepath.FromSlash(name))
			testutil.NoError(t, os.MkdirAll(filepath.Dir(p), 0700))
			testutil.NoError(t, os.WriteFile(p, []byte(data), 0600))
		}
	}
	dockerfile := func(marker string) string {
		return fmt.Sprintf("FROM %s\nCMD [\"/bin/sh\",\"-c\",\"echo %s\"]\n", base, marker)
	}
	write(pid, map[string]string{"problem.yaml": image, "question.en.md": "Build the image from Dockerfile and save image.tar.\n", "explanation.ko.md": "Dockerfile을 수정하고 이미지를 빌드한 뒤 저장합니다.\n", "setup/provided/Dockerfile": dockerfile("TASK14_WRONG"), "solution/Dockerfile": dockerfile("TASK14_OK")})
	for n := 0; n < 4; n++ {
		id := fmt.Sprintf("task14-file-%d", n)
		definition := header(id) + `criteria:
  - id: answer
    kind: file
    feedback: "출력 파일을 확인하세요."
    points: 1
    args: {path: answer.txt, contains: [OK]}
solution:
  - kind: file
    args: {source: solution/answer.txt, path: answer.txt}
initial_expected: {answer: FAIL}
initial_score: 0
cases:
  - id: absent
    actions:
      - kind: file
        args: {source: cases/wrong.txt, path: answer.txt}
    expected: {answer: FAIL}
    expected_score: 0
`
		write(id, map[string]string{"problem.yaml": definition, "question.en.md": "Write answer.txt.\n", "explanation.ko.md": "출력 파일을 작성합니다.\n", "solution/answer.txt": "OK\n", "cases/wrong.txt": "WRONG\n"})
	}
	testutil.NoError(t, os.MkdirAll(filepath.Join(bank, "exams"), 0700))
	return bank, pid, ref
}

func TestTask14FixtureSchema(t *testing.T) {
	bank, _, _ := task14Bank(t, "schemafixture")
	dirs, err := filepath.Glob(filepath.Join(bank, "problems", "*"))
	testutil.NoError(t, err)
	testutil.Equal(t, len(dirs), 5)
	for _, dir := range dirs {
		_, err := problem.Load(dir)
		testutil.NoError(t, err)
	}
}

func TestTask14ImageWorkflow(t *testing.T) {
	requireTools(t, "docker", "kind", "kubectl", "helm")
	ctx := context.Background()
	phase := func(s string) { t.Logf("PHASE %s at %s", s, time.Now().UTC().Format(time.RFC3339)) }
	phase("boot isolated cluster")
	state := t.TempDir()
	st, err := store.Open(filepath.Join(state, "sessions.db"))
	testutil.NoError(t, err)
	m := environment.New(state, st, hostCompatOptions(t)...)
	t.Cleanup(func() {
		defer st.Close()
		if recorded, e := m.RecordedIdentity(); e == nil {
			testutil.NoError(t, m.StopPlatform(ctx, recorded))
		}
		ops, e := st.ListOperations(ctx, environment.OperationCreate)
		testutil.NoError(t, e)
		for _, op := range ops {
			kindDelete(t, environment.ClusterName(op.EnvironmentID), filepath.Join(state, "cleanup.kubeconfig"))
		}
	})
	id, err := m.Ensure(ctx)
	testutil.NoError(t, err)
	bank, pid, ref := task14Bank(t, id.ID)
	cfg := author.GraderConfig{StateDir: state}
	rt, err := author.NewRuntime(ctx, m, st, id, cfg)
	testutil.NoError(t, err)
	defer rt.Close()
	dirs, err := filepath.Glob(filepath.Join(bank, "problems", "*"))
	testutil.NoError(t, err)
	phase("certify scoped file/image fixture with real grader and archives")
	for _, dir := range dirs {
		p, err := problem.Load(dir)
		testutil.NoError(t, err)
		rec, err := rt.Validator.Validate(ctx, p, []uint64{1})
		testutil.NoError(t, err)
		testutil.Equal(t, rec.Passed, true)
		t.Logf("CERTIFIED %s", p.ID)
	}
	workspace := t.TempDir()
	svc := session.New(st, m, rt, bank, state, session.WithWorkspace(workspace))
	phase("prepare and start genuine learner session")
	view, err := svc.Prepare(ctx, session.StartOptions{Mode: model.ModePractice, Count: 5, Seed: 1})
	testutil.NoError(t, err)
	testutil.NoError(t, svc.Start(ctx, view.ID, time.Now()))
	t.Setenv("CKAD_HOST_COMPAT", os.Getenv("JUNKLOUD_TEST_HOST_COMPAT"))
	command := func(want int, args ...string) {
		t.Helper()
		var out, diag bytes.Buffer
		full := append([]string{"--state-dir", state, "--bank-dir", bank, "--workspace", workspace}, args...)
		code := cli.Run(ctx, full, strings.NewReader(""), &out, &diag)
		t.Logf("CLI %s => %d; %s; %s", strings.Join(args, " "), code, strings.TrimSpace(out.String()), strings.TrimSpace(diag.String()))
		if code != want {
			t.Fatalf("CLI exit %d, want %d", code, want)
		}
	}
	personal := filepath.Join(workspace, ".bashrc")
	testutil.NoError(t, os.WriteFile(personal, []byte("alias k=kubectl\n"), 0600))
	// Foreign sentinel is a new tag on an existing provided image. Removal at
	// test cleanup is tag-only and conditional on its original immutable ID.
	sentinel := "junkloud.local/task14-foreign-" + id.ID + ":sentinel"
	run(t, "", "docker", "tag", environment.ProbeImage.Name, sentinel)
	sentinelID := strings.TrimSpace(run(t, "", "docker", "image", "inspect", "--format", "{{.Id}}", sentinel))
	t.Cleanup(func() {
		got, err := toolEnv.Run(ctx, "docker", []string{"image", "inspect", "--format", "{{.Id}}", sentinel}, nil)
		if err == nil && strings.TrimSpace(string(got.Stdout)) == sentinelID {
			_, err = toolEnv.Run(ctx, "docker", []string{"image", "rm", sentinel}, nil)
			testutil.NoError(t, err)
		}
	})
	_, err = svc.BuildImage(ctx, view.ID, session.ImageBuildOptions{ProblemID: pid, ContextDir: ".", Reference: sentinel})
	testutil.ErrorIs(t, err, environment.ErrNotOwned)
	var archived []string
	correct := fmt.Sprintf("FROM %s\nCMD [\"/bin/sh\",\"-c\",\"echo TASK14_OK\"]\n", environment.ProbeImage.Name)
	for cycle := 1; cycle <= 2; cycle++ {
		phase(fmt.Sprintf("managed learner cycle %d: build rebuild missing archive grade export grade reset", cycle))
		dir, n, err := session.LatestAttempt(workspace, view.ID, pid)
		testutil.NoError(t, err)
		testutil.Equal(t, n, cycle)
		initial, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
		testutil.NoError(t, err)
		if !strings.Contains(string(initial), "TASK14_WRONG") {
			t.Fatal("reset leaked private solution")
		}
		testutil.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(correct), 0600))
		command(0, "image", "build", "--problem", pid, "--context", ".", "--tag", ref)
		oldID := strings.TrimSpace(run(t, "", "docker", "image", "inspect", "--format", "{{.Id}}", ref))
		testutil.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(correct+fmt.Sprintf("LABEL learner.rebuild=%d\n", cycle)), 0600))
		command(0, "image", "build", "--problem", pid, "--context", ".", "--tag", ref)
		gotID := strings.TrimSpace(run(t, "", "docker", "image", "inspect", "--format", "{{.Id}}", ref))
		if oldID == gotID {
			t.Fatal("rebuild did not change image")
		}
		_, err = toolEnv.Run(ctx, "docker", []string{"image", "inspect", oldID}, nil)
		if err == nil {
			t.Fatal("rebuild leaked previous ID")
		}
		command(1, "grade", "--problem", pid)
		archive := filepath.Join(dir, "image.tar")
		run(t, "", "docker", "save", "--output", archive, ref)
		command(0, "grade", "--problem", pid, "--resubmit")
		inv, err := st.GetInventory(ctx, id.ID, view.ID, pid)
		testutil.NoError(t, err)
		testutil.Equal(t, len(inv.Images), 1)
		testutil.Equal(t, inv.Images[0].ID, gotID)
		command(0, "reset", "--problem", pid)
		_, err = toolEnv.Run(ctx, "docker", []string{"image", "inspect", gotID}, nil)
		if err == nil {
			t.Fatal("reset left owned image")
		}
		if info, err := os.Stat(archive); err != nil || info.Size() == 0 {
			t.Fatal("reset removed personal archive")
		}
		archived = append(archived, archive)
		t.Logf("CLEAN CYCLE %d prior=%s current=%s archived=%s", cycle, oldID, gotID, archive)
	}
	phase("ordinary Docker build passes functional grading; unproven cleanup is retained")
	dir, _, err := session.LatestAttempt(workspace, view.ID, pid)
	testutil.NoError(t, err)
	testutil.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(correct), 0600))
	run(t, "", "docker", "build", "--pull=false", "--network=none", "--tag", ref, dir)
	directID := strings.TrimSpace(run(t, "", "docker", "image", "inspect", "--format", "{{.Id}}", ref))
	run(t, "", "docker", "save", "--output", filepath.Join(dir, "image.tar"), ref)
	command(0, "grade", "--problem", pid)
	err = svc.Reset(ctx, view.ID, session.ResetOptions{ProblemID: pid})
	testutil.ErrorIs(t, err, environment.ErrNotOwned)
	if got := strings.TrimSpace(run(t, "", "docker", "image", "inspect", "--format", "{{.Id}}", ref)); got != directID {
		t.Fatal("unlabelled image changed")
	}
	err = svc.Start(ctx, view.ID, time.Now())
	testutil.ErrorIs(t, err, session.ErrResetPending)
	status, err := svc.Status(ctx, view.ID, time.Now())
	testutil.NoError(t, err)
	if len(status.Pending) == 0 {
		t.Fatal("cleanup failure not visible")
	}
	// The test owns this ordinary Docker build and explicitly removes its
	// exact ID. Application recovery must then reconcile the retained record.
	run(t, "", "docker", "image", "rm", directID)
	testutil.NoError(t, svc.Recover(ctx, id.ID))
	testutil.NoError(t, svc.Reset(ctx, view.ID, session.ResetOptions{ConfirmedEnvironmentID: id.ID}))
	if got := strings.TrimSpace(run(t, "", "docker", "image", "inspect", "--format", "{{.Id}}", sentinel)); got != sentinelID {
		t.Fatal("foreign sentinel changed")
	}
	data, err := os.ReadFile(personal)
	testutil.NoError(t, err)
	testutil.Equal(t, string(data), "alias k=kubectl\n")
	for _, path := range archived {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("archived files missing")
		}
	}
	ops, err := st.ListOperations(ctx, environment.OperationImageBuild)
	testutil.NoError(t, err)
	for _, op := range ops {
		if op.Phase != "done" {
			t.Fatalf("unfinished build %s", op.ID)
		}
	}
	if err = svc.Recover(ctx, id.ID); err != nil && !errors.Is(err, session.ErrResetPending) {
		t.Fatal(err)
	}
	phase("complete: two managed cycles, direct Docker grading, retained recovery, foreign sentinel and personal files verified")
}
