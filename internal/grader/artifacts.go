package grader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
)

// ArtifactChecker grades the criterion kinds that look at what a learner
// built or submitted rather than at one API object: files in the session
// workspace, container images, Helm releases, Kustomize overlays, pod logs
// and Jobs/CronJobs (spec 8.2). It is registered for all of them with
// AddArtifactCheckers.
//
// Args per kind (all strict: an unknown key is an author error, graded ERROR
// with zero reads). The kinds file, image, helm and kustomize do not use
// Target (leave it empty); logs, job, cronjob-template and cronjob-schedule
// take the object they are about as Target. Criterion.Target.Namespace and
// Args namespaces default to the problem namespace.
//
//	file              artifacts_file.go
//	logs              artifacts_file.go
//	image             artifacts_image.go
//	helm              artifacts_helm.go
//	kustomize         artifacts_kustomize.go
//	job               artifacts_job.go
//	cronjob-template  artifacts_job.go
//	cronjob-schedule  artifacts_job.go
//
// FAIL versus ERROR follows spec 8.1 and the Task 6 conventions: a missing
// or wrong learner artifact is FAIL with the observation; a tool that cannot
// run, a cluster that cannot answer, or evidence that does not show whose
// fault an outcome is, is ERROR.
type ArtifactChecker struct {
	cfg ArtifactConfig
	obs observer
	set settings

	scratchOnce sync.Once
	scratch     string
	scratchErr  error
}

// ArtifactConfig wires the checker to the outside world. Every field is
// optional; a criterion whose kind needs a missing dependency is graded ERROR
// ("not configured"), never FAIL.
type ArtifactConfig struct {
	// Reader reads API objects (all kinds).
	Reader ObjectReader
	// Tools runs docker, helm and kubectl (image, helm, kustomize).
	Tools ToolRunner
	// Pods reads logs and runs exec (logs).
	Pods PodAccess
	// Images runs a learner's image in the cluster (image run checks).
	Images ImageRunner
	// KubeconfigPath and Context are the environment's explicit kubeconfig
	// and context, passed to helm as flags. They are never taken from the
	// process environment.
	KubeconfigPath string
	Context        string
	// Binaries; defaults "docker", "helm" and "kubectl" from PATH.
	DockerBin  string
	HelmBin    string
	KubectlBin string
	// InputKey keys the hashes CaptureInputs makes of anything that may hold
	// secrets (see CaptureInputs). It should be random, per installation and
	// stable across runs.
	InputKey []byte
}

// NewArtifactChecker returns the checker. Pass the same options as to the
// engine (clock, redactor, default timeout, poll interval).
func NewArtifactChecker(cfg ArtifactConfig, opts ...Option) *ArtifactChecker {
	set := newSettings(opts)
	if len(cfg.InputKey) == 0 {
		cfg.InputKey = set.inputKey
	}
	if cfg.DockerBin == "" {
		cfg.DockerBin = "docker"
	}
	if cfg.HelmBin == "" {
		cfg.HelmBin = "helm"
	}
	if cfg.KubectlBin == "" {
		cfg.KubectlBin = "kubectl"
	}
	return &ArtifactChecker{cfg: cfg, obs: observer{reader: cfg.Reader, set: set}, set: set}
}

// AddArtifactCheckers registers c for file, image, helm, kustomize, logs,
// job, cronjob-template and cronjob-schedule.
func AddArtifactCheckers(checkers map[string]Checker, c *ArtifactChecker) {
	for _, kind := range artifactKinds {
		checkers[kind] = c
	}
}

var artifactKinds = []string{
	model.CriterionFile, model.CriterionImage, model.CriterionHelm, model.CriterionKustomize,
	model.CriterionLogs, model.CriterionJob, model.CriterionCronJobTemplate, model.CriterionCronJobSchedule,
}

// Close removes the scratch directory helm uses for its caches.
func (c *ArtifactChecker) Close() error {
	if c.scratch != "" {
		return os.RemoveAll(c.scratch)
	}
	return nil
}

// scratchDir is a private directory for tool caches, created on first use.
func (c *ArtifactChecker) scratchDir() (string, error) {
	c.scratchOnce.Do(func() { c.scratch, c.scratchErr = os.MkdirTemp("", "junkloud-tools-*") })
	return c.scratch, c.scratchErr
}

// Mutates implements MutatingChecker: running an image creates a pod, an
// engine tag and a node image, so such criteria run alone.
func (c *ArtifactChecker) Mutates(crit model.Criterion) bool {
	if crit.Kind != model.CriterionImage {
		return false
	}
	_, ok := crit.Args["run"]
	return ok
}

// ValidateCriterion implements ArgsValidator: strict Args and target rules
// without touching a cluster or a tool.
func (c *ArtifactChecker) ValidateCriterion(crit model.Criterion) error {
	switch crit.Kind {
	case model.CriterionFile:
		_, err := parseFileArgs(crit)
		return err
	case model.CriterionLogs:
		_, err := parseLogsArgs(crit)
		return err
	case model.CriterionImage:
		_, err := parseImageArgs(crit, c.set.defaultTimeout)
		return err
	case model.CriterionHelm:
		_, err := parseHelmArgs(crit)
		return err
	case model.CriterionKustomize:
		_, err := parseKustomizeArgs(crit)
		return err
	case model.CriterionJob:
		_, err := parseJobArgs(crit)
		return err
	case model.CriterionCronJobTemplate:
		_, err := parseCronTemplateArgs(crit)
		return err
	case model.CriterionCronJobSchedule:
		_, err := parseCronScheduleArgs(crit, c.set.defaultTimeout)
		return err
	}
	return fmt.Errorf("%w: artifact checker does not grade kind %q", ErrInvalidCriterion, crit.Kind)
}

