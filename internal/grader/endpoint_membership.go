package grader

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sort"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
	"k8s.io/apimachinery/pkg/util/validation"
)

// EndpointMembershipTimeout includes identity observations, one bounded probe
// wave, and revalidation. All connections use the ordinary native probe runner.
const EndpointMembershipTimeout = 90 * time.Second

type membershipBackend struct {
	Deployment string `json:"deployment"`
	BodyMarker string `json:"body_marker"`
}
type membershipArgs struct {
	Source   netSource           `json:"source"`
	Port     *int                `json:"port"`
	Path     string              `json:"path"`
	Backends []membershipBackend `json:"backends"`
}

func parseMembership(crit model.Criterion) (membershipArgs, error) {
	var a membershipArgs
	if err := checkRef(crit.Target); err != nil {
		return a, err
	}
	if !isService(crit.Target) {
		return a, invalid("endpoint-membership targets a v1 Service")
	}
	for k := range crit.Args {
		if k != "source" && k != "port" && k != "path" && k != "backends" {
			return a, invalid("unknown endpoint-membership argument %q", k)
		}
	}
	if err := decodeArgs(crit.Args, &a); err != nil {
		return a, err
	}
	if crit.Timeout != EndpointMembershipTimeout {
		return a, invalid("endpoint-membership requires explicit timeout %s", EndpointMembershipTimeout)
	}
	if err := checkSource(a.Source); err != nil {
		return a, err
	}
	if _, err := checkPort(a.Port, 0); err != nil {
		return a, err
	}
	if len(a.Backends) < 2 || len(a.Backends) > maxEndpoints {
		return a, invalid("backends requires 2..%d Deployments", maxEndpoints)
	}
	seen := map[string]bool{}
	for _, b := range a.Backends {
		if len(validation.IsDNS1123Subdomain(b.Deployment)) != 0 || seen[b.Deployment] {
			return a, invalid("backend Deployment names must be valid and unique")
		}
		seen[b.Deployment] = true
		if _, err := buildHTTP(netHTTPArgs{Path: a.Path, BodyMarker: b.BodyMarker}); err != nil {
			return a, err
		}
	}
	return a, nil
}

type membershipObservation struct {
	service object
	anchors map[model.ObjectRef]string
	slices  []string
	members []membershipEndpoint
}
type membershipEndpoint struct {
	deployment, ip string
	port           int
	proof          map[string]any
}

func membershipUnproven(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInsufficientEvidence, fmt.Sprintf(format, args...))
}

