//go:build linux

package runtime_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// runAcceptanceFunc sources runtime/acceptance.sh (which only runs main when
// executed) and calls one of its functions.
func runAcceptanceFunc(t *testing.T, script string) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", "source runtime/acceptance.sh; "+script)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestAcceptanceScriptSyntax(t *testing.T) {
	if out, err := exec.Command("bash", "-n", filepath.Join("..", "..", "runtime", "acceptance.sh")).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
}

// The script must hand the doctor's FAILed capabilities to the acceptance test
// through JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES; otherwise the test would run
// the full mock on a host that cannot serve it.
func TestAcceptanceScriptDerivesUnsupportedCapabilities(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	allPass := write("pass.txt", "전용 환경: abc123def456\nkind: v0.33.0\ndns: PASS\nservice: PASS\nnetwork-policy: PASS\ningress: PASS\nstorage: PASS\nmetrics: PASS\nimage-build: PASS\n")
	oneFail := write("one.txt", "전용 환경: abc123def456\nkind: v0.33.0\ndns: PASS\nservice: PASS\nnetwork-policy: FAIL\ningress: PASS\nstorage: PASS\nmetrics: PASS\nimage-build: PASS\n")
	twoFail := write("two.txt", "전용 환경: abc123def456\ndns: PASS\nservice: PASS\nnetwork-policy: FAIL\ningress: FAIL\nstorage: PASS\nmetrics: PASS\nimage-build: PASS\n")
	dnsFail := write("dns.txt", "전용 환경: abc123def456\ndns: FAIL\nservice: PASS\nnetwork-policy: PASS\ningress: PASS\nstorage: PASS\nmetrics: PASS\nimage-build: PASS\n")
	// Only a network-policy FAIL makes the run PARTIAL (the test selects a
	// partial exam only for what is listed, and the product treats the other
	// capabilities as required). Any other FAIL is unexpected and reported.
	for _, tc := range []struct{ file, unsupported, unexpected string }{
		{allPass, "", ""}, {oneFail, "network-policy", ""}, {twoFail, "network-policy", "ingress"}, {dnsFail, "", "dns"},
	} {
		if got := runAcceptanceFunc(t, `unsupported_from_doctor "`+tc.file+`"`); got != tc.unsupported {
			t.Errorf("%s: unsupported = %q, want %q", filepath.Base(tc.file), got, tc.unsupported)
		}
		if got := runAcceptanceFunc(t, `unexpected_from_doctor "`+tc.file+`"`); got != tc.unexpected {
			t.Errorf("%s: unexpected = %q, want %q", filepath.Base(tc.file), got, tc.unexpected)
		}
	}
	if got := runAcceptanceFunc(t, `environment_id_from_doctor "`+oneFail+`"`); got != "abc123def456" {
		t.Errorf("environment id = %q", got)
	}
	// An output with no capability lines (doctor could not run) is an error,
	// never "all supported".
	empty := write("empty.txt", "doctor failed\n")
	cmd := exec.Command("bash", "-c", `source runtime/acceptance.sh; unsupported_from_doctor "`+empty+`"`)
	cmd.Dir = filepath.Join("..", "..")
	if err := cmd.Run(); err == nil {
		t.Error("a doctor output without capability lines must be an error")
	}
}

// The validated ID list must be the real bank, so a new problem cannot be
// forgotten.
func TestAcceptanceScriptCoversTheWholeBank(t *testing.T) {
	root := filepath.Join("..", "..")
	entries, err := os.ReadDir(filepath.Join(root, "problems"))
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, e := range entries {
		if e.IsDir() {
			want = append(want, e.Name())
		}
	}
	got := strings.Fields(runAcceptanceFunc(t, `CKAD_BANK_DIR=. problem_ids`))
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("problem_ids = %v, want %v", got, want)
	}
	if len(got) != 20 {
		t.Errorf("want 20 problems, got %d", len(got))
	}
}

// acceptance.sh itself must never choose the unsupported list by hand.
func TestAcceptanceScriptNeverHardcodesUnsupported(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "runtime", "acceptance.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES=") && strings.Contains(line, "network-policy") {
			t.Errorf("hard-coded unsupported capability: %s", line)
		}
		if strings.Contains(line, "JUNKLOUD_TEST_HOST_COMPAT=") && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			t.Errorf("a real device run must not set the sandbox compat variable: %s", line)
		}
	}
}

// stubbedAcceptance runs the whole script with fake docker/kind/ckad/make on
// PATH. The fakes append their calls to calls.log.
func stubbedAcceptance(t *testing.T, dockerPS, ckadBank string, args ...string) (out string, calls string, exit int) {
	t.Helper()
	return stubbedAcceptanceEnv(t, dockerPS, ckadBank, "", args...)
}

func stubbedAcceptanceDoctor(t *testing.T, doctor string) (string, string, int) {
	t.Helper()
	return stubbedAcceptanceEnv(t, "", "exit 0", doctor)
}

