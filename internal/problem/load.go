// Package problem loads, validates, renders, freezes and selects declarative
// problem packages (spec §6), and confines package and session file paths to
// their roots.
package problem

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/schemas"
)

// ErrInvalidDefinition wraps every reason a problem package is rejected.
var ErrInvalidDefinition = errors.New("invalid problem definition")

// DefinitionFile is the single metadata file of a problem package.
const DefinitionFile = "problem.yaml"

// Load reads the problem package in dir and validates it: strict YAML (no
// duplicate keys) converted to JSON, JSON Schema 2020-12 validation against
// schemas/problem-v1.schema.json, conversion to model types (durations via
// time.ParseDuration, quantities via Kubernetes resource.ParseQuantity), then
// semantic checks (unique IDs, dependency graph, expected vectors and
// scores, parameter types and references, ports, namespaces, resource
// scope), and every ${name} in setup/, solution/ and cases/ text files must
// be a declared parameter. Every other
// package file is loaded into Problem.Files; symlinks must resolve inside
// dir. All errors wrap ErrInvalidDefinition.
func Load(dir string) (model.Problem, error) {
	p, err := load(dir)
	if err != nil {
		return model.Problem{}, fmt.Errorf("%w: %s: %w", ErrInvalidDefinition, dir, err)
	}
	return p, nil
}

func load(dir string) (model.Problem, error) {
	files, err := readPackage(dir)
	if err != nil {
		return model.Problem{}, err
	}
	for _, name := range []string{DefinitionFile, QuestionFile, ExplanationFile} {
		if _, ok := files[name]; !ok {
			return model.Problem{}, fmt.Errorf("missing %s", name)
		}
	}
	raw := files[DefinitionFile]
	delete(files, DefinitionFile)

	doc, err := yaml.YAMLToJSONStrict(raw)
	if err != nil {
		return model.Problem{}, fmt.Errorf("%s: %w", DefinitionFile, err)
	}
	if err := validateSchema(doc); err != nil {
		return model.Problem{}, fmt.Errorf("%s: %w", DefinitionFile, err)
	}
	p, err := convert(doc)
	if err != nil {
		return model.Problem{}, fmt.Errorf("%s: %w", DefinitionFile, err)
	}
	p.Files = files
	if err := validate(p); err != nil {
		return model.Problem{}, fmt.Errorf("%s: %w", DefinitionFile, err)
	}
	if err := checkPackageReferences(p); err != nil {
		return model.Problem{}, err
	}
	return p, nil
}

// readPackage returns every regular file under dir keyed by slash path.
// Symlinked files are read through their real path, which must stay inside
// dir; symlinked directories are rejected.
func readPackage(dir string) (map[string][]byte, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		real := path
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			if real, err = ResolveAsset(root, rel); err != nil {
				return err
			}
			info, err := os.Stat(real)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("%s: symlink must point to a regular file", filepath.ToSlash(rel))
			}
		case !d.Type().IsRegular():
			return fmt.Errorf("%s: not a regular file", filepath.ToSlash(rel))
		}
		b, err := os.ReadFile(real)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = b
		return nil
	})
	return files, err
}

var problemSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemas.ProblemV1))
	if err != nil {
		return nil, err
	}
	const url = "https://github.com/JunYupK/JunKloud/schemas/problem-v1.schema.json"
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(url, doc); err != nil {
		return nil, err
	}
	return c.Compile(url)
})

func validateSchema(doc []byte) error {
	sch, err := problemSchema()
	if err != nil {
		return fmt.Errorf("compile schema: %w", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return err
	}
	return sch.Validate(inst)
}

// Raw shapes of the schema-validated JSON document, before type conversion.
type rawProblem struct {
	SchemaVersion   int                 `json:"schema_version"`
	ID              string              `json:"id"`
	Version         string              `json:"version"`
	Title           string              `json:"title"`
	Domain          string              `json:"domain"`
	Difficulty      string              `json:"difficulty"`
	Type            string              `json:"type"`
	Tags            []string            `json:"tags"`
	Competencies    []string            `json:"competencies"`
	Capabilities    []string            `json:"capabilities"`
	Parameters      map[string][]any    `json:"parameters"`
	Namespace       string              `json:"namespace"`
	Scope           model.ResourceScope `json:"scope"`
	Budget          rawBudget           `json:"budget"`
	Sources         []string            `json:"sources"`
	Criteria        []rawCriterion      `json:"criteria"`
	Setup           []model.Action      `json:"setup"`
	Solution        []model.Action      `json:"solution"`
	InitialExpected map[string]string   `json:"initial_expected"`
	InitialScore    int                 `json:"initial_score"`
	Cases           []model.Case        `json:"cases"`
}

// rawBudget holds quantities as written: a string ("250m") or a JSON number
// (1).
type rawBudget struct {
	CPU    any `json:"cpu"`
	Memory any `json:"memory"`
}

type rawCriterion struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	Feedback  string          `json:"feedback"`
	Target    model.ObjectRef `json:"target"`
	Points    int             `json:"points"`
	DependsOn []string        `json:"depends_on"`
	Timeout   string          `json:"timeout"`
	Args      map[string]any  `json:"args"`
}

