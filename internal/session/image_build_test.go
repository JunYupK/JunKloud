//go:build linux

package session_test

import (
	"context"
	"errors"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/testutil"
	"os"
	"path/filepath"
	"testing"
)

func (e *fakeEnv) BuildImage(_ context.Context, inv model.Inventory, dir, ref string, _ []environment.PinnedImage) (model.OwnedImage, error) {
	e.log(event{Kind: "image-build", Problem: inv.ProblemID, Session: dir})
	return model.OwnedImage{Reference: ref, ID: "sha256:test"}, nil
}
func TestManagedImageBuildHonorsCurrentAttemptAndScope(t *testing.T) {
	h := newHarness(t)
	v, err := h.svc.Prepare(ctx, session.StartOptions{Mode: model.ModePractice, Count: 5, Seed: 7})
	testutil.NoError(t, err)
	pid := v.Problems[0].ID
	opts := session.ImageBuildOptions{ProblemID: pid, ContextDir: ".", Reference: "local/practice:v1"}
	if _, err = h.svc.BuildImage(ctx, v.ID, opts); !errors.Is(err, session.ErrNotRunning) {
		t.Fatalf("before start %v", err)
	}
	testutil.NoError(t, h.svc.Start(ctx, v.ID, t0))
	old, _, err := session.LatestAttempt(h.workspace, v.ID, pid)
	testutil.NoError(t, err)
	_, err = h.svc.BuildImage(ctx, v.ID, opts)
	testutil.NoError(t, err)
	for _, bad := range []string{"../..", t.TempDir()} {
		opts.ContextDir = bad
		if _, err = h.svc.BuildImage(ctx, v.ID, opts); err == nil {
			t.Fatal("escaped context accepted")
		}
	}
	testutil.NoError(t, h.svc.Reset(ctx, v.ID, session.ResetOptions{ProblemID: pid}))
	latest, n, err := session.LatestAttempt(h.workspace, v.ID, pid)
	testutil.NoError(t, err)
	testutil.Equal(t, n, 2)
	testutil.NoError(t, os.RemoveAll(latest))
	opts.ContextDir = "."
	if _, err = h.svc.BuildImage(ctx, v.ID, opts); err == nil {
		t.Fatal("archived attempt accepted after current attempt vanished")
	}
	if _, err = os.Stat(filepath.Join(old, session.QuestionFile)); err != nil {
		t.Fatal("archived personal files changed")
	}
	builds := 0
	for _, event := range h.env.events {
		if event.Kind == "image-build" {
			builds++
			testutil.Equal(t, event.Session, old)
		}
	}
	testutil.Equal(t, builds, 1)
}
