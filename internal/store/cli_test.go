package store_test

import (
	"context"
	"errors"
	"github.com/JunYupK/JunKloud/internal/store"
	"testing"
)

func TestCurrentSessionAndMarksPersistAcrossConnections(t *testing.T) {
	s, path := openStore(t)
	ctx := context.Background()
	if _, err := s.CurrentSessionID(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	rec := mustCreate(t, s, "older", "RUNNING")
	mustCreate(t, s, "newer", "RUNNING")
	if id, err := s.CurrentSessionID(ctx); err != nil || id != "newer" {
		t.Fatal(id, err)
	}
	other, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	pid := rec.Problems[0].Definition.ID
	if err := s.SetMark(ctx, "older", pid, true); err != nil {
		t.Fatal(err)
	}
	loaded, err := other.LoadSession(ctx, "older")
	if err != nil || !loaded.Marks[pid] {
		t.Fatal(loaded.Marks, err)
	}
	if err := other.SetMark(ctx, "older", pid, false); err != nil {
		t.Fatal(err)
	}
	loaded, err = s.LoadSession(ctx, "older")
	if err != nil || loaded.Marks[pid] {
		t.Fatal(loaded.Marks, err)
	}
	if err := s.SetMark(ctx, "older", "absent", true); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
}
