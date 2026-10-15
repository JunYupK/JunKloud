//go:build linux

package environment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/runtime/platform"
)

// DoctorReport is the verified state of every capability of one
// environment, plus the versions in use. It never contains anything read
// from a problem or exam namespace.
type DoctorReport struct {
	EnvironmentID string            `json:"environment_id"`
	Checks        []CapabilityCheck `json:"checks"`
	Versions      map[string]string `json:"versions"`
}

// Unavailable returns the capabilities in required that did not PASS (an
// unknown name is unavailable too), in order. A problem whose required
// capabilities are not all available must not be served.
func (r DoctorReport) Unavailable(required []string) []string {
	var out []string
	for _, name := range required {
		pass := slices.ContainsFunc(r.Checks, func(c CapabilityCheck) bool { return c.Name == name && c.Status == StatusPass })
		if !pass && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// Probe limits (spec: network operations 3s, at most 3 attempts; state
// convergence 30s by default).
const (
	probeNetTimeout  = 3 * time.Second
	probeNetAttempts = 3
	probeConverge    = 30 * time.Second
	// probePodTimeout bounds one probe pod: start-up plus its network
	// attempts.
	probePodTimeout = probeConverge + probeNetAttempts*(probeNetTimeout+time.Second)
	probeCleanup    = time.Minute
	buildTimeout    = 3 * time.Minute
	probeHTTPPort   = 8080
)

// Doctor verifies id exactly as Check does and then probes every capability
// with short-lived objects in the platform namespace (and, for image-build,
// one throwaway image). It reads nothing outside the platform namespace,
// the marker and kube-system, and never any problem namespace. A failing
// capability is reported as FAIL with a reason, not as an error; an error
// means the doctor could not run (identity mismatch, API unavailable). Every
// probe object is removed before Doctor returns, also on failure or
// cancellation, together with leftovers of an interrupted earlier run.
// Callers hold the environment lock.
func (m *Manager) Doctor(ctx context.Context, id model.Identity) (DoctorReport, error) {
	live, cl, err := m.verify(ctx, id)
	if err != nil {
		return DoctorReport{}, err
	}
	report := DoctorReport{EnvironmentID: id.ID, Versions: map[string]string{}}
	for k, v := range live.Versions {
		report.Versions[k] = v
	}
	report.Versions["cloud-provider-kind"] = CloudProviderKindImage.Ref()
	report.Versions["envoy-proxy"] = EnvoyProxyImage.Ref()
	report.Versions["metrics-server"] = MetricsServerImage.Ref()
	report.Versions["probe-image"] = ProbeImage.Ref()

	if _, err := ensurePlatformNamespace(ctx, cl, id.ID); err != nil {
		return report, err
	}
	runID, err := newID()
	if err != nil {
		return report, err
	}
	p := &prober{m: m, cl: cl, id: id, run: runID[:8], token: "junkloud-probe-" + runID}
	cleanupCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), probeCleanup)
	}
	cctx, cancel := cleanupCtx()
	p.cleanup(cctx, "") // leftovers of an interrupted run
	cancel()
	defer func() {
		cctx, cancel := cleanupCtx()
		defer cancel()
		p.cleanup(cctx, p.run)
	}()

	serverErr := p.startServer(ctx, "probe-web-"+p.run)
	probes := map[string]func(context.Context) error{
		CapabilityDNS:           func(ctx context.Context) error { return needServer(serverErr, p.checkDNS, ctx) },
		CapabilityService:       func(ctx context.Context) error { return needServer(serverErr, p.checkService, ctx) },
		CapabilityIngress:       func(ctx context.Context) error { return needServer(serverErr, p.checkIngress, ctx) },
		CapabilityNetworkPolicy: p.checkNetworkPolicy,
		CapabilityStorage:       p.checkStorage,
		CapabilityMetrics:       p.checkMetrics,
		CapabilityImageBuild:    p.checkImageBuild,
	}
	results := make([]error, len(Capabilities))
	var wg sync.WaitGroup
	for i, name := range Capabilities {
		wg.Go(func() { results[i] = probes[name](ctx) })
	}
	wg.Wait()
	for i, name := range Capabilities {
		c := CapabilityCheck{Name: name, Status: StatusPass}
		if results[i] != nil {
			c.Status, c.Reason = StatusFail, results[i].Error()
		}
		report.Checks = append(report.Checks, c)
	}
	return report, nil
}

