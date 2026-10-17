package grader_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func inputsTarget(ws string, criteria ...model.Criterion) grader.Target {
	t := artifactTarget(ws, criteria...)
	return t
}

func capture(t testing.TB, cfg grader.ArtifactConfig, ws string, criteria ...model.Criterion) map[string]string {
	t.Helper()
	m, err := grader.NewArtifactChecker(cfg, grader.WithRedactor(grader.NewRedactor())).CaptureInputs(context.Background(), inputsTarget(ws, criteria...))
	testutil.NoError(t, err)
	return m
}

var inputKey = []byte("per-installation-random-key-0123456789")

func deployInput(t testing.TB, replicas int, image string) map[string]any {
	return obj(t, fmt.Sprintf(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"jk-p1","uid":"dep-1","generation":%d,
	 "resourceVersion":"100","annotations":{"deployment.kubernetes.io/revision":"1"}},
	 "spec":{"replicas":%d,"selector":{"matchLabels":{"app":"web"}},"template":{"spec":{"containers":[{"name":"web","image":%q}]}}},
	 "status":{"observedGeneration":%d,"readyReplicas":%d,"availableReplicas":%d}}`, replicas, replicas, image, replicas, replicas, replicas))
}

// TestInputChange: the fingerprint follows what the learner submitted and
// nothing the controllers write.
func TestInputChange(t *testing.T) {
	arch := "amd64"
	imageTools := func(id string) *fakeTools {
		return dockerTools(func(ref string) grader.ToolResult { return out(inspectJSON(id, arch, nil)) })
	}
	imageCriterion := model.Criterion{ID: "img", Kind: model.CriterionImage, Points: 1, Args: map[string]any{"reference": "shop:1"}}
	resourceCriterion := model.Criterion{ID: "res", Kind: model.CriterionReady, Points: 1, Target: deployRef}

	t.Run("a changed image changes the fingerprint", func(t *testing.T) {
		before := capture(t, grader.ArtifactConfig{Tools: imageTools("sha256:" + strings.Repeat("a", 64)), InputKey: inputKey}, "", imageCriterion)
		afterImageChanged := capture(t, grader.ArtifactConfig{Tools: imageTools("sha256:" + strings.Repeat("b", 64)), InputKey: inputKey}, "", imageCriterion)
		testutil.Equal(t, reflect.DeepEqual(before, afterImageChanged), false)
		same := capture(t, grader.ArtifactConfig{Tools: imageTools("sha256:" + strings.Repeat("a", 64)), InputKey: inputKey}, "", imageCriterion)
		testutil.Equal(t, reflect.DeepEqual(before, same), true)
		testutil.Equal(t, len(before), 1)
		testutil.Equal(t, before["image:shop:1"], "id=sha256:"+strings.Repeat("a", 64))
	})

	t.Run("status and controller churn do not", func(t *testing.T) {
		base := deployInput(t, 2, "nginx:1.27.0")
		churned := deployInput(t, 2, "nginx:1.27.0")
		churned["status"] = map[string]any{"observedGeneration": int64(9), "readyReplicas": int64(0), "conditions": []any{map[string]any{"type": "Progressing"}}}
		meta := churned["metadata"].(map[string]any)
		meta["resourceVersion"] = "99999"
		meta["annotations"] = map[string]any{"deployment.kubernetes.io/revision": "7"}
		meta["managedFields"] = []any{map[string]any{"manager": "kube-controller-manager"}}
		a := capture(t, grader.ArtifactConfig{Reader: newReader().put(deployRef, base), InputKey: inputKey}, "", resourceCriterion)
		b := capture(t, grader.ArtifactConfig{Reader: newReader().put(deployRef, churned), InputKey: inputKey}, "", resourceCriterion)
		testutil.Equal(t, reflect.DeepEqual(a, b), true)
	})

	t.Run("a changed spec does", func(t *testing.T) {
		a := capture(t, grader.ArtifactConfig{Reader: newReader().put(deployRef, deployInput(t, 2, "nginx:1.27.0")), InputKey: inputKey}, "", resourceCriterion)
		b := capture(t, grader.ArtifactConfig{Reader: newReader().put(deployRef, deployInput(t, 2, "nginx:1.28.0")), InputKey: inputKey}, "", resourceCriterion)
		testutil.Equal(t, reflect.DeepEqual(a, b), false)
		if !strings.Contains(a["object:apps/v1/Deployment/jk-p1/web"], "uid=dep-1;gen=2;") {
			t.Errorf("fingerprint does not carry uid and generation: %v", a)
		}
	})

	t.Run("a replaced object does", func(t *testing.T) {
		other := deployInput(t, 2, "nginx:1.27.0")
		other["metadata"].(map[string]any)["uid"] = "dep-2"
		a := capture(t, grader.ArtifactConfig{Reader: newReader().put(deployRef, deployInput(t, 2, "nginx:1.27.0"))}, "", resourceCriterion)
		b := capture(t, grader.ArtifactConfig{Reader: newReader().put(deployRef, other)}, "", resourceCriterion)
		testutil.Equal(t, reflect.DeepEqual(a, b), false)
	})

	t.Run("a pod's node assignment is not an input", func(t *testing.T) {
		pod := func(node string) map[string]any {
			return obj(t, fmt.Sprintf(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"app","namespace":"jk-p1","uid":"p1"},"spec":{"nodeName":%q,"containers":[{"name":"a","image":"i"}]}}`, node))
		}
		c := model.Criterion{ID: "r", Kind: model.CriterionResource, Points: 1, Target: refOf("v1", "Pod", "jk-p1", "app")}
		a := capture(t, grader.ArtifactConfig{Reader: newReader().put(c.Target, pod(""))}, "", c)
		b := capture(t, grader.ArtifactConfig{Reader: newReader().put(c.Target, pod("node-1"))}, "", c)
		testutil.Equal(t, reflect.DeepEqual(a, b), true)
	})

	t.Run("only objects named by the criteria are read", func(t *testing.T) {
		r := newReader().put(deployRef, deployInput(t, 1, "i")).put(refOf("v1", "ConfigMap", "jk-p1", "unrelated"), obj(t, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"unrelated","uid":"x"}}`))
		capture(t, grader.ArtifactConfig{Reader: r}, "", resourceCriterion)
		testutil.Equal(t, r.callsFor(refOf("v1", "ConfigMap", "jk-p1", "unrelated")), 0)
		testutil.Equal(t, len(r.listQuery), 0)
	})

	t.Run("an absent object is recorded, not an error", func(t *testing.T) {
		m := capture(t, grader.ArtifactConfig{Reader: newReader()}, "", resourceCriterion)
		testutil.Equal(t, m["object:apps/v1/Deployment/jk-p1/web"], "absent")
	})

	t.Run("an unreachable API is an error", func(t *testing.T) {
		r := newReader().put(deployRef, fmt.Errorf("etcd timeout"))
		_, err := grader.NewArtifactChecker(grader.ArtifactConfig{Reader: r}).CaptureInputs(context.Background(), inputsTarget("", resourceCriterion))
		testutil.Equal(t, err != nil, true)
	})

	t.Run("one object named by several criteria is one entry", func(t *testing.T) {
		second := model.Criterion{ID: "f", Kind: model.CriterionField, Points: 1, Target: deployRef, Args: map[string]any{"fields": []any{map[string]any{"path": "spec.replicas", "exists": true}}}}
		m := capture(t, grader.ArtifactConfig{Reader: newReader().put(deployRef, deployInput(t, 1, "i"))}, "", resourceCriterion, second)
		testutil.Equal(t, len(m), 1)
	})

	t.Run("deterministic", func(t *testing.T) {
		cfg := grader.ArtifactConfig{Reader: newReader().put(deployRef, deployInput(t, 1, "i")), Tools: imageTools("sha256:" + strings.Repeat("c", 64)), InputKey: inputKey}
		a := capture(t, cfg, "", resourceCriterion, imageCriterion)
		b := capture(t, cfg, "", imageCriterion, resourceCriterion)
		testutil.Equal(t, reflect.DeepEqual(a, b), true)
	})
}

func TestInputsOfFiles(t *testing.T) {
	c := fileCrit(map[string]any{"path": "answer.yaml"})
	cfg := grader.ArtifactConfig{InputKey: inputKey}
	a := capture(t, cfg, workspace(t, map[string]string{"answer.yaml": answerYAML}), c)
	same := capture(t, cfg, workspace(t, map[string]string{"answer.yaml": answerYAML}), c)
	edited := capture(t, cfg, workspace(t, map[string]string{"answer.yaml": answerYAMLWrongReplicas}), c)
	absent := capture(t, cfg, workspace(t, nil), c)
	testutil.Equal(t, reflect.DeepEqual(a, same), true)
	testutil.Equal(t, reflect.DeepEqual(a, edited), false)
	testutil.Equal(t, absent["file:answer.yaml"], "absent")
	// The digest is keyed: it is not the plain sha256 of the bytes, and another key gives another value.
	plain := sha256.Sum256([]byte(answerYAML))
	if strings.Contains(a["file:answer.yaml"], hex.EncodeToString(plain[:])) {
		t.Errorf("file digest is the plain sha256: %v", a)
	}
	other := capture(t, grader.ArtifactConfig{InputKey: []byte("another-installation-key-0123456789")}, workspace(t, map[string]string{"answer.yaml": answerYAML}), c)
	testutil.Equal(t, reflect.DeepEqual(a, other), false)

	t.Run("a symlink out of the workspace is recorded as such, never read", func(t *testing.T) {
		outside := t.TempDir()
		testutil.NoError(t, os.WriteFile(filepath.Join(outside, "x.yaml"), []byte("secret: yes\n"), 0o644))
		ws := workspace(t, nil)
		testutil.NoError(t, os.Symlink(filepath.Join(outside, "x.yaml"), filepath.Join(ws, "answer.yaml")))
		m := capture(t, cfg, ws, c)
		testutil.Equal(t, m["file:answer.yaml"], "outside-workspace")
	})
	t.Run("without a key the digest is plain but present", func(t *testing.T) {
		m := capture(t, grader.ArtifactConfig{}, workspace(t, map[string]string{"answer.yaml": answerYAML}), c)
		testutil.Equal(t, m["file:answer.yaml"], "sha256:"+hex.EncodeToString(plain[:]))
	})
	t.Run("archives are inputs too", func(t *testing.T) {
		ci := model.Criterion{ID: "i", Kind: model.CriterionImage, Points: 1, Args: map[string]any{"archive": map[string]any{"path": "shop.tar"}}}
		ws1, ws2 := workspace(t, map[string]string{"shop.tar": "one"}), workspace(t, map[string]string{"shop.tar": "two"})
		a, b := capture(t, cfg, ws1, ci), capture(t, cfg, ws2, ci)
		testutil.Equal(t, reflect.DeepEqual(a, b), false)
		testutil.Equal(t, len(a), 1)
	})
}

// Secrets: the value is covered (changing it changes the fingerprint) only
// through a keyed hash; without a key the value is omitted. A plain sha256
// of a low-entropy Secret would be a dictionary lookup.
func TestInputsKeepSecretsOut(t *testing.T) {
	ref := refOf("v1", "Secret", "jk-p1", "db")
	c := model.Criterion{ID: "s", Kind: model.CriterionField, Points: 1, Target: ref, Args: map[string]any{"fields": []any{map[string]any{"path": "data.password", "exists": true}}}}
	// resourceVersion follows the payload here, as it does for a Secret nobody but the learner writes.
	secret := func(pw string) map[string]any {
		return obj(t, fmt.Sprintf(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db","namespace":"jk-p1","uid":"sec-1","resourceVersion":"rv-%d"},"data":{"password":%q}}`,
			len(pw)*1000+int(pw[0]), base64.StdEncoding.EncodeToString([]byte(pw))))
	}
	leaks := func(m map[string]string, pw string) bool {
		plainSum := sha256.Sum256([]byte(pw))
		for _, v := range m {
			for _, needle := range []string{pw, base64.StdEncoding.EncodeToString([]byte(pw)), hex.EncodeToString(plainSum[:])} {
				if strings.Contains(v, needle) {
					return true
				}
			}
		}
		return false
	}
	pwA, pwB := "123456", "654321"
	withKey := func(pw string) map[string]string {
		return capture(t, grader.ArtifactConfig{Reader: newReader().put(ref, secret(pw)), InputKey: inputKey}, "", c)
	}
	a, b := withKey(pwA), withKey(pwB)
	testutil.Equal(t, reflect.DeepEqual(a, b), false)
	testutil.Equal(t, leaks(a, pwA), false)
	// Even the digest of the JSON the object hash is built from must not appear.
	noKey := func(pw string) map[string]string {
		return capture(t, grader.ArtifactConfig{Reader: newReader().put(ref, secret(pw))}, "", c)
	}
	x, y := noKey(pwA), noKey(pwB)
	testutil.Equal(t, leaks(x, pwA), false)
	// Controller ruling R22: no key never means "ignore the Secret". The value
	// is not hashed, but uid + resourceVersion still signal a change.
	testutil.Equal(t, reflect.DeepEqual(x, y), false)
	testutil.Equal(t, reflect.DeepEqual(x, noKey(pwA)), true)
	if !strings.Contains(x["object:v1/Secret/jk-p1/db"], "uid=sec-1") {
		t.Errorf("secret identity missing: %v", x)
	}
	// Another installation key gives another fingerprint for the same Secret.
	other := capture(t, grader.ArtifactConfig{Reader: newReader().put(ref, secret(pwA)), InputKey: []byte("another-installation-key-0123456789")}, "", c)
	testutil.Equal(t, reflect.DeepEqual(a, other), false)
}

