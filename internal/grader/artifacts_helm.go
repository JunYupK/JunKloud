package grader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/JunYupK/JunKloud/internal/model"
)

// helmArgs are the Args of a "helm" criterion (Target stays empty). The
// criterion proves a Helm RELEASE, not just objects: a chart's manifests
// applied with kubectl and no release record fail it.
//
//	release:         release name (required)
//	namespace:       release namespace (default the problem namespace)
//	status:          required release status (default "deployed")
//	chart:           {name?, version?, app_version?} of the installed chart
//	min_revision:    the release must have been upgraded/rolled back to at
//	                 least this revision
//	values:          field requirements (same shape as the field checker's
//	                 fields) over `helm get values -o json`, i.e. the
//	                 user-supplied values; all_values: true uses the computed
//	                 values (--all). Values under keys that look sensitive
//	                 (pass, secret, token, key, cred, cert, auth) are
//	                 compared but never shown, and learned by the redactor.
//	objects:         default true: every object of the release manifest must
//	                 exist and be owned by this release (Helm's
//	                 meta.helm.sh/release-name annotation)
//	workloads_ready: the release's Deployments, StatefulSets and DaemonSets
//	                 must have rolled out (the ready checker's rules)
//	wait:            poll up to the criterion timeout for the status
//
// Everything is read through `helm list/get values -o json` (and `helm get manifest`) run with the environment's
// kubeconfig and context passed as flags (never $KUBECONFIG) and private
// cache directories. helm is a pinned external tool (runtime/versions.lock.yaml).
type helmArgs struct {
	Release        string       `json:"release"`
	Namespace      string       `json:"namespace"`
	Status         string       `json:"status"`
	Chart          *helmChart   `json:"chart"`
	MinRevision    int          `json:"min_revision"`
	Values         []fieldInput `json:"values"`
	AllValues      bool         `json:"all_values"`
	Objects        *bool        `json:"objects"`
	WorkloadsReady bool         `json:"workloads_ready"`
	Wait           bool         `json:"wait"`

	reqs []requirement
}

type helmChart struct {
	Name       *string `json:"name"`
	Version    *string `json:"version"`
	AppVersion *string `json:"app_version"`
}

var (
	helmNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	nsPattern       = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	sensitiveKey    = regexp.MustCompile(`(?i)(pass|secret|token|key|cred|cert|auth|private)`)
)

var helmStatuses = map[string]bool{
	"deployed": true, "superseded": true, "failed": true, "uninstalled": true, "uninstalling": true,
	"pending-install": true, "pending-upgrade": true, "pending-rollback": true, "unknown": true,
}

const maxHelmObjects = 100

func parseHelmArgs(crit model.Criterion) (helmArgs, error) {
	var a helmArgs
	if err := noTarget(crit); err != nil {
		return a, err
	}
	if err := decodeArgs(crit.Args, &a); err != nil {
		return a, err
	}
	if len(a.Release) == 0 || len(a.Release) > 53 || !helmNamePattern.MatchString(a.Release) {
		return a, invalidf("args.release %q is not a release name", a.Release)
	}
	if a.Namespace != "" && (len(a.Namespace) > 63 || !nsPattern.MatchString(a.Namespace)) {
		return a, invalidf("args.namespace %q is not a namespace name", a.Namespace)
	}
	if a.Status == "" {
		a.Status = "deployed"
	}
	if !helmStatuses[a.Status] {
		return a, invalidf("args.status %q is not a Helm release status", a.Status)
	}
	if a.MinRevision < 0 {
		return a, invalidf("args.min_revision cannot be negative")
	}
	if a.Chart != nil && a.Chart.Name == nil && a.Chart.Version == nil && a.Chart.AppVersion == nil {
		return a, invalidf("args.chart is empty")
	}
	if a.AllValues && len(a.Values) == 0 {
		return a, invalidf("args.all_values needs args.values")
	}
	if len(a.Values) > 0 {
		reqs, err := compileRequirements("args.values", a.Values)
		if err != nil {
			return a, err
		}
		a.reqs = reqs
	}
	return a, nil
}

func (a helmArgs) wantObjects() bool { return a.Objects == nil || *a.Objects }

