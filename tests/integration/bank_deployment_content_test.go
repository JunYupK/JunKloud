//go:build linux

package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/testutil"
	"sigs.k8s.io/yaml"
)

var deploymentIDs = []string{"rollout-001", "release-001", "helm-001", "kustomize-001"}

// Removing a package, weakening a bypass case, or allowing incomplete rollout
// certification must fail before spending time on a real cluster.
func TestDeploymentBankContent(t *testing.T) {
	ps := loadGroup(t, deploymentIDs)
	testutil.Equal(t, len(ps), 4)
	testutil.Equal(t, sumPoints(ps), 20)
	required := map[string][]string{
		"rollout-001":   {"wrong-version", "wrong-strategy", "incomplete-rollout", "alternative-output"},
		"release-001":   {"missing-stable", "missing-canary", "stable-only-selector", "wrong-canary-body", "alternative-selector"},
		"helm-001":      {"raw-apply", "wrong-values", "live-drift", "alternative-values"},
		"kustomize-001": {"changed-base", "base-bypass", "raw-apply", "wrong-replicas", "alternative-overlay"},
	}
	for _, p := range ps {
		t.Run(p.ID, func(t *testing.T) {
			testutil.Equal(t, p.Domain, model.DomainDeployment)
			testutil.Equal(t, sumPoints([]model.Problem{p}), 5)
			seeds, err := author.CoveringSeeds(p)
			testutil.NoError(t, err)
			if len(seeds) < 2 {
				t.Fatal("two covering variants required")
			}
			for _, seed := range seeds {
				f, err := problem.Freeze(p, seed)
				testutil.NoError(t, err)
				if f.Question == "" || len(f.Assets) == 0 {
					t.Fatal("public question and scaffold required")
				}
			}
			for _, id := range required[p.ID] {
				found := false
				for _, c := range p.Cases {
					if c.ID != id {
						continue
					}
					found = true
					if !strings.HasPrefix(id, "alternative-") && c.ExpectedScore >= 5 {
						t.Fatalf("bypass/wrong case %s earns full credit", id)
					}
					if len(c.Expected) != len(p.Criteria) {
						t.Fatalf("incomplete status vector for %s", id)
					}
				}
				if !found {
					t.Fatalf("missing required case %s", id)
				}
			}
		})
	}
}

// Static preflight must never ask for cluster state. Native renders still run
// through the real pinned tools and the real ordered author preflight.
type offlineBankReader struct{ t *testing.T }

func (r offlineBankReader) Get(context.Context, model.ObjectRef) (map[string]any, error) {
	r.t.Fatal("offline preflight queried API")
	return nil, nil
}
func (r offlineBankReader) List(context.Context, grader.ListQuery) ([]map[string]any, error) {
	r.t.Fatal("offline preflight listed API")
	return nil, nil
}

func TestDeploymentBankPreflight(t *testing.T) { preflightBank(t, loadGroup(t, deploymentIDs)) }

