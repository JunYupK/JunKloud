package grader_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// The author's solution, and the same Deployment as another learner writes
// it: different key order, flow style, comments, quoting.
const (
	answerYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  replicas: 2
  template:
    spec:
      containers:
      - name: web
        image: nginx:1.27.0
`
	answerYAMLReordered = `# my answer
kind: Deployment
apiVersion: apps/v1
spec: {template: {spec: {containers: [{image: "nginx:1.27.0", name: web}]}}, replicas: 2}
metadata: {name: web}
`
	answerYAMLWrongReplicas = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  replicas: 3
  template:
    spec:
      containers:
      - name: web
        image: nginx:1.27.0
`
)

func deployFileArgs() map[string]any {
	return map[string]any{
		"path":   "answer.yaml",
		"format": "yaml",
		"select": map[string]any{"kind": "Deployment", "name": "web"},
		"fields": []any{
			map[string]any{"path": "spec.replicas", "equals": 2},
			map[string]any{"path": "spec.template.spec.containers[name=web].image", "equals": "nginx:1.27.0"},
		},
	}
}

func TestFileStructure(t *testing.T) {
	cases := []struct {
		name   string
		files  map[string]string
		args   map[string]any
		want   string
		reason []string
	}{
		{name: "solution", files: map[string]string{"answer.yaml": answerYAML}, args: deployFileArgs(), want: "PASS"},
		{name: "different key order and style", files: map[string]string{"answer.yaml": answerYAMLReordered}, args: deployFileArgs(), want: "PASS"},
		{name: "wrong value", files: map[string]string{"answer.yaml": answerYAMLWrongReplicas}, args: deployFileArgs(), want: "FAIL", reason: []string{"spec.replicas"}},
		{name: "invalid yaml", files: map[string]string{"answer.yaml": "a: [unclosed\n"}, args: deployFileArgs(), want: "FAIL", reason: []string{"not valid YAML"}},
		{name: "missing file", files: map[string]string{}, args: deployFileArgs(), want: "FAIL", reason: []string{"answer.yaml", "does not exist"}},
		{name: "directory", files: map[string]string{"answer.yaml/x": "y"}, args: deployFileArgs(), want: "FAIL", reason: []string{"directory"}},
		{name: "no document matches the selector", files: map[string]string{"answer.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: web\n"}, args: deployFileArgs(), want: "FAIL", reason: []string{"no document"}},
		{
			name:  "second document of several matches",
			files: map[string]string{"answer.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: web\n---\n" + answerYAML},
			args:  deployFileArgs(), want: "PASS",
		},
		{
			name:  "json",
			files: map[string]string{"cfg.json": `{"mode":"fast","limits":{"cpu":"500m"}}`},
			args: map[string]any{"path": "cfg.json", "format": "json", "fields": []any{
				map[string]any{"path": "limits.cpu", "equals": "0.5"}, map[string]any{"path": "mode", "equals": "fast"}}},
			want: "PASS",
		},
		{
			name:  "invalid json",
			files: map[string]string{"cfg.json": `{"mode":`},
			args:  map[string]any{"path": "cfg.json", "format": "json", "fields": []any{map[string]any{"path": "mode", "exists": true}}},
			want:  "FAIL", reason: []string{"not valid JSON"},
		},
		{
			name:  "text contains and pattern",
			files: map[string]string{"notes.txt": "kubectl rollout undo deploy/web\nrevision: 3\n"},
			args: map[string]any{"path": "notes.txt", "contains": []any{"rollout undo"},
				"not_contains": []any{"delete"}, "matches": []any{`revision:\s+\d+`}},
			want: "PASS",
		},
		{
			name:  "text lacks a required string",
			files: map[string]string{"notes.txt": "nothing here\n"},
			args:  map[string]any{"path": "notes.txt", "contains": []any{"rollout undo"}},
			want:  "FAIL", reason: []string{"rollout undo"},
		},
		{
			name:  "forbidden text",
			files: map[string]string{"notes.txt": "kubectl delete ns prod\n"},
			args:  map[string]any{"path": "notes.txt", "not_contains": []any{"delete"}},
			want:  "FAIL", reason: []string{"delete"},
		},
		{
			name:  "exact text when the criterion asks for it",
			files: map[string]string{"out.txt": "hello\n"},
			args:  map[string]any{"path": "out.txt", "exact": "hello\n"},
			want:  "PASS",
		},
		{
			name:  "exact text differs",
			files: map[string]string{"out.txt": "hello \n"},
			args:  map[string]any{"path": "out.txt", "exact": "hello\n"},
			want:  "FAIL", reason: []string{"exact"},
		},
		{
			name:  "absence required and absent",
			files: map[string]string{},
			args:  map[string]any{"path": "secret.txt", "exists": false},
			want:  "PASS",
		},
		{
			name:  "absence required but present",
			files: map[string]string{"secret.txt": "x"},
			args:  map[string]any{"path": "secret.txt", "exists": false},
			want:  "FAIL", reason: []string{"must not exist"},
		},
		{
			name:  "size bounds",
			files: map[string]string{"big.txt": strings.Repeat("a", 20)},
			args:  map[string]any{"path": "big.txt", "max_bytes": 10},
			want:  "FAIL", reason: []string{"20 bytes"},
		},
		{
			name:  "content digest",
			files: map[string]string{"a.bin": "abc"},
			args:  map[string]any{"path": "a.bin", "sha256": "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
			want:  "PASS",
		},
		{
			name:  "other content digest",
			files: map[string]string{"a.bin": "abd"},
			args:  map[string]any{"path": "a.bin", "sha256": "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
			want:  "FAIL", reason: []string{"sha256"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := gradeOne(t, grader.ArtifactConfig{}, workspace(t, tc.files), fileCrit(tc.args))
			testutil.Equal(t, res.Status, tc.want)
			reasonHas(t, res, tc.reason...)
			if tc.want == "PASS" {
				testutil.Equal(t, res.Points, 3)
			}
		})
	}
}

// TestAssetSymlinkEscape: workspace paths are resolved with
// problem.ResolveAsset. The original fixture is a valid answer inside the
// workspace; the wrong answers are links that lead out of it (spec 6.2).
func TestAssetSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	testutil.NoError(t, os.WriteFile(filepath.Join(outside, "answer.yaml"), []byte(answerYAML), 0o644))

	ws := workspace(t, map[string]string{"real/answer.yaml": answerYAML})
	link := func(name, target string) {
		t.Helper()
		testutil.NoError(t, os.Symlink(target, filepath.Join(ws, name)))
	}
	link("inside.yaml", "real/answer.yaml")                               // control: stays inside
	link("escape.yaml", filepath.Join(outside, "answer.yaml"))            // file link out
	link("dangling.yaml", filepath.Join(outside, "not-yet.yaml"))         // dangling link out
	link("outdir", outside)                                               // directory link out
	link("relative.yaml", "../"+filepath.Base(outside)+"/answer.yaml")    // relative link out
	testutil.NoError(t, os.Symlink("real", filepath.Join(ws, "realdir"))) // directory link inside

	pathArgs := func(p string) map[string]any {
		a := deployFileArgs()
		a["path"] = p
		return a
	}
	for _, p := range []string{"inside.yaml", "realdir/answer.yaml"} {
		res := gradeOne(t, grader.ArtifactConfig{}, ws, fileCrit(pathArgs(p)))
		testutil.Equal(t, res.Status, "PASS")
	}
	for _, p := range []string{"escape.yaml", "dangling.yaml", "outdir/answer.yaml", "relative.yaml"} {
		t.Run(p, func(t *testing.T) {
			res := gradeOne(t, grader.ArtifactConfig{}, ws, fileCrit(pathArgs(p)))
			testutil.Equal(t, res.Status, "FAIL")
			reasonHas(t, res, "outside the session workspace")
			if strings.Contains(evidenceJSON(t, res), "apiVersion") {
				t.Errorf("content of the escaped file reached the evidence: %s", evidenceJSON(t, res))
			}
		})
	}

	// A path that is itself an escape is the author's mistake, not the learner's.
	for _, p := range []string{"../answer.yaml", "/etc/hostname", "a/../../x"} {
		res := gradeOne(t, grader.ArtifactConfig{}, ws, fileCrit(pathArgs(p)))
		testutil.Equal(t, res.Status, "ERROR")
		reasonHas(t, res, "invalid criterion")
	}
}

func TestFileArgsAreStrict(t *testing.T) {
	ck := grader.NewArtifactChecker(grader.ArtifactConfig{})
	bad := map[string]map[string]any{
		"unknown key":          {"path": "a", "bogus": 1},
		"no path":              {"contains": []any{"x"}},
		"fields need a format": {"path": "a", "fields": []any{map[string]any{"path": "x", "exists": true}}},
		"fields on text":       {"path": "a", "format": "text", "fields": []any{map[string]any{"path": "x", "exists": true}}},
		"unknown format":       {"path": "a", "format": "toml"},
		"absence with content": {"path": "a", "exists": false, "contains": []any{"x"}},
		"bad regexp":           {"path": "a", "matches": []any{"("}},
		"bad digest":           {"path": "a", "sha256": "xyz"},
		"select on text":       {"path": "a", "select": map[string]any{"kind": "Pod"}},
		"negative bound":       {"path": "a", "max_bytes": -1},
		"inverted bounds":      {"path": "a", "min_bytes": 5, "max_bytes": 2},
		"target given":         {"path": "a"},
	}
	for name, args := range bad {
		c := fileCrit(args)
		if name == "target given" {
			c.Target = refOf("v1", "Pod", "ns", "x")
		}
		err := ck.ValidateCriterion(c)
		if !isInvalid(err) {
			t.Errorf("%s: err = %v, want ErrInvalidCriterion", name, err)
		}
	}
	testutil.NoError(t, ck.ValidateCriterion(fileCrit(deployFileArgs())))
}

func TestFileWithoutWorkspaceIsAnError(t *testing.T) {
	res := gradeOne(t, grader.ArtifactConfig{}, "", fileCrit(deployFileArgs()))
	testutil.Equal(t, res.Status, "ERROR")
	reasonHas(t, res, "workspace")
}

// A Secret manifest in a submitted file must never reach the stored evidence.
func TestFileSecretValuesNeverAppearInEvidence(t *testing.T) {
	const secretYAML = `apiVersion: v1
kind: Secret
metadata:
  name: db
stringData:
  password: hunter2-very-secret
data:
  token: c3VwZXItc2VjcmV0LXRva2Vu
`
	ws := workspace(t, map[string]string{"s.yaml": secretYAML})
	res := gradeOne(t, grader.ArtifactConfig{}, ws, fileCrit(map[string]any{
		"path": "s.yaml", "format": "yaml",
		"fields": []any{
			map[string]any{"path": "stringData.password", "equals": "something-else"},
			map[string]any{"path": "data.token", "equals": "another"}},
	}))
	testutil.Equal(t, res.Status, "FAIL")
	js := evidenceJSON(t, res)
	for _, leak := range []string{"hunter2-very-secret", "c3VwZXItc2VjcmV0LXRva2Vu", "super-secret-token"} {
		if strings.Contains(js, leak) {
			t.Errorf("evidence leaks %q: %s", leak, js)
		}
	}
	testutil.Equal(t, res.Evidence.InputHash, "")
}

func TestFileEvidenceCarriesContentHash(t *testing.T) {
	ws := workspace(t, map[string]string{"answer.yaml": answerYAML})
	res := gradeOne(t, grader.ArtifactConfig{}, ws, fileCrit(deployFileArgs()))
	testutil.Equal(t, res.Status, "PASS")
	if !strings.HasPrefix(res.Evidence.InputHash, "sha256:") {
		t.Errorf("InputHash = %q", res.Evidence.InputHash)
	}
	// The same content in another workspace has the same hash; other content differs.
	res2 := gradeOne(t, grader.ArtifactConfig{}, workspace(t, map[string]string{"answer.yaml": answerYAML}), fileCrit(deployFileArgs()))
	testutil.Equal(t, res2.Evidence.InputHash, res.Evidence.InputHash)
	res3 := gradeOne(t, grader.ArtifactConfig{}, workspace(t, map[string]string{"answer.yaml": answerYAMLReordered}), fileCrit(deployFileArgs()))
	if res3.Evidence.InputHash == res.Evidence.InputHash {
		t.Error("different bytes, same hash")
	}
}

func TestFileCancelled(t *testing.T) {
	eng, _ := artifactEngine(grader.ArtifactConfig{}, newClock())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := eng.Evaluate(ctx, artifactTarget(workspace(t, nil), fileCrit(deployFileArgs())))
	if err == nil {
		t.Fatal("cancelled evaluation returned no error")
	}
}

var _ = model.CriterionFile
