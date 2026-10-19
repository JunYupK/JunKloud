//go:build linux

package environment

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

const maxBuildBytes = 32 << 20

func buildArch() string { return runtime.GOARCH }

var buildReference = regexp.MustCompile(`^(?:[a-z0-9]+(?:[._-][a-z0-9]+)*(?::[0-9]+)?/)*[a-z0-9]+(?:[._-][a-z0-9]+)*:[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
var observedReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,254}$`)
var syntaxDirective = regexp.MustCompile(`(?i)^#\s*syntax\s*=`)

func ValidBuildReference(ref string) bool {
	return buildReference.MatchString(ref) && !strings.Contains(ref, "..")
}

// CopyBuildContext snapshots only regular files. No symlinks, devices,
// sockets or remote contexts reach the Docker daemon; input is bounded.
func CopyBuildContext(src, dst string) error {
	total := int64(0)
	entries := 0
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		entries++
		if entries > 4096 {
			return fmt.Errorf("%w: too many context entries", ErrInvalidAction)
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 || (!d.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("%w: context contains non-regular input", ErrInvalidAction)
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0700)
		}
		total += info.Size()
		if total > maxBuildBytes {
			return fmt.Errorf("%w: context exceeds 32 MiB", ErrInvalidAction)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), data, 0600)
	})
}

func ValidateBuildContext(dir string, provided []PinnedImage) error {
	snapshot, err := os.MkdirTemp("", "junkloud-context-check-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(snapshot)
	if err = CopyBuildContext(dir, snapshot); err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(snapshot, "Dockerfile"))
	if err != nil {
		return fmt.Errorf("%w: build needs Dockerfile", ErrInvalidAction)
	}
	if len(b) > 64<<10 {
		return fmt.Errorf("%w: Dockerfile too large", ErrInvalidAction)
	}
	base := map[string]bool{"scratch": true}
	for _, img := range provided {
		base[img.Name] = true
		base[img.Ref()] = true
	}
	from := 0
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if syntaxDirective.MatchString(line) {
				return fmt.Errorf("%w: remote Dockerfile frontend forbidden", ErrInvalidAction)
			}
			continue
		}
		fields := strings.Fields(line)
		op := strings.ToUpper(fields[0])
		switch op {
		case "FROM":
			if len(fields) != 2 || !base[fields[1]] {
				return fmt.Errorf("%w: FROM must name one provided native base or scratch", ErrInvalidAction)
			}
			from++
		case "COPY":
			if strings.Contains(line, "--") || strings.Contains(line, "://") {
				return fmt.Errorf("%w: only local COPY is supported", ErrInvalidAction)
			}
		case "RUN":
			if len(fields) > 1 && strings.HasPrefix(fields[1], "--") {
				return fmt.Errorf("%w: RUN options cannot override build isolation", ErrInvalidAction)
			}
		case "CMD", "ENTRYPOINT", "ENV", "LABEL", "EXPOSE", "WORKDIR", "USER", "STOPSIGNAL", "SHELL", "VOLUME":
		default:
			return fmt.Errorf("%w: unsupported Dockerfile instruction", ErrInvalidAction)
		}
	}
	if from != 1 {
		return fmt.Errorf("%w: exactly one native build stage is supported", ErrInvalidAction)
	}
	return nil
}
