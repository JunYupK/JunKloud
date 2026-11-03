//go:build linux && (e2e || acceptance)

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/lock"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/store"
)

// cliHarness runs the real ckad binary in separate processes against a
// private state directory, workspace and bank. env is the complete process
// environment every ckad run receives.
type cliHarness struct {
	t         *testing.T
	bin       string
	state     string
	workspace string
	bank      string
	env       []string
}

// repoRoot is the repository root (the tests run in tests/e2e).
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// newHarness builds ./cmd/ckad, points the CKAD_* variables at private
// directories and a decoy KUBECONFIG, and registers the cleanup that stops
// the platform and deletes the recorded kind cluster. A missing tool is a
// failure, never a skip.
func newHarness(t *testing.T, bank string) *cliHarness {
	t.Helper()
	for _, name := range []string{"go", "docker", "kind", "kubectl"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatalf("live CLI tests require %s: %v", name, err)
		}
	}
	root := repoRoot(t)
	tmp := t.TempDir()
	h := &cliHarness{
		t:         t,
		bin:       filepath.Join(tmp, "ckad"),
		state:     filepath.Join(tmp, "state"),
		workspace: filepath.Join(tmp, "work"),
		bank:      bank,
	}
	// Preserve external-tool failure evidence in the test harness, without
	// exposing arbitrary internal errors through the learner CLI.
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal(err)
	}
	toolDir := filepath.Join(tmp, "tools")
	if err := os.Mkdir(toolDir, 0700); err != nil {
		t.Fatal(err)
	}
	wrapper := `#!/bin/sh
error_file="$(mktemp)"
"$CKAD_E2E_REAL_DOCKER" "$@" 2>"$error_file"
code=$?
cat "$error_file" >&2
if [ "$code" -ne 0 ]; then
  printf 'docker %s\n' "$*" >> "$CKAD_E2E_DOCKER_FAILURES"
  cat "$error_file" >> "$CKAD_E2E_DOCKER_FAILURES"
fi
rm -f "$error_file"
exit "$code"
`
	if err := os.WriteFile(filepath.Join(toolDir, "docker"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CKAD_E2E_REAL_DOCKER", dockerPath)
	t.Setenv("CKAD_E2E_DOCKER_FAILURES", h.dockerFailures())
	t.Setenv("PATH", toolDir+":"+os.Getenv("PATH"))
	buildCtx, stop := context.WithTimeout(context.Background(), 10*time.Minute)
	defer stop()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", h.bin, "./cmd/ckad")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	t.Setenv("CKAD_STATE_DIR", h.state)
	t.Setenv("CKAD_WORKSPACE", h.workspace)
	t.Setenv("CKAD_BANK_DIR", bank)
	t.Setenv("CKAD_HOST_COMPAT", os.Getenv("JUNKLOUD_TEST_HOST_COMPAT"))
	// A decoy verifies the flow never adopts the ambient host configuration.
	decoy := filepath.Join(tmp, "decoy-kubeconfig")
	if err := os.WriteFile(decoy, []byte("deliberately not a kubeconfig\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", decoy)
	h.env = os.Environ()
	t.Cleanup(h.cleanup)
	return h
}

func (h *cliHarness) dockerFailures() string {
	return filepath.Join(filepath.Dir(h.state), "docker-failures.txt")
}

// cleanup verifies the recorded cluster is this harness's own before it
// stops the platform and deletes it.
func (h *cliHarness) cleanup() {
	t := h.t
	st, err := store.Open(filepath.Join(h.state, "sessions.db"))
	if err != nil {
		t.Errorf("cleanup store: %v", err)
		return
	}
	defer st.Close()
	m := environment.New(h.state, st)
	id, err := m.RecordedIdentity()
	if err != nil {
		t.Logf("no recorded E2E cluster: %v", err)
		return
	}
	cleanupCtx, stop := context.WithTimeout(context.Background(), 2*time.Minute)
	defer stop()
	release, err := lock.Acquire(author.LockPath(h.state))
	if err != nil {
		t.Errorf("cleanup lock: %v", err)
		return
	}
	defer release()
	if err := m.Check(cleanupCtx, id); err != nil {
		t.Errorf("refusing cleanup of unverified cluster %s: %v", id.ClusterName, err)
		return
	}
	if err := m.StopPlatform(cleanupCtx, id); err != nil {
		t.Errorf("cleanup platform: %v", err)
		return
	}
	cmd := exec.CommandContext(cleanupCtx, "kind", "delete", "cluster", "--name", id.ClusterName, "--kubeconfig", m.KubeconfigPath())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("cleanup verified cluster: %v\n%s", err, out)
	}
}

// run executes ckad to completion and fails the test unless it exits with
// wantExit. It logs the full command, input, exit code and output.
func (h *cliHarness) run(stdin string, wantExit int, args ...string) (stdout, stderr string) {
	t := h.t
	t.Helper()
	t.Logf("running ckad %s", strings.Join(args, " "))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.bin, args...)
	cmd.Dir = repoRoot(t)
	cmd.Env = h.env
	cmd.Stdin = strings.NewReader(stdin)
	var out, diag bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &diag
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatal(err)
		}
	}
	t.Logf("ckad %s; input=%q; exit=%d; stdout=%s; stderr=%s", strings.Join(args, " "), stdin, code, out.String(), diag.String())
	if code != wantExit {
		if diagnostics, err := os.ReadFile(h.dockerFailures()); err == nil {
			t.Logf("external Docker failures:\n%s", diagnostics)
		}
		h.dumpNonPassResults()
		t.Fatalf("expected exit %d, got %d", wantExit, code)
	}
	return out.String(), diag.String()
}

