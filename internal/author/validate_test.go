//go:build linux

package author_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/lock"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

var ctx = context.Background()

type harness struct {
	t        *testing.T
	cluster  *fakeCluster
	store    *store.Store
	lockPath string
	id       model.Identity
	envHash  string
	prepared int
	v        *author.Validator
	prepare  func(context.Context) error
	provided []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "junkloud.db"))
	testutil.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	h := &harness{
		t: t, store: s, cluster: newFakeCluster(s), lockPath: filepath.Join(dir, "env.lock"),
		id:       model.Identity{ID: "0123456789ab", ClusterName: "junkloud-0123456789ab", Context: "junkloud-0123456789ab"},
		envHash:  author.EnvironmentHash(manifest),
		provided: []string{environment.ProbeImage.Name},
	}
	return h
}

// build creates the validator; call it after setting up the cluster knobs.
func (h *harness) build() *author.Validator {
	h.t.Helper()
	v, err := author.NewValidator(author.Config{
		Identity: h.id, Env: h.cluster, Janitor: h.cluster, Engine: h.cluster, Store: h.store,
		LockPath: h.lockPath, GraderVersion: grader.GraderVersion, EnvironmentHash: h.envHash,
		ProvidedImages: h.provided,
		Prepare: func(c context.Context) error {
			h.prepared++
			if h.prepare != nil {
				return h.prepare(c)
			}
			return nil
		},
	})
	testutil.NoError(h.t, err)
	h.v = v
	return v
}

func (h *harness) validate(p model.Problem, seeds ...uint64) (model.ValidationRecord, error) {
	h.t.Helper()
	if h.v == nil {
		h.build()
	}
	return h.v.Validate(ctx, p, seeds)
}

func (h *harness) saved(p model.Problem) (model.ValidationRecord, error) {
	h.t.Helper()
	hash, err := problem.Hash(p)
	testutil.NoError(h.t, err)
	return h.store.GetValidation(ctx, hash, grader.GraderVersion, h.envHash)
}

func (h *harness) noLeftovers() {
	h.t.Helper()
	h.cluster.mu.Lock()
	defer h.cluster.mu.Unlock()
	if len(h.cluster.namespaces) != 0 || len(h.cluster.objects) != 0 {
		h.t.Errorf("validation left namespaces %v objects %v", h.cluster.namespaces, h.cluster.objects)
	}
	inv, err := h.store.GetInventory(ctx, h.id.ID, author.SessionID, "scale-web")
	if err == nil && len(inv.Resources) != 0 {
		h.t.Errorf("stored inventory not cleared: %+v", inv.Resources)
	}
}

func statusesOf(ev model.Evaluation) map[string]string {
	out := map[string]string{}
	for _, r := range ev.Results {
		out[r.CriterionID] = r.Status
	}
	return out
}

func TestInitialSolvedWrongReset(t *testing.T) {
	h := newHarness(t)
	p := loadFixture(t)
	hash, err := problem.Hash(p)
	testutil.NoError(t, err)

	record, err := h.validate(p, 1)
	testutil.NoError(t, err)

	testutil.Equal(t, record.Passed, true)
	testutil.Equal(t, record.ProblemHash, hash)
	testutil.Equal(t, record.GraderVersion, grader.GraderVersion)
	testutil.Equal(t, record.EnvironmentHash, h.envHash)
	testutil.Equal(t, record.Seeds, []uint64{1})

	testutil.Equal(t, record.Cases["initial"].MaxScore, 10)
	testutil.Equal(t, *record.Cases["initial"].Score, 2)
	testutil.Equal(t, *record.Cases["solved"].Score, 10)
	testutil.Equal(t, *record.Cases[author.CaseKey("wrong-replicas")].Score, 6)
	testutil.Equal(t, *record.Cases[author.CaseKey("alt-short-image")].Score, 10) // the alternative answer
	testutil.Equal(t, *record.Cases["reset"].Score, 2)                            // initial again after a solved run
	testutil.Equal(t, len(record.Cases), 5)

	testutil.Equal(t, statusesOf(record.Cases["initial"]), p.InitialExpected)
	testutil.Equal(t, statusesOf(record.Cases[author.CaseKey("wrong-replicas")]), p.Cases[0].Expected)
	for _, r := range record.Cases["solved"].Results {
		testutil.Equal(t, r.Status, model.StatusPass)
	}

	// The certificate is stored and matches exactly the validated triple.
	got, err := h.saved(p)
	testutil.NoError(t, err)
	testutil.Equal(t, author.CertificateMatches(got, hash, grader.GraderVersion, h.envHash), true)
	testutil.Equal(t, *got.Cases["solved"].Score, 10)

	// Nothing is left behind: namespaces, objects or recorded inventory.
	h.noLeftovers()
	testutil.Equal(t, h.prepared, 1)
}

