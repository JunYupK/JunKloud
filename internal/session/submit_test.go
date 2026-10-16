//go:build linux

package session_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/lock"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// at is a time of the test day in UTC. The mock exam starts at 08:30 and lasts
// 120 minutes; the fixed times of the submission tests are 10:00 (first
// submission), 10:02 (retry) and 10:05 (resubmission).
func at(hour, minute int) time.Time {
	return time.Date(2026, 10, 15, hour, minute, 0, 0, time.UTC)
}

// startedMock prepares the dev mock exam and starts its clock at 08:30.
func (h *harness) startedMock() string {
	h.t.Helper()
	rec := h.prepareMock()
	testutil.NoError(h.t, h.svc.Start(ctx, rec.ID, at(8, 30)))
	return rec.ID
}

// startedPractice prepares a practice session of the five dev problems and
// starts it at 08:30.
func (h *harness) startedPractice() string {
	h.t.Helper()
	rec, err := h.svc.Prepare(ctx, session.StartOptions{Mode: model.ModePractice, Count: 5})
	testutil.NoError(h.t, err)
	testutil.NoError(h.t, h.svc.Start(ctx, rec.ID, at(8, 30)))
	return rec.ID
}

// answer makes the fake grader judge every problem with fn (nil: as solved).
func (h *harness) answer(fn func(model.FrozenProblem) model.Evaluation) {
	h.t.Helper()
	if fn == nil {
		fn = solvedEvaluation
	}
	h.eng.mu.Lock()
	defer h.eng.mu.Unlock()
	h.eng.override = map[string]func(model.FrozenProblem) model.Evaluation{}
	for _, p := range h.bankProblems() {
		h.eng.override[p.ID] = fn
	}
}

// answerOnly makes one problem judged with fn and every other one solved.
func (h *harness) answerOnly(problemID string, fn func(model.FrozenProblem) model.Evaluation) {
	h.t.Helper()
	h.answer(nil)
	h.eng.mu.Lock()
	defer h.eng.mu.Unlock()
	h.eng.override[problemID] = fn
}

func (h *harness) frozenIDs(sessionID string) []string {
	h.t.Helper()
	rec, err := h.st.LoadSession(ctx, sessionID)
	testutil.NoError(h.t, err)
	return problemIDs(rec)
}

func (h *harness) state(sessionID string) string {
	h.t.Helper()
	rec, err := h.st.LoadSession(ctx, sessionID)
	testutil.NoError(h.t, err)
	return rec.State
}

func (h *harness) submissions(sessionID string) []model.Submission {
	h.t.Helper()
	subs, err := h.st.ListSubmissions(ctx, sessionID)
	testutil.NoError(h.t, err)
	return subs
}

func (h *harness) runsOf(submissionID string) []model.GradingRun {
	h.t.Helper()
	runs, err := h.st.ListRuns(ctx, submissionID)
	testutil.NoError(h.t, err)
	return runs
}

// privateTexts are the reference answer (every solution file) and the
// explanation of a problem, read from the bank the session was frozen from.
func (h *harness) privateTexts(problemID string) []string {
	h.t.Helper()
	p := h.loadBankProblem(problemID)
	var out []string
	for name, content := range p.Files {
		if strings.HasPrefix(name, "solution/") || name == "explanation.ko.md" {
			out = append(out, string(content))
		}
	}
	if len(out) < 2 {
		h.t.Fatalf("problem %s has no solution or explanation", problemID)
	}
	return out
}

func (h *harness) allPrivateTexts() []string {
	var out []string
	for _, p := range h.bankProblems() {
		out = append(out, h.privateTexts(p.ID)...)
	}
	return append(out, privateMarkers...)
}

func requireNone(t *testing.T, what, output string, secrets []string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(output, secret) {
			t.Errorf("%s reveals %q", what, secret)
		}
	}
}