// Membership snapshots retain exact UIDs and submitted controller fields; Pod
// and Slice identity is observation evidence, not an input-fingerprint exception.
func membershipStamp(o object) string {
	snapshot := object{"uid": uidOf(o), "generation": generationOf(o), "metadata": submittedMetadata(o), "owners": listAt(o, "metadata", "ownerReferences"), "deleted": stringAt(o, "metadata", "deletionTimestamp"), "payload": submittedPayload(o)}
	if stringAt(o, "kind") == "Pod" {
		snapshot["ready"] = membershipPodReady(o)
		snapshot["ip"] = stringAt(o, "status", "podIP")
		snapshot["ips"] = listAt(o, "status", "podIPs")
	}
	return hashOf(snapshot)
}
func (c *NetworkChecker) membershipNamespace(ctx context.Context, t Target, ns string) (object, error) {
	if t.SessionID == "" || t.Identity.ID == "" || t.Problem.Definition.ID == "" || ns != problemNamespace(t) {
		return nil, membershipUnproven("expected membership session/namespace is missing")
	}
	o, err := c.get(ctx, model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: ns})
	if err != nil {
		return nil, err
	}
	if uidOf(o) == "" || labelOf(o, "junkloud.io/environment-id") != t.Identity.ID || labelOf(o, "junkloud.io/session-id") != t.SessionID || labelOf(o, "junkloud.io/problem-id") != t.Problem.Definition.ID {
		return nil, membershipUnproven("namespace does not match expected membership session")
	}
	return o, nil
}
func controllingReference(o object, api, kind string) (object, error) {
	var owner object
	for _, v := range listAt(o, "metadata", "ownerReferences") {
		m := asMap(v)
		if b, _ := m["controller"].(bool); !b {
			continue
		}
		if owner != nil {
			return nil, membershipUnproven("multiple controlling owners")
		}
		owner = m
	}
	if owner == nil || stringAt(owner, "apiVersion") != api || stringAt(owner, "kind") != kind || stringAt(owner, "name") == "" || stringAt(owner, "uid") == "" {
		return nil, membershipUnproven("missing %s controller UID", kind)
	}
	return owner, nil
}
func (o *membershipObservation) remember(ref model.ObjectRef, obj object) error {
	if uidOf(obj) == "" {
		return membershipUnproven("%s %s has no UID", ref.Kind, ref.Name)
	}
	stamp := membershipStamp(obj)
	if old, ok := o.anchors[ref]; ok && old != stamp {
		return ErrTargetChanged
	}
	o.anchors[ref] = stamp
	return nil
}
func (c *NetworkChecker) membershipGet(ctx context.Context, o *membershipObservation, ref model.ObjectRef) (object, error) {
	obj, err := c.get(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err = o.remember(ref, obj); err != nil {
		return nil, err
	}
	return obj, nil
}

func membershipPodReady(p object) bool {
	if stringAt(p, "metadata", "deletionTimestamp") != "" || stringAt(p, "status", "phase") != "Running" {
		return false
	}
	for _, v := range listAt(p, "status", "conditions") {
		m := asMap(v)
		if stringAt(m, "type") == "Ready" && stringAt(m, "status") == "True" {
			return true
		}
	}
	return false
}
func membershipPodIP(p object, ip string) bool {
	if net.ParseIP(ip) == nil {
		return false
	}
	for _, v := range listAt(p, "status", "podIPs") {
		if stringAt(asMap(v), "ip") == ip {
			return true
		}
	}
	return stringAt(p, "status", "podIP") == ip
}
func membershipNumber(v any) (int, bool) {
	n, ok := ratOf(v)
	if !ok || !n.IsInt() || !n.Num().IsInt64() {
		return 0, false
	}
	i := int(n.Num().Int64())
	return i, i > 0 && i <= 65535
}
func membershipPort(servicePort object, slice object, pod object) (int, error) {
	name := stringAt(servicePort, "name")
	found := 0
	for _, v := range listAt(slice, "ports") {
		p := asMap(v)
		if stringAt(p, "name") != name {
			continue
		}
		if protocol := stringAt(p, "protocol"); protocol != "" && protocol != "TCP" {
			continue
		}
		n, ok := membershipNumber(p["port"])
		if !ok || found != 0 {
			return 0, membershipUnproven("ambiguous EndpointSlice port")
		}
		found = n
	}
	if found == 0 {
		return 0, membershipUnproven("EndpointSlice does not prove Service target port")
	}
	target := servicePort["targetPort"]
	if target == nil {
		target = servicePort["port"]
	}
	if named, ok := target.(string); ok {
		resolved := 0
		for _, v := range listAt(pod, "spec", "containers") {
			for _, pv := range listAt(asMap(v), "ports") {
				p := asMap(pv)
				if stringAt(p, "name") != named {
					continue
				}
				if protocol := stringAt(p, "protocol"); protocol != "" && protocol != "TCP" {
					continue
				}
				n, valid := membershipNumber(p["containerPort"])
				if !valid || (resolved != 0 && resolved != n) {
					return 0, membershipUnproven("ambiguous named Pod TCP port")
				}
				resolved = n
			}
		}
		if resolved == 0 || resolved != found {
			return 0, membershipUnproven("named Pod TCP target port differs from Slice")
		}
	} else if numeric, ok := membershipNumber(target); !ok || numeric != found {
		return 0, membershipUnproven("numeric Service target port differs from Slice")
	}
	return found, nil
}
func (c *NetworkChecker) observeMembership(ctx context.Context, t Target, ref model.ObjectRef, a membershipArgs) (membershipObservation, string, error) {
	o := membershipObservation{anchors: map[model.ObjectRef]string{}}
	ns, err := c.membershipNamespace(ctx, t, ref.Namespace)
	if err != nil {
		return o, "", err
	}
	if err = o.remember(model.ObjectRef{APIVersion: "v1", Kind: "Namespace", Name: ref.Namespace}, ns); err != nil {
		return o, "", err
	}
	svc, err := c.membershipGet(ctx, &o, ref)
	if errors.Is(err, ErrNotFound) {
		return o, "Service is missing", nil
	}
	if err != nil {
		return o, "", err
	}
	o.service = svc
	selector := mapAt(svc, "spec", "selector")
	if len(selector) == 0 {
		return o, "Service has no Pod selector", nil
	}
	var port object
	for _, v := range listAt(svc, "spec", "ports") {
		p := asMap(v)
		if n, ok := membershipNumber(p["port"]); ok && n == *a.Port {
			if protocol := stringAt(p, "protocol"); protocol != "" && protocol != "TCP" {
				continue
			}
			if port != nil {
				return o, "", membershipUnproven("ambiguous Service TCP port")
			}
			port = p
		}
	}
	if port == nil {
		return o, "Service does not expose the requested TCP port", nil
	}
	deployments := map[string]object{}
	for _, b := range a.Backends {
		d, err := c.membershipGet(ctx, &o, model.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: ref.Namespace, Name: b.Deployment})
		if errors.Is(err, ErrNotFound) {
			return o, "required Deployment " + b.Deployment + " is missing", nil
		}
		if err != nil {
			return o, "", err
		}
		deployments[b.Deployment] = d
	}
	slices, err := c.listSlices(ctx, ref)
	if err != nil {
		return o, "", err
	}
	seen := map[string]string{}
	present := map[string]bool{}
	for _, s := range slices {
		if stringAt(s, "metadata", "labels", serviceNameLabel) != ref.Name {
			return o, "", membershipUnproven("Slice Service label differs")
		}
		owner, err := controllingReference(s, "v1", "Service")
		if err != nil {
			return o, "", err
		}
		if stringAt(owner, "name") != ref.Name || stringAt(owner, "uid") != uidOf(svc) {
			return o, "", membershipUnproven("Slice Service owner UID differs")
		}
		if uidOf(s) == "" {
			return o, "", membershipUnproven("Slice has no UID")
		}
		o.slices = append(o.slices, membershipStamp(s))
		for _, v := range listAt(s, "endpoints") {
			ep := asMap(v)
			ready, known := mapAt(ep, "conditions")["ready"].(bool)
			if !known {
				return o, "", membershipUnproven("Slice endpoint readiness is unknown")
			}
			if !ready {
				continue
			}
			if terminating, _ := mapAt(ep, "conditions")["terminating"].(bool); terminating {
				return o, "ready endpoint is terminating", nil
			}
			tr := mapAt(ep, "targetRef")
			if (stringAt(tr, "apiVersion") != "" && stringAt(tr, "apiVersion") != "v1") || stringAt(tr, "kind") != "Pod" || (stringAt(tr, "namespace") != "" && stringAt(tr, "namespace") != ref.Namespace) || stringAt(tr, "uid") == "" || stringAt(tr, "name") == "" {
				return o, "", membershipUnproven("Slice does not prove namespaced Pod target UID")
			}
			pod, err := c.membershipGet(ctx, &o, model.ObjectRef{APIVersion: "v1", Kind: "Pod", Namespace: ref.Namespace, Name: stringAt(tr, "name")})
			if err != nil {
				return o, "", err
			}
			if uidOf(pod) != stringAt(tr, "uid") {
				return o, "", membershipUnproven("Slice target Pod UID differs")
			}
			if !membershipPodReady(pod) {
				return o, "Service endpoint Pod is not Ready", nil
			}
			if !labelsContain(mapAt(pod, "metadata", "labels"), selector) {
				return o, "Service selector does not select endpoint Pod", nil
			}
			rsOwner, err := controllingReference(pod, "apps/v1", "ReplicaSet")
			if err != nil {
				return o, "", err
			}
			rs, err := c.membershipGet(ctx, &o, model.ObjectRef{APIVersion: "apps/v1", Kind: "ReplicaSet", Namespace: ref.Namespace, Name: stringAt(rsOwner, "name")})
			if err != nil {
				return o, "", err
			}
			if uidOf(rs) != stringAt(rsOwner, "uid") {
				return o, "", membershipUnproven("Pod ReplicaSet UID differs")
			}
			depOwner, err := controllingReference(rs, "apps/v1", "Deployment")
			if err != nil {
				return o, "", err
			}
			name := stringAt(depOwner, "name")
			dep := deployments[name]
			if dep == nil {
				dep, err = c.membershipGet(ctx, &o, model.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: ref.Namespace, Name: name})
				if err != nil {
					return o, "", err
				}
				if uidOf(dep) != stringAt(depOwner, "uid") {
					return o, "", membershipUnproven("ReplicaSet Deployment UID differs")
				}
				return o, "ready endpoint belongs to undeclared Deployment " + name, nil
			}
			if !volumeControlledBy(rs, "Deployment", name, uidOf(dep)) {
				return o, "", membershipUnproven("ReplicaSet intended Deployment UID differs")
			}
			targetPort, err := membershipPort(port, s, pod)
			if err != nil {
				return o, "", err
			}
			addresses := listAt(ep, "addresses")
			if len(addresses) == 0 {
				return o, "", membershipUnproven("ready endpoint has no IP")
			}
			for _, av := range addresses {
				ip, ok := av.(string)
				if !ok || !membershipPodIP(pod, ip) {
					return o, "", membershipUnproven("Slice IP differs from actual Pod IP")
				}
				identity := fmt.Sprintf("%s/%d", uidOf(pod), targetPort)
				if old, exists := seen[ip]; exists {
					if old != identity {
						return o, "", membershipUnproven("conflicting endpoint address")
					}
					continue
				}
				seen[ip] = identity
				o.members = append(o.members, membershipEndpoint{deployment: name, ip: ip, port: targetPort, proof: map[string]any{"deployment": name, "deployment_uid": uidOf(dep), "replica_set": nameOf(rs), "replica_set_uid": uidOf(rs), "pod": nameOf(pod), "pod_uid": uidOf(pod), "slice": nameOf(s), "slice_uid": uidOf(s), "ip": ip, "port": targetPort}})
				if len(o.members) > maxEndpoints {
					return o, "", membershipUnproven("more than %d ready endpoints", maxEndpoints)
				}
			}
			present[name] = true
		}
	}
	for _, b := range a.Backends {
		if !present[b.Deployment] {
			return o, "required Deployment " + b.Deployment + " has no Ready Service member", nil
		}
	}
	sort.Strings(o.slices)
	return o, "", nil
}