func convert(doc []byte) (model.Problem, error) {
	var r rawProblem
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return model.Problem{}, err
	}

	cpu, err := parseQuantity("budget.cpu", r.Budget.CPU)
	if err != nil {
		return model.Problem{}, err
	}
	memory, err := parseQuantity("budget.memory", r.Budget.Memory)
	if err != nil {
		return model.Problem{}, err
	}
	p := model.Problem{
		SchemaVersion:   r.SchemaVersion,
		ID:              r.ID,
		Version:         r.Version,
		Title:           r.Title,
		Domain:          r.Domain,
		Difficulty:      r.Difficulty,
		Type:            r.Type,
		Tags:            r.Tags,
		Competencies:    r.Competencies,
		Capabilities:    r.Capabilities,
		Namespace:       r.Namespace,
		Scope:           r.Scope,
		Budget:          model.ResourceBudget{CPUMillis: cpu.MilliValue(), MemoryBytes: memory.Value()},
		Sources:         r.Sources,
		Setup:           normalizeActions(r.Setup),
		Solution:        normalizeActions(r.Solution),
		InitialExpected: r.InitialExpected,
		InitialScore:    r.InitialScore,
		Cases:           r.Cases,
	}
	if r.Parameters != nil {
		p.Parameters = make(map[string][]any, len(r.Parameters))
		for name, values := range r.Parameters {
			p.Parameters[name] = normalizeNumbers(values).([]any)
		}
	}
	for i := range p.Cases {
		p.Cases[i].Actions = normalizeActions(p.Cases[i].Actions)
	}
	for _, c := range r.Criteria {
		var timeout time.Duration
		if c.Timeout != "" {
			if timeout, err = time.ParseDuration(c.Timeout); err != nil || timeout <= 0 {
				return model.Problem{}, fmt.Errorf("criterion %q: timeout %q must be a positive duration", c.ID, c.Timeout)
			}
		}
		if !hasHangul(c.Feedback) {
			return model.Problem{}, fmt.Errorf("criterion %q: feedback must be written in Korean (spec 8.1), got %q", c.ID, c.Feedback)
		}
		args, _ := normalizeNumbers(c.Args).(map[string]any)
		p.Criteria = append(p.Criteria, model.Criterion{
			ID: c.ID, Kind: c.Kind, Feedback: c.Feedback, Target: c.Target, Points: c.Points,
			DependsOn: c.DependsOn, Timeout: timeout, Args: args,
		})
	}
	return p, nil
}

// hasHangul reports whether s contains a Hangul syllable or letter: the
// criterion feedback is Korean text (it may name Kubernetes terms in
// English around it).
func hasHangul(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Hangul, r) {
			return true
		}
	}
	return false
}

// parseQuantity parses a Kubernetes quantity ("250m", "1", "128Mi"); it must
// be positive.
func parseQuantity(field string, v any) (resource.Quantity, error) {
	s := fmt.Sprint(v)
	q, err := resource.ParseQuantity(s)
	if err != nil || q.Sign() <= 0 {
		return resource.Quantity{}, fmt.Errorf("%s: %q is not a positive quantity", field, s)
	}
	return q, nil
}

func normalizeActions(in []model.Action) []model.Action {
	for i := range in {
		in[i].Args, _ = normalizeNumbers(in[i].Args).(map[string]any)
	}
	return in
}

