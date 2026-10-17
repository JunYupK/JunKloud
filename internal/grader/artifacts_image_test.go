package grader_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

const imageID = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func inspectJSON(id, arch string, extra map[string]any) string {
	cfg := map[string]any{"User": "1000", "WorkingDir": "/app", "Entrypoint": []string{"/app/server"}, "Cmd": []string{"--port", "8080"},
		"Env": []string{"PATH=/usr/bin", "MODE=prod"}, "ExposedPorts": map[string]any{"8080/tcp": map[string]any{}}, "Labels": map[string]string{"team": "a"}}
	doc := map[string]any{"Id": id, "RepoTags": []string{"shop:1"}, "RepoDigests": []string{}, "Architecture": arch, "Os": "linux", "Config": cfg}
	for k, v := range extra {
		doc[k] = v
	}
	raw, _ := json.Marshal([]any{doc})
	return string(raw)
}

func dockerTools(inspect func(ref string) grader.ToolResult) *fakeTools {
	return &fakeTools{handler: func(call grader.ToolCall) (grader.ToolResult, error) {
		if len(call.Args) >= 3 && call.Args[0] == "image" && call.Args[1] == "inspect" {
			return inspect(call.Args[2]), nil
		}
		return grader.ToolResult{}, fmt.Errorf("unexpected tool call %s", joined(call))
	}}
}

func okInspect(arch string) *fakeTools {
	return dockerTools(func(ref string) grader.ToolResult {
		if ref != "shop:1" {
			return exit(1, "Error response from daemon: No such image: "+ref)
		}
		return out(inspectJSON(imageID, arch, nil))
	})
}

func imageCrit(args map[string]any) model.Criterion {
	return model.Criterion{ID: "c", Kind: model.CriterionImage, Points: 4, Args: args}
}

type fakeImageRunner struct {
	res  grader.ImageRunResult
	err  error
	reqs []grader.ImageRunRequest
}

func (f *fakeImageRunner) Run(_ context.Context, req grader.ImageRunRequest) (grader.ImageRunResult, error) {
	f.reqs = append(f.reqs, req)
	return f.res, f.err
}

func TestImageExistenceAndConfig(t *testing.T) {
	arch := runtime.GOARCH
	otherArch := "arm64"
	if arch == "arm64" {
		otherArch = "amd64"
	}
	cfg := func(m map[string]any) map[string]any { return map[string]any{"reference": "shop:1", "config": m} }
	cases := []struct {
		name   string
		tools  *fakeTools
		args   map[string]any
		want   string
		reason []string
	}{
		{name: "exists", tools: okInspect(arch), args: map[string]any{"reference": "shop:1"}, want: "PASS"},
		{name: "missing image is the learner's", tools: okInspect(arch), args: map[string]any{"reference": "shop:2"}, want: "FAIL", reason: []string{"shop:2", "does not exist"}},
		{name: "must be absent and is", tools: okInspect(arch), args: map[string]any{"reference": "shop:2", "exists": false}, want: "PASS"},
		{name: "must be absent but exists", tools: okInspect(arch), args: map[string]any{"reference": "shop:1", "exists": false}, want: "FAIL", reason: []string{"must not exist"}},
		{name: "other architecture", tools: okInspect(otherArch), args: map[string]any{"reference": "shop:1"}, want: "FAIL", reason: []string{otherArch, "linux/" + arch}},
		{name: "config matches", tools: okInspect(arch), args: cfg(map[string]any{"user": "1000", "workdir": "/app", "entrypoint": []any{"/app/server"},
			"cmd": []any{"--port", "8080"}, "env": map[string]any{"MODE": "prod"}, "exposed_ports": []any{"8080"}, "labels": map[string]any{"team": "a"}}), want: "PASS"},
		{name: "wrong user", tools: okInspect(arch), args: cfg(map[string]any{"user": "0"}), want: "FAIL", reason: []string{"user"}},
		{name: "wrong entrypoint", tools: okInspect(arch), args: cfg(map[string]any{"entrypoint": []any{"/bin/sh"}}), want: "FAIL", reason: []string{"entrypoint"}},
		{name: "port not exposed", tools: okInspect(arch), args: cfg(map[string]any{"exposed_ports": []any{"9090/tcp"}}), want: "FAIL", reason: []string{"9090/tcp"}},
		{name: "env value differs without echoing it", tools: okInspect(arch), args: cfg(map[string]any{"env": map[string]any{"MODE": "dev"}}), want: "FAIL", reason: []string{"MODE"}},
		{name: "daemon down is an error", tools: dockerTools(func(string) grader.ToolResult {
			return exit(1, "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")
		}), args: map[string]any{"reference": "shop:1"}, want: "ERROR"},
		{name: "docker missing is an error", tools: &fakeTools{}, args: map[string]any{"reference": "shop:1"}, want: "ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := gradeOne(t, grader.ArtifactConfig{Tools: tc.tools}, "", imageCrit(tc.args))
			testutil.Equal(t, res.Status, tc.want)
			reasonHas(t, res, tc.reason...)
			if tc.name == "env value differs without echoing it" && strings.Contains(evidenceJSON(t, res), "prod") {
				t.Errorf("env value echoed: %s", evidenceJSON(t, res))
			}
			if tc.want == "PASS" && tc.args["exists"] == nil {
				testutil.Equal(t, res.Evidence.InputHash, imageID)
			}
		})
	}
}