func TestInputsOfHelmKustomizeAndRBAC(t *testing.T) {
	t.Run("helm: revision and keyed values", func(t *testing.T) {
		mk := func(rev int, values string) map[string]string {
			h := deployedFixture()
			h.status = func() grader.ToolResult { return out(helmStatusJSON("deployed", "web", "0.1.0", rev)) }
			h.values = values
			return capture(t, grader.ArtifactConfig{Tools: h.tools(), KubeconfigPath: "/k", Context: "c", InputKey: inputKey}, "", helmCrit(nil))
		}
		a := mk(1, `{"password":"hunter2"}`)
		testutil.Equal(t, reflect.DeepEqual(a, mk(1, `{"password":"hunter2"}`)), true)
		testutil.Equal(t, reflect.DeepEqual(a, mk(2, `{"password":"hunter2"}`)), false)
		testutil.Equal(t, reflect.DeepEqual(a, mk(1, `{"password":"hunter3"}`)), false)
		for _, v := range a {
			if strings.Contains(v, "hunter2") {
				t.Errorf("helm values leak: %v", a)
			}
		}
		// A status flip (failed after a bad upgrade) is not a change of the submission.
		h := deployedFixture()
		h.status = func() grader.ToolResult { return out(helmStatusJSON("failed", "web", "0.1.0", 1)) }
		h.values = `{"password":"hunter2"}`
		f := capture(t, grader.ArtifactConfig{Tools: h.tools(), KubeconfigPath: "/k", Context: "c", InputKey: inputKey}, "", helmCrit(nil))
		testutil.Equal(t, reflect.DeepEqual(a, f), true)
	})
	t.Run("helm: no release", func(t *testing.T) {
		h := deployedFixture()
		h.fail = map[string]grader.ToolResult{" list ": out("[]")}
		m := capture(t, grader.ArtifactConfig{Tools: h.tools(), KubeconfigPath: "/k", Context: "c"}, "", helmCrit(nil))
		testutil.Equal(t, m["helm:jk-p1/web"], "absent")
	})
	t.Run("kustomize: the overlay and the base trees", func(t *testing.T) {
		ws1, _ := kzWorkspace(t)
		ws2, _ := kzWorkspace(t)
		testutil.NoError(t, os.WriteFile(filepath.Join(ws2, "overlay", "kustomization.yaml"), []byte("resources:\n  - ../base\nreplicas:\n  - name: web\n    count: 9\n"), 0o644))
		c := kzCrit(nil)
		cfg := grader.ArtifactConfig{InputKey: inputKey}
		a, b := capture(t, cfg, ws1, c), capture(t, cfg, ws2, c)
		testutil.Equal(t, reflect.DeepEqual(a, b), false)
		testutil.Equal(t, a["kustomize:overlay"] != "", true)
		testutil.Equal(t, a["kustomize-base:base"] != "", true)
		testutil.Equal(t, a["kustomize-base:base"], b["kustomize-base:base"])
	})
	t.Run("rbac: bindings and roles that grant the service account", func(t *testing.T) {
		binding := func(role string, subjects string) map[string]any {
			return obj(t, fmt.Sprintf(`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"RoleBinding","metadata":{"name":"rb","namespace":"jk-p1","uid":"rb-1"},
			 "roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"Role","name":%q},"subjects":%s}`, role, subjects))
		}
		role := func(verbs string) map[string]any {
			return obj(t, fmt.Sprintf(`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"Role","metadata":{"name":"pod-reader","namespace":"jk-p1","uid":"role-1"},
			 "rules":[{"apiGroups":[""],"resources":["pods"],"verbs":%s}]}`, verbs))
		}
		mk := func(b, r map[string]any) map[string]string {
			rd := newReader().put(saRef, saDoc(t, "reader")).
				put(refOf("rbac.authorization.k8s.io/v1", "Role", "jk-p1", "pod-reader"), r).
				put(refOf("rbac.authorization.k8s.io/v1", "Role", "jk-p1", "other"), role(`["*"]`)).
				putList("RoleBinding", "jk-p1", b,
					obj(t, `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"RoleBinding","metadata":{"name":"unrelated","namespace":"jk-p1","uid":"rb-2"},"roleRef":{"kind":"Role","name":"other"},"subjects":[{"kind":"ServiceAccount","name":"someone-else","namespace":"jk-p1"}]}`)).
				putList("ClusterRoleBinding", "")
			return capture(t, grader.ArtifactConfig{Reader: rd, InputKey: inputKey}, "", authCrit(map[string]any{"checks": []any{check("get", "pods", true)}}))
		}
		subj := `[{"kind":"ServiceAccount","name":"reader","namespace":"jk-p1"}]`
		a := mk(binding("pod-reader", subj), role(`["get"]`))
		testutil.Equal(t, reflect.DeepEqual(a, mk(binding("pod-reader", subj), role(`["get"]`))), true)
		testutil.Equal(t, reflect.DeepEqual(a, mk(binding("pod-reader", subj), role(`["get","delete"]`))), false) // the role's rules changed
		testutil.Equal(t, reflect.DeepEqual(a, mk(binding("pod-reader", `[{"kind":"ServiceAccount","name":"reader","namespace":"jk-p1"},{"kind":"User","name":"x"}]`), role(`["get"]`))), false)
		for k := range a {
			if strings.Contains(k, "unrelated") || strings.Contains(k, "other") {
				t.Errorf("an unrelated binding or role was fingerprinted: %v", a)
			}
		}
	})
}

