//go:build integration

package integration

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/JunYupK/JunKloud/internal/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

// TestResetStorageReconciliation targets the review's provisioning/deleted
// claim gap without repeating the five fixture certification workflow.
func TestResetStorageReconciliation(t *testing.T) {
	requireTools(t, "docker", "kind", "kubectl", "go")
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "sessions.db"))
	testutil.NoError(t, err)
	m := environment.New(dir, st, hostCompatOptions(t)...)
	id, err := m.Ensure(ctx)
	testutil.NoError(t, err)
	t.Cleanup(func() {
		if e := m.StopPlatform(ctx, id); e != nil {
			t.Error(e)
		}
		kindDelete(t, id.ClusterName, filepath.Join(dir, "cleanup.kubeconfig"))
		st.Close()
	})
	testutil.NoError(t, m.EnsurePlatform(ctx, id))
	client := kubeClient(t, m, id)
	owner := environment.Owner{Inventory: model.Inventory{EnvironmentID: id.ID, SessionID: "storage-regression", ProblemID: "pending"}, Namespace: "storage-pending", CreateNamespaces: true}
	claim := map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": "data"}, "spec": map[string]any{"accessModes": []any{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]any{"storage": "32Mi"}}, "storageClassName": "standard"}}
	pod := map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "writer"}, "spec": map[string]any{"containers": []any{map[string]any{"name": "writer", "image": environment.ProbeImage.Ref(), "command": []any{"sh", "-c", "sleep 3600"}, "volumeMounts": []any{map[string]any{"name": "data", "mountPath": "/data"}}}}, "volumes": []any{map[string]any{"name": "data", "persistentVolumeClaim": map[string]any{"claimName": "data"}}}}}
	apply := func(objects ...map[string]any) model.Inventory {
		inv, e := m.Apply(ctx, id, owner, []environment.Action{{Kind: model.ActionApply, Objects: objects}})
		testutil.NoError(t, e)
		return inv
	}
	waitWriter := func() {
		testutil.NoError(t, wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
			p, e := client.CoreV1().Pods(owner.Namespace).Get(ctx, "writer", metav1.GetOptions{})
			return e == nil && p.Status.Phase == corev1.PodRunning, nil
		}))
	}
	inv := apply(claim)
	pvc, err := client.CoreV1().PersistentVolumeClaims(owner.Namespace).Get(ctx, "data", metav1.GetOptions{})
	testutil.NoError(t, err)
	testutil.Equal(t, pvc.Spec.VolumeName, "")
	testutil.ErrorIs(t, m.Cleanup(ctx, id, inv), environment.ErrStorageRetained)
	ops, err := st.ListOperations(ctx, environment.OperationCleanup)
	testutil.NoError(t, err)
	if len(ops) != 1 || ops[0].Phase != environment.PhasePlanned || len(ops[0].Planned.Volumes) != 1 || ops[0].Planned.Volumes[0].ClaimUID != string(pvc.UID) {
		t.Fatal("pending claim evidence lost", ops)
	}
	owner.Inventory = inv
	inv = apply(pod)
	waitWriter()
	testutil.NoError(t, m.Cleanup(ctx, id, inv))
	t.Log("Unbound PVC refused before deletion; later binding recovered with backing deletion proof")

	owner.Inventory = model.Inventory{EnvironmentID: id.ID, SessionID: "storage-regression", ProblemID: "deleted-retain"}
	owner.Namespace = "storage-retained"
	inv = apply(claim, pod)
	waitWriter()
	pvc, err = client.CoreV1().PersistentVolumeClaims(owner.Namespace).Get(ctx, "data", metav1.GetOptions{})
	testutil.NoError(t, err)
	pv, err := client.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
	testutil.NoError(t, err)
	backing := pv.Spec.HostPath.Path
	cmd := exec.Command("kubectl", "--kubeconfig", m.KubeconfigPath(), "--context", id.Context, "exec", "-n", owner.Namespace, "writer", "--", "sh", "-c", "echo retained-marker > /data/marker")
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("write marker: %v %s", e, out)
	}
	pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
	_, err = client.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{})
	testutil.NoError(t, err)
	testutil.NoError(t, client.CoreV1().Pods(owner.Namespace).Delete(ctx, "writer", metav1.DeleteOptions{}))
	testutil.NoError(t, client.CoreV1().PersistentVolumeClaims(owner.Namespace).Delete(ctx, "data", metav1.DeleteOptions{}))
	testutil.NoError(t, wait.PollUntilContextTimeout(ctx, time.Second, 90*time.Second, true, func(ctx context.Context) (bool, error) {
		_, e := client.CoreV1().PersistentVolumeClaims(owner.Namespace).Get(ctx, "data", metav1.GetOptions{})
		return apierrors.IsNotFound(e), nil
	}))
	testutil.NoError(t, wait.PollUntilContextTimeout(ctx, time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		v, e := client.CoreV1().PersistentVolumes().Get(ctx, pv.Name, metav1.GetOptions{})
		return e == nil && v.Status.Phase == corev1.VolumeReleased, nil
	}))
	testutil.ErrorIs(t, m.Cleanup(ctx, id, inv), environment.ErrStorageRetained)
	kept, err := client.CoreV1().PersistentVolumes().Get(ctx, pv.Name, metav1.GetOptions{})
	testutil.NoError(t, err)
	testutil.Equal(t, kept.UID, pv.UID)
	_, err = client.CoreV1().Namespaces().Get(ctx, owner.Namespace, metav1.GetOptions{})
	testutil.NoError(t, err)
	_, err = toolEnv.Run(ctx, "docker", []string{"exec", id.ContainerIDs[0], "test", "-f", backing + "/marker"}, nil)
	testutil.NoError(t, err)
	live, err := st.GetInventory(ctx, id.ID, owner.Inventory.SessionID, owner.Inventory.ProblemID)
	testutil.NoError(t, err)
	testutil.Equal(t, live.Resources, inv.Resources)
	ops, err = st.ListOperations(ctx, environment.OperationCleanup)
	testutil.NoError(t, err)
	found := false
	for _, op := range ops {
		if op.Planned.ProblemID == owner.Inventory.ProblemID && op.Phase != environment.PhaseDone && len(op.Planned.Volumes) == 1 && op.Planned.Volumes[0].ClaimUID == string(pvc.UID) {
			found = true
		}
	}
	if !found {
		t.Fatal("deleted claim backing proof and pending journal lost", ops)
	}
	binary := filepath.Join(dir, "ckad")
	build := exec.Command("go", "build", "-o", binary, "./cmd/ckad")
	build.Dir = "../.."
	if out, e := build.CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, out)
	}
	bank, err := filepath.Abs(devBank)
	testutil.NoError(t, err)
	cmd = exec.Command(binary, "--state-dir", dir, "--bank-dir", bank, "--workspace", filepath.Join(dir, "work"), "start", "--mode", "practice", "--count", "5")
	cmd.Env = append(os.Environ(), "CKAD_HOST_COMPAT="+os.Getenv("JUNKLOUD_TEST_HOST_COMPAT"))
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 || !strings.Contains(string(out), "초기화") {
		t.Fatalf("new start bypassed deleted Retain PVC: %v %s", err, out)
	}
	t.Log("Deleted PVC + Released Retain PV refused; namespace, marker, inventory and journal preserved; real CLI start blocked")
}
