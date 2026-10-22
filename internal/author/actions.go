//go:build linux

package author

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
)

// actionDirs are the package directories an action may read manifests from:
// the private setup, solution and case content.
var actionDirs = []string{"setup/", "solution/", "cases/"}

// BuildActions turns a problem's declarative actions (setup, solution or one
// case) into the rendered environment.Actions the Manager applies. It is the
// manifest decoder for the runtime's apply/delete-owned path (R15):
//
//   - apply: args {path} names a manifest in the problem package (under
//     setup/, solution/ or cases/). The file may hold several YAML documents;
//     each is decoded and rendered with problem.RenderValue(doc, params) and
//     becomes one object of a single environment.Action.
//   - delete-owned: args {targets: [{api_version, kind, namespace?, name}]},
//     rendered, deleted by the Manager only if they are in the owner's
//     inventory.
//   - helm, kustomize, image, file, patch: unsupported by this decoder. The
//     author ActionExecutor separately handles staged file/image/native tools.
//     There is no shell or eval action.
//
// files is the unrendered package (model.Problem.Files); params are the
// frozen parameters. Args are strict: an unknown key, a missing or escaping
// path, a missing file or a document that is not an object is an author error
// (environment.ErrInvalidAction).
func BuildActions(files map[string][]byte, params map[string]any, actions []model.Action) ([]environment.Action, error) {
	out := make([]environment.Action, 0, len(actions))
	for i, a := range actions {
		built, err := buildAction(files, params, a)
		if err != nil {
			return nil, fmt.Errorf("action %d (%s): %w", i, a.Kind, err)
		}
		out = append(out, built)
	}
	return out, nil
}

func buildAction(files map[string][]byte, params map[string]any, a model.Action) (environment.Action, error) {
	switch a.Kind {
	case model.ActionApply:
		if err := onlyArgs(a.Args, "path"); err != nil {
			return environment.Action{}, err
		}
		rel, ok := a.Args["path"].(string)
		if !ok || rel == "" {
			return environment.Action{}, invalid("args.path must name a package file")
		}
		objects, err := manifestObjects(files, params, rel)
		if err != nil {
			return environment.Action{}, err
		}
		return environment.Action{Kind: model.ActionApply, Objects: objects}, nil

	case model.ActionDeleteOwned:
		if err := onlyArgs(a.Args, "targets"); err != nil {
			return environment.Action{}, err
		}
		rendered, err := problem.RenderValue(a.Args["targets"], params)
		if err != nil {
			return environment.Action{}, err
		}
		list, ok := rendered.([]any)
		if !ok || len(list) == 0 {
			return environment.Action{}, invalid("args.targets must be a non-empty list")
		}
		targets := make([]model.ObjectRef, 0, len(list))
		for j, item := range list {
			ref, err := objectRef(item)
			if err != nil {
				return environment.Action{}, invalid("args.targets[%d]: %v", j, err)
			}
			targets = append(targets, ref)
		}
		return environment.Action{Kind: model.ActionDeleteOwned, Targets: targets}, nil

	case model.ActionHelm, model.ActionKustomize, model.ActionImage, model.ActionFile, model.ActionPatch:
		return environment.Action{}, fmt.Errorf("%w: problem validation cannot run %q actions yet", environment.ErrUnsupportedAction, a.Kind)
	default:
		return environment.Action{}, invalid("unknown action kind %q", a.Kind)
	}
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", environment.ErrInvalidAction, fmt.Sprintf(format, args...))
}

// onlyArgs requires args to contain exactly the keys in allowed (all of them
// present), so a misspelled key cannot silently turn into a no-op.
func onlyArgs(args map[string]any, allowed ...string) error {
	for _, k := range slices.Sorted(maps.Keys(args)) {
		if !slices.Contains(allowed, k) {
			return invalid("unknown argument %q (allowed: %s)", k, strings.Join(allowed, ", "))
		}
	}
	for _, k := range allowed {
		if _, ok := args[k]; !ok {
			return invalid("missing argument %q", k)
		}
	}
	return nil
}

// packagePath validates a package-relative slash path and returns it cleaned.
func packagePath(rel string) (string, error) {
	if strings.Contains(rel, `\`) || path.IsAbs(rel) {
		return "", invalid("path %q must be a relative slash path", rel)
	}
	clean := path.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != rel {
		return "", invalid("path %q must be clean and stay inside the package", rel)
	}
	if !slices.ContainsFunc(actionDirs, func(dir string) bool { return strings.HasPrefix(clean, dir) }) {
		return "", invalid("path %q must be under %s", rel, strings.Join(actionDirs, ", "))
	}
	return clean, nil
}

// manifestObjects reads one package manifest, splits its YAML documents and
// renders each object with the frozen parameters.
func manifestObjects(files map[string][]byte, params map[string]any, rel string) ([]map[string]any, error) {
	clean, err := packagePath(rel)
	if err != nil {
		return nil, err
	}
	content, ok := files[clean]
	if !ok {
		return nil, invalid("package file %q does not exist", clean)
	}
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(content)))
	var objects []map[string]any
	for n := 1; ; n++ {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, invalid("%s: read document %d: %v", clean, n, err)
		}
		var obj map[string]any
		if err := yaml.UnmarshalStrict(doc, &obj); err != nil {
			return nil, invalid("%s: document %d: %v", clean, n, err)
		}
		if len(obj) == 0 { // a comment-only or empty document
			continue
		}
		rendered, err := problem.RenderValue(obj, params)
		if err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", clean, n, err)
		}
		objects = append(objects, rendered.(map[string]any))
	}
	if len(objects) == 0 {
		return nil, invalid("%s holds no object", clean)
	}
	return objects, nil
}

// objectRef reads one delete-owned target.
func objectRef(item any) (model.ObjectRef, error) {
	m, ok := item.(map[string]any)
	if !ok {
		return model.ObjectRef{}, errors.New("not an object")
	}
	var ref model.ObjectRef
	for _, k := range slices.Sorted(maps.Keys(m)) {
		s, isString := m[k].(string)
		if !isString {
			return model.ObjectRef{}, fmt.Errorf("%q must be a string", k)
		}
		switch k {
		case "api_version":
			ref.APIVersion = s
		case "kind":
			ref.Kind = s
		case "namespace":
			ref.Namespace = s
		case "name":
			ref.Name = s
		default:
			return model.ObjectRef{}, fmt.Errorf("unknown key %q", k)
		}
	}
	if ref.APIVersion == "" || ref.Kind == "" || ref.Name == "" {
		return model.ObjectRef{}, errors.New("api_version, kind and name are required")
	}
	return ref, nil
}

// podSpecLists are the pod spec fields that list containers.
var podSpecLists = []string{"containers", "initContainers", "ephemeralContainers"}

// ObjectImages returns the image references of every container, init
// container and ephemeral container found anywhere in a rendered object (a
// Pod, the pod template of a workload, a CronJob's job template), in a
// stable order (object keys sorted, lists in order).
func ObjectImages(obj map[string]any) []string {
	var images []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for _, k := range slices.Sorted(maps.Keys(x)) {
				if slices.Contains(podSpecLists, k) {
					if list, ok := x[k].([]any); ok {
						for _, item := range list {
							if c, ok := item.(map[string]any); ok {
								if image, ok := c["image"].(string); ok {
									images = append(images, image)
								}
							}
						}
					}
				}
				walk(x[k])
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(obj)
	return images
}
