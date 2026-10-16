//go:build linux

package session

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
)

// history is everything recorded about the grading of a session, in the
// order it was recorded.
type history struct {
	subs    []model.Submission
	runs    map[string][]model.GradingRun // by submission ID
	reviews []model.ReviewEvent
	ops     []model.Operation // the session's grading operations, oldest first
}

func (s *Service) loadHistory(ctx context.Context, sessionID string) (history, error) {
	h := history{runs: map[string][]model.GradingRun{}}
	var err error
	if h.subs, err = s.cfg.Store.ListSubmissions(ctx, sessionID); err != nil {
		return history{}, fmt.Errorf("session: load the submissions of %q: %w", sessionID, err)
	}
	for _, sub := range h.subs {
		if h.runs[sub.ID], err = s.cfg.Store.ListRuns(ctx, sub.ID); err != nil {
			return history{}, fmt.Errorf("session: load the runs of %q: %w", sub.ID, err)
		}
	}
	if h.reviews, err = s.cfg.Store.ListReviews(ctx, sessionID); err != nil {
		return history{}, fmt.Errorf("session: load the reviews of %q: %w", sessionID, err)
	}
	ops, err := s.cfg.Store.ListOperations(ctx, GradeOperationKind)
	if err != nil {
		return history{}, fmt.Errorf("session: load the grading operations: %w", err)
	}
	for _, op := range ops {
		if op.SessionID == sessionID {
			h.ops = append(h.ops, op)
		}
	}
	return h, nil
}

// inScope returns the submissions of one scope: a problem of a practice
// session, or ("") the whole session.
func (h history) inScope(scope string) []model.Submission {
	var out []model.Submission
	for _, sub := range h.subs {
		if sub.ProblemID == scope {
			out = append(out, sub)
		}
	}
	return out
}

func (h history) hasRun(runID string) bool {
	for _, runs := range h.runs {
		for _, r := range runs {
			if r.ID == runID {
				return true
			}
		}
	}
	return false
}

func (h history) hasSubmission(id string) bool {
	return slices.ContainsFunc(h.subs, func(s model.Submission) bool { return s.ID == id })
}

// unfinished lists the grading operations that never produced their run and
// were not closed. Their phase tells how far they got.
func (h history) unfinished() []model.Operation {
	var out []model.Operation
	for _, op := range h.ops {
		if op.Phase == GradeDone || op.Phase == GradeSuperseded {
			continue
		}
		if h.hasRun(strings.TrimPrefix(op.ID, "grade-")) {
			continue // the run was committed; only the phase update was lost
		}
		out = append(out, op)
	}
	return out
}

func (h history) nextSubmissionID(sessionID string) string {
	for n := len(h.subs) + 1; ; n++ {
		if id := fmt.Sprintf("%s-sub-%d", sessionID, n); !h.hasSubmission(id) {
			return id
		}
	}
}

// nextRunID is a run ID no run and no operation used before, so an interrupted
// grading and the retry that recovers it are different operations.
func (h history) nextRunID(submissionID string) string {
	for n := 1; ; n++ {
		id := fmt.Sprintf("%s-run-%d", submissionID, n)
		if h.hasRun(id) || slices.ContainsFunc(h.ops, func(op model.Operation) bool { return op.ID == GradeOperationID(id) }) {
			continue
		}
		return id
	}
}

// gradePlan is what a Grade request comes to, decided before anything is
// recorded.
type gradePlan struct {
	scope    string // "" for the whole session, else a practice problem
	problems []model.FrozenProblem
	retry    bool // a new run of sub; no new submission
	isNew    bool // sub is a new submission
	sub      model.Submission
	runID    string

	from, to                 string // the move that starts the grading
	final, finalOK, finalErr string // the final commit: from, and to when complete / incomplete
	stale                    []model.Operation
}

// alreadyError is ErrAlreadySubmitted with the result that exists.
type alreadyError struct {
	msg string
	run *model.GradingRun
}

