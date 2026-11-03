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
	Start            string // "cold" or "warm"
	EnvironmentSetup time.Duration
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
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()

	sel := selectExam(t)
	t.Logf("EXAM %s (%d problems, %d points)", sel.Label, len(sel.IDs), sel.Points)
	evidenceDir := os.Getenv("JUNKLOUD_ACCEPTANCE_EVIDENCE")
	if evidenceDir == "" {
		evidenceDir = t.TempDir()
	}
	testutil.NoError(t, os.MkdirAll(evidenceDir, 0o755))
	cold := os.Getenv("JUNKLOUD_ACCEPTANCE_COLD") == "1"

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
		ev := cycleEvidence{Cycle: cycle, Exam: sel.Label, Certificates: standInCertificates, Problems: len(sel.IDs), Start: "warm"}
		if cycle == 1 {
			ev.EnvironmentSetup = setup
			if cold {
				ev.Start = "cold"
			}
		}
		sampler := startMemorySampler(ctx, 5*time.Second)

		started := time.Now()
		h.run("yes\n", 0, "start", sel.ExamID)
		ev.PrepareStart = time.Since(started)

		sid, frozen := currentSession(ctx, t, h)
		initialSum, pointsSum := 0, 0
		for _, f := range frozen {
			initialSum += f.Definition.InitialScore
			pointsSum += problemPoints(f.Definition)
		}
		testutil.Equal(t, len(frozen), len(sel.IDs))
		testutil.Equal(t, pointsSum, totalWant)
		testutil.Equal(t, initialSum, initialWant)

		// While RUNNING, build one session-owned image so that reset's owned
		// image cleanup is exercised (a submitted mock no longer accepts
		// `ckad image build`, so the graded image is built by ordinary Docker
		// after the first grade; see solveCurrent).
		probe := buildOwnedProbe(ctx, t, h, frozen)
		graded := gradedImages(t, h, frozen)
		// A failed run must not leave its images for the next run's preflight.
		t.Cleanup(func() {
			for _, ref := range append(slices.Clone(graded), probe) {
				if ref != "" {
					docker(context.Background(), "image", "rm", ref)
				}
			}
		})

		// A personal file the learner keeps in the first problem's attempt.
		attempt, _, err := session.LatestAttempt(h.workspace, sid, frozen[0].Definition.ID)
		testutil.NoError(t, err)
		notes := filepath.Join(attempt, "answer-notes.txt")
		writePersonal(t, notes, fmt.Sprintf("cycle %d private notes\n", cycle))
		expectedPersonal[notes] = personalFiles(t, h.workspace)[notes]

		started = time.Now()
		initialOut, _ := h.run("", 1, "grade")
		ev.Grade = time.Since(started)
		ev.InitialScore, ev.Total = parseScore(t, initialOut)
		testutil.Equal(t, ev.InitialScore, initialWant)
		testutil.Equal(t, ev.Total, totalWant)

		h.solveCurrent(ctx)

		solvedOut, _ := h.run("", 0, "grade", "--resubmit")
		var solvedTotal int
		ev.SolvedScore, solvedTotal = parseScore(t, solvedOut)
		testutil.Equal(t, solvedTotal, totalWant)
		if !strings.Contains(solvedOut, fmt.Sprintf("점수: %d/%d", totalWant, totalWant)) {
			t.Fatalf("cycle %d: full score line missing:\n%s", cycle, solvedOut)
		}
		h.run("", 0, "review")

		// Pods exist and are Ready here: OOM or Pending would show now.
		ev.OOMOrPending = oomOrPending(ctx, t, h)

		// The graded image was built with ordinary Docker, so the session does
		// not own it, and reset refuses an image it cannot prove it owns
		// ("ordinary Docker images require your explicit removal before reset
		// recovery", image-001's question). The learner removes it first.
		for _, ref := range graded {
			if out, err := docker(ctx, "image", "rm", ref); err != nil {
				t.Fatalf("cycle %d: remove the learner image %s: %v %s", cycle, ref, err, out)
			}
		}

		started = time.Now()
		h.run(environmentID(ctx, t, h)+"\n", 0, "reset")
		ev.Reset = time.Since(started)

		// The session-owned probe image is gone with the reset.
		if probe != "" {
			if _, err := docker(ctx, "image", "inspect", probe); err == nil {
				t.Errorf("cycle %d: reset left the owned image %s", cycle, probe)
			}
		}

		ev.PeakMiB, ev.PeakByContainer = sampler.Stop()
		ev.Leftovers = observeLeftovers(ctx, t, h)
		ev.NamespacesAfter, ev.VolumesAfter = clusterCounts(ctx, t, h)
		ev.Summary = fmt.Sprintf("%s cycle %d (%s): start %s, grade %s, reset %s, score %d->%d/%d, peak %d MiB, leftovers %d, oom/pending %d",
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

const ownedProbeTag = "junkloud.local/image-001:owned-probe"

// buildOwnedProbe runs `ckad image build` for image-001 (when the exam has
// it) with the provided, unfinished Dockerfile under a tag no criterion
// grades, so the initial score is unchanged. It returns the tag, or "".
func buildOwnedProbe(ctx context.Context, t *testing.T, h *cliHarness, frozen []model.FrozenProblem) string {
	t.Helper()
	for i, f := range frozen {
		if f.Definition.ID != "image-001" {
			continue
		}
		h.run("", 0, "image", "build", "--problem", fmt.Sprintf("Q%d", i+1), "--context", ".", "--tag", ownedProbeTag)
		out, err := docker(ctx, "image", "inspect", "--format", `{{index .Config.Labels "junkloud.io/session-id"}}`, ownedProbeTag)
		testutil.NoError(t, err)
		if strings.TrimSpace(out) == "" {
			t.Fatalf("the managed build did not label %s with the session", ownedProbeTag)
		}
		return ownedProbeTag
	}
	return ""
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