func imageCriterionTimeout(c model.Criterion) model.Criterion {
	c.Timeout = 90 * time.Second
	return c
}

// TestImageActuallyRuns: the image exists and has the right config in every
// case below; only what it does when it runs differs.
func TestImageActuallyRuns(t *testing.T) {
	arch := runtime.GOARCH
	run := func(extra map[string]any) map[string]any {
		return map[string]any{"reference": "shop:1", "run": extra}
	}
	cases := []struct {
		name   string
		res    grader.ImageRunResult
		err    error
		args   map[string]any
		want   string
		reason []string
	}{
		{name: "prints the token and exits 0", res: grader.ImageRunResult{Completed: true, Output: "order service v2\nok\n"},
			args: run(map[string]any{"stdout_contains": []any{"order service v2"}, "stdout_matches": []any{`^ok$`}}), want: "PASS"},
		{name: "expected non-zero exit", res: grader.ImageRunResult{Completed: true, ExitCode: 3},
			args: run(map[string]any{"expect_exit_code": 3}), want: "PASS"},
		{name: "exits non-zero", res: grader.ImageRunResult{Completed: true, ExitCode: 1, Output: "panic"},
			args: run(map[string]any{}), want: "FAIL", reason: []string{"exited with code 1"}},
		{name: "wrong output", res: grader.ImageRunResult{Completed: true, Output: "hello world"},
			args: run(map[string]any{"stdout_contains": []any{"order service v2"}}), want: "FAIL", reason: []string{"order service v2"}},
		{name: "cannot start", res: grader.ImageRunResult{StartError: `exec: "/app/server": no such file or directory`},
			args: run(map[string]any{}), want: "FAIL", reason: []string{"cannot start"}},
		{name: "never finishes", res: grader.ImageRunResult{TimedOut: true},
			args: run(map[string]any{"timeout": "10s"}), want: "FAIL", reason: []string{"still running after 10s"}},
		{name: "serves", res: grader.ImageRunResult{Probe: grader.ProbeResult{Connected: true, HTTPStatus: 200, BodyMarker: "shop-v2"}},
			args: run(map[string]any{"serve": map[string]any{"port": 8080, "body_marker": "shop-v2"}}), want: "PASS"},
		{name: "serves a different application", res: grader.ImageRunResult{Probe: grader.ProbeResult{Connected: true, HTTPStatus: 200}},
			args: run(map[string]any{"serve": map[string]any{"port": 8080, "body_marker": "shop-v2"}}), want: "FAIL", reason: []string{"marker"}},
		{name: "serves the wrong status", res: grader.ImageRunResult{Probe: grader.ProbeResult{Connected: true, HTTPStatus: 500, BodyMarker: "shop-v2"}},
			args: run(map[string]any{"serve": map[string]any{"port": 8080, "body_marker": "shop-v2"}}), want: "FAIL", reason: []string{"500"}},
		{name: "nothing listens", res: grader.ImageRunResult{Probe: grader.ProbeResult{FailureClass: grader.FailureRefused}},
			args: run(map[string]any{"serve": map[string]any{"port": 8080, "body_marker": "shop-v2"}}), want: "FAIL", reason: []string{"nothing answered"}},
		{name: "exits instead of serving", res: grader.ImageRunResult{Completed: true, ExitCode: 0},
			args: run(map[string]any{"serve": map[string]any{"port": 8080, "body_marker": "shop-v2"}}), want: "FAIL", reason: []string{"instead of serving"}},
		{name: "network error while probing proves nothing", res: grader.ImageRunResult{Probe: grader.ProbeResult{FailureClass: grader.FailureNetwork}},
			args: run(map[string]any{"serve": map[string]any{"port": 8080, "body_marker": "shop-v2"}}), want: "ERROR"},
		{name: "environment cannot run it", err: fmt.Errorf("load into node failed"),
			args: run(map[string]any{}), want: "ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeImageRunner{res: tc.res, err: tc.err}
			cfg := grader.ArtifactConfig{Tools: okInspect(arch), Images: runner}
			res := gradeOne(t, cfg, "", imageCriterionTimeout(imageCrit(tc.args)))
			testutil.Equal(t, res.Status, tc.want)
			reasonHas(t, res, tc.reason...)
			testutil.Equal(t, len(runner.reqs), 1)
			testutil.Equal(t, runner.reqs[0].LocalRef, "shop:1")
			testutil.Equal(t, runner.reqs[0].Identity.ID, "0123456789ab")
			if strings.Contains(evidenceJSON(t, res), "panic") || strings.Contains(evidenceJSON(t, res), "hello world") {
				t.Errorf("container output reached the evidence: %s", evidenceJSON(t, res))
			}
		})
	}
	t.Run("a missing image is never run", func(t *testing.T) {
		runner := &fakeImageRunner{}
		res := gradeOne(t, grader.ArtifactConfig{Tools: okInspect(arch), Images: runner}, "",
			imageCriterionTimeout(imageCrit(map[string]any{"reference": "shop:9", "run": map[string]any{}})))
		testutil.Equal(t, res.Status, "FAIL")
		testutil.Equal(t, len(runner.reqs), 0)
	})
	t.Run("a wrong config is not run either", func(t *testing.T) {
		runner := &fakeImageRunner{}
		res := gradeOne(t, grader.ArtifactConfig{Tools: okInspect(arch), Images: runner}, "",
			imageCriterionTimeout(imageCrit(map[string]any{"reference": "shop:1", "config": map[string]any{"user": "0"}, "run": map[string]any{}})))
		testutil.Equal(t, res.Status, "FAIL")
		testutil.Equal(t, len(runner.reqs), 0)
	})
	t.Run("no runner configured is an error", func(t *testing.T) {
		res := gradeOne(t, grader.ArtifactConfig{Tools: okInspect(arch)}, "", imageCriterionTimeout(imageCrit(run(map[string]any{}))))
		testutil.Equal(t, res.Status, "ERROR")
	})
	t.Run("run marks the criterion as mutating", func(t *testing.T) {
		ck := grader.NewArtifactChecker(grader.ArtifactConfig{})
		testutil.Equal(t, ck.Mutates(imageCriterionTimeout(imageCrit(run(map[string]any{})))), true)
		testutil.Equal(t, ck.Mutates(imageCrit(map[string]any{"reference": "shop:1"})), false)
	})
}

