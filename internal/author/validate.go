//go:build linux

package author

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/lock"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/store"
)

// SessionID is the session ID validation runs under. Everything a validation
// applies carries it in its owner labels (junkloud.io/session-id), so it is
// told apart from every learner session and never adopts or deletes a
// learner's objects. The problem ID is the problem's own.
const SessionID = "validation"

// Keys of ValidationRecord.Cases. The first validated seed uses the plain
// keys; every other seed's keys are prefixed (SeedKey).
const (
	KeyInitial = "initial" // right after setup
	KeySolved  = "solved"  // after the reference solution
	KeyReset   = "reset"   // initial state again, after the solved run was reset
)

// CaseKey is the record key of the case with this ID (a planted wrong answer
// or an alternative correct one).
func CaseKey(id string) string { return "case:" + id }

// SeedKey prefixes a record key with the seed it belongs to. Validate uses
// it for every seed but the first.
func SeedKey(seed uint64, key string) string { return fmt.Sprintf("seed-%d/%s", seed, key) }

// Errors. Match them with errors.Is.
var (
	// ErrNotValid: the problem was proven unsound. The error is a
	// *ValidationError listing every finding; the record (which is also
	// stored, with Passed false) keeps the evaluations. Anything that is not
	// the problem's fault (cluster unavailable, a foreign namespace in the
	// way, cleanup that cannot finish, cancellation) is a different error and
	// stores nothing.
	ErrNotValid = errors.New("author: problem did not validate")
	// ErrSessionActive: a training or exam session of the environment is in a
	// non-terminal state (model.ActiveSessionStates). Real-cluster validation
	// is refused until it ends (spec 7.1).
	ErrSessionActive = errors.New("author: a session is active on this environment")
	// ErrNoSeeds: Validate needs at least one seed, each distinct.
	ErrNoSeeds = errors.New("author: validation needs at least one seed and no repeated seed")
)

// ValidationError is the ErrNotValid error: what is wrong with the problem.
type ValidationError struct {
	ProblemID string
	Problems  []string
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %q:", ErrNotValid.Error(), e.ProblemID)
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p)
	}
	return b.String()
}

// Unwrap makes errors.Is(err, ErrNotValid) true.
func (e *ValidationError) Unwrap() error { return ErrNotValid }

// Applier applies rendered actions for an owner; *environment.Manager.
type Applier interface {
	Apply(ctx context.Context, id model.Identity, owner environment.Owner, actions []environment.Action) (model.Inventory, error)
}

// Evaluator grades a frozen problem and checks its criteria statically;
// *grader.Engine, the same engine sessions use.
type Evaluator interface {
	Validate(criteria []model.Criterion) error
	Evaluate(ctx context.Context, t grader.Target) (model.Evaluation, error)
}

// Records is the part of the store a validation uses; *store.Store.
type Records interface {
	ActiveSessions(ctx context.Context, environmentID string) ([]store.SessionStatus, error)
	SaveValidation(ctx context.Context, rec model.ValidationRecord) error
	PutInventory(ctx context.Context, inv model.Inventory) error
}

// Config wires a Validator. NewRuntime builds one from a real Manager.
type Config struct {
	// Identity is the verified environment (Manager.Ensure).
	Identity model.Identity
	// Env applies setup, solution and case actions (the Manager).
	Env Applier
	// Janitor resets the temporary bundle between and after the steps.
	Janitor Janitor
	// Engine is the grader engine, built with the composition options the
	// grader requires (WithPreloadedImages, WithInputKey, shared timeouts).
	Engine Evaluator
	Store  Records
	// LockPath is the environment's mutation lock file (LockPath(stateDir)).
	LockPath string
	// GraderVersion and EnvironmentHash complete the certificate key.
	GraderVersion   string
	EnvironmentHash string
	// ProvidedImages are the image references the environment provides: the
	// pinned, preloaded set (probe image plus the problems' pinned images).
	// The images used by a problem's setup, solution and alternative answers
	// must all be in it, because nodes never pull and no verdict may depend
	// on a registry. Fail-closed: an empty list provides nothing.
	ProvidedImages []string
	// Prepare, if set, runs once per Validate while the lock is held and no
	// session is active, before the first step: EnsurePlatform and image
	// preloading (PrepareEnvironment), which need the lock too.
	Prepare func(ctx context.Context) error
	// ScratchDir holds the scratch workspaces stages are evaluated in
	// (ScratchWorkspace); "" means the system temporary directory.
	ScratchDir string
}

