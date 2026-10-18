//go:build linux

package session_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/lock"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	testutil.NoError(t, err)
	return string(b)
}

var examOrder = []string{"dev-service-001", "dev-config-001", "dev-crashloop-001", "dev-sa-001", "dev-scale-001"}

// TestPrepareHidesQuestions: a prepared session is frozen, set up and
// verified, but nothing the learner may read exists yet: no question, no
// provided file, no running clock.
func TestPrepareHidesQuestions(t *testing.T) {
	h := newHarness(t)
	prepared := h.prepareMock()

	testutil.Equal(t, prepared.State, model.StatePreparing)
	testutil.Equal(t, prepared.StartedAt, (*time.Time)(nil))
	testutil.Equal(t, prepared.Mode, model.ModeMock)
	testutil.Equal(t, prepared.DurationLimit, 120*time.Minute)
	testutil.Equal(t, prepared.EnvironmentID, testEnvID)
	testutil.Equal(t, prepared.Seed, uint64(42))
	testutil.Equal(t, viewIDs(prepared), examOrder) // the exam file's order

	publicQuestionFilesBeforeStart := countFiles(t, h.workspace)
	testutil.Equal(t, publicQuestionFilesBeforeStart, 0)

	// What the caller gets back carries no question text or provided file.
	for _, f := range prepared.Problems {
		testutil.Equal(t, f.Question, "")
		testutil.Equal(t, len(f.Assets), 0)
	}
	// The record in the store keeps both, for Start to publish.
	stored, err := h.st.LoadSession(ctx, prepared.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, stored.State, model.StatePreparing)
	testutil.Equal(t, stored.StartedAt, (*time.Time)(nil))
	for _, f := range stored.Problems {
		if !strings.HasPrefix(f.Question, "# ") {
			t.Errorf("%s: stored question = %q", f.Definition.ID, f.Question)
		}
	}
	testutil.Equal(t, len(stored.Problems[1].Assets), 1) // dev-config-001's template

	// The initial evaluations ran in a private scratch workspace holding the
	// provided files, and the scratch is gone again.
	testutil.Equal(t, h.eng.seen["dev-config-001"], []string{"settings-template.yaml"})
	testutil.Equal(t, h.eng.seen["dev-crashloop-001"], []string(nil))
	if _, err := os.Stat(filepath.Join(h.stateDir, "prepare")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(h.stateDir, "prepare"))
		testutil.Equal(t, len(entries), 0)
	}
	for _, tgt := range h.eng.targets {
		if strings.HasPrefix(tgt.Workspace, h.workspace) {
			t.Errorf("evaluated inside the learner workspace: %s", tgt.Workspace)
		}
	}
}

// TestPrepareFreezesAndSetsUp: the seed, the problem versions and hashes and
// the rendered parameters are recorded, the private package is stored by
// hash, every setup is applied for its own owner inside its declared scope,
// all of them before the first initial evaluation.
func TestPrepareFreezesAndSetsUp(t *testing.T) {
	h := newHarness(t)
	prepared := h.prepareMock()

	wantKinds := []string{"ensure", "prepare-env"}
	for range examOrder {
		wantKinds = append(wantKinds, "apply")
	}
	for range examOrder {
		wantKinds = append(wantKinds, "evaluate")
	}
	testutil.Equal(t, h.env.kinds(), wantKinds)
	testutil.Equal(t, h.env.count("doctor"), 0) // no fixture declares a capability

	for i, f := range mustLoad(t, h, prepared.ID).Problems {
		p := h.loadBankProblem(f.Definition.ID)
		hash, err := problem.Hash(p)
		testutil.NoError(t, err)
		want, err := problem.Freeze(p, 42)
		testutil.NoError(t, err)

		testutil.Equal(t, f.Hash, hash)
		testutil.Equal(t, f.Seed, uint64(42))
		testutil.Equal(t, f.SeedAlgorithm, problem.SeedAlgorithm)
		// Numbers come back from the store as float64: compare by JSON.
		testutil.Equal(t, asJSON(t, f.Parameters), asJSON(t, want.Parameters))
		testutil.Equal(t, f.Namespace, want.Namespace)
		testutil.Equal(t, f.Definition.Version, p.Version)
		testutil.Equal(t, f.Context, testIdentity.Context)

		owner := h.env.owners[i]
		testutil.Equal(t, owner.CreateNamespaces, true)
		testutil.Equal(t, owner.Namespace, want.Namespace)
		testutil.Equal(t, owner.Scope, want.Definition.Scope)
		testutil.Equal(t, owner.Inventory, model.Inventory{EnvironmentID: testEnvID, SessionID: prepared.ID, ProblemID: f.Definition.ID})

		// The private package is stored under its hash, complete.
		stored, err := session.LoadPackage(h.stateDir, hash)
		testutil.NoError(t, err)
		testutil.Equal(t, stored.Files, p.Files)
		testutil.Equal(t, stored.ID, p.ID)

		// The inventory the setup recorded is kept.
		inv, err := h.st.GetInventory(ctx, testEnvID, prepared.ID, f.Definition.ID)
		testutil.NoError(t, err)
		testutil.Equal(t, len(inv.Resources), 1)
	}

	// Setup manifests were rendered with the frozen parameters.
	cfg := h.env.acts["dev-config-001"]
	testutil.Equal(t, len(cfg), 1)
	meta := cfg[0].Objects[0]["metadata"].(map[string]any)
	wantNS := prepared.Problems[1].Namespace
	testutil.Equal(t, meta["namespace"], wantNS)

	op, err := h.st.GetOperation(ctx, session.OperationID(prepared.ID))
	testutil.NoError(t, err)
	testutil.Equal(t, op.Kind, session.OperationKind)
	testutil.Equal(t, op.Phase, session.PhaseDone)
	testutil.Equal(t, op.CreatedAt, t0)
}

