//go:build integration

package integration

import (
	"context"
	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/testutil"
	"testing"
)

// Run separately on frozen source before spending time on every seed/case.
// Real setup -> solution transitions use the actual NewRuntime grader and
// native author executor, without issuing or fabricating any certificates.
func TestDeploymentBankSmoke(t *testing.T) {
	b := openBankRuntime(t)
	testutil.NoError(t, author.PrepareEnvironment(context.Background(), b.m, b.id, b.cfg.Images))
	smokeBank(t, b, loadGroup(t, deploymentIDs))
}

func TestDeploymentBank(t *testing.T) {
	validateGroupAfterSmoke(t, loadGroup(t, deploymentIDs), false)
}
