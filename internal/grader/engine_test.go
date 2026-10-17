package grader_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// scripted is a fake Checker: it answers by criterion ID.
type scripted struct {
	mu       sync.Mutex
	results  map[string]string // criterion ID -> status
	errs     map[string]error
	panics   map[string]bool
	mutating map[string]bool
	hold     time.Duration // real-time hold to expose overlap
	called   []string
	seen     map[string]model.Criterion
	running  atomic.Int32
	peak     atomic.Int32
	// overlap: a mutating criterion saw another criterion running.
	overlap atomic.Bool
}

func newScripted(results map[string]string) *scripted {
	return &scripted{results: results, errs: map[string]error{}, panics: map[string]bool{}, mutating: map[string]bool{}, seen: map[string]model.Criterion{}}
}

func (s *scripted) Mutates(c model.Criterion) bool { return s.mutating[c.ID] }

func (s *scripted) Check(ctx context.Context, _ grader.Target, c model.Criterion) (model.CheckResult, error) {
	n := s.running.Add(1)
	defer s.running.Add(-1)
	for {
		p := s.peak.Load()
		if n <= p || s.peak.CompareAndSwap(p, n) {
			break
		}
	}
	if s.mutating[c.ID] && n > 1 {
		s.overlap.Store(true)
	}
	s.mu.Lock()
	s.called = append(s.called, c.ID)
	s.seen[c.ID] = c
	s.mu.Unlock()
	if s.hold > 0 {
		time.Sleep(s.hold)
		if s.mutating[c.ID] && s.running.Load() > 1 {
			s.overlap.Store(true)
		}
	}
	if s.panics[c.ID] {
		panic("checker blew up")
	}
	if err := s.errs[c.ID]; err != nil {
		return model.CheckResult{}, err
	}
	st, ok := s.results[c.ID]
	if !ok {
		return model.CheckResult{}, fmt.Errorf("no script for %s", c.ID)
	}
	return model.CheckResult{CriterionID: c.ID, Status: st, Reason: "scripted " + st}, nil
}

func (s *scripted) wasCalled(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.called {
		if c == id {
			return true
		}
	}
	return false
}

func engineFor(s *scripted, opts ...grader.Option) *grader.Engine {
	checkers := map[string]grader.Checker{}
	for _, k := range model.CriterionKinds() {
		checkers[k] = s
	}
	return grader.NewEngine(checkers, append([]grader.Option{grader.WithClock(newClock())}, opts...)...)
}

// scoring fixture: 2+2+2+4 points.
func scoringCriteria() []model.Criterion {
	return []model.Criterion{
		crit("a", model.CriterionResource, 2, deployRef, nil),
		crit("b", model.CriterionField, 2, deployRef, nil),
		crit("c", model.CriterionField, 2, deployRef, nil),
		crit("d", model.CriterionReady, 4, deployRef, nil),
	}
}

func TestPartialScore(t *testing.T) {
	s := newScripted(map[string]string{"a": "PASS", "b": "FAIL", "c": "FAIL", "d": "PASS"})
	graded, err := engineFor(s).Evaluate(context.Background(), target(scoringCriteria()...))
	testutil.NoError(t, err)
	testutil.Equal(t, *graded.Score, 6)
	testutil.Equal(t, graded.MaxScore, 10)
	testutil.Equal(t, graded.Complete, true)
	testutil.Equal(t, len(graded.Results), 4)
	// Results keep declaration order and award points only for PASS.
	var pts []int
	for i, id := range []string{"a", "b", "c", "d"} {
		testutil.Equal(t, graded.Results[i].CriterionID, id)
		pts = append(pts, graded.Results[i].Points)
	}
	testutil.Equal(t, pts, []int{2, 0, 0, 4})
}

func TestErrorKeepsScorePending(t *testing.T) {
	s := newScripted(map[string]string{"a": "PASS", "b": "FAIL", "c": "ERROR", "d": "PASS"})
	pending, err := engineFor(s).Evaluate(context.Background(), target(scoringCriteria()...))
	testutil.NoError(t, err)
	testutil.Equal(t, pending.Score, (*int)(nil))
	testutil.Equal(t, pending.Complete, false)
	// The ERROR points stay in the denominator and known results are kept.
	testutil.Equal(t, pending.MaxScore, 10)
	testutil.Equal(t, statusOf(pending.Results), map[string]string{"a": "PASS", "b": "FAIL", "c": "ERROR", "d": "PASS"})
	testutil.Equal(t, resultFor(t, pending, "d").Points, 4)
}

