//go:build linux

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/lock"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/store"
)

// Only the external cluster and input observation are substituted. These
// tests run real selection, freezing, SQLite, attempts, submission and review.
type cluster struct{ id model.Identity }

func (e cluster) Ensure(context.Context) (model.Identity, error) { return e.id, nil }
func (e cluster) Check(context.Context, model.Identity) error    { return nil }
func (e cluster) Doctor(context.Context, model.Identity) (environment.DoctorReport, error) {
	return environment.DoctorReport{}, nil
}
func (e cluster) Occupied(context.Context, model.Identity, []string, []model.ObjectRef) ([]string, error) {
	return nil, nil
}
func (e cluster) Apply(context.Context, model.Identity, environment.Owner, []environment.Action) (model.Inventory, error) {
	return model.Inventory{}, nil
}

type inputs struct{}

func (inputs) CaptureInputs(_ context.Context, t grader.Target) (map[string]string, error) {
	return map[string]string{t.Problem.Definition.ID: "unchanged"}, nil
}

type evaluator struct {
	calls          int
	solved, broken bool
	cause          error
}

func (e *evaluator) Evaluate(ctx context.Context, target grader.Target) (model.Evaluation, error) {
	if err := ctx.Err(); err != nil {
		return model.Evaluation{}, err
	}
	preparing := strings.Contains(target.Workspace, string(filepath.Separator)+"prepare"+string(filepath.Separator))
	if !preparing {
		e.calls++
		if e.cause != nil {
			return model.Evaluation{}, e.cause
		}
	}
	ev := model.Evaluation{Complete: true}
	score := 0
	for _, c := range target.Problem.Definition.Criteria {
		status := target.Problem.Definition.InitialExpected[c.ID]
		if !preparing && e.solved {
			status = model.StatusPass
		}
		if !preparing && e.broken {
			status = model.StatusError
			ev.Complete = false
		}
		r := model.CheckResult{CriterionID: c.ID, Status: status, Reason: "observed state", Evidence: model.Evidence{ObservedAt: time.Now(), Expected: "required", Actual: "observed"}}
		if status == model.StatusPass {
			r.Points = c.Points
			score += c.Points
		}
		ev.Results = append(ev.Results, r)
		ev.MaxScore += c.Points
	}
	if ev.Complete {
		ev.Score = &score
	}
	return ev, nil
}

type harness struct {
	deps                 Dependencies
	engine               *evaluator
	state, workspace, db string
	t                    *testing.T
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	must(t, os.MkdirAll(state, 0700))
	db := filepath.Join(state, "sessions.db")
	st, err := store.Open(db)
	must(t, err)
	t.Cleanup(func() { st.Close() })
	id := model.Identity{ID: "cli-test", Context: "junkloud-cli-test"}
	engine := &evaluator{}
	manifest := map[string]string{"test": "cli"}
	bank, err := filepath.Abs("../../tests/fixtures/dev-bank")
	must(t, err)
	entries, err := os.ReadDir(filepath.Join(bank, "problems"))
	must(t, err)
	for _, entry := range entries {
		p, err := problem.Load(filepath.Join(bank, "problems", entry.Name()))
		must(t, err)
		hash, err := problem.Hash(p)
		must(t, err)
		seeds, err := author.CoveringSeeds(p)
		must(t, err)
		must(t, st.SaveValidation(context.Background(), model.ValidationRecord{ProblemHash: hash, GraderVersion: "cli-test", EnvironmentHash: author.EnvironmentHash(manifest), Passed: true, Seeds: seeds, Cases: map[string]model.Evaluation{}}))
	}
	now := func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC) }
	svc, err := session.NewService(session.Config{Store: st, Env: cluster{id}, Engine: engine, Inputs: inputs{}, Identity: id, GraderVersion: "cli-test", EnvironmentManifest: func(context.Context, model.Identity) (map[string]string, error) { return manifest, nil }, PrepareEnvironment: func(context.Context, model.Identity) error { return nil }, BankDir: bank, StateDir: state, Workspace: filepath.Join(root, "work"), Now: now, NewSessionID: func(mode string) (string, error) { return mode + "-1", nil }, NewSeed: func() (uint64, error) { return 42, nil }})
	must(t, err)
	return &harness{deps: Dependencies{Session: svc, Store: st, Now: now}, engine: engine, state: state, workspace: filepath.Join(root, "work"), db: db, t: t}
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func (h *harness) run(input string, args ...string) (int, string, string) {
	h.t.Helper()
	var out, errOut bytes.Buffer
	cmd := NewRoot(h.deps, strings.NewReader(input), &out, &errOut)
	code := Execute(context.Background(), cmd, args)
	h.t.Logf("ckad %s; input=%q; exit=%d; stdout=%q; stderr=%q", strings.Join(args, " "), input, code, out.String(), errOut.String())
	return code, out.String(), errOut.String()
}
func (h *harness) start(mode string) {
	h.t.Helper()
	args := []string{"start", "--mode", mode}
	if mode == model.ModePractice {
		args = append(args, "--count", "5")
	} else {
		args = append(args, "dev-mock")
	}
	code, _, errOut := h.run("yes\n", args...)
	if code != 0 {
		h.t.Fatalf("start: %d %s", code, errOut)
	}
}

