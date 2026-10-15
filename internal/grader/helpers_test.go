package grader_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
)

// fakeClock never sleeps: Sleep advances the clock.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 15, 9, 0, 0, 0, time.FixedZone("KST", 9*3600))}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.sleeps = append(c.sleeps, d)
	return nil
}

func (c *fakeClock) slept() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var total time.Duration
	for _, d := range c.sleeps {
		total += d
	}
	return total
}

// fakeReader serves objects from memory. steps, when set for a ref, are
// served in order (the last one repeats); an entry may be an error or nil
// (not found) instead of an object.
type fakeReader struct {
	mu    sync.Mutex
	steps map[string][]any
	calls map[string]int
}

func newReader() *fakeReader {
	return &fakeReader{steps: map[string][]any{}, calls: map[string]int{}}
}

func key(ref model.ObjectRef) string {
	return ref.APIVersion + "/" + ref.Kind + "/" + ref.Namespace + "/" + ref.Name
}

// put serves objs in order for the object's own reference.
func (r *fakeReader) put(ref model.ObjectRef, steps ...any) *fakeReader {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps[key(ref)] = steps
	return r
}

func (r *fakeReader) callsFor(ref model.ObjectRef) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[key(ref)]
}

func (r *fakeReader) Get(_ context.Context, ref model.ObjectRef) (*unstructured.Unstructured, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := key(ref)
	n := r.calls[k]
	r.calls[k]++
	steps := r.steps[k]
	if len(steps) == 0 {
		return nil, fmt.Errorf("%w: %s", grader.ErrNotFound, ref.Name)
	}
	if n >= len(steps) {
		n = len(steps) - 1
	}
	switch v := steps[n].(type) {
	case nil:
		return nil, fmt.Errorf("%w: %s", grader.ErrNotFound, ref.Name)
	case error:
		return nil, v
	case map[string]any:
		return &unstructured.Unstructured{Object: v}, nil
	default:
		panic(fmt.Sprintf("bad step %T", v))
	}
}

// obj decodes JSON into an unstructured-shaped map (numbers as int64 or
// float64 the way client-go delivers them).
func obj(t testing.TB, doc string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return fixInts(m).(map[string]any)
}

// fixInts turns whole float64 numbers into int64 like the Kubernetes JSON
// decoder does, so fixtures look like live objects.
func fixInts(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = fixInts(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = fixInts(e)
		}
		return x
	case float64:
		if x == float64(int64(x)) {
			return int64(x)
		}
	}
	return v
}

func refOf(apiVersion, kind, ns, name string) model.ObjectRef {
	return model.ObjectRef{APIVersion: apiVersion, Kind: kind, Namespace: ns, Name: name}
}

var deployRef = refOf("apps/v1", "Deployment", "jk-p1", "web")

func target(criteria ...model.Criterion) grader.Target {
	return grader.Target{
		Identity: model.Identity{ID: "0123456789ab", Context: "junkloud-0123456789ab"},
		Problem: model.FrozenProblem{
			Definition: model.Problem{ID: "p1", Namespace: "jk-p1", Criteria: criteria},
			Namespace:  "jk-p1",
		},
	}
}

func crit(id, kind string, points int, ref model.ObjectRef, args map[string]any, deps ...string) model.Criterion {
	return model.Criterion{ID: id, Kind: kind, Target: ref, Points: points, Args: args, DependsOn: deps}
}

func statusOf(res []model.CheckResult) map[string]string {
	out := map[string]string{}
	for _, r := range res {
		out[r.CriterionID] = r.Status
	}
	return out
}

func resultFor(t testing.TB, ev model.Evaluation, id string) model.CheckResult {
	t.Helper()
	for _, r := range ev.Results {
		if r.CriterionID == id {
			return r
		}
	}
	t.Fatalf("no result for %s in %+v", id, ev.Results)
	return model.CheckResult{}
}
