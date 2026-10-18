//go:build linux

package session

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/lock"
	"github.com/JunYupK/JunKloud/internal/model"
)

// Errors of grading and review. Match them with errors.Is. Most carry the
// next command in their message; the CLI maps the sentinels to exit codes and
// prints the message.
var (
	// ErrConflictingOptions: --retry and --resubmit were both asked for.
	ErrConflictingOptions = errors.New("session: --retry and --resubmit exclude each other")
	// ErrNotStarted: the session's clock has not started (PREPARING,
	// SETUP_ERROR): there is nothing to submit or review yet.
	ErrNotStarted = errors.New("session: the session has not been started")
	// ErrNotRunning: the session is in a state that does not accept the
	// request (abandoned, or a per-problem grade outside a running practice
	// session).
	ErrNotRunning = errors.New("session: the session is not in a state that accepts this request")
	// ErrProblemGradingUnavailable: a mock exam is submitted as a whole; a
	// single problem cannot be graded (spec 9.2).
	ErrProblemGradingUnavailable = errors.New("session: a mock exam is graded as a whole")
	// ErrUnknownProblem: the session has no problem with that ID.
	ErrUnknownProblem = errors.New("session: the session has no such problem")
	// ErrAlreadySubmitted: a plain grade of something that was submitted
	// already. Grade also returns the existing result; the message names the
	// next command (--retry, --resubmit, review).
	ErrAlreadySubmitted = errors.New("session: already submitted")
	// ErrNothingToRetry: --retry, but no submission ended incomplete.
	ErrNothingToRetry = errors.New("session: there is no incomplete submission to retry")
	// ErrNothingToResubmit: --resubmit, but nothing was submitted before.
	ErrNothingToResubmit = errors.New("session: there is no earlier submission to resubmit")
	// ErrAnswerChanged: --retry, but an input differs from what the
	// submission recorded. Resubmit instead.
	ErrAnswerChanged = errors.New("session: the answer changed since it was submitted")
	// ErrRetryNotProvable: --retry, but it cannot be proven that nothing
	// changed (no fingerprint was recorded, or none can be taken now).
	// Resubmit instead.
	ErrRetryNotProvable = errors.New("session: it cannot be proven that the answer is unchanged")
	// ErrGradeInterrupted: an earlier grading of the same scope never
	// finished (a crash). The message says how to recover (--retry or
	// --resubmit); there is no confirmed score for it.
	ErrGradeInterrupted = errors.New("session: an earlier grading did not finish")
	// ErrDisclosureLocked: the explanation or solution is not available
	// yet (a mock exam before a complete submission).
	ErrDisclosureLocked = errors.New("session: the answer is not available yet")
	// ErrReviewUnsupported: reviewing an abandoned session belongs to the
	// reset task.
	ErrReviewUnsupported = errors.New("session: reviewing this session is not supported yet")
)

// Operation kind and phases of a grading, recorded in the store so an
// interrupted grading is detected (spec 11). The ID is "grade-" + the ID of
// the run the operation produces. A grading is finished when its phase is
// GradeDone or GradeSuperseded, or when its run exists.
const (
	GradeOperationKind = "session-grade"

	GradePlanned    = "planned"    // the request was recorded; nothing graded or changed yet
	GradeRecorded   = "recorded"   // the submission is stored and the session is GRADING (or the problem's turn began)
	GradeDone       = "done"       // the run and the new session state were committed together
	GradeSuperseded = "superseded" // an interrupted grading that a retry, a resubmission or a new request replaced
)

// GradeOperationID is the ID of the operation that produces the run.
func GradeOperationID(runID string) string { return "grade-" + runID }

// Results of a run are named "<problem ID>/<criterion ID>" so one run can cover
// a whole session; synthetic results the session adds when a grade cannot be
// confirmed start with "!". Problem IDs contain no "/" and no "!".
const (
	resultInputsChanged    = "!inputs-changed"
	resultInputsUnverified = "!inputs-unverified"
	resultEvaluatePrefix   = "!evaluate-"

	resultEnvironmentUnavailable = "!environment-unavailable"
	resultEnvironmentStale       = "!environment-stale"
	resultGraderChanged          = "!grader-changed"
	resultWorkspaceMissing       = "!workspace-missing-" // + problem
)

