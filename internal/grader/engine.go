// Package grader evaluates a frozen problem's criteria against the live
// cluster and turns the outcomes into a score (spec section 8).
//
// The Engine owns scoring, ordering and failure semantics; Checkers own what
// a criterion kind means. Checkers are plugged in per kind (see NewEngine and
// DefaultCheckers) and receive their Kubernetes access by injection.
//
// Statuses follow spec 8.1: PASS earns the points, FAIL is a learner-visible
// miss with 0 points, and ERROR means the managed environment could not
// decide. Any ERROR leaves the score pending (Evaluation.Score == nil,
// Complete == false) and the ERROR points stay in MaxScore.
package grader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
)

// Sentinel errors. Match them with errors.Is.
var (
	// ErrInvalidCriterion marks an author error: unknown Args keys, a bad
	// path or an unsupported target. The criterion is graded ERROR, never a
	// learner FAIL, and nothing is read from the cluster for it.
	ErrInvalidCriterion = errors.New("invalid criterion")
	// ErrInvalidCriteria marks a criteria graph the engine refuses to run:
	// none at all, duplicate IDs, unknown prerequisites or a cycle.
	ErrInvalidCriteria = errors.New("invalid criteria")
	// ErrNotFound is returned by an ObjectReader when the object (or its
	// whole resource type) does not exist. It is the only reader error that
	// means "the learner did not create it".
	ErrNotFound = errors.New("object not found")
	// ErrInsufficientEvidence: the check ran but the evidence does not prove
	// whose fault the outcome is (for example a rollout that stalled for a
	// cluster reason). Spec 8.1: never guess FAIL; the criterion is ERROR.
	ErrInsufficientEvidence = errors.New("insufficient evidence")
	// ErrTargetChanged: the target's UID or spec changed, or it vanished,
	// while it was being graded. No consistent result exists, so the
	// criterion is ERROR (spec 8.3).
	ErrTargetChanged = errors.New("target changed during grading")
)

// ReasonPrerequisite starts the reason of every criterion that was not run
// because a prerequisite did not pass.
const ReasonPrerequisite = "prerequisite not satisfied"

// Defaults.
const (
	// DefaultTimeout is the convergence limit when a criterion declares none
	// (spec 8.3).
	DefaultTimeout = 30 * time.Second
	// DefaultParallelism is how many criteria run at once.
	DefaultParallelism = 2
	// DefaultPollInterval separates two observations of a converging object.
	DefaultPollInterval = time.Second

	// hardLimitGrace is added to a criterion's timeout to bound the real
	// time a misbehaving checker can hold a worker.
	hardLimitGrace = 15 * time.Second
	// argMutates is the Args key that marks a criterion as mutating.
	argMutates = "mutates"
)

// Target is what is graded: the environment, the frozen problem (whose
// Definition.Criteria are already rendered) and the session workspace that
// holds submitted files.
type Target struct {
	Identity  model.Identity
	Problem   model.FrozenProblem
	Workspace string
}

// Checker grades one criterion kind. It returns a CheckResult for PASS and
// FAIL. A returned error means the check could not be decided and is graded
// ERROR; wrap ErrInvalidCriterion for author errors. The engine assigns
// CriterionID and Points, so a checker only sets Status, Reason and
// Evidence.
type Checker interface {
	Check(ctx context.Context, t Target, c model.Criterion) (model.CheckResult, error)
}

// MutatingChecker is implemented by checkers whose criteria change cluster
// state while probing. Such criteria run alone: never beside any other
// criterion. A criterion can also opt in itself with Args "mutates: true".
type MutatingChecker interface {
	Checker
	Mutates(c model.Criterion) bool
}

// ArgsValidator is implemented by checkers that can validate a criterion's
// definition without a cluster. Engine.Validate uses it for authoring.
type ArgsValidator interface {
	ValidateCriterion(c model.Criterion) error
}

// Clock abstracts time so tests never sleep.
type Clock interface {
	Now() time.Time
	// Sleep waits d or until ctx ends, returning ctx.Err() in that case.
	Sleep(ctx context.Context, d time.Duration) error
}

// SystemClock is the real clock.
type SystemClock struct{}

// Now implements Clock.
func (SystemClock) Now() time.Time { return time.Now() }

