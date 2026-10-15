package grader

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/JunYupK/JunKloud/internal/testutil"
)

const testProbeImage = "docker.io/library/busybox:1.37.0"

// stepClock never sleeps: Sleep advances virtual time.
type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stepClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *stepClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return nil
}

// fakePods is an in-memory pod API that, like the real one, refuses calls
// made with a cancelled context.
type fakePods struct {
	mu             sync.Mutex
	pods           map[string]*corev1.Pod
	created        []*corev1.Pod
	onCreate       func(*corev1.Pod)
	afterCreateErr error // returned after the pod was stored
	createErr      error // returned before anything is stored
	onGet          func(n int, p *corev1.Pod)
	getN           int
	deleteFailures int // the first n deletes fail
	deletes        int
	cancelledCalls int // calls made with an already cancelled context
}

func newFakePods() *fakePods { return &fakePods{pods: map[string]*corev1.Pod{}} }

func (f *fakePods) enter(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		f.cancelledCalls++
		return err
	}
	return nil
}

func (f *fakePods) Create(ctx context.Context, ns string, pod *corev1.Pod) (*corev1.Pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(ctx); err != nil {
		return nil, err
	}
	if f.createErr != nil {
		return nil, f.createErr
	}
	cp := pod.DeepCopy()
	cp.Namespace = ns
	if f.onCreate != nil {
		f.onCreate(cp)
	}
	f.pods[ns+"/"+cp.Name] = cp
	f.created = append(f.created, cp.DeepCopy())
	if f.afterCreateErr != nil {
		return nil, f.afterCreateErr
	}
	return cp.DeepCopy(), nil
}

func (f *fakePods) Get(ctx context.Context, ns, name string) (*corev1.Pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(ctx); err != nil {
		return nil, err
	}
	p, ok := f.pods[ns+"/"+name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, name)
	}
	f.getN++
	if f.onGet != nil {
		f.onGet(f.getN, p)
	}
	return p.DeepCopy(), nil
}

func (f *fakePods) Delete(ctx context.Context, ns, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(ctx); err != nil {
		return err
	}
	f.deletes++
	if f.deletes <= f.deleteFailures {
		return errors.New("etcd leader changed")
	}
	if _, ok := f.pods[ns+"/"+name]; !ok {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, name)
	}
	delete(f.pods, ns+"/"+name)
	return nil
}

func (f *fakePods) remaining() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pods)
}

const okMessage = "v=1\ndns=ok\nconnected=1\nclass=\nstatus=200\nmarker=1\nattempts=1\ndetail=\n"

func terminated(msg string, exit int32) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodSucceeded
		if exit != 0 {
			p.Status.Phase = corev1.PodFailed
		}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "probe", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Message: msg}}}}
	}
}

func waiting(reason, msg string) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodPending
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "probe", State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}}}}
	}
}

func testRunner(t *testing.T, f *fakePods) *podProbeRunner {
	t.Helper()
	r, err := newPodProbeRunner(f, WithProbeImage(testProbeImage), WithClock(&stepClock{now: time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)}))
	testutil.NoError(t, err)
	return r
}

func httpRequest() ProbeRequest {
	return ProbeRequest{
		Protocol: ProbeHTTP, Namespace: "jk-net", Labels: map[string]string{"role": "frontend"},
		Host: "web.jk-net.svc.cluster.local", Port: 80, HTTPHost: "shop.example.com", Path: "/healthz",
		ExpectStatus: 200, BodyMarker: "APP-WEB", Attempts: ProbeAttempts, Timeout: ProbeAttemptTimeout,
		EnvironmentID: "0123456789ab", ProblemID: "net-1",
	}
}

