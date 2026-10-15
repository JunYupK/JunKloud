package grader_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

var (
	jobRef  = refOf("batch/v1", "Job", "jk-p1", "report")
	cronRef = refOf("batch/v1", "CronJob", "jk-p1", "nightly")
)

func jobDoc(t testing.TB, extraSpec, status string) map[string]any {
	return obj(t, fmt.Sprintf(`{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"report","namespace":"jk-p1","uid":"job-1","generation":1},
	 "spec":{"completions":3,"parallelism":1,"backoffLimit":2,"selector":{"matchLabels":{"job-name":"report"}}%s},"status":%s}`, extraSpec, status))
}

func jobCrit(args map[string]any) model.Criterion {
	return model.Criterion{ID: "c", Kind: model.CriterionJob, Target: jobRef, Points: 3, Args: args, Timeout: 20 * time.Second}
}

func gradeJob(t testing.TB, r *fakeReader, c model.Criterion) model.CheckResult {
	return gradeOne(t, grader.ArtifactConfig{Reader: r}, "", c)
}

func TestJobOutcome(t *testing.T) {
	complete := `{"succeeded":3,"conditions":[{"type":"Complete","status":"True"}],"completionTime":"2026-10-15T00:00:00Z"}`
	failedJob := `{"failed":3,"conditions":[{"type":"Failed","status":"True","reason":"BackoffLimitExceeded","message":"Job has reached the specified backoff limit"}]}`
	running := `{"active":1,"succeeded":1}`
	cases := []struct {
		name   string
		status string
		extra  string
		args   map[string]any
		pods   []map[string]any
		want   string
		reason []string
	}{
		{name: "ran to completion", status: complete, want: "PASS"},
		{name: "completion counts as the spec asks", status: complete, args: map[string]any{"succeeded": 3, "fields": []any{
			map[string]any{"path": "spec.backoffLimit", "equals": 2}}}, want: "PASS"},
		{name: "wrong spec field", status: complete, args: map[string]any{"fields": []any{
			map[string]any{"path": "spec.parallelism", "equals": 3}}}, want: "FAIL", reason: []string{"spec.parallelism"}},
		{name: "wrong succeeded count", status: complete, args: map[string]any{"succeeded": 5}, want: "FAIL", reason: []string{"3 succeeded", "5"}},
		{name: "failed job when completion is expected", status: failedJob, want: "FAIL", reason: []string{"BackoffLimitExceeded"}},
		{name: "failure expected and observed", status: failedJob, args: map[string]any{"outcome": "failed", "failed": 3}, want: "PASS"},
		{name: "failure expected with its reason", status: failedJob, args: map[string]any{"outcome": "failed", "reason": "DeadlineExceeded"}, want: "FAIL", reason: []string{"DeadlineExceeded"}},
		{name: "completed although failure was expected", status: complete, args: map[string]any{"outcome": "failed"}, want: "FAIL", reason: []string{"completed"}},
		{name: "suspended job", status: `{}`, extra: `,"suspend":true`, want: "FAIL", reason: []string{"suspended"}},
		{name: "still running at the timeout", status: running, pods: []map[string]any{
			podDoc(t, "report-a", "job-1", "Running", "main")}, want: "FAIL", reason: []string{"still running"}},
		{name: "pod crash looping", status: running, pods: []map[string]any{
			crashLoopJobPod(t, "report-a")}, want: "FAIL", reason: []string{"CrashLoopBackOff"}},
		{name: "no pods at all is not proven", status: `{}`, want: "ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newReader().put(jobRef, jobDoc(t, tc.extra, tc.status)).putList("Pod", "jk-p1", tc.pods...).putList("Event", "jk-p1")
			res := gradeJob(t, r, jobCrit(tc.args))
			testutil.Equal(t, res.Status, tc.want)
			reasonHas(t, res, tc.reason...)
			if tc.want == "PASS" {
				testutil.Equal(t, res.Points, 3)
			}
		})
	}
	t.Run("waits for completion", func(t *testing.T) {
		r := newReader().put(jobRef,
			jobDoc(t, "", running), jobDoc(t, "", running), jobDoc(t, "", complete))
		res := gradeJob(t, r, jobCrit(nil))
		testutil.Equal(t, res.Status, "PASS")
		testutil.Equal(t, r.callsFor(jobRef) >= 3, true)
	})
	t.Run("missing job", func(t *testing.T) {
		res := gradeJob(t, newReader(), jobCrit(nil))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "not found")
	})
	t.Run("api failure", func(t *testing.T) {
		res := gradeJob(t, newReader().put(jobRef, fmt.Errorf("etcd")), jobCrit(nil))
		testutil.Equal(t, res.Status, "ERROR")
	})
	t.Run("a job replaced during grading", func(t *testing.T) {
		other := jobDoc(t, "", running)
		other["metadata"].(map[string]any)["uid"] = "job-2"
		r := newReader().put(jobRef, jobDoc(t, "", running), other)
		res := gradeJob(t, r, jobCrit(nil))
		testutil.Equal(t, res.Status, "ERROR")
		reasonHas(t, res, "changed")
	})
}