// Validator proves authored problems with the runtime grader.
type Validator struct{ cfg Config }

// NewValidator checks that cfg is complete.
func NewValidator(cfg Config) (*Validator, error) {
	switch {
	case cfg.Identity.ID == "":
		return nil, errors.New("author: validator needs a verified environment identity")
	case cfg.Env == nil, cfg.Janitor == nil, cfg.Engine == nil, cfg.Store == nil:
		return nil, errors.New("author: validator needs an Env, Janitor, Engine and Store")
	case cfg.LockPath == "":
		return nil, errors.New("author: validator needs the environment lock path")
	case cfg.GraderVersion == "", cfg.EnvironmentHash == "":
		return nil, errors.New("author: validator needs the grader version and the environment hash")
	}
	return &Validator{cfg: cfg}, nil
}

// finalCleanupBudget bounds the cleanup that runs after cancellation.
const finalCleanupBudget = 5 * time.Minute

// Validate proves p for each seed and stores the result (spec 7.1).
//
// It first takes the environment lock and refuses while any session of the
// environment is active, then freezes p for every seed and statically checks
// the rendered criteria, that every manifest renders and that setup, the
// solution and alternative answers use only provided images (no cluster is
// touched if that fails). For each seed
// it uses a fresh temporary namespace bundle (the problem namespace plus its
// declared scope), owned by SessionID, and the same Apply and engine that
// sessions use:
//
//  1. Initial: setup, evaluate. The statuses and score must equal the
//     problem's initial_expected / initial_score and must not be full marks.
//  2. Solved: reset, setup, solution, evaluate: every criterion PASS, full
//     marks.
//  3. Each case (a planted wrong answer or an alternative correct one):
//     reset, setup, the case's actions, evaluate: exactly the case's
//     expected statuses and expected_score, so a lax grader that passes a
//     wrong answer is caught. At least one case must score below the maximum.
//  4. Reset: reset, setup, evaluate: the initial result again, proving that
//     nothing of the solved and wrong runs leaked.
//
// Every stage is evaluated with a fresh scratch workspace holding the
// problem's provided files (ScratchWorkspace), as a session's preparation
// does. Any ERROR result anywhere means the problem is not valid; the
// evaluations stay in the record. The bundle is removed after every step and
// at the end.
//
// A problem that fails any rule returns the record (Passed false, stored)
// and a *ValidationError (ErrNotValid). Infrastructure failures return a
// different error and store nothing. The record is keyed by (problem hash,
// grader version, environment hash); see CertificateMatches.
func (v *Validator) Validate(ctx context.Context, p model.Problem, seeds []uint64) (model.ValidationRecord, error) {
	distinct := map[uint64]bool{}
	for _, seed := range seeds {
		distinct[seed] = true
	}
	if len(seeds) == 0 || len(distinct) != len(seeds) {
		return model.ValidationRecord{}, ErrNoSeeds
	}
	hash, err := problem.Hash(p)
	if err != nil {
		return model.ValidationRecord{}, err
	}
	record := model.ValidationRecord{
		ProblemHash: hash, GraderVersion: v.cfg.GraderVersion, EnvironmentHash: v.cfg.EnvironmentHash,
		Seeds: slices.Clone(seeds), Cases: map[string]model.Evaluation{},
	}

	release, err := lock.Acquire(v.cfg.LockPath)
	if errors.Is(err, lock.ErrLocked) {
		return record, fmt.Errorf("author: validate %q: another start, reset, validation or grade is running on this environment: %w", p.ID, err)
	}
	if err != nil {
		return record, fmt.Errorf("author: validate %q: %w", p.ID, err)
	}
	defer release()

	active, err := v.cfg.Store.ActiveSessions(ctx, v.cfg.Identity.ID)
	if err != nil {
		return record, fmt.Errorf("author: validate %q: %w", p.ID, err)
	}
	if len(active) > 0 {
		var list []string
		for _, s := range active {
			list = append(list, fmt.Sprintf("%s (%s, %s)", s.ID, s.Mode, s.State))
		}
		return record, fmt.Errorf("%w: finish or abandon %s before validating %q", ErrSessionActive, strings.Join(list, ", "), p.ID)
	}

	var problems []string
	frozen := make([]model.FrozenProblem, 0, len(seeds))
	for _, seed := range seeds {
		f, err := problem.Freeze(p, seed)
		if err == nil {
			err = v.cfg.Engine.Validate(f.Definition.Criteria)
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("seed %d: %v", seed, err))
			continue
		}
		// Every manifest of every seed must render and may use only the
		// provided images, before any of it is applied.
		before := len(problems)
		for _, msg := range v.staticActionFindings(p, f) {
			problems = append(problems, fmt.Sprintf("seed %d: %s", seed, msg))
		}
		if len(problems) == before {
			frozen = append(frozen, f)
		}
	}
	if !slices.ContainsFunc(p.Cases, func(c model.Case) bool { return c.ExpectedScore < maxScore(p) }) {
		problems = append(problems, "no wrong case: declare at least one case with a planted error that scores below the maximum")
	}

	if len(problems) == 0 {
		if v.cfg.Prepare != nil {
			if err := v.cfg.Prepare(ctx); err != nil {
				return record, fmt.Errorf("author: prepare environment to validate %q: %w", p.ID, err)
			}
		}
		for i, f := range frozen {
			run := &seedRun{v: v, p: p, f: f, rec: &record, problems: &problems, first: i == 0}
			if err := run.execute(ctx); err != nil {
				return record, err
			}
		}
	}

	record.Passed = len(problems) == 0
	if err := v.cfg.Store.SaveValidation(context.WithoutCancel(ctx), record); err != nil {
		return record, fmt.Errorf("author: store the validation of %q: %w", p.ID, err)
	}
	if !record.Passed {
		return record, &ValidationError{ProblemID: p.ID, Problems: problems}
	}
	return record, nil
}