// QualifiedID names a criterion of a problem in the results of a run.
func QualifiedID(problemID, criterionID string) string { return problemID + "/" + criterionID }

// SplitResultID splits a result's ID into problem and criterion. ok is false
// for the synthetic results ("!...") that belong to the submission as a
// whole.
func SplitResultID(id string) (problemID, criterionID string, ok bool) {
	return strings.Cut(id, "/")
}

// GradeOptions chooses what Grade does.
//
// The zero value is the first, whole-session submission. ProblemID grades one
// problem of a practice session. Retry grades the last submission again
// after a technical failure (a new run, the original submission time);
// Resubmit makes a new submission (new submission time, the earlier records
// stay). At is the time of the request (zero: now), which is the submission
// time of a new submission.
type GradeOptions struct {
	ProblemID       string
	Retry, Resubmit bool
	At              time.Time
}

// savingBudget bounds the bookkeeping that runs after a grading was
// cancelled, so the incomplete run is still recorded.
const savingBudget = time.Minute

// Grade submits and grades (spec 9.1, 9.2, 11). Under the environment lock
// (the same lock start, reset and validation take) it:
//
//  1. refuses what cannot be done before recording anything, from the
//     session's own records only: a session that was not started, a mock
//     problem graded alone, a plain grade of something already submitted, a
//     retry of a finished submission, a retry of an answer that changed or
//     whose sameness cannot be proven;
//  2. fingerprints the answer (grader.CaptureInputs) in each problem's
//     attempt directory, then records the intent (an operation) and, in ONE
//     transaction, the submission (time, solving time, overtime, fingerprints,
//     solutions already seen) together with the move to GRADING. A retry adds
//     no submission: it records its intent and goes on with the old one;
//  3. checks that the grading can be trusted and evaluates every problem of
//     the scope with the runtime grader against the session's FROZEN problems,
//     never the bank. An unreachable environment, a grader or tool versions
//     other than those the session was prepared with, or a problem whose
//     workspace cannot be determined are not refusals (the submission and its
//     time are already recorded): each is an ERROR result of the run
//     (`!environment-unavailable`, `!grader-changed`, `!environment-stale`,
//     `!workspace-missing-<problem>`), so nothing is confirmed and a later
//     --retry keeps the submission time. A learner who deleted a problem's
//     attempt directory while the session's workspace exists is graded as an
//     empty workspace (files absent);
//  4. fingerprints the answer again. If it changed while it was graded, or the
//     fingerprints could not be taken, the result is not confirmed: an ERROR
//     result says why, and the run is incomplete;
//  5. commits the run and the new state in one transaction
//     (SaveRunAndTransition). Only a complete evaluation, one without any
//     ERROR, becomes COMPLETED with a score. Otherwise the session is
//     GRADING_ERROR and there is no score, no total and no pass/fail.
//     Grading one practice problem leaves the session RUNNING.
//
// The result is returned as a run. An incomplete run (Evaluation.Complete
// false) is a normal outcome, not an error. In a mock exam the criterion
// details of an incomplete run are withheld (spec 9.2).
//
// opts.Retry grades the last submission of the scope again as a new run, but
// only if its last run was incomplete (or it has none, after a crash) and the
// answer is provably unchanged. opts.Resubmit records a new submission whose
// parent is the last one. A plain grade of a session that was submitted
// already returns that submission's latest run together with
// ErrAlreadySubmitted. A grading that was recorded but never finished (the
// process died) is reported as ErrGradeInterrupted; retry or resubmit
// recovers it, and nothing is confirmed meanwhile.
//
// The solving time ends at opts.At; the grading time is the run's own
// StartedAt..FinishedAt and never counts as solving time. Overtime does not
// block the submission; it is recorded on it.
func (s *Service) Grade(ctx context.Context, sessionID string, opts GradeOptions) (model.GradingRun, error) {
	if opts.Retry && opts.Resubmit {
		return model.GradingRun{}, ErrConflictingOptions
	}
	at := opts.At
	if at.IsZero() {
		at = s.cfg.Now()
	}
	at = at.Round(0).UTC()
	opts.At = at

	release, err := lock.Acquire(author.LockPath(s.cfg.StateDir))
	if errors.Is(err, lock.ErrLocked) {
		return model.GradingRun{}, fmt.Errorf("session: grade: another start, reset, validation or grade is running on this environment: %w", err)
	}
	if err != nil {
		return model.GradingRun{}, fmt.Errorf("session: grade: %w", err)
	}
	defer release()
	if err := s.requireNoPendingReset(ctx); err != nil {
		return model.GradingRun{}, err
	}

	rec, err := s.cfg.Store.LoadSession(ctx, sessionID)
	if err != nil {
		return model.GradingRun{}, fmt.Errorf("session: grade %q: %w", sessionID, err)
	}
	if rec.EnvironmentID != s.cfg.Identity.ID {
		return model.GradingRun{}, fmt.Errorf("%w: session %q belongs to environment %q", ErrEnvironmentChanged, sessionID, rec.EnvironmentID)
	}
	hist, err := s.loadHistory(ctx, rec.ID)
	if err != nil {
		return model.GradingRun{}, err
	}
	p, err := s.planGrade(rec, hist, opts)
	if err != nil {
		var already *alreadyError
		var stopped *interruptedError
		switch {
		case errors.As(err, &already) && already.run != nil:
			return s.disclosed(rec, hist, *already.run), err
		case errors.As(err, &stopped):
			return model.GradingRun{}, s.interrupted(rec, hist, stopped.scope)
		}
		return model.GradingRun{}, err
	}
	// A session that is over (COMPLETED) is not active; a resubmission makes
	// it active again over its namespaces, which another active session may
	// be using by now.
	if !slices.Contains(model.ActiveSessionStates(), rec.State) && slices.Contains(model.ActiveSessionStates(), p.to) {
		if err := s.requireNoOtherActiveSession(ctx, rec.ID); err != nil {
			return model.GradingRun{}, err
		}
	}
	return s.execute(ctx, rec, hist, p, at)
}

