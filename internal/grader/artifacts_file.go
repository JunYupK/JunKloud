package grader

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
)

// ------------------------------------------------------------------- file

// maxFileBytes bounds what the grader reads of a submitted file.
const maxFileBytes = 4 << 20

// fileArgs are the Args of a "file" criterion: a file in the session
// workspace (the learner's submitted output). Target stays empty.
//
//	path:         workspace-relative slash path (required). Resolved with
//	              problem.ResolveAsset, so a symlink leading out of the
//	              workspace is never followed (spec 6.2): FAIL.
//	exists:       default true; false requires the file to be absent
//	format:       text (default) | yaml | json. yaml/json parse the file
//	select:       {api_version?, kind?, name?, namespace?} picks documents of a
//	              multi-document file
//	fields:       field requirements on the parsed document (same shape and
//	              semantics as the field checker: key order, quoting and flow
//	              style never matter). At least one selected document must
//	              satisfy all of them.
//	contains / not_contains: substrings; matches: RE2 patterns, always in
//	              multi-line mode (^ and $ match at line boundaries)
//	exact:        the whole text, only for problems that ask for exact text
//	min_bytes / max_bytes, sha256: size bounds and content digest (hex)
//
// Structure is checked, not whole-string equality, unless exact is given.
type fileArgs struct {
	Path        string        `json:"path"`
	Exists      *bool         `json:"exists"`
	Format      string        `json:"format"`
	Select      *docSelector  `json:"select"`
	Fields      []fieldInput  `json:"fields"`
	Contains    []string      `json:"contains"`
	NotContains []string      `json:"not_contains"`
	Matches     []string      `json:"matches"`
	Exact       *string       `json:"exact"`
	MinBytes    *int64        `json:"min_bytes"`
	MaxBytes    *int64        `json:"max_bytes"`
	SHA256      string        `json:"sha256"`
	reqs        []requirement `json:"-"`
	patterns    []*regexp.Regexp
}

