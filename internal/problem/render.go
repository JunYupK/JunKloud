package problem

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/JunYupK/JunKloud/internal/model"
)

// ErrUndefinedParameter reports a ${name} reference to no declared parameter.
var ErrUndefinedParameter = errors.New("undefined parameter")

// Package layout (spec §6.1).
const (
	QuestionFile    = "question.en.md"
	ExplanationFile = "explanation.ko.md"
	// ProvidedDir holds the files handed to the learner; everything else in
	// the package is private.
	ProvidedDir = "setup/provided/"
)

// refPattern matches a parameter reference. Only lower-case names are
// references, so shell-style ${HOME} in text is left alone.
var refPattern = regexp.MustCompile(`\$\{([a-z][a-z0-9_]*)\}`)

// Freeze fixes p's parameters with the seed-v1 rule and renders the
// definition and question with them. Parameter names are visited in sorted
// order; each takes values[next() % len(values)] from a SplitMix64 stream
// seeded with seed XOR FNV-1a("seed-v1/parameters/" + p.ID). Only the
// English question and setup/provided/ files become public; the rest of the
// package stays in Definition.Files and is identified by Hash.
func Freeze(p model.Problem, seed uint64) (model.FrozenProblem, error) {
	hash, err := Hash(p)
	if err != nil {
		return model.FrozenProblem{}, err
	}
	params := chooseParameters(p, seed)
	def, question, err := render(p, params)
	if err != nil {
		return model.FrozenProblem{}, fmt.Errorf("freeze %s: %w", p.ID, err)
	}
	if err := validateRendered(def); err != nil {
		return model.FrozenProblem{}, fmt.Errorf("freeze %s: %w", p.ID, err)
	}

	assets := map[string][]byte{}
	for name, content := range p.Files {
		if rel, ok := strings.CutPrefix(name, ProvidedDir); ok {
			assets[rel] = content
		}
	}
	return model.FrozenProblem{
		Definition:    def,
		Hash:          hash,
		Question:      question,
		Parameters:    params,
		Assets:        assets,
		Namespace:     def.Namespace,
		Seed:          seed,
		SeedAlgorithm: SeedAlgorithm,
	}, nil
}

// Hash is the content hash of an unrendered problem: SHA-256 over the
// canonical JSON of the definition followed by every package file in sorted
// path order, each path and content length-prefixed. problem.yaml itself
// is represented by the normalized definition, so formatting and comments do
// not change the hash.
func Hash(p model.Problem) (string, error) {
	def, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", p.ID, err)
	}
	h := sha256.New()
	h.Write([]byte("junkloud-problem-v1\x00"))
	writeChunk(h, def)
	for _, name := range slices.Sorted(maps.Keys(p.Files)) {
		writeChunk(h, []byte(name))
		writeChunk(h, p.Files[name])
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func writeChunk(h interface{ Write([]byte) (int, error) }, b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	h.Write(n[:])
	h.Write(b)
}

func chooseParameters(p model.Problem, seed uint64) map[string]any {
	if len(p.Parameters) == 0 {
		return nil
	}
	names := slices.Sorted(maps.Keys(p.Parameters))
	rng := newSeedStream(seed, "parameters/"+p.ID)
	out := make(map[string]any, len(names))
	for _, name := range names {
		values := p.Parameters[name]
		out[name] = values[rng.next()%uint64(len(values))]
	}
	return out
}

// render returns a deep copy of p with every string value rendered, and the
// rendered question. Map keys, IDs, kinds and points are never rendered.
func render(p model.Problem, params map[string]any) (model.Problem, string, error) {
	var firstErr error
	keep := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	text := func(s string) string {
		out, err := RenderText(s, params)
		keep(err)
		return out
	}
	object := func(m map[string]any) map[string]any {
		out, err := RenderValue(m, params)
		keep(err)
		r, _ := out.(map[string]any)
		return r
	}
	actions := func(in []model.Action) []model.Action {
		if in == nil {
			return nil
		}
		out := make([]model.Action, len(in))
		for i, a := range in {
			out[i] = model.Action{Kind: a.Kind, Args: object(a.Args)}
		}
		return out
	}

	d := p
	d.Title = text(p.Title)
	d.Namespace = text(p.Namespace)
	d.Tags = slices.Clone(p.Tags)
	d.Competencies = slices.Clone(p.Competencies)
	d.Capabilities = slices.Clone(p.Capabilities)
	d.Sources = slices.Clone(p.Sources)
	d.Parameters = maps.Clone(p.Parameters)
	for name, values := range d.Parameters {
		d.Parameters[name] = slices.Clone(values)
	}
	d.InitialExpected = maps.Clone(p.InitialExpected)
	d.Files = maps.Clone(p.Files)
	d.Criteria = make([]model.Criterion, len(p.Criteria))
	for i, c := range p.Criteria {
		c.Target = model.ObjectRef{
			APIVersion: text(c.Target.APIVersion),
			Kind:       text(c.Target.Kind),
			Namespace:  text(c.Target.Namespace),
			Name:       text(c.Target.Name),
		}
		c.DependsOn = slices.Clone(c.DependsOn)
		c.Args = object(c.Args)
		d.Criteria[i] = c
	}
	d.Setup = actions(p.Setup)
	d.Solution = actions(p.Solution)
	if p.Cases != nil {
		d.Cases = make([]model.Case, len(p.Cases))
		for i, c := range p.Cases {
			d.Cases[i] = model.Case{ID: c.ID, Actions: actions(c.Actions), Expected: maps.Clone(c.Expected), ExpectedScore: c.ExpectedScore}
		}
	}

	question, ok := p.Files[QuestionFile]
	if !ok {
		keep(fmt.Errorf("missing %s", QuestionFile))
	}
	q := text(string(question))
	if firstErr != nil {
		return model.Problem{}, "", firstErr
	}
	return d, q, nil
}

// RenderValue structurally substitutes parameters into a decoded YAML/JSON
// value (map[string]any, []any, scalars). A string that is exactly one
// reference becomes the parameter value with its own type, so "${port}"
// yields an int; references embedded in longer strings are interpolated.
// Map keys are left untouched. Unknown references fail with
// ErrUndefinedParameter. The input is not modified.
func RenderValue(v any, params map[string]any) (any, error) {
	switch x := v.(type) {
	case string:
		if m := refPattern.FindStringSubmatchIndex(x); m != nil && m[0] == 0 && m[1] == len(x) {
			name := x[m[2]:m[3]]
			value, ok := params[name]
			if !ok {
				return nil, fmt.Errorf("%w %q", ErrUndefinedParameter, name)
			}
			return value, nil
		}
		return RenderText(x, params)
	case map[string]any:
		if x == nil {
			return x, nil
		}
		out := make(map[string]any, len(x))
		for _, k := range slices.Sorted(maps.Keys(x)) { // deterministic first error
			r, err := RenderValue(x[k], params)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		if x == nil {
			return x, nil
		}
		out := make([]any, len(x))
		for i, e := range x {
			r, err := RenderValue(e, params)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	default:
		return v, nil
	}
}

// RenderText substitutes every ${name} reference in s with the parameter's
// value formatted with fmt.Sprint. Unknown references fail with
// ErrUndefinedParameter.
func RenderText(s string, params map[string]any) (string, error) {
	var missing error
	out := refPattern.ReplaceAllStringFunc(s, func(ref string) string {
		name := ref[2 : len(ref)-1]
		v, ok := params[name]
		if !ok {
			if missing == nil {
				missing = fmt.Errorf("%w %q", ErrUndefinedParameter, name)
			}
			return ref
		}
		return fmt.Sprint(v)
	})
	return out, missing
}
