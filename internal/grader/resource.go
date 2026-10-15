package grader

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/JunYupK/JunKloud/internal/model"
)

// ResourceChecker grades model.CriterionResource: the target object exists.
//
// Args (strict; unknown keys are an author error):
//
//	wait: bool   keep looking until the criterion's timeout (default: look once)
type ResourceChecker struct{ observer }

// FieldChecker grades model.CriterionField: the target object exists and the
// requested fields hold the requested values, compared by meaning.
//
// Args:
//
//	wait: bool                  poll until the fields match or the timeout ends
//	fields: [ requirement ... ] at least one
//
// Requirement (exactly one of equals / exists):
//
//	path:    field path, e.g. spec.template.spec.containers[name=web].image
//	equals:  the expected value (any JSON value except null)
//	exists:  true/false, presence of the path
//	match:   equal (default) | subset | unordered
//	           equal      maps have the same keys; lists of named objects match
//	                      by name, other lists by position
//	           subset     expected maps/lists need only be contained
//	           unordered  lists of any kind match in any order
//	compare: auto (default) | exact | quantity | image
//	           auto       resource quantities (1 == 1000m) under requests,
//	                      limits, hard and capacity, and container images
//	                      (containers, initContainers, ephemeralContainers;
//	                      "nginx" == "docker.io/library/nginx:latest"), also
//	                      inside a compared parent map such as a whole
//	                      resources block or container; everything else exact
//	           exact      the literal string/number, no quantity or image
//	                      interpretation (for authors who require it)
//	           quantity   always parse strings/numbers as Kubernetes quantities
//	           image      treat the value as an image reference everywhere
//
// Numbers compare by value (1, 1.0 and "1" JSON round trips are the same
// number); a number never equals a string. For Secrets, "data" values are
// compared base64-decoded and never appear in evidence.
type FieldChecker struct{ observer }

// NewResourceChecker returns the checker for model.CriterionResource.
func NewResourceChecker(reader ObjectReader, opts ...Option) *ResourceChecker {
	return &ResourceChecker{observer{reader: reader, set: newSettings(opts)}}
}

// NewFieldChecker returns the checker for model.CriterionField.
func NewFieldChecker(reader ObjectReader, opts ...Option) *FieldChecker {
	return &FieldChecker{observer{reader: reader, set: newSettings(opts)}}
}

// DefaultCheckers returns this package's checkers keyed by criterion kind:
// resource, field and ready. Later tasks add their kinds to the map before
// calling NewEngine. Pass the same options (and Redactor) to NewEngine.
func DefaultCheckers(reader ObjectReader, opts ...Option) map[string]Checker {
	return map[string]Checker{
		model.CriterionResource: NewResourceChecker(reader, opts...),
		model.CriterionField:    NewFieldChecker(reader, opts...),
		model.CriterionReady:    NewReadyChecker(reader, opts...),
	}
}

type resourceArgs struct {
	Wait bool `json:"wait"`
}

// ValidateCriterion implements ArgsValidator.
func (c *ResourceChecker) ValidateCriterion(crit model.Criterion) error {
	var a resourceArgs
	if err := decodeArgs(crit.Args, &a); err != nil {
		return err
	}
	return checkRef(crit.Target)
}

// Check implements Checker.
func (c *ResourceChecker) Check(ctx context.Context, t Target, crit model.Criterion) (model.CheckResult, error) {
	var a resourceArgs
	if err := decodeArgs(crit.Args, &a); err != nil {
		return model.CheckResult{}, err
	}
	ref, err := resolveTarget(t, crit)
	if err != nil {
		return model.CheckResult{}, err
	}
	spec := observeSpec{
		ref: ref, timeout: timeoutFor(crit, c.set), poll: a.Wait, waitForCreate: a.Wait,
		eval: func(object) verdict { return verdict{ok: true} },
	}
	obs, err := c.observe(ctx, spec)
	if err != nil {
		return model.CheckResult{}, err
	}
	res := c.result(obs, spec)
	res.Evidence.Expected = map[string]any{"exists": true, "object": describeRef(ref)}
	res.Evidence.Actual = map[string]any{"exists": obs.found}
	return res, nil
}

