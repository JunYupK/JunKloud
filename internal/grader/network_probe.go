package grader

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// probeScript runs inside the probe pod (see probe.sh).
//
//go:embed probe.sh
var probeScript string

// Probe pod labels. Every probe pod carries them, so leftovers of a crashed
// grader can be found and removed by label.
const (
	LabelProbeComponent = "junkloud.io/component"
	ProbeComponent      = "grading-probe"
	LabelProbeEnv       = "junkloud.io/environment-id"
	LabelProbeProblem   = "junkloud.io/problem-id"
	LabelProbeRun       = "junkloud.io/probe-run"
	labelManagedBy      = "app.kubernetes.io/managed-by"
)

const (
	// podWait bounds how long the grader waits for one probe pod; the pod's
	// own activeDeadlineSeconds is the same bound on the cluster side.
	podWait         = 40 * time.Second
	podDeadlineSecs = 40
	probePoll       = 300 * time.Millisecond
	// unschedulableGrace is how long a pod may stay unschedulable before it
	// is called an environment fault (the scheduler retries on its own).
	unschedulableGrace = 15 * time.Second
	cleanupAttempts    = 15
	cleanupBudget      = 30 * time.Second
	cleanupPoll        = 200 * time.Millisecond
	maxWaitAPIErrors   = 5
)

// podClient is the part of the Kubernetes API the pod prober uses.
type podClient interface {
	Create(ctx context.Context, ns string, pod *corev1.Pod) (*corev1.Pod, error)
	Get(ctx context.Context, ns, name string) (*corev1.Pod, error)
	Delete(ctx context.Context, ns, name string) error
}

type kubePods struct{ cs kubernetes.Interface }

func (k kubePods) Create(ctx context.Context, ns string, pod *corev1.Pod) (*corev1.Pod, error) {
	return k.cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{FieldManager: "junkloud"})
}

func (k kubePods) Get(ctx context.Context, ns, name string) (*corev1.Pod, error) {
	return k.cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
}

func (k kubePods) Delete(ctx context.Context, ns, name string) error {
	zero := int64(0)
	return k.cs.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: &zero})
}

type podProbeRunner struct {
	pods podClient
	set  settings
}

// NewPodProbeRunner returns the ProbeRunner that runs every probe as a
// short-lived pod in the probe's source namespace, for the cluster that one
// explicit kubeconfig file and context point at. The pod carries the source
// labels (so the learner's NetworkPolicy selectors apply to it), no service
// account token, runs non-root with resource limits and a bounded
// activeDeadlineSeconds, reports through its termination message and is
// removed afterwards, even when ctx is cancelled.
//
// WithProbeImage is required: the pinned, preloaded probe image
// (environment.ProbeImage.Name). Probe pods never pull images; a node
// without the image makes the probe fail with ErrProbeFailed (ERROR), never
// a learner FAIL.
func NewPodProbeRunner(kubeconfigPath, contextName string, opts ...Option) (ProbeRunner, error) {
	cfg, err := restConfig(kubeconfigPath, contextName)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("grader: kubernetes client: %w", err)
	}
	return newPodProbeRunner(kubePods{cs}, opts...)
}

func newPodProbeRunner(pods podClient, opts ...Option) (*podProbeRunner, error) {
	set := newSettings(opts)
	if set.probeImage == "" {
		return nil, errors.New("grader: a pinned probe image is required (WithProbeImage)")
	}
	return &podProbeRunner{pods: pods, set: set}, nil
}

func probeFailed(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrProbeFailed, fmt.Sprintf(format, a...))
}

func validateProbe(req ProbeRequest) error {
	switch req.Protocol {
	case ProbeHTTP, ProbeTCP, ProbeDNS:
	default:
		return fmt.Errorf("unknown probe protocol %q", req.Protocol)
	}
	if req.Namespace == "" {
		return errors.New("probe has no namespace")
	}
	if req.Host == "" || strings.ContainsAny(req.Host, " \t\r\n") {
		return fmt.Errorf("bad probe host %q", req.Host)
	}
	if req.Protocol != ProbeDNS && (req.Port < 1 || req.Port > 65535) {
		return fmt.Errorf("bad probe port %d", req.Port)
	}
	if req.Attempts < 1 || req.Attempts > ProbeAttempts {
		return fmt.Errorf("attempts must be 1..%d, got %d", ProbeAttempts, req.Attempts)
	}
	if req.Timeout < time.Second || req.Timeout > ProbeAttemptTimeout {
		return fmt.Errorf("per-attempt timeout must be 1s..%s, got %s", ProbeAttemptTimeout, req.Timeout)
	}
	for k := range req.Labels {
		if strings.HasPrefix(k, reservedLabelNS) {
			return fmt.Errorf("label %q is reserved", k)
		}
	}
	if req.Protocol == ProbeHTTP {
		if !strings.HasPrefix(req.Path, "/") || strings.ContainsAny(req.Path+req.HTTPHost, "\r\n \t") {
			return errors.New("bad HTTP path or host")
		}
		if req.ExpectStatus < 100 || req.ExpectStatus > 599 {
			return fmt.Errorf("bad expected status %d", req.ExpectStatus)
		}
	}
	return nil
}

