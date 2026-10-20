package grader

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
	"k8s.io/apimachinery/pkg/util/validation"
)

// This is a read-only addition to logs, not an author exec action. It proves
// final public files through each selected running container's shared mount.
type sharedFilesArgs struct {
	Volume     string           `json:"volume"`
	Containers []string         `json:"containers"`
	Files      []sharedFileRule `json:"files"`
}
type sharedFileRule struct {
	Path   string `json:"path"`
	Equals string `json:"equals"`
}

func validateSharedFiles(crit model.Criterion, a *sharedFilesArgs) error {
	if crit.Target.Kind != "Pod" || crit.Target.APIVersion != "v1" {
		return invalidf("shared_files supports a v1 Pod target")
	}
	if len(validation.IsDNS1123Label(a.Volume)) != 0 || len(a.Containers) < 2 || len(a.Containers) > 4 || len(a.Files) < 1 || len(a.Files) > 4 {
		return invalidf("shared_files needs a named volume, 2..4 containers and 1..4 files")
	}
	seen := map[string]bool{}
	for _, name := range a.Containers {
		if len(validation.IsDNS1123Label(name)) != 0 || seen[name] {
			return invalidf("shared_files container names must be valid and distinct")
		}
		seen[name] = true
	}
	seen = map[string]bool{}
	for _, f := range a.Files {
		if !safeVolumePath(f.Path) || f.Path == "/" || len(f.Path) > 256 || len(f.Equals) > 4096 || seen[f.Path] {
			return invalidf("shared_files paths must be normalized absolute distinct files and content <=4096 bytes")
		}
		seen[f.Path] = true
	}
	return nil
}

// Every variable is argv; neither paths nor expected contents become shell
// source. Resolve both mount and file before a bounded read. Missing data and
// an escaped/unreadable path get distinct learner exit codes; stderr is ignored.
const sharedReadScript = `# junkloud-shared-read
set -eu
[ -d "$1" ] || exit 2
root=$(readlink -f -- "$1") || exit 2
[ "$root" = "$1" ] || exit 2
[ -e "$2" ] || { [ ! -L "$2" ] && exit 3; exit 2; }
resolved=$(readlink -f -- "$2") || exit 2
case "$resolved" in "$root"/*) ;; *) exit 2;; esac
[ -f "$resolved" ] && [ -r "$resolved" ] || exit 2
exec 3< "$resolved" || exit 2
opened=$(readlink -f -- /proc/self/fd/3) || exit 2
case "$opened" in "$root"/*) ;; *) exit 2;; esac
[ -f /proc/self/fd/3 ] || exit 2
head -c 4097 <&3 || exit 5`

// Select the effective longest mount, so nested mounts cannot hide a different
// volume behind the shared path. Return an identity within the shared volume,
// allowing different mount paths when they refer to the same actual file.
func sharedFileMount(p object, container, file, volume string) (string, string, bool) {
	var selected object
	longest := -1
	for _, raw := range listAt(p, "spec", "containers") {
		c := asMap(raw)
		if stringAt(c, "name") != container {
			continue
		}
		for _, v := range listAt(c, "volumeMounts") {
			m := asMap(v)
			mount := stringAt(m, "mountPath")
			if !safeVolumePath(mount) {
				return "", "", false
			}
			if withinVolume(file, mount) && len(mount) > longest {
				selected = m
				longest = len(mount)
			}
		}
	}
	if selected == nil || stringAt(selected, "name") != volume || stringAt(selected, "subPathExpr") != "" {
		return "", "", false
	}
	root := stringAt(selected, "mountPath")
	sub := stringAt(selected, "subPath")
	if sub != "" && (path.IsAbs(sub) || path.Clean(sub) != sub || sub == ".." || strings.HasPrefix(sub, "../") || strings.ContainsAny(sub, "\x00\r\n\\")) {
		return "", "", false
	}
	return root, path.Join(sub, strings.TrimPrefix(strings.TrimPrefix(file, root), "/")), true
}

func (c *ArtifactChecker) checkSharedFiles(ctx context.Context, ns string, p object, a *sharedFilesArgs) ([]string, error) {
	emptyDir := false
	for _, raw := range listAt(p, "spec", "volumes") {
		v := asMap(raw)
		if stringAt(v, "name") == a.Volume {
			_, emptyDir = v["emptyDir"].(map[string]any)
		}
	}
	if !emptyDir {
		return []string{"required shared volume is not an emptyDir"}, nil
	}
	identities := map[string]string{}
	for _, container := range a.Containers {
		running := false
		for _, raw := range listAt(p, "status", "containerStatuses") {
			s := asMap(raw)
			if stringAt(s, "name") == container {
				_, running = mapAt(s, "state")["running"].(map[string]any)
			}
		}
		if !running {
			return []string{"shared-file container is not running"}, nil
		}
		for _, f := range a.Files {
			root, identity, ok := sharedFileMount(p, container, f.Path, a.Volume)
			if !ok {
				return []string{"file is not on the effective shared emptyDir mount"}, nil
			}
			if old, ok := identities[f.Path]; ok && old != identity {
				return []string{"containers resolve the file to different shared-volume paths"}, nil
			}
			identities[f.Path] = identity
			ref := model.ObjectRef{APIVersion: "v1", Kind: "Pod", Namespace: ns, Name: nameOf(p)}
			current, err := c.cfg.Reader.Get(ctx, ref)
			if err != nil {
				return nil, fmt.Errorf("%w: shared-file Pod cannot be observed", ErrInsufficientEvidence)
			}
			if keyOf(current) != keyOf(p) {
				return nil, ErrTargetChanged
			}
			ectx, cancel := context.WithTimeout(ctx, 3*time.Second)
			result, err := c.cfg.Pods.Exec(ectx, ns, nameOf(p), container, []string{"sh", "-c", sharedReadScript, "shared-read", root, f.Path})
			cancel()
			if err != nil {
				return nil, fmt.Errorf("%w: shared-file exec unavailable", ErrInsufficientEvidence)
			}
			switch result.ExitCode {
			case 0:
				if len(result.Stdout) > 4096 || result.Stdout != f.Equals && strings.TrimSuffix(result.Stdout, "\n") != f.Equals {
					return []string{"shared file has unexpected content"}, nil
				}
			case 2:
				return []string{"shared file path is escaped or unreadable"}, nil
			case 3:
				return []string{"required shared file is missing"}, nil
			default:
				return nil, fmt.Errorf("%w: shared-file read could not complete", ErrInsufficientEvidence)
			}
		}
	}
	current, err := c.cfg.Reader.Get(ctx, model.ObjectRef{APIVersion: "v1", Kind: "Pod", Namespace: ns, Name: nameOf(p)})
	if err != nil {
		return nil, fmt.Errorf("%w: shared-file Pod cannot be re-observed", ErrInsufficientEvidence)
	}
	if keyOf(current) != keyOf(p) {
		return nil, ErrTargetChanged
	}
	return nil, nil
}
