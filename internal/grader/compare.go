package grader

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Semantic comparison and field paths for the field checker.
//
// A path is dotted keys with optional brackets:
//
//	spec.replicas
//	spec.template.spec.containers[name=web].image   named list element
//	spec.template.spec.containers[0].image          list index
//	metadata.annotations['example.com/note']        key that holds dots or slashes
//
// Selector values compare as text, so [port=80] matches the number 80.

type segKind int

const (
	segKey segKind = iota
	segIndex
	segSelect
)

type pathSeg struct {
	kind     segKind
	key      string // segKey: map key; segSelect: field name
	index    int    // segIndex
	selValue string // segSelect
}

// parsePath compiles a field path. Errors are descriptive and contain no
// sentinel: callers wrap them in ErrInvalidCriterion.
func parsePath(p string) ([]pathSeg, error) {
	if p == "" {
		return nil, fmt.Errorf("empty path")
	}
	var segs []pathSeg
	i := 0
	for {
		start := i
		for i < len(p) && p[i] != '.' && p[i] != '[' {
			i++
		}
		if i == start {
			return nil, fmt.Errorf("path %q: empty key at offset %d", p, start)
		}
		segs = append(segs, pathSeg{kind: segKey, key: p[start:i]})
		for i < len(p) && p[i] == '[' {
			seg, next, err := parseBracket(p, i)
			if err != nil {
				return nil, err
			}
			segs = append(segs, seg)
			i = next
		}
		if i == len(p) {
			return segs, nil
		}
		if p[i] != '.' {
			return nil, fmt.Errorf("path %q: unexpected %q at offset %d", p, p[i], i)
		}
		i++
		if i == len(p) {
			return nil, fmt.Errorf("path %q: trailing dot", p)
		}
	}
}

// parseBracket parses the bracket that starts at p[open] and returns the
// segment and the offset after the closing bracket.
func parseBracket(p string, open int) (pathSeg, int, error) {
	i := open + 1
	if i < len(p) && (p[i] == '\'' || p[i] == '"') {
		q := p[i]
		end := strings.IndexByte(p[i+1:], q)
		if end < 0 || i+1+end+1 >= len(p) || p[i+1+end+1] != ']' {
			return pathSeg{}, 0, fmt.Errorf("path %q: unterminated quoted key at offset %d", p, open)
		}
		return pathSeg{kind: segKey, key: p[i+1 : i+1+end]}, i + 1 + end + 2, nil
	}
	end := strings.IndexByte(p[i:], ']')
	if end < 0 {
		return pathSeg{}, 0, fmt.Errorf("path %q: unclosed bracket at offset %d", p, open)
	}
	body := p[i : i+end]
	next := i + end + 1
	if n, err := strconv.Atoi(body); err == nil {
		if n < 0 {
			return pathSeg{}, 0, fmt.Errorf("path %q: negative index", p)
		}
		return pathSeg{kind: segIndex, index: n}, next, nil
	}
	k, v, ok := strings.Cut(body, "=")
	k = strings.TrimSpace(k)
	v = strings.Trim(strings.TrimSpace(v), `'"`)
	if !ok || k == "" {
		return pathSeg{}, 0, fmt.Errorf("path %q: bracket %q must be an index, [key=value] or a quoted key", p, body)
	}
	return pathSeg{kind: segSelect, key: k, selValue: v}, next, nil
}

// lookup follows segs from cur.
func lookup(cur any, segs []pathSeg) (any, bool) {
	for _, s := range segs {
		switch s.kind {
		case segKey:
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, false
			}
			if cur, ok = m[s.key]; !ok {
				return nil, false
			}
		case segIndex:
			l, ok := cur.([]any)
			if !ok || s.index >= len(l) {
				return nil, false
			}
			cur = l[s.index]
		case segSelect:
			l, ok := cur.([]any)
			if !ok {
				return nil, false
			}
			var found any
			hit := false
			for _, e := range l {
				if m, ok := e.(map[string]any); ok && scalarText(m[s.key]) == s.selValue {
					found, hit = e, true
					break
				}
			}
			if !hit {
				return nil, false
			}
			cur = found
		}
	}
	return cur, true
}

// scalarText renders a scalar the way a selector value is written.
func scalarText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case nil:
		return "\x00"
	}
	if r, ok := ratOf(v); ok {
		if r.IsInt() {
			return r.Num().String()
		}
		f, _ := r.Float64()
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return "\x00"
}

// quantityKeys are the map keys under which values are resource quantities.
var quantityKeys = map[string]bool{"requests": true, "limits": true, "hard": true, "capacity": true}

// autoQuantity reports whether the path goes through a quantity-valued map.
func autoQuantity(segs []pathSeg) bool {
	for _, s := range segs {
		if s.kind == segKey && quantityKeys[s.key] {
			return true
		}
	}
	return false
}