func TestEnglishPromptKoreanFeedback(t *testing.T) {
	h := newHarness(t)
	h.start(model.ModePractice)
	code, q, _ := h.run("", "show", "Q4")
	if code != 0 || !strings.Contains(q, "Namespace") {
		t.Fatal(code, q)
	}
	code, out, _ := h.run("", "grade", "--problem", "Q4")
	if code != 1 || !strings.Contains(out, "요구사항") {
		t.Fatal(code, out)
	}
	_, out, _ = h.run("", "review", "--problem", "Q4")
	if strings.Contains(out, "DEV-EXPLANATION-MARKER") {
		t.Fatal("implicit solution disclosure")
	}
	code, out, _ = h.run("", "review", "--problem", "Q4", "--solution")
	if code != 0 || !strings.Contains(out, "DEV-EXPLANATION-MARKER") {
		t.Fatal(code, out)
	}
	reviews, err := h.deps.Store.ListReviews(context.Background(), "practice-1")
	must(t, err)
	if len(reviews) != 1 {
		t.Fatal("solution view not recorded")
	}
}
func TestModeRestrictions(t *testing.T) {
	h := newHarness(t)
	h.start(model.ModeMock)
	for _, args := range [][]string{{"grade", "--problem", "Q4"}, {"review", "--problem", "Q4", "--solution"}, {"reset", "--problem", "Q4"}} {
		code, out, diag := h.run("", args...)
		if code != 2 || strings.Contains(out+diag, "DEV-") {
			t.Fatal(code, out, diag)
		}
	}
	if h.engine.calls != 0 {
		t.Fatal("mode rejection graded")
	}
}
func TestMarkDoesNotGrade(t *testing.T) {
	h := newHarness(t)
	h.start(model.ModePractice)
	code, _, _ := h.run("", "mark", "Q4", "--review")
	if code != 0 || h.engine.calls != 0 {
		t.Fatal("mark graded or failed")
	}
	_, out, _ := h.run("", "status")
	if !strings.Contains(out, "[review]") {
		t.Fatal(out)
	}
	code, _, _ = h.run("", "mark", "Q4", "--clear")
	if code != 0 {
		t.Fatal(code)
	}
	_, out, _ = h.run("", "status")
	if strings.Contains(out, "[review]") {
		t.Fatal(out)
	}
}
func TestCLIExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name           string
		solved, broken bool
		want           int
	}{{"unmet", false, false, 1}, {"full", true, false, 0}, {"error", false, true, 3}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.start(model.ModePractice)
			h.engine.solved, h.engine.broken = tc.solved, tc.broken
			code, out, _ := h.run("", "grade")
			if code != tc.want {
				t.Fatal(code, out)
			}
			if tc.broken && strings.Contains(out, "점수:") {
				t.Fatal("ERROR got total", out)
			}
		})
	}
	h := newHarness(t)
	for _, args := range [][]string{{"nonsense"}, {"start", "--mode", "practice", "--count", "2"}, {"grade", "--retry", "--resubmit"}, {"mark", "Q4", "--review", "--clear"}} {
		code, _, _ := h.run("", args...)
		if code != 2 {
			t.Fatal(args, code)
		}
	}
}
func TestTwoTerminalRead(t *testing.T) {
	h := newHarness(t)
	h.start(model.ModePractice)
	release, err := lock.Acquire(author.LockPath(h.state))
	must(t, err)
	defer release()
	st, err := store.Open(h.db)
	must(t, err)
	defer st.Close()
	h.deps.Store = st
	h.deps.Session = session.NewReader(st, "../../tests/fixtures/dev-bank", h.state, h.workspace)
	code, out, _ := h.run("", "status")
	if code != 0 || !strings.Contains(out, "practice-1") {
		t.Fatal(code, out)
	}
	code, out, _ = h.run("", "show", "Q4")
	if code != 0 || !strings.Contains(out, "Namespace") {
		t.Fatal(code, out)
	}
	code, _, _ = h.run("", "review")
	if code != 0 {
		t.Fatal(code)
	}
}
func TestNoPrivateContentInErrors(t *testing.T) {
	h := newHarness(t)
	h.start(model.ModePractice)
	h.engine.cause = errors.New("DEV-SOLUTION-MARKER DEV-CASE-MARKER Secret-token")
	for _, args := range [][]string{{"grade"}, {"review"}} {
		code, out, diag := h.run("", args...)
		if args[0] == "grade" && code != 3 {
			t.Fatal(code)
		}
		for _, private := range []string{"DEV-SOLUTION-MARKER", "DEV-CASE-MARKER", "Secret-token"} {
			if strings.Contains(out+diag, private) {
				t.Fatal("private error leaked", out, diag)
			}
		}
	}
}
func TestStartConfirmationAndResume(t *testing.T) {
	h := newHarness(t)
	code, out, _ := h.run("no\n", "start", "--mode", "practice", "--count", "5")
	if code != 0 || strings.Contains(out, "Namespace") {
		t.Fatal(code, out)
	}
	rec, err := h.deps.Store.LoadSession(context.Background(), "practice-1")
	must(t, err)
	if rec.StartedAt != nil {
		t.Fatal("clock started without confirmation")
	}
	code, out, _ = h.run("yes\n", "start")
	if code != 0 || !strings.Contains(out, "practice-1") {
		t.Fatal(code, out)
	}
}
func TestMockReferenceAndExistingGrade(t *testing.T) {
	h := newHarness(t)
	h.start(model.ModeMock)
	h.engine.solved = true
	code, out, _ := h.run("", "grade")
	if code != 0 || !strings.Contains(out, "66%") || strings.Contains(out, "합격") {
		t.Fatal(code, out)
	}
	calls := h.engine.calls
	code, out, _ = h.run("", "grade")
	if code != 0 || h.engine.calls != calls || !strings.Contains(out, "--resubmit") {
		t.Fatal(code, out)
	}
}
func TestCancellationAndUnimplementedCommands(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, diag bytes.Buffer
	code := Execute(ctx, NewRoot(h.deps, strings.NewReader("yes\n"), &out, &diag), []string{"start", "--mode", "practice", "--count", "5"})
	if code != 3 {
		t.Fatal(code)
	}
	for _, args := range [][]string{{"reset"}, {"reset", "--cluster"}, {"start", "--retry", "prior", "--failed"}} {
		code, _, diag := h.run("", args...)
		if code != 3 || !strings.Contains(diag, "아직") {
			t.Fatal(fmt.Sprint(args), code, diag)
		}
	}
}
func TestListOnlyShowsCertifiedPublicMetadata(t *testing.T) {
	h := newHarness(t)
	code, out, diag := h.run("", "list")
	if code != 0 || !strings.Contains(out, "dev-mock") || !strings.Contains(out, "dev-config-001") {
		t.Fatal(code, out, diag)
	}
	for _, private := range []string{"DEV-", "web-replicas", "solution/", "expected"} {
		if strings.Contains(out+diag, private) {
			t.Fatal(out, diag)
		}
	}
}
func TestExecutableReadsWithoutClusterAndHelpHasNoSideEffects(t *testing.T) {
	h := newHarness(t)
	h.start(model.ModePractice)
	t.Setenv("CKAD_STATE_DIR", h.state)
	t.Setenv("CKAD_WORKSPACE", h.workspace)
	t.Setenv("CKAD_BANK_DIR", "../../tests/fixtures/dev-bank")
	t.Setenv("PATH", t.TempDir())
	release, err := lock.Acquire(author.LockPath(h.state))
	must(t, err)
	defer release()
	for _, args := range [][]string{{"status"}, {"show", "Q4"}, {"review"}, {"--help"}} {
		var out, diag bytes.Buffer
		code := Run(context.Background(), args, strings.NewReader(""), &out, &diag)
		if code != 0 {
			t.Fatal(args, code, diag.String())
		}
	}
	state := filepath.Join(t.TempDir(), "absent")
	t.Setenv("CKAD_STATE_DIR", state)
	var out, diag bytes.Buffer
	if code := Run(context.Background(), []string{"--help"}, strings.NewReader(""), &out, &diag); code != 0 {
		t.Fatal(code)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("help created state", err)
	}
}

