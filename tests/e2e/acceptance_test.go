//go:build linux && acceptance

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

const (
	acceptanceCycles    = 3
	standInCertificates = "stand-in certificates: lifecycle evidence, not package validation"
)

var scoreLine = regexp.MustCompile(`점수: (\d+)/(\d+)`)

// cycleEvidence is one lifecycle cycle's measurements, written as JSON.
// Durations are nanoseconds in the JSON; Summary is the human-readable form.
type cycleEvidence struct {
	Cycle        int
	Exam         string
	PrepareStart time.Duration
	Grade        time.Duration
	Reset        time.Duration
	InitialScore int
	SolvedScore  int
	Total        int
	PeakMiB      int
	Leftovers    ownedLeftovers
	OOMOrPending []string

	// Further evidence beyond the minimum record.
	// Start labels cycle 1's timing: "warm-after-setup" for every cycle
	// except cycle 1 of a run that proved cold ("cold") or only claimed it
	// ("cold (unverified)"). PrepareStart alone is warm-after-setup in
	// cycle 1; ColdStart (EnvironmentSetup + PrepareStart) is reported only
	// for a verified cold run.
	Start            string
	EnvironmentSetup time.Duration
	ColdStart        time.Duration
	InitialScoreFrom string // where InitialScore comes from
	OwnedObjects     int    // session-labelled objects found right after start (positive control)
	SolvedGrade      time.Duration
	OOMAfterStart    []string
	Certificates     string
	Problems         int
	PeakByContainer  map[string]int
	NamespacesAfter  int
	VolumesAfter     int
	Summary          string
}

