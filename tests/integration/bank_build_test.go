//go:build integration

package integration

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/cli"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// Only adds observability; validation and grading are the actual runtime implementations.
type bankProgress struct {
	author.Evaluator
	t      *testing.T
	stages []string
	next   map[uint64]int
}

func (p *bankProgress) Evaluate(ctx context.Context, target grader.Target) (model.Evaluation, error) {
	seed := target.Problem.Seed
	stage := p.stages[p.next[seed]]
	p.next[seed]++
	p.t.Logf("BANK %s seed=%d stage=%s START %s", target.Problem.Definition.ID, seed, stage, time.Now().UTC().Format(time.RFC3339))
	ev, err := p.Evaluator.Evaluate(ctx, target)
	score := -1
	if ev.Score != nil {
		score = *ev.Score
	}
	p.t.Logf("BANK %s seed=%d stage=%s END score=%d/%d complete=%t error=%v", target.Problem.Definition.ID, seed, stage, score, ev.MaxScore, ev.Complete, err)
	for _, r := range ev.Results {
		p.t.Logf("  %s %s: %s", r.CriterionID, r.Status, r.Reason)
	}
	return ev, err
}

type bankRuntime struct {
	m   *environment.Manager
	id  model.Identity
	dir string
	st  *store.Store
	rt  *author.Runtime
	cfg author.GraderConfig
}

func bankActionExecutor(t *testing.T, b bankRuntime) author.ActionExecutor {
	t.Helper()
	reader, err := grader.NewClusterReader(b.m.KubeconfigPath(), b.id.Context)
	testutil.NoError(t, err)
	tools := b.cfg.Tools
	if tools == nil {
		tools = grader.NewToolRunner()
	}
	return author.NewActionExecutorWithTools(b.m, b.m, author.ToolActionConfig{Tools: tools, Reader: reader, KubeconfigPath: b.m.KubeconfigPath(), Context: b.id.Context}, b.cfg.ProvidedImages()...)
}

func openBankRuntime(t *testing.T) bankRuntime {
	t.Helper()
	requireTools(t, "docker", "kind", "kubectl", "helm")
	ctx := context.Background()
	// Canonical runtime DB lets cli.Run reuse the actual bank certificates.
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "sessions.db"))
	testutil.NoError(t, err)
	m := environment.New(dir, st, hostCompatOptions(t)...)
	t.Cleanup(func() {
		defer st.Close()
		if id, err := m.RecordedIdentity(); err == nil {
			testutil.NoError(t, m.StopPlatform(ctx, id))
		}
		ops, err := st.ListOperations(ctx, environment.OperationCreate)
		testutil.NoError(t, err)
		for _, op := range ops {
			kindDelete(t, environment.ClusterName(op.EnvironmentID), filepath.Join(dir, "cleanup.kubeconfig"))
		}
	})
	id, err := m.Ensure(ctx)
	testutil.NoError(t, err)
	cfg := author.GraderConfig{StateDir: dir}
	rt, err := author.NewRuntime(ctx, m, st, id, cfg)
	testutil.NoError(t, err)
	t.Cleanup(func() { testutil.NoError(t, rt.Close()) })
	return bankRuntime{m, id, dir, st, rt, cfg}
}

