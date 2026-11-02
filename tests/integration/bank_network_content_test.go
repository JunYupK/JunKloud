//go:build linux

package integration

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

var networkIDs = []string{"service-001", "ingress-001", "networkpolicy-001"}

// networkPending lists the networking packages that a later task adds. A
// pending package may be missing; every other package must exist. The task
// that adds a package removes its ID here (TestNetworkTablesHavePackages
// fails while an existing package is still listed as pending).
var networkPending = []string{"service-001", "ingress-001", "networkpolicy-001"}

// networkPoints pins the points of each networking problem.
var networkPoints = map[string]int{
	"service-001":       6,
	"ingress-001":       6,
	"networkpolicy-001": 8,
}

// networkCases maps problem ID -> case ID -> expected score. Each package adds
// its row with its task; the table pins the exact case set of each package.
var networkCases = map[string]map[string]int{}

// networkCapabilities and networkInitialScores pin each package's declared
// capabilities and the score of the untouched environment.
var networkCapabilities = map[string][]string{
	"service-001":       nil,
	"ingress-001":       {"ingress"},
	"networkpolicy-001": {"network-policy"},
}

var networkInitialScores = map[string]int{}

// networkCompetencies are the three official items of spec section 3.1 that
// the networking domain covers, used exactly.
var networkCompetencies = []string{
	"NetworkPolicy",
	"Service access and troubleshooting",
	"Ingress",
}

// presentNetworkIDs returns the networking IDs whose package exists, so the
// bank tests grow with the packages instead of failing before they are added.
func presentNetworkIDs(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, id := range networkIDs {
		_, err := os.Stat(filepath.Join(bankRoot, "problems", id))
		switch {
		case err == nil:
			out = append(out, id)
		case !os.IsNotExist(err):
			t.Fatal(err)
		case !slices.Contains(networkPending, id):
			// Only a pending package may be missing.
			t.Errorf("problems/%s is missing and is not pending", id)
		}
	}
	return out
}

// A pending package may be missing; a package that is not pending must exist,
// and a pending one that exists must be removed from networkPending.
func TestNetworkTablesHavePackages(t *testing.T) {
	for _, id := range networkIDs {
		_, err := os.Stat(filepath.Join(bankRoot, "problems", id))
		pending := slices.Contains(networkPending, id)
		switch {
		case err == nil && pending:
			t.Errorf("problems/%s exists: remove it from networkPending", id)
		case err != nil && !pending:
			t.Errorf("problems/%s: %v", id, err)
		}
	}
	for _, id := range networkPending {
		if !slices.Contains(networkIDs, id) {
			t.Errorf("networkPending names %s which is not in networkIDs", id)
		}
	}
	for id := range networkCases {
		if !slices.Contains(networkIDs, id) {
			t.Errorf("networkCases has a row for %s which is not in networkIDs", id)
		}
	}
	for _, id := range networkIDs {
		if _, ok := networkPoints[id]; !ok {
			t.Errorf("networkPoints has no row for %s", id)
		}
		if _, ok := networkCapabilities[id]; !ok {
			t.Errorf("networkCapabilities has no row for %s", id)
		}
	}
	testutil.Equal(t, len(networkPoints), len(networkIDs))
	testutil.Equal(t, len(networkCapabilities), len(networkIDs))
}

// Every present networking package is a networking problem with the points of
// its row and uses only the official competency strings.
func TestNetworkCompetencies(t *testing.T) {
	for _, p := range loadGroup(t, presentNetworkIDs(t)) {
		if len(p.Competencies) == 0 {
			t.Errorf("%s declares no competency", p.ID)
		}
		for _, c := range p.Competencies {
			if !slices.Contains(networkCompetencies, c) {
				t.Errorf("%s uses competency %q which is not one of the 3 official items", p.ID, c)
			}
		}
	}
}

