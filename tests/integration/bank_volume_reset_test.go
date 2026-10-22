//go:build integration

package integration

import (
	"context"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

type volumeResetTrace struct {
	environment.Janitor
	b bankRuntime
	t *testing.T
}

type volumeActionTrace struct {
	author.ActionExecutor
	t *testing.T
}

func (e volumeActionTrace) Execute(ctx context.Context, id model.Identity, owner environment.Owner, p model.Problem, f model.FrozenProblem, ws string, actions []model.Action) (model.Inventory, error) {
	inv, err := e.ActionExecutor.Execute(ctx, id, owner, p, f, ws, actions)
	e.t.Logf("VOLUME actions=%+v error=%v", actions, err)
	return inv, err
}

func (j volumeResetTrace) Cleanup(ctx context.Context, owner environment.Owner) error {
	client := kubeClient(j.t, j.b.m, j.b.id)
	for _, r := range owner.Inventory.Resources {
		if r.Ref.Kind == "PersistentVolumeClaim" {
			j.t.Logf("RESET inventory claim=%s/%s uid=%s", r.Ref.Namespace, r.Ref.Name, r.UID)
		}
	}
	claims, err := client.CoreV1().PersistentVolumeClaims(owner.Namespace).List(ctx, metav1.ListOptions{})
	testutil.NoError(j.t, err)
	for _, c := range claims.Items {
		j.t.Logf("RESET live claim=%s/%s uid=%s phase=%s pv=%s", c.Namespace, c.Name, c.UID, c.Status.Phase, c.Spec.VolumeName)
	}
	pvs, err := client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	testutil.NoError(j.t, err)
	for _, pv := range pvs.Items {
		j.t.Logf("RESET live pv=%s uid=%s claim=%+v", pv.Name, pv.UID, pv.Spec.ClaimRef)
	}
	return j.Janitor.Cleanup(ctx, owner)
}

// Focused acceptance uses the same real Validator and every volume seed/case,
// without claiming this one-package diagnostic is a five-package practice set.
func TestBuildVolumeValidation(t *testing.T) {
	b := openBankRuntime(t)
	p := loadGroup(t, []string{"volume-001"})[0]
	seeds, err := author.CoveringSeeds(p)
	testutil.NoError(t, err)
	stages := []string{author.KeyInitial, author.KeySolved}
	for _, c := range p.Cases {
		stages = append(stages, author.CaseKey(c.ID))
	}
	stages = append(stages, author.KeyReset)
	progress := &bankProgress{Evaluator: b.rt.Grader.Engine, t: t, stages: stages, next: map[uint64]int{}}
	var names []string
	for _, img := range b.cfg.ProvidedImages() {
		names = append(names, img.Name, img.Ref())
	}
	v, err := author.NewValidator(author.Config{Identity: b.id, Env: b.m,
		Janitor: volumeResetTrace{Janitor: environment.ManagedJanitor{Manager: b.m, Identity: b.id}, b: b, t: t},
		Engine:  progress, Store: b.st, Actions: volumeActionTrace{ActionExecutor: author.NewActionExecutor(b.m, b.m, b.cfg.ProvidedImages()...), t: t},
		LockPath: author.LockPath(b.dir), GraderVersion: grader.GraderVersion, EnvironmentHash: b.rt.EnvironmentHash,
		ProvidedImages: names, Prepare: func(ctx context.Context) error { return author.PrepareEnvironment(ctx, b.m, b.id, b.cfg.Images) },
		ScratchDir: filepath.Join(b.dir, "validate")})
	testutil.NoError(t, err)
	record, err := v.Validate(context.Background(), p, seeds)
	testutil.NoError(t, err)
	testutil.Equal(t, record.Passed, true)
	testutil.Equal(t, len(record.Cases), len(seeds)*len(stages))
	for key, ev := range record.Cases {
		if !ev.Complete || ev.Score == nil {
			t.Fatalf("incomplete volume stage %s: %+v", key, ev)
		}
		for _, r := range ev.Results {
			if r.Status == model.StatusError {
				t.Fatalf("volume stage %s/%s ERROR: %s", key, r.CriterionID, r.Reason)
			}
		}
	}
}