// Probe implements ProbeRunner.
func (r *podProbeRunner) Probe(ctx context.Context, req ProbeRequest) (res ProbeResult, err error) {
	if err := validateProbe(req); err != nil {
		return ProbeResult{}, probeFailed("%v", err)
	}
	id, err := randomID()
	if err != nil {
		return ProbeResult{}, probeFailed("random id: %v", err)
	}
	pod := r.buildPod("jk-probe-"+id, id, req)
	defer func() {
		// The pod may exist even if Create failed or was cancelled in
		// flight, so removal is always attempted, and with a context that
		// survives the caller's cancellation.
		if cerr := r.cleanup(ctx, req.Namespace, pod.Name); cerr != nil {
			cerr = probeFailed("could not remove probe pod %s/%s: %v", req.Namespace, pod.Name, cerr)
			res, err = ProbeResult{}, errors.Join(err, cerr)
		}
	}()

	if _, err := r.pods.Create(ctx, req.Namespace, pod); err != nil {
		switch {
		case ctx.Err() != nil:
			return ProbeResult{}, fmt.Errorf("create probe pod: %w", errors.Join(ctx.Err(), err))
		case apierrors.IsNotFound(err):
			return ProbeResult{}, probeFailed("namespace %q does not exist, so the source pod cannot run (reset the problem?): %v", req.Namespace, err)
		default:
			return ProbeResult{}, probeFailed("create probe pod in %s: %v", req.Namespace, err)
		}
	}
	return r.await(ctx, req, pod.Name)
}

// await polls the pod until it reports or an environment fault is clear.
func (r *podProbeRunner) await(ctx context.Context, req ProbeRequest, name string) (ProbeResult, error) {
	clock := r.set.clock
	start := clock.Now()
	apiErrors := 0
	for {
		if err := ctx.Err(); err != nil {
			return ProbeResult{}, err
		}
		pod, err := r.pods.Get(ctx, req.Namespace, name)
		switch {
		case err == nil:
			apiErrors = 0
			res, done, perr := r.outcome(pod, req, clock.Now().Sub(start))
			if perr != nil {
				return ProbeResult{}, perr
			}
			if done {
				return res, nil
			}
		case ctx.Err() != nil:
			return ProbeResult{}, ctx.Err()
		case apierrors.IsNotFound(err):
			return ProbeResult{}, probeFailed("probe pod %s/%s disappeared", req.Namespace, name)
		default:
			if apiErrors++; apiErrors > maxWaitAPIErrors {
				return ProbeResult{}, probeFailed("cannot read probe pod %s/%s: %v", req.Namespace, name, err)
			}
		}
		if clock.Now().Sub(start) >= podWait {
			return ProbeResult{}, probeFailed("probe pod %s/%s did not finish within %s", req.Namespace, name, podWait)
		}
		if err := clock.Sleep(ctx, probePoll); err != nil {
			return ProbeResult{}, err
		}
	}
}

// outcome reads a probe pod's state: not done yet, a result, or a probe
// fault. Nothing here blames the learner.
func (r *podProbeRunner) outcome(p *corev1.Pod, req ProbeRequest, waited time.Duration) (ProbeResult, bool, error) {
	for _, cs := range p.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "ErrImageNeverPull", "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
				return ProbeResult{}, false, probeFailed("probe image %s is not available on the node (it must be preloaded; probe pods never pull): %s %s",
					r.set.probeImage, w.Reason, w.Message)
			case "CreateContainerConfigError", "CreateContainerError", "RunContainerError":
				return ProbeResult{}, false, probeFailed("probe container cannot start: %s %s", w.Reason, w.Message)
			}
		}
	}
	switch p.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		for _, cs := range p.Status.ContainerStatuses {
			t := cs.State.Terminated
			if t == nil {
				continue
			}
			msg := strings.TrimSpace(t.Message)
			if msg == "" {
				return ProbeResult{}, false, probeFailed("probe exited with exit code %d without reporting (%s)", t.ExitCode, t.Reason)
			}
			res, err := parseProbeMessage(msg, req)
			if err != nil {
				return ProbeResult{}, false, probeFailed("unreadable probe report: %v", err)
			}
			return res, true, nil
		}
		return ProbeResult{}, false, probeFailed("probe pod ended as %s: %s %s", p.Status.Phase, p.Status.Reason, p.Status.Message)
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && waited >= unschedulableGrace {
			return ProbeResult{}, false, probeFailed("probe pod cannot be scheduled: %s %s", c.Reason, c.Message)
		}
	}
	return ProbeResult{}, false, nil
}

