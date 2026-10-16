package grader

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// DefaultStableFor is how long the ready checker watches a workload whose
// rollout counters are reached before it PASSes: every pod of the current
// revision must stay Ready, keep its containers running and not restart for
// this long (spec 8.1: a crash-looping container is briefly Ready after each
// restart, so one ready observation proves nothing).
const DefaultStableFor = 10 * time.Second

const (
	annotationDeploymentRevision = "deployment.kubernetes.io/revision"
	labelPodTemplateHash         = "pod-template-hash"
	labelControllerRevisionHash  = "controller-revision-hash"
	labelPodTemplateGeneration   = "pod-template-generation"
)

// currentPods lists the pods of the workload's CURRENT revision: for a
// Deployment the pods of its ReplicaSet of the Deployment's revision, for a
// StatefulSet the pods of status.updateRevision, for a DaemonSet the pods of
// the current template generation or ControllerRevision, for a ReplicaSet
// its pods and for a Pod the pod itself. Pods of older revisions say nothing
// about the answer being graded.
//
// unknown is non-empty when the current revision cannot be determined (the
// controller has not published it yet): nothing can be judged from the pods
// then.
func currentPods(ctx context.Context, r ObjectReader, kind string, obj object) (pods []object, unknown string, err error) {
	if kind == "Pod" {
		return []object{obj}, "", nil
	}
	ns := stringAt(obj, "metadata", "namespace")
	selector := selectorString(mapAt(obj, "spec", "selector"))
	if selector == "" {
		return nil, "", fmt.Errorf("%w: %s %s has no usable selector", ErrInsufficientEvidence, kind, nameOf(obj))
	}
	list := func() ([]object, error) {
		return r.List(ctx, ListQuery{APIVersion: "v1", Kind: "Pod", Namespace: ns, LabelSelector: selector})
	}
	switch kind {
	case "Deployment":
		rsUID, unknown, err := currentReplicaSet(ctx, r, obj, ns, selector)
		if err != nil || unknown != "" {
			return nil, unknown, err
		}
		all, err := list()
		if err != nil {
			return nil, "", err
		}
		return controlledBy(all, rsUID), "", nil
	case "StatefulSet":
		rev := stringAt(obj, "status", "updateRevision")
		if rev == "" {
			return nil, "the StatefulSet has not published status.updateRevision yet", nil
		}
		all, err := list()
		if err != nil {
			return nil, "", err
		}
		var out []object
		for _, p := range controlledBy(all, uidOf(obj)) {
			if labelOf(p, labelControllerRevisionHash) == rev {
				out = append(out, p)
			}
		}
		return out, "", nil
	case "DaemonSet":
		all, err := list()
		if err != nil {
			return nil, "", err
		}
		owned := controlledBy(all, uidOf(obj))
		gen := fmt.Sprint(generationOf(obj))
		hash := ""
		needHash := false
		for _, p := range owned {
			if labelOf(p, labelPodTemplateGeneration) != gen {
				needHash = true
			}
		}
		if needHash {
			if hash, err = currentDaemonSetRevision(ctx, r, obj, ns, selector); err != nil {
				return nil, "", err
			}
		}
		var out []object
		for _, p := range owned {
			if labelOf(p, labelPodTemplateGeneration) == gen || (hash != "" && labelOf(p, labelControllerRevisionHash) == hash) {
				out = append(out, p)
			}
		}
		return out, "", nil
	default: // ReplicaSet
		all, err := list()
		if err != nil {
			return nil, "", err
		}
		return controlledBy(all, uidOf(obj)), "", nil
	}
}

// currentReplicaSet finds the Deployment's ReplicaSet of its current
// revision: the owned ReplicaSet carrying the Deployment's revision
// annotation, or (when the Deployment has none yet) the owned ReplicaSet whose
// pod template equals the Deployment's apart from pod-template-hash, the way
// the deployment controller itself matches them.
func currentReplicaSet(ctx context.Context, r ObjectReader, d object, ns, selector string) (string, string, error) {
	sets, err := r.List(ctx, ListQuery{APIVersion: "apps/v1", Kind: "ReplicaSet", Namespace: ns, LabelSelector: selector})
	if err != nil {
		return "", "", err
	}
	owned := controlledBy(sets, uidOf(d))
	if rev := annotationOf(d, annotationDeploymentRevision); rev != "" {
		for _, rs := range owned {
			if annotationOf(rs, annotationDeploymentRevision) == rev {
				return uidOf(rs), "", nil
			}
		}
	}
	want := templateWithoutHash(mapAt(d, "spec", "template"))
	for _, rs := range owned {
		if reflect.DeepEqual(templateWithoutHash(mapAt(rs, "spec", "template")), want) {
			return uidOf(rs), "", nil
		}
	}
	return "", fmt.Sprintf("the ReplicaSet of the Deployment's current revision %q was not found", annotationOf(d, annotationDeploymentRevision)), nil
}