// Every apply goes through the Manager with the owner and scope the problem
// declares; only setup creates namespaces (Task 4 rule, R13).
func TestValidationAppliesWithOwnerAndScope(t *testing.T) {
	h := newHarness(t)
	p := loadFixture(t)
	p.Scope = model.ResourceScope{Namespaces: []string{"jk-val-extra"}}
	frozen, err := problem.Freeze(p, 7)
	testutil.NoError(t, err)

	_, err = h.validate(p, 7)
	testutil.NoError(t, err)

	// initial: 1 setup; solved: setup + solution; two cases: setup + case; reset: 1 setup.
	testutil.Equal(t, len(h.cluster.applies), 1+2+2*2+1)
	for i, call := range h.cluster.applies {
		o := call.Owner
		testutil.Equal(t, o.Inventory.EnvironmentID, h.id.ID)
		testutil.Equal(t, o.Inventory.SessionID, author.SessionID)
		testutil.Equal(t, o.Inventory.ProblemID, "scale-web")
		testutil.Equal(t, o.Namespace, frozen.Namespace)
		testutil.Equal(t, o.Scope.Namespaces, []string{"jk-val-extra"})
		isSetup := len(call.Actions) == 1 && len(call.Actions[0].Objects) == 1 && call.Actions[0].Objects[0]["kind"] == "ConfigMap"
		testutil.Equal(t, o.CreateNamespaces, isSetup)
		if !isSetup && len(o.Inventory.Resources) == 0 {
			t.Errorf("apply %d: later step lost the owner's inventory", i)
		}
	}
	// Cleanup works on the same bundle, never on anything else.
	if len(h.cluster.cleanups) == 0 {
		t.Fatal("no cleanup ran")
	}
	for _, o := range h.cluster.cleanups {
		testutil.Equal(t, o.Namespace, frozen.Namespace)
		testutil.Equal(t, o.Scope.Namespaces, []string{"jk-val-extra"})
		testutil.Equal(t, o.Inventory.SessionID, author.SessionID)
	}
	// The grader sees the rendered problem, identity and parameters.
	testutil.Equal(t, h.cluster.evals, 5)
	h.cluster.mu.Lock()
	defer h.cluster.mu.Unlock()
	if len(h.cluster.statics) == 0 {
		t.Error("criteria were not checked statically before touching the cluster")
	}
}

func TestMultipleSeedsAreEachProven(t *testing.T) {
	p := loadFullFixture(t)
	var a, b uint64
	var fa, fb model.FrozenProblem
	for seed := uint64(1); seed < 200; seed++ {
		f, err := problem.Freeze(p, seed)
		testutil.NoError(t, err)
		if a == 0 {
			a, fa = seed, f
		} else if f.Namespace != fa.Namespace && f.Parameters["replicas"] != fa.Parameters["replicas"] {
			b, fb = seed, f
			break
		}
	}
	if b == 0 {
		t.Fatal("no two seeds with different namespaces and replicas")
	}
	h := newHarness(t)
	record, err := h.validate(p, a, b)
	testutil.NoError(t, err)
	testutil.Equal(t, record.Passed, true)
	testutil.Equal(t, record.Seeds, []uint64{a, b})

	// The first seed keeps the plain keys; later seeds are prefixed.
	for _, key := range []string{"initial", "solved", "reset", author.CaseKey("wrong-replicas")} {
		if _, ok := record.Cases[key]; !ok {
			t.Errorf("missing %q", key)
		}
		if _, ok := record.Cases[author.SeedKey(b, key)]; !ok {
			t.Errorf("missing %q", author.SeedKey(b, key))
		}
	}
	testutil.Equal(t, len(record.Cases), 10)
	namespaces := map[string]bool{}
	for _, call := range h.cluster.applies {
		namespaces[call.Owner.Namespace] = true
	}
	testutil.Equal(t, namespaces, map[string]bool{fa.Namespace: true, fb.Namespace: true})
	h.noLeftovers()
}

