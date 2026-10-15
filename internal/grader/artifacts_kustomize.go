package grader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
)

// kustomizeArgs are the Args of a "kustomize" criterion (Target stays
// empty). The learner's overlay directory is rendered with the kustomize
// built into the pinned kubectl (`kubectl kustomize`), the rendered objects
// are compared as objects (key order, quoting and flow style never matter),
// and the deployed objects must match what the overlay renders.
//
//	path:    workspace-relative overlay directory (required)
//	base:    {path, protect?: true, uses?: true}: the provided base directory.
//	         protect: every file the problem provided under it must be
//	         unchanged and none added or removed (modifying the base instead
//	         of overlaying it is FAIL); uses: the overlay's kustomization
//	         must reach this base through its resources/bases.
//	expect:  [{api_version, kind, name, namespace?, fields?, absent?, live?}]
//	         the rendered object must exist and satisfy fields (same shape
//	         as the field checker); absent: it must not be rendered. At least
//	         one of base and expect is required.
//	live:    default true: each expected object must also exist in the
//	         cluster, satisfy the same fields, and contain what the overlay
//	         rendered for it (spec/data and labels/annotations). Per-entry
//	         live overrides.
//
// Why kubectl's kustomize and not the sigs.k8s.io/kustomize/api library: it
// is the renderer the learner runs themselves (`kubectl apply -k`), it is
// already a pinned, checksum-verified tool (kubectl v1.37.0), and the
// library would add a large dependency tree that can drift from kubectl's.
// Rendering uses the default LoadRestrictionsRootOnly, no exec plugins or
// Helm inflation, and no cluster access. Remote resources (URLs, git) and
// symlinks leading out of the workspace are refused before rendering.
type kustomizeArgs struct {
	Path   string     `json:"path"`
	Base   *kzBase    `json:"base"`
	Expect []kzExpect `json:"expect"`
	Live   *bool      `json:"live"`
}

type kzBase struct {
	Path    string `json:"path"`
	Protect *bool  `json:"protect"`
	Uses    *bool  `json:"uses"`
}

type kzExpect struct {
	APIVersion string       `json:"api_version"`
	Kind       string       `json:"kind"`
	Name       string       `json:"name"`
	Namespace  string       `json:"namespace"`
	Fields     []fieldInput `json:"fields"`
	Absent     bool         `json:"absent"`
	Live       *bool        `json:"live"`

	reqs []requirement
}

func (e kzExpect) String() string {
	return fmt.Sprintf("%s %s", e.Kind, e.Name)
}

func localDir(what, p string) error {
	clean := path.Clean(p)
	if p == "" || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
		return invalidf("%s %q must be a path below the session workspace", what, p)
	}
	return nil
}

func parseKustomizeArgs(crit model.Criterion) (kustomizeArgs, error) {
	var a kustomizeArgs
	if err := noTarget(crit); err != nil {
		return a, err
	}
	if err := decodeArgs(crit.Args, &a); err != nil {
		return a, err
	}
	if err := localDir("args.path", a.Path); err != nil {
		return a, err
	}
	if a.Base == nil && len(a.Expect) == 0 {
		return a, invalidf("kustomize needs args.base or args.expect")
	}
	if a.Base != nil {
		if err := localDir("args.base.path", a.Base.Path); err != nil {
			return a, err
		}
	}
	for i := range a.Expect {
		e := &a.Expect[i]
		if e.APIVersion == "" || e.Kind == "" || e.Name == "" {
			return a, invalidf("args.expect[%d] needs api_version, kind and name", i)
		}
		if e.Absent && (len(e.Fields) > 0 || e.Live != nil) {
			return a, invalidf("args.expect[%d]: absent cannot be combined with fields or live", i)
		}
		if len(e.Fields) > 0 {
			reqs, err := compileRequirements(fmt.Sprintf("args.expect[%d].fields", i), e.Fields)
			if err != nil {
				return a, err
			}
			e.reqs = reqs
		}
	}
	return a, nil
}

func (a kustomizeArgs) live(e kzExpect) bool {
	if e.Live != nil {
		return *e.Live
	}
	return a.Live == nil || *a.Live
}

func (b kzBase) protect() bool { return b.Protect == nil || *b.Protect }
func (b kzBase) uses() bool    { return b.Uses == nil || *b.Uses }

var kustomizationNames = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

// kzGraph is what scanning an overlay's kustomization files found.
type kzGraph struct {
	dirs   map[string]bool // real directories reached (including the overlay)
	remote string          // first remote reference, if any
	escape string          // first path leading out of the workspace, if any
}

