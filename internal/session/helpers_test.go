//go:build linux

package session_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

var ctx = context.Background()

// devBank is the development bank: five fixture problems and one exam. It is
// test content, not product content.
const devBank = "../../tests/fixtures/dev-bank"

const (
	testEnvID         = "env-test"
	testGraderVersion = "grader-test"
)

// testManifest is the environment version manifest of the fake environment;
// certificates are keyed by its hash, exactly as the real composition does.
var testManifest = map[string]string{
	"kind": "v0.33.0", "kubernetes": "v1.37.0", "probe-image": "docker.io/library/busybox:1.37.0",
	"tool:helm": "v4.3.0", "tool:kubectl": "v1.37.0",
}

var testEnvHash = author.EnvironmentHash(testManifest)

// Markers planted in the fixtures' private files (solution, case, explanation).
var privateMarkers = []string{"DEV-SOLUTION-MARKER", "DEV-CASE-MARKER", "DEV-EXPLANATION-MARKER"}

var testIdentity = model.Identity{ID: testEnvID, ClusterName: "junkloud-" + testEnvID, Context: "junkloud-" + testEnvID}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
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

// fakeClock is a settable clock; tests never sleep.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

var t0 = time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)

// event is one cluster-facing call, in order.
type event struct {
	Kind    string // ensure, prepare-env, doctor, apply, evaluate, check
	Problem string
	Session string
}

// fakeEnv stands in for the Manager: it applies nothing but records each call
// and, like the real Manager, stores the inventory of what it "created".
type fakeEnv struct {
	mu       sync.Mutex
	st       *store.Store
	identity model.Identity
	report   environment.DoctorReport

	ensureErr, checkErr, doctorErr error
	applyErr                       func(owner environment.Owner) error

	events []event
	owners []environment.Owner
	acts   map[string][]environment.Action

	occupiedErr   error
	occupiedCalls int
}

// Occupied reports, like a cluster that keeps what setups created until a
// reset, every asked namespace or cluster-scoped object that an earlier
// owner's setup created (and reset did not remove).
func (e *fakeEnv) Occupied(_ context.Context, _ model.Identity, namespaces []string, cluster []model.ObjectRef) ([]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.occupiedCalls++
	if e.occupiedErr != nil {
		return nil, e.occupiedErr
	}
	var out []string
	for _, o := range e.owners {
		for _, ns := range append([]string{o.Namespace}, o.Scope.Namespaces...) {
			if slices.Contains(namespaces, ns) {
				out = append(out, fmt.Sprintf("namespace %q (session %s)", ns, o.Inventory.SessionID))
			}
		}
		for _, r := range o.Scope.ClusterResources {
			if slices.Contains(cluster, r) {
				out = append(out, fmt.Sprintf("%s %q (session %s)", r.Kind, r.Name, o.Inventory.SessionID))
			}
		}
	}
	return out, nil
}

// reset forgets what a session's setups created, as the reset task will.
func (e *fakeEnv) reset(sessionID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.owners = slices.DeleteFunc(e.owners, func(o environment.Owner) bool { return o.Inventory.SessionID == sessionID })
}

func (e *fakeEnv) log(ev event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
}

func (e *fakeEnv) Ensure(context.Context) (model.Identity, error) {
	e.log(event{Kind: "ensure"})
	return e.identity, e.ensureErr
}

func (e *fakeEnv) Check(context.Context, model.Identity) error {
	e.log(event{Kind: "check"})
	return e.checkErr
}

func (e *fakeEnv) Doctor(context.Context, model.Identity) (environment.DoctorReport, error) {
	e.log(event{Kind: "doctor"})
	return e.report, e.doctorErr
}

