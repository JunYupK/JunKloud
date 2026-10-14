package problem

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/JunYupK/JunKloud/internal/model"
)

var (
	// ErrInsufficientProblems reports that fewer distinct problems match
	// than were requested. Select never pads a selection with duplicates.
	ErrInsufficientProblems = errors.New("not enough problems")
	// ErrInvalidCount reports a count outside the mode's allowed range.
	ErrInvalidCount = errors.New("invalid problem count")
	// ErrInvalidMode reports a mode Select does not compose. Mock exams are
	// composed from an exam file, not by Select.
	ErrInvalidMode = errors.New("invalid selection mode")
	// ErrDuplicateProblem reports a bank holding one problem ID twice.
	ErrDuplicateProblem = errors.New("duplicate problem id in bank")
)

// Count bounds per mode (spec §7.2).
const (
	PracticeMinCount = 5
	PracticeMaxCount = 20
	RetryMinCount    = 1
	RetryMaxCount    = 20
)

// SelectOptions chooses problems for a practice or retry session. Empty
// Domain, Difficulty and Tag match everything.
type SelectOptions struct {
	Mode, Domain, Difficulty, Tag string
	Count                         int
	Seed                          uint64
}

// Select returns Count distinct problems from bank that match opts, in
// session order. The seed-v1 rule: matching candidates are sorted by ID, then
// a SplitMix64 stream seeded with Seed XOR FNV-1a("seed-v1/select") drives a
// partial Fisher-Yates shuffle (position i swaps with i + next()%(n-i)), and
// the first Count candidates are returned.
func Select(bank []model.Problem, opts SelectOptions) ([]model.Problem, error) {
	var lo, hi int
	switch opts.Mode {
	case model.ModePractice:
		lo, hi = PracticeMinCount, PracticeMaxCount
	case model.ModeRetry:
		lo, hi = RetryMinCount, RetryMaxCount
	default:
		return nil, fmt.Errorf("%w %q: want %q or %q", ErrInvalidMode, opts.Mode, model.ModePractice, model.ModeRetry)
	}
	if opts.Count < lo || opts.Count > hi {
		return nil, fmt.Errorf("%w: %s sessions take %d-%d problems, got %d", ErrInvalidCount, opts.Mode, lo, hi, opts.Count)
	}

	seen := make(map[string]bool, len(bank))
	var candidates []model.Problem
	for _, p := range bank {
		if seen[p.ID] {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateProblem, p.ID)
		}
		seen[p.ID] = true
		if matches(p, opts) {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) < opts.Count {
		return nil, fmt.Errorf("%w matching %s: %d requested, %d available", ErrInsufficientProblems, describe(opts), opts.Count, len(candidates))
	}

	slices.SortFunc(candidates, func(a, b model.Problem) int { return strings.Compare(a.ID, b.ID) })
	rng := newSeedStream(opts.Seed, "select")
	n := uint64(len(candidates))
	for i := uint64(0); i < uint64(opts.Count); i++ {
		j := i + rng.next()%(n-i)
		candidates[i], candidates[j] = candidates[j], candidates[i]
	}
	return candidates[:opts.Count:opts.Count], nil
}

func matches(p model.Problem, opts SelectOptions) bool {
	return (opts.Domain == "" || p.Domain == opts.Domain) &&
		(opts.Difficulty == "" || p.Difficulty == opts.Difficulty) &&
		(opts.Tag == "" || slices.Contains(p.Tags, opts.Tag))
}

func describe(opts SelectOptions) string {
	var parts []string
	for _, f := range [][2]string{{"domain", opts.Domain}, {"difficulty", opts.Difficulty}, {"tag", opts.Tag}} {
		if f[1] != "" {
			parts = append(parts, f[0]+"="+f[1])
		}
	}
	if len(parts) == 0 {
		return "any problem"
	}
	return strings.Join(parts, ", ")
}

// SeedAlgorithm names the seed-v1 rules used by Select and Freeze. Any change
// to those rules (stream derivation, ordering, index arithmetic) needs a new
// version string; the golden tests lock the current one.
const SeedAlgorithm = "seed-v1"

// seedStream is SplitMix64. It is defined here rather than taken from
// math/rand so seed-v1 output can never change with the Go release.
type seedStream struct{ state uint64 }

// newSeedStream derives an independent stream per purpose:
// state = seed XOR FNV-1a-64("seed-v1/" + purpose).
func newSeedStream(seed uint64, purpose string) *seedStream {
	h := uint64(14695981039346656037)
	for _, b := range []byte(SeedAlgorithm + "/" + purpose) {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return &seedStream{state: seed ^ h}
}

func (s *seedStream) next() uint64 {
	s.state += 0x9E3779B97F4A7C15
	z := s.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}
