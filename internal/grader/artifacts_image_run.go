package grader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/JunYupK/JunKloud/internal/imageexport"
	"github.com/JunYupK/JunKloud/internal/model"
)

// ClusterImageRunner runs a learner's image as a pod in the environment's
// own cluster. Decision (Task 8): the cluster path is used instead of
// `docker run` on the shared engine because
//
//   - the image runs where graded workloads run (containerd in the node, the
//     pod's resource limits and seccomp profile, the node's architecture),
//   - the learner's code is contained in a pod sandbox with limits rather than
//     started directly on the engine that also hosts the user's other
//     containers, and
//   - it needs no published host ports: a serving image is requested from a
//     probe pod by its pod IP, the same mechanism as the network checks.
//
// The sequence for one run is: tag the learner's image with a per-run tag
// (junkloud.local/grade-image:<run>), `docker save --platform` it, `kind load
// image-archive` it into the environment's nodes, run a pod that never pulls,
// and finally remove exactly what this run created: the pod, the per-run tag
// in the engine and in each node (crictl rmi of that tag only). The learner's
// own tag, other images and the build cache are never touched, and the image
// is never pruned.
//
// The pod is labelled junkloud.io/component=grading-probe like the network
// probe pods, so one label sweep removes leftovers of a crashed grader.
type ClusterImageRunner struct {
	tools  ToolRunner
	pods   podClient
	logs   PodAccess
	probes ProbeRunner
	set    settings
	arch   string
}

const (
	imageRunRepository = "junkloud.local/grade-image"
	imageRunStartWait  = 45 * time.Second
	imageRunLogLimit   = 64 << 10
	toolCleanupBudget  = 30 * time.Second
	imageRunPollGap    = time.Second
)

