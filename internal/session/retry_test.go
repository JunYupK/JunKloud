//go:build linux

package session_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// failFirst makes the first grading of the session end in a platform ERROR
// (original submission at 10:00) and returns the incomplete run.
func (h *harness) failFirst(id string) model.GradingRun {
	h.t.Helper()
	ids := h.frozenIDs(id)
	h.answerOnly(ids[1], erroredEvaluation)
	run, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 0)})
	testutil.NoError(h.t, err)
	testutil.Equal(h.t, run.Evaluation.Complete, false)
	testutil.Equal(h.t, h.state(id), model.StateGradingError)
	return run
}

// TestRetryPreservesSubmissionTime (spec 9.1): a retry grades the same
// submission again as a NEW run; the submission keeps its 10:00 time and the
// retry's own observation time (10:02) is on the run.
func TestRetryPreservesSubmissionTime(t *testing.T) {
	h := newHarness(t)
	id := h.startedMock()
	failed := h.failFirst(id)
	originalSubmission, err := h.st.GetSubmission(ctx, failed.SubmissionID)
	testutil.NoError(t, err)
	testutil.Equal(t, originalSubmission.SubmittedAt, at(10, 0))

	h.answer(nil) // the platform failure is gone; the answer is untouched
	retried, err := h.svc.Grade(ctx, id, session.GradeOptions{Retry: true, At: at(10, 2)})
	testutil.NoError(t, err)

	retrySubmission, err := h.st.GetSubmission(ctx, retried.SubmissionID)
	testutil.NoError(t, err)
	testutil.Equal(t, retrySubmission.SubmittedAt, originalSubmission.SubmittedAt)
	testutil.Equal(t, retried.SubmissionID, failed.SubmissionID)
	testutil.Equal(t, retried.StartedAt, at(10, 2))
	testutil.Equal(t, failed.StartedAt, at(10, 0))
	testutil.Equal(t, retried.ID == failed.ID, false)
	testutil.Equal(t, retried.Evaluation.Complete, true)
	testutil.Equal(t, h.state(id), model.StateCompleted)

	// The submission's solving time is the original one, not 10:02's.
	testutil.Equal(t, retrySubmission.SolveDuration, 90*time.Minute)
	testutil.Equal(t, len(h.submissions(id)), 1)
	runs := h.runsOf(failed.SubmissionID)
	testutil.Equal(t, len(runs), 2)
	// The failed run is still there, still incomplete, untouched.
	testutil.Equal(t, runs[0].ID, failed.ID)
	testutil.Equal(t, runs[0].Evaluation.Complete, false)
	testutil.Equal(t, runs[0].Evaluation.Score, (*int)(nil))
	testutil.Equal(t, runs[1].ID, retried.ID)

	// Retrying a finished submission is not a thing.
	_, err = h.svc.Grade(ctx, id, session.GradeOptions{Retry: true, At: at(10, 3)})
	testutil.ErrorIs(t, err, session.ErrNothingToRetry)
}

// TestRetryAgainAfterAnotherError: every retry is a new run of the same
// submission, however many fail.
func TestRetryAgainAfterAnotherError(t *testing.T) {
	h := newHarness(t)
	id := h.startedMock()
	failed := h.failFirst(id)

	again, err := h.svc.Grade(ctx, id, session.GradeOptions{Retry: true, At: at(10, 2)})
	testutil.NoError(t, err)
	testutil.Equal(t, again.Evaluation.Complete, false)
	testutil.Equal(t, again.SubmissionID, failed.SubmissionID)
	testutil.Equal(t, h.state(id), model.StateGradingError)

	h.answer(nil)
	last, err := h.svc.Grade(ctx, id, session.GradeOptions{Retry: true, At: at(10, 4)})
	testutil.NoError(t, err)
	testutil.Equal(t, last.Evaluation.Complete, true)
	ids := map[string]bool{}
	for _, r := range h.runsOf(failed.SubmissionID) {
		ids[r.ID] = true
	}
	testutil.Equal(t, len(ids), 3)
	sub, _ := h.st.GetSubmission(ctx, failed.SubmissionID)
	testutil.Equal(t, sub.SubmittedAt, at(10, 0))
}