// TestPrepareUsesSeedParameters: two seeds that choose different parameters
// produce differently frozen sessions, and the same seed the same one.
func TestPrepareUsesSeedParameters(t *testing.T) {
	p := newHarness(t).loadBankProblem("dev-config-001")
	var a, b uint64
	first, err := problem.Freeze(p, 1)
	testutil.NoError(t, err)
	a = 1
	for seed := uint64(2); seed < 200 && b == 0; seed++ {
		f, err := problem.Freeze(p, seed)
		testutil.NoError(t, err)
		if f.Namespace != first.Namespace {
			b = seed
		}
	}
	if b == 0 {
		t.Fatal("no seed renders a different namespace")
	}
	namespaces := func(seed uint64) []string {
		h := newHarness(t)
		opts := h.mock()
		opts.Seed = seed
		rec, err := h.svc.Prepare(ctx, opts)
		testutil.NoError(t, err)
		testutil.Equal(t, rec.Seed, seed)
		var ns []string
		for _, f := range rec.Problems {
			ns = append(ns, f.Namespace)
		}
		return ns
	}
	testutil.Equal(t, namespaces(a), namespaces(a))
	if slices.Equal(namespaces(a), namespaces(b)) {
		t.Errorf("seeds %d and %d froze the same namespaces", a, b)
	}
}

// TestBrokenInitialStateIsExpected: an intentionally broken app (the
// crash-looping Deployment) is the declared starting state; preparing it is
// not a platform fault. Only a state that differs from the declaration, or one
// that cannot be judged, fails the preparation, and then the inventory stays
// for recovery and the error does not reveal the problem's checks.
func TestBrokenInitialStateIsExpected(t *testing.T) {
	crash := func(f model.FrozenProblem) model.Evaluation {
		ev := initialEvaluation(f)
		for i := range ev.Results {
			if ev.Results[i].CriterionID == "api-ready" {
				ev.Results[i].Reason = "the pods are in CrashLoopBackOff"
			}
		}
		return ev
	}

	t.Run("CrashLoopMatchesTheDeclaration", func(t *testing.T) {
		h := newHarness(t)
		h.eng.override = map[string]func(model.FrozenProblem) model.Evaluation{"dev-crashloop-001": crash}
		prepared := h.prepareMock()
		testutil.Equal(t, prepared.State, model.StatePreparing)
		stored, err := h.st.LoadSession(ctx, prepared.ID)
		testutil.NoError(t, err)
		testutil.Equal(t, stored.State, model.StatePreparing)
		// The declaration really is a failing readiness check.
		p := h.loadBankProblem("dev-crashloop-001")
		testutil.Equal(t, p.InitialExpected["api-ready"], model.StatusFail)
		testutil.Equal(t, p.InitialScore, 2)
	})

	failing := map[string]func(model.FrozenProblem) model.Evaluation{
		"platform error": func(f model.FrozenProblem) model.Evaluation {
			ev := crash(f)
			ev.Results[1].Status = model.StatusError
			ev.Results[1].Reason = "api-ready: the grader could not decide"
			ev.Score, ev.Complete = nil, false
			return ev
		},
		"fixed by accident": func(f model.FrozenProblem) model.Evaluation {
			ev := crash(f)
			ev.Results[1].Status = model.StatusPass
			ev.Results[1].Points = 4
			six := 6
			ev.Score = &six
			return ev
		},
		"wrong score": func(f model.FrozenProblem) model.Evaluation {
			ev := crash(f)
			zero := 0
			ev.Score = &zero
			return ev
		},
		"result missing": func(f model.FrozenProblem) model.Evaluation {
			ev := crash(f)
			ev.Results = ev.Results[:1]
			return ev
		},
	}
	for name, override := range failing {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.eng.override = map[string]func(model.FrozenProblem) model.Evaluation{"dev-crashloop-001": override}
			rec, err := h.svc.Prepare(ctx, h.mock())
			testutil.ErrorIs(t, err, session.ErrSetupFailed)

			// The session is SETUP_ERROR, the clock never started, and nothing is public.
			testutil.Equal(t, rec.State, model.StateSetupError)
			stored, lerr := h.st.LoadSession(ctx, rec.ID)
			testutil.NoError(t, lerr)
			testutil.Equal(t, stored.State, model.StateSetupError)
			testutil.Equal(t, stored.StartedAt, (*time.Time)(nil))
			testutil.Equal(t, countFiles(t, h.workspace), 0)
			op, oerr := h.st.GetOperation(ctx, session.OperationID(rec.ID))
			testutil.NoError(t, oerr)
			testutil.Equal(t, op.Phase, session.PhaseFailed)

			// Every applied problem's inventory is preserved for recovery.
			for _, id := range examOrder {
				inv, ierr := h.st.GetInventory(ctx, testEnvID, rec.ID, id)
				testutil.NoError(t, ierr)
				testutil.Equal(t, len(inv.Resources), 1)
			}

			// The error names the problem but none of its checks, and the
			// details go to a private file.
			msg := err.Error()
			for _, secret := range []string{"api-ready", "api-exists", "CrashLoopBackOff", "could not decide"} {
				if strings.Contains(msg, secret) {
					t.Errorf("error reveals %q: %s", secret, msg)
				}
			}
			for _, want := range []string{"dev-crashloop-001", "SETUP_ERROR"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error lacks %q: %s", want, msg)
				}
			}
			diag, rerr := os.ReadFile(filepath.Join(h.stateDir, "diagnostics", rec.ID+"-prepare.txt"))
			testutil.NoError(t, rerr)
			if !strings.Contains(string(diag), "dev-crashloop-001") {
				t.Errorf("diagnostics lack the problem: %s", diag)
			}
			if _, err := os.Stat(filepath.Join(h.stateDir, "prepare", rec.ID)); err == nil {
				t.Error("scratch workspace left behind")
			}

			// A failed preparation still occupies the environment until it is
			// recovered, and cannot be started.
			_, err = h.svc.Prepare(ctx, h.mock())
			testutil.ErrorIs(t, err, session.ErrSessionActive)
			testutil.ErrorIs(t, h.svc.Start(ctx, rec.ID, t0), session.ErrNotPrepared)
		})
	}
}

