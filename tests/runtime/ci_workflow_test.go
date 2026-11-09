//go:build linux

package runtime_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

type ciJob struct {
	If     string `json:"if"`
	RunsOn any    `json:"runs-on"`
	Needs  any    `json:"needs"`
	Strat  struct {
		Matrix struct {
			Runner []string `json:"runner"`
		} `json:"matrix"`
	} `json:"strategy"`
	Steps []struct {
		Name string `json:"name"`
		Uses string `json:"uses"`
		Run  string `json:"run"`
	} `json:"steps"`
}

func loadCIWorkflow(t *testing.T) (map[string]any, map[string]ciJob, string) {
	t.Helper()
	path := filepath.Join("..", "..", ".github", "workflows", "ci.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	var top struct {
		Jobs map[string]ciJob `json:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &top); err != nil {
		t.Fatalf("workflow is not valid YAML: %v", err)
	}
	var generic map[string]any
	if err := yaml.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	return generic, top.Jobs, string(raw)
}

func (j ciJob) text() string {
	var b strings.Builder
	for _, s := range j.Steps {
		b.WriteString(s.Uses + "\n" + s.Run + "\n")
	}
	return b.String()
}

// TestCIWorkflowParses keeps the CI definition honest: the four jobs exist, the
// two heavy ones run only on manual dispatch, image builds run on native
// runners of both architectures, and nothing uses emulation (M2 results must
// never come from QEMU), and the integration tests CI names really exist.
func TestCIWorkflowParses(t *testing.T) {
	generic, jobs, raw := loadCIWorkflow(t)
	for _, name := range []string{"static", "image", "integration-smoke", "validate-networkpolicy"} {
		if _, ok := jobs[name]; !ok {
			t.Errorf("job %q is missing", name)
		}
	}
	if len(jobs) != 4 {
		t.Errorf("want exactly 4 jobs, got %d", len(jobs))
	}

	// YAML 1.1 parsers read the key `on` as boolean true.
	on, ok := generic["on"].(map[string]any)
	if !ok {
		on, ok = generic["true"].(map[string]any)
	}
	if !ok {
		t.Fatalf("workflow has no trigger map: %v", generic)
	}
	if _, ok := on["workflow_dispatch"]; !ok {
		t.Error("workflow_dispatch trigger is missing")
	}

	var code []string
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			code = append(code, line)
		}
	}
	lower := strings.ToLower(strings.Join(code, "\n")) // comments may explain why emulation is banned
	for _, banned := range []string{"qemu", "setup-qemu-action", "binfmt", "--platform"} {
		if strings.Contains(lower, banned) {
			t.Errorf("workflow mentions %q; CI must use native runners, never emulation", banned)
		}
	}

	for _, name := range []string{"integration-smoke", "validate-networkpolicy"} {
		if !strings.Contains(jobs[name].If, "workflow_dispatch") {
			t.Errorf("job %q must run only on workflow_dispatch, if: %q", name, jobs[name].If)
		}
	}
	for _, name := range []string{"static", "image"} {
		if strings.Contains(jobs[name].If, "workflow_dispatch") {
			t.Errorf("job %q must run on every trigger, if: %q", name, jobs[name].If)
		}
	}

	if got := jobs["static"].RunsOn; got != "ubuntu-24.04" {
		t.Errorf("static runs-on = %v, want ubuntu-24.04", got)
	}
	runners := jobs["image"].Strat.Matrix.Runner
	if strings.Join(runners, ",") != "ubuntu-24.04,ubuntu-24.04-arm" {
		t.Errorf("image matrix runners = %v, want ubuntu-24.04 and ubuntu-24.04-arm", runners)
	}
	if !strings.Contains(raw, "${{ matrix.runner }}") {
		t.Error("image job must run on matrix.runner")
	}
	if !strings.Contains(jobs["image"].text(), "docker build") {
		t.Error("image job must run docker build")
	}

	static := jobs["static"].text()
	for _, want := range []string{"go test ./... -count=1", "make lint", "GOARCH=amd64", "GOARCH=arm64", "go build"} {
		if !strings.Contains(static, want) {
			t.Errorf("static job lacks %q", want)
		}
	}

	// Every test CI names must exist under tests/integration.
	src := ""
	files, _ := filepath.Glob(filepath.Join("..", "integration", "*_test.go"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src += string(b)
	}
	if len(files) == 0 {
		t.Fatal("no integration test files found")
	}
	smoke := jobs["integration-smoke"].text()
	for _, fn := range []string{"TestNetworkBankSmoke", "TestReadyFailedCreateIsLearnerFault"} {
		if !strings.Contains(smoke, fn) {
			t.Errorf("integration-smoke does not run %s", fn)
		}
	}
	if !strings.Contains(smoke, "-tags=integration") || !strings.Contains(smoke, "-timeout=60m") {
		t.Error("integration-smoke must use -tags=integration and -timeout=60m")
	}
	cert := jobs["validate-networkpolicy"].text()
	if !strings.Contains(cert, "^TestNetworkBank$/^networkpolicy-001$") || !strings.Contains(cert, "-timeout=90m") {
		t.Error("validate-networkpolicy must run subtest TestNetworkBank/networkpolicy-001 (-run ^TestNetworkBank$/^networkpolicy-001$) with -timeout=90m")
	}
	if !strings.Contains(jobs["validate-networkpolicy"].text(), "upload-artifact") {
		t.Error("validate-networkpolicy must upload its log")
	}
	for _, text := range []string{smoke, cert} {
		for _, m := range regexp.MustCompile(`\bTest[A-Za-z0-9]+`).FindAllString(text, -1) {
			if !regexp.MustCompile(`func ` + m + `\(`).MatchString(src) {
				t.Errorf("CI references %s, which does not exist in tests/integration", m)
			}
		}
	}
}
