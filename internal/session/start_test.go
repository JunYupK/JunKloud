//go:build linux

package session_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/lock"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// TestStartClock: Start publishes the questions and provided files into new
// attempt directories, stores the start time in UTC and runs the clock; a
// 121-minute-old mock exam is one minute over its 120 minutes.
func TestStartClock(t *testing.T) {
	h := newHarness(t)
	prepared := h.prepareMock()

	// The learner confirms in a local zone; it is stored in UTC.
	kst := time.FixedZone("KST", 9*3600)
	at := time.Date(2026, 10, 15, 18, 30, 0, 0, kst)
	testutil.NoError(t, h.svc.Start(ctx, prepared.ID, at))

	started, err := h.st.LoadSession(ctx, prepared.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, started.State, model.StateRunning)
	testutil.Equal(t, *started.StartedAt, at.UTC())
	testutil.Equal(t, started.StartedAt.Location(), time.UTC)

	// Public content: the English question and the provided files, exactly.
	for _, f := range started.Problems {
		dir := session.AttemptDir(h.workspace, prepared.ID, f.Definition.ID, 1)
		q, err := os.ReadFile(filepath.Join(dir, session.QuestionFile))
		testutil.NoError(t, err)
		testutil.Equal(t, string(q), f.Question)
		for name, content := range f.Assets {
			got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
			testutil.NoError(t, err)
			testutil.Equal(t, got, content)
		}
		testutil.Equal(t, countFiles(t, dir), 1+len(f.Assets))
		latest, n, err := session.LatestAttempt(h.workspace, prepared.ID, f.Definition.ID)
		testutil.NoError(t, err)
		testutil.Equal(t, latest, dir)
		testutil.Equal(t, n, 1)
	}
	// Nothing private: no solution, no wrong-answer case, no explanation, no
	// check definition.
	for _, marker := range privateMarkers {
		if treeContains(t, h.workspace, marker) {
			t.Errorf("the workspace contains private content %q", marker)
		}
	}
	for _, name := range []string{"problem.yaml", "explanation.ko.md", "solution", "cases"} {
		if matches, _ := filepath.Glob(filepath.Join(h.workspace, "*", "*", "*", name)); len(matches) > 0 {
			t.Errorf("private file published: %v", matches)
		}
	}

	status, err := h.svc.Status(ctx, prepared.ID, at.Add(121*time.Minute))
	testutil.NoError(t, err)
	testutil.Equal(t, status.Elapsed, 121*time.Minute)
	testutil.Equal(t, status.Overtime, time.Minute)
	testutil.Equal(t, status.ClockAnomaly, false)
	testutil.Equal(t, status.Session.State, model.StateRunning)

	// Within the limit there is no overtime; exactly at the limit neither.
	for _, d := range []time.Duration{0, 119 * time.Minute, 120 * time.Minute} {
		status, err = h.svc.Status(ctx, prepared.ID, at.Add(d))
		testutil.NoError(t, err)
		testutil.Equal(t, status.Elapsed, d)
		testutil.Equal(t, status.Overtime, time.Duration(0))
	}

	// A local-zone "now" gives the same answer.
	status, err = h.svc.Status(ctx, prepared.ID, at.Add(125*time.Minute).In(time.UTC))
	testutil.NoError(t, err)
	testutil.Equal(t, status.Overtime, 5*time.Minute)
}

// TestStatusBeforeStart: nothing runs, nothing is overtime and no question
// text comes with the record.
func TestStatusBeforeStart(t *testing.T) {
	h := newHarness(t)
	prepared := h.prepareMock()
	status, err := h.svc.Status(ctx, prepared.ID, t0.Add(10*time.Hour))
	testutil.NoError(t, err)
	testutil.Equal(t, status.Elapsed, time.Duration(0))
	testutil.Equal(t, status.Overtime, time.Duration(0))
	testutil.Equal(t, status.ClockAnomaly, false)
	testutil.Equal(t, status.Session.State, model.StatePreparing)
	testutil.Equal(t, status.Session.StartedAt, (*time.Time)(nil))
	for _, f := range status.Session.Problems {
		testutil.Equal(t, f.Question, "")
		testutil.Equal(t, len(f.Assets), 0)
	}
	_, err = h.svc.Status(ctx, "no-such-session", t0)
	testutil.ErrorIs(t, err, store.ErrNotFound)
}

