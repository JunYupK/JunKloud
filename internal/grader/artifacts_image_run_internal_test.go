package grader

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

const testNode = "0123456789abcdef"

// recTools records tool calls; fail returns an exit code for a call prefix.
type recTools struct {
	mu     sync.Mutex
	calls  []string
	fail   map[string]int
	stderr string
}

func (r *recTools) Run(_ context.Context, call ToolCall) (ToolResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	line := call.Exe + " " + strings.Join(call.Args, " ")
	r.calls = append(r.calls, line)
	for prefix, code := range r.fail {
		if strings.HasPrefix(line, prefix) {
			msg := "boom"
			if r.stderr != "" {
				msg = r.stderr
			}
			return ToolResult{ExitCode: code, Stderr: []byte(msg)}, nil
		}
	}
	if strings.HasPrefix(line, "docker image inspect --format") {
		id := "sha256:" + strings.Repeat("1", 64)
		count := len(call.Args) - 4
		return ToolResult{Stdout: []byte(strings.Repeat(id+" linux/amd64\n", count))}, nil
	}
	return ToolResult{}, nil
}

func TestImageRunDocker24VerifiedExport(t *testing.T) {
	tools := &recTools{fail: map[string]int{"docker save --platform": 1}, stderr: `"--platform" requires API version 1.48, but the Docker daemon API version is 1.43`}
	pods := newFakePods()
	pods.onCreate = exited(0, "Completed", "")
	result, err := runnerFor(t, tools, pods, nil, "native run").Run(context.Background(), runRequest())
	testutil.NoError(t, err)
	testutil.Equal(t, result.Completed, true)
	testutil.Equal(t, tools.has("docker save --output"), 1)
}

func (r *recTools) has(prefix string) int {
	n := 0
	for _, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (r *recTools) index(prefix string) int {
	for i, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

type outPods struct{ text string }

func (o outPods) Logs(context.Context, string, string, string, LogOptions) (string, error) {
	return o.text, nil
}
func (o outPods) Exec(context.Context, string, string, string, []string) (ExecResult, error) {
	return ExecResult{}, errors.New("not used")
}

func appStatus(state corev1.ContainerState, ip string) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Status.PodIP = ip
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", State: state}}
	}
}

func exited(code int32, reason, msg string) func(*corev1.Pod) {
	return appStatus(corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: reason, Message: msg}}, "")
}

func runnerFor(t *testing.T, tools *recTools, pods *fakePods, probes ProbeRunner, logs string) *ClusterImageRunner {
	t.Helper()
	r, err := newClusterImageRunner(tools, pods, outPods{logs}, probes,
		WithClock(&stepClock{now: time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)}), WithProbeControlNamespace("junkloud-platform"))
	testutil.NoError(t, err)
	r.arch = "amd64"
	return r
}

func runRequest() ImageRunRequest {
	return ImageRunRequest{
		Identity:  model.Identity{ID: "0123456789ab", ClusterName: "junkloud-0123456789ab", ContainerIDs: []string{testNode}},
		ProblemID: "img-1", LocalRef: "shop:1", Timeout: 10 * time.Second,
	}
}

