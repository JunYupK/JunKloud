//go:build linux

package session_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/JunYupK/JunKloud/internal/testutil"
)

func TestCatalogPracticeOnlyBank(t *testing.T) {
	h := newHarness(t)
	testutil.NoError(t, os.RemoveAll(filepath.Join(h.bank, "exams")))
	catalog, err := h.svc.Catalog(context.Background())
	testutil.NoError(t, err)
	if len(catalog.Problems) == 0 || len(catalog.Exams) != 0 {
		t.Fatalf("practice-only bank: %d problems, %d exams", len(catalog.Problems), len(catalog.Exams))
	}
}

func TestCatalogPreservesBankErrors(t *testing.T) {
	for _, kind := range []string{"exams is a file", "malformed exam", "missing problems"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t)
			switch kind {
			case "exams is a file":
				path := filepath.Join(h.bank, "exams")
				testutil.NoError(t, os.RemoveAll(path))
				testutil.NoError(t, os.WriteFile(path, []byte("not a directory"), 0600))
			case "malformed exam":
				testutil.NoError(t, os.WriteFile(filepath.Join(h.bank, "exams", "bad.yaml"), []byte("[invalid"), 0600))
			case "missing problems":
				testutil.NoError(t, os.RemoveAll(filepath.Join(h.bank, "problems")))
			}
			if _, err := h.svc.Catalog(context.Background()); err == nil {
				t.Fatal("invalid bank was silently accepted")
			}
		})
	}
}
