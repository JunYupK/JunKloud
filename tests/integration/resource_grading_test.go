//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

const gradeNamespace = "jk-grade"

// gradeDeployment is a learner's Deployment. image and the optional
// replicas/cpu are the knobs the scenarios turn. Everything else (strategy,
// imagePullPolicy, revisionHistoryLimit, ...) is left to API defaults.
func gradeDeployment(name, image string, replicas any, cpu any) map[string]any {
	spec := map[string]any{
		"selector": map[string]any{"matchLabels": map[string]any{"tier": "frontend", "app": name}},
		"template": map[string]any{
			"metadata": map[string]any{"labels": map[string]any{"app": name, "tier": "frontend"}},
			"spec": map[string]any{"containers": []any{
				// Declared before "web" on purpose: containers are matched by name.
				map[string]any{"name": "sidecar", "image": environment.ProbeImage.Name,
					"command": []any{"sleep", "3600"}},
				map[string]any{"name": "web", "image": image, "command": []any{"sleep", "3600"},
					"resources": map[string]any{"limits": map[string]any{"cpu": cpu, "memory": "64Mi"}}},
			}},
		},
	}
	if replicas != nil {
		spec["replicas"] = replicas
	}
	return map[string]any{"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": name, "namespace": gradeNamespace}, "spec": spec}
}

// gradeCriteria is the problem's grading definition for Deployment `name`:
// 2+2+2+2+4 points. The CPU limit and the replica count are written the way
// the author thinks of them; learners may use any equivalent form.
func gradeCriteria(name string, readyTimeout time.Duration) []model.Criterion {
	ref := model.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: gradeNamespace, Name: name}
	containers := "spec.template.spec.containers[name=web]"
	return []model.Criterion{
		{ID: "exists", Kind: model.CriterionResource, Target: ref, Points: 2},
		{ID: "image", Kind: model.CriterionField, Target: ref, Points: 2, DependsOn: []string{"exists"},
			Args: map[string]any{"fields": []any{
				map[string]any{"path": containers + ".image", "equals": environment.ProbeImage.Name}}}},
		{ID: "limits", Kind: model.CriterionField, Target: ref, Points: 2, DependsOn: []string{"exists"},
			Args: map[string]any{"fields": []any{
				map[string]any{"path": containers + ".resources.limits.cpu", "equals": "1000m"},
				map[string]any{"path": containers + ".resources.limits.memory", "equals": 67108864}}}},
		{ID: "defaults", Kind: model.CriterionField, Target: ref, Points: 2, DependsOn: []string{"exists"},
			Args: map[string]any{"fields": []any{
				map[string]any{"path": "spec.replicas", "equals": 1},
				map[string]any{"path": "spec.strategy.type", "equals": "RollingUpdate"},
				map[string]any{"path": containers + ".imagePullPolicy", "equals": "IfNotPresent"},
				map[string]any{"path": "spec.selector.matchLabels", "equals": map[string]any{"app": name, "tier": "frontend"}}}}},
		{ID: "ready", Kind: model.CriterionReady, Target: ref, Points: 4, DependsOn: []string{"exists"}, Timeout: readyTimeout},
	}
}

func gradeTarget(id model.Identity, criteria []model.Criterion) grader.Target {
	return grader.Target{
		Identity: id,
		Problem: model.FrozenProblem{
			Definition: model.Problem{ID: "grade-res", Namespace: gradeNamespace, Criteria: criteria},
			Namespace:  gradeNamespace,
		},
	}
}

func statuses(ev model.Evaluation) map[string]string {
	out := map[string]string{}
	for _, r := range ev.Results {
		out[r.CriterionID] = r.Status
	}
	return out
}

