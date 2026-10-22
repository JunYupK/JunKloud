// Package localoverlay checks the local file graph before native Kustomize can
// load resources. It is shared by author execution and artifact grading.
package localoverlay

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/JunYupK/JunKloud/internal/problem"
	"sigs.k8s.io/yaml"
)

type Graph struct{ Dirs map[string]bool }
type Violation struct{ Reason string }

func (e *Violation) Error() string { return e.Reason }
func bad(f string, a ...any) error { return &Violation{fmt.Sprintf(f, a...)} }

// Inspect accepts only local inputs under workspace. Unsupported loaders fail
// closed; kubectl still owns rendering and its RootOnly load restriction.
func Inspect(ws, dir string) (Graph, error) {
	g := Graph{Dirs: map[string]bool{}}
	seen := map[string]bool{}
	var count int
	var total int64
	realWS, err := filepath.EvalSymlinks(ws)
	if err != nil {
		return g, err
	}
	inspectTree := func(root string) error {
		return filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if seen[p] {
				return nil
			}
			seen[p] = true
			count++
			if count > 4096 {
				return bad("overlay has too many entries")
			}
			rel, e := filepath.Rel(realWS, p)
			if e != nil {
				return e
			}
			_, e = problem.ResolveAsset(realWS, filepath.ToSlash(rel))
			if errors.Is(e, problem.ErrAssetEscape) {
				return bad("symlink or path outside the session workspace: %s", rel)
			}
			if e != nil {
				return e
			}
			info, e := d.Info()
			if e != nil {
				return e
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			if !d.IsDir() && !info.Mode().IsRegular() {
				return bad("nonregular overlay input")
			}
			total += info.Size()
			if total > 32<<20 {
				return bad("overlay exceeds 32 MiB")
			}
			return nil
		})
	}
	var scan func(string, int) error
	scan = func(current string, depth int) error {
		if depth > 8 {
			return bad("overlay graph exceeds depth8")
		}
		if g.Dirs[current] {
			return nil
		}
		g.Dirs[current] = true
		if err := inspectTree(current); err != nil {
			return err
		}
		var raw []byte
		for _, n := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
			b, e := os.ReadFile(filepath.Join(current, n))
			if e == nil {
				if raw != nil {
					return bad("multiple kustomization files")
				}
				raw = b
			} else if !errors.Is(e, fs.ErrNotExist) {
				return e
			}
		}
		if raw == nil {
			return bad("kustomization file is missing")
		}
		if len(raw) > 1<<20 {
			return bad("kustomization exceeds limit")
		}
		var doc map[string]any
		if err := yaml.UnmarshalStrict(raw, &doc); err != nil {
			return bad("kustomization does not build: invalid YAML")
		}
		allowed := strings.Fields("apiVersion kind metadata namespace namePrefix nameSuffix commonLabels commonAnnotations labels annotations images replicas patches patchesStrategicMerge patchesJson6902 configMapGenerator secretGenerator generatorOptions resources bases components configurations crds replacements openapi sortOptions buildMetadata")
		for k := range doc {
			ok := false
			for _, a := range allowed {
				if k == a {
					ok = true
				}
			}
			if !ok {
				return bad("unsupported Kustomize loader/field %s", k)
			}
		}
		pathInput := func(raw any, recurse bool) error {
			p, ok := raw.(string)
			if !ok || p == "" {
				return bad("local input path must be a nonempty string")
			}
			if strings.ContainsAny(p, ":\\") || filepath.IsAbs(p) || strings.HasPrefix(p, "git@") || strings.HasPrefix(p, "github.com/") || strings.HasPrefix(p, "gitlab.com/") || strings.HasPrefix(p, "bitbucket.org/") {
				return bad("remote input is unavailable: %s", p)
			}
			target := filepath.Join(current, filepath.FromSlash(p))
			rel, e := filepath.Rel(realWS, target)
			if e != nil || !filepath.IsLocal(rel) {
				return bad("input outside the session workspace: %s", p)
			}
			real, e := problem.ResolveAsset(realWS, filepath.ToSlash(rel))
			if errors.Is(e, problem.ErrAssetEscape) {
				return bad("input outside the session workspace: %s", p)
			}
			if e != nil {
				return bad("overlay does not build: local input missing: %s", p)
			}
			st, e := os.Stat(real)
			if e != nil {
				return bad("overlay does not build: local input missing: %s", p)
			}
			if st.IsDir() {
				if !recurse {
					return bad("local input must be a regular file: %s", p)
				}
				return scan(real, depth+1)
			}
			if !st.Mode().IsRegular() {
				return bad("nonregular input %s", p)
			}
			return inspectTree(real)
		}
		listPaths := func(v any, recurse bool) error {
			if v == nil {
				return nil
			}
			list, ok := v.([]any)
			if !ok {
				return bad("local path list required")
			}
			for _, v := range list {
				if e := pathInput(v, recurse); e != nil {
					return e
				}
			}
			return nil
		}
		for _, k := range []string{"resources", "bases", "components"} {
			if e := listPaths(doc[k], true); e != nil {
				return e
			}
		}
		for _, k := range []string{"configurations", "crds"} {
			if e := listPaths(doc[k], false); e != nil {
				return e
			}
		}
		for _, k := range []string{"patches", "patchesJson6902", "replacements"} {
			if doc[k] == nil {
				continue
			}
			list, ok := doc[k].([]any)
			if !ok {
				return bad("invalid %s", k)
			}
			for _, v := range list {
				m, ok := v.(map[string]any)
				if !ok {
					return bad("invalid %s entry", k)
				}
				if p, yes := m["path"]; yes {
					if e := pathInput(p, false); e != nil {
						return e
					}
				}
			}
		}
		if doc["patchesStrategicMerge"] != nil {
			list, ok := doc["patchesStrategicMerge"].([]any)
			if !ok {
				return bad("invalid patchesStrategicMerge")
			}
			for _, v := range list {
				if s, ok := v.(string); ok {
					var inline map[string]any
					if yaml.UnmarshalStrict([]byte(s), &inline) == nil && inline["apiVersion"] != nil && inline["kind"] != nil {
						continue
					}
					if e := pathInput(s, false); e != nil {
						return e
					}
				} else if _, ok := v.(map[string]any); !ok {
					return bad("invalid patchesStrategicMerge entry")
				}
			}
		}
		for _, k := range []string{"configMapGenerator", "secretGenerator"} {
			if doc[k] == nil {
				continue
			}
			list, ok := doc[k].([]any)
			if !ok {
				return bad("invalid generator")
			}
			for _, v := range list {
				m, ok := v.(map[string]any)
				if !ok {
					return bad("invalid generator entry")
				}
				for field := range m {
					if field != "name" && field != "namespace" && field != "behavior" && field != "literals" && field != "files" && field != "envs" && field != "env" && field != "options" {
						return bad("unsupported generator field %s", field)
					}
				}
				if p, yes := m["env"]; yes {
					if e := pathInput(p, false); e != nil {
						return e
					}
				}
				if e := listPaths(m["envs"], false); e != nil {
					return e
				}
				if m["files"] != nil {
					files, ok := m["files"].([]any)
					if !ok {
						return bad("invalid generator files")
					}
					for _, f := range files {
						s, ok := f.(string)
						if !ok {
							return bad("invalid generator file")
						}
						if _, p, found := strings.Cut(s, "="); found {
							s = p
						}
						if e := pathInput(s, false); e != nil {
							return e
						}
					}
				}
			}
		}
		if doc["openapi"] != nil || doc["crds"] != nil {
			return bad("unsupported external schema loader")
		}
		return nil
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return g, err
	}
	rel, err := filepath.Rel(realWS, realDir)
	if err != nil || !filepath.IsLocal(rel) {
		return g, bad("overlay outside the session workspace")
	}
	return g, scan(realDir, 0)
}
