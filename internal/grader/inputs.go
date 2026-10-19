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
	"strings"

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
// criteria, and what those criteria are really about, and nothing else:
//
//	object:<apiVersion>/<Kind>/<ns>/<name>   every API object a criterion
//	    targets, plus the objects behind it: for a Service (http, tcp,
//	    network-policy) the workloads whose pod template its selector selects
//	    (also at zero replicas) and bare pods it selects; for an Ingress its
//	    backend Services and their workloads; for any captured workload or pod
//	    the ConfigMaps, Secrets and ServiceAccount its pod spec references
//	    (containers, init and ephemeral containers, imagePullSecrets, the
//	    ServiceAccount's imagePullSecrets; more than 30 is an error); for
//	    http/tcp/network-policy the NetworkPolicies of the target's and every
//	    source namespace; for authorization the RoleBindings/ClusterRoleBindings
//	    granting the ServiceAccount with the Roles they reference and the
//	    runs_as workload; for kustomize the live expected objects.
//	    Value: "uid=<uid>;gen=<generation>;input=sha256:<hash>" or "absent".
//	    The hash covers the spec (or, for kinds without one, the payload
//	    fields) AND metadata.labels and metadata.annotations, because a
//	    learner's label or annotation can be the answer. Fields that
//	    controllers, kubectl and the system write are excluded: status,
//	    resourceVersion, managedFields, a Pod's spec.nodeName, a PVC's
//	    spec.volumeName; annotations deployment.kubernetes.io/*,
//	    kubectl.kubernetes.io/last-applied-configuration (a copy of the last
//	    apply, which changes only when the spec does), autoscaling.alpha.*,
//	    endpoints.kubernetes.io/*, pv.kubernetes.io/*, volume.*kubernetes.io/*,
//	    batch.kubernetes.io/*, cronjob.kubernetes.io/*,
//	    control-plane.alpha.kubernetes.io/*; labels pod-template-hash,
//	    controller-revision-hash, controller-uid, job-name,
//	    statefulset.kubernetes.io/pod-name, apps.kubernetes.io/pod-index,
//	    batch.kubernetes.io/*, kubernetes.io/metadata.name.
//	    A Secret's payload is covered through a keyed hash (below); without a
//	    key its uid and resourceVersion are the change signal instead (R22):
//	    a Secret is never silently ignored.
//	file:<path>      workspace file content ("absent", "outside-workspace",
//	                 or a digest); archive:<path> likewise
//	image:<ref>      "id=<image ID>" from the Docker engine ("absent" if none)
//	helm:<ns>/<release>   "revision=<n>;chart=<name-version>[;values=<mac>]"
//	                 or "absent"; the release status is not an input. Without a
//	                 key the revision (every upgrade or rollback increments it)
//	                 is the change signal for the values.
//	kustomize:<dir>, kustomize-base:<dir>   keyed digest of the directory tree
//
// Digests of anything that may carry secrets (files, trees, Helm values,
// Secret payloads) are HMAC-SHA256 values keyed with ArtifactConfig.InputKey
// (prefixed "mac:"). A plain unkeyed hash of a low-entropy Secret would be a
// dictionary lookup, so without a key Secret payloads and Helm values are not
// hashed at all (their signals above are used) and file/tree digests are
// plain "sha256:" digests (compose with a key in production). The result is
// deterministic for identical inputs. Missing objects and files are
// recorded, not errors; an API or tool failure is an error.
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
	case model.CriterionVolumePersistence:
		return c.captureVolume(ctx, t, crit, out)
	case model.CriterionResource, model.CriterionField, model.CriterionReady, model.CriterionHTTP, model.CriterionTCP,
		model.CriterionLogs, model.CriterionJob, model.CriterionCronJobTemplate, model.CriterionCronJobSchedule,
		model.CriterionSecurityContext:
		if err := c.captureTarget(ctx, t, crit, out); err != nil {
			return err
		}
		if crit.Kind == model.CriterionHTTP || crit.Kind == model.CriterionTCP {
			return c.captureNetworkPolicies(ctx, t, crit, out)
		}
		return nil
	case model.CriterionNetworkPolicy:
		if err := c.captureTarget(ctx, t, crit, out); err != nil {
			return err
		}
		return c.captureNetworkPolicies(ctx, t, crit, out)
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
	return c.captureDeep(ctx, ref, out, 0)
}

const maxInputDepth = 4

// captureDeep fingerprints ref and what the answer behind it consists of.
func (c *ArtifactChecker) captureDeep(ctx context.Context, ref model.ObjectRef, out map[string]string, depth int) error {
	obj, err := c.captureObject(ctx, ref, out)
	if err != nil || obj == nil {
		return err
	}
	return c.follow(ctx, ref, obj, out, depth)
}

