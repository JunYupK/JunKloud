//go:build linux

package author_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/store"
)

// fakeCluster is the in-memory stand-in for the Manager, the janitor and the
// grader engine of the validator's unit tests. It keeps just enough cluster
// state (namespaces and objects) for the partial-credit fixture's resource,
// field and ready criteria to be evaluated from what was applied, so the
// validator's own flow is what is under test.
type fakeCluster struct {
	mu         sync.Mutex
	store      *store.Store
	namespaces map[string]bool
	objects    map[string]map[string]any

	// Behaviour knobs.
	leak       func(key string) bool                  // objects that survive Cleanup
	errorOn    map[string]bool                        // criterion IDs graded ERROR
	loose      map[string]bool                        // criterion IDs that always PASS (a lax grader)
	applyErr   func(n int, o environment.Owner) error // error for the n-th Apply (1-based)
	cleanupErr error
	onApply    func(n int)
	onEvaluate func(t grader.Target) // runs inside Evaluate (the lock is held)

	// Observations.
	applies  []applyCall
	cleanups []environment.Owner
	cleanCtx []error // ctx.Err() seen by each Cleanup
	evals    int
	statics  [][]model.Criterion
}

type applyCall struct {
	Owner   environment.Owner
	Actions []environment.Action
}

func newFakeCluster(s *store.Store) *fakeCluster {
	return &fakeCluster{store: s, namespaces: map[string]bool{}, objects: map[string]map[string]any{}}
}

func objKey(ns, kind, name string) string { return ns + "/" + kind + "/" + name }

func (c *fakeCluster) Apply(ctx context.Context, id model.Identity, owner environment.Owner, actions []environment.Action) (model.Inventory, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applies = append(c.applies, applyCall{Owner: owner, Actions: actions})
	n := len(c.applies)
	if c.onApply != nil {
		c.onApply(n)
	}
	inv := owner.Inventory
	if c.applyErr != nil {
		if err := c.applyErr(n, owner); err != nil {
			return inv, err
		}
	}
	if err := ctx.Err(); err != nil {
		return inv, err
	}
	if owner.CreateNamespaces {
		for _, ns := range append([]string{owner.Namespace}, owner.Scope.Namespaces...) {
			if !c.namespaces[ns] {
				c.namespaces[ns] = true
				inv.Resources = append(inv.Resources, model.OwnedResource{Ref: model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: ns}, UID: "uid-" + ns})
			}
		}
	}
	for _, a := range actions {
		for _, o := range a.Objects {
			meta := o["metadata"].(map[string]any)
			ns, _ := meta["namespace"].(string)
			if ns == "" {
				ns = owner.Namespace
			}
			if !c.namespaces[ns] {
				return inv, fmt.Errorf("%w: %q", environment.ErrNamespaceMissing, ns)
			}
			kind, name := o["kind"].(string), meta["name"].(string)
			c.objects[objKey(ns, kind, name)] = o
			inv.Resources = append(inv.Resources, model.OwnedResource{Ref: model.ObjectRef{APIVersion: o["apiVersion"].(string), Kind: kind, Namespace: ns, Name: name}, UID: "uid-" + name})
		}
	}
	if err := c.store.PutInventory(ctx, inv); err != nil {
		return inv, err
	}
	return inv, nil
}

func (c *fakeCluster) Cleanup(ctx context.Context, owner environment.Owner) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleanups = append(c.cleanups, owner)
	c.cleanCtx = append(c.cleanCtx, ctx.Err())
	if c.cleanupErr != nil {
		return c.cleanupErr
	}
	for _, ns := range append([]string{owner.Namespace}, owner.Scope.Namespaces...) {
		delete(c.namespaces, ns)
		for k := range c.objects {
			if strings.HasPrefix(k, ns+"/") && (c.leak == nil || !c.leak(k)) {
				delete(c.objects, k)
			}
		}
	}
	return nil
}

func (c *fakeCluster) Validate(criteria []model.Criterion) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statics = append(c.statics, criteria)
	return nil
}

// Evaluate grades the fixture's criteria the way the real checkers would for
// these kinds, including "a failed prerequisite fails its dependents".
func (c *fakeCluster) Evaluate(ctx context.Context, t grader.Target) (model.Evaluation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evals++
	if err := ctx.Err(); err != nil {
		return model.Evaluation{}, err
	}
	if c.onEvaluate != nil {
		c.onEvaluate(t)
	}
	statuses := map[string]string{}
	ev := model.Evaluation{Complete: true}
	score := 0
	for _, cr := range t.Problem.Definition.Criteria {
		status := model.StatusPass
		switch {
		case slices.ContainsFunc(cr.DependsOn, func(d string) bool { return statuses[d] != model.StatusPass }):
			status = model.StatusFail
		case c.errorOn[cr.ID]:
			status = model.StatusError
		case c.loose[cr.ID]:
		default:
			if !c.holds(cr) {
				status = model.StatusFail
			}
		}
		statuses[cr.ID] = status
		res := model.CheckResult{CriterionID: cr.ID, Status: status, Reason: "fake " + status}
		if status == model.StatusPass {
			res.Points = cr.Points
			score += cr.Points
		}
		if status == model.StatusError {
			ev.Complete = false
		}
		ev.Results = append(ev.Results, res)
		ev.MaxScore += cr.Points
	}
	if ev.Complete {
		ev.Score = &score
	}
	return ev, nil
}

func (c *fakeCluster) holds(cr model.Criterion) bool {
	ref := cr.Target
	obj, ok := c.objects[objKey(ref.Namespace, ref.Kind, ref.Name)]
	if !ok {
		return false
	}
	switch cr.Kind {
	case model.CriterionResource:
		return true
	case model.CriterionReady:
		want, _ := toInt(cr.Args["replicas"])
		have, _ := toInt(obj["spec"].(map[string]any)["replicas"])
		return want == have
	case model.CriterionField:
		field := cr.Args["fields"].([]any)[0].(map[string]any)
		spec := obj["spec"].(map[string]any)
		switch field["path"] {
		case "spec.replicas":
			want, _ := toInt(field["equals"])
			have, _ := toInt(spec["replicas"])
			return want == have
		case "spec.template.spec.containers[name=web].image":
			containers := spec["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
			image := containers[0].(map[string]any)["image"].(string)
			return strings.TrimPrefix(image, "docker.io/library/") == strings.TrimPrefix(field["equals"].(string), "docker.io/library/")
		}
	}
	return false
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	}
	return 0, false
}

func (c *fakeCluster) has(ns, kind, name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.objects[objKey(ns, kind, name)]
	return ok
}
