package store_test

import (
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// A submission records which problem it covers, how long the learner solved
// (the clock up to the submission, grading excluded), the overtime, whether it
// was within the limit, whether the clock ran backwards, and which solutions
// had already been opened.
func TestSubmissionRecordsScopeAndClock(t *testing.T) {
	s, _ := openStore(t)
	mustCreate(t, s, "s1", model.StateRunning)

	sub := submission("sub1", "s1")
	sub.Kind = model.SubmissionProblem
	sub.ProblemID = "q1"
	sub.SolveDuration = 125 * time.Minute
	sub.Overtime = 5 * time.Minute
	sub.WithinLimit = false
	sub.ClockAnomaly = true
	sub.ViewedSolutions = []string{"q1", "q2"}
	testutil.NoError(t, s.CreateSubmission(ctx, sub))

	got, err := s.GetSubmission(ctx, "sub1")
	testutil.NoError(t, err)
	testutil.Equal(t, got, sub)

	// The defaults are what an older row (or a plain submission) reads as.
	plain := submission("sub2", "s1")
	plain.WithinLimit = true
	testutil.NoError(t, s.CreateSubmission(ctx, plain))
	got, err = s.GetSubmission(ctx, "sub2")
	testutil.NoError(t, err)
	testutil.Equal(t, got.ProblemID, "")
	testutil.Equal(t, got.WithinLimit, true)
	testutil.Equal(t, len(got.ViewedSolutions), 0)
}

// SubmitAndTransition stores the submission and moves the session in one
// transaction: a state conflict leaves no submission behind, and a failed
// insert leaves the state alone.
func TestSubmitAndTransitionIsAtomic(t *testing.T) {
	s, path := openStore(t)
	mustCreate(t, s, "s1", model.StateRunning)
	db := rawDB(t, path)

	testutil.NoError(t, s.SubmitAndTransition(ctx, submission("sub1", "s1"), model.StateRunning, model.StateGrading))
	got, err := s.LoadSession(ctx, "s1")
	testutil.NoError(t, err)
	testutil.Equal(t, got.State, model.StateGrading)
	_, err = s.GetSubmission(ctx, "sub1")
	testutil.NoError(t, err)

	// The session is no longer RUNNING: the second submission is rejected as a
	// whole.
	err = s.SubmitAndTransition(ctx, submission("sub2", "s1"), model.StateRunning, model.StateGrading)
	testutil.ErrorIs(t, err, store.ErrStateConflict)
	_, err = s.GetSubmission(ctx, "sub2")
	testutil.ErrorIs(t, err, store.ErrNotFound)

	// A duplicate submission ID fails and the state stays where it was.
	testutil.NoError(t, s.Transition(ctx, "s1", model.StateGrading, model.StateGradingError))
	err = s.SubmitAndTransition(ctx, submission("sub1", "s1"), model.StateGradingError, model.StateGrading)
	testutil.ErrorIs(t, err, store.ErrAlreadyExists)
	got, _ = s.LoadSession(ctx, "s1")
	testutil.Equal(t, got.State, model.StateGradingError)

	// A parent that does not exist, an unknown session, an abandoned session.
	orphan := submission("sub3", "s1")
	orphan.ParentSubmissionID = "missing"
	testutil.ErrorIs(t, s.SubmitAndTransition(ctx, orphan, model.StateGradingError, model.StateGrading), store.ErrNotFound)
	testutil.ErrorIs(t, s.SubmitAndTransition(ctx, submission("sub4", "nope"), model.StateRunning, model.StateGrading), store.ErrNotFound)
	testutil.ErrorIs(t, s.SubmitAndTransition(ctx, submission("sub5", "s1"), model.StateAbandoned, model.StateGrading), store.ErrSessionAbandoned)
	testutil.Equal(t, count(t, db, `SELECT COUNT(*) FROM submissions`), 1)

	// Staying in the same state is a pure compare-and-swap: practice problems
	// are graded without ending the session.
	mustCreate(t, s, "s2", model.StateRunning)
	testutil.NoError(t, s.SubmitAndTransition(ctx, submission("sub6", "s2"), model.StateRunning, model.StateRunning))
	got, _ = s.LoadSession(ctx, "s2")
	testutil.Equal(t, got.State, model.StateRunning)
	mustCreate(t, s, "s3", model.StateCompleted)
	testutil.ErrorIs(t, s.SubmitAndTransition(ctx, submission("sub7", "s3"), model.StateRunning, model.StateRunning), store.ErrStateConflict)
}

// The history readers return everything in the order it was recorded, and
// nothing of another session.
func TestHistoryInRecordedOrder(t *testing.T) {
	s, _ := openStore(t)
	mustCreate(t, s, "s1", model.StateGrading)
	mustCreate(t, s, "s2", model.StateGrading)

	// Submitted-at times deliberately disagree with the insertion order: the
	// history is the order of the records, not of the (possibly rewound) clock.
	first := submission("zzz", "s1")
	first.SubmittedAt = t0.Add(time.Hour)
	second := submission("aaa", "s1")
	second.SubmittedAt = t0
	second.ParentSubmissionID = "zzz"
	for _, sub := range []model.Submission{first, second, submission("other", "s2")} {
		testutil.NoError(t, s.CreateSubmission(ctx, sub))
	}
	subs, err := s.ListSubmissions(ctx, "s1")
	testutil.NoError(t, err)
	testutil.Equal(t, len(subs), 2)
	testutil.Equal(t, subs[0].ID, "zzz")
	testutil.Equal(t, subs[1].ID, "aaa")

	none, err := s.ListSubmissions(ctx, "missing")
	testutil.NoError(t, err)
	testutil.Equal(t, len(none), 0)

	// Runs of one submission, results included, oldest first.
	testutil.NoError(t, s.SaveRun(ctx, run("r-b", "zzz")))
	testutil.NoError(t, s.SaveRun(ctx, run("r-a", "zzz")))
	testutil.NoError(t, s.SaveRun(ctx, run("r-c", "aaa")))
	runs, err := s.ListRuns(ctx, "zzz")
	testutil.NoError(t, err)
	testutil.Equal(t, len(runs), 2)
	testutil.Equal(t, runs[0].ID, "r-b")
	testutil.Equal(t, runs[1].ID, "r-a")
	testutil.Equal(t, len(runs[0].Evaluation.Results), 2)
	testutil.Equal(t, *runs[0].Evaluation.Score, 4)
	none2, err := s.ListRuns(ctx, "missing")
	testutil.NoError(t, err)
	testutil.Equal(t, len(none2), 0)

	// Review events (solution views), oldest first.
	testutil.NoError(t, s.RecordReview(ctx, "s1", "q2", t0.Add(2*time.Minute)))
	testutil.NoError(t, s.RecordReview(ctx, "s1", "q1", t0.Add(time.Minute)))
	testutil.NoError(t, s.RecordReview(ctx, "s2", "q1", t0))
	events, err := s.ListReviews(ctx, "s1")
	testutil.NoError(t, err)
	testutil.Equal(t, events, []model.ReviewEvent{
		{SessionID: "s1", ProblemID: "q2", ReviewedAt: t0.Add(2 * time.Minute)},
		{SessionID: "s1", ProblemID: "q1", ReviewedAt: t0.Add(time.Minute)},
	})
}
