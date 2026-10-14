package store_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/lock"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

var ctx = context.Background()

var t0 = time.Date(2026, 10, 14, 9, 0, 0, 123456789, time.UTC)

func openStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "junkloud.db")
	s, err := store.Open(path)
	testutil.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s, path
}

// rawDB opens a second, independent connection to the database file for
// assertions that the Store API deliberately does not expose.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	testutil.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	testutil.NoError(t, db.QueryRow(query, args...).Scan(&n))
	return n
}

func frozen(id string) model.FrozenProblem {
	return model.FrozenProblem{
		Definition:    model.Problem{ID: id, Version: "1", Title: "Problem " + id},
		Hash:          "sha256:" + id,
		Question:      "Question " + id,
		Parameters:    map[string]any{"port": 8080, "name": "web"},
		Assets:        map[string][]byte{"client-pod.yaml": []byte("kind: Pod\n")},
		Namespace:     "ns-" + id,
		Seed:          1<<63 + 5, // above MaxInt64: must survive storage
		SeedAlgorithm: "seed-v1",
	}
}

func session(id, state string) model.SessionRecord {
	started := t0.Add(time.Minute)
	return model.SessionRecord{
		ID:            id,
		EnvironmentID: "env-1",
		Mode:          model.ModePractice,
		State:         state,
		Seed:          1<<63 + 5,
		Problems:      []model.FrozenProblem{frozen("q1"), frozen("q2")},
		StartedAt:     &started,
		DurationLimit: 2 * time.Hour,
		Marks:         map[string]bool{"q2": true},
	}
}

func mustCreate(t *testing.T, s *store.Store, id, state string) model.SessionRecord {
	t.Helper()
	rec := session(id, state)
	testutil.NoError(t, s.CreateSession(ctx, rec))
	return rec
}

func submission(id, sessionID string) model.Submission {
	return model.Submission{
		ID: id, SessionID: sessionID, Kind: "submit", SubmittedAt: t0,
		InputHashes: map[string]string{"answer": "sha256:abc"},
	}
}

func score(n int) *int { return &n }

func evaluation() model.Evaluation {
	return model.Evaluation{
		Results: []model.CheckResult{
			{CriterionID: "c1", Status: model.StatusPass, Points: 4, Evidence: model.Evidence{
				Expected: "web", Actual: "web", ObservedAt: t0, UID: "uid-1", Generation: 3, Duration: time.Second}},
			{CriterionID: "c2", Status: model.StatusFail, Reason: "port mismatch", Points: 0, Evidence: model.Evidence{
				Expected: 8080, Actual: 80, ObservedAt: t0}},
		},
		Score: score(4), MaxScore: 8, Complete: true,
	}
}

func run(id, submissionID string) model.GradingRun {
	return model.GradingRun{
		ID: id, SubmissionID: submissionID,
		StartedAt: t0, FinishedAt: t0.Add(2 * time.Second), Evaluation: evaluation(),
	}
}

func TestOpenMigratesAndReopens(t *testing.T) {
	s, path := openStore(t)
	mustCreate(t, s, "s1", model.StatePreparing)
	testutil.NoError(t, s.Close())

	db := rawDB(t, path)
	testutil.Equal(t, count(t, db, `SELECT MAX(version) FROM schema_migrations`), 1)

	s2, err := store.Open(path)
	testutil.NoError(t, err)
	defer s2.Close()
	got, err := s2.LoadSession(ctx, "s1")
	testutil.NoError(t, err)
	testutil.Equal(t, got.ID, "s1")
	testutil.Equal(t, count(t, db, `SELECT COUNT(*) FROM schema_migrations`), 1)
}

func TestSessionRoundTrip(t *testing.T) {
	s, _ := openStore(t)
	rec := mustCreate(t, s, "s1", model.StateRunning)
	rec.ParentID = ""

	got, err := s.LoadSession(ctx, "s1")
	testutil.NoError(t, err)

	// Numbers inside Parameters come back as float64 after the JSON trip;
	// private package files never persist.
	want := rec
	want.Problems = []model.FrozenProblem{frozen("q1"), frozen("q2")}
	for i := range want.Problems {
		want.Problems[i].Parameters = map[string]any{"port": float64(8080), "name": "web"}
	}
	testutil.Equal(t, got, want)
	testutil.Equal(t, got.Seed, uint64(1<<63+5))

	// A retry session links to its parent and has no start time yet.
	child := session("s2", model.StatePreparing)
	child.Mode, child.ParentID, child.StartedAt, child.Marks = model.ModeRetry, "s1", nil, nil
	testutil.NoError(t, s.CreateSession(ctx, child))
	got, err = s.LoadSession(ctx, "s2")
	testutil.NoError(t, err)
	testutil.Equal(t, got.ParentID, "s1")
	testutil.Equal(t, got.StartedAt == nil, true)
}

