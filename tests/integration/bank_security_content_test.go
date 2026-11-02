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

var securityIDs = []string{"custom-resource-001", "access-001", "resources-001", "configuration-001", "security-001"}

// securityCases maps problem ID -> case ID -> expected score. Each package adds
// its row with its task; the table pins the exact case set of each package.
var securityCases = map[string]map[string]int{}

// securityCapabilities and securityInitialScores pin each package's declared
// capabilities and the score of the untouched environment.
var securityCapabilities = map[string][]string{}

var securityInitialScores = map[string]int{}

// presentSecurityIDs returns the security IDs whose package exists, so the
// bank tests grow with the packages instead of failing before they are added.
func presentSecurityIDs(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, id := range securityIDs {
		_, err := os.Stat(filepath.Join(bankRoot, "problems", id))
		switch {
		case err == nil:
			out = append(out, id)
		case !os.IsNotExist(err):
			t.Fatal(err)
		}
	}
	return out
}

func TestSecurityProblems(t *testing.T) {
	for _, p := range loadGroup(t, presentSecurityIDs(t)) {
		t.Run(p.ID, func(t *testing.T) {
			testutil.Equal(t, p.Domain, model.DomainEnvironmentSecurity)
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
			want, ok := securityCases[p.ID]
			if !ok {
				t.Fatalf("securityCases has no row for %s", p.ID)
			}
			for id, score := range want {
				testutil.Equal(t, caseScore(p, id), score)
			}
			testutil.Equal(t, len(p.Cases), len(want))
			testutil.Equal(t, initialBelowMax(p), true)
			testutil.Equal(t, p.InitialScore, securityInitialScores[p.ID])
			if want := securityCapabilities[p.ID]; !slices.Equal(p.Capabilities, want) {
				t.Errorf("capabilities = %v, want %v", p.Capabilities, want)
			}
			for id, score := range want {
				if !strings.HasPrefix(id, "alternative-") && score >= 5 {
					t.Fatalf("wrong case %s earns full credit", id)
				}
			}
			// Full status vectors: every case states every criterion.
			for _, c := range p.Cases {
				testutil.Equal(t, len(c.Expected), len(p.Criteria))
				for _, crit := range p.Criteria {
					switch c.Expected[crit.ID] {
					case model.StatusPass, model.StatusFail, model.StatusError:
					default:
						t.Errorf("case %s has no status for criterion %s", c.ID, crit.ID)
					}
				}
			}
			// Ruling R1: every http criterion observes for 60 seconds.
			for _, c := range p.Criteria {
				if c.Kind == model.CriterionHTTP {
					testutil.Equal(t, c.Timeout, 60*time.Second)
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

func TestSecurityBankPreflight(t *testing.T) {
	ids := presentSecurityIDs(t)
	if len(ids) == 0 {
		t.Skip("no environment-security package yet")
	}
	preflightBank(t, loadGroup(t, ids))
}