// TestWallClockAfterSleep: the clock is wall time since the stored start.
// Closing the terminal, restarting the program or a host that slept for hours
// pauses nothing, and a monotonic reading in `now` or in the start time
// cannot shorten the elapsed time.
func TestWallClockAfterSleep(t *testing.T) {
	h := newHarness(t)
	prepared := h.prepareMock()

	// time.Now carries a monotonic reading, which stands still while a host
	// sleeps; the service must use wall time.
	at := time.Now()
	testutil.NoError(t, h.svc.Start(ctx, prepared.ID, at))

	// "The terminal was closed and the host slept": a new Service on the same
	// state, three and a half hours of wall time later.
	reopened, err := session.NewService(session.Config{
		Store: h.st, Env: h.env, Engine: h.eng, Identity: testIdentity, GraderVersion: testGraderVersion,
		EnvironmentManifest: func(context.Context, model.Identity) (map[string]string, error) { return testManifest, nil },
		PrepareEnvironment:  func(context.Context, model.Identity) error { return nil },
		BankDir:             h.bank, StateDir: h.stateDir, Workspace: h.workspace,
	})
	testutil.NoError(t, err)
	later := at.Round(0).Add(3*time.Hour + 30*time.Minute)
	status, err := reopened.Status(ctx, prepared.ID, later)
	testutil.NoError(t, err)
	testutil.Equal(t, status.Elapsed, 3*time.Hour+30*time.Minute)
	testutil.Equal(t, status.Overtime, 90*time.Minute)

	// The same instant with a monotonic reading that disagrees with the wall
	// clock: wall time wins.
	mono := at.Add(150 * time.Minute) // keeps at's monotonic offset
	status, err = reopened.Status(ctx, prepared.ID, mono)
	testutil.NoError(t, err)
	testutil.Equal(t, status.Elapsed, 150*time.Minute)

	// Starting stores nothing monotonic: the stored value is plain UTC wall time.
	stored, err := h.st.LoadSession(ctx, prepared.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, stored.StartedAt.Equal(at), true)
}

// TestClockAnomaly: a clock that went backwards is flagged, never reported as
// negative time.
func TestClockAnomaly(t *testing.T) {
	h := newHarness(t)
	prepared := h.prepareMock()
	at := t0.Add(time.Hour)
	testutil.NoError(t, h.svc.Start(ctx, prepared.ID, at))

	status, err := h.svc.Status(ctx, prepared.ID, at.Add(-5*time.Minute))
	testutil.NoError(t, err)
	testutil.Equal(t, status.ClockAnomaly, true)
	testutil.Equal(t, status.Elapsed, time.Duration(0))
	testutil.Equal(t, status.Overtime, time.Duration(0))

	status, err = h.svc.Status(ctx, prepared.ID, at)
	testutil.NoError(t, err)
	testutil.Equal(t, status.ClockAnomaly, false)
}

// TestPracticeHasNoTimeLimit: only the mock exam has an official time.
func TestPracticeHasNoTimeLimit(t *testing.T) {
	h := newHarness(t)
	prepared, err := h.svc.Prepare(ctx, session.StartOptions{Mode: model.ModePractice, Count: 5})
	testutil.NoError(t, err)
	testutil.NoError(t, h.svc.Start(ctx, prepared.ID, t0))
	status, err := h.svc.Status(ctx, prepared.ID, t0.Add(30*time.Hour))
	testutil.NoError(t, err)
	testutil.Equal(t, status.Elapsed, 30*time.Hour)
	testutil.Equal(t, status.Overtime, time.Duration(0))
}

