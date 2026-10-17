//go:build linux

package session

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
)

// solutionDir holds a package's reference solution files.
const solutionDir = "solution/"

// Review is what the learner may see of a session's results (spec 9.2, 9.3).
//
// Runs and Submissions are every run and submission of the asked scope, in the
// order they were recorded, and never summed: a practice session's best scores
// of different problems or times are not one total. A run of a whole-session
// submission is narrowed to the asked problem when one is asked for.
// Explanation and ReferenceSolution come from the session's frozen private
// package and only when the solution was asked for and may be opened.
// SolutionViewed says whether the solution of the asked problem (of any
// problem when none is asked for) has been opened, including by this call; a
// later submission also records the problems that had been seen
// (Submission.ViewedSolutions).
//
// Confirmed is the one result a total may be shown for. For a mock exam it is
// the exam result: the first complete run of a whole-exam submission (narrowed
// to the asked problem), whatever happened after it; submissions made after
// it are review submissions (ReviewSubmissions, ReviewRuns) and never replace
// it (spec 9.2). For practice it is the newest run of the scope, only if it is
// complete, no grading of the scope is unfinished, and, for the whole session,
// the session is COMPLETED. It is nil while anything is incomplete or pending,
// whatever Runs holds.
//
// While a mock exam's details are not open, Submissions carry no
// InputHashes: their keys name the objects and files the criteria read.
type Review struct {
	// Feedback is Korean criterion feedback, subject to the same disclosure
	// gate as Results. Keys are qualified problem/criterion IDs.
	Feedback                       map[string]string
	State                          string
	Runs                           []model.GradingRun
	Submissions                    []model.Submission
	Explanation, ReferenceSolution string
	SolutionViewed                 bool
	Confirmed                      *model.GradingRun
	// Pending is every unfinished grading of the session, with how to
	// recover.
	Pending []Pending
	// ReviewSubmissions and ReviewRuns are, for a mock exam, the
	// submissions made after the exam already had its complete result, and
	// their runs: review attempts made with the results (and possibly the
	// solutions) in view. They are shown apart and never become the exam
	// total. Empty for practice sessions.
	ReviewSubmissions []model.Submission
	ReviewRuns        []model.GradingRun
}

// detailsOpen reports whether criterion details and the answer may be shown
// (spec 9.2). A mock exam shows neither until the whole exam is submitted and
// graded without an ERROR, i.e. it has an exam result (examResult); from then
// on they stay open, whatever a later review submission does to the session's
// state. Practice shows them whenever they are asked for.
func detailsOpen(rec model.SessionRecord, hist history) bool {
	if rec.Mode == model.ModeMock {
		_, run := examResult(hist)
		return run != nil
	}
	return true
}

// examResult is a mock exam's result: the first complete run of a
// whole-exam submission, in recording order, and the index of its submission
// in hist.subs (-1 and nil while there is none). A submission after that
// index is a review submission.
func examResult(hist history) (int, *model.GradingRun) {
	for i, sub := range hist.subs {
		if sub.ProblemID != "" {
			continue
		}
		for _, r := range hist.runs[sub.ID] {
			if r.Evaluation.Complete {
				r := r
				return i, &r
			}
		}
	}
	return -1, nil
}

// withheld is a run without the parts that reveal what was checked: results,
// score and maximum. That it is complete or not stays visible.
func withheld(run model.GradingRun) model.GradingRun {
	run.Evaluation.Results = nil
	run.Evaluation.Score = nil
	run.Evaluation.MaxScore = 0
	return run
}

