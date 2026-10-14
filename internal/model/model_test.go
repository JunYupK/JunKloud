package model

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

var snakeCase = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// TestJSONNamesAreSnakeCase enforces the shared contract that every exported
// field of every shared value type serialises under a snake_case name.
func TestJSONNamesAreSnakeCase(t *testing.T) {
	types := []any{
		ObjectRef{}, ResourceScope{}, Criterion{}, Action{}, ResourceBudget{}, Problem{}, Case{}, FrozenProblem{},
		SessionRecord{}, Submission{}, Evidence{}, CheckResult{}, Evaluation{}, GradingRun{},
		ValidationRecord{}, Identity{}, OwnedResource{}, Inventory{}, Operation{},
	}
	for _, v := range types {
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name, _, _ := strings.Cut(tag, ",")
			if !snakeCase.MatchString(name) {
				t.Errorf("%s.%s has JSON name %q, want snake_case", rt.Name(), f.Name, name)
			}
		}
	}
}

// TestProblemFilesNotSerialised keeps private package files out of any JSON
// snapshot of a definition.
func TestProblemFilesNotSerialised(t *testing.T) {
	b, err := json.Marshal(Problem{Files: map[string][]byte{"solution/answer.yaml": []byte("secret")}})
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`secret|c2VjcmV0|files`).Match(b) {
		t.Fatalf("Problem JSON leaks package files: %s", b)
	}
}

func TestSessionStates(t *testing.T) {
	want := []string{"PREPARING", "RUNNING", "GRADING", "COMPLETED", "SETUP_ERROR", "GRADING_ERROR", "ABANDONED"}
	if got := SessionStates(); !reflect.DeepEqual(got, want) {
		t.Fatalf("SessionStates() = %v, want %v", got, want)
	}
}
