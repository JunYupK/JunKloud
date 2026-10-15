package grader

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
)

// ------------------------------------------------------------------ job

// jobArgs are the Args of a "job" criterion: a Job RAN with the expected
// outcome. Target is a batch/v1 Job. A declaration alone proves nothing here;
// use cronjob-template for what a CronJob says and cronjob-schedule for what
// it did.
//
//	outcome:   "complete" (default): the Job's Complete condition is True and
//	           it succeeded as often as its spec asks; "failed": its Failed
//	           condition is True (for problems about backoffLimit or
//	           activeDeadlineSeconds)
//	reason:    with outcome failed: the Failed condition's reason
//	           (BackoffLimitExceeded, DeadlineExceeded, ...)
//	succeeded / failed: exact status.succeeded / status.failed counts
//	fields:    field requirements on the Job (same shape as the field checker)
//	wait:      default true: poll until the outcome is reached or the
//	           criterion timeout ends
//
// A Job that has not finished at the timeout is judged by its pods like a
// rollout (ready checker rules): still running or a learner-caused pod
// problem is FAIL; an environment problem or no evidence is ERROR.
type jobArgs struct {
	Outcome   string       `json:"outcome"`
	Reason    string       `json:"reason"`
	Succeeded *int         `json:"succeeded"`
	Failed    *int         `json:"failed"`
	Fields    []fieldInput `json:"fields"`
	Wait      *bool        `json:"wait"`

	reqs []requirement
}

func targetIs(crit model.Criterion, apiVersion, kind string) error {
	if err := checkRef(crit.Target); err != nil {
		return err
	}
	if crit.Target.APIVersion != apiVersion || crit.Target.Kind != kind {
		return invalidf("kind %s needs a %s %s target, not %s %s", crit.Kind, apiVersion, kind, crit.Target.APIVersion, crit.Target.Kind)
	}
	return nil
}

func parseJobArgs(crit model.Criterion) (jobArgs, error) {
	var a jobArgs
	if err := targetIs(crit, "batch/v1", "Job"); err != nil {
		return a, err
	}
	if err := decodeArgs(crit.Args, &a); err != nil {
		return a, err
	}
	switch a.Outcome {
	case "":
		a.Outcome = "complete"
	case "complete", "failed":
	default:
		return a, invalidf("args.outcome %q is not complete or failed", a.Outcome)
	}
	if a.Reason != "" && a.Outcome != "failed" {
		return a, invalidf("args.reason needs outcome failed")
	}
	for name, v := range map[string]*int{"succeeded": a.Succeeded, "failed": a.Failed} {
		if v != nil && *v < 0 {
			return a, invalidf("args.%s cannot be negative", name)
		}
	}
	if len(a.Fields) > 0 {
		reqs, err := compileRequirements("args.fields", a.Fields)
		if err != nil {
			return a, err
		}
		a.reqs = reqs
	}
	return a, nil
}

func (a jobArgs) wait() bool { return a.Wait == nil || *a.Wait }

type jobState struct {
	complete, failed bool
	failedReason     string
	failedMessage    string
	suspended        bool
	succeeded, nFail int
	active           int
}

func jobStateOf(obj object) jobState {
	var s jobState
	for _, c := range listAt(obj, "status", "conditions") {
		m, _ := c.(map[string]any)
		if m["status"] != "True" {
			continue
		}
		switch m["type"] {
		case "Complete":
			s.complete = true
		case "Failed":
			s.failed = true
			s.failedReason, _ = m["reason"].(string)
			s.failedMessage, _ = m["message"].(string)
		}
	}
	s.suspended, _ = valueAt(obj, "spec", "suspend").(bool)
	count := func(key string) int {
		if r, ok := ratOf(valueAt(obj, "status", key)); ok && r.IsInt() {
			return int(r.Num().Int64())
		}
		return 0
	}
	s.succeeded, s.nFail, s.active = count("succeeded"), count("failed"), count("active")
	return s
}

