//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// Run separately on frozen source before spending time on every seed/case.
// Present-only until the last networking package lands (Task 4 validates all
// three IDs).
func TestNetworkBankSmoke(t *testing.T) {
	ids := presentNetworkIDs(t)
	if len(ids) == 0 {
		t.Skip("no networking package yet")
	}
	b := openBankRuntime(t)
	testutil.NoError(t, author.PrepareEnvironment(context.Background(), b.m, b.id, b.cfg.Images))
	smokeBank(t, b, loadGroup(t, ids))
}

// TestNetworkBank validates every present networking package on all its seeds
// and cases. A package whose capability this host lacks (network-policy on
// the sandbox kernel) is validated honestly: its solved stage fails here and
// it is reported as not certified, never edited to match.
func TestNetworkBank(t *testing.T) {
	ids := presentNetworkIDs(t)
	if len(ids) == 0 {
		t.Skip("no networking package yet")
	}
	validateGroupAfterSmoke(t, loadGroup(t, ids), false)
}
