package grader

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"path"
	"strings"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
	"k8s.io/apimachinery/pkg/util/validation"
)

// VolumePersistenceTimeout bounds the write/replacement/read observation.
const VolumePersistenceTimeout = 120 * time.Second

// PodUIDDeleter replaces only the exact Pod whose ownership was proven.
// It is separate from PodAccess so existing read-only clients remain compatible.
type PodUIDDeleter interface {
	DeletePodUID(ctx context.Context, namespace, name, uid string) error
}

type volumeArgs struct {
	Container string `json:"container"`
	Path      string `json:"path"`
	Claim     string `json:"claim"`
}

func parseVolumeArgs(c model.Criterion) (volumeArgs, error) {
	var a volumeArgs
	// Unlike the general Args decoder, this destructive check accepts only these three keys.
	for k := range c.Args {
		if k != "container" && k != "path" && k != "claim" {
			return a, fmt.Errorf("%w: unknown volume argument %q", ErrInvalidCriterion, k)
		}
	}
	if err := decodeArgs(c.Args, &a); err != nil {
		return a, err
	}
	if len(validation.IsDNS1123Label(a.Container)) != 0 || len(validation.IsDNS1123Subdomain(a.Claim)) != 0 || !safeVolumePath(a.Path) || a.Path == "/" {
		return a, fmt.Errorf("%w: volume requires container, claim and a normalized absolute directory path", ErrInvalidCriterion)
	}
	if c.Target.APIVersion != "apps/v1" || c.Target.Kind != "Deployment" || c.Target.Name == "" {
		return a, fmt.Errorf("%w: volume target must be one apps/v1 Deployment", ErrInvalidCriterion)
	}
	return a, nil
}
func safeVolumePath(s string) bool {
	return path.IsAbs(s) && path.Clean(s) == s && !strings.ContainsAny(s, "\x00\r\n\t \\")
}
func withinVolume(p, m string) bool { return p == m || strings.HasPrefix(p, m+"/") || m == "/" }

// volumeMount proves the directory's longest covering mount is the named PVC.
// A nested EmptyDir/Secret/other claim therefore cannot masquerade as persistence.
func volumeMount(spec object, a volumeArgs, dir string) (string, string, error) {
	var container object
	for _, v := range listAt(spec, "containers") {
		if stringAt(asMap(v), "name") == a.Container {
			container = asMap(v)
		}
	}
	if container == nil {
		return "", "container not found", nil
	}
	var selected object
	longest := -1
	for _, v := range listAt(container, "volumeMounts") {
		m := asMap(v)
		p := stringAt(m, "mountPath")
		if !safeVolumePath(p) {
			return "", "", fmt.Errorf("%w: mount path cannot be proven safe", ErrInsufficientEvidence)
		}
		if withinVolume(dir, p) && len(p) > longest {
			selected = m
			longest = len(p)
		}
	}
	if selected == nil {
		return "", "directory is outside the container's PVC mounts", nil
	}
	if b, _ := selected["readOnly"].(bool); b {
		return "", "volume mount is read-only", nil
	}
	sub := stringAt(selected, "subPath")
	if stringAt(selected, "subPathExpr") != "" || sub != "" && (path.IsAbs(sub) || path.Clean(sub) != sub || sub == ".." || strings.HasPrefix(sub, "../") || strings.ContainsAny(sub, "\x00\r\n\\")) {
		return "", "", fmt.Errorf("%w: unsafe or dynamic subPath", ErrInsufficientEvidence)
	}
	for _, v := range listAt(spec, "volumes") {
		vol := asMap(v)
		if stringAt(vol, "name") != stringAt(selected, "name") {
			continue
		}
		pvc := mapAt(vol, "persistentVolumeClaim")
		if stringAt(pvc, "claimName") != a.Claim {
			return "", "directory is not mounted from the required PVC", nil
		}
		if b, _ := pvc["readOnly"].(bool); b {
			return "", "PVC is read-only", nil
		}
		return stringAt(selected, "mountPath"), "", nil
	}
	return "", "mount's volume not found", nil
}

