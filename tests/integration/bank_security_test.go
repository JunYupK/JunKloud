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
	validateGroupGuarded(t, loadGroup(t, ids), false, securityForbidden)
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
	// kubectl returns the combined output and the command's error: callers that
	// expect success use mustKubectl so a failure is never hidden by output
	// that merely mentions the CRD name (a NotFound message contains it too).
	kubectl := func(args ...string) (string, error) {
		out, err := exec.Command("kubectl", append([]string{"--kubeconfig", b.m.KubeconfigPath(), "--context", b.id.Context}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	mustKubectl := func(args ...string) string {
		t.Helper()
		out, err := kubectl(args...)
		if err != nil {
			t.Fatalf("kubectl %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
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
	// The CRD must exist: the command has to succeed, not merely print a
	// message that contains the name.
	before := mustKubectl("get", "crd", "backups.ops.junkloud.dev")
	t.Logf("RESET before cleanup: kubectl get crd backups.ops.junkloud.dev:\n%s", before)
	t.Logf("RESET before cleanup: kubectl get backups -n %s:\n%s", f.Namespace, mustKubectl("get", "backups", "-n", f.Namespace))
	if !strings.Contains(before, "backups.ops.junkloud.dev") || strings.Contains(before, "NotFound") {
		t.Fatalf("setup did not install the CRD: %s", before)
	}
	testutil.NoError(t, janitor.Cleanup(ctx, owner))
	after, afterErr := kubectl("get", "crd", "backups.ops.junkloud.dev")
	t.Logf("RESET after cleanup: kubectl get crd backups.ops.junkloud.dev:\n%s", after)
	t.Logf("RESET after cleanup: kubectl get crd:\n%s", mustKubectl("get", "crd"))
	// With the CRD gone the Backup kind is not served, so this listing is
	// expected to fail; only its message is of interest.
	gone, _ := kubectl("get", "backups", "-A")
	t.Logf("RESET after cleanup: kubectl get backups -A:\n%s", gone)
	if afterErr == nil || !strings.Contains(after, "NotFound") {
		t.Fatalf("the CRD survived the cleanup (err=%v): %s", afterErr, after)
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
	again := mustKubectl("get", "crd", "backups.ops.junkloud.dev")
	t.Logf("RESET after re-setup: kubectl get crd backups.ops.junkloud.dev:\n%s", again)
	if !strings.Contains(again, "backups.ops.junkloud.dev") || strings.Contains(again, "NotFound") {
		t.Fatalf("the CRD is not installed again: %s", again)
	}
}