func (e *alreadyError) Error() string { return e.msg }
func (e *alreadyError) Unwrap() error { return ErrAlreadySubmitted }

// gradeCommand renders a command of the CLI contract (spec 10).
func gradeCommand(scope string, flags ...string) string {
	cmd := "ckad grade"
	if scope != "" {
		cmd += " --problem " + scope
	}
	for _, f := range flags {
		cmd += " " + f
	}
	return "`" + cmd + "`"
}

// planGrade decides what a request does, or why it cannot. It records nothing.
func (s *Service) planGrade(rec model.SessionRecord, hist history, opts GradeOptions) (gradePlan, error) {
	scope := opts.ProblemID
	p := gradePlan{scope: scope}
	if scope != "" {
		if rec.Mode == model.ModeMock {
			return gradePlan{}, fmt.Errorf("%w: submit the whole exam with %s", ErrProblemGradingUnavailable, gradeCommand(""))
		}
		f, ok := findProblem(rec, scope)
		if !ok {
			return gradePlan{}, fmt.Errorf("%w: %q", ErrUnknownProblem, scope)
		}
		p.problems = []model.FrozenProblem{f}
	} else {
		p.problems = rec.Problems
	}
	switch rec.State {
	case model.StatePreparing, model.StateSetupError:
		return gradePlan{}, fmt.Errorf("%w: session %q is %s", ErrNotStarted, rec.ID, rec.State)
	case model.StateAbandoned:
		return gradePlan{}, fmt.Errorf("%w: session %q was abandoned", ErrNotRunning, rec.ID)
	}
	if rec.StartedAt == nil {
		return gradePlan{}, fmt.Errorf("%w: session %q has no start time", ErrNotStarted, rec.ID)
	}

	var latest *model.Submission
	var last *model.GradingRun
	if subs := hist.inScope(scope); len(subs) > 0 {
		latest = &subs[len(subs)-1]
		if runs := hist.runs[latest.ID]; len(runs) > 0 {
			last = &runs[len(runs)-1]
		}
	}
	unresolved := latest != nil && (last == nil || !last.Evaluation.Complete)
	for _, op := range hist.unfinished() {
		if op.Planned.ProblemID == scope {
			p.stale = append(p.stale, op)
		}
	}

	resubmit := func(from, to string) gradePlan {
		p.isNew, p.from, p.to = true, from, to
		p.sub = model.Submission{ID: hist.nextSubmissionID(rec.ID), SessionID: rec.ID, Kind: model.SubmissionSession, SubmittedAt: s.submittedAt(opts)}
		if scope != "" {
			p.sub.Kind, p.sub.ProblemID = model.SubmissionProblem, scope
		}
		if latest != nil {
			p.sub.ParentSubmissionID = latest.ID
		}
		p.runID = hist.nextRunID(p.sub.ID)
		return p
	}
	retry := func(from, to string) gradePlan {
		p.retry, p.from, p.to = true, from, to
		p.sub = *latest
		p.runID = hist.nextRunID(latest.ID)
		return p
	}
	if scope != "" {
		p.final, p.finalOK, p.finalErr = model.StateRunning, model.StateRunning, model.StateRunning
	} else {
		p.final, p.finalOK, p.finalErr = model.StateGrading, model.StateCompleted, model.StateGradingError
	}

	// Per-problem grading of a practice session.
	if scope != "" {
		if rec.State != model.StateRunning {
			return gradePlan{}, fmt.Errorf("%w: a problem is graded alone while the practice session is running, this one is %s", ErrNotRunning, rec.State)
		}
		switch {
		case opts.Retry:
			if !unresolved {
				return gradePlan{}, fmt.Errorf("%w: %s has no incomplete grade", ErrNothingToRetry, scope)
			}
			return retry(model.StateRunning, model.StateRunning), nil
		case opts.Resubmit:
			return resubmit(model.StateRunning, model.StateRunning), nil
		case len(p.stale) > 0:
			return gradePlan{}, s.interrupted(hist, rec, scope)
		case unresolved:
			return gradePlan{}, &alreadyError{
				msg: fmt.Sprintf("%v: the last grade of %s did not complete; %s grades the same answer again, %s submits a changed one",
					ErrAlreadySubmitted, scope, gradeCommand(scope, "--retry"), gradeCommand(scope, "--resubmit")),
				run: last,
			}
		}
		return resubmit(model.StateRunning, model.StateRunning), nil
	}

	// The whole session.
	switch rec.State {
	case model.StateRunning:
		switch {
		case opts.Retry:
			return gradePlan{}, fmt.Errorf("%w: nothing was submitted yet; run %s", ErrNothingToRetry, gradeCommand(""))
		case opts.Resubmit:
			return gradePlan{}, fmt.Errorf("%w: run %s to submit", ErrNothingToResubmit, gradeCommand(""))
		}
		return resubmit(model.StateRunning, model.StateGrading), nil
	case model.StateGrading:
		// The lock is ours, so no grading is running: the one the session
		// still records was interrupted.
		switch {
		case latest == nil:
			return gradePlan{}, fmt.Errorf("%w: session %q is GRADING but records no submission; abandon it and start again", ErrNotRunning, rec.ID)
		case opts.Retry:
			if !unresolved {
				return gradePlan{}, fmt.Errorf("%w: the last submission has a complete result", ErrNothingToRetry)
			}
			return retry(model.StateGrading, model.StateGrading), nil
		case opts.Resubmit:
			return resubmit(model.StateGrading, model.StateGrading), nil
		}
		return gradePlan{}, s.interrupted(hist, rec, scope)
	case model.StateGradingError:
		switch {
		case latest == nil:
			return gradePlan{}, fmt.Errorf("%w: session %q is GRADING_ERROR but records no submission; abandon it and start again", ErrNotRunning, rec.ID)
		case opts.Retry:
			if !unresolved {
				return gradePlan{}, fmt.Errorf("%w: the last submission has a complete result", ErrNothingToRetry)
			}
			return retry(model.StateGradingError, model.StateGrading), nil
		case opts.Resubmit:
			return resubmit(model.StateGradingError, model.StateGrading), nil
		}
		return gradePlan{}, &alreadyError{
			msg: fmt.Sprintf("%v: the submission was graded incompletely and has no total; %s grades the same answer again, %s submits a changed one",
				ErrAlreadySubmitted, gradeCommand("", "--retry"), gradeCommand("", "--resubmit")),
			run: last,
		}
	case model.StateCompleted:
		switch {
		case opts.Retry:
			return gradePlan{}, fmt.Errorf("%w: the submission has a complete result; use %s for a new submission", ErrNothingToRetry, gradeCommand("", "--resubmit"))
		case opts.Resubmit:
			return resubmit(model.StateCompleted, model.StateGrading), nil
		}
		return gradePlan{}, &alreadyError{
			msg: fmt.Sprintf("%v: the session was submitted and graded; `ckad review` shows the result, %s makes a new submission",
				ErrAlreadySubmitted, gradeCommand("", "--resubmit")),
			run: last,
		}
	}
	return gradePlan{}, fmt.Errorf("%w: session %q is %s", ErrNotRunning, rec.ID, rec.State)
}