func TestCreateSessionRejects(t *testing.T) {
	s, _ := openStore(t)
	mustCreate(t, s, "s1", model.StatePreparing)

	testutil.ErrorIs(t, s.CreateSession(ctx, session("s1", model.StatePreparing)), store.ErrAlreadyExists)

	bad := session("s9", "BOGUS")
	testutil.ErrorIs(t, s.CreateSession(ctx, bad), store.ErrInvalidState)

	orphan := session("s8", model.StatePreparing)
	orphan.ParentID = "missing"
	testutil.ErrorIs(t, s.CreateSession(ctx, orphan), store.ErrNotFound)

	// The failed creates left nothing behind, including session problems.
	_, err := s.LoadSession(ctx, "s8")
	testutil.ErrorIs(t, err, store.ErrNotFound)
	dup := session("s7", model.StatePreparing)
	dup.Problems = []model.FrozenProblem{frozen("q1"), frozen("q1")}
	testutil.ErrorIs(t, s.CreateSession(ctx, dup), store.ErrAlreadyExists)
	_, err = s.LoadSession(ctx, "s7")
	testutil.ErrorIs(t, err, store.ErrNotFound)
}

func TestTransitionCompareAndSwap(t *testing.T) {
	s, _ := openStore(t)
	mustCreate(t, s, "s1", model.StateRunning)

	err := s.Transition(ctx, "s1", "RUNNING", "GRADING")
	testutil.NoError(t, err)
	err = s.Transition(ctx, "s1", "RUNNING", "GRADING")
	testutil.ErrorIs(t, err, store.ErrStateConflict)

	got, err := s.LoadSession(ctx, "s1")
	testutil.NoError(t, err)
	testutil.Equal(t, got.State, model.StateGrading)

	testutil.ErrorIs(t, s.Transition(ctx, "missing", "RUNNING", "GRADING"), store.ErrNotFound)
	testutil.ErrorIs(t, s.Transition(ctx, "s1", "GRADING", "BOGUS"), store.ErrInvalidState)
	testutil.ErrorIs(t, s.Transition(ctx, "s1", "BOGUS", "COMPLETED"), store.ErrInvalidState)
	got, _ = s.LoadSession(ctx, "s1")
	testutil.Equal(t, got.State, model.StateGrading)
}

func TestTransitionCompareAndSwapConcurrent(t *testing.T) {
	s, _ := openStore(t)
	mustCreate(t, s, "s1", model.StateRunning)

	const n = 16
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.Transition(ctx, "s1", model.StateRunning, model.StateGrading)
		}()
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else {
			testutil.ErrorIs(t, err, store.ErrStateConflict)
		}
	}
	testutil.Equal(t, wins, 1)
}