// TestExamDisclosure (spec 9.2): until the whole exam is submitted and graded
// without errors, a mock exam shows no explanation, reference answer or
// criterion detail, in any output; the grade itself is whole-session only.
// After a complete submission the answer opens on explicit request, and the
// view is recorded.
func TestExamDisclosure(t *testing.T) {
	h := newHarness(t)
	id := h.startedMock()
	ids := h.frozenIDs(id)
	secrets := h.allPrivateTexts()

	// Everything the learner can ask before submitting.
	var before strings.Builder
	status, err := h.svc.Status(ctx, id, at(9, 0))
	testutil.NoError(t, err)
	fmt.Fprintf(&before, "%+v\n", status)
	review, err := h.svc.Review(ctx, id, "", false)
	testutil.NoError(t, err)
	fmt.Fprintf(&before, "%+v\n", review)
	testutil.Equal(t, len(review.Runs), 0)
	for _, p := range ids {
		for _, solution := range []bool{false, true} {
			review, err := h.svc.Review(ctx, id, p, solution)
			fmt.Fprintf(&before, "%+v\n%v\n", review, err)
			if solution {
				testutil.ErrorIs(t, err, session.ErrDisclosureLocked)
			} else {
				testutil.NoError(t, err)
			}
			testutil.Equal(t, review.Explanation, "")
			testutil.Equal(t, review.ReferenceSolution, "")
		}
	}
	_, err = h.svc.Grade(ctx, id, session.GradeOptions{ProblemID: ids[0], At: at(9, 0)})
	testutil.ErrorIs(t, err, session.ErrProblemGradingUnavailable)
	fmt.Fprintf(&before, "%v\n", err)
	requireNone(t, "the output before submitting", before.String(), secrets)
	testutil.Equal(t, len(h.submissions(id)), 0)
	events, err := h.st.ListReviews(ctx, id)
	testutil.NoError(t, err)
	testutil.Equal(t, len(events), 0)

	// The first submission, with the answer solved: COMPLETED with a total.
	h.answer(nil)
	run, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 0)})
	testutil.NoError(t, err)
	testutil.Equal(t, run.Evaluation.Complete, true)
	testutil.Equal(t, *run.Evaluation.Score, run.Evaluation.MaxScore)
	testutil.Equal(t, h.state(id), model.StateCompleted)
	sub, err := h.st.GetSubmission(ctx, run.SubmissionID)
	testutil.NoError(t, err)
	testutil.Equal(t, sub.Kind, model.SubmissionSession)
	testutil.Equal(t, sub.SubmittedAt, at(10, 0))
	testutil.Equal(t, sub.SolveDuration, 90*time.Minute)
	testutil.Equal(t, sub.Overtime, time.Duration(0))
	testutil.Equal(t, sub.WithinLimit, true)
	testutil.Equal(t, sub.ClockAnomaly, false)
	testutil.Equal(t, len(sub.InputHashes) > 0, true)

	// Results are open now; the answer only on explicit request.
	review, err = h.svc.Review(ctx, id, "", false)
	testutil.NoError(t, err)
	testutil.Equal(t, len(review.Runs), 1)
	testutil.Equal(t, len(review.Runs[0].Evaluation.Results) > 0, true)
	testutil.Equal(t, review.Confirmed != nil, true)
	testutil.Equal(t, review.Explanation, "")
	testutil.Equal(t, review.ReferenceSolution, "")

	review, err = h.svc.Review(ctx, id, ids[0], true)
	testutil.NoError(t, err)
	for _, text := range h.privateTexts(ids[0]) {
		if !strings.Contains(review.Explanation+review.ReferenceSolution, strings.TrimSpace(text)) {
			t.Errorf("the opened answer lacks %q", text)
		}
	}
	testutil.Equal(t, review.SolutionViewed, true)
	events, err = h.st.ListReviews(ctx, id)
	testutil.NoError(t, err)
	testutil.Equal(t, len(events), 1)
	testutil.Equal(t, events[0].ProblemID, ids[0])

	other, err := h.svc.Review(ctx, id, ids[1], false)
	testutil.NoError(t, err)
	testutil.Equal(t, other.SolutionViewed, false)
}

