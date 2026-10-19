//go:build linux

package author

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/model"
)

// ActionExecutor executes private author actions against the same staged
// workspace the grader receives. Frozen public assets are never modified.
type ActionExecutor interface {
	Execute(context.Context, model.Identity, environment.Owner, model.Problem, model.FrozenProblem, string, []model.Action) (model.Inventory, error)
}

type ImageActions interface {
	BuildImage(context.Context, model.Inventory, string, string, []environment.PinnedImage) (model.OwnedImage, error)
	SaveImage(context.Context, string, string) error
	PreflightSaveReference(context.Context, string) error
}
type executor struct {
	env      Applier
	images   ImageActions
	provided []environment.PinnedImage
}

func NewActionExecutor(env Applier, images ImageActions, provided ...environment.PinnedImage) ActionExecutor {
	return &executor{env: env, images: images, provided: provided}
}

func (e *executor) Execute(ctx context.Context, id model.Identity, owner environment.Owner, p model.Problem, f model.FrozenProblem, ws string, actions []model.Action) (model.Inventory, error) {
	steps, err := preflightActions(p, f, ws, actions, e.provided)
	if err != nil {
		return owner.Inventory, err
	}
	inv := owner.Inventory
	var clusterActions []environment.Action
	clusterOnly := true
	for _, step := range steps {
		if step.kind == model.ActionFile || step.kind == model.ActionImage {
			clusterOnly = false
		} else {
			clusterActions = append(clusterActions, step.cluster)
		}
		if step.kind == model.ActionImage && e.images == nil {
			return inv, fmt.Errorf("%w: image execution unavailable", environment.ErrUnsupportedAction)
		}
		if step.kind == model.ActionImage && step.operation == "build" {
			if checker, ok := e.images.(interface {
				PreflightImageReference(context.Context, model.Inventory, string) error
			}); ok {
				if err = checker.PreflightImageReference(ctx, inv, step.reference); err != nil {
					return inv, err
				}
			}
		}
		if step.checkSaveReference {
			if err = e.images.PreflightSaveReference(ctx, step.reference); err != nil {
				return inv, err
			}
		}
	}
	if clusterOnly {
		return e.env.Apply(ctx, id, owner, clusterActions)
	}
	if preflight, ok := e.env.(interface {
		PreflightActions(context.Context, model.Identity, environment.Owner, []environment.Action) error
	}); ok {
		if err = preflight.PreflightActions(ctx, id, owner, clusterActions); err != nil {
			return inv, err
		}
	}
	for _, s := range steps {
		if err := ctx.Err(); err != nil {
			return inv, err
		}
		switch s.kind {
		case model.ActionFile:
			if err = os.MkdirAll(filepath.Dir(s.path), 0700); err == nil {
				err = os.WriteFile(s.path, s.data, 0600)
			}
		case model.ActionImage:
			if e.images == nil {
				return inv, fmt.Errorf("%w: image execution unavailable", environment.ErrUnsupportedAction)
			}
			if s.operation == "build" {
				var image model.OwnedImage
				image, err = e.images.BuildImage(ctx, inv, s.path, s.reference, e.provided)
				if err == nil {
					inv.Images = appendImage(inv.Images, image)
				}
			} else {
				if err = os.MkdirAll(filepath.Dir(s.path), 0700); err == nil {
					err = e.images.SaveImage(ctx, s.reference, s.path)
				}
			}
		default:
			owner.Inventory = inv
			inv, err = e.env.Apply(ctx, id, owner, []environment.Action{s.cluster})
			owner.CreateNamespaces = false
		}
		if err != nil {
			return inv, err
		}
	}
	// Empty setup must still create its declared namespace bundle.
	if len(steps) == 0 && owner.CreateNamespaces {
		return e.env.Apply(ctx, id, owner, nil)
	}
	return inv, nil
}
func appendImage(images []model.OwnedImage, image model.OwnedImage) []model.OwnedImage {
	for i := range images {
		if images[i].Reference == image.Reference {
			images[i] = image
			return images
		}
	}
	return append(images, image)
}
