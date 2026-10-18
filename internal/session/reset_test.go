//go:build linux

package session_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func (e *fakeEnv) Cleanup(_ context.Context, _ model.Identity, inv model.Inventory) error {
	e.log(event{Kind: "cleanup", Problem: inv.ProblemID})
	e.mu.Lock()
	defer e.mu.Unlock()
	e.owners = slices.DeleteFunc(e.owners, func(o environment.Owner) bool {
		return o.Inventory.SessionID == inv.SessionID && o.Inventory.ProblemID == inv.ProblemID
	})
	return nil
}

func TestPersonalFilesPreserved(t *testing.T) {
	h := newHarness(t)
	v, err := h.svc.Prepare(ctx, session.StartOptions{Mode: model.ModePractice, Count: 5, Seed: 7})
	testutil.NoError(t, err)
	testutil.NoError(t, h.svc.Start(ctx, v.ID, t0))
	pid := v.Problems[0].ID
	dir, _, err := session.LatestAttempt(h.workspace, v.ID, pid)
	testutil.NoError(t, err)
	answer := filepath.Join(dir, "answer.yaml")
	testutil.NoError(t, os.WriteFile(answer, []byte("my solution"), 0600))
	alias := filepath.Join(h.workspace, ".bashrc")
	testutil.NoError(t, os.WriteFile(alias, []byte("alias k=kubectl"), 0600))
	testutil.NoError(t, h.svc.Reset(ctx, v.ID, session.ResetOptions{ProblemID: pid}))
	_, n, err := session.LatestAttempt(h.workspace, v.ID, pid)
	testutil.NoError(t, err)
	testutil.Equal(t, n, 2)
	b, err := os.ReadFile(answer)
	testutil.NoError(t, err)
	testutil.Equal(t, string(b), "my solution")
	b, err = os.ReadFile(alias)
	testutil.NoError(t, err)
	testutil.Equal(t, string(b), "alias k=kubectl")
}

func TestResetRequiresFullConfirmation(t *testing.T) {
	h := newHarness(t)
	v := h.prepareMock()
	testutil.NoError(t, h.svc.Start(ctx, v.ID, t0))
	if err := h.svc.Reset(ctx, v.ID, session.ResetOptions{ProblemID: v.Problems[0].ID}); err == nil {
		t.Fatal("mock individual reset allowed")
	}
	if err := h.svc.Reset(ctx, v.ID, session.ResetOptions{}); err == nil {
		t.Fatal("abandon without confirmation")
	}
	testutil.NoError(t, h.svc.Reset(ctx, v.ID, session.ResetOptions{ConfirmedEnvironmentID: testEnvID}))
	r, err := h.st.LoadSession(ctx, v.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, r.State, model.StateAbandoned)
}

func TestRetryOneWrongProblem(t *testing.T) {
	h := newHarness(t)
	v := h.prepareMock()
	testutil.NoError(t, h.svc.Start(ctx, v.ID, t0))
	rec, err := h.st.LoadSession(ctx, v.ID)
	testutil.NoError(t, err)
	wrong := rec.Problems[0].Definition.ID
	h.eng.override = map[string]func(model.FrozenProblem) model.Evaluation{}
	for _, f := range rec.Problems {
		h.eng.override[f.Definition.ID] = solvedEvaluation
	}
	h.eng.override[wrong] = func(f model.FrozenProblem) model.Evaluation {
		ev := solvedEvaluation(f)
		ev.Results[0].Status = model.StatusFail
		ev.Results[0].Points = 0
		*ev.Score -= f.Definition.Criteria[0].Points
		return ev
	}
	_, err = h.svc.Grade(ctx, v.ID, session.GradeOptions{})
	testutil.NoError(t, err)
	testutil.NoError(t, h.svc.Reset(ctx, v.ID, session.ResetOptions{ConfirmedEnvironmentID: testEnvID}))
	h.eng.override = nil
	retry, err := h.svc.Prepare(ctx, session.StartOptions{Mode: model.ModeRetry, RetrySessionID: v.ID})
	testutil.NoError(t, err)
	testutil.Equal(t, len(retry.Problems), 1)
	testutil.Equal(t, retry.Problems[0].ID, wrong)
	testutil.Equal(t, retry.ParentID, v.ID)
	r, err := h.st.LoadSession(ctx, retry.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, r.Seed, rec.Seed)
	testutil.Equal(t, r.Problems[0].Parameters, rec.Problems[0].Parameters)
}