func TestResourceGrading(t *testing.T) {
	requireTools(t, "docker", "kind")
	ctx := context.Background()
	m, id := bootEnvironment(t)
	testutil.NoError(t, m.EnsurePlatform(ctx, id))
	testutil.NoError(t, m.PreloadImages(ctx, id, environment.ProbeImage))

	redactor := grader.NewRedactor()
	// The environment itself provides the probe image (preloaded above); a
	// pull failure of any other image is the learner's wrong image.
	opts := []grader.Option{grader.WithRedactor(redactor), grader.WithPreloadedImages(environment.ProbeImage.Name)}
	reader, err := grader.NewClusterReader(m.KubeconfigPath(), id.Context)
	testutil.NoError(t, err)
	engine := grader.NewEngine(grader.DefaultCheckers(reader, opts...), opts...)

	// Setup creates the problem namespace; every later step only writes into it.
	owner := environment.Owner{
		Inventory:        model.Inventory{EnvironmentID: id.ID, SessionID: "s1", ProblemID: "grade-res"},
		Namespace:        gradeNamespace,
		CreateNamespaces: true,
	}
	inv, err := m.Apply(ctx, id, owner, []environment.Action{{Kind: model.ActionApply, Objects: []map[string]any{
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "marker"}, "data": map[string]any{"k": "v"}},
	}}})
	testutil.NoError(t, err)
	owner.Inventory, owner.CreateNamespaces = inv, false
	apply := func(objs ...map[string]any) {
		t.Helper()
		var err error
		owner.Inventory, err = m.Apply(ctx, id, owner, []environment.Action{{Kind: model.ActionApply, Objects: objs}})
		testutil.NoError(t, err)
	}
	uidOf := func(kind, name string) string {
		for _, r := range owner.Inventory.Resources {
			if r.Ref.Kind == kind && r.Ref.Name == name {
				return r.UID
			}
		}
		t.Fatalf("%s %s is not in the inventory", kind, name)
		return ""
	}
	grade := func(criteria []model.Criterion) model.Evaluation {
		t.Helper()
		ev, err := engine.Evaluate(ctx, gradeTarget(id, criteria))
		testutil.NoError(t, err)
		return ev
	}
	pass := map[string]string{"exists": "PASS", "image": "PASS", "limits": "PASS", "defaults": "PASS", "ready": "PASS"}

	t.Run("EquivalentAnswerWithAPIDefaults", func(t *testing.T) {
		// replicas, strategy and imagePullPolicy are omitted (defaulted by
		// the API); the CPU limit is the integer 1 where the criterion says
		// 1000m; the selector map is in another key order.
		apply(gradeDeployment("equiv", environment.ProbeImage.Name, nil, 1))
		start := time.Now()
		ev := grade(gradeCriteria("equiv", 90*time.Second))
		t.Logf("graded in %v", time.Since(start))
		testutil.Equal(t, statuses(ev), pass)
		testutil.Equal(t, *ev.Score, 12)
		testutil.Equal(t, ev.MaxScore, 12)
		testutil.Equal(t, ev.Complete, true)
		r := ev.Results[4].Evidence
		testutil.Equal(t, r.UID, uidOf("Deployment", "equiv"))
		if r.Generation < 1 || r.ObservedAt.IsZero() || r.Duration <= 0 || !strings.HasPrefix(r.InputHash, "sha256:") {
			t.Fatalf("evidence incomplete: %+v", r)
		}
		testutil.Equal(t, r.ObservedAt.Location(), time.UTC)
	})

	// Two kinds of wrong image. A malformed reference is proven wrong by the
	// kubelet alone, on any host. A tag that does not exist is proven wrong
	// only by a registry that answers "not found"; a node that cannot reach
	// the registry (this sandbox) proves nothing, and the honest result is
	// ERROR.
	const (
		malformedImage = "docker.io/library/BusyBox:1.37.0" // repository names must be lowercase
		missingTag     = "docker.io/library/busybox:jk-does-not-exist"
	)

	t.Run("WrongImageIsFail", func(t *testing.T) {
		// The pod never becomes ready because of the learner's image
		// reference. A short explicit limit keeps the test fast.
		apply(gradeDeployment("wrong", malformedImage, nil, 1))
		start := time.Now()
		ev := grade(gradeCriteria("wrong", 8*time.Second))
		elapsed := time.Since(start)
		testutil.Equal(t, statuses(ev), map[string]string{"exists": "PASS", "image": "FAIL", "limits": "PASS", "defaults": "PASS", "ready": "FAIL"})
		testutil.Equal(t, *ev.Score, 6)
		testutil.Equal(t, ev.Complete, true)
		for _, r := range ev.Results {
			switch r.CriterionID {
			case "ready":
				for _, want := range []string{"8s", "availableReplicas", "cause", "InvalidImageName", "BusyBox"} {
					if !strings.Contains(r.Reason, want) {
						t.Errorf("ready reason lacks %q: %q", want, r.Reason)
					}
				}
				act, _ := r.Evidence.Actual.(map[string]any)
				if pods, _ := act["pods"].([]any); len(pods) == 0 {
					t.Errorf("evidence lacks the failing pod: %v", r.Evidence.Actual)
				}
			case "image":
				if !strings.Contains(r.Reason, "BusyBox") {
					t.Errorf("image reason does not show the actual image: %q", r.Reason)
				}
			}
		}
		if elapsed < 8*time.Second || elapsed > 40*time.Second {
			t.Errorf("grading took %v, want about the 8s limit", elapsed)
		}
	})

	t.Run("MissingTagFollowsTheRegistryAnswer", func(t *testing.T) {
		apply(gradeDeployment("missing", missingTag, nil, "100m")) // small: the node is already busy
		crits := gradeCriteria("missing", 8*time.Second)
		ev := grade([]model.Criterion{crits[0], crits[4]})

		// What did the node's registry pull actually say? Prove the expected
		// class from the cluster's own events instead of assuming it.
		var pulls []string
		events, err := kubeClient(t, m, id).CoreV1().Events(gradeNamespace).List(ctx, metav1.ListOptions{})
		testutil.NoError(t, err)
		for _, e := range events.Items {
			if e.InvolvedObject.Kind == "Pod" && strings.HasPrefix(e.InvolvedObject.Name, "missing-") && strings.HasPrefix(e.Message, "Failed to pull image") {
				pulls = append(pulls, e.Message)
			}
		}
		if len(pulls) == 0 {
			t.Fatal("the node reported no image pull event for the missing tag")
		}
		last := strings.ToLower(pulls[len(pulls)-1])
		t.Logf("node pull answer: %s", pulls[len(pulls)-1])
		transport := false
		for _, marker := range []string{"dial tcp", "connection refused", "proxyconnect", "timeout", "no such host", "toomanyrequests", "tls"} {
			transport = transport || strings.Contains(last, marker)
		}
		switch {
		case transport:
			// The registry was not consulted: nothing proves the tag wrong.
			t.Log("observed: the node cannot reach a registry, so the correct class is ERROR")
			testutil.Equal(t, statuses(ev), map[string]string{"exists": "PASS", "ready": "ERROR"})
			testutil.Equal(t, ev.Score, (*int)(nil))
			if !strings.Contains(ev.Results[1].Reason, "not proven wrong") {
				t.Errorf("reason: %q", ev.Results[1].Reason)
			}
		case strings.Contains(last, "not found") || strings.Contains(last, "manifest unknown"):
			t.Log("observed: the registry answered not found, so the correct class is FAIL")
			testutil.Equal(t, statuses(ev), map[string]string{"exists": "PASS", "ready": "FAIL"})
			if !strings.Contains(ev.Results[1].Reason, "registry says the reference is wrong") {
				t.Errorf("reason: %q", ev.Results[1].Reason)
			}
		default:
			t.Fatalf("unrecognised pull answer, cannot decide the expected class: %s", last)
		}
	})

	t.Run("PullFailureOfProvidedImageIsError", func(t *testing.T) {
		// The same stuck pod is an environment fault, not the learner's, when
		// the image is one the environment promised to provide.
		provided := []grader.Option{grader.WithRedactor(redactor), grader.WithPreloadedImages(environment.ProbeImage.Name, missingTag)}
		e := grader.NewEngine(grader.DefaultCheckers(reader, provided...), provided...)
		crits := gradeCriteria("missing", 8*time.Second)
		ev, err := e.Evaluate(ctx, gradeTarget(id, []model.Criterion{crits[0], crits[4]}))
		testutil.NoError(t, err)
		testutil.Equal(t, statuses(ev), map[string]string{"exists": "PASS", "ready": "ERROR"})
		testutil.Equal(t, ev.Score, (*int)(nil))
		testutil.Equal(t, ev.Complete, false)
		if !strings.Contains(ev.Results[1].Reason, "image preparation failed") {
			t.Errorf("reason: %q", ev.Results[1].Reason)
		}
	})

	t.Run("UnconfiguredProvidedSetFailsClosed", func(t *testing.T) {
		// Composition that forgot WithPreloadedImages must never turn a pull
		// failure into a learner FAIL.
		forgot := []grader.Option{grader.WithRedactor(redactor)}
		e := grader.NewEngine(grader.DefaultCheckers(reader, forgot...), forgot...)
		crits := gradeCriteria("missing", 8*time.Second)
		ev, err := e.Evaluate(ctx, gradeTarget(id, []model.Criterion{crits[0], crits[4]}))
		testutil.NoError(t, err)
		testutil.Equal(t, statuses(ev), map[string]string{"exists": "PASS", "ready": "ERROR"})
		if !strings.Contains(ev.Results[1].Reason, "not configured") {
			t.Errorf("reason: %q", ev.Results[1].Reason)
		}
	})

	t.Run("UnschedulableIsError", func(t *testing.T) {
		// A Deployment that asks for more CPU than the node has: the pods
		// stay Pending for capacity, which the grader must not call wrong.
		apply(gradeDeployment("huge", environment.ProbeImage.Name, nil, 64))
		crits := gradeCriteria("huge", 6*time.Second)
		ev := grade([]model.Criterion{crits[0], crits[4]})
		testutil.Equal(t, statuses(ev), map[string]string{"exists": "PASS", "ready": "ERROR"})
		testutil.Equal(t, ev.Score, (*int)(nil))
		testutil.Equal(t, ev.Complete, false)
		if !strings.Contains(ev.Results[1].Reason, "Insufficient cpu") {
			t.Errorf("reason: %q", ev.Results[1].Reason)
		}
	})

	t.Run("ScaleUpWaitsForObservedRollout", func(t *testing.T) {
		// A new generation is applied right before grading: the readiness
		// check must see the controller observe it and the second replica
		// become ready, instead of trusting the old, ready status. (Small
		// CPU limits: the 1-CPU deployments above already fill the node.)
		scaleReady := func(replicas int) model.Evaluation {
			crits := gradeCriteria("scale", 90*time.Second)
			crits[4].Args = map[string]any{"replicas": replicas}
			return grade([]model.Criterion{crits[0], crits[4]})
		}
		apply(gradeDeployment("scale", environment.ProbeImage.Name, 1, "100m"))
		testutil.Equal(t, statuses(scaleReady(1)), map[string]string{"exists": "PASS", "ready": "PASS"})
		apply(gradeDeployment("scale", environment.ProbeImage.Name, 2, "100m"))
		ev := scaleReady(2)
		testutil.Equal(t, statuses(ev), map[string]string{"exists": "PASS", "ready": "PASS"})
		act, _ := ev.Results[1].Evidence.Actual.(map[string]any)
		testutil.Equal(t, act["readyReplicas"], int64(2))
		testutil.Equal(t, act["observedGeneration"], act["generation"])
		// The declared target is checked too: 2 replicas do not satisfy 3.
		testutil.Equal(t, statuses(scaleReady(3)), map[string]string{"exists": "PASS", "ready": "FAIL"})
	})

	t.Run("MissingTargetFailsDependents", func(t *testing.T) {
		ev := grade(gradeCriteria("ghost", 5*time.Second))
		testutil.Equal(t, statuses(ev), map[string]string{"exists": "FAIL", "image": "FAIL", "limits": "FAIL", "defaults": "FAIL", "ready": "FAIL"})
		testutil.Equal(t, *ev.Score, 0)
		testutil.Equal(t, ev.Complete, true)
		for _, r := range ev.Results[1:] {
			if !strings.HasPrefix(r.Reason, "prerequisite not satisfied") {
				t.Errorf("%s reason %q", r.CriterionID, r.Reason)
			}
		}
	})

	t.Run("SecretNeverAppearsInEvidence", func(t *testing.T) {
		const canary = "secret-canary-value"
		apply(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "creds"},
			"type": "Opaque", "stringData": map[string]any{"token": canary}})
		ref := model.ObjectRef{APIVersion: "v1", Kind: "Secret", Namespace: gradeNamespace, Name: "creds"}
		ev := grade([]model.Criterion{{ID: "token", Kind: model.CriterionField, Target: ref, Points: 3,
			Args: map[string]any{"fields": []any{
				map[string]any{"path": "data.token", "equals": canary},
				map[string]any{"path": "metadata.managedFields", "exists": true},
			}}}})
		testutil.Equal(t, statuses(ev), map[string]string{"token": "PASS"})
		raw, err := json.Marshal(ev)
		testutil.NoError(t, err)
		testutil.Equal(t, strings.Contains(string(raw), canary), false)
	})

	t.Run("UnreachableAPIIsErrorNotFail", func(t *testing.T) {
		// Same kubeconfig, but the server points at a closed port.
		kc, err := clientcmd.LoadFromFile(m.KubeconfigPath())
		testutil.NoError(t, err)
		for _, cluster := range kc.Clusters {
			cluster.Server = "https://127.0.0.1:1"
		}
		brokenPath := filepath.Join(t.TempDir(), "kubeconfig")
		testutil.NoError(t, clientcmd.WriteToFile(*kc, brokenPath))
		brokenReader, err := grader.NewClusterReader(brokenPath, id.Context)
		testutil.NoError(t, err)
		fast := []grader.Option{grader.WithPollInterval(50 * time.Millisecond)}
		e := grader.NewEngine(grader.DefaultCheckers(brokenReader, fast...), fast...)
		ev, err := e.Evaluate(ctx, gradeTarget(id, gradeCriteria("equiv", 5*time.Second)))
		testutil.NoError(t, err)
		testutil.Equal(t, ev.Score, (*int)(nil))
		testutil.Equal(t, ev.Complete, false)
		testutil.Equal(t, statuses(ev), map[string]string{"exists": "ERROR", "image": "ERROR", "limits": "ERROR", "defaults": "ERROR", "ready": "ERROR"})
		testutil.Equal(t, ev.MaxScore, 12)
	})
}