// actionGroup is one list of actions of a problem and whether its end state
// must be reachable offline, with only the provided images.
type actionGroup struct {
	label   string
	actions []model.Action
	pinned  bool
}

// staticActionFindings renders every action list of the frozen problem
// without touching the cluster and reports what is wrong with it: a manifest
// that does not render, an action kind validation cannot run, an image that
// the environment does not provide. Wrong-answer cases may use any image (a
// wrong image is a typical planted error); setup, the solution and
// alternative answers (cases scoring the maximum) may not.
func (v *Validator) staticActionFindings(p model.Problem, f model.FrozenProblem) []string {
	groups := []actionGroup{{"setup", p.Setup, true}, {"solution", p.Solution, true}}
	for _, c := range p.Cases {
		groups = append(groups, actionGroup{fmt.Sprintf("case %q actions", c.ID), c.Actions, c.ExpectedScore >= maxScore(p)})
	}
	provided := map[string]bool{}
	for _, img := range v.cfg.ProvidedImages {
		provided[grader.NormalizeImageRef(img)] = true
	}
	var findings []string
	for _, g := range groups {
		built, err := BuildActions(p.Files, f.Parameters, g.actions)
		if err != nil {
			findings = append(findings, fmt.Sprintf("%s: %v", g.label, err))
			continue
		}
		if !g.pinned {
			continue
		}
		for _, a := range built {
			for _, obj := range a.Objects {
				for _, image := range ObjectImages(obj) {
					if !provided[grader.NormalizeImageRef(image)] {
						findings = append(findings, fmt.Sprintf("%s: image %q is not among the images the environment provides (pinned and preloaded); use one of them", g.label, image))
					}
				}
			}
		}
	}
	return findings
}