// TestFullMockLifecycle is the release-acceptance evidence of spec section 13:
// prepare, model solution, grade and reset, three times in a row, against the
// real binary and a real kind cluster. The exam is mock-01, or, when the host
// cannot support a capability, a labelled PARTIAL exam (never reported as
// mock-01). Validation certificates are stand-ins.
func TestFullMockLifecycle(t *testing.T) {
	for _, name := range []string{"go", "docker", "kind", "kubectl", "helm"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatalf("the lifecycle acceptance test requires %s: %v", name, err)
		}
	}
	// Shorter than the go test -timeout=6h of make test-acceptance: if the
	// context were equal, a slow device would hit the test binary's panic
	// before the deferred cleanups run. 20 minutes remain for them.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Hour+40*time.Minute)
	defer cancel()

	sel := selectExam(t)
	t.Logf("EXAM %s (%d problems, %d points)", sel.Label, len(sel.IDs), sel.Points)
	evidenceDir := os.Getenv("JUNKLOUD_ACCEPTANCE_EVIDENCE")
	if evidenceDir == "" {
		evidenceDir = t.TempDir()
	}
	testutil.NoError(t, os.MkdirAll(evidenceDir, 0o755))
	coldRequested := os.Getenv("JUNKLOUD_ACCEPTANCE_COLD") == "1"
	coldVerified := coldRequested && pinnedImagesAbsent(ctx)
	t.Logf("cold requested=%t, pinned and node images verified absent before setup=%t", coldRequested, coldVerified)

	h := newHarness(t, sel.Bank)

	// Stand-in certificates (also creates the environment, so the baselines
	// below exist before cycle 1).
	setupStart := time.Now()
	h.certifyStandIn(ctx, sel.IDs)
	setup := time.Since(setupStart)
	t.Logf("%s; environment setup took %s", standInCertificates, setup)

	// Preflight: foreign objects and personal files that must survive.
	sentinelBefore := plantForeign(ctx, t, h)
	testutil.NoError(t, os.MkdirAll(h.workspace, 0o755))
	writePersonal(t, filepath.Join(h.workspace, ".bashrc"), "export PS1='learner$ '\n")
	writePersonal(t, filepath.Join(h.workspace, "notes.txt"), "my own notes\n")
	personalBefore := personalFiles(t, h.workspace)
	baseNamespaces, basePVs := clusterCounts(ctx, t, h)
	t.Logf("BASELINE namespaces=%d persistentvolumes=%d foreign=%+v", baseNamespaces, basePVs, sentinelBefore)
	if left := observeLeftovers(ctx, t, h); left.Count() != 0 {
		t.Fatalf("leftovers before cycle 1: %+v", left)
	}

	var (
		initialScores, solvedScores []int
		allLeftovers                []ownedLeftovers
		allOOM                      []string
		nsAfter, pvAfter            []int
		evidence                    []cycleEvidence
	)
	expectedPersonal := cloneMap(personalBefore)
	totalWant, initialWant := sel.Points, sel.Initial

	for cycle := 1; cycle <= acceptanceCycles; cycle++ {
		cycle := cycle
		ev := cycleEvidence{Cycle: cycle, Exam: sel.Label, Certificates: standInCertificates, Problems: len(sel.IDs), Start: "warm-after-setup"}
		if cycle == 1 {
			ev.EnvironmentSetup = setup
			switch {
			case coldVerified:
				ev.Start = "cold"
			case coldRequested:
				ev.Start = "cold (unverified)"
			}
		}
		sampler := startMemorySampler(ctx, 5*time.Second)

		started := time.Now()
		h.run("yes\n", 0, "start", sel.ExamID)
		ev.PrepareStart = time.Since(started)
		if ev.Start == "cold" {
			ev.ColdStart = ev.EnvironmentSetup + ev.PrepareStart
		}

		sid, frozen := currentSession(ctx, t, h)
		initialSum, pointsSum := 0, 0
		for _, f := range frozen {
			initialSum += f.Definition.InitialScore
			pointsSum += problemPoints(f.Definition)
		}
		testutil.Equal(t, len(frozen), len(sel.IDs))
		testutil.Equal(t, pointsSum, totalWant)
		testutil.Equal(t, initialSum, initialWant)
		// A successful start proves the initial vector: prepare's
		// verifyInitialStates fails the session (SETUP_ERROR) unless every
		// criterion and each problem's score equal the declared InitialScore.
		// This number is the sum of the frozen problems' declared scores, not
		// a learner grade.
		ev.InitialScore = initialSum
		ev.InitialScoreFrom = "prepare verification (verifyInitialStates/compareInitial) of the declared InitialScore; not a learner grade"

		// Positive controls: the owned-object selector matches after start,
		// and the pods are healthy.
		ev.OwnedObjects = ownedObjectCount(ctx, t, h)
		if ev.OwnedObjects == 0 {
			t.Fatalf("cycle %d: no session-labelled object after start; the leak selector cannot be trusted", cycle)
		}
		ev.OOMAfterStart = oomOrPending(ctx, t, h)
		graded := gradedImages(t, h, frozen)
		if slices.Contains(sel.IDs, "image-001") && len(graded) == 0 {
			t.Fatalf("cycle %d: the exam has image-001 but no graded image reference was found; the image assertions would be vacuous", cycle)
		}
		// A failed run must not leave its images for the next run's preflight.
		t.Cleanup(func() {
			for _, ref := range graded {
				docker(context.Background(), "image", "rm", ref)
			}
		})

		// A personal file the learner keeps in the first problem's attempt.
		attempt, _, err := session.LatestAttempt(h.workspace, sid, frozen[0].Definition.ID)
		testutil.NoError(t, err)
		notes := filepath.Join(attempt, "answer-notes.txt")
		writePersonal(t, notes, fmt.Sprintf("cycle %d private notes\n", cycle))
		expectedPersonal[notes] = personalFiles(t, h.workspace)[notes]

		h.solveCurrent(ctx)

		started = time.Now()
		solvedOut, _ := h.run("", 0, "grade")
		ev.Grade = time.Since(started)
		ev.SolvedGrade = ev.Grade
		var solvedTotal int
		ev.SolvedScore, solvedTotal = parseScore(t, solvedOut)
		ev.Total = solvedTotal
		testutil.Equal(t, solvedTotal, totalWant)
		if !strings.Contains(solvedOut, fmt.Sprintf("점수: %d/%d", totalWant, totalWant)) {
			t.Fatalf("cycle %d: full score line missing:\n%s", cycle, solvedOut)
		}
		h.run("", 0, "review")

		// Pods exist and are Ready here: OOM or Pending would show now.
		ev.OOMOrPending = append(slices.Clone(ev.OOMAfterStart), oomOrPending(ctx, t, h)...)
		// The managed graded image must exist before reset and be removed by it.
		for _, ref := range graded {
			if _, err := docker(ctx, "image", "inspect", ref); err != nil {
				t.Fatalf("cycle %d: the solved image %s is missing before reset: %v", cycle, ref, err)
			}
		}

		started = time.Now()
		h.run(environmentID(ctx, t, h)+"\n", 0, "reset")
		ev.Reset = time.Since(started)
		for _, ref := range graded {
			if _, err := docker(ctx, "image", "inspect", ref); err == nil {
				t.Errorf("cycle %d: reset left the managed image %s", cycle, ref)
			}
		}

		ev.PeakMiB, ev.PeakByContainer = sampler.Stop()
		if !sampler.Saw() {
			t.Fatalf("cycle %d: the memory sampler never saw a container; the peak is meaningless", cycle)
		}
		ev.Leftovers = observeLeftovers(ctx, t, h)
		ev.NamespacesAfter, ev.VolumesAfter = clusterCounts(ctx, t, h)
		ev.Summary = fmt.Sprintf("%s cycle %d (%s): start %s, solved grade %s, reset %s, score %d->%d/%d, peak %d MiB, leftovers %d, oom/pending %d",
			sel.Label, cycle, ev.Start, ev.PrepareStart.Round(time.Second), ev.Grade.Round(time.Second), ev.Reset.Round(time.Second),
			ev.InitialScore, ev.SolvedScore, ev.Total, ev.PeakMiB, ev.Leftovers.Count(), len(ev.OOMOrPending))
		t.Log(ev.Summary)

		data, err := json.MarshalIndent(ev, "", "  ")
		testutil.NoError(t, err)
		path := filepath.Join(evidenceDir, fmt.Sprintf("cycle-%d.json", cycle))
		testutil.NoError(t, os.WriteFile(path, data, 0o644))
		t.Logf("evidence: %s", path)

		initialScores = append(initialScores, ev.InitialScore)
		solvedScores = append(solvedScores, ev.SolvedScore)
		allLeftovers = append(allLeftovers, ev.Leftovers)
		allOOM = append(allOOM, ev.OOMOrPending...)
		if cycle == acceptanceCycles {
			allOOM = append(allOOM, oomOrPending(ctx, t, h)...) // after the last reset
		}
		nsAfter = append(nsAfter, ev.NamespacesAfter)
		pvAfter = append(pvAfter, ev.VolumesAfter)
		evidence = append(evidence, ev)

		// Each cycle's reset must leave foreign objects and personal files.
		testutil.Equal(t, readForeign(ctx, t, h), sentinelBefore)
		testutil.Equal(t, personalFiles(t, h.workspace), expectedPersonal)
	}

	t.Run("NoLeaksAfterThreeCycles", func(t *testing.T) {
		testutil.Equal(t, solvedScores, []int{totalWant, totalWant, totalWant})
		testutil.Equal(t, initialScores, []int{initialWant, initialWant, initialWant})
		for i, left := range allLeftovers {
			if left.Count() != 0 {
				t.Errorf("cycle %d leftovers: %+v", i+1, left)
			}
			testutil.Equal(t, left.Count(), 0)
		}
		var oomOrPendingFound []string
		oomOrPendingFound = append(oomOrPendingFound, allOOM...)
		testutil.Equal(t, oomOrPendingFound, []string(nil))
		// No obvious accumulation: counts return to the pre-cycle baseline.
		testutil.Equal(t, nsAfter, []int{baseNamespaces, baseNamespaces, baseNamespaces})
		testutil.Equal(t, pvAfter, []int{basePVs, basePVs, basePVs})
		testutil.Equal(t, readForeign(ctx, t, h), sentinelBefore)
		testutil.Equal(t, personalFiles(t, h.workspace), expectedPersonal)
		// Every non-personal original is untouched too.
		for path, sum := range personalBefore {
			testutil.Equal(t, personalFiles(t, h.workspace)[path], sum)
		}
	})

	peak := 0
	for _, ev := range evidence {
		peak = max(peak, ev.PeakMiB)
	}
	t.Logf("RESULT %s: %d cycles, scores %v of %d, initial %v, peak memory %d MiB (recorded, not asserted); %s",
		sel.Label, acceptanceCycles, solvedScores, totalWant, initialScores, peak, standInCertificates)
	if sel.Partial {
		t.Logf("%s", sel.Label)
	}
}