type waitingInput struct {
	entered chan struct{}
	release chan struct{}
}

func (r waitingInput) Read([]byte) (int, error) { close(r.entered); <-r.release; return 0, io.EOF }
func TestCancellationWhileAwaitingStartConfirmation(t *testing.T) {
	h := newHarness(t)
	h.run("no\n", "start", "--mode", "practice", "--count", "5")
	input := waitingInput{make(chan struct{}), make(chan struct{})}
	defer close(input.release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- Execute(ctx, NewRoot(h.deps, input, io.Discard, io.Discard), []string{"start"}) }()
	<-input.entered
	cancel()
	select {
	case code := <-done:
		if code != 3 {
			t.Fatal(code)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not stop confirmation wait")
	}
	rec, err := h.deps.Store.LoadSession(context.Background(), "practice-1")
	must(t, err)
	if rec.StartedAt != nil {
		t.Fatal("cancellation started clock")
	}
}

func TestTechnicalRetryKeepsSubmissionAndResubmitCreatesAnother(t *testing.T) {
	h := newHarness(t)
	h.start(model.ModePractice)
	h.engine.broken = true
	if code, _, _ := h.run("", "grade"); code != 3 {
		t.Fatal(code)
	}
	h.engine.broken = false
	h.engine.solved = true
	if code, _, diag := h.run("", "grade", "--retry"); code != 0 {
		t.Fatal(code, diag)
	}
	subs, err := h.deps.Store.ListSubmissions(context.Background(), "practice-1")
	must(t, err)
	if len(subs) != 1 {
		t.Fatal("retry made a new submission")
	}
	runs, err := h.deps.Store.ListRuns(context.Background(), subs[0].ID)
	must(t, err)
	if len(runs) != 2 {
		t.Fatal("retry did not record another grading run")
	}
	if code, _, diag := h.run("", "grade", "--resubmit"); code != 0 {
		t.Fatal(code, diag)
	}
	subs, err = h.deps.Store.ListSubmissions(context.Background(), "practice-1")
	must(t, err)
	if len(subs) != 2 || subs[1].ParentSubmissionID != subs[0].ID {
		t.Fatal("resubmit did not preserve submission lineage")
	}
}

func TestMockErrorWithholdsCriteriaAndFeedback(t *testing.T) {
	h := newHarness(t)
	h.start(model.ModeMock)
	h.engine.broken = true
	for _, args := range [][]string{{"grade"}, {"review"}} {
		code, out, diag := h.run("", args...)
		if args[0] == "grade" && code != 3 {
			t.Fatal(code, diag)
		}
		for _, hidden := range []string{"요구사항", "sa-no-automount", "자동으로 마운트", "점수:", "DEV-"} {
			if strings.Contains(out+diag, hidden) {
				t.Fatal("incomplete mock disclosed private content", out, diag)
			}
		}
	}
	if code, _, _ := h.run("", "review", "--problem", "Q4", "--solution"); code != 2 {
		t.Fatal(code)
	}
}

func TestFailureShowsExpectedAndObservedEvidence(t *testing.T) {
	h := newHarness(t)
	h.start(model.ModePractice)
	code, out, _ := h.run("", "grade", "--problem", "Q4")
	if code != 1 || !strings.Contains(out, "기대: required | 관찰: observed") {
		t.Fatal(code, out)
	}
}

func TestStartResumesPreparedSessionWithGlobalPathFlag(t *testing.T) {
	h := newHarness(t)
	h.run("no\n", "start", "--mode", "practice", "--count", "5")
	var out, diag bytes.Buffer
	root := NewRoot(h.deps, strings.NewReader("yes\n"), &out, &diag)
	root.PersistentFlags().String("state-dir", "", "installation path")
	if code := Execute(context.Background(), root, []string{"--state-dir", "injected-test-path", "start"}); code != 0 {
		t.Fatal(code, diag.String())
	}
	rec, err := h.deps.Store.LoadSession(context.Background(), "practice-1")
	must(t, err)
	if rec.StartedAt == nil {
		t.Fatal("prepared session did not start")
	}
}

func TestPendingGuidancePreservesProblemScopeAndAcceptedCommand(t *testing.T) {
	var out bytes.Buffer
	root := NewRoot(Dependencies{}, strings.NewReader(""), &out, io.Discard)
	printPending(root, []session.Pending{{OperationID: "pending", ProblemID: "dev-sa-001", Recorded: true, Guidance: "Accepted now: ckad grade --problem dev-sa-001 --resubmit (submits the answer as a new submission)."}})
	if !strings.Contains(out.String(), "ckad grade --problem dev-sa-001 --resubmit") || strings.Contains(out.String(), "ckad grade --retry") {
		t.Fatal(out.String())
	}
}

func TestExecutableBankValidationRefusesActiveLearningBeforeTools(t *testing.T) {
	h := newHarness(t)
	h.start(model.ModePractice)
	t.Setenv("CKAD_STATE_DIR", h.state)
	t.Setenv("CKAD_WORKSPACE", h.workspace)
	t.Setenv("CKAD_BANK_DIR", "../../tests/fixtures/dev-bank")
	t.Setenv("PATH", t.TempDir())
	dir := filepath.Join(h.state, "environment")
	must(t, os.MkdirAll(dir, 0700))
	identity, err := json.Marshal(model.Identity{ID: "cli-test"})
	must(t, err)
	must(t, os.WriteFile(filepath.Join(dir, "identity.json"), identity, 0600))
	var out, diag bytes.Buffer
	if code := Run(context.Background(), []string{"bank", "validate", "dev-config-001"}, strings.NewReader(""), &out, &diag); code != 2 || !strings.Contains(diag.String(), "진행 중") {
		t.Fatal(code, out.String(), diag.String())
	}
}