// follow captures what an already recorded object leads to.
func (c *ArtifactChecker) follow(ctx context.Context, ref model.ObjectRef, obj object, out map[string]string, depth int) error {
	if depth > maxInputDepth {
		return nil
	}
	switch {
	case ref.APIVersion == "v1" && ref.Kind == "Service":
		return c.captureServiceBackends(ctx, obj, out, depth)
	case ref.APIVersion == "networking.k8s.io/v1" && ref.Kind == "Ingress":
		return c.captureIngressBackends(ctx, obj, out, depth)
	}
	if _, ok := podSpecPath[ref.Kind]; ok {
		return c.captureReferenced(ctx, ref, obj, out, depth)
	}
	return nil
}

// captureObject records one object's fingerprint (once) and returns the
// object (nil when absent or already recorded).
func (c *ArtifactChecker) captureObject(ctx context.Context, ref model.ObjectRef, out map[string]string) (object, error) {
	key := objectKey(ref)
	if _, done := out[key]; done {
		return nil, nil
	}
	if c.cfg.Reader == nil {
		return nil, notConfigured("input fingerprints need an object reader")
	}
	obj, err := c.obs.get(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		out[key] = "absent"
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out[key] = c.fingerprint(obj)
	return obj, nil
}

// listOptional lists a kind; a kind the cluster does not serve is no objects.
func (c *ArtifactChecker) listOptional(ctx context.Context, q ListQuery) ([]object, error) {
	list, err := c.cfg.Reader.List(ctx, q)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %ss: %w", q.Kind, err)
	}
	return list, nil
}

var backingKinds = []struct{ apiVersion, kind string }{
	{"apps/v1", "Deployment"}, {"apps/v1", "StatefulSet"}, {"apps/v1", "DaemonSet"}, {"apps/v1", "ReplicaSet"},
	{"batch/v1", "Job"}, {"v1", "Pod"},
}