// TestExamErrorWithholdsAnswer: an exam with an ERROR left is not complete, so
// it shows no total, no criterion detail and no answer until a retry
// completes it.
func TestExamErrorWithholdsAnswer(t *testing.T) {
	h := newHarness(t)
	id := h.startedMock()
	ids := h.frozenIDs(id)
	secrets := h.allPrivateTexts()

	h.answerOnly(ids[2], erroredEvaluation)
	run, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 0)})
	testutil.NoError(t, err)
	testutil.Equal(t, h.state(id), model.StateGradingError)
	testutil.Equal(t, run.Evaluation.Complete, false)

	var out strings.Builder
	fmt.Fprintf(&out, "%+v\n", run)
	review, err := h.svc.Review(ctx, id, "", false)
	testutil.NoError(t, err)
	fmt.Fprintf(&out, "%+v\n", review)
	testutil.Equal(t, review.Confirmed == nil, true)
	for _, r := range review.Runs {
		testutil.Equal(t, len(r.Evaluation.Results), 0)
		testutil.Equal(t, r.Evaluation.Score, (*int)(nil))
	}
	for _, p := range ids {
		review, err := h.svc.Review(ctx, id, p, true)
		testutil.ErrorIs(t, err, session.ErrDisclosureLocked)
		fmt.Fprintf(&out, "%+v\n%v\n", review, err)
	}
	requireNone(t, "an exam with an ERROR left", out.String(), secrets)
	events, err := h.st.ListReviews(ctx, id)
	testutil.NoError(t, err)
	testutil.Equal(t, len(events), 0)

	// Retried without the platform failure: complete, and open.
	h.answer(nil)
	run, err = h.svc.Grade(ctx, id, session.GradeOptions{Retry: true, At: at(10, 2)})
	testutil.NoError(t, err)
	testutil.Equal(t, run.Evaluation.Complete, true)
	_, err = h.svc.Review(ctx, id, ids[2], true)
	testutil.NoError(t, err)
}

// TestErrorNoFinalScore (spec 8.1): an ERROR anywhere leaves the submission
// incomplete: no score, no confirmed total, the session GRADING_ERROR, and
// the same for every way a result can fail to be determined.
func TestErrorNoFinalScore(t *testing.T) {
	t.Run("a criterion that could not be judged", func(t *testing.T) {
		h := newHarness(t)
		id := h.startedPractice()
		ids := h.frozenIDs(id)
		h.answerOnly(ids[0], erroredEvaluation)

		// Practice: the problem's grade shows what was confirmed, no total.
		incomplete, err := h.svc.Grade(ctx, id, session.GradeOptions{ProblemID: ids[0], At: at(9, 0)})
		testutil.NoError(t, err)
		testutil.Equal(t, incomplete.Evaluation.Score, (*int)(nil))
		testutil.Equal(t, incomplete.Evaluation.Complete, false)
		testutil.Equal(t, len(incomplete.Evaluation.Results) > 0, true)
		stored, err := h.st.GetRun(ctx, incomplete.ID)
		testutil.NoError(t, err)
		testutil.Equal(t, stored.Evaluation.Score, (*int)(nil))
		testutil.Equal(t, stored.Evaluation.Complete, false)
		testutil.Equal(t, h.state(id), model.StateRunning)

		review, err := h.svc.Review(ctx, id, ids[0], false)
		testutil.NoError(t, err)
		testutil.Equal(t, review.Confirmed == nil, true)

		// The whole session: ERROR in one problem leaves no total at all.
		whole, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(9, 5)})
		testutil.NoError(t, err)
		testutil.Equal(t, whole.Evaluation.Score, (*int)(nil))
		testutil.Equal(t, h.state(id), model.StateGradingError)
	})

	t.Run("the evaluation itself failed", func(t *testing.T) {
		h := newHarness(t)
		id := h.startedMock()
		ids := h.frozenIDs(id)
		h.answer(nil)
		h.eng.errs = map[string]error{ids[1]: errors.New("the grader lost the cluster")}
		run, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 0)})
		testutil.NoError(t, err)
		testutil.Equal(t, run.Evaluation.Score, (*int)(nil))
		testutil.Equal(t, run.Evaluation.Complete, false)
		testutil.Equal(t, h.state(id), model.StateGradingError)
		stored, err := h.st.GetRun(ctx, run.ID)
		testutil.NoError(t, err)
		testutil.Equal(t, stored.Evaluation.Complete, false)
		hasError := false
		for _, r := range stored.Evaluation.Results {
			if r.Status == model.StatusError && strings.Contains(r.Reason, "lost the cluster") {
				hasError = true
			}
		}
		testutil.Equal(t, hasError, true)
	})

	t.Run("the answer changed while it was graded", func(t *testing.T) {
		h := newHarness(t)
		id := h.startedMock()
		ids := h.frozenIDs(id)
		h.answer(nil)
		// A perfect evaluation, but the learner edits an object meanwhile.
		h.eng.beforeReturn = func(f model.FrozenProblem) {
			if f.Definition.ID == ids[3] {
				h.inputs.change(ids[3])
			}
		}
		run, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 0)})
		testutil.NoError(t, err)
		testutil.Equal(t, run.Evaluation.Score, (*int)(nil))
		testutil.Equal(t, run.Evaluation.Complete, false)
		testutil.Equal(t, h.state(id), model.StateGradingError)
		stored, err := h.st.GetRun(ctx, run.ID)
		testutil.NoError(t, err)
		var reasons []string
		for _, r := range stored.Evaluation.Results {
			if r.Status == model.StatusError {
				reasons = append(reasons, r.Reason)
			}
		}
		testutil.Equal(t, len(reasons), 1)
		if !strings.Contains(reasons[0], ids[3]) {
			t.Errorf("the reason does not name the problem whose answer changed: %q", reasons[0])
		}
	})

	t.Run("the answer could not be fingerprinted", func(t *testing.T) {
		h := newHarness(t)
		id := h.startedMock()
		h.answer(nil)
		h.inputs.setErr(errors.New("the API server is unreachable"))
		run, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 0)})
		testutil.NoError(t, err)
		testutil.Equal(t, run.Evaluation.Score, (*int)(nil))
		testutil.Equal(t, h.state(id), model.StateGradingError)
		sub, err := h.st.GetSubmission(ctx, run.SubmissionID)
		testutil.NoError(t, err)
		testutil.Equal(t, len(sub.InputHashes), 0)
	})

	t.Run("a complete grade needs a clean session", func(t *testing.T) {
		h := newHarness(t)
		id := h.startedMock()
		h.answer(nil)
		run, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 0)})
		testutil.NoError(t, err)
		testutil.Equal(t, run.Evaluation.Complete, true)
		testutil.Equal(t, h.state(id), model.StateCompleted)
	})
}

