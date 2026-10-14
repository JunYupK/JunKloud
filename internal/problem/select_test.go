package problem_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func bank(n int, mutate func(i int, p *model.Problem)) []model.Problem {
	out := make([]model.Problem, n)
	for i := range out {
		out[i] = model.Problem{
			ID:         fmt.Sprintf("p%02d", i+1),
			Domain:     model.DomainNetworking,
			Difficulty: model.DifficultyEasy,
			Tags:       []string{"service"},
		}
		if mutate != nil {
			mutate(i, &out[i])
		}
	}
	return out
}

func ids(ps []model.Problem) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}

func TestSelectInsufficient(t *testing.T) {
	b := bank(12, func(i int, p *model.Problem) {
		if i%2 == 1 {
			p.Domain = model.DomainDeployment
		}
	})
	// 6 networking problems cannot fill a 7-problem practice set: no
	// duplicates are used to make up the shortfall.
	got, err := problem.Select(b, problem.SelectOptions{Mode: model.ModePractice, Domain: model.DomainNetworking, Count: 7, Seed: 1})
	testutil.ErrorIs(t, err, problem.ErrInsufficientProblems)
	testutil.Equal(t, got, []model.Problem(nil))
	if !strings.Contains(err.Error(), "6 available") || !strings.Contains(err.Error(), "7 requested") {
		t.Fatalf("error %q does not report requested and available counts", err)
	}

	// Exactly as many as available works and every problem appears once.
	got, err = problem.Select(b, problem.SelectOptions{Mode: model.ModePractice, Domain: model.DomainNetworking, Count: 6, Seed: 1})
	testutil.NoError(t, err)
	seen := map[string]bool{}
	for _, p := range got {
		if seen[p.ID] || p.Domain != model.DomainNetworking {
			t.Fatalf("bad selection %v", ids(got))
		}
		seen[p.ID] = true
	}
	testutil.Equal(t, len(seen), 6)
}

func TestSelectFilters(t *testing.T) {
	b := bank(20, func(i int, p *model.Problem) {
		if i < 7 {
			p.Difficulty = model.DifficultyHard
		}
		if i%3 == 0 {
			p.Tags = []string{"ingress", "service"}
		}
	})
	got, err := problem.Select(b, problem.SelectOptions{Mode: model.ModePractice, Difficulty: model.DifficultyHard, Tag: "ingress", Count: 5, Seed: 9})
	testutil.ErrorIs(t, err, problem.ErrInsufficientProblems) // p01, p04, p07 only
	if !strings.Contains(err.Error(), "3 available") {
		t.Fatalf("error %q does not report 3 available", err)
	}
	got, err = problem.Select(b, problem.SelectOptions{Mode: model.ModeRetry, Difficulty: model.DifficultyHard, Tag: "ingress", Count: 3, Seed: 9})
	testutil.NoError(t, err)
	gotIDs := ids(got)
	for _, id := range []string{"p01", "p04", "p07"} {
		if !strings.Contains(strings.Join(gotIDs, ","), id) {
			t.Fatalf("selection %v misses %s", gotIDs, id)
		}
	}
}

func TestSelectCountBounds(t *testing.T) {
	b := bank(25, nil)
	cases := []struct {
		mode  string
		count int
		ok    bool
	}{
		{model.ModePractice, 4, false},
		{model.ModePractice, 5, true},
		{model.ModePractice, 20, true},
		{model.ModePractice, 21, false},
		{model.ModeRetry, 0, false},
		{model.ModeRetry, 1, true},
		{model.ModeRetry, 20, true},
		{model.ModeRetry, 21, false},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s-%d", c.mode, c.count), func(t *testing.T) {
			got, err := problem.Select(b, problem.SelectOptions{Mode: c.mode, Count: c.count, Seed: 3})
			if c.ok {
				testutil.NoError(t, err)
				testutil.Equal(t, len(got), c.count)
				return
			}
			testutil.ErrorIs(t, err, problem.ErrInvalidCount)
		})
	}

	// Mock exams are composed from an exam file, not by Select.
	for _, mode := range []string{model.ModeMock, "", "exam"} {
		_, err := problem.Select(b, problem.SelectOptions{Mode: mode, Count: 5})
		testutil.ErrorIs(t, err, problem.ErrInvalidMode)
	}
}

func TestSelectRejectsDuplicateIDs(t *testing.T) {
	b := bank(6, nil)
	b[5].ID = "p01"
	_, err := problem.Select(b, problem.SelectOptions{Mode: model.ModePractice, Count: 5, Seed: 1})
	testutil.ErrorIs(t, err, problem.ErrDuplicateProblem)
}

// TestSelectGolden locks the seed-v1 selection rule: candidates sorted by ID,
// SplitMix64 partial Fisher-Yates. The expected order was computed
// independently of this package.
func TestSelectGolden(t *testing.T) {
	b := bank(10, nil)
	// Input order must not matter.
	reversed := make([]model.Problem, len(b))
	for i := range b {
		reversed[len(b)-1-i] = b[i]
	}
	for _, in := range [][]model.Problem{b, reversed} {
		got, err := problem.Select(in, problem.SelectOptions{Mode: model.ModePractice, Count: 5, Seed: 38129})
		testutil.NoError(t, err)
		testutil.Equal(t, ids(got), []string{"p02", "p01", "p05", "p08", "p09"})
	}
	got, err := problem.Select(b, problem.SelectOptions{Mode: model.ModeRetry, Count: 10, Seed: 38129})
	testutil.NoError(t, err)
	testutil.Equal(t, ids(got), []string{"p02", "p01", "p05", "p08", "p09", "p10", "p07", "p03", "p04", "p06"})
	testutil.Equal(t, problem.SeedAlgorithm, "seed-v1")
}
