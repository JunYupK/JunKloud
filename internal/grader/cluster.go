package grader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
)

// This file is the grader's access to the live behaviour of pods and to the
// cluster's authorizer: logs, exec, SubjectAccessReview and TokenReview. All
// of it goes through the environment's own kubeconfig and an explicit
// context (never $KUBECONFIG), like the object reader.

// Limits for what the grader reads from a running container.
const (
	maxLogBytes  = 1 << 20
	maxExecBytes = 64 << 10
)

// ErrContainerNotStarted: the container has produced no log stream yet (it
// is waiting to start). It says nothing about whose fault that is.
var ErrContainerNotStarted = errors.New("container has not started")

// LogOptions bounds one log read.
type LogOptions struct {
	TailLines  int64
	Previous   bool
	LimitBytes int64
}

// ExecResult is the outcome of a command run in a container. A command that
// ran and failed has a non-zero ExitCode and a nil error from Exec.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// PodAccess reads what running containers say and do.
type PodAccess interface {
	// Logs returns the container's log (at most opts.LimitBytes). It returns
	// ErrContainerNotStarted while the container has no log stream.
	Logs(ctx context.Context, namespace, pod, container string, opts LogOptions) (string, error)
	// Exec runs command in the container without a shell or a TTY. An error
	// means the command could not be run at all (for example the container
	// has no such executable); a command that ran and exited non-zero is a
	// result.
	Exec(ctx context.Context, namespace, pod, container string, command []string) (ExecResult, error)
}

// AccessSubject is the identity whose permissions are reviewed.
type AccessSubject struct {
	User   string
	Groups []string
}

// AccessAttributes is one request to authorize. For a resource request
// Resource is set; for a non-resource URL, Path.
type AccessAttributes struct {
	Namespace   string
	Verb        string
	Group       string
	Resource    string
	Subresource string
	Name        string
	Path        string
}

// AccessDecision is the authorizer's answer.
type AccessDecision struct {
	Allowed bool
	Reason  string
}

// AccessReviewer asks the cluster's own authorizer what an identity may do
// and who a credential really is.
type AccessReviewer interface {
	// Review is a SubjectAccessReview.
	Review(ctx context.Context, subject AccessSubject, attrs AccessAttributes) (AccessDecision, error)
	// TokenUser is a TokenReview: the authenticated user name of a bearer
	// token, or authenticated=false.
	TokenUser(ctx context.Context, token string) (user string, authenticated bool, err error)
}

// clusterClients implements PodAccess and AccessReviewer on client-go.
type clusterClients struct {
	cs  kubernetes.Interface
	cfg *rest.Config
}

func newClusterClients(kubeconfigPath, contextName string) (*clusterClients, error) {
	cfg, err := restConfig(kubeconfigPath, contextName)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("grader: kubernetes client: %w", err)
	}
	return &clusterClients{cs: cs, cfg: cfg}, nil
}

// Logs implements PodAccess.
func (k *clusterClients) Logs(ctx context.Context, ns, pod, container string, opts LogOptions) (string, error) {
	limit := opts.LimitBytes
	if limit <= 0 || limit > maxLogBytes {
		limit = maxLogBytes
	}
	o := &corev1.PodLogOptions{Container: container, Previous: opts.Previous, LimitBytes: &limit}
	if opts.TailLines > 0 {
		o.TailLines = &opts.TailLines
	}
	stream, err := k.cs.CoreV1().Pods(ns).GetLogs(pod, o).Stream(ctx)
	if err != nil {
		if apierrors.IsBadRequest(err) {
			return "", fmt.Errorf("%w: %v", ErrContainerNotStarted, err)
		}
		return "", fmt.Errorf("read logs of %s/%s: %w", ns, pod, err)
	}
	defer stream.Close()
	b, err := io.ReadAll(io.LimitReader(stream, limit))
	if err != nil {
		return "", fmt.Errorf("read logs of %s/%s: %w", ns, pod, err)
	}
	return string(b), nil
}

// Exec implements PodAccess.
func (k *clusterClients) Exec(ctx context.Context, ns, pod, container string, command []string) (ExecResult, error) {
	req := k.cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod).SubResource("exec")
	req.VersionedParams(&corev1.PodExecOptions{
		Container: container, Command: command, Stdout: true, Stderr: true,
	}, scheme.ParameterCodec)
	ex, err := remotecommand.NewSPDYExecutor(k.cfg, http.MethodPost, req.URL())
	if err != nil {
		return ExecResult{}, fmt.Errorf("exec in %s/%s: %w", ns, pod, err)
	}
	var stdout, stderr limitedBuffer
	stdout.limit, stderr.limit = maxExecBytes, maxExecBytes
	err = ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
	res := ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	var exit utilexec.CodeExitError
	switch {
	case err == nil:
		return res, nil
	case errors.As(err, &exit):
		res.ExitCode = exit.Code
		return res, nil
	default:
		return res, fmt.Errorf("exec in %s/%s: %w", ns, pod, err)
	}
}

// limitedBuffer keeps at most limit bytes and silently drops the rest.
type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }

// Review implements AccessReviewer.
func (k *clusterClients) Review(ctx context.Context, s AccessSubject, a AccessAttributes) (AccessDecision, error) {
	spec := authzv1.SubjectAccessReviewSpec{User: s.User, Groups: s.Groups}
	if a.Resource != "" {
		spec.ResourceAttributes = &authzv1.ResourceAttributes{
			Namespace: a.Namespace, Verb: a.Verb, Group: a.Group, Resource: a.Resource,
			Subresource: a.Subresource, Name: a.Name,
		}
	} else {
		spec.NonResourceAttributes = &authzv1.NonResourceAttributes{Path: a.Path, Verb: a.Verb}
	}
	out, err := k.cs.AuthorizationV1().SubjectAccessReviews().Create(ctx,
		&authzv1.SubjectAccessReview{Spec: spec}, metav1.CreateOptions{})
	if err != nil {
		return AccessDecision{}, fmt.Errorf("subject access review: %w", err)
	}
	return AccessDecision{Allowed: out.Status.Allowed, Reason: out.Status.Reason}, nil
}

// TokenUser implements AccessReviewer.
func (k *clusterClients) TokenUser(ctx context.Context, token string) (string, bool, error) {
	out, err := k.cs.AuthenticationV1().TokenReviews().Create(ctx,
		&authnv1.TokenReview{Spec: authnv1.TokenReviewSpec{Token: token}}, metav1.CreateOptions{})
	if err != nil {
		return "", false, fmt.Errorf("token review: %w", err)
	}
	if !out.Status.Authenticated {
		return "", false, nil
	}
	return out.Status.User.Username, true, nil
}