func needServer(serverErr error, check func(context.Context) error, ctx context.Context) error {
	if serverErr != nil {
		return fmt.Errorf("probe server did not start: %w", serverErr)
	}
	return check(ctx)
}

// prober runs one doctor run's probes. Every object it creates is named
// with the run ID and labeled LabelProbeRun=<run>.
type prober struct {
	m     *Manager
	cl    *clients
	id    model.Identity
	run   string
	token string

	serviceIP string
}

const probeServiceName = "probe-web"

func (p *prober) labels(extra map[string]string) map[string]string {
	l := platformLabels(p.id.ID, ComponentPlatform)
	l[labelProbeRun] = p.run
	for k, v := range extra {
		l[k] = v
	}
	return l
}

// probePod is a bounded, unprivileged pod running script in the preloaded
// probe image. Its output goes to the termination message.
func probePod(envID, run, name string, labels map[string]string, script string) *corev1.Pod {
	l := platformLabels(envID, ComponentPlatform)
	l[labelProbeRun] = run
	for k, v := range labels {
		l[k] = v
	}
	f, t := false, true
	user := int64(65534)
	grace := int64(1)
	deadline := int64(probePodTimeout / time.Second)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: PlatformNamespace, Labels: l},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			AutomountServiceAccountToken:  &f,
			EnableServiceLinks:            &f,
			TerminationGracePeriodSeconds: &grace,
			ActiveDeadlineSeconds:         &deadline,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &t, RunAsUser: &user, RunAsGroup: &user,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            "probe",
				Image:           ProbeImage.Name,
				ImagePullPolicy: corev1.PullNever,
				Command:         []string{"sh", "-c", script},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &f,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				TerminationMessagePolicy: corev1.TerminationMessageReadFile,
			}},
		},
	}
}

// clientScript wraps body (which must exit 0 on success) so that all output
// becomes the termination message.
func clientScript(body string) string {
	return "exec >/dev/termination-log 2>&1\n" + body
}

// startServer runs the HTTP probe server (serving the run token at / and
// /probe/) behind the probe Service and waits until it is ready.
func (p *prober) startServer(ctx context.Context, name string) error {
	if err := p.createServerPod(ctx, name); err != nil {
		return err
	}
	svc, err := p.cl.kube.CoreV1().Services(PlatformNamespace).Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: probeServiceName + "-" + p.run, Namespace: PlatformNamespace, Labels: p.labels(nil)},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": name},
			Ports:    []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt32(probeHTTPPort)}},
		},
	}, metav1.CreateOptions{FieldManager: fieldManager})
	if err != nil {
		return apiError("create probe service", err)
	}
	p.serviceIP = svc.Spec.ClusterIP
	return p.waitReady(ctx, name)
}

func (p *prober) createServerPod(ctx context.Context, name string) error {
	script := fmt.Sprintf("mkdir -p /tmp/www/probe && printf '%%s' %[1]s > /tmp/www/index.html && printf '%%s' %[1]s > /tmp/www/probe/index.html && exec httpd -f -p %[2]d -h /tmp/www",
		p.token, probeHTTPPort)
	pod := probePod(p.id.ID, p.run, name, map[string]string{"app": name}, script)
	pod.Spec.ActiveDeadlineSeconds = nil // serves until the run is cleaned up
	pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{Name: "http", ContainerPort: probeHTTPPort}}
	pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
		ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(probeHTTPPort)}},
		PeriodSeconds: 1,
	}
	if _, err := p.cl.kube.CoreV1().Pods(PlatformNamespace).Create(ctx, pod, metav1.CreateOptions{FieldManager: fieldManager}); err != nil {
		return apiError("create probe server", err)
	}
	return nil
}

// waitReady waits until the pod is Ready.
func (p *prober) waitReady(ctx context.Context, name string) error {
	var last string
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, probeConverge, true, func(ctx context.Context) (bool, error) {
		pod, err := p.cl.kube.CoreV1().Pods(PlatformNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			last = err.Error()
			return false, nil
		}
		if _, done, err := podOutcome(pod); done {
			if err == nil {
				err = fmt.Errorf("exited")
			}
			return false, err
		}
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				return true, nil
			}
		}
		last = "phase " + string(pod.Status.Phase)
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("pod %s not ready within %s: %v (%s)", name, probeConverge, err, last)
	}
	return nil
}