func describeRef(ref model.ObjectRef) string {
	if ref.Namespace == "" {
		return fmt.Sprintf("%s %s %s", ref.APIVersion, ref.Kind, ref.Name)
	}
	return fmt.Sprintf("%s %s %s/%s", ref.APIVersion, ref.Kind, ref.Namespace, ref.Name)
}

type fieldArgs struct {
	Wait   bool         `json:"wait"`
	Fields []fieldInput `json:"fields"`
}

type fieldInput struct {
	Path    string          `json:"path"`
	Equals  json.RawMessage `json:"equals"`
	Exists  *bool           `json:"exists"`
	Match   string          `json:"match"`
	Compare string          `json:"compare"`
}

type requirement struct {
	path      string
	segs      []pathSeg
	hasEquals bool
	equals    any
	exists    bool // wanted presence when !hasEquals
	cmp       comparer
}

func compileFields(args map[string]any) (fieldArgs, []requirement, error) {
	var a fieldArgs
	if err := decodeArgs(args, &a); err != nil {
		return a, nil, err
	}
	if len(a.Fields) == 0 {
		return a, nil, fmt.Errorf("%w: args.fields needs at least one requirement", ErrInvalidCriterion)
	}
	reqs := make([]requirement, 0, len(a.Fields))
	for i, f := range a.Fields {
		bad := func(format string, v ...any) error {
			return fmt.Errorf("%w: args.fields[%d]: %s", ErrInvalidCriterion, i, fmt.Sprintf(format, v...))
		}
		segs, err := parsePath(f.Path)
		if err != nil {
			return a, nil, bad("%v", err)
		}
		r := requirement{path: f.Path, segs: segs}
		switch {
		case len(f.Equals) > 0 && f.Exists != nil:
			return a, nil, bad("%q: give equals or exists, not both", f.Path)
		case len(f.Equals) > 0:
			dec := json.NewDecoder(bytes.NewReader(f.Equals))
			dec.UseNumber()
			if err := dec.Decode(&r.equals); err != nil {
				return a, nil, bad("%q: equals: %v", f.Path, err)
			}
			if r.equals == nil {
				return a, nil, bad("%q: equals cannot be null; use exists: false", f.Path)
			}
			r.hasEquals = true
		case f.Exists != nil:
			r.exists = *f.Exists
		default:
			return a, nil, bad("%q: needs equals or exists", f.Path)
		}
		switch f.Match {
		case "", matchEqual:
			r.cmp.match = matchEqual
		case matchSubset, matchUnordered:
			r.cmp.match = f.Match
		default:
			return a, nil, bad("%q: match %q is not equal, subset or unordered", f.Path, f.Match)
		}
		switch f.Compare {
		case "", compareAuto:
			r.cmp.auto = true
			r.cmp.quantity = autoQuantity(segs)
			r.cmp.image, r.cmp.container, r.cmp.containerList = autoContext(segs)
		case compareExact:
		case compareQuantity:
			r.cmp.quantity = true
		case compareImage:
			r.cmp.image = true
		default:
			return a, nil, bad("%q: compare %q is not auto, exact, quantity or image", f.Path, f.Compare)
		}
		reqs = append(reqs, r)
	}
	return a, reqs, nil
}

// ValidateCriterion implements ArgsValidator.
func (c *FieldChecker) ValidateCriterion(crit model.Criterion) error {
	if _, _, err := compileFields(crit.Args); err != nil {
		return err
	}
	return checkRef(crit.Target)
}