func TestSaveRunAtomic(t *testing.T) {
	s, path := openStore(t)
	mustCreate(t, s, "s1", model.StateGrading)
	testutil.NoError(t, s.CreateSubmission(ctx, submission("sub1", "s1")))
	db := rawDB(t, path)

	// A run and all of its criterion results are saved together.
	r := run("r1", "sub1")
	testutil.NoError(t, s.SaveRun(ctx, r))
	got, err := s.GetRun(ctx, "r1")
	testutil.NoError(t, err)
	want := r
	want.Evaluation.Results[1].Evidence.Expected = float64(8080)
	want.Evaluation.Results[1].Evidence.Actual = float64(80)
	testutil.Equal(t, got, want)
	testutil.Equal(t, count(t, db, `SELECT COUNT(*) FROM criterion_results WHERE run_id='r1'`), 2)

	// Failing part-way (second result repeats a criterion id) rolls back
	// the run row and the result already written.
	bad := run("r2", "sub1")
	bad.Evaluation.Results[1].CriterionID = "c1"
	err = s.SaveRun(ctx, bad)
	if err == nil {
		t.Fatal("SaveRun with duplicate criterion ids succeeded")
	}
	_, err = s.GetRun(ctx, "r2")
	testutil.ErrorIs(t, err, store.ErrNotFound)
	testutil.Equal(t, count(t, db, `SELECT COUNT(*) FROM criterion_results WHERE run_id='r2'`), 0)
	testutil.Equal(t, count(t, db, `SELECT COUNT(*) FROM grading_runs`), 1)

	// The same run id cannot be saved twice, and the first score survives.
	again := run("r1", "sub1")
	again.Evaluation.Score = score(8)
	again.Evaluation.Results = again.Evaluation.Results[:1]
	testutil.ErrorIs(t, s.SaveRun(ctx, again), store.ErrAlreadyExists)
	got, err = s.GetRun(ctx, "r1")
	testutil.NoError(t, err)
	testutil.Equal(t, *got.Evaluation.Score, 4)
	testutil.Equal(t, len(got.Evaluation.Results), 2)

	// A run for an unknown submission is rejected by the foreign key.
	testutil.ErrorIs(t, s.SaveRun(ctx, run("r3", "nope")), store.ErrNotFound)

	// An incomplete evaluation keeps its nil score.
	partial := run("r4", "sub1")
	partial.Evaluation.Score, partial.Evaluation.Complete = nil, false
	partial.Evaluation.Results[1].Status = model.StatusError
	testutil.NoError(t, s.SaveRun(ctx, partial))
	got, err = s.GetRun(ctx, "r4")
	testutil.NoError(t, err)
	testutil.Equal(t, got.Evaluation.Score == nil, true)
	testutil.Equal(t, got.Evaluation.Complete, false)
}

func TestSubmissionsAndRunsAreAppendOnly(t *testing.T) {
	s, path := openStore(t)
	mustCreate(t, s, "s1", model.StateGrading)
	sub := submission("sub1", "s1")
	testutil.NoError(t, s.CreateSubmission(ctx, sub))
	testutil.NoError(t, s.SaveRun(ctx, run("r1", "sub1")))

	// Re-creating a submission with different content is refused and the
	// stored one is unchanged.
	other := sub
	other.Kind = "resubmit"
	other.SubmittedAt = t0.Add(time.Hour)
	testutil.ErrorIs(t, s.CreateSubmission(ctx, other), store.ErrAlreadyExists)
	got, err := s.GetSubmission(ctx, "sub1")
	testutil.NoError(t, err)
	testutil.Equal(t, got, sub)

	// The database itself refuses UPDATE and DELETE of these records.
	db := rawDB(t, path)
	for _, stmt := range []string{
		`UPDATE submissions SET kind='x'`,
		`DELETE FROM submissions`,
		`UPDATE grading_runs SET score=99`,
		`DELETE FROM grading_runs`,
		`UPDATE criterion_results SET points=99`,
		`DELETE FROM criterion_results`,
	} {
		if _, err := db.Exec(stmt); err == nil {
			t.Errorf("%q succeeded, want append-only rejection", stmt)
		}
	}
	gotRun, err := s.GetRun(ctx, "r1")
	testutil.NoError(t, err)
	testutil.Equal(t, *gotRun.Evaluation.Score, 4)
}

func TestCreateSubmissionLinks(t *testing.T) {
	s, _ := openStore(t)
	mustCreate(t, s, "s1", model.StateGrading)
	testutil.NoError(t, s.CreateSubmission(ctx, submission("sub1", "s1")))

	re := submission("sub2", "s1")
	re.Kind, re.ParentSubmissionID = "resubmit", "sub1"
	testutil.NoError(t, s.CreateSubmission(ctx, re))
	got, err := s.GetSubmission(ctx, "sub2")
	testutil.NoError(t, err)
	testutil.Equal(t, got.ParentSubmissionID, "sub1")

	testutil.ErrorIs(t, s.CreateSubmission(ctx, submission("sub3", "missing")), store.ErrNotFound)
	orphan := submission("sub4", "s1")
	orphan.ParentSubmissionID = "missing"
	testutil.ErrorIs(t, s.CreateSubmission(ctx, orphan), store.ErrNotFound)
	_, err = s.GetSubmission(ctx, "sub3")
	testutil.ErrorIs(t, err, store.ErrNotFound)
}

