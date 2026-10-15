package grader

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/JunYupK/JunKloud/internal/model"
)

// apiAttempts is how often one read is tried before an API failure is
// reported (as ERROR).
const apiAttempts = 3

// verdict is a checker's judgement of one observation of the target.
type verdict struct {
	ok       bool
	terminal bool // waiting longer cannot change a not-ok verdict
	reason   string
	expected any
	actual   any
}

type observeSpec struct {
	ref     model.ObjectRef
	timeout time.Duration
	// poll keeps observing until the verdict is ok, terminal or the timeout
	// ends. Without it the target is observed exactly once.
	poll bool
	// waitForCreate also polls while the target does not exist yet.
	waitForCreate bool
	// confirm re-reads the target after an ok verdict and requires the same
	// object and a still-ok verdict before PASSing.
	confirm bool
	eval    func(*unstructured.Unstructured) verdict
}

type observation struct {
	obj   *unstructured.Unstructured // last observed object; nil if not found
	found bool
	v     verdict
	start time.Time
	now   time.Time
	polls int
}

// consistencyKey identifies "the same object, graded against the same
// spec": UID and generation, plus the content hash for kinds without a spec
// (ConfigMap, Secret, ...) that have no generation to watch.
type consistencyKey struct {
	uid        string
	generation int64
	hash       string
}

func keyOf(obj *unstructured.Unstructured) consistencyKey {
	k := consistencyKey{uid: string(obj.GetUID()), generation: obj.GetGeneration()}
	if _, hasSpec := obj.Object["spec"]; !hasSpec {
		k.hash = InputHash(obj)
	}
	return k
}

type observer struct {
	reader ObjectReader
	set    settings
}

// get reads the target, retrying API failures. ErrNotFound and author errors
// are returned at once.
func (o observer) get(ctx context.Context, ref model.ObjectRef) (*unstructured.Unstructured, error) {
	var last error
	for attempt := 0; attempt < apiAttempts; attempt++ {
		obj, err := o.reader.Get(ctx, ref)
		if err == nil {
			o.set.redactor.LearnObject(obj)
			return obj, nil
		}
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalidCriterion) || ctx.Err() != nil {
			return nil, err
		}
		last = err
		if attempt < apiAttempts-1 {
			if err := o.set.clock.Sleep(ctx, o.set.pollInterval); err != nil {
				return nil, err
			}
		}
	}
	return nil, last
}

// observe reads the target, judges it with spec.eval and, if asked to,
// keeps doing so until the verdict is ok or the time is up. A target that is
// replaced, edited or deleted while being observed yields ErrTargetChanged.
func (o observer) observe(ctx context.Context, spec observeSpec) (observation, error) {
	clock := o.set.clock
	obs := observation{start: clock.Now()}
	deadline := obs.start.Add(spec.timeout)
	var base *consistencyKey
	for {
		obj, err := o.get(ctx, spec.ref)
		obs.polls++
		switch {
		case errors.Is(err, ErrNotFound):
			if base != nil {
				return obs, fmt.Errorf("%w: %s %s no longer exists", ErrTargetChanged, spec.ref.Kind, spec.ref.Name)
			}
			obs.obj, obs.found = nil, false
			obs.v = verdict{reason: fmt.Sprintf("%s %q not found", spec.ref.Kind, spec.ref.Name), terminal: !spec.waitForCreate}
		case err != nil:
			return obs, err
		default:
			k := keyOf(obj)
			if base == nil {
				base = &k
			} else if k != *base {
				return obs, fmt.Errorf("%w: %s %s was replaced or edited", ErrTargetChanged, spec.ref.Kind, spec.ref.Name)
			}
			obs.obj, obs.found = obj, true
			obs.v = spec.eval(obj)
			if obs.v.ok && spec.confirm {
				again, err := o.get(ctx, spec.ref)
				if err != nil {
					if errors.Is(err, ErrNotFound) {
						return obs, fmt.Errorf("%w: %s %s no longer exists", ErrTargetChanged, spec.ref.Kind, spec.ref.Name)
					}
					return obs, err
				}
				if keyOf(again) != *base {
					return obs, fmt.Errorf("%w: %s %s was replaced or edited", ErrTargetChanged, spec.ref.Kind, spec.ref.Name)
				}
				obs.obj = again
				obs.v = spec.eval(again)
			}
		}
		obs.now = clock.Now()
		if obs.v.ok || obs.v.terminal || !spec.poll || !obs.now.Before(deadline) {
			return obs, nil
		}
		wait := o.set.pollInterval
		if remaining := deadline.Sub(obs.now); remaining < wait {
			wait = remaining
		}
		if err := clock.Sleep(ctx, wait); err != nil {
			return obs, err
		}
	}
}

// result turns an observation into a PASS or FAIL CheckResult.
func (o observer) result(obs observation, spec observeSpec) model.CheckResult {
	ev := baseEvidence(obs.now, obs.start, obs.obj)
	ev.Expected = obs.v.expected
	ev.Actual = obs.v.actual
	res := model.CheckResult{Status: model.StatusPass, Evidence: ev}
	if !obs.v.ok {
		res.Status = model.StatusFail
		res.Reason = obs.v.reason
		if spec.poll && obs.found && !obs.v.terminal {
			res.Reason = fmt.Sprintf("not satisfied within %s: %s", spec.timeout, obs.v.reason)
		}
	}
	return res
}

// timeoutFor is the criterion's convergence limit.
func timeoutFor(c model.Criterion, set settings) time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return set.defaultTimeout
}

// resolveTarget validates a criterion's target reference and defaults the
// namespace to the problem's.
func resolveTarget(t Target, c model.Criterion) (model.ObjectRef, error) {
	ref := c.Target
	if err := checkRef(ref); err != nil {
		return ref, err
	}
	if ref.Namespace == "" {
		ref.Namespace = t.Problem.Namespace
		if ref.Namespace == "" {
			ref.Namespace = t.Problem.Definition.Namespace
		}
	}
	return ref, nil
}

func checkRef(ref model.ObjectRef) error {
	if ref.APIVersion == "" || ref.Kind == "" || ref.Name == "" {
		return fmt.Errorf("%w: target needs api_version, kind and name", ErrInvalidCriterion)
	}
	return nil
}