// requireNoOtherActiveSession refuses while a session other than this one is
// active on the environment. The caller holds the lock.
func (s *Service) requireNoOtherActiveSession(ctx context.Context, sessionID string) error {
	active, err := s.cfg.Store.ActiveSessions(ctx, s.cfg.Identity.ID)
	if err != nil {
		return fmt.Errorf("session: look for active sessions: %w", err)
	}
	var list []string
	for _, a := range active {
		if a.ID != sessionID {
			list = append(list, fmt.Sprintf("%s (%s, %s)", a.ID, a.Mode, a.State))
		}
	}
	if len(list) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s; finish or abandon it before resubmitting this session", ErrSessionActive, strings.Join(list, ", "))
}

// environmentProblem checks, once the submission is recorded, that the grading
// can be trusted: the environment is reachable and usable, the grader is the
// version the session was prepared with, and the environment and tool versions
// are those frozen on the session (a session that predates the frozen values
// skips the comparison). The first problem found becomes an ERROR result of
// the run: it is recorded, never refused, so the submission keeps its time, and
// nothing is graded with a grader or environment the problems were not
// certified on. ok is true when there is none.
func (s *Service) environmentProblem(ctx context.Context, rec model.SessionRecord) (result model.CheckResult, ok bool) {
	problem := func(id, reason string) (model.CheckResult, bool) {
		return s.synthetic(s.cfg.Now().Round(0).UTC(), id, reason), false
	}
	if err := s.cfg.Env.Check(ctx, s.cfg.Identity); err != nil {
		return problem(resultEnvironmentUnavailable, fmt.Sprintf("the environment is not usable, so nothing was graded: %v", err))
	}
	if rec.GraderVersion != "" && rec.GraderVersion != s.cfg.GraderVersion {
		return problem(resultGraderChanged, fmt.Sprintf(
			"the session was prepared with grader %s and this program grades with %s, so nothing was graded; retry with the version that prepared the session",
			rec.GraderVersion, s.cfg.GraderVersion))
	}
	if rec.EnvironmentHash != "" {
		manifest, err := s.cfg.EnvironmentManifest(ctx, s.cfg.Identity)
		if err != nil {
			return problem(resultEnvironmentUnavailable, fmt.Sprintf("the environment versions could not be determined, so nothing was graded: %v", err))
		}
		if author.EnvironmentHash(manifest) != rec.EnvironmentHash {
			return problem(resultEnvironmentStale, fmt.Sprintf(
				"the versions of the environment and its tools are not those the session was prepared with (%s), so nothing was graded; restore them and retry",
				differingVersions(rec.EnvironmentManifest, manifest)))
		}
	}
	return model.CheckResult{}, true
}