// TestPrepareSetupFailureKeepsInventory: a setup that fails halfway leaves
// the session SETUP_ERROR with what was applied recorded, and the cause stays
// matchable.
func TestPrepareSetupFailureKeepsInventory(t *testing.T) {
	h := newHarness(t)
	boom := errors.New("api server went away")
	h.env.applyErr = func(owner environment.Owner) error {
		if owner.Inventory.ProblemID == "dev-crashloop-001" {
			return boom
		}
		return nil
	}
	rec, err := h.svc.Prepare(ctx, h.mock())
	testutil.ErrorIs(t, err, session.ErrSetupFailed)
	testutil.ErrorIs(t, err, boom)
	if strings.Contains(err.Error(), "api server went away") {
		t.Errorf("the platform error text is private diagnostics, not the message: %v", err)
	}
	testutil.Equal(t, rec.State, model.StateSetupError)
	testutil.Equal(t, h.env.count("evaluate"), 0)
	for _, id := range examOrder[:3] { // up to and including the failing one
		inv, ierr := h.st.GetInventory(ctx, testEnvID, rec.ID, id)
		testutil.NoError(t, ierr)
		testutil.Equal(t, len(inv.Resources), 1)
	}
	_, ierr := h.st.GetInventory(ctx, testEnvID, rec.ID, examOrder[3])
	testutil.ErrorIs(t, ierr, store.ErrNotFound)
	op, oerr := h.st.GetOperation(ctx, session.OperationID(rec.ID))
	testutil.NoError(t, oerr)
	testutil.Equal(t, op.Phase, session.PhaseFailed)
}

// TestSecondSessionRejected: one unfinished session per environment, whether
// it is prepared, running or waiting for recovery, and the check happens
// before anything is changed.
func TestSecondSessionRejected(t *testing.T) {
	h := newHarness(t)
	first := h.prepareMock()
	applies := h.env.count("apply")

	_, err := h.svc.Prepare(ctx, h.mock())
	testutil.ErrorIs(t, err, session.ErrSessionActive)
	if !strings.Contains(err.Error(), first.ID) {
		t.Errorf("error does not name the blocking session: %v", err)
	}
	testutil.Equal(t, h.env.count("apply"), applies)
	testutil.Equal(t, h.env.count("ensure"), 1)

	// Running sessions block as well.
	testutil.NoError(t, h.svc.Start(ctx, first.ID, t0))
	_, err = h.svc.Prepare(ctx, session.StartOptions{Mode: model.ModePractice, Count: 5})
	testutil.ErrorIs(t, err, session.ErrSessionActive)
	active, err := h.st.ActiveSessions(ctx, testEnvID)
	testutil.NoError(t, err)
	testutil.Equal(t, len(active), 1)

	// Once it is over and its resources are reset, a new one can be prepared.
	testutil.NoError(t, h.st.Transition(ctx, first.ID, model.StateRunning, model.StateAbandoned))
	h.env.reset(first.ID)
	second, err := h.svc.Prepare(ctx, session.StartOptions{Mode: model.ModePractice, Count: 5})
	testutil.NoError(t, err)
	if second.ID == first.ID {
		t.Fatal("second session reuses the ID")
	}
}

// TestPrepareTakesTheEnvironmentLock: a start or validation holding the lock
// makes Prepare refuse at once, with nothing recorded.
func TestPrepareTakesTheEnvironmentLock(t *testing.T) {
	h := newHarness(t)
	release, err := lock.Acquire(author.LockPath(h.stateDir))
	testutil.NoError(t, err)
	_, err = h.svc.Prepare(ctx, h.mock())
	testutil.ErrorIs(t, err, lock.ErrLocked)
	active, aerr := h.st.ActiveSessions(ctx, testEnvID)
	testutil.NoError(t, aerr)
	testutil.Equal(t, len(active), 0)
	testutil.Equal(t, len(h.env.kinds()), 0)
	testutil.NoError(t, release())

	h.prepareMock() // the lock is released again afterwards
	again, err := lock.Acquire(author.LockPath(h.stateDir))
	testutil.NoError(t, err)
	testutil.NoError(t, again())
}

// TestPrepareRequiresCurrentCertificates: only problems with a passing
// validation for this exact package, grader version and environment are
// served. A missing or stale certificate refuses the whole session with the
// reason; nothing is skipped or replaced, nothing is recorded or applied.
func TestPrepareRequiresCurrentCertificates(t *testing.T) {
	refused := func(t *testing.T, h *harness, mentions ...string) {
		t.Helper()
		_, err := h.svc.Prepare(ctx, h.mock())
		testutil.ErrorIs(t, err, session.ErrNotCertified)
		for _, m := range mentions {
			if !strings.Contains(err.Error(), m) {
				t.Errorf("error does not name %q: %v", m, err)
			}
		}
		active, aerr := h.st.ActiveSessions(ctx, testEnvID)
		testutil.NoError(t, aerr)
		testutil.Equal(t, len(active), 0)
		testutil.Equal(t, h.env.count("apply"), 0)
		testutil.Equal(t, h.env.count("prepare-env"), 0)
		testutil.Equal(t, countFiles(t, filepath.Join(h.stateDir, "packages")), 0)
	}

	t.Run("never validated", func(t *testing.T) {
		h := newHarness(t, func(o *harnessOption) { o.uncertified = []string{"dev-sa-001", "dev-scale-001"} })
		refused(t, h, "dev-sa-001", "dev-scale-001")
	})
	t.Run("validation failed", func(t *testing.T) {
		h := newHarness(t, func(o *harnessOption) { o.uncertified = []string{"dev-sa-001"} })
		h.certifyProblem(h.loadBankProblem("dev-sa-001"), testGraderVersion, testEnvHash, false)
		refused(t, h, "dev-sa-001")
	})
	t.Run("another grader version", func(t *testing.T) {
		h := newHarness(t, func(o *harnessOption) { o.uncertified = []string{"dev-sa-001"} })
		h.certifyProblem(h.loadBankProblem("dev-sa-001"), "grader-old", testEnvHash, true)
		refused(t, h, "dev-sa-001")
	})
	t.Run("another environment", func(t *testing.T) {
		h := newHarness(t, func(o *harnessOption) { o.uncertified = []string{"dev-sa-001"} })
		h.certifyProblem(h.loadBankProblem("dev-sa-001"), testGraderVersion, "sha256:other-env", true)
		refused(t, h, "dev-sa-001")
	})
	t.Run("package edited after validation", func(t *testing.T) {
		h := newHarness(t)
		path := filepath.Join(h.bank, "problems", "dev-sa-001", "solution", "builder.yaml")
		b, err := os.ReadFile(path)
		testutil.NoError(t, err)
		testutil.NoError(t, os.WriteFile(path, append(b, "# edited after validation\n"...), 0o644))
		refused(t, h, "dev-sa-001")
	})
	t.Run("a certified bank is served", func(t *testing.T) {
		h := newHarness(t)
		h.prepareMock()
	})
}