func TestNetworkProblems(t *testing.T) {
	for _, p := range loadGroup(t, presentNetworkIDs(t)) {
		t.Run(p.ID, func(t *testing.T) {
			testutil.Equal(t, p.Domain, model.DomainNetworking)
			testutil.Equal(t, sumPoints([]model.Problem{p}), networkPoints[p.ID])
			if p.Difficulty == "" || p.Type == "" {
				t.Fatal("explicit difficulty/type required")
			}
			seeds, err := author.CoveringSeeds(p)
			testutil.NoError(t, err)
			testutil.Equal(t, len(seeds) >= 2, true)
			for _, seed := range seeds {
				f, err := problem.Freeze(p, seed)
				testutil.NoError(t, err)
				if f.Question == "" || len(f.Assets) == 0 {
					t.Fatal("public question and scaffold required")
				}
			}
			want, ok := networkCases[p.ID]
			if !ok {
				t.Fatalf("networkCases has no row for %s", p.ID)
			}
			for id, score := range want {
				testutil.Equal(t, caseScore(p, id), score)
			}
			testutil.Equal(t, len(p.Cases), len(want))
			testutil.Equal(t, initialBelowMax(p), true)
			testutil.Equal(t, p.InitialScore, networkInitialScores[p.ID])
			if want := networkCapabilities[p.ID]; !slices.Equal(p.Capabilities, want) {
				t.Errorf("capabilities = %v, want %v", p.Capabilities, want)
			}
			full := networkPoints[p.ID]
			for id, score := range want {
				if !strings.HasPrefix(id, "alternative-") && score >= full {
					t.Fatalf("wrong case %s earns full credit", id)
				}
				if strings.HasPrefix(id, "alternative-") && score != full {
					t.Fatalf("alternative case %s must earn full credit, has %d of %d", id, score, full)
				}
			}
			// Full status vectors: the initial state and every case state
			// every criterion, and only PASS or FAIL is an acceptable
			// expectation (a validated stage never ends in ERROR).
			vectors := map[string]map[string]string{"initial": p.InitialExpected}
			for _, c := range p.Cases {
				m := map[string]string{}
				for id, st := range c.Expected {
					m[id] = string(st)
				}
				vectors["case "+c.ID] = m
			}
			for name, vec := range vectors {
				testutil.Equal(t, len(vec), len(p.Criteria))
				for _, crit := range p.Criteria {
					switch vec[crit.ID] {
					case string(model.StatusPass), string(model.StatusFail):
					default:
						t.Errorf("%s: expected status of criterion %s is %q, want PASS or FAIL (ERROR is never an expected status)", name, crit.ID, vec[crit.ID])
					}
				}
			}
			// Ruling R1: every network criterion declares 60 seconds (the
			// minimum is 45 s; 60 s for an Ingress) and depends on the
			// ready criterion of its target workload, because EndpointSlices
			// are read once and not waited for.
			byID := map[string]model.Criterion{}
			for _, c := range p.Criteria {
				byID[c.ID] = c
			}
			for _, c := range p.Criteria {
				switch c.Kind {
				case model.CriterionHTTP, model.CriterionTCP, model.CriterionNetworkPolicy:
					testutil.Equal(t, c.Timeout, 60*time.Second)
					if !dependsOnKind(byID, c, model.CriterionReady) {
						t.Errorf("network criterion %s does not depend on a ready criterion", c.ID)
					}
					if bad := reservedSourceLabels(c.Args); len(bad) > 0 {
						t.Errorf("network criterion %s uses reserved source labels %v", c.ID, bad)
					}
				}
			}
			// Private file sources are copied raw, so they must not rely on
			// parameter rendering.
			groups := [][]model.Action{p.Setup, p.Solution}
			for _, c := range p.Cases {
				groups = append(groups, c.Actions)
			}
			for _, g := range groups {
				for _, a := range g {
					if a.Kind != model.ActionFile {
						continue
					}
					src, _ := a.Args["source"].(string)
					b, ok := p.Files[src]
					if !ok {
						t.Fatalf("file source %q is not part of the package", src)
					}
					if strings.Contains(string(b), "${") {
						t.Errorf("file source %s contains an unrendered ${param}", src)
					}
				}
			}
		})
	}
}

// dependsOnKind reports whether c depends, directly or through other
// criteria, on a criterion of the given kind.
func dependsOnKind(byID map[string]model.Criterion, c model.Criterion, kind string) bool {
	seen := map[string]bool{}
	var walk func(model.Criterion) bool
	walk = func(c model.Criterion) bool {
		for _, d := range c.DependsOn {
			if seen[d] {
				continue
			}
			seen[d] = true
			dep, ok := byID[d]
			if !ok {
				continue
			}
			if dep.Kind == kind || walk(dep) {
				return true
			}
		}
		return false
	}
	return walk(c)
}

// reservedSourceLabels returns the junkloud.io/ source label keys of a network
// criterion's args (single source or expectations).
func reservedSourceLabels(args map[string]any) []string {
	var bad []string
	check := func(v any) {
		src, _ := v.(map[string]any)
		labels, _ := src["labels"].(map[string]any)
		for k := range labels {
			if strings.HasPrefix(k, "junkloud.io/") {
				bad = append(bad, k)
			}
		}
	}
	check(args["source"])
	exps, _ := args["expectations"].([]any)
	for _, e := range exps {
		em, _ := e.(map[string]any)
		check(em["source"])
	}
	return bad
}

func TestNetworkBankPreflight(t *testing.T) {
	preflightBank(t, loadGroup(t, presentNetworkIDs(t)))
}

// smokeNetworkCriteria returns the criteria the networking smoke evaluates and
// the IDs it leaves out. The smoke keeps every criterion of a networking
// problem; only when network-policy is unenforced does it drop the
// network-policy criteria and, transitively, every criterion that depends on
// one of them.
func smokeNetworkCriteria(all []model.Criterion, policyUnenforced bool) (kept []model.Criterion, dropped []string) {
	if !policyUnenforced {
		return all, nil
	}
	drop := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, c := range all {
			if drop[c.ID] {
				continue
			}
			isDropped := c.Kind == model.CriterionNetworkPolicy
			for _, d := range c.DependsOn {
				isDropped = isDropped || drop[d]
			}
			if isDropped {
				drop[c.ID] = true
				changed = true
			}
		}
	}
	for _, c := range all {
		if drop[c.ID] {
			dropped = append(dropped, c.ID)
		} else {
			kept = append(kept, c)
		}
	}
	return kept, dropped
}

func TestNetworkSmokeCriteriaFilter(t *testing.T) {
	ids := func(cs []model.Criterion) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.ID)
		}
		return out
	}
	all := []model.Criterion{
		{ID: "ready", Kind: model.CriterionReady},
		{ID: "allowed", Kind: model.CriterionHTTP, DependsOn: []string{"ready"}},
		{ID: "blocked", Kind: model.CriterionNetworkPolicy, DependsOn: []string{"ready"}},
		{ID: "after-blocked", Kind: model.CriterionField, DependsOn: []string{"blocked"}},
		{ID: "after-after", Kind: model.CriterionLogs, DependsOn: []string{"after-blocked", "ready"}},
		{ID: "independent", Kind: model.CriterionField},
	}
	kept, dropped := smokeNetworkCriteria(all, false)
	testutil.Equal(t, ids(kept), ids(all))
	testutil.Equal(t, len(dropped), 0)
	kept, dropped = smokeNetworkCriteria(all, true)
	testutil.Equal(t, ids(kept), []string{"ready", "allowed", "independent"})
	testutil.Equal(t, dropped, []string{"blocked", "after-blocked", "after-after"})
}