// containerListKeys are the pod-spec lists whose elements have an image.
var containerListKeys = map[string]bool{"containers": true, "initContainers": true, "ephemeralContainers": true}

// autoContext derives, from the compared path, where in a pod spec the
// compared value sits so that auto mode can apply image semantics:
// image (the value is a container image), container (the value is one
// container object) and containerList (the value is a container list).
func autoContext(segs []pathSeg) (image, container, containerList bool) {
	n := len(segs)
	keyAt := func(i int) (string, bool) {
		if i < 0 || segs[i].kind != segKey {
			return "", false
		}
		return segs[i].key, true
	}
	if n > 0 {
		if k, ok := keyAt(n - 1); ok && containerListKeys[k] {
			return false, false, true
		}
	}
	if n > 1 && segs[n-1].kind != segKey {
		if k, ok := keyAt(n - 2); ok && containerListKeys[k] {
			return false, true, false
		}
	}
	if n > 2 {
		if k, ok := keyAt(n - 1); ok && k == "image" && segs[n-2].kind != segKey {
			if lk, ok := keyAt(n - 3); ok && containerListKeys[lk] {
				return true, false, false
			}
		}
	}
	return false, false, false
}

// Comparison options.
const (
	matchEqual     = "equal"
	matchSubset    = "subset"
	matchUnordered = "unordered"

	compareAuto     = "auto"
	compareExact    = "exact"
	compareQuantity = "quantity"
	compareImage    = "image"
)

// comparer compares an expected value with a live one. In auto mode it
// switches to quantity semantics below requests/limits/hard/capacity maps and
// to image semantics for container images, wherever they are reached from
// the compared value (so a whole resources map or a whole container object
// compares like its fields would).
type comparer struct {
	match    string
	quantity bool
	image    bool
	auto     bool
	// container: the map being compared is a container; containerList: the
	// list being compared is a containers/initContainers/ephemeralContainers
	// list. Only meaningful in auto mode.
	container     bool
	containerList bool
}

// diff returns "" when got satisfies want, otherwise a short description of
// the first difference. want comes from the criterion, got from the live
// object.
func (c comparer) diff(path string, want, got any) string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return mismatch(path, want, got)
		}
		for _, k := range sortedKeys(w) {
			gv, ok := g[k]
			if !ok {
				return fmt.Sprintf("%s.%s: missing, expected %s", path, k, show(w[k]))
			}
			child := c
			child.container, child.containerList = false, false
			if c.auto {
				if quantityKeys[k] {
					child.quantity = true
				}
				if c.container && k == "image" {
					child.image = true
				}
				child.containerList = containerListKeys[k]
			}
			if d := child.diff(path+"."+k, w[k], gv); d != "" {
				return d
			}
		}
		if c.match != matchSubset {
			for _, k := range sortedKeys(g) {
				if _, ok := w[k]; !ok {
					return fmt.Sprintf("%s.%s: unexpected", path, k)
				}
			}
		}
		return ""
	case []any:
		g, ok := got.([]any)
		if !ok {
			return mismatch(path, want, got)
		}
		return c.diffList(path, w, g)
	default:
		return c.diffScalar(path, want, got)
	}
}

func (c comparer) diffScalar(path string, want, got any) string {
	if want == nil || got == nil {
		if want == nil && got == nil {
			return ""
		}
		return mismatch(path, want, got)
	}
	if c.quantity {
		qw, okw := quantityOf(want)
		qg, okg := quantityOf(got)
		if okw && okg {
			if qw.Cmp(qg) == 0 {
				return ""
			}
			return mismatch(path, want, got)
		}
	}
	ws, wIsStr := want.(string)
	gs, gIsStr := got.(string)
	if wIsStr || gIsStr {
		if !(wIsStr && gIsStr) {
			return mismatch(path, want, got)
		}
		if c.image {
			if normalizeImage(ws) == normalizeImage(gs) {
				return ""
			}
			return mismatch(path, want, got)
		}
		if ws == gs {
			return ""
		}
		return mismatch(path, want, got)
	}
	if wb, ok := want.(bool); ok {
		if gb, ok := got.(bool); ok && wb == gb {
			return ""
		}
		return mismatch(path, want, got)
	}
	rw, okw := ratOf(want)
	rg, okg := ratOf(got)
	if okw && okg && rw.Cmp(rg) == 0 {
		return ""
	}
	return mismatch(path, want, got)
}

// namedElements returns the elements keyed by their "name" when every
// element is an object with a distinct string name.
func namedElements(l []any) (map[string]any, bool) {
	if len(l) == 0 {
		return nil, false
	}
	out := make(map[string]any, len(l))
	for _, e := range l {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, false
		}
		n, ok := m["name"].(string)
		if !ok {
			return nil, false
		}
		if _, dup := out[n]; dup {
			return nil, false
		}
		out[n] = e
	}
	return out, true
}

