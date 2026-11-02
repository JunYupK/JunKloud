//go:build integration

package integration

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// Run separately on frozen source before spending time on every seed/case.
func TestSecurityBankSmoke(t *testing.T) {
	ids := presentSecurityIDs(t)
	if len(ids) == 0 {
		t.Skip("no environment-security package yet")
	}
	b := openBankRuntime(t)
	testutil.NoError(t, author.PrepareEnvironment(context.Background(), b.m, b.id, b.cfg.Images))
	smokeBank(t, b, loadGroup(t, ids))
}

// Present-only until the last package lands (Task 7 validates all five IDs).
func TestSecurityBank(t *testing.T) {
	ids := presentSecurityIDs(t)
	if len(ids) == 0 {
		t.Skip("no environment-security package yet")
	}
	validateGroupAfterSmoke(t, loadGroup(t, ids), false)
}

// custom-resource-001 owns a cluster-scoped CRD through its scope. Reset
// (the janitor cleanup every validation stage starts with) must remove it, and
// grading a Backup after that must be FAIL "not found", never ERROR.
func TestSecurityCustomResourceReset(t *testing.T) {
	b := openBankRuntime(t)
	ctx := context.Background()
	testutil.NoError(t, author.PrepareEnvironment(ctx, b.m, b.id, b.cfg.Images))
	p := loadGroup(t, []string{"custom-resource-001"})[0]
	seeds, err := author.CoveringSeeds(p)
	testutil.NoError(t, err)
	f, err := problem.Freeze(p, seeds[0])
	testutil.NoError(t, err)
	owner := environment.Owner{Inventory: model.Inventory{EnvironmentID: b.id.ID, SessionID: author.SessionID, ProblemID: p.ID}, Namespace: f.Namespace, Scope: f.Definition.Scope, CreateNamespaces: true}
	janitor := environment.ManagedJanitor{Manager: b.m, Identity: b.id}
	defer func() { testutil.NoError(t, janitor.Cleanup(ctx, owner)) }()
	kubectl := func(args ...string) string {
		out, _ := exec.Command("kubectl", append([]string{"--kubeconfig", b.m.KubeconfigPath(), "--context", b.id.Context}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	executor := bankActionExecutor(t, b)
	ws, remove, err := author.ScratchWorkspace(b.dir, f)
	testutil.NoError(t, err)
	defer remove()
	owner.Inventory, err = executor.Execute(ctx, b.id, owner, p, f, ws, p.Setup)
	testutil.NoError(t, err)
	owner.CreateNamespaces = false
	owner.Inventory, err = executor.Execute(ctx, b.id, owner, p, f, ws, p.Solution)
	testutil.NoError(t, err)
	before := kubectl("get", "crd", "backups.ops.junkloud.dev")
	t.Logf("RESET before cleanup: kubectl get crd backups.ops.junkloud.dev:\n%s", before)
	t.Logf("RESET before cleanup: kubectl get backups -n %s:\n%s", f.Namespace, kubectl("get", "backups", "-n", f.Namespace))
	if !strings.Contains(before, "backups.ops.junkloud.dev") {
		t.Fatal("setup did not install the CRD")
	}
	testutil.NoError(t, janitor.Cleanup(ctx, owner))
	after := kubectl("get", "crd", "backups.ops.junkloud.dev")
	t.Logf("RESET after cleanup: kubectl get crd backups.ops.junkloud.dev:\n%s", after)
	t.Logf("RESET after cleanup: kubectl get crd:\n%s", kubectl("get", "crd"))
	t.Logf("RESET after cleanup: kubectl get backups -A:\n%s", kubectl("get", "backups", "-A"))
	if !strings.Contains(after, "NotFound") {
		t.Fatalf("the CRD survived the cleanup: %s", after)
	}
	// Grade the now unserved Backup kind: a fresh workspace has no answer.
	fresh, removeFresh, err := author.ScratchWorkspace(b.dir, f)
	testutil.NoError(t, err)
	defer removeFresh()
	ev, err := b.rt.Grader.Engine.Evaluate(ctx, grader.Target{Identity: b.id, SessionID: author.SessionID, Problem: f, Workspace: fresh})
	testutil.NoError(t, err)
	for _, r := range ev.Results {
		t.Logf("RESET CRD absent: %s %s: %s", r.CriterionID, r.Status, r.Reason)
		testutil.Equal(t, r.Status, model.StatusFail)
	}
	testutil.Equal(t, ev.Complete, true)
	testutil.Equal(t, *ev.Score, 0)
	for _, r := range ev.Results {
		if r.CriterionID == "backup-exists" && !strings.Contains(r.Reason, "not found") {
			t.Errorf("backup-exists reason %q does not say not found", r.Reason)
		}
	}
	// The next attempt installs the CRD again.
	owner.CreateNamespaces = true
	owner.Inventory = model.Inventory{EnvironmentID: b.id.ID, SessionID: author.SessionID, ProblemID: p.ID}
	owner.Inventory, err = executor.Execute(ctx, b.id, owner, p, f, ws, p.Setup)
	testutil.NoError(t, err)
	again := kubectl("get", "crd", "backups.ops.junkloud.dev")
	t.Logf("RESET after re-setup: kubectl get crd backups.ops.junkloud.dev:\n%s", again)
	if !strings.Contains(again, "backups.ops.junkloud.dev") || strings.Contains(again, "NotFound") {
		t.Fatalf("the CRD is not installed again: %s", again)
	}
}