// normalizeNumbers replaces json.Number with int when integral (and in
// range), float64 otherwise, so decoded values have ordinary Go types.
func normalizeNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil && i >= math.MinInt && i <= math.MaxInt {
			return int(i)
		}
		f, _ := x.Float64()
		return f
	case map[string]any:
		if x == nil {
			return x
		}
		for k, e := range x {
			x[k] = normalizeNumbers(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = normalizeNumbers(e)
		}
		return x
	default:
		return v
	}
}

// validate runs the semantic checks the schema cannot express.
func validate(p model.Problem) error {
	byID := make(map[string]model.Criterion, len(p.Criteria))
	maxScore := 0
	for _, c := range p.Criteria {
		if _, dup := byID[c.ID]; dup {
			return fmt.Errorf("duplicate criterion id %q", c.ID)
		}
		byID[c.ID] = c
		maxScore += c.Points
	}
	for _, c := range p.Criteria {
		for _, dep := range c.DependsOn {
			if _, ok := byID[dep]; !ok {
				return fmt.Errorf("criterion %q depends on unknown criterion %q", c.ID, dep)
			}
		}
	}
	if err := checkAcyclic(p.Criteria); err != nil {
		return err
	}

	if err := checkExpected(p.Criteria, "initial_expected", p.InitialExpected, "initial_score", p.InitialScore); err != nil {
		return err
	}
	if p.InitialScore == maxScore {
		return fmt.Errorf("initial_score %d equals the maximum score: the problem starts solved", p.InitialScore)
	}
	caseIDs := map[string]bool{}
	for _, c := range p.Cases {
		if caseIDs[c.ID] {
			return fmt.Errorf("duplicate case id %q", c.ID)
		}
		caseIDs[c.ID] = true
		label := fmt.Sprintf("case %q: expected", c.ID)
		if err := checkExpected(p.Criteria, label, c.Expected, fmt.Sprintf("case %q: expected_score", c.ID), c.ExpectedScore); err != nil {
			return err
		}
	}

	if err := checkParameterTypes(p.Parameters); err != nil {
		return err
	}
	return checkEveryParameterValue(p)
}

// referencingDirs hold package files that are rendered with the frozen
// parameters: setup/ (including setup/provided/) when applied or handed out,
// solution/ and cases/ when applied.
var referencingDirs = []string{"setup/", "solution/", "cases/"}

// checkPackageReferences requires every ${name} in the package's text files
// under referencingDirs to name a declared parameter, so a typo fails at load
// time instead of at apply time. Binary files are not scanned or rendered.
func checkPackageReferences(p model.Problem) error {
	for _, name := range slices.Sorted(maps.Keys(p.Files)) {
		content := p.Files[name]
		if !slices.ContainsFunc(referencingDirs, func(dir string) bool { return strings.HasPrefix(name, dir) }) || !isText(content) {
			continue
		}
		for _, m := range refPattern.FindAllSubmatch(content, -1) {
			if _, ok := p.Parameters[string(m[1])]; !ok {
				return fmt.Errorf("%s: %w %q", name, ErrUndefinedParameter, m[1])
			}
		}
	}
	return nil
}