// submittedAt is the time of a new submission; Grade already normalised it
// into opts.At.
func (s *Service) submittedAt(opts GradeOptions) time.Time {
	if opts.At.IsZero() {
		return s.cfg.Now().Round(0).UTC()
	}
	return opts.At.Round(0).UTC()
}

func findProblem(rec model.SessionRecord, id string) (model.FrozenProblem, bool) {
	for _, f := range rec.Problems {
		if f.Definition.ID == id {
			return f, true
		}
	}
	return model.FrozenProblem{}, false
}

// retryProvable decides whether a retry may go on: only if the submission
// recorded fingerprints and the answer still has exactly those (spec 9.1:
// retry is a technical retry of an unchanged answer; anything else is a new
// submission). Nothing is recorded by a refusal.
func retryProvable(sub model.Submission, now map[string]string, nowErr error, p gradePlan) error {
	resubmit := gradeCommand(p.scope, "--resubmit")
	switch {
	case len(sub.InputHashes) == 0:
		return fmt.Errorf("%w: no fingerprint of the answer was recorded with the submission, so nothing can show it is unchanged; run %s to submit again", ErrRetryNotProvable, resubmit)
	case nowErr != nil:
		return fmt.Errorf("%w: the answer cannot be fingerprinted now (%v); retry when the cluster answers, or run %s", ErrRetryNotProvable, nowErr, resubmit)
	}
	changed := changedInputs(sub.InputHashes, now)
	if len(changed) == 0 {
		return nil
	}
	shown := changed
	if len(shown) > 5 {
		shown = append(slices.Clone(changed[:5]), fmt.Sprintf("and %d more", len(changed)-5))
	}
	return fmt.Errorf("%w: %s differ from the submission; a retry only grades an unchanged answer again, run %s to submit the changed one",
		ErrAnswerChanged, strings.Join(shown, ", "), resubmit)
}

