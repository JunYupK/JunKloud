//go:build linux

package environment

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/JunYupK/JunKloud/internal/model"
)

// cleanupImages removes only an unshared immutable image ID, never global
// caches or a tag that moved to another image. Docker retains shared layers.
func (m *Manager) cleanupImages(ctx context.Context, inv model.Inventory) error {
	type imageRecord struct {
		ID       string   `json:"Id"`
		RepoTags []string `json:"RepoTags"`
		Config   struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	var approved []string
	artifacts := slices.Concat(inv.Images, inv.ObservedImages)
	if len(artifacts) == 0 {
		return nil
	}
	builds, err := m.store.ListOperations(ctx, OperationImageBuild)
	if err != nil {
		return err
	}
	for _, artifact := range artifacts {
		if !strings.HasPrefix(artifact.ID, "sha256:") || artifact.Reference == "" || strings.HasPrefix(artifact.Reference, "-") {
			return ErrNotOwned
		}
		out, err := m.output(ctx, dockerTimeout, "docker", "image", "inspect", "--", artifact.Reference)
		if err != nil {
			// Missing is idempotent only if the recorded ID is also gone. A moved
			// tag or an inaccessible daemon never counts as cleaned.
			if !strings.Contains(err.Error(), "No such image") {
				return err
			}
			_, idErr := m.output(ctx, dockerTimeout, "docker", "image", "inspect", "--", artifact.ID)
			if idErr != nil && strings.Contains(idErr.Error(), "No such image") {
				continue
			}
			return fmt.Errorf("%w: image %s lost its owned reference", ErrNotOwned, artifact.ID)
		}
		var records []imageRecord
		if err = json.Unmarshal(out, &records); err != nil || len(records) != 1 {
			return fmt.Errorf("environment: cannot inspect image %s", artifact.Reference)
		}
		got := records[0]
		if got.ID != artifact.ID || !ownedBy(got.Config.Labels, inv) {
			return fmt.Errorf("%w: image %s identity or labels differ", ErrNotOwned, artifact.Reference)
		}
		proven := slices.ContainsFunc(builds, func(op model.Operation) bool {
			return op.EnvironmentID == inv.EnvironmentID && op.SessionID == inv.SessionID && op.Planned.ProblemID == inv.ProblemID && op.ID == got.Config.Labels[labelImageBuild] && (op.Phase == "building" || op.Phase == "built" || op.Phase == PhaseDone) && slices.Contains(op.Planned.Images, artifact)
		})
		if !proven {
			return fmt.Errorf("%w: image %s has no journaled exact build identity", ErrNotOwned, artifact.Reference)
		}
		for _, tag := range got.RepoTags {
			if !slices.ContainsFunc(inv.Images, func(i model.OwnedImage) bool { return i.ID == got.ID && i.Reference == tag }) {
				return fmt.Errorf("%w: image %s has foreign tag %s", ErrNotOwned, artifact.ID, tag)
			}
		}
		// Multiple tags cannot safely be removed by ID without force; fail closed.
		if len(got.RepoTags) != 1 {
			return fmt.Errorf("%w: image %s has multiple tags", ErrNotOwned, artifact.ID)
		}
		if !slices.Contains(approved, artifact.ID) {
			approved = append(approved, artifact.ID)
		}
	}
	for _, id := range approved {
		if err := m.command(ctx, dockerTimeout, "docker", "image", "rm", id); err != nil {
			return err
		}
	}
	return nil
}
