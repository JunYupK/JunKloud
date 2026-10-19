//go:build linux

package environment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/store"
)

const OperationImageBuild = "image-build"
const labelImageBuild = "junkloud.io/image-build"

var ErrImageBuildPending = errors.New("environment: image build unfinished; run ckad reset --recover")

type buildImageConfig struct {
	Labels map[string]string `json:"Labels"`
}
type buildImageRecord struct {
	ID           string           `json:"Id"`
	OS           string           `json:"Os"`
	Architecture string           `json:"Architecture"`
	RepoTags     []string         `json:"RepoTags"`
	Config       buildImageConfig `json:"Config"`
}

func (m *Manager) inspectBuildImage(ctx context.Context, ref string) (buildImageRecord, bool, error) {
	raw, err := m.output(ctx, dockerTimeout, "docker", "image", "inspect", "--", ref)
	if err != nil {
		if strings.Contains(err.Error(), "No such image") {
			return buildImageRecord{}, false, nil
		}
		return buildImageRecord{}, false, err
	}
	var records []buildImageRecord
	if err = json.Unmarshal(raw, &records); err != nil || len(records) != 1 {
		return buildImageRecord{}, false, fmt.Errorf("environment: invalid image inspection")
	}
	return records[0], true, nil
}

func (m *Manager) imageBuildState(ctx context.Context, inv model.Inventory, ref string) (model.Inventory, buildImageRecord, bool, error) {
	ops, err := m.store.ListOperations(ctx, OperationImageBuild)
	if err != nil {
		return inv, buildImageRecord{}, false, err
	}
	for _, op := range ops {
		if op.EnvironmentID == inv.EnvironmentID && op.Phase != PhaseDone {
			return inv, buildImageRecord{}, false, ErrImageBuildPending
		}
	}
	current, found, err := m.inspectBuildImage(ctx, ref)
	if err != nil {
		return inv, buildImageRecord{}, false, err
	}
	stored, err := m.store.GetInventory(ctx, inv.EnvironmentID, inv.SessionID, inv.ProblemID)
	if err == nil {
		inv = stored
	} else if !errors.Is(err, store.ErrNotFound) {
		return inv, buildImageRecord{}, false, err
	}
	provenBuild := false
	for _, op := range ops {
		if op.Phase == PhaseDone && op.EnvironmentID == inv.EnvironmentID && op.SessionID == inv.SessionID && op.Planned.ProblemID == inv.ProblemID && current.Config.Labels[labelImageBuild] == op.ID && slices.Contains(op.Planned.Images, model.OwnedImage{ID: current.ID, Reference: ref}) {
			provenBuild = true
		}
	}
	if found && (!provenBuild || !ownedBy(current.Config.Labels, inv) || !slices.Contains(inv.Images, model.OwnedImage{ID: current.ID, Reference: ref}) || len(current.RepoTags) != 1 || current.RepoTags[0] != ref) {
		return inv, buildImageRecord{}, false, fmt.Errorf("%w: image tag already exists without exclusive recorded ownership", ErrNotOwned)
	}

	return inv, current, found, nil
}

func (m *Manager) PreflightImageReference(ctx context.Context, inv model.Inventory, ref string) error {
	_, _, _, err := m.imageBuildState(ctx, inv, ref)
	return err
}