func (c *NetworkChecker) recheckMembership(ctx context.Context, t Target, ref model.ObjectRef, o membershipObservation) error {
	if _, err := c.membershipNamespace(ctx, t, ref.Namespace); err != nil {
		return err
	}
	for key, stamp := range o.anchors {
		again, err := c.get(ctx, key)
		if err != nil {
			return err
		}
		if membershipStamp(again) != stamp {
			return fmt.Errorf("%w: %s %s changed during membership probes", ErrTargetChanged, key.Kind, key.Name)
		}
	}
	slices, err := c.listSlices(ctx, ref)
	if err != nil {
		return err
	}
	var stamps []string
	for _, s := range slices {
		stamps = append(stamps, membershipStamp(s))
	}
	sort.Strings(stamps)
	if !reflect.DeepEqual(stamps, o.slices) {
		return fmt.Errorf("%w: EndpointSlice observation changed during probes", ErrTargetChanged)
	}
	return nil
}
func (c *NetworkChecker) checkMembership(ctx context.Context, t Target, crit model.Criterion) (model.CheckResult, error) {
	a, err := parseMembership(crit)
	if err != nil {
		return model.CheckResult{}, err
	}
	ref, err := resolveTarget(t, crit)
	if err != nil {
		return model.CheckResult{}, err
	}
	source := a.Source.Namespace
	if source == "" {
		source = problemNamespace(t)
	}
	if source != ref.Namespace {
		return model.CheckResult{}, membershipUnproven("probe source namespace differs from owned problem namespace")
	}
	ctx, cancel := context.WithTimeout(ctx, crit.Timeout)
	defer cancel()
	start := c.set.clock.Now()
	deadline := start.Add(crit.Timeout)
	o, fail, err := c.observeMembership(ctx, t, ref, a)
	if err != nil {
		return model.CheckResult{}, err
	}
	result := model.CheckResult{Evidence: baseEvidence(c.set.clock.Now(), start, o.service)}
	result.Evidence.Expected = map[string]any{"backends": a.Backends, "port": *a.Port}
	if fail != "" {
		result.Status = model.StatusFail
		result.Reason = fail
		return result, nil
	}
	dial := fmt.Sprintf("%s.%s.%s", ref.Name, ref.Namespace, clusterDomain)
	dst := destination{dial: dial, httpHost: dial}
	expect := netExpect{source: a.Source, port: *a.Port}
	jobs := []*probeJob{{kind: "service", req: c.request(t, expect, dial, *a.Port, dst, deadline)}}
	for _, ep := range o.members {
		for _, b := range a.Backends {
			if b.Deployment != ep.deployment {
				continue
			}
			h, _ := buildHTTP(netHTTPArgs{Path: a.Path, BodyMarker: b.BodyMarker})
			e := expect
			e.http = h
			q := c.request(t, e, ep.ip, ep.port, dst, deadline)
			q.ResolveName = dial
			jobs = append(jobs, &probeJob{kind: "backend", req: q})
		}
	}
	if len(jobs) > maxProbes {
		return model.CheckResult{}, membershipUnproven("probe bound exceeded")
	}
	if c.runner == nil {
		return model.CheckResult{}, notConfigured("endpoint membership probe runner")
	}
	c.runJobs(ctx, jobs)
	if err := ctx.Err(); err != nil {
		return model.CheckResult{}, err
	}
	if err := c.recheckMembership(ctx, t, ref, o); err != nil {
		return model.CheckResult{}, err
	}
	var failures []string
	observed := []any{}
	for _, j := range jobs {
		if j.err != nil {
			return model.CheckResult{}, j.err
		}
		q, p := j.req, j.res
		observed = append(observed, map[string]any{"host": q.Host, "port": q.Port, "protocol": q.Protocol, "expected_marker": q.BodyMarker, "result": p})
		switch p.FailureClass {
		case FailureDNS, FailureNetwork, FailureProbeError:
			return model.CheckResult{}, membershipUnproven("probe %s is unverifiable: %s", q.Host, p.FailureClass)
		case "", FailureHTTPMismatch, FailureRefused, FailureTimeout:
		default:
			return model.CheckResult{}, membershipUnproven("unrecognized probe outcome")
		}
		if p.FailureClass != "" || !p.Connected || (q.Protocol == ProbeHTTP && (p.HTTPStatus != 200 || p.BodyMarker != q.BodyMarker)) {
			failures = append(failures, q.Host)
		}
	}
	memberEvidence := []any{}
	for _, ep := range o.members {
		memberEvidence = append(memberEvidence, ep.proof)
	}
	result.Evidence.Actual = map[string]any{"ready_members": memberEvidence, "service_uid": uidOf(o.service), "probes": observed}
	result.Evidence.Duration = c.set.clock.Now().Sub(start)
	result.Status = model.StatusPass
	result.Reason = "every named backend is a Ready Service member and serves its expected HTTP content"
	if len(failures) > 0 {
		result.Status = model.StatusFail
		result.Reason = fmt.Sprintf("Service/backend connection or application check failed: %v", failures)
	}
	return result, nil
}

func (c *ArtifactChecker) captureMembership(ctx context.Context, t Target, crit model.Criterion, out map[string]string) error {
	a, err := parseMembership(crit)
	if err != nil {
		return err
	}
	ref, err := resolveTarget(t, crit)
	if err != nil {
		return err
	}
	if err = c.captureDeep(ctx, ref, out, 0); err != nil {
		return err
	}
	for _, b := range a.Backends {
		if err = c.captureDeep(ctx, model.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: ref.Namespace, Name: b.Deployment}, out, 0); err != nil {
			return err
		}
	}
	return c.captureNetworkPolicies(ctx, t, crit, out)
}