// examSelection is the exam the run serves, and how it must be labelled.
type examSelection struct {
	Bank    string
	ExamID  string
	IDs     []string
	Points  int
	Initial int
	Label   string
	Partial bool
}

// selectExam serves mock-01 unchanged when the host supports every
// capability. Otherwise it serves a copy of the bank with an exam made of
// mock-01 minus every problem that needs an unsupported capability, and the
// label says so: that run is never reported as mock-01.
func selectExam(t *testing.T) examSelection {
	t.Helper()
	root := repoRoot(t)
	unsupported := unsupportedCapabilities(t)
	exam, err := problem.LoadExam(filepath.Join(root, "exams", "mock-01.yaml"))
	testutil.NoError(t, err)
	sel := examSelection{Bank: root, ExamID: exam.ID, Label: "mock-01"}
	var skipped []string
	var ids []string
	for _, id := range exam.Problems {
		p, err := problem.Load(filepath.Join(root, "problems", id))
		testutil.NoError(t, err)
		if slices.ContainsFunc(p.Capabilities, func(c string) bool { return unsupported[c] }) {
			skipped = append(skipped, id)
			continue
		}
		ids = append(ids, id)
		sel.Points += problemPoints(p)
		sel.Initial += p.InitialScore
	}
	sel.IDs = ids
	if len(skipped) == 0 {
		return sel
	}
	names := make([]string, 0, len(unsupported))
	for name := range unsupported {
		names = append(names, name)
	}
	slices.Sort(names)
	sel.Partial = true
	sel.Label = fmt.Sprintf("PARTIAL: mock-01 minus [%s] (capability %s unsupported on this host)", strings.Join(skipped, ", "), strings.Join(names, ", "))
	sel.ExamID = "mock-" + strconv.Itoa(len(ids))
	sel.Bank = t.TempDir()
	copyDir(t, filepath.Join(root, "problems"), filepath.Join(sel.Bank, "problems"))
	testutil.NoError(t, os.MkdirAll(filepath.Join(sel.Bank, "exams"), 0o755))
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version: 1\nid: %s\nduration: %s\nproblems:\n", sel.ExamID, exam.Duration)
	for _, id := range ids {
		fmt.Fprintf(&b, "  - %s\n", id)
	}
	testutil.NoError(t, os.WriteFile(filepath.Join(sel.Bank, "exams", sel.ExamID+".yaml"), []byte(b.String()), 0o644))
	return sel
}