// differingVersions names the manifest entries that differ, for a message.
func differingVersions(frozen, live map[string]string) string {
	var diff []string
	for _, k := range slices.Sorted(maps.Keys(live)) {
		if frozen[k] != live[k] {
			diff = append(diff, fmt.Sprintf("%s: %q, now %q", k, frozen[k], live[k]))
		}
	}
	for _, k := range slices.Sorted(maps.Keys(frozen)) {
		if _, ok := live[k]; !ok {
			diff = append(diff, fmt.Sprintf("%s: %q, now absent", k, frozen[k]))
		}
	}
	if len(diff) == 0 {
		return "no entry differs by name; the manifest changed"
	}
	return strings.Join(diff, "; ")
}

// execute carries a planned grading out (steps 2 to 5 of Grade).
func (s *Service) execute(ctx context.Context, rec model.SessionRecord, hist history, p gradePlan, at time.Time) (model.GradingRun, error) {
	items := s.items(rec, p.problems)
	// A reset's published attempt remains current even if the learner removes
	// its directory; never fall back to the archived answer of an older one.
	for i := range items {
		it := &items[i]
		minimum := hist.currentAttempt(it.frozen.Definition.ID)
		n, _ := strconv.Atoi(strings.TrimPrefix(filepath.Base(it.target.Workspace), attemptPrefix))
		if n < minimum && it.missing == "" {
			it.target.Workspace = AttemptDir(s.cfg.Workspace, rec.ID, it.frozen.Definition.ID, minimum)
		}
	}
	var targets []grader.Target
	var unreadable []string // problems whose workspace cannot be determined
	for _, it := range items {
		if it.missing != "" {
			unreadable = append(unreadable, it.frozen.Definition.ID)
			continue
		}
		targets = append(targets, it.target)
	}

	// What the learner submitted. A failure to fingerprint does not stop the
	// submission; it means the result cannot be shown to match it. Nothing
	// that goes wrong from here on, in the environment or in the workspace,
	// refuses the request: the submission is recorded first and the problem
	// becomes an ERROR in its run.
	baseline, capErr := s.capture(ctx, targets)
	switch {
	case len(unreadable) > 0:
		baseline, capErr = nil, fmt.Errorf("the attempt directory of %s cannot be determined", strings.Join(unreadable, ", "))
	case capErr == nil && len(baseline) == 0:
		capErr = errors.New("no input of the answer was found to fingerprint")
	}
	if p.retry {
		if err := retryProvable(p.sub, baseline, capErr, p, detailsOpen(rec, hist)); err != nil {
			return model.GradingRun{}, err
		}
	}

	// The intent first, with the time of the request (spec 11).
	attempts := map[string]int{}
	for _, it := range items {
		n, _ := strconv.Atoi(strings.TrimPrefix(filepath.Base(it.target.Workspace), attemptPrefix))
		attempts[it.frozen.Definition.ID] = max(1, n, hist.currentAttempt(it.frozen.Definition.ID))
	}
	op := model.Operation{
		ID: GradeOperationID(p.runID), EnvironmentID: rec.EnvironmentID, SessionID: rec.ID,
		Kind: GradeOperationKind, Phase: GradePlanned,
		Planned:   model.Inventory{EnvironmentID: rec.EnvironmentID, SessionID: rec.ID, ProblemID: p.scope, Attempts: attempts},
		CreatedAt: at,
	}
	if err := s.cfg.Store.PutOperation(ctx, op); err != nil {
		return model.GradingRun{}, fmt.Errorf("session: grade %q: record the grading: %w", rec.ID, err)
	}
	for _, stale := range p.stale {
		stale.Phase = GradeSuperseded
		if err := s.cfg.Store.PutOperation(ctx, stale); err != nil {
			s.closeOperation(op, GradeSuperseded) // this request does not go ahead either
			return model.GradingRun{}, fmt.Errorf("session: grade %q: close the interrupted grading %s: %w", rec.ID, stale.ID, err)
		}
	}

	if p.isNew {
		sub := p.sub
		if capErr == nil {
			sub.InputHashes = baseline
		}
		sub.ViewedSolutions = viewedIn(hist.reviews, p.problems)
		sub.SolveDuration, sub.Overtime, sub.WithinLimit, sub.ClockAnomaly = solveTiming(rec, at)
		if err := s.cfg.Store.SubmitAndTransition(ctx, sub, p.from, p.to); err != nil {
			s.closeOperation(op, GradeSuperseded)
			return model.GradingRun{}, fmt.Errorf("session: grade %q: record the submission: %w", rec.ID, err)
		}
	} else if p.from != p.to {
		if err := s.cfg.Store.Transition(ctx, rec.ID, p.from, p.to); err != nil {
			s.closeOperation(op, GradeSuperseded)
			return model.GradingRun{}, fmt.Errorf("session: grade %q: %w", rec.ID, err)
		}
	}
	op.Phase = GradeRecorded
	// A failure here only leaves the phase at "planned", which is just as
	// unfinished; the final commit reports a store that is really gone.
	_ = s.cfg.Store.PutOperation(ctx, op)

	// From here on the session is GRADING (or the problem is being graded):
	// whatever happens, an incomplete run is recorded.
	var ev model.Evaluation
	var extra []model.CheckResult
	if problem, ok := s.environmentProblem(ctx, rec); !ok {
		// Nothing is graded: the run says why. Its maximum is still the
		// scope's, an unknown score is not a zero.
		for _, f := range p.problems {
			ev.MaxScore += maxPoints(f)
		}
		extra = append(extra, problem)
	} else {
		ev = s.evaluate(ctx, items)
		switch {
		case len(unreadable) > 0:
			// Already explained by the workspace results.
		case capErr != nil:
			extra = append(extra, s.synthetic(at, resultInputsUnverified, fmt.Sprintf(
				"the answer could not be fingerprinted when it was submitted (%v), so the result cannot be shown to match what was submitted", capErr)))
		case ctx.Err() != nil:
			extra = append(extra, s.synthetic(at, resultInputsUnverified,
				"the command was interrupted before the answer could be fingerprinted again, so the result cannot be shown to match what was submitted"))
		default:
			after, err := s.capture(ctx, targets)
			switch {
			case err != nil:
				extra = append(extra, s.synthetic(at, resultInputsUnverified, fmt.Sprintf(
					"the answer could not be fingerprinted again after grading (%v), so the result cannot be shown to match what was submitted", err)))
			default:
				if changed := changedInputs(baseline, after); len(changed) > 0 {
					extra = append(extra, s.synthetic(at, resultInputsChanged, fmt.Sprintf(
						"the answer of %s changed while it was graded (%s); stop editing and submit again with --resubmit",
						strings.Join(changedProblems(changed), ", "), strings.Join(changed, ", "))))
				}
			}
		}
	}
	ev = settle(ev, extra)

	run := model.GradingRun{ID: p.runID, SubmissionID: p.sub.ID, StartedAt: at, FinishedAt: at, Evaluation: ev}
	if now := s.cfg.Now().Round(0).UTC(); now.After(at) {
		run.FinishedAt = now
	}

	// Saving must survive a cancelled command: the learner pressing Ctrl-C
	// must not lose the incomplete run.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), savingBudget)
	defer cancel()
	to := p.finalErr
	if ev.Complete {
		to = p.finalOK
	}
	if err := s.cfg.Store.SaveRunAndTransition(saveCtx, run, rec.ID, p.final, to); err != nil {
		return model.GradingRun{}, fmt.Errorf("session: grade %q: the result could not be committed (there is no confirmed score; see the pending grading): %w", rec.ID, err)
	}
	op.Phase = GradeDone
	_ = s.cfg.Store.PutOperation(saveCtx, op) // the run exists, so the grading counts as finished either way

	rec.State = to
	out := s.disclosed(rec, hist, run)
	if err := ctx.Err(); err != nil {
		return out, fmt.Errorf("session: grading was interrupted and is recorded as incomplete: %w", err)
	}
	return out, nil
}