// TestInterruptedGradeIsRecorded: a command cancelled mid-grade (Ctrl-C) still
// records its run as incomplete, so the session is GRADING_ERROR with nothing
// pending rather than stuck in GRADING.
func TestInterruptedGradeIsRecorded(t *testing.T) {
	h := newHarness(t)
	id := h.startedMock()
	h.answer(nil)
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	h.eng.beforeReturn = func(model.FrozenProblem) { cancel() }

	run, err := h.svc.Grade(cancelCtx, id, session.GradeOptions{At: at(10, 0)})
	testutil.ErrorIs(t, err, context.Canceled)
	testutil.Equal(t, run.Evaluation.Complete, false)
	testutil.Equal(t, run.Evaluation.Score, (*int)(nil))
	testutil.Equal(t, h.state(id), model.StateGradingError)
	stored := h.runsOf(h.submissions(id)[0].ID)
	testutil.Equal(t, len(stored), 1)
	testutil.Equal(t, stored[0].Evaluation.Complete, false)
	status, err := h.svc.Status(ctx, id, at(10, 1))
	testutil.NoError(t, err)
	testutil.Equal(t, len(status.Pending), 0)

	// And the recovery is the ordinary retry.
	h.eng.beforeReturn = nil
	retried, err := h.svc.Grade(ctx, id, session.GradeOptions{Retry: true, At: at(10, 2)})
	testutil.NoError(t, err)
	testutil.Equal(t, retried.Evaluation.Complete, true)
}

