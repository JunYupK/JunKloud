package grader

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
)

// InputCapturer fingerprints what a learner submitted, so a later grading
// request can tell a technical retry (nothing changed) from a resubmission
// (an input changed), spec 9.1. *ArtifactChecker implements it.
type InputCapturer interface {
	CaptureInputs(ctx context.Context, t Target) (map[string]string, error)
}

// CaptureInputs fingerprints the submission's inputs named by the problem's
// criteria and nothing else:
//
//	object:<apiVersion>/<Kind>/<ns>/<name>   every API object a criterion
//	    targets (and, for authorization, the RoleBindings/ClusterRoleBindings
//	    granting the ServiceAccount with the Roles they reference; for
//	    network-policy the namespace's NetworkPolicies). Value:
//	    "uid=<uid>;gen=<generation>;input=<sha256 of the spec>" or "absent".
//	    Only the object's spec (or, for kinds without one, its payload fields)
//	    is hashed: status, resourceVersion, managedFields, annotations the
//	    controllers write and a Pod's spec.nodeName are not. A Secret's
//	    payload is covered only through a keyed hash (below), else omitted.
//	file:<path>      workspace file content ("absent", "outside-workspace",
//	                 or a digest); archive:<path> likewise
//	image:<ref>      "id=<image ID>" from the Docker engine ("absent" if none)
//	helm:<ns>/<release>   "revision=<n>;chart=<name-version>[;values=<mac>]"
//	                 or "absent"; the release status is not an input
//	kustomize:<dir>, kustomize-base:<dir>   keyed digest of the directory tree
//
// Digests of anything that may carry secrets (files, trees, Helm values,
// Secret payloads) are HMAC-SHA256 values keyed with ArtifactConfig.InputKey,
// prefixed "mac:". A plain unkeyed hash of a low-entropy Secret would be a
// dictionary lookup, so without a key Secret payloads and Helm values are
// omitted, and file/tree digests are plain "sha256:" digests (compose with a
// key in production). The result is deterministic for identical inputs.
// Missing objects and files are recorded, not errors; an API or tool failure
// is an error.
func (c *ArtifactChecker) CaptureInputs(ctx context.Context, t Target) (map[string]string, error) {
	out := map[string]string{}
	for _, crit := range t.Problem.Definition.Criteria {
		if err := c.captureCriterion(ctx, t, crit, out); err != nil {
			return nil, fmt.Errorf("fingerprint inputs of criterion %q: %w", crit.ID, err)
		}
	}
	return out, nil
}

func (c *ArtifactChecker) captureCriterion(ctx context.Context, t Target, crit model.Criterion, out map[string]string) error {
	switch crit.Kind {
	case model.CriterionResource, model.CriterionField, model.CriterionReady, model.CriterionHTTP, model.CriterionTCP,
		model.CriterionLogs, model.CriterionJob, model.CriterionCronJobTemplate, model.CriterionCronJobSchedule,
		model.CriterionSecurityContext:
		return c.captureTarget(ctx, t, crit, out)
	case model.CriterionNetworkPolicy:
		if err := c.captureTarget(ctx, t, crit, out); err != nil {
			return err
		}
		return c.capturePolicies(ctx, problemNamespaceOf(t, crit), out)
	case model.CriterionAuthorization:
		return c.captureAuthorization(ctx, t, crit, out)
	case model.CriterionFile:
		a, err := parseFileArgs(crit)
		if err != nil {
			return err
		}
		out["file:"+a.Path] = c.fileInput(t.Workspace, a.Path)
		return nil
	case model.CriterionImage:
		return c.captureImage(ctx, t, crit, out)
	case model.CriterionHelm:
		return c.captureHelm(ctx, t, crit, out)
	case model.CriterionKustomize:
		return c.captureKustomize(ctx, t, crit, out)
	}
	return nil
}

func problemNamespaceOf(t Target, crit model.Criterion) string {
	if crit.Target.Namespace != "" {
		return crit.Target.Namespace
	}
	return problemNamespace(t)
}

func objectKey(ref model.ObjectRef) string {
	return fmt.Sprintf("object:%s/%s/%s/%s", ref.APIVersion, ref.Kind, ref.Namespace, ref.Name)
}

func (c *ArtifactChecker) captureTarget(ctx context.Context, t Target, crit model.Criterion, out map[string]string) error {
	ref, err := resolveTarget(t, crit)
	if err != nil {
		return err
	}
	return c.captureObject(ctx, ref, out)
}

// captureObject records one object's fingerprint (once).
func (c *ArtifactChecker) captureObject(ctx context.Context, ref model.ObjectRef, out map[string]string) error {
	key := objectKey(ref)
	if _, done := out[key]; done {
		return nil
	}
	if c.cfg.Reader == nil {
		return notConfigured("input fingerprints need an object reader")
	}
	obj, err := c.obs.get(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		out[key] = "absent"
		return nil
	}
	if err != nil {
		return err
	}
	out[key] = c.fingerprint(obj)
	return nil
}

