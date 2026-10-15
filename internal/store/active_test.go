package store_test

import (
	"testing"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// TestActiveSessions pins which session states still occupy an environment
// (R5): the in-flight and the failed-but-unresolved ones. COMPLETED and
// ABANDONED sessions do not, and neither do other environments' sessions.
func TestActiveSessions(t *testing.T) {
	s, _ := openStore(t)
	states := map[string]string{
		"s-preparing":    model.StatePreparing,
		"s-running":      model.StateRunning,
		"s-grading":      model.StateGrading,
		"s-grading-err":  model.StateGradingError,
		"s-setup-err":    model.StateSetupError,
		"s-completed":    model.StateCompleted,
		"s-abandoned":    model.StateAbandoned,
		"s-other-env":    model.StateRunning,
		"s-other-env-ok": model.StateCompleted,
	}
	for id, state := range states {
		rec := session(id, state)
		if id == "s-other-env" || id == "s-other-env-ok" {
			rec.EnvironmentID = "env-2"
		}
		testutil.NoError(t, s.CreateSession(ctx, rec))
	}

	got, err := s.ActiveSessions(ctx, "env-1")
	testutil.NoError(t, err)
	want := []store.SessionStatus{
		{ID: "s-grading", Mode: model.ModePractice, State: model.StateGrading},
		{ID: "s-grading-err", Mode: model.ModePractice, State: model.StateGradingError},
		{ID: "s-preparing", Mode: model.ModePractice, State: model.StatePreparing},
		{ID: "s-running", Mode: model.ModePractice, State: model.StateRunning},
		{ID: "s-setup-err", Mode: model.ModePractice, State: model.StateSetupError},
	}
	testutil.Equal(t, got, want)

	// A session leaving its active state stops blocking.
	testutil.NoError(t, s.Transition(ctx, "s-running", model.StateRunning, model.StateAbandoned))
	got, err = s.ActiveSessions(ctx, "env-1")
	testutil.NoError(t, err)
	testutil.Equal(t, len(got), 4)

	// An environment without sessions has none, and an empty ID is refused
	// instead of matching everything.
	got, err = s.ActiveSessions(ctx, "env-3")
	testutil.NoError(t, err)
	testutil.Equal(t, len(got), 0)
	if _, err := s.ActiveSessions(ctx, ""); err == nil {
		t.Fatal("empty environment ID accepted")
	}
}