// TestStartRefusals: only a fully prepared, unstarted session starts, once.
func TestStartRefusals(t *testing.T) {
	t.Run("unknown", func(t *testing.T) {
		h := newHarness(t)
		testutil.ErrorIs(t, h.svc.Start(ctx, "nope", t0), store.ErrNotFound)
	})
	t.Run("twice", func(t *testing.T) {
		h := newHarness(t)
		prepared := h.prepareMock()
		testutil.NoError(t, h.svc.Start(ctx, prepared.ID, t0))
		before := countFiles(t, h.workspace)
		testutil.ErrorIs(t, h.svc.Start(ctx, prepared.ID, t0.Add(time.Hour)), session.ErrAlreadyStarted)
		stored, err := h.st.LoadSession(ctx, prepared.ID)
		testutil.NoError(t, err)
		testutil.Equal(t, *stored.StartedAt, t0) // the clock did not move
		testutil.Equal(t, countFiles(t, h.workspace), before)
	})
	t.Run("preparation did not finish", func(t *testing.T) {
		h := newHarness(t)
		prepared := h.prepareMock()
		// A crash after the intent but before verification leaves the
		// operation unfinished.
		testutil.NoError(t, h.st.PutOperation(ctx, model.Operation{
			ID: session.OperationID(prepared.ID), EnvironmentID: testEnvID, SessionID: prepared.ID,
			Kind: session.OperationKind, Phase: session.PhaseSetupApplied, CreatedAt: t0,
		}))
		testutil.ErrorIs(t, h.svc.Start(ctx, prepared.ID, t0), session.ErrNotPrepared)
		testutil.Equal(t, countFiles(t, h.workspace), 0)
		stored, err := h.st.LoadSession(ctx, prepared.ID)
		testutil.NoError(t, err)
		testutil.Equal(t, stored.State, model.StatePreparing)
		testutil.Equal(t, stored.StartedAt, (*time.Time)(nil))
	})
	t.Run("no recorded preparation", func(t *testing.T) {
		h := newHarness(t)
		rec := model.SessionRecord{ID: "orphan", EnvironmentID: testEnvID, Mode: model.ModePractice, State: model.StatePreparing}
		testutil.NoError(t, h.st.CreateSession(ctx, rec))
		testutil.ErrorIs(t, h.svc.Start(ctx, "orphan", t0), session.ErrNotPrepared)
	})
	t.Run("abandoned", func(t *testing.T) {
		h := newHarness(t)
		prepared := h.prepareMock()
		testutil.NoError(t, h.st.Transition(ctx, prepared.ID, model.StatePreparing, model.StateAbandoned))
		testutil.ErrorIs(t, h.svc.Start(ctx, prepared.ID, t0), session.ErrNotPrepared)
		testutil.Equal(t, countFiles(t, h.workspace), 0)
	})
	t.Run("another environment", func(t *testing.T) {
		h := newHarness(t)
		rec := model.SessionRecord{ID: "foreign", EnvironmentID: "env-other", Mode: model.ModePractice, State: model.StatePreparing}
		testutil.NoError(t, h.st.CreateSession(ctx, rec))
		testutil.ErrorIs(t, h.svc.Start(ctx, "foreign", t0), session.ErrEnvironmentChanged)
	})
	t.Run("environment gone", func(t *testing.T) {
		h := newHarness(t)
		prepared := h.prepareMock()
		boom := errors.New("the node container is stopped")
		h.env.checkErr = boom
		testutil.ErrorIs(t, h.svc.Start(ctx, prepared.ID, t0), boom)
		testutil.Equal(t, countFiles(t, h.workspace), 0)
		stored, err := h.st.LoadSession(ctx, prepared.ID)
		testutil.NoError(t, err)
		testutil.Equal(t, stored.State, model.StatePreparing)
	})
	t.Run("locked", func(t *testing.T) {
		h := newHarness(t)
		prepared := h.prepareMock()
		release, err := lock.Acquire(author.LockPath(h.stateDir))
		testutil.NoError(t, err)
		testutil.ErrorIs(t, h.svc.Start(ctx, prepared.ID, t0), lock.ErrLocked)
		testutil.NoError(t, release())
		testutil.Equal(t, countFiles(t, h.workspace), 0)
		testutil.NoError(t, h.svc.Start(ctx, prepared.ID, t0))
	})
	t.Run("zero time means now", func(t *testing.T) {
		h := newHarness(t)
		prepared := h.prepareMock()
		h.clock.Set(t0.Add(7 * time.Minute))
		testutil.NoError(t, h.svc.Start(ctx, prepared.ID, time.Time{}))
		stored, err := h.st.LoadSession(ctx, prepared.ID)
		testutil.NoError(t, err)
		testutil.Equal(t, *stored.StartedAt, t0.Add(7*time.Minute))
	})
}

// TestStartNeverOverwritesWork: Start only creates. Personal files in the
// workspace and earlier attempts stay as they are; the new questions go into
// the next free attempt directory.
func TestStartNeverOverwritesWork(t *testing.T) {
	h := newHarness(t)
	prepared := h.prepareMock()
	personal := filepath.Join(h.workspace, "notes.txt")
	attempt1 := session.AttemptDir(h.workspace, prepared.ID, "dev-sa-001", 1)
	earlier := filepath.Join(attempt1, "my-answer.yaml")
	testutil.NoError(t, os.MkdirAll(attempt1, 0o755))
	testutil.NoError(t, os.WriteFile(personal, []byte("my notes"), 0o644))
	testutil.NoError(t, os.WriteFile(earlier, []byte("earlier work"), 0o644))

	testutil.NoError(t, h.svc.Start(ctx, prepared.ID, t0))

	for path, want := range map[string]string{personal: "my notes", earlier: "earlier work"} {
		got, err := os.ReadFile(path)
		testutil.NoError(t, err)
		testutil.Equal(t, string(got), want)
	}
	// The question went into attempt-2; attempt-1 only has the learner's file.
	_, err := os.Stat(filepath.Join(attempt1, session.QuestionFile))
	testutil.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(session.AttemptDir(h.workspace, prepared.ID, "dev-sa-001", 2), session.QuestionFile))
	testutil.NoError(t, err)
	latest, n, err := session.LatestAttempt(h.workspace, prepared.ID, "dev-sa-001")
	testutil.NoError(t, err)
	testutil.Equal(t, n, 2)
	testutil.Equal(t, latest, session.AttemptDir(h.workspace, prepared.ID, "dev-sa-001", 2))
}