func TestProbePodSpec(t *testing.T) {
	f := newFakePods()
	f.onCreate = terminated(okMessage, 0)
	res, err := testRunner(t, f).Probe(context.Background(), httpRequest())
	testutil.NoError(t, err)
	testutil.Equal(t, res, ProbeResult{Connected: true, HTTPStatus: 200, BodyMarker: "APP-WEB"})
	testutil.Equal(t, len(f.created), 1)
	pod := f.created[0]
	testutil.Equal(t, pod.Namespace, "jk-net")
	// The source identity is the pod's labels, so learner selectors apply.
	testutil.Equal(t, pod.Labels["role"], "frontend")
	testutil.Equal(t, pod.Labels["junkloud.io/environment-id"], "0123456789ab")
	testutil.Equal(t, pod.Labels["junkloud.io/problem-id"], "net-1")
	testutil.Equal(t, pod.Labels["junkloud.io/component"], "grading-probe")
	testutil.Equal(t, pod.Labels["app.kubernetes.io/managed-by"], "junkloud")
	testutil.Equal(t, pod.Labels["junkloud.io/probe-run"] != "", true)
	testutil.Equal(t, strings.HasPrefix(pod.Name, "jk-probe-"), true)

	spec := pod.Spec
	testutil.Equal(t, spec.RestartPolicy, corev1.RestartPolicyNever)
	testutil.Equal(t, *spec.AutomountServiceAccountToken, false)
	testutil.Equal(t, *spec.EnableServiceLinks, false)
	testutil.Equal(t, spec.ServiceAccountName, "")
	testutil.Equal(t, *spec.SecurityContext.RunAsNonRoot, true)
	testutil.Equal(t, spec.SecurityContext.SeccompProfile.Type, corev1.SeccompProfileTypeRuntimeDefault)
	testutil.Equal(t, *spec.ActiveDeadlineSeconds > 0 && *spec.ActiveDeadlineSeconds <= 60, true)
	testutil.Equal(t, spec.HostNetwork, false)
	testutil.Equal(t, len(spec.Containers), 1)
	c := spec.Containers[0]
	testutil.Equal(t, c.Image, testProbeImage)
	testutil.Equal(t, c.ImagePullPolicy, corev1.PullNever)
	testutil.Equal(t, *c.SecurityContext.AllowPrivilegeEscalation, false)
	testutil.Equal(t, *c.SecurityContext.Privileged == false, true)
	testutil.Equal(t, c.Resources.Limits.Cpu().IsZero(), false)
	testutil.Equal(t, c.Resources.Limits.Memory().IsZero(), false)
	testutil.Equal(t, c.Resources.Requests.Cpu().IsZero(), false)
	testutil.Equal(t, c.TerminationMessagePolicy, corev1.TerminationMessageReadFile)
	testutil.Equal(t, c.Command[0], "sh")
	testutil.Equal(t, strings.Contains(c.Command[len(c.Command)-1], "/dev/termination-log"), true)
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	for k, v := range map[string]string{
		"JK_PROTO": "http", "JK_HOST": "web.jk-net.svc.cluster.local", "JK_PORT": "80", "JK_HTTP_HOST": "shop.example.com",
		"JK_PATH": "/healthz", "JK_STATUS": "200", "JK_MARKER": "APP-WEB", "JK_ATTEMPTS": "3", "JK_TIMEOUT": "3",
	} {
		testutil.Equal(t, env[k], v)
	}
	testutil.Equal(t, f.remaining(), 0)
}

