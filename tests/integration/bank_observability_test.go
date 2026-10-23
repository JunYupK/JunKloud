//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// Run separately on frozen source before spending time on every seed/case.
func TestObservabilityBankSmoke(t *testing.T) {
	b := openBankRuntime(t)
	testutil.NoError(t, author.PrepareEnvironment(context.Background(), b.m, b.id, b.cfg.Images))
	smokeBank(t, b, loadGroup(t, observabilityIDs))
}

func TestObservabilityBank(t *testing.T) {
	validateGroupAfterSmoke(t, loadGroup(t, observabilityIDs), false)
}