// gradeItem is one problem of a grading: its frozen definition and the
// target the grader reads. missing is not empty when the problem's workspace
// cannot be determined; such a problem is not graded.
type gradeItem struct {
	frozen  model.FrozenProblem
	target  grader.Target
	missing string
}

// items resolves each problem's latest attempt directory, the workspace its
// file, image and kustomize criteria read. A problem whose attempt directory
// does not exist is not a reason to refuse the submission:
//
//   - if the session's workspace directory still exists, the learner deleted
//     that attempt (or the problem's directory). The target then names the
//     first attempt directory, which does not exist, so the problem is graded
//     as it is: its file criteria find the files absent (the file checker
//     reports "does not exist", a FAIL with evidence) and the cluster criteria
//     are unaffected;
//   - if the session's whole workspace directory is gone nothing says what
//     happened (an unmounted disk, a moved folder, a wiped home), so the
//     problem is not graded and becomes an ERROR result.
func (s *Service) items(rec model.SessionRecord, problems []model.FrozenProblem) []gradeItem {
	_, statErr := os.Stat(filepath.Join(s.cfg.Workspace, rec.ID))
	sessionDir := statErr == nil
	items := make([]gradeItem, 0, len(problems))
	for _, f := range problems {
		id := f.Definition.ID
		it := gradeItem{frozen: f, target: grader.Target{Identity: s.cfg.Identity, Problem: f}}
		dir, _, err := LatestAttempt(s.cfg.Workspace, rec.ID, id)
		switch {
		case err == nil:
			it.target.Workspace = dir
		case sessionDir && errors.Is(err, fs.ErrNotExist):
			it.target.Workspace = AttemptDir(s.cfg.Workspace, rec.ID, id, 1)
		default:
			it.missing = fmt.Sprintf("the attempt directory of %s cannot be determined, so it was not graded: %v", id, err)
		}
		items = append(items, it)
	}
	return items
}