type helmRelease struct {
	status     string
	chart      string
	version    string
	appVersion string
	revision   int
}

// helm runs one helm command against the environment's cluster.
func (c *ArtifactChecker) helm(ctx context.Context, ns string, args ...string) (ToolResult, error) {
	dir, err := c.scratchDir()
	if err != nil {
		return ToolResult{}, fmt.Errorf("helm scratch directory: %w", err)
	}
	full := append([]string{"--kubeconfig", c.cfg.KubeconfigPath, "--kube-context", c.cfg.Context, "-n", ns}, args...)
	return c.cfg.Tools.Run(ctx, ToolCall{Exe: c.cfg.HelmBin, Args: full, Env: []string{
		"HELM_CACHE_HOME=" + dir + "/cache", "HELM_CONFIG_HOME=" + dir + "/config", "HELM_DATA_HOME=" + dir + "/data",
	}})
}

func (c *ArtifactChecker) checkHelm(ctx context.Context, t Target, crit model.Criterion) (model.CheckResult, error) {
	a, err := parseHelmArgs(crit)
	if err != nil {
		return model.CheckResult{}, err
	}
	if c.cfg.Tools == nil || c.cfg.KubeconfigPath == "" || c.cfg.Context == "" {
		return model.CheckResult{}, notConfigured("helm checks need the helm CLI and the environment's kubeconfig and context")
	}
	ns := a.Namespace
	if ns == "" {
		ns = problemNamespace(t)
	}
	if ns == "" {
		return model.CheckResult{}, invalidf("no namespace: set args.namespace or the problem namespace")
	}
	if err := c.learnNamespaceSecrets(ctx, ns); err != nil {
		return model.CheckResult{}, err
	}
	start := c.set.clock.Now()
	expected := map[string]any{"release": a.Release, "namespace": ns, "status": a.Status}
	if a.Chart != nil {
		expected["chart"] = a.Chart
	}

	var rel helmRelease
	found := false
	err = c.pollUntil(ctx, timeoutFor(crit, c.set), a.Wait, func(ctx context.Context) (bool, error) {
		var err error
		rel, found, err = c.helmStatus(ctx, ns, a.Release)
		return err == nil && found && rel.status == a.Status, err
	})
	if err != nil {
		return model.CheckResult{}, err
	}
	if !found {
		return c.verdictResult(start, false,
			fmt.Sprintf("no Helm release named %q in namespace %s (objects applied without helm install/upgrade are not a release)", a.Release, ns),
			expected, map[string]any{"release_found": false}), nil
	}

	actual := map[string]any{"release_found": true, "status": rel.status, "chart": rel.chart, "chart_version": rel.version,
		"app_version": rel.appVersion, "revision": rel.revision}
	var failures []string
	add := func(f string, v ...any) { failures = append(failures, fmt.Sprintf(f, v...)) }
	if rel.status != a.Status {
		add("release %s is %s, want %s", a.Release, rel.status, a.Status)
	}
	if ch := a.Chart; ch != nil {
		if ch.Name != nil && rel.chart != *ch.Name {
			add("release uses chart %q, want %q", rel.chart, *ch.Name)
		}
		if ch.Version != nil && rel.version != *ch.Version {
			add("chart version is %q, want %q", rel.version, *ch.Version)
		}
		if ch.AppVersion != nil && rel.appVersion != *ch.AppVersion {
			add("chart appVersion is %q, want %q", rel.appVersion, *ch.AppVersion)
		}
	}
	if a.MinRevision > 0 {
		expected["min_revision"] = a.MinRevision
		if rel.revision < a.MinRevision {
			add("release is at revision %d, want at least %d", rel.revision, a.MinRevision)
		}
	}
	if len(a.reqs) > 0 {
		f, vexp, vact, err := c.helmValues(ctx, a, ns)
		if err != nil {
			return model.CheckResult{}, err
		}
		expected["values"], actual["values"] = vexp, vact
		failures = append(failures, f...)
	}
	if a.wantObjects() || a.WorkloadsReady {
		f, n, err := c.helmObjects(ctx, a, ns, crit)
		if err != nil {
			return model.CheckResult{}, err
		}
		actual["objects"] = n
		failures = append(failures, f...)
	}
	res := c.verdictResult(start, len(failures) == 0, strings.Join(failures, "; "), expected, actual)
	res.Evidence.Generation = int64(rel.revision)
	return res, nil
}