func metaOf2(o map[string]any) map[string]any { return o["metadata"].(map[string]any) }

// Labels and annotations the learner sets are part of the answer; the ones
// controllers and kubectl write are not.
func TestInputChangeSeesMetadataButNotControllerNoise(t *testing.T) {
	c := model.Criterion{ID: "r", Kind: model.CriterionResource, Points: 1, Target: deployRef}
	fp := func(mut func(meta map[string]any)) map[string]string {
		d := deployInput(t, 2, "nginx:1.27.0")
		mut(metaOf2(d))
		return capture(t, grader.ArtifactConfig{Reader: newReader().put(deployRef, d), InputKey: inputKey}, "", c)
	}
	base := fp(func(map[string]any) {})
	labeled := fp(func(m map[string]any) { m["labels"] = map[string]any{"tier": "frontend"} })
	relabeled := fp(func(m map[string]any) { m["labels"] = map[string]any{"tier": "backend"} })
	annotated := fp(func(m map[string]any) { m["annotations"] = map[string]any{"example.com/owner": "me"} })
	testutil.Equal(t, reflect.DeepEqual(base, labeled), false)
	testutil.Equal(t, reflect.DeepEqual(labeled, relabeled), false)
	testutil.Equal(t, reflect.DeepEqual(base, annotated), false)
	noise := fp(func(m map[string]any) {
		m["annotations"] = map[string]any{
			"deployment.kubernetes.io/revision":                "9",
			"kubectl.kubernetes.io/last-applied-configuration": "{...}",
			"autoscaling.alpha.kubernetes.io/conditions":       "[]",
			"endpoints.kubernetes.io/last-change-trigger-time": "2026-10-15T00:00:00Z",
			"pv.kubernetes.io/bind-completed":                  "yes",
		}
		m["labels"] = map[string]any{"pod-template-hash": "abc", "controller-uid": "u", "batch.kubernetes.io/job-name": "j", "kubernetes.io/metadata.name": "jk-p1"}
		m["resourceVersion"] = "424242"
		m["managedFields"] = []any{map[string]any{"manager": "x"}}
	})
	testutil.Equal(t, reflect.DeepEqual(base, noise), true)
}