func stubbedAcceptanceEnv(t *testing.T, dockerPS, ckadBank, doctor string, args ...string) (out string, calls string, exit int) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	put := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	put("docker", `case "$*" in
 "info --format {{.MemTotal}}") echo 4294967296;;
 "info --format {{.NCPU}}") echo 4;;
 "info --format {{.ServerVersion}}") echo 29.0.0;;
 "ps --format {{.Names}}") printf '%s' "$FAKE_PS";;
 *) echo "docker $*" >> "$CALLS";;
esac
`)
	put("kind", `echo "kind $*" >> "$CALLS"
`)
	put("make", `echo "make $* UNSUP=$JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES" >> "$CALLS"
echo "    RESULT $FAKE_RESULT"
`)
	put("ckad", `echo "ckad $*" >> "$CALLS"
case "$1" in
 doctor) printf '전용 환경: stub123456ab\n'; printf '%s\n' "$FAKE_DOCTOR"; exit 0;;
 bank) `+ckadBank+`;;
esac
`)
	calls = filepath.Join(dir, "calls.log")
	home := filepath.Join(dir, "home")
	_ = os.MkdirAll(home, 0o755)
	cmd := exec.Command("bash", append([]string{"runtime/acceptance.sh"}, args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HOME="+home, "CALLS="+calls, "FAKE_PS="+dockerPS,
		"CKAD_BANK_DIR="+root, "FAKE_RESULT=PARTIAL: mock-01 minus [networkpolicy-001] (stub)",
		"FAKE_DOCTOR=dns: PASS\nservice: PASS\nnetwork-policy: FAIL\ningress: PASS\nstorage: PASS\nmetrics: PASS\nimage-build: PASS")
	if doctor != "" {
		cmd.Env = append(cmd.Env, "FAKE_DOCTOR="+doctor)
	}
	b, err := cmd.CombinedOutput()
	exit = 0
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	c, _ := os.ReadFile(calls)
	return string(b), string(c), exit
}

// Another JunKloud environment (the user's own practice cluster) would be summed
// into the lifecycle test's peak memory and would put two clusters into one
// small VM, so the script refuses to start next to one.
func TestAcceptanceScriptRefusesRunningJunkloudContainers(t *testing.T) {
	got := runAcceptanceFunc(t, `docker() { printf 'junkloud-abc-control-plane\nredis\nkindccm-gw-1\njunkloud-cpk-abc\n'; }; running_junkloud_containers`)
	if strings.Fields(got)[0] != "junkloud-abc-control-plane" || len(strings.Fields(got)) != 3 {
		t.Errorf("running_junkloud_containers = %q", got)
	}
	// The compose containers of this very repository (project "junkloud") are
	// named junkloud-dev-1 and junkloud-workspace-1; they are not another environment.
	if got := runAcceptanceFunc(t, `docker() { printf 'junkloud-dev-1\njunkloud-workspace-1\njunkloud-sock-init-1\n'; }; running_junkloud_containers`); got != "" {
		t.Errorf("compose service containers must not count as another environment: %q", got)
	}
	out, calls, exit := stubbedAcceptance(t, "junkloud-xyz-control-plane\n", "exit 0")
	if exit == 0 || strings.Contains(calls, "ckad ") || strings.Contains(calls, "make ") {
		t.Errorf("must refuse before running anything: exit %d calls %q\n%s", exit, calls, out)
	}
	if !strings.Contains(out, "junkloud-xyz-control-plane") {
		t.Errorf("the refusal must name the container:\n%s", out)
	}
	// The suggested docker stop must never match the work containers: the
	// script runs inside one of them, so stopping it would kill the user's session.
	filters := regexp.MustCompile(`name=(\^[^" ]+)`).FindAllStringSubmatch(out, -1)
	if len(filters) == 0 {
		t.Errorf("the refusal must print anchored name= filters:\n%s", out)
	}
	for _, f := range filters {
		re := regexp.MustCompile(f[1])
		for _, work := range []string{"junkloud-dev-1", "junkloud-workspace-1", "junkloud-sock-init-1"} {
			if re.MatchString(work) {
				t.Errorf("suggested filter %q matches the work container %s", f[1], work)
			}
		}
	}
	if regexp.MustCompile(`name=junkloud-`).MatchString(out) {
		t.Errorf("the suggestion must not use the unanchored name=junkloud- filter:\n%s", out)
	}
	// With nothing running it proceeds.
	if _, calls, exit = stubbedAcceptance(t, "", "exit 0"); exit != 0 || !strings.Contains(calls, "make test-acceptance") {
		t.Errorf("clean host must run: exit %d calls %q", exit, calls)
	}
}

// An interrupt during validation must remove the throwaway cluster, not only
// the work directory.
func TestAcceptanceScriptInterruptReleasesTheThrowawayCluster(t *testing.T) {
	_, calls, exit := stubbedAcceptance(t, "", `kill -INT $PPID; sleep 1; exit 0`)
	if exit == 0 {
		t.Errorf("an interrupted run must not exit 0")
	}
	if !strings.Contains(calls, "kind delete cluster --name junkloud-stub123456ab") {
		t.Errorf("the throwaway cluster was not released after SIGINT; calls:\n%s", calls)
	}
	if strings.Contains(calls, "make ") {
		t.Errorf("the acceptance phase must not run after an interrupt; calls:\n%s", calls)
	}
}

// A non network-policy FAIL is unexpected: it must not look like a clean
// PARTIAL run, and the run must not exit 0.
func TestAcceptanceScriptFlagsUnexpectedCapabilityFailures(t *testing.T) {
	out, calls, exit := stubbedAcceptanceDoctor(t, "dns: FAIL\nservice: PASS\nnetwork-policy: PASS\ningress: PASS\nstorage: PASS\nmetrics: PASS\nimage-build: PASS")
	if exit == 0 {
		t.Errorf("an unexpected capability FAIL must give a non-zero exit\n%s", out)
	}
	if !strings.Contains(out, "UNEXPECTED") || !strings.Contains(out, "dns") {
		t.Errorf("summary must flag the unexpected FAIL:\n%s", out)
	}
	if strings.Contains(calls, "UNSUP=dns") {
		t.Errorf("dns must never be passed as an unsupported capability:\n%s", calls)
	}
}