type volumeProof struct {
	ref                                                                      model.ObjectRef
	namespaceUID, deployment, pvc, binding, pv, rsUID, rsName, rsFingerprint string
	a                                                                        volumeArgs
}

func (c *ArtifactChecker) volumeNamespace(ctx context.Context, t Target, ns string) (object, error) {
	if t.Identity.ID == "" || t.SessionID == "" || t.Problem.Definition.ID == "" || ns != problemNamespace(t) {
		return nil, fmt.Errorf("%w: expected volume session/namespace ownership is missing", ErrInsufficientEvidence)
	}
	o, err := c.obs.get(ctx, model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: ns})
	if err != nil {
		return nil, err
	}
	if uidOf(o) == "" || labelOf(o, "junkloud.io/environment-id") != t.Identity.ID || labelOf(o, "junkloud.io/session-id") != t.SessionID || labelOf(o, "junkloud.io/problem-id") != t.Problem.Definition.ID {
		return nil, fmt.Errorf("%w: volume namespace ownership does not match expected session", ErrInsufficientEvidence)
	}
	return o, nil
}
func (c *ArtifactChecker) volumeAnchors(ctx context.Context, t Target, p volumeProof) (object, error) {
	ns, err := c.volumeNamespace(ctx, t, p.ref.Namespace)
	if err != nil {
		return nil, err
	}
	if uidOf(ns) != p.namespaceUID {
		return nil, ErrTargetChanged
	}
	d, err := c.obs.get(ctx, p.ref)
	if err != nil {
		return nil, err
	}
	if uidOf(d) == "" || c.fingerprint(d) != p.deployment {
		return nil, ErrTargetChanged
	}
	claim, err := c.obs.get(ctx, model.ObjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Namespace: p.ref.Namespace, Name: p.a.Claim})
	if err != nil {
		return nil, err
	}
	if c.fingerprint(claim) != p.pvc || stringAt(claim, "spec", "volumeName") != p.binding || stringAt(claim, "status", "phase") != "Bound" {
		return nil, ErrTargetChanged
	}
	bound, err := c.volumeBinding(ctx, p.ref.Namespace, p.a.Claim, uidOf(claim), p.binding)
	if err != nil {
		return nil, err
	}
	if c.fingerprint(bound) != p.pv {
		return nil, ErrTargetChanged
	}
	sets, err := c.cfg.Reader.List(ctx, ListQuery{APIVersion: "apps/v1", Kind: "ReplicaSet", Namespace: p.ref.Namespace})
	if err != nil {
		return nil, err
	}
	for _, rs := range sets {
		if uidOf(rs) == p.rsUID && volumeControlledBy(rs, "Deployment", p.ref.Name, uidOf(d)) && c.fingerprint(rs) == p.rsFingerprint {
			return d, nil
		}
	}
	return nil, fmt.Errorf("%w: Deployment to ReplicaSet UID chain changed", ErrTargetChanged)
}
func volumeReady(p object, container string) bool {
	if stringAt(p, "metadata", "deletionTimestamp") != "" || stringAt(p, "status", "phase") != "Running" {
		return false
	}
	ready := false
	for _, cond := range listAt(p, "status", "conditions") {
		if stringAt(asMap(cond), "type") == "Ready" && stringAt(asMap(cond), "status") == "True" {
			ready = true
		}
	}
	if !ready {
		return false
	}
	for _, s := range listAt(p, "status", "containerStatuses") {
		m := asMap(s)
		if stringAt(m, "name") == container {
			b, _ := m["ready"].(bool)
			_, running := mapAt(m, "state")["running"]
			return b && running
		}
	}
	return false
}
func (c *ArtifactChecker) volumePods(ctx context.Context, p volumeProof) ([]object, error) {
	all, err := c.cfg.Reader.List(ctx, ListQuery{APIVersion: "v1", Kind: "Pod", Namespace: p.ref.Namespace})
	if err != nil {
		return nil, err
	}
	var out []object
	for _, pod := range all {
		if uidOf(pod) != "" && volumeControlledBy(pod, "ReplicaSet", p.rsName, p.rsUID) && stringAt(pod, "metadata", "namespace") == p.ref.Namespace && volumeReady(pod, p.a.Container) {
			out = append(out, pod)
		}
	}
	return out, nil
}
func (c *ArtifactChecker) volumePod(ctx context.Context, t Target, p volumeProof, name, uid string) (object, error) {
	if _, err := c.volumeAnchors(ctx, t, p); err != nil {
		return nil, err
	}
	pods, err := c.volumePods(ctx, p)
	if err != nil {
		return nil, err
	}
	for _, pod := range pods {
		if nameOf(pod) == name && uidOf(pod) == uid {
			_, wrong, err := volumeMount(mapAt(pod, "spec"), p.a, p.a.Path)
			if err != nil {
				return nil, err
			}
			if wrong != "" {
				return nil, fmt.Errorf("%w: replacement mount changed", ErrTargetChanged)
			}
			return pod, nil
		}
	}
	return nil, fmt.Errorf("%w: selected Pod UID/ownership is no longer ready", ErrTargetChanged)
}
func (c *ArtifactChecker) volumeExec(ctx context.Context, t Target, p volumeProof, pod object, script string, args ...string) (ExecResult, error) {
	if _, err := c.volumePod(ctx, t, p, nameOf(pod), uidOf(pod)); err != nil {
		return ExecResult{}, err
	}
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := append([]string{"sh", "-c", script, "junkloud-volume"}, args...)
	r, err := c.cfg.Pods.Exec(call, p.ref.Namespace, nameOf(pod), p.a.Container, cmd)
	if err != nil {
		return ExecResult{}, fmt.Errorf("%w: volume exec could not run", ErrInsufficientEvidence)
	}
	return r, nil
}