func TestImageArgsAreStrict(t *testing.T) {
	ck := grader.NewArtifactChecker(grader.ArtifactConfig{})
	short := imageCrit(map[string]any{"reference": "shop:1", "run": map[string]any{"timeout": "30s"}})
	short.Timeout = 40 * time.Second
	bad := map[string]model.Criterion{
		"unknown key":      imageCrit(map[string]any{"reference": "shop:1", "bogus": true}),
		"nothing to grade": imageCrit(map[string]any{}),
		"flag as image":    imageCrit(map[string]any{"reference": "--privileged"}),
		"digest":           imageCrit(map[string]any{"reference": "shop@sha256:abc"}),
		"shell in ref":     imageCrit(map[string]any{"reference": "shop:1;rm"}),
		"empty config":     imageCrit(map[string]any{"reference": "shop:1", "config": map[string]any{}}),
		"bad port":         imageCrit(map[string]any{"reference": "shop:1", "config": map[string]any{"exposed_ports": []any{"http"}}}),
		"run needs ref":    imageCriterionTimeout(imageCrit(map[string]any{"archive": map[string]any{"path": "a.tar"}, "run": map[string]any{}})),
		"timeout too big":  imageCriterionTimeout(imageCrit(map[string]any{"reference": "shop:1", "run": map[string]any{"timeout": "5m"}})),
		"criterion timeout too short for the run": short,
		"serve with exit code": imageCriterionTimeout(imageCrit(map[string]any{"reference": "shop:1", "run": map[string]any{"expect_exit_code": 0,
			"serve": map[string]any{"port": 80, "body_marker": "x"}}})),
		"serve without marker": imageCriterionTimeout(imageCrit(map[string]any{"reference": "shop:1", "run": map[string]any{"serve": map[string]any{"port": 80}}})),
		"archive escapes":      imageCrit(map[string]any{"archive": map[string]any{"path": "../a.tar"}}),
		"archive bad format":   imageCrit(map[string]any{"archive": map[string]any{"path": "a.tar", "format": "zip"}}),
		"absence with config":  imageCrit(map[string]any{"reference": "shop:1", "exists": false, "config": map[string]any{"user": "1"}}),
	}
	for name, c := range bad {
		if err := ck.ValidateCriterion(c); !isInvalid(err) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	withTarget := imageCrit(map[string]any{"reference": "shop:1"})
	withTarget.Target = refOf("v1", "Pod", "ns", "x")
	if err := ck.ValidateCriterion(withTarget); !isInvalid(err) {
		t.Errorf("target given: err = %v", err)
	}
}

// ------------------------------------------------------------- archives

type tarFile struct{ name, body string }

func buildTar(t testing.TB, files []tarFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, f := range files {
		testutil.NoError(t, w.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg}))
		_, err := w.Write([]byte(f.body))
		testutil.NoError(t, err)
	}
	testutil.NoError(t, w.Close())
	return buf.Bytes()
}