// failingRecords makes the state change of Start fail.
type failingRecords struct {
	*store.Store
	err error
}

func (f failingRecords) MarkStarted(context.Context, string, time.Time) error { return f.err }

// TestStartLeavesNothingVisibleWithoutAClock: if the clock cannot be started
// the published questions are taken away again.
func TestStartLeavesNothingVisibleWithoutAClock(t *testing.T) {
	boom := errors.New("disk full")
	h := newHarness(t, func(o *harnessOption) {
		o.records = func(s *store.Store) session.Records { return failingRecords{Store: s, err: boom} }
	})
	prepared := h.prepareMock()
	testutil.ErrorIs(t, h.svc.Start(ctx, prepared.ID, t0), boom)
	testutil.Equal(t, countFiles(t, h.workspace), 0)
	stored, err := h.st.LoadSession(ctx, prepared.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, stored.State, model.StatePreparing)
	testutil.Equal(t, stored.StartedAt, (*time.Time)(nil))
}

// TestBankChangeDoesNotChangeActiveSession: a session is frozen. Editing or
// deleting bank packages after Prepare changes neither what Start publishes
// nor the stored private package.
func TestBankChangeDoesNotChangeActiveSession(t *testing.T) {
	h := newHarness(t)
	prepared := h.prepareMock()
	original := h.loadBankProblem("dev-config-001")
	originalHash, err := problem.Hash(original)
	testutil.NoError(t, err)
	frozenBefore, err := h.st.LoadSession(ctx, prepared.ID)
	testutil.NoError(t, err)

	// Edit the question, the provided file and the solution, then delete a
	// whole package and the exam.
	dir := filepath.Join(h.bank, "problems", "dev-config-001")
	testutil.NoError(t, os.WriteFile(filepath.Join(dir, "question.en.md"), []byte("# A different question\n"), 0o644))
	testutil.NoError(t, os.WriteFile(filepath.Join(dir, "setup", "provided", "settings-template.yaml"), []byte("changed: true\n"), 0o644))
	testutil.NoError(t, os.WriteFile(filepath.Join(dir, "solution", "app-settings.yaml"), []byte("# DEV-SOLUTION-EDITED\n"), 0o644))
	testutil.NoError(t, os.RemoveAll(filepath.Join(h.bank, "problems", "dev-scale-001")))
	testutil.NoError(t, os.Remove(filepath.Join(h.bank, "exams", "dev-mock.yaml")))
	edited, err := problem.Load(dir)
	testutil.NoError(t, err)
	editedHash, err := problem.Hash(edited)
	testutil.NoError(t, err)
	if editedHash == originalHash {
		t.Fatal("the edit did not change the package hash")
	}

	testutil.NoError(t, h.svc.Start(ctx, prepared.ID, t0))

	frozenAfter, err := h.st.LoadSession(ctx, prepared.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, frozenAfter.Problems, frozenBefore.Problems)

	attempt := session.AttemptDir(h.workspace, prepared.ID, "dev-config-001", 1)
	q, err := os.ReadFile(filepath.Join(attempt, session.QuestionFile))
	testutil.NoError(t, err)
	if string(q) == "# A different question\n" || !treeContains(t, attempt, "Create the app-settings ConfigMap") {
		t.Errorf("Start published the edited question: %q", q)
	}
	tmpl, err := os.ReadFile(filepath.Join(attempt, "settings-template.yaml"))
	testutil.NoError(t, err)
	if string(tmpl) == "changed: true\n" {
		t.Error("Start published the edited provided file")
	}
	// The deleted package's question is still published.
	_, err = os.Stat(filepath.Join(session.AttemptDir(h.workspace, prepared.ID, "dev-scale-001", 1), session.QuestionFile))
	testutil.NoError(t, err)

	// The stored private package is the original, byte for byte, under the
	// original hash; the edited package would live under its own.
	stored, err := session.LoadPackage(h.stateDir, originalHash)
	testutil.NoError(t, err)
	testutil.Equal(t, stored.Files, original.Files)
	if !treeContains(t, h.stateDir, "DEV-SOLUTION-MARKER-CONFIG") || treeContains(t, h.stateDir, "DEV-SOLUTION-EDITED") {
		t.Error("the stored package does not hold the original solution")
	}
	_, err = session.LoadPackage(h.stateDir, editedHash)
	testutil.ErrorIs(t, err, session.ErrPackageMissing)
}