func maxScore(p model.Problem) int {
	sum := 0
	for _, c := range p.Criteria {
		sum += c.Points
	}
	return sum
}

// seedRun is the validation of one seed.
type seedRun struct {
	v        *Validator
	p        model.Problem
	f        model.FrozenProblem
	rec      *model.ValidationRecord
	problems *[]string
	first    bool

	owner environment.Owner
	clean bool // nothing of ours is in the cluster
}

func (s *seedRun) key(k string) string {
	if s.first {
		return k
	}
	return SeedKey(s.f.Seed, k)
}

func (s *seedRun) finding(label, format string, args ...any) {
	msg := label + ": " + fmt.Sprintf(format, args...)
	if !s.first || len(s.rec.Seeds) > 1 {
		msg = fmt.Sprintf("seed %d: %s", s.f.Seed, msg)
	}
	*s.problems = append(*s.problems, msg)
}

// stageSpec is one evaluated state of a seed's validation: the extra actions
// applied after setup (nil for the plain initial state), their description
// for error messages, and the rule the evaluation must satisfy.
type stageSpec struct {
	label, key string
	actions    []model.Action
	what       string
	check      func(label string, ev model.Evaluation)
}

func (s *seedRun) execute(ctx context.Context) (err error) {
	s.owner = environment.Owner{
		Inventory: model.Inventory{EnvironmentID: s.v.cfg.Identity.ID, SessionID: SessionID, ProblemID: s.p.ID},
		Namespace: s.f.Namespace,
		Scope:     s.f.Definition.Scope,
	}
	// Whatever happens, including cancellation, the bundle must not stay.
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalCleanupBudget)
		defer cancel()
		if cerr := s.reset(cctx); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()

	// Leftovers of an interrupted earlier validation of this problem.
	if err := s.reset(ctx); err != nil {
		return err
	}
	stages := []stageSpec{
		{"initial", KeyInitial, nil, "", s.checkInitial},
		{"solved", KeySolved, s.p.Solution, "solution", s.checkSolved},
	}
	for _, c := range s.p.Cases {
		stages = append(stages, stageSpec{
			label: fmt.Sprintf("case %q", c.ID), key: CaseKey(c.ID), actions: c.Actions,
			what:  fmt.Sprintf("case %q actions", c.ID),
			check: func(label string, ev model.Evaluation) { s.compare(label, ev, c.Expected, c.ExpectedScore) },
		})
	}
	stages = append(stages, stageSpec{"reset (initial state again after the other runs)", KeyReset, nil, "", s.checkInitial})

	for _, st := range stages {
		if err := s.stage(ctx, st); err != nil {
			return err
		}
	}
	return nil
}