// Review shows the results of a session, or of one problem of it.
//
// It reads the store only: it needs no environment lock and works while a
// grading runs in another terminal. problemID narrows the review to one
// problem ("" is the whole session). With solution set it also opens the
// explanation and the reference solution of that problem and records the view
// (store.RecordReview), as the spec asks the learner to open them
// explicitly (spec 9.3):
//
//   - practice: any time after the session started;
//   - mock exam: only once the whole exam is submitted and graded without an
//     ERROR (state COMPLETED). Before that, and while an ERROR is left, nothing
//     that reveals a criterion or the answer is returned, and a request for the
//     solution gets ErrDisclosureLocked together with the withheld review.
//
// The content is read from the session's frozen package, never the bank.
// Reviewing an abandoned session is ErrReviewUnsupported until the reset task
// defines it.
func (s *Service) Review(ctx context.Context, sessionID, problemID string, solution bool) (Review, error) {
	rec, err := s.cfg.Store.LoadSession(ctx, sessionID)
	if err != nil {
		return Review{}, fmt.Errorf("session: review %q: %w", sessionID, err)
	}
	switch rec.State {
	case model.StatePreparing, model.StateSetupError:
		return Review{}, fmt.Errorf("%w: session %q is %s", ErrNotStarted, sessionID, rec.State)
	case model.StateAbandoned:
		return Review{}, fmt.Errorf("%w: session %q was abandoned; reviewing an abandoned session comes with the reset support", ErrReviewUnsupported, sessionID)
	}
	var frozen model.FrozenProblem
	if problemID != "" {
		var ok bool
		if frozen, ok = findProblem(rec, problemID); !ok {
			return Review{}, fmt.Errorf("%w: %q", ErrUnknownProblem, problemID)
		}
	} else if solution {
		return Review{}, fmt.Errorf("%w: the solution is opened for one problem at a time", ErrInvalidOptions)
	}
	hist, err := s.loadHistory(ctx, sessionID)
	if err != nil {
		return Review{}, err
	}

	open := detailsOpen(rec, hist)
	rv := Review{State: rec.State, Pending: s.pending(rec, hist)}
	if open {
		rv.Feedback = map[string]string{}
		for _, p := range rec.Problems {
			if problemID != "" && p.Definition.ID != problemID {
				continue
			}
			for _, c := range p.Definition.Criteria {
				rv.Feedback[QualifiedID(p.Definition.ID, c.ID)] = c.Feedback
			}
		}
	}
	examHist, reviewHist := hist, history{}
	if rec.Mode == model.ModeMock {
		if i, run := examResult(hist); run != nil {
			examHist, reviewHist = hist.split(i + 1)
		}
	}
	rv.Submissions, rv.Runs = scopeOf(rec, examHist, problemID)
	rv.ReviewSubmissions, rv.ReviewRuns = scopeOf(rec, reviewHist, problemID)
	if !open {
		for i, r := range rv.Runs {
			rv.Runs[i] = withheld(r)
		}
		for i := range rv.Submissions {
			rv.Submissions[i].InputHashes = nil
		}
	}
	rv.Confirmed = confirmed(rec, hist, rv.Pending, problemID)
	if !open && rv.Confirmed != nil {
		rv.Confirmed = nil
	}
	for _, ev := range hist.reviews {
		if problemID == "" || ev.ProblemID == problemID {
			rv.SolutionViewed = true
		}
	}
	if !solution {
		return rv, nil
	}

	if !open {
		return rv, fmt.Errorf("%w: %s", ErrDisclosureLocked, lockedReason(rec, hist))
	}
	pkg, err := LoadPackage(s.cfg.StateDir, frozen.Hash)
	if err != nil {
		return rv, fmt.Errorf("session: review %q: the stored package of %s: %w", sessionID, problemID, err)
	}
	// The view is recorded before it is shown; nothing is shown if the
	// record cannot be made.
	if err := s.cfg.Store.RecordReview(ctx, sessionID, problemID, s.cfg.Now().Round(0).UTC()); err != nil {
		return rv, fmt.Errorf("session: review %q: record the view of %s: %w", sessionID, problemID, err)
	}
	rv.Explanation = strings.TrimRight(string(pkg.Files[problem.ExplanationFile]), "\n") + "\n"
	rv.ReferenceSolution = referenceSolution(pkg)
	rv.SolutionViewed = true
	return rv, nil
}