// TestChangedAnswerRequiresResubmit (spec 9.1): when the answer is not what was
// submitted any more, or it cannot be proven that it is, a retry is refused
// and the learner is sent to --resubmit. Nothing is recorded by the refusal.
func TestChangedAnswerRequiresResubmit(t *testing.T) {
	assertRefused := func(t *testing.T, h *harness, id string, want error, runsBefore int) {
		t.Helper()
		h.answer(nil)
		_, err := h.svc.Grade(ctx, id, session.GradeOptions{Retry: true, At: at(10, 2)})
		testutil.ErrorIs(t, err, want)
		if !strings.Contains(err.Error(), "--resubmit") {
			t.Errorf("the refusal does not point to --resubmit: %v", err)
		}
		// No new run, no new submission, state and pending operations as before.
		testutil.Equal(t, len(h.submissions(id)), 1)
		testutil.Equal(t, len(h.runsOf(h.submissions(id)[0].ID)), runsBefore)
		testutil.Equal(t, h.state(id), model.StateGradingError)
		status, err := h.svc.Status(ctx, id, at(10, 3))
		testutil.NoError(t, err)
		testutil.Equal(t, len(status.Pending), 0)
	}

	t.Run("an object changed", func(t *testing.T) {
		h := newHarness(t)
		id := h.startedMock()
		failed := h.failFirst(id)
		h.inputs.change(h.frozenIDs(id)[3])
		assertRefused(t, h, id, session.ErrAnswerChanged, 1)

		// The refusal names the problem whose answer changed; a mock exam
		// whose details are not open names no fingerprint key (spec 9.2).
		_, err := h.svc.Grade(ctx, id, session.GradeOptions{Retry: true, At: at(10, 2)})
		if !strings.Contains(err.Error(), h.frozenIDs(id)[3]) || strings.Contains(err.Error(), "Deployment") {
			t.Errorf("the refusal should name the problem and no key: %v", err)
		}
		_ = failed
	})

	t.Run("a file changed", func(t *testing.T) {
		h := newHarness(t)
		id := h.startedMock()
		h.failFirst(id)
		dir, _, err := session.LatestAttempt(h.workspace, id, h.frozenIDs(id)[0])
		testutil.NoError(t, err)
		testutil.NoError(t, os.WriteFile(filepath.Join(dir, "answer.yaml"), []byte("kind: Pod\n"), 0o644))
		assertRefused(t, h, id, session.ErrAnswerChanged, 1)
	})

	t.Run("unchanged answers may retry", func(t *testing.T) {
		h := newHarness(t)
		id := h.startedMock()
		h.failFirst(id)
		// Churn the fingerprint does not see: nothing changes in the fake.
		h.answer(nil)
		_, err := h.svc.Grade(ctx, id, session.GradeOptions{Retry: true, At: at(10, 2)})
		testutil.NoError(t, err)
	})

	t.Run("no fingerprint was recorded", func(t *testing.T) {
		h := newHarness(t)
		id := h.startedMock()
		ids := h.frozenIDs(id)
		// The first submission could not be fingerprinted (the API server was
		// unreachable at that moment), so the grade is incomplete as well.
		h.answerOnly(ids[0], erroredEvaluation)
		h.inputs.setErr(os.ErrDeadlineExceeded)
		run, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 0)})
		testutil.NoError(t, err)
		testutil.Equal(t, run.Evaluation.Complete, false)
		h.inputs.setErr(nil)
		assertRefused(t, h, id, session.ErrRetryNotProvable, 1)
	})

	t.Run("the fingerprint cannot be taken now", func(t *testing.T) {
		h := newHarness(t)
		id := h.startedMock()
		h.failFirst(id)
		h.answer(nil)
		h.inputs.setErr(os.ErrDeadlineExceeded)
		_, err := h.svc.Grade(ctx, id, session.GradeOptions{Retry: true, At: at(10, 2)})
		testutil.ErrorIs(t, err, session.ErrRetryNotProvable)
		testutil.Equal(t, len(h.runsOf(h.submissions(id)[0].ID)), 1)
	})

	t.Run("resubmitting is the way out", func(t *testing.T) {
		h := newHarness(t)
		id := h.startedMock()
		h.failFirst(id)
		h.inputs.change(h.frozenIDs(id)[3])
		h.answer(nil)
		run, err := h.svc.Grade(ctx, id, session.GradeOptions{Resubmit: true, At: at(10, 5)})
		testutil.NoError(t, err)
		testutil.Equal(t, run.Evaluation.Complete, true)
		testutil.Equal(t, h.state(id), model.StateCompleted)
	})
}