func TestImageRunSequenceAndCleanup(t *testing.T) {
	tools := &recTools{}
	pods := newFakePods()
	pods.onCreate = exited(0, "Completed", "")
	res, err := runnerFor(t, tools, pods, nil, "hello from shop\n").Run(context.Background(), runRequest())
	testutil.NoError(t, err)
	testutil.Equal(t, res.Completed, true)
	testutil.Equal(t, res.ExitCode, 0)
	testutil.Equal(t, res.Output, "hello from shop\n")

	// Order: tag, save (current architecture only), load, then clean-up.
	tag, save, load := tools.index("docker tag sha256:"), tools.index("docker save --platform linux/amd64"), tools.index("kind load image-archive")
	rmi, rm := tools.index("docker exec "+testNode+" crictl rmi junkloud.local/grade-image:"), tools.index("docker image rm junkloud.local/grade-image:")
	if !(tag >= 0 && tag < save && save < load && load < rmi && load < rm) {
		t.Fatalf("unexpected order: %v", tools.calls)
	}
	if !strings.Contains(tools.calls[load], "--name junkloud-0123456789ab") {
		t.Errorf("load does not name the environment's cluster: %s", tools.calls[load])
	}
	// Only the per-run tag is ever removed; never the learner's tag, never a prune.
	for _, c := range tools.calls {
		if strings.Contains(c, "prune") {
			t.Errorf("prune called: %s", c)
		}
		if (strings.Contains(c, " rm ") || strings.Contains(c, " rmi ")) && !strings.Contains(c, "junkloud.local/grade-image:") {
			t.Errorf("removes something that is not this run's tag: %s", c)
		}
		if strings.HasSuffix(c, " shop:1") && !strings.HasPrefix(c, "docker image inspect ") {
			t.Errorf("touches the learner's tag: %s", c)
		}
	}
	testutil.Equal(t, pods.remaining(), 0)

	pod := pods.created[0]
	testutil.Equal(t, pod.Namespace, "junkloud-platform")
	testutil.Equal(t, pod.Labels["junkloud.io/component"], "grading-probe")
	testutil.Equal(t, pod.Labels["junkloud.io/environment-id"], "0123456789ab")
	c := pod.Spec.Containers[0]
	testutil.Equal(t, c.ImagePullPolicy, corev1.PullNever)
	testutil.Equal(t, strings.HasPrefix(c.Image, "junkloud.local/grade-image:"), true)
	testutil.Equal(t, *pod.Spec.AutomountServiceAccountToken, false)
	testutil.Equal(t, *c.SecurityContext.Privileged, false)
	testutil.Equal(t, c.Resources.Limits.Memory().IsZero(), false)
	testutil.Equal(t, len(c.Command), 0) // the image's own entrypoint runs
}

func TestImageRunArgsReplaceCmdOnly(t *testing.T) {
	tools, pods := &recTools{}, newFakePods()
	pods.onCreate = exited(0, "Completed", "")
	req := runRequest()
	req.Args = []string{"--mode", "check"}
	_, err := runnerFor(t, tools, pods, nil, "").Run(context.Background(), req)
	testutil.NoError(t, err)
	testutil.Equal(t, pods.created[0].Spec.Containers[0].Args, []string{"--mode", "check"})
	testutil.Equal(t, len(pods.created[0].Spec.Containers[0].Command), 0)
}

