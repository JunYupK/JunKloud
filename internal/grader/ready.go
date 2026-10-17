package grader

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
)

// ReadyChecker grades model.CriterionReady: the target workload finished
// rolling out and stays up. It observes, until the criterion's timeout
// (default 30s):
//
//   - status.observedGeneration >= metadata.generation (the controller has
//     seen the current spec; counters of an older generation prove nothing)
//   - the desired replicas are updated, ready and available, and no
//     surplus old replicas remain
//   - then, for the stability window (args.stable_for, default
//     DefaultStableFor, widened to the target's spec.minReadySeconds when
//     that is larger): every pod of the CURRENT revision stays Ready with
//     every container running since at least the window, the same pods and
//     no container restart. A crash-looping container without a readiness
//     probe is Ready for a moment after each restart; counters alone would
//     PASS it by luck.
//
// A container that restarts, or a pod that falls out of Ready, while being
// watched is a crash loop: if the workload is not proven stable by the
// deadline the result is FAIL with that cause. Otherwise, if the rollout is
// not complete when the time is up, the current revision's pods decide whose
// fault that is (spec 8.1): a wrong or unpullable image the learner chose, an
// invalid image name, a crash loop, a config error, a failing readiness probe
// or constraints only the pod itself imposes are FAIL (the reason names the
// cause); cluster capacity, node trouble, a pull failure of an image the
// environment provides (WithPreloadedImages), a controller that never
// observed the spec, no pods at all, ready pods not yet observed stable for
// the whole window, or any cause the pods do not show are ERROR
// (ErrInsufficientEvidence).
//
// Supported targets: Deployment, StatefulSet, DaemonSet, ReplicaSet and Pod
// (Running and Ready). Any other kind is an author error.
//
// Args (strict):
//
//	replicas: int >= 0     the rollout target the problem declares; spec.replicas
//	                       must equal it (not for DaemonSet and Pod)
//	stable_for: duration   the stability window ("15s"); must be shorter than
//	                       the criterion's timeout (as must the default)
type ReadyChecker struct{ observer }

// NewReadyChecker returns the checker for model.CriterionReady.
func NewReadyChecker(reader ObjectReader, opts ...Option) *ReadyChecker {
	return &ReadyChecker{observer{reader: reader, set: newSettings(opts)}}
}

type readyArgs struct {
	Replicas  *int64 `json:"replicas"`
	StableFor any    `json:"stable_for"`

	stableFor time.Duration // parsed stable_for; 0 = not declared
}

// window is the stability window for one observation of the target.
func (a readyArgs) window(obj object) time.Duration {
	if a.stableFor > 0 {
		return a.stableFor
	}
	d := DefaultStableFor
	if s, ok := intField(obj, "spec", "minReadySeconds"); ok && time.Duration(s)*time.Second > d {
		d = time.Duration(s) * time.Second
	}
	return d
}

func readyKind(ref model.ObjectRef) (string, bool) {
	switch {
	case ref.APIVersion == "apps/v1" && (ref.Kind == "Deployment" || ref.Kind == "StatefulSet" ||
		ref.Kind == "DaemonSet" || ref.Kind == "ReplicaSet"):
		return ref.Kind, true
	case ref.APIVersion == "v1" && ref.Kind == "Pod":
		return "Pod", true
	}
	return "", false
}

