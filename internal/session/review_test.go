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