// All variable values are argv, never shell source. readlink and pwd prove
// symlinks resolve into the selected PVC mount before any marker operation.
const volumeResolveScript = `# junkloud-volume-resolve
set -eu
[ -d "$1" ] && [ -d "$2" ]
readlink -f -- "$1"
readlink -f -- "$2"`
const volumeWriteScript = `# junkloud-volume-write
set -eu
cd -P -- "$1"
[ "$(pwd -P)" = "$1" ]
umask 077
set -C
printf '%s' "$3" > "$2"`
const volumeReadScript = `# junkloud-volume-read
set -eu
cd -P -- "$1"
[ "$(pwd -P)" = "$1" ]
[ ! -L "$2" ] || exit 2
if [ ! -e "$2" ]; then exit 3; fi
[ -f "$2" ] && [ -r "$2" ] || exit 2
cat -- "$2"`
const volumeCleanupScript = `# junkloud-volume-cleanup
set -eu
cd -P -- "$1"
[ "$(pwd -P)" = "$1" ]
if [ ! -e "$2" ] && [ ! -L "$2" ]; then exit 0; fi
[ ! -L "$2" ] && [ -f "$2" ] || exit 2
[ "$(cat -- "$2")" = "$3" ] || exit 2
rm -- "$2"`

func (c *ArtifactChecker) checkVolume(ctx context.Context, t Target, crit model.Criterion) (result model.CheckResult, retErr error) {
	a, err := parseVolumeArgs(crit)
	if err != nil {
		return result, err
	}
	if c.cfg.Reader == nil || c.cfg.Pods == nil {
		return result, notConfigured("volume checks need object and pod access")
	}
	deleter, ok := c.cfg.Pods.(PodUIDDeleter)
	if !ok {
		return result, notConfigured("volume checks need UID-preconditioned pod deletion")
	}
	timeout := crit.Timeout
	if timeout <= 0 {
		timeout = VolumePersistenceTimeout
	}
	if timeout > VolumePersistenceTimeout {
		timeout = VolumePersistenceTimeout
	}
	deadline := c.set.clock.Now().Add(timeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ref, err := resolveTarget(t, crit)
	if err != nil {
		return result, err
	}
	ns, err := c.volumeNamespace(ctx, t, ref.Namespace)
	if err != nil {
		return result, err
	}
	d, err := c.obs.get(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return model.CheckResult{Status: model.StatusFail, Reason: "Deployment not found"}, nil
	}
	if err != nil {
		return result, err
	}
	mount, wrong, err := volumeMount(mapAt(d, "spec", "template", "spec"), a, a.Path)
	if err != nil {
		return result, err
	}
	if wrong != "" {
		return model.CheckResult{Status: model.StatusFail, Reason: wrong}, nil
	}
	claim, err := c.obs.get(ctx, model.ObjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Namespace: ref.Namespace, Name: a.Claim})
	if errors.Is(err, ErrNotFound) {
		return model.CheckResult{Status: model.StatusFail, Reason: "required PVC not found"}, nil
	}
	if err != nil {
		return result, err
	}
	binding := stringAt(claim, "spec", "volumeName")
	if uidOf(d) == "" || uidOf(claim) == "" || binding == "" || stringAt(claim, "status", "phase") != "Bound" {
		return result, fmt.Errorf("%w: PVC binding/target UID is not proven", ErrInsufficientEvidence)
	}
	bound, err := c.volumeBinding(ctx, ref.Namespace, a.Claim, uidOf(claim), binding)
	if err != nil {
		return result, err
	}
	rsUID, unknown, err := currentReplicaSet(ctx, c.cfg.Reader, d, ref.Namespace, selectorString(mapAt(d, "spec", "selector")))
	if err != nil {
		return result, err
	}
	if unknown != "" || rsUID == "" {
		return result, fmt.Errorf("%w: current ReplicaSet is unproven", ErrInsufficientEvidence)
	}
	sets, err := c.cfg.Reader.List(ctx, ListQuery{APIVersion: "apps/v1", Kind: "ReplicaSet", Namespace: ref.Namespace})
	if err != nil {
		return result, err
	}
	p := volumeProof{ref: ref, namespaceUID: uidOf(ns), deployment: c.fingerprint(d), pvc: c.fingerprint(claim), binding: binding, pv: c.fingerprint(bound), rsUID: rsUID, a: a}
	for _, rs := range sets {
		if uidOf(rs) == rsUID && volumeControlledBy(rs, "Deployment", ref.Name, uidOf(d)) {
			p.rsName = nameOf(rs)
			p.rsFingerprint = c.fingerprint(rs)
		}
	}
	if p.rsFingerprint == "" {
		return result, ErrInsufficientEvidence
	}
	pods, err := c.volumePods(ctx, p)
	if err != nil {
		return result, err
	}
	if len(pods) != 1 {
		return result, fmt.Errorf("%w: persistence requires exactly one ready owned Pod", ErrInsufficientEvidence)
	}
	old := maps.Clone(pods[0])
	old["metadata"] = maps.Clone(metaOf(pods[0]))
	actualMount, wrong, err := volumeMount(mapAt(old, "spec"), a, a.Path)
	if err != nil {
		return result, err
	}
	if wrong != "" {
		return model.CheckResult{Status: model.StatusFail, Reason: wrong}, nil
	}
	if actualMount != mount {
		return result, ErrTargetChanged
	}
	resolved, err := c.volumeExec(ctx, t, p, old, volumeResolveScript, mount, a.Path)
	if err != nil {
		return result, err
	}
	dirs := strings.Split(strings.TrimSuffix(resolved.Stdout, "\n"), "\n")
	if resolved.ExitCode != 0 || len(dirs) != 2 || dirs[0] != mount || !safeVolumePath(dirs[1]) || !withinVolume(dirs[1], mount) {
		return result, fmt.Errorf("%w: directory symlink/mount proof failed", ErrInsufficientEvidence)
	}
	if _, wrong, err := volumeMount(mapAt(old, "spec"), a, dirs[1]); err != nil || wrong != "" {
		return result, fmt.Errorf("%w: resolved directory is outside the required PVC", ErrInsufficientEvidence)
	}
	dir := dirs[1]
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return result, err
	}
	token := hex.EncodeToString(nonce)
	marker := ".junkloud-persistence-" + token
	markerAttempted := false
	defer func() {
		if !markerAttempted {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cleanupCancel()
		if _, err := c.volumeAnchors(cleanupCtx, t, p); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("required marker cleanup refused: %w", err))
			return
		}
		candidates, err := c.volumePods(cleanupCtx, p)
		if err == nil && len(candidates) == 1 {
			var res ExecResult
			res, err = c.volumeExec(cleanupCtx, t, p, candidates[0], volumeCleanupScript, dir, marker, token)
			if err == nil && res.ExitCode != 0 {
				err = ErrInsufficientEvidence
			}
		} else if err == nil {
			err = ErrInsufficientEvidence
		}
		if err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("required marker cleanup could not be proven: %w", err))
		}
	}()
	markerAttempted = true
	write, err := c.volumeExec(ctx, t, p, old, volumeWriteScript, dir, marker, token)
	if err != nil {
		return result, err
	}
	if write.ExitCode != 0 {
		return result, fmt.Errorf("%w: marker write failed", ErrInsufficientEvidence)
	}
	initialRead, err := c.volumeExec(ctx, t, p, old, volumeReadScript, dir, marker)
	if err != nil {
		return result, err
	}
	if initialRead.ExitCode != 0 || initialRead.Stdout != token {
		return result, fmt.Errorf("%w: initial marker write was not readable", ErrInsufficientEvidence)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if _, err := c.volumePod(ctx, t, p, nameOf(old), uidOf(old)); err != nil {
		return result, err
	}
	if err := deleter.DeletePodUID(ctx, ref.Namespace, nameOf(old), uidOf(old)); err != nil {
		return result, fmt.Errorf("replace owned Pod: %w", err)
	}
	var replacement object
	for {
		if _, err := c.volumeAnchors(ctx, t, p); err != nil {
			return result, err
		}
		candidates, err := c.volumePods(ctx, p)
		if err != nil {
			return result, err
		}
		// A terminating Pod is no longer Ready but still exists. Require its
		// UID to disappear from the API, rather than merely from ready candidates.
		all, err := c.cfg.Reader.List(ctx, ListQuery{APIVersion: "v1", Kind: "Pod", Namespace: ref.Namespace})
		if err != nil {
			return result, err
		}
		oldPresent := false
		for _, pod := range all {
			if uidOf(pod) == uidOf(old) {
				oldPresent = true
			}
		}
		for _, pod := range candidates {
			if uidOf(pod) != uidOf(old) {
				replacement = pod
			}
		}
		if !oldPresent && replacement != nil && len(candidates) == 1 {
			break
		}
		replacement = nil
		if !c.set.clock.Now().Before(deadline) {
			return result, fmt.Errorf("%w: no ready replacement Pod with a new UID", ErrInsufficientEvidence)
		}
		if err := c.set.clock.Sleep(ctx, c.set.pollInterval); err != nil {
			return result, err
		}
	}
	// Repeat the canonical mount proof inside the replacement container.
	resolved, err = c.volumeExec(ctx, t, p, replacement, volumeResolveScript, mount, a.Path)
	if err != nil {
		return result, err
	}
	if resolved.ExitCode != 0 || strings.TrimSuffix(resolved.Stdout, "\n") != mount+"\n"+dir {
		return result, fmt.Errorf("%w: replacement directory/mount proof changed", ErrInsufficientEvidence)
	}
	read, err := c.volumeExec(ctx, t, p, replacement, volumeReadScript, dir, marker)
	if err != nil {
		return result, err
	}
	result = model.CheckResult{Status: model.StatusPass, Reason: "fresh marker survived owned Pod replacement", Evidence: model.Evidence{UID: uidOf(d), Generation: generationOf(d), InputHash: inputHash(d), Actual: map[string]any{"old_pod_uid": uidOf(old), "new_pod_uid": uidOf(replacement), "claim": a.Claim, "persisted": true}}}
	if read.ExitCode == 3 || read.ExitCode == 0 && read.Stdout != token {
		result.Status = model.StatusFail
		result.Reason = "marker did not persist across Pod replacement"
		result.Evidence.Actual.(map[string]any)["persisted"] = false
	} else if read.ExitCode != 0 {
		return result, fmt.Errorf("%w: marker could not be read", ErrInsufficientEvidence)
	}
	if _, err := c.volumePod(ctx, t, p, nameOf(replacement), uidOf(replacement)); err != nil {
		return result, err
	}
	return result, nil
}