func TestCheckerFailuresBecomeError(t *testing.T) {
	s := newScripted(map[string]string{"a": "PASS", "b": "PASS", "c": "PASS", "d": "PASS"})
	s.errs["a"] = errors.New("apiserver unreachable")
	s.errs["b"] = fmt.Errorf("%w: unknown key %q", grader.ErrInvalidCriterion, "typo")
	s.panics["c"] = true
	ev, err := engineFor(s).Evaluate(context.Background(), target(scoringCriteria()...))
	testutil.NoError(t, err)
	testutil.Equal(t, statusOf(ev.Results), map[string]string{"a": "ERROR", "b": "ERROR", "c": "ERROR", "d": "PASS"})
	if !strings.Contains(resultFor(t, ev, "a").Reason, "apiserver unreachable") {
		t.Fatalf("reason lost: %q", resultFor(t, ev, "a").Reason)
	}
	if !strings.Contains(resultFor(t, ev, "b").Reason, "invalid criterion") {
		t.Fatalf("author error must be labelled: %q", resultFor(t, ev, "b").Reason)
	}
	if !strings.Contains(resultFor(t, ev, "c").Reason, "panic") {
		t.Fatalf("panic must be reported: %q", resultFor(t, ev, "c").Reason)
	}
	testutil.Equal(t, ev.Score, (*int)(nil))
	testutil.Equal(t, ev.Complete, false)
}

func TestUnknownKindIsError(t *testing.T) {
	e := grader.NewEngine(map[string]grader.Checker{}, grader.WithClock(newClock()))
	ev, err := e.Evaluate(context.Background(), target(crit("a", model.CriterionHTTP, 3, deployRef, nil)))
	testutil.NoError(t, err)
	testutil.Equal(t, resultFor(t, ev, "a").Status, "ERROR")
	testutil.Equal(t, ev.Score, (*int)(nil))
}

func TestInvalidStatusFromCheckerIsError(t *testing.T) {
	s := newScripted(map[string]string{"a": "MAYBE"})
	ev, err := engineFor(s).Evaluate(context.Background(), target(crit("a", model.CriterionResource, 1, deployRef, nil)))
	testutil.NoError(t, err)
	testutil.Equal(t, resultFor(t, ev, "a").Status, "ERROR")
}

func TestPrerequisiteFailure(t *testing.T) {
	crits := []model.Criterion{
		crit("exists", model.CriterionResource, 1, deployRef, nil),
		crit("image", model.CriterionField, 2, deployRef, nil, "exists"),
		crit("ready", model.CriterionReady, 3, deployRef, nil, "image"),
		crit("other", model.CriterionField, 4, deployRef, nil),
	}
	s := newScripted(map[string]string{"exists": "FAIL", "image": "PASS", "ready": "PASS", "other": "PASS"})
	ev, err := engineFor(s).Evaluate(context.Background(), target(crits...))
	testutil.NoError(t, err)
	testutil.Equal(t, statusOf(ev.Results), map[string]string{"exists": "FAIL", "image": "FAIL", "ready": "FAIL", "other": "PASS"})
	if !strings.HasPrefix(resultFor(t, ev, "image").Reason, "prerequisite not satisfied") {
		t.Fatalf("reason: %q", resultFor(t, ev, "image").Reason)
	}
	if !strings.Contains(resultFor(t, ev, "image").Reason, "exists") {
		t.Fatalf("reason must name the prerequisite: %q", resultFor(t, ev, "image").Reason)
	}
	// Transitive: ready depends on a skipped criterion.
	if !strings.HasPrefix(resultFor(t, ev, "ready").Reason, "prerequisite not satisfied") {
		t.Fatalf("reason: %q", resultFor(t, ev, "ready").Reason)
	}
	// Dependents are never run, and a fully graded run is complete.
	testutil.Equal(t, s.wasCalled("image"), false)
	testutil.Equal(t, s.wasCalled("ready"), false)
	testutil.Equal(t, *ev.Score, 4)
	testutil.Equal(t, ev.Complete, true)
}

