//go:build integration

package integration

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// faultyEngine is the real grader with an explicit fault injection point: it
// can make one criterion of a chosen problem come out as a platform ERROR,
// once, as a failing checking environment would. Nothing else is faked.
type faultyEngine struct {
	inner session.Evaluator
	mu    sync.Mutex
	fail  map[string]bool // problem ID -> the next evaluation has an ERROR
}

func (e *faultyEngine) failNext(problemID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fail == nil {
		e.fail = map[string]bool{}
	}
	e.fail[problemID] = true
}

func (e *faultyEngine) Evaluate(ctx context.Context, t grader.Target) (model.Evaluation, error) {
	ev, err := e.inner.Evaluate(ctx, t)
	if err != nil {
		return ev, err
	}
	e.mu.Lock()
	fail := e.fail[t.Problem.Definition.ID]
	delete(e.fail, t.Problem.Definition.ID)
	e.mu.Unlock()
	if fail && len(ev.Results) > 0 {
		last := len(ev.Results) - 1
		ev.Results[last].Status, ev.Results[last].Points = model.StatusError, 0
		ev.Results[last].Reason = "injected: the checking environment failed"
		ev.Complete, ev.Score = false, nil
	}
	return ev, nil
}

// TestSubmission runs the submission flow on a real environment with the real
// grader and the real input fingerprints: a mock exam is prepared and started;
// nothing is revealed before the submission; a first submission ends in a
// platform ERROR and has no score; an edited object makes --retry refused;
// restoring it lets the retry grade the same submission again (its submission
// time kept); a changed answer is resubmitted as a new submission, the earlier
// records untouched; and the answer opens only after a complete result.
func TestSubmission(t *testing.T) {
	requireTools(t, "docker", "kind")
	ctx := context.Background()
	m, id, dir, st := bootEnvironmentState(t)

	cfg := author.GraderConfig{StateDir: dir}
	rt, err := author.NewRuntime(ctx, m, st, id, cfg)
	testutil.NoError(t, err)
	t.Cleanup(func() { rt.Close() })

	// The dev fixtures need certificates before a session may serve them.
	dirs, err := filepath.Glob(filepath.Join(devBank, "problems", "*"))
	testutil.NoError(t, err)
	for _, d := range dirs {
		p, err := problem.Load(d)
		testutil.NoError(t, err)
		record, err := rt.Validator.Validate(ctx, p, coveringSeeds(t, p))
		testutil.NoError(t, err)
		testutil.Equal(t, record.Passed, true)
	}

	engine := &faultyEngine{inner: rt.Grader.Engine}
	workspace := t.TempDir()
	svc, err := session.NewService(session.Config{
		Store: st, Env: m, Engine: engine, Inputs: rt.Grader.Artifacts, Identity: rt.Identity,
		GraderVersion:       grader.GraderVersion,
		EnvironmentManifest: cfg.EnvironmentManifest,
		PrepareEnvironment: func(ctx context.Context, id model.Identity) error {
			return author.PrepareEnvironment(ctx, m, id, cfg.Images)
		},
		BankDir: devBank, StateDir: dir, Workspace: workspace,
	})
	testutil.NoError(t, err)

	// The exam starts at 08:30 and is submitted at 10:00, retried at 10:02 and
	// resubmitted at 10:05.
	at := func(h, min int) time.Time { return time.Date(2026, 10, 15, h, min, 0, 0, time.UTC) }
	prepared, err := svc.Prepare(ctx, session.StartOptions{Mode: model.ModeMock, MockID: "dev-mock", Seed: 7})
	testutil.NoError(t, err)
	sid := prepared.ID
	testutil.NoError(t, svc.Start(ctx, sid, at(8, 30)))
	frozen, err := st.LoadSession(ctx, sid)
	testutil.NoError(t, err)
	byID := map[string]model.FrozenProblem{}
	maxScore := 0
	for _, f := range frozen.Problems {
		byID[f.Definition.ID] = f
		for _, c := range f.Definition.Criteria {
			maxScore += c.Points
		}
	}
	cs := kubeClient(t, m, id)

	// solve applies a problem's reference solution, as a learner's answer.
	solve := func(problemID string) {
		t.Helper()
		f := byID[problemID]
		pkg, err := session.LoadPackage(dir, f.Hash)
		testutil.NoError(t, err)
		actions, err := author.BuildActions(pkg.Files, f.Parameters, f.Definition.Solution)
		testutil.NoError(t, err)
		_, err = m.Apply(ctx, id, environment.Owner{
			Inventory: model.Inventory{EnvironmentID: id.ID, SessionID: sid, ProblemID: problemID},
			Namespace: f.Namespace, Scope: f.Definition.Scope,
		}, actions)
		testutil.NoError(t, err)
	}
	var secrets []string
	for _, f := range frozen.Problems {
		pkg, err := session.LoadPackage(dir, f.Hash)
		testutil.NoError(t, err)
		for name, content := range pkg.Files {
			if strings.HasPrefix(name, "solution/") || name == problem.ExplanationFile {
				secrets = append(secrets, string(content))
			}
		}
	}
	secrets = append(secrets, "DEV-SOLUTION-MARKER", "DEV-CASE-MARKER", "DEV-EXPLANATION-MARKER")

	t.Run("NothingIsRevealedBeforeSubmitting", func(t *testing.T) {
		var out strings.Builder
		for _, f := range frozen.Problems {
			rv, err := svc.Review(ctx, sid, f.Definition.ID, true)
			testutil.ErrorIs(t, err, session.ErrDisclosureLocked)
			fmt.Fprintf(&out, "%+v\n%v\n", rv, err)
		}
		status, err := svc.Status(ctx, sid, at(9, 0))
		testutil.NoError(t, err)
		fmt.Fprintf(&out, "%+v\n", status)
		_, err = svc.Grade(ctx, sid, session.GradeOptions{ProblemID: frozen.Problems[0].Definition.ID, At: at(9, 0)})
		testutil.ErrorIs(t, err, session.ErrProblemGradingUnavailable)
		for _, s := range secrets {
			if strings.Contains(out.String(), s) {
				t.Errorf("revealed before the submission: %q", s)
			}
		}
	})

	// Four problems are solved for real; dev-sa-001 is left for later.
	for _, p := range devExamOrder {
		if p != "dev-sa-001" {
			solve(p)
		}
	}

	var original model.GradingRun
	t.Run("APlatformErrorLeavesNoScore", func(t *testing.T) {
		engine.failNext("dev-config-001")
		run, err := svc.Grade(ctx, sid, session.GradeOptions{At: at(10, 0)})
		testutil.NoError(t, err)
		original = run
		testutil.Equal(t, run.Evaluation.Complete, false)
		testutil.Equal(t, run.Evaluation.Score, (*int)(nil))
		testutil.Equal(t, len(run.Evaluation.Results), 0) // a mock withholds the details
		cur, err := st.LoadSession(ctx, sid)
		testutil.NoError(t, err)
		testutil.Equal(t, cur.State, model.StateGradingError)

		// Stored for real: the solved problem really passed, the total is
		// unknown, and the submission recorded its fingerprints and times.
		stored, err := st.GetRun(ctx, run.ID)
		testutil.NoError(t, err)
		testutil.Equal(t, stored.Evaluation.Score, (*int)(nil))
		passed := 0
		for _, r := range stored.Evaluation.Results {
			if strings.HasPrefix(r.CriterionID, "dev-service-001/") && r.Status == model.StatusPass {
				passed++
			}
		}
		testutil.Equal(t, passed, len(byID["dev-service-001"].Definition.Criteria))
		sub, err := st.GetSubmission(ctx, run.SubmissionID)
		testutil.NoError(t, err)
		testutil.Equal(t, sub.SubmittedAt, at(10, 0))
		testutil.Equal(t, sub.SolveDuration, 90*time.Minute)
		testutil.Equal(t, sub.WithinLimit, true)
		if len(sub.InputHashes) < 5 {
			t.Errorf("only %d input fingerprints were recorded: %v", len(sub.InputHashes), sub.InputHashes)
		}
		for k, v := range sub.InputHashes {
			if strings.Contains(v, "DEV-") {
				t.Errorf("fingerprint %s carries content: %q", k, v)
			}
		}

		_, err = svc.Review(ctx, sid, "dev-config-001", true)
		testutil.ErrorIs(t, err, session.ErrDisclosureLocked)
	})

	cfgNS := byID["dev-config-001"].Namespace
	label := func(value string) {
		t.Helper()
		patch := fmt.Sprintf(`{"metadata":{"labels":{"edited":%s}}}`, value)
		_, err := cs.CoreV1().ConfigMaps(cfgNS).Patch(ctx, "app-settings", types.MergePatchType, []byte(patch), metav1.PatchOptions{})
		testutil.NoError(t, err)
	}

	t.Run("AChangedObjectRefusesRetry", func(t *testing.T) {
		label(`"yes"`)
		_, err := svc.Grade(ctx, sid, session.GradeOptions{Retry: true, At: at(10, 2)})
		testutil.ErrorIs(t, err, session.ErrAnswerChanged)
		if !strings.Contains(err.Error(), "--resubmit") || !strings.Contains(err.Error(), "ConfigMap") {
			t.Errorf("the refusal does not say what changed and what to do: %v", err)
		}
		runs, err := st.ListRuns(ctx, original.SubmissionID)
		testutil.NoError(t, err)
		testutil.Equal(t, len(runs), 1)
		cur, _ := st.LoadSession(ctx, sid)
		testutil.Equal(t, cur.State, model.StateGradingError)
	})

	var retried model.GradingRun
	t.Run("AnUnchangedAnswerRetriesTheSameSubmission", func(t *testing.T) {
		label("null")
		var err error
		retried, err = svc.Grade(ctx, sid, session.GradeOptions{Retry: true, At: at(10, 2)})
		testutil.NoError(t, err)
		testutil.Equal(t, retried.SubmissionID, original.SubmissionID)
		testutil.Equal(t, retried.ID == original.ID, false)
		testutil.Equal(t, retried.StartedAt, at(10, 2))
		testutil.Equal(t, retried.Evaluation.Complete, true)
		sub, err := st.GetSubmission(ctx, retried.SubmissionID)
		testutil.NoError(t, err)
		testutil.Equal(t, sub.SubmittedAt, at(10, 0))

		// Four problems solved, the fifth in its starting state: a real total
		// that is neither zero nor the maximum.
		want := byID["dev-sa-001"].Definition.InitialScore
		for _, f := range frozen.Problems {
			if f.Definition.ID == "dev-sa-001" {
				continue
			}
			for _, c := range f.Definition.Criteria {
				want += c.Points
			}
		}
		testutil.Equal(t, *retried.Evaluation.Score, want)
		if want >= maxScore {
			t.Fatalf("the unsolved problem was worth nothing; the test proves little (%d of %d)", want, maxScore)
		}
		cur, _ := st.LoadSession(ctx, sid)
		testutil.Equal(t, cur.State, model.StateCompleted)

		// Both runs exist, the failed one untouched.
		runs, err := st.ListRuns(ctx, original.SubmissionID)
		testutil.NoError(t, err)
		testutil.Equal(t, len(runs), 2)
		testutil.Equal(t, runs[0].Evaluation.Complete, false)
	})

	t.Run("AChangedAnswerIsResubmitted", func(t *testing.T) {
		firstSub, err := st.GetSubmission(ctx, original.SubmissionID)
		testutil.NoError(t, err)
		firstRuns, err := st.ListRuns(ctx, original.SubmissionID)
		testutil.NoError(t, err)

		solve("dev-sa-001")
		second, err := svc.Grade(ctx, sid, session.GradeOptions{Resubmit: true, At: at(10, 5)})
		testutil.NoError(t, err)
		testutil.Equal(t, second.Evaluation.Complete, true)
		testutil.Equal(t, *second.Evaluation.Score, maxScore)
		sub, err := st.GetSubmission(ctx, second.SubmissionID)
		testutil.NoError(t, err)
		testutil.Equal(t, sub.SubmittedAt, firstSub.SubmittedAt.Add(5*time.Minute))
		testutil.Equal(t, sub.ParentSubmissionID, firstSub.ID)

		// Nothing earlier was touched.
		again, err := st.GetSubmission(ctx, original.SubmissionID)
		testutil.NoError(t, err)
		testutil.Equal(t, again, firstSub)
		againRuns, err := st.ListRuns(ctx, original.SubmissionID)
		testutil.NoError(t, err)
		testutil.Equal(t, againRuns, firstRuns)
		cur, _ := st.LoadSession(ctx, sid)
		testutil.Equal(t, cur.State, model.StateCompleted)
	})

	t.Run("TheAnswerOpensAfterAGoodResult", func(t *testing.T) {
		rv, err := svc.Review(ctx, sid, "dev-sa-001", true)
		testutil.NoError(t, err)
		if !strings.Contains(rv.ReferenceSolution, "DEV-SOLUTION-MARKER-SA") || !strings.Contains(rv.Explanation, "DEV-EXPLANATION-MARKER-SA") {
			t.Errorf("the answer was not opened from the frozen package: %q / %q", rv.ReferenceSolution, rv.Explanation)
		}
		testutil.Equal(t, rv.SolutionViewed, true)
		// The problem's own result, narrowed from the whole-session run.
		saMax := 0
		for _, c := range byID["dev-sa-001"].Definition.Criteria {
			saMax += c.Points
		}
		testutil.Equal(t, rv.Confirmed != nil, true)
		testutil.Equal(t, *rv.Confirmed.Evaluation.Score, saMax)
		whole, err := svc.Review(ctx, sid, "", false)
		testutil.NoError(t, err)
		testutil.Equal(t, whole.Confirmed != nil, true)
		testutil.Equal(t, *whole.Confirmed.Evaluation.Score, maxScore)
		testutil.Equal(t, len(whole.Runs), 3) // the failed run, its retry and the resubmission
		events, err := st.ListReviews(ctx, sid)
		testutil.NoError(t, err)
		testutil.Equal(t, len(events), 1)
		other, err := svc.Review(ctx, sid, "dev-scale-001", false)
		testutil.NoError(t, err)
		testutil.Equal(t, other.SolutionViewed, false)
	})
}
