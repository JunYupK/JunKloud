package grader

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
)

// SecurityChecker grades model.CriterionAuthorization and
// model.CriterionSecurityContext: what an identity may actually do, and how
// containers are actually run (spec 8.2, "Auth / Security"). Register it
// with AddSecurityCheckers.
//
// authorization (Target: a v1 ServiceAccount; a missing one is FAIL):
//
//	checks:  [ {verb, resource? (with group?, subresource?, name?), or
//	           path (a non-resource URL), namespace? (default the
//	           ServiceAccount's), cluster_scope? (no namespace: all
//	           namespaces / cluster-scoped), allowed: bool} ] 1..32
//	runs_as: {api_version, kind, name, namespace?, identity? "spec"|"token"}
//	         a Pod or workload whose pods must really run as the
//	         ServiceAccount: every running pod's spec.serviceAccountName
//	         (default "default") must be it; with identity "token" the
//	         pod's mounted credential is read (exec) and authenticated by the
//	         cluster (TokenReview) and must belong to the ServiceAccount. The
//	         token itself is never stored.
//
// Permissions come from the cluster's own authorizer (SubjectAccessReview
// for system:serviceaccount:<ns>:<name> with its service account groups),
// for the verbs that must be allowed AND for those that must not: a ServiceAccount
// with more rights than the problem allows fails.
//
// security-context is described in security_context.go.
type SecurityChecker struct {
	cfg SecurityConfig
	obs observer
	set settings
}

// SecurityConfig wires the checker; every field is optional and a missing
// dependency makes the criteria that need it ERROR (not configured).
type SecurityConfig struct {
	Reader ObjectReader
	// Pods runs exec in containers (runs_as identity token, behavior).
	Pods PodAccess
	// Access asks the authorizer (SubjectAccessReview, TokenReview).
	Access AccessReviewer
}

// NewSecurityChecker returns the checker.
func NewSecurityChecker(cfg SecurityConfig, opts ...Option) *SecurityChecker {
	set := newSettings(opts)
	return &SecurityChecker{cfg: cfg, obs: observer{reader: cfg.Reader, set: set}, set: set}
}

// NewClusterSecurityChecker wires a SecurityChecker to the cluster that one
// explicit kubeconfig file and context point at.
func NewClusterSecurityChecker(kubeconfigPath, contextName string, opts ...Option) (*SecurityChecker, error) {
	reader, err := NewClusterReader(kubeconfigPath, contextName)
	if err != nil {
		return nil, err
	}
	k, err := newClusterClients(kubeconfigPath, contextName)
	if err != nil {
		return nil, err
	}
	return NewSecurityChecker(SecurityConfig{Reader: reader, Pods: k, Access: k}, opts...), nil
}

// AddSecurityCheckers registers c for authorization and security-context.
func AddSecurityCheckers(checkers map[string]Checker, c *SecurityChecker) {
	checkers[model.CriterionAuthorization] = c
	checkers[model.CriterionSecurityContext] = c
}

// ValidateCriterion implements ArgsValidator.
func (c *SecurityChecker) ValidateCriterion(crit model.Criterion) error {
	switch crit.Kind {
	case model.CriterionAuthorization:
		_, err := parseAuthzArgs(crit)
		return err
	case model.CriterionSecurityContext:
		_, err := parseSecurityContextArgs(crit)
		return err
	}
	return fmt.Errorf("%w: security checker does not grade kind %q", ErrInvalidCriterion, crit.Kind)
}

// Check implements Checker.
func (c *SecurityChecker) Check(ctx context.Context, t Target, crit model.Criterion) (model.CheckResult, error) {
	switch crit.Kind {
	case model.CriterionAuthorization:
		return c.checkAuthorization(ctx, t, crit)
	case model.CriterionSecurityContext:
		return c.checkSecurityContext(ctx, t, crit)
	}
	return model.CheckResult{}, fmt.Errorf("%w: kind %q", ErrInvalidCriterion, crit.Kind)
}