func templateWithoutHash(t map[string]any) map[string]any {
	if t == nil {
		return nil
	}
	out := make(map[string]any, len(t))
	for k, v := range t {
		out[k] = v
	}
	md, _ := t["metadata"].(map[string]any)
	if md == nil {
		return out
	}
	md2 := make(map[string]any, len(md))
	for k, v := range md {
		md2[k] = v
	}
	if labels, _ := md["labels"].(map[string]any); labels != nil {
		l2 := make(map[string]any, len(labels))
		for k, v := range labels {
			if k != labelPodTemplateHash {
				l2[k] = v
			}
		}
		md2["labels"] = l2
	}
	out["metadata"] = md2
	return out
}

// currentDaemonSetRevision is the controller-revision-hash of the
// DaemonSet's newest ControllerRevision ("" if it has none yet).
func currentDaemonSetRevision(ctx context.Context, r ObjectReader, ds object, ns, selector string) (string, error) {
	revs, err := r.List(ctx, ListQuery{APIVersion: "apps/v1", Kind: "ControllerRevision", Namespace: ns, LabelSelector: selector})
	if err != nil {
		return "", err
	}
	var best object
	var bestRev int64 = -1
	for _, cr := range controlledBy(revs, uidOf(ds)) {
		if n := intOr(cr, -1, "revision"); n > bestRev {
			best, bestRev = cr, n
		}
	}
	if best == nil {
		return "", nil
	}
	if h := labelOf(best, labelControllerRevisionHash); h != "" {
		return h, nil
	}
	return strings.TrimPrefix(nameOf(best), nameOf(ds)+"-"), nil
}

func controlledBy(objs []object, uid string) []object {
	var out []object
	for _, o := range objs {
		if uid != "" && controllerUID(o) == uid {
			out = append(out, o)
		}
	}
	return out
}

func labelOf(o object, key string) string {
	v, _ := mapAt(o, "metadata", "labels")[key].(string)
	return v
}

func annotationOf(o object, key string) string {
	v, _ := mapAt(o, "metadata", "annotations")[key].(string)
	return v
}

// podActive mirrors the controllers' notion of an active pod: not
// terminating and not finished.
func podActive(p object) bool {
	if metaOf(p)["deletionTimestamp"] != nil {
		return false
	}
	switch stringAt(p, "status", "phase") {
	case "Succeeded", "Failed":
		return false
	}
	return true
}

// containerStatuses returns every container status of a pod (init
// containers first), each with whether it is an init container.
func containerStatuses(p object) (out []containerStatus) {
	for _, group := range []string{"initContainerStatuses", "containerStatuses"} {
		for _, cs := range listAt(p, "status", group) {
			st, _ := cs.(map[string]any)
			if st == nil {
				continue
			}
			name, _ := st["name"].(string)
			out = append(out, containerStatus{name: name, init: group == "initContainerStatuses", st: st})
		}
	}
	return out
}

type containerStatus struct {
	name string
	init bool
	st   map[string]any
}

// unstableReason says why a Ready pod cannot count as stable at now ("" if
// it can): every regular container must be ready and running since at least
// stableFor; an init container must have completed successfully or (a
// sidecar) be running since at least stableFor.
func unstableReason(p object, now time.Time, stableFor time.Duration) string {
	if !podReady(p) {
		return fmt.Sprintf("pod %s is not Ready", nameOf(p))
	}
	for _, c := range containerStatuses(p) {
		st := c.st
		if w := mapAt(st, "state", "waiting"); w != nil {
			reason, _ := w["reason"].(string)
			return fmt.Sprintf("container %s of pod %s is waiting (%s)", c.name, nameOf(p), orDefault(reason, "no reason"))
		}
		if term := mapAt(st, "state", "terminated"); term != nil {
			code, _ := ratOf(term["exitCode"])
			if c.init && code != nil && code.Sign() == 0 {
				continue // a finished init container
			}
			return fmt.Sprintf("container %s of pod %s terminated", c.name, nameOf(p))
		}
		run := mapAt(st, "state", "running")
		if run == nil {
			return fmt.Sprintf("container %s of pod %s has no running state", c.name, nameOf(p))
		}
		if ready, _ := st["ready"].(bool); !ready && !c.init {
			return fmt.Sprintf("container %s of pod %s is not ready", c.name, nameOf(p))
		}
		started, err := time.Parse(time.RFC3339, fmt.Sprint(run["startedAt"]))
		if err != nil {
			return fmt.Sprintf("container %s of pod %s has no start time", c.name, nameOf(p))
		}
		if age := now.Sub(started); age < stableFor {
			return fmt.Sprintf("container %s of pod %s started %s ago, less than the %s stability window", c.name, nameOf(p), age.Truncate(time.Second), stableFor)
		}
	}
	return ""
}