func crashLoopJobPod(t testing.TB, name string) map[string]any {
	return obj(t, fmt.Sprintf(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":%q,"namespace":"jk-p1","uid":"u-%s","ownerReferences":[{"uid":"job-1","controller":true}]},
	 "spec":{"containers":[{"name":"main","image":"busybox:1.37.0"}]},
	 "status":{"phase":"Running","containerStatuses":[{"name":"main","ready":false,"restartCount":4,"state":{"waiting":{"reason":"CrashLoopBackOff","message":"back-off"}},
	  "lastState":{"terminated":{"exitCode":1,"reason":"Error"}}}]}}`, name, name))
}

func cronDoc(t testing.TB, schedule, extra string) map[string]any {
	return obj(t, fmt.Sprintf(`{"apiVersion":"batch/v1","kind":"CronJob","metadata":{"name":"nightly","namespace":"jk-p1","uid":"cron-1","generation":1},
	 "spec":{"schedule":%q,"concurrencyPolicy":"Forbid","successfulJobsHistoryLimit":3,"jobTemplate":{"spec":{"template":{"spec":{
	  "restartPolicy":"OnFailure","containers":[{"name":"main","image":"busybox:1.37.0","command":["date"]}]}}}}%s}}`, schedule, extra))
}

func cronTemplateCrit(args map[string]any) model.Criterion {
	return model.Criterion{ID: "c", Kind: model.CriterionCronJobTemplate, Target: cronRef, Points: 2, Args: args}
}

func TestCronTemplate(t *testing.T) {
	cases := []struct {
		name     string
		schedule string
		extra    string
		args     map[string]any
		want     string
		reason   []string
	}{
		{name: "same schedule", schedule: "*/5 * * * *", args: map[string]any{"schedule": "*/5 * * * *"}, want: "PASS"},
		{name: "equivalent schedule written another way", schedule: "0,5,10,15,20,25,30,35,40,45,50,55 * * * *", args: map[string]any{"schedule": "*/5 * * * *"}, want: "PASS"},
		{name: "daily macro", schedule: "@daily", args: map[string]any{"schedule": "0 0 * * *"}, want: "PASS"},
		{name: "day names", schedule: "30 2 * * mon-fri", args: map[string]any{"schedule": "30 2 * * 1-5"}, want: "PASS"},
		{name: "other schedule", schedule: "*/10 * * * *", args: map[string]any{"schedule": "*/5 * * * *"}, want: "FAIL", reason: []string{"schedule"}},
		{name: "day-of-week star is not the same as a list", schedule: "0 0 * * 1", args: map[string]any{"schedule": "0 0 1 * *"}, want: "FAIL", reason: []string{"schedule"}},
		{name: "settings", schedule: "*/5 * * * *", extra: `,"suspend":false`, args: map[string]any{"concurrency_policy": "Forbid", "suspend": false,
			"successful_jobs_history_limit": 3}, want: "PASS"},
		{name: "wrong concurrency policy", schedule: "*/5 * * * *", args: map[string]any{"concurrency_policy": "Replace"}, want: "FAIL", reason: []string{"concurrencyPolicy"}},
		{name: "suspended when it must not be", schedule: "*/5 * * * *", extra: `,"suspend":true`, args: map[string]any{"suspend": false}, want: "FAIL", reason: []string{"suspend"}},
		{name: "job template field", schedule: "*/5 * * * *", args: map[string]any{"fields": []any{
			map[string]any{"path": "spec.jobTemplate.spec.template.spec.containers[name=main].command", "equals": []any{"date"}}}}, want: "PASS"},
		{name: "job template restart policy", schedule: "*/5 * * * *", args: map[string]any{"fields": []any{
			map[string]any{"path": "spec.jobTemplate.spec.template.spec.restartPolicy", "equals": "Never"}}}, want: "FAIL", reason: []string{"restartPolicy"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newReader().put(cronRef, cronDoc(t, tc.schedule, tc.extra))
			res := gradeOne(t, grader.ArtifactConfig{Reader: r}, "", cronTemplateCrit(tc.args))
			testutil.Equal(t, res.Status, tc.want)
			reasonHas(t, res, tc.reason...)
			// A template check reads the CronJob and nothing else: it says
			// nothing about what the CronJob has done.
			testutil.Equal(t, len(r.listQuery), 0)
		})
	}
	t.Run("not a cronjob target", func(t *testing.T) {
		c := cronTemplateCrit(map[string]any{"schedule": "* * * * *"})
		c.Target = refOf("apps/v1", "Deployment", "jk-p1", "x")
		res := gradeOne(t, grader.ArtifactConfig{Reader: newReader()}, "", c)
		testutil.Equal(t, res.Status, "ERROR")
	})
	t.Run("missing", func(t *testing.T) {
		res := gradeOne(t, grader.ArtifactConfig{Reader: newReader()}, "", cronTemplateCrit(map[string]any{"schedule": "* * * * *"}))
		testutil.Equal(t, res.Status, "FAIL")
	})
}

// ----------------------------------------------------- schedule observation

const scheduledAnnotation = "batch.kubernetes.io/cronjob-scheduled-timestamp"

// cronChild is a Job owned by the CronJob. controller says which manager
// wrote it into the API.
func cronChild(t testing.TB, name, controller string, annotations map[string]string) map[string]any {
	ann, _ := jsonString(annotations)
	return obj(t, fmt.Sprintf(`{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":%q,"namespace":"jk-p1","uid":"uid-%s",
	 "ownerReferences":[{"apiVersion":"batch/v1","kind":"CronJob","name":"nightly","uid":"cron-1","controller":true}],
	 "annotations":%s,"managedFields":[{"manager":%q,"operation":"Update","apiVersion":"batch/v1"}]}}`, name, name, ann, controller))
}

func jsonString(m map[string]string) (string, error) {
	var parts []string
	for k, v := range m {
		parts = append(parts, fmt.Sprintf("%q:%q", k, v))
	}
	return "{" + strings.Join(parts, ",") + "}", nil
}

func firedJob(t testing.TB, name string) map[string]any {
	return cronChild(t, name, "kube-controller-manager", map[string]string{scheduledAnnotation: "2026-10-15T00:01:00Z"})
}

// manualJob is what `kubectl create job --from=cronjob/nightly` makes: the
// CronJob owns it, but nothing scheduled it.
func manualJob(t testing.TB, name string) map[string]any {
	return cronChild(t, name, "kubectl-create", map[string]string{"cronjob.kubernetes.io/instantiate": "manual"})
}

// seqReader serves the Jobs list differently on each call.
type seqReader struct {
	*fakeReader
	mu    sync.Mutex
	steps [][]map[string]any
	calls int
}

func (s *seqReader) List(ctx context.Context, q grader.ListQuery) ([]map[string]any, error) {
	if q.Kind != "Job" {
		return s.fakeReader.List(ctx, q)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := min(s.calls, len(s.steps)-1)
	s.calls++
	return s.steps[i], nil
}

func scheduleCrit(window string) model.Criterion {
	return model.Criterion{ID: "c", Kind: model.CriterionCronJobSchedule, Target: cronRef, Points: 4, Timeout: 100 * time.Second,
		Args: map[string]any{"schedule": "* * * * *", "window": window}}
}

// cronNever is a live CronJob whose schedule cannot fire in a short window.
func cronNever(t testing.TB) map[string]any { return cronDoc(t, "0 0 1 1 *", "") }

func gradeSchedule(t testing.TB, steps [][]map[string]any, window string, cron map[string]any) (model.CheckResult, *seqReader, *fakeClock) {
	t.Helper()
	r := &seqReader{fakeReader: newReader().put(cronRef, cron), steps: steps}
	clock := newClock()
	eng, _ := artifactEngine(grader.ArtifactConfig{Reader: r}, clock)
	ev, err := eng.Evaluate(context.Background(), artifactTarget("", scheduleCrit(window)))
	testutil.NoError(t, err)
	return ev.Results[0], r, clock
}

// TestCronScheduleNotProvedByManualJob: the original fixture is a Job the
// CronJob controller created during the window; the wrong answers are jobs a
// person made from the CronJob or forged by hand.
func TestCronScheduleNotProvedByManualJob(t *testing.T) {
	cron := cronDoc(t, "* * * * *", "")
	t.Run("the controller fired during the window", func(t *testing.T) {
		res, _, _ := gradeSchedule(t, [][]map[string]any{nil, nil, {firedJob(t, "nightly-29000001")}}, "90s", cron)
		testutil.Equal(t, res.Status, "PASS")
		testutil.Equal(t, res.Points, 4)
	})
	t.Run("a manual job proves nothing", func(t *testing.T) {
		// The live schedule cannot fire in the window, so nothing was due; the
		// manual job is the only Job and proves nothing.
		res, _, _ := gradeSchedule(t, [][]map[string]any{{manualJob(t, "nightly-manual")}}, "90s", cronNever(t))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "does not fire within", "90s", "do not prove the schedule")
		if !strings.Contains(evidenceJSON(t, res), "manual") {
			t.Errorf("evidence does not mention the ignored manual job: %s", evidenceJSON(t, res))
		}
	})
	t.Run("a manual job that existed before the window is still reported as ignored", func(t *testing.T) {
		m := manualJob(t, "nightly-manual")
		res, _, _ := gradeSchedule(t, [][]map[string]any{{m}, {m}}, "90s", cronNever(t))
		testutil.Equal(t, res.Status, "FAIL")
		if !strings.Contains(evidenceJSON(t, res), `"manual_jobs_ignored":1`) {
			t.Errorf("evidence: %s", evidenceJSON(t, res))
		}
	})
	t.Run("a job with the scheduled annotation written by a person", func(t *testing.T) {
		forged := cronChild(t, "nightly-forged", "kubectl-client-side-apply", map[string]string{scheduledAnnotation: "2026-10-15T00:01:00Z"})
		res, _, _ := gradeSchedule(t, [][]map[string]any{nil, {forged}}, "90s", cronNever(t))
		testutil.Equal(t, res.Status, "FAIL")
		// And when a run WAS due, a forged job does not turn the missing run into a pass.
		res, _, _ = gradeSchedule(t, [][]map[string]any{nil, {forged}}, "90s", cron)
		testutil.Equal(t, res.Status, "ERROR")
	})
	t.Run("a job owned by another cronjob", func(t *testing.T) {
		other := firedJob(t, "other-1")
		other["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"kind": "CronJob", "name": "other", "uid": "cron-9", "controller": true}}
		res, _, _ := gradeSchedule(t, [][]map[string]any{nil, {other}}, "90s", cronNever(t))
		testutil.Equal(t, res.Status, "FAIL")
	})
	t.Run("jobs from before the window do not count", func(t *testing.T) {
		old := firedJob(t, "nightly-old")
		res, _, _ := gradeSchedule(t, [][]map[string]any{{old}, {old}, {old}}, "90s", cronNever(t))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "does not fire")
	})
	t.Run("an old job and a new one: only the new one counts", func(t *testing.T) {
		old := firedJob(t, "nightly-old")
		res, _, _ := gradeSchedule(t, [][]map[string]any{{old}, {old}, {old, firedJob(t, "nightly-new")}}, "90s", cron)
		testutil.Equal(t, res.Status, "PASS")
	})
	t.Run("suspended cronjob can never fire", func(t *testing.T) {
		res, r, clock := gradeSchedule(t, [][]map[string]any{nil}, "90s", cronDoc(t, "* * * * *", `,"suspend":true`))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "suspended")
		testutil.Equal(t, clock.slept() < 5*time.Second, true) // no pointless waiting
		testutil.Equal(t, r.calls <= 1, true)
	})
	t.Run("missing cronjob", func(t *testing.T) {
		r := &seqReader{fakeReader: newReader(), steps: [][]map[string]any{nil}}
		eng, _ := artifactEngine(grader.ArtifactConfig{Reader: r}, newClock())
		ev, err := eng.Evaluate(context.Background(), artifactTarget("", scheduleCrit("90s")))
		testutil.NoError(t, err)
		testutil.Equal(t, ev.Results[0].Status, "FAIL")
	})
	t.Run("listing failure is an error", func(t *testing.T) {
		r := newReader().put(cronRef, cron)
		r.listErr = fmt.Errorf("etcd timeout")
		eng, _ := artifactEngine(grader.ArtifactConfig{Reader: r}, newClock())
		ev, err := eng.Evaluate(context.Background(), artifactTarget("", scheduleCrit("90s")))
		testutil.NoError(t, err)
		testutil.Equal(t, ev.Results[0].Status, "ERROR")
	})
	t.Run("the cronjob replaced during the window", func(t *testing.T) {
		other := cronDoc(t, "* * * * *", "")
		other["metadata"].(map[string]any)["uid"] = "cron-2"
		r := &seqReader{fakeReader: newReader().put(cronRef, cron, other), steps: [][]map[string]any{nil}}
		eng, _ := artifactEngine(grader.ArtifactConfig{Reader: r}, newClock())
		ev, err := eng.Evaluate(context.Background(), artifactTarget("", scheduleCrit("90s")))
		testutil.NoError(t, err)
		testutil.Equal(t, ev.Results[0].Status, "ERROR")
	})
}

func TestCronScheduleWindowIsExplicit(t *testing.T) {
	ck := grader.NewArtifactChecker(grader.ArtifactConfig{})
	mk := func(args map[string]any, timeout time.Duration) model.Criterion {
		c := scheduleCrit("60s")
		c.Args = args
		c.Timeout = timeout
		return c
	}
	bad := map[string]model.Criterion{
		"no window":         mk(map[string]any{}, 100*time.Second),
		"window above 120s": mk(map[string]any{"window": "130s"}, 300*time.Second),
		"window below 5s":   mk(map[string]any{"window": "2s"}, 100*time.Second),
		"not a duration":    mk(map[string]any{"window": "soon"}, 100*time.Second),
		"timeout shorter":   mk(map[string]any{"window": "90s"}, 95*time.Second),
		"default timeout":   mk(map[string]any{"window": "90s"}, 0),
		"too many runs":     mk(map[string]any{"window": "60s", "min_runs": 9}, 100*time.Second),
		"unknown key":       mk(map[string]any{"window": "60s", "bogus": 1}, 100*time.Second),
		"wrong target kind": func() model.Criterion { c := scheduleCrit("60s"); c.Target = jobRef; return c }(),
	}
	for name, c := range bad {
		if err := ck.ValidateCriterion(c); !isInvalid(err) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	testutil.NoError(t, ck.ValidateCriterion(mk(map[string]any{"schedule": "* * * * *", "window": "70s"}, 80*time.Second)))
}

func TestJobArgsAreStrict(t *testing.T) {
	ck := grader.NewArtifactChecker(grader.ArtifactConfig{})
	bad := map[string]model.Criterion{
		"unknown key":        jobCrit(map[string]any{"bogus": 1}),
		"bad outcome":        jobCrit(map[string]any{"outcome": "maybe"}),
		"reason on complete": jobCrit(map[string]any{"reason": "BackoffLimitExceeded"}),
		"negative count":     jobCrit(map[string]any{"succeeded": -1}),
		"not a job":          func() model.Criterion { c := jobCrit(nil); c.Target = cronRef; return c }(),
		"template args":      cronTemplateCrit(map[string]any{}),
		"bad policy":         cronTemplateCrit(map[string]any{"concurrency_policy": "Sometimes"}),
		"bad cron":           cronTemplateCrit(map[string]any{"schedule": "61 * * * *"}),
		"template on job": func() model.Criterion {
			c := cronTemplateCrit(map[string]any{"schedule": "* * * * *"})
			c.Target = jobRef
			return c
		}(),
	}
	for name, c := range bad {
		if err := ck.ValidateCriterion(c); !isInvalid(err) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// A schedule that was due inside the window but produced no controller Job is
// not proof of a wrong answer: the controller may be late or have skipped the
// run for a reason the CronJob does not show (spec 8.1: unproven cause is ERROR).
func TestCronScheduleDueButLateIsNotAFail(t *testing.T) {
	t.Run("due and late is an error", func(t *testing.T) {
		res, _, _ := gradeSchedule(t, [][]map[string]any{nil}, "90s", cronDoc(t, "* * * * *", ""))
		testutil.Equal(t, res.Status, "ERROR")
		reasonHas(t, res, "insufficient evidence", "was due")
	})
	t.Run("due only at the very end of the window is an error too", func(t *testing.T) {
		// The fake clock starts at 00:00:00 UTC; the run is due at 00:01:00,
		// 15s before a 75s window ends and well inside the controller's slack.
		res, _, _ := gradeSchedule(t, [][]map[string]any{nil}, "75s", cronDoc(t, "* * * * *", ""))
		testutil.Equal(t, res.Status, "ERROR")
	})
	t.Run("due run that appeared on time passes", func(t *testing.T) {
		res, _, _ := gradeSchedule(t, [][]map[string]any{nil, nil, {firedJob(t, "nightly-1")}}, "90s", cronDoc(t, "* * * * *", ""))
		testutil.Equal(t, res.Status, "PASS")
	})
	t.Run("not due in the window is the learner's schedule", func(t *testing.T) {
		res, _, clock := gradeSchedule(t, [][]map[string]any{nil}, "90s", cronDoc(t, "*/5 * * * *", ""))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "does not fire within 90s", "*/5 * * * *")
		testutil.Equal(t, clock.slept() < 5*time.Second, true) // nothing to wait for
	})
	t.Run("the time zone of the CronJob decides when it is due", func(t *testing.T) {
		// 00:00:00 UTC is 09:00:00 in Seoul: "1 9 * * *" in Asia/Seoul is due
		// at 09:01 local, inside the window; in UTC it would not be.
		seoul := cronDoc(t, "1 9 * * *", `,"timeZone":"Asia/Seoul"`)
		res, _, _ := gradeSchedule(t, [][]map[string]any{nil}, "90s", seoul)
		testutil.Equal(t, res.Status, "ERROR")
		utc := cronDoc(t, "1 9 * * *", "")
		res, _, _ = gradeSchedule(t, [][]map[string]any{nil}, "90s", utc)
		testutil.Equal(t, res.Status, "FAIL")
	})
	t.Run("an unknown time zone cannot be judged", func(t *testing.T) {
		res, _, _ := gradeSchedule(t, [][]map[string]any{nil}, "90s", cronDoc(t, "* * * * *", `,"timeZone":"Mars/Olympus"`))
		testutil.Equal(t, res.Status, "ERROR")
	})
}
