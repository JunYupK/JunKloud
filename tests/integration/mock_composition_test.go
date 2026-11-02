//go:build linux

package integration

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// mock01Order is the reviewed exam order: domains interleave (no two
// neighbours share a domain), the two easy problems come first and the two
// hard ones last.
var mock01Order = []string{
	"configuration-001", "service-001", "custom-resource-001", "batch-001", "probes-001",
	"rollout-001", "ingress-001", "multicontainer-001", "diagnostics-001", "release-001",
	"access-001", "daemonset-001", "api-migration-001", "kustomize-001", "volume-001",
	"resources-001", "helm-001", "image-001", "security-001", "networkpolicy-001",
}

// bankProblemIDs lists every package directory of the bank, sorted.
func bankProblemIDs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(bankRoot, "problems"))
	testutil.NoError(t, err)
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	slices.Sort(ids)
	return ids
}

func domainPoints(problems []model.Problem) map[string]int {
	out := map[string]int{}
	for _, p := range problems {
		out[p.Domain] += sumPoints([]model.Problem{p})
	}
	return out
}

func typesPresent(problems []model.Problem) []string {
	set := map[string]bool{}
	for _, p := range problems {
		set[p.Type] = true
	}
	var out []string
	for ty := range set {
		out = append(out, ty)
	}
	slices.Sort(out)
	return out
}

// TestMockComposition pins the reviewed mock-01 against the spec numbers
// (section 7.2): 20 problems, 120 minutes, 100 points, domain points
// 20/20/15/25/20, all four problem types, every bank problem exactly once.
func TestMockComposition(t *testing.T) {
	exam, err := problem.LoadExam(filepath.Join(bankRoot, "exams", "mock-01.yaml"))
	testutil.NoError(t, err)
	testutil.Equal(t, exam.ID, "mock-01")
	testutil.Equal(t, exam.Duration, 120*time.Minute)
	testutil.Equal(t, exam.Problems, mock01Order)

	problems := loadGroup(t, exam.Problems)
	testutil.Equal(t, len(problems), 20)
	testutil.Equal(t, sumPoints(problems), 100)
	testutil.Equal(t, domainPoints(problems), map[string]int{
		"design-build": 20, "deployment": 20, "observability": 15, "environment-security": 25, "networking": 20,
	})
	testutil.Equal(t, typesPresent(problems), []string{"behavioral", "build", "modify", "troubleshoot"})

	// Distinct IDs, and exactly the bank: nothing missing, nothing extra.
	seen := map[string]bool{}
	for _, p := range problems {
		if seen[p.ID] {
			t.Errorf("%s is listed twice", p.ID)
		}
		seen[p.ID] = true
	}
	testutil.Equal(t, slices.Sorted(slices.Values(exam.Problems)), bankProblemIDs(t))

	// Order: domains interleave, easy first, hard last.
	for i := 1; i < len(problems); i++ {
		if problems[i].Domain == problems[i-1].Domain {
			t.Errorf("%s and %s are neighbours in domain %s", problems[i-1].ID, problems[i].ID, problems[i].Domain)
		}
	}
	for i, p := range problems {
		if p.Difficulty == "easy" && i >= 3 {
			t.Errorf("easy problem %s is at position %d, after the first three", p.ID, i+1)
		}
		if p.Difficulty == "hard" && i < len(problems)-2 {
			t.Errorf("hard problem %s is at position %d, before the last two", p.ID, i+1)
		}
	}

	// Footprint for the record: the summed budgets against the 4 GiB target.
	var cpu, mem int64
	for _, p := range problems {
		cpu += p.Budget.CPUMillis
		mem += p.Budget.MemoryBytes
	}
	t.Logf("mock-01 summed budget: cpu %dm, memory %d MiB (%.2f GiB) over %d problems; per-problem budgets follow",
		cpu, mem>>20, float64(mem)/(1<<30), len(problems))
	for _, p := range problems {
		t.Logf("  %-20s %-20s %-12s %-12s %4dm %4d MiB", p.ID, p.Domain, p.Type, p.Difficulty, p.Budget.CPUMillis, p.Budget.MemoryBytes>>20)
	}
}
