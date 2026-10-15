package grader

import (
	"context"
	"fmt"
	"strings"

	"github.com/JunYupK/JunKloud/internal/model"
)

// ReadyChecker grades model.CriterionReady: the target workload finished
// rolling out. It observes, until the criterion's timeout (default 30s):
//
//   - status.observedGeneration >= metadata.generation (the controller has
//     seen the current spec; counters of an older generation prove nothing)
//   - the desired replicas are updated, ready and available, and no
//     surplus old replicas remain
//
// If the rollout is not complete when the time is up, the workload's pods
// decide whose fault that is (spec 8.1): a wrong or unpullable image the
// learner chose, an invalid image name, a crash loop, a config error, a
// failing readiness probe or constraints only the pod itself imposes are
// FAIL (the reason names the cause); cluster capacity, node trouble, a pull
// failure of an image the environment provides (WithPreloadedImages), a
// controller that never observed the spec, no pods at all, or any cause the
// pods do not show are ERROR (ErrInsufficientEvidence).
//
// Supported targets: Deployment, StatefulSet, DaemonSet, ReplicaSet and Pod
// (Running and Ready). Any other kind is an author error.
//
// Args (strict):
//
//	replicas: int >= 0   the rollout target the problem declares; spec.replicas
//	                     must equal it (not for DaemonSet and Pod)
type ReadyChecker struct{ observer }

// NewReadyChecker returns the checker for model.CriterionReady.
func NewReadyChecker(reader ObjectReader, opts ...Option) *ReadyChecker {
	return &ReadyChecker{observer{reader: reader, set: newSettings(opts)}}
}

type readyArgs struct {
	Replicas *int64 `json:"replicas"`
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
	spec := observeSpec{
		ref: ref, timeout: timeoutFor(crit, c.set), poll: true, confirm: true,
		eval: func(obj object) verdict { return evalReady(kind, obj, a) },
	}
	obs, err := c.observe(ctx, spec)
	if err != nil {
		return model.CheckResult{}, err
	}
	res := c.result(obs, spec)
	if !obs.found {
		res.Evidence.Expected = map[string]any{"rolledOut": true}
		res.Evidence.Actual = map[string]any{"exists": false}
		return res, nil
	}
	if obs.v.ok {
		return res, nil
	}
	if obs.v.undecided {
		return model.CheckResult{}, fmt.Errorf("%w: %s", ErrInsufficientEvidence, obs.v.reason)
	}
	if !obs.v.needsPods {
		return res, nil // the object itself proves the learner's fault
	}
	// The rollout did not complete: find out whose fault that is.
	pods, err := ownedPods(ctx, c.reader, kind, obs.obj)
	if err != nil {
		return model.CheckResult{}, err
	}
	cause := classifyRollout(pods, obs.v.expectPods, c.set.preloaded)
	if cause.class != faultLearner {
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