// helmStatus reads the release with `helm list --filter ^name$ -o json` (Helm 4 lists releases of every status by default)
// (name, namespace, revision, status, chart "name-version", app_version): the
// documented JSON that is stable across Helm versions. found=false only when
// helm lists no such release.
func (c *ArtifactChecker) helmStatus(ctx context.Context, ns, name string) (helmRelease, bool, error) {
	res, err := c.helm(ctx, ns, "list", "--filter", "^"+name+"$", "-o", "json")
	if err != nil {
		return helmRelease{}, false, fmt.Errorf("helm list: %w", err)
	}
	if res.ExitCode != 0 {
		return helmRelease{}, false, fmt.Errorf("helm list failed (exit %d): %s", res.ExitCode, shortenLine(string(res.Stderr)))
	}
	var rows []struct {
		Name       string `json:"name"`
		Namespace  string `json:"namespace"`
		Revision   string `json:"revision"`
		Status     string `json:"status"`
		Chart      string `json:"chart"`
		AppVersion string `json:"app_version"`
	}
	if len(bytes.TrimSpace(res.Stdout)) == 0 {
		return helmRelease{}, false, nil // "no results" prints nothing
	}
	if err := json.Unmarshal(res.Stdout, &rows); err != nil {
		return helmRelease{}, false, fmt.Errorf("helm list: unreadable JSON answer")
	}
	for _, r := range rows {
		if r.Name != name {
			continue
		}
		rev, err := strconv.Atoi(r.Revision)
		if err != nil || r.Status == "" {
			return helmRelease{}, false, fmt.Errorf("helm list: unreadable row for release %s", name)
		}
		chart, version := splitChart(r.Chart)
		return helmRelease{status: r.Status, chart: chart, version: version, appVersion: r.AppVersion, revision: rev}, true, nil
	}
	return helmRelease{}, false, nil
}

var chartNameVersion = regexp.MustCompile(`^(.+?)-(v?[0-9]+\.[0-9]+\.[0-9]+.*)$`)

// splitChart splits helm list's "name-version" chart column.
func splitChart(s string) (name, version string) {
	if m := chartNameVersion.FindStringSubmatch(s); m != nil {
		return m[1], m[2]
	}
	return s, ""
}