func TestOperationPhaseAdvances(t *testing.T) {
	s, _ := openStore(t)
	op := model.Operation{
		ID: "op1", EnvironmentID: "env-1", SessionID: "s1", Kind: "prepare", Phase: "planned",
		Planned: model.Inventory{EnvironmentID: "env-1", SessionID: "s1", ProblemID: "q1",
			Resources: []model.OwnedResource{{Ref: model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: "ns-q1"}, OwnerID: "s1"}}},
		CreatedAt: t0,
	}
	// An operation may be recorded before its session exists: intent first.
	testutil.NoError(t, s.PutOperation(ctx, op))
	got, err := s.GetOperation(ctx, "op1")
	testutil.NoError(t, err)
	testutil.Equal(t, got, op)

	op.Phase = "applied"
	op.Planned.Resources[0].UID = "uid-1"
	testutil.NoError(t, s.PutOperation(ctx, op))
	got, err = s.GetOperation(ctx, "op1")
	testutil.NoError(t, err)
	testutil.Equal(t, got.Phase, "applied")
	testutil.Equal(t, got.Planned.Resources[0].UID, "uid-1")

	// Identity fields cannot be rewritten by a later Put.
	changed := op
	changed.Kind = "reset"
	testutil.ErrorIs(t, s.PutOperation(ctx, changed), store.ErrOperationMismatch)
	got, _ = s.GetOperation(ctx, "op1")
	testutil.Equal(t, got.Kind, "prepare")

	_, err = s.GetOperation(ctx, "nope")
	testutil.ErrorIs(t, err, store.ErrNotFound)
}

func TestInventoryReplaced(t *testing.T) {
	s, _ := openStore(t)
	inv := model.Inventory{
		EnvironmentID: "env-1", SessionID: "s1", ProblemID: "q1",
		Resources: []model.OwnedResource{
			{Ref: model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: "ns-q1"}, UID: "u1", OwnerID: "s1"}},
		DockerIDs: []string{"abc"}, ArtifactPaths: []string{"/tmp/x"},
	}
	testutil.NoError(t, s.PutInventory(ctx, inv))
	got, err := s.GetInventory(ctx, "env-1", "s1", "q1")
	testutil.NoError(t, err)
	testutil.Equal(t, got, inv)

	inv.Resources = nil
	inv.DockerIDs = nil
	testutil.NoError(t, s.PutInventory(ctx, inv))
	got, err = s.GetInventory(ctx, "env-1", "s1", "q1")
	testutil.NoError(t, err)
	testutil.Equal(t, len(got.Resources), 0)
	testutil.Equal(t, got.ArtifactPaths, []string{"/tmp/x"})

	// An environment-level inventory has no session or problem.
	envInv := model.Inventory{EnvironmentID: "env-1", DockerIDs: []string{"node"}}
	testutil.NoError(t, s.PutInventory(ctx, envInv))
	got, err = s.GetInventory(ctx, "env-1", "", "")
	testutil.NoError(t, err)
	testutil.Equal(t, got, envInv)

	_, err = s.GetInventory(ctx, "env-1", "s9", "q1")
	testutil.ErrorIs(t, err, store.ErrNotFound)
}

func TestValidationLatestWins(t *testing.T) {
	s, path := openStore(t)
	rec := model.ValidationRecord{
		ProblemHash: "sha256:p", GraderVersion: "g1", EnvironmentHash: "e1",
		Seeds: []uint64{1, 1<<63 + 7}, Passed: false,
		Cases: map[string]model.Evaluation{"initial": evaluation()},
	}
	testutil.NoError(t, s.SaveValidation(ctx, rec))
	got, err := s.GetValidation(ctx, "sha256:p", "g1", "e1")
	testutil.NoError(t, err)
	want := rec
	want.Cases = map[string]model.Evaluation{"initial": evaluation()}
	want.Cases["initial"].Results[1].Evidence.Expected = float64(8080)
	want.Cases["initial"].Results[1].Evidence.Actual = float64(80)
	testutil.Equal(t, got, want)

	// Re-validation appends; the newest record answers lookups and the old
	// one stays in the history.
	rec.Passed = true
	testutil.NoError(t, s.SaveValidation(ctx, rec))
	got, err = s.GetValidation(ctx, "sha256:p", "g1", "e1")
	testutil.NoError(t, err)
	testutil.Equal(t, got.Passed, true)
	testutil.Equal(t, count(t, rawDB(t, path), `SELECT COUNT(*) FROM problem_validations`), 2)

	// A different grader version has no certificate.
	_, err = s.GetValidation(ctx, "sha256:p", "g2", "e1")
	testutil.ErrorIs(t, err, store.ErrNotFound)
}