func evalJob(obj object, a jobArgs) verdict {
	s := jobStateOf(obj)
	actual := map[string]any{"complete": s.complete, "failed": s.failed, "succeeded": s.succeeded, "failed_pods": s.nFail, "active": s.active}
	expected := map[string]any{"outcome": a.Outcome}
	v := verdict{expected: expected, actual: actual}
	if s.failed {
		actual["failed_reason"] = s.failedReason
	}
	if len(a.reqs) > 0 {
		if fv := evalFields(obj, a.reqs); !fv.ok {
			v.terminal, v.reason = true, fv.reason
			return v
		}
	}
	switch a.Outcome {
	case "complete":
		switch {
		case s.failed:
			v.terminal = true
			v.reason = fmt.Sprintf("the Job failed (%s): %s", s.failedReason, shortenLine(s.failedMessage))
			return v
		case !s.complete:
			if s.suspended {
				v.terminal, v.reason = true, "the Job is suspended, so it never runs"
				return v
			}
			v.reason = "the Job has not completed"
			return v
		}
	case "failed":
		switch {
		case s.complete:
			v.terminal, v.reason = true, "the Job completed, but a failed Job was expected"
			return v
		case !s.failed:
			v.reason = "the Job has not failed"
			return v
		}
		if a.Reason != "" && s.failedReason != a.Reason {
			v.terminal, v.reason = true, fmt.Sprintf("the Job failed with reason %s, want %s", s.failedReason, a.Reason)
			return v
		}
	}
	if a.Succeeded != nil {
		expected["succeeded"] = *a.Succeeded
		if s.succeeded != *a.Succeeded {
			v.terminal, v.reason = true, fmt.Sprintf("%d succeeded, want %d", s.succeeded, *a.Succeeded)
			return v
		}
	} else if a.Outcome == "complete" {
		want := 1
		if r, ok := ratOf(valueAt(obj, "spec", "completions")); ok && r.IsInt() {
			want = int(r.Num().Int64())
		}
		if _, hasCompletions := valueAt(obj, "spec", "completions").(int64); hasCompletions && s.succeeded < want {
			v.terminal, v.reason = true, fmt.Sprintf("%d succeeded but the Job asks for %d completions", s.succeeded, want)
			return v
		}
	}
	if a.Failed != nil {
		expected["failed"] = *a.Failed
		if s.nFail != *a.Failed {
			v.terminal, v.reason = true, fmt.Sprintf("%d failed pods, want %d", s.nFail, *a.Failed)
			return v
		}
	}
	v.ok = true
	return v
}

func (c *ArtifactChecker) checkJob(ctx context.Context, t Target, crit model.Criterion) (model.CheckResult, error) {
	a, err := parseJobArgs(crit)
	if err != nil {
		return model.CheckResult{}, err
	}
	if c.cfg.Reader == nil {
		return model.CheckResult{}, notConfigured("job checks need an object reader")
	}
	ref, err := resolveTarget(t, crit)
	if err != nil {
		return model.CheckResult{}, err
	}
	spec := observeSpec{
		ref: ref, timeout: timeoutFor(crit, c.set), poll: a.wait(), waitForCreate: a.wait(), confirm: true,
		eval: func(obj object) verdict { return evalJob(obj, a) },
	}
	obs, err := c.obs.observe(ctx, spec)
	if err != nil {
		return model.CheckResult{}, err
	}
	res := c.obs.result(obs, spec)
	if !obs.found || obs.v.ok || obs.v.terminal || !a.wait() {
		if !obs.found {
			res.Evidence.Expected = map[string]any{"outcome": a.Outcome}
			res.Evidence.Actual = map[string]any{"exists": false}
		}
		return res, nil
	}
	// Not finished when the time was up: the pods say whose fault that is.
	pods, err := ownedPods(ctx, c.cfg.Reader, "Job", obs.obj)
	if err != nil {
		return model.CheckResult{}, err
	}
	msgs, err := pullMessages(ctx, c.cfg.Reader, pods)
	if err != nil {
		return model.CheckResult{}, err
	}
	cause := classifyRollout(pods, true, causeConfig{preloaded: c.set.preloaded, configured: c.set.preloadedSet, pullMessages: msgs})
	switch {
	case cause.class == faultLearner:
		res.Reason += "; cause: " + cause.text
		return res, nil
	case cause.class == faultEnvironment:
		return model.CheckResult{}, fmt.Errorf("%w: %s; %s", ErrInsufficientEvidence, obs.v.reason, cause.text)
	}
	var running []string
	for _, p := range pods {
		if stringAt(p, "status", "phase") == "Running" {
			running = append(running, nameOf(p))
		}
	}
	if len(running) > 0 {
		res.Reason = fmt.Sprintf("the Job did not complete within %s: pod(s) %s still running", spec.timeout, strings.Join(boundedList(running, 3), ", "))
		return res, nil
	}
	return model.CheckResult{}, fmt.Errorf("%w: %s; %s", ErrInsufficientEvidence, obs.v.reason, cause.text)
}

// ------------------------------------------------------ cronjob-template

