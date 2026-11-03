//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/cli"
	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// TestMockServing proves the all-or-nothing gate of the default mock on a host
// whose doctor reports network-policy FAIL (the sandbox kernel): mock-01 is
// refused as a whole, the reason names networkpolicy-001 and network-policy,
// nothing is substituted and no session is left.
//
// Design (cheapest honest): serving refuses an uncertified problem before it
// looks at capabilities, and networkpolicy-001 can never be certified on this
// host. So the test stores a stand-in certificate (current problem hash,
// grader version, environment hash, covering seeds) for each bank problem in
// its own temporary store. The stand-ins are NOT validation evidence: they
// only take the certificate check out of the way so that the refusal under
// test is the capability one. Real certification of the 19 other problems is
// the full bank run; a positive full-mock start is NOT RUN here (Task 19, on
// an enforcing host).
func TestMockServing(t *testing.T) {
	if !unsupportedCapabilities(t)[environmentNetworkPolicy] {
		t.Skip("this test needs a host whose doctor reports network-policy FAIL (JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES=network-policy)")
	}
	b := openBankRuntime(t)
	ctx := context.Background()
	testutil.NoError(t, author.PrepareEnvironment(ctx, b.m, b.id, b.cfg.Images))
	bank, err := filepath.Abs(bankRoot)
	testutil.NoError(t, err)
	svc := session.New(b.st, b.m, b.rt, bank, b.dir, session.WithWorkspace(t.TempDir()))
	mock := session.StartOptions{Mode: model.ModeMock, MockID: "mock-01", Seed: 1}

	// Control: the doctor really reports network-policy FAIL.
	report, err := svc.Doctor(ctx)
	testutil.NoError(t, err)
	for _, c := range report.Checks {
		t.Logf("DOCTOR %s %s: %s", c.Name, c.Status, c.Reason)
	}
	testutil.Equal(t, len(report.Unavailable([]string{"network-policy"})), 1)

	stored := 0
	certify := func(ids ...string) {
		t.Helper()
		for _, p := range loadGroup(t, ids) {
			seeds, err := author.CoveringSeeds(p)
			testutil.NoError(t, err)
			hash, err := problem.Hash(p)
			testutil.NoError(t, err)
			testutil.NoError(t, b.st.SaveValidation(ctx, model.ValidationRecord{
				ProblemHash: hash, GraderVersion: grader.GraderVersion, EnvironmentHash: b.rt.EnvironmentHash,
				Seeds: seeds, Passed: true, Cases: map[string]model.Evaluation{},
			}))
			stored++
		}
	}
	noSession := func() {
		t.Helper()
		active, err := b.st.ActiveSessions(ctx, b.id.ID)
		testutil.NoError(t, err)
		testutil.Equal(t, len(active), 0)
	}
	var others []string
	for _, id := range mock01Order {
		if id != "networkpolicy-001" {
			others = append(others, id)
		}
	}
	testutil.Equal(t, len(others), 19)
	certify(others...)

	// Stage A, the real state of this host: networkpolicy-001 cannot be
	// certified, so the mock is refused for that. Nothing else is named.
	t.Run("RefusedUncertified", func(t *testing.T) {
		_, _, err := svc.PrepareWithReport(ctx, mock)
		if err == nil {
			t.Fatal("mock-01 was prepared although networkpolicy-001 has no validation")
		}
		t.Logf("REFUSAL (uncertified): %v", err)
		testutil.ErrorIs(t, err, session.ErrNotCertified)
		if !strings.Contains(err.Error(), "networkpolicy-001") {
			t.Errorf("refusal does not name networkpolicy-001: %v", err)
		}
		for _, id := range others {
			if strings.Contains(err.Error(), id) {
				t.Errorf("refusal names %s, which is certified: %v", id, err)
			}
		}
		noSession()
	})

	// Stage B: with a stand-in certificate the refusal must be the capability
	// gate. A certificate never overrides a failing capability.
	certify("networkpolicy-001")
	testutil.Equal(t, stored, 20)
	t.Run("RefusedCapability", func(t *testing.T) {
		_, _, err := svc.PrepareWithReport(ctx, mock)
		if err == nil {
			t.Fatal("mock-01 was prepared although network-policy is not available")
		}
		t.Logf("REFUSAL (capability): %v", err)
		testutil.ErrorIs(t, err, session.ErrCapabilityUnavailable)
		if errors.Is(err, session.ErrNotCertified) {
			t.Errorf("the refusal is a certification one, not the capability gate: %v", err)
		}
		for _, want := range []string{"networkpolicy-001", "needs network-policy", "FAIL"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal lacks %q: %v", want, err)
			}
		}
		// The other 19 are certified and their capabilities PASS (ingress is
		// served once the platform is up), so none is named: the whole mock
		// is refused for the one problem, and nothing was substituted.
		for _, id := range others {
			if strings.Contains(err.Error(), id) {
				t.Errorf("refusal names %s, whose certificate and capabilities are fine: %v", id, err)
			}
		}
		noSession()
	})

	// The real CLI: the default `ckad start` is refused with exit code 3 and
	// prepares nothing. (The CLI prints a generic capability message; the
	// reason with the problem and capability names is the service error above.)
	t.Setenv("CKAD_HOST_COMPAT", os.Getenv("JUNKLOUD_TEST_HOST_COMPAT"))
	run := func(args ...string) (int, string, string) {
		var out, diag bytes.Buffer
		code := cli.Run(ctx, append([]string{"--state-dir", b.dir, "--bank-dir", bank, "--workspace", t.TempDir()}, args...), strings.NewReader("yes\n"), &out, &diag)
		t.Logf("CLI %v exit=%d stdout=%q stderr=%q", args, code, out.String(), diag.String())
		return code, out.String(), diag.String()
	}
	t.Run("CLIStartRefused", func(t *testing.T) {
		code, out, _ := run("start")
		testutil.Equal(t, code, 3)
		if strings.Contains(out, "준비 완료") || strings.Contains(out, "학습 시작") {
			t.Errorf("start went on after the refusal: %q", out)
		}
		noSession()
		code, _, _ = run("start", "mock-01")
		testutil.Equal(t, code, 3)
		noSession()
	})

	// ckad list: mock-01 is not offered, networkpolicy-001 is listed as not
	// servable, and the other 19 problems are offered.
	t.Run("CLIListUnavailable", func(t *testing.T) {
		code, out, _ := run("list")
		testutil.Equal(t, code, 0)
		var offered, excluded []string
		for _, line := range strings.Split(out, "\n") {
			switch {
			case strings.HasPrefix(line, "모의시험 mock-01"):
				t.Errorf("mock-01 is offered: %q", line)
			case strings.HasPrefix(line, "출제 불가: "):
				excluded = append(excluded, strings.Fields(strings.TrimPrefix(line, "출제 불가: "))[0])
			case strings.Contains(line, " | ") && !strings.HasPrefix(line, "모의시험"):
				offered = append(offered, strings.TrimSpace(strings.SplitN(line, " | ", 2)[0]))
			}
		}
		testutil.Equal(t, excluded, []string{"networkpolicy-001"})
		testutil.Equal(t, len(offered), 19)
		noSession()
	})
}

const environmentNetworkPolicy = "network-policy"