func serviceFor(t testing.TB, selector string) map[string]any {
	return obj(t, fmt.Sprintf(`{"apiVersion":"v1","kind":"Service","metadata":{"name":"web","namespace":"jk-p1","uid":"svc-1"},"spec":{"selector":%s,"ports":[{"port":80}],"clusterIP":"10.0.0.1"}}`, selector))
}

func workload(t testing.TB, kind, name, image string, labels string, podExtra string) map[string]any {
	api := "apps/v1"
	return obj(t, fmt.Sprintf(`{"apiVersion":%q,"kind":%q,"metadata":{"name":%q,"namespace":"jk-p1","uid":"uid-%s","generation":1},
	 "spec":{"replicas":0,"selector":{"matchLabels":%s},"template":{"metadata":{"labels":%s},"spec":{"containers":[{"name":"c","image":%q}]%s}}},"status":{"readyReplicas":0}}`, api, kind, name, name, labels, labels, image, podExtra))
}

// TestInputChangeSeesTheBackingWorkloads: an http, tcp or network-policy
// criterion names a Service, but the learner's answer is the Deployment
// behind it (spec 9.1: a changed answer must not pass as a retry).
func TestInputChangeSeesTheBackingWorkloads(t *testing.T) {
	svcRef := refOf("v1", "Service", "jk-p1", "web")
	tcp := model.Criterion{ID: "t", Kind: model.CriterionTCP, Points: 1, Target: svcRef, Args: map[string]any{"port": 80,
		"source": map[string]any{"labels": map[string]any{"role": "x"}}}}
	mk := func(image string, pods ...map[string]any) map[string]string {
		r := newReader().put(svcRef, serviceFor(t, `{"app":"web"}`)).
			putList("Deployment", "jk-p1", workload(t, "Deployment", "web", image, `{"app":"web"}`, ""),
				workload(t, "Deployment", "other", "other:1", `{"app":"other"}`, "")).
			putList("StatefulSet", "jk-p1").putList("DaemonSet", "jk-p1").putList("ReplicaSet", "jk-p1").putList("Job", "jk-p1").putList("Pod", "jk-p1", pods...).
			putList("NetworkPolicy", "jk-p1")
		return capture(t, grader.ArtifactConfig{Reader: r, InputKey: inputKey}, "", tcp)
	}
	a := mk("nginx:1.27.0")
	testutil.Equal(t, reflect.DeepEqual(a, mk("nginx:1.27.0")), true)
	testutil.Equal(t, reflect.DeepEqual(a, mk("nginx:1.28.0")), false)
	if _, ok := a["object:apps/v1/Deployment/jk-p1/web"]; !ok {
		t.Fatalf("the Deployment behind the Service is not fingerprinted: %v", a)
	}
	if _, ok := a["object:apps/v1/Deployment/jk-p1/other"]; ok {
		t.Fatalf("a Deployment the Service does not select was fingerprinted: %v", a)
	}
	// Scaled to zero replicas it is still found (no pods needed).
	for k, v := range a {
		if strings.HasPrefix(k, "object:apps/v1/Deployment/jk-p1/web") && v == "absent" {
			t.Fatalf("%s absent", k)
		}
	}
	// A bare pod behind the service counts too, but its controller-written labels do not.
	pod := func(hash string) map[string]any {
		return obj(t, fmt.Sprintf(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"solo","namespace":"jk-p1","uid":"p","labels":{"app":"web","pod-template-hash":%q}},"spec":{"containers":[{"name":"c","image":"i"}]}}`, hash))
	}
	b1, b2 := mk("nginx:1.27.0", pod("h1")), mk("nginx:1.27.0", pod("h2"))
	testutil.Equal(t, reflect.DeepEqual(b1, b2), true)
	if _, ok := b1["object:v1/Pod/jk-p1/solo"]; !ok {
		t.Fatalf("the bare pod behind the Service is not fingerprinted: %v", b1)
	}
	testutil.Equal(t, reflect.DeepEqual(a, b1), false)
}

func TestInputChangeFollowsIngressAndReferencedConfig(t *testing.T) {
	ingRef := refOf("networking.k8s.io/v1", "Ingress", "jk-p1", "site")
	ing := obj(t, `{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"name":"site","namespace":"jk-p1","uid":"ing-1"},
	 "spec":{"rules":[{"host":"a.example","http":{"paths":[{"path":"/","pathType":"Prefix","backend":{"service":{"name":"web","port":{"number":80}}}}]}}]}}`)
	crit := model.Criterion{ID: "h", Kind: model.CriterionHTTP, Points: 1, Target: ingRef, Args: map[string]any{"body_marker": "x", "source": map[string]any{}}}
	cmRef := refOf("v1", "ConfigMap", "jk-p1", "app-config")
	mk := func(image, cmValue string) map[string]string {
		dep := workload(t, "Deployment", "web", image, `{"app":"web"}`,
			`,"volumes":[{"name":"v","configMap":{"name":"app-config"}}]`)
		dep["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["envFrom"] =
			[]any{map[string]any{"secretRef": map[string]any{"name": "db"}}}
		r := newReader().put(ingRef, ing).put(refOf("v1", "Service", "jk-p1", "web"), serviceFor(t, `{"app":"web"}`)).
			put(cmRef, obj(t, fmt.Sprintf(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"app-config","namespace":"jk-p1","uid":"cm-1"},"data":{"k":%q}}`, cmValue))).
			put(refOf("v1", "Secret", "jk-p1", "db"), obj(t, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db","namespace":"jk-p1","uid":"s","resourceVersion":"1"},"data":{"p":"cA=="}}`)).
			putList("Deployment", "jk-p1", dep).
			putList("StatefulSet", "jk-p1").putList("DaemonSet", "jk-p1").putList("ReplicaSet", "jk-p1").putList("Job", "jk-p1").putList("Pod", "jk-p1").putList("NetworkPolicy", "jk-p1")
		return capture(t, grader.ArtifactConfig{Reader: r, InputKey: inputKey}, "", crit)
	}
	a := mk("nginx:1", "one")
	for _, want := range []string{"object:networking.k8s.io/v1/Ingress/jk-p1/site", "object:v1/Service/jk-p1/web", "object:apps/v1/Deployment/jk-p1/web",
		"object:v1/ConfigMap/jk-p1/app-config", "object:v1/Secret/jk-p1/db"} {
		if _, ok := a[want]; !ok {
			t.Errorf("%s is not fingerprinted: %v", want, a)
		}
	}
	testutil.Equal(t, reflect.DeepEqual(a, mk("nginx:1", "one")), true)
	testutil.Equal(t, reflect.DeepEqual(a, mk("nginx:2", "one")), false) // the backing Deployment
	testutil.Equal(t, reflect.DeepEqual(a, mk("nginx:1", "two")), false) // the ConfigMap it mounts
}