func (c *ReadyChecker) validate(crit model.Criterion) (readyArgs, string, error) {
	var a readyArgs
	if err := decodeArgs(crit.Args, &a); err != nil {
		return a, "", err
	}
	if err := checkRef(crit.Target); err != nil {
		return a, "", err
	}
	kind, ok := readyKind(crit.Target)
	if !ok {
		return a, "", fmt.Errorf("%w: ready supports Deployment, StatefulSet, DaemonSet, ReplicaSet and Pod, not %s %s",
			ErrInvalidCriterion, crit.Target.APIVersion, crit.Target.Kind)
	}
	if a.Replicas != nil {
		if *a.Replicas < 0 {
			return a, "", fmt.Errorf("%w: args.replicas must be >= 0", ErrInvalidCriterion)
		}
		if kind == "DaemonSet" || kind == "Pod" {
			return a, "", fmt.Errorf("%w: args.replicas does not apply to a %s", ErrInvalidCriterion, kind)
		}
	}
	window := DefaultStableFor
	if a.StableFor != nil {
		str, isString := a.StableFor.(string)
		d, err := time.ParseDuration(str)
		if !isString || err != nil || d <= 0 {
			return a, "", fmt.Errorf("%w: args.stable_for must be a positive duration such as \"15s\", not %v", ErrInvalidCriterion, a.StableFor)
		}
		a.stableFor, window = d, d
	}
	if timeout := timeoutFor(crit, c.set); window >= timeout {
		return a, "", fmt.Errorf("%w: the stability window %s (args.stable_for, default %s) must be shorter than the criterion timeout %s",
			ErrInvalidCriterion, window, DefaultStableFor, timeout)
	}
	return a, kind, nil
}

// ValidateCriterion implements ArgsValidator.
func (c *ReadyChecker) ValidateCriterion(crit model.Criterion) error {
	_, _, err := c.validate(crit)
	return err
}

// Check implements Checker.
func (c *ReadyChecker) Check(ctx context.Context, t Target, crit model.Criterion) (model.CheckResult, error) {
	a, kind, err := c.validate(crit)
	if err != nil {
		return model.CheckResult{}, err
	}
	ref, err := resolveTarget(t, crit)
	if err != nil {
		return model.CheckResult{}, err
	}
	spec := observeSpec{ref: ref, timeout: timeoutFor(crit, c.set), poll: true}
	clock := c.set.clock
	obs := observation{start: clock.Now()}
	deadline := obs.start.Add(spec.timeout)
	var base *consistencyKey
	track := newStability()
	var pods []object  // the current revision's pods at the last look
	var unknown string // why the current revision could not be determined
	for {
		obj, err := c.get(ctx, ref)
		obs.polls++
		switch {
		case errors.Is(err, ErrNotFound):
			if base != nil {
				return model.CheckResult{}, fmt.Errorf("%w: %s %s no longer exists", ErrTargetChanged, ref.Kind, ref.Name)
			}
			obs.obj, obs.found, obs.now = nil, false, clock.Now()
			obs.v = verdict{reason: fmt.Sprintf("%s %q not found", ref.Kind, ref.Name), terminal: true}
			res := c.result(obs, spec)
			res.Evidence.Expected = map[string]any{"rolledOut": true}
			res.Evidence.Actual = map[string]any{"exists": false}
			return res, nil
		case err != nil:
			return model.CheckResult{}, err
		}
		if k := keyOf(obj); base == nil {
			base = &k
		} else if k != *base {
			return model.CheckResult{}, fmt.Errorf("%w: %s %s was replaced or edited", ErrTargetChanged, ref.Kind, ref.Name)
		}
		obs.obj, obs.found = obj, true
		obs.v = evalReady(kind, obj, a)
		now := clock.Now()
		window := a.window(obj)
		if !obs.v.undecided && !obs.v.terminal {
			if pods, unknown, err = currentPods(ctx, c.reader, kind, obj); err != nil {
				return model.CheckResult{}, err
			}
			switch {
			case unknown != "":
				if obs.v.ok {
					obs.v.ok, obs.v.unstable = false, true
					obs.v.reason = "rollout counters reached, but " + unknown
				}
			default:
				stable, why := track.observe(pods, obs.v.ok, obs.v.wantPods, now, window)
				if obs.v.ok && !stable {
					obs.v.ok, obs.v.unstable = false, true
					obs.v.reason = "rollout counters reached, but " + why
				}
			}
			if exp, ok := obs.v.expected.(map[string]any); ok {
				exp["stableFor"] = window.String()
			}
		}
		obs.now = now
		if obs.v.ok || obs.v.terminal || !now.Before(deadline) {
			break
		}
		wait := c.set.pollInterval
		if remaining := deadline.Sub(now); remaining < wait {
			wait = remaining
		}
		if err := clock.Sleep(ctx, wait); err != nil {
			return model.CheckResult{}, err
		}
	}

	res := c.result(obs, spec)
	switch {
	case obs.v.ok:
		return res, nil
	case track.crash != nil:
		// Proven while watching: a container restarted or a pod fell out of
		// Ready, and the workload was not stable again by the deadline.
		res.Reason += "; cause: pod " + track.crash.Pod + ": " + track.crash.Reason
		if act, ok := res.Evidence.Actual.(map[string]any); ok {
			act["pods"] = rolloutCause{findings: []podFinding{*track.crash}}.summary()
		}
		return res, nil
	case obs.v.undecided:
		return model.CheckResult{}, fmt.Errorf("%w: %s", ErrInsufficientEvidence, obs.v.reason)
	case !obs.v.needsPods && !obs.v.unstable:
		return res, nil // the object itself proves the learner's fault
	}
	// The rollout did not complete (or not stably): find out whose fault
	// that is, from the current revision's pods only.
	if pods == nil && unknown == "" {
		if pods, unknown, err = currentPods(ctx, c.reader, kind, obs.obj); err != nil {
			return model.CheckResult{}, err
		}
	}
	if unknown != "" {
		return model.CheckResult{}, fmt.Errorf("%w: %s; %s", ErrInsufficientEvidence, obs.v.reason, unknown)
	}
	msgs, err := pullMessages(ctx, c.reader, pods)
	if err != nil {
		return model.CheckResult{}, err
	}
	cause := classifyRollout(pods, obs.v.expectPods || obs.v.unstable && obs.v.wantPods > 0, causeConfig{
		preloaded: c.set.preloaded, configured: c.set.preloadedSet, pullMessages: msgs})
	if cause.class != faultLearner {
		if obs.v.unstable && len(cause.findings) == 0 {
			return model.CheckResult{}, fmt.Errorf("%w: %s (the pods are Ready but were not observed stable for the whole window before the limit)", ErrInsufficientEvidence, res.Reason)
		}
		return model.CheckResult{}, fmt.Errorf("%w: %s; %s", ErrInsufficientEvidence, obs.v.reason, cause.text)
	}
	res.Reason += "; cause: " + cause.text
	if act, ok := res.Evidence.Actual.(map[string]any); ok {
		act["pods"] = cause.summary()
	}
	return res, nil
}