// stage runs reset, setup, extra actions and one evaluation. An author error
// (a bad manifest, an out-of-scope object) ends the stage as a finding; an
// infrastructure error aborts the validation.
func (s *seedRun) stage(ctx context.Context, st stageSpec) error {
	label := st.label
	if err := s.reset(ctx); err != nil {
		return err
	}
	finding, err := s.apply(ctx, "setup", s.p.Setup, true)
	if err == nil && finding == "" && st.actions != nil {
		finding, err = s.apply(ctx, st.what, st.actions, false)
	}
	if err != nil {
		return err
	}
	if finding != "" {
		s.finding(label, "%s", finding)
		return nil
	}
	// The workspace a learner has right after Start: the provided files,
	// rendered, and nothing else (the same helper a session's preparation
	// uses), fresh for every stage.
	ws, remove, err := ScratchWorkspace(s.v.cfg.ScratchDir, s.f)
	if err != nil {
		return err
	}
	defer remove()
	ev, err := s.v.cfg.Engine.Evaluate(ctx, grader.Target{Identity: s.v.cfg.Identity, Problem: s.f, Workspace: ws})
	if errors.Is(err, grader.ErrInvalidCriteria) {
		s.finding(label, "%v", err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("author: evaluate %s of %q: %w", label, s.p.ID, err)
	}
	s.rec.Cases[s.key(st.key)] = ev
	st.check(label, ev)
	return nil
}

// apply builds and applies one action list. Setup (create true) is the only
// step that may create the bundle's namespaces.
func (s *seedRun) apply(ctx context.Context, what string, actions []model.Action, create bool) (finding string, err error) {
	built, berr := BuildActions(s.p.Files, s.f.Parameters, actions)
	if berr != nil {
		return fmt.Sprintf("%s: %v", what, berr), nil
	}
	owner := s.owner
	owner.CreateNamespaces = create
	s.clean = false
	inv, aerr := s.v.cfg.Env.Apply(ctx, s.v.cfg.Identity, owner, built)
	s.owner.Inventory = inv // what is owned now, also after a partial failure
	switch {
	case aerr == nil:
		return "", nil
	case isAuthorError(aerr):
		return fmt.Sprintf("%s: %v", what, aerr), nil
	default:
		return "", fmt.Errorf("author: %s of %q: %w", what, s.p.ID, aerr)
	}
}

// isAuthorError: the failure is the problem package's fault, not the
// environment's.
func isAuthorError(err error) bool {
	return errors.Is(err, environment.ErrInvalidAction) || errors.Is(err, environment.ErrOutOfScope) ||
		errors.Is(err, environment.ErrUnsupportedAction) || errors.Is(err, problem.ErrUndefinedParameter) ||
		apierrors.IsInvalid(err) || apierrors.IsBadRequest(err)
}

// reset removes the temporary bundle (waiting for it to be gone) and clears
// the stored inventory.
func (s *seedRun) reset(ctx context.Context) error {
	if s.clean {
		return nil
	}
	if err := s.v.cfg.Janitor.Cleanup(ctx, s.owner); err != nil {
		return fmt.Errorf("author: reset the validation namespaces of %q: %w", s.p.ID, err)
	}
	s.owner.Inventory = model.Inventory{EnvironmentID: s.v.cfg.Identity.ID, SessionID: SessionID, ProblemID: s.p.ID}
	if err := s.v.cfg.Store.PutInventory(ctx, s.owner.Inventory); err != nil {
		return fmt.Errorf("author: clear the validation inventory of %q: %w", s.p.ID, err)
	}
	s.clean = true
	return nil
}

func (s *seedRun) checkInitial(label string, ev model.Evaluation) {
	if ev.Complete && ev.Score != nil && *ev.Score == ev.MaxScore {
		s.finding(label, "the problem starts at full marks (%d of %d): the unsolved state must not satisfy every requirement", *ev.Score, ev.MaxScore)
	}
	s.compare(label, ev, s.p.InitialExpected, s.p.InitialScore)
}

func (s *seedRun) checkSolved(label string, ev model.Evaluation) {
	want := map[string]string{}
	for _, c := range s.p.Criteria {
		want[c.ID] = model.StatusPass
	}
	s.compare(label, ev, want, ev.MaxScore)
}

// compare reports every difference between the evaluation and what the
// problem declares: ERROR results, statuses, the score.
func (s *seedRun) compare(label string, ev model.Evaluation, wantStatuses map[string]string, wantScore int) {
	for _, r := range ev.Results {
		if r.Status == model.StatusError {
			s.finding(label, "criterion %q graded ERROR: %s", r.CriterionID, r.Reason)
		}
	}
	for _, r := range ev.Results {
		if r.Status == model.StatusError {
			continue
		}
		if want := wantStatuses[r.CriterionID]; r.Status != want {
			s.finding(label, "criterion %q: got %s, want %s (%s)", r.CriterionID, r.Status, want, r.Reason)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(wantStatuses)) {
		if !slices.ContainsFunc(ev.Results, func(r model.CheckResult) bool { return r.CriterionID == id }) {
			s.finding(label, "criterion %q is declared but was not graded", id)
		}
	}
	switch {
	case ev.Score == nil:
		s.finding(label, "the score is pending because of ERROR results; want %d", wantScore)
	case *ev.Score != wantScore:
		s.finding(label, "score %d of %d, want %d", *ev.Score, ev.MaxScore, wantScore)
	}
}