// cronTemplateArgs are the Args of a "cronjob-template" criterion: what the
// CronJob DECLARES. It reads the CronJob object and nothing it has created.
// Target is a batch/v1 CronJob.
//
//	schedule:           compared by meaning ("*/5 * * * *" equals
//	                    "0,5,10,...,55 * * * *", "@daily" equals "0 0 * * *")
//	suspend, concurrency_policy (Allow|Forbid|Replace), time_zone,
//	starting_deadline_seconds, successful_jobs_history_limit,
//	failed_jobs_history_limit
//	fields:             field requirements from the CronJob root, for the job
//	                    template (spec.jobTemplate.spec.template.spec...)
//
// At least one setting is required.
type cronTemplateArgs struct {
	Schedule                   *string      `json:"schedule"`
	Suspend                    *bool        `json:"suspend"`
	ConcurrencyPolicy          *string      `json:"concurrency_policy"`
	TimeZone                   *string      `json:"time_zone"`
	StartingDeadlineSeconds    *int         `json:"starting_deadline_seconds"`
	SuccessfulJobsHistoryLimit *int         `json:"successful_jobs_history_limit"`
	FailedJobsHistoryLimit     *int         `json:"failed_jobs_history_limit"`
	Fields                     []fieldInput `json:"fields"`

	reqs []requirement
}

func parseCronTemplateArgs(crit model.Criterion) (cronTemplateArgs, error) {
	var a cronTemplateArgs
	if err := targetIs(crit, "batch/v1", "CronJob"); err != nil {
		return a, err
	}
	if err := decodeArgs(crit.Args, &a); err != nil {
		return a, err
	}
	if a.Schedule != nil {
		if _, err := parseCron(*a.Schedule); err != nil {
			return a, invalidf("args.schedule: %v", err)
		}
	}
	if p := a.ConcurrencyPolicy; p != nil && *p != "Allow" && *p != "Forbid" && *p != "Replace" {
		return a, invalidf("args.concurrency_policy %q is not Allow, Forbid or Replace", *p)
	}
	for name, v := range map[string]*int{"starting_deadline_seconds": a.StartingDeadlineSeconds,
		"successful_jobs_history_limit": a.SuccessfulJobsHistoryLimit, "failed_jobs_history_limit": a.FailedJobsHistoryLimit} {
		if v != nil && *v < 0 {
			return a, invalidf("args.%s cannot be negative", name)
		}
	}
	if len(a.Fields) > 0 {
		reqs, err := compileRequirements("args.fields", a.Fields)
		if err != nil {
			return a, err
		}
		a.reqs = reqs
	}
	if a.Schedule == nil && a.Suspend == nil && a.ConcurrencyPolicy == nil && a.TimeZone == nil && a.StartingDeadlineSeconds == nil &&
		a.SuccessfulJobsHistoryLimit == nil && a.FailedJobsHistoryLimit == nil && len(a.reqs) == 0 {
		return a, invalidf("cronjob-template needs at least one setting to check")
	}
	return a, nil
}

func (c *ArtifactChecker) checkCronTemplate(ctx context.Context, t Target, crit model.Criterion) (model.CheckResult, error) {
	a, err := parseCronTemplateArgs(crit)
	if err != nil {
		return model.CheckResult{}, err
	}
	if c.cfg.Reader == nil {
		return model.CheckResult{}, notConfigured("cronjob checks need an object reader")
	}
	ref, err := resolveTarget(t, crit)
	if err != nil {
		return model.CheckResult{}, err
	}
	spec := observeSpec{ref: ref, timeout: timeoutFor(crit, c.set), eval: func(obj object) verdict { return evalCronTemplate(obj, a) }}
	obs, err := c.obs.observe(ctx, spec)
	if err != nil {
		return model.CheckResult{}, err
	}
	res := c.obs.result(obs, spec)
	if !obs.found {
		res.Evidence.Expected, res.Evidence.Actual = map[string]any{"cronjob": describeRef(ref)}, map[string]any{"exists": false}
	}
	return res, nil
}