func intField(obj map[string]any, path ...string) (int64, bool) {
	var cur any = obj
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return 0, false
		}
		if cur, ok = m[p]; !ok {
			return 0, false
		}
	}
	r, ok := ratOf(cur)
	if !ok || !r.IsInt() {
		return 0, false
	}
	return r.Num().Int64(), true
}

func intOr(obj map[string]any, def int64, path ...string) int64 {
	if v, ok := intField(obj, path...); ok {
		return v
	}
	return def
}

// evalReady judges one observation of a workload.
func evalReady(kind string, obj object, a readyArgs) verdict {
	o := obj
	if kind == "Pod" {
		return evalPod(o)
	}
	generation := generationOf(obj)
	observed := intOr(o, 0, "status", "observedGeneration")
	actual := map[string]any{"generation": generation, "observedGeneration": observed}
	expected := map[string]any{"observedGeneration": fmt.Sprintf(">= %d", generation)}
	v := verdict{expected: expected, actual: actual}

	if observed < generation {
		v.reason = fmt.Sprintf("status.observedGeneration %d is behind metadata.generation %d: the controller has not seen the current spec", observed, generation)
		v.undecided = true // cluster trouble, not the learner's doing
		return v
	}

	var problems []string
	need := func(name string, got, want int64) {
		actual[name] = got
		expected[name] = want
		if got != want {
			problems = append(problems, fmt.Sprintf("%s %d of %d", name, got, want))
		}
	}
	switch kind {
	case "DaemonSet":
		desired := intOr(o, 0, "status", "desiredNumberScheduled")
		expected["desiredNumberScheduled"] = ">= 1"
		actual["desiredNumberScheduled"] = desired
		if desired == 0 {
			v.reason = "no node is scheduled to run the DaemonSet yet"
			v.needsPods, v.expectPods = true, true
			return v
		}
		need("updatedNumberScheduled", intOr(o, 0, "status", "updatedNumberScheduled"), desired)
		need("numberReady", intOr(o, 0, "status", "numberReady"), desired)
		need("numberAvailable", intOr(o, 0, "status", "numberAvailable"), desired)
		need("numberMisscheduled", intOr(o, 0, "status", "numberMisscheduled"), 0)
		v.wantPods = desired
	default:
		want := intOr(o, 1, "spec", "replicas") // the API defaults an omitted replicas to 1
		if a.Replicas != nil {
			expected["spec.replicas"], actual["spec.replicas"] = *a.Replicas, want
			if want != *a.Replicas {
				v.terminal = true
				v.reason = fmt.Sprintf("spec.replicas is %d, the rollout target is %d", want, *a.Replicas)
				return v
			}
		}
		if kind == "Deployment" {
			spec, _ := o["spec"].(map[string]any)
			if paused, _ := spec["paused"].(bool); paused {
				v.terminal = true
				actual["paused"] = true
				v.reason = "the rollout is paused (spec.paused)"
				return v
			}
			if progressDeadlineExceeded(o) {
				v.terminal, v.needsPods, v.expectPods = true, true, want > 0
				actual["progressing"] = "ProgressDeadlineExceeded"
				v.reason = "the Deployment exceeded its progress deadline (Progressing=False, ProgressDeadlineExceeded)"
				return v
			}
		}
		v.wantPods = want
		need("replicas", intOr(o, 0, "status", "replicas"), want)
		if kind != "ReplicaSet" {
			need("updatedReplicas", intOr(o, 0, "status", "updatedReplicas"), want)
		}
		need("readyReplicas", intOr(o, 0, "status", "readyReplicas"), want)
		need("availableReplicas", intOr(o, 0, "status", "availableReplicas"), want)
	}
	if len(problems) > 0 {
		v.reason = "rollout incomplete: " + strings.Join(problems, ", ")
		v.needsPods = true
		v.expectPods = kind == "DaemonSet" || intOr(o, 1, "spec", "replicas") > 0
		return v
	}
	v.ok = true
	return v
}

func progressDeadlineExceeded(o map[string]any) bool {
	status, _ := o["status"].(map[string]any)
	conds, _ := status["conditions"].([]any)
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == "Progressing" && m["status"] == "False" && m["reason"] == "ProgressDeadlineExceeded" {
			return true
		}
	}
	return false
}

func evalPod(o map[string]any) verdict {
	status, _ := o["status"].(map[string]any)
	phase, _ := status["phase"].(string)
	ready := false
	conds, _ := status["conditions"].([]any)
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == "Ready" && m["status"] == "True" {
			ready = true
		}
	}
	v := verdict{
		expected: map[string]any{"phase": "Running", "ready": true},
		actual:   map[string]any{"phase": phase, "ready": ready},
		wantPods: 1,
	}
	switch {
	case phase == "Succeeded":
		v.terminal = true
		v.reason = "pod ran to completion (phase Succeeded) instead of staying up"
	case phase != "Running":
		v.reason = fmt.Sprintf("pod phase is %q, not Running", phase)
		v.needsPods, v.expectPods = true, true
		v.terminal = phase == "Failed"
	case !ready:
		v.reason = "pod is Running but its Ready condition is not True"
		v.needsPods, v.expectPods = true, true
	default:
		v.ok = true
	}
	return v
}