// dumpNonPassResults logs every non-PASS result, with its stored reason, of
// the current session's grading runs (the CLI prints ERROR reasons only as a
// generic sentence). It is best-effort diagnostics for a failed expectation.
func (h *cliHarness) dumpNonPassResults() {
	t := h.t
	ctx := context.Background()
	st, err := store.Open(filepath.Join(h.state, "sessions.db"))
	if err != nil {
		return
	}
	defer st.Close()
	sid, err := st.CurrentSessionID(ctx)
	if err != nil {
		return
	}
	subs, err := st.ListSubmissions(ctx, sid)
	if err != nil {
		return
	}
	for _, sub := range subs {
		runs, err := st.ListRuns(ctx, sub.ID)
		if err != nil {
			continue
		}
		for _, run := range runs {
			for _, r := range run.Evaluation.Results {
				if r.Status != model.StatusPass {
					t.Logf("DIAG run %s: %s %s: %s", run.ID, r.CriterionID, r.Status, r.Reason)
				}
			}
		}
	}
}

// start begins ckad asynchronously and returns the running command, for
// tests that kill it mid-flight. Stdin is empty; stdout and stderr are
// captured into cmd.Stdout and cmd.Stderr (*bytes.Buffer). The caller must
// Wait for it (after Kill, if it kills it).
func (h *cliHarness) start(args ...string) *exec.Cmd {
	t := h.t
	t.Helper()
	cmd := exec.Command(h.bin, args...)
	cmd.Dir = repoRoot(t)
	cmd.Env = h.env
	cmd.Stdin = strings.NewReader("")
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start ckad %s: %v", strings.Join(args, " "), err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil { // never waited: still running or unreaped
			cmd.Process.Kill()
		}
	})
	t.Logf("started ckad %s (pid %d)", strings.Join(args, " "), cmd.Process.Pid)
	return cmd
}

// hostCompat parses the same JUNKLOUD_TEST_HOST_COMPAT the binary receives.
func (h *cliHarness) managerOptions() []environment.Option {
	compat, err := environment.ParseHostCompat(os.Getenv("JUNKLOUD_TEST_HOST_COMPAT"))
	if err != nil {
		h.t.Fatalf("JUNKLOUD_TEST_HOST_COMPAT: %v", err)
	}
	return []environment.Option{environment.WithHostCompat(compat)}
}