func TestProbeResolveNameAndBudget(t *testing.T) {
	clk := &stepClock{now: time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)}
	newRunner := func(f *fakePods) *podProbeRunner {
		r, err := newPodProbeRunner(f, WithProbeImage(testProbeImage), WithClock(clk))
		testutil.NoError(t, err)
		return r
	}
	t.Run("the name to resolve is passed to the pod", func(t *testing.T) {
		f := newFakePods()
		f.onCreate = terminated(okMessage, 0)
		q := httpRequest()
		q.Protocol, q.Host, q.ResolveName = ProbeTCP, "10.0.0.5", "web.jk-net.svc.cluster.local"
		_, err := newRunner(f).Probe(context.Background(), q)
		testutil.NoError(t, err)
		env := map[string]string{}
		for _, e := range f.created[0].Spec.Containers[0].Env {
			env[e.Name] = e.Value
		}
		testutil.Equal(t, env["JK_RESOLVE"], "web.jk-net.svc.cluster.local")
		testutil.Equal(t, env["JK_HOST"], "10.0.0.5")
	})
	t.Run("the pod deadline and the wait follow the remaining budget", func(t *testing.T) {
		f := newFakePods() // never finishes
		q := httpRequest()
		start := clk.Now()
		q.Deadline = start.Add(12 * time.Second)
		_, err := newRunner(f).Probe(context.Background(), q)
		if !errors.Is(err, ErrProbeFailed) || !strings.Contains(err.Error(), "time budget") {
			t.Fatalf("Probe = %v", err)
		}
		waited := clk.Now().Sub(start)
		testutil.Equal(t, waited >= 12*time.Second && waited < 14*time.Second, true)
		testutil.Equal(t, *f.created[0].Spec.ActiveDeadlineSeconds <= 14, true)
		testutil.Equal(t, f.remaining(), 0)
	})
	t.Run("no budget left means no pod", func(t *testing.T) {
		f := newFakePods()
		q := httpRequest()
		q.Deadline = clk.Now().Add(time.Second)
		_, err := newRunner(f).Probe(context.Background(), q)
		if !errors.Is(err, ErrProbeFailed) || !strings.Contains(err.Error(), "time budget") {
			t.Fatalf("Probe = %v", err)
		}
		testutil.Equal(t, len(f.created), 0)
	})
	t.Run("without a deadline the runner's own bound applies", func(t *testing.T) {
		f := newFakePods()
		start := clk.Now()
		_, err := newRunner(f).Probe(context.Background(), httpRequest())
		if !errors.Is(err, ErrProbeFailed) {
			t.Fatalf("Probe = %v", err)
		}
		testutil.Equal(t, clk.Now().Sub(start) >= podWait, true)
	})
}

