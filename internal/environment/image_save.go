//go:build linux

package environment

import (
	"context"
	"fmt"
	"github.com/JunYupK/JunKloud/internal/imageexport"
)

// PreflightSaveReference proves an existing reference can be exported on the
// native platform before any earlier action in its list mutates the workspace.
func (m *Manager) PreflightSaveReference(ctx context.Context, ref string) error {
	if !ValidBuildReference(ref) {
		return ErrInvalidImage
	}
	got, found, err := m.inspectBuildImage(ctx, ref)
	if err != nil {
		return err
	}
	if !found || !imageDigestPattern.MatchString(got.ID) || got.OS != "linux" || got.Architecture != buildArch() {
		return fmt.Errorf("%w: save needs an existing native image", ErrInvalidAction)
	}
	return nil
}

func (m *Manager) SaveImage(ctx context.Context, ref, path string) error {
	if !ValidBuildReference(ref) {
		return ErrInvalidImage
	}
	got, found, err := m.inspectBuildImage(ctx, ref)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("environment: image to save is missing")
	}
	return imageexport.Save(ctx, func(c context.Context, args []string) ([]byte, error) {
		return m.output(c, loadTimeout, "docker", args...)
	}, ref, got.ID, buildArch(), path)
}