// TestPrepareChecksCapabilities: a problem declaring a capability that the
// doctor does not report as PASS is refused with the doctor's reason; the
// doctor only runs when some chosen problem declares a capability.
func TestPrepareChecksCapabilities(t *testing.T) {
	needsNetworkPolicy := func(o *harnessOption) {
		o.mutateBank = func(t *testing.T, bank string) {
			path := filepath.Join(bank, "problems", "dev-service-001", "problem.yaml")
			b, err := os.ReadFile(path)
			testutil.NoError(t, err)
			edited := strings.Replace(string(b), "capabilities: []", "capabilities: [network-policy, dns]", 1)
			testutil.Equal(t, edited != string(b), true)
			testutil.NoError(t, os.WriteFile(path, []byte(edited), 0o644))
		}
	}
	report := func(np string) environment.DoctorReport {
		return environment.DoctorReport{EnvironmentID: testEnvID, Checks: []environment.CapabilityCheck{
			{Name: environment.CapabilityDNS, Status: environment.StatusPass},
			{Name: environment.CapabilityNetworkPolicy, Status: np, Reason: "NetworkPolicy is not enforced"},
		}}
	}

	t.Run("unavailable", func(t *testing.T) {
		h := newHarness(t, needsNetworkPolicy)
		h.env.report = report(environment.StatusFail)
		_, err := h.svc.Prepare(ctx, h.mock())
		testutil.ErrorIs(t, err, session.ErrCapabilityUnavailable)
		for _, want := range []string{"dev-service-001", "network-policy", "NetworkPolicy is not enforced"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error lacks %q: %v", want, err)
			}
		}
		if strings.Contains(err.Error(), "needs dns") {
			t.Errorf("the passing capability is reported: %v", err)
		}
		testutil.Equal(t, h.env.count("doctor"), 1)
		testutil.Equal(t, h.env.count("apply"), 0)
		active, aerr := h.st.ActiveSessions(ctx, testEnvID)
		testutil.NoError(t, aerr)
		testutil.Equal(t, len(active), 0)
	})
	t.Run("missing from the report", func(t *testing.T) {
		h := newHarness(t, needsNetworkPolicy)
		h.env.report = environment.DoctorReport{}
		_, err := h.svc.Prepare(ctx, h.mock())
		testutil.ErrorIs(t, err, session.ErrCapabilityUnavailable)
	})
	t.Run("available", func(t *testing.T) {
		h := newHarness(t, needsNetworkPolicy)
		h.env.report = report(environment.StatusPass)
		h.prepareMock()
		testutil.Equal(t, h.env.count("doctor"), 1)
	})
	t.Run("doctor failure is not a pass", func(t *testing.T) {
		h := newHarness(t, needsNetworkPolicy)
		h.env.doctorErr = errors.New("api unreachable")
		_, err := h.svc.Prepare(ctx, h.mock())
		if err == nil || errors.Is(err, session.ErrCapabilityUnavailable) {
			t.Fatalf("err = %v, want the doctor's failure", err)
		}
		testutil.Equal(t, h.env.count("apply"), 0)
	})
}

// TestPrepareComposesPracticeSessions: practice sessions are drawn with
// problem.Select, have no time limit, and a zero seed means a fresh seed.
func TestPrepareComposesPracticeSessions(t *testing.T) {
	t.Run("selection and seed", func(t *testing.T) {
		h := newHarness(t)
		want, err := problem.Select(h.bankProblems(), problem.SelectOptions{Mode: model.ModePractice, Count: 5, Seed: 7})
		testutil.NoError(t, err)
		rec, err := h.svc.Prepare(ctx, session.StartOptions{Mode: model.ModePractice, Count: 5, Seed: 7})
		testutil.NoError(t, err)
		var ids []string
		for _, p := range want {
			ids = append(ids, p.ID)
		}
		testutil.Equal(t, viewIDs(rec), ids)
		testutil.Equal(t, rec.Seed, uint64(7))
		testutil.Equal(t, rec.DurationLimit, time.Duration(0))
		testutil.Equal(t, rec.Mode, model.ModePractice)
	})
	t.Run("zero seed picks one and records it", func(t *testing.T) {
		h := newHarness(t)
		rec, err := h.svc.Prepare(ctx, session.StartOptions{Mode: model.ModePractice, Count: 5})
		testutil.NoError(t, err)
		testutil.Equal(t, rec.Seed, uint64(42)) // the harness' NewSeed
		for _, f := range mustLoad(t, h, rec.ID).Problems {
			testutil.Equal(t, f.Seed, uint64(42))
		}
	})
	t.Run("too few problems", func(t *testing.T) {
		h := newHarness(t)
		for _, opts := range []session.StartOptions{
			{Mode: model.ModePractice, Count: 6},
			{Mode: model.ModePractice, Count: 5, Domain: model.DomainNetworking},
		} {
			_, err := h.svc.Prepare(ctx, opts)
			testutil.ErrorIs(t, err, problem.ErrInsufficientProblems)
		}
		testutil.Equal(t, h.env.count("apply"), 0)
		active, aerr := h.st.ActiveSessions(ctx, testEnvID)
		testutil.NoError(t, aerr)
		testutil.Equal(t, len(active), 0)
		h2 := newHarness(t)
		_, err := h2.svc.Prepare(ctx, session.StartOptions{Mode: model.ModePractice, Count: 4})
		testutil.ErrorIs(t, err, problem.ErrInvalidCount)
		testutil.Equal(t, len(h2.env.kinds()), 0) // a bad count is refused before the environment is asked
	})
	t.Run("a package that does not load is an error, not a smaller bank", func(t *testing.T) {
		h := newHarness(t)
		testutil.NoError(t, os.WriteFile(filepath.Join(h.bank, "problems", "dev-sa-001", "problem.yaml"), []byte("id: x\n"), 0o644))
		_, err := h.svc.Prepare(ctx, session.StartOptions{Mode: model.ModePractice, Count: 5})
		testutil.ErrorIs(t, err, problem.ErrInvalidDefinition)
		if !strings.Contains(err.Error(), "dev-sa-001") {
			t.Errorf("error does not name the package: %v", err)
		}
	})
}

