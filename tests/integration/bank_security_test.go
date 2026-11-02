//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// Run separately on frozen source before spending time on every seed/case.
func TestSecurityBankSmoke(t *testing.T) {
	ids := presentSecurityIDs(t)
	if len(ids) == 0 {
		t.Skip("no environment-security package yet")
	}
	b := openBankRuntime(t)
	testutil.NoError(t, author.PrepareEnvironment(context.Background(), b.m, b.id, b.cfg.Images))
	smokeBank(t, b, loadGroup(t, ids))
}

// Present-only until the last package lands (Task 7 validates all five IDs).
func TestSecurityBank(t *testing.T) {
	ids := presentSecurityIDs(t)
	if len(ids) == 0 {
		t.Skip("no environment-security package yet")
	}
	validateGroupAfterSmoke(t, loadGroup(t, ids), false)
}