// Sleep implements Clock.
func (SystemClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Option configures an Engine or a checker. Pass the same options to the
// engine and to every checker so they share one clock and one Redactor.
type Option func(*settings)

type settings struct {
	clock            Clock
	defaultTimeout   time.Duration
	parallelism      int
	pollInterval     time.Duration
	redactor         *Redactor
	preloaded        []string
	preloadedSet     bool // WithPreloadedImages was given, even if empty
	probeImage       string
	controlNamespace string
	inputKey         []byte
	tools            ToolRunner
}

// WithClock injects the clock (default SystemClock).
func WithClock(c Clock) Option { return func(s *settings) { s.clock = c } }

// WithDefaultTimeout sets the convergence limit used when a criterion has no
// Timeout (default DefaultTimeout).
func WithDefaultTimeout(d time.Duration) Option { return func(s *settings) { s.defaultTimeout = d } }

// WithParallelism sets how many criteria the engine runs at once (default
// DefaultParallelism). Values below 1 are ignored.
func WithParallelism(n int) Option {
	return func(s *settings) {
		if n >= 1 {
			s.parallelism = n
		}
	}
}

// WithPollInterval sets the gap between observations (default
// DefaultPollInterval).
func WithPollInterval(d time.Duration) Option {
	return func(s *settings) {
		if d > 0 {
			s.pollInterval = d
		}
	}
}

// WithRedactor sets the Redactor an engine uses. The engine hands it to every
// checker through the context, so checkers need no matching option; a checker
// used without an engine falls back to its own option or a private Redactor.
func WithRedactor(r *Redactor) Option { return func(s *settings) { s.redactor = r } }

// WithPreloadedImages lists the image references the environment provides
// itself (the pinned, preloaded set). A pod that cannot start because one of
// them failed to pull is an environment fault (ERROR); a pull failure of any
// other image may be the learner's wrong image (FAIL) when the registry's
// answer proves the reference wrong. References are compared after Docker
// defaulting, so "busybox:1.37.0" equals "docker.io/library/busybox:1.37.0".
//
// Fail-closed: without this option the grader does not know which images the
// environment provides, so every image pull failure is ERROR. Composition
// code must always pass the environment's provided set (probe image plus
// every pinned problem image), possibly empty.
func WithPreloadedImages(images ...string) Option {
	return func(s *settings) {
		s.preloaded = append(s.preloaded, images...)
		s.preloadedSet = true
	}
}

// WithInputKey sets the secret key that keys the input fingerprints of
// ArtifactChecker.CaptureInputs (same as ArtifactConfig.InputKey).
func WithInputKey(key []byte) Option {
	return func(s *settings) { s.inputKey = append([]byte(nil), key...) }
}

// WithToolRunner replaces the ToolRunner NewClusterArtifactChecker builds
// (NewToolRunner), for example to observe every external invocation in a test.
func WithToolRunner(r ToolRunner) Option { return func(s *settings) { s.tools = r } }

type redactorKey struct{}

// withRedactor returns ctx carrying the engine's Redactor.
func withRedactor(ctx context.Context, r *Redactor) context.Context {
	return context.WithValue(ctx, redactorKey{}, r)
}

// redactorFor is the Redactor a checker must learn Secrets into: the
// engine's when the check runs under an engine, otherwise fallback.
func redactorFor(ctx context.Context, fallback *Redactor) *Redactor {
	if r, ok := ctx.Value(redactorKey{}).(*Redactor); ok && r != nil {
		return r
	}
	return fallback
}

func newSettings(opts []Option) settings {
	s := settings{
		clock:          SystemClock{},
		defaultTimeout: DefaultTimeout,
		parallelism:    DefaultParallelism,
		pollInterval:   DefaultPollInterval,
	}
	for _, o := range opts {
		o(&s)
	}
	if s.clock == nil {
		s.clock = SystemClock{}
	}
	if s.defaultTimeout <= 0 {
		s.defaultTimeout = DefaultTimeout
	}
	if s.redactor == nil {
		s.redactor = NewRedactor()
	}
	return s
}

// Engine evaluates every criterion of a frozen problem.
type Engine struct {
	checkers map[string]Checker
	set      settings
}

// NewEngine returns an engine that grades each criterion kind with the
// checker registered under that kind's name (the model.Criterion* constants).
func NewEngine(checkers map[string]Checker, opts ...Option) *Engine {
	cp := make(map[string]Checker, len(checkers))
	for k, v := range checkers {
		cp[k] = v
	}
	return &Engine{checkers: cp, set: newSettings(opts)}
}

// Validate checks the criteria graph and, for checkers that implement
// ArgsValidator, every criterion's definition. Errors wrap ErrInvalidCriteria
// or ErrInvalidCriterion. It never touches a cluster.
func (e *Engine) Validate(criteria []model.Criterion) error {
	if _, err := planGraph(criteria); err != nil {
		return err
	}
	for _, c := range criteria {
		ck, ok := e.checkers[c.Kind]
		if !ok {
			return fmt.Errorf("%w: criterion %q: no checker for kind %q", ErrInvalidCriterion, c.ID, c.Kind)
		}
		if v, ok := ck.(ArgsValidator); ok {
			if err := v.ValidateCriterion(c); err != nil {
				return fmt.Errorf("criterion %q: %w", c.ID, err)
			}
		}
	}
	return nil
}

// Evaluate grades t.Problem.Definition.Criteria. Results keep declaration
// order. A criterion whose prerequisite FAILed is FAIL with ReasonPrerequisite
// and is not run; one whose prerequisite ERRORed is ERROR. The returned error
// is non-nil only when no evaluation could be produced (invalid criteria
// graph, cancelled context).
func (e *Engine) Evaluate(ctx context.Context, t Target) (model.Evaluation, error) {
	crits := t.Problem.Definition.Criteria
	deps, err := planGraph(crits)
	if err != nil {
		return model.Evaluation{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.Evaluation{}, err
	}
	ctx = withRedactor(ctx, e.set.redactor)

	results := make([]model.CheckResult, len(crits))
	done := make([]chan struct{}, len(crits))
	for i := range done {
		done[i] = make(chan struct{})
	}
	sem := make(chan struct{}, e.set.parallelism)
	var gate sync.RWMutex // mutating criteria hold it exclusively
	var wg sync.WaitGroup
	for i := range crits {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done[i])
			for _, d := range deps[i] {
				select {
				case <-done[d]:
				case <-ctx.Done():
					return
				}
			}
			if r, skip := e.skipFor(crits[i], deps[i], results); skip {
				results[i] = r
				return
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			results[i] = e.run(ctx, t, crits[i], &gate)
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return model.Evaluation{}, err
	}

	// Scrub only now: every Secret any criterion read is known, whatever
	// order the criteria finished in.
	for i := range results {
		results[i] = e.set.redactor.ScrubResult(results[i])
	}

	ev := model.Evaluation{Results: results, Complete: true}
	score := 0
	for i, r := range results {
		ev.MaxScore += crits[i].Points
		score += r.Points
		if r.Status == model.StatusError {
			ev.Complete = false
		}
	}
	if ev.Complete {
		ev.Score = &score
	}
	return ev, nil
}

// skipFor decides a criterion from its prerequisites' results without
// running it.
func (e *Engine) skipFor(c model.Criterion, deps []int, results []model.CheckResult) (model.CheckResult, bool) {
	var failed, errored []string
	for _, d := range deps {
		switch results[d].Status {
		case model.StatusPass:
		case model.StatusFail:
			failed = append(failed, results[d].CriterionID)
		default:
			errored = append(errored, results[d].CriterionID)
		}
	}
	switch {
	case len(failed) > 0:
		return e.synthetic(c, model.StatusFail, fmt.Sprintf("%s: %v did not pass", ReasonPrerequisite, failed)), true
	case len(errored) > 0:
		return e.synthetic(c, model.StatusError, fmt.Sprintf("prerequisite could not be evaluated: %v", errored)), true
	}
	return model.CheckResult{}, false
}

func (e *Engine) synthetic(c model.Criterion, status, reason string) model.CheckResult {
	return model.CheckResult{
		CriterionID: c.ID, Status: status, Reason: reason,
		Evidence: model.Evidence{ObservedAt: e.set.clock.Now().UTC()},
	}
}

// run executes one criterion and normalizes whatever the checker did.
func (e *Engine) run(ctx context.Context, t Target, c model.Criterion, gate *sync.RWMutex) model.CheckResult {
	start := e.set.clock.Now()
	if c.Timeout <= 0 {
		c.Timeout = e.set.defaultTimeout
	}
	ck, ok := e.checkers[c.Kind]
	if !ok {
		return e.synthetic(c, model.StatusError, fmt.Sprintf("no checker is registered for criterion kind %q", c.Kind))
	}
	if mutates(ck, c) {
		gate.Lock()
		defer gate.Unlock()
	} else {
		gate.RLock()
		defer gate.RUnlock()
	}
	cctx, cancel := context.WithTimeout(ctx, c.Timeout+hardLimitGrace)
	defer cancel()

	res, err := safeCheck(cctx, ck, t, c)
	res.CriterionID = c.ID
	switch {
	case err != nil:
		res.Status = model.StatusError
		res.Reason = errorReason(err)
	case res.Status != model.StatusPass && res.Status != model.StatusFail && res.Status != model.StatusError:
		res.Reason = fmt.Sprintf("checker returned invalid status %q", res.Status)
		res.Status = model.StatusError
	}
	res.Points = 0
	if res.Status == model.StatusPass {
		res.Points = c.Points
	}
	if res.Evidence.ObservedAt.IsZero() {
		res.Evidence.ObservedAt = e.set.clock.Now()
	}
	res.Evidence.ObservedAt = res.Evidence.ObservedAt.UTC()
	if res.Evidence.Duration == 0 {
		res.Evidence.Duration = e.set.clock.Now().Sub(start)
	}
	return res
}

func safeCheck(ctx context.Context, ck Checker, t Target, c model.Criterion) (res model.CheckResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			res, err = model.CheckResult{}, fmt.Errorf("checker panic: %v", r)
		}
	}()
	return ck.Check(ctx, t, c)
}