func TestProbeRunnerBoundsAndImage(t *testing.T) {
	_, err := newPodProbeRunner(newFakePods())
	if err == nil || !strings.Contains(err.Error(), "probe image") {
		t.Fatalf("a runner without a pinned probe image must not exist (no default, no :latest): %v", err)
	}
	// The bounds of the spec are enforced by the runner as well.
	f := newFakePods()
	f.onCreate = terminated(okMessage, 0)
	r := testRunner(t, f)
	for name, mod := range map[string]func(*ProbeRequest){
		"too many attempts": func(q *ProbeRequest) { q.Attempts = 4 },
		"no attempts":       func(q *ProbeRequest) { q.Attempts = 0 },
		"long timeout":      func(q *ProbeRequest) { q.Timeout = 4 * time.Second },
		"no namespace":      func(q *ProbeRequest) { q.Namespace = "" },
		"unknown protocol":  func(q *ProbeRequest) { q.Protocol = "udp" },
		"bad port":          func(q *ProbeRequest) { q.Port = 0 },
		"reserved label":    func(q *ProbeRequest) { q.Labels = map[string]string{"junkloud.io/probe-run": "x"} },
	} {
		q := httpRequest()
		mod(&q)
		if _, err := r.Probe(context.Background(), q); !errors.Is(err, ErrProbeFailed) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	testutil.Equal(t, len(f.created), 0)
}

func TestProbeCleanupAfterSuccess(t *testing.T) {
	for _, msg := range []string{okMessage, "v=1\ndns=ok\nconnected=0\nclass=timeout\nstatus=0\nmarker=0\nattempts=3\ndetail=\n"} {
		f := newFakePods()
		f.onCreate = terminated(msg, 0)
		_, err := testRunner(t, f).Probe(context.Background(), httpRequest())
		testutil.NoError(t, err)
		testutil.Equal(t, f.remaining(), 0)
		testutil.Equal(t, f.deletes, 1)
	}
}

func TestProbeCleanupAfterCancel(t *testing.T) {
	t.Run("cancelled while waiting", func(t *testing.T) {
		f := newFakePods() // the pod never finishes
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.onGet = func(n int, _ *corev1.Pod) {
			if n == 3 {
				cancel()
			}
		}
		_, err := testRunner(t, f).Probe(ctx, httpRequest())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Probe = %v, want a cancellation", err)
		}
		testutil.Equal(t, f.remaining(), 0) // cleanup survived the cancellation
		testutil.Equal(t, f.deletes >= 1, true)
		// Every call after the cancellation (the cleanup) used a live context.
		testutil.Equal(t, f.cancelledCalls <= 1, true)
	})
	t.Run("deadline expires while waiting", func(t *testing.T) {
		f := newFakePods()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		f.onGet = func(n int, _ *corev1.Pod) {
			if n == 3 {
				time.Sleep(1100 * time.Millisecond)
			}
		}
		_, err := testRunner(t, f).Probe(ctx, httpRequest())
		testutil.Equal(t, err != nil, true)
		testutil.Equal(t, f.remaining(), 0)
	})
	t.Run("cancelled during create, which the server completed", func(t *testing.T) {
		f := newFakePods()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.afterCreateErr = context.Canceled
		f.onCreate = func(*corev1.Pod) { cancel() }
		_, err := testRunner(t, f).Probe(ctx, httpRequest())
		testutil.Equal(t, err != nil, true)
		testutil.Equal(t, f.remaining(), 0)
	})
	t.Run("create answered with an error after storing the pod", func(t *testing.T) {
		f := newFakePods()
		f.afterCreateErr = errors.New("etcdserver: request timed out")
		_, err := testRunner(t, f).Probe(context.Background(), httpRequest())
		if !errors.Is(err, ErrProbeFailed) {
			t.Fatalf("Probe = %v", err)
		}
		testutil.Equal(t, f.remaining(), 0)
	})
	t.Run("a failing delete is retried", func(t *testing.T) {
		f := newFakePods()
		f.onCreate = terminated(okMessage, 0)
		f.deleteFailures = 2
		_, err := testRunner(t, f).Probe(context.Background(), httpRequest())
		testutil.NoError(t, err)
		testutil.Equal(t, f.remaining(), 0)
		testutil.Equal(t, f.deletes, 3)
	})
	t.Run("a pod that cannot be removed is reported", func(t *testing.T) {
		f := newFakePods()
		f.onCreate = terminated(okMessage, 0)
		f.deleteFailures = 1000
		_, err := testRunner(t, f).Probe(context.Background(), httpRequest())
		if !errors.Is(err, ErrProbeFailed) || !strings.Contains(err.Error(), "remove") {
			t.Fatalf("Probe = %v", err)
		}
	})
}

func TestProbePodFaultsAreProbeErrors(t *testing.T) {
	notFoundNS := apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "jk-net")
	cases := []struct {
		name  string
		setup func(*fakePods)
		want  string
	}{
		{"image not preloaded", func(f *fakePods) { f.onCreate = waiting("ErrImageNeverPull", "Container image not present") }, "image"},
		{"invalid image name", func(f *fakePods) { f.onCreate = waiting("InvalidImageName", "bad") }, "image"},
		{"container config error", func(f *fakePods) { f.onCreate = waiting("CreateContainerConfigError", "secret missing") }, "container"},
		{"unschedulable", func(f *fakePods) {
			f.onCreate = func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodPending
				p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "0/1 nodes: Insufficient cpu"}}
			}
		}, "Insufficient cpu"},
		{"evicted", func(f *fakePods) {
			f.onCreate = func(p *corev1.Pod) {
				p.Status.Phase, p.Status.Reason, p.Status.Message = corev1.PodFailed, "Evicted", "low on memory"
			}
		}, "Evicted"},
		{"failed without a message", func(f *fakePods) { f.onCreate = terminated("", 1) }, "exit code 1"},
		{"succeeded with garbage", func(f *fakePods) { f.onCreate = terminated("hello", 0) }, "report"},
		{"never finishes", func(f *fakePods) {}, "did not finish"},
		{"namespace is gone", func(f *fakePods) { f.createErr = notFoundNS }, "namespace"},
		{"admission refuses", func(f *fakePods) {
			f.createErr = apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "x", errors.New("exceeded quota"))
		}, "quota"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakePods()
			tc.setup(f)
			res, err := testRunner(t, f).Probe(context.Background(), httpRequest())
			if !errors.Is(err, ErrProbeFailed) {
				t.Fatalf("Probe = %+v, %v; want ErrProbeFailed", res, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q lacks %q", err, tc.want)
			}
			testutil.Equal(t, f.remaining(), 0)
		})
	}
}