// cleanup removes the pod and confirms it is gone.
func (r *podProbeRunner) cleanup(ctx context.Context, ns, name string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupBudget)
	defer cancel()
	var last error
	for attempt := 0; attempt < cleanupAttempts; attempt++ {
		err := r.pods.Delete(ctx, ns, name)
		if err == nil || apierrors.IsNotFound(err) {
			_, gerr := r.pods.Get(ctx, ns, name)
			if apierrors.IsNotFound(gerr) {
				return nil
			}
			if gerr != nil {
				last = gerr
			} else {
				last = errors.New("pod still exists")
			}
		} else {
			last = err
		}
		if serr := r.set.clock.Sleep(ctx, cleanupPoll); serr != nil {
			return errors.Join(last, serr)
		}
	}
	return last
}

func randomID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (r *podProbeRunner) buildPod(name, run string, req ProbeRequest) *corev1.Pod {
	labels := map[string]string{}
	for k, v := range req.Labels {
		labels[k] = v
	}
	if _, set := labels[labelManagedBy]; !set {
		labels[labelManagedBy] = "junkloud"
	}
	labels[LabelProbeComponent] = ProbeComponent
	labels[LabelProbeRun] = run
	if req.EnvironmentID != "" {
		labels[LabelProbeEnv] = req.EnvironmentID
	}
	if req.ProblemID != "" && len(req.ProblemID) <= 63 {
		labels[LabelProbeProblem] = req.ProblemID
	}

	env := []corev1.EnvVar{
		{Name: "JK_PROTO", Value: req.Protocol},
		{Name: "JK_HOST", Value: req.Host},
		{Name: "JK_ATTEMPTS", Value: strconv.Itoa(req.Attempts)},
		{Name: "JK_TIMEOUT", Value: strconv.Itoa(int(req.Timeout / time.Second))},
	}
	if req.Protocol != ProbeDNS {
		env = append(env, corev1.EnvVar{Name: "JK_PORT", Value: strconv.Itoa(req.Port)})
	}
	if req.Protocol == ProbeHTTP {
		env = append(env,
			corev1.EnvVar{Name: "JK_HTTP_HOST", Value: req.HTTPHost},
			corev1.EnvVar{Name: "JK_PATH", Value: req.Path},
			corev1.EnvVar{Name: "JK_STATUS", Value: strconv.Itoa(req.ExpectStatus)},
			corev1.EnvVar{Name: "JK_MARKER", Value: req.BodyMarker})
	}

	no, yes := false, true
	user := int64(65534)
	grace := int64(1)
	deadline := int64(podDeadlineSecs)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: req.Namespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			AutomountServiceAccountToken:  &no,
			EnableServiceLinks:            &no,
			TerminationGracePeriodSeconds: &grace,
			ActiveDeadlineSeconds:         &deadline,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &yes, RunAsUser: &user, RunAsGroup: &user,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            "probe",
				Image:           r.set.probeImage,
				ImagePullPolicy: corev1.PullNever,
				Command:         []string{"sh", "-c", probeScript},
				Env:             env,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &no,
					Privileged:               &no,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				TerminationMessagePolicy: corev1.TerminationMessageReadFile,
			}},
		},
	}
}

// parseProbeMessage reads the probe's termination message. Anything
// incomplete or inconsistent is an error (a probe fault), never a result.
func parseProbeMessage(msg string, req ProbeRequest) (ProbeResult, error) {
	kv := map[string]string{}
	for _, line := range strings.Split(msg, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			kv[strings.TrimSpace(k)] = v
		}
	}
	if kv["v"] != "1" {
		return ProbeResult{}, fmt.Errorf("unsupported or missing report version %q", kv["v"])
	}
	for _, k := range []string{"dns", "connected", "class", "status", "marker", "attempts", "detail"} {
		if _, ok := kv[k]; !ok {
			return ProbeResult{}, fmt.Errorf("report lacks %q", k)
		}
	}
	flag := func(k string) (bool, error) {
		switch kv[k] {
		case "0":
			return false, nil
		case "1":
			return true, nil
		}
		return false, fmt.Errorf("report %s=%q is not 0 or 1", k, kv[k])
	}
	connected, err := flag("connected")
	if err != nil {
		return ProbeResult{}, err
	}
	marker, err := flag("marker")
	if err != nil {
		return ProbeResult{}, err
	}
	status, err := strconv.Atoi(kv["status"])
	if err != nil || status < 0 || status > 999 {
		return ProbeResult{}, fmt.Errorf("report status=%q is not an HTTP status", kv["status"])
	}
	class := kv["class"]
	switch class {
	case "", FailureDNS, FailureRefused, FailureTimeout, FailureHTTPMismatch, FailureNetwork:
	default:
		return ProbeResult{}, fmt.Errorf("report class=%q is unknown", class)
	}
	switch {
	case class == FailureDNS && (connected || kv["dns"] != "fail"):
		return ProbeResult{}, errors.New("report says DNS failed but also that it connected")
	case connected && class != "" && class != FailureHTTPMismatch:
		return ProbeResult{}, fmt.Errorf("report is connected yet class=%q", class)
	case !connected && class == "":
		return ProbeResult{}, errors.New("report is neither connected nor a failure")
	}
	res := ProbeResult{Connected: connected, HTTPStatus: status, FailureClass: class}
	if marker && req.BodyMarker != "" {
		res.BodyMarker = req.BodyMarker
	}
	return res, nil
}