func TestImageRunOutcomes(t *testing.T) {
	t.Run("non-zero exit is a result", func(t *testing.T) {
		pods := newFakePods()
		pods.onCreate = exited(3, "Error", "")
		res, err := runnerFor(t, &recTools{}, pods, nil, "bad").Run(context.Background(), runRequest())
		testutil.NoError(t, err)
		testutil.Equal(t, res.ExitCode, 3)
	})
	t.Run("start failure is the image's", func(t *testing.T) {
		pods := newFakePods()
		pods.onCreate = exited(128, "StartError", `exec: "/app/server": stat /app/server: no such file or directory`)
		res, err := runnerFor(t, &recTools{}, pods, nil, "").Run(context.Background(), runRequest())
		testutil.NoError(t, err)
		testutil.Equal(t, strings.Contains(res.StartError, "/app/server"), true)
		testutil.Equal(t, res.Completed, false)
	})
	t.Run("create error naming a missing executable is the image's", func(t *testing.T) {
		pods := newFakePods()
		pods.onCreate = appStatus(corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerError", Message: `exec: "x": executable file not found in $PATH`}}, "")
		res, err := runnerFor(t, &recTools{}, pods, nil, "").Run(context.Background(), runRequest())
		testutil.NoError(t, err)
		testutil.Equal(t, res.StartError != "", true)
	})
	t.Run("other create errors are the environment's", func(t *testing.T) {
		pods := newFakePods()
		pods.onCreate = appStatus(corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerError", Message: "name is reserved"}}, "")
		_, err := runnerFor(t, &recTools{}, pods, nil, "").Run(context.Background(), runRequest())
		testutil.Equal(t, err != nil, true)
	})
	t.Run("an image missing on the node is the environment's", func(t *testing.T) {
		tools, pods := &recTools{}, newFakePods()
		pods.onCreate = appStatus(corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImageNeverPull", Message: "absent"}}, "")
		_, err := runnerFor(t, tools, pods, nil, "").Run(context.Background(), runRequest())
		testutil.Equal(t, err != nil, true)
		testutil.Equal(t, pods.remaining(), 0)
		testutil.Equal(t, tools.has("docker image rm junkloud.local/grade-image:"), 1)
	})
	t.Run("still running at the timeout", func(t *testing.T) {
		pods := newFakePods()
		pods.onCreate = appStatus(corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, "10.0.0.5")
		res, err := runnerFor(t, &recTools{}, pods, nil, "").Run(context.Background(), runRequest())
		testutil.NoError(t, err)
		testutil.Equal(t, res.TimedOut, true)
	})
	t.Run("never starting is the environment's", func(t *testing.T) {
		pods := newFakePods()
		pods.onCreate = appStatus(corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}, "")
		_, err := runnerFor(t, &recTools{}, pods, nil, "").Run(context.Background(), runRequest())
		testutil.Equal(t, err != nil, true)
		testutil.Equal(t, strings.Contains(err.Error(), "did not start"), true)
	})
}

func TestImageRunCleansUpAfterFailures(t *testing.T) {
	cases := []struct {
		name       string
		fail       map[string]int
		wantImgRm  int
		wantNodeRm int
		wantPod    bool
	}{
		{name: "save fails", fail: map[string]int{"docker save": 1}, wantImgRm: 1},
		{name: "load fails after maybe importing", fail: map[string]int{"kind load": 1}, wantImgRm: 1, wantNodeRm: 1},
		{name: "tag fails", fail: map[string]int{"docker tag": 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tools, pods := &recTools{fail: tc.fail}, newFakePods()
			_, err := runnerFor(t, tools, pods, nil, "").Run(context.Background(), runRequest())
			testutil.Equal(t, err != nil, true)
			testutil.Equal(t, tools.has("docker image rm junkloud.local/grade-image:"), tc.wantImgRm)
			testutil.Equal(t, tools.has("docker exec "+testNode+" crictl rmi"), tc.wantNodeRm)
			testutil.Equal(t, len(pods.created), 0)
		})
	}
	t.Run("a clean-up that fails is reported", func(t *testing.T) {
		tools, pods := &recTools{fail: map[string]int{"docker image rm": 1}}, newFakePods()
		pods.onCreate = exited(0, "Completed", "")
		_, err := runnerFor(t, tools, pods, nil, "").Run(context.Background(), runRequest())
		testutil.Equal(t, err != nil && strings.Contains(err.Error(), "clean-up"), true)
	})
	t.Run("a node that does not know the tag is fine", func(t *testing.T) {
		tools, pods := &recTools{}, newFakePods()
		pods.onCreate = exited(0, "Completed", "")
		tools.fail, tools.stderr = map[string]int{"docker exec": 1}, "rpc error: code = NotFound desc = image not found"
		_, err := runnerFor(t, tools, pods, nil, "").Run(context.Background(), runRequest())
		testutil.NoError(t, err)
	})
	t.Run("cancelled context still removes everything", func(t *testing.T) {
		tools, pods := &recTools{}, newFakePods()
		ctx, cancel := context.WithCancel(context.Background())
		pods.onCreate = func(p *corev1.Pod) { cancel() }
		_, err := runnerFor(t, tools, pods, nil, "").Run(ctx, runRequest())
		testutil.Equal(t, errors.Is(err, context.Canceled), true)
		testutil.Equal(t, pods.remaining(), 0)
		testutil.Equal(t, tools.has("docker image rm junkloud.local/grade-image:"), 1)
		testutil.Equal(t, tools.has("docker exec "+testNode+" crictl rmi"), 1)
	})
}

func TestImageRunRefusesBadInput(t *testing.T) {
	for name, mut := range map[string]func(*ImageRunRequest){
		"flag as reference": func(r *ImageRunRequest) { r.LocalRef = "--privileged" },
		"shell characters":  func(r *ImageRunRequest) { r.LocalRef = "shop:1;id" },
		"no cluster name":   func(r *ImageRunRequest) { r.Identity.ClusterName = "" },
		"option as cluster": func(r *ImageRunRequest) { r.Identity.ClusterName = "--all" },
	} {
		t.Run(name, func(t *testing.T) {
			tools := &recTools{}
			req := runRequest()
			mut(&req)
			_, err := runnerFor(t, tools, newFakePods(), nil, "").Run(context.Background(), req)
			testutil.Equal(t, err != nil, true)
			testutil.Equal(t, len(tools.calls), 0)
		})
	}
	t.Run("nodes found by the engine when the identity lists none", func(t *testing.T) {
		tools, pods := &recTools{}, newFakePods()
		pods.onCreate = exited(0, "Completed", "")
		req := runRequest()
		req.Identity.ContainerIDs = nil
		_, err := runnerFor(t, tools, pods, nil, "").Run(context.Background(), req)
		testutil.NoError(t, err)
		testutil.Equal(t, tools.has("docker ps -q --filter label=io.x-k8s.kind.cluster=junkloud-0123456789ab"), 1)
	})
}

type probeFn func(ProbeRequest) (ProbeResult, error)

func (f probeFn) Probe(_ context.Context, r ProbeRequest) (ProbeResult, error) { return f(r) }

func TestImageRunServing(t *testing.T) {
	serve := func() ImageRunRequest {
		r := runRequest()
		r.Serve = &ImageServe{Port: 8080, Path: "/", Status: 200, BodyMarker: "shop-v2"}
		return r
	}
	t.Run("requests the pod by IP", func(t *testing.T) {
		pods := newFakePods()
		pods.onCreate = appStatus(corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, "10.1.2.3")
		var got ProbeRequest
		probes := probeFn(func(r ProbeRequest) (ProbeResult, error) {
			got = r
			return ProbeResult{Connected: true, HTTPStatus: 200, BodyMarker: "shop-v2"}, nil
		})
		res, err := runnerFor(t, &recTools{}, pods, probes, "").Run(context.Background(), serve())
		testutil.NoError(t, err)
		testutil.Equal(t, res.Probe.BodyMarker, "shop-v2")
		testutil.Equal(t, got.Host, "10.1.2.3")
		testutil.Equal(t, got.Port, 8080)
		testutil.Equal(t, got.Namespace, "junkloud-platform")
		testutil.Equal(t, got.BodyMarker, "shop-v2")
		testutil.Equal(t, pods.remaining(), 0)
	})
	t.Run("keeps asking until the timeout and returns the last answer", func(t *testing.T) {
		pods := newFakePods()
		pods.onCreate = appStatus(corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, "10.1.2.3")
		n := 0
		probes := probeFn(func(ProbeRequest) (ProbeResult, error) {
			n++
			return ProbeResult{FailureClass: FailureRefused}, nil
		})
		res, err := runnerFor(t, &recTools{}, pods, probes, "").Run(context.Background(), serve())
		testutil.NoError(t, err)
		testutil.Equal(t, res.Probe.FailureClass, FailureRefused)
		testutil.Equal(t, n > 1, true)
	})
	t.Run("a container that exits meanwhile is reported as exited", func(t *testing.T) {
		pods := newFakePods()
		pods.onCreate = appStatus(corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, "10.1.2.3")
		pods.onGet = func(n int, p *corev1.Pod) {
			if n >= 2 {
				exited(1, "Error", "")(p)
			}
		}
		probes := probeFn(func(ProbeRequest) (ProbeResult, error) { return ProbeResult{FailureClass: FailureRefused}, nil })
		res, err := runnerFor(t, &recTools{}, pods, probes, "").Run(context.Background(), serve())
		testutil.NoError(t, err)
		testutil.Equal(t, res.Completed, true)
		testutil.Equal(t, res.ExitCode, 1)
	})
	t.Run("a probe that cannot run is an error", func(t *testing.T) {
		pods := newFakePods()
		pods.onCreate = appStatus(corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, "10.1.2.3")
		probes := probeFn(func(ProbeRequest) (ProbeResult, error) {
			return ProbeResult{}, fmt.Errorf("%w: no node", ErrProbeFailed)
		})
		_, err := runnerFor(t, &recTools{}, pods, probes, "").Run(context.Background(), serve())
		testutil.ErrorIs(t, err, ErrProbeFailed)
	})
	t.Run("serving without a probe runner", func(t *testing.T) {
		_, err := runnerFor(t, &recTools{}, newFakePods(), nil, "").Run(context.Background(), serve())
		testutil.Equal(t, err != nil, true)
	})
}