func TestSeedsRequired(t *testing.T) {
	h := newHarness(t)
	p := loadFixture(t)
	_, err := h.validate(p)
	testutil.ErrorIs(t, err, author.ErrNoSeeds)
	_, err = h.validate(p, 3, 4, 3)
	testutil.ErrorIs(t, err, author.ErrNoSeeds)
	testutil.Equal(t, len(h.cluster.applies), 0)
	testutil.Equal(t, h.prepared, 0)
}

func notValid(t *testing.T, err error, wants ...string) *author.ValidationError {
	t.Helper()
	testutil.ErrorIs(t, err, author.ErrNotValid)
	var verr *author.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("not a *ValidationError: %v", err)
	}
	for _, w := range wants {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q lacks %q", err, w)
		}
	}
	return verr
}

func TestInitiallyPerfectRejected(t *testing.T) {
	t.Run("setup already solves it", func(t *testing.T) {
		h := newHarness(t)
		p := loadFixture(t)
		p.Setup = append(slices.Clone(p.Setup), p.Solution...) // the answer ships in setup
		record, err := h.validate(p, 1)
		notValid(t, err, "scale-web", "initial", "full marks")

		testutil.Equal(t, record.Passed, false)
		testutil.Equal(t, *record.Cases["initial"].Score, 10)
		got, err := h.saved(p)
		testutil.NoError(t, err)
		testutil.Equal(t, got.Passed, false)
		hash, _ := problem.Hash(p)
		testutil.Equal(t, author.CertificateMatches(got, hash, grader.GraderVersion, h.envHash), false)
		h.noLeftovers()
	})
	t.Run("declared to match is still rejected", func(t *testing.T) {
		h := newHarness(t)
		p := loadFixture(t)
		p.Setup = append(slices.Clone(p.Setup), p.Solution...)
		p.InitialExpected = map[string]string{"config-exists": "PASS", "web-exists": "PASS", "web-image": "PASS", "web-replicas": "PASS", "web-ready": "PASS"}
		p.InitialScore = 10
		record, err := h.validate(p, 1)
		notValid(t, err, "full marks")
		testutil.Equal(t, record.Passed, false)
		testutil.Equal(t, statusesOf(record.Cases["initial"]), p.InitialExpected) // it matched; the rule is separate
	})
	t.Run("a lax grader that passes the initial state", func(t *testing.T) {
		h := newHarness(t)
		h.cluster.loose = map[string]bool{"web-exists": true, "web-image": true, "web-replicas": true, "web-ready": true}
		_, err := h.validate(loadFixture(t), 1)
		notValid(t, err, "initial", "web-exists")
	})
}