// fingerprint is "uid=..;gen=..;input=.." over the submitted part of obj.
func (c *ArtifactChecker) fingerprint(obj object) string {
	head := fmt.Sprintf("uid=%s;gen=%d", uidOf(obj), generationOf(obj))
	if isSecret(obj) {
		if mac := c.mac("secret", canonicalJSON(map[string]any{"data": obj["data"], "stringData": obj["stringData"], "type": obj["type"]})); mac != "" {
			return head + ";data=" + mac
		}
		return head
	}
	return head + ";input=" + inputHash(submittedView(obj))
}

// submittedView drops the fields controllers fill in after the learner
// applied the object, so they do not count as the learner's input.
func submittedView(obj object) object {
	kind, _ := obj["kind"].(string)
	var drop string
	switch kind {
	case "Pod":
		drop = "nodeName"
	case "PersistentVolumeClaim":
		drop = "volumeName"
	default:
		return obj
	}
	spec, ok := obj["spec"].(map[string]any)
	if !ok {
		return obj
	}
	cp := make(map[string]any, len(spec))
	for k, v := range spec {
		if k != drop {
			cp[k] = v
		}
	}
	view := make(map[string]any, len(obj))
	for k, v := range obj {
		view[k] = v
	}
	view["spec"] = cp
	return view
}

func canonicalJSON(v any) []byte {
	raw, _ := json.Marshal(v) // maps marshal with sorted keys
	return raw
}

// mac is the keyed digest of data ("" when no key is configured).
func (c *ArtifactChecker) mac(domain string, data []byte) string {
	if len(c.cfg.InputKey) == 0 {
		return ""
	}
	h := hmac.New(sha256.New, c.cfg.InputKey)
	fmt.Fprintf(h, "junkloud-input/%s/%d/", domain, len(data))
	h.Write(data)
	return "mac:" + hex.EncodeToString(h.Sum(nil))
}

