package grader_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
)

type volumeFixture struct {
	r                                       *fakeReader
	pod, deployment, pvc, pv, namespace, rs map[string]any
	deleted, writes, cleanups               int
	marker                                  string
	noReplacement, unreadable, cleanupError bool
	lost                                    bool
	writeNoEffect                           bool
	resolved                                string
	tokens                                  []string
	onWrite                                 func()
}

func volumeCriterion() model.Criterion {
	return model.Criterion{ID: "persist", Kind: "volume-persistence", Points: 1, Timeout: 120 * time.Second, Target: model.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "jk-p1", Name: "app"}, Args: map[string]any{"container": "app", "path": "/data", "claim": "data"}}
}
func volumeObject(kind, name, uid string) map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": kind, "metadata": map[string]any{"name": name, "namespace": "jk-p1", "uid": uid}}
}
func volumeOwner(o map[string]any, kind, name, uid string) {
	o["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"apiVersion": "apps/v1", "kind": kind, "name": name, "uid": uid, "controller": true}}
}
func newVolumeFixture(t *testing.T) *volumeFixture {
	f := &volumeFixture{r: newReader()}
	f.namespace = volumeObject("Namespace", "jk-p1", "ns-uid")
	f.namespace["metadata"].(map[string]any)["labels"] = map[string]any{"junkloud.io/environment-id": "env", "junkloud.io/session-id": "session", "junkloud.io/problem-id": "p1"}
	spec := map[string]any{"containers": []any{map[string]any{"name": "app", "volumeMounts": []any{map[string]any{"name": "storage", "mountPath": "/data"}}}}, "volumes": []any{map[string]any{"name": "storage", "persistentVolumeClaim": map[string]any{"claimName": "data"}}}}
	f.deployment = volumeObject("Deployment", "app", "dep-uid")
	f.deployment["apiVersion"] = "apps/v1"
	f.deployment["spec"] = map[string]any{"replicas": 1, "selector": map[string]any{"matchLabels": map[string]any{"app": "app"}}, "template": map[string]any{"spec": spec}}
	f.rs = volumeObject("ReplicaSet", "app-rs", "rs-uid")
	f.rs["apiVersion"] = "apps/v1"
	f.rs["spec"] = map[string]any{"template": map[string]any{"spec": spec}}
	volumeOwner(f.rs, "Deployment", "app", "dep-uid")
	f.pod = volumeObject("Pod", "app-old", "old-uid")
	f.pod["spec"] = spec
	volumeOwner(f.pod, "ReplicaSet", "app-rs", "rs-uid")
	f.pod["status"] = map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}, "containerStatuses": []any{map[string]any{"name": "app", "ready": true, "state": map[string]any{"running": map[string]any{}}}}}
	f.pvc = volumeObject("PersistentVolumeClaim", "data", "pvc-uid")
	f.pvc["spec"] = map[string]any{"volumeName": "pv-data"}
	f.pvc["status"] = map[string]any{"phase": "Bound"}
	f.pv = volumeObject("PersistentVolume", "pv-data", "pv-uid")
	f.pv["spec"] = map[string]any{"claimRef": map[string]any{"namespace": "jk-p1", "name": "data", "uid": "pvc-uid"}}
	f.pv["status"] = map[string]any{"phase": "Bound"}
	f.r.put(model.ObjectRef{APIVersion: "v1", Kind: "PersistentVolume", Name: "pv-data"}, f.pv)
	f.r.put(model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: "jk-p1"}, f.namespace).put(volumeCriterion().Target, f.deployment).put(model.ObjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Namespace: "jk-p1", Name: "data"}, f.pvc)
	f.r.putList("ReplicaSet", "jk-p1", f.rs)
	f.r.putListFn("Pod", "jk-p1", func(int) []map[string]any { return []map[string]any{f.pod} })
	return f
}
func (f *volumeFixture) Logs(context.Context, string, string, string, grader.LogOptions) (string, error) {
	return "", nil
}
func (f *volumeFixture) Exec(ctx context.Context, ns, pod, container string, cmd []string) (grader.ExecResult, error) {
	if err := ctx.Err(); err != nil {
		return grader.ExecResult{}, err
	}
	script := strings.Join(cmd, " ")
	switch {
	case strings.Contains(script, "junkloud-volume-resolve"):
		if f.resolved != "" {
			return grader.ExecResult{Stdout: f.resolved}, nil
		}
		return grader.ExecResult{Stdout: "/data\n/data\n"}, nil
	case strings.Contains(script, "junkloud-volume-write"):
		f.writes++
		if !f.writeNoEffect {
			f.marker = cmd[len(cmd)-1]
		}
		f.tokens = append(f.tokens, f.marker)
		if f.onWrite != nil {
			f.onWrite()
		}
		return grader.ExecResult{}, nil
	case strings.Contains(script, "junkloud-volume-read"):
		if f.unreadable && f.deleted > 0 {
			return grader.ExecResult{ExitCode: 2}, nil
		}
		if f.lost && f.deleted > 0 {
			return grader.ExecResult{ExitCode: 3}, nil
		}
		return grader.ExecResult{Stdout: f.marker}, nil
	case strings.Contains(script, "junkloud-volume-cleanup"):
		f.cleanups++
		if f.cleanupError {
			return grader.ExecResult{ExitCode: 2}, nil
		}
		f.marker = ""
		return grader.ExecResult{}, nil
	}
	return grader.ExecResult{}, errors.New("unexpected exec")
}
func (f *volumeFixture) DeletePodUID(ctx context.Context, ns, name, uid string) error {
	if uid != fmt.Sprint(f.pod["metadata"].(map[string]any)["uid"]) {
		return errors.New("UID precondition missing")
	}
	f.deleted++
	if !f.noReplacement {
		f.pod = cloneVolumeObject(f.pod)
		f.pod["metadata"].(map[string]any)["name"] = "app-new"
		f.pod["metadata"].(map[string]any)["uid"] = "new-uid"
	}
	return nil
}
func volumeTarget(c model.Criterion) grader.Target {
	tgt := grader.Target{Identity: model.Identity{ID: "env"}, Problem: model.FrozenProblem{Definition: model.Problem{ID: "p1", Namespace: "jk-p1", Criteria: []model.Criterion{c}}}}
	tgt.SessionID = "session"
	return tgt
}
func volumeRun(t *testing.T, f *volumeFixture, c model.Criterion, ctx context.Context) model.CheckResult {
	ck := grader.NewArtifactChecker(grader.ArtifactConfig{Reader: f.r, Pods: f}, grader.WithClock(newClock()))
	checks := grader.DefaultCheckers(f.r)
	grader.AddArtifactCheckers(checks, ck)
	ev, err := grader.NewEngine(checks).Evaluate(ctx, volumeTarget(c))
	if err != nil {
		t.Fatal(err)
	}
	return ev.Results[0]
}
func TestVolumePersistence(t *testing.T) {
	t.Run("fresh marker survives owned replacement and is cleaned", func(t *testing.T) {
		f := newVolumeFixture(t)
		r := volumeRun(t, f, volumeCriterion(), context.Background())
		if r.Status != model.StatusPass || f.deleted != 1 || f.writes != 1 || f.cleanups != 1 {
			t.Fatalf("status=%s reason=%s delete=%d write=%d cleanup=%d", r.Status, r.Reason, f.deleted, f.writes, f.cleanups)
		}
		if !strings.Contains(fmt.Sprint(r.Evidence.Actual), "old-uid") || !strings.Contains(fmt.Sprint(r.Evidence.Actual), "new-uid") {
			t.Fatalf("missing UID evidence: %+v", r.Evidence)
		}
	})
	for _, kind := range []string{"emptydir", "wrong claim", "wrong container"} {
		t.Run(kind, func(t *testing.T) {
			f := newVolumeFixture(t)
			c := volumeCriterion()
			switch kind {
			case "emptydir":
				f.pod["spec"].(map[string]any)["volumes"] = []any{map[string]any{"name": "storage", "emptyDir": map[string]any{}}}
			case "wrong claim":
				c.Args["claim"] = "other"
			case "wrong container":
				c.Args["container"] = "other"
			}
			r := volumeRun(t, f, c, context.Background())
			if r.Status != model.StatusFail || f.deleted != 0 || f.writes != 0 {
				t.Fatalf("%s: %+v effects=%d/%d", kind, r, f.deleted, f.writes)
			}
		})
	}
	for _, kind := range []string{"unknown args", "foreign session", "foreign RS", "replaced namespace", "replaced controller", "replaced PVC", "replaced pod", "escaping subpath", "symlink", "readonly"} {
		t.Run(kind, func(t *testing.T) {
			f := newVolumeFixture(t)
			c := volumeCriterion()
			switch kind {
			case "unknown args":
				c.Args["unknown"] = true
			case "foreign session":
				f.namespace["metadata"].(map[string]any)["labels"].(map[string]any)["junkloud.io/session-id"] = "foreign"
			case "foreign RS":
				volumeOwner(f.rs, "Deployment", "app", "foreign")
			case "replaced namespace":
				f.onWrite = func() { f.namespace["metadata"].(map[string]any)["uid"] = "new" }
			case "replaced controller":
				f.onWrite = func() { f.deployment["metadata"].(map[string]any)["uid"] = "new" }
			case "replaced PVC":
				f.onWrite = func() { f.pvc["metadata"].(map[string]any)["uid"] = "new" }
			case "replaced pod":
				f.onWrite = func() { f.pod["metadata"].(map[string]any)["uid"] = "new" }
			case "escaping subpath":
				f.pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["volumeMounts"].([]any)[0].(map[string]any)["subPath"] = "../escape"
			case "symlink":
				c.Args["path"] = "/data/../etc"
			case "readonly":
				f.pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["volumeMounts"].([]any)[0].(map[string]any)["readOnly"] = true
			}
			r := volumeRun(t, f, c, context.Background())
			if r.Status != model.StatusError && !(kind == "readonly" && r.Status == model.StatusFail) {
				t.Fatalf("%s: %+v", kind, r)
			}
			if f.deleted != 0 {
				t.Fatalf("unsafe deletion %s", kind)
			}
		})
	}
	t.Run("no replacement is error", func(t *testing.T) {
		f := newVolumeFixture(t)
		f.noReplacement = true
		r := volumeRun(t, f, volumeCriterion(), context.Background())
		if r.Status != model.StatusError || f.deleted != 1 {
			t.Fatalf("%+v deletes=%d", r, f.deleted)
		}
	})
	t.Run("unreadable is error", func(t *testing.T) {
		f := newVolumeFixture(t)
		f.unreadable = true
		r := volumeRun(t, f, volumeCriterion(), context.Background())
		if r.Status != model.StatusError || f.deleted != 1 || f.cleanups != 1 {
			t.Fatalf("%+v deletes=%d cleanup=%d", r, f.deleted, f.cleanups)
		}
	})
	t.Run("cleanup failure is error", func(t *testing.T) {
		f := newVolumeFixture(t)
		f.cleanupError = true
		r := volumeRun(t, f, volumeCriterion(), context.Background())
		if r.Status != model.StatusError || f.deleted != 1 {
			t.Fatalf("%+v deletes=%d", r, f.deleted)
		}
	})
	t.Run("cancellation cleans marker without deleting", func(t *testing.T) {
		f := newVolumeFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		f.onWrite = cancel
		ck := grader.NewArtifactChecker(grader.ArtifactConfig{Reader: f.r, Pods: f}, grader.WithClock(newClock()))
		_, err := ck.Check(ctx, volumeTarget(volumeCriterion()), volumeCriterion())
		if err == nil || f.deleted != 0 || f.cleanups != 1 {
			t.Fatalf("err=%v deleted=%d cleanup=%d", err, f.deleted, f.cleanups)
		}
	})
}
func TestVolumePersistenceInputs(t *testing.T) {
	f := newVolumeFixture(t)
	c := volumeCriterion()
	ck := grader.NewArtifactChecker(grader.ArtifactConfig{Reader: f.r, Pods: f}, grader.WithClock(newClock()))
	target := volumeTarget(c)
	a, err := ck.CaptureInputs(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) < 2 {
		t.Fatalf("controller and PVC missing: %v", a)
	}
	r := volumeRun(t, f, c, context.Background())
	if r.Status != model.StatusPass {
		t.Fatalf("%+v", r)
	}
	b, err := ck.CaptureInputs(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("intentional replacement changed input: %v %v", a, b)
	}
	f.pvc["spec"].(map[string]any)["volumeName"] = "new-binding"
	d, err := ck.CaptureInputs(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(a, d) {
		t.Fatal("PVC rebinding ignored")
	}
}

func cloneVolumeObject(o map[string]any) map[string]any {
	b, _ := json.Marshal(o)
	var c map[string]any
	_ = json.Unmarshal(b, &c)
	return c
}

func TestVolumeCriterionModel(t *testing.T) {
	found := false
	for _, kind := range model.CriterionKinds() {
		if kind == "volume-persistence" {
			found = true
		}
	}
	if !found {
		t.Fatal("volume-persistence absent from model/schema contract")
	}
}

func TestVolumePersistenceSafety(t *testing.T) {
	for _, resolved := range []string{"/tmp\n/tmp\n", "/data\n/etc\n"} {
		t.Run("symlink resolution "+resolved, func(t *testing.T) {
			f := newVolumeFixture(t)
			f.resolved = resolved
			r := volumeRun(t, f, volumeCriterion(), context.Background())
			if r.Status != model.StatusError || f.writes != 0 || f.deleted != 0 {
				t.Fatalf("%+v writes=%d deletes=%d", r, f.writes, f.deleted)
			}
		})
	}
	t.Run("unknown arguments cause zero reads and writes", func(t *testing.T) {
		f := newVolumeFixture(t)
		c := volumeCriterion()
		c.Args["mutates"] = true
		r := volumeRun(t, f, c, context.Background())
		if r.Status != model.StatusError || len(f.r.calls) != 0 || len(f.r.listCalls) != 0 || f.writes != 0 || f.deleted != 0 {
			t.Fatalf("%+v calls=%v", r, f.r.calls)
		}
	})
	t.Run("missing expected session causes zero effects", func(t *testing.T) {
		f := newVolumeFixture(t)
		target := volumeTarget(volumeCriterion())
		target.SessionID = ""
		ck := grader.NewArtifactChecker(grader.ArtifactConfig{Reader: f.r, Pods: f})
		_, err := ck.Check(context.Background(), target, volumeCriterion())
		if err == nil || f.writes != 0 || f.deleted != 0 {
			t.Fatalf("error=%v effects=%d/%d", err, f.writes, f.deleted)
		}
	})
	t.Run("actual missing marker after replacement is fail", func(t *testing.T) {
		f := newVolumeFixture(t)
		f.lost = true
		r := volumeRun(t, f, volumeCriterion(), context.Background())
		if r.Status != model.StatusFail || f.deleted != 1 || f.cleanups != 1 {
			t.Fatalf("%+v effects=%d cleanup=%d", r, f.deleted, f.cleanups)
		}
	})
	t.Run("markers are unpredictable and absent from evidence", func(t *testing.T) {
		f := newVolumeFixture(t)
		r := volumeRun(t, f, volumeCriterion(), context.Background())
		g := newVolumeFixture(t)
		other := volumeRun(t, g, volumeCriterion(), context.Background())
		if r.Status != model.StatusPass || other.Status != model.StatusPass || len(f.tokens) != 1 || f.tokens[0] == g.tokens[0] || strings.Contains(fmt.Sprint(r), f.tokens[0]) {
			t.Fatalf("markers/evidence invalid: %+v %+v", r, other)
		}
	})
	t.Run("safe subpath stays inside named PVC", func(t *testing.T) {
		f := newVolumeFixture(t)
		f.pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["volumeMounts"].([]any)[0].(map[string]any)["subPath"] = "safe/child"
		r := volumeRun(t, f, volumeCriterion(), context.Background())
		if r.Status != model.StatusPass {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("controller mount and workspace changes remain inputs", func(t *testing.T) {
		f := newVolumeFixture(t)
		c := volumeCriterion()
		target := volumeTarget(c)
		ck := grader.NewArtifactChecker(grader.ArtifactConfig{Reader: f.r})
		a, err := ck.CaptureInputs(context.Background(), target)
		if err != nil {
			t.Fatal(err)
		}
		f.deployment["spec"].(map[string]any)["replicas"] = 2
		b, err := ck.CaptureInputs(context.Background(), target)
		if err != nil {
			t.Fatal(err)
		}
		if reflect.DeepEqual(a, b) {
			t.Fatal("controller spec ignored")
		}
		f.pvc["metadata"].(map[string]any)["uid"] = "new"
		d, err := ck.CaptureInputs(context.Background(), target)
		if err != nil {
			t.Fatal(err)
		}
		if reflect.DeepEqual(b, d) {
			t.Fatal("claim identity ignored")
		}
	})
}

func TestVolumePersistenceRejectsUnprovedWriteAndControllerKind(t *testing.T) {
	t.Run("write must be readable before deletion", func(t *testing.T) {
		f := newVolumeFixture(t)
		f.writeNoEffect = true
		r := volumeRun(t, f, volumeCriterion(), context.Background())
		if r.Status != model.StatusError || f.deleted != 0 {
			t.Fatalf("%+v deleted=%d", r, f.deleted)
		}
	})
	t.Run("owner reference kind is part of chain", func(t *testing.T) {
		f := newVolumeFixture(t)
		volumeOwner(f.pod, "StatefulSet", "app-rs", "rs-uid")
		r := volumeRun(t, f, volumeCriterion(), context.Background())
		if r.Status != model.StatusError || f.deleted != 0 || f.writes != 0 {
			t.Fatalf("%+v deletes=%d writes=%d", r, f.deleted, f.writes)
		}
	})
}

func TestVolumePersistenceBindingProof(t *testing.T) {
	t.Run("foreign PV binding refuses deletion", func(t *testing.T) {
		f := newVolumeFixture(t)
		f.pv["spec"].(map[string]any)["claimRef"].(map[string]any)["uid"] = "foreign"
		r := volumeRun(t, f, volumeCriterion(), context.Background())
		if r.Status != model.StatusError || f.writes != 0 || f.deleted != 0 {
			t.Fatalf("%+v effects=%d/%d", r, f.writes, f.deleted)
		}
	})
	t.Run("replaced PV is a changed submitted binding", func(t *testing.T) {
		f := newVolumeFixture(t)
		c := volumeCriterion()
		ck := grader.NewArtifactChecker(grader.ArtifactConfig{Reader: f.r})
		a, err := ck.CaptureInputs(context.Background(), volumeTarget(c))
		if err != nil {
			t.Fatal(err)
		}
		f.pv["metadata"].(map[string]any)["uid"] = "other-pv"
		b, err := ck.CaptureInputs(context.Background(), volumeTarget(c))
		if err != nil {
			t.Fatal(err)
		}
		if reflect.DeepEqual(a, b) {
			t.Fatal("replaced PV binding ignored")
		}
	})
}

func TestVolumePersistenceWaitsForTerminatingUID(t *testing.T) {
	f := newVolumeFixture(t)
	old := cloneVolumeObject(f.pod)
	old["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-20T00:00:00Z"
	polls := 0
	f.r.putListFn("Pod", "jk-p1", func(int) []map[string]any {
		if f.deleted > 0 {
			polls++
			if polls < 10 {
				return []map[string]any{old, f.pod}
			}
		}
		return []map[string]any{f.pod}
	})
	clock := newClock()
	ck := grader.NewArtifactChecker(grader.ArtifactConfig{Reader: f.r, Pods: f}, grader.WithClock(clock))
	result, err := ck.Check(context.Background(), volumeTarget(volumeCriterion()), volumeCriterion())
	if err != nil || result.Status != model.StatusPass || clock.slept() == 0 {
		t.Fatalf("result=%+v error=%v polls=%d slept=%s", result, err, polls, clock.slept())
	}
}

func TestVolumeInputsKeepLearnerFilesAndMounts(t *testing.T) {
	f := newVolumeFixture(t)
	c := volumeCriterion()
	target := volumeTarget(c)
	target.Workspace = workspace(t, map[string]string{"answer.txt": "OK"})
	target.Problem.Definition.Criteria = append(target.Problem.Definition.Criteria, fileCrit(map[string]any{"path": "answer.txt", "contains": []any{"OK"}}))
	ck := grader.NewArtifactChecker(grader.ArtifactConfig{Reader: f.r})
	a, err := ck.CaptureInputs(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target.Workspace, "answer.txt"), []byte("CHANGED"), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := ck.CaptureInputs(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(a, b) {
		t.Fatal("learner file content ignored")
	}
	f.deployment["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["volumeMounts"].([]any)[0].(map[string]any)["mountPath"] = "/changed"
	d, err := ck.CaptureInputs(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(b, d) {
		t.Fatal("controller PVC mount change ignored")
	}
}