func TestInputChangeSeesNetworkPoliciesOfTheInvolvedNamespaces(t *testing.T) {
	svcRef := refOf("v1", "Service", "jk-p1", "web")
	crit := model.Criterion{ID: "n", Kind: model.CriterionNetworkPolicy, Points: 1, Target: svcRef, Args: map[string]any{"expectations": []any{
		map[string]any{"expect": "allow", "port": 80, "source": map[string]any{"namespace": "jk-other"}},
		map[string]any{"expect": "deny", "port": 80, "source": map[string]any{"namespace": "jk-third"}}}}}
	policy := func(ns, name, from string) map[string]any {
		return obj(t, fmt.Sprintf(`{"apiVersion":"networking.k8s.io/v1","kind":"NetworkPolicy","metadata":{"name":%q,"namespace":%q,"uid":"np-%s"},"spec":{"podSelector":{},"ingress":[{"from":[{"podSelector":{"matchLabels":{"role":%q}}}]}]}}`, name, ns, name, from))
	}
	mk := func(p1, p2 map[string]any) map[string]string {
		r := newReader().put(svcRef, serviceFor(t, `{"app":"web"}`)).
			putList("Deployment", "jk-p1").putList("StatefulSet", "jk-p1").putList("DaemonSet", "jk-p1").putList("ReplicaSet", "jk-p1").putList("Job", "jk-p1").putList("Pod", "jk-p1").
			putList("NetworkPolicy", "jk-p1", p1).putList("NetworkPolicy", "jk-other", p2).putList("NetworkPolicy", "jk-third")
		return capture(t, grader.ArtifactConfig{Reader: r, InputKey: inputKey}, "", crit)
	}
	a := mk(policy("jk-p1", "web-in", "frontend"), policy("jk-other", "egress", "x"))
	testutil.Equal(t, reflect.DeepEqual(a, mk(policy("jk-p1", "web-in", "frontend"), policy("jk-other", "egress", "x"))), true)
	testutil.Equal(t, reflect.DeepEqual(a, mk(policy("jk-p1", "web-in", "intruder"), policy("jk-other", "egress", "x"))), false)
	testutil.Equal(t, reflect.DeepEqual(a, mk(policy("jk-p1", "web-in", "frontend"), policy("jk-other", "egress", "y"))), false)
}

