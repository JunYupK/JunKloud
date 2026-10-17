//go:build integration

package integration

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/lock"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// devBank is the development bank: five fixture problems (one of them an
// intentionally broken app) and a dev exam. It is test content; it is not
// counted as product problems.
var devBank = filepath.Join("..", "fixtures", "dev-bank")

var devExamOrder = []string{"dev-service-001", "dev-config-001", "dev-crashloop-001", "dev-sa-001", "dev-scale-001"}

// TestSessionStart prepares and starts a mock session on a real environment
// from the five dev fixtures: the fixtures are validated first (so they have
// certificates), a session is prepared without revealing any question, the
// intentionally broken app really is broken and still accepted, a second
// session is refused, and Start publishes the questions and runs the clock.
func TestSessionStart(t *testing.T) {
	requireTools(t, "docker", "kind")
	ctx := context.Background()
	m, id, dir, st := bootEnvironmentState(t)

	cfg := author.GraderConfig{StateDir: dir}
	rt, err := author.NewRuntime(ctx, m, st, id, cfg)
	testutil.NoError(t, err)
	t.Cleanup(func() { rt.Close() })

	// Certify the dev fixtures with the runtime grader (ledger: T10 depends on
	// T9 certificates).
	dirs, err := filepath.Glob(filepath.Join(devBank, "problems", "*"))
	testutil.NoError(t, err)
	testutil.Equal(t, len(dirs), 5)
	for _, d := range dirs {
		p, err := problem.Load(d)
		testutil.NoError(t, err)
		seeds, err := author.CoveringSeeds(p)
		testutil.NoError(t, err)
		start := time.Now()
		record, err := rt.Validator.Validate(ctx, p, seeds)
		t.Logf("validated %s with seeds %v in %v", p.ID, seeds, time.Since(start).Round(time.Second))
		testutil.NoError(t, err)
		testutil.Equal(t, record.Passed, true)
	}

	// The identity commands compose the runtime with comes from
	// EnsureEnvironment, under the environment lock (I7): the same
	// environment, and a concurrent holder of the lock is refused.
	t.Run("EnsureEnvironmentTakesTheLock", func(t *testing.T) {
		got, err := author.EnsureEnvironment(ctx, m, dir)
		testutil.NoError(t, err)
		testutil.Equal(t, got.ID, id.ID)
		release, err := lock.Acquire(author.LockPath(dir))
		testutil.NoError(t, err)
		_, err = author.EnsureEnvironment(ctx, m, dir)
		testutil.ErrorIs(t, err, lock.ErrLocked)
		testutil.NoError(t, release())
	})

	workspace := t.TempDir()
	svc := session.New(st, m, rt, devBank, dir, session.WithWorkspace(workspace))
	mock := session.StartOptions{Mode: model.ModeMock, MockID: "dev-mock", Seed: 7}

	// A bank whose package changed after validation is refused: no
	// certificate for the new content. The environment is not touched.
	t.Run("RefusesAnUnvalidatedPackage", func(t *testing.T) {
		edited := t.TempDir()
		copyDir(t, devBank, edited)
		expl := filepath.Join(edited, "problems", "dev-sa-001", "explanation.ko.md")
		b, err := os.ReadFile(expl)
		testutil.NoError(t, err)
		testutil.NoError(t, os.WriteFile(expl, append(b, "\n수정됨\n"...), 0o644))
		other := session.New(st, m, rt, edited, dir, session.WithWorkspace(t.TempDir()))
		_, err = other.Prepare(ctx, mock)
		testutil.ErrorIs(t, err, session.ErrNotCertified)
		if !strings.Contains(err.Error(), "dev-sa-001") {
			t.Errorf("error does not name the problem: %v", err)
		}
		active, err := st.ActiveSessions(ctx, id.ID)
		testutil.NoError(t, err)
		testutil.Equal(t, len(active), 0)
	})

	var prepared session.SessionView
	t.Run("PrepareHidesQuestions", func(t *testing.T) {
		start := time.Now()
		var err error
		prepared, err = svc.Prepare(ctx, mock)
		t.Logf("prepared %d problems in %v", len(prepared.Problems), time.Since(start).Round(time.Second))
		testutil.NoError(t, err)
		testutil.Equal(t, prepared.State, model.StatePreparing)
		testutil.Equal(t, prepared.StartedAt, (*time.Time)(nil))
		testutil.Equal(t, prepared.DurationLimit, 120*time.Minute)
		var ids []string
		for _, f := range prepared.Problems {
			ids = append(ids, f.ID)
			testutil.Equal(t, f.Question, "")
		}
		testutil.Equal(t, ids, devExamOrder)

		// The grader version and environment manifest the problems were
		// certified against are frozen on the session.
		stored, err := st.LoadSession(ctx, prepared.ID)
		testutil.NoError(t, err)
		testutil.Equal(t, stored.GraderVersion, grader.GraderVersion)
		testutil.Equal(t, stored.EnvironmentHash, rt.EnvironmentHash)
		testutil.Equal(t, author.EnvironmentHash(stored.EnvironmentManifest), rt.EnvironmentHash)
		if stored.EnvironmentManifest["kubernetes"] == "" || stored.EnvironmentManifest["tool:helm"] == "" {
			t.Errorf("manifest incomplete: %v", stored.EnvironmentManifest)
		}

		publicQuestionFilesBeforeStart := 0
		_ = filepath.WalkDir(workspace, func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				publicQuestionFilesBeforeStart++
			}
			return nil
		})
		testutil.Equal(t, publicQuestionFilesBeforeStart, 0)

		// Every problem's setup is really in the cluster, owned by the session.
		cs := kubeClient(t, m, id)
		for _, f := range prepared.Problems {
			ns, err := cs.CoreV1().Namespaces().Get(ctx, f.Namespace, metav1.GetOptions{})
			testutil.NoError(t, err)
			testutil.Equal(t, ns.Labels[environment.LabelSessionID], prepared.ID)
			testutil.Equal(t, ns.Labels[environment.LabelProblemID], f.ID)
			inv, err := st.GetInventory(ctx, id.ID, prepared.ID, f.ID)
			testutil.NoError(t, err)
			if len(inv.Resources) < 2 {
				t.Errorf("%s: inventory has %d resources", f.ID, len(inv.Resources))
			}
		}
	})

	// The broken app is broken, and that is what the problem declares.
	t.Run("BrokenInitialStateIsExpected", func(t *testing.T) {
		cs := kubeClient(t, m, id)
		d, err := cs.AppsV1().Deployments("dev-crash").Get(ctx, "api", metav1.GetOptions{})
		testutil.NoError(t, err)
		testutil.Equal(t, d.Status.ReadyReplicas, int32(0))
		p, err := problem.Load(filepath.Join(devBank, "problems", "dev-crashloop-001"))
		testutil.NoError(t, err)
		testutil.Equal(t, p.InitialExpected["api-ready"], model.StatusFail)
		stored, err := st.LoadSession(ctx, prepared.ID)
		testutil.NoError(t, err)
		testutil.Equal(t, stored.State, model.StatePreparing)
	})

	t.Run("SecondSessionRejected", func(t *testing.T) {
		_, err := svc.Prepare(ctx, session.StartOptions{Mode: model.ModePractice, Count: 5})
		testutil.ErrorIs(t, err, session.ErrSessionActive)
		active, err := st.ActiveSessions(ctx, id.ID)
		testutil.NoError(t, err)
		testutil.Equal(t, len(active), 1)
	})

	t.Run("StartClock", func(t *testing.T) {
		at := time.Now()
		testutil.NoError(t, svc.Start(ctx, prepared.ID, at))

		status, err := svc.Status(ctx, prepared.ID, at.Add(121*time.Minute))
		testutil.NoError(t, err)
		testutil.Equal(t, status.Session.State, model.StateRunning)
		testutil.Equal(t, status.Elapsed, 121*time.Minute)
		testutil.Equal(t, status.Overtime, time.Minute)
		testutil.Equal(t, status.ClockAnomaly, false)
		testutil.Equal(t, status.Session.StartedAt.Location(), time.UTC)

		// The learner now has the English question and the provided files.
		for _, f := range status.Session.Problems {
			attempt := session.AttemptDir(workspace, prepared.ID, f.ID, 1)
			q, err := os.ReadFile(filepath.Join(attempt, session.QuestionFile))
			testutil.NoError(t, err)
			if !strings.HasPrefix(string(q), "# ") || f.Question != string(q) {
				t.Errorf("%s: question = %q", f.ID, q)
			}
			for _, name := range slices.Sorted(maps.Keys(f.Assets)) {
				got, err := os.ReadFile(filepath.Join(attempt, name))
				testutil.NoError(t, err)
				testutil.Equal(t, got, f.Assets[name])
			}
		}
		// Provided files are rendered with the frozen parameters.
		cfgFile := session.AttemptDir(workspace, prepared.ID, "dev-config-001", 1)
		tmpl, err := os.ReadFile(filepath.Join(cfgFile, "settings-template.yaml"))
		testutil.NoError(t, err)
		if strings.Contains(string(tmpl), "${") {
			t.Errorf("provided file is not rendered: %s", tmpl)
		}

		// Private content never reaches the workspace.
		for _, marker := range []string{"DEV-SOLUTION-MARKER", "DEV-CASE-MARKER", "DEV-EXPLANATION-MARKER"} {
			err := filepath.WalkDir(workspace, func(path string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				b, err := os.ReadFile(path)
				if err == nil && strings.Contains(string(b), marker) {
					return errors.New(path + " contains " + marker)
				}
				return err
			})
			testutil.NoError(t, err)
		}

		// Still the only session, now running.
		_, err = svc.Prepare(ctx, mock)
		testutil.ErrorIs(t, err, session.ErrSessionActive)
	})
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	testutil.NoError(t, err)
}
