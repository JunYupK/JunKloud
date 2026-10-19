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

func validateGroup(t *testing.T, problems []model.Problem) {
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
	defer rt.Close()
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
				Actions: author.NewActionExecutor(m, m, cfg.ProvidedImages()...), LockPath: author.LockPath(dir), GraderVersion: grader.GraderVersion, EnvironmentHash: rt.EnvironmentHash,
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
