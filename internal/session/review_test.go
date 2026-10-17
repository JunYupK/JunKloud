//go:build linux

package session_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// TestPracticeDisclosure (spec 9.2): a practice session grades one problem at
// a time without ending, shows the failure details when asked to grade, opens
// the explanation and solution on explicit request at any time, records each
// view, and remembers in later submissions that the solution had been seen.
func TestPracticeDisclosure(t *testing.T) {
	h := newHarness(t)
	id := h.startedPractice()
	ids := h.frozenIDs(id)
	p1, p2 := ids[0], ids[1]

	// Before any grading the explanation opens on request, and only that
	// problem's.
	review, err := h.svc.Review(ctx, id, p1, true)
	testutil.NoError(t, err)
	for _, text := range h.privateTexts(p1) {
		if !strings.Contains(review.Explanation+review.ReferenceSolution, strings.TrimSpace(text)) {
			t.Errorf("the opened answer lacks %q", text)
		}
	}
	requireNone(t, "the review of "+p1, fmt.Sprintf("%+v", review), h.privateTexts(p2))
	testutil.Equal(t, review.SolutionViewed, true)
	events, err := h.st.ListReviews(ctx, id)
	testutil.NoError(t, err)
	testutil.Equal(t, len(events), 1)

	// Grading p2 (not yet viewed): the details come with the request, the
	// session goes on.
	h.answerOnly(p2, initialEvaluation)
	first, err := h.svc.Grade(ctx, id, session.GradeOptions{ProblemID: p2, At: at(9, 0)})
	testutil.NoError(t, err)
	testutil.Equal(t, first.Evaluation.Complete, true)
	testutil.Equal(t, len(first.Evaluation.Results) > 0, true)
	for _, r := range first.Evaluation.Results {
		if !strings.HasPrefix(r.CriterionID, p2+"/") {
			t.Errorf("a result of another problem in %s's grade: %s", p2, r.CriterionID)
		}
	}
	testutil.Equal(t, h.state(id), model.StateRunning)
	sub, err := h.st.GetSubmission(ctx, first.SubmissionID)
	testutil.NoError(t, err)
	testutil.Equal(t, sub.Kind, model.SubmissionProblem)
	testutil.Equal(t, sub.ProblemID, p2)
	testutil.Equal(t, len(sub.ViewedSolutions), 0)
	testutil.Equal(t, first.StartedAt, at(9, 0))

	// Grade p1, whose solution was looked at: the submission says so.
	h.answer(nil)
	viewedRun, err := h.svc.Grade(ctx, id, session.GradeOptions{ProblemID: p1, At: at(9, 10)})
	testutil.NoError(t, err)
	viewedSub, _ := h.st.GetSubmission(ctx, viewedRun.SubmissionID)
	testutil.Equal(t, viewedSub.ViewedSolutions, []string{p1})

	// Grade p2 again after opening its solution: a new run, flagged.
	_, err = h.svc.Review(ctx, id, p2, true)
	testutil.NoError(t, err)
	second, err := h.svc.Grade(ctx, id, session.GradeOptions{ProblemID: p2, At: at(9, 20)})
	testutil.NoError(t, err)
	testutil.Equal(t, second.ID == first.ID, false)
	testutil.Equal(t, second.SubmissionID == first.SubmissionID, false)
	secondSub, _ := h.st.GetSubmission(ctx, second.SubmissionID)
	testutil.Equal(t, secondSub.ViewedSolutions, []string{p2})
	testutil.Equal(t, secondSub.ParentSubmissionID, first.SubmissionID)
	testutil.Equal(t, h.state(id), model.StateRunning)

	// Review: every run of the problem with its time, the first one earlier
	// than the second; SolutionViewed for p2 and p1, not for the others.
	review, err = h.svc.Review(ctx, id, p2, false)
	testutil.NoError(t, err)
	testutil.Equal(t, len(review.Runs), 2)
	testutil.Equal(t, review.Runs[0].ID, first.ID)
	testutil.Equal(t, review.Runs[1].ID, second.ID)
	testutil.Equal(t, review.SolutionViewed, true)
	testutil.Equal(t, review.Explanation, "")
	testutil.Equal(t, review.Confirmed != nil && review.Confirmed.ID == second.ID, true)
	untouched, err := h.svc.Review(ctx, id, ids[2], false)
	testutil.NoError(t, err)
	testutil.Equal(t, untouched.SolutionViewed, false)
	testutil.Equal(t, len(untouched.Runs), 0)
	testutil.Equal(t, untouched.Confirmed == nil, true)

	// Best scores of different runs are never added into one total: the
	// session-wide review lists the problems' own runs, and there is no run
	// that covers more than one problem.
	all, err := h.svc.Review(ctx, id, "", false)
	testutil.NoError(t, err)
	testutil.Equal(t, len(all.Runs), 3)
	testutil.Equal(t, all.Confirmed == nil, true)
	for _, r := range all.Runs {
		problems := map[string]bool{}
		for _, res := range r.Evaluation.Results {
			problems[strings.SplitN(res.CriterionID, "/", 2)[0]] = true
		}
		testutil.Equal(t, len(problems), 1)
	}

	// A whole-session grade is a new consistent total of its own; it ends the
	// practice session.
	whole, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(9, 40)})
	testutil.NoError(t, err)
	testutil.Equal(t, whole.Evaluation.Complete, true)
	testutil.Equal(t, h.state(id), model.StateCompleted)
	wholeSub, _ := h.st.GetSubmission(ctx, whole.SubmissionID)
	testutil.Equal(t, wholeSub.Kind, model.SubmissionSession)
	testutil.Equal(t, len(wholeSub.ViewedSolutions), 2)
	all, err = h.svc.Review(ctx, id, "", false)
	testutil.NoError(t, err)
	testutil.Equal(t, all.Confirmed != nil && all.Confirmed.ID == whole.ID, true)

	// Per-problem grading is for a running practice session.
	_, err = h.svc.Grade(ctx, id, session.GradeOptions{ProblemID: p1, At: at(9, 50)})
	testutil.ErrorIs(t, err, session.ErrNotRunning)
}

