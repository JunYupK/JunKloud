package grader_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func TestCronScheduleRequireSuccess(t *testing.T) {
	finished := func(name string, success bool) map[string]any {
		j := firedJob(t, name)
		j["spec"] = map[string]any{"completions": 1}
		if success {
			j["status"] = map[string]any{"succeeded": 1, "conditions": []any{map[string]any{"type": "Complete", "status": "True"}}}
		} else {
			j["status"] = map[string]any{"failed": 1, "conditions": []any{map[string]any{"type": "Failed", "status": "True", "reason": "BackoffLimitExceeded"}}}
		}
		return j
	}
	for _, tc := range []struct {
		name    string
		steps   [][]map[string]any
		want    string
		minWait time.Duration
	}{
		{"fresh scheduled completion", [][]map[string]any{nil, {firedJob(t, "new")}, {finished("new", true)}}, "PASS", 2 * time.Second},
		{"fresh scheduled failure", [][]map[string]any{nil, {finished("new", false)}}, "FAIL", time.Second},
		{"creation alone", [][]map[string]any{nil, {firedJob(t, "new")}}, "ERROR", 90 * time.Second},
		{"old successful job", [][]map[string]any{{finished("old", true)}, {finished("old", true)}}, "ERROR", 90 * time.Second},
		{"manual successful job", [][]map[string]any{nil, {func() map[string]any {
			j := manualJob(t, "manual")
			j["status"] = finished("x", true)["status"]
			return j
		}()}}, "ERROR", 90 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newClock()
			r := &seqReader{fakeReader: newReader().put(cronRef, cronDoc(t, "* * * * *", "")), steps: tc.steps}
			engine, _ := artifactEngine(grader.ArtifactConfig{Reader: r}, clock)
			c := scheduleCrit("90s")
			c.Args["require_success"] = true
			ev, err := engine.Evaluate(context.Background(), artifactTarget("", c))
			testutil.NoError(t, err)
			testutil.Equal(t, ev.Results[0].Status, tc.want)
			if clock.slept() < tc.minWait || clock.slept() > 90*time.Second {
				t.Fatalf("observation duration %v, want [%v,90s]", clock.slept(), tc.minWait)
			}
		})
	}
	t.Run("API failure", func(t *testing.T) {
		r := newReader().put(cronRef, cronDoc(t, "* * * * *", ""))
		r.listErr = fmt.Errorf("API unavailable")
		c := scheduleCrit("90s")
		c.Args["require_success"] = true
		result := gradeOne(t, grader.ArtifactConfig{Reader: r}, "", c)
		testutil.Equal(t, result.Status, "ERROR")
		reasonHas(t, result, "API unavailable")
	})
	t.Run("no extra completion window", func(t *testing.T) {
		steps := make([][]map[string]any, 92)
		for i := 1; i < len(steps); i++ {
			steps[i] = []map[string]any{firedJob(t, "late")}
		}
		steps[91] = []map[string]any{finished("late", true)}
		clock := newClock()
		r := &seqReader{fakeReader: newReader().put(cronRef, cronDoc(t, "* * * * *", "")), steps: steps}
		engine, _ := artifactEngine(grader.ArtifactConfig{Reader: r}, clock)
		c := scheduleCrit("90s")
		c.Args["require_success"] = true
		ev, err := engine.Evaluate(context.Background(), artifactTarget("", c))
		testutil.NoError(t, err)
		testutil.Equal(t, ev.Results[0].Status, "ERROR")
		testutil.Equal(t, clock.slept(), 90*time.Second)
	})
}
