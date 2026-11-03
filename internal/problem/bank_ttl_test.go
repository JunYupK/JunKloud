package problem_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// minTTLSeconds: a Job a bank problem creates or provides must outlive a
// full 19-problem grading (about 5 minutes) with a wide margin. With a
// ttlSecondsAfterFinished of 300 the TTL controller deleted batch-001's
// parcel-once Job while `ckad grade` was still running, so a correct answer
// graded ERROR (!inputs-changed) or, later, FAIL (Job not found).
const minTTLSeconds = 1800

var ttlLine = regexp.MustCompile(`"?ttlSecondsAfterFinished"?\s*:\s*(\d+)`)

func TestBankJobsOutliveGrading(t *testing.T) {
	root := filepath.Join("..", "..", "problems")
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range ttlLine.FindAllSubmatch(data, -1) {
			checked++
			if n, _ := strconv.Atoi(string(m[1])); n < minTTLSeconds {
				t.Errorf("%s: ttlSecondsAfterFinished %d < %d: the Job can vanish while it is being graded", path, n, minTTLSeconds)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no ttlSecondsAfterFinished found; the test would pass vacuously")
	}
}