// lockedReason says why a mock exam does not open its answers yet.
func lockedReason(rec model.SessionRecord, hist history) string {
	switch rec.State {
	case model.StateGradingError:
		// --retry is named only if it can be accepted (the same test as the
		// guidance uses): a submission without fingerprints cannot be retried.
		if subs := hist.inScope(""); len(subs) == 0 || retryable(subs[len(subs)-1]) {
			return "the submission still has an ERROR, so it is incomplete; grade it again with `ckad grade --retry` (or `--resubmit` after changing the answer)"
		}
		return "the submission still has an ERROR, so it is incomplete, and no fingerprint of the answer was recorded so it cannot be retried; submit the answer again with `ckad grade --resubmit`"
	case model.StateGrading:
		return "the grading has not finished; if it was interrupted, `ckad status` says how to recover"
	}
	return "a mock exam shows its explanations and solutions once the whole exam is submitted and graded without errors"
}

// referenceSolution joins the package's solution files, each under its path.
func referenceSolution(pkg model.Problem) string {
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(pkg.Files)) {
		if !strings.HasPrefix(name, solutionDir) {
			continue
		}
		fmt.Fprintf(&b, "# %s\n%s", name, strings.TrimRight(string(pkg.Files[name]), "\n")+"\n\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// scopeOf collects the submissions and runs of a problem (or of everything),
// in recording order. A whole-session run is narrowed to the problem asked
// for: only that problem's results, and, if the run is complete, the problem's
// own score. An incomplete run keeps no score: the submission has no total.
func scopeOf(rec model.SessionRecord, hist history, problemID string) ([]model.Submission, []model.GradingRun) {
	var subs []model.Submission
	var runs []model.GradingRun
	for _, sub := range hist.subs {
		switch {
		case problemID == "" || sub.ProblemID == problemID:
			subs = append(subs, sub)
			runs = append(runs, hist.runs[sub.ID]...)
		case sub.ProblemID == "":
			subs = append(subs, sub)
			for _, r := range hist.runs[sub.ID] {
				runs = append(runs, narrow(rec, r, problemID))
			}
		}
	}
	return subs, runs
}

// narrow keeps the results of one problem of a whole-session run, and the
// synthetic results that concern the submission as a whole.
func narrow(rec model.SessionRecord, run model.GradingRun, problemID string) model.GradingRun {
	ev := run.Evaluation
	var results []model.CheckResult
	score := 0
	for _, r := range ev.Results {
		pid, _, qualified := SplitResultID(r.CriterionID)
		if qualified && pid != problemID {
			continue
		}
		results = append(results, r)
		score += r.Points
	}
	ev.Results = results
	ev.MaxScore = 0
	if f, ok := findProblem(rec, problemID); ok {
		ev.MaxScore = maxPoints(f)
	}
	ev.Score = nil
	if ev.Complete {
		ev.Score = &score
	}
	run.Evaluation = ev
	return run
}

// confirmed is the result a total may be shown for (see Review): a mock
// exam's exam result, else the newest run of the scope.
func confirmed(rec model.SessionRecord, hist history, pending []Pending, problemID string) *model.GradingRun {
	if rec.Mode == model.ModeMock {
		_, run := examResult(hist)
		if run != nil && problemID != "" {
			narrowed := narrow(rec, *run, problemID)
			run = &narrowed
		}
		return run
	}
	for _, p := range pending {
		// A request that recorded nothing changes no result and hides none.
		if p.Recorded && (p.ProblemID == "" || p.ProblemID == problemID) {
			return nil
		}
	}
	var newest *model.GradingRun
	for _, sub := range hist.subs {
		if problemID == "" && sub.ProblemID != "" {
			continue // a problem's grade is not a session total
		}
		if problemID != "" && sub.ProblemID != "" && sub.ProblemID != problemID {
			continue
		}
		for _, r := range hist.runs[sub.ID] {
			r := r
			if problemID != "" && sub.ProblemID == "" {
				r = narrow(rec, r, problemID)
			}
			newest = &r
		}
	}
	if newest == nil || !newest.Evaluation.Complete {
		return nil
	}
	if (problemID == "" || rec.Mode == model.ModeMock) && rec.State != model.StateCompleted {
		return nil
	}
	return newest
}