// checkAcyclic rejects dependency cycles (including self-dependencies) and
// names the cycle.
func checkAcyclic(criteria []model.Criterion) error {
	deps := make(map[string][]string, len(criteria))
	for _, c := range criteria {
		deps[c.ID] = c.DependsOn
	}
	const (
		unvisited = iota
		visiting
		done
	)
	state := map[string]int{}
	var stack []string
	var visit func(id string) error
	visit = func(id string) error {
		switch state[id] {
		case visiting:
			start := slices.Index(stack, id)
			return fmt.Errorf("dependency cycle: %s", strings.Join(append(slices.Clone(stack[start:]), id), " -> "))
		case done:
			return nil
		}
		state[id] = visiting
		stack = append(stack, id)
		for _, dep := range deps[id] {
			if err := visit(dep); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = done
		return nil
	}
	for _, c := range criteria {
		if err := visit(c.ID); err != nil {
			return err
		}
	}
	return nil
}

// checkExpected requires an expected status for every criterion and no
// others, and the score to equal the points of the PASS criteria.
func checkExpected(criteria []model.Criterion, label string, expected map[string]string, scoreLabel string, score int) error {
	known := map[string]bool{}
	sum := 0
	for _, c := range criteria {
		known[c.ID] = true
		status, ok := expected[c.ID]
		if !ok {
			return fmt.Errorf("%s misses criterion %q", label, c.ID)
		}
		if status == model.StatusPass {
			sum += c.Points
		}
	}
	for _, id := range slices.Sorted(maps.Keys(expected)) {
		if !known[id] {
			return fmt.Errorf("%s names unknown criterion %q", label, id)
		}
	}
	if score != sum {
		return fmt.Errorf("%s %d does not match the PASS criteria, which sum to %d", scoreLabel, score, sum)
	}
	return nil
}

func checkParameterTypes(params map[string][]any) error {
	for _, name := range slices.Sorted(maps.Keys(params)) {
		first := fmt.Sprintf("%T", params[name][0])
		for _, v := range params[name][1:] {
			if t := fmt.Sprintf("%T", v); t != first {
				return fmt.Errorf("parameter %q mixes %s and %s values", name, first, t)
			}
		}
	}
	return nil
}

// checkEveryParameterValue renders the problem once per value column (the
// i-th value of every parameter, wrapping shorter lists), so every allowed
// value of every parameter is rendered and checked at least once, and every
// reference in the definition and question is resolved.
func checkEveryParameterValue(p model.Problem) error {
	columns := 1
	for _, values := range p.Parameters {
		columns = max(columns, len(values))
	}
	for i := range columns {
		params := map[string]any{}
		for name, values := range p.Parameters {
			params[name] = values[i%len(values)]
		}
		d, _, err := render(p, params)
		if err != nil {
			return err
		}
		if err := validateRendered(d); err != nil {
			return err
		}
	}
	return nil
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// validateRendered checks values that are only known after rendering:
// namespaces must be DNS-1123 labels and every args key named "port" or
// ending in "_port" must hold an integer in 1-65535.
func validateRendered(p model.Problem) error {
	if !dnsLabel.MatchString(p.Namespace) {
		return fmt.Errorf("namespace %q is not a DNS-1123 label", p.Namespace)
	}
	if err := checkScope(p); err != nil {
		return err
	}
	for _, c := range p.Criteria {
		if ns := c.Target.Namespace; ns != "" && !dnsLabel.MatchString(ns) {
			return fmt.Errorf("criterion %q: target namespace %q is not a DNS-1123 label", c.ID, ns)
		}
		if err := checkPorts(c.Args, "args"); err != nil {
			return fmt.Errorf("criterion %q: %w", c.ID, err)
		}
	}
	actionGroups := map[string][]model.Action{"setup": p.Setup, "solution": p.Solution}
	for _, c := range p.Cases {
		actionGroups[fmt.Sprintf("case %q", c.ID)] = c.Actions
	}
	for _, group := range slices.Sorted(maps.Keys(actionGroups)) {
		for i, a := range actionGroups[group] {
			if err := checkPorts(a.Args, "args"); err != nil {
				return fmt.Errorf("%s action %d: %w", group, i, err)
			}
		}
	}
	return nil
}

// checkScope validates the rendered resource scope: extra namespaces are
// distinct DNS-1123 labels other than the problem namespace, and cluster
// resources are fully named cluster-scoped objects.
func checkScope(p model.Problem) error {
	seen := map[string]bool{}
	for _, ns := range p.Scope.Namespaces {
		switch {
		case !dnsLabel.MatchString(ns):
			return fmt.Errorf("scope namespace %q is not a DNS-1123 label", ns)
		case ns == p.Namespace:
			return fmt.Errorf("scope namespace %q is the problem namespace, which is always in scope", ns)
		case seen[ns]:
			return fmt.Errorf("duplicate scope namespace %q", ns)
		}
		seen[ns] = true
	}
	for i, r := range p.Scope.ClusterResources {
		if r.APIVersion == "" || r.Kind == "" || r.Name == "" || r.Namespace != "" {
			return fmt.Errorf("scope cluster_resources[%d] must name api_version, kind and name and no namespace", i)
		}
	}
	return nil
}

func checkPorts(v any, path string) error {
	switch x := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(x)) {
			p := path + "." + k
			if k == "port" || strings.HasSuffix(k, "_port") {
				if n, ok := integer(x[k]); !ok || n < 1 || n > 65535 {
					return fmt.Errorf("%s %v is not a port in 1-65535", p, x[k])
				}
				continue
			}
			if err := checkPorts(x[k], p); err != nil {
				return err
			}
		}
	case []any:
		for i, e := range x {
			if err := checkPorts(e, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// integer accepts int and integral float64 (the type JSON round trips give).
func integer(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return int64(x), true
		}
	}
	return 0, false
}