// TestPrepareRejectsBadOptions: options that do not describe a session are
// refused before the environment is touched.
func TestPrepareRejectsBadOptions(t *testing.T) {
	h := newHarness(t)
	for name, opts := range map[string]session.StartOptions{
		"no mode":            {},
		"unknown mode":       {Mode: "speedrun"},
		"mock without exam":  {Mode: model.ModeMock},
		"mock path escape":   {Mode: model.ModeMock, MockID: "../problems/dev-sa-001/problem"},
		"mock with a filter": {Mode: model.ModeMock, MockID: "dev-mock", Domain: model.DomainNetworking},
		"mock with a count":  {Mode: model.ModeMock, MockID: "dev-mock", Count: 5},
		"practice with exam": {Mode: model.ModePractice, Count: 5, MockID: "dev-mock"},
		"practice new seed":  {Mode: model.ModePractice, Count: 5, NewSeed: true},
		"no such exam":       {Mode: model.ModeMock, MockID: "mock-99"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.svc.Prepare(ctx, opts)
			testutil.ErrorIs(t, err, session.ErrInvalidOptions)
		})
	}
	_, err := h.svc.Prepare(ctx, session.StartOptions{Mode: model.ModeRetry, RetrySessionID: "practice-1"})
	testutil.ErrorIs(t, err, store.ErrNotFound)

	// An exam naming a problem the bank lacks is refused, not shortened.
	exam := filepath.Join(h.bank, "exams", "dev-mock.yaml")
	b, err := os.ReadFile(exam)
	testutil.NoError(t, err)
	testutil.NoError(t, os.WriteFile(exam, append(b, "  - dev-missing-001\n"...), 0o644))
	_, err = h.svc.Prepare(ctx, h.mock())
	if err == nil || !strings.Contains(err.Error(), "dev-missing-001") {
		t.Fatalf("err = %v, want the missing problem named", err)
	}
	testutil.Equal(t, len(h.env.kinds()), 0)
}

// TestPrepareRefusesSharedNamespaces: two problems must not own one namespace.
func TestPrepareRefusesSharedNamespaces(t *testing.T) {
	h := newHarness(t, func(o *harnessOption) {
		o.mutateBank = func(t *testing.T, bank string) {
			path := filepath.Join(bank, "problems", "dev-sa-001", "problem.yaml")
			b, err := os.ReadFile(path)
			testutil.NoError(t, err)
			testutil.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(b), "namespace: dev-sec\n", "namespace: dev-svc\n", 1)), 0o644))
		}
	})
	_, err := h.svc.Prepare(ctx, h.mock())
	testutil.ErrorIs(t, err, session.ErrNamespaceClash)
	testutil.Equal(t, h.env.count("apply"), 0)
}

// TestPrepareChecksTheEnvironment: the live environment must be the one the
// service was composed for.
func TestPrepareChecksTheEnvironment(t *testing.T) {
	t.Run("other environment", func(t *testing.T) {
		h := newHarness(t)
		h.env.identity.ID = "env-other"
		_, err := h.svc.Prepare(ctx, h.mock())
		testutil.ErrorIs(t, err, session.ErrEnvironmentChanged)
		testutil.Equal(t, h.env.count("apply"), 0)
	})
	t.Run("ensure fails", func(t *testing.T) {
		h := newHarness(t)
		boom := errors.New("docker is not running")
		h.env.ensureErr = boom
		_, err := h.svc.Prepare(ctx, h.mock())
		testutil.ErrorIs(t, err, boom)
		active, aerr := h.st.ActiveSessions(ctx, testEnvID)
		testutil.NoError(t, aerr)
		testutil.Equal(t, len(active), 0)
	})
}

// TestPrepareFreezesTheEnvironmentManifest (spec 6.3): the grader version, the
// environment version manifest and its hash are stored on the session at
// prepare time, are exactly what the certificates were matched with, and
// survive Start unchanged.
func TestPrepareFreezesTheEnvironmentManifest(t *testing.T) {
	h := newHarness(t)
	prepared := h.prepareMock()
	testutil.Equal(t, prepared.GraderVersion, testGraderVersion)
	testutil.Equal(t, prepared.EnvironmentHash, testEnvHash)
	testutil.Equal(t, prepared.EnvironmentManifest, testManifest)
	for name, rec := range map[string]model.SessionRecord{"stored": mustLoad(t, h, prepared.ID)} {
		testutil.Equal(t, rec.GraderVersion, testGraderVersion)
		testutil.Equal(t, rec.EnvironmentHash, testEnvHash)
		testutil.Equal(t, rec.EnvironmentManifest, testManifest)
		testutil.Equal(t, author.EnvironmentHash(rec.EnvironmentManifest), rec.EnvironmentHash)
		// The frozen triple is the certificate key: every frozen problem has a
		// passing certificate under it.
		for _, f := range rec.Problems {
			_, ok, err := author.CurrentCertificate(ctx, h.st, f.Hash, rec.GraderVersion, rec.EnvironmentHash)
			testutil.NoError(t, err)
			if !ok {
				t.Errorf("%s: %s has no certificate under the frozen grader version and environment hash", name, f.Definition.ID)
			}
		}
	}
	testutil.NoError(t, h.svc.Start(ctx, prepared.ID, t0))
	after := mustLoad(t, h, prepared.ID)
	testutil.Equal(t, after.EnvironmentHash, testEnvHash)
	testutil.Equal(t, after.EnvironmentManifest, testManifest)
	st, err := h.svc.Status(ctx, prepared.ID, t0)
	testutil.NoError(t, err)
	testutil.Equal(t, st.Session.EnvironmentManifest, testManifest)
}

func mustLoad(t *testing.T, h *harness, id string) model.SessionRecord {
	t.Helper()
	rec, err := h.st.LoadSession(ctx, id)
	testutil.NoError(t, err)
	return rec
}