// TestGradingTimeIsNotSolvingTime: the solving time ends at the submission;
// the grader's own running time is recorded on the run only. Overtime never
// blocks a submission, it is recorded; a clock that ran backwards is flagged.
func TestGradingTimeIsNotSolvingTime(t *testing.T) {
	h := newHarness(t)
	id := h.startedMock()
	h.answer(nil)
	h.eng.beforeReturn = func(model.FrozenProblem) { h.clock.Set(h.clock.Now().Add(4 * time.Minute)) }
	h.clock.Set(at(10, 50))

	// 10:50 is 140 minutes after the 08:30 start: 20 minutes over the limit.
	run, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 50)})
	testutil.NoError(t, err)
	testutil.Equal(t, run.Evaluation.Complete, true)
	sub, err := h.st.GetSubmission(ctx, run.SubmissionID)
	testutil.NoError(t, err)
	testutil.Equal(t, sub.SolveDuration, 140*time.Minute)
	testutil.Equal(t, sub.Overtime, 20*time.Minute)
	testutil.Equal(t, sub.WithinLimit, false)
	testutil.Equal(t, sub.ClockAnomaly, false)
	testutil.Equal(t, run.StartedAt, at(10, 50))
	// Five problems each moved the fake clock four minutes while grading.
	testutil.Equal(t, run.FinishedAt.After(run.StartedAt), true)

	t.Run("exactly at the limit is within it", func(t *testing.T) {
		h := newHarness(t)
		id := h.startedMock()
		h.answer(nil)
		run, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 30)})
		testutil.NoError(t, err)
		sub, _ := h.st.GetSubmission(ctx, run.SubmissionID)
		testutil.Equal(t, sub.SolveDuration, 120*time.Minute)
		testutil.Equal(t, sub.WithinLimit, true)
		testutil.Equal(t, sub.Overtime, time.Duration(0))
	})

	t.Run("a clock that ran backwards is flagged", func(t *testing.T) {
		h := newHarness(t)
		id := h.startedMock()
		h.answer(nil)
		run, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(8, 0)})
		testutil.NoError(t, err)
		sub, _ := h.st.GetSubmission(ctx, run.SubmissionID)
		testutil.Equal(t, sub.ClockAnomaly, true)
		testutil.Equal(t, sub.SolveDuration, time.Duration(0))
		testutil.Equal(t, sub.WithinLimit, false)
		testutil.Equal(t, run.Evaluation.Complete, true)
	})

	t.Run("a practice session has no limit", func(t *testing.T) {
		h := newHarness(t)
		id := h.startedPractice()
		h.answer(nil)
		ids := h.frozenIDs(id)
		run, err := h.svc.Grade(ctx, id, session.GradeOptions{ProblemID: ids[0], At: at(20, 0)})
		testutil.NoError(t, err)
		sub, _ := h.st.GetSubmission(ctx, run.SubmissionID)
		testutil.Equal(t, sub.Kind, model.SubmissionProblem)
		testutil.Equal(t, sub.ProblemID, ids[0])
		testutil.Equal(t, sub.Overtime, time.Duration(0))
		testutil.Equal(t, sub.WithinLimit, true)
	})
}

// TestGradeUsesTheFrozenPackage: grading and reviewing use what the session
// froze, not the bank as it is now.
func TestGradeUsesTheFrozenPackage(t *testing.T) {
	h := newHarness(t)
	id := h.startedMock()
	rec, err := h.st.LoadSession(ctx, id)
	testutil.NoError(t, err)
	ids := problemIDs(rec)
	h.answer(nil)

	// The bank is edited (and then removed) after the session was frozen.
	editFile(t, filepath.Join(h.bank, "problems", "dev-scale-001", "problem.yaml"), "points: 2\n    depends_on: [web-exists]", "points: 9\n    depends_on: [web-exists]")
	editFile(t, filepath.Join(h.bank, "problems", "dev-scale-001", "solution", "web.yaml"), "DEV-SOLUTION-MARKER-SCALE", "CHANGED-SOLUTION")
	prepared := len(h.eng.targets) // the initial-state evaluations of Prepare
	run, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 0)})
	testutil.NoError(t, err)
	testutil.Equal(t, run.Evaluation.Complete, true)
	graded := h.eng.targets[prepared:]
	for _, target := range graded {
		var frozen model.FrozenProblem
		for _, f := range rec.Problems {
			if f.Definition.ID == target.Problem.Definition.ID {
				frozen = f
			}
		}
		testutil.Equal(t, target.Problem.Hash, frozen.Hash)
		testutil.Equal(t, len(target.Problem.Definition.Criteria), len(frozen.Definition.Criteria))
		for i, c := range frozen.Definition.Criteria {
			testutil.Equal(t, target.Problem.Definition.Criteria[i].Points, c.Points)
		}
		// Each problem is graded in its own attempt directory.
		dir, _, err := session.LatestAttempt(h.workspace, id, frozen.Definition.ID)
		testutil.NoError(t, err)
		testutil.Equal(t, target.Workspace, dir)
	}
	testutil.Equal(t, len(graded), len(ids))

	testutil.NoError(t, os.RemoveAll(h.bank))
	review, err := h.svc.Review(ctx, id, "dev-scale-001", true)
	testutil.NoError(t, err)
	if !strings.Contains(review.ReferenceSolution, "DEV-SOLUTION-MARKER-SCALE") || strings.Contains(review.ReferenceSolution, "CHANGED-SOLUTION") {
		t.Errorf("the review does not come from the frozen package: %q", review.ReferenceSolution)
	}
}