// scanKustomizations follows resources/bases/components directories from dir.
// It never renders anything; it only reads the kustomization files.
func scanKustomizations(ws, dir string, g *kzGraph, depth int) error {
	realWS, err := filepath.EvalSymlinks(ws)
	if err != nil {
		return err
	}
	if g.dirs[dir] || depth > 8 {
		return nil
	}
	g.dirs[dir] = true
	var raw []byte
	for _, n := range kustomizationNames {
		if b, err := os.ReadFile(filepath.Join(dir, n)); err == nil {
			raw = b
			break
		}
	}
	if raw == nil {
		return nil // a plain directory of resources, or no kustomization yet: kustomize reports it
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil // kustomize will report the syntax error with its own message
	}
	for _, key := range []string{"resources", "bases", "components"} {
		list, _ := doc[key].([]any)
		for _, e := range list {
			entry, _ := e.(string)
			if entry == "" {
				continue
			}
			if isRemoteResource(entry) {
				if g.remote == "" {
					g.remote = entry
				}
				continue
			}
			target := filepath.Join(dir, filepath.FromSlash(entry))
			rel, err := filepath.Rel(realWS, target)
			if err != nil || !filepath.IsLocal(rel) && rel != "." {
				if g.escape == "" {
					g.escape = entry
				}
				continue
			}
			real, err := problem.ResolveAsset(realWS, filepath.ToSlash(rel))
			if errors.Is(err, problem.ErrAssetEscape) {
				if g.escape == "" {
					g.escape = entry
				}
				continue
			}
			if err != nil {
				continue // missing: kustomize reports it
			}
			if st, err := os.Stat(real); err == nil && st.IsDir() {
				if err := scanKustomizations(ws, real, g, depth+1); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func isRemoteResource(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "://") || strings.HasPrefix(l, "git@") || strings.HasPrefix(l, "git::") ||
		strings.HasPrefix(l, "github.com/") || strings.HasPrefix(l, "gitlab.com/") || strings.HasPrefix(l, "bitbucket.org/")
}

// symlinkEscape walks dir (without following links) and returns the first
// symlink that resolves outside the workspace.
func symlinkEscape(ws, dir string) (string, error) {
	realWS, err := filepath.EvalSymlinks(ws)
	if err != nil {
		return "", err
	}
	var bad string
	n := 0
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if n++; n > maxTreeEntries {
			return fmt.Errorf("directory has more than %d entries", maxTreeEntries)
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		rel, err := filepath.Rel(realWS, p)
		if err != nil {
			return err
		}
		if _, err := problem.ResolveAsset(realWS, filepath.ToSlash(rel)); errors.Is(err, problem.ErrAssetEscape) {
			bad = filepath.ToSlash(rel)
			return fs.SkipAll
		}
		return nil
	})
	return bad, err
}

func (c *ArtifactChecker) checkKustomize(ctx context.Context, t Target, crit model.Criterion) (model.CheckResult, error) {
	a, err := parseKustomizeArgs(crit)
	if err != nil {
		return model.CheckResult{}, err
	}
	if c.cfg.Tools == nil {
		return model.CheckResult{}, notConfigured("kustomize checks need kubectl (ArtifactConfig.Tools)")
	}
	if t.Workspace == "" {
		return model.CheckResult{}, errors.New("no session workspace is configured for this target")
	}
	start := c.set.clock.Now()
	expected := map[string]any{"path": a.Path}
	actual := map[string]any{}
	fail := func(f string, v ...any) (model.CheckResult, error) {
		return c.verdictResult(start, false, fmt.Sprintf(f, v...), expected, actual), nil
	}

	overlay, kind, err := c.resolveDir(t.Workspace, a.Path)
	if err != nil {
		return model.CheckResult{}, err
	}
	if kind != "" {
		return fail("overlay %s: %s", a.Path, kind)
	}
	var baseDir string
	if a.Base != nil {
		expected["base"] = a.Base.Path
		baseDir, kind, err = c.resolveDir(t.Workspace, a.Base.Path)
		if err != nil {
			return model.CheckResult{}, err
		}
		if kind != "" && a.Base.protect() {
			return fail("base %s was modified: %s", a.Base.Path, kind)
		}
	}

	// Look at the overlay's references before rendering anything.
	g := &kzGraph{dirs: map[string]bool{}}
	if err := scanKustomizations(t.Workspace, overlay, g, 0); err != nil {
		return model.CheckResult{}, fmt.Errorf("scan %s: %w", a.Path, err)
	}
	if g.remote != "" {
		return fail("overlay %s references a remote resource (%s); remote bases are not available when grading", a.Path, g.remote)
	}
	if g.escape != "" {
		return fail("overlay %s references %s, which is outside the session workspace", a.Path, g.escape)
	}
	for dir := range g.dirs {
		if bad, err := symlinkEscape(t.Workspace, dir); err != nil {
			return model.CheckResult{}, fmt.Errorf("scan %s: %w", a.Path, err)
		} else if bad != "" {
			return fail("%s is a symlink to a location outside the session workspace", bad)
		}
	}

	var failures []string
	if a.Base != nil {
		if a.Base.protect() {
			mods, err := baseModifications(t, a.Base.Path, baseDir)
			if err != nil {
				return model.CheckResult{}, err
			}
			if len(mods) > 0 {
				failures = append(failures, fmt.Sprintf("the provided base %s was modified (%s); change it with an overlay instead", a.Base.Path, strings.Join(boundedList(mods, 5), ", ")))
			}
		}
		if a.Base.uses() && baseDir != "" && !g.dirs[baseDir] {
			failures = append(failures, fmt.Sprintf("overlay %s does not use the provided base %s", a.Path, a.Base.Path))
		}
	}

	hash, err := hashTree(overlay)
	if err != nil {
		return model.CheckResult{}, fmt.Errorf("fingerprint %s: %w", a.Path, err)
	}

	docs, buildErr, err := c.renderOverlay(ctx, overlay)
	if err != nil {
		return model.CheckResult{}, err
	}
	if buildErr != "" {
		failures = append(failures, fmt.Sprintf("overlay %s does not build: %s", a.Path, buildErr))
		res := c.verdictResult(start, false, strings.Join(failures, "; "), expected, actual)
		res.Evidence.InputHash = hash
		return res, nil
	}
	actual["rendered_objects"] = len(docs)

	rf, err := c.compareRendered(ctx, t, a, docs, expected, actual)
	if err != nil {
		return model.CheckResult{}, err
	}
	failures = append(failures, rf...)
	res := c.verdictResult(start, len(failures) == 0, strings.Join(failures, "; "), expected, actual)
	res.Evidence.InputHash = hash
	return res, nil
}

// resolveDir resolves a workspace directory. problem is a learner-facing
// description when it cannot be used ("" when fine).
func (c *ArtifactChecker) resolveDir(ws, p string) (real, problemText string, err error) {
	real, err = problem.ResolveAsset(ws, p)
	switch {
	case errors.Is(err, problem.ErrAssetEscape):
		return "", "resolves to a location outside the session workspace", nil
	case err != nil && errors.Is(err, fs.ErrNotExist):
		return "", "directory does not exist", nil
	case err != nil:
		return "", "", fmt.Errorf("resolve %s: %w", p, err)
	}
	st, err := os.Stat(real)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "directory does not exist", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("stat %s: %w", p, err)
	}
	if !st.IsDir() {
		return "", "is not a directory", nil
	}
	return real, "", nil
}

// renderOverlay runs kubectl's kustomize. buildErr is the learner-facing
// message when the overlay does not build.
func (c *ArtifactChecker) renderOverlay(ctx context.Context, dir string) ([]map[string]any, string, error) {
	res, err := c.cfg.Tools.Run(ctx, ToolCall{Exe: c.cfg.KubectlBin, Args: []string{"kustomize", "--load-restrictor=LoadRestrictionsRootOnly", dir}})
	if err != nil {
		return nil, "", fmt.Errorf("kubectl kustomize: %w", err)
	}
	if res.ExitCode != 0 {
		msg := firstErrorLine(string(res.Stderr))
		return nil, strings.ReplaceAll(msg, dir, "<overlay>"), nil
	}
	docs, err := parseDocuments("yaml", res.Stdout)
	if err != nil {
		return nil, "", fmt.Errorf("kubectl kustomize produced unreadable YAML: %v", err)
	}
	return docs, "", nil
}

func firstErrorLine(s string) string {
	var first string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if first == "" {
			first = line
		}
		if strings.HasPrefix(line, "Error") {
			first = line
			break
		}
	}
	return shortenLine(first)
}