func (c *ArtifactChecker) captureVolume(ctx context.Context, t Target, crit model.Criterion, out map[string]string) error {
	a, err := parseVolumeArgs(crit)
	if err != nil {
		return err
	}
	if err := c.captureTarget(ctx, t, crit, out); err != nil {
		return err
	}
	ref, err := resolveTarget(t, crit)
	if err != nil {
		return err
	}
	claimRef := model.ObjectRef{APIVersion: "v1", Kind: "PersistentVolumeClaim", Namespace: ref.Namespace, Name: a.Claim}
	claim, err := c.captureObject(ctx, claimRef, out)
	if err != nil {
		return err
	}
	// A prior generic PVC criterion may have populated its key. Deduplication
	// must not consume this criterion's separate binding/PV obligations.
	if claim == nil && out[objectKey(claimRef)] != "absent" {
		claim, err = c.obs.get(ctx, claimRef)
		if err != nil {
			return err
		}
	}
	// Binding is stable submitted evidence for this destructive check only.
	// Child Pod/ReplicaSet observations are deliberately absent: replacing one
	// does not change a submitted Deployment, claim, mount, or workspace file.
	if claim != nil {
		base, _, _ := strings.Cut(out[objectKey(claimRef)], ";binding=")
		if base != c.fingerprint(claim) {
			return fmt.Errorf("%w: PVC changed during input capture", ErrTargetChanged)
		}
		out[objectKey(claimRef)] = base + ";binding=" + stringAt(claim, "spec", "volumeName")
		if name := stringAt(claim, "spec", "volumeName"); name != "" {
			if _, err := c.captureObject(ctx, model.ObjectRef{APIVersion: "v1", Kind: "PersistentVolume", Name: name}, out); err != nil {
				return err
			}
		}
	}
	return nil
}

func volumeControlledBy(o object, kind, name, uid string) bool {
	for _, v := range listAt(o, "metadata", "ownerReferences") {
		m := asMap(v)
		controlled, _ := m["controller"].(bool)
		if controlled {
			return stringAt(m, "apiVersion") == "apps/v1" && stringAt(m, "kind") == kind && stringAt(m, "name") == name && stringAt(m, "uid") == uid && uid != ""
		}
	}
	return false
}

func (c *ArtifactChecker) volumeBinding(ctx context.Context, namespace, claim, claimUID, binding string) (object, error) {
	pv, err := c.obs.get(ctx, model.ObjectRef{APIVersion: "v1", Kind: "PersistentVolume", Name: binding})
	if err != nil {
		return nil, err
	}
	if uidOf(pv) == "" || stringAt(pv, "status", "phase") != "Bound" || stringAt(pv, "spec", "claimRef", "namespace") != namespace || stringAt(pv, "spec", "claimRef", "name") != claim || stringAt(pv, "spec", "claimRef", "uid") != claimUID {
		return nil, fmt.Errorf("%w: PV to PVC UID binding is unproven", ErrInsufficientEvidence)
	}
	return pv, nil
}
