package problem

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"sigs.k8s.io/yaml"
)

// ErrInvalidExam wraps every reason an exam file is rejected.
var ErrInvalidExam = errors.New("invalid exam definition")

// ExamSchemaVersion is the only exam file version this build understands.
const ExamSchemaVersion = 1

// Exam is a mock exam: a fixed, reviewed list of problems in exam order and
// the total time allowed. Mock sessions are composed from it; practice and
// retry sessions are composed by Select.
type Exam struct {
	ID       string
	Duration time.Duration
	Problems []string
}

// examFile is the YAML shape of an exam (exams/<id>.yaml):
//
//	schema_version: 1
//	id: mock-01
//	duration: 120m
//	problems: [problem-id, ...]
type examFile struct {
	SchemaVersion *int     `json:"schema_version"`
	ID            string   `json:"id"`
	Duration      string   `json:"duration"`
	Problems      []string `json:"problems"`
}

// LoadExam reads and validates an exam file. The YAML is strict (no
// duplicate or unknown keys); id and problem IDs use the problem ID syntax,
// the duration is a positive Go duration and the problem list is non-empty
// with no repeated ID. It does not look the problems up: the caller does,
// and refuses an exam that names a problem the bank lacks. Errors wrap
// ErrInvalidExam, except a file that cannot be read.
func LoadExam(path string) (Exam, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Exam{}, fmt.Errorf("read exam: %w", err)
	}
	e, err := parseExam(raw)
	if err != nil {
		return Exam{}, fmt.Errorf("%w: %s: %w", ErrInvalidExam, path, err)
	}
	return e, nil
}

func parseExam(raw []byte) (Exam, error) {
	doc, err := yaml.YAMLToJSONStrict(raw)
	if err != nil {
		return Exam{}, err
	}
	var f examFile
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Exam{}, err
	}
	if f.SchemaVersion == nil || *f.SchemaVersion != ExamSchemaVersion {
		return Exam{}, fmt.Errorf("schema_version must be %d", ExamSchemaVersion)
	}
	if !dnsLabel.MatchString(f.ID) {
		return Exam{}, fmt.Errorf("id %q must be lower-case letters, digits and hyphens", f.ID)
	}
	if f.Duration == "" {
		return Exam{}, errors.New("duration is required")
	}
	d, err := time.ParseDuration(f.Duration)
	if err != nil {
		return Exam{}, fmt.Errorf("duration: %w", err)
	}
	if d <= 0 {
		return Exam{}, fmt.Errorf("duration %q must be positive", f.Duration)
	}
	if len(f.Problems) == 0 {
		return Exam{}, errors.New("problems must list at least one problem ID")
	}
	seen := map[string]bool{}
	for _, id := range f.Problems {
		if !dnsLabel.MatchString(id) {
			return Exam{}, fmt.Errorf("problem ID %q must be lower-case letters, digits and hyphens", id)
		}
		if seen[id] {
			return Exam{}, fmt.Errorf("problem %q is listed twice", id)
		}
		seen[id] = true
	}
	return Exam{ID: f.ID, Duration: d, Problems: f.Problems}, nil
}