func TestWrongCasesMustFailExactly(t *testing.T) {
	t.Run("lax grader passes the planted error", func(t *testing.T) {
		h := newHarness(t)
		h.cluster.loose = map[string]bool{"web-replicas": true, "web-ready": true}
		record, err := h.validate(loadFixture(t), 1)
		verr := notValid(t, err, "wrong-replicas", "web-replicas", "got PASS, want FAIL")
		testutil.Equal(t, record.Passed, false)
		// The planted error is also graded 10/10 instead of 6.
		testutil.Equal(t, *record.Cases[author.CaseKey("wrong-replicas")].Score, 10)
		if len(verr.Problems) == 0 {
			t.Fatal("no problems listed")
		}
		h.noLeftovers()
	})
	t.Run("declared score disagrees", func(t *testing.T) {
		h := newHarness(t)
		p := loadFixture(t)
		p.Cases = slices.Clone(p.Cases)
		p.Cases[0].ExpectedScore = 8
		_, err := h.validate(p, 1)
		notValid(t, err, "wrong-replicas", "score 6", "want 8")
	})
	t.Run("alternative answer must reach full marks", func(t *testing.T) {
		h := newHarness(t)
		h.cluster.loose = nil
		p := loadFixture(t)
		p.Cases = slices.Clone(p.Cases)
		// The alternative case is rewritten to a wrong answer but still
		// declared as an alternative.
		p.Cases[1].Actions = p.Cases[0].Actions
		_, err := h.validate(p, 1)
		notValid(t, err, "alt-short-image")
	})
	t.Run("solution does not solve", func(t *testing.T) {
		h := newHarness(t)
		p := loadFixture(t)
		p.Solution = []model.Action{{Kind: model.ActionApply, Args: map[string]any{"path": "cases/wrong-replicas.yaml"}}}
		record, err := h.validate(p, 1)
		notValid(t, err, "solved", "web-replicas")
		testutil.Equal(t, *record.Cases["solved"].Score, 6)
	})
}

// Spec 7.1 requires a planted wrong answer; a problem proven only by its
// initial state and its reference answer is accepted by any lax grader.
func TestProblemNeedsAWrongCase(t *testing.T) {
	for name, cases := range map[string]func(model.Problem) []model.Case{
		"no cases":               func(model.Problem) []model.Case { return nil },
		"only an alternative":    func(p model.Problem) []model.Case { return p.Cases[1:] },
		"only a perfect variant": func(p model.Problem) []model.Case { return p.Cases[1:2] },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			p := loadFixture(t)
			p.Cases = cases(p)
			record, err := h.validate(p, 1)
			notValid(t, err, "no wrong case")
			testutil.Equal(t, record.Passed, false)
			// Caught before anything is prepared or applied.
			testutil.Equal(t, h.prepared, 0)
			testutil.Equal(t, len(h.cluster.applies), 0)
			got, err := h.saved(p)
			testutil.NoError(t, err)
			testutil.Equal(t, got.Passed, false)
		})
	}
}

func TestResetLeakageRejected(t *testing.T) {
	h := newHarness(t)
	// A broken reset that forgets the Deployment: the solved run's state
	// reaches the next stages.
	h.cluster.leak = func(key string) bool { return strings.Contains(key, "/Deployment/") }
	record, err := h.validate(loadFixture(t), 1)
	notValid(t, err, "reset")
	testutil.Equal(t, record.Passed, false)
	// The final re-run of the initial state shows the leaked Deployment.
	testutil.Equal(t, statusesOf(record.Cases["reset"])["web-exists"], model.StatusPass)
}

func TestErrorAnywhereIsNotPassed(t *testing.T) {
	h := newHarness(t)
	h.cluster.errorOn = map[string]bool{"web-ready": true}
	record, err := h.validate(loadFixture(t), 1)
	notValid(t, err, "ERROR", "web-ready")
	testutil.Equal(t, record.Passed, false)
	// The record keeps the evaluations, with the pending score.
	solved := record.Cases["solved"]
	testutil.Equal(t, solved.Complete, false)
	if solved.Score != nil {
		t.Fatalf("an evaluation with an ERROR has score %d", *solved.Score)
	}
	testutil.Equal(t, statusesOf(solved)["web-ready"], model.StatusError)
	// Initial never reached the error (no Deployment), and is still recorded.
	testutil.Equal(t, *record.Cases["initial"].Score, 2)
	got, err := h.saved(loadFixture(t))
	testutil.NoError(t, err)
	testutil.Equal(t, got.Passed, false)
	h.noLeftovers()
}

