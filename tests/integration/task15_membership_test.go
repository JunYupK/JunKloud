//go:build integration

package integration

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

// Exercises the real runtime composition, native API identity chain and the
// existing bounded probe runner. No random Service response ratio is asserted.
func TestTask15EndpointMembership(t *testing.T) {
	requireTools(t, "docker", "kind", "kubectl")
	ctx, cancel := context.WithTimeout(context.Background(), 19*time.Minute)
	defer cancel()
	phase := func(s string) { t.Logf("PHASE %s at %s", s, time.Now().UTC().Format(time.RFC3339)) }
	state, st := newEnvironmentDir(t)
	m := environment.New(state, st, hostCompatOptions(t)...)
	phase("boot scoped backend membership environment")
	id, err := m.Ensure(ctx)
	testutil.NoError(t, err)
	t.Cleanup(func() { testutil.NoError(t, m.StopPlatform(context.Background(), id)) })
	testutil.NoError(t, author.PrepareEnvironment(ctx, m, id, nil))
	rt, err := author.NewRuntime(ctx, m, st, id, author.GraderConfig{StateDir: state})
	testutil.NoError(t, err)
	defer rt.Close()
	client := kubeClient(t, m, id)
	owner := environment.Owner{Inventory: model.Inventory{EnvironmentID: id.ID, SessionID: "task15-members", ProblemID: "task15-members"}, Namespace: "task15-members", CreateNamespaces: true}
	var setup []environment.Action
	for _, name := range []string{"stable", "canary"} {
		marker := "STABLE"
		if name == "canary" {
			marker = "CANARY"
		}
		setup = append(setup, environment.Action{Kind: model.ActionApply, Objects: []map[string]any{
			map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": name + "-config"}, "data": map[string]any{"marker": marker}},
			map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": name}, "spec": map[string]any{"replicas": 1, "selector": map[string]any{"matchLabels": map[string]any{"app": "release", "track": name}}, "template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": "release", "track": name}}, "spec": map[string]any{"containers": []any{map[string]any{"name": "web", "image": environment.ProbeImage.Name, "imagePullPolicy": "Never", "command": []any{"sh", "-c", `mkdir -p /www; printf '%s' "$MARKER" > /www/index.html; exec httpd -f -p 8080 -h /www`}, "env": []any{map[string]any{"name": "MARKER", "valueFrom": map[string]any{"configMapKeyRef": map[string]any{"name": name + "-config", "key": "marker"}}}}, "resources": map[string]any{"requests": map[string]any{"cpu": "10m", "memory": "8Mi"}, "limits": map[string]any{"cpu": "100m", "memory": "32Mi"}}}}}}}},
		}})
	}
	setup = append(setup, environment.Action{Kind: model.ActionApply, Objects: []map[string]any{{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "web"}, "spec": map[string]any{"selector": map[string]any{"app": "release"}, "ports": []any{map[string]any{"port": 80, "targetPort": 8080, "protocol": "TCP"}}}}}})
	inv, err := m.Apply(ctx, id, owner, setup)
	testutil.NoError(t, err)
	t.Cleanup(func() { testutil.NoError(t, m.Cleanup(context.Background(), id, inv)) })
	// A preexisting probe-like foreign Pod must survive every grader invocation.
	sentinel, err := client.CoreV1().Pods(owner.Namespace).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "jk-probe-sentinel", Labels: map[string]string{grader.LabelProbeComponent: grader.ProbeComponent, grader.LabelProbeEnv: "foreign"}}, Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "sentinel", Image: environment.ProbeImage.Name, ImagePullPolicy: corev1.PullNever, Command: []string{"sh", "-c", "sleep 3600"}}}}}, metav1.CreateOptions{})
	testutil.NoError(t, err)
	waitMembers := func(want int) {
		t.Helper()
		testutil.NoError(t, wait.PollUntilContextTimeout(ctx, time.Second, 90*time.Second, true, func(ctx context.Context) (bool, error) {
			slices, e := client.DiscoveryV1().EndpointSlices(owner.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "kubernetes.io/service-name=web"})
			if e != nil {
				return false, e
			}
			n := 0
			for _, s := range slices.Items {
				for _, ep := range s.Endpoints {
					if ep.Conditions.Ready != nil && *ep.Conditions.Ready {
						n++
					}
				}
			}
			return n == want, nil
		}))
	}
	waitMembers(2)
	testutil.NoError(t, wait.PollUntilContextTimeout(ctx, time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		p, e := client.CoreV1().Pods(owner.Namespace).Get(ctx, sentinel.Name, metav1.GetOptions{})
		if e != nil {
			return false, e
		}
		if p.Spec.NodeName == "" {
			return false, nil
		}
		sentinel = p
		return true, nil
	}))
	criterion := model.Criterion{ID: "members", Kind: model.CriterionEndpointMembership, Target: model.ObjectRef{APIVersion: "v1", Kind: "Service", Namespace: owner.Namespace, Name: "web"}, Points: 1, Timeout: 90 * time.Second, Args: map[string]any{"source": map[string]any{"labels": map[string]any{"role": "client"}}, "port": 80, "path": "/", "backends": []any{map[string]any{"deployment": "stable", "body_marker": "STABLE"}, map[string]any{"deployment": "canary", "body_marker": "CANARY"}}}}
	target := grader.Target{Identity: id, SessionID: owner.Inventory.SessionID, Problem: model.FrozenProblem{Definition: model.Problem{ID: owner.Inventory.ProblemID, Namespace: owner.Namespace, Criteria: []model.Criterion{criterion}}}}
	evaluate := func(want string) model.CheckResult {
		t.Helper()
		ev, e := rt.Grader.Engine.Evaluate(ctx, target)
		testutil.NoError(t, e)
		if len(ev.Results) != 1 || ev.Results[0].Status != want {
			t.Fatalf("want %s got %+v", want, ev)
		}
		t.Logf("MEMBERSHIP %s evidence=%+v", want, ev.Results[0].Evidence.Actual)
		return ev.Results[0]
	}
	checkCleanup := func() {
		t.Helper()
		pods, e := client.CoreV1().Pods(owner.Namespace).List(ctx, metav1.ListOptions{LabelSelector: grader.LabelProbeComponent + "=" + grader.ProbeComponent})
		testutil.NoError(t, e)
		if len(pods.Items) != 1 || pods.Items[0].UID != sentinel.UID || !reflect.DeepEqual(pods.Items[0].Spec, sentinel.Spec) || !reflect.DeepEqual(pods.Items[0].Labels, sentinel.Labels) {
			t.Fatalf("foreign sentinel changed or probe leaked: %+v", pods.Items)
		}
	}
	phase("both stable and canary prove exact native UID chain and actual HTTP content")
	before, err := rt.Grader.Artifacts.CaptureInputs(ctx, target)
	testutil.NoError(t, err)
	res := evaluate(model.StatusPass)
	checkCleanup()
	evidence := res.Evidence.Actual.(map[string]any)
	service, err := client.CoreV1().Services(owner.Namespace).Get(ctx, "web", metav1.GetOptions{})
	testutil.NoError(t, err)
	testutil.Equal(t, evidence["service_uid"], string(service.UID))
	members := evidence["ready_members"].([]any)
	testutil.Equal(t, len(members), 2)
	for _, v := range members {
		proof := v.(map[string]any)
		pod, e := client.CoreV1().Pods(owner.Namespace).Get(ctx, proof["pod"].(string), metav1.GetOptions{})
		testutil.NoError(t, e)
		testutil.Equal(t, proof["pod_uid"], string(pod.UID))
		rs, e := client.AppsV1().ReplicaSets(owner.Namespace).Get(ctx, proof["replica_set"].(string), metav1.GetOptions{})
		testutil.NoError(t, e)
		testutil.Equal(t, proof["replica_set_uid"], string(rs.UID))
		dep, e := client.AppsV1().Deployments(owner.Namespace).Get(ctx, proof["deployment"].(string), metav1.GetOptions{})
		testutil.NoError(t, e)
		testutil.Equal(t, proof["deployment_uid"], string(dep.UID))
		slice, e := client.DiscoveryV1().EndpointSlices(owner.Namespace).Get(ctx, proof["slice"].(string), metav1.GetOptions{})
		testutil.NoError(t, e)
		testutil.Equal(t, proof["slice_uid"], string(slice.UID))
		testutil.Equal(t, string(slice.OwnerReferences[0].UID), string(service.UID))
		testutil.Equal(t, string(pod.OwnerReferences[0].UID), string(rs.UID))
		testutil.Equal(t, string(rs.OwnerReferences[0].UID), string(dep.UID))
	}
	after, err := rt.Grader.Artifacts.CaptureInputs(ctx, target)
	testutil.NoError(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("ephemeral probes changed submission inputs")
	}
	setTrack := func(track string) {
		t.Helper()
		patch := `{"spec":{"selector":{"track":null}}}`
		if track != "" {
			patch = fmt.Sprintf(`{"spec":{"selector":{"track":%q}}}`, track)
		}
		_, e := client.CoreV1().Services(owner.Namespace).Patch(ctx, "web", types.MergePatchType, []byte(patch), metav1.PatchOptions{})
		testutil.NoError(t, e)
		want := 2
		if track != "" {
			want = 1
		}
		waitMembers(want)
	}
	for _, track := range []string{"stable", "canary"} {
		phase("selector omits the other backend: " + track)
		setTrack(track)
		evaluate(model.StatusFail)
		checkCleanup()
	}
	phase("excluded canary referenced configuration remains an input")
	setTrack("stable")
	before, err = rt.Grader.Artifacts.CaptureInputs(ctx, target)
	testutil.NoError(t, err)
	cm, err := client.CoreV1().ConfigMaps(owner.Namespace).Get(ctx, "canary-config", metav1.GetOptions{})
	testutil.NoError(t, err)
	cm.Data["marker"] = "CHANGED"
	_, err = client.CoreV1().ConfigMaps(owner.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
	testutil.NoError(t, err)
	after, err = rt.Grader.Artifacts.CaptureInputs(ctx, target)
	testutil.NoError(t, err)
	if reflect.DeepEqual(before, after) {
		t.Fatal("excluded backend configuration change invisible")
	}
	setTrack("")
	pods, err := client.CoreV1().Pods(owner.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "track=canary"})
	testutil.NoError(t, err)
	testutil.Equal(t, len(pods.Items), 1)
	_, err = toolEnv.Run(ctx, "kubectl", []string{"--kubeconfig", m.KubeconfigPath(), "--context", id.Context, "exec", "-n", owner.Namespace, pods.Items[0].Name, "--", "sh", "-c", "printf WRONG > /www/index.html"}, nil)
	testutil.NoError(t, err)
	phase("both memberships retained but wrong actual canary output fails")
	evaluate(model.StatusFail)
	checkCleanup()
	phase("missing expected session produces ERROR and zero probe residue")
	target.SessionID = ""
	evaluate(model.StatusError)
	checkCleanup()
	phase("PASS stable/canary membership, omitted backends, actual wrong response, exact UID proof, foreign sentinel and probe cleanup")
}