// baseModifications lists the provided base files that were changed, added
// or removed. The provided files are the frozen problem's assets under the
// base path.
func baseModifications(t Target, basePath, baseDir string) ([]string, error) {
	prefix := path.Clean(basePath) + "/"
	provided := map[string][]byte{}
	for k, v := range t.Problem.Assets {
		if strings.HasPrefix(path.Clean(k), prefix) {
			provided[path.Clean(k)] = v
		}
	}
	if len(provided) == 0 {
		return nil, fmt.Errorf("%w: the problem provides no files under %s, so the base cannot be protected", ErrInvalidCriterion, basePath)
	}
	var mods []string
	present := map[string]bool{}
	if baseDir != "" {
		err := filepath.WalkDir(baseDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(baseDir, p)
			key := prefix + filepath.ToSlash(rel)
			present[key] = true
			want, ok := provided[key]
			if !ok {
				mods = append(mods, "added "+key)
				return nil
			}
			got, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if !bytes.Equal(normalizeEOL(got), normalizeEOL(want)) {
				mods = append(mods, "changed "+key)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", basePath, err)
		}
	}
	for key := range provided {
		if !present[key] {
			mods = append(mods, "removed "+key)
		}
	}
	sort.Strings(mods)
	return mods, nil
}

func boundedList(l []string, n int) []string {
	if len(l) <= n {
		return l
	}
	return append(append([]string(nil), l[:n]...), fmt.Sprintf("and %d more", len(l)-n))
}

// compareRendered judges the rendered objects (and their deployed twins).
func (c *ArtifactChecker) compareRendered(ctx context.Context, t Target, a kustomizeArgs, docs []map[string]any, expected, actual map[string]any) ([]string, error) {
	red := redactorFor(ctx, c.set.redactor)
	for _, d := range docs {
		red.LearnObject(d)
	}
	var failures []string
	var exp, act []any
	for _, e := range a.Expect {
		doc, found := findRendered(docs, e)
		entryExp := map[string]any{"object": e.String(), "absent": e.Absent}
		entryAct := map[string]any{"rendered": found}
		exp, act = append(exp, entryExp), append(act, entryAct)
		switch {
		case e.Absent:
			if found {
				failures = append(failures, fmt.Sprintf("%s must not be rendered by the overlay but it is", e))
			}
			continue
		case !found:
			failures = append(failures, fmt.Sprintf("%s is not rendered by the overlay", e))
			continue
		}
		if len(e.reqs) > 0 {
			entryExp["fields"] = expectedOf(e.reqs, isSecret(doc))
			if v := evalFields(doc, e.reqs); !v.ok {
				failures = append(failures, fmt.Sprintf("rendered %s: %s", e, v.reason))
				continue
			}
		}
		if !a.live(e) {
			continue
		}
		if c.cfg.Reader == nil {
			return nil, notConfigured("deployed-state checks need an object reader")
		}
		lf, err := c.compareLive(ctx, t, e, doc, entryAct)
		if err != nil {
			return nil, err
		}
		failures = append(failures, lf...)
	}
	expected["expect"], actual["expect"] = exp, act
	return failures, nil
}

func findRendered(docs []map[string]any, e kzExpect) (map[string]any, bool) {
	for _, d := range docs {
		meta := asMap(d["metadata"])
		if d["apiVersion"] == e.APIVersion && d["kind"] == e.Kind && meta["name"] == e.Name {
			if e.Namespace != "" && meta["namespace"] != e.Namespace {
				continue
			}
			return d, true
		}
	}
	return nil, false
}

// compareLive checks the deployed object against the expectation and against
// what the overlay rendered for it.
func (c *ArtifactChecker) compareLive(ctx context.Context, t Target, e kzExpect, rendered map[string]any, entryAct map[string]any) ([]string, error) {
	ns, _ := asMap(rendered["metadata"])["namespace"].(string)
	if ns == "" {
		ns = e.Namespace
	}
	if ns == "" {
		ns = problemNamespace(t)
	}
	ref := model.ObjectRef{APIVersion: e.APIVersion, Kind: e.Kind, Namespace: ns, Name: e.Name}
	live, err := c.obs.get(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		entryAct["deployed"] = false
		return []string{fmt.Sprintf("deployed %s %s/%s not found: the overlay was not applied", e.Kind, ns, e.Name)}, nil
	}
	if err != nil {
		return nil, err
	}
	entryAct["deployed"] = true
	var failures []string
	if len(e.reqs) > 0 {
		if v := evalFields(live, e.reqs); !v.ok {
			failures = append(failures, fmt.Sprintf("deployed %s: %s", e, v.reason))
		}
	}
	cmp := comparer{match: matchSubset, auto: true}
	for _, key := range sortedKeys(rendered) {
		switch key {
		case "apiVersion", "kind", "status":
			continue
		case "metadata":
			for _, sub := range []string{"labels", "annotations"} {
				want := asMap(asMap(rendered["metadata"])[sub])
				if len(want) == 0 {
					continue
				}
				if d := cmp.diff("metadata."+sub, want, asMap(asMap(live["metadata"])[sub])); d != "" {
					failures = append(failures, fmt.Sprintf("deployed %s differs from the rendered overlay: %s", e, d))
				}
			}
			continue
		}
		got, ok := live[key]
		if !ok {
			failures = append(failures, fmt.Sprintf("deployed %s lacks %s that the overlay renders", e, key))
			continue
		}
		if d := cmp.diff(key, rendered[key], got); d != "" {
			failures = append(failures, fmt.Sprintf("deployed %s differs from the rendered overlay: %s", e, d))
		}
	}
	return failures, nil
}
