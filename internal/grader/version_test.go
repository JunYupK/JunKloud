package grader

import (
	"regexp"
	"testing"
)

// TestGraderVersionFormat keeps the validation-certificate key well formed:
// "grader-<positive integer>", compared as an opaque string everywhere else.
func TestGraderVersionFormat(t *testing.T) {
	if !regexp.MustCompile(`^grader-[1-9][0-9]*$`).MatchString(GraderVersion) {
		t.Fatalf("GraderVersion %q is not grader-<n>", GraderVersion)
	}
}