// TestGradeRequests covers the requests Grade refuses before it records
// anything.
func TestGradeRequests(t *testing.T) {
	h := newHarness(t)
	rec := h.prepareMock()
	id := rec.ID

	// Not started: the clock does not run and nothing can be submitted.
	_, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(9, 0)})
	testutil.ErrorIs(t, err, session.ErrNotStarted)
	_, err = h.svc.Review(ctx, id, "", false)
	testutil.ErrorIs(t, err, session.ErrNotStarted)

	testutil.NoError(t, h.svc.Start(ctx, id, at(8, 30)))
	h.answer(nil)

	_, err = h.svc.Grade(ctx, id, session.GradeOptions{Retry: true, Resubmit: true, At: at(9, 0)})
	testutil.ErrorIs(t, err, session.ErrConflictingOptions)
	_, err = h.svc.Grade(ctx, id, session.GradeOptions{Retry: true, At: at(9, 0)})
	testutil.ErrorIs(t, err, session.ErrNothingToRetry)
	_, err = h.svc.Grade(ctx, id, session.GradeOptions{Resubmit: true, At: at(9, 0)})
	testutil.ErrorIs(t, err, session.ErrNothingToResubmit)
	_, err = h.svc.Grade(ctx, "no-such-session", session.GradeOptions{At: at(9, 0)})
	testutil.ErrorIs(t, err, store.ErrNotFound)
	_, err = h.svc.Review(ctx, id, "dev-nope-001", false)
	testutil.ErrorIs(t, err, session.ErrUnknownProblem)
	_, err = h.svc.Review(ctx, id, "", true)
	testutil.ErrorIs(t, err, session.ErrInvalidOptions)
	testutil.Equal(t, len(h.submissions(id)), 0)

	// Once submitted, a plain grade only points to the next command and
	// returns the existing result; nothing new is recorded.
	first, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 0)})
	testutil.NoError(t, err)
	again, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 1)})
	testutil.ErrorIs(t, err, session.ErrAlreadySubmitted)
	if !strings.Contains(err.Error(), "--resubmit") {
		t.Errorf("the error does not point to the next command: %v", err)
	}
	testutil.Equal(t, again.ID, first.ID)
	_, err = h.svc.Grade(ctx, id, session.GradeOptions{Retry: true, At: at(10, 1)})
	testutil.ErrorIs(t, err, session.ErrNothingToRetry)
	testutil.Equal(t, len(h.submissions(id)), 1)

	// A practice problem must exist in the session.
	practice := newHarness(t)
	pid := practice.startedPractice()
	_, err = practice.svc.Grade(ctx, pid, session.GradeOptions{ProblemID: "dev-nope-001", At: at(9, 0)})
	testutil.ErrorIs(t, err, session.ErrUnknownProblem)
}

// TestGradeRefusesAStaleSession: a session graded by another grader version
// or in an environment with other tool versions is not graded; nothing is
// recorded.
func TestGradeRefusesAStaleSession(t *testing.T) {
	h := newHarness(t)
	id := h.startedMock()
	h.answer(nil)

	root := h.root
	upgraded := newHarness(t, func(o *harnessOption) { o.root = root; o.graderVersion = "grader-next" })
	_, err := upgraded.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 0)})
	testutil.ErrorIs(t, err, session.ErrSessionStale)

	newTools := newHarness(t, func(o *harnessOption) {
		o.root = root
		o.manifest = func() map[string]string {
			m := map[string]string{}
			for k, v := range testManifest {
				m[k] = v
			}
			m["tool:kubectl"] = "v9.9.9"
			return m
		}
	})
	_, err = newTools.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 0)})
	testutil.ErrorIs(t, err, session.ErrSessionStale)

	testutil.Equal(t, len(h.submissions(id)), 0)
	testutil.Equal(t, h.state(id), model.StateRunning)
	_, err = h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 0)})
	testutil.NoError(t, err)
}