var errProbeImageMissing = errors.New("probe image is not loaded on the node (run the platform setup)")

// probeOutcome is how a finished probe pod ended.
type probeOutcome struct {
	exitCode int
	message  string
}

// podOutcome reports whether a probe pod has finished (or can never run)
// and how it ended.
func podOutcome(p *corev1.Pod) (probeOutcome, bool, error) {
	for _, cs := range p.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "ErrImageNeverPull", "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
				return probeOutcome{}, true, fmt.Errorf("%w: %s %s", errProbeImageMissing, w.Reason, w.Message)
			case "CreateContainerConfigError", "CreateContainerError", "RunContainerError":
				return probeOutcome{}, true, fmt.Errorf("container cannot start: %s %s", w.Reason, w.Message)
			}
		}
	}
	if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
		return probeOutcome{}, false, nil
	}
	for _, cs := range p.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil {
			return probeOutcome{exitCode: int(t.ExitCode), message: strings.TrimSpace(t.Message)}, true, nil
		}
	}
	return probeOutcome{exitCode: -1, message: strings.TrimSpace(p.Status.Reason + " " + p.Status.Message)}, true, nil
}

// runPod creates a probe pod and waits until it has finished.
func (p *prober) runPod(ctx context.Context, pod *corev1.Pod) (probeOutcome, error) {
	if _, err := p.cl.kube.CoreV1().Pods(PlatformNamespace).Create(ctx, pod, metav1.CreateOptions{FieldManager: fieldManager}); err != nil {
		return probeOutcome{}, apiError("create probe pod "+pod.Name, err)
	}
	var out probeOutcome
	var last string
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, probePodTimeout, true, func(ctx context.Context) (bool, error) {
		cur, err := p.cl.kube.CoreV1().Pods(PlatformNamespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			last = err.Error()
			return false, nil
		}
		o, done, err := podOutcome(cur)
		if err != nil {
			return false, err
		}
		last = "phase " + string(cur.Status.Phase)
		out = o
		return done, nil
	})
	if err != nil {
		if errors.Is(err, errProbeImageMissing) {
			return out, err
		}
		return out, fmt.Errorf("probe pod %s did not finish within %s: %v (%s)", pod.Name, probePodTimeout, err, last)
	}
	return out, nil
}

// runClient runs a client probe script; it fails unless the script exits 0.
func (p *prober) runClient(ctx context.Context, name string, labels map[string]string, body string) (string, error) {
	out, err := p.runPod(ctx, probePod(p.id.ID, p.run, name+"-"+p.run, labels, clientScript(body)))
	if err != nil {
		return "", err
	}
	if out.exitCode != 0 {
		return out.message, fmt.Errorf("%s (exit %d)", tail(out.message), out.exitCode)
	}
	return out.message, nil
}

func tail(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 240 {
		s = "…" + s[len(s)-240:]
	}
	return s
}

// fetchScript fetches url up to probeNetAttempts times (probeNetTimeout
// each); it exits 0 as soon as the body is exactly the token.
func (p *prober) fetchScript(url string) string {
	return fmt.Sprintf(`i=0
while [ $i -lt %[1]d ]; do
  i=$((i+1))
  body=$(wget -T %[2]d -q -O - %[3]s 2>&1) && [ "$body" = %[4]s ] && { echo "reached %[3]s"; exit 0; }
  sleep 1
done
echo "could not fetch %[3]s: $body"
exit 1`, probeNetAttempts, int(probeNetTimeout/time.Second), url, p.token)
}

// checkDNS: the probe Service's cluster DNS name resolves to its ClusterIP.
func (p *prober) checkDNS(ctx context.Context) error {
	name := probeServiceName + "-" + p.run + "." + PlatformNamespace + ".svc.cluster.local"
	_, err := p.runClient(ctx, "probe-dns", nil, fmt.Sprintf(`i=0
while [ $i -lt %[1]d ]; do
  i=$((i+1))
  out=$(timeout %[2]d nslookup %[3]s 2>&1) && echo "$out" | grep -q "^Address: *%[4]s$" && { echo "%[3]s -> %[4]s"; exit 0; }
  sleep 1
done
echo "cannot resolve %[3]s to %[4]s: $out"
exit 1`, probeNetAttempts, int(probeNetTimeout/time.Second), name, p.serviceIP))
	if err != nil {
		return fmt.Errorf("cluster DNS: %w", err)
	}
	return nil
}