func TestDeploymentReleaseContracts(t *testing.T) {
	p := loadGroup(t, []string{"release-001"})[0]
	f, err := problem.Freeze(p, 1)
	testutil.NoError(t, err)
	for _, g := range append([][]model.Action{p.Setup, p.Solution}, func() [][]model.Action {
		var groups [][]model.Action
		for _, c := range p.Cases {
			groups = append(groups, c.Actions)
		}
		return groups
	}()...) {
		for _, a := range g {
			if a.Kind != model.ActionApply {
				continue
			}
			built, err := author.BuildActions(p.Files, f.Parameters, []model.Action{a})
			testutil.NoError(t, err)
			for _, step := range built {
				for _, o := range step.Objects {
					if o["kind"] != "Service" {
						continue
					}
					for key, value := range o["spec"].(map[string]any)["selector"].(map[string]any) {
						if value == nil || value == "" {
							t.Errorf("%s selector %s uses null/empty string: native Service string maps retain an empty label requirement", a.Args["path"], key)
						}
					}
				}
			}
		}
	}
	for _, path := range []string{problem.QuestionFile, "setup/provided/README.txt"} {
		if !strings.Contains(string(p.Files[path]), "backend port is flexible") {
			t.Errorf("%s must disclose flexible backend port", path)
		}
	}
	for _, c := range p.Cases {
		if c.ID == "alternative-selector" {
			built, err := author.BuildActions(p.Files, f.Parameters, c.Actions)
			testutil.NoError(t, err)
			var workloads, services int
			for _, step := range built {
				for _, o := range step.Objects {
					spec := o["spec"].(map[string]any)
					switch o["kind"] {
					case "Deployment":
						container := spec["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
						if !strings.Contains(container["command"].([]any)[2].(string), "9090") {
							t.Error("alternative must actually serve on9090")
						}
						workloads++
					case "Service":
						port := spec["ports"].([]any)[0].(map[string]any)
						testutil.Equal(t, port["targetPort"], any("backend"))
						services++
					}
				}
			}
			testutil.Equal(t, workloads, 2)
			testutil.Equal(t, services, 1)
		}
	}
}

func TestDeploymentCoverage(t *testing.T) {
	var cov struct {
		Groups []struct {
			ID       string   `json:"id"`
			Domain   string   `json:"domain"`
			Count    int      `json:"count"`
			Points   int      `json:"points"`
			Problems []string `json:"problems"`
		} `json:"groups"`
	}
	b, err := os.ReadFile(filepath.Join(bankRoot, "curriculum", "coverage.yaml"))
	testutil.NoError(t, err)
	testutil.NoError(t, yaml.UnmarshalStrict(b, &cov))
	seen := map[string]bool{}
	count, points, deployment := 0, 0, 0
	for _, g := range cov.Groups {
		if g.Domain == model.DomainDeployment {
			deployment++
			testutil.Equal(t, g.Count, 4)
			testutil.Equal(t, g.Points, 20)
		}
		testutil.Equal(t, len(g.Problems), g.Count)
		ps := loadGroup(t, g.Problems)
		testutil.Equal(t, sumPoints(ps), g.Points)
		for _, p := range ps {
			if seen[p.ID] {
				t.Fatalf("duplicate coverage %s", p.ID)
			}
			seen[p.ID] = true
			testutil.Equal(t, p.Domain, g.Domain)
		}
		count += g.Count
		points += g.Points
	}
	testutil.Equal(t, deployment, 1)
	testutil.Equal(t, count, 9)
	testutil.Equal(t, points, 40)
	testutil.Equal(t, problem.PracticeMinCount, 5)
	_, err = problem.Select(loadGroup(t, deploymentIDs), problem.SelectOptions{Mode: model.ModePractice, Count: 4, Seed: 1})
	if !errors.Is(err, problem.ErrInvalidCount) {
		t.Fatalf("practice4 accepted: %v", err)
	}
}

// Read-only semantic checks catch both wrong Kustomize bypasses and equivalent
// overlays for every covering seed before any real cluster is created.
func TestDeploymentOverlayRenders(t *testing.T) {
	p := loadGroup(t, []string{"kustomize-001"})[0]
	seeds, err := author.CoveringSeeds(p)
	testutil.NoError(t, err)
	groups := []struct {
		name     string
		actions  []model.Action
		expected map[string]string
	}{{"initial", nil, p.InitialExpected}, {"solved", p.Solution, nil}}
	for _, c := range p.Cases {
		groups = append(groups, struct {
			name     string
			actions  []model.Action
			expected map[string]string
		}{c.ID, c.Actions, c.Expected})
	}
	for _, seed := range seeds {
		for _, g := range groups {
			t.Run(g.name+"/"+strconv.FormatUint(seed, 10), func(t *testing.T) {
				f, err := problem.Freeze(p, seed)
				testutil.NoError(t, err)
				ws, remove, err := author.ScratchWorkspace(t.TempDir(), f)
				testutil.NoError(t, err)
				defer remove()
				for _, a := range g.actions {
					if a.Kind == model.ActionFile {
						testutil.NoError(t, os.WriteFile(filepath.Join(ws, a.Args["path"].(string)), p.Files[a.Args["source"].(string)], 0600))
					}
				}
				checker := grader.NewArtifactChecker(grader.ArtifactConfig{Tools: grader.NewToolRunner()})
				defer checker.Close()
				for _, c := range f.Definition.Criteria {
					if c.ID != "base-intact" && c.ID != "deployment-render" && c.ID != "config-render" {
						continue
					}
					r, err := checker.Check(context.Background(), grader.Target{Problem: f, Workspace: ws}, c)
					testutil.NoError(t, err)
					want := model.StatusPass
					if g.expected != nil {
						want = g.expected[c.ID]
					}
					if r.Status != want {
						t.Fatalf("%s got %s want %s: %s", c.ID, r.Status, want, r.Reason)
					}
				}
			})
		}
	}
}
