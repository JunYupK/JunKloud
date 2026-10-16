//go:build integration

package integration

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/lock"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// fixturePackage is the partial-credit problem: 2 of 10 before any work, 10
// for the reference answer, 6 for a Deployment with the wrong replica count,
// 10 again for an equivalent answer written differently. It uses only
// resource, field and ready criteria and the preloaded probe image.
var fixturePackage = filepath.Join("..", "..", "internal", "author", "testdata", "partial-credit")

func loadValidationFixture(t *testing.T) model.Problem {
	t.Helper()
	p, err := problem.Load(fixturePackage)
	testutil.NoError(t, err)
	return p
}

// pinnedValidationFixture is the fixture with each parameter narrowed to the
// value seed freezes, so validating with that one seed covers every allowed
// value (a validation must; author.CoveringSeeds).
func pinnedValidationFixture(t *testing.T, seed uint64) model.Problem {
	t.Helper()
	p := loadValidationFixture(t)
	f, err := problem.Freeze(p, seed)
	testutil.NoError(t, err)
	p.Parameters = map[string][]any{}
	for name, v := range f.Parameters {
		p.Parameters[name] = []any{v}
	}
	return p
}

// validationLeftovers lists namespaces and cluster-scoped objects that carry
// the validation owner's session label, anywhere in the cluster.
func validationLeftovers(t *testing.T, m *environment.Manager, id model.Identity) []string {
	t.Helper()
	cs := kubeClient(t, m, id)
	selector := environment.LabelSessionID + "=" + author.SessionID
	nss, err := cs.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{LabelSelector: selector})
	testutil.NoError(t, err)
	var left []string
	for _, ns := range nss.Items {
		left = append(left, "namespace/"+ns.Name)
	}
	deployments, err := cs.AppsV1().Deployments("").List(context.Background(), metav1.ListOptions{LabelSelector: selector})
	testutil.NoError(t, err)
	for _, d := range deployments.Items {
		left = append(left, "deployment/"+d.Namespace+"/"+d.Name)
	}
	return left
}

// distinctSeeds returns two seeds for which the fixture renders a different
// namespace and a different replica count, so one run proves that parameters
// are re-rendered and that nothing leaks from one seed's bundle into the next.
func distinctSeeds(t *testing.T, p model.Problem) (uint64, uint64, model.FrozenProblem, model.FrozenProblem) {
	t.Helper()
	first, err := problem.Freeze(p, 1)
	testutil.NoError(t, err)
	for seed := uint64(2); seed < 500; seed++ {
		f, err := problem.Freeze(p, seed)
		testutil.NoError(t, err)
		if f.Namespace != first.Namespace && f.Parameters["replicas"] != first.Parameters["replicas"] {
			return 1, seed, first, f
		}
	}
	t.Fatal("no two seeds render different namespaces and replica counts")
	return 0, 0, first, model.FrozenProblem{}
}

func evaluationStatuses(ev model.Evaluation) map[string]string {
	out := map[string]string{}
	for _, r := range ev.Results {
		out[r.CriterionID] = r.Status
	}
	return out
}