func TestAuthorErrorsAreVerdicts(t *testing.T) {
	invalid := apierrors.NewInvalid(schema.GroupKind{Group: "apps", Kind: "Deployment"}, "web", nil)
	tests := []struct {
		name  string
		edit  func(p *model.Problem)
		apply func(n int, o environment.Owner) error
		want  []string
	}{
		{"missing manifest", func(p *model.Problem) {
			p.Cases = slices.Clone(p.Cases)
			p.Cases[0].Actions = []model.Action{{Kind: model.ActionApply, Args: map[string]any{"path": "cases/absent.yaml"}}}
		}, nil, []string{"wrong-replicas", "does not exist"}},
		{"unsupported kind", func(p *model.Problem) {
			p.Setup = []model.Action{{Kind: model.ActionHelm, Args: map[string]any{"chart": "x"}}}
		}, nil, []string{"setup", "helm"}},
		{"API rejects the manifest", nil, func(n int, o environment.Owner) error {
			if len(o.Inventory.Resources) > 0 && !o.CreateNamespaces { // the solution step
				return fmt.Errorf("environment: apply Deployment: %w", invalid)
			}
			return nil
		}, []string{"solved", "invalid"}},
		{"object outside the scope", nil, func(n int, o environment.Owner) error {
			if !o.CreateNamespaces {
				return fmt.Errorf("action 0 object 0: %w: Deployment other/web", environment.ErrOutOfScope)
			}
			return nil
		}, []string{"solved", "outside"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			p := loadFixture(t)
			if tt.edit != nil {
				tt.edit(&p)
			}
			h.cluster.applyErr = tt.apply
			record, err := h.validate(p, 1)
			notValid(t, err, tt.want...)
			testutil.Equal(t, record.Passed, false)
			h.noLeftovers()
		})
	}
}

func TestInfrastructureErrorsAreNotVerdicts(t *testing.T) {
	t.Run("environment unavailable", func(t *testing.T) {
		h := newHarness(t)
		h.cluster.applyErr = func(n int, o environment.Owner) error {
			if n == 2 {
				return fmt.Errorf("%w: connection refused", environment.ErrUnavailable)
			}
			return nil
		}
		_, err := h.validate(loadFixture(t), 1)
		testutil.ErrorIs(t, err, environment.ErrUnavailable)
		if errors.Is(err, author.ErrNotValid) {
			t.Fatal("an unavailable environment was reported as an invalid problem")
		}
		if _, err := h.saved(loadFixture(t)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a certificate verdict was stored for an infrastructure failure: %v", err)
		}
		h.noLeftovers() // the cleanup still ran
	})
	t.Run("foreign namespace in the way", func(t *testing.T) {
		h := newHarness(t)
		h.cluster.applyErr = func(n int, o environment.Owner) error {
			return fmt.Errorf("%w: namespace exists and is not marked as validation/scale-web's", environment.ErrNotOwned)
		}
		_, err := h.validate(loadFixture(t), 1)
		testutil.ErrorIs(t, err, environment.ErrNotOwned)
		if errors.Is(err, author.ErrNotValid) {
			t.Fatal("a namespace conflict was reported as an invalid problem")
		}
	})
	t.Run("cleanup fails after a passing run", func(t *testing.T) {
		h := newHarness(t)
		p := loadFixture(t)
		h.cluster.cleanupErr = errors.New("namespace stuck terminating")
		_, err := h.validate(p, 1)
		if err == nil || !strings.Contains(err.Error(), "stuck terminating") {
			t.Fatalf("err = %v", err)
		}
		if errors.Is(err, author.ErrNotValid) {
			t.Fatal("cleanup failure reported as an invalid problem")
		}
		if _, err := h.saved(p); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a record was stored although the environment could not be cleaned: %v", err)
		}
	})
	t.Run("prepare fails", func(t *testing.T) {
		h := newHarness(t)
		h.prepare = func(context.Context) error { return errors.New("preload failed") }
		_, err := h.validate(loadFixture(t), 1)
		if err == nil || !strings.Contains(err.Error(), "preload failed") {
			t.Fatalf("err = %v", err)
		}
		testutil.Equal(t, len(h.cluster.applies), 0)
	})
	t.Run("cancelled", func(t *testing.T) {
		h := newHarness(t)
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		h.cluster.onApply = func(n int) {
			if n == 3 {
				cancel()
			}
		}
		v := h.build()
		_, err := v.Validate(cctx, loadFixture(t), []uint64{1})
		testutil.ErrorIs(t, err, context.Canceled)
		// Cleanup must still run, with a context that is not cancelled.
		last := len(h.cluster.cleanCtx) - 1
		if last < 0 || h.cluster.cleanCtx[last] != nil {
			t.Fatalf("final cleanup context errors = %v", h.cluster.cleanCtx)
		}
		h.noLeftovers()
	})
}