// stability follows the current revision's pods across the observations of
// one ready check. It proves two things a single look cannot: that the
// workload stayed Ready with the same pods and without any container restart
// for the whole window (PASS), or that a container restarted or a pod fell
// out of Ready while being watched (a crash loop: the learner's FAIL).
type stability struct {
	restarts      map[string]int64 // podUID/container -> last restartCount seen
	firstRestarts map[string]int64 // podUID/container -> first restartCount seen
	wasReady      map[string]bool  // podUID -> seen Ready before
	crash         *podFinding      // the latest proven instability
	window        *stableWindow
}

type stableWindow struct {
	start time.Time
	uids  string
}

func newStability() *stability {
	return &stability{restarts: map[string]int64{}, firstRestarts: map[string]int64{}, wasReady: map[string]bool{}}
}

// observe records one listing of the current pods. countersOK says the
// workload's own status counters are reached; want is how many ready pods
// that requires. It reports whether the stability window is complete
// (PASS) and, if not, why.
func (s *stability) observe(pods []object, countersOK bool, want int64, now time.Time, stableFor time.Duration) (bool, string) {
	var active []object
	for _, p := range pods {
		if podActive(p) {
			active = append(active, p)
		}
	}
	sort.SliceStable(active, func(i, j int) bool { return nameOf(active[i]) < nameOf(active[j]) })
	for _, p := range active {
		uid := uidOf(p)
		for _, c := range containerStatuses(p) {
			k := uid + "/" + c.name
			n := intOr(c.st, 0, "restartCount")
			if _, seen := s.firstRestarts[k]; !seen {
				s.firstRestarts[k] = n
			} else if prev := s.restarts[k]; n > prev {
				f := finding(p, faultLearner, "container %s restarted %d times during the observation (crash loop%s)",
					c.name, n-s.firstRestarts[k], exitSuffix(c.st))
				s.crash, s.window = &f, nil
			}
			s.restarts[k] = n
		}
		ready := podReady(p)
		if s.wasReady[uid] && !ready {
			// Only a container-level cause is the answer's doing; a pod
			// marked not Ready with every container ready (node trouble)
			// merely restarts the window.
			if detail := podNotReadyDetail(p); detail != "" {
				f := finding(p, faultLearner, "pod left Ready during the observation (%s)", detail)
				s.crash = &f
			}
			s.window = nil
		}
		if ready {
			s.wasReady[uid] = true
		}
	}

	if !countersOK {
		s.window = nil
		return false, ""
	}
	var ready int64
	var uids []string
	for _, p := range active {
		uids = append(uids, uidOf(p))
		if podReady(p) {
			ready++
		}
	}
	if ready < want || ready < int64(len(active)) {
		s.window = nil
		return false, fmt.Sprintf("%d of %d current pods are Ready (need %d)", ready, len(active), want)
	}
	set := strings.Join(uids, ",")
	if s.window == nil || s.window.uids != set {
		s.window = &stableWindow{start: now, uids: set}
	}
	for _, p := range active {
		if why := unstableReason(p, now, stableFor); why != "" {
			return false, "not yet stable: " + why
		}
	}
	if elapsed := now.Sub(s.window.start); elapsed < stableFor {
		return false, fmt.Sprintf("not yet stable: observed ready for %s of the %s stability window", elapsed.Truncate(time.Second), stableFor)
	}
	return true, ""
}

func exitSuffix(st map[string]any) string {
	term := mapAt(st, "lastState", "terminated")
	if term == nil {
		return ""
	}
	code, _ := ratOf(term["exitCode"])
	if code == nil {
		return ""
	}
	out := ", last exit code " + code.Num().String()
	if reason, _ := term["reason"].(string); reason != "" {
		out += " " + reason
	}
	return out
}

func podNotReadyDetail(p object) string {
	for _, c := range containerStatuses(p) {
		if w := mapAt(c.st, "state", "waiting"); w != nil {
			return fmt.Sprintf("container %s waiting: %s", c.name, orDefault(fmt.Sprint(w["reason"]), "no reason"))
		}
		if mapAt(c.st, "state", "terminated") != nil && !c.init {
			return fmt.Sprintf("container %s terminated%s", c.name, exitSuffix(map[string]any{"lastState": map[string]any{"terminated": mapAt(c.st, "state", "terminated")}}))
		}
		if ready, _ := c.st["ready"].(bool); !ready && !c.init {
			return fmt.Sprintf("container %s not ready", c.name)
		}
	}
	return ""
}