// BuildImage is shared by private author actions and managed learner builds.
// The caller holds env.lock and has verified the current environment/session.
// Rebuild removes the proven previous ID before replacement, keeping intent
// durable across both cleanup and build. No tag is silently adopted.
func (m *Manager) BuildImage(ctx context.Context, inv model.Inventory, dir, ref string, provided []PinnedImage) (model.OwnedImage, error) {
	var zero model.OwnedImage
	if inv.EnvironmentID == "" || inv.SessionID == "" || inv.ProblemID == "" {
		return zero, ErrInvalidOwner
	}
	if !ValidBuildReference(ref) {
		return zero, ErrInvalidImage
	}
	if err := ValidateBuildContext(dir, provided); err != nil {
		return zero, err
	}
	inv, current, found, err := m.imageBuildState(ctx, inv, ref)
	if err != nil {
		return zero, err
	}
	snapshot, err := os.MkdirTemp("", "junkloud-build-*")
	if err != nil {
		return zero, err
	}
	defer os.RemoveAll(snapshot)
	if err = CopyBuildContext(dir, snapshot); err != nil {
		return zero, err
	}
	if err = ValidateBuildContext(snapshot, provided); err != nil {
		return zero, err
	}
	// Prove every provided base is present on the native platform before effects.
	dockerfile, _ := os.ReadFile(filepath.Join(snapshot, "Dockerfile"))
	for _, line := range strings.Split(string(dockerfile), "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 && strings.EqualFold(parts[0], "FROM") && parts[1] != "scratch" {
			base, exists, e := m.inspectBuildImage(ctx, parts[1])
			if e != nil {
				return zero, e
			}
			if !exists || base.OS != "linux" || base.Architecture != buildArch() {
				return zero, ErrInvalidImage
			}
			for _, providedBase := range provided {
				if providedBase.Name == parts[1] {
					pinned, present, e := m.inspectBuildImage(ctx, providedBase.Ref())
					if e != nil {
						return zero, e
					}
					if !present || pinned.ID != base.ID || pinned.OS != "linux" || pinned.Architecture != buildArch() {
						return zero, ErrInvalidImage
					}
				}
			}
		}
	}
	suffix, err := newID()
	if err != nil {
		return zero, err
	}
	planned := model.Inventory{EnvironmentID: inv.EnvironmentID, SessionID: inv.SessionID, ProblemID: inv.ProblemID, Images: []model.OwnedImage{{Reference: ref}}}
	for _, prior := range inv.Images {
		if prior.Reference == ref {
			if planned.Images[0].ID != "" && planned.Images[0].ID != prior.ID {
				return zero, ErrNotOwned
			}
			planned.Images[0].ID = prior.ID
		}
	}
	if found && planned.Images[0].ID != current.ID {
		return zero, ErrNotOwned
	}
	op := model.Operation{ID: "image-build-" + suffix, EnvironmentID: inv.EnvironmentID, SessionID: inv.SessionID, Kind: OperationImageBuild, Phase: PhasePlanned, Planned: planned, CreatedAt: m.now()}
	if err = m.store.PutOperation(ctx, op); err != nil {
		return zero, err
	}
	if planned.Images[0].ID != "" {
		if err = m.cleanupImages(ctx, planned); err != nil {
			return zero, err
		}
	}
	inv.Images = slices.DeleteFunc(inv.Images, func(i model.OwnedImage) bool { return i.Reference == ref })
	if err = m.store.PutInventory(ctx, inv); err != nil {
		return zero, err
	}
	op.Phase = "building"
	op.Planned.Images[0].ID = ""
	if err = m.store.PutOperation(ctx, op); err != nil {
		return zero, err
	}
	args := []string{"build", "--pull=false", "--network=none", "--platform", "linux/" + buildArch(), "--tag", ref}
	for _, label := range []struct{ k, v string }{{LabelEnvironmentID, inv.EnvironmentID}, {LabelSessionID, inv.SessionID}, {LabelProblemID, inv.ProblemID}, {labelImageBuild, op.ID}} {
		args = append(args, "--label", label.k+"="+label.v)
	}
	args = append(args, snapshot)
	if err = m.command(ctx, 5*time.Minute, "docker", args...); err != nil {
		// Docker output may quote private Dockerfile RUN lines. Keep diagnostics
		// opaque while preserving cancellation and the retained recovery intent.
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		return zero, errors.Join(errors.New("environment: image build failed; intent retained for reset --recover"), safeBuildCause(err))
	}
	got, exists, err := m.inspectBuildImage(ctx, ref)
	if err != nil {
		return zero, err
	}
	if !exists || !ownedBy(got.Config.Labels, inv) || got.Config.Labels[labelImageBuild] != op.ID || !imageDigestPattern.MatchString(got.ID) || got.OS != "linux" || got.Architecture != buildArch() {
		return zero, ErrNotOwned
	}
	image := model.OwnedImage{Reference: ref, ID: got.ID}
	op.Planned.Images = []model.OwnedImage{image}
	op.Phase = "built"
	if err = m.store.PutOperation(ctx, op); err != nil {
		return zero, err
	}
	inv.Images = append(inv.Images, image)
	if err = m.store.PutInventory(ctx, inv); err != nil {
		return zero, err
	}
	op.Phase = PhaseDone
	err = m.store.PutOperation(ctx, op)
	return image, err
}
func safeBuildCause(err error) error {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, cause) {
			return cause
		}
	}
	return nil
}