// TestPracticeProblemRetry: a problem grade that ended in ERROR is retried
// for that problem alone, with the same submission time; the others are not
// touched and the session never leaves RUNNING.
func TestPracticeProblemRetry(t *testing.T) {
	h := newHarness(t)
	id := h.startedPractice()
	ids := h.frozenIDs(id)
	h.answerOnly(ids[0], erroredEvaluation)

	failed, err := h.svc.Grade(ctx, id, session.GradeOptions{ProblemID: ids[0], At: at(9, 0)})
	testutil.NoError(t, err)
	testutil.Equal(t, failed.Evaluation.Complete, false)

	// A plain grade does not silently start a second submission over an
	// unresolved one.
	_, err = h.svc.Grade(ctx, id, session.GradeOptions{ProblemID: ids[0], At: at(9, 1)})
	testutil.ErrorIs(t, err, session.ErrAlreadySubmitted)
	// Another problem is independent.
	_, err = h.svc.Grade(ctx, id, session.GradeOptions{ProblemID: ids[1], At: at(9, 1)})
	testutil.NoError(t, err)

	h.answer(nil)
	retried, err := h.svc.Grade(ctx, id, session.GradeOptions{ProblemID: ids[0], Retry: true, At: at(9, 2)})
	testutil.NoError(t, err)
	testutil.Equal(t, retried.SubmissionID, failed.SubmissionID)
	testutil.Equal(t, retried.Evaluation.Complete, true)
	sub, _ := h.st.GetSubmission(ctx, retried.SubmissionID)
	testutil.Equal(t, sub.SubmittedAt, at(9, 0))
	testutil.Equal(t, h.state(id), model.StateRunning)

	// Complete now: nothing left to retry.
	_, err = h.svc.Grade(ctx, id, session.GradeOptions{ProblemID: ids[0], Retry: true, At: at(9, 3)})
	testutil.ErrorIs(t, err, session.ErrNothingToRetry)
	// Plain grading of a finished problem is a new run.
	next, err := h.svc.Grade(ctx, id, session.GradeOptions{ProblemID: ids[0], At: at(9, 4)})
	testutil.NoError(t, err)
	testutil.Equal(t, next.SubmissionID == retried.SubmissionID, false)
}