func createSession(t *testing.T, s *store.Store, id, env, state string) {
	t.Helper()
	testutil.NoError(t, s.CreateSession(ctx, model.SessionRecord{ID: id, EnvironmentID: env, Mode: model.ModePractice, State: state}))
}

func TestValidationBlockedDuringSession(t *testing.T) {
	for _, state := range model.ActiveSessionStates() {
		t.Run(state, func(t *testing.T) {
			h := newHarness(t)
			createSession(t, h.store, "sess-1", h.id.ID, state)
			_, err := h.validate(loadFixture(t), 1)
			testutil.ErrorIs(t, err, author.ErrSessionActive)
			if !strings.Contains(err.Error(), "sess-1") || !strings.Contains(err.Error(), state) {
				t.Errorf("error %q does not name the session and its state", err)
			}
			// Nothing was prepared, applied or stored.
			testutil.Equal(t, h.prepared, 0)
			testutil.Equal(t, len(h.cluster.applies), 0)
			testutil.Equal(t, len(h.cluster.cleanups), 0)
			if _, err := h.saved(loadFixture(t)); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("a record was stored: %v", err)
			}
		})
	}
	for _, state := range []string{model.StateCompleted, model.StateAbandoned} {
		t.Run("allowed after "+state, func(t *testing.T) {
			h := newHarness(t)
			createSession(t, h.store, "sess-1", h.id.ID, state)
			createSession(t, h.store, "sess-other-env", "ffffffffffff", model.StateRunning)
			record, err := h.validate(loadFixture(t), 1)
			testutil.NoError(t, err)
			testutil.Equal(t, record.Passed, true)
		})
	}
	t.Run("another operation holds the environment lock", func(t *testing.T) {
		h := newHarness(t)
		release, err := lock.Acquire(h.lockPath)
		testutil.NoError(t, err)
		defer release()
		_, err = h.validate(loadFixture(t), 1)
		testutil.ErrorIs(t, err, lock.ErrLocked)
		testutil.Equal(t, h.prepared, 0)
		testutil.Equal(t, len(h.cluster.applies), 0)
	})
	t.Run("the lock is held for the whole validation and released after", func(t *testing.T) {
		h := newHarness(t)
		var during atomic.Int32
		h.prepare = func(context.Context) error {
			if _, err := lock.Acquire(h.lockPath); errors.Is(err, lock.ErrLocked) {
				during.Add(1)
			}
			return nil
		}
		h.cluster.onApply = func(n int) {
			if release, err := lock.Acquire(h.lockPath); errors.Is(err, lock.ErrLocked) {
				during.Add(1)
			} else if err == nil {
				release()
			}
		}
		_, err := h.validate(loadFixture(t), 1)
		testutil.NoError(t, err)
		testutil.Equal(t, int(during.Load()), 1+8) // Prepare plus each of the 8 applies
		release, err := lock.Acquire(h.lockPath)
		testutil.NoError(t, err)
		release()
	})
}

