package grader_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func TestLogsActualNodeName(t *testing.T) {
	for _, tc := range []struct{ log, want string }{{"node=kind-a\n", "PASS"}, {"node=\n", "FAIL"}, {"node=kind-other\n", "FAIL"}, {"node=kind-a-suffix\n", "FAIL"}, {"notnode=kind-a\n", "FAIL"}} {
		t.Run(tc.log, func(t *testing.T) {
			p := podDoc(t, "app", "", "Running", "app")
			p["spec"].(map[string]any)["nodeName"] = "kind-a"
			pods := newPods()
			pods.logs["jk-p1/app/app"] = tc.log
			r := gradeOne(t, grader.ArtifactConfig{Reader: newReader().put(podRef, p), Pods: pods}, "", logsCrit(podRef, map[string]any{"node_name_prefix": "node="}))
			testutil.Equal(t, r.Status, tc.want)
		})
	}
}

func sharedLogFixture(t *testing.T) (map[string]any, *fakePods, model.Criterion) {
	p := podDoc(t, "app", "", "Running", "app", "sidecar")
	spec := p["spec"].(map[string]any)
	spec["volumes"] = []any{map[string]any{"name": "shared", "emptyDir": map[string]any{"sizeLimit": "8Mi"}}}
	for _, raw := range spec["containers"].([]any) {
		raw.(map[string]any)["volumeMounts"] = []any{map[string]any{"name": "shared", "mountPath": "/work"}}
	}
	p["status"].(map[string]any)["containerStatuses"] = []any{map[string]any{"name": "app", "state": map[string]any{"running": map[string]any{}}}, map[string]any{"name": "sidecar", "state": map[string]any{"running": map[string]any{}}}}
	pods := newPods()
	pods.logs["jk-p1/app/app"] = "delivered:seeded\n"
	pods.execFn = func(_ string, cmd []string) (grader.ExecResult, error) {
		if cmd[len(cmd)-1] == "/work/message" {
			return grader.ExecResult{Stdout: "seeded\n"}, nil
		}
		return grader.ExecResult{Stdout: "delivered:seeded\n"}, nil
	}
	c := logsCrit(podRef, map[string]any{"container": "app", "contains": []any{"delivered:seeded"}, "shared_files": map[string]any{"volume": "shared", "containers": []any{"app", "sidecar"}, "files": []any{map[string]any{"path": "/work/message", "equals": "seeded\n"}, map[string]any{"path": "/work/ack", "equals": "delivered:seeded\n"}}}})
	return p, pods, c
}

func TestLogsSharedFiles(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		edit       func(map[string]any, *fakePods)
	}{
		{"actual files on configured emptyDir", "PASS", nil},
		{"phony literal log without files", "FAIL", func(_ map[string]any, p *fakePods) {
			p.execFn = func(string, []string) (grader.ExecResult, error) { return grader.ExecResult{ExitCode: 3}, nil }
		}},
		{"wrong shared content", "FAIL", func(_ map[string]any, p *fakePods) {
			p.execFn = func(string, []string) (grader.ExecResult, error) {
				return grader.ExecResult{Stdout: "private-wrong-value"}, nil
			}
		}},
		{"sidecar cannot read shared file", "FAIL", func(_ map[string]any, p *fakePods) {
			p.execFn = func(key string, _ []string) (grader.ExecResult, error) {
				if strings.HasSuffix(key, "/sidecar") {
					return grader.ExecResult{ExitCode: 3}, nil
				}
				return grader.ExecResult{Stdout: "seeded\n"}, nil
			}
		}},
		{"symlink outside mount", "FAIL", func(_ map[string]any, p *fakePods) {
			p.execFn = func(string, []string) (grader.ExecResult, error) { return grader.ExecResult{ExitCode: 2}, nil }
		}},
		{"platform exec failure", "ERROR", func(_ map[string]any, p *fakePods) {
			p.execFn = func(string, []string) (grader.ExecResult, error) {
				return grader.ExecResult{}, fmt.Errorf("API transport unavailable")
			}
		}},
		{"nested mount masks shared path", "FAIL", func(p map[string]any, _ *fakePods) {
			cs := p["spec"].(map[string]any)["containers"].([]any)
			cs[0].(map[string]any)["volumeMounts"] = []any{map[string]any{"name": "shared", "mountPath": "/work"}, map[string]any{"name": "other", "mountPath": "/work/message"}}
		}},
		{"different subPath", "FAIL", func(p map[string]any, _ *fakePods) {
			cs := p["spec"].(map[string]any)["containers"].([]any)
			cs[1].(map[string]any)["volumeMounts"].([]any)[0].(map[string]any)["subPath"] = "other"
		}},
		{"unsafe mount cannot be ignored", "FAIL", func(p map[string]any, _ *fakePods) {
			cs := p["spec"].(map[string]any)["containers"].([]any)
			cs[0].(map[string]any)["volumeMounts"] = []any{map[string]any{"name": "shared", "mountPath": "/work"}, map[string]any{"name": "other", "mountPath": "/work/../work/ack"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, pods, c := sharedLogFixture(t)
			if tc.edit != nil {
				tc.edit(p, pods)
			}
			r := gradeOne(t, grader.ArtifactConfig{Reader: newReader().put(podRef, p), Pods: pods}, "", c)
			testutil.Equal(t, r.Status, tc.want)
			if strings.Contains(evidenceJSON(t, r), "private-wrong-value") {
				t.Fatal("file content leaked")
			}
		})
	}
}

func TestLogsSharedFilesStrictPaths(t *testing.T) {
	for _, bad := range []string{"/work/../secret", "relative", "/work/a\nb"} {
		t.Run(bad, func(t *testing.T) {
			_, _, c := sharedLogFixture(t)
			c.Args["shared_files"].(map[string]any)["files"] = []any{map[string]any{"path": bad, "equals": "x"}}
			err := grader.NewArtifactChecker(grader.ArtifactConfig{}).ValidateCriterion(c)
			if !isInvalid(err) {
				t.Fatalf("path %q accepted: %v", bad, err)
			}
		})
	}
}