// Pending is a grading that was recorded and never finished (the process was
// killed, the machine slept for good). There is no confirmed score for what it
// was grading, and Guidance says what to do. A grading running in another
// terminal right now is listed the same way until it commits; recovering needs
// the environment lock, which that grading holds.
type Pending struct {
	OperationID  string
	SubmissionID string
	RunID        string
	// ProblemID is the problem of a practice grade, "" for the whole session.
	ProblemID string
	// Phase is how far the grading got: GradePlanned or GradeRecorded.
	Phase string
	// Since is the time of the request.
	Since    time.Time
	Guidance string
}

// pending describes the grading operations of the session that did not finish.
func (s *Service) pending(hist history) []Pending {
	var out []Pending
	for _, op := range hist.unfinished() {
		runID := strings.TrimPrefix(op.ID, "grade-")
		subID := runID
		if i := strings.LastIndex(runID, "-run-"); i >= 0 {
			subID = runID[:i]
		}
		scope := op.Planned.ProblemID
		p := Pending{OperationID: op.ID, SubmissionID: subID, RunID: runID, ProblemID: scope, Phase: op.Phase, Since: op.CreatedAt}
		if !hist.hasSubmission(subID) {
			p.Guidance = fmt.Sprintf("A grading requested at %s stopped before anything was recorded. Nothing was graded and there is no score; run %s again.",
				op.CreatedAt.Format(time.RFC3339), gradeCommand(scope))
		} else {
			p.Guidance = guidance(hist, subID, scope)
		}
		out = append(out, p)
	}
	return out
}

func guidance(hist history, submissionID, scope string) string {
	var submitted time.Time
	for _, sub := range hist.subs {
		if sub.ID == submissionID {
			submitted = sub.SubmittedAt
		}
	}
	return fmt.Sprintf("The grading of submission %s (submitted %s) did not finish, so it has no confirmed score. If no other terminal is grading, run %s to grade the same answer again (its submission time stays), or %s if you changed your answer.",
		submissionID, submitted.Format(time.RFC3339), gradeCommand(scope, "--retry"), gradeCommand(scope, "--resubmit"))
}

// interrupted is the refusal of a plain grade when the scope has an unfinished
// grading.
func (s *Service) interrupted(hist history, rec model.SessionRecord, scope string) error {
	subs := hist.inScope(scope)
	id := ""
	if len(subs) > 0 {
		id = subs[len(subs)-1].ID
	}
	return fmt.Errorf("%w: %s", ErrGradeInterrupted, guidance(hist, id, scope))
}