func configBlob(arch string) string {
	return fmt.Sprintf(`{"architecture":%q,"os":"linux","config":{"User":"1000","Entrypoint":["/app/server"],"ExposedPorts":{"8080/tcp":{}}}}`, arch)
}

func dockerArchive(t testing.TB, arch string, tags []string, dropLayer, dropConfig bool) []byte {
	manifest, _ := json.Marshal([]any{map[string]any{"Config": "cfg123.json", "RepoTags": tags, "Layers": []string{"layer1/layer.tar", "layer2/layer.tar"}}})
	files := []tarFile{{"manifest.json", string(manifest)}, {"layer1/layer.tar", "L1"}}
	if !dropLayer {
		files = append(files, tarFile{"layer2/layer.tar", "L2"})
	}
	if !dropConfig {
		files = append(files, tarFile{"cfg123.json", configBlob(arch)})
	}
	return buildTar(t, files)
}

func digestOf(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func ociArchive(t testing.TB, arch, tag string) []byte {
	cfg := configBlob(arch)
	layer := "LAYER"
	man, _ := json.Marshal(map[string]any{"schemaVersion": 2, "config": map[string]any{"digest": "sha256:" + digestOf(cfg)},
		"layers": []any{map[string]any{"digest": "sha256:" + digestOf(layer)}}})
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []any{map[string]any{
		"digest": "sha256:" + digestOf(string(man)), "annotations": map[string]any{"io.containerd.image.name": tag}}}})
	return buildTar(t, []tarFile{{"oci-layout", `{"imageLayoutVersion":"1.0.0"}`}, {"index.json", string(idx)},
		{"blobs/sha256/" + digestOf(string(man)), string(man)}, {"blobs/sha256/" + digestOf(cfg), cfg}, {"blobs/sha256/" + digestOf(layer), layer}})
}

func writeArchive(t testing.TB, ws, name string, data []byte) {
	t.Helper()
	testutil.NoError(t, os.WriteFile(filepath.Join(ws, name), data, 0o644))
}