func TestNewValidatorRejectsIncompleteConfig(t *testing.T) {
	h := newHarness(t)
	full := author.Config{
		Identity: h.id, Env: h.cluster, Janitor: h.cluster, Engine: h.cluster, Store: h.store,
		LockPath: h.lockPath, GraderVersion: grader.GraderVersion, EnvironmentHash: h.envHash,
	}
	_, err := author.NewValidator(full)
	testutil.NoError(t, err) // Prepare is optional
	for name, edit := range map[string]func(*author.Config){
		"identity": func(c *author.Config) { c.Identity = model.Identity{} },
		"env":      func(c *author.Config) { c.Env = nil },
		"janitor":  func(c *author.Config) { c.Janitor = nil },
		"engine":   func(c *author.Config) { c.Engine = nil },
		"store":    func(c *author.Config) { c.Store = nil },
		"lock":     func(c *author.Config) { c.LockPath = "" },
		"grader":   func(c *author.Config) { c.GraderVersion = "" },
		"env hash": func(c *author.Config) { c.EnvironmentHash = "" },
	} {
		c := full
		edit(&c)
		if _, err := author.NewValidator(c); err == nil {
			t.Errorf("accepted a config without %s", name)
		}
	}
}

// Nothing may depend on a registry being reachable (spec 6.1): the images a
// problem's setup, solution and alternative answers use must be among the
// pinned images the environment provides. A wrong answer may name any image.
func TestImagesMustBeProvided(t *testing.T) {
	swap := func(p *model.Problem, file, image string) {
		files := map[string][]byte{}
		for k, v := range p.Files {
			files[k] = v
		}
		files[file] = []byte(strings.ReplaceAll(string(files[file]), "docker.io/library/busybox:1.37.0", image))
		files[file] = []byte(strings.ReplaceAll(string(files[file]), "image: busybox:1.37.0", "image: "+image))
		p.Files = files
	}
	tests := []struct {
		name  string
		file  string
		image string
		want  string // "" = accepted
	}{
		{"solution outside the set", "solution/web.yaml", "nginx:1.29.1", `solution: image "nginx:1.29.1"`},
		{"solution with an unpinned tag", "solution/web.yaml", "busybox", `solution: image "busybox"`},
		{"digest-only reference", "solution/web.yaml", "busybox:1.37.0@sha256:" + strings.Repeat("0", 64), "solution: image"},
		{"alternative answer outside the set", "cases/alt-short-image.yaml", "nginx:1.29.1", `case "alt-short-image"`},
		{"default registry spelling is the same image", "solution/web.yaml", "busybox:1.37.0", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			p := loadFixture(t)
			swap(&p, tt.file, tt.image)
			record, err := h.validate(p, 1)
			if tt.want == "" {
				testutil.NoError(t, err)
				testutil.Equal(t, record.Passed, true)
				return
			}
			notValid(t, err, tt.want, "not among the images the environment provides")
			testutil.Equal(t, record.Passed, false)
			// Caught statically: no cluster work.
			testutil.Equal(t, len(h.cluster.applies), 0)
			testutil.Equal(t, h.prepared, 0)
		})
	}
	t.Run("wrong answer may use any image", func(t *testing.T) {
		h := newHarness(t)
		p := loadFixture(t)
		files := map[string][]byte{}
		for k, v := range p.Files {
			files[k] = v
		}
		files["cases/wrong-replicas.yaml"] = []byte(strings.Replace(string(files["cases/wrong-replicas.yaml"]),
			"      containers:\n", "      initContainers:\n        - {name: init, image: \"nginx:1.29.1\", command: [\"true\"]}\n      containers:\n", 1))
		p.Files = files
		record, err := h.validate(p, 1)
		testutil.NoError(t, err)
		testutil.Equal(t, record.Passed, true)
	})
	t.Run("nothing is provided", func(t *testing.T) {
		h := newHarness(t)
		h.provided = []string{}
		_, err := h.validate(loadFixture(t), 1)
		notValid(t, err, `solution: image "docker.io/library/busybox:1.37.0"`)
	})
}