// TestGradeLockAndReads: grading takes the environment lock, so it cannot
// overlap start, reset, validation or another grade; reading (status, review)
// never needs it.
func TestGradeLockAndReads(t *testing.T) {
	h := newHarness(t)
	id := h.startedMock()
	h.answer(nil)

	release, err := lock.Acquire(author.LockPath(h.stateDir))
	testutil.NoError(t, err)
	_, err = h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 0)})
	testutil.ErrorIs(t, err, lock.ErrLocked)
	_, err = h.svc.Status(ctx, id, at(10, 0))
	testutil.NoError(t, err)
	_, err = h.svc.Review(ctx, id, "", false)
	testutil.NoError(t, err)
	testutil.NoError(t, release())
	testutil.Equal(t, len(h.submissions(id)), 0)

	_, err = h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 0)})
	testutil.NoError(t, err)
	// The lock is free again afterwards.
	release, err = lock.Acquire(author.LockPath(h.stateDir))
	testutil.NoError(t, err)
	testutil.NoError(t, release())
}

// crashExit is the exit status of the helper process that dies mid-grade.
const crashExit = 87

// crashRecords is the helper process's view of the store: it is the real
// store, except that it dies at one chosen step, like a killed process would.
type crashRecords struct {
	*store.Store
	point string
}

func (c crashRecords) SubmitAndTransition(ctx2 context.Context, sub model.Submission, from, to string) error {
	if c.point == "submit" {
		os.Exit(crashExit)
	}
	return c.Store.SubmitAndTransition(ctx2, sub, from, to)
}

func (c crashRecords) SaveRunAndTransition(ctx2 context.Context, run model.GradingRun, sessionID, from, to string) error {
	if c.point == "save" {
		os.Exit(crashExit)
	}
	return c.Store.SaveRunAndTransition(ctx2, run, sessionID, from, to)
}

// TestCrashHelperProcess is not a test: it is the process TestCrashDuringGrade
// starts and kills. It attaches to the installation named by the environment,
// grades, and exits abruptly at the chosen point.
func TestCrashHelperProcess(t *testing.T) {
	root := os.Getenv("JUNKLOUD_CRASH_ROOT")
	if root == "" {
		t.Skip("helper process of TestCrashDuringGrade")
	}
	point, sid := os.Getenv("JUNKLOUD_CRASH_POINT"), os.Getenv("JUNKLOUD_CRASH_SESSION")
	h := newHarness(t, func(o *harnessOption) {
		o.root = root
		o.records = func(st *store.Store) session.Records { return crashRecords{Store: st, point: point} }
	})
	h.answer(nil)
	if point == "evaluate" {
		h.eng.beforeReturn = func(model.FrozenProblem) { os.Exit(crashExit) }
	}
	_, err := h.svc.Grade(ctx, sid, session.GradeOptions{At: at(10, 0)})
	t.Fatalf("the process should have died at %q; Grade returned %v", point, err)
}