// checkService: a pod reaches the probe server through its Service.
func (p *prober) checkService(ctx context.Context) error {
	_, err := p.runClient(ctx, "probe-svc", nil, p.fetchScript("http://"+net.JoinHostPort(p.serviceIP, "80")+"/"))
	if err != nil {
		return fmt.Errorf("Service ClusterIP: %w", err)
	}
	return nil
}

// checkNetworkPolicy: with a policy that admits only labeled clients to a
// dedicated server, a labeled client gets through (positive) and an
// unlabeled one does not (negative).
func (p *prober) checkNetworkPolicy(ctx context.Context) error {
	server := "probe-np-" + p.run
	allowLabel := "junkloud.io/probe-access"
	_, err := p.cl.kube.NetworkingV1().NetworkPolicies(PlatformNamespace).Create(ctx, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: server, Namespace: PlatformNamespace, Labels: p.labels(nil)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": server}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{allowLabel: p.run}},
			}}}},
		},
	}, metav1.CreateOptions{FieldManager: fieldManager})
	if err != nil {
		return apiError("create probe network policy", err)
	}
	if err := p.createServerPod(ctx, server); err != nil {
		return err
	}
	if err := p.waitReady(ctx, server); err != nil {
		return fmt.Errorf("probe server: %w", err)
	}
	pod, err := p.cl.kube.CoreV1().Pods(PlatformNamespace).Get(ctx, server, metav1.GetOptions{})
	if err != nil {
		return apiError("read probe server", err)
	}
	url := "http://" + net.JoinHostPort(pod.Status.PodIP, fmt.Sprint(probeHTTPPort)) + "/"
	if _, err := p.runClient(ctx, "probe-np-allowed", map[string]string{allowLabel: p.run}, p.fetchScript(url)); err != nil {
		return fmt.Errorf("allowed client blocked: %w", err)
	}
	msg, err := p.runClient(ctx, "probe-np-denied", nil, fmt.Sprintf(`i=0
while [ $i -lt %[1]d ]; do
  i=$((i+1))
  if wget -T %[2]d -q -O /dev/null %[3]s 2>/dev/null; then echo "reached %[3]s"; exit 1; fi
  sleep 1
done
echo blocked`, probeNetAttempts, int(probeNetTimeout/time.Second), url))
	if err != nil {
		if strings.Contains(msg, "reached") {
			return fmt.Errorf("a pod not admitted by the NetworkPolicy reached the server: NetworkPolicy is not enforced")
		}
		return fmt.Errorf("denied client: %w", err)
	}
	return nil
}