// degradedBank has seven problems: the five fixtures, one variant that will
// have no certificate (dev-sa-002) and one that needs network-policy
// (dev-sa-003).
func degradedBank(o *harnessOption) {
	o.uncertified = []string{"dev-sa-002"}
	o.mutateBank = func(t *testing.T, bank string) {
		addVariant(t, bank, "dev-sa-001", "dev-sa-002", "dev-sec", "dev-sec2")
		addVariant(t, bank, "dev-sa-001", "dev-sa-003", "dev-sec", "dev-sec3")
		editFile(t, filepath.Join(bank, "problems", "dev-sa-003", "problem.yaml"), "capabilities: []", "capabilities: [network-policy]")
	}
}

func npFails() environment.DoctorReport {
	return environment.DoctorReport{Checks: []environment.CapabilityCheck{
		{Name: environment.CapabilityNetworkPolicy, Status: environment.StatusFail, Reason: "NetworkPolicy is not enforced"},
	}}
}

// TestPracticeDrawsOnlyFromServableProblems (spec 7.2, ruling R27): the
// candidates are narrowed to problems with a current certificate and PASSing
// capabilities BEFORE the draw, so success never depends on the seed, and what
// was left out is reported.
func TestPracticeDrawsOnlyFromServableProblems(t *testing.T) {
	servable := []string{"dev-config-001", "dev-crashloop-001", "dev-sa-001", "dev-scale-001", "dev-service-001"}

	for seed := uint64(1); seed <= 8; seed++ {
		h := newHarness(t, degradedBank)
		h.env.report = npFails()
		rec, report, err := h.svc.PrepareWithReport(ctx, session.StartOptions{Mode: model.ModePractice, Count: 5, Seed: seed})
		testutil.NoError(t, err)
		got := viewIDs(rec)
		slices.Sort(got)
		testutil.Equal(t, got, servable)

		var ids []string
		reasons := map[string]string{}
		for _, e := range report.Excluded {
			ids = append(ids, e.ProblemID)
			reasons[e.ProblemID] = e.Reason
		}
		slices.Sort(ids)
		testutil.Equal(t, ids, []string{"dev-sa-002", "dev-sa-003"})
		if !strings.Contains(reasons["dev-sa-002"], "no passing validation") {
			t.Errorf("dev-sa-002 reason = %q", reasons["dev-sa-002"])
		}
		if !strings.Contains(reasons["dev-sa-003"], "network-policy") || !strings.Contains(reasons["dev-sa-003"], "NetworkPolicy is not enforced") {
			t.Errorf("dev-sa-003 reason = %q", reasons["dev-sa-003"])
		}
		testutil.Equal(t, h.env.count("doctor"), 1)
	}

	t.Run("the pool is too small", func(t *testing.T) {
		h := newHarness(t, degradedBank)
		h.env.report = npFails()
		_, report, err := h.svc.PrepareWithReport(ctx, session.StartOptions{Mode: model.ModePractice, Count: 6, Seed: 3})
		testutil.ErrorIs(t, err, problem.ErrInsufficientProblems)
		for _, want := range []string{"6 requested, 5 available", "dev-sa-002", "no passing validation", "dev-sa-003", "NetworkPolicy is not enforced"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error lacks %q: %v", want, err)
			}
		}
		testutil.Equal(t, len(report.Excluded), 2)
		testutil.Equal(t, h.env.count("apply"), 0)
		active, aerr := h.st.ActiveSessions(ctx, testEnvID)
		testutil.NoError(t, aerr)
		testutil.Equal(t, len(active), 0)
	})

	t.Run("only the requested slice is accounted for", func(t *testing.T) {
		h := newHarness(t, degradedBank)
		h.env.report = npFails()
		_, report, err := h.svc.PrepareWithReport(ctx, session.StartOptions{Mode: model.ModePractice, Count: 5, Domain: model.DomainNetworking})
		testutil.ErrorIs(t, err, problem.ErrInsufficientProblems)
		if !strings.Contains(err.Error(), "5 requested, 1 available") || strings.Contains(err.Error(), "dev-sa-00") {
			t.Errorf("error = %v", err)
		}
		testutil.Equal(t, len(report.Excluded), 0)
	})

	t.Run("a reserved provided-file name excludes the problem", func(t *testing.T) {
		h := newHarness(t, func(o *harnessOption) {
			o.mutateBank = func(t *testing.T, bank string) {
				addVariant(t, bank, "dev-sa-001", "dev-sa-002", "dev-sec", "dev-sec2")
				addVariant(t, bank, "dev-sa-001", "dev-sa-003", "dev-sec", "dev-sec3")
				dir := filepath.Join(bank, "problems", "dev-sa-002", "setup", "provided")
				testutil.NoError(t, os.MkdirAll(dir, 0o755))
				testutil.NoError(t, os.WriteFile(filepath.Join(dir, "question.md"), []byte("sneaky"), 0o644))
			}
		})
		_, report, err := h.svc.PrepareWithReport(ctx, session.StartOptions{Mode: model.ModePractice, Count: 7, Seed: 5})
		testutil.ErrorIs(t, err, problem.ErrInsufficientProblems)
		testutil.Equal(t, len(report.Excluded), 1)
		testutil.Equal(t, report.Excluded[0].ProblemID, "dev-sa-002")
	})
}