func TestCrashAfterDelete(t *testing.T) {
	h := newHarness(t)
	v, err := h.svc.Prepare(ctx, session.StartOptions{Mode: model.ModePractice, Count: 5, Seed: 7})
	testutil.NoError(t, err)
	testutil.NoError(t, h.svc.Start(ctx, v.ID, t0))
	h.env.applyErr = func(environment.Owner) error { return errors.New("interrupted setup") }
	err = h.svc.Reset(ctx, v.ID, session.ResetOptions{ProblemID: v.Problems[0].ID})
	if err == nil {
		t.Fatal("expected interruption")
	}
	_, err = h.svc.Prepare(ctx, h.mock())
	testutil.ErrorIs(t, err, session.ErrResetPending)
	h.env.applyErr = nil
	testutil.NoError(t, h.svc.Recover(ctx, testEnvID))
	_, n, err := session.LatestAttempt(h.workspace, v.ID, v.Problems[0].ID)
	testutil.NoError(t, err)
	testutil.Equal(t, n, 2)
}

func TestResetPendingVisibleBeforeStart(t *testing.T) {
	h := newHarness(t)
	v := h.prepareMock()
	testutil.NoError(t, h.st.PutOperation(ctx, model.Operation{ID: "reset-crash", Kind: session.ResetOperationKind, EnvironmentID: testEnvID, SessionID: v.ID, Phase: "planned", Planned: model.Inventory{EnvironmentID: testEnvID, SessionID: v.ID}}))
	st, err := h.svc.Status(ctx, v.ID, t0)
	testutil.NoError(t, err)
	if len(st.Pending) != 1 {
		t.Fatal("unfinished reset hidden in status", st.Pending)
	}
}

func TestRetryErrorOnlyAndMissingPackage(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			h := newHarness(t)
			v := h.prepareMock()
			testutil.NoError(t, h.svc.Start(ctx, v.ID, t0))
			rec, err := h.st.LoadSession(ctx, v.ID)
			testutil.NoError(t, err)
			h.eng.override = map[string]func(model.FrozenProblem) model.Evaluation{}
			for _, f := range rec.Problems {
				h.eng.override[f.Definition.ID] = erroredEvaluation
			}
			if missing {
				h.eng.override[rec.Problems[0].Definition.ID] = initialEvaluation
			}
			_, err = h.svc.Grade(ctx, v.ID, session.GradeOptions{})
			testutil.NoError(t, err)
			testutil.NoError(t, h.svc.Reset(ctx, v.ID, session.ResetOptions{ConfirmedEnvironmentID: testEnvID}))
			if missing {
				dir, err := session.PackageDir(h.stateDir, rec.Problems[0].Hash)
				testutil.NoError(t, err)
				testutil.NoError(t, os.RemoveAll(dir))
			}
			_, err = h.svc.Prepare(ctx, session.StartOptions{Mode: model.ModeRetry, RetrySessionID: v.ID})
			if missing {
				testutil.ErrorIs(t, err, session.ErrPackageMissing)
			} else {
				testutil.ErrorIs(t, err, session.ErrInvalidOptions)
			}
		})
	}
}

func TestResetPendingCleanupBlocksNewSession(t *testing.T) {
	h := newHarness(t)
	testutil.NoError(t, h.st.PutOperation(ctx, model.Operation{ID: "cleanup-failed", Kind: environment.OperationCleanup, EnvironmentID: testEnvID, SessionID: "older", Phase: "snapshotted", Planned: model.Inventory{EnvironmentID: testEnvID, SessionID: "older", ProblemID: "p"}}))
	_, err := h.svc.Prepare(ctx, h.mock())
	testutil.ErrorIs(t, err, session.ErrResetPending)
}

type resetCrashStore struct {
	*store.Store
	phase string
	armed bool
}

