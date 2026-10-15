//go:build linux

package environment

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// Doctor never probes anything before the identity check passes: without a
// recorded environment, or with someone else's identity, it fails with no
// command run.
func TestDoctorRequiresIdentity(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeRunner{}
	m := New(dir, openTestStore(t, dir), WithRunner(fake))
	_, err := m.Doctor(bg, model.Identity{ID: "0123456789ab"})
	testutil.ErrorIs(t, err, ErrNoEnvironment)
	testutil.Equal(t, len(fake.Calls()), 0)

	// Someone else's identity: refused by the local comparison, before any
	// Docker or API call.
	m, recorded, api, fake := newLiveManager(t, func(model.Identity, *liveSetup) {})
	other := recorded
	other.ID = "someone-else"
	_, err = m.Doctor(bg, other)
	testutil.ErrorIs(t, err, ErrIdentityMismatch)
	testutil.Equal(t, len(fake.Calls()), 0)
	testutil.Equal(t, len(api.Requests()), 0)

	// The recorded identity no longer matches the live cluster (marker
	// replaced): refused after read-only checks, with nothing created.
	m, recorded, api, _ = newLiveManager(t, func(_ model.Identity, s *liveSetup) { s.markerUID = "uid-theirs" })
	_, err = m.Doctor(bg, recorded)
	testutil.ErrorIs(t, err, ErrIdentityMismatch)
	testutil.Equal(t, api.writes(), 0)
	for _, r := range api.Requests() {
		if strings.Contains(r, PlatformNamespace) {
			t.Fatalf("doctor touched the probe namespace of an unverified cluster: %s", r)
		}
	}
}

// routingServer imitates an Ingress data plane: only host/path is routed.
func routingServer(t *testing.T, host, prefix, token string, anyHost bool) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (anyHost || r.Host == host) && strings.HasPrefix(r.URL.Path, prefix) {
			_, _ = w.Write([]byte(token + "\n"))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestCheckIngressRouting(t *testing.T) {
	client := probeHTTPClient()
	addr := routingServer(t, "probe-r1.junkloud.test", "/probe", "tok-r1", false)
	testutil.NoError(t, checkIngressRouting(bg, client, addr, "probe-r1.junkloud.test", "/probe/", "tok-r1", 5*time.Second))

	// Wrong-host requests must not be routed to the backend.
	loose := routingServer(t, "probe-r1.junkloud.test", "/probe", "tok-r1", true)
	err := checkIngressRouting(bg, client, loose, "probe-r1.junkloud.test", "/probe/", "tok-r1", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "host") {
		t.Fatalf("host-ignoring router passed: %v", err)
	}
	// Wrong-path requests must not be routed either.
	anyPath := routingServer(t, "probe-r1.junkloud.test", "/", "tok-r1", false)
	err = checkIngressRouting(bg, client, anyPath, "probe-r1.junkloud.test", "/probe/", "tok-r1", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("path-ignoring router passed: %v", err)
	}
	// A backend that never answers with the token fails within the deadline.
	start := time.Now()
	err = checkIngressRouting(bg, client, addr, "probe-r1.junkloud.test", "/probe/", "other-token", 2*time.Second)
	if err == nil {
		t.Fatal("wrong token accepted")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("routing check overran its deadline: %v", time.Since(start))
	}
}

// The probe HTTP client never uses the environment's HTTP proxy settings:
// Ingress addresses are on the local kind network.
func TestProbeHTTPClientIgnoresProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	addr := routingServer(t, "h.junkloud.test", "/", "tok", false)
	status, body, err := ingressRequest(bg, probeHTTPClient(), addr, "h.junkloud.test", "/")
	testutil.NoError(t, err)
	testutil.Equal(t, status, 200)
	testutil.Equal(t, strings.TrimSpace(body), "tok")
}

// Probe pods only ever run the preloaded probe image, never pull, carry the
// run label for cleanup, get no API credentials and are bounded.
func TestProbePodSpec(t *testing.T) {
	p := probePod("0123456789ab", "r1", "probe-dns-r1", map[string]string{"x": "y"}, "echo hi")
	testutil.Equal(t, p.Namespace, PlatformNamespace)
	testutil.Equal(t, p.Labels[labelProbeRun], "r1")
	testutil.Equal(t, p.Labels[LabelEnvironmentID], "0123456789ab")
	testutil.Equal(t, p.Labels[LabelComponent], ComponentPlatform)
	testutil.Equal(t, p.Labels["x"], "y")
	testutil.Equal(t, p.Spec.RestartPolicy, corev1.RestartPolicyNever)
	testutil.Equal(t, *p.Spec.AutomountServiceAccountToken, false)
	if p.Spec.ActiveDeadlineSeconds == nil || *p.Spec.ActiveDeadlineSeconds > 120 {
		t.Fatalf("probe pod has no tight deadline: %v", p.Spec.ActiveDeadlineSeconds)
	}
	c := p.Spec.Containers[0]
	testutil.Equal(t, c.Image, ProbeImage.Name)
	testutil.Equal(t, c.ImagePullPolicy, corev1.PullNever)
	if c.Resources.Limits.Memory().IsZero() || c.Resources.Limits.Cpu().IsZero() {
		t.Fatal("probe container has no limits")
	}
	testutil.Equal(t, *c.SecurityContext.AllowPrivilegeEscalation, false)
}

func TestPodOutcome(t *testing.T) {
	done := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: "tok\n"}},
	}}}}
	out, finished, err := podOutcome(done)
	testutil.NoError(t, err)
	testutil.Equal(t, finished, true)
	testutil.Equal(t, out.exitCode, 0)
	testutil.Equal(t, out.message, "tok")

	pulling := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImageNeverPull", Message: "not present"}},
	}}}}
	_, finished, err = podOutcome(pulling)
	testutil.Equal(t, finished, true)
	if err == nil || !errors.Is(err, errProbeImageMissing) {
		t.Fatalf("missing probe image not reported: %v", err)
	}
}