// TestValidateGradesInTheServedWorkspace: every stage is evaluated with a
// scratch workspace holding the frozen problem's provided files, exactly what
// a session's preparation and the learner get, so file, kustomize and archive
// criteria are judged on the same files that are certified and served. Each
// stage gets a fresh one, and none is left behind.
func TestValidateGradesInTheServedWorkspace(t *testing.T) {
	h := newHarness(t)
	p := loadFixture(t)
	p.Files = maps.Clone(p.Files)
	p.Files["setup/provided/notes.txt"] = []byte("namespace ${namespace}\n")
	f, err := problem.Freeze(p, 1)
	testutil.NoError(t, err)
	var dirs []string
	h.cluster.onEvaluate = func(tgt grader.Target) {
		dirs = append(dirs, tgt.Workspace)
		if tgt.Workspace == "" {
			t.Errorf("a stage was evaluated without a workspace")
			return
		}
		b, err := os.ReadFile(filepath.Join(tgt.Workspace, "notes.txt"))
		if err != nil {
			t.Errorf("the provided file is missing from the workspace: %v", err)
			return
		}
		testutil.Equal(t, string(b), string(f.Assets["notes.txt"]))
		entries, _ := os.ReadDir(tgt.Workspace)
		testutil.Equal(t, len(entries), len(f.Assets))
	}
	_, err = h.validate(p, 1)
	testutil.NoError(t, err)
	testutil.Equal(t, len(dirs), 3+len(p.Cases)) // initial, solved, each case, reset
	seen := map[string]bool{}
	for _, d := range dirs {
		if seen[d] {
			t.Errorf("workspace %s reused across stages", d)
		}
		seen[d] = true
		if _, err := os.Stat(d); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("scratch workspace %s left behind (%v)", d, err)
		}
	}
	if !strings.Contains(string(f.Assets["notes.txt"]), "namespace ") || strings.Contains(string(f.Assets["notes.txt"]), "${") {
		t.Fatalf("the provided file is not rendered: %q", f.Assets["notes.txt"])
	}
}

// TestCoveringSeeds: the product helper picks seeds whose frozen parameters
// together use every allowed value of every parameter.
func TestCoveringSeeds(t *testing.T) {
	p := loadFullFixture(t)
	seeds, err := author.CoveringSeeds(p)
	testutil.NoError(t, err)
	used := map[string]map[string]bool{}
	for _, seed := range seeds {
		f, err := problem.Freeze(p, seed)
		testutil.NoError(t, err)
		for name, v := range f.Parameters {
			if used[name] == nil {
				used[name] = map[string]bool{}
			}
			used[name][fmt.Sprint(v)] = true
		}
	}
	for name, values := range p.Parameters {
		testutil.Equal(t, len(used[name]), len(values))
	}
	again, err := author.CoveringSeeds(p)
	testutil.NoError(t, err)
	testutil.Equal(t, again, seeds) // deterministic
	missing, err := author.UncoveredValues(p, seeds)
	testutil.NoError(t, err)
	testutil.Equal(t, len(missing), 0)

	// Without parameters one seed is enough.
	none := p
	none.Parameters = nil
	one, err := author.CoveringSeeds(none)
	testutil.NoError(t, err)
	testutil.Equal(t, len(one), 1)
}

// TestValidationMustCoverEveryParameterValue (spec 7.1): a certificate vouches
// for the seeds it validated; a session may freeze any allowed value, so a
// validation whose seeds leave a value untried is not passed.
func TestValidationMustCoverEveryParameterValue(t *testing.T) {
	p := loadFullFixture(t)
	h := newHarness(t)
	record, err := h.validate(p, 1)
	testutil.ErrorIs(t, err, author.ErrNotValid)
	testutil.Equal(t, record.Passed, false)
	missing, merr := author.UncoveredValues(p, []uint64{1})
	testutil.NoError(t, merr)
	if len(missing) == 0 {
		t.Fatal("seed 1 alone unexpectedly covers every value")
	}
	for _, m := range missing {
		if !strings.Contains(err.Error(), m) {
			t.Errorf("the finding does not name %s: %v", m, err)
		}
	}
	if !strings.Contains(err.Error(), "CoveringSeeds") {
		t.Errorf("the finding should say how to cover the values: %v", err)
	}
	// Nothing was applied for an uncovered validation.
	testutil.Equal(t, len(h.cluster.applies), 0)

	seeds, err := author.CoveringSeeds(p)
	testutil.NoError(t, err)
	record, err = newHarness(t).validate(p, seeds...)
	testutil.NoError(t, err)
	testutil.Equal(t, record.Passed, true)
}