func TestProblemValidation(t *testing.T) {
	requireTools(t, "docker", "kind")
	ctx := context.Background()
	m, id, dir, st := bootEnvironmentState(t)
	testutil.NoError(t, m.EnsurePlatform(ctx, id))

	cfg := author.GraderConfig{StateDir: dir}
	rt, err := author.NewRuntime(ctx, m, st, id, cfg)
	testutil.NoError(t, err)
	t.Cleanup(func() { rt.Close() })

	p := loadValidationFixture(t)
	hash, err := problem.Hash(p)
	testutil.NoError(t, err)
	envHash, err := cfg.EnvironmentHash(ctx, id)
	testutil.NoError(t, err)
	testutil.Equal(t, rt.EnvironmentHash, envHash)

	certified := func(h string) bool {
		t.Helper()
		_, ok, err := author.CurrentCertificate(ctx, st, h, grader.GraderVersion, envHash)
		testutil.NoError(t, err)
		return ok
	}
	testutil.Equal(t, certified(hash), false) // nothing is certified before it is validated

	seedA, seedB, frozenA, frozenB := distinctSeeds(t, p)

	t.Run("ValidatesTheFixtureWithTheRuntimeGrader", func(t *testing.T) {
		start := time.Now()
		record, err := rt.Validator.Validate(ctx, p, []uint64{seedA, seedB})
		t.Logf("validated seeds %d (%s, %v replicas) and %d (%s, %v replicas) in %v", seedA, frozenA.Namespace,
			frozenA.Parameters["replicas"], seedB, frozenB.Namespace, frozenB.Parameters["replicas"], time.Since(start))
		testutil.NoError(t, err)
		testutil.Equal(t, record.Passed, true)
		testutil.Equal(t, record.Seeds, []uint64{seedA, seedB})
		testutil.Equal(t, record.ProblemHash, hash)
		testutil.Equal(t, record.GraderVersion, grader.GraderVersion)
		testutil.Equal(t, record.EnvironmentHash, envHash)

		// Initial 2, solved 10, planted wrong answer 6, equivalent answer 10,
		// and the initial state again after the solved run.
		testutil.Equal(t, record.Cases[author.KeyInitial].MaxScore, 10)
		testutil.Equal(t, *record.Cases[author.KeyInitial].Score, 2)
		testutil.Equal(t, *record.Cases[author.KeySolved].Score, 10)
		testutil.Equal(t, *record.Cases[author.CaseKey("wrong-replicas")].Score, 6)
		testutil.Equal(t, *record.Cases[author.CaseKey("alt-short-image")].Score, 10)
		testutil.Equal(t, *record.Cases[author.KeyReset].Score, 2)
		testutil.Equal(t, evaluationStatuses(record.Cases[author.KeyInitial]), p.InitialExpected)
		testutil.Equal(t, evaluationStatuses(record.Cases[author.CaseKey("wrong-replicas")]), p.Cases[0].Expected)
		testutil.Equal(t, evaluationStatuses(record.Cases[author.KeyReset]), p.InitialExpected)
		// The second seed is proven the same way under its own keys.
		testutil.Equal(t, len(record.Cases), 10)
		testutil.Equal(t, *record.Cases[author.SeedKey(seedB, author.KeyInitial)].Score, 2)
		testutil.Equal(t, *record.Cases[author.SeedKey(seedB, author.KeySolved)].Score, 10)
		testutil.Equal(t, *record.Cases[author.SeedKey(seedB, author.CaseKey("wrong-replicas"))].Score, 6)
		testutil.Equal(t, *record.Cases[author.SeedKey(seedB, author.KeyReset)].Score, 2)

		// The wrong answer fails for the stated reason, with evidence from
		// the live object.
		for _, r := range record.Cases[author.CaseKey("wrong-replicas")].Results {
			switch r.CriterionID {
			case "web-ready":
				if !strings.Contains(r.Reason, "rollout target") {
					t.Errorf("web-ready reason = %q", r.Reason)
				}
			case "web-replicas":
				testutil.Equal(t, r.Status, model.StatusFail)
			}
		}
		for _, r := range record.Cases[author.KeySolved].Results {
			if r.Evidence.ObservedAt.IsZero() {
				t.Errorf("%s has no observation time", r.CriterionID)
			}
		}

		// The certificate was stored and is current; every temporary object
		// is gone from the cluster.
		testutil.Equal(t, certified(hash), true)
		testutil.Equal(t, validationLeftovers(t, m, id), []string(nil))
		inv, err := st.GetInventory(ctx, id.ID, author.SessionID, p.ID)
		testutil.NoError(t, err)
		testutil.Equal(t, len(inv.Resources), 0)
	})

	t.Run("StaleCertificate", func(t *testing.T) {
		// A different problem hash, grader version or environment hash finds
		// no valid certificate and demands a new validation.
		changed := p
		changed.Files = maps.Clone(p.Files)
		changed.Files["solution/web.yaml"] = append(slices.Clone(p.Files["solution/web.yaml"]), "# edited\n"...)
		changedHash, err := problem.Hash(changed)
		testutil.NoError(t, err)
		testutil.Equal(t, certified(changedHash), false)

		record, ok, err := author.CurrentCertificate(ctx, st, hash, grader.GraderVersion, envHash)
		testutil.NoError(t, err)
		testutil.Equal(t, ok, true)
		testutil.Equal(t, author.CertificateMatches(record, changedHash, grader.GraderVersion, envHash), false)
		testutil.Equal(t, author.CertificateMatches(record, hash, "grader-99", envHash), false)
		tools, err := author.DetectToolVersions(ctx, grader.NewToolRunner())
		testutil.NoError(t, err)
		testutil.Equal(t, tools["helm"] != "" && tools["kubectl"] != "" && tools["docker-server"] != "", true)
		tools["helm"] = "v9.9.9"
		moved, err := author.EnvironmentManifest(id, cfg.ProvidedImages(), tools)
		testutil.NoError(t, err)
		testutil.Equal(t, author.CertificateMatches(record, hash, grader.GraderVersion, author.EnvironmentHash(moved)), false)
		pins := cfg.ProvidedImages()
		pins[0].Digest = "sha256:" + strings.Repeat("0", 64)
		moved, err = author.EnvironmentManifest(id, pins, tools)
		testutil.NoError(t, err)
		testutil.Equal(t, author.CertificateMatches(record, hash, grader.GraderVersion, author.EnvironmentHash(moved)), false)
		_, ok, err = author.CurrentCertificate(ctx, st, hash, "grader-99", envHash)
		testutil.NoError(t, err)
		testutil.Equal(t, ok, false)
	})

	t.Run("BlockedWhileASessionIsActive", func(t *testing.T) {
		testutil.NoError(t, st.CreateSession(ctx, model.SessionRecord{
			ID: "practice-1", EnvironmentID: id.ID, Mode: model.ModePractice, State: model.StateRunning,
		}))
		other := p
		other.ID = "scale-web-blocked"
		_, err := rt.Validator.Validate(ctx, other, []uint64{seedA})
		testutil.ErrorIs(t, err, author.ErrSessionActive)
		testutil.Equal(t, validationLeftovers(t, m, id), []string(nil))

		// The environment lock is honoured as well.
		testutil.NoError(t, st.Transition(ctx, "practice-1", model.StateRunning, model.StateAbandoned))
		release, err := lock.Acquire(author.LockPath(dir))
		testutil.NoError(t, err)
		_, err = rt.Validator.Validate(ctx, other, []uint64{seedA})
		testutil.ErrorIs(t, err, lock.ErrLocked)
		testutil.NoError(t, release())
	})

	// A grader that is too lax is exactly what validation exists to catch:
	// without the replicas argument the ready criterion passes the planted
	// wrong answer too.
	t.Run("RejectsALaxGrader", func(t *testing.T) {
		lax := pinnedValidationFixture(t, seedA)
		lax.Criteria = slices.Clone(lax.Criteria)
		for i := range lax.Criteria {
			if lax.Criteria[i].ID == "web-ready" {
				lax.Criteria[i].Args = nil
			}
		}
		lax.Cases = lax.Cases[:1] // only the wrong answer, to keep the run short
		record, err := rt.Validator.Validate(ctx, lax, []uint64{seedA})
		testutil.ErrorIs(t, err, author.ErrNotValid)
		var verr *author.ValidationError
		testutil.Equal(t, errors.As(err, &verr), true)
		if !strings.Contains(err.Error(), "wrong-replicas") || !strings.Contains(err.Error(), `"web-ready": got PASS, want FAIL`) {
			t.Errorf("error does not name the lax criterion: %v", err)
		}
		testutil.Equal(t, record.Passed, false)
		testutil.Equal(t, *record.Cases[author.CaseKey("wrong-replicas")].Score, 8)
		laxHash, err := problem.Hash(lax)
		testutil.NoError(t, err)
		testutil.Equal(t, certified(laxHash), false)
		testutil.Equal(t, validationLeftovers(t, m, id), []string(nil))
	})

	// A problem whose setup already contains the answer starts at full marks
	// and must never be served.
	t.Run("RejectsAnInitiallyPerfectProblem", func(t *testing.T) {
		solved := pinnedValidationFixture(t, seedA)
		solved.Setup = append(slices.Clone(solved.Setup), solved.Solution...)
		solved.Cases = solved.Cases[:1]
		record, err := rt.Validator.Validate(ctx, solved, []uint64{seedA})
		testutil.ErrorIs(t, err, author.ErrNotValid)
		if !strings.Contains(err.Error(), "full marks") {
			t.Errorf("error does not say the problem starts at full marks: %v", err)
		}
		testutil.Equal(t, record.Passed, false)
		testutil.Equal(t, *record.Cases[author.KeyInitial].Score, 10)
		solvedHash, err := problem.Hash(solved)
		testutil.NoError(t, err)
		testutil.Equal(t, certified(solvedHash), false)
		testutil.Equal(t, validationLeftovers(t, m, id), []string(nil))
	})

	// Validation owns only what it created. A namespace of the same name that
	// belongs to somebody else (here a learner session) is never adopted,
	// cleaned or deleted: validation stops with the conflict, which is not a
	// verdict about the problem.
	t.Run("NeverTouchesAForeignNamespace", func(t *testing.T) {
		cs := kubeClient(t, m, id)
		foreign := frozenA.Namespace
		labels := map[string]string{
			environment.LabelEnvironmentID: id.ID, environment.LabelSessionID: "practice-1",
			environment.LabelProblemID: p.ID, environment.LabelManagedBy: environment.ManagedByValue,
		}
		_, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: foreign, Labels: labels}}, metav1.CreateOptions{})
		testutil.NoError(t, err)
		_, err = cs.CoreV1().ConfigMaps(foreign).Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "settings", Labels: labels}, Data: map[string]string{"mode": "learner's"},
		}, metav1.CreateOptions{})
		testutil.NoError(t, err)

		other := pinnedValidationFixture(t, seedA)
		other.Version = "1.0.1" // a package change: a fresh hash, so no certificate exists
		_, err = rt.Validator.Validate(ctx, other, []uint64{seedA})
		testutil.ErrorIs(t, err, environment.ErrNotOwned)
		if errors.Is(err, author.ErrNotValid) {
			t.Fatalf("a namespace conflict was reported as an invalid problem: %v", err)
		}
		ns, err := cs.CoreV1().Namespaces().Get(ctx, foreign, metav1.GetOptions{})
		testutil.NoError(t, err)
		testutil.Equal(t, ns.DeletionTimestamp == nil, true)
		testutil.Equal(t, ns.Labels[environment.LabelSessionID], "practice-1")
		cmap, err := cs.CoreV1().ConfigMaps(foreign).Get(ctx, "settings", metav1.GetOptions{})
		testutil.NoError(t, err)
		testutil.Equal(t, cmap.Data["mode"], "learner's")
		otherHash, err := problem.Hash(other)
		testutil.NoError(t, err)
		testutil.Equal(t, certified(otherHash), false)
	})

	// Every criterion of the fixture carries its Korean feedback (spec 8.1),
	// rendered with the seed's parameters like the rest of the definition.
	t.Run("CriteriaCarryKoreanFeedback", func(t *testing.T) {
		for _, c := range frozenA.Definition.Criteria {
			if c.Feedback == "" || strings.Contains(c.Feedback, "${") {
				t.Errorf("criterion %s feedback = %q", c.ID, c.Feedback)
			}
		}
	})

	// Seeds that never freeze some allowed value certify nothing: a session
	// could serve that value (spec 7.1). Nothing is applied.
	t.Run("RefusesSeedsThatMissAParameterValue", func(t *testing.T) {
		other := loadValidationFixture(t)
		other.Version = "1.0.2"
		missing, err := author.UncoveredValues(other, []uint64{seedA})
		testutil.NoError(t, err)
		testutil.Equal(t, len(missing) > 0, true)
		record, err := rt.Validator.Validate(ctx, other, []uint64{seedA})
		testutil.ErrorIs(t, err, author.ErrNotValid)
		testutil.Equal(t, record.Passed, false)
		for _, m := range missing {
			if !strings.Contains(err.Error(), m) {
				t.Errorf("the finding does not name %s: %v", m, err)
			}
		}
		testutil.Equal(t, len(record.Cases), 0)
		testutil.Equal(t, validationLeftovers(t, m, id), []string(nil))
	})

	// A file criterion is judged in the workspace the session will serve: the
	// provided files, rendered. Before the fix validation graded without a
	// workspace and such a problem could never be certified.
	t.Run("GradesFilesInTheServedWorkspace", func(t *testing.T) {
		withFile := pinnedValidationFixture(t, seedA)
		withFile.Version = "1.0.3"
		withFile.Files = maps.Clone(withFile.Files)
		withFile.Files["setup/provided/notes.txt"] = []byte("namespace ${namespace}\n")
		withFile.Criteria = append(slices.Clone(withFile.Criteria), model.Criterion{
			ID: "notes-provided", Kind: model.CriterionFile, Points: 1, Feedback: "제공된 notes.txt 파일이 그대로 있는지 확인하세요.",
			Args: map[string]any{"path": "notes.txt", "contains": []any{"namespace ${namespace}"}},
		})
		withFile.InitialExpected = maps.Clone(withFile.InitialExpected)
		withFile.InitialExpected["notes-provided"] = model.StatusPass
		withFile.InitialScore++
		withFile.Cases = slices.Clone(withFile.Cases[:1])
		withFile.Cases[0].Expected = maps.Clone(withFile.Cases[0].Expected)
		withFile.Cases[0].Expected["notes-provided"] = model.StatusPass
		withFile.Cases[0].ExpectedScore++
		record, err := rt.Validator.Validate(ctx, withFile, []uint64{seedA})
		testutil.NoError(t, err)
		testutil.Equal(t, record.Passed, true)
		for _, key := range []string{author.KeyInitial, author.KeySolved, author.KeyReset} {
			for _, r := range record.Cases[key].Results {
				if r.CriterionID == "notes-provided" {
					testutil.Equal(t, r.Status, model.StatusPass)
				}
			}
		}
		testutil.Equal(t, validationLeftovers(t, m, id), []string(nil))
		entries, _ := filepath.Glob(filepath.Join(dir, "validate", "*"))
		testutil.Equal(t, len(entries), 0) // the scratch workspaces are gone
	})

	// The first, valid problem keeps its certificate through all of the above.
	testutil.Equal(t, certified(hash), true)
}
