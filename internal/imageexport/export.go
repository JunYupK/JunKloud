// Package imageexport exports one exact native image, with a narrowly
// verified compatibility path for Docker engines before API 1.48.
package imageexport

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

type Run func(context.Context, []string) ([]byte, error)

var unsupported = regexp.MustCompile(`"--platform" requires API version 1\.48, but the Docker daemon API version is 1\.(?:[0-3][0-9]|4[0-7])(?:\r?\n|$)`)
var imageID = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func Save(ctx context.Context, run Run, ref, id, arch, path string) error {
	if !imageID.MatchString(id) {
		return fmt.Errorf("image export requires an exact image ID")
	}
	platform := "linux/" + arch
	proof := func() error {
		raw, err := run(ctx, []string{"image", "inspect", "--format", "{{.Id}} {{.Os}}/{{.Architecture}}", ref, id})
		if err != nil {
			return err
		}
		fields := strings.Fields(string(raw))
		if len(fields) != 4 || fields[0] != id || fields[2] != id || fields[1] != platform || fields[3] != platform {
			return fmt.Errorf("image reference moved or is not native %s", platform)
		}
		return nil
	}
	if err := proof(); err != nil {
		return err
	}
	_, err := run(ctx, []string{"save", "--platform", platform, "--output", path, ref})
	if err == nil {
		return proof()
	}
	if ctx.Err() != nil || !unsupported.MatchString(err.Error()) {
		return err
	}
	if err = proof(); err != nil {
		return err
	}
	if _, err = run(ctx, []string{"save", "--output", path, ref}); err != nil {
		return err
	}
	return proof()
}
