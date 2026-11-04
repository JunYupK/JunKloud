//go:build linux && e2e

package e2e_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/store"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// waitOperationPhase polls the state store read-only (opened per poll and
// closed again) every 100ms, for at most five minutes, until an operation of
// the kind is in the phase, and returns it.
func waitOperationPhase(ctx context.Context, t *testing.T, h *cliHarness, kind, phase string) model.Operation {
	t.Helper()
	return waitOperationAnyPhase(ctx, t, h, kind, phase)
}

// waitOperationAnyPhase is waitOperationPhase for several acceptable phases.
func waitOperationAnyPhase(ctx context.Context, t *testing.T, h *cliHarness, kind string, phases ...string) model.Operation {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if op, ok := pollOperation(ctx, h, kind, phases); ok {
			return op
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no %s operation reached phase %v: %v", kind, phases, ctx.Err())
		case <-tick.C:
		}
	}
}

func pollOperation(ctx context.Context, h *cliHarness, kind string, phases []string) (model.Operation, bool) {
	// Read-only: the poller must never write to, migrate or check the
	// database the CLI under test owns.
	st, err := store.OpenReadOnly(filepath.Join(h.state, "sessions.db"))
	if err != nil {
		return model.Operation{}, false
	}
	defer st.Close()
	// Only the current session's operations count, never an earlier one's.
	sid, err := st.CurrentSessionID(ctx)
	if err != nil {
		return model.Operation{}, false
	}
	ops, err := st.ListOperations(ctx, kind)
	if err != nil {
		return model.Operation{}, false
	}
	for i := len(ops) - 1; i >= 0; i-- {
		if ops[i].SessionID == sid && slices.Contains(phases, ops[i].Phase) {
			return ops[i], true
		}
	}
	return model.Operation{}, false
}

func operationsOf(ctx context.Context, t *testing.T, h *cliHarness, kind string) []model.Operation {
	t.Helper()
	st, err := store.Open(filepath.Join(h.state, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ops, err := st.ListOperations(ctx, kind)
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

// proc is a running ckad whose exit is observed by a goroutine, so waiting
// for a store phase stops as soon as the process is gone.
type proc struct {
	cmd  *exec.Cmd
	out  *bytes.Buffer
	diag *bytes.Buffer
	done chan struct{}
	err  error
}

// startProc starts ckad with the given stdin.
func startProc(t *testing.T, h *cliHarness, stdin string, args ...string) *proc {
	t.Helper()
	cmd := exec.Command(h.bin, args...)
	cmd.Dir = repoRoot(t)
	cmd.Env = h.env
	cmd.Stdin = strings.NewReader(stdin)
	p := &proc{cmd: cmd, out: &bytes.Buffer{}, diag: &bytes.Buffer{}, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = p.out, p.diag
	if err := cmd.Start(); err != nil {
		t.Fatalf("start ckad %s: %v", strings.Join(args, " "), err)
	}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			cmd.Process.Kill()
			<-p.done
		}
	})
	t.Logf("started ckad %s (pid %d)", strings.Join(args, " "), cmd.Process.Pid)
	return p
}

// watch is a context that ends when the process exits.
func (p *proc) watch(parent context.Context) context.Context {
	ctx, cancel := context.WithCancel(parent)
	go func() { <-p.done; cancel() }()
	return ctx
}

func (p *proc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// wait blocks until the process exits (at most limit) and returns its exit
// code, or -1 when it was killed by a signal.
func (p *proc) wait(t *testing.T, limit time.Duration) int {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(limit):
		p.cmd.Process.Kill()
		<-p.done
		t.Fatalf("ckad did not exit within %s; stdout=%s stderr=%s", limit, p.out, p.diag)
	}
	var ee *exec.ExitError
	if errors.As(p.err, &ee) {
		return ee.ExitCode()
	}
	if p.err != nil {
		t.Fatal(p.err)
	}
	return 0
}

func (p *proc) signal(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signal %v: %v", sig, err)
	}
}

func (p *proc) kill(t *testing.T) {
	t.Helper()
	p.signal(t, syscall.SIGKILL)
	select {
	case <-p.done:
	case <-time.After(30 * time.Second):
		t.Fatal("killed ckad did not exit")
	}
}