// checkIngress: through cloud-provider-kind, an Ingress routes its host and
// path to the probe server, and neither another host nor another path
// reaches it. The request is made from the doctor's own process, as a
// learner's curl would be.
func (p *prober) checkIngress(ctx context.Context) error {
	c, found, err := p.m.inspectHostContainer(ctx, CloudProviderContainerName(p.id.ID))
	switch {
	case err != nil:
		return err
	case !found || !cloudProviderOwned(c, p.id.ID):
		return fmt.Errorf("cloud-provider-kind is not installed for this environment (run the platform setup)")
	case !c.Running:
		return fmt.Errorf("cloud-provider-kind container %s is not running (run the platform setup to restart it)", CloudProviderContainerName(p.id.ID))
	}
	class, err := p.cl.kube.NetworkingV1().IngressClasses().Get(ctx, cloudProviderClass, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("IngressClass %s: %w", cloudProviderClass, apiError("read ingress class", err))
	}
	if class.Annotations[networkingv1.AnnotationIsDefaultIngressClass] != "true" {
		return fmt.Errorf("IngressClass %s is not the default class", cloudProviderClass)
	}
	host := "probe-" + p.run + ".junkloud.test"
	prefix := networkingv1.PathTypePrefix
	name := "probe-" + p.run
	_, err = p.cl.kube.NetworkingV1().Ingresses(PlatformNamespace).Create(ctx, &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: PlatformNamespace, Labels: p.labels(nil)},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			Host: host,
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
				Path: "/probe", PathType: &prefix,
				Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
					Name: probeServiceName + "-" + p.run, Port: networkingv1.ServiceBackendPort{Number: 80},
				}},
			}}}},
		}}},
	}, metav1.CreateOptions{FieldManager: fieldManager})
	if err != nil {
		return apiError("create probe ingress", err)
	}
	var addr string
	err = wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, probeConverge, true, func(ctx context.Context) (bool, error) {
		ing, err := p.cl.kube.NetworkingV1().Ingresses(PlatformNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		for _, lb := range ing.Status.LoadBalancer.Ingress {
			if lb.IP != "" {
				addr = lb.IP
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("Ingress got no address within %s%s", probeConverge, p.loadBalancerDiagnosis(ctx))
	}
	return checkIngressRouting(ctx, probeHTTPClient(), addr, host, "/probe/", p.token, probeConverge)
}

// loadBalancerDiagnosis names the load-balancer containers
// cloud-provider-kind made for this cluster that are not running, with
// Docker's start error (for example a sysctl the engine's kernel rejects).
func (p *prober) loadBalancerDiagnosis(ctx context.Context) string {
	out, err := p.m.output(ctx, dockerTimeout, "docker", "ps", "--all", "--quiet", "--no-trunc", "--filter", "label="+cloudProviderLBLabel+"="+p.id.ClusterName)
	if err != nil {
		return ""
	}
	var notes []string
	for _, cid := range parseIDs(out) {
		if !containerIDPattern.MatchString(cid) {
			continue
		}
		if c, ok, err := p.m.inspectHostContainer(ctx, cid); err == nil && ok && !c.Running {
			notes = append(notes, fmt.Sprintf("load balancer container %s is not running: %s", c.Name, tail(c.Error)))
		}
	}
	if len(notes) == 0 {
		return ""
	}
	return " (" + strings.Join(notes, "; ") + ")"
}

// probeHTTPClient makes single, bounded HTTP requests directly (never
// through an HTTP proxy from the environment, never following redirects).
func probeHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: probeNetTimeout}).DialContext,
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: probeNetTimeout,
		},
		Timeout:       probeNetTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// ingressRequest sends GET http://addr/path with the given Host header.
func ingressRequest(ctx context.Context, c *http.Client, addr, host, path string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return 0, "", err
	}
	req.Host = host
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, string(body), err
}

// checkIngressRouting requires host+path to reach the backend serving token
// within wait, and then requests for another host and for another path to
// get an HTTP answer that is not the backend's.
func checkIngressRouting(ctx context.Context, c *http.Client, addr, host, path, token string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		status, body, err := ingressRequest(ctx, c, addr, host, path)
		if err == nil && status == http.StatusOK && strings.TrimSpace(body) == token {
			break
		}
		if time.Now().After(deadline) {
			last := fmt.Sprintf("HTTP %d %q", status, tail(body))
			if err != nil {
				last = err.Error()
			}
			return fmt.Errorf("http://%s%s with host %s was not routed to the probe backend within %s (last: %s)", addr, path, host, wait, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	negative := func(what, h, pth string) error {
		var status int
		var body string
		var err error
		for i := 0; i < probeNetAttempts; i++ {
			if status, body, err = ingressRequest(ctx, c, addr, h, pth); err == nil {
				break
			}
		}
		switch {
		case err != nil:
			return fmt.Errorf("request for another %s (%s%s) got no HTTP answer: %v", what, h, pth, err)
		case strings.Contains(body, token):
			return fmt.Errorf("request for another %s (%s%s) reached the backend (HTTP %d): %s rules are not applied", what, h, pth, status, what)
		}
		return nil
	}
	if err := negative("host", "other-"+host, path); err != nil {
		return err
	}
	return negative("path", host, "/not"+path)
}

// checkStorage: data a pod writes to a PVC from the default StorageClass is
// read back by a new pod after the writer is gone.
func (p *prober) checkStorage(ctx context.Context) error {
	claim := "probe-data-" + p.run
	_, err := p.cl.kube.CoreV1().PersistentVolumeClaims(PlatformNamespace).Create(ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: claim, Namespace: PlatformNamespace, Labels: p.labels(nil)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("16Mi")}},
		},
	}, metav1.CreateOptions{FieldManager: fieldManager})
	if err != nil {
		return apiError("create probe claim", err)
	}
	withClaim := func(pod *corev1.Pod) *corev1.Pod {
		pod.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
		}}}
		pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
		return pod
	}
	writer := withClaim(probePod(p.id.ID, p.run, "probe-pvc-write-"+p.run, nil,
		clientScript(fmt.Sprintf("printf '%%s' %s > /data/probe && sync && echo written", p.token))))
	out, err := p.runPod(ctx, writer)
	if err == nil && out.exitCode != 0 {
		err = fmt.Errorf("%s (exit %d)", tail(out.message), out.exitCode)
	}
	if err != nil {
		return fmt.Errorf("PVC write: %w%s", err, p.claimState(ctx, claim))
	}
	if err := p.deletePodAndWait(ctx, writer.Name); err != nil {
		return err
	}
	reader := withClaim(probePod(p.id.ID, p.run, "probe-pvc-read-"+p.run, nil, clientScript("cat /data/probe")))
	out, err = p.runPod(ctx, reader)
	if err != nil {
		return fmt.Errorf("PVC read: %w", err)
	}
	if out.exitCode != 0 || out.message != p.token {
		return fmt.Errorf("PVC read in a new pod returned %q (exit %d), want the written data", tail(out.message), out.exitCode)
	}
	return nil
}