// TestCrashDuringGrade (spec 11): a process killed after the grading intent
// was recorded and before the run was saved leaves an unfinished operation.
// The next Status/Review/Grade finds it, shows no confirmed score and says how
// to recover; recovering keeps the original submission time.
func TestCrashDuringGrade(t *testing.T) {
	for _, point := range []string{"submit", "evaluate", "save"} {
		t.Run(point, func(t *testing.T) {
			h := newHarness(t)
			id := h.startedMock()

			cmd := exec.Command(os.Args[0], "-test.run=^TestCrashHelperProcess$", "-test.count=1")
			cmd.Env = append(os.Environ(), "JUNKLOUD_CRASH_ROOT="+h.root, "JUNKLOUD_CRASH_POINT="+point, "JUNKLOUD_CRASH_SESSION="+id)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != crashExit {
				t.Fatalf("the helper did not die at %q: %v\n%s", point, err, out)
			}

			// The next command finds the unfinished operation.
			status, err := h.svc.Status(ctx, id, at(10, 1))
			testutil.NoError(t, err)
			testutil.Equal(t, len(status.Pending), 1)
			pending := status.Pending[0]
			testutil.Equal(t, pending.ProblemID, "")
			if pending.Guidance == "" {
				t.Error("no recovery guidance")
			}
			review, err := h.svc.Review(ctx, id, "", false)
			testutil.NoError(t, err)
			testutil.Equal(t, review.Confirmed == nil, true)
			testutil.Equal(t, len(review.Pending), 1)
			testutil.Equal(t, len(review.Runs), 0)
			requireNone(t, "the review after a crash", fmt.Sprintf("%+v", review), h.allPrivateTexts())

			if point == "submit" {
				// Nothing but the intent was recorded: the session is still
				// RUNNING and the next grade is a first submission.
				testutil.Equal(t, h.state(id), model.StateRunning)
				testutil.Equal(t, len(h.submissions(id)), 0)
				h.answer(nil)
				run, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 1)})
				testutil.NoError(t, err)
				testutil.Equal(t, run.Evaluation.Complete, true)
				sub, _ := h.st.GetSubmission(ctx, run.SubmissionID)
				testutil.Equal(t, sub.SubmittedAt, at(10, 1))
				status, err = h.svc.Status(ctx, id, at(10, 5))
				testutil.NoError(t, err)
				testutil.Equal(t, len(status.Pending), 0)
				return
			}

			// The submission is recorded, the session is GRADING, and there is
			// no run: the score of that submission is unknown.
			testutil.Equal(t, h.state(id), model.StateGrading)
			subs := h.submissions(id)
			testutil.Equal(t, len(subs), 1)
			testutil.Equal(t, subs[0].SubmittedAt, at(10, 0))
			testutil.Equal(t, len(h.runsOf(subs[0].ID)), 0)
			testutil.Equal(t, pending.SubmissionID, subs[0].ID)
			if !strings.Contains(pending.Guidance, "--retry") || !strings.Contains(pending.Guidance, "--resubmit") {
				t.Errorf("the guidance does not name the recovery commands: %q", pending.Guidance)
			}

			_, err = h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 1)})
			testutil.ErrorIs(t, err, session.ErrGradeInterrupted)
			testutil.Equal(t, len(h.submissions(id)), 1)

			// Retrying grades the same submission again and keeps its time.
			h.answer(nil)
			run, err := h.svc.Grade(ctx, id, session.GradeOptions{Retry: true, At: at(10, 2)})
			testutil.NoError(t, err)
			testutil.Equal(t, run.SubmissionID, subs[0].ID)
			testutil.Equal(t, run.Evaluation.Complete, true)
			testutil.Equal(t, run.StartedAt, at(10, 2))
			testutil.Equal(t, h.state(id), model.StateCompleted)
			sub, _ := h.st.GetSubmission(ctx, run.SubmissionID)
			testutil.Equal(t, sub.SubmittedAt, at(10, 0))
			status, err = h.svc.Status(ctx, id, at(10, 5))
			testutil.NoError(t, err)
			testutil.Equal(t, len(status.Pending), 0)
			review, err = h.svc.Review(ctx, id, "", false)
			testutil.NoError(t, err)
			testutil.Equal(t, review.Confirmed != nil, true)
		})
	}

	t.Run("resubmitting instead of retrying", func(t *testing.T) {
		h := newHarness(t)
		id := h.startedMock()
		cmd := exec.Command(os.Args[0], "-test.run=^TestCrashHelperProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(), "JUNKLOUD_CRASH_ROOT="+h.root, "JUNKLOUD_CRASH_POINT=save", "JUNKLOUD_CRASH_SESSION="+id)
		_, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != crashExit {
			t.Fatalf("the helper did not die: %v", err)
		}
		h.answer(nil)
		run, err := h.svc.Grade(ctx, id, session.GradeOptions{Resubmit: true, At: at(10, 5)})
		testutil.NoError(t, err)
		subs := h.submissions(id)
		testutil.Equal(t, len(subs), 2)
		testutil.Equal(t, run.SubmissionID, subs[1].ID)
		testutil.Equal(t, subs[1].ParentSubmissionID, subs[0].ID)
		testutil.Equal(t, subs[1].SubmittedAt, at(10, 5))
		status, err := h.svc.Status(ctx, id, at(10, 6))
		testutil.NoError(t, err)
		testutil.Equal(t, len(status.Pending), 0)
	})
}