func errorReason(err error) string {
	switch {
	case errors.Is(err, ErrInvalidCriterion), errors.Is(err, ErrTargetChanged), errors.Is(err, ErrInsufficientEvidence):
		return err.Error()
	default:
		return "grading error: " + err.Error()
	}
}

// mutates reports whether a criterion must run alone.
func mutates(ck Checker, c model.Criterion) bool {
	if v, present := c.Args[argMutates]; present {
		if b, ok := v.(bool); !ok || b {
			return true
		}
	}
	if m, ok := ck.(MutatingChecker); ok {
		return m.Mutates(c)
	}
	return false
}

// planGraph validates the criteria and returns, per criterion index, the
// indexes of its prerequisites.
func planGraph(crits []model.Criterion) ([][]int, error) {
	if len(crits) == 0 {
		return nil, fmt.Errorf("%w: no criteria", ErrInvalidCriteria)
	}
	index := make(map[string]int, len(crits))
	for i, c := range crits {
		if c.ID == "" {
			return nil, fmt.Errorf("%w: criterion %d has no id", ErrInvalidCriteria, i)
		}
		if _, dup := index[c.ID]; dup {
			return nil, fmt.Errorf("%w: duplicate criterion id %q", ErrInvalidCriteria, c.ID)
		}
		index[c.ID] = i
	}
	deps := make([][]int, len(crits))
	for i, c := range crits {
		for _, d := range c.DependsOn {
			j, ok := index[d]
			if !ok {
				return nil, fmt.Errorf("%w: criterion %q depends on unknown %q", ErrInvalidCriteria, c.ID, d)
			}
			if j == i {
				return nil, fmt.Errorf("%w: criterion %q depends on itself", ErrInvalidCriteria, c.ID)
			}
			deps[i] = append(deps[i], j)
		}
	}
	// Depth-first search with colours finds cycles.
	const (
		white = iota
		grey
		black
	)
	colour := make([]int, len(crits))
	var visit func(i int) error
	visit = func(i int) error {
		colour[i] = grey
		for _, j := range deps[i] {
			switch colour[j] {
			case grey:
				return fmt.Errorf("%w: dependency cycle through %q and %q", ErrInvalidCriteria, crits[i].ID, crits[j].ID)
			case white:
				if err := visit(j); err != nil {
					return err
				}
			}
		}
		colour[i] = black
		return nil
	}
	for i := range crits {
		if colour[i] == white {
			if err := visit(i); err != nil {
				return nil, err
			}
		}
	}
	return deps, nil
}

// decodeArgs strictly decodes a criterion's Args into dst: unknown keys and
// wrong types are author errors (ErrInvalidCriterion). The engine-owned
// "mutates" key is accepted everywhere and must be a bool.
func decodeArgs(args map[string]any, dst any) error {
	m := make(map[string]any, len(args))
	for k, v := range args {
		if k == argMutates {
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("%w: args.%s must be a boolean", ErrInvalidCriterion, argMutates)
			}
			continue
		}
		m[k] = v
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("%w: args are not plain data: %v", ErrInvalidCriterion, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: args: %v", ErrInvalidCriterion, err)
	}
	return nil
}

// sortedKeys is a small helper for deterministic output.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