func evalCronTemplate(obj object, a cronTemplateArgs) verdict {
	expected, actual := map[string]any{}, map[string]any{}
	var failures []string
	add := func(f string, v ...any) { failures = append(failures, fmt.Sprintf(f, v...)) }
	if a.Schedule != nil {
		got, _ := valueAt(obj, "spec", "schedule").(string)
		expected["schedule"], actual["schedule"] = *a.Schedule, got
		if same, err := cronEqual(*a.Schedule, got); err != nil || !same {
			add("schedule %q does not mean %q", got, *a.Schedule)
		}
	}
	var reqs []requirement
	push := func(path string, equals any) {
		segs, _ := parsePath(path)
		reqs = append(reqs, requirement{path: path, segs: segs, hasEquals: true, equals: equals, cmp: comparer{match: matchEqual}})
	}
	if a.Suspend != nil {
		// An absent field means false.
		got, _ := valueAt(obj, "spec", "suspend").(bool)
		expected["suspend"], actual["suspend"] = *a.Suspend, got
		if got != *a.Suspend {
			add("spec.suspend is %t, want %t", got, *a.Suspend)
		}
	}
	if a.ConcurrencyPolicy != nil {
		push("spec.concurrencyPolicy", *a.ConcurrencyPolicy)
	}
	if a.TimeZone != nil {
		push("spec.timeZone", *a.TimeZone)
	}
	for path, v := range map[string]*int{"spec.startingDeadlineSeconds": a.StartingDeadlineSeconds,
		"spec.successfulJobsHistoryLimit": a.SuccessfulJobsHistoryLimit, "spec.failedJobsHistoryLimit": a.FailedJobsHistoryLimit} {
		if v != nil {
			push(path, *v)
		}
	}
	reqs = append(reqs, a.reqs...)
	if len(reqs) > 0 {
		v := evalFields(obj, reqs)
		for k, e := range asMap(v.expected) {
			expected[k] = e
		}
		for k, e := range asMap(v.actual) {
			actual[k] = e
		}
		if !v.ok {
			add("%s", v.reason)
		}
	}
	return verdict{ok: len(failures) == 0, terminal: true, reason: strings.Join(failures, "; "), expected: expected, actual: actual}
}

// ------------------------------------------------------ cronjob-schedule

const (
	annotationCronScheduled = "batch.kubernetes.io/cronjob-scheduled-timestamp"
	annotationCronManual    = "cronjob.kubernetes.io/instantiate"
	managerCronJob          = "kube-controller-manager"
	minScheduleWindow       = 5 * time.Second
	maxScheduleWindow       = 120 * time.Second
	scheduleSlack           = 10 * time.Second
	maxScheduleRuns         = 5
)

// cronScheduleArgs are the Args of a "cronjob-schedule" criterion: the
// CronJob FIRED on its schedule. Target is a batch/v1 CronJob.
//
//	window:   REQUIRED, 5s..120s: how long the grader watches. The criterion's
//	          own timeout must exceed it by 10s. A schedule that cannot fire
//	          within the window cannot pass: such a problem must grade the
//	          declaration with cronjob-template instead.
//	min_runs: default 1, max 5: how many runs must appear in the window.
//
// A run counts only if a Job
//   - is owned (controller owner reference) by THIS CronJob (UID),
//   - appeared after the observation began (jobs that existed at the start are
//     remembered and ignored),
//   - carries the scheduled-time annotation the CronJob controller sets, and
//   - was written by kube-controller-manager (managedFields), so a Job made
//     with `kubectl create job --from=cronjob/...` or copied with forged
//     annotations does not count. Manual Jobs are listed in the evidence as
//     ignored.
type cronScheduleArgs struct {
	Window  string `json:"window"`
	MinRuns int    `json:"min_runs"`

	window time.Duration
}

func parseCronScheduleArgs(crit model.Criterion, defaultTimeout time.Duration) (cronScheduleArgs, error) {
	var a cronScheduleArgs
	if err := targetIs(crit, "batch/v1", "CronJob"); err != nil {
		return a, err
	}
	if err := decodeArgs(crit.Args, &a); err != nil {
		return a, err
	}
	if a.Window == "" {
		return a, invalidf("args.window is required: the observation window (%s..%s) must be declared", minScheduleWindow, maxScheduleWindow)
	}
	d, err := time.ParseDuration(a.Window)
	if err != nil || d < minScheduleWindow || d > maxScheduleWindow {
		return a, invalidf("args.window %q must be a duration of %s..%s", a.Window, minScheduleWindow, maxScheduleWindow)
	}
	a.window = d
	if a.MinRuns == 0 {
		a.MinRuns = 1
	}
	if a.MinRuns < 1 || a.MinRuns > maxScheduleRuns {
		return a, invalidf("args.min_runs must be 1..%d", maxScheduleRuns)
	}
	timeout := crit.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if timeout < d+scheduleSlack {
		return a, invalidf("criterion timeout %s must exceed args.window %s by at least %s", timeout, d, scheduleSlack)
	}
	return a, nil
}

type cronRun struct {
	Job       string `json:"job"`
	Scheduled string `json:"scheduled_at"`
}

