//go:build linux

package session

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
)

// serve decides which problems the session is made of (spec 7.2). Only
// servable problems are served: the package is validated for this exact
// grader version and environment (a current certificate), the capabilities it
// declares are all PASS, and its provided files do not shadow the question.
//
//   - Mock: the exam file is strict. Any unservable problem refuses the whole
//     session with every reason; nothing is dropped or replaced.
//   - Practice: the candidates are narrowed to the servable problems matching
//     the request BEFORE Select draws from them, so whether a session can be
//     prepared never depends on the seed hitting an unservable problem. The
//     ones left out are reported (Report.Excluded), and a pool too small for
//     the requested count is an error that says how many are available and
//     why the others are not.
func (s *Service) serve(ctx context.Context, id model.Identity, envHash string, pl plan, opts StartOptions, seed uint64) ([]model.Problem, Report, error) {
	prepared := false
	prepare := func() error {
		if prepared {
			return nil
		}
		if err := s.cfg.PrepareEnvironment(ctx, id); err != nil {
			return fmt.Errorf("session: prepare the environment: %w", err)
		}
		prepared = true
		return nil
	}
	filter := problem.SelectOptions{Mode: opts.Mode, Domain: opts.Domain, Difficulty: opts.Difficulty, Tag: opts.Tag, Count: opts.Count, Seed: seed}

	// Stage 1, no cluster work: reserved file names and certificates.
	var candidates []model.Problem
	var refusals []refusal
	for _, p := range pl.problems {
		if !pl.mock && !problem.Matches(p, filter) {
			continue
		}
		r, err := s.staticRefusal(ctx, envHash, p)
		if err != nil {
			return nil, Report{}, err
		}
		if r != nil {
			refusals = append(refusals, *r)
			continue
		}
		candidates = append(candidates, p)
	}
	if pl.mock && len(refusals) > 0 {
		return nil, Report{}, refusalError(refusals)
	}

	// Stage 2: the declared capabilities, which need the doctor (and so the
	// platform) only if some candidate declares any.
	if slices.ContainsFunc(candidates, func(p model.Problem) bool { return len(p.Capabilities) > 0 }) {
		if err := prepare(); err != nil {
			return nil, Report{}, err
		}
		report, err := s.cfg.Env.Doctor(ctx, id)
		if err != nil {
			return nil, Report{}, fmt.Errorf("session: check the environment capabilities: %w", err)
		}
		var capable []model.Problem
		var capRefusals []refusal
		for _, p := range candidates {
			if why := unavailable(report, p); why != "" {
				capRefusals = append(capRefusals, refusal{problemID: p.ID, reason: why, kind: ErrCapabilityUnavailable})
				continue
			}
			capable = append(capable, p)
		}
		if pl.mock && len(capRefusals) > 0 {
			return nil, Report{}, refusalError(capRefusals)
		}
		candidates, refusals = capable, append(refusals, capRefusals...)
	}

	var report Report
	for _, r := range refusals {
		report.Excluded = append(report.Excluded, Exclusion{ProblemID: r.problemID, Reason: r.reason})
	}
	if pl.mock {
		return candidates, report, prepare()
	}
	chosen, err := problem.Select(candidates, filter)
	if err != nil {
		if len(report.Excluded) > 0 {
			err = fmt.Errorf("%w; not servable: %s", err, describeExclusions(report.Excluded))
		}
		return nil, report, err
	}
	return chosen, report, prepare()
}

// refusal is one unservable problem; kind is the sentinel a refusal wraps.
type refusal struct {
	problemID, reason string
	kind              error
}

func describeExclusions(ex []Exclusion) string {
	parts := make([]string, len(ex))
	for i, e := range ex {
		parts[i] = e.ProblemID + " (" + e.Reason + ")"
	}
	return strings.Join(parts, "; ")
}

// refusalError reports every refusal, wrapping one sentinel per kind.
func refusalError(refusals []refusal) error {
	var errs []error
	for _, kind := range []error{ErrReservedAsset, ErrNotCertified, ErrCapabilityUnavailable} {
		var parts []string
		for _, r := range refusals {
			if r.kind == kind {
				parts = append(parts, r.problemID+" ("+r.reason+")")
			}
		}
		if len(parts) > 0 {
			errs = append(errs, fmt.Errorf("%w: %s", kind, strings.Join(parts, "; ")))
		}
	}
	return errors.Join(errs...)
}

// staticRefusal checks what needs no cluster: that the provided files leave
// the question's file name alone, and that the problem has a current
// certificate for this grader version and environment hash. A nil result
// means servable so far; only a store failure is an error.
func (s *Service) staticRefusal(ctx context.Context, envHash string, p model.Problem) (*refusal, error) {
	if _, clash := p.Files[path.Join(problem.ProvidedDir, QuestionFile)]; clash {
		return &refusal{p.ID, "it provides a file named " + QuestionFile + ", which is reserved for the question", ErrReservedAsset}, nil
	}
	hash, err := problem.Hash(p)
	if err != nil {
		return nil, err
	}
	_, ok, err := author.CurrentCertificate(ctx, s.cfg.Store, hash, s.cfg.GraderVersion, envHash)
	if err != nil {
		return nil, fmt.Errorf("session: look up the validation of %q: %w", p.ID, err)
	}
	if !ok {
		return &refusal{p.ID, fmt.Sprintf("no passing validation for this exact package, grader version %s and environment: it was never validated, failed, or the package, grader or environment changed since; validate the bank", s.cfg.GraderVersion), ErrNotCertified}, nil
	}
	return nil, nil
}

// unavailable explains which of p's declared capabilities the doctor does not
// report as PASS ("" when all are).
func unavailable(report environment.DoctorReport, p model.Problem) string {
	var parts []string
	for _, name := range report.Unavailable(p.Capabilities) {
		reason := "not reported by the doctor"
		if i := slices.IndexFunc(report.Checks, func(c environment.CapabilityCheck) bool { return c.Name == name }); i >= 0 {
			reason = report.Checks[i].Status
			if r := report.Checks[i].Reason; r != "" {
				reason += ": " + r
			}
		}
		parts = append(parts, fmt.Sprintf("needs %s (%s)", name, reason))
	}
	return strings.Join(parts, ", ")
}
