//go:build integration

package integration

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/cli"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// The exact085 full bank already proved every package's real seed/case graph.
// Its temporary certificate DB was deleted after CLI startup failed. This
// targeted regression issues only a genuine image certificate and verifies
// the actual CLI catalog; it does not substitute a clone bank or claim practice5.
func TestBuildImageDoctorEligibility(t *testing.T) {
	b := openBankRuntime(t)
	ctx := context.Background()
	testutil.NoError(t, author.PrepareEnvironment(ctx, b.m, b.id, b.cfg.Images))
	absoluteBank, err := filepath.Abs(bankRoot)
	testutil.NoError(t, err)
	workspace := t.TempDir()
	svc := session.New(b.st, b.m, b.rt, absoluteBank, b.dir, session.WithWorkspace(workspace))
	report, err := svc.Doctor(ctx)
	testutil.NoError(t, err)
	imageAvailable := false
	for _, c := range report.Checks {
		t.Logf("REAL DOCTOR %s %s: %s", c.Name, c.Status, c.Reason)
		if c.Name == environment.CapabilityImageBuild && c.Status == environment.StatusPass {
			imageAvailable = true
		}
	}
	testutil.Equal(t, imageAvailable, true)
	p := loadGroup(t, []string{"image-001"})[0]
	seeds, err := author.CoveringSeeds(p)
	testutil.NoError(t, err)
	record, err := b.rt.Validator.Validate(ctx, p, seeds)
	testutil.NoError(t, err)
	testutil.Equal(t, record.Passed, true)
	t.Logf("REAL IMAGE VALIDATION passed=%t hash=%s seeds=%v stages=%d", record.Passed, record.ProblemHash, record.Seeds, len(record.Cases))
	t.Setenv("CKAD_HOST_COMPAT", os.Getenv("JUNKLOUD_TEST_HOST_COMPAT"))
	var out, diag bytes.Buffer
	code := cli.Run(ctx, []string{"--state-dir", b.dir, "--bank-dir", absoluteBank, "--workspace", workspace, "list"}, strings.NewReader(""), &out, &diag)
	t.Logf("REAL CLI list exit=%d stdout=%s stderr=%s", code, out.String(), diag.String())
	testutil.Equal(t, code, 0)
	eligible := false
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "image-001 | ") {
			eligible = true
		}
		if strings.HasPrefix(line, "출제 불가: image-001 ") {
			t.Fatal("genuinely validated image was excluded by actual CLI catalog")
		}
	}
	testutil.Equal(t, eligible, true)
}