var (
	nodeContainerPattern = regexp.MustCompile(`^[0-9a-f]{12,64}$`)
	clusterNamePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}$`)
)

// NewClusterImageRunner builds the runner for the cluster that one explicit
// kubeconfig file and context point at. WithProbeControlNamespace names the
// namespace the pod runs in (the platform namespace); probes may be nil when
// no serving image is graded.
func NewClusterImageRunner(kubeconfigPath, contextName string, tools ToolRunner, probes ProbeRunner, opts ...Option) (*ClusterImageRunner, error) {
	k, err := newClusterClients(kubeconfigPath, contextName)
	if err != nil {
		return nil, err
	}
	return newClusterImageRunner(tools, kubePods{k.cs}, k, probes, opts...)
}

func newClusterImageRunner(tools ToolRunner, pods podClient, logs PodAccess, probes ProbeRunner, opts ...Option) (*ClusterImageRunner, error) {
	set := newSettings(opts)
	if tools == nil || pods == nil || logs == nil {
		return nil, errors.New("grader: image runner needs tools, a pod client and pod access")
	}
	if set.controlNamespace == "" {
		return nil, errors.New("grader: image runner needs the namespace to run in (WithProbeControlNamespace)")
	}
	return &ClusterImageRunner{tools: tools, pods: pods, logs: logs, probes: probes, set: set, arch: runtime.GOARCH}, nil
}

func (r *ClusterImageRunner) tool(ctx context.Context, exe string, args ...string) (ToolResult, error) {
	res, err := r.tools.Run(ctx, ToolCall{Exe: exe, Args: args})
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("%s %s failed (exit %d): %s", exe, strings.Join(args[:min(len(args), 2)], " "), res.ExitCode, shortenLine(string(res.Stderr)))
	}
	return res, nil
}

// Run implements ImageRunner.
func (r *ClusterImageRunner) Run(ctx context.Context, req ImageRunRequest) (res ImageRunResult, err error) {
	if !imageRefPattern.MatchString(req.LocalRef) {
		return res, fmt.Errorf("refusing to run %q: not a plain image reference", req.LocalRef)
	}
	if !clusterNamePattern.MatchString(req.Identity.ClusterName) {
		return res, fmt.Errorf("the environment identity has no usable cluster name")
	}
	if req.Serve != nil && r.probes == nil {
		return res, errors.New("a serving image needs a probe runner")
	}
	id, err := randomID()
	if err != nil {
		return res, err
	}
	runTag := imageRunRepository + ":" + id
	dir, err := os.MkdirTemp("", "junkloud-image-*")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(dir)

	// Clean-up runs even when ctx is cancelled and reports what it could not
	// remove: a leftover per-run tag must not go unnoticed.
	var cleanups []func(context.Context) error
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), toolCleanupBudget)
		defer cancel()
		for i := len(cleanups) - 1; i >= 0; i-- {
			if cerr := cleanups[i](cctx); cerr != nil {
				err = errors.Join(err, fmt.Errorf("clean-up of the image run failed: %w", cerr))
				res = ImageRunResult{}
			}
		}
	}()

	inspect, err := r.tool(ctx, "docker", "image", "inspect", "--format", "{{.Id}} {{.Os}}/{{.Architecture}}", req.LocalRef)
	if err != nil {
		return res, err
	}
	fields := strings.Fields(string(inspect.Stdout))
	if len(fields) != 2 || fields[1] != "linux/"+r.arch {
		return res, fmt.Errorf("image is not native linux/%s", r.arch)
	}
	imageID := fields[0]
	if _, err := r.tool(ctx, "docker", "tag", imageID, runTag); err != nil {
		return res, fmt.Errorf("tag the image for the run: %w", err)
	}
	cleanups = append(cleanups, func(c context.Context) error {
		proof, e := r.tool(c, "docker", "image", "inspect", "--format", "{{.Id}} {{.Os}}/{{.Architecture}}", runTag)
		if e != nil {
			if strings.Contains(e.Error(), "No such image") {
				return nil
			}
			return e
		}
		fields := strings.Fields(string(proof.Stdout))
		if len(fields) != 2 || fields[0] != imageID || fields[1] != "linux/"+r.arch {
			return fmt.Errorf("retained per-run image tag: its exact native identity changed")
		}
		_, e = r.tool(c, "docker", "image", "rm", runTag)
		return e
	})
	archive := filepath.Join(dir, "image.tar")
	if err := imageexport.Save(ctx, func(c context.Context, args []string) ([]byte, error) {
		out, e := r.tool(c, "docker", args...)
		return out.Stdout, e
	}, runTag, imageID, r.arch, archive); err != nil {
		return res, fmt.Errorf("save the image: %w", err)
	}
	// Registered before the load: a load that fails half way may still have
	// imported the image into some node.
	cleanups = append(cleanups, func(c context.Context) error { return r.removeFromNodes(c, req.Identity, runTag) })
	if _, err := r.tool(ctx, "kind", "load", "image-archive", archive, "--name", req.Identity.ClusterName); err != nil {
		return res, fmt.Errorf("load the image into the nodes: %w", err)
	}

	ns := r.set.controlNamespace
	pod := r.buildPod(id, runTag, req)
	cleanups = append(cleanups, func(c context.Context) error {
		pr := &podProbeRunner{pods: r.pods, set: r.set}
		return pr.cleanup(c, ns, pod.Name)
	})
	if _, err := r.pods.Create(ctx, ns, pod); err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		return res, fmt.Errorf("create the run pod in %s: %w", ns, err)
	}
	return r.await(ctx, ns, pod.Name, req)
}

// removeFromNodes removes the per-run tag from every node container of the
// environment. A node that does not know the tag is fine.
func (r *ClusterImageRunner) removeFromNodes(ctx context.Context, id model.Identity, tag string) error {
	nodes := id.ContainerIDs
	if len(nodes) == 0 {
		// The identity recorded no node containers: ask the engine for the
		// containers kind labelled with this cluster's name.
		res, err := r.tools.Run(ctx, ToolCall{Exe: "docker", Args: []string{"ps", "-q", "--filter", "label=io.x-k8s.kind.cluster=" + id.ClusterName}})
		if err != nil {
			return fmt.Errorf("find the cluster's nodes: %w", err)
		}
		nodes = strings.Fields(string(res.Stdout))
	}
	var errs []error
	for _, node := range nodes {
		if !nodeContainerPattern.MatchString(node) {
			continue
		}
		res, err := r.tools.Run(ctx, ToolCall{Exe: "docker", Args: []string{"exec", node, "crictl", "rmi", tag}})
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("remove %s from node %s: %w", tag, node[:12], err))
		case res.ExitCode != 0 && !strings.Contains(strings.ToLower(string(res.Stderr)), "not found"):
			errs = append(errs, fmt.Errorf("remove %s from node %s: exit %d: %s", tag, node[:12], res.ExitCode, shortenLine(string(res.Stderr))))
		}
	}
	return errors.Join(errs...)
}

func (r *ClusterImageRunner) buildPod(run, image string, req ImageRunRequest) *corev1.Pod {
	no := false
	grace := int64(1)
	deadline := int64((req.Timeout + imageRunStartWait + 10*time.Second) / time.Second)
	labels := map[string]string{
		labelManagedBy: "junkloud", LabelProbeComponent: ProbeComponent, LabelProbeRun: run,
	}
	if req.Identity.ID != "" {
		labels[LabelProbeEnv] = req.Identity.ID
	}
	if req.ProblemID != "" && len(req.ProblemID) <= 63 {
		labels[LabelProbeProblem] = req.ProblemID
	}
	c := corev1.Container{
		Name:            "app",
		Image:           image,
		ImagePullPolicy: corev1.PullNever,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		},
		SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &no, Privileged: &no},
	}
	if req.Args != nil {
		c.Args = req.Args
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "jk-image-" + run, Namespace: r.set.controlNamespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			AutomountServiceAccountToken:  &no,
			EnableServiceLinks:            &no,
			TerminationGracePeriodSeconds: &grace,
			ActiveDeadlineSeconds:         &deadline,
			SecurityContext:               &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Containers:                    []corev1.Container{c},
		},
	}
}

var startFailureMarkers = []string{"executable file not found", "no such file or directory", "exec format error", "permission denied", "not a directory", "is a directory"}

func startFailure(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range startFailureMarkers {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// await follows the pod until it ends, or (serving) is up and probed.
func (r *ClusterImageRunner) await(ctx context.Context, ns, name string, req ImageRunRequest) (ImageRunResult, error) {
	clock := r.set.clock
	start := clock.Now()
	var runningSince time.Time
	for {
		if err := ctx.Err(); err != nil {
			return ImageRunResult{}, err
		}
		pod, err := r.pods.Get(ctx, ns, name)
		if err != nil {
			if ctx.Err() != nil {
				return ImageRunResult{}, ctx.Err()
			}
			return ImageRunResult{}, fmt.Errorf("read the run pod: %w", err)
		}
		var cs *corev1.ContainerStatus
		if len(pod.Status.ContainerStatuses) > 0 {
			cs = &pod.Status.ContainerStatuses[0]
		}
		switch {
		case cs != nil && cs.State.Terminated != nil:
			t := cs.State.Terminated
			if t.Reason == "StartError" || t.Reason == "ContainerCannotRun" {
				return ImageRunResult{StartError: strings.TrimSpace(t.Message)}, nil
			}
			out, lerr := r.readOutput(ctx, ns, name)
			if lerr != nil {
				return ImageRunResult{}, lerr
			}
			return ImageRunResult{Completed: true, ExitCode: int(t.ExitCode), Output: out}, nil
		case cs != nil && cs.State.Waiting != nil:
			w := cs.State.Waiting
			switch w.Reason {
			case "ErrImageNeverPull", "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
				return ImageRunResult{}, fmt.Errorf("the per-run image is not on the node (%s): %s", w.Reason, shortenLine(w.Message))
			case "CreateContainerConfigError", "CreateContainerError", "RunContainerError":
				if startFailure(w.Message) {
					return ImageRunResult{StartError: strings.TrimSpace(w.Message)}, nil
				}
				return ImageRunResult{}, fmt.Errorf("the container could not be created (%s): %s", w.Reason, shortenLine(w.Message))
			}
		case cs != nil && cs.State.Running != nil:
			if runningSince.IsZero() {
				runningSince = clock.Now()
			}
			if req.Serve != nil && pod.Status.PodIP != "" {
				return r.serve(ctx, ns, name, pod.Status.PodIP, runningSince, req)
			}
			if clock.Now().Sub(runningSince) >= req.Timeout {
				return ImageRunResult{TimedOut: true}, nil
			}
		}
		if runningSince.IsZero() && clock.Now().Sub(start) >= imageRunStartWait {
			return ImageRunResult{}, fmt.Errorf("the run pod did not start within %s (phase %s)", imageRunStartWait, pod.Status.Phase)
		}
		if err := clock.Sleep(ctx, imageRunPollGap); err != nil {
			return ImageRunResult{}, err
		}
	}
}

func (r *ClusterImageRunner) readOutput(ctx context.Context, ns, name string) (string, error) {
	out, err := r.logs.Logs(ctx, ns, name, "app", LogOptions{LimitBytes: imageRunLogLimit})
	if err != nil && !errors.Is(err, ErrContainerNotStarted) {
		return "", fmt.Errorf("read the run pod's output: %w", err)
	}
	return out, nil
}

// serve requests the running container until it answers or the run timeout
// ends; the last probe result is the verdict.
func (r *ClusterImageRunner) serve(ctx context.Context, ns, name, ip string, since time.Time, req ImageRunRequest) (ImageRunResult, error) {
	clock := r.set.clock
	deadline := since.Add(req.Timeout)
	var last ProbeResult
	for {
		pr := ProbeRequest{
			Protocol: ProbeHTTP, Namespace: ns, Host: ip, Port: req.Serve.Port, HTTPHost: ip,
			Path: req.Serve.Path, ExpectStatus: req.Serve.Status, BodyMarker: req.Serve.BodyMarker,
			Attempts: 2, Timeout: 2 * time.Second, Deadline: deadline.Add(probeFloor),
			EnvironmentID: req.Identity.ID, ProblemID: req.ProblemID,
		}
		var err error
		last, err = r.probes.Probe(ctx, pr)
		if err != nil {
			return ImageRunResult{}, err
		}
		if last.Connected && last.HTTPStatus == req.Serve.Status && last.BodyMarker != "" {
			return ImageRunResult{Probe: last}, nil
		}
		// The container may have exited meanwhile: that is the better explanation.
		if pod, gerr := r.pods.Get(ctx, ns, name); gerr == nil && len(pod.Status.ContainerStatuses) > 0 {
			if t := pod.Status.ContainerStatuses[0].State.Terminated; t != nil {
				return ImageRunResult{Completed: true, ExitCode: int(t.ExitCode)}, nil
			}
		}
		if !clock.Now().Add(imageRunPollGap).Before(deadline) {
			return ImageRunResult{Probe: last}, nil
		}
		if err := clock.Sleep(ctx, imageRunPollGap); err != nil {
			return ImageRunResult{}, err
		}
	}
}
