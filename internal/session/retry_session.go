//go:build linux

package session

import (
	"context"
	"fmt"
	"slices"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/model"
)

func (s *Service) retryPlan(ctx context.Context, opts StartOptions) (plan, uint64, error) {
	if opts.RetrySessionID == "" || opts.Count != 0 || opts.MockID != "" || opts.Domain != "" || opts.Difficulty != "" || opts.Tag != "" || opts.Seed != 0 {
		return plan{}, 0, ErrInvalidOptions
	}
	parent, err := s.cfg.Store.LoadSession(ctx, opts.RetrySessionID)
	if err != nil {
		return plan{}, 0, err
	}
	if parent.State != model.StateCompleted && parent.State != model.StateAbandoned {
		return plan{}, 0, fmt.Errorf("%w: finish or reset parent session first", ErrSessionActive)
	}
	hist, err := s.loadHistory(ctx, parent.ID)
	if err != nil {
		return plan{}, 0, err
	}
	// Newest run touching each problem is authoritative. ERROR alone is never
	// evidence of weakness, and unresolved submissions do not reuse older FAILs.
	failed := map[string]bool{}
	for _, sub := range hist.subs {
		for _, f := range parent.Problems {
			if sub.ProblemID == "" || sub.ProblemID == f.Definition.ID {
				failed[f.Definition.ID] = false
			}
		}
		runs := hist.runs[sub.ID]
		if len(runs) == 0 {
			continue
		}
		run := runs[len(runs)-1]
		for _, r := range run.Evaluation.Results {
			pid, _, ok := SplitResultID(r.CriterionID)
			if ok && r.Status == model.StatusFail {
				failed[pid] = true
			}
		}
	}
	var chosen []model.Problem
	for _, f := range parent.Problems {
		if failed[f.Definition.ID] {
			p, err := LoadPackage(s.cfg.StateDir, f.Hash)
			if err != nil {
				return plan{}, 0, err
			}
			chosen = append(chosen, p)
		}
	}
	if len(chosen) < 1 || len(chosen) > 20 {
		return plan{}, 0, fmt.Errorf("%w: retry needs 1-20 problems with failed criteria, found %d", ErrInvalidOptions, len(chosen))
	}
	seed := parent.Seed
	if opts.NewSeed {
		for i := 0; i < 10; i++ {
			seed, err = s.cfg.NewSeed()
			if err != nil {
				return plan{}, 0, err
			}
			if seed != parent.Seed && seed != 0 {
				break
			}
		}
		if seed == parent.Seed || seed == 0 {
			return plan{}, 0, fmt.Errorf("%w: could not obtain a different seed", ErrInvalidOptions)
		}
	}
	return plan{mock: true, problems: chosen}, seed, nil // strict fixed selection; no drops or substitutions
}

func (s *Service) retryFrozen(ctx context.Context, id model.Identity, opts StartOptions, chosen []model.Problem) ([]model.FrozenProblem, [][]environment.Action, error) {
	parent, err := s.cfg.Store.LoadSession(ctx, opts.RetrySessionID)
	if err != nil {
		return nil, nil, err
	}
	var frozen []model.FrozenProblem
	var setups [][]environment.Action
	for _, p := range chosen {
		i := slices.IndexFunc(parent.Problems, func(f model.FrozenProblem) bool { return f.Definition.ID == p.ID })
		if i < 0 {
			return nil, nil, ErrInvalidOptions
		}
		f := parent.Problems[i]
		f.Context = id.Context
		actions, err := author.BuildActions(p.Files, f.Parameters, p.Setup)
		if err != nil {
			return nil, nil, err
		}
		frozen = append(frozen, f)
		setups = append(setups, actions)
	}
	return frozen, setups, nil
}