type docSelector struct {
	APIVersion string `json:"api_version"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
}

func (s *docSelector) matches(doc map[string]any) bool {
	if s == nil {
		return true
	}
	str := func(v any) string { x, _ := v.(string); return x }
	meta, _ := doc["metadata"].(map[string]any)
	return (s.APIVersion == "" || str(doc["apiVersion"]) == s.APIVersion) &&
		(s.Kind == "" || str(doc["kind"]) == s.Kind) &&
		(s.Name == "" || str(meta["name"]) == s.Name) &&
		(s.Namespace == "" || str(meta["namespace"]) == s.Namespace)
}

func (s *docSelector) String() string {
	if s == nil {
		return "any document"
	}
	var parts []string
	for _, kv := range [][2]string{{"apiVersion", s.APIVersion}, {"kind", s.Kind}, {"name", s.Name}, {"namespace", s.Namespace}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+"="+kv[1])
		}
	}
	if len(parts) == 0 {
		return "any document"
	}
	return strings.Join(parts, ",")
}

func invalidf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidCriterion, fmt.Sprintf(format, a...))
}

// compileStrings validates a list of non-empty strings.
func compileStrings(what string, list []string) error {
	for i, s := range list {
		if s == "" {
			return invalidf("args.%s[%d] is empty", what, i)
		}
	}
	return nil
}

// compilePatterns compiles RE2 patterns.
func compilePatterns(what string, list []string) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	for i, p := range list {
		if p == "" || len(p) > 1000 {
			return nil, invalidf("args.%s[%d] must be 1..1000 characters", what, i)
		}
		re, err := regexp.Compile("(?m)" + p)
		if err != nil {
			return nil, invalidf("args.%s[%d]: %v", what, i, err)
		}
		out = append(out, re)
	}
	return out, nil
}

var sha256Hex = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

func parseFileArgs(crit model.Criterion) (fileArgs, error) {
	var a fileArgs
	if err := noTarget(crit); err != nil {
		return a, err
	}
	if err := decodeArgs(crit.Args, &a); err != nil {
		return a, err
	}
	clean := filepath.Clean(filepath.FromSlash(a.Path))
	if a.Path == "" || clean == "." || !filepath.IsLocal(clean) {
		return a, invalidf("args.path %q must be a path below the session workspace", a.Path)
	}
	switch a.Format {
	case "":
		a.Format = "text"
	case "text", "yaml", "json":
	default:
		return a, invalidf("args.format %q is not text, yaml or json", a.Format)
	}
	structured := a.Format == "yaml" || a.Format == "json"
	if (len(a.Fields) > 0 || a.Select != nil) && !structured {
		return a, invalidf("args.fields and args.select need format yaml or json")
	}
	if a.Select != nil && len(a.Fields) == 0 && *a.Select == (docSelector{}) {
		return a, invalidf("args.select is empty")
	}
	if len(a.Fields) > 0 {
		reqs, err := compileRequirements("args.fields", a.Fields)
		if err != nil {
			return a, err
		}
		a.reqs = reqs
	}
	if err := compileStringsAll(map[string][]string{"contains": a.Contains, "not_contains": a.NotContains}); err != nil {
		return a, err
	}
	pats, err := compilePatterns("matches", a.Matches)
	if err != nil {
		return a, err
	}
	a.patterns = pats
	if a.SHA256 != "" {
		if !sha256Hex.MatchString(a.SHA256) {
			return a, invalidf("args.sha256 must be 64 hex digits")
		}
		a.SHA256 = strings.ToLower(a.SHA256)
	}
	for name, v := range map[string]*int64{"min_bytes": a.MinBytes, "max_bytes": a.MaxBytes} {
		if v != nil && (*v < 0 || *v > maxFileBytes) {
			return a, invalidf("args.%s must be 0..%d", name, maxFileBytes)
		}
	}
	if a.MinBytes != nil && a.MaxBytes != nil && *a.MinBytes > *a.MaxBytes {
		return a, invalidf("args.min_bytes is larger than args.max_bytes")
	}
	hasContent := len(a.Fields) > 0 || a.Select != nil || len(a.Contains) > 0 || len(a.NotContains) > 0 ||
		len(a.Matches) > 0 || a.Exact != nil || a.MinBytes != nil || a.MaxBytes != nil || a.SHA256 != ""
	if a.Exists != nil && !*a.Exists && hasContent {
		return a, invalidf("args.exists: false cannot be combined with content requirements")
	}
	return a, nil
}

func compileStringsAll(m map[string][]string) error {
	for _, k := range sortedKeys(m) {
		if err := compileStrings(k, m[k]); err != nil {
			return err
		}
	}
	return nil
}

func (a fileArgs) wantExists() bool { return a.Exists == nil || *a.Exists }

func (c *ArtifactChecker) checkFile(ctx context.Context, t Target, crit model.Criterion) (model.CheckResult, error) {
	a, err := parseFileArgs(crit)
	if err != nil {
		return model.CheckResult{}, err
	}
	if t.Workspace == "" {
		return model.CheckResult{}, errors.New("no session workspace is configured for this target")
	}
	start := c.set.clock.Now()
	expected := map[string]any{"path": a.Path, "exists": a.wantExists()}
	fail := func(reason string, actual any) (model.CheckResult, error) {
		return c.verdictResult(start, false, reason, expected, actual), nil
	}

	real, err := problem.ResolveAsset(t.Workspace, a.Path)
	switch {
	case errors.Is(err, problem.ErrAssetEscape):
		return fail(fmt.Sprintf("%s resolves to a location outside the session workspace (a symlink?); it is not read", a.Path),
			map[string]any{"exists": false, "escapes_workspace": true})
	case errors.Is(err, problem.ErrInvalidAssetPath):
		return model.CheckResult{}, invalidf("args.path: %v", err)
	case err != nil && errors.Is(err, fs.ErrNotExist):
		// A path through a missing directory: the file is simply absent.
	case err != nil:
		return model.CheckResult{}, fmt.Errorf("resolve %s: %w", a.Path, err)
	}

	var info fs.FileInfo
	if err == nil {
		info, err = os.Stat(real)
	}
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return model.CheckResult{}, fmt.Errorf("stat %s: %w", a.Path, err)
		}
		if !a.wantExists() {
			return c.verdictResult(start, true, "", expected, map[string]any{"exists": false}), nil
		}
		return fail(fmt.Sprintf("file %s does not exist", a.Path), map[string]any{"exists": false})
	}
	if !a.wantExists() {
		return fail(fmt.Sprintf("file %s must not exist but it does", a.Path), map[string]any{"exists": true})
	}
	if info.IsDir() {
		return fail(fmt.Sprintf("%s is a directory, not a file", a.Path), map[string]any{"exists": true, "directory": true})
	}
	if !info.Mode().IsRegular() {
		return fail(fmt.Sprintf("%s is not a regular file", a.Path), map[string]any{"exists": true, "mode": info.Mode().String()})
	}
	data, err := readLimited(real, maxFileBytes)
	switch {
	case errors.Is(err, errTooLarge):
		return fail(fmt.Sprintf("%s is larger than %d bytes", a.Path, maxFileBytes), map[string]any{"exists": true, "bytes": info.Size()})
	case err != nil:
		return model.CheckResult{}, fmt.Errorf("read %s: %w", a.Path, err)
	}

	digest := sha256.Sum256(data)
	hexDigest := hex.EncodeToString(digest[:])
	actual := map[string]any{"exists": true, "bytes": len(data)}
	var failures []string
	ev := func(f string, v ...any) { failures = append(failures, fmt.Sprintf(f, v...)) }

	if a.MinBytes != nil && int64(len(data)) < *a.MinBytes {
		ev("file has %d bytes, fewer than the required %d", len(data), *a.MinBytes)
	}
	if a.MaxBytes != nil && int64(len(data)) > *a.MaxBytes {
		ev("file has %d bytes, more than the allowed %d", len(data), *a.MaxBytes)
	}
	if a.SHA256 != "" {
		expected["sha256"] = a.SHA256
		if hexDigest != a.SHA256 {
			ev("sha256 of the file differs from the required digest")
		}
	}
	text := string(data)
	if a.Exact != nil {
		expected["exact"] = true
		if text != *a.Exact {
			ev("content is not the exact required text")
		}
	}
	failures = append(failures, textRuleFailures(text, a.Contains, a.NotContains, a.Patterns())...)
	for k, v := range map[string]any{"contains": a.Contains, "not_contains": a.NotContains, "matches": a.Matches} {
		if l, ok := v.([]string); ok && len(l) > 0 {
			expected[k] = l
		}
	}

	secretDoc := false
	if a.Format != "text" {
		docs, perr := parseDocuments(a.Format, data)
		actual["documents"] = len(docs)
		redactor := redactorFor(ctx, c.set.redactor)
		for _, d := range docs {
			if isSecret(d) {
				secretDoc = true
				redactor.LearnObject(d)
			}
		}
		switch {
		case perr != nil:
			ev("%s is not valid %s: %s", a.Path, strings.ToUpper(a.Format), shortenLine(perr.Error()))
		case len(a.reqs) > 0 || a.Select != nil:
			expected["select"] = a.Select.String()
			expected["fields"] = expectedOf(a.reqs, false)
			if msg := evalDocuments(docs, a.Select, a.reqs); msg != "" {
				ev("%s", msg)
			}
		}
	}

	res := c.verdictResult(start, len(failures) == 0, strings.Join(failures, "; "), expected, actual)
	if !secretDoc {
		res.Evidence.InputHash = "sha256:" + hexDigest
	}
	return res, nil
}

// Patterns returns the compiled matches.
func (a fileArgs) Patterns() []*regexp.Regexp { return a.patterns }

// evalDocuments judges parsed documents: at least one document picked by the
// selector must satisfy every requirement. It returns "" on success.
func evalDocuments(docs []map[string]any, sel *docSelector, reqs []requirement) string {
	var selected []map[string]any
	for _, d := range docs {
		if sel.matches(d) {
			selected = append(selected, d)
		}
	}
	if len(selected) == 0 {
		return fmt.Sprintf("no document matches %s (the file has %d)", sel, len(docs))
	}
	if len(reqs) == 0 {
		return ""
	}
	first := ""
	for _, d := range selected {
		v := evalFields(d, reqs)
		if v.ok {
			return ""
		}
		if first == "" {
			first = v.reason
		}
	}
	return first
}

// textRuleFailures checks substring and pattern rules over text.
func textRuleFailures(text string, contains, notContains []string, patterns []*regexp.Regexp) []string {
	var out []string
	for _, s := range contains {
		if !strings.Contains(text, s) {
			out = append(out, fmt.Sprintf("does not contain %q", s))
		}
	}
	for _, s := range notContains {
		if strings.Contains(text, s) {
			out = append(out, fmt.Sprintf("contains forbidden %q", s))
		}
	}
	for _, re := range patterns {
		if !re.MatchString(text) {
			out = append(out, fmt.Sprintf("nothing matches /%s/", strings.TrimPrefix(re.String(), "(?m)")))
		}
	}
	return out
}

var errTooLarge = errors.New("file is too large")

// readLimited reads a regular file of at most limit bytes without blocking
// on special files.
func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errTooLarge
	}
	return data, nil
}

// parseDocuments parses a YAML (possibly multi-document) or JSON file into
// objects. Null and empty documents are skipped; a non-object document is an
// error.
func parseDocuments(format string, data []byte) ([]map[string]any, error) {
	if format == "json" {
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, err
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, errors.New("the top-level value is not an object")
		}
		return []map[string]any{m}, nil
	}
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var docs []map[string]any
	for {
		raw, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return docs, err
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var v any
		if err := yaml.Unmarshal(raw, &v); err != nil {
			return docs, err
		}
		if v == nil {
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			return docs, errors.New("a document is not a mapping")
		}
		docs = append(docs, m)
	}
	return docs, nil
}

// shortenLine keeps the first line of a parser message, bounded.
func shortenLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
