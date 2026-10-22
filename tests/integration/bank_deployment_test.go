//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// Preserve exact owned fixture observations before normal test cleanup. These
// reads neither wait for convergence nor change grading or deletion authority.
func deploymentSmokeState(t *testing.T, b bankRuntime, ns string) {
	t.Helper()
	c := kubeClient(t, b.m, b.id)
	ctx := context.Background()
	services, err := c.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Logf("DIAGNOSTIC Service list: %v", err)
	} else {
		for _, s := range services.Items {
			raw, _ := json.Marshal(map[string]any{"name": s.Name, "uid": s.UID, "selector": s.Spec.Selector, "ports": s.Spec.Ports})
			t.Logf("DIAGNOSTIC Service %s", raw)
		}
	}
	slices, err := c.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Logf("DIAGNOSTIC Slice list: %v", err)
	} else {
		for _, s := range slices.Items {
			raw, _ := json.Marshal(s)
			t.Logf("DIAGNOSTIC Slice %s", raw)
		}
	}
	pods, err := c.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Logf("DIAGNOSTIC Pod list: %v", err)
	} else {
		for _, p := range pods.Items {
			raw, _ := json.Marshal(map[string]any{"name": p.Name, "uid": p.UID, "owners": p.OwnerReferences, "labels": p.Labels, "ip": p.Status.PodIP, "conditions": p.Status.Conditions, "containers": p.Spec.Containers, "statuses": p.Status.ContainerStatuses})
			t.Logf("DIAGNOSTIC Pod %s", raw)
		}
	}
}
