package grader

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Why is a workload not ready? Spec 8.1 splits the answer: the learner's
// choices (wrong image, crashing process, failing probe, impossible
// scheduling constraints) are FAIL; trouble of the managed environment
// (cluster capacity, a node that is not ready, a pull failure of an image the
// environment itself provides) is ERROR; and when the pods do not show the
// cause, it is ERROR too, never a guessed FAIL.

type faultClass int

const (
	faultUnknown     faultClass = iota // cause not provable: ERROR
	faultEnvironment                   // managed environment trouble: ERROR
	faultLearner                       // the learner's answer: FAIL
)

type podFinding struct {
	Pod    string `json:"pod"`
	Phase  string `json:"phase,omitempty"`
	Class  string `json:"class"`
	Reason string `json:"reason"`

	class faultClass
}

func (c faultClass) String() string {
	switch c {
	case faultLearner:
		return "answer"
	case faultEnvironment:
		return "environment"
	}
	return "unknown"
}

// rolloutCause is the verdict on a rollout that did not complete.
type rolloutCause struct {
	class    faultClass
	text     string
	findings []podFinding
}

// summary is the evidence form of the findings (bounded).
func (r rolloutCause) summary() []any {
	out := []any{}
	for i, f := range r.findings {
		if i == 5 {
			out = append(out, fmt.Sprintf("... and %d more pods", len(r.findings)-5))
			break
		}
		out = append(out, map[string]any{"pod": f.Pod, "phase": f.Phase, "class": f.Class, "reason": f.Reason})
	}
	return out
}

// classifyRollout decides whose fault an incomplete rollout is from the pods
// the workload owns. expectPods: the workload should have pods by now.
func classifyRollout(pods []object, expectPods bool, preloaded []string) rolloutCause {
	var findings []podFinding
	for _, p := range pods {
		if metaOf(p)["deletionTimestamp"] != nil || podReady(p) {
			continue
		}
		findings = append(findings, classifyPod(p, preloaded))
	}
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].Pod < findings[j].Pod })

	switch {
	case len(pods) == 0 && expectPods:
		return rolloutCause{class: faultUnknown, text: "the controller created no pods (quota, admission or controller trouble is possible)"}
	case len(findings) == 0:
		return rolloutCause{class: faultUnknown, text: "every pod is ready yet the rollout counters were not reached"}
	}
	// A learner-caused pod problem is conclusive on its own: the workload
	// cannot become ready whatever else is wrong. Otherwise any unproven pod
	// keeps the whole result undecided.
	for _, want := range []faultClass{faultLearner, faultEnvironment, faultUnknown} {
		var texts []string
		for _, f := range findings {
			if f.class == want {
				texts = append(texts, fmt.Sprintf("pod %s: %s", f.Pod, f.Reason))
			}
		}
		if len(texts) > 0 {
			if len(texts) > 3 {
				texts = append(texts[:3], fmt.Sprintf("and %d more", len(texts)-3))
			}
			return rolloutCause{class: want, text: strings.Join(texts, "; "), findings: findings}
		}
	}
	return rolloutCause{class: faultUnknown, text: "no cause found", findings: findings}
}

func podReady(p object) bool {
	for _, c := range listAt(p, "status", "conditions") {
		m, _ := c.(map[string]any)
		if m["type"] == "Ready" && m["status"] == "True" {
			return true
		}
	}
	return false
}

func finding(p object, class faultClass, format string, args ...any) podFinding {
	return podFinding{
		Pod: nameOf(p), Phase: stringAt(p, "status", "phase"), class: class,
		Class: class.String(), Reason: fmt.Sprintf(format, args...),
	}
}

