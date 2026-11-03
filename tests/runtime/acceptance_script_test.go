//go:build linux

package runtime_test

import (
	"os"
	"os/exec"
	"path/filepath"
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
	for _, tc := range []struct{ file, want string }{{allPass, ""}, {oneFail, "network-policy"}, {twoFail, "network-policy,ingress"}} {
		if got := runAcceptanceFunc(t, `unsupported_from_doctor "`+tc.file+`"`); got != tc.want {
			t.Errorf("%s: unsupported = %q, want %q", filepath.Base(tc.file), got, tc.want)
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