// TestInputsCoverPullCredentialsAndRefuseTooMany: the Secrets a pod pulls its
// images with are part of the answer (a changed pull credential can turn
// ImagePullBackOff into a running pod), so are references from ephemeral
// containers and the deprecated serviceAccount field; and a pod spec naming
// more objects than the grader fingerprints makes the answer unprovable, it
// never drops the rest silently (Codex review P1/P2 on PR #2).
func TestInputsCoverPullCredentialsAndRefuseTooMany(t *testing.T) {
	ready := model.Criterion{ID: "r", Kind: model.CriterionReady, Points: 1, Target: deployRef}
	secret := func(name, rv string) map[string]any {
		return obj(t, fmt.Sprintf(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":%q,"namespace":"jk-p1","uid":"sec-%s","resourceVersion":%q},
		  "type":"kubernetes.io/dockerconfigjson","data":{".dockerconfigjson":"e30="}}`, name, name, rv))
	}
	withPodSpec := func(podSpec string) map[string]any {
		return obj(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"jk-p1","uid":"dep-1","generation":1},
		  "spec":{"replicas":1,"selector":{"matchLabels":{"app":"web"}},"template":{"metadata":{"labels":{"app":"web"}},"spec":`+podSpec+`}}}`)
	}
	t.Run("imagePullSecrets of the pod spec", func(t *testing.T) {
		dep := withPodSpec(`{"imagePullSecrets":[{"name":"regcred"}],"containers":[{"name":"c","image":"registry.example/app:1"}]}`)
		mk := func(rv string) map[string]string {
			r := newReader().put(deployRef, dep).put(refOf("v1", "Secret", "jk-p1", "regcred"), secret("regcred", rv))
			return capture(t, grader.ArtifactConfig{Reader: r}, "", ready)
		}
		a := mk("1")
		if _, ok := a["object:v1/Secret/jk-p1/regcred"]; !ok {
			t.Fatalf("the pull Secret is not fingerprinted: %v", a)
		}
		testutil.Equal(t, reflect.DeepEqual(a, mk("1")), true)
		testutil.Equal(t, reflect.DeepEqual(a, mk("2")), false) // the credential was changed
		// With a key the payload itself is covered (keyed), as for every Secret.
		withKey := capture(t, grader.ArtifactConfig{Reader: newReader().put(deployRef, dep).put(refOf("v1", "Secret", "jk-p1", "regcred"), secret("regcred", "1")), InputKey: inputKey}, "", ready)
		if !strings.Contains(withKey["object:v1/Secret/jk-p1/regcred"], "mac:") {
			t.Errorf("pull Secret not keyed like other Secrets: %v", withKey)
		}
	})
	t.Run("imagePullSecrets of the pod's ServiceAccount", func(t *testing.T) {
		dep := withPodSpec(`{"serviceAccountName":"puller","containers":[{"name":"c","image":"registry.example/app:1"}]}`)
		mk := func(rv string) map[string]string {
			r := newReader().put(deployRef, dep).
				put(refOf("v1", "ServiceAccount", "jk-p1", "puller"), obj(t, `{"apiVersion":"v1","kind":"ServiceAccount","metadata":{"name":"puller","namespace":"jk-p1","uid":"sa-1"},"imagePullSecrets":[{"name":"sa-cred"}]}`)).
				put(refOf("v1", "Secret", "jk-p1", "sa-cred"), secret("sa-cred", rv))
			return capture(t, grader.ArtifactConfig{Reader: r}, "", ready)
		}
		a := mk("1")
		for _, want := range []string{"object:v1/ServiceAccount/jk-p1/puller", "object:v1/Secret/jk-p1/sa-cred"} {
			if _, ok := a[want]; !ok {
				t.Fatalf("%s is not fingerprinted: %v", want, a)
			}
		}
		testutil.Equal(t, reflect.DeepEqual(a, mk("2")), false)
	})
	t.Run("the deprecated serviceAccount field", func(t *testing.T) {
		dep := withPodSpec(`{"serviceAccount":"legacy","containers":[{"name":"c","image":"i"}]}`)
		r := newReader().put(deployRef, dep).put(refOf("v1", "ServiceAccount", "jk-p1", "legacy"), obj(t, `{"apiVersion":"v1","kind":"ServiceAccount","metadata":{"name":"legacy","namespace":"jk-p1","uid":"sa-2"}}`))
		a := capture(t, grader.ArtifactConfig{Reader: r}, "", ready)
		if v := a["object:v1/ServiceAccount/jk-p1/legacy"]; v == "" || v == "absent" {
			t.Fatalf("serviceAccount is not followed: %v", a)
		}
	})
	t.Run("ephemeral containers", func(t *testing.T) {
		podRef := refOf("v1", "Pod", "jk-p1", "solo")
		p := obj(t, `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"solo","namespace":"jk-p1","uid":"p-1"},
		  "spec":{"containers":[{"name":"c","image":"i"}],
		  "ephemeralContainers":[{"name":"dbg","image":"busybox:1.37.0","envFrom":[{"secretRef":{"name":"dbg-env"}}],"env":[{"name":"X","valueFrom":{"configMapKeyRef":{"name":"dbg-cm","key":"k"}}}]}]}}`)
		c := model.Criterion{ID: "r", Kind: model.CriterionResource, Points: 1, Target: podRef}
		a := capture(t, grader.ArtifactConfig{Reader: newReader().put(podRef, p)}, "", c)
		for _, want := range []string{"object:v1/Secret/jk-p1/dbg-env", "object:v1/ConfigMap/jk-p1/dbg-cm"} {
			if _, ok := a[want]; !ok {
				t.Fatalf("%s is not fingerprinted: %v", want, a)
			}
		}
	})
	t.Run("more references than the limit make the answer unprovable", func(t *testing.T) {
		var env []string
		for i := 0; i < 31; i++ {
			env = append(env, fmt.Sprintf(`{"name":"V%d","valueFrom":{"configMapKeyRef":{"name":"cm-%02d","key":"k"}}}`, i, i))
		}
		dep := withPodSpec(`{"containers":[{"name":"c","image":"i","env":[` + strings.Join(env, ",") + `]}]}`)
		_, err := grader.NewArtifactChecker(grader.ArtifactConfig{Reader: newReader().put(deployRef, dep)}).CaptureInputs(context.Background(), inputsTarget("", ready))
		testutil.ErrorIs(t, err, grader.ErrInsufficientEvidence)
		if !strings.Contains(err.Error(), "30") {
			t.Fatalf("error should name the limit: %v", err)
		}
		// Exactly the limit is still fine.
		dep = withPodSpec(`{"containers":[{"name":"c","image":"i","env":[` + strings.Join(env[:30], ",") + `]}]}`)
		_, err = grader.NewArtifactChecker(grader.ArtifactConfig{Reader: newReader().put(deployRef, dep)}).CaptureInputs(context.Background(), inputsTarget("", ready))
		testutil.NoError(t, err)
	})
}