// classifyPod explains why one not-ready, not-terminating pod is not ready.
func classifyPod(p object, preloaded []string) podFinding {
	status := mapAt(p, "status")
	var unprovenStart string
	for _, group := range []string{"initContainerStatuses", "containerStatuses"} {
		for _, cs := range listAt(status, group) {
			st, _ := cs.(map[string]any)
			name, _ := st["name"].(string)
			if ready, _ := st["ready"].(bool); ready && group == "containerStatuses" {
				continue
			}
			if waiting := mapAt(st, "state", "waiting"); waiting != nil {
				reason, _ := waiting["reason"].(string)
				msg, _ := waiting["message"].(string)
				switch reason {
				case "ErrImagePull", "ImagePullBackOff", "ErrImageNeverPull":
					image := containerImage(p, name, st)
					if isPreloaded(image, preloaded) {
						return finding(p, faultEnvironment, "container %s: %s for image %q, which the environment itself provides (image preparation failed): %s", name, reason, image, shorten(msg))
					}
					return finding(p, faultLearner, "container %s: %s, image %q cannot be pulled: %s", name, reason, image, shorten(msg))
				case "InvalidImageName":
					return finding(p, faultLearner, "container %s: InvalidImageName for image %q: %s", name, containerImage(p, name, st), shorten(msg))
				case "CrashLoopBackOff":
					return finding(p, faultLearner, "container %s: CrashLoopBackOff%s", name, lastExit(st))
				case "CreateContainerConfigError":
					return finding(p, faultLearner, "container %s: CreateContainerConfigError: %s", name, shorten(msg))
				case "ContainerCreating", "PodInitializing", "":
					unprovenStart = fmt.Sprintf("container %s is still %s", name, orDefault(reason, "waiting"))
				default:
					unprovenStart = fmt.Sprintf("container %s is waiting: %s %s", name, reason, shorten(msg))
				}
				continue
			}
			if term := mapAt(st, "state", "terminated"); term != nil {
				code, _ := ratOf(term["exitCode"])
				reason, _ := term["reason"].(string)
				if code != nil && code.Sign() != 0 {
					return finding(p, faultLearner, "container %s terminated with exit code %s (%s)", name, code.Num().String(), orDefault(reason, "Error"))
				}
				if group == "containerStatuses" {
					return finding(p, faultLearner, "container %s exited (%s) instead of staying up", name, orDefault(reason, "Completed"))
				}
				continue
			}
			if mapAt(st, "state", "running") != nil && group == "containerStatuses" {
				return finding(p, faultLearner, "container %s is running but not ready: its readiness probe is failing or has not passed yet", name)
			}
		}
	}
	if unprovenStart != "" {
		return finding(p, faultUnknown, "%s", unprovenStart)
	}
	if reason, _ := status["reason"].(string); reason == "Evicted" {
		return finding(p, faultEnvironment, "pod was evicted by the node: %s", shorten(fmt.Sprint(status["message"])))
	}
	for _, c := range listAt(status, "conditions") {
		m, _ := c.(map[string]any)
		if m["type"] == "PodScheduled" && m["status"] == "False" {
			msg, _ := m["message"].(string)
			if learnerSchedulingProblem(msg) {
				return finding(p, faultLearner, "unschedulable because of the pod's own constraints: %s", shorten(msg))
			}
			return finding(p, faultEnvironment, "unschedulable because of cluster capacity or node state, not the answer: %s", shorten(msg))
		}
	}
	return finding(p, faultUnknown, "phase %s without a container state that explains it", orDefault(stringAt(p, "status", "phase"), "unknown"))
}

// learnerSchedulingProblem reports whether a scheduler message blames the
// pod's own constraints. Any resource or node-state reason wins: those are
// the environment's.
func learnerSchedulingProblem(msg string) bool {
	m := strings.ToLower(msg)
	for _, env := range []string{"insufficient", "not-ready", "unreachable", "pressure", "unschedulable", "untolerated taint"} {
		if strings.Contains(m, env) {
			return false
		}
	}
	return strings.Contains(m, "didn't match pod's node affinity") ||
		strings.Contains(m, "didn't match pod's node selector") ||
		strings.Contains(m, "persistentvolumeclaim")
}

