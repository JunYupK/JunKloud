//go:build linux

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

var observabilityIDs = []string{"api-migration-001", "probes-001", "diagnostics-001"}

// observabilityCases maps problem ID -> case ID -> expected score. A problem
// joins the bank only once its row exists here, so each task extends the table
// together with its package. Task 4 removes the "present" guard.
var observabilityCases = map[string]map[string]int{
	"api-migration-001": {
		"apiversion-only":               0,
		"fixed-not-applied":             2,
		"wrong-hpa-target":              3,
		"selector-drops-tier":           4,
		"alternative-match-expressions": 5,
	},
	"probes-001": {
		"probes-deleted":            2,
		"readiness-port-kept":       3,
		"liveness-delay-no-startup": 4,
		"alternative-named-port":    5,
	},
}

// presentObservabilityIDs are the bank IDs whose package has been authored.
func presentObservabilityIDs() []string {
	var ids []string
	for _, id := range observabilityIDs {
		if _, ok := observabilityCases[id]; ok {
			ids = append(ids, id)
		}
	}
	return ids
}

func caseScore(p model.Problem, id string) int {
	for _, c := range p.Cases {
		if c.ID == id {
			return c.ExpectedScore
		}
	}
	return -1
}

func initialBelowMax(p model.Problem) bool {
	return p.InitialScore < sumPoints([]model.Problem{p})
}

func TestObservabilityProblems(t *testing.T) {
	for _, p := range loadGroup(t, presentObservabilityIDs()) {
		t.Run(p.ID, func(t *testing.T) {
			testutil.Equal(t, p.Domain, model.DomainObservability)
			testutil.Equal(t, sumPoints([]model.Problem{p}), 5)
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
			for id, want := range observabilityCases[p.ID] {
				testutil.Equal(t, caseScore(p, id), want)
			}
			testutil.Equal(t, len(p.Cases), len(observabilityCases[p.ID]))
			testutil.Equal(t, initialBelowMax(p), true)
			for id, want := range observabilityCases[p.ID] {
				if !strings.HasPrefix(id, "alternative-") && want >= 5 {
					t.Fatalf("wrong case %s earns full credit", id)
				}
			}
			for _, c := range p.Cases {
				testutil.Equal(t, len(c.Expected), len(p.Criteria))
			}
			// Ruling R1: every http criterion observes for 60 seconds.
			for _, c := range p.Criteria {
				if c.Kind == model.CriterionHTTP {
					testutil.Equal(t, c.Timeout, 60*time.Second)
				}
			}
			// Private file sources are copied raw, so they must not rely on
			// parameter rendering. Every apply manifest, wherever it is used
			// (setup, solution or case), must also avoid the unserved legacy
			// GroupVersions.
			groups := [][]model.Action{p.Setup, p.Solution}
			for _, c := range p.Cases {
				groups = append(groups, c.Actions)
			}
			for _, g := range groups {
				for _, a := range g {
					switch a.Kind {
					case model.ActionFile:
						src, _ := a.Args["source"].(string)
						b, ok := p.Files[src]
						if !ok {
							t.Fatalf("file source %q is not part of the package", src)
						}
						if strings.Contains(string(b), "${") {
							t.Errorf("file source %s contains an unrendered ${param}", src)
						}
					case model.ActionApply:
						path, _ := a.Args["path"].(string)
						b, ok := p.Files[path]
						if !ok {
							t.Fatalf("apply manifest %q is not part of the package", path)
						}
						for _, legacy := range []string{"extensions/v1beta1", "v1beta1", "v2beta1", "v2beta2"} {
							if strings.Contains(string(b), legacy) {
								t.Errorf("apply manifest %s uses unserved API version %q", path, legacy)
							}
						}
					}
				}
			}
		})
	}
}

func TestObservabilityBankPreflight(t *testing.T) {
	preflightBank(t, loadGroup(t, presentObservabilityIDs()))
}

// The legacy manifests are only ever workspace files: the server rejects both
// unserved GVKs, and apply of one would be an author error.
func TestObservabilityAPIMigrationContracts(t *testing.T) {
	p := loadGroup(t, []string{"api-migration-001"})[0]
	legacy := string(p.Files["setup/provided/legacy/app.yaml"])
	for _, want := range []string{"extensions/v1beta1", "autoscaling/v2beta1", "targetAverageUtilization"} {
		if !strings.Contains(legacy, want) {
			t.Errorf("legacy manifest lacks %q", want)
		}
	}
	if strings.Contains(legacy, "namespace:") {
		t.Error("legacy manifest must not hard-code a namespace")
	}
	for _, a := range append(append([]model.Action{}, p.Setup...), p.Solution...) {
		if a.Kind == model.ActionApply && a.Args["path"] == "setup/provided/legacy/app.yaml" {
			t.Error("legacy manifest must never be applied")
		}
	}
	for _, c := range p.Cases {
		for _, a := range c.Actions {
			if a.Kind != model.ActionApply {
				continue
			}
			b, err := os.ReadFile(filepath.Join(bankRoot, "problems", p.ID, filepath.FromSlash(a.Args["path"].(string))))
			testutil.NoError(t, err)
			if strings.Contains(string(b), "v1beta1") {
				t.Errorf("case %s applies a manifest with an unserved API", c.ID)
			}
		}
	}
}