// Run once before all seed/case cycles: prove the actual provided images run
// offline and the initial storage fixture has observable, cleanable backing.
// This does not issue certificates or replace full Validator certification.
func smokeBank(t *testing.T, b bankRuntime, problems []model.Problem) {
	t.Helper()
	ctx := context.Background()
	for _, p := range problems {
		if p.ID == "image-001" {
			continue
		} // certified by its actual Docker build/run cases
		t.Run("smoke-"+p.ID, func(t *testing.T) {
			seeds, err := author.CoveringSeeds(p)
			testutil.NoError(t, err)
			f, err := problem.Freeze(p, seeds[0])
			testutil.NoError(t, err)
			owner := environment.Owner{Inventory: model.Inventory{EnvironmentID: b.id.ID, SessionID: author.SessionID, ProblemID: p.ID}, Namespace: f.Namespace, Scope: f.Definition.Scope, CreateNamespaces: true}
			defer func() {
				testutil.NoError(t, (environment.ManagedJanitor{Manager: b.m, Identity: b.id}).Cleanup(ctx, owner))
			}()
			ws, remove, err := author.ScratchWorkspace(b.dir, f)
			testutil.NoError(t, err)
			defer remove()
			executor := bankActionExecutor(t, b)
			owner.Inventory, err = executor.Execute(ctx, b.id, owner, p, f, ws, p.Setup)
			testutil.NoError(t, err)
			if p.ID != "volume-001" {
				owner.CreateNamespaces = false
				owner.Inventory, err = executor.Execute(ctx, b.id, owner, p, f, ws, p.Solution)
				testutil.NoError(t, err)
			}
			var chosen []model.Criterion
			for _, c := range f.Definition.Criteria {
				if c.Kind == model.CriterionReady || c.Kind == model.CriterionJob || c.Kind == model.CriterionLogs {
					c.DependsOn = nil
					c.Timeout = 30 * time.Second
					chosen = append(chosen, c)
				}
			}
			if p.ID == "volume-001" {
				chosen = f.Definition.Criteria
			}
			f.Definition.Criteria = chosen
			t.Logf("SMOKE %s seed=%d actual offline readiness START", p.ID, seeds[0])
			ev, err := b.rt.Grader.Engine.Evaluate(ctx, grader.Target{Identity: b.id, SessionID: author.SessionID, Problem: f, Workspace: ws})
			testutil.NoError(t, err)
			for _, r := range ev.Results {
				t.Logf("SMOKE %s %s %s: %s", p.ID, r.CriterionID, r.Status, r.Reason)
				want := model.StatusPass
				if p.ID == "volume-001" {
					want = p.InitialExpected[r.CriterionID]
				}
				testutil.Equal(t, r.Status, want)
			}
			if p.ID == "volume-001" {
				testutil.Equal(t, ev.Complete, true)
				testutil.Equal(t, *ev.Score, p.InitialScore)
				client := kubeClient(t, b.m, b.id)
				for _, name := range []string{"ledger-data", "spare-data"} {
					claim, err := client.CoreV1().PersistentVolumeClaims(f.Namespace).Get(ctx, name, metav1.GetOptions{})
					testutil.NoError(t, err)
					testutil.Equal(t, string(claim.Status.Phase), "Bound")
					if claim.Spec.VolumeName == "" {
						t.Fatal("bound claim has no backing PV")
					}
					t.Logf("SMOKE %s claim=%s phase=%s volume=%s", p.ID, name, claim.Status.Phase, claim.Spec.VolumeName)
				}
				target := grader.Target{Identity: b.id, SessionID: author.SessionID, Problem: f, Workspace: ws}
				before, err := b.rt.Grader.Artifacts.CaptureInputs(ctx, target)
				testutil.NoError(t, err)
				again, err := b.rt.Grader.Engine.Evaluate(ctx, target)
				testutil.NoError(t, err)
				testutil.Equal(t, evaluationStatuses(again), p.InitialExpected)
				after, err := b.rt.Grader.Artifacts.CaptureInputs(ctx, target)
				testutil.NoError(t, err)
				testutil.Equal(t, after, before)
				t.Logf("SMOKE volume-001 full initial3/4 and stable binding/PV input capture PASS")
			}
		})
		if t.Failed() {
			t.Fatal("runtime smoke failed; refusing full bank cycles")
		}
	}
}

func TestBuildBankSmoke(t *testing.T) {
	b := openBankRuntime(t)
	testutil.NoError(t, author.PrepareEnvironment(context.Background(), b.m, b.id, b.cfg.Images))
	smokeBank(t, b, loadGroup(t, buildIDs))
}