func (s *resetCrashStore) PutOperation(ctx context.Context, op model.Operation) error {
	if s.armed && op.Kind == session.ResetOperationKind && op.Phase == s.phase {
		s.armed = false
		return errors.New("crash at " + s.phase)
	}
	return s.Store.PutOperation(ctx, op)
}
func TestResetCrashPhases(t *testing.T) {
	for _, phase := range []string{"deleted", "rebuilding", "setup-applied", "verified", "published", "done"} {
		t.Run(phase, func(t *testing.T) {
			var cs *resetCrashStore
			h := newHarness(t, func(o *harnessOption) {
				o.records = func(st *store.Store) session.Records { cs = &resetCrashStore{Store: st, phase: phase}; return cs }
			})
			v, err := h.svc.Prepare(ctx, session.StartOptions{Mode: model.ModePractice, Count: 5, Seed: 7})
			testutil.NoError(t, err)
			testutil.NoError(t, h.svc.Start(ctx, v.ID, t0))
			cs.armed = true
			if err = h.svc.Reset(ctx, v.ID, session.ResetOptions{ProblemID: v.Problems[0].ID}); err == nil {
				t.Fatal("missing injected interruption")
			}
			testutil.NoError(t, h.svc.Recover(ctx, testEnvID))
			_, n, err := session.LatestAttempt(h.workspace, v.ID, v.Problems[0].ID)
			testutil.NoError(t, err)
			testutil.Equal(t, n, 2)
		})
	}
}

func TestResetClusterWithoutSessionRequiresConfirmation(t *testing.T) {
	h := newHarness(t)
	testutil.ErrorIs(t, h.svc.Reset(ctx, "", session.ResetOptions{Cluster: true, ConfirmedEnvironmentID: "wrong"}), session.ErrResetConfirmation)
	if h.env.count("check") != 0 {
		t.Fatal("invalid confirmation contacted cluster")
	}
}

func (e *fakeEnv) RecreateReset(_ context.Context, id model.Identity, _, _ string) (model.Identity, error) {
	e.log(event{Kind: "recreate"})
	return id, nil
}

// Recovery must check the replacement again before marking old cleanup
// complete: the replacement may have disappeared since the process stopped.
func TestResetClusterRecoveryVerifiesReplacement(t *testing.T) {
	h := newHarness(t)
	v := h.prepareMock()
	prior := model.Operation{ID: "cleanup-retained", Kind: environment.OperationCleanup, EnvironmentID: testEnvID, Phase: "snapshotted"}
	op := model.Operation{ID: "cluster-recreated", Kind: session.ResetOperationKind, EnvironmentID: testEnvID, SessionID: v.ID, Phase: "recreated", Planned: model.Inventory{ProblemID: "!cluster"}}
	testutil.NoError(t, h.st.PutOperation(ctx, prior))
	testutil.NoError(t, h.st.PutOperation(ctx, op))
	h.env.checkErr = environment.ErrIdentityMismatch
	testutil.ErrorIs(t, h.svc.Recover(ctx, testEnvID), environment.ErrIdentityMismatch)
	got, err := h.st.GetOperation(ctx, prior.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got.Phase, "snapshotted")
	r, err := h.st.LoadSession(ctx, v.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, r.State, model.StatePreparing)
	h.env.checkErr = nil
	testutil.NoError(t, h.svc.Recover(ctx, testEnvID))
	got, err = h.st.GetOperation(ctx, prior.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got.Phase, session.PhaseDone)
}

func TestResetAttemptSeparatesGradingHistory(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		t.Run(fmt.Sprint(incomplete), func(t *testing.T) {
			h := newHarness(t)
			v, err := h.svc.Prepare(ctx, session.StartOptions{Mode: model.ModePractice, Count: 5, Seed: 7})
			testutil.NoError(t, err)
			testutil.NoError(t, h.svc.Start(ctx, v.ID, t0))
			pid := v.Problems[0].ID
			evaluate := solvedEvaluation
			if incomplete {
				evaluate = erroredEvaluation
			}
			h.eng.override = map[string]func(model.FrozenProblem) model.Evaluation{pid: evaluate}
			old, err := h.svc.Grade(ctx, v.ID, session.GradeOptions{ProblemID: pid})
			testutil.NoError(t, err)
			before, err := h.st.ListSubmissions(ctx, v.ID)
			testutil.NoError(t, err)
			h.eng.override = nil
			testutil.NoError(t, h.svc.Reset(ctx, v.ID, session.ResetOptions{ProblemID: pid}))
			rv, err := h.svc.Review(ctx, v.ID, pid, false)
			testutil.NoError(t, err)
			if rv.Confirmed != nil {
				t.Fatal("pre-reset result presented as current", rv.Confirmed.ID)
			}
			testutil.Equal(t, len(rv.Runs), 1)
			_, err = h.svc.Grade(ctx, v.ID, session.GradeOptions{ProblemID: pid, Retry: true})
			testutil.ErrorIs(t, err, session.ErrAnswerChanged)
			h.eng.override = map[string]func(model.FrozenProblem) model.Evaluation{pid: initialEvaluation}
			fresh, err := h.svc.Grade(ctx, v.ID, session.GradeOptions{ProblemID: pid})
			testutil.NoError(t, err)
			if fresh.SubmissionID == old.SubmissionID {
				t.Fatal("new attempt reused old submission")
			}
			after, err := h.st.ListSubmissions(ctx, v.ID)
			testutil.NoError(t, err)
			testutil.Equal(t, len(after), 2)
			testutil.Equal(t, after[0], before[0])
			testutil.Equal(t, after[1].ParentSubmissionID, before[0].ID)
			kept, err := h.st.GetRun(ctx, old.ID)
			testutil.NoError(t, err)
			testutil.Equal(t, kept, old)
			rv, err = h.svc.Review(ctx, v.ID, pid, false)
			testutil.NoError(t, err)
			if rv.Confirmed == nil || rv.Confirmed.ID != fresh.ID {
				t.Fatal("new attempt result not current")
			}
		})
	}
}