// TestReviewNotYetSupported: reviewing an abandoned session belongs to the
// reset task; until then it is a clear error, never a silent disclosure.
func TestReviewNotYetSupported(t *testing.T) {
	h := newHarness(t)
	id := h.startedMock()
	testutil.NoError(t, h.st.Transition(ctx, id, model.StateRunning, model.StateAbandoned))
	_, err := h.svc.Review(ctx, id, h.frozenIDs(id)[0], true)
	testutil.ErrorIs(t, err, session.ErrReviewUnsupported)
	_, err = h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 0)})
	testutil.ErrorIs(t, err, session.ErrNotRunning)
}

// TestMockExamResultIsTheFirstCompleteRun (spec 9.2): a mock exam's result is
// the first whole-exam submission that was graded completely. A submission
// after it (made once the results and solutions could be seen) is a review
// submission: listed apart, never the exam total, and an ERROR in it hides
// nothing that was already open.
func TestMockExamResultIsTheFirstCompleteRun(t *testing.T) {
	h := newHarness(t)
	id := h.startedMock()
	ids := h.frozenIDs(id)

	h.answer(initialEvaluation) // a partial answer
	exam, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 0)})
	testutil.NoError(t, err)
	testutil.Equal(t, exam.Evaluation.Complete, true)
	testutil.Equal(t, *exam.Evaluation.Score < exam.Evaluation.MaxScore, true)

	// The learner opens a solution, fixes everything and submits again.
	_, err = h.svc.Review(ctx, id, ids[0], true)
	testutil.NoError(t, err)
	h.answer(nil)
	fixed, err := h.svc.Grade(ctx, id, session.GradeOptions{Resubmit: true, At: at(10, 30)})
	testutil.NoError(t, err)
	testutil.Equal(t, fixed.Evaluation.Complete, true)
	testutil.Equal(t, *fixed.Evaluation.Score, fixed.Evaluation.MaxScore)

	review, err := h.svc.Review(ctx, id, "", false)
	testutil.NoError(t, err)
	testutil.Equal(t, review.Confirmed != nil && review.Confirmed.ID == exam.ID, true)
	testutil.Equal(t, *review.Confirmed.Evaluation.Score, *exam.Evaluation.Score)
	testutil.Equal(t, len(review.Submissions), 1)
	testutil.Equal(t, review.Submissions[0].ID, exam.SubmissionID)
	testutil.Equal(t, len(review.Runs), 1)
	testutil.Equal(t, len(review.ReviewSubmissions), 1)
	testutil.Equal(t, review.ReviewSubmissions[0].ID, fixed.SubmissionID)
	testutil.Equal(t, len(review.ReviewRuns), 1)
	testutil.Equal(t, review.ReviewRuns[0].ID, fixed.ID)

	// One problem: the exam's own result for it.
	one, err := h.svc.Review(ctx, id, ids[1], false)
	testutil.NoError(t, err)
	testutil.Equal(t, one.Confirmed != nil && one.Confirmed.ID == exam.ID, true)

	// A review submission that ends in ERROR re-hides nothing.
	h.answerOnly(ids[2], erroredEvaluation)
	broken, err := h.svc.Grade(ctx, id, session.GradeOptions{Resubmit: true, At: at(10, 40)})
	testutil.NoError(t, err)
	testutil.Equal(t, broken.Evaluation.Complete, false)
	testutil.Equal(t, len(broken.Evaluation.Results) > 0, true) // details stay open
	testutil.Equal(t, h.state(id), model.StateGradingError)
	review, err = h.svc.Review(ctx, id, "", false)
	testutil.NoError(t, err)
	testutil.Equal(t, review.Confirmed != nil && review.Confirmed.ID == exam.ID, true)
	testutil.Equal(t, len(review.ReviewSubmissions), 2)
	for _, r := range append(review.Runs, review.ReviewRuns...) {
		testutil.Equal(t, len(r.Evaluation.Results) > 0, true)
	}
	opened, err := h.svc.Review(ctx, id, ids[1], true)
	testutil.NoError(t, err)
	testutil.Equal(t, opened.Explanation != "", true)
}