func validateGroup(t *testing.T, problems []model.Problem) {
	t.Helper()
	b := openBankRuntime(t)
	m, id, dir, st, rt, cfg := b.m, b.id, b.dir, b.st, b.rt, b.cfg
	ctx := context.Background()
	testutil.NoError(t, author.PrepareEnvironment(ctx, m, id, cfg.Images))
	smokeBank(t, b, problems)
	for _, p := range problems {
		t.Run(p.ID, func(t *testing.T) {
			seeds, err := author.CoveringSeeds(p)
			testutil.NoError(t, err)
			hash, err := problem.Hash(p)
			testutil.NoError(t, err)
			t.Logf("BANK %s hash=%s seeds=%v", p.ID, hash, seeds)
			stages := []string{author.KeyInitial, author.KeySolved}
			for _, c := range p.Cases {
				stages = append(stages, author.CaseKey(c.ID))
			}
			stages = append(stages, author.KeyReset)
			progress := &bankProgress{Evaluator: rt.Grader.Engine, t: t, stages: stages, next: map[uint64]int{}}
			names := []string{}
			for _, img := range cfg.ProvidedImages() {
				names = append(names, img.Name, img.Ref())
			}
			v, err := author.NewValidator(author.Config{Identity: id, Env: m, Janitor: environment.ManagedJanitor{Manager: m, Identity: id}, Engine: progress, Store: st,
				Actions: bankActionExecutor(t, b), LockPath: author.LockPath(dir), GraderVersion: grader.GraderVersion, EnvironmentHash: rt.EnvironmentHash,
				ProvidedImages: names, Prepare: func(ctx context.Context) error { return author.PrepareEnvironment(ctx, m, id, cfg.Images) }, ScratchDir: filepath.Join(dir, "validate")})
			testutil.NoError(t, err)
			record, err := v.Validate(ctx, p, seeds)
			testutil.NoError(t, err)
			testutil.Equal(t, record.Passed, true)
			testutil.Equal(t, len(record.Cases), len(seeds)*len(stages))
			for i, seed := range seeds {
				for _, stage := range stages {
					key := stage
					if i > 0 {
						key = author.SeedKey(seed, key)
					}
					ev, ok := record.Cases[key]
					if !ok {
						t.Fatalf("missing stage %s", key)
					}
					if !ev.Complete || ev.Score == nil {
						t.Fatalf("incomplete stage %s: %+v", key, ev)
					}
					for _, r := range ev.Results {
						if r.Status == model.StatusError {
							t.Fatalf("ERROR %s/%s: %s", key, r.CriterionID, r.Reason)
						}
					}
					if stage == author.KeyInitial || stage == author.KeyReset {
						testutil.Equal(t, *ev.Score, p.InitialScore)
						testutil.Equal(t, evaluationStatuses(ev), p.InitialExpected)
					}
					for _, c := range p.Cases {
						if stage == author.CaseKey(c.ID) {
							testutil.Equal(t, *ev.Score, c.ExpectedScore)
							testutil.Equal(t, evaluationStatuses(ev), c.Expected)
						}
					}
				}
			}
		})
	}
	if t.Failed() {
		return
	}
	t.Setenv("CKAD_HOST_COMPAT", os.Getenv("JUNKLOUD_TEST_HOST_COMPAT"))
	workspace := t.TempDir()
	absoluteBank, err := filepath.Abs(bankRoot)
	testutil.NoError(t, err)
	var out, diag bytes.Buffer
	code := cli.Run(ctx, []string{"--state-dir", dir, "--bank-dir", absoluteBank, "--workspace", workspace, "start", "--mode", "practice", "--count", "5", "--seed", "1"}, strings.NewReader("yes\n"), &out, &diag)
	t.Logf("BANK CLI start practice5 exit=%d stdout=%s stderr=%s", code, out.String(), diag.String())
	testutil.Equal(t, code, 0)
	svc := session.New(st, m, rt, absoluteBank, dir, session.WithWorkspace(workspace))
	sessionID, err := st.CurrentSessionID(ctx)
	testutil.NoError(t, err)
	status, err := svc.Status(ctx, sessionID, time.Now())
	testutil.NoError(t, err)
	testutil.Equal(t, len(status.Session.Problems), 5)
	testutil.Equal(t, status.Session.State, model.StateRunning)
}

func TestBuildBank(t *testing.T) { validateGroup(t, loadGroup(t, buildIDs)) }