func unsupportedCapabilities(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, name := range strings.Split(os.Getenv("JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES"), ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !slices.Contains(environment.Capabilities, name) {
			t.Fatalf("JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES names unknown capability %q", name)
		}
		out[name] = true
	}
	return out
}

func parseScore(t *testing.T, out string) (score, total int) {
	t.Helper()
	m := scoreLine.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no score line in:\n%s", out)
	}
	score, _ = strconv.Atoi(m[1])
	total, _ = strconv.Atoi(m[2])
	return score, total
}

func writePersonal(t *testing.T, path, content string) {
	t.Helper()
	testutil.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	testutil.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// currentSession reads the current session and its frozen problems.
func currentSession(ctx context.Context, t *testing.T, h *cliHarness) (string, []model.FrozenProblem) {
	t.Helper()
	st, err := store.Open(filepath.Join(h.state, "sessions.db"))
	testutil.NoError(t, err)
	defer st.Close()
	sid, err := st.CurrentSessionID(ctx)
	testutil.NoError(t, err)
	rec, err := st.LoadSession(ctx, sid)
	testutil.NoError(t, err)
	return sid, rec.Problems
}

// environmentID is the recorded environment's ID, which `ckad reset` asks for.
func environmentID(ctx context.Context, t *testing.T, h *cliHarness) string {
	t.Helper()
	return h.identity(ctx).ID
}

// problemPoints is the problem's total score: the sum of its criteria points.
func problemPoints(p model.Problem) int {
	total := 0
	for _, c := range p.Criteria {
		total += c.Points
	}
	return total
}

// gradedImages are the image references the model solution builds with
// ordinary Docker (rendered from the frozen problem's image actions).
func gradedImages(t *testing.T, h *cliHarness, frozen []model.FrozenProblem) []string {
	t.Helper()
	var refs []string
	for _, f := range frozen {
		pkg, err := session.LoadPackage(h.state, f.Hash)
		testutil.NoError(t, err)
		for _, a := range pkg.Solution {
			if a.Kind != model.ActionImage {
				continue
			}
			rendered, err := problem.RenderValue(a.Args, f.Parameters)
			testutil.NoError(t, err)
			args := rendered.(map[string]any)
			if ref, _ := args["reference"].(string); ref != "" && args["operation"] == "build" && !slices.Contains(refs, ref) {
				refs = append(refs, ref)
			}
		}
	}
	return refs
}

// pinnedImagesAbsent reports whether none of the node and pinned platform
// images is in the local Docker store (a cold start's precondition).
func pinnedImagesAbsent(ctx context.Context) bool {
	for _, ref := range []string{environment.DefaultNodeImage, environment.CloudProviderKindImage.Ref(), environment.EnvoyProxyImage.Ref(), environment.MetricsServerImage.Ref(), environment.ProbeImage.Ref()} {
		if _, err := docker(ctx, "image", "inspect", ref); err == nil {
			return false
		}
	}
	return true
}