// RecoverImageBuilds reconciles only recorded intents. A building intent may
// discover its ID only through its unique operation label and three owners.
func (m *Manager) RecoverImageBuilds(ctx context.Context, envID string) error {
	ops, err := m.store.ListOperations(ctx, OperationImageBuild)
	if err != nil {
		return err
	}
	for _, op := range ops {
		if op.EnvironmentID != envID || op.Phase == PhaseDone {
			continue
		}
		if len(op.Planned.Images) != 1 {
			return ErrNotOwned
		}
		artifact := op.Planned.Images[0]
		if artifact.ID == "" {
			got, found, e := m.inspectBuildImage(ctx, artifact.Reference)
			if e != nil {
				return e
			}
			if found {
				if !ownedBy(got.Config.Labels, op.Planned) || got.Config.Labels[labelImageBuild] != op.ID {
					return ErrNotOwned
				}
				op.Planned.Images[0].ID = got.ID
				if err = m.store.PutOperation(ctx, op); err != nil {
					return err
				}
			}
			if !found {
				raw, e := m.output(ctx, dockerTimeout, "docker", "image", "ls", "--all", "--quiet", "--no-trunc", "--filter", "label="+labelImageBuild+"="+op.ID)
				if e != nil {
					return e
				}
				if len(strings.Fields(string(raw))) > 0 {
					return fmt.Errorf("%w: interrupted build lost its reference; image IDs retained: %s", ErrNotOwned, strings.TrimSpace(string(raw)))
				}
				op.Planned.Images = nil
			}
		}
		if err = m.cleanupImages(ctx, op.Planned); err != nil {
			return err
		}
		inv, e := m.store.GetInventory(ctx, envID, op.SessionID, op.Planned.ProblemID)
		if e != nil && !errors.Is(e, store.ErrNotFound) {
			return e
		}
		if e == nil {
			inv.Images = slices.DeleteFunc(inv.Images, func(i model.OwnedImage) bool { return i.Reference == artifact.Reference })
			if err = m.store.PutInventory(ctx, inv); err != nil {
				return err
			}
		}
		op.Phase = PhaseDone
		if err = m.store.PutOperation(ctx, op); err != nil {
			return err
		}
	}
	return nil
}

// ObserveImages records declared artifacts without adopting their labels.
// Functional grading is independent; cleanup subsequently fails closed for
// direct Docker images lacking ownership, moved tags or shared references.
func (m *Manager) ObserveImages(ctx context.Context, inv model.Inventory, refs []string) (model.Inventory, error) {
	for _, ref := range refs {
		if !observedReference.MatchString(ref) || strings.Contains(ref, "..") || strings.Contains(ref, "//") {
			return inv, ErrInvalidImage
		}
		got, found, err := m.inspectBuildImage(ctx, ref)
		if err != nil {
			return inv, err
		}
		if !found {
			continue
		}
		image := model.OwnedImage{Reference: ref, ID: got.ID}
		if !slices.Contains(inv.Images, image) && !slices.Contains(inv.ObservedImages, image) {
			inv.ObservedImages = append(inv.ObservedImages, image)
		}
	}
	return inv, m.store.PutInventory(ctx, inv)
}