func (e *fakeEnv) Apply(ctx context.Context, id model.Identity, owner environment.Owner, actions []environment.Action) (model.Inventory, error) {
	e.log(event{Kind: "apply", Problem: owner.Inventory.ProblemID, Session: owner.Inventory.SessionID})
	e.mu.Lock()
	e.owners = append(e.owners, owner)
	if e.acts == nil {
		e.acts = map[string][]environment.Action{}
	}
	e.acts[owner.Inventory.ProblemID] = actions
	e.mu.Unlock()

	inv := owner.Inventory
	inv.Resources = append(inv.Resources, model.OwnedResource{
		Ref: model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: owner.Namespace}, UID: "uid-" + owner.Namespace,
		OwnerID: environment.OwnerID(owner.Inventory),
	})
	if err := e.st.PutInventory(ctx, inv); err != nil {
		return inv, err
	}
	if e.applyErr != nil {
		if err := e.applyErr(owner); err != nil {
			return inv, err
		}
	}
	return inv, nil
}

func (e *fakeEnv) count(kind string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, ev := range e.events {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

func (e *fakeEnv) kinds() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, ev := range e.events {
		out = append(out, ev.Kind)
	}
	return out
}

// fakeEngine grades every problem as its declared starting state unless a
// test overrides it, which is what a correctly prepared environment yields.
type fakeEngine struct {
	mu       sync.Mutex
	env      *fakeEnv
	override map[string]func(model.FrozenProblem) model.Evaluation
	// errs makes Evaluate fail for a problem (no evaluation at all).
	errs    map[string]error
	targets []grader.Target
	seen    map[string][]string // problem ID -> workspace files at evaluation time
	// beforeReturn runs inside Evaluate, after the evaluation is chosen and
	// before it is returned: tests use it to move the clock, change the
	// learner's answer mid-grading or kill the process.
	beforeReturn func(model.FrozenProblem)
}

func initialEvaluation(f model.FrozenProblem) model.Evaluation {
	ev := model.Evaluation{Complete: true}
	score := 0
	for _, c := range f.Definition.Criteria {
		status := f.Definition.InitialExpected[c.ID]
		r := model.CheckResult{CriterionID: c.ID, Status: status, Reason: "observed " + status}
		if status == model.StatusPass {
			r.Points = c.Points
			score += c.Points
		}
		ev.Results = append(ev.Results, r)
		ev.MaxScore += c.Points
	}
	ev.Score = &score
	return ev
}

func (g *fakeEngine) Evaluate(ctx context.Context, t grader.Target) (model.Evaluation, error) {
	g.env.log(event{Kind: "evaluate", Problem: t.Problem.Definition.ID})
	var files []string
	if t.Workspace != "" {
		_ = filepath.WalkDir(t.Workspace, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				rel, _ := filepath.Rel(t.Workspace, path)
				files = append(files, filepath.ToSlash(rel))
			}
			return nil
		})
	}
	g.mu.Lock()
	g.targets = append(g.targets, t)
	if g.seen == nil {
		g.seen = map[string][]string{}
	}
	g.seen[t.Problem.Definition.ID] = files
	fn := g.override[t.Problem.Definition.ID]
	evalErr := g.errs[t.Problem.Definition.ID]
	hook := g.beforeReturn
	g.mu.Unlock()
	if hook != nil {
		hook(t.Problem)
	}
	if evalErr != nil {
		return model.Evaluation{}, evalErr
	}
	if fn != nil {
		return fn(t.Problem), nil
	}
	return initialEvaluation(t.Problem), nil
}

// solvedEvaluation is a fully correct answer: every criterion PASSes.
func solvedEvaluation(f model.FrozenProblem) model.Evaluation {
	ev := model.Evaluation{Complete: true}
	score := 0
	for _, c := range f.Definition.Criteria {
		ev.Results = append(ev.Results, model.CheckResult{CriterionID: c.ID, Status: model.StatusPass, Points: c.Points, Reason: "observed PASS"})
		ev.MaxScore += c.Points
		score += c.Points
	}
	ev.Score = &score
	return ev
}

// erroredEvaluation is a solved answer whose last criterion could not be
// judged: the platform failed, so the evaluation is incomplete.
func erroredEvaluation(f model.FrozenProblem) model.Evaluation {
	ev := solvedEvaluation(f)
	last := len(ev.Results) - 1
	ev.Results[last].Status, ev.Results[last].Points = model.StatusError, 0
	ev.Results[last].Reason = "the checking environment failed"
	ev.Complete, ev.Score = false, nil
	return ev
}

