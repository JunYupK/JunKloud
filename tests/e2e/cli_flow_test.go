//go:build linux && e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
)

// TestCLIFlow deliberately fails when a requested live test lacks its tools
// or runtime. It builds and executes the real binary in separate processes,
// validates all five development fixtures with the real runtime grader, then
// solves their live resources using kubectl. No fake CLI or cluster is used.
func TestCLIFlow(t *testing.T) {
	verifyFixtureSolutions(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	bank := filepath.Join(repoRoot(t), "tests", "fixtures", "dev-bank")
	h := newHarness(t, bank)
	run := func(input string, want int, args ...string) string {
		t.Helper()
		out, _ := h.run(input, want, args...)
		return out
	}
	decoy := filepath.Join(filepath.Dir(h.state), "decoy-kubeconfig")
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
	h.applyFrozenObjects(ctx)
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