// ---------------------------------------------------------- authorization

type authzArgs struct {
	Checks []authzCheck `json:"checks"`
	RunsAs *runsAs      `json:"runs_as"`
}

type authzCheck struct {
	Verb         string `json:"verb"`
	Group        string `json:"group"`
	Resource     string `json:"resource"`
	Subresource  string `json:"subresource"`
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	ClusterScope bool   `json:"cluster_scope"`
	Path         string `json:"path"`
	Allowed      *bool  `json:"allowed"`
}

type runsAs struct {
	APIVersion string `json:"api_version"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	Identity   string `json:"identity"`
}

const maxAuthzChecks = 32

var runsAsKinds = map[string]string{
	"Pod": "v1", "Deployment": "apps/v1", "StatefulSet": "apps/v1", "DaemonSet": "apps/v1", "ReplicaSet": "apps/v1", "Job": "batch/v1",
}

func parseAuthzArgs(crit model.Criterion) (authzArgs, error) {
	var a authzArgs
	if err := targetIs(crit, "v1", "ServiceAccount"); err != nil {
		return a, err
	}
	if err := decodeArgs(crit.Args, &a); err != nil {
		return a, err
	}
	if len(a.Checks) == 0 || len(a.Checks) > maxAuthzChecks {
		return a, invalidf("args.checks needs 1..%d entries", maxAuthzChecks)
	}
	for i, ch := range a.Checks {
		bad := func(f string, v ...any) error { return invalidf("args.checks[%d]: %s", i, fmt.Sprintf(f, v...)) }
		switch {
		case ch.Verb == "":
			return a, bad("verb is required")
		case ch.Allowed == nil:
			return a, bad("allowed (true or false) is required")
		case (ch.Resource == "") == (ch.Path == ""):
			return a, bad("give exactly one of resource and path")
		case ch.Path != "" && !strings.HasPrefix(ch.Path, "/"):
			return a, bad("path must start with /")
		case ch.Path != "" && (ch.Group != "" || ch.Subresource != "" || ch.Name != "" || ch.Namespace != "" || ch.ClusterScope):
			return a, bad("a non-resource path takes no group, subresource, name or namespace")
		case ch.ClusterScope && ch.Namespace != "":
			return a, bad("cluster_scope and namespace exclude each other")
		}
	}
	if r := a.RunsAs; r != nil {
		want, ok := runsAsKinds[r.Kind]
		switch {
		case !ok:
			return a, invalidf("args.runs_as.kind %q is not Pod, Deployment, StatefulSet, DaemonSet, ReplicaSet or Job", r.Kind)
		case r.APIVersion != want:
			return a, invalidf("args.runs_as.api_version for %s must be %s", r.Kind, want)
		case r.Name == "":
			return a, invalidf("args.runs_as.name is required")
		}
		switch r.Identity {
		case "":
			a.RunsAs.Identity = "spec"
		case "spec", "token":
		default:
			return a, invalidf("args.runs_as.identity %q is not spec or token", r.Identity)
		}
	}
	return a, nil
}

const saTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

func (c *SecurityChecker) checkAuthorization(ctx context.Context, t Target, crit model.Criterion) (model.CheckResult, error) {
	a, err := parseAuthzArgs(crit)
	if err != nil {
		return model.CheckResult{}, err
	}
	if c.cfg.Reader == nil || c.cfg.Access == nil {
		return model.CheckResult{}, notConfigured("authorization checks need an object reader and an access reviewer")
	}
	ref, err := resolveTarget(t, crit)
	if err != nil {
		return model.CheckResult{}, err
	}
	start := c.set.clock.Now()
	user := "system:serviceaccount:" + ref.Namespace + ":" + ref.Name
	expected := map[string]any{"service_account": user}
	sa, err := c.obs.get(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return verdictAt(c.set, start, false, fmt.Sprintf("ServiceAccount %s/%s not found", ref.Namespace, ref.Name), expected, map[string]any{"exists": false}), nil
	}
	if err != nil {
		return model.CheckResult{}, err
	}
	var failures []string
	actual := map[string]any{}

	if a.RunsAs != nil {
		f, info, err := c.verifyRunsAs(ctx, t, a.RunsAs, ref)
		if err != nil {
			return model.CheckResult{}, err
		}
		expected["runs_as"], actual["runs_as"] = fmt.Sprintf("%s %s as %s", a.RunsAs.Kind, a.RunsAs.Name, a.RunsAs.Identity), info
		failures = append(failures, f...)
	}

	subject := AccessSubject{User: user, Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + ref.Namespace, "system:authenticated"}}
	var exp, act []any
	for _, ch := range a.Checks {
		attrs := AccessAttributes{Verb: ch.Verb, Path: ch.Path}
		desc := ch.Verb + " " + ch.Path
		if ch.Resource != "" {
			attrs.Group, attrs.Resource, attrs.Subresource, attrs.Name = ch.Group, ch.Resource, ch.Subresource, ch.Name
			switch {
			case ch.ClusterScope:
			case ch.Namespace != "":
				attrs.Namespace = ch.Namespace
			default:
				attrs.Namespace = ref.Namespace
			}
			desc = ch.Verb + " " + ch.Resource
			if ch.Subresource != "" {
				desc += "/" + ch.Subresource
			}
			if ch.Name != "" {
				desc += " " + ch.Name
			}
			if ch.ClusterScope {
				desc += " (all namespaces)"
			} else {
				desc += " in " + attrs.Namespace
			}
		}
		dec, err := c.cfg.Access.Review(ctx, subject, attrs)
		if err != nil {
			return model.CheckResult{}, err
		}
		want := *ch.Allowed
		exp = append(exp, map[string]any{"request": desc, "allowed": want})
		act = append(act, map[string]any{"request": desc, "allowed": dec.Allowed, "reason": shortenLine(dec.Reason)})
		switch {
		case dec.Allowed && !want:
			failures = append(failures, fmt.Sprintf("%s must not be allowed for %s, but it is (more privilege than needed)", desc, user))
		case !dec.Allowed && want:
			failures = append(failures, fmt.Sprintf("%s is not allowed for %s", desc, user))
		}
	}
	expected["checks"], actual["checks"] = exp, act
	res := verdictAt(c.set, start, len(failures) == 0, strings.Join(failures, "; "), expected, actual)
	res.Evidence.UID = uidOf(sa)
	return res, nil
}

// verifyRunsAs proves the pods really run as the ServiceAccount.
func (c *SecurityChecker) verifyRunsAs(ctx context.Context, t Target, r *runsAs, sa model.ObjectRef) ([]string, []any, error) {
	ns := r.Namespace
	if ns == "" {
		ns = sa.Namespace
	}
	ref := model.ObjectRef{APIVersion: r.APIVersion, Kind: r.Kind, Namespace: ns, Name: r.Name}
	obj, err := c.obs.get(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return []string{fmt.Sprintf("%s %s not found, so nothing runs as %s", r.Kind, r.Name, sa.Name)}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	pods, err := ownedPods(ctx, c.cfg.Reader, r.Kind, obj)
	if err != nil {
		return nil, nil, err
	}
	running := runningPods(pods)
	if len(running) == 0 {
		reason, err := c.explainNoRunningPods(ctx, pods, r.Kind+" "+r.Name)
		return []string{reason}, nil, err
	}
	var failures []string
	var info []any
	user := "system:serviceaccount:" + sa.Namespace + ":" + sa.Name
	for _, p := range running[:min(len(running), maxBehaviorPods)] {
		got, _ := valueAt(p, "spec", "serviceAccountName").(string)
		if got == "" {
			got = "default"
		}
		entry := map[string]any{"pod": nameOf(p), "service_account": got}
		info = append(info, entry)
		if got != sa.Name {
			failures = append(failures, fmt.Sprintf("pod %s runs as service account %q, not %q", nameOf(p), got, sa.Name))
			continue
		}
		if r.Identity != "token" {
			continue
		}
		f, err := c.verifyToken(ctx, p, user)
		if err != nil {
			return nil, nil, err
		}
		entry["credential_verified"] = f == ""
		if f != "" {
			failures = append(failures, f)
		}
	}
	return failures, info, nil
}

// verifyToken reads the pod's mounted credential, has the cluster
// authenticate it, and compares the identity. The token is learned by the
// redactor and never stored.
func (c *SecurityChecker) verifyToken(ctx context.Context, pod object, user string) (string, error) {
	if c.cfg.Pods == nil {
		return "", notConfigured("identity token checks need pod access")
	}
	container, ok := pickContainer(pod, "")
	if !ok {
		return "", fmt.Errorf("%w: pod %s has no container", ErrInsufficientEvidence, nameOf(pod))
	}
	res, err := c.cfg.Pods.Exec(ctx, stringAt(pod, "metadata", "namespace"), nameOf(pod), container, []string{"cat", saTokenPath})
	if err != nil {
		return "", fmt.Errorf("%w: cannot read the credential of pod %s (the image needs a `cat` command): %v", ErrInsufficientEvidence, nameOf(pod), err)
	}
	if res.ExitCode != 0 {
		if strings.Contains(strings.ToLower(res.Stderr), "no such file") {
			return fmt.Sprintf("pod %s has no service account credential mounted (is automountServiceAccountToken disabled?)", nameOf(pod)), nil
		}
		return "", fmt.Errorf("%w: reading the credential of pod %s failed (exit %d)", ErrInsufficientEvidence, nameOf(pod), res.ExitCode)
	}
	token := strings.TrimSpace(res.Stdout)
	redactorFor(ctx, c.set.redactor).Learn(token)
	got, authenticated, err := c.cfg.Access.TokenUser(ctx, token)
	if err != nil {
		return "", err
	}
	switch {
	case !authenticated:
		return fmt.Sprintf("the credential of pod %s is not authenticated by the cluster", nameOf(pod)), nil
	case got != user:
		return fmt.Sprintf("the credential of pod %s authenticates as %s, not %s", nameOf(pod), got, user), nil
	}
	return "", nil
}

// runningPods filters pods that are running and not being deleted, sorted by
// name.
func runningPods(pods []object) []object {
	var out []object
	for _, p := range pods {
		if stringAt(p, "status", "phase") == "Running" && metaOf(p)["deletionTimestamp"] == nil {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return nameOf(out[i]) < nameOf(out[j]) })
	return out
}

const maxBehaviorPods = 3

// explainNoRunningPods turns "there is no running pod" into a learner FAIL
// reason when the pods show a learner-caused problem, otherwise an
// insufficient-evidence error.
func (c *SecurityChecker) explainNoRunningPods(ctx context.Context, pods []object, what string) (string, error) {
	msgs, err := pullMessages(ctx, c.cfg.Reader, pods)
	if err != nil {
		return "", err
	}
	cause := classifyRollout(pods, true, causeConfig{preloaded: c.set.preloaded, configured: c.set.preloadedSet, pullMessages: msgs})
	if cause.class == faultLearner {
		return fmt.Sprintf("%s has no running pod: %s", what, cause.text), nil
	}
	return "", fmt.Errorf("%w: %s has no running pod; %s", ErrInsufficientEvidence, what, cause.text)
}

// verdictAt builds a PASS/FAIL result (the SecurityChecker's counterpart of
// ArtifactChecker.verdictResult).
func verdictAt(set settings, start time.Time, ok bool, reason string, expected, actual any) model.CheckResult {
	now := set.clock.Now()
	res := model.CheckResult{Status: model.StatusPass, Evidence: model.Evidence{
		Expected: clampValue(expected), Actual: clampValue(actual), ObservedAt: now.UTC(), Duration: now.Sub(start),
	}}
	if !ok {
		res.Status, res.Reason = model.StatusFail, reason
	}
	return res
}
