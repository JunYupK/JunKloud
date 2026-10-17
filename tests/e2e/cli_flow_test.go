//go:build linux && e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/lock"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/store"
)

// TestCLIFlow deliberately fails when a requested live test lacks its tools
// or runtime. It builds and executes the real binary in separate processes,
// validates all five development fixtures with the real runtime grader, then
// solves their live resources using kubectl. No fake CLI or cluster is used.
func TestCLIFlow(t *testing.T) {
	verifyFixtureSolutions(t)
	for _, name := range []string{"go", "docker", "kind", "kubectl"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatalf("live CLI E2E requires %s: %v", name, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	binary := filepath.Join(tmp, "ckad")
	state := filepath.Join(tmp, "state")
	workspace := filepath.Join(tmp, "work")
	bank := filepath.Join(root, "tests", "fixtures", "dev-bank")
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
	dockerFailures := filepath.Join(tmp, "docker-failures.txt")
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
	t.Setenv("CKAD_E2E_DOCKER_FAILURES", dockerFailures)
	t.Setenv("PATH", toolDir+":"+os.Getenv("PATH"))
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/ckad")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	t.Setenv("CKAD_STATE_DIR", state)
	t.Setenv("CKAD_WORKSPACE", workspace)
	t.Setenv("CKAD_BANK_DIR", bank)
	t.Setenv("CKAD_HOST_COMPAT", os.Getenv("JUNKLOUD_TEST_HOST_COMPAT"))
	// A decoy verifies the flow never adopts the ambient host configuration.
	decoy := filepath.Join(tmp, "decoy-kubeconfig")
	if err := os.WriteFile(decoy, []byte("deliberately not a kubeconfig\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", decoy)
	t.Cleanup(func() {
		st, err := store.Open(filepath.Join(state, "sessions.db"))
		if err != nil {
			t.Errorf("cleanup store: %v", err)
			return
		}
		defer st.Close()
		m := environment.New(state, st)
		id, err := m.RecordedIdentity()
		if err != nil {
			t.Logf("no recorded E2E cluster: %v", err)
			return
		}
		cleanupCtx, stop := context.WithTimeout(context.Background(), 2*time.Minute)
		defer stop()
		release, err := lock.Acquire(author.LockPath(state))
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
	})
	run := func(input string, want int, args ...string) string {
		t.Helper()
		t.Logf("running ckad %s", strings.Join(args, " "))
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = root
		cmd.Stdin = strings.NewReader(input)
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
		t.Logf("ckad %s; input=%q; exit=%d; stdout=%s; stderr=%s", strings.Join(args, " "), input, code, out.String(), diag.String())
		if code != want {
			if diagnostics, err := os.ReadFile(dockerFailures); err == nil {
				t.Logf("external Docker failures:\n%s", diagnostics)
			}
			t.Fatalf("expected exit %d, got %d", want, code)
		}
		return out.String()
	}
	run("", 0, "--help")
	ids := []string{"dev-config-001", "dev-crashloop-001", "dev-sa-001", "dev-scale-001", "dev-service-001"}
	for _, id := range ids {
		run("", 0, "bank", "validate", id)
	}
	listing := run("", 0, "list")
	for _, id := range ids {
		if !strings.Contains(listing, id) {
			t.Fatalf("certified problem absent: %s", id)
		}
	}
	run("yes\n", 0, "start", "--mode", "practice", "--count", "5", "--seed", "42")
	status := run("", 0, "status")
	for _, id := range ids {
		if !strings.Contains(status, id) {
			t.Fatalf("problem absent from session: %s", id)
		}
	}
	question := run("", 0, "show", "Q4")
	if !strings.Contains(question, "Namespace") || strings.Contains(question, "DEV-SOLUTION") {
		t.Fatal("question contract", question)
	}
	run("", 0, "mark", "Q4", "--review")
	run("", 1, "grade")
	st, err := store.Open(filepath.Join(state, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	m := environment.New(state, st)
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
	// ordinary learner commands above never disclose them. Render typed
	// parameters exactly as validation does, rather than applying raw templates.
	for _, frozen := range record.Problems {
		pkg, err := session.LoadPackage(state, frozen.Hash)
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
	graded := run("", 0, "grade", "--resubmit")
	if !strings.Contains(graded, "점수: 29/29") {
		t.Fatal("full live score missing", graded)
	}
	reviewed := run("", 0, "review")
	if strings.Contains(reviewed, "DEV-EXPLANATION") || strings.Contains(reviewed, "DEV-SOLUTION") {
		t.Fatal("solution was not explicitly requested")
	}
	reviewed = run("", 0, "review", "--problem", "Q4", "--solution")
	if !strings.Contains(reviewed, "DEV-EXPLANATION") || !strings.Contains(reviewed, "DEV-SOLUTION") {
		t.Fatal("explicit solution missing")
	}
	unchanged, err := os.ReadFile(decoy)
	if err != nil || string(unchanged) != "deliberately not a kubeconfig\n" {
		t.Fatal("ambient kubeconfig changed", fmt.Sprint(err))
	}
}

func TestFixtureSolutionPreflight(t *testing.T) { verifyFixtureSolutions(t) }

// Run before creating a live cluster so a broken solve harness fails cheaply.
// Expected namespaces come from the five reviewed fixtures, independently of
// the renderer. Both parameter variants and the live workflow's seed are used.
func verifyFixtureSolutions(t *testing.T) {
	t.Helper()
	bank, err := filepath.Abs("../fixtures/dev-bank")
	if err != nil {
		t.Fatal(err)
	}
	namespaces := map[string]string{"dev-crashloop-001": "dev-crash", "dev-sa-001": "dev-sec", "dev-scale-001": "dev-scale", "dev-service-001": "dev-svc", "dev-config-001": ""}
	for id, expectedNamespace := range namespaces {
		p, err := problem.Load(filepath.Join(bank, "problems", id))
		if err != nil {
			t.Fatal(err)
		}
		seeds, err := author.CoveringSeeds(p)
		if err != nil {
			t.Fatal(err)
		}
		seeds = append(seeds, 42)
		for _, seed := range seeds {
			f, err := problem.Freeze(p, seed)
			if err != nil {
				t.Fatal(err)
			}
			if id == "dev-config-001" {
				if f.Namespace != "dev-cfg-a" && f.Namespace != "dev-cfg-b" {
					t.Fatal("unexpected parameter namespace", f.Namespace)
				}
			} else if f.Namespace != expectedNamespace {
				t.Fatal(id, f.Namespace)
			}
			actions, err := author.BuildActions(p.Files, f.Parameters, f.Definition.Solution)
			if err != nil {
				t.Fatal(err)
			}
			if len(actions) == 0 {
				t.Fatal("fixture has no solution", id)
			}
			for _, action := range actions {
				if action.Kind != model.ActionApply || len(action.Objects) == 0 {
					t.Fatalf("solve harness requires apply objects for %s", id)
				}
				for _, object := range action.Objects {
					raw, err := json.Marshal(object)
					if err != nil {
						t.Fatal(err)
					}
					if bytes.Contains(raw, []byte("${")) {
						t.Fatalf("unrendered parameter in %s seed %d", id, seed)
					}
					metadata, ok := object["metadata"].(map[string]any)
					if !ok || metadata["namespace"] != f.Namespace {
						t.Fatalf("solution namespace differs from published target for %s", id)
					}
				}
			}
		}
	}
}