func TestPrerequisiteErrorIsNotLearnerFault(t *testing.T) {
	crits := []model.Criterion{
		crit("exists", model.CriterionResource, 1, deployRef, nil),
		crit("image", model.CriterionField, 2, deployRef, nil, "exists"),
	}
	s := newScripted(map[string]string{"exists": "ERROR", "image": "PASS"})
	ev, err := engineFor(s).Evaluate(context.Background(), target(crits...))
	testutil.NoError(t, err)
	testutil.Equal(t, resultFor(t, ev, "image").Status, "ERROR")
	testutil.Equal(t, s.wasCalled("image"), false)
	testutil.Equal(t, ev.Score, (*int)(nil))
	testutil.Equal(t, ev.Complete, false)
}

func TestPrerequisitePassRunsDependent(t *testing.T) {
	crits := []model.Criterion{
		crit("exists", model.CriterionResource, 1, deployRef, nil),
		crit("image", model.CriterionField, 2, deployRef, nil, "exists"),
	}
	s := newScripted(map[string]string{"exists": "PASS", "image": "PASS"})
	ev, err := engineFor(s).Evaluate(context.Background(), target(crits...))
	testutil.NoError(t, err)
	testutil.Equal(t, *ev.Score, 3)
}

func TestInvalidCriteriaGraph(t *testing.T) {
	s := newScripted(map[string]string{})
	e := engineFor(s)
	cases := map[string][]model.Criterion{
		"cycle": {
			crit("a", model.CriterionResource, 1, deployRef, nil, "b"),
			crit("b", model.CriterionResource, 1, deployRef, nil, "a"),
		},
		"self":    {crit("a", model.CriterionResource, 1, deployRef, nil, "a")},
		"unknown": {crit("a", model.CriterionResource, 1, deployRef, nil, "ghost")},
		"duplicate": {
			crit("a", model.CriterionResource, 1, deployRef, nil),
			crit("a", model.CriterionResource, 1, deployRef, nil),
		},
		"empty": {},
	}
	for name, crits := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := e.Evaluate(context.Background(), target(crits...))
			testutil.ErrorIs(t, err, grader.ErrInvalidCriteria)
			testutil.Equal(t, len(s.called), 0)
		})
	}
}

func TestEvidenceIsStamped(t *testing.T) {
	clock := newClock()
	s := newScripted(map[string]string{"a": "PASS"})
	e := grader.NewEngine(map[string]grader.Checker{model.CriterionResource: s}, grader.WithClock(clock))
	ev, err := e.Evaluate(context.Background(), target(crit("a", model.CriterionResource, 1, deployRef, nil)))
	testutil.NoError(t, err)
	got := resultFor(t, ev, "a").Evidence
	testutil.Equal(t, got.ObservedAt.Location().String(), "UTC")
	testutil.Equal(t, got.ObservedAt.Equal(clock.Now()), true)
}

func TestTimeouts(t *testing.T) {
	s := newScripted(map[string]string{"a": "PASS", "b": "PASS"})
	crits := []model.Criterion{
		crit("a", model.CriterionResource, 1, deployRef, nil),
		crit("b", model.CriterionResource, 1, deployRef, nil),
	}
	crits[1].Timeout = 5 * time.Second
	_, err := engineFor(s).Evaluate(context.Background(), target(crits...))
	testutil.NoError(t, err)
	// Spec 8.3: 30s convergence by default; a criterion override wins.
	testutil.Equal(t, s.seen["a"].Timeout, 30*time.Second)
	testutil.Equal(t, s.seen["b"].Timeout, 5*time.Second)

	s2 := newScripted(map[string]string{"a": "PASS"})
	_, err = engineFor(s2, grader.WithDefaultTimeout(7*time.Second)).Evaluate(context.Background(), target(crits[0]))
	testutil.NoError(t, err)
	testutil.Equal(t, s2.seen["a"].Timeout, 7*time.Second)
}