func (c *ArtifactChecker) checkCronSchedule(ctx context.Context, t Target, crit model.Criterion) (model.CheckResult, error) {
	a, err := parseCronScheduleArgs(crit, c.set.defaultTimeout)
	if err != nil {
		return model.CheckResult{}, err
	}
	if c.cfg.Reader == nil {
		return model.CheckResult{}, notConfigured("cronjob checks need an object reader")
	}
	ref, err := resolveTarget(t, crit)
	if err != nil {
		return model.CheckResult{}, err
	}
	start := c.set.clock.Now()
	expected := map[string]any{"cronjob": describeRef(ref), "window": a.Window, "min_runs": a.MinRuns}
	cron, err := c.obs.get(ctx, ref)
	if isNotFound(err) {
		return c.verdictResult(start, false, fmt.Sprintf("CronJob %q not found", ref.Name), expected, map[string]any{"exists": false}), nil
	}
	if err != nil {
		return model.CheckResult{}, err
	}
	base := keyOf(cron)
	finish := func(ok bool, reason string, actual map[string]any) model.CheckResult {
		res := c.verdictResult(start, ok, reason, expected, actual)
		res.Evidence.UID, res.Evidence.Generation, res.Evidence.InputHash = uidOf(cron), generationOf(cron), inputHash(cron)
		return res
	}
	if suspended, _ := valueAt(cron, "spec", "suspend").(bool); suspended {
		return finish(false, "the CronJob is suspended, so it can never fire", map[string]any{"suspend": true}), nil
	}

	cronUID := uidOf(cron)
	seen := map[string]bool{} // jobs that existed when the observation began
	var runs []cronRun
	manual := map[string]bool{}
	first := true
	deadline := start.Add(a.window)
	for {
		jobs, err := c.cfg.Reader.List(ctx, ListQuery{APIVersion: "batch/v1", Kind: "Job", Namespace: ref.Namespace})
		if err != nil {
			return model.CheckResult{}, fmt.Errorf("list Jobs: %w", err)
		}
		runs = runs[:0]
		for _, j := range jobs {
			if controllerUID(j) != cronUID {
				continue
			}
			name := nameOf(j)
			kind := scheduledByController(j)
			if kind == runManual {
				manual[name] = true // listed as ignored, whenever it was made
				continue
			}
			if first {
				seen[uidOf(j)] = true
				continue
			}
			if seen[uidOf(j)] || kind != runScheduled {
				continue
			}
			runs = append(runs, cronRun{Job: name, Scheduled: stringAt(j, "metadata", "annotations", annotationCronScheduled)})
		}
		first = false
		if len(runs) >= a.MinRuns {
			break
		}
		now := c.set.clock.Now()
		if !now.Before(deadline) {
			break
		}
		wait := min(c.set.pollInterval, deadline.Sub(now))
		if err := c.set.clock.Sleep(ctx, wait); err != nil {
			return model.CheckResult{}, err
		}
		again, err := c.obs.get(ctx, ref)
		if err != nil {
			if isNotFound(err) {
				return model.CheckResult{}, fmt.Errorf("%w: CronJob %s no longer exists", ErrTargetChanged, ref.Name)
			}
			return model.CheckResult{}, err
		}
		if keyOf(again) != base {
			return model.CheckResult{}, fmt.Errorf("%w: CronJob %s was replaced or edited during the observation", ErrTargetChanged, ref.Name)
		}
	}
	actual := map[string]any{"runs_observed": len(runs), "runs": append([]cronRun{}, runs...)}
	if len(manual) > 0 {
		actual["manual_jobs_ignored"] = len(manual)
	}
	if len(runs) >= a.MinRuns {
		return finish(true, "", actual), nil
	}
	return finish(false, fmt.Sprintf("no Job was created by the CronJob controller within %s (%d of %d runs observed; Jobs made by hand from the CronJob do not prove the schedule)",
		a.Window, len(runs), a.MinRuns), actual), nil
}

type runKind int

const (
	runOther runKind = iota
	runScheduled
	runManual
)

// scheduledByController: the Job carries the controller's scheduled-time
// annotation and the controller wrote it.
func scheduledByController(job object) runKind {
	ann := asMap(metaOf(job)["annotations"])
	if _, ok := ann[annotationCronScheduled]; ok {
		for _, mf := range listAt(job, "metadata", "managedFields") {
			m, _ := mf.(map[string]any)
			if m["manager"] == managerCronJob {
				return runScheduled
			}
		}
		return runOther
	}
	if ann[annotationCronManual] == "manual" {
		return runManual
	}
	return runOther
}

func isNotFound(err error) bool { return err != nil && errors.Is(err, ErrNotFound) }