// TestMockRefusesUnservableProblems: an exam file is strict. Any problem in it
// that cannot be served refuses the session and every reason is listed.
func TestMockRefusesUnservableProblems(t *testing.T) {
	h := newHarness(t, func(o *harnessOption) {
		degradedBank(o)
		mutate := o.mutateBank
		o.mutateBank = func(t *testing.T, bank string) {
			mutate(t, bank)
			exam := filepath.Join(bank, "exams", "dev-mock.yaml")
			b, err := os.ReadFile(exam)
			testutil.NoError(t, err)
			testutil.NoError(t, os.WriteFile(exam, append(b, "  - dev-sa-002\n  - dev-sa-003\n"...), 0o644))
		}
	})
	h.env.report = npFails()

	_, err := h.svc.Prepare(ctx, h.mock())
	testutil.ErrorIs(t, err, session.ErrNotCertified)
	if !strings.Contains(err.Error(), "dev-sa-002") {
		t.Errorf("error does not name the uncertified problem: %v", err)
	}
	testutil.Equal(t, h.env.count("apply"), 0)
	active, aerr := h.st.ActiveSessions(ctx, testEnvID)
	testutil.NoError(t, aerr)
	testutil.Equal(t, len(active), 0)

	// With the certificate in place, the capability failure still refuses it.
	h.certifyProblem(h.loadBankProblem("dev-sa-002"), testGraderVersion, testEnvHash, true)
	_, err = h.svc.Prepare(ctx, h.mock())
	testutil.ErrorIs(t, err, session.ErrCapabilityUnavailable)
	if !strings.Contains(err.Error(), "dev-sa-003") || !strings.Contains(err.Error(), "NetworkPolicy is not enforced") {
		t.Errorf("error does not explain the capability refusal: %v", err)
	}
	testutil.Equal(t, h.env.count("apply"), 0)
}

// TestPrepareRefusesAReservedAssetName (R28): a provided file that would
// shadow the question is caught before any cluster change, so no session is
// left blocking the environment.
func TestPrepareRefusesAReservedAssetName(t *testing.T) {
	h := newHarness(t, func(o *harnessOption) {
		o.mutateBank = func(t *testing.T, bank string) {
			dir := filepath.Join(bank, "problems", "dev-sa-001", "setup", "provided")
			testutil.NoError(t, os.MkdirAll(dir, 0o755))
			testutil.NoError(t, os.WriteFile(filepath.Join(dir, "question.md"), []byte("sneaky"), 0o644))
		}
	})
	_, err := h.svc.Prepare(ctx, h.mock())
	testutil.ErrorIs(t, err, session.ErrReservedAsset)
	if !strings.Contains(err.Error(), "dev-sa-001") {
		t.Errorf("error does not name the problem: %v", err)
	}
	testutil.Equal(t, h.env.count("apply"), 0)
	testutil.Equal(t, h.env.count("prepare-env"), 0)
	active, aerr := h.st.ActiveSessions(ctx, testEnvID)
	testutil.NoError(t, aerr)
	testutil.Equal(t, len(active), 0)
	testutil.Equal(t, countFiles(t, h.workspace), 0)
}

// TestPrepareRefusesAnOccupiedEnvironment: a session that is over (COMPLETED,
// ABANDONED) does not occupy the environment as a session, but until it is
// reset its namespaces and cluster-scoped objects are still there. A new
// session that needs any of them is refused before anything is recorded,
// with a read-only check, instead of failing half-way into SETUP_ERROR.
func TestPrepareRefusesAnOccupiedEnvironment(t *testing.T) {
	h := newHarness(t)
	id := h.startedPractice()
	h.answer(nil)
	_, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(9, 0)})
	testutil.NoError(t, err)
	testutil.Equal(t, h.state(id), model.StateCompleted)
	applies := h.env.count("apply")

	_, err = h.svc.Prepare(ctx, h.mock())
	testutil.ErrorIs(t, err, session.ErrEnvironmentOccupied)
	for _, want := range []string{"reset", id, "dev-svc"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal lacks %q: %v", want, err)
		}
	}
	testutil.Equal(t, h.env.count("apply"), applies)
	active, err := h.st.ActiveSessions(ctx, testEnvID)
	testutil.NoError(t, err)
	testutil.Equal(t, len(active), 0) // no SETUP_ERROR session was created
	_, err = h.st.LoadSession(ctx, "mock-2")
	testutil.ErrorIs(t, err, store.ErrNotFound)

	// After the reset the environment is free again.
	h.env.reset(id)
	h.answer(initialEvaluation) // a fresh setup is in its starting state
	_, err = h.svc.Prepare(ctx, h.mock())
	testutil.NoError(t, err)

	t.Run("a failing check refuses too", func(t *testing.T) {
		h := newHarness(t)
		h.env.occupiedErr = errors.New("apiserver busy")
		_, err := h.svc.Prepare(ctx, h.mock())
		testutil.ErrorIs(t, err, h.env.occupiedErr)
		testutil.Equal(t, h.env.count("apply"), 0)
		active, err := h.st.ActiveSessions(ctx, testEnvID)
		testutil.NoError(t, err)
		testutil.Equal(t, len(active), 0)
	})
}

// TestPrepareRefusesSharedClusterResources: two problems of one session must
// not both declare the same cluster-scoped object, and one left by an earlier
// session blocks a new one like a namespace does.
func TestPrepareRefusesSharedClusterResources(t *testing.T) {
	declare := func(t *testing.T, bank, problemID string) {
		path := filepath.Join(bank, "problems", problemID, "problem.yaml")
		editFile(t, path, "budget:\n", "scope:\n  cluster_resources:\n    - {api_version: rbac.authorization.k8s.io/v1, kind: ClusterRole, name: dev-shared-reader}\nbudget:\n")
	}
	h := newHarness(t, func(o *harnessOption) {
		o.mutateBank = func(t *testing.T, bank string) {
			declare(t, bank, "dev-sa-001")
			declare(t, bank, "dev-scale-001")
		}
	})
	_, err := h.svc.Prepare(ctx, h.mock())
	testutil.ErrorIs(t, err, session.ErrNamespaceClash)
	if !strings.Contains(err.Error(), "dev-shared-reader") {
		t.Errorf("error does not name the object: %v", err)
	}
	testutil.Equal(t, h.env.count("apply"), 0)

	h2 := newHarness(t, func(o *harnessOption) {
		o.mutateBank = func(t *testing.T, bank string) { declare(t, bank, "dev-sa-001") }
	})
	h2.env.owners = append(h2.env.owners, environment.Owner{Inventory: model.Inventory{SessionID: "practice-old"},
		Namespace: "elsewhere", Scope: model.ResourceScope{ClusterResources: []model.ObjectRef{
			{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: "dev-shared-reader"}}}})
	_, err = h2.svc.Prepare(ctx, h2.mock())
	testutil.ErrorIs(t, err, session.ErrEnvironmentOccupied)
	if !strings.Contains(err.Error(), "dev-shared-reader") {
		t.Errorf("error does not name the object: %v", err)
	}
}

