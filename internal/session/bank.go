//go:build linux

package session

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
)

// Bank layout under StartOptions' BankDir.
const (
	ProblemsDir = "problems" // problems/<id>/ packages
	ExamsDir    = "exams"    // exams/<id>.yaml
)

var idPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// loadProblem reads one bank package. The directory name is the problem ID.
func (s *Service) loadProblem(id string) (model.Problem, error) {
	dir := filepath.Join(s.cfg.BankDir, ProblemsDir, id)
	p, err := problem.Load(dir)
	if err != nil {
		return model.Problem{}, err
	}
	if p.ID != id {
		return model.Problem{}, fmt.Errorf("%w: %s declares id %q, but the bank directory is named %q", problem.ErrInvalidDefinition, dir, p.ID, id)
	}
	return p, nil
}

// loadBank reads every package of the bank. A package that does not load is
// an error, not something to skip: selecting from a bank that quietly lost
// problems would change what a seed means.
func (s *Service) loadBank() ([]model.Problem, error) {
	root := filepath.Join(s.cfg.BankDir, ProblemsDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("session: read the problem bank: %w", err)
	}
	var bank []model.Problem
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p, err := s.loadProblem(e.Name())
		if err != nil {
			return nil, fmt.Errorf("session: bank problem %q: %w", e.Name(), err)
		}
		bank = append(bank, p)
	}
	return bank, nil
}

// compose chooses the session's problems: mock sessions follow the exam file
// in its order and take its duration; practice sessions use problem.Select
// and have no time limit (spec: only the mock exam has an official time).
func (s *Service) compose(opts StartOptions, seed uint64) ([]model.Problem, time.Duration, error) {
	switch opts.Mode {
	case model.ModeMock:
		return s.composeMock(opts)
	case model.ModePractice:
		if opts.MockID != "" || opts.RetrySessionID != "" || opts.NewSeed {
			return nil, 0, fmt.Errorf("%w: a practice session takes no exam, retry session or new-seed option", ErrInvalidOptions)
		}
		bank, err := s.loadBank()
		if err != nil {
			return nil, 0, err
		}
		chosen, err := problem.Select(bank, problem.SelectOptions{
			Mode: model.ModePractice, Domain: opts.Domain, Difficulty: opts.Difficulty,
			Tag: opts.Tag, Count: opts.Count, Seed: seed,
		})
		return chosen, 0, err
	case model.ModeRetry:
		return nil, 0, ErrRetryUnavailable
	default:
		return nil, 0, fmt.Errorf("%w: mode %q, want %q or %q", ErrInvalidOptions, opts.Mode, model.ModePractice, model.ModeMock)
	}
}

func (s *Service) composeMock(opts StartOptions) ([]model.Problem, time.Duration, error) {
	if opts.Domain != "" || opts.Difficulty != "" || opts.Tag != "" || opts.Count != 0 || opts.RetrySessionID != "" || opts.NewSeed {
		return nil, 0, fmt.Errorf("%w: a mock exam is fixed by its exam file and takes no filters or count", ErrInvalidOptions)
	}
	if !idPattern.MatchString(opts.MockID) {
		return nil, 0, fmt.Errorf("%w: mock exam %q must be an exam ID (lower-case letters, digits, hyphens)", ErrInvalidOptions, opts.MockID)
	}
	exam, err := problem.LoadExam(filepath.Join(s.cfg.BankDir, ExamsDir, opts.MockID+".yaml"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, fmt.Errorf("%w: there is no exam %q in the bank", ErrInvalidOptions, opts.MockID)
	}
	if err != nil {
		return nil, 0, err
	}
	if exam.ID != opts.MockID {
		return nil, 0, fmt.Errorf("%w: exams/%s.yaml declares id %q", problem.ErrInvalidExam, opts.MockID, exam.ID)
	}
	problems := make([]model.Problem, 0, len(exam.Problems))
	for _, id := range exam.Problems {
		p, err := s.loadProblem(id)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, fmt.Errorf("session: exam %q lists problem %q, which the bank does not have", exam.ID, id)
		}
		if err != nil {
			return nil, 0, fmt.Errorf("session: exam %q problem %q: %w", exam.ID, id, err)
		}
		problems = append(problems, p)
	}
	return problems, exam.Duration, nil
}
