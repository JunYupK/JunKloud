//go:build linux

package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/JunYupK/JunKloud/internal/testutil"
	"sigs.k8s.io/yaml"
)

// Exercise Validator's entire static action/criteria/image/coverage preflight,
// stopping at Prepare before any runtime effects. The actual live test does not
// replace or bypass this gate.
func TestBuildBankPreflight(t *testing.T) {
	preflightBank(t, loadGroup(t, buildIDs))
}

func preflightBank(t *testing.T, ps []model.Problem) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "sessions.db"))
	testutil.NoError(t, err)
	defer st.Close()
	m := environment.New(dir, st)
	checks := grader.DefaultCheckers(nil)
	artifacts := grader.NewArtifactChecker(grader.ArtifactConfig{})
	defer artifacts.Close()
	grader.AddArtifactCheckers(checks, artifacts)
	grader.AddNetworkCheckers(checks, grader.NewNetworkChecker(nil, nil))
	engine := grader.NewEngine(checks)
	stop := errors.New("static preflight complete; no runtime effects")
	v, err := author.NewValidator(author.Config{Identity: model.Identity{ID: "0123456789ab"}, Env: m, Janitor: environment.ManagedJanitor{Manager: m}, Engine: engine, Store: st,
		LockPath: author.LockPath(dir), GraderVersion: grader.GraderVersion, EnvironmentHash: "static-preflight", ProvidedImages: []string{environment.ProbeImage.Name, environment.ProbeImage.Ref()},
		ToolActions: author.ToolActionConfig{Tools: grader.NewToolRunner(), Reader: offlineBankReader{t}, KubeconfigPath: "/unused-offline", Context: "offline"},
		Prepare:     func(context.Context) error { return stop }})
	testutil.NoError(t, err)
	for _, p := range ps {
		t.Run(p.ID, func(t *testing.T) {
			seeds, err := author.CoveringSeeds(p)
			testutil.NoError(t, err)
			_, err = v.Validate(context.Background(), p, seeds)
			if !errors.Is(err, stop) {
				t.Fatalf("preflight did not reach runtime boundary: %v", err)
			}
		})
	}
}

var buildIDs = []string{"image-001", "batch-001", "daemonset-001", "multicontainer-001", "volume-001"}
var bankRoot = filepath.Join("..", "..")

// The native offline archive is imported under Name; the manifest-list digest
// remains pinned by EnvironmentManifest, but is not a runnable node alias.
func TestBuildBankOfflineWorkloadImages(t *testing.T) {
	for _, p := range loadGroup(t, buildIDs) {
		seeds, err := author.CoveringSeeds(p)
		testutil.NoError(t, err)
		for _, seed := range seeds {
			f, err := problem.Freeze(p, seed)
			testutil.NoError(t, err)
			groups := [][]model.Action{p.Setup, p.Solution}
			for _, c := range p.Cases {
				groups = append(groups, c.Actions)
			}
			for _, group := range groups {
				for _, a := range group {
					if a.Kind != model.ActionApply {
						continue
					}
					built, err := author.BuildActions(p.Files, f.Parameters, []model.Action{a})
					testutil.NoError(t, err)
					for _, step := range built {
						for _, o := range step.Objects {
							for _, image := range author.ObjectImages(o) {
								if image != environment.ProbeImage.Name {
									t.Errorf("%s seed=%d: workload image %q is not the native offline provided alias %q", p.ID, seed, image, environment.ProbeImage.Name)
								}
							}
						}
					}
				}
			}
		}
	}
}

func loadGroup(t *testing.T, ids []string) []model.Problem {
	t.Helper()
	var out []model.Problem
	for _, id := range ids {
		p, err := problem.Load(filepath.Join(bankRoot, "problems", id))
		testutil.NoError(t, err)
		testutil.Equal(t, p.ID, id)
		out = append(out, p)
	}
	return out
}
func sumPoints(problems []model.Problem) int {
	total := 0
	for _, p := range problems {
		for _, c := range p.Criteria {
			total += c.Points
		}
	}
	return total
}

func TestBuildBankContent(t *testing.T) {
	ps := loadGroup(t, buildIDs)
	testutil.Equal(t, len(ps), 5)
	testutil.Equal(t, sumPoints(ps), 20)
	for _, p := range ps {
		t.Run(p.ID, func(t *testing.T) {
			testutil.Equal(t, p.Domain, model.DomainDesignBuild)
			testutil.Equal(t, sumPoints([]model.Problem{p}), 4)
			if p.Difficulty == "" || p.Type == "" {
				t.Fatal("explicit difficulty/type required")
			}
			seeds, err := author.CoveringSeeds(p)
			testutil.NoError(t, err)
			if len(seeds) < 2 {
				t.Fatal("at least two parameter variants must be exercised")
			}
			wrong, alt := 0, 0
			for _, c := range p.Cases {
				if c.ExpectedScore < 4 {
					wrong++
				} else {
					alt++
				}
			}
			if wrong < 2 || alt < 1 {
				t.Fatalf("need multiple wrong cases and an alternative: wrong=%d alt=%d", wrong, alt)
			}
			for _, seed := range seeds {
				f, err := problem.Freeze(p, seed)
				testutil.NoError(t, err)
				if f.Question == "" || len(f.Assets) == 0 {
					t.Fatal("question and provided scaffold required")
				}
			}
		})
	}
	if _, err := problem.Load(filepath.Join(bankRoot, "problems", "missing-build-package")); err == nil {
		t.Fatal("missing package accepted")
	}
}

func TestBuildCoverage(t *testing.T) {
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
	var index = -1
	for i, g := range cov.Groups {
		if g.Domain == model.DomainDesignBuild {
			if index >= 0 {
				t.Fatal("duplicate design-build coverage")
			}
			index = i
		}
	}
	if index < 0 {
		t.Fatal("missing design-build coverage")
	}
	g := cov.Groups[index]
	testutil.Equal(t, g.ID, "design-build")
	testutil.Equal(t, g.Domain, model.DomainDesignBuild)
	testutil.Equal(t, g.Count, 5)
	testutil.Equal(t, g.Points, 20)
	got, want := slices.Clone(g.Problems), slices.Clone(buildIDs)
	slices.Sort(got)
	slices.Sort(want)
	testutil.Equal(t, got, want)
}