// identity is the recorded environment identity.
func (h *cliHarness) identity(ctx context.Context) model.Identity {
	t := h.t
	t.Helper()
	st, err := store.Open(filepath.Join(h.state, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := environment.New(h.state, st).RecordedIdentity()
	if err != nil {
		t.Fatalf("recorded identity: %v", err)
	}
	return id
}

func (h *cliHarness) kubeconfig() string {
	return environment.New(h.state, nil).KubeconfigPath()
}

// certifyStandIn creates the environment (as the first ckad command would)
// and stores a stand-in validation certificate for each bank problem in the
// harness's own store, as tests/integration/mock_serving_test.go does.
//
// stand-in certificates: lifecycle evidence, not package validation. Package
// validation evidence comes from the bank tests and `ckad bank validate`.
func (h *cliHarness) certifyStandIn(ctx context.Context, ids []string) {
	t := h.t
	t.Helper()
	st, err := store.Open(filepath.Join(h.state, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := environment.New(h.state, st, h.managerOptions()...)
	id, err := author.EnsureEnvironment(ctx, m, h.state)
	if err != nil {
		t.Fatalf("create the environment: %v", err)
	}
	rt, err := author.NewRuntime(ctx, m, st, id, author.GraderConfig{StateDir: h.state})
	if err != nil {
		t.Fatalf("compose the runtime: %v", err)
	}
	defer rt.Close()
	for _, pid := range ids {
		p, err := problem.Load(filepath.Join(h.bank, "problems", pid))
		if err != nil {
			t.Fatal(err)
		}
		seeds, err := author.CoveringSeeds(p)
		if err != nil {
			t.Fatal(err)
		}
		hash, err := problem.Hash(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SaveValidation(ctx, model.ValidationRecord{
			ProblemHash: hash, GraderVersion: grader.GraderVersion, EnvironmentHash: rt.EnvironmentHash,
			Seeds: seeds, Passed: true, Cases: map[string]model.Evaluation{},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// solveCurrent applies every frozen problem's solution actions of the current
// session, through the same action executor the author tooling uses, into the
// problem's latest attempt directory. Image build actions run as
// `ckad image build` so that reset owns the image; image save runs plain
// `docker save`, as the learner would.
func (h *cliHarness) solveCurrent(ctx context.Context) {
	t := h.t
	t.Helper()
	st, err := store.Open(filepath.Join(h.state, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := environment.New(h.state, st, h.managerOptions()...)
	id, err := m.RecordedIdentity()
	if err != nil {
		t.Fatal(err)
	}
	sid, err := st.CurrentSessionID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	record, err := st.LoadSession(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	cfg := author.GraderConfig{StateDir: h.state}
	reader, err := grader.NewClusterReader(m.KubeconfigPath(), id.Context)
	if err != nil {
		t.Fatal(err)
	}
	executor := author.NewActionExecutorWithTools(m, m, author.ToolActionConfig{Tools: grader.NewToolRunner(), Reader: reader, KubeconfigPath: m.KubeconfigPath(), Context: id.Context}, cfg.ProvidedImages()...)
	for i, frozen := range record.Problems {
		pkg, err := session.LoadPackage(h.state, frozen.Hash)
		if err != nil {
			t.Fatal(err)
		}
		attempt, _, err := session.LatestAttempt(h.workspace, sid, frozen.Definition.ID)
		if err != nil {
			t.Fatal(err)
		}
		label := fmt.Sprintf("Q%d %s", i+1, frozen.Definition.ID)
		var batch []model.Action
		flush := func() {
			if len(batch) == 0 {
				return
			}
			inv, err := st.GetInventory(ctx, id.ID, sid, frozen.Definition.ID)
			if errors.Is(err, store.ErrNotFound) {
				inv = model.Inventory{EnvironmentID: id.ID, SessionID: sid, ProblemID: frozen.Definition.ID}
			} else if err != nil {
				t.Fatalf("solve %s: load the inventory: %v", label, err)
			}
			owner := environment.Owner{Inventory: inv, Namespace: frozen.Namespace, Scope: frozen.Definition.Scope}
			t.Logf("solve %s: %d action(s)", label, len(batch))
			if _, err := executor.Execute(ctx, id, owner, pkg, frozen, attempt, batch); err != nil {
				t.Fatalf("solve %s: %v", label, err)
			}
			batch = nil
		}
		for _, action := range pkg.Solution {
			if action.Kind != model.ActionImage && action.Kind != model.ActionHelm && action.Kind != model.ActionKustomize {
				batch = append(batch, action)
				continue
			}
			flush()
			rendered, err := problem.RenderValue(action.Args, frozen.Parameters)
			if err != nil {
				t.Fatal(err)
			}
			args := rendered.(map[string]any)
			if action.Kind != model.ActionImage {
				h.runNative(ctx, label, action.Kind, args, attempt, frozen.Namespace, id.Context)
				continue
			}
			reference, _ := args["reference"].(string)
			path, _ := args["path"].(string)
			switch args["operation"] {
			case "build":
				// Managed build: the session owns the image, so reset removes it.
				// ckad refuses this once the session is no longer RUNNING.
				h.run("", 0, "image", "build", "--problem", fmt.Sprintf("Q%d", i+1), "--context", path, "--tag", reference)
			case "save":
				output := filepath.Join(attempt, path)
				if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
					t.Fatal(err)
				}
				out, err := exec.CommandContext(ctx, "docker", "save", "--output", output, reference).CombinedOutput()
				if err != nil {
					t.Fatalf("solve %s: docker save: %v\n%s", label, err, out)
				}
			default:
				t.Fatalf("solve %s: unsupported image operation %v", label, args["operation"])
			}
		}
		flush()
	}
}

// runNative performs a helm or kustomize solution action the way a learner
// does, with the real tools in the attempt directory. The author executor
// refuses these actions outside its validation stage session (it requires
// author.SessionID in verifyToolNamespace), so a real session's model
// solution cannot go through it.
func (h *cliHarness) runNative(ctx context.Context, label, kind string, args map[string]any, attempt, namespace, kubeContext string) {
	t := h.t
	t.Helper()
	var cmd *exec.Cmd
	switch kind {
	case model.ActionKustomize:
		path, _ := args["path"].(string)
		cmd = exec.CommandContext(ctx, "kubectl", "--kubeconfig", h.kubeconfig(), "--context", kubeContext, "--namespace", namespace, "apply", "-k", path)
	case model.ActionHelm:
		release, _ := args["release"].(string)
		chart, _ := args["chart"].(string)
		if args["operation"] != "install" || release == "" || chart == "" {
			t.Fatalf("solve %s: unsupported helm action %v", label, args)
		}
		argv := []string{"install", release, chart}
		if raw, present := args["values"]; present {
			values, ok := raw.([]any)
			if !ok {
				t.Fatalf("solve %s: helm values must be a list, got %T", label, raw)
			}
			for _, v := range values {
				argv = append(argv, "--values", fmt.Sprint(v))
			}
		}
		argv = append(argv, "--namespace", namespace, "--kubeconfig", h.kubeconfig(), "--kube-context", kubeContext)
		cmd = exec.CommandContext(ctx, "helm", argv...)
		// Never touch the learner's real Helm cache, config or data.
		cmd.Env = append(os.Environ(),
			"HELM_CACHE_HOME="+t.TempDir(), "HELM_CONFIG_HOME="+t.TempDir(), "HELM_DATA_HOME="+t.TempDir())
	default:
		t.Fatalf("solve %s: unsupported native action %s", label, kind)
	}
	cmd.Dir = attempt
	t.Logf("solve %s: native %s", label, strings.Join(cmd.Args, " "))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("solve %s: %s: %v\n%s", label, kind, err, out)
	}
}

// applyFrozenObjects applies the apply-only solution objects of the current
// session through kubectl, the way TestCLIFlow supplies its answers.
func (h *cliHarness) applyFrozenObjects(ctx context.Context) {
	t := h.t
	t.Helper()
	st, err := store.Open(filepath.Join(h.state, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	m := environment.New(h.state, st)
	id, err := m.RecordedIdentity()
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	sid, err := st.CurrentSessionID(ctx)
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	record, err := st.LoadSession(ctx, sid)
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	// The test author opens the private frozen references to supply answers;
	// ordinary learner commands never disclose them. Render typed parameters
	// exactly as validation does, rather than applying raw templates.
	for _, frozen := range record.Problems {
		pkg, err := session.LoadPackage(h.state, frozen.Hash)
		if err != nil {
			t.Fatal(err)
		}
		actions, err := author.BuildActions(pkg.Files, frozen.Parameters, frozen.Definition.Solution)
		if err != nil {
			t.Fatal(err)
		}
		for _, action := range actions {
			for _, object := range action.Objects {
				data, err := json.Marshal(object)
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", m.KubeconfigPath(), "--context", id.Context, "apply", "-f", "-")
				cmd.Stdin = bytes.NewReader(data)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("solve %s: %v\n%s", frozen.Definition.ID, err, out)
				}
			}
		}
	}
}

// copyDir copies regular files and directories, never symlinks (the tests
// packages cannot import each other's helpers; see tests/integration).
func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("copyDir: %s is not a regular file", path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}