func TestResetAbandonedMockReviewPreservesHistoryAndRecordsExposure(t *testing.T) {
	h := newHarness(t)
	v := h.prepareMock()
	testutil.NoError(t, h.svc.Start(ctx, v.ID, t0))
	testutil.NoError(t, h.svc.Reset(ctx, v.ID, session.ResetOptions{ConfirmedEnvironmentID: testEnvID}))
	rv, err := h.svc.Review(ctx, v.ID, v.Problems[0].ID, true)
	testutil.NoError(t, err)
	if !rv.SolutionViewed || rv.Explanation == "" || rv.Confirmed != nil {
		t.Fatal("abandoned review did not preserve archival semantics", rv)
	}
	events, err := h.st.ListReviews(ctx, v.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, len(events), 1)
}

func TestResetLegacyGradeJournalKeepsAttemptOneCompatibility(t *testing.T) {
	h := newHarness(t)
	v, err := h.svc.Prepare(ctx, session.StartOptions{Mode: model.ModePractice, Count: 5, Seed: 7})
	testutil.NoError(t, err)
	testutil.NoError(t, h.svc.Start(ctx, v.ID, t0))
	pid := v.Problems[0].ID
	h.eng.override = map[string]func(model.FrozenProblem) model.Evaluation{pid: erroredEvaluation}
	old, err := h.svc.Grade(ctx, v.ID, session.GradeOptions{ProblemID: pid})
	testutil.NoError(t, err)
	intent, err := h.st.GetOperation(ctx, session.GradeOperationID(old.ID))
	testutil.NoError(t, err)
	intent.Planned.Attempts = nil // exact pre-Task13 optional JSON shape
	testutil.NoError(t, h.st.PutOperation(ctx, intent))
	retried, err := h.svc.Grade(ctx, v.ID, session.GradeOptions{ProblemID: pid, Retry: true})
	testutil.NoError(t, err)
	testutil.Equal(t, retried.SubmissionID, old.SubmissionID)
	h.eng.override = nil
	testutil.NoError(t, h.svc.Reset(ctx, v.ID, session.ResetOptions{ProblemID: pid}))
	_, err = h.svc.Grade(ctx, v.ID, session.GradeOptions{ProblemID: pid, Retry: true})
	testutil.ErrorIs(t, err, session.ErrAnswerChanged)
}

func TestResetDeletedNewAttemptNeverGradesArchivedFiles(t *testing.T) {
	h := newHarness(t)
	v, err := h.svc.Prepare(ctx, session.StartOptions{Mode: model.ModePractice, Count: 5, Seed: 7})
	testutil.NoError(t, err)
	testutil.NoError(t, h.svc.Start(ctx, v.ID, t0))
	pid := v.Problems[0].ID
	testutil.NoError(t, h.svc.Reset(ctx, v.ID, session.ResetOptions{ProblemID: pid}))
	dir := session.AttemptDir(h.workspace, v.ID, pid, 2)
	testutil.NoError(t, os.RemoveAll(dir))
	_, err = h.svc.Grade(ctx, v.ID, session.GradeOptions{ProblemID: pid})
	testutil.NoError(t, err)
	testutil.Equal(t, h.eng.targets[len(h.eng.targets)-1].Workspace, dir)
}