func (p *prober) claimState(ctx context.Context, claim string) string {
	pvc, err := p.cl.kube.CoreV1().PersistentVolumeClaims(PlatformNamespace).Get(ctx, claim, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return fmt.Sprintf(" (claim %s is %s)", claim, pvc.Status.Phase)
}

func (p *prober) deletePodAndWait(ctx context.Context, name string) error {
	pods := p.cl.kube.CoreV1().Pods(PlatformNamespace)
	if err := pods.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return apiError("delete probe pod "+name, err)
	}
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, probeConverge, true, func(ctx context.Context) (bool, error) {
		_, err := pods.Get(ctx, name, metav1.GetOptions{})
		return apierrors.IsNotFound(err), nil
	})
	if err != nil {
		return fmt.Errorf("probe pod %s was not deleted within %s", name, probeConverge)
	}
	return nil
}

// checkMetrics: the metrics API is available and reports usage for every
// node and for the metrics-server pod (what `kubectl top` reads).
func (p *prober) checkMetrics(ctx context.Context) error {
	svc, err := p.cl.dyn.Resource(apiServiceGVR).Get(ctx, metricsAPIService, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("metrics API %s is not installed (run the platform setup)", metricsAPIService)
	}
	if err != nil {
		return apiError("read APIService", err)
	}
	if !apiServiceAvailable(svc) {
		return fmt.Errorf("metrics API %s is not available", metricsAPIService)
	}
	nodes, err := ownNodes(ctx, p.cl, p.id.ID)
	if err != nil {
		return err
	}
	rc := p.cl.kube.Discovery().RESTClient()
	var last error
	err = wait.PollUntilContextTimeout(ctx, time.Second, probeConverge, true, func(ctx context.Context) (bool, error) {
		for name := range nodes {
			raw, err := rc.Get().AbsPath("/apis/metrics.k8s.io/v1beta1/nodes", name).Do(ctx).Raw()
			if err != nil {
				last = fmt.Errorf("node %s: %v", name, err)
				return false, nil
			}
			var nm struct {
				Usage map[string]string `json:"usage"`
			}
			if json.Unmarshal(raw, &nm) != nil || nm.Usage["cpu"] == "" || nm.Usage["memory"] == "" {
				last = fmt.Errorf("node %s has no usage yet", name)
				return false, nil
			}
		}
		raw, err := rc.Get().AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces", metricsNamespace, "pods").
			Param("labelSelector", "k8s-app="+metricsName).Do(ctx).Raw()
		if err != nil {
			last = fmt.Errorf("pod metrics: %v", err)
			return false, nil
		}
		var pm struct {
			Items []struct {
				Containers []struct {
					Usage map[string]string `json:"usage"`
				} `json:"containers"`
			} `json:"items"`
		}
		if json.Unmarshal(raw, &pm) != nil || len(pm.Items) == 0 || len(pm.Items[0].Containers) == 0 {
			last = fmt.Errorf("no pod metrics yet")
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("metrics API did not report node and pod usage within %s: %v", probeConverge, last)
	}
	return nil
}

// checkImageBuild: an image built with the Docker CLI on the shared engine
// from the embedded probe context, loaded into the node, runs as a pod and
// reports the token baked in at build time. Only the image this run built
// is removed afterwards.
func (p *prober) checkImageBuild(ctx context.Context) error {
	arch, err := nodeArch(ctx, p.cl)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(p.m.dir, 0o700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(p.m.dir, ".image-build-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	df, err := fs.ReadFile(platform.FS, platform.ImageBuildProbeDir+"/Dockerfile")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), df, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "probe-token"), []byte(p.token), 0o644); err != nil {
		return err
	}
	tag := ImageBuildProbeRepository + ":" + p.id.ID + "-" + p.run
	defer p.removeBuiltImage(tag)
	if err := p.m.command(ctx, buildTimeout, "docker", "build", "--quiet", "--platform", "linux/"+arch, "--tag", tag, dir); err != nil {
		return fmt.Errorf("docker build: %w", err)
	}
	archive := filepath.Join(dir, "image.tar")
	if err := p.m.command(ctx, loadTimeout, "docker", "save", "--platform", "linux/"+arch, "--output", archive, tag); err != nil {
		return fmt.Errorf("docker save: %w", err)
	}
	if err := p.m.command(ctx, loadTimeout, "kind", "load", "image-archive", archive, "--name", p.id.ClusterName); err != nil {
		return fmt.Errorf("load into node: %w", err)
	}
	pod := probePod(p.id.ID, p.run, "probe-build-"+p.run, nil, "")
	pod.Spec.Containers[0].Image = tag
	pod.Spec.Containers[0].Command = nil
	out, err := p.runPod(ctx, pod)
	if err != nil {
		return fmt.Errorf("run built image: %w", err)
	}
	if out.exitCode != 0 || out.message != p.token {
		return fmt.Errorf("built image reported %q (exit %d), want its build token", tail(out.message), out.exitCode)
	}
	return nil
}

// removeBuiltImage removes the run's image tag from the engine and from the
// environment's nodes (best effort; nothing else is removed).
func (p *prober) removeBuiltImage(tag string) {
	ctx, cancel := context.WithTimeout(context.Background(), probeCleanup)
	defer cancel()
	_ = p.m.command(ctx, dockerTimeout, "docker", "image", "rm", tag)
	for _, node := range p.id.ContainerIDs {
		if containerIDPattern.MatchString(node) {
			_ = p.m.command(ctx, dockerTimeout, "docker", "exec", node, "crictl", "rmi", tag)
		}
	}
}

// cleanup deletes the probe objects of one run (run == "": of every run)
// from the platform namespace and waits, bounded, until they are gone.
func (p *prober) cleanup(ctx context.Context, run string) {
	sel := labelProbeRun
	if run != "" {
		sel = labelProbeRun + "=" + run
	}
	list := metav1.ListOptions{LabelSelector: sel}
	bg := metav1.DeletePropagationBackground
	grace := int64(0)
	opts := metav1.DeleteOptions{PropagationPolicy: &bg, GracePeriodSeconds: &grace}
	core, netv1 := p.cl.kube.CoreV1(), p.cl.kube.NetworkingV1()
	if l, err := netv1.Ingresses(PlatformNamespace).List(ctx, list); err == nil {
		for _, o := range l.Items {
			_ = netv1.Ingresses(PlatformNamespace).Delete(ctx, o.Name, opts)
		}
	}
	if l, err := netv1.NetworkPolicies(PlatformNamespace).List(ctx, list); err == nil {
		for _, o := range l.Items {
			_ = netv1.NetworkPolicies(PlatformNamespace).Delete(ctx, o.Name, opts)
		}
	}
	if l, err := core.Services(PlatformNamespace).List(ctx, list); err == nil {
		for _, o := range l.Items {
			_ = core.Services(PlatformNamespace).Delete(ctx, o.Name, opts)
		}
	}
	_ = core.Pods(PlatformNamespace).DeleteCollection(ctx, opts, list)
	if l, err := core.PersistentVolumeClaims(PlatformNamespace).List(ctx, list); err == nil {
		for _, o := range l.Items {
			_ = core.PersistentVolumeClaims(PlatformNamespace).Delete(ctx, o.Name, opts)
		}
	}
	_ = wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, probeConverge, true, func(ctx context.Context) (bool, error) {
		pods, err := core.Pods(PlatformNamespace).List(ctx, list)
		if err != nil || len(pods.Items) > 0 {
			return false, nil
		}
		pvcs, err := core.PersistentVolumeClaims(PlatformNamespace).List(ctx, list)
		return err == nil && len(pvcs.Items) == 0, nil
	})
}
