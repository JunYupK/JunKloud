// Package testutil provides the three small assertion helpers every test in
// this module uses: Equal, NoError, and ErrorIs. They exist so test files
// don't each redeclare the same reflect/errors boilerplate.
package testutil

import (
	"errors"
	"reflect"
	"testing"
)

// Equal fails the test with a diagnostic message if got and want are not
// deeply equal.
func Equal(t testing.TB, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// NoError fails the test if err is non-nil.
func NoError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ErrorIs fails the test unless errors.Is(err, want) holds.
func ErrorIs(t testing.TB, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got error %v, want error matching %v", err, want)
	}
}
