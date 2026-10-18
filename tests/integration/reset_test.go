//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/JunYupK/JunKloud/internal/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

// TestReset exercises actual PVC backing deletion, retained-data refusal,
// namespace bundles and the real executable reset/retry/cluster workflow.
func TestReset(t *testing.T) {
	requireTools(t, "docker", "kind", "kubectl", "go")
	ctx := context.Background()
	dir := t.TempDir()
	workspace := filepath.Join(dir, "work")
	st, err := store.Open(filepath.Join(dir, "sessions.db"))
	testutil.NoError(t, err)
	m := environment.New(dir, st, hostCompatOptions(t)...)
	id, err := m.Ensure(ctx)
	testutil.NoError(t, err)
	t.Cleanup(func() {
		cur, e := m.RecordedIdentity()
		if e == nil {
			if e = m.StopPlatform(ctx, cur); e != nil {
				t.Error(e)
			}
		}
		kindDelete(t, id.ClusterName, filepath.Join(dir, "cleanup.kubeconfig"))
		st.Close()
	})
	binary := filepath.Join(dir, "ckad")
	build := exec.Command("go", "build", "-o", binary, "./cmd/ckad")
	build.Dir = "../.."
	outBytes, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v %s", err, outBytes)
	}
	bank, err := filepath.Abs(devBank)
	testutil.NoError(t, err)
	run := func(input string, want int, args ...string) string {
		t.Logf("Starting CLI %v", args)
		a := append([]string{"--state-dir", dir, "--bank-dir", bank, "--workspace", workspace}, args...)
		cmd := exec.Command(binary, a...)
		cmd.Stdin = strings.NewReader(input)
		cmd.Env = append(os.Environ(), "CKAD_HOST_COMPAT="+os.Getenv("JUNKLOUD_TEST_HOST_COMPAT"))
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		code := 0
		if err != nil {
			var ex *exec.ExitError
			if errors.As(err, &ex) {
				code = ex.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if code != want {
			t.Fatalf("CLI %v exit %d want %d: %s", args, code, want, out.String())
		}
		t.Logf("CLI %v exit %d", args, code)
		return out.String()
	}
	run("wrong\n", 2, "reset", "--cluster")
	testutil.NoError(t, m.Check(ctx, id))
	run(id.ID+"\n", 0, "reset", "--cluster")
	freshWithoutSession, err := m.RecordedIdentity()
	testutil.NoError(t, err)
	if freshWithoutSession.MarkerUID == id.MarkerUID {
		t.Fatal("no-session reset did not replace the cluster")
	}
	id = freshWithoutSession
	testutil.NoError(t, m.Check(ctx, id))
	t.Log("Confirmed no-session cluster reset verified before learning")
	testutil.NoError(t, m.EnsurePlatform(ctx, id))
	client := kubeClient(t, m, id)
	owner := environment.Owner{Inventory: model.Inventory{EnvironmentID: id.ID, SessionID: "reset-live", ProblemID: "volume-test"}, Namespace: "reset-volume", Scope: model.ResourceScope{Namespaces: []string{"reset-peer"}}, CreateNamespaces: true}
	claim := map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": "data"}, "spec": map[string]any{"accessModes": []any{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]any{"storage": "32Mi"}}, "storageClassName": "standard"}}
	pod := map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "writer"}, "spec": map[string]any{"containers": []any{map[string]any{"name": "writer", "image": environment.ProbeImage.Ref(), "command": []any{"sh", "-c", "sleep 3600"}, "volumeMounts": []any{map[string]any{"name": "data", "mountPath": "/data"}}}}, "volumes": []any{map[string]any{"name": "data", "persistentVolumeClaim": map[string]any{"claimName": "data"}}}}}
	setup := []environment.Action{{Kind: model.ActionApply, Objects: []map[string]any{claim, pod}}}
	apply := func() (model.Inventory, string) {
		inv, err := m.Apply(ctx, id, owner, setup)
		testutil.NoError(t, err)
		testutil.NoError(t, wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
			p, e := client.CoreV1().Pods(owner.Namespace).Get(ctx, "writer", metav1.GetOptions{})
			return e == nil && p.Status.Phase == corev1.PodRunning, nil
		}))
		pvc, e := client.CoreV1().PersistentVolumeClaims(owner.Namespace).Get(ctx, "data", metav1.GetOptions{})
		testutil.NoError(t, e)
		return inv, pvc.Spec.VolumeName
	}
	kubectl := func(args ...string) string {
		cmd := exec.Command("kubectl", append([]string{"--kubeconfig", m.KubeconfigPath(), "--context", id.Context}, args...)...)
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("kubectl %v: %v %s", args, e, b)
		}
		return string(b)
	}
	inv, pvName := apply()
	kubectl("exec", "-n", owner.Namespace, "writer", "--", "sh", "-c", "echo old-attempt-marker > /data/marker")
	pv, err := client.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	testutil.NoError(t, err)
	backing := pv.Spec.HostPath.Path
	testutil.NoError(t, m.Cleanup(ctx, id, inv))
	owner.Inventory.ProblemID = "delete-volume"
	owner.Namespace = "reset-delete"
	owner.Scope.Namespaces = []string{"reset-delete-peer"}
	inv, pvName = apply()
	kubectl("exec", "-n", owner.Namespace, "writer", "--", "sh", "-c", "echo old-attempt-marker > /data/marker")
	pv, err = client.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	testutil.NoError(t, err)
	backing = pv.Spec.HostPath.Path
	testutil.NoError(t, m.Cleanup(ctx, id, inv))
	_, err = toolEnv.Run(ctx, "docker", []string{"exec", id.ContainerIDs[0], "test", "!", "-e", backing}, nil)
	testutil.NoError(t, err)
	inv, _ = apply()
	out := kubectl("exec", "-n", owner.Namespace, "writer", "--", "sh", "-c", "test ! -e /data/marker && echo clean")
	if strings.Contains(out, "old-attempt-marker") || !strings.Contains(out, "clean") {
		t.Fatal(out)
	}
	testutil.NoError(t, m.Cleanup(ctx, id, inv))
	t.Log("Delete-backed PVC path disappeared; replacement volume is clean")

	// Real CLI runtime: no injected service/engine. Certify existing test fixtures.
	rt, err := author.NewRuntime(ctx, m, st, id, author.GraderConfig{StateDir: dir})
	testutil.NoError(t, err)
	defer rt.Close()
	for _, pid := range devExamOrder {
		t.Logf("Certifying fixture %s", pid)
		p, e := problem.Load(filepath.Join(devBank, "problems", pid))
		testutil.NoError(t, e)
		seeds, e := author.CoveringSeeds(p)
		testutil.NoError(t, e)
		r, e := rt.Validator.Validate(ctx, p, seeds)
		testutil.NoError(t, e)
		testutil.Equal(t, r.Passed, true)
		t.Logf("Certified fixture %s", pid)
	}
	run("yes\n", 0, "start", "--mode", "practice", "--count", "5", "--seed", "7")
	sid, err := st.CurrentSessionID(ctx)
	testutil.NoError(t, err)
	rec, err := st.LoadSession(ctx, sid)
	testutil.NoError(t, err)
	personal := filepath.Join(workspace, ".bashrc")
	testutil.NoError(t, os.WriteFile(personal, []byte("alias k=kubectl"), 0600))
	first := rec.Problems[0].Definition.ID
	_, err = client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "foreign-reset-sentinel", Labels: map[string]string{"owner": "foreign"}}}, metav1.CreateOptions{})
	testutil.NoError(t, err)
	sentinel, err := client.CoreV1().ConfigMaps("foreign-reset-sentinel").Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "keep"}, Data: map[string]string{"value": "untouched"}}, metav1.CreateOptions{})
	testutil.NoError(t, err)
	old, _, err := session.LatestAttempt(workspace, sid, first)
	testutil.NoError(t, err)
	testutil.NoError(t, os.WriteFile(filepath.Join(old, "answer.yaml"), []byte("personal solution"), 0600))
	run("", 1, "grade", "--problem", "Q1")
	run("", 0, "reset", "--problem", "Q1")
	_, n, err := session.LatestAttempt(workspace, sid, first)
	testutil.NoError(t, err)
	testutil.Equal(t, n, 2)
	archived := run("", 0, "review", "--problem", "Q1")
	if strings.Contains(archived, "확정 결과:") || !strings.Contains(archived, "과거 관찰") {
		t.Fatal("pre-reset score presented as current", archived)
	}
	run("", 2, "grade", "--problem", "Q1", "--retry")
	run("", 1, "grade", "--problem", "Q1")
	kept, err := client.CoreV1().ConfigMaps(sentinel.Namespace).Get(ctx, sentinel.Name, metav1.GetOptions{})
	testutil.NoError(t, err)
	testutil.Equal(t, kept.UID, sentinel.UID)
	testutil.Equal(t, kept.Data["value"], "untouched")
	run("", 1, "grade")
	run("wrong\n", 2, "reset")
	run(id.ID+"\n", 0, "reset")
	packageDir, err := session.PackageDir(dir, rec.Problems[0].Hash)
	testutil.NoError(t, err)
	testutil.NoError(t, os.Rename(packageDir, packageDir+".held"))
	refused := run("yes\n", 3, "start", "--retry", sid, "--failed")
	testutil.NoError(t, os.Rename(packageDir+".held", packageDir))
	if !strings.Contains(refused, "패키지") {
		t.Fatal("missing exact package did not explain refusal", refused)
	}
	run("yes\n", 0, "start", "--retry", sid, "--failed")
	retryID, err := st.CurrentSessionID(ctx)
	testutil.NoError(t, err)
	retry, err := st.LoadSession(ctx, retryID)
	testutil.NoError(t, err)
	testutil.Equal(t, retry.ParentID, sid)
	testutil.Equal(t, retry.Seed, rec.Seed)
	for _, f := range retry.Problems {
		for _, old := range rec.Problems {
			if f.Definition.ID == old.Definition.ID {
				testutil.Equal(t, f.Hash, old.Hash)
				testutil.Equal(t, f.Parameters, old.Parameters)
			}
		}
	}
	if len(retry.Problems) < 1 || len(retry.Problems) > 20 {
		t.Fatal("retry count", len(retry.Problems))
	}
	run(id.ID+"\n", 0, "reset")
	run("yes\n", 0, "start", "--retry", sid, "--failed", "--new-seed")
	newID, err := st.CurrentSessionID(ctx)
	testutil.NoError(t, err)
	newRec, err := st.LoadSession(ctx, newID)
	testutil.NoError(t, err)
	if newRec.Seed == rec.Seed {
		t.Fatal("new seed unchanged")
	}
	b, err := os.ReadFile(personal)
	testutil.NoError(t, err)
	testutil.Equal(t, string(b), "alias k=kubectl")
	b, err = os.ReadFile(filepath.Join(old, "answer.yaml"))
	testutil.NoError(t, err)
	testutil.Equal(t, string(b), "personal solution")
	owner.Inventory.ProblemID = "retained-volume"
	owner.Namespace = "reset-retained"
	owner.Scope.Namespaces = []string{"reset-retained-peer"}
	inv, pvName = apply()
	pv, err = client.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	testutil.NoError(t, err)
	pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
	_, err = client.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{})
	testutil.NoError(t, err)
	testutil.ErrorIs(t, m.Cleanup(ctx, id, inv), environment.ErrStorageRetained)
	_, err = client.CoreV1().Namespaces().Get(ctx, owner.Namespace, metav1.GetOptions{})
	testutil.NoError(t, err)
	ops, err := st.ListOperations(ctx, environment.OperationCleanup)
	testutil.NoError(t, err)
	pending := false
	for _, op := range ops {
		if op.Planned.ProblemID == owner.Inventory.ProblemID && op.Phase != environment.PhaseDone {
			pending = true
		}
	}
	if !pending {
		t.Fatal("Retain refusal lost its pending journal")
	}
	blocked := run("yes\n", 3, "start", "--mode", "practice", "--count", "5")
	if !strings.Contains(blocked, "초기화") {
		t.Fatal("pending cleanup did not block new start", blocked)
	}
	t.Log("Retain refusal preserved namespace and journal")
	run(id.ID+"\n", 0, "reset", "--cluster")
	fresh, err := m.RecordedIdentity()
	testutil.NoError(t, err)
	if fresh.MarkerUID == id.MarkerUID {
		t.Fatal("cluster not recreated")
	}
	testutil.NoError(t, m.Check(ctx, fresh))
	t.Log(fmt.Sprintf("cluster recreation verified: %s -> %s", id.MarkerUID, fresh.MarkerUID))
}