// TestMockExamSubmissionChainBeforeCompletion: before the exam has a complete
// result, a resubmission (after an ERROR) is still the exam: the first
// complete run of the chain is the exam result.
func TestMockExamSubmissionChainBeforeCompletion(t *testing.T) {
	h := newHarness(t)
	id := h.startedMock()
	ids := h.frozenIDs(id)
	h.answerOnly(ids[0], erroredEvaluation)
	first, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 0)})
	testutil.NoError(t, err)
	testutil.Equal(t, first.Evaluation.Complete, false)
	h.answer(nil)
	second, err := h.svc.Grade(ctx, id, session.GradeOptions{Resubmit: true, At: at(10, 5)})
	testutil.NoError(t, err)
	testutil.Equal(t, second.Evaluation.Complete, true)
	review, err := h.svc.Review(ctx, id, "", false)
	testutil.NoError(t, err)
	testutil.Equal(t, review.Confirmed != nil && review.Confirmed.ID == second.ID, true)
	testutil.Equal(t, len(review.Submissions), 2)
	testutil.Equal(t, len(review.ReviewSubmissions), 0)
}

// TestMockHidesWhatIsGradedUntilDetailsOpen (spec 9.2): the fingerprint keys
// of a submission name the objects and files the criteria read. A mock exam
// shows none of them, in a review or in a refused retry, before its result is
// open.
func TestMockHidesWhatIsGradedUntilDetailsOpen(t *testing.T) {
	keyish := []string{"object:", "file:", "Deployment/", "answer.yaml"}
	h := newHarness(t)
	id := h.startedMock()
	ids := h.frozenIDs(id)
	h.answerOnly(ids[2], erroredEvaluation)
	_, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 0)})
	testutil.NoError(t, err)
	testutil.Equal(t, h.state(id), model.StateGradingError)

	review, err := h.svc.Review(ctx, id, "", false)
	testutil.NoError(t, err)
	testutil.Equal(t, len(review.Submissions), 1)
	testutil.Equal(t, len(review.Submissions[0].InputHashes), 0)
	for _, p := range ids {
		r, _ := h.svc.Review(ctx, id, p, false)
		requireNone(t, "a locked review", fmt.Sprintf("%+v", r), keyish)
	}

	// The answer changed: the refusal says so without naming what is graded.
	h.inputs.change(ids[0])
	_, err = h.svc.Grade(ctx, id, session.GradeOptions{Retry: true, At: at(10, 2)})
	testutil.ErrorIs(t, err, session.ErrAnswerChanged)
	requireNone(t, "the changed-answer refusal", err.Error(), keyish)
	if !strings.Contains(err.Error(), ids[0]) {
		t.Errorf("the refusal should name the changed problem: %v", err)
	}
	// The answer cannot be fingerprinted now: the cause (which names a
	// criterion and an object) is not shown either.
	h.inputs.setErr(fmt.Errorf("fingerprint inputs of criterion %q: object:apps/v1/Deployment/ns/web: etcd timeout", "web-replicas"))
	_, err = h.svc.Grade(ctx, id, session.GradeOptions{Retry: true, At: at(10, 3)})
	testutil.ErrorIs(t, err, session.ErrRetryNotProvable)
	requireNone(t, "the unprovable-retry refusal", err.Error(), append(keyish, "web-replicas"))

	// Once the exam has a complete result, its details (and the keys) open.
	h.inputs.setErr(nil)
	h.answer(nil)
	_, err = h.svc.Grade(ctx, id, session.GradeOptions{Resubmit: true, At: at(10, 5)})
	testutil.NoError(t, err)
	review, err = h.svc.Review(ctx, id, "", false)
	testutil.NoError(t, err)
	testutil.Equal(t, len(review.Submissions[len(review.Submissions)-1].InputHashes) > 0, true)

	// Practice shows the keys: its details are open on request.
	hp := newHarness(t)
	pid := hp.startedPractice()
	pids := hp.frozenIDs(pid)
	hp.answerOnly(pids[0], erroredEvaluation)
	_, err = hp.svc.Grade(ctx, pid, session.GradeOptions{ProblemID: pids[0], At: at(9, 0)})
	testutil.NoError(t, err)
	hp.inputs.change(pids[0])
	_, err = hp.svc.Grade(ctx, pid, session.GradeOptions{ProblemID: pids[0], Retry: true, At: at(9, 1)})
	testutil.ErrorIs(t, err, session.ErrAnswerChanged)
	if !strings.Contains(err.Error(), "object:") {
		t.Errorf("practice should name the changed input: %v", err)
	}
}