// digest is a keyed digest when a key exists, else a plain sha256.
func (c *ArtifactChecker) digest(domain string, data []byte) string {
	if m := c.mac(domain, data); m != "" {
		return m
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (c *ArtifactChecker) fileInput(workspace, p string) string {
	if workspace == "" {
		return "absent"
	}
	real, err := problem.ResolveAsset(workspace, p)
	switch {
	case errors.Is(err, problem.ErrAssetEscape):
		return "outside-workspace"
	case err != nil:
		return "absent"
	}
	st, err := os.Stat(real)
	if err != nil {
		return "absent"
	}
	if st.IsDir() {
		sum, err := hashTree(real)
		if err != nil {
			return "unreadable"
		}
		return "tree:" + c.digest("tree", []byte(sum))
	}
	data, err := readLimited(real, maxFileBytes)
	switch {
	case errors.Is(err, errTooLarge):
		return fmt.Sprintf("too-large:%d", st.Size())
	case err != nil:
		return "unreadable"
	}
	return c.digest("file", data)
}

func (c *ArtifactChecker) captureImage(ctx context.Context, t Target, crit model.Criterion, out map[string]string) error {
	a, err := parseImageArgs(crit, c.set.defaultTimeout)
	if err != nil {
		return err
	}
	if a.Reference != "" {
		if c.cfg.Tools == nil {
			return notConfigured("image fingerprints need the docker CLI")
		}
		img, found, err := c.inspectImage(ctx, a.Reference)
		if err != nil {
			return err
		}
		out["image:"+a.Reference] = map[bool]string{true: "id=" + img.ID, false: "absent"}[found]
	}
	if a.Archive != nil {
		out["archive:"+a.Archive.Path] = c.fileInput(t.Workspace, a.Archive.Path)
	}
	return nil
}

func (c *ArtifactChecker) captureHelm(ctx context.Context, t Target, crit model.Criterion, out map[string]string) error {
	a, err := parseHelmArgs(crit)
	if err != nil {
		return err
	}
	if c.cfg.Tools == nil || c.cfg.KubeconfigPath == "" || c.cfg.Context == "" {
		return notConfigured("helm fingerprints need the helm CLI and the environment's kubeconfig")
	}
	ns := a.Namespace
	if ns == "" {
		ns = problemNamespace(t)
	}
	key := "helm:" + ns + "/" + a.Release
	rel, found, err := c.helmStatus(ctx, ns, a.Release)
	if err != nil {
		return err
	}
	if !found {
		out[key] = "absent"
		return nil
	}
	value := fmt.Sprintf("revision=%d;chart=%s-%s", rel.revision, rel.chart, rel.version)
	if len(c.cfg.InputKey) > 0 {
		res, err := c.helm(ctx, ns, "get", "values", a.Release, "-o", "json")
		if err != nil {
			return fmt.Errorf("helm get values: %w", err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("helm get values %s failed (exit %d): %s", a.Release, res.ExitCode, shortenLine(string(res.Stderr)))
		}
		var values any
		if err := json.Unmarshal(res.Stdout, &values); err != nil {
			return fmt.Errorf("helm get values %s: unreadable JSON answer", a.Release)
		}
		value += ";values=" + c.mac("helm-values", canonicalJSON(values))
	}
	out[key] = value
	return nil
}

func (c *ArtifactChecker) captureKustomize(ctx context.Context, t Target, crit model.Criterion, out map[string]string) error {
	a, err := parseKustomizeArgs(crit)
	if err != nil {
		return err
	}
	dirs := []struct{ prefix, p string }{{"kustomize:", a.Path}}
	if a.Base != nil {
		dirs = append(dirs, struct{ prefix, p string }{"kustomize-base:", a.Base.Path})
	}
	for _, d := range dirs {
		out[d.prefix+d.p] = c.fileInput(t.Workspace, d.p)
	}
	for _, e := range a.Expect {
		if e.Absent || !a.live(e) {
			continue
		}
		ns := e.Namespace
		if ns == "" {
			ns = problemNamespace(t)
		}
		if err := c.captureObject(ctx, model.ObjectRef{APIVersion: e.APIVersion, Kind: e.Kind, Namespace: ns, Name: e.Name}, out); err != nil {
			return err
		}
	}
	return nil
}

func (c *ArtifactChecker) capturePolicies(ctx context.Context, ns string, out map[string]string) error {
	if c.cfg.Reader == nil {
		return notConfigured("input fingerprints need an object reader")
	}
	list, err := c.cfg.Reader.List(ctx, ListQuery{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy", Namespace: ns})
	if err != nil {
		return fmt.Errorf("list NetworkPolicies: %w", err)
	}
	for _, p := range list {
		out[objectKey(model.ObjectRef{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy", Namespace: ns, Name: nameOf(p)})] = c.fingerprint(p)
	}
	return nil
}

// captureAuthorization covers the ServiceAccount, the pod or workload that
// must run as it, and the RBAC objects that grant it something.
func (c *ArtifactChecker) captureAuthorization(ctx context.Context, t Target, crit model.Criterion, out map[string]string) error {
	a, err := parseAuthzArgs(crit)
	if err != nil {
		return err
	}
	ref, err := resolveTarget(t, crit)
	if err != nil {
		return err
	}
	if err := c.captureObject(ctx, ref, out); err != nil {
		return err
	}
	if r := a.RunsAs; r != nil {
		ns := r.Namespace
		if ns == "" {
			ns = ref.Namespace
		}
		if err := c.captureObject(ctx, model.ObjectRef{APIVersion: r.APIVersion, Kind: r.Kind, Namespace: ns, Name: r.Name}, out); err != nil {
			return err
		}
	}
	if c.cfg.Reader == nil {
		return notConfigured("input fingerprints need an object reader")
	}
	const rbac = "rbac.authorization.k8s.io/v1"
	for _, q := range []ListQuery{
		{APIVersion: rbac, Kind: "RoleBinding", Namespace: ref.Namespace},
		{APIVersion: rbac, Kind: "ClusterRoleBinding"},
	} {
		bindings, err := c.cfg.Reader.List(ctx, q)
		if err != nil {
			return fmt.Errorf("list %ss: %w", q.Kind, err)
		}
		sort.Slice(bindings, func(i, j int) bool { return nameOf(bindings[i]) < nameOf(bindings[j]) })
		for _, b := range bindings {
			if !bindingGrants(b, ref) {
				continue
			}
			bref := model.ObjectRef{APIVersion: rbac, Kind: q.Kind, Namespace: q.Namespace, Name: nameOf(b)}
			out[objectKey(bref)] = c.fingerprint(b)
			rr := mapAt(b, "roleRef")
			role := model.ObjectRef{APIVersion: rbac, Kind: path.Clean(fmt.Sprint(rr["kind"])), Name: fmt.Sprint(rr["name"])}
			if role.Kind == "Role" {
				role.Namespace = q.Namespace
			}
			if role.Kind != "Role" && role.Kind != "ClusterRole" {
				continue
			}
			if err := c.captureObject(ctx, role, out); err != nil {
				return err
			}
		}
	}
	return nil
}

// bindingGrants: the binding names the ServiceAccount as a subject.
func bindingGrants(binding object, sa model.ObjectRef) bool {
	bindingNS := stringAt(binding, "metadata", "namespace")
	for _, s := range listAt(binding, "subjects") {
		m, _ := s.(map[string]any)
		if m["kind"] != "ServiceAccount" || m["name"] != sa.Name {
			continue
		}
		ns, _ := m["namespace"].(string)
		if ns == "" {
			ns = bindingNS
		}
		if ns == sa.Namespace {
			return true
		}
	}
	return false
}
