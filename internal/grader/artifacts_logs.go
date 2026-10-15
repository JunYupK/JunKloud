package grader

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/JunYupK/JunKloud/internal/model"
)

// logsArgs are the Args of a "logs" criterion. Target is a Pod or a workload
// that owns pods (Deployment, StatefulSet, DaemonSet, ReplicaSet, Job).
//
//	container:    container name; default the first container of the pod
//	previous:     read the previous container instance's log
//	tail_lines:   how many trailing lines to read (default 500, max 5000)
//	wait:         poll until the rules hold or the timeout ends
//	pods:         workload targets: "any" (default) pod or "all" pods must hold
//	contains / not_contains / matches: substrings and RE2 patterns over the log
//	json_fields:  field requirements (same shape as the field checker's fields)
//	              evaluated on every log line that is a JSON object; at least
//	              min_matches (default 1) lines must satisfy all of them
//	min_lines:    the log must have at least this many lines
//
// The log text is never stored in the evidence (an application may print
// tokens): evidence names the unmet rules and counts lines. Secret values of
// the namespace are additionally registered with the redactor.
type logsArgs struct {
	Container   string       `json:"container"`
	Previous    bool         `json:"previous"`
	TailLines   int64        `json:"tail_lines"`
	Wait        bool         `json:"wait"`
	Pods        string       `json:"pods"`
	Contains    []string     `json:"contains"`
	NotContains []string     `json:"not_contains"`
	Matches     []string     `json:"matches"`
	JSONFields  []fieldInput `json:"json_fields"`
	MinMatches  int          `json:"min_matches"`
	MinLines    int          `json:"min_lines"`

	patterns []*regexp.Regexp
	reqs     []requirement
}

const (
	defaultTailLines = 500
	maxTailLines     = 5000
)

var logTargetKinds = map[string]bool{
	"Pod": true, "Deployment": true, "StatefulSet": true, "DaemonSet": true, "ReplicaSet": true, "Job": true,
}

func parseLogsArgs(crit model.Criterion) (logsArgs, error) {
	var a logsArgs
	if err := checkRef(crit.Target); err != nil {
		return a, err
	}
	if !logTargetKinds[crit.Target.Kind] {
		return a, invalidf("logs cannot read %s: target must be a Pod, Deployment, StatefulSet, DaemonSet, ReplicaSet or Job", crit.Target.Kind)
	}
	if err := decodeArgs(crit.Args, &a); err != nil {
		return a, err
	}
	switch a.Pods {
	case "":
		a.Pods = "any"
	case "any", "all":
	default:
		return a, invalidf("args.pods %q is not any or all", a.Pods)
	}
	if a.TailLines < 0 || a.TailLines > maxTailLines {
		return a, invalidf("args.tail_lines must be 0..%d", maxTailLines)
	}
	if a.TailLines == 0 {
		a.TailLines = defaultTailLines
	}
	if a.MinLines < 0 || a.MinMatches < 0 {
		return a, invalidf("args.min_lines and args.min_matches cannot be negative")
	}
	if a.MinMatches > 0 && len(a.JSONFields) == 0 {
		return a, invalidf("args.min_matches needs args.json_fields")
	}
	if err := compileStringsAll(map[string][]string{"contains": a.Contains, "not_contains": a.NotContains}); err != nil {
		return a, err
	}
	var err error
	if a.patterns, err = compilePatterns("matches", a.Matches); err != nil {
		return a, err
	}
	if len(a.JSONFields) > 0 {
		if a.reqs, err = compileRequirements("args.json_fields", a.JSONFields); err != nil {
			return a, err
		}
		if a.MinMatches == 0 {
			a.MinMatches = 1
		}
	}
	if len(a.Contains)+len(a.NotContains)+len(a.Matches)+len(a.JSONFields) == 0 && a.MinLines == 0 {
		return a, invalidf("logs needs at least one rule (contains, not_contains, matches, json_fields or min_lines)")
	}
	return a, nil
}

// evaluate applies the rules to one log. It returns the line count and the
// unmet rules (empty when the log satisfies everything).
func (a logsArgs) evaluate(text string) (int, []string) {
	lines := 0
	jsonMatches, jsonTotal := 0, 0
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		lines++
		line := strings.TrimSpace(sc.Text())
		if len(a.reqs) == 0 || !strings.HasPrefix(line, "{") {
			continue
		}
		var doc map[string]any
		if json.Unmarshal([]byte(line), &doc) != nil {
			continue
		}
		jsonTotal++
		if evalFields(doc, a.reqs).ok {
			jsonMatches++
		}
	}
	unmet := textRuleFailures(text, a.Contains, a.NotContains, a.patterns)
	if a.MinLines > 0 && lines < a.MinLines {
		unmet = append(unmet, fmt.Sprintf("has %d lines, fewer than the required %d", lines, a.MinLines))
	}
	if len(a.reqs) > 0 && jsonMatches < a.MinMatches {
		unmet = append(unmet, fmt.Sprintf("json_fields: %d of the %d required JSON lines satisfy the fields (%d JSON lines seen)", jsonMatches, a.MinMatches, jsonTotal))
	}
	return lines, unmet
}

type podLog struct {
	Pod       string   `json:"pod"`
	Container string   `json:"container"`
	Lines     int      `json:"lines"`
	Unmet     []string `json:"unmet,omitempty"`
	ok        bool
}

type logsRound struct {
	found      bool // the target exists
	pods       []object
	reads      []podLog
	unreadable []object // pods whose container produced no log
	missing    []string // pods lacking the named container
	holds      bool
}