func TestImageArchiveStructure(t *testing.T) {
	arch := runtime.GOARCH
	other := "arm64"
	if arch == "arm64" {
		other = "amd64"
	}
	args := func(extra map[string]any) map[string]any {
		a := map[string]any{"path": "shop.tar"}
		for k, v := range extra {
			a[k] = v
		}
		return map[string]any{"archive": a}
	}
	cases := []struct {
		name    string
		archive []byte
		args    map[string]any
		want    string
		reason  []string
	}{
		{name: "docker save layout", archive: dockerArchive(t, arch, []string{"shop:1"}, false, false), args: args(map[string]any{"repo_tag": "shop:1"}), want: "PASS"},
		{name: "tag is matched after defaulting", archive: dockerArchive(t, arch, []string{"docker.io/library/shop:1"}, false, false), args: args(map[string]any{"repo_tag": "shop:1"}), want: "PASS"},
		{name: "oci layout", archive: ociArchive(t, arch, "shop:1"), args: args(map[string]any{"repo_tag": "shop:1", "format": "oci"}), want: "PASS"},
		{name: "config is checked in the archive", archive: dockerArchive(t, arch, []string{"shop:1"}, false, false),
			args: map[string]any{"archive": map[string]any{"path": "shop.tar", "repo_tag": "shop:1"}, "config": map[string]any{"user": "1000", "exposed_ports": []any{"8080"}}}, want: "PASS"},
		{name: "archive config differs", archive: dockerArchive(t, arch, []string{"shop:1"}, false, false),
			args: map[string]any{"archive": map[string]any{"path": "shop.tar"}, "config": map[string]any{"user": "0"}}, want: "FAIL", reason: []string{"archive image config", "user"}},
		{name: "wrong tag", archive: dockerArchive(t, arch, []string{"other:1"}, false, false), args: args(map[string]any{"repo_tag": "shop:1"}), want: "FAIL", reason: []string{"shop:1"}},
		{name: "layer missing", archive: dockerArchive(t, arch, []string{"shop:1"}, true, false), args: args(map[string]any{"repo_tag": "shop:1"}), want: "FAIL", reason: []string{"layer2/layer.tar", "missing"}},
		{name: "config missing", archive: dockerArchive(t, arch, []string{"shop:1"}, false, true), args: args(map[string]any{"repo_tag": "shop:1"}), want: "FAIL", reason: []string{"cfg123.json"}},
		{name: "other architecture", archive: dockerArchive(t, other, []string{"shop:1"}, false, false), args: args(nil), want: "FAIL", reason: []string{other}},
		{name: "not a tar", archive: []byte("this is not a tar archive at all, just text\n"), args: args(nil), want: "FAIL", reason: []string{"not a readable image tar"}},
		{name: "a tar without a manifest", archive: buildTar(t, []tarFile{{"hello.txt", "x"}}), args: args(nil), want: "FAIL", reason: []string{"manifest.json"}},
		{name: "layout mismatch", archive: dockerArchive(t, arch, []string{"shop:1"}, false, false), args: args(map[string]any{"format": "oci"}), want: "FAIL", reason: []string{"layout"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := workspace(t, nil)
			writeArchive(t, ws, "shop.tar", tc.archive)
			res := gradeOne(t, grader.ArtifactConfig{}, ws, imageCrit(tc.args))
			testutil.Equal(t, res.Status, tc.want)
			reasonHas(t, res, tc.reason...)
		})
	}
	t.Run("missing archive", func(t *testing.T) {
		res := gradeOne(t, grader.ArtifactConfig{}, workspace(t, nil), imageCrit(args(nil)))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "does not exist")
	})
	t.Run("archive symlinked out of the workspace", func(t *testing.T) {
		outside := t.TempDir()
		testutil.NoError(t, os.WriteFile(filepath.Join(outside, "shop.tar"), dockerArchive(t, arch, []string{"shop:1"}, false, false), 0o644))
		ws := workspace(t, nil)
		testutil.NoError(t, os.Symlink(filepath.Join(outside, "shop.tar"), filepath.Join(ws, "shop.tar")))
		res := gradeOne(t, grader.ArtifactConfig{}, ws, imageCrit(args(nil)))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "outside the session workspace")
	})
}