// Check implements Checker.
func (c *ArtifactChecker) Check(ctx context.Context, t Target, crit model.Criterion) (model.CheckResult, error) {
	if err := c.ValidateCriterion(crit); err != nil {
		return model.CheckResult{}, err
	}
	switch crit.Kind {
	case model.CriterionFile:
		return c.checkFile(ctx, t, crit)
	case model.CriterionLogs:
		return c.checkLogs(ctx, t, crit)
	case model.CriterionImage:
		return c.checkImage(ctx, t, crit)
	case model.CriterionHelm:
		return c.checkHelm(ctx, t, crit)
	case model.CriterionKustomize:
		return c.checkKustomize(ctx, t, crit)
	case model.CriterionJob:
		return c.checkJob(ctx, t, crit)
	case model.CriterionCronJobTemplate:
		return c.checkCronTemplate(ctx, t, crit)
	case model.CriterionCronJobSchedule:
		return c.checkCronSchedule(ctx, t, crit)
	}
	return model.CheckResult{}, fmt.Errorf("%w: kind %q", ErrInvalidCriterion, crit.Kind)
}

// ------------------------------------------------------------------ helpers

var errNotConfigured = errors.New("checker is not configured for this")

func notConfigured(what string) error {
	return fmt.Errorf("%w: %s", errNotConfigured, what)
}

// noTarget rejects a Target on kinds that do not use one.
func noTarget(crit model.Criterion) error {
	if crit.Target != (model.ObjectRef{}) {
		return fmt.Errorf("%w: kind %s takes no target (put its settings in args)", ErrInvalidCriterion, crit.Kind)
	}
	return nil
}

// problemNamespace is the namespace a criterion defaults to.
func problemNamespace(t Target) string {
	if t.Problem.Namespace != "" {
		return t.Problem.Namespace
	}
	return t.Problem.Definition.Namespace
}

// pass and fail build results. Callers fill Expected/Actual.
func (c *ArtifactChecker) verdictResult(start time.Time, ok bool, reason string, expected, actual any) model.CheckResult {
	now := c.set.clock.Now()
	res := model.CheckResult{Status: model.StatusPass, Evidence: model.Evidence{
		Expected: clampValue(expected), Actual: clampValue(actual),
		ObservedAt: now.UTC(), Duration: now.Sub(start),
	}}
	if !ok {
		res.Status, res.Reason = model.StatusFail, reason
	}
	return res
}

// pollUntil runs step until it reports done. Without wait it runs once; with
// it, until done or timeout, sleeping the poll interval between runs. The
// last step decides: pollUntil returns after the timeout without error and
// the caller judges the final observation.
func (c *ArtifactChecker) pollUntil(ctx context.Context, timeout time.Duration, wait bool, step func(ctx context.Context) (bool, error)) error {
	clock := c.set.clock
	deadline := clock.Now().Add(timeout)
	for {
		done, err := step(ctx)
		if err != nil {
			return err
		}
		now := clock.Now()
		if done || !wait || !now.Before(deadline) {
			return nil
		}
		d := c.set.pollInterval
		if rem := deadline.Sub(now); rem < d {
			d = rem
		}
		if err := clock.Sleep(ctx, d); err != nil {
			return err
		}
	}
}

// learnNamespaceSecrets registers every Secret value of ns with the
// redactor, so anything a log, a rendered manifest or a Helm value shows of
// them is scrubbed from the evidence.
func (c *ArtifactChecker) learnNamespaceSecrets(ctx context.Context, ns string) error {
	if c.cfg.Reader == nil || ns == "" {
		return nil
	}
	secrets, err := c.cfg.Reader.List(ctx, ListQuery{APIVersion: "v1", Kind: "Secret", Namespace: ns})
	if err != nil {
		return fmt.Errorf("list secrets for redaction: %w", err)
	}
	r := redactorFor(ctx, c.set.redactor)
	for _, s := range secrets {
		r.LearnObject(s)
	}
	return nil
}

// NewClusterArtifactChecker wires an ArtifactChecker to the cluster that one
// explicit kubeconfig file and context point at (never $KUBECONFIG), the
// docker/helm/kubectl CLIs found on PATH, and, when WithProbeControlNamespace
// is given, an image runner that runs learner images as pods in that
// namespace. probes may be nil unless a criterion runs a serving image.
func NewClusterArtifactChecker(kubeconfigPath, contextName string, probes ProbeRunner, opts ...Option) (*ArtifactChecker, error) {
	reader, err := NewClusterReader(kubeconfigPath, contextName)
	if err != nil {
		return nil, err
	}
	k, err := newClusterClients(kubeconfigPath, contextName)
	if err != nil {
		return nil, err
	}
	tools := ToolRunner(NewToolRunner())
	if set := newSettings(opts); set.tools != nil {
		tools = set.tools
	}
	cfg := ArtifactConfig{Reader: reader, Tools: tools, Pods: k, KubeconfigPath: kubeconfigPath, Context: contextName}
	if newSettings(opts).controlNamespace != "" {
		runner, err := newClusterImageRunner(tools, kubePods{k.cs}, k, probes, opts...)
		if err != nil {
			return nil, err
		}
		cfg.Images = runner
	}
	return NewArtifactChecker(cfg, opts...), nil
}
