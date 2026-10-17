//go:build linux

package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/lock"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/store"
)

// NewReader serves Status, Review and Mark without creating or connecting to
// an environment. Mutating cluster operations need the fully composed New.
func NewReader(st *store.Store, bankDir, stateDir, workspace string) *Service {
	return &Service{cfg: Config{Store: st, BankDir: bankDir, StateDir: stateDir, Workspace: workspace, Now: time.Now}}
}

// Doctor changes only platform probe resources, under the environment lock.
// It never observes a problem namespace or exposes a hidden criterion.
func (s *Service) Doctor(ctx context.Context) (environment.DoctorReport, error) {
	release, err := lock.Acquire(author.LockPath(s.cfg.StateDir))
	if err != nil {
		return environment.DoctorReport{}, err
	}
	defer release()
	report, err := s.cfg.Env.Doctor(ctx, s.cfg.Identity)
	if err != nil {
		return report, err
	}
	manifest, err := s.cfg.EnvironmentManifest(ctx, s.cfg.Identity)
	if err != nil {
		return report, err
	}
	if report.Versions == nil {
		report.Versions = map[string]string{}
	}
	for k, v := range manifest {
		report.Versions[k] = v
	}
	return report, nil
}

// Mark is a bookmark, independent of submissions and grading.
func (s *Service) Mark(ctx context.Context, id, pid string, marked bool) error {
	rec, err := s.cfg.Store.LoadSession(ctx, id)
	if err != nil {
		return err
	}
	if rec.StartedAt == nil {
		return ErrNotStarted
	}
	if _, ok := findProblem(rec, pid); !ok {
		return ErrUnknownProblem
	}
	return s.cfg.Store.SetMark(ctx, id, pid, marked)
}

// Catalog contains only public metadata of currently servable packages and
// exams. Excluded entries name an unavailable package without private detail.
type Catalog struct {
	Problems []ProblemView
	Exams    []problem.Exam
	Excluded []Exclusion
}

func (s *Service) Catalog(ctx context.Context) (Catalog, error) {
	release, err := lock.Acquire(author.LockPath(s.cfg.StateDir))
	if err != nil {
		return Catalog{}, err
	}
	defer release()
	if err := s.cfg.Env.Check(ctx, s.cfg.Identity); err != nil {
		return Catalog{}, err
	}
	manifest, err := s.cfg.EnvironmentManifest(ctx, s.cfg.Identity)
	if err != nil {
		return Catalog{}, err
	}
	bank, err := s.loadBank()
	if err != nil {
		return Catalog{}, err
	}
	out := Catalog{}
	available := map[string]bool{}
	needsDoctor := false
	for _, p := range bank {
		if len(p.Capabilities) > 0 {
			needsDoctor = true
		}
	}
	var capabilities map[string]bool
	if needsDoctor {
		report, err := s.cfg.Env.Doctor(ctx, s.cfg.Identity)
		if err != nil {
			return Catalog{}, err
		}
		capabilities = map[string]bool{}
		for _, c := range report.Checks {
			capabilities[c.Name] = c.Status == "PASS"
		}
	}
	for _, p := range bank {
		seeds, err := author.CoveringSeeds(p)
		if err != nil {
			return Catalog{}, err
		}
		blocked := false
		for _, seed := range seeds {
			r, err := s.staticRefusal(ctx, author.EnvironmentHash(manifest), p, seed)
			if err != nil {
				return Catalog{}, err
			}
			if r != nil {
				blocked = true
				break
			}
		}
		for _, c := range p.Capabilities {
			if !capabilities[c] {
				blocked = true
			}
		}
		if blocked {
			out.Excluded = append(out.Excluded, Exclusion{p.ID, "검증 또는 환경 capability를 확인하세요."})
			continue
		}
		points := 0
		for _, c := range p.Criteria {
			points += c.Points
		}
		out.Problems = append(out.Problems, ProblemView{ID: p.ID, Title: p.Title, Domain: p.Domain, Points: points})
		available[p.ID] = true
	}
	entries, err := os.ReadDir(filepath.Join(s.cfg.BankDir, ExamsDir))
	if err != nil {
		return Catalog{}, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		exam, err := problem.LoadExam(filepath.Join(s.cfg.BankDir, ExamsDir, entry.Name()))
		if err != nil {
			return Catalog{}, err
		}
		servable := true
		for _, pid := range exam.Problems {
			if !available[pid] {
				servable = false
			}
		}
		if servable {
			out.Exams = append(out.Exams, exam)
		}
	}
	return out, nil
}

// ValidateBank keeps package loading and seed selection inside the application
// boundary. Validator enforces the environment lock and active-session guard.
func (s *Service) ValidateBank(ctx context.Context, v *author.Validator, id string) (model.ValidationRecord, error) {
	if !idPattern.MatchString(id) {
		return model.ValidationRecord{}, ErrInvalidOptions
	}
	p, err := s.loadProblem(id)
	if err != nil {
		return model.ValidationRecord{}, err
	}
	seeds, err := author.CoveringSeeds(p)
	if err != nil {
		return model.ValidationRecord{}, err
	}
	if v == nil {
		return model.ValidationRecord{}, fmt.Errorf("session: validator unavailable")
	}
	return v.Validate(ctx, p, seeds)
}