func manyCriteria(n int) ([]model.Criterion, map[string]string) {
	var crits []model.Criterion
	res := map[string]string{}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("c%d", i)
		crits = append(crits, crit(id, model.CriterionResource, 1, deployRef, nil))
		res[id] = "PASS"
	}
	return crits, res
}

func TestParallelismDefaultsToTwo(t *testing.T) {
	crits, res := manyCriteria(8)
	s := newScripted(res)
	s.hold = 20 * time.Millisecond
	_, err := engineFor(s).Evaluate(context.Background(), target(crits...))
	testutil.NoError(t, err)
	testutil.Equal(t, s.peak.Load(), int32(2))

	s = newScripted(res)
	s.hold = 20 * time.Millisecond
	_, err = engineFor(s, grader.WithParallelism(1)).Evaluate(context.Background(), target(crits...))
	testutil.NoError(t, err)
	testutil.Equal(t, s.peak.Load(), int32(1))
}

func TestMutatingChecksRunAlone(t *testing.T) {
	crits, res := manyCriteria(6)
	s := newScripted(res)
	s.hold = 15 * time.Millisecond
	s.mutating["c1"] = true // by checker declaration
	crits[3].Args = map[string]any{"mutates": true}
	s.mutating["c3"] = true
	crits[4].Args = map[string]any{"mutates": true} // by Args only
	_, err := engineFor(s).Evaluate(context.Background(), target(crits...))
	testutil.NoError(t, err)
	testutil.Equal(t, s.overlap.Load(), false)
}

func TestMutatesArgFlagSerializesWithoutCheckerSupport(t *testing.T) {
	crits, res := manyCriteria(6)
	for i := range crits {
		crits[i].Args = map[string]any{"mutates": true}
	}
	s := newScripted(res)
	s.hold = 10 * time.Millisecond
	for id := range res {
		s.mutating[id] = false
	}
	_, err := engineFor(s).Evaluate(context.Background(), target(crits...))
	testutil.NoError(t, err)
	testutil.Equal(t, s.peak.Load(), int32(1))
}

func TestCancelledContextAbortsEvaluation(t *testing.T) {
	crits, res := manyCriteria(3)
	s := newScripted(res)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := engineFor(s).Evaluate(ctx, target(crits...))
	testutil.ErrorIs(t, err, context.Canceled)
}

func TestCheckerPointsAreIgnored(t *testing.T) {
	// The engine owns scoring: a checker cannot award or withhold points.
	liar := checkerFunc(func(_ context.Context, _ grader.Target, c model.Criterion) (model.CheckResult, error) {
		return model.CheckResult{CriterionID: "other-id", Status: model.StatusFail, Points: 99}, nil
	})
	e := grader.NewEngine(map[string]grader.Checker{model.CriterionResource: liar}, grader.WithClock(newClock()))
	ev, err := e.Evaluate(context.Background(), target(crit("a", model.CriterionResource, 3, deployRef, nil)))
	testutil.NoError(t, err)
	r := resultFor(t, ev, "a")
	testutil.Equal(t, r.Points, 0)
	testutil.Equal(t, *ev.Score, 0)
}

type checkerFunc func(context.Context, grader.Target, model.Criterion) (model.CheckResult, error)

func (f checkerFunc) Check(ctx context.Context, t grader.Target, c model.Criterion) (model.CheckResult, error) {
	return f(ctx, t, c)
}

func TestValidateCriteria(t *testing.T) {
	rc := grader.NewResourceChecker(newReader())
	e := grader.NewEngine(map[string]grader.Checker{model.CriterionResource: rc})
	good := crit("a", model.CriterionResource, 1, deployRef, map[string]any{"wait": true})
	bad := crit("b", model.CriterionResource, 1, deployRef, map[string]any{"waiit": true})
	testutil.NoError(t, e.Validate([]model.Criterion{good}))
	testutil.ErrorIs(t, e.Validate([]model.Criterion{good, bad}), grader.ErrInvalidCriterion)
	unknown := crit("c", model.CriterionHelm, 1, deployRef, nil)
	testutil.ErrorIs(t, e.Validate([]model.Criterion{unknown}), grader.ErrInvalidCriterion)
}