// Check implements Checker.
func (c *FieldChecker) Check(ctx context.Context, t Target, crit model.Criterion) (model.CheckResult, error) {
	a, reqs, err := compileFields(crit.Args)
	if err != nil {
		return model.CheckResult{}, err
	}
	ref, err := resolveTarget(t, crit)
	if err != nil {
		return model.CheckResult{}, err
	}
	secret := ref.APIVersion == "v1" && ref.Kind == "Secret"
	spec := observeSpec{
		ref: ref, timeout: timeoutFor(crit, c.set), poll: a.Wait, waitForCreate: a.Wait,
		eval: func(obj object) verdict { return evalFields(obj, reqs) },
	}
	obs, err := c.observe(ctx, spec)
	if err != nil {
		return model.CheckResult{}, err
	}
	res := c.result(obs, spec)
	if !obs.found {
		actual := map[string]any{}
		for _, r := range reqs {
			actual[r.path] = "<object not found>"
		}
		res.Evidence.Actual = actual
	}
	if res.Evidence.Expected == nil {
		res.Evidence.Expected = expectedOf(reqs, secret)
	}
	return res, nil
}

func expectedOf(reqs []requirement, secret bool) map[string]any {
	out := make(map[string]any, len(reqs))
	for _, r := range reqs {
		switch {
		case secret && sensitivePath(r.segs):
			out[r.path] = Redacted
		case r.hasEquals:
			out[r.path] = clampValue(r.equals)
		default:
			out[r.path] = map[string]any{"exists": r.exists}
		}
	}
	return out
}

// sensitivePath reports whether the path reads a Secret's data.
func sensitivePath(segs []pathSeg) bool {
	return len(segs) > 0 && segs[0].kind == segKey && (segs[0].key == "data" || segs[0].key == "stringData")
}

// secretView is the object with Secret data decoded, so criteria are written
// in plain text. It exists only inside evalFields; evidence never carries it.
func secretView(obj object) map[string]any {
	view := make(map[string]any, len(obj))
	for k, v := range obj {
		view[k] = v
	}
	if data, ok := obj["data"].(map[string]any); ok {
		dec := make(map[string]any, len(data))
		for k, v := range data {
			s, _ := v.(string)
			if b, err := base64.StdEncoding.DecodeString(s); err == nil {
				dec[k] = string(b)
			} else {
				dec[k] = v
			}
		}
		view["data"] = dec
	}
	return view
}

func evalFields(obj object, reqs []requirement) verdict {
	secret := isSecret(obj)
	root := obj
	if secret {
		root = secretView(obj)
	}
	v := verdict{ok: true}
	expected := make(map[string]any, len(reqs))
	actual := make(map[string]any, len(reqs))
	var failures []string
	for _, r := range reqs {
		sensitive := secret && sensitivePath(r.segs)
		got, found := lookup(root, r.segs)
		var fail string
		switch {
		case !r.hasEquals:
			expected[r.path] = map[string]any{"exists": r.exists}
			actual[r.path] = map[string]any{"exists": found}
			if found != r.exists {
				fail = fmt.Sprintf("%s: expected exists=%t, got exists=%t", r.path, r.exists, found)
			}
		case !found:
			expected[r.path] = r.equals
			actual[r.path] = "<absent>"
			fail = fmt.Sprintf("%s: field not present, expected %s", r.path, show(r.equals))
		default:
			expected[r.path] = r.equals
			actual[r.path] = got
			if d := r.cmp.diff(r.path, r.equals, got); d != "" {
				fail = d
			}
		}
		if sensitive {
			expected[r.path], actual[r.path] = Redacted, Redacted
			if fail != "" {
				fail = r.path + ": Secret value does not match the requirement"
			}
		} else {
			expected[r.path] = clampValue(expected[r.path])
			actual[r.path] = clampValue(actual[r.path])
		}
		if fail != "" {
			failures = append(failures, fail)
		}
	}
	v.expected, v.actual = expected, actual
	if len(failures) > 0 {
		v.ok = false
		v.reason = strings.Join(failures, "; ")
	}
	return v
}
