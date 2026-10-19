//go:build integration

package integration

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

// One scoped engine fixture: real PVC, real ReplicaSet replacement, a rejected
// EmptyDir answer, and the same owned storage cleanup used by Task13 reset.
func TestTask14VolumePersistence(t *testing.T) {
	requireTools(t, "docker", "kind", "kubectl")
	ctx := context.Background()
	phase := func(s string) { t.Logf("PHASE %s at %s", s, time.Now().UTC().Format(time.RFC3339)) }
	state, st := newEnvironmentDir(t)
	m := environment.New(state, st, hostCompatOptions(t)...)
	phase("boot scoped volume environment")
	id, err := m.Ensure(ctx)
	testutil.NoError(t, err)
	t.Cleanup(func() { testutil.NoError(t, m.StopPlatform(ctx, id)) })
	testutil.NoError(t, author.PrepareEnvironment(ctx, m, id, nil))
	client := kubeClient(t, m, id)
	owner := environment.Owner{Inventory: model.Inventory{EnvironmentID: id.ID, SessionID: "task14-volume", ProblemID: "task14-volume"}, Namespace: "task14-volume", CreateNamespaces: true}
	claim := map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": "data"}, "spec": map[string]any{"accessModes": []any{"ReadWriteOnce"}, "storageClassName": "standard", "resources": map[string]any{"requests": map[string]any{"storage": "32Mi"}}}}
	deployment := func(name string, persistent bool) map[string]any {
		vol := map[string]any{"name": "storage", "emptyDir": map[string]any{}}
		if persistent {
			vol = map[string]any{"name": "storage", "persistentVolumeClaim": map[string]any{"claimName": "data"}}
		}
		return map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": name}, "spec": map[string]any{"replicas": 1, "selector": map[string]any{"matchLabels": map[string]any{"app": name}}, "template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": name}}, "spec": map[string]any{"containers": []any{map[string]any{"name": "app", "image": environment.ProbeImage.Ref(), "command": []any{"sh", "-c", "sleep 3600"}, "resources": map[string]any{"requests": map[string]any{"cpu": "10m", "memory": "8Mi"}, "limits": map[string]any{"cpu": "100m", "memory": "32Mi"}}, "volumeMounts": []any{map[string]any{"name": "storage", "mountPath": "/data"}}}}, "volumes": []any{vol}}}}}
	}
	setup := []environment.Action{{Kind: model.ActionApply, Objects: []map[string]any{claim, deployment("persistent", true), deployment("ephemeral", false)}}}
	inv, err := m.Apply(ctx, id, owner, setup)
	testutil.NoError(t, err)
	waitReady := func() {
		t.Helper()
		testutil.NoError(t, wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
			pods, e := client.CoreV1().Pods(owner.Namespace).List(ctx, metav1.ListOptions{})
			if e != nil {
				return false, e
			}
			ready := 0
			for _, pod := range pods.Items {
				for _, c := range pod.Status.Conditions {
					if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
						ready++
					}
				}
			}
			return ready == 2, nil
		}))
	}
	waitReady()
	engine, err := author.ComposeGrader(m, id, author.GraderConfig{StateDir: state})
	testutil.NoError(t, err)
	defer engine.Close()
	criterion := model.Criterion{ID: "persistence", Kind: model.CriterionVolumePersistence, Target: model.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: owner.Namespace, Name: "persistent"}, Points: 1, Timeout: 120 * time.Second, Args: map[string]any{"container": "app", "path": "/data", "claim": "data"}}
	target := grader.Target{Identity: id, SessionID: owner.Inventory.SessionID, Problem: model.FrozenProblem{Definition: model.Problem{ID: owner.Inventory.ProblemID, Namespace: owner.Namespace, Criteria: []model.Criterion{criterion}}}}
	kubectl := func(args ...string) string {
		t.Helper()
		r, e := toolEnv.Run(ctx, "kubectl", append([]string{"--kubeconfig", m.KubeconfigPath(), "--context", id.Context}, args...), nil)
		testutil.NoError(t, e)
		return string(r.Stdout)
	}
	evaluate := func(c model.Criterion) model.CheckResult {
		t.Helper()
		tgt := target
		tgt.Problem.Definition.Criteria = []model.Criterion{c}
		ev, e := engine.Engine.Evaluate(ctx, tgt)
		testutil.NoError(t, e)
		return ev.Results[0]
	}
	pods, err := client.CoreV1().Pods(owner.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=persistent"})
	testutil.NoError(t, err)
	testutil.Equal(t, len(pods.Items), 1)
	old := pods.Items[0]
	kubectl("exec", "-n", owner.Namespace, old.Name, "--", "sh", "-c", "printf learner-data > /data/learner-file")
	before, err := engine.Artifacts.CaptureInputs(ctx, target)
	testutil.NoError(t, err)
	phase("prove PVC marker across one UID-preconditioned Pod replacement")
	res := evaluate(criterion)
	testutil.Equal(t, res.Status, model.StatusPass)
	t.Logf("PVC evidence: %+v", res.Evidence.Actual)
	after, err := engine.Artifacts.CaptureInputs(ctx, target)
	testutil.NoError(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("intentional child replacement changed inputs: before=%v after=%v", before, after)
	}
	pods, err = client.CoreV1().Pods(owner.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=persistent"})
	testutil.NoError(t, err)
	testutil.Equal(t, len(pods.Items), 1)
	fresh := pods.Items[0]
	if fresh.UID == old.UID {
		t.Fatal("Pod UID was not replaced")
	}
	testutil.Equal(t, kubectl("exec", "-n", owner.Namespace, fresh.Name, "--", "cat", "/data/learner-file"), "learner-data")
	markers := kubectl("exec", "-n", owner.Namespace, fresh.Name, "--", "sh", "-c", "find /data -name '.junkloud-persistence-*' -print")
	if strings.TrimSpace(markers) != "" {
		t.Fatal("checker left its marker")
	}
	phase("reject EmptyDir and wrong claim without Pod replacement")
	wrong := criterion
	wrong.Target.Name = "ephemeral"
	res = evaluate(wrong)
	testutil.Equal(t, res.Status, model.StatusFail)
	wrong = criterion
	wrong.Args = map[string]any{"container": "app", "path": "/data", "claim": "wrong"}
	res = evaluate(wrong)
	testutil.Equal(t, res.Status, model.StatusFail)
	unchanged, err := client.CoreV1().Pods(owner.Namespace).Get(ctx, fresh.Name, metav1.GetOptions{})
	testutil.NoError(t, err)
	testutil.Equal(t, unchanged.UID, fresh.UID)
	pvc, err := client.CoreV1().PersistentVolumeClaims(owner.Namespace).Get(ctx, "data", metav1.GetOptions{})
	testutil.NoError(t, err)
	pv, err := client.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
	testutil.NoError(t, err)
	if pv.Spec.HostPath == nil {
		t.Fatal("expected Task13 supported local-path backing")
	}
	backing := pv.Spec.HostPath.Path
	phase("Task13 owned reset removes actual backing and restores clean initial state")
	testutil.NoError(t, m.Cleanup(ctx, id, inv))
	_, err = toolEnv.Run(ctx, "docker", []string{"exec", id.ContainerIDs[0], "test", "!", "-e", backing}, nil)
	testutil.NoError(t, err)
	inv, err = m.Apply(ctx, id, owner, setup)
	testutil.NoError(t, err)
	waitReady()
	pods, err = client.CoreV1().Pods(owner.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=persistent"})
	testutil.NoError(t, err)
	testutil.Equal(t, len(pods.Items), 1)
	clean := kubectl("exec", "-n", owner.Namespace, pods.Items[0].Name, "--", "sh", "-c", "test ! -e /data/learner-file && test -z \"$(find /data -name '.junkloud-persistence-*' -print)\" && printf clean")
	testutil.Equal(t, clean, "clean")
	testutil.NoError(t, m.Cleanup(ctx, id, inv))
	phase(fmt.Sprintf("PASS PVC/EmptyDir/fingerprint/reset (state %s)", filepath.Base(state)))
}
