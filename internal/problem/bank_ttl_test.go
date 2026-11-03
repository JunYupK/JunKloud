package problem_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// A mock exam lasts 120 minutes and the learner reviews (and may resubmit)
// after it, so a Job a bank problem provides must still exist well after the
// whole exam. A correct learner who completes it early and grades or reviews
// later must not lose it: the TTL controller would turn that into a
// job-completed FAIL or an !inputs-changed ERROR (batch-001 did, with 300).
// The floor is the mock duration plus a review margin of the same size; a
// problem with no TTL at all passes.
const (
	mockDurationSeconds = 7200
	reviewMarginSeconds = 7200
	minTTLSeconds       = mockDurationSeconds + reviewMarginSeconds // 14400
)

var ttlLine = regexp.MustCompile(`"?ttlSecondsAfterFinished"?\s*:\s*(\d+)`)

func TestBankJobsOutliveGrading(t *testing.T) {
	root := filepath.Join("..", "..", "problems")
	checked, scanned := 0, 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		scanned++
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
	if scanned == 0 {
		t.Fatal("no problem file scanned; the test would pass vacuously")
	}
	t.Logf("checked %d ttlSecondsAfterFinished values", checked)
}
