package problem_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func writeExam(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "exam.yaml")
	testutil.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

const validExam = `# comment
schema_version: 1
id: mock-dev
duration: 120m
problems:
  - svc-one
  - svc-two
  - svc-three
`

func TestLoadExam(t *testing.T) {
	e, err := problem.LoadExam(writeExam(t, validExam))
	testutil.NoError(t, err)
	testutil.Equal(t, e, problem.Exam{ID: "mock-dev", Duration: 120 * time.Minute, Problems: []string{"svc-one", "svc-two", "svc-three"}})

	// Composite durations are plain Go durations.
	e, err = problem.LoadExam(writeExam(t, strings.Replace(validExam, "120m", "1h30m", 1)))
	testutil.NoError(t, err)
	testutil.Equal(t, e.Duration, 90*time.Minute)
}

func TestLoadExamRejects(t *testing.T) {
	cases := map[string]string{
		"unknown field":      validExam + "extra: 1\n",
		"duplicate key":      validExam + "id: other\n",
		"wrong schema":       strings.Replace(validExam, "schema_version: 1", "schema_version: 2", 1),
		"missing schema":     strings.Replace(validExam, "schema_version: 1\n", "", 1),
		"missing id":         strings.Replace(validExam, "id: mock-dev\n", "", 1),
		"bad id":             strings.Replace(validExam, "id: mock-dev", "id: Mock_Dev", 1),
		"missing duration":   strings.Replace(validExam, "duration: 120m\n", "", 1),
		"zero duration":      strings.Replace(validExam, "120m", "0s", 1),
		"negative duration":  strings.Replace(validExam, "120m", "-5m", 1),
		"not a duration":     strings.Replace(validExam, "120m", "two hours", 1),
		"numeric duration":   strings.Replace(validExam, "duration: 120m", "duration: 120", 1),
		"no problems":        "schema_version: 1\nid: mock-dev\nduration: 120m\nproblems: []\n",
		"missing problems":   "schema_version: 1\nid: mock-dev\nduration: 120m\n",
		"repeated problem":   validExam + "  - svc-one\n",
		"bad problem id":     validExam + "  - Not_An_ID\n",
		"not yaml":           "id: [unclosed\n",
		"empty file":         "",
		"problems not list":  "schema_version: 1\nid: mock-dev\nduration: 120m\nproblems: svc-one\n",
		"problem not string": "schema_version: 1\nid: mock-dev\nduration: 120m\nproblems: [1, 2]\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := problem.LoadExam(writeExam(t, content))
			testutil.ErrorIs(t, err, problem.ErrInvalidExam)
		})
	}
}

func TestLoadExamMissingFile(t *testing.T) {
	_, err := problem.LoadExam(filepath.Join(t.TempDir(), "absent.yaml"))
	testutil.ErrorIs(t, err, os.ErrNotExist)
}