// capture fingerprints the answer of every target. The keys are qualified with
// the problem ID, since file paths repeat from problem to problem.
func (s *Service) capture(ctx context.Context, targets []grader.Target) (map[string]string, error) {
	out := map[string]string{}
	for _, t := range targets {
		m, err := s.cfg.Inputs.CaptureInputs(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("problem %s: %w", t.Problem.Definition.ID, err)
		}
		for k, v := range m {
			out[QualifiedID(t.Problem.Definition.ID, k)] = v
		}
	}
	return out, nil
}

// changedInputs lists the fingerprint keys whose value differs, or which
// exist on one side only.
func changedInputs(old, now map[string]string) []string {
	var changed []string
	for k, v := range old {
		if nv, ok := now[k]; !ok || nv != v {
			changed = append(changed, k)
		}
	}
	for k := range now {
		if _, ok := old[k]; !ok {
			changed = append(changed, k)
		}
	}
	slices.Sort(changed)
	return changed
}

// changedProblems names the problems of qualified keys, once each.
func changedProblems(keys []string) []string {
	var out []string
	for _, k := range keys {
		if p, _, ok := strings.Cut(k, "/"); ok && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// viewedIn lists, in problem order, the problems of the scope whose solution
// was opened before.
func viewedIn(reviews []model.ReviewEvent, problems []model.FrozenProblem) []string {
	var out []string
	for _, f := range problems {
		if slices.ContainsFunc(reviews, func(r model.ReviewEvent) bool { return r.ProblemID == f.Definition.ID }) {
			out = append(out, f.Definition.ID)
		}
	}
	return out
}

// evaluate grades every target with the runtime grader and merges the
// problems into one evaluation whose results are named by problem. A problem
// that could not be evaluated at all is an ERROR result of its own: it never
// reads as a failed answer.
func (s *Service) evaluate(ctx context.Context, items []gradeItem) model.Evaluation {
	merged := model.Evaluation{Complete: true}
	for _, it := range items {
		t, pid := it.target, it.frozen.Definition.ID
		if it.missing != "" {
			merged.Results = append(merged.Results, s.synthetic(s.cfg.Now().Round(0).UTC(), resultWorkspaceMissing+pid, it.missing))
			merged.MaxScore += maxPoints(it.frozen)
			merged.Complete = false
			continue
		}
		ev, err := s.cfg.Engine.Evaluate(ctx, t)
		if err != nil {
			reason := fmt.Sprintf("problem %s could not be graded: %v", pid, err)
			if ctx.Err() != nil {
				reason = fmt.Sprintf("problem %s was not graded: the command was interrupted", pid)
			}
			merged.Results = append(merged.Results, model.CheckResult{
				CriterionID: resultEvaluatePrefix + pid, Status: model.StatusError, Reason: reason,
				Evidence: model.Evidence{ObservedAt: s.cfg.Now().Round(0).UTC()},
			})
			merged.MaxScore += maxPoints(t.Problem)
			merged.Complete = false
			continue
		}
		for _, r := range ev.Results {
			r.CriterionID = QualifiedID(pid, r.CriterionID)
			merged.Results = append(merged.Results, r)
		}
		merged.MaxScore += ev.MaxScore
		if !ev.Complete || ev.Score == nil {
			merged.Complete = false
		}
	}
	return merged
}

func maxPoints(f model.FrozenProblem) int {
	total := 0
	for _, c := range f.Definition.Criteria {
		total += c.Points
	}
	return total
}

func (s *Service) synthetic(at time.Time, id, reason string) model.CheckResult {
	return model.CheckResult{CriterionID: id, Status: model.StatusError, Reason: reason, Evidence: model.Evidence{ObservedAt: at}}
}

// settle makes the evaluation obey the one rule of spec 8.1: any ERROR leaves
// the submission incomplete and without a score; a score exists only for a
// complete evaluation, and is the sum of the points of the PASSed criteria.
func settle(ev model.Evaluation, extra []model.CheckResult) model.Evaluation {
	ev.Results = append(slices.Clone(ev.Results), extra...)
	score := 0
	for _, r := range ev.Results {
		if r.Status == model.StatusError {
			ev.Complete = false
		}
		score += r.Points
	}
	if len(ev.Results) == 0 {
		ev.Complete = false
	}
	ev.Score = nil
	if ev.Complete {
		ev.Score = &score
	}
	return ev
}

// closeOperation ends an operation after a failure, even when the command's
// context is gone.
func (s *Service) closeOperation(op model.Operation, phase string) {
	ctx, cancel := context.WithTimeout(context.Background(), savingBudget)
	defer cancel()
	op.Phase = phase
	_ = s.cfg.Store.PutOperation(ctx, op)
}

// solveTiming is the learner's time for a submission at `at`: the wall clock
// since the session started, the part beyond the limit, whether the
// submission met the limit, and whether the clock ran backwards (then the
// solving time is zero and a limit is not claimed to be met). A session
// without a limit (practice) is always within it.
func solveTiming(rec model.SessionRecord, at time.Time) (solve, overtime time.Duration, within, anomaly bool) {
	solve, overtime, anomaly = wallClock(rec, at)
	within = rec.DurationLimit <= 0 || (!anomaly && solve <= rec.DurationLimit)
	return solve, overtime, within, anomaly
}

// disclosed is the run as the learner may see it now: a mock exam's
// incomplete run shows no criterion detail (spec 9.2) until the exam has its
// complete result, everything else is as stored.
func (s *Service) disclosed(rec model.SessionRecord, hist history, run model.GradingRun) model.GradingRun {
	if detailsOpen(rec, hist) || run.Evaluation.Complete {
		return run
	}
	return withheld(run)
}