func (c *ArtifactChecker) checkLogs(ctx context.Context, t Target, crit model.Criterion) (model.CheckResult, error) {
	a, err := parseLogsArgs(crit)
	if err != nil {
		return model.CheckResult{}, err
	}
	if c.cfg.Reader == nil || c.cfg.Pods == nil {
		return model.CheckResult{}, notConfigured("logs need an object reader and pod access")
	}
	ref, err := resolveTarget(t, crit)
	if err != nil {
		return model.CheckResult{}, err
	}
	if err := c.learnNamespaceSecrets(ctx, ref.Namespace); err != nil {
		return model.CheckResult{}, err
	}
	start := c.set.clock.Now()
	var round logsRound
	var target object
	err = c.pollUntil(ctx, timeoutFor(crit, c.set), a.Wait, func(ctx context.Context) (bool, error) {
		var err error
		round, target, err = c.logsRound(ctx, ref, a)
		return round.holds, err
	})
	if err != nil {
		return model.CheckResult{}, err
	}
	expected := map[string]any{"target": describeRef(ref), "pods": a.Pods}
	if !round.found {
		return c.verdictResult(start, false, fmt.Sprintf("%s %q not found", ref.Kind, ref.Name), expected, map[string]any{"exists": false}), nil
	}
	actual := map[string]any{"pods": boundedLogs(round.reads)}
	res := c.verdictResult(start, round.holds, "", expected, actual)
	res.Evidence.UID, res.Evidence.Generation = uidOf(target), generationOf(target)
	if round.holds {
		return res, nil
	}
	if len(round.reads) == 0 {
		// No container produced a log: only the learner's own pod problems
		// are a FAIL; anything else is not proven (spec 8.1).
		if len(round.missing) > 0 {
			res.Status, res.Reason = model.StatusFail, fmt.Sprintf("container %q does not exist in pod(s) %s", a.Container, strings.Join(round.missing, ", "))
			return res, nil
		}
		msgs, err := pullMessages(ctx, c.cfg.Reader, round.pods)
		if err != nil {
			return model.CheckResult{}, err
		}
		cause := classifyRollout(round.pods, true, causeConfig{preloaded: c.set.preloaded, configured: c.set.preloadedSet, pullMessages: msgs})
		if cause.class != faultLearner {
			return model.CheckResult{}, fmt.Errorf("%w: no container of %s %s has produced a log; %s", ErrInsufficientEvidence, ref.Kind, ref.Name, cause.text)
		}
		res.Status, res.Reason = model.StatusFail, "no log can be read: "+cause.text
		return res, nil
	}
	res.Status = model.StatusFail
	var parts []string
	for _, r := range round.reads {
		if !r.ok {
			parts = append(parts, fmt.Sprintf("pod %s: %s", r.Pod, strings.Join(r.Unmet, "; ")))
		}
	}
	if a.Pods == "all" && len(round.unreadable) > 0 {
		parts = append(parts, fmt.Sprintf("%d pod(s) have no log yet", len(round.unreadable)))
	}
	if len(parts) > 3 {
		parts = append(parts[:3], fmt.Sprintf("and %d more", len(parts)-3))
	}
	res.Reason = strings.Join(parts, "; ")
	return res, nil
}

// logsRound reads the logs once and judges them.
func (c *ArtifactChecker) logsRound(ctx context.Context, ref model.ObjectRef, a logsArgs) (logsRound, object, error) {
	var r logsRound
	target, err := c.obs.get(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return r, nil, nil
	}
	if err != nil {
		return r, nil, err
	}
	r.found = true
	pods, err := ownedPods(ctx, c.cfg.Reader, ref.Kind, target)
	if err != nil {
		return r, target, err
	}
	sort.Slice(pods, func(i, j int) bool { return nameOf(pods[i]) < nameOf(pods[j]) })
	r.pods = pods
	for _, p := range pods {
		if metaOf(p)["deletionTimestamp"] != nil && ref.Kind != "Pod" {
			continue
		}
		container, ok := pickContainer(p, a.Container)
		if !ok {
			r.missing = append(r.missing, nameOf(p))
			continue
		}
		text, err := c.cfg.Pods.Logs(ctx, ref.Namespace, nameOf(p), container, LogOptions{TailLines: a.TailLines, Previous: a.Previous})
		if errors.Is(err, ErrContainerNotStarted) {
			r.unreadable = append(r.unreadable, p)
			continue
		}
		if err != nil {
			return r, target, err
		}
		lines, unmet := a.evaluate(text)
		r.reads = append(r.reads, podLog{Pod: nameOf(p), Container: container, Lines: lines, Unmet: unmet, ok: len(unmet) == 0})
	}
	switch a.Pods {
	case "all":
		r.holds = len(r.reads) > 0 && len(r.unreadable) == 0 && len(r.missing) == 0
		for _, l := range r.reads {
			r.holds = r.holds && l.ok
		}
	default:
		for _, l := range r.reads {
			r.holds = r.holds || l.ok
		}
	}
	return r, target, nil
}

// pickContainer names the container to read: the requested one (which must
// exist in the pod) or the first regular container.
func pickContainer(pod object, want string) (string, bool) {
	var first string
	for _, c := range listAt(pod, "spec", "containers") {
		m, _ := c.(map[string]any)
		name, _ := m["name"].(string)
		if first == "" {
			first = name
		}
		if want != "" && name == want {
			return name, true
		}
	}
	if want != "" {
		return "", false
	}
	return first, first != ""
}

func boundedLogs(l []podLog) []podLog {
	if len(l) > 5 {
		return l[:5]
	}
	return l
}
