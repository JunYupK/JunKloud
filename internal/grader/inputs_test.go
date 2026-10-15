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
	secret := func(pw string) map[string]any {
		return obj(t, fmt.Sprintf(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db","namespace":"jk-p1","uid":"sec-1"},"data":{"password":%q}}`,
			base64.StdEncoding.EncodeToString([]byte(pw))))
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
	testutil.Equal(t, reflect.DeepEqual(x, y), true) // the value is omitted, identity (uid) is kept
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
