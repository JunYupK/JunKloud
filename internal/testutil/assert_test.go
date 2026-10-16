package testutil

import (
	"errors"
	"testing"
)

// fakeT records whether Fatalf was called instead of actually failing the
// enclosing test, so we can assert on both the pass and fail branches of
// each helper.
type fakeT struct {
	testing.T
	failed bool
}

func (f *fakeT) Helper() {}

func (f *fakeT) Fatalf(format string, args ...any) {
	f.failed = true
}

func TestEqual(t *testing.T) {
	ft := &fakeT{}
	Equal(ft, 1, 1)
	if ft.failed {
		t.Fatalf("Equal(1, 1) reported failure, want pass")
	}

	ft = &fakeT{}
	Equal(ft, 1, 2)
	if !ft.failed {
		t.Fatalf("Equal(1, 2) reported pass, want failure")
	}
}

func TestNoError(t *testing.T) {
	ft := &fakeT{}
	NoError(ft, nil)
	if ft.failed {
		t.Fatalf("NoError(nil) reported failure, want pass")
	}

	ft = &fakeT{}
	NoError(ft, errors.New("boom"))
	if !ft.failed {
		t.Fatalf("NoError(err) reported pass, want failure")
	}
}

func TestErrorIs(t *testing.T) {
	sentinel := errors.New("sentinel")
	wrapped := errors.Join(errors.New("context"), sentinel)

	ft := &fakeT{}
	ErrorIs(ft, wrapped, sentinel)
	if ft.failed {
		t.Fatalf("ErrorIs(wrapped, sentinel) reported failure, want pass")
	}

	ft = &fakeT{}
	ErrorIs(ft, errors.New("unrelated"), sentinel)
	if !ft.failed {
		t.Fatalf("ErrorIs(unrelated, sentinel) reported pass, want failure")
	}
}