// TestResubmitRefusedWhileAnotherSessionIsActive: resubmitting a COMPLETED
// session makes it active again (GRADING over its namespaces); that must not
// happen while another session of the environment is active.
func TestResubmitRefusedWhileAnotherSessionIsActive(t *testing.T) {
	h := newHarness(t)
	id := h.startedMock()
	h.answer(nil)
	_, err := h.svc.Grade(ctx, id, session.GradeOptions{At: at(10, 0)})
	testutil.NoError(t, err)
	testutil.Equal(t, h.state(id), model.StateCompleted)

	other := mustLoad(t, h, id)
	other.ID, other.State, other.StartedAt = "practice-other", model.StateRunning, nil
	testutil.NoError(t, h.st.CreateSession(ctx, other))

	_, err = h.svc.Grade(ctx, id, session.GradeOptions{Resubmit: true, At: at(10, 30)})
	testutil.ErrorIs(t, err, session.ErrSessionActive)
	if !strings.Contains(err.Error(), "practice-other") {
		t.Errorf("error does not name the active session: %v", err)
	}
	testutil.Equal(t, len(h.submissions(id)), 1)
	testutil.Equal(t, h.state(id), model.StateCompleted)

	// Without the other session it goes ahead.
	testutil.NoError(t, h.st.Transition(ctx, "practice-other", model.StateRunning, model.StateAbandoned))
	_, err = h.svc.Grade(ctx, id, session.GradeOptions{Resubmit: true, At: at(10, 31)})
	testutil.NoError(t, err)
}

// TestCallersNeverSeeTheGradingDefinition (spec 6.3, 9.1): what Prepare,
// PrepareWithReport and Status hand to a caller (the CLI) never carries the
// grading definition: no criterion, expected value, setup, solution or case
// action, explanation or reference answer, before or after Start. The
// question and provided files appear only after Start.
func TestCallersNeverSeeTheGradingDefinition(t *testing.T) {
	h := newHarness(t)
	var secrets []string
	for _, p := range h.bankProblems() {
		for _, c := range p.Criteria {
			secrets = append(secrets, c.ID)
		}
		for _, c := range p.Cases {
			secrets = append(secrets, "cases/"+c.ID)
		}
	}
	secrets = append(secrets, privateMarkers...)
	secrets = append(secrets, "solution/", "setup/", "initial_expected", "InitialExpected", "Criteria", "criteria")
	check := func(what string, v any) {
		t.Helper()
		requireNone(t, what, fmt.Sprintf("%+v", v), secrets)
		requireNone(t, what+" (JSON)", asJSON(t, v), secrets)
	}

	prepared, report, err := h.svc.PrepareWithReport(ctx, h.mock())
	testutil.NoError(t, err)
	check("PrepareWithReport", prepared)
	check("the report", report)
	before, err := h.svc.Status(ctx, prepared.ID, t0)
	testutil.NoError(t, err)
	check("Status before Start", before)
	if strings.Contains(asJSON(t, before), "# ") {
		t.Errorf("a question is visible before Start: %s", asJSON(t, before))
	}

	// What the view does carry, per problem.
	stored := mustLoad(t, h, prepared.ID)
	testutil.Equal(t, len(prepared.Problems), len(stored.Problems))
	for i, f := range stored.Problems {
		v := prepared.Problems[i]
		testutil.Equal(t, v.ID, f.Definition.ID)
		testutil.Equal(t, v.Title, f.Definition.Title)
		testutil.Equal(t, v.Domain, f.Definition.Domain)
		testutil.Equal(t, v.Namespace, f.Namespace)
		testutil.Equal(t, v.Context, testIdentity.Context)
		points := 0
		for _, c := range f.Definition.Criteria {
			points += c.Points
		}
		testutil.Equal(t, v.Points, points)
		testutil.Equal(t, v.Question, "")
		testutil.Equal(t, len(v.Assets), 0)
		testutil.Equal(t, before.Session.Problems[i].Question, "")
	}

	testutil.NoError(t, h.svc.Start(ctx, prepared.ID, t0))
	after, err := h.svc.Status(ctx, prepared.ID, t0.Add(time.Minute))
	testutil.NoError(t, err)
	check("Status after Start", after)
	for i, f := range stored.Problems {
		testutil.Equal(t, after.Session.Problems[i].Question, f.Question)
		testutil.Equal(t, len(after.Session.Problems[i].Assets), len(f.Assets))
	}
	testutil.Equal(t, after.Session.StartedAt != nil, true)

	h2 := newHarness(t)
	plain, err := h2.svc.Prepare(ctx, h2.mock())
	testutil.NoError(t, err)
	check("Prepare", plain)
}

// TestServingRefusesAnUnvalidatedParameterValue (spec 7.1): a certificate
// vouches only for the parameter values its seeds froze. A session whose seed
// freezes a value no validated seed used is refused, never served on trust.
func TestServingRefusesAnUnvalidatedParameterValue(t *testing.T) {
	h := newHarness(t)
	p := h.loadBankProblem("dev-config-001")
	byNamespace := map[string]uint64{}
	for seed := uint64(1); len(byNamespace) < 2; seed++ {
		f, err := problem.Freeze(p, seed)
		testutil.NoError(t, err)
		if _, ok := byNamespace[f.Namespace]; !ok {
			byNamespace[f.Namespace] = seed
		}
	}
	var validated, other uint64
	var otherNS string
	for ns, seed := range byNamespace {
		if validated == 0 {
			validated = seed
		} else {
			other, otherNS = seed, ns
		}
	}
	// A newer certificate of the same package that only tried one value.
	h.certifySeeds(p, testGraderVersion, testEnvHash, true, []uint64{validated})

	mock := h.mock()
	mock.Seed = other
	_, err := h.svc.Prepare(ctx, mock)
	testutil.ErrorIs(t, err, session.ErrNotCertified)
	for _, want := range []string{"dev-config-001", otherNS} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal lacks %q: %v", want, err)
		}
	}
	testutil.Equal(t, h.env.count("apply"), 0)

	mock.Seed = validated
	_, err = h.svc.Prepare(ctx, mock)
	testutil.NoError(t, err)
}