// runCode runs ckad to completion and returns its exit code and output.
func runCode(t *testing.T, h *cliHarness, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	p := startProc(t, h, stdin, args...)
	code = p.wait(t, 30*time.Minute)
	t.Logf("ckad %s; input=%q; exit=%d; stdout=%s; stderr=%s", strings.Join(args, " "), stdin, code, p.out, p.diag)
	return code, p.out.String(), p.diag.String()
}

// expectCode runs ckad and fails unless it exits with want. It returns the
// stdout and stderr joined, which is what the learner sees.
func expectCode(t *testing.T, h *cliHarness, stdin string, want int, args ...string) string {
	t.Helper()
	code, out, diag := runCode(t, h, stdin, args...)
	if code != want {
		t.Fatalf("ckad %s: expected exit %d, got %d", strings.Join(args, " "), want, code)
	}
	return out + diag
}

func practiceArgs() []string {
	return []string{"start", "--mode", "practice", "--count", "5", "--seed", "42"}
}

// currentSessionOf is the current session ID and its frozen problems.
func currentSessionOf(ctx context.Context, t *testing.T, h *cliHarness) (string, []model.FrozenProblem) {
	t.Helper()
	st, err := store.Open(filepath.Join(h.state, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sid, err := st.CurrentSessionID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := st.LoadSession(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	return sid, rec.Problems
}

// submissionsAndRuns counts the session's submissions and grading runs.
func submissionsAndRuns(ctx context.Context, t *testing.T, h *cliHarness, sid string) (subs, runs int) {
	t.Helper()
	st, err := store.Open(filepath.Join(h.state, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	list, err := st.ListSubmissions(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		rs, err := st.ListRuns(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		runs += len(rs)
	}
	return len(list), runs
}

// resetAll abandons the current session through the real CLI and requires
// that no owned object, volume, image or open operation is left.
func resetAll(ctx context.Context, t *testing.T, h *cliHarness) {
	t.Helper()
	expectCode(t, h, h.identity(ctx).ID+"\n", 0, "reset")
	if left := observeLeftovers(ctx, t, h); left.Count() != 0 {
		t.Fatalf("leftovers after reset: %+v", left)
	}
}

var scoreRe = regexp.MustCompile(`점수: (\d+)/(\d+)`)

// cleanSlate is a best-effort return to an empty environment after a failed
// subtest, so one failure does not cascade into the next subtest.
func cleanSlate(ctx context.Context, t *testing.T, h *cliHarness) {
	t.Helper()
	runCode(t, h, "", "reset", "--recover")
	runCode(t, h, h.identity(ctx).ID+"\n", "reset")
}

// TestInterruptedOperations kills or interrupts the real CLI in the middle of
// prepare, grade and reset, cuts the API server off during a grading, and
// races two gradings, then requires the next commands to detect the
// unfinished work and recover it (spec 11) without inventing a score.
func TestInterruptedOperations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()
	bank := filepath.Join(repoRoot(t), "tests", "fixtures", "dev-bank")
	h := newHarness(t, bank)
	// Subtests log through their own t; the harness cleanup (registered
	// first, so it runs last) must log through the parent again.
	t.Cleanup(func() { h.t = t })
	for _, id := range []string{"dev-config-001", "dev-crashloop-001", "dev-sa-001", "dev-scale-001", "dev-service-001"} {
		h.run("", 0, "bank", "validate", id)
	}
	if left := observeLeftovers(ctx, t, h); left.Count() != 0 {
		t.Fatalf("leftovers before the first subtest: %+v", left)
	}

	t.Run("KillDuringPrepare", func(t *testing.T) {
		h.t = t
		defer func() {
			if t.Failed() {
				cleanSlate(ctx, t, h)
			}
		}()
		p := startProc(t, h, "", practiceArgs()...)
		op := waitOperationPhase(p.watch(ctx), t, h, session.OperationKind, session.PhaseSetupApplied)
		p.kill(t)
		after := operationsOf(ctx, t, h, session.OperationKind)
		if last := after[len(after)-1]; last.ID != op.ID || last.Phase != session.PhaseSetupApplied {
			t.Fatalf("the prepare finished before the kill landed: %+v", last)
		}
		sid, frozen := currentSessionOf(ctx, t, h)

		// A namespace of the half-prepared session goes missing. Nothing may
		// recreate it together with answers.
		missing := frozen[0].Namespace
		k := h.kube(ctx)
		if err := k.cs.CoreV1().Namespaces().Delete(ctx, missing, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(2 * time.Minute)
		for namespaceExists(ctx, k, missing) {
			if time.Now().After(deadline) {
				t.Fatalf("namespace %s did not terminate", missing)
			}
			time.Sleep(time.Second)
		}

		status := expectCode(t, h, "", 3, "status")
		if !strings.Contains(status, op.ID) || !strings.Contains(status, "ckad reset") {
			t.Fatalf("status does not name the interrupted prepare %s and ckad reset:\n%s", op.ID, status)
		}
		refused := expectCode(t, h, "yes\n", 3, practiceArgs()...)
		if strings.Contains(refused, "준비 완료") || !strings.Contains(refused, "이전 준비가 중단되었습니다") || !strings.Contains(refused, "ckad reset") {
			t.Fatalf("a new start was not refused with the interrupted-prepare guidance:\n%s", refused)
		}
		if namespaceExists(ctx, k, missing) {
			t.Fatalf("namespace %s was recreated", missing)
		}

		expectCode(t, h, h.identity(ctx).ID+"\n", 0, "reset")
		if left := observeLeftovers(ctx, t, h); left.Count() != 0 {
			t.Fatalf("leftovers after reset of session %s: %+v", sid, left)
		}
		expectCode(t, h, "yes\n", 0, practiceArgs()...)
		resetAll(ctx, t, h)
	})

	t.Run("InterruptDuringGrade", func(t *testing.T) {
		h.t = t
		defer func() {
			if t.Failed() {
				cleanSlate(ctx, t, h)
			}
		}()
		expectCode(t, h, "yes\n", 0, practiceArgs()...)
		sid, _ := currentSessionOf(ctx, t, h)
		p := startProc(t, h, "", "grade")
		op := waitOperationAnyPhase(p.watch(ctx), t, h, session.GradeOperationKind, session.GradePlanned, session.GradeRecorded)
		p.signal(t, syscall.SIGINT)
		code := p.wait(t, 10*time.Minute)
		t.Logf("interrupted grade %s exited %d; stdout=%s stderr=%s", op.ID, code, p.out, p.diag)
		if code == 0 || code == 1 {
			t.Fatalf("the SIGINT did not interrupt the grading of %s (exit %d)", op.ID, code)
		}

		interrupted := expectCode(t, h, "", 3, "grade")
		if !strings.Contains(interrupted, "채점을 확정할 수 없습니다") || !strings.Contains(interrupted, "ckad grade --retry") {
			t.Fatalf("grade does not report the unconfirmed grading and its recovery:\n%s", interrupted)
		}
		code, out, diag := runCode(t, h, "", "grade", "--retry")
		if code != 0 && code != 1 {
			t.Fatalf("grade --retry exit %d", code)
		}
		if !scoreRe.MatchString(out + diag) {
			t.Fatalf("grade --retry confirmed no score:\n%s%s", out, diag)
		}
		if subs, _ := submissionsAndRuns(ctx, t, h, sid); subs != 1 {
			t.Fatalf("expected one submission, got %d", subs)
		}
		resetAll(ctx, t, h)
	})

	t.Run("KillDuringReset", func(t *testing.T) {
		h.t = t
		defer func() {
			if t.Failed() {
				cleanSlate(ctx, t, h)
			}
		}()
		expectCode(t, h, "yes\n", 0, practiceArgs()...)
		sid, frozen := currentSessionOf(ctx, t, h)
		for _, f := range frozen {
			dir, _, err := session.LatestAttempt(h.workspace, sid, f.Definition.ID)
			if err != nil {
				t.Fatal(err)
			}
			writePersonalFile(t, filepath.Join(dir, "answer-notes.txt"), "my notes for "+f.Definition.ID+"\n")
		}
		writePersonalFile(t, filepath.Join(h.workspace, ".bashrc"), "alias k=kubectl\n")
		writePersonalFile(t, filepath.Join(h.workspace, "notes.txt"), "personal\n")
		before := personalFiles(t, h.workspace)
		if len(before) != len(frozen)+2 {
			t.Fatalf("expected %d personal files, found %d", len(frozen)+2, len(before))
		}

		envID := h.identity(ctx).ID
		p := startProc(t, h, envID+"\n", "reset")
		op := waitOperationAnyPhase(p.watch(ctx), t, h, session.ResetOperationKind, session.PhasePlanned, "deleted")
		p.kill(t)
		ops := operationsOf(ctx, t, h, session.ResetOperationKind)
		if last := ops[len(ops)-1]; last.ID != op.ID || last.Phase == session.PhaseDone {
			t.Fatalf("the reset finished before the kill landed: %+v", last)
		}

		refused := expectCode(t, h, "yes\n", 3, practiceArgs()...)
		t.Logf("the reset was killed in phase %q (operation %s)", op.Phase, op.ID)
		if !strings.Contains(refused, "초기화가 중단되었습니다") {
			t.Fatalf("start does not report the interrupted reset:\n%s", refused)
		}
		expectCode(t, h, "", 0, "reset", "--recover")
		if left := observeLeftovers(ctx, t, h); left.Count() != 0 {
			t.Fatalf("leftovers after reset --recover: %+v", left)
		}
		after := personalFiles(t, h.workspace)
		if !equalMaps(before, after) {
			t.Fatalf("personal files changed:\nbefore=%v\nafter=%v", before, after)
		}
	})

	t.Run("APIOutageDuringGrade", func(t *testing.T) {
		h.t = t
		defer func() {
			if t.Failed() {
				cleanSlate(ctx, t, h)
			}
		}()
		expectCode(t, h, "yes\n", 0, practiceArgs()...)
		sid, _ := currentSessionOf(ctx, t, h)
		node := h.identity(ctx).ClusterName + "-control-plane"
		h.applyFrozenObjects(ctx) // a correct answer: any FAIL below would be the outage's fault
		// Always attempt the unpause, even when no pause is known to have
		// succeeded: a pause that reported an error may still have frozen the
		// node. "is not paused" is the normal answer then and is ignored.
		unpause := func() {
			if out, err := docker(context.Background(), "unpause", node); err != nil && !strings.Contains(out+err.Error(), "not paused") {
				t.Errorf("unpause %s: %v %s", node, err, out)
			}
		}
		t.Cleanup(unpause)
		defer unpause() // before the failure cleanup below talks to the cluster

		p := startProc(t, h, "", "grade")
		waitOperationAnyPhase(p.watch(ctx), t, h, session.GradeOperationKind, session.GradePlanned, session.GradeRecorded)
		// The kernel's cgroup freezer occasionally refuses a pause for a
		// moment ("unable to freeze"); retry before giving up.
		var pauseErr error
		for attempt := 0; attempt < 5; attempt++ {
			if _, pauseErr = docker(ctx, "pause", node); pauseErr == nil {
				break
			}
			t.Logf("pause %s attempt %d: %v", node, attempt+1, pauseErr)
			time.Sleep(2 * time.Second)
		}
		if pauseErr != nil {
			p.kill(t) // never leave a grading holding the lock behind
			t.Fatalf("pause %s: %v", node, pauseErr)
		}
		code := p.wait(t, 15*time.Minute)
		unpause()
		combined := p.out.String() + p.diag.String()
		t.Logf("grade during the outage: exit=%d\n%s", code, combined)
		if code != 3 {
			t.Fatalf("an API outage must give exit 3 (ERROR), got %d", code)
		}
		if scoreRe.MatchString(combined) {
			t.Fatalf("a score was printed during the outage:\n%s", combined)
		}
		if strings.Contains(combined, ": FAIL (") {
			t.Fatalf("the outage was graded as a FAIL:\n%s", combined)
		}

		// The cluster answers again. Wait until the node and the session's
		// pods are Ready too, not only the API: a retry against pods that
		// are still restarting would grade the outage's aftermath.
		waitAPI(ctx, t, h)
		waitNodeAndPodsReady(ctx, t, h)
		retry := expectCode(t, h, "", 0, "grade", "--retry")
		if !strings.Contains(retry, "점수: 29/29") {
			t.Fatalf("grade --retry did not confirm the full score:\n%s", retry)
		}
		if subs, _ := submissionsAndRuns(ctx, t, h, sid); subs != 1 {
			t.Fatalf("expected one submission, got %d", subs)
		}
		resetAll(ctx, t, h)
	})

	t.Run("ConcurrentCommands", func(t *testing.T) {
		h.t = t
		defer func() {
			if t.Failed() {
				cleanSlate(ctx, t, h)
			}
		}()
		expectCode(t, h, "yes\n", 0, practiceArgs()...)
		sid, _ := currentSessionOf(ctx, t, h)
		h.applyFrozenObjects(ctx)
		a := startProc(t, h, "", "grade")
		b := startProc(t, h, "", "grade")
		ca, cb := a.wait(t, 10*time.Minute), b.wait(t, 10*time.Minute)
		t.Logf("grade A exit=%d stdout=%s stderr=%s", ca, a.out, a.diag)
		t.Logf("grade B exit=%d stdout=%s stderr=%s", cb, b.out, b.diag)
		winner, loser := a, b
		if ca == 3 {
			winner, loser = b, a
		}
		if codes := []int{ca, cb}; !(slices.Contains(codes, 0) && slices.Contains(codes, 3)) {
			t.Fatalf("expected one success and one lock refusal, got exits %v", codes)
		}
		if !strings.Contains(winner.out.String(), "점수: 29/29") {
			t.Fatalf("the winning grade has no full score:\n%s", winner.out)
		}
		if !strings.Contains(loser.diag.String()+loser.out.String(), "다른 준비·채점·검증 작업이 실행 중입니다") {
			t.Fatalf("the refused grade does not report the lock:\n%s%s", loser.out, loser.diag)
		}
		subs, runs := submissionsAndRuns(ctx, t, h, sid)
		if subs != 1 || runs != 1 {
			t.Fatalf("expected one submission and one run, got %d and %d", subs, runs)
		}
		resetAll(ctx, t, h)
	})
}

func namespaceExists(ctx context.Context, k kubeClients, name string) bool {
	_, err := k.cs.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	return err == nil
}

// waitAPI waits until the unpaused API server answers again.
func waitAPI(ctx context.Context, t *testing.T, h *cliHarness) {
	t.Helper()
	k := h.kube(ctx)
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if _, err := k.cs.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{}); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the API server did not answer after the unpause")
		}
		time.Sleep(time.Second)
	}
}

// waitNodeAndPodsReady waits (at most five minutes) until every node is Ready
// and every pod outside the cluster's own system namespaces is Ready or has
// Succeeded.
func waitNodeAndPodsReady(ctx context.Context, t *testing.T, h *cliHarness) {
	t.Helper()
	k := h.kube(ctx)
	system := map[string]bool{"kube-system": true, "kube-public": true, "kube-node-lease": true, "local-path-storage": true}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		problem := ""
		nodes, err := k.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		switch {
		case err != nil:
			problem = "list nodes: " + err.Error()
		case len(nodes.Items) == 0:
			problem = "no nodes"
		default:
			for _, n := range nodes.Items {
				ready := false
				for _, c := range n.Status.Conditions {
					if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
						ready = true
					}
				}
				if !ready {
					problem = "node " + n.Name + " is not Ready"
				}
			}
		}
		if problem == "" {
			pods, err := k.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
			if err != nil {
				problem = "list pods: " + err.Error()
			} else {
				for _, p := range pods.Items {
					if system[p.Namespace] || p.Status.Phase == corev1.PodSucceeded {
						continue
					}
					ready := false
					for _, c := range p.Status.Conditions {
						if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
							ready = true
						}
					}
					if !ready {
						problem = "pod " + p.Namespace + "/" + p.Name + " is not Ready"
						break
					}
				}
			}
		}
		if problem == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cluster did not settle after the unpause: %s", problem)
		}
		time.Sleep(2 * time.Second)
	}
}

func writePersonalFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