func TestRecordReview(t *testing.T) {
	s, path := openStore(t)
	mustCreate(t, s, "s1", model.StateCompleted)

	testutil.NoError(t, s.RecordReview(ctx, "s1", "q1", t0))
	testutil.NoError(t, s.RecordReview(ctx, "s1", "q1", t0.Add(time.Minute)))
	db := rawDB(t, path)
	testutil.Equal(t, count(t, db, `SELECT COUNT(*) FROM review_events WHERE session_id='s1' AND problem_id='q1'`), 2)

	testutil.ErrorIs(t, s.RecordReview(ctx, "s1", "q9", t0), store.ErrNotFound)
	testutil.ErrorIs(t, s.RecordReview(ctx, "missing", "q1", t0), store.ErrNotFound)
	testutil.Equal(t, count(t, db, `SELECT COUNT(*) FROM review_events`), 2)
}

func TestCorruptDBPreserved(t *testing.T) {
	t.Run("not a database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "junkloud.db")
		orig := bytes.Repeat([]byte("this is not sqlite, but it is the learner's file\n"), 200)
		testutil.NoError(t, os.WriteFile(path, orig, 0o600))

		s, err := store.Open(path)
		if err == nil {
			s.Close()
			t.Fatal("Open accepted a corrupt database")
		}
		testutil.ErrorIs(t, err, store.ErrCorrupt)
		got, rerr := os.ReadFile(path)
		testutil.NoError(t, rerr)
		testutil.Equal(t, bytes.Equal(got, orig), true)
		entries, derr := os.ReadDir(filepath.Dir(path))
		testutil.NoError(t, derr)
		testutil.Equal(t, len(entries), 1) // no stray journal files either
	})

	t.Run("damaged pages", func(t *testing.T) {
		s, path := openStore(t)
		for i := 0; i < 40; i++ {
			rec := session("s"+string(rune('a'+i%26))+string(rune('a'+i/26)), model.StatePreparing)
			testutil.NoError(t, s.CreateSession(ctx, rec))
		}
		testutil.NoError(t, s.Close())

		orig, err := os.ReadFile(path)
		testutil.NoError(t, err)
		if len(orig) < 3*4096 {
			t.Fatalf("database is only %d bytes", len(orig))
		}
		for i := 4096; i < len(orig)-4096 && i < 4096*6; i++ {
			orig[i] = 0xA5 // wreck the interior pages, keep the header valid
		}
		testutil.NoError(t, os.WriteFile(path, orig, 0o600))

		s2, err := store.Open(path)
		if err == nil {
			s2.Close()
			t.Fatal("Open accepted a damaged database")
		}
		testutil.ErrorIs(t, err, store.ErrCorrupt)
		got, rerr := os.ReadFile(path)
		testutil.NoError(t, rerr)
		testutil.Equal(t, bytes.Equal(got, orig), true)
	})
}