func lastExit(st map[string]any) string {
	term := mapAt(st, "lastState", "terminated")
	if term == nil {
		return ""
	}
	code, _ := ratOf(term["exitCode"])
	if code == nil {
		return ""
	}
	return fmt.Sprintf(" (last exit code %s)", code.Num().String())
}

// containerImage is the image the learner's pod spec names for a container
// (falling back to the status's resolved name).
func containerImage(p object, name string, st map[string]any) string {
	for _, group := range []string{"initContainers", "containers", "ephemeralContainers"} {
		for _, c := range listAt(p, "spec", group) {
			m, _ := c.(map[string]any)
			if m["name"] == name {
				if img, _ := m["image"].(string); img != "" {
					return img
				}
			}
		}
	}
	img, _ := st["image"].(string)
	return img
}

func isPreloaded(image string, preloaded []string) bool {
	if image == "" {
		return false
	}
	n := normalizeImage(image)
	for _, p := range preloaded {
		if normalizeImage(p) == n {
			return true
		}
	}
	return false
}

func shorten(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// selectorString renders a workload's spec.selector as a label selector.
func selectorString(sel map[string]any) string {
	var parts []string
	labels, _ := sel["matchLabels"].(map[string]any)
	for _, k := range sortedKeys(labels) {
		parts = append(parts, fmt.Sprintf("%s=%v", k, labels[k]))
	}
	exprs, _ := sel["matchExpressions"].([]any)
	for _, e := range exprs {
		m, _ := e.(map[string]any)
		key, _ := m["key"].(string)
		var vals []string
		for _, v := range listAt(m, "values") {
			vals = append(vals, fmt.Sprint(v))
		}
		switch m["operator"] {
		case "In":
			parts = append(parts, fmt.Sprintf("%s in (%s)", key, strings.Join(vals, ",")))
		case "NotIn":
			parts = append(parts, fmt.Sprintf("%s notin (%s)", key, strings.Join(vals, ",")))
		case "Exists":
			parts = append(parts, key)
		case "DoesNotExist":
			parts = append(parts, "!"+key)
		}
	}
	return strings.Join(parts, ",")
}

func controllerUID(o object) string {
	for _, r := range listAt(o, "metadata", "ownerReferences") {
		m, _ := r.(map[string]any)
		if c, _ := m["controller"].(bool); c {
			uid, _ := m["uid"].(string)
			return uid
		}
	}
	return ""
}

// ownedPods lists the pods a workload controls: directly (StatefulSet,
// DaemonSet, ReplicaSet) or through its ReplicaSets (Deployment).
func ownedPods(ctx context.Context, r ObjectReader, kind string, obj object) ([]object, error) {
	if kind == "Pod" {
		return []object{obj}, nil
	}
	ns := stringAt(obj, "metadata", "namespace")
	selector := selectorString(mapAt(obj, "spec", "selector"))
	if selector == "" {
		return nil, fmt.Errorf("%w: %s %s has no usable selector", ErrInsufficientEvidence, kind, nameOf(obj))
	}
	owners := map[string]bool{uidOf(obj): true}
	if kind == "Deployment" {
		owners = map[string]bool{}
		sets, err := r.List(ctx, ListQuery{APIVersion: "apps/v1", Kind: "ReplicaSet", Namespace: ns, LabelSelector: selector})
		if err != nil {
			return nil, err
		}
		for _, rs := range sets {
			if controllerUID(rs) == uidOf(obj) {
				owners[uidOf(rs)] = true
			}
		}
	}
	pods, err := r.List(ctx, ListQuery{APIVersion: "v1", Kind: "Pod", Namespace: ns, LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	var out []object
	for _, p := range pods {
		if owners[controllerUID(p)] {
			out = append(out, p)
		}
	}
	return out, nil
}
