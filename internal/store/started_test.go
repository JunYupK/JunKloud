package store_test

import (
	"sync"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func preparing(id string) model.SessionRecord {
	rec := session(id, model.StatePreparing)
	rec.StartedAt = nil
	return rec
}

// TestMarkStarted: starting the clock and the PREPARING -> RUNNING move are
// one atomic step (R5), the stored time is UTC, and nothing else can start a
// session: not a second start, not a failed preparation, not an unknown ID.
func TestMarkStarted(t *testing.T) {
	s, _ := openStore(t)
	testutil.NoError(t, s.CreateSession(ctx, preparing("s1")))

	// A local-zone time with a monotonic reading is stored as plain UTC.
	local := time.Date(2026, 10, 15, 9, 30, 15, 987654321, time.FixedZone("KST", 9*3600))
	testutil.NoError(t, s.MarkStarted(ctx, "s1", local))

	got, err := s.LoadSession(ctx, "s1")
	testutil.NoError(t, err)
	testutil.Equal(t, got.State, model.StateRunning)
	if got.StartedAt == nil {
		t.Fatal("StartedAt not stored")
	}
	testutil.Equal(t, *got.StartedAt, local.UTC())
	testutil.Equal(t, got.StartedAt.Location(), time.UTC)

	// The clock starts once: a second start neither moves it nor succeeds.
	err = s.MarkStarted(ctx, "s1", local.Add(time.Hour))
	testutil.ErrorIs(t, err, store.ErrStateConflict)
	got, err = s.LoadSession(ctx, "s1")
	testutil.NoError(t, err)
	testutil.Equal(t, *got.StartedAt, local.UTC())

	// A failed preparation never starts, and keeps no clock.
	testutil.NoError(t, s.CreateSession(ctx, preparing("s2")))
	testutil.NoError(t, s.Transition(ctx, "s2", model.StatePreparing, model.StateSetupError))
	testutil.ErrorIs(t, s.MarkStarted(ctx, "s2", local), store.ErrStateConflict)
	got, err = s.LoadSession(ctx, "s2")
	testutil.NoError(t, err)
	testutil.Equal(t, got.State, model.StateSetupError)
	testutil.Equal(t, got.StartedAt, (*time.Time)(nil))

	testutil.ErrorIs(t, s.MarkStarted(ctx, "nope", local), store.ErrNotFound)
}

// TestMarkStartedRace: of several terminals starting the same session, exactly
// one wins and the stored time is the winner's.
func TestMarkStartedRace(t *testing.T) {
	s, _ := openStore(t)
	testutil.NoError(t, s.CreateSession(ctx, preparing("s1")))

	const n = 6
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.MarkStarted(ctx, "s1", t0.Add(time.Duration(i)*time.Minute))
		}()
	}
	wg.Wait()

	winners := -1
	for i, err := range errs {
		switch {
		case err == nil:
			if winners != -1 {
				t.Fatalf("two starts won: %d and %d", winners, i)
			}
			winners = i
		default:
			testutil.ErrorIs(t, err, store.ErrStateConflict)
		}
	}
	if winners == -1 {
		t.Fatal("no start won")
	}
	got, err := s.LoadSession(ctx, "s1")
	testutil.NoError(t, err)
	testutil.Equal(t, *got.StartedAt, t0.Add(time.Duration(winners)*time.Minute).UTC())
}

// TestSessionEnvironmentManifestRoundTrips: the grader version, environment
// manifest and its hash are stored with the session (spec 6.3), come back
// unchanged, and are frozen: nothing can rewrite them afterwards. A session
// created without them reads back with none.
func TestSessionEnvironmentManifestRoundTrips(t *testing.T) {
	s, path := openStore(t)
	rec := preparing("s1")
	rec.GraderVersion = "grader-1"
	rec.EnvironmentHash = "sha256:abc"
	rec.EnvironmentManifest = map[string]string{"kind": "v0.33.0", "tool:helm": "v4.3.0", "image:busybox:1.37.0": "sha256:def"}
	testutil.NoError(t, s.CreateSession(ctx, rec))

	got, err := s.LoadSession(ctx, "s1")
	testutil.NoError(t, err)
	testutil.Equal(t, got.GraderVersion, "grader-1")
	testutil.Equal(t, got.EnvironmentHash, "sha256:abc")
	testutil.Equal(t, got.EnvironmentManifest, rec.EnvironmentManifest)

	// Starting the session leaves them alone.
	testutil.NoError(t, s.MarkStarted(ctx, "s1", t0))
	got, err = s.LoadSession(ctx, "s1")
	testutil.NoError(t, err)
	testutil.Equal(t, got.EnvironmentHash, "sha256:abc")

	// Frozen at the database level too.
	db := rawDB(t, path)
	for _, col := range []string{"grader_version", "environment_hash", "environment_manifest_json"} {
		if _, err := db.Exec(`UPDATE sessions SET ` + col + ` = 'x' WHERE id = 's1'`); err == nil {
			t.Errorf("UPDATE of %s succeeded", col)
		}
	}

	// Without them: empty, not an error.
	testutil.NoError(t, s.CreateSession(ctx, preparing("s2")))
	got, err = s.LoadSession(ctx, "s2")
	testutil.NoError(t, err)
	testutil.Equal(t, got.GraderVersion, "")
	testutil.Equal(t, got.EnvironmentHash, "")
	testutil.Equal(t, got.EnvironmentManifest, map[string]string(nil))
}
