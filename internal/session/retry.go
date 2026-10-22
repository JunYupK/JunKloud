//go:build linux

package session

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
)

// history is everything recorded about the grading of a session, in the
// order it was recorded.
type history struct {
	subs     []model.Submission
	runs     map[string][]model.GradingRun // by submission ID
	reviews  []model.ReviewEvent
	ops      []model.Operation // the session's grading operations, oldest first
	attempts map[string]int    // minimum attempt published by a completed reset
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
	resets, err := s.cfg.Store.ListOperations(ctx, ResetOperationKind)
	if err != nil {
		return history{}, err
	}
	h.attempts = map[string]int{}
	for _, op := range resets {
		if op.SessionID != sessionID || op.Phase != PhaseDone || op.Planned.ProblemID == "" || len(op.Planned.ArtifactPaths) != 1 {
			continue
		}
		name := filepath.Base(op.Planned.ArtifactPaths[0])
		n, err := strconv.Atoi(strings.TrimPrefix(name, attemptPrefix))
		if err == nil && n > h.attempts[op.Planned.ProblemID] {
			h.attempts[op.Planned.ProblemID] = n
		}
	}
	return h, nil
}

func (h history) currentAttempt(problemID string) int { return max(1, h.attempts[problemID]) }
func (h history) currentSubmission(sub model.Submission, problemID string) bool {
	attempt := 1
	for _, op := range h.ops {
		if strings.HasPrefix(op.ID, GradeOperationID(sub.ID+"-run-")) {
			attempt = max(attempt, op.Planned.Attempts[problemID])
		}
	}
	return attempt >= h.currentAttempt(problemID)
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

// split divides the history at a submission index: the submissions before it
// with their runs, and the rest with theirs. Reviews and operations stay with
// the first part.
func (h history) split(at int) (history, history) {
	a := history{subs: h.subs[:at], runs: map[string][]model.GradingRun{}, reviews: h.reviews, ops: h.ops}
	b := history{subs: h.subs[at:], runs: map[string][]model.GradingRun{}}
	for _, sub := range a.subs {
		a.runs[sub.ID] = h.runs[sub.ID]
	}
	for _, sub := range b.subs {
		b.runs[sub.ID] = h.runs[sub.ID]
	}
	return a, b
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

// nextSubmissionID is an ID no submission and no grading operation used
// before. Submission IDs are session-wide, so without the second condition a
// request that only recorded its intent (an operation named after the ID it
// meant to use) would look recorded once another problem's grade took that ID.
func (h history) nextSubmissionID(sessionID string) string {
	for n := len(h.subs) + 1; ; n++ {
		id := fmt.Sprintf("%s-sub-%d", sessionID, n)
		reserved := slices.ContainsFunc(h.ops, func(op model.Operation) bool {
			return strings.HasPrefix(op.ID, GradeOperationID(id+"-run-"))
		})
		if !h.hasSubmission(id) && !reserved {
			return id
		}
	}
}

// submission returns the submission with the ID.
func (h history) submission(id string) (model.Submission, bool) {
	for _, sub := range h.subs {
		if sub.ID == id {
			return sub, true
		}
	}
	return model.Submission{}, false
}

// retryable reports whether a retry of the submission can ever be accepted: it
// recorded fingerprints, so the answer can be shown unchanged.
func retryable(sub model.Submission) bool { return len(sub.InputHashes) > 0 }

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

// interruptedError is ErrGradeInterrupted before its message exists: the
// message names the commands the session accepts, which is worked out by
// planning, so planning cannot build it itself. Grade completes it.
type interruptedError struct{ scope string }

func (e *interruptedError) Error() string { return ErrGradeInterrupted.Error() }
func (e *interruptedError) Unwrap() error { return ErrGradeInterrupted }

// incompleteMessage is the ErrAlreadySubmitted text for an incomplete
// submission. It offers --retry only if a retry can be accepted.
func incompleteMessage(what, scope string, latest model.Submission) string {
	if !retryable(latest) {
		return fmt.Sprintf("%v: %s, and no fingerprint of the answer was recorded, so it cannot be shown unchanged; %s submits the answer again",
			ErrAlreadySubmitted, what, gradeCommand(scope, "--resubmit"))
	}
	return fmt.Sprintf("%v: %s; %s grades the same answer again, %s submits a changed one",
		ErrAlreadySubmitted, what, gradeCommand(scope, "--retry"), gradeCommand(scope, "--resubmit"))
}

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
		case latest != nil && !hist.currentSubmission(*latest, scope):
			if opts.Retry {
				return gradePlan{}, fmt.Errorf("%w: %s was reset into a new attempt; submit it with %s", ErrAnswerChanged, scope, gradeCommand(scope))
			}
			return resubmit(model.StateRunning, model.StateRunning), nil
		case opts.Retry:
			if !unresolved {
				return gradePlan{}, fmt.Errorf("%w: %s has no incomplete grade", ErrNothingToRetry, scope)
			}
			return retry(model.StateRunning, model.StateRunning), nil
		case opts.Resubmit:
			return resubmit(model.StateRunning, model.StateRunning), nil
		case unresolved && last == nil:
			// Recorded, never graded: the process died after the submission.
			return gradePlan{}, &interruptedError{scope: scope}
		case unresolved:
			return gradePlan{}, &alreadyError{msg: incompleteMessage("the last grade of "+scope+" did not complete", scope, *latest), run: last}
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
		return gradePlan{}, &interruptedError{scope: scope}
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
		return gradePlan{}, &alreadyError{msg: incompleteMessage("the submission was graded incompletely and has no total", "", *latest), run: last}
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
// submission). Nothing is recorded by a refusal. With open false (a mock exam
// whose details are not open) the messages name no fingerprint key and no
// cause: keys and capture errors name the objects, files and criteria that
// are graded (spec 9.2), so only the problems are named.
func retryProvable(sub model.Submission, now map[string]string, nowErr error, p gradePlan, open bool) error {
	resubmit := gradeCommand(p.scope, "--resubmit")
	switch {
	case len(sub.InputHashes) == 0:
		return fmt.Errorf("%w: no fingerprint of the answer was recorded with the submission, so nothing can show it is unchanged; run %s to submit again", ErrRetryNotProvable, resubmit)
	case nowErr != nil && !open:
		return fmt.Errorf("%w: the answer cannot be fingerprinted now (the cluster or the workspace did not answer); retry when it does, or run %s", ErrRetryNotProvable, resubmit)
	case nowErr != nil:
		return fmt.Errorf("%w: the answer cannot be fingerprinted now (%v); retry when the cluster answers, or run %s", ErrRetryNotProvable, nowErr, resubmit)
	}
	changed := changedInputs(sub.InputHashes, now)
	if len(changed) == 0 {
		return nil
	}
	if !open {
		return fmt.Errorf("%w: the answer of %s differs from the submission; a retry only grades an unchanged answer again, run %s to submit the changed one",
			ErrAnswerChanged, strings.Join(changedProblems(changed), ", "), resubmit)
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
	Since time.Time
	// Recorded is true when the grading got as far as its submission (or, for a
	// retry, the move to GRADING): a score for it is unknown. False means only
	// the intent was recorded: nothing was graded and nothing changed, so it
	// blocks nothing and the next grade of the scope closes it.
	Recorded bool
	Guidance string
}

// pending describes the grading operations of the session that did not finish.
func (s *Service) pending(rec model.SessionRecord, hist history) []Pending {
	var out []Pending
	for _, op := range hist.unfinished() {
		runID := strings.TrimPrefix(op.ID, "grade-")
		subID := runID
		if i := strings.LastIndex(runID, "-run-"); i >= 0 {
			subID = runID[:i]
		}
		scope := op.Planned.ProblemID
		// Recorded is judged from this operation's own evidence: it got as far as
		// its recorded phase, or the submission it names exists in the same scope
		// (a new submission its transaction stored, or the one a retry grades
		// again). IDs are reserved by nextSubmissionID, so another request never
		// takes this operation's submission ID.
		sub, found := hist.submission(subID)
		recorded := op.Phase == GradeRecorded || (found && sub.ProblemID == scope)
		p := Pending{OperationID: op.ID, SubmissionID: subID, RunID: runID, ProblemID: scope, Phase: op.Phase, Since: op.CreatedAt, Recorded: recorded}
		p.Guidance = s.guidance(rec, hist, scope, subID, recorded, op.CreatedAt)
		out = append(out, p)
	}
	return out
}

// guidance says what happened and which commands the session accepts now. The
// commands are not guessed: each of a plain grade, --retry and --resubmit is
// run through the same planning Grade uses, and only the ones it would accept
// are named. (--retry additionally needs the answer to be unchanged, which only
// the grading itself can prove.)
func (s *Service) guidance(rec model.SessionRecord, hist history, scope, submissionID string, recorded bool, since time.Time) string {
	var head string
	if recorded {
		var submitted time.Time
		for _, sub := range hist.subs {
			if sub.ID == submissionID {
				submitted = sub.SubmittedAt
			}
		}
		head = fmt.Sprintf("The grading of submission %s (submitted %s) did not finish, so it has no confirmed score.", submissionID, submitted.Format(time.RFC3339))
	} else {
		head = fmt.Sprintf("A grading requested at %s stopped before it recorded anything: nothing was graded and no score changed.", since.Format(time.RFC3339))
	}
	// The submission a retry would grade again: the one named, else the latest
	// of the scope.
	retrySub, haveSub := hist.submission(submissionID)
	if !haveSub {
		if subs := hist.inScope(scope); len(subs) > 0 {
			retrySub, haveSub = subs[len(subs)-1], true
		}
	}
	var accepted []string
	for _, c := range []struct {
		flag, does string
		opts       GradeOptions
	}{
		{"", "grades now", GradeOptions{ProblemID: scope}},
		{"--retry", "grades the same answer again and keeps the submission time, if the answer is unchanged", GradeOptions{ProblemID: scope, Retry: true}},
		{"--resubmit", "submits the answer as a new submission", GradeOptions{ProblemID: scope, Resubmit: true}},
	} {
		if _, err := s.planGrade(rec, hist, c.opts); err != nil {
			continue
		}
		if c.opts.Retry && haveSub && !retryable(retrySub) {
			continue // execution would refuse it: nothing shows the answer unchanged
		}
		cmd := gradeCommand(scope)
		if c.flag != "" {
			cmd = gradeCommand(scope, c.flag)
		}
		accepted = append(accepted, cmd+" ("+c.does+")")
	}
	if len(accepted) == 0 {
		return head + " The session accepts no grading command in its present state."
	}
	return head + " Accepted now: " + strings.Join(accepted, "; ") + "."
}

// interrupted is the refusal of a plain grade when the scope's latest
// submission was recorded and never graded.
func (s *Service) interrupted(rec model.SessionRecord, hist history, scope string) error {
	subs := hist.inScope(scope)
	id := ""
	if len(subs) > 0 {
		id = subs[len(subs)-1].ID
	}
	return fmt.Errorf("%w: %s", ErrGradeInterrupted, s.guidance(rec, hist, scope, id, true, time.Time{}))
}
