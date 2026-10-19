//go:build linux

package environment

import (
	"context"
	"fmt"
	"github.com/JunYupK/JunKloud/internal/imageexport"
)

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