// captureServiceBackends fingerprints the workloads a Service selects: those
// whose pod template labels carry the whole selector (so a Deployment scaled
// to zero is still found), and bare pods. Objects owned by another
// controller (ReplicaSets of a Deployment, pods of a workload) are skipped:
// their owner is the learner's object.
func (c *ArtifactChecker) captureServiceBackends(ctx context.Context, svc object, out map[string]string, depth int) error {
	selector := mapAt(svc, "spec", "selector")
	if len(selector) == 0 {
		return nil
	}
	ns := stringAt(svc, "metadata", "namespace")
	for _, k := range backingKinds {
		list, err := c.listOptional(ctx, ListQuery{APIVersion: k.apiVersion, Kind: k.kind, Namespace: ns})
		if err != nil {
			return err
		}
		sort.Slice(list, func(i, j int) bool { return nameOf(list[i]) < nameOf(list[j]) })
		for _, w := range list {
			if controllerUID(w) != "" {
				continue
			}
			labels := mapAt(w, "metadata", "labels")
			if k.kind != "Pod" {
				path := append([]string{}, podSpecPath[k.kind][:len(podSpecPath[k.kind])-1]...)
				labels = mapAt(w, append(path, "metadata", "labels")...)
			}
			if !labelsContain(labels, selector) {
				continue
			}
			ref := model.ObjectRef{APIVersion: k.apiVersion, Kind: k.kind, Namespace: ns, Name: nameOf(w)}
			key := objectKey(ref)
			if _, done := out[key]; done {
				continue
			}
			out[key] = c.fingerprint(w) // already read by the list
			if err := c.follow(ctx, ref, w, out, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func labelsContain(labels, selector map[string]any) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// captureIngressBackends follows an Ingress to its backend Services.
func (c *ArtifactChecker) captureIngressBackends(ctx context.Context, ing object, out map[string]string, depth int) error {
	ns := stringAt(ing, "metadata", "namespace")
	names := map[string]bool{}
	if n := stringAt(ing, "spec", "defaultBackend", "service", "name"); n != "" {
		names[n] = true
	}
	for _, r := range listAt(ing, "spec", "rules") {
		for _, p := range listAt(asMap(asMap(r)["http"]), "paths") {
			if n, _ := asMap(asMap(asMap(p)["backend"])["service"])["name"].(string); n != "" {
				names[n] = true
			}
		}
	}
	for _, n := range sortedKeys(names) {
		if err := c.captureDeep(ctx, model.ObjectRef{APIVersion: "v1", Kind: "Service", Namespace: ns, Name: n}, out, depth+1); err != nil {
			return err
		}
	}
	return nil
}

const maxReferenced = 30

// captureReferenced fingerprints the ConfigMaps, Secrets and ServiceAccount
// that a workload's pod spec refers to: part of what the learner submitted.
// That includes the Secrets named by spec.imagePullSecrets and by the
// ServiceAccount's imagePullSecrets (a changed pull credential can turn an
// ImagePullBackOff into a running pod), references from ephemeral
// containers and the deprecated spec.serviceAccount field. A pod spec naming
// more than maxReferenced objects is not silently cut short: the answer
// cannot be fingerprinted completely, so it is an error (the submission is
// then never confirmed as unchanged).
func (c *ArtifactChecker) captureReferenced(ctx context.Context, ref model.ObjectRef, obj object, out map[string]string, depth int) error {
	spec := mapAt(obj, podSpecPath[ref.Kind]...)
	refs := map[model.ObjectRef]bool{}
	add := func(kind, name string) {
		if name != "" {
			refs[model.ObjectRef{APIVersion: "v1", Kind: kind, Namespace: ref.Namespace, Name: name}] = true
		}
	}
	name := func(v any, key string) string { s, _ := asMap(v)[key].(string); return s }
	for _, v := range listAt(spec, "volumes") {
		vm := asMap(v)
		add("ConfigMap", name(vm["configMap"], "name"))
		add("Secret", name(vm["secret"], "secretName"))
		for _, src := range listAt(asMap(vm["projected"]), "sources") {
			add("ConfigMap", name(asMap(src)["configMap"], "name"))
			add("Secret", name(asMap(src)["secret"], "name"))
		}
	}
	for _, group := range []string{"containers", "initContainers", "ephemeralContainers"} {
		for _, cv := range listAt(spec, group) {
			cm := asMap(cv)
			for _, e := range listAt(cm, "env") {
				vf := asMap(asMap(e)["valueFrom"])
				add("ConfigMap", name(vf["configMapKeyRef"], "name"))
				add("Secret", name(vf["secretKeyRef"], "name"))
			}
			for _, e := range listAt(cm, "envFrom") {
				add("ConfigMap", name(asMap(e)["configMapRef"], "name"))
				add("Secret", name(asMap(e)["secretRef"], "name"))
			}
		}
	}
	for _, s := range listAt(spec, "imagePullSecrets") {
		add("Secret", name(s, "name"))
	}
	var accounts []string
	for _, key := range []string{"serviceAccountName", "serviceAccount"} {
		if sa, _ := spec[key].(string); sa != "" {
			add("ServiceAccount", sa)
			accounts = append(accounts, sa)
		}
	}
	if err := c.captureRefs(ctx, ref, refs, out); err != nil {
		return err
	}
	// The ServiceAccount's own pull credentials are injected into the pods.
	pull := map[model.ObjectRef]bool{}
	for _, sa := range accounts {
		saRef := model.ObjectRef{APIVersion: "v1", Kind: "ServiceAccount", Namespace: ref.Namespace, Name: sa}
		if out[objectKey(saRef)] == "absent" {
			continue
		}
		saObj, err := c.obs.get(ctx, saRef)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		for _, s := range listAt(saObj, "imagePullSecrets") {
			if n := name(s, "name"); n != "" {
				pull[model.ObjectRef{APIVersion: "v1", Kind: "Secret", Namespace: ref.Namespace, Name: n}] = true
			}
		}
	}
	return c.captureRefs(ctx, ref, pull, out)
}

// captureRefs fingerprints a set of referenced objects, refusing more than
// maxReferenced.
func (c *ArtifactChecker) captureRefs(ctx context.Context, owner model.ObjectRef, refs map[model.ObjectRef]bool, out map[string]string) error {
	if len(refs) > maxReferenced {
		return fmt.Errorf("%w: the pod spec of %s %s references %d ConfigMaps, Secrets and ServiceAccounts, more than the %d the grader fingerprints, so the answer cannot be shown unchanged",
			ErrInsufficientEvidence, owner.Kind, owner.Name, len(refs), maxReferenced)
	}
	list := make([]model.ObjectRef, 0, len(refs))
	for r := range refs {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return objectKey(list[i]) < objectKey(list[j]) })
	for _, r := range list {
		if _, err := c.captureObject(ctx, r, out); err != nil {
			return err
		}
	}
	return nil
}

// captureNetworkPolicies fingerprints the NetworkPolicies of the namespaces a
// network criterion involves: the target's and every probe source namespace.
func (c *ArtifactChecker) captureNetworkPolicies(ctx context.Context, t Target, crit model.Criterion, out map[string]string) error {
	if c.cfg.Reader == nil {
		return notConfigured("input fingerprints need an object reader")
	}
	namespaces := map[string]bool{problemNamespaceOf(t, crit): true}
	addSource := func(v any) {
		ns, _ := asMap(asMap(v)["source"])["namespace"].(string)
		if ns == "" {
			ns = problemNamespace(t)
		}
		namespaces[ns] = true
	}
	addSource(map[string]any(crit.Args))
	if l, ok := crit.Args["expectations"].([]any); ok {
		for _, e := range l {
			addSource(e)
		}
	}
	for _, ns := range sortedKeys(namespaces) {
		if ns == "" {
			continue
		}
		list, err := c.listOptional(ctx, ListQuery{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy", Namespace: ns})
		if err != nil {
			return err
		}
		for _, p := range list {
			out[objectKey(model.ObjectRef{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy", Namespace: ns, Name: nameOf(p)})] = c.fingerprint(p)
		}
	}
	return nil
}

// fingerprint is "uid=..;gen=..;input=.." over what the learner submitted of
// obj: its payload plus its labels and annotations, without the fields
// controllers and the system write.
func (c *ArtifactChecker) fingerprint(obj object) string {
	head := fmt.Sprintf("uid=%s;gen=%d", uidOf(obj), generationOf(obj))
	meta := hashOf(submittedMetadata(obj))
	if isSecret(obj) {
		head += ";meta=" + meta
		if mac := c.mac("secret", canonicalJSON(map[string]any{"data": obj["data"], "stringData": obj["stringData"], "type": obj["type"]})); mac != "" {
			return head + ";data=" + mac
		}
		// No key: the payload is not hashed (a plain hash of a weak secret is
		// a dictionary lookup); uid + resourceVersion signal a change (R22).
		return head + ";rv=" + stringAt(obj, "metadata", "resourceVersion")
	}
	return head + ";input=" + hashOf(map[string]any{"payload": submittedPayload(obj), "metadata": submittedMetadata(obj)})
}

func hashOf(v any) string {
	sum := sha256.Sum256(canonicalJSON(v))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// submittedPayload is the spec, or for kinds without one every top-level
// field except apiVersion, kind, metadata and status; minus the fields a
// controller fills in after the learner applied the object.
func submittedPayload(obj object) any {
	if spec, ok := obj["spec"].(map[string]any); ok {
		drop := ""
		switch obj["kind"] {
		case "Pod":
			drop = "nodeName"
		case "PersistentVolumeClaim":
			drop = "volumeName"
		}
		cp := make(map[string]any, len(spec))
		for k, v := range spec {
			if k != drop {
				cp[k] = v
			}
		}
		return cp
	}
	payload := map[string]any{}
	for k, v := range obj {
		switch k {
		case "apiVersion", "kind", "metadata", "status":
		default:
			payload[k] = v
		}
	}
	return payload
}

var (
	systemAnnotationPrefixes = []string{
		"deployment.kubernetes.io/", "autoscaling.alpha.kubernetes.io/", "endpoints.kubernetes.io/", "pv.kubernetes.io/",
		"volume.kubernetes.io/", "volume.beta.kubernetes.io/", "batch.kubernetes.io/", "cronjob.kubernetes.io/",
		"control-plane.alpha.kubernetes.io/",
	}
	systemAnnotations = map[string]bool{"kubectl.kubernetes.io/last-applied-configuration": true}
	systemLabelKeys   = map[string]bool{
		"pod-template-hash": true, "controller-revision-hash": true, "controller-uid": true, "job-name": true,
		"statefulset.kubernetes.io/pod-name": true, "apps.kubernetes.io/pod-index": true, "kubernetes.io/metadata.name": true,
	}
)

// submittedMetadata is the learner-settable part of metadata.
func submittedMetadata(obj object) map[string]any {
	meta := metaOf(obj)
	labels, annotations := map[string]any{}, map[string]any{}
	for k, v := range asMap(meta["labels"]) {
		if !systemLabelKeys[k] && !strings.HasPrefix(k, "batch.kubernetes.io/") {
			labels[k] = v
		}
	}
next:
	for k, v := range asMap(meta["annotations"]) {
		if systemAnnotations[k] {
			continue
		}
		for _, p := range systemAnnotationPrefixes {
			if strings.HasPrefix(k, p) {
				continue next
			}
		}
		annotations[k] = v
	}
	return map[string]any{"labels": labels, "annotations": annotations}
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
	a, err := parseHelmArgs(crit, c.set.defaultTimeout)
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
		if err := c.captureDeep(ctx, model.ObjectRef{APIVersion: e.APIVersion, Kind: e.Kind, Namespace: ns, Name: e.Name}, out, 0); err != nil {
			return err
		}
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
	if _, err := c.captureObject(ctx, ref, out); err != nil {
		return err
	}
	if r := a.RunsAs; r != nil {
		ns := r.Namespace
		if ns == "" {
			ns = ref.Namespace
		}
		if err := c.captureDeep(ctx, model.ObjectRef{APIVersion: r.APIVersion, Kind: r.Kind, Namespace: ns, Name: r.Name}, out, 0); err != nil {
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
			if _, err := c.captureObject(ctx, role, out); err != nil {
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