func TestParseProbeMessage(t *testing.T) {
	req := httpRequest()
	good := map[string]ProbeResult{
		okMessage: {Connected: true, HTTPStatus: 200, BodyMarker: "APP-WEB"},
		"v=1\ndns=ok\nconnected=1\nclass=http-mismatch\nstatus=200\nmarker=0\nattempts=3\ndetail=\n":                    {Connected: true, HTTPStatus: 200, FailureClass: FailureHTTPMismatch},
		"v=1\ndns=ok\nconnected=0\nclass=timeout\nstatus=0\nmarker=0\nattempts=3\ndetail=nc: x: Connection timed out\n": {FailureClass: FailureTimeout},
		"v=1\ndns=ok\nconnected=0\nclass=refused\nstatus=0\nmarker=0\nattempts=3\ndetail=\n":                            {FailureClass: FailureRefused},
		"v=1\ndns=fail\nconnected=0\nclass=dns\nstatus=0\nmarker=0\nattempts=0\ndetail= server cant find x\n":           {FailureClass: FailureDNS},
		"v=1\ndns=ok\nconnected=0\nclass=network-error\nstatus=0\nmarker=0\nattempts=3\ndetail=x\n":                     {FailureClass: FailureNetwork},
	}
	for msg, want := range good {
		got, err := parseProbeMessage(msg, req)
		testutil.NoError(t, err)
		testutil.Equal(t, got, want)
	}
	// A marker is only credited when it is the request's own.
	got, err := parseProbeMessage(strings.Replace(okMessage, "marker=1", "marker=0", 1), req)
	testutil.NoError(t, err)
	testutil.Equal(t, got.BodyMarker, "")
	for name, msg := range map[string]string{
		"empty":             "",
		"no version":        "dns=ok\nconnected=1\n",
		"future version":    strings.Replace(okMessage, "v=1", "v=2", 1),
		"unknown class":     strings.Replace(okMessage, "class=\n", "class=weird\n", 1),
		"connected + dns":   "v=1\ndns=fail\nconnected=1\nclass=dns\nstatus=0\nmarker=0\nattempts=1\ndetail=\n",
		"not connected, ok": "v=1\ndns=ok\nconnected=0\nclass=\nstatus=0\nmarker=0\nattempts=1\ndetail=\n",
		"bad status":        strings.Replace(okMessage, "status=200", "status=abc", 1),
		"truncated":         "v=1\ndns=ok\nconnected=1",
		"missing keys":      "v=1\nconnected=1\n",
	} {
		if _, err := parseProbeMessage(msg, req); err == nil {
			t.Fatalf("%s: accepted %q", name, msg)
		}
	}
	// The probe-only DNS check reports success as connected.
	dns := ProbeRequest{Protocol: ProbeDNS, Namespace: "x", Host: "web.x.svc.cluster.local", Attempts: 3, Timeout: 3 * time.Second}
	got, err = parseProbeMessage("v=1\ndns=ok\nconnected=1\nclass=\nstatus=0\nmarker=0\nattempts=0\ndetail=\n", dns)
	testutil.NoError(t, err)
	testutil.Equal(t, got.Connected, true)
}