// helmValues evaluates the values requirements. Sensitive paths are never
// shown.
func (c *ArtifactChecker) helmValues(ctx context.Context, a helmArgs, ns string) ([]string, map[string]any, map[string]any, error) {
	args := []string{"get", "values", a.Release, "-o", "json"}
	if a.AllValues {
		args = append(args, "--all")
	}
	res, err := c.helm(ctx, ns, args...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("helm get values: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, nil, nil, fmt.Errorf("helm get values %s failed (exit %d): %s", a.Release, res.ExitCode, shortenLine(string(res.Stderr)))
	}
	var values map[string]any
	if err := json.Unmarshal(res.Stdout, &values); err != nil {
		return nil, nil, nil, fmt.Errorf("helm get values %s: unreadable JSON answer", a.Release)
	}
	if values == nil {
		values = map[string]any{}
	}
	red := redactorFor(ctx, c.set.redactor)
	exp, act := map[string]any{}, map[string]any{}
	var failures []string
	for _, r := range a.reqs {
		sensitive := sensitiveKey.MatchString(r.path)
		if sensitive {
			if got, ok := lookup(values, r.segs); ok {
				learnScalars(red, got)
			}
			if r.hasEquals {
				learnScalars(red, r.equals)
			}
		}
		v := evalFields(values, []requirement{r})
		if sensitive {
			exp[r.path], act[r.path] = Redacted, Redacted
			if !v.ok {
				failures = append(failures, fmt.Sprintf("values %s does not match the requirement (value not shown)", r.path))
			}
			continue
		}
		for k, e := range asMap(v.expected) {
			exp[k] = e
		}
		for k, e := range asMap(v.actual) {
			act[k] = e
		}
		if !v.ok {
			failures = append(failures, "values "+v.reason)
		}
	}
	return failures, exp, act, nil
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

// learnScalars registers string and numeric leaves with the redactor.
func learnScalars(r *Redactor, v any) {
	switch x := v.(type) {
	case string:
		r.Learn(x)
	case map[string]any:
		for _, e := range x {
			learnScalars(r, e)
		}
	case []any:
		for _, e := range x {
			learnScalars(r, e)
		}
	case json.Number:
		r.Learn(x.String())
	case float64, int, int64:
		r.Learn(fmt.Sprint(x))
	}
}

// helmObjects checks the release manifest's objects in the cluster.
func (c *ArtifactChecker) helmObjects(ctx context.Context, a helmArgs, ns string, crit model.Criterion) ([]string, int, error) {
	if c.cfg.Reader == nil {
		return nil, 0, notConfigured("helm object checks need an object reader")
	}
	res, err := c.helm(ctx, ns, "get", "manifest", a.Release)
	if err != nil {
		return nil, 0, fmt.Errorf("helm get manifest: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, 0, fmt.Errorf("helm get manifest %s failed (exit %d): %s", a.Release, res.ExitCode, shortenLine(string(res.Stderr)))
	}
	docs, err := parseDocuments("yaml", res.Stdout)
	if err != nil {
		return nil, 0, fmt.Errorf("helm get manifest %s: unreadable manifest: %v", a.Release, err)
	}
	red := redactorFor(ctx, c.set.redactor)
	var refs []model.ObjectRef
	for _, d := range docs {
		red.LearnObject(d)
		meta := asMap(d["metadata"])
		if _, hook := asMap(meta["annotations"])["helm.sh/hook"]; hook {
			continue
		}
		name, _ := meta["name"].(string)
		kind, _ := d["kind"].(string)
		apiVersion, _ := d["apiVersion"].(string)
		if name == "" || kind == "" || apiVersion == "" {
			continue
		}
		ns2, _ := meta["namespace"].(string)
		if ns2 == "" {
			ns2 = ns
		}
		refs = append(refs, model.ObjectRef{APIVersion: apiVersion, Kind: kind, Namespace: ns2, Name: name})
	}
	if len(refs) > maxHelmObjects {
		return nil, len(refs), fmt.Errorf("%w: the release has %d objects, more than the %d the grader checks", ErrInsufficientEvidence, len(refs), maxHelmObjects)
	}
	var failures []string
	for _, ref := range refs {
		obj, err := c.obs.get(ctx, ref)
		if errors.Is(err, ErrNotFound) {
			if a.wantObjects() {
				failures = append(failures, fmt.Sprintf("%s %s of release %s not found", ref.Kind, ref.Name, a.Release))
			}
			continue
		}
		if err != nil {
			return nil, len(refs), err
		}
		if a.wantObjects() && !ownedByRelease(obj, a.Release, ns) {
			failures = append(failures, fmt.Sprintf("%s %s is not managed by release %s (it was not created by helm install/upgrade)", ref.Kind, ref.Name, a.Release))
		}
	}
	if a.WorkloadsReady {
		ready := &ReadyChecker{observer{reader: c.cfg.Reader, set: c.set}}
		for _, ref := range refs {
			if ref.APIVersion != "apps/v1" || (ref.Kind != "Deployment" && ref.Kind != "StatefulSet" && ref.Kind != "DaemonSet") {
				continue
			}
			rc := model.Criterion{ID: crit.ID, Kind: model.CriterionReady, Target: ref, Timeout: timeoutFor(crit, c.set)}
			r, err := ready.Check(ctx, Target{}, rc)
			if err != nil {
				return nil, len(refs), err
			}
			if r.Status != model.StatusPass {
				failures = append(failures, fmt.Sprintf("%s %s is not rolled out: %s", ref.Kind, ref.Name, r.Reason))
			}
		}
	}
	return failures, len(refs), nil
}

// ownedByRelease: Helm's ownership metadata on a live object.
func ownedByRelease(obj object, release, ns string) bool {
	meta := metaOf(obj)
	ann := asMap(meta["annotations"])
	lbl := asMap(meta["labels"])
	return ann["meta.helm.sh/release-name"] == release &&
		ann["meta.helm.sh/release-namespace"] == ns &&
		lbl["app.kubernetes.io/managed-by"] == "Helm"
}