// fakeInputs fingerprints an answer the way the real capturer does in
// spirit: one object per problem whose version the test can bump, and the
// learner's answer.yaml in the attempt directory.
type fakeInputs struct {
	mu      sync.Mutex
	version map[string]int // problem ID -> object generation
	err     error
	calls   int
	onCall  func(n int)
}

func (f *fakeInputs) CaptureInputs(_ context.Context, t grader.Target) (map[string]string, error) {
	f.mu.Lock()
	f.calls++
	n, hook, err := f.calls, f.onCall, f.err
	version := f.version[t.Problem.Definition.ID]
	f.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{
		"object:apps/v1/Deployment/" + t.Problem.Namespace + "/web": fmt.Sprintf("uid=uid-1;gen=%d;input=sha256:aaa", version+1),
		"file:answer.yaml": "absent",
	}
	if b, err := os.ReadFile(filepath.Join(t.Workspace, "answer.yaml")); err == nil {
		out["file:answer.yaml"] = "mac:" + string(b)
	}
	return out, nil
}

// change simulates the learner editing an object of the problem.
func (f *fakeInputs) change(problemID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.version == nil {
		f.version = map[string]int{}
	}
	f.version[problemID]++
}

func (f *fakeInputs) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// harness is a Service on a real SQLite store with a fake cluster.
type harness struct {
	t         *testing.T
	st        *store.Store
	rec       session.Records
	env       *fakeEnv
	eng       *fakeEngine
	svc       *session.Service
	clock     *fakeClock
	bank      string
	stateDir  string
	workspace string

	inputs *fakeInputs
	root   string

	graderVersion string
	prepared      int
}

type harnessOption struct {
	mutateBank   func(t *testing.T, bank string)
	uncertified  []string // problem IDs left without a certificate
	records      func(*store.Store) session.Records
	graderForSvc string
	// root reuses an existing test directory (database, bank, state, workspace)
	// instead of making a new one: a second process attaches to the first
	// one's installation.
	root string
	// manifest and graderVersion override what the service believes is live.
	manifest      func() map[string]string
	graderVersion string
}

func newHarness(t *testing.T, opts ...func(*harnessOption)) *harness {
	t.Helper()
	var o harnessOption
	for _, f := range opts {
		f(&o)
	}
	root, attach := o.root, o.root != ""
	if !attach {
		root = t.TempDir()
	}
	h := &harness{
		t: t, clock: &fakeClock{t: t0}, bank: filepath.Join(root, "bank"),
		stateDir: filepath.Join(root, "state"), workspace: filepath.Join(root, "workspace"),
		graderVersion: testGraderVersion, root: root, inputs: &fakeInputs{},
	}
	testutil.NoError(t, os.MkdirAll(h.stateDir, 0o700))
	if !attach {
		copyTree(t, devBank, h.bank)
		if o.mutateBank != nil {
			o.mutateBank(t, h.bank)
		}
	}

	var err error
	h.st, err = store.Open(filepath.Join(root, "junkloud.db"))
	testutil.NoError(t, err)
	t.Cleanup(func() { h.st.Close() })
	h.rec = h.st
	if o.records != nil {
		h.rec = o.records(h.st)
	}

	if !attach {
		h.certify(o.uncertified...)
	}

	h.env = &fakeEnv{st: h.st, identity: testIdentity}
	h.eng = &fakeEngine{env: h.env}
	n := 0
	h.svc, err = session.NewService(session.Config{
		Store: h.rec, Env: h.env, Engine: h.eng, Identity: testIdentity,
		GraderVersion: cmpOr(o.graderVersion, testGraderVersion), Inputs: h.inputs,
		EnvironmentManifest: func(context.Context, model.Identity) (map[string]string, error) {
			if o.manifest != nil {
				return o.manifest(), nil
			}
			return maps.Clone(testManifest), nil
		},
		PrepareEnvironment: func(context.Context, model.Identity) error {
			h.env.log(event{Kind: "prepare-env"})
			return nil
		},
		BankDir: h.bank, StateDir: h.stateDir, Workspace: h.workspace,
		Now: h.clock.Now,
		NewSessionID: func(mode string) (string, error) {
			n++
			return fmt.Sprintf("%s-%d", mode, n), nil
		},
		NewSeed: func() (uint64, error) { return 42, nil },
	})
	testutil.NoError(t, err)
	return h
}

