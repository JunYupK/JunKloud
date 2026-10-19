//go:build linux

package session

import (
	"context"
	"fmt"
	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/lock"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"os"
	"path/filepath"
	"strings"
)

type ImageBuildOptions struct{ ProblemID, ContextDir, Reference string }
type imageBuilder interface {
	BuildImage(context.Context, model.Inventory, string, string, []environment.PinnedImage) (model.OwnedImage, error)
}
type imageObserver interface {
	ObserveImages(context.Context, model.Inventory, []string) (model.Inventory, error)
}
type imageRecoverer interface {
	RecoverImageBuilds(context.Context, string) error
}

func (s *Service) BuildImage(ctx context.Context, sessionID string, opts ImageBuildOptions) (model.OwnedImage, error) {
	var zero model.OwnedImage
	release, err := lock.Acquire(author.LockPath(s.cfg.StateDir))
	if err != nil {
		return zero, err
	}
	defer release()
	if err = s.requireNoPendingReset(ctx); err != nil {
		return zero, err
	}
	rec, err := s.cfg.Store.LoadSession(ctx, sessionID)
	if err != nil {
		return zero, err
	}
	if rec.EnvironmentID != s.cfg.Identity.ID {
		return zero, ErrEnvironmentChanged
	}
	if rec.State != model.StateRunning || rec.StartedAt == nil {
		return zero, ErrNotRunning
	}
	f, ok := findProblem(rec, opts.ProblemID)
	if !ok {
		return zero, ErrUnknownProblem
	}
	if !environment.ValidBuildReference(opts.Reference) {
		return zero, ErrInvalidOptions
	}
	hist, err := s.loadHistory(ctx, sessionID)
	if err != nil {
		return zero, err
	}
	dir, n, err := LatestAttempt(s.cfg.Workspace, sessionID, f.Definition.ID)
	if err != nil {
		return zero, err
	}
	if n < hist.currentAttempt(f.Definition.ID) {
		return zero, fmt.Errorf("%w: current attempt is missing; archived work cannot be rebuilt", ErrInvalidOptions)
	}
	rel := opts.ContextDir
	if rel == "" {
		rel = "."
	}
	if filepath.IsAbs(rel) || filepath.Clean(rel) != rel || strings.Contains(rel, `\`) {
		return zero, ErrInvalidOptions
	}
	contextDir := dir
	if rel != "." {
		contextDir, err = problem.ResolveAsset(dir, rel)
		if err != nil {
			return zero, ErrInvalidOptions
		}
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return zero, err
	}
	absolute, err := filepath.Abs(dir)
	if err != nil || real != absolute {
		return zero, ErrInvalidOptions
	}
	info, err := os.Stat(contextDir)
	if err != nil {
		return zero, err
	}
	if !info.IsDir() {
		return zero, ErrInvalidOptions
	}
	if err = s.cfg.Env.Check(ctx, s.cfg.Identity); err != nil {
		return zero, err
	}
	builder, ok := s.cfg.Env.(imageBuilder)
	if !ok {
		return zero, fmt.Errorf("session: image builder unavailable")
	}
	inv, err := s.problemInventory(ctx, rec, f)
	if err != nil {
		return zero, err
	}
	provided := s.cfg.Images
	if len(provided) == 0 {
		provided = []environment.PinnedImage{environment.ProbeImage}
	}
	return builder.BuildImage(ctx, inv, contextDir, opts.Reference, provided)
}

func imageReferences(f model.FrozenProblem) []string {
	var refs []string
	for _, criterion := range f.Definition.Criteria {
		if criterion.Kind == model.CriterionImage {
			if ref, ok := criterion.Args["reference"].(string); ok && ref != "" {
				refs = append(refs, ref)
			}
		}
	}
	return refs
}
func (s *Service) observeProblemImages(ctx context.Context, inv model.Inventory, f model.FrozenProblem) (model.Inventory, error) {
	refs := imageReferences(f)
	if len(refs) == 0 {
		return inv, nil
	}
	observer, ok := s.cfg.Env.(imageObserver)
	if !ok {
		return inv, fmt.Errorf("session: image observation unavailable")
	}
	return observer.ObserveImages(ctx, inv, refs)
}