// TestResubmitAppendOnly: a resubmission is a NEW submission with its own
// time (10:05, parented to the 10:00 one); earlier submissions, failed runs
// and complete runs are never touched, whatever state the session was in.
func TestResubmitAppendOnly(t *testing.T) {
	h := newHarness(t)
	id := h.startedMock()
	failed := h.failFirst(id)
	originalSubmission, err := h.st.GetSubmission(ctx, failed.SubmissionID)
	testutil.NoError(t, err)

	storedFailed := h.runsOf(failed.SubmissionID)

	// Resubmitting from GRADING_ERROR.
	h.inputs.change(h.frozenIDs(id)[2]) // the learner changed the answer
	h.answer(nil)
	second, err := h.svc.Grade(ctx, id, session.GradeOptions{Resubmit: true, At: at(10, 5)})
	testutil.NoError(t, err)
	testutil.Equal(t, second.Evaluation.Complete, true)
	testutil.Equal(t, h.state(id), model.StateCompleted)
	resubmission, err := h.st.GetSubmission(ctx, second.SubmissionID)
	testutil.NoError(t, err)
	testutil.Equal(t, resubmission.SubmittedAt, originalSubmission.SubmittedAt.Add(5*time.Minute))
	testutil.Equal(t, resubmission.ID == originalSubmission.ID, false)
	testutil.Equal(t, resubmission.ParentSubmissionID, originalSubmission.ID)
	testutil.Equal(t, resubmission.SolveDuration, 95*time.Minute)
	testutil.Equal(t, resubmission.Kind, model.SubmissionSession)

	// The earlier records are exactly as they were.
	again, err := h.st.GetSubmission(ctx, failed.SubmissionID)
	testutil.NoError(t, err)
	testutil.Equal(t, again, originalSubmission)
	testutil.Equal(t, h.runsOf(failed.SubmissionID), storedFailed)
	storedSecond := h.runsOf(second.SubmissionID)
	testutil.Equal(t, len(storedSecond), 1)
	testutil.Equal(t, storedSecond[0].ID, second.ID)

	// Resubmitting after COMPLETED (mock: a new submission, not an overwrite).
	third, err := h.svc.Grade(ctx, id, session.GradeOptions{Resubmit: true, At: at(10, 20)})
	testutil.NoError(t, err)
	testutil.Equal(t, third.Evaluation.Complete, true)
	subs := h.submissions(id)
	testutil.Equal(t, len(subs), 3)
	testutil.Equal(t, subs[2].ParentSubmissionID, subs[1].ID)
	testutil.Equal(t, h.runsOf(second.SubmissionID), storedSecond)
	testutil.Equal(t, h.state(id), model.StateCompleted)

	// A resubmission that fails: back to GRADING_ERROR, nothing earlier lost.
	// The exam's result stays the first complete run (the second
	// submission); the later ones are review submissions (spec 9.2).
	ids := h.frozenIDs(id)
	h.answerOnly(ids[0], erroredEvaluation)
	fourth, err := h.svc.Grade(ctx, id, session.GradeOptions{Resubmit: true, At: at(10, 25)})
	testutil.NoError(t, err)
	testutil.Equal(t, fourth.Evaluation.Complete, false)
	testutil.Equal(t, h.state(id), model.StateGradingError)
	review, err := h.svc.Review(ctx, id, "", false)
	testutil.NoError(t, err)
	testutil.Equal(t, review.Confirmed != nil && review.Confirmed.ID == second.ID, true)
	testutil.Equal(t, len(review.Runs), 2)
	testutil.Equal(t, len(review.Submissions), 2)
	testutil.Equal(t, len(review.ReviewRuns), 2)
	testutil.Equal(t, len(review.ReviewSubmissions), 2)
	testutil.Equal(t, review.ReviewSubmissions[1].ID, fourth.SubmissionID)
}