func cmpOr(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

// certify stores a passing validation for every bank problem except skip.
func (h *harness) certify(skip ...string) {
	h.t.Helper()
	for _, p := range h.bankProblems() {
		if slices.Contains(skip, p.ID) {
			continue
		}
		h.certifyProblem(p, testGraderVersion, testEnvHash, true)
	}
}

func (h *harness) certifyProblem(p model.Problem, graderVersion, envHash string, passed bool) {
	h.t.Helper()
	seeds, err := author.CoveringSeeds(p)
	testutil.NoError(h.t, err)
	h.certifySeeds(p, graderVersion, envHash, passed, seeds)
}

// certifySeeds stores a validation of p made with exactly these seeds.
func (h *harness) certifySeeds(p model.Problem, graderVersion, envHash string, passed bool, seeds []uint64) {
	h.t.Helper()
	hash, err := problem.Hash(p)
	testutil.NoError(h.t, err)
	testutil.NoError(h.t, h.st.SaveValidation(ctx, model.ValidationRecord{
		ProblemHash: hash, GraderVersion: graderVersion, EnvironmentHash: envHash,
		Seeds: seeds, Passed: passed, Cases: map[string]model.Evaluation{},
	}))
}

func (h *harness) bankProblems() []model.Problem {
	h.t.Helper()
	dirs, err := filepath.Glob(filepath.Join(h.bank, "problems", "*"))
	testutil.NoError(h.t, err)
	var out []model.Problem
	for _, d := range dirs {
		p, err := problem.Load(d)
		testutil.NoError(h.t, err)
		out = append(out, p)
	}
	return out
}

func (h *harness) loadBankProblem(id string) model.Problem {
	h.t.Helper()
	p, err := problem.Load(filepath.Join(h.bank, "problems", id))
	testutil.NoError(h.t, err)
	return p
}

func (h *harness) mock() session.StartOptions {
	return session.StartOptions{Mode: model.ModeMock, MockID: "dev-mock"}
}

func (h *harness) prepareMock() session.SessionView {
	h.t.Helper()
	rec, err := h.svc.Prepare(ctx, h.mock())
	testutil.NoError(h.t, err)
	return rec
}

// countFiles counts regular files below dir (zero if it does not exist).
func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	testutil.NoError(t, err)
	return n
}

// treeContains reports whether any file below dir contains needle.
func treeContains(t *testing.T, dir, needle string) bool {
	t.Helper()
	found := false
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), needle) {
			found = true
		}
		return nil
	})
	return found
}

func problemIDs(rec model.SessionRecord) []string {
	var ids []string
	for _, p := range rec.Problems {
		ids = append(ids, p.Definition.ID)
	}
	return ids
}

func viewIDs(v session.SessionView) []string {
	var ids []string
	for _, p := range v.Problems {
		ids = append(ids, p.ID)
	}
	return ids
}

// addVariant copies a bank problem under a new ID and namespace, so a test
// can have more than the five fixtures to draw from.
func addVariant(t *testing.T, bank, from, to, nsFrom, nsTo string) {
	t.Helper()
	src, dst := filepath.Join(bank, "problems", from), filepath.Join(bank, "problems", to)
	copyTree(t, src, dst)
	err := filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		edited := strings.NewReplacer(from, to, nsFrom, nsTo).Replace(string(b))
		return os.WriteFile(path, []byte(edited), 0o644)
	})
	testutil.NoError(t, err)
}

// editFile applies a text replacement to a bank file and requires it to match.
func editFile(t *testing.T, path, old, new string) {
	t.Helper()
	b, err := os.ReadFile(path)
	testutil.NoError(t, err)
	edited := strings.Replace(string(b), old, new, 1)
	testutil.Equal(t, edited != string(b), true)
	testutil.NoError(t, os.WriteFile(path, []byte(edited), 0o644))
}