func (c comparer) diffList(path string, want, got []any) string {
	// Elements of a container list are containers.
	c.container, c.containerList = c.containerList, false
	// Named objects (containers, env vars, ports, volumes) match by name,
	// never by position.
	if wn, ok := namedElements(want); ok {
		if gn, ok := namedElements(got); ok {
			for _, name := range sortedKeys(wn) {
				g, ok := gn[name]
				if !ok {
					return fmt.Sprintf("%s[name=%s]: missing", path, name)
				}
				if d := c.diff(fmt.Sprintf("%s[name=%s]", path, name), wn[name], g); d != "" {
					return d
				}
			}
			if c.match != matchSubset && len(gn) != len(wn) {
				for _, name := range sortedKeys(gn) {
					if _, ok := wn[name]; !ok {
						return fmt.Sprintf("%s[name=%s]: unexpected", path, name)
					}
				}
			}
			return ""
		}
	}
	switch c.match {
	case matchSubset:
		if !c.assign(want, got) {
			return fmt.Sprintf("%s: expected elements %s not all present in %s", path, show(want), show(got))
		}
		return ""
	case matchUnordered:
		if len(want) != len(got) {
			return fmt.Sprintf("%s: expected %d elements, got %d", path, len(want), len(got))
		}
		if !c.assign(want, got) {
			return mismatch(path, want, got)
		}
		return ""
	default:
		if len(want) != len(got) {
			return fmt.Sprintf("%s: expected %d elements, got %d (%s)", path, len(want), len(got), show(got))
		}
		for i := range want {
			if d := c.diff(fmt.Sprintf("%s[%d]", path, i), want[i], got[i]); d != "" {
				return d
			}
		}
		return ""
	}
}

// assign finds a one-to-one assignment of every want element to a got
// element that satisfies it (bipartite matching).
func (c comparer) assign(want, got []any) bool {
	owner := make([]int, len(got)) // got index -> want index, -1 free
	for i := range owner {
		owner[i] = -1
	}
	var try func(w int, seen []bool) bool
	try = func(w int, seen []bool) bool {
		for g := range got {
			if seen[g] || c.diff("", want[w], got[g]) != "" {
				continue
			}
			seen[g] = true
			if owner[g] < 0 || try(owner[g], seen) {
				owner[g] = w
				return true
			}
		}
		return false
	}
	for w := range want {
		if !try(w, make([]bool, len(got))) {
			return false
		}
	}
	return true
}

func mismatch(path string, want, got any) string {
	return fmt.Sprintf("%s: expected %s, got %s", path, show(want), show(got))
}

func show(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	if len(raw) > 120 {
		return string(raw[:120]) + "..."
	}
	return string(raw)
}

// ratOf converts any Go or JSON number to an exact rational.
func ratOf(v any) (*big.Rat, bool) {
	switch x := v.(type) {
	case int:
		return new(big.Rat).SetInt64(int64(x)), true
	case int32:
		return new(big.Rat).SetInt64(int64(x)), true
	case int64:
		return new(big.Rat).SetInt64(x), true
	case float32:
		return ratOf(float64(x))
	case float64:
		r := new(big.Rat)
		if r.SetFloat64(x) == nil {
			return nil, false
		}
		return r, true
	case json.Number:
		return new(big.Rat).SetString(x.String())
	}
	return nil, false
}

// quantityOf parses a Kubernetes quantity from a string or a number.
func quantityOf(v any) (resource.Quantity, bool) {
	switch x := v.(type) {
	case string:
		q, err := resource.ParseQuantity(x)
		return q, err == nil
	case json.Number:
		q, err := resource.ParseQuantity(x.String())
		return q, err == nil
	}
	if r, ok := ratOf(v); ok {
		q, err := resource.ParseQuantity(r.FloatString(9))
		if err != nil {
			return resource.Quantity{}, false
		}
		return q, true
	}
	return resource.Quantity{}, false
}

// normalizeImage expands the defaults of an image reference so that
// "nginx" and "docker.io/library/nginx:latest" compare equal. Only used when
// a criterion asks for compare: image; images are otherwise exact.
func normalizeImage(ref string) string {
	name, digest, _ := strings.Cut(ref, "@")
	registry, rest := "docker.io", name
	if i := strings.IndexByte(name, '/'); i >= 0 {
		first := name[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			registry, rest = first, name[i+1:]
		}
	}
	if registry == "index.docker.io" {
		registry = "docker.io"
	}
	repo, tag := rest, ""
	if i := strings.LastIndexByte(rest, ':'); i >= 0 && !strings.Contains(rest[i:], "/") {
		repo, tag = rest[:i], rest[i+1:]
	}
	if registry == "docker.io" && !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	if tag == "" && digest == "" {
		tag = "latest"
	}
	out := registry + "/" + repo
	if tag != "" {
		out += ":" + tag
	}
	if digest != "" {
		out += "@" + digest
	}
	return out
}