func TestNewerSchemaRefused(t *testing.T) {
	s, path := openStore(t)
	testutil.NoError(t, s.Close())
	db := rawDB(t, path)
	_, err := db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (99, 'x')`)
	testutil.NoError(t, err)
	db.Close()

	s2, err := store.Open(path)
	if err == nil {
		s2.Close()
		t.Fatal("Open accepted a newer schema")
	}
	testutil.ErrorIs(t, err, store.ErrSchemaTooNew)
}

func TestDatabaseUsesWAL(t *testing.T) {
	// WAL lets other terminals read while one command writes.
	_, path := openStore(t)
	var mode string
	testutil.NoError(t, rawDB(t, path).QueryRow(`PRAGMA journal_mode`).Scan(&mode))
	testutil.Equal(t, mode, "wal")
}

// --- cross-process tests -------------------------------------------------

const (
	helperEnv = "JUNKLOUD_STORE_HELPER"
	dbEnv     = "JUNKLOUD_STORE_DB"
)

// TestMain makes the test binary double as a helper process. The helper
// reports "ready", waits for a line on stdin, then performs one action
// against the shared database and prints its outcome.
func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		os.Exit(runHelper(mode))
	}
	os.Exit(m.Run())
}

func runHelper(mode string) int {
	s, err := store.Open(os.Getenv(dbEnv))
	if err != nil {
		os.Stdout.WriteString("error: open: " + err.Error() + "\n")
		return 1
	}
	defer s.Close()
	os.Stdout.WriteString("ready\n")
	bufio.NewReader(os.Stdin).ReadString('\n')

	switch mode {
	case "transition":
		err := s.Transition(ctx, "s1", model.StateRunning, model.StateGrading)
		switch {
		case err == nil:
			os.Stdout.WriteString("ok\n")
		case strings.Contains(err.Error(), store.ErrStateConflict.Error()):
			os.Stdout.WriteString("conflict\n")
		default:
			os.Stdout.WriteString("error: " + err.Error() + "\n")
			return 1
		}
	case "read":
		rec, err := s.LoadSession(ctx, "s1")
		if err != nil {
			os.Stdout.WriteString("error: " + err.Error() + "\n")
			return 1
		}
		os.Stdout.WriteString("state=" + rec.State + "\n")
	}
	return 0
}

type helperProc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *bufio.Reader
}

func startHelper(t *testing.T, mode, dbPath string) *helperProc {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"="+mode, dbEnv+"="+dbPath)
	in, err := cmd.StdinPipe()
	testutil.NoError(t, err)
	out, err := cmd.StdoutPipe()
	testutil.NoError(t, err)
	testutil.NoError(t, cmd.Start())
	h := &helperProc{cmd: cmd, stdin: in, out: bufio.NewReader(out)}
	t.Cleanup(func() { in.Close(); cmd.Process.Kill(); cmd.Wait() })
	return h
}

func (h *helperProc) line(t *testing.T) string {
	t.Helper()
	l, err := h.out.ReadString('\n')
	testutil.NoError(t, err)
	return strings.TrimSpace(l)
}

func TestTransitionAcrossProcesses(t *testing.T) {
	s, path := openStore(t)
	mustCreate(t, s, "s1", model.StateRunning)

	const n = 6
	hs := make([]*helperProc, n)
	for i := range hs {
		hs[i] = startHelper(t, "transition", path)
		testutil.Equal(t, hs[i].line(t), "ready")
	}
	for _, h := range hs { // release them all at once
		io.WriteString(h.stdin, "go\n")
	}
	ok, conflict := 0, 0
	for _, h := range hs {
		switch got := h.line(t); got {
		case "ok":
			ok++
		case "conflict":
			conflict++
		default:
			t.Fatalf("unexpected helper output %q", got)
		}
		testutil.NoError(t, h.cmd.Wait())
	}
	testutil.Equal(t, ok, 1)
	testutil.Equal(t, conflict, n-1)

	got, err := s.LoadSession(ctx, "s1")
	testutil.NoError(t, err)
	testutil.Equal(t, got.State, model.StateGrading)
}

// TestReadersUnblockedByEnvironmentLock checks the spec's rule that one
// mutating operation holds the environment lock while other terminals can
// still read: a second process reads while this one holds the lock and has
// an open writer.
func TestReadersUnblockedByEnvironmentLock(t *testing.T) {
	s, path := openStore(t)
	mustCreate(t, s, "s1", model.StateRunning)
	release, err := lock.Acquire(filepath.Join(filepath.Dir(path), "env.lock"))
	testutil.NoError(t, err)
	defer release()

	h := startHelper(t, "read", path)
	testutil.Equal(t, h.line(t), "ready")
	io.WriteString(h.stdin, "go\n")
	testutil.Equal(t, h.line(t), "state=RUNNING")
	testutil.NoError(t, h.cmd.Wait())

	// The reader sees later changes made by this process too.
	testutil.NoError(t, s.Transition(ctx, "s1", model.StateRunning, model.StateGrading))
	h = startHelper(t, "read", path)
	testutil.Equal(t, h.line(t), "ready")
	io.WriteString(h.stdin, "go\n")
	testutil.Equal(t, h.line(t), "state=GRADING")
	testutil.NoError(t, h.cmd.Wait())
}
