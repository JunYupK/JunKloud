package grader

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
)

// Limits of one criterion's probe set. NetworkPolicy is checked pod by pod
// (see judgeDenyOutcome), so the number of probes grows with the number of
// ready endpoints.
const (
	maxEndpoints = 4
	maxProbes    = 16
)

// outcomeState is the judgement of one expectation. The zero value is
// stateUnknown, so an outcome that no judging code assigned can never read as
// holding (and PASS a criterion).
type outcomeState int

const (
	stateUnknown outcomeState = iota
	stateHolds
	stateFails
)

func (s outcomeState) String() string {
	switch s {
	case stateHolds:
		return "holds"
	case stateFails:
		return "fails"
	}
	return "unknown"
}

// probeJob is one probe of a criterion.
//
//	service      an allow expectation through the Service name (routing, DNS)
//	pod-deny     a deny expectation against one ready pod (address, target port)
//	pod-control  an allowed source reaching that same pod and port
type probeJob struct {
	kind string
	exp  int       // expectation index (the allow expectation for a control)
	pod  string    // pod address for pod-deny / pod-control
	http *httpSpec // what an HTTP probe must see
	req  ProbeRequest
	res  ProbeResult
	err  error
	ran  bool
}

// resolves reports whether the probe resolved a name from its source pod.
func (j *probeJob) resolves() bool {
	return j.req.ResolveName != "" || net.ParseIP(j.req.Host) == nil
}

// bad reports a probe that did not produce a result.
func (j *probeJob) bad() (string, bool) {
	switch {
	case j.err != nil:
		msg := strings.TrimPrefix(j.err.Error(), ErrProbeFailed.Error()+": ")
		return msg, true
	case !j.ran:
		return "the probe did not run", true
	case j.res.FailureClass == FailureProbeError:
		return "the probe could not run or report", true
	}
	return "", false
}

func (j *probeJob) evidence() map[string]any {
	m := map[string]any{"probe": j.kind, "host": j.req.Host, "port": j.req.Port, "protocol": j.req.Protocol}
	if j.err != nil {
		m["error"] = j.err.Error()
	}
	if j.ran {
		m["connected"] = j.res.Connected
		if j.res.HTTPStatus != 0 {
			m["http_status"] = j.res.HTTPStatus
		}
		if j.res.FailureClass != "" {
			m["failure_class"] = j.res.FailureClass
		}
	}
	return m
}

// podCheck pairs the deny probe and the allow control of one ready pod.
type podCheck struct {
	pod        string
	deny, ctrl *probeJob
}

// netOutcome is the judgement of one expectation.
type netOutcome struct {
	exp   netExpect
	state outcomeState
	text  string
	err   error
	main  *probeJob  // allow expectations: the probe through the Service name
	pods  []podCheck // deny expectations: one check per ready pod
}

func (o netOutcome) evidence() map[string]any {
	m := map[string]any{"expectation": o.exp.name, "expect": map[bool]string{true: "allow", false: "deny"}[o.exp.allow],
		"port": o.exp.port, "state": o.state.String()}
	var probes []any
	if o.main != nil {
		probes = append(probes, o.main.evidence())
	}
	for _, pc := range o.pods {
		probes = append(probes, pc.deny.evidence(), pc.ctrl.evidence())
	}
	if probes != nil {
		m["probes"] = probes
	}
	if o.text != "" {
		m["note"] = o.text
	}
	return m
}

// endpointRef is one ready pod behind the Service.
type endpointRef struct {
	ip    string
	ports map[string]int // EndpointSlice port name -> target port number
}

// destination is what the probes dial.
type destination struct {
	dial      string // name or IP
	httpHost  string // default Host header
	portNames map[int]string
	endpoints []endpointRef
}

func (d destination) targetPort(ep endpointRef, servicePort int) (int, bool) {
	n, ok := ep.ports[d.portNames[servicePort]]
	return n, ok
}

// Check implements Checker.
func (c *NetworkChecker) Check(ctx context.Context, t Target, crit model.Criterion) (model.CheckResult, error) {
	plan, err := c.plan(crit)
	if err != nil {
		return model.CheckResult{}, err
	}
	ref, err := resolveTarget(t, crit)
	if err != nil {
		return model.CheckResult{}, err
	}
	// Everything below, the Ingress wait included, happens within the
	// criterion's declared timeout (MinNetworkTimeout guarantees it is
	// enough for the normal paths).
	start := c.set.clock.Now()
	deadline := start.Add(timeoutFor(crit, c.set))
	dst, obj, fail, err := c.destination(ctx, crit, ref, plan)
	if err != nil {
		return model.CheckResult{}, err
	}
	if fail != "" {
		res := model.CheckResult{Status: model.StatusFail, Reason: fail, Evidence: baseEvidence(c.set.clock.Now(), start, obj)}
		res.Evidence.Expected = describePlan(plan, ref)
		return res, nil
	}

	jobs, outcomes, err := c.buildJobs(t, plan, dst, deadline)
	if err != nil {
		return model.CheckResult{}, err
	}
	c.runJobs(ctx, jobs)
	if err := ctx.Err(); err != nil {
		return model.CheckResult{}, err
	}
	c.judge(ctx, t, dst, outcomes, jobs, deadline)
	if err := ctx.Err(); err != nil {
		return model.CheckResult{}, err
	}

	// The destination must still be the object we probed (spec 8.3).
	if again, err := c.get(ctx, ref); err != nil {
		if errors.Is(err, ErrNotFound) {
			return model.CheckResult{}, fmt.Errorf("%w: %s %s no longer exists", ErrTargetChanged, ref.Kind, ref.Name)
		}
		return model.CheckResult{}, err
	} else if keyOf(again) != keyOf(obj) {
		return model.CheckResult{}, fmt.Errorf("%w: %s %s was replaced during the probes", ErrTargetChanged, ref.Kind, ref.Name)
	}

	res := model.CheckResult{Evidence: baseEvidence(c.set.clock.Now(), start, obj)}
	res.Evidence.Expected = describePlan(plan, ref)
	probes := make([]any, 0, len(outcomes))
	var fails, unknowns []string
	for _, o := range outcomes {
		probes = append(probes, o.evidence())
		switch o.state {
		case stateFails:
			fails = append(fails, fmt.Sprintf("%s: %s", o.exp.name, o.text))
		case stateUnknown:
			unknowns = append(unknowns, fmt.Sprintf("%s: %s", o.exp.name, o.text))
		}
	}
	res.Evidence.Actual = map[string]any{"destination": dst.dial, "ready_endpoints": len(dst.endpoints), "probes": probes}
	switch {
	case len(fails) > 0:
		res.Status = model.StatusFail
		res.Reason = fmt.Sprintf("%d of %d expectations not met: %s", len(fails), len(outcomes), strings.Join(fails, "; "))
		if len(unknowns) > 0 {
			res.Reason += "; undecided: " + strings.Join(unknowns, "; ")
		}
	case len(unknowns) > 0:
		cause := ErrInsufficientEvidence
		for _, o := range outcomes {
			if o.state == stateUnknown && o.err != nil && errors.Is(o.err, ErrProbeFailed) {
				cause = ErrProbeFailed
			}
		}
		return model.CheckResult{}, fmt.Errorf("%w: %s", cause, strings.Join(unknowns, "; "))
	default:
		res.Status = model.StatusPass
	}
	return res, nil
}

func describePlan(p netPlan, ref model.ObjectRef) map[string]any {
	exps := make([]any, 0, len(p.expects))
	for _, e := range p.expects {
		exps = append(exps, map[string]any{"name": e.name, "allow": e.allow, "port": e.port})
	}
	return map[string]any{"destination": describeRef(ref), "expectations": exps}
}

// destination reads the target. fail is a learner-visible reason for a FAIL
// that needs no probe.
func (c *NetworkChecker) destination(ctx context.Context, crit model.Criterion, ref model.ObjectRef, p netPlan) (destination, object, string, error) {
	if p.ingress {
		return c.ingressDestination(ctx, crit, ref)
	}
	obj, err := c.get(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return destination{}, nil, fmt.Sprintf("%s: Service %s/%s not found", ReasonPrerequisite, ref.Namespace, ref.Name), nil
	}
	if err != nil {
		return destination{}, nil, "", err
	}
	d := destination{dial: fmt.Sprintf("%s.%s.%s", ref.Name, ref.Namespace, clusterDomain), portNames: map[int]string{}}
	d.httpHost = d.dial
	for _, raw := range listAt(obj, "spec", "ports") {
		pm, _ := raw.(map[string]any)
		if n, ok := ratOf(pm["port"]); ok && n.IsInt() {
			name, _ := pm["name"].(string)
			d.portNames[int(n.Num().Int64())] = name
		}
	}
	for _, e := range p.expects {
		if _, ok := d.portNames[e.port]; !ok {
			return d, obj, fmt.Sprintf("Service %s/%s does not expose port %d", ref.Namespace, ref.Name, e.port), nil
		}
	}
	slices, err := c.listSlices(ctx, ref)
	if err != nil {
		return d, obj, "", err
	}
	seen := map[string]bool{}
	for _, s := range slices {
		if stringAt(s, "metadata", "labels", serviceNameLabel) != ref.Name {
			continue
		}
		ports := map[string]int{}
		for _, raw := range listAt(s, "ports") {
			pm, _ := raw.(map[string]any)
			name, _ := pm["name"].(string)
			if n, ok := ratOf(pm["port"]); ok && n.IsInt() {
				ports[name] = int(n.Num().Int64())
			}
		}
		for _, raw := range listAt(s, "endpoints") {
			ep, _ := raw.(map[string]any)
			if r, set := mapAt(ep, "conditions")["ready"]; set && r == false {
				continue
			}
			addrs := listAt(ep, "addresses")
			if len(addrs) == 0 {
				continue
			}
			ip, _ := addrs[0].(string)
			if ip == "" || seen[ip] {
				continue
			}
			seen[ip] = true
			d.endpoints = append(d.endpoints, endpointRef{ip: ip, ports: ports})
		}
	}
	if len(d.endpoints) == 0 {
		return d, obj, fmt.Sprintf("Service %s/%s has no ready endpoints: its selector matches no ready pod, so nothing can be reached or blocked meaningfully", ref.Namespace, ref.Name), nil
	}
	return d, obj, "", nil
}

func (c *NetworkChecker) listSlices(ctx context.Context, ref model.ObjectRef) ([]map[string]any, error) {
	var last error
	for attempt := 0; attempt < apiAttempts; attempt++ {
		out, err := c.reader.List(ctx, ListQuery{APIVersion: endpointSliceAPI, Kind: "EndpointSlice", Namespace: ref.Namespace,
			LabelSelector: serviceNameLabel + "=" + ref.Name})
		if err == nil {
			return out, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		last = err
		if attempt < apiAttempts-1 {
			if err := c.set.clock.Sleep(ctx, c.set.pollInterval); err != nil {
				return nil, err
			}
		}
	}
	return nil, last
}

func (c *NetworkChecker) ingressDestination(ctx context.Context, crit model.Criterion, ref model.ObjectRef) (destination, object, string, error) {
	spec := observeSpec{
		ref: ref, timeout: min(ingressWait, timeoutFor(crit, c.set)/2), poll: true, waitForCreate: false,
		eval: func(o object) verdict {
			if ingressAddress(o) != "" {
				return verdict{ok: true}
			}
			return verdict{reason: "no load balancer address yet"}
		},
	}
	obs, err := c.observe(ctx, spec)
	if err != nil {
		return destination{}, nil, "", err
	}
	if !obs.found {
		return destination{}, nil, fmt.Sprintf("%s: Ingress %s/%s not found", ReasonPrerequisite, ref.Namespace, ref.Name), nil
	}
	if obs.v.ok {
		d := destination{dial: ingressAddress(obs.obj)}
		if rules := listAt(obs.obj, "spec", "rules"); len(rules) > 0 {
			if r, ok := rules[0].(map[string]any); ok {
				d.httpHost, _ = r["host"].(string)
			}
		}
		if d.httpHost == "" {
			d.httpHost = d.dial
		}
		return d, obs.obj, "", nil
	}
	if class := stringAt(obs.obj, "spec", "ingressClassName"); class != "" && class != ingressClassOwn {
		return destination{}, obs.obj, fmt.Sprintf("Ingress %s/%s names ingress class %q, which no controller of this environment serves", ref.Namespace, ref.Name, class), nil
	}
	return destination{}, nil, "", fmt.Errorf("%w: Ingress %s/%s has no load balancer address; the environment's ingress controller did not assign one", ErrInsufficientEvidence, ref.Namespace, ref.Name)
}

func ingressAddress(o object) string {
	for _, raw := range listAt(o, "status", "loadBalancer", "ingress") {
		m, _ := raw.(map[string]any)
		if ip, _ := m["ip"].(string); ip != "" {
			return ip
		}
		if h, _ := m["hostname"].(string); h != "" {
			return h
		}
	}
	return ""
}

// request builds the probe of expectation e against host:port.
func (c *NetworkChecker) request(t Target, e netExpect, host string, port int, d destination, deadline time.Time) ProbeRequest {
	ns := e.source.Namespace
	if ns == "" {
		ns = t.Problem.Namespace
		if ns == "" {
			ns = t.Problem.Definition.Namespace
		}
	}
	req := ProbeRequest{
		Protocol: ProbeTCP, Namespace: ns, Labels: e.source.Labels, Host: host, Port: port,
		Attempts: ProbeAttempts, Timeout: ProbeAttemptTimeout, Deadline: deadline,
		EnvironmentID: t.Identity.ID, ProblemID: t.Problem.Definition.ID,
	}
	if e.http != nil {
		req.Protocol = ProbeHTTP
		req.HTTPHost = e.http.host
		if req.HTTPHost == "" {
			req.HTTPHost = d.httpHost
		}
		req.Path, req.ExpectStatus, req.BodyMarker = e.http.path, e.http.status, e.http.marker
	}
	return req
}

// buildJobs lists every probe of the criterion.
//
// kube-proxy picks a backend per connection, so a probe through the Service
// name proves nothing about any particular pod: one backend may be covered by
// the learner's policy and another not, or one may be alive and another not.
// NetworkPolicy is evaluated on the pod address after the Service's DNAT, so
// each deny expectation is checked against every ready pod's address and
// target port, and each of those has its own allowed-source control.
func (c *NetworkChecker) buildJobs(t Target, p netPlan, d destination, deadline time.Time) ([]*probeJob, []*netOutcome, error) {
	outs := make([]*netOutcome, len(p.expects))
	var jobs []*probeJob
	controlFor := map[int]int{} // service port -> first allow expectation
	hasDeny := false
	for i, e := range p.expects {
		outs[i] = &netOutcome{exp: e}
		if e.allow {
			if _, ok := controlFor[e.port]; !ok {
				controlFor[e.port] = i
			}
			j := &probeJob{kind: "service", exp: i, http: e.http, req: c.request(t, e, d.dial, e.port, d, deadline)}
			outs[i].main = j
			jobs = append(jobs, j)
		} else {
			hasDeny = true
		}
	}
	if hasDeny {
		if len(d.endpoints) > maxEndpoints {
			return nil, nil, fmt.Errorf("%w: the Service has %d ready endpoints; at most %d can be verified pod by pod", ErrInsufficientEvidence, len(d.endpoints), maxEndpoints)
		}
		ctrl := map[[2]int]*probeJob{} // (allow expectation, endpoint) -> control
		for i, e := range p.expects {
			if e.allow {
				continue
			}
			a := controlFor[e.port]
			for k, ep := range d.endpoints {
				tp, ok := d.targetPort(ep, e.port)
				if !ok {
					return nil, nil, fmt.Errorf("%w: the EndpointSlice of pod %s lists no port for Service port %d, so it cannot be probed", ErrInsufficientEvidence, ep.ip, e.port)
				}
				dj := &probeJob{kind: "pod-deny", exp: i, pod: ep.ip, req: c.request(t, e, ep.ip, tp, d, deadline)}
				dj.req.ResolveName = d.dial // DNS is still proven from the denied source
				cj := ctrl[[2]int{a, k}]
				if cj == nil {
					cj = &probeJob{kind: "pod-control", exp: a, pod: ep.ip, http: p.expects[a].http, req: c.request(t, p.expects[a], ep.ip, tp, d, deadline)}
					ctrl[[2]int{a, k}] = cj
					jobs = append(jobs, cj)
				}
				jobs = append(jobs, dj)
				outs[i].pods = append(outs[i].pods, podCheck{pod: ep.ip, deny: dj, ctrl: cj})
			}
		}
	}
	if len(jobs) > maxProbes {
		return nil, nil, fmt.Errorf("%w: the criterion needs %d probes (at most %d): split it", ErrInsufficientEvidence, len(jobs), maxProbes)
	}
	return jobs, outs, nil
}

// budgetLeft is the time left of the criterion's budget.
func (c *NetworkChecker) budgetLeft(deadline time.Time) time.Duration {
	return deadline.Sub(c.set.clock.Now())
}

var errBudget = fmt.Errorf("%w: the criterion's time budget is used up", ErrInsufficientEvidence)

// runJobs runs every probe concurrently.
func (c *NetworkChecker) runJobs(ctx context.Context, jobs []*probeJob) {
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					j.err = fmt.Errorf("%w: probe panic: %v", ErrProbeFailed, r)
				}
			}()
			if c.budgetLeft(j.req.Deadline) < probeFloor {
				j.err = errBudget
				return
			}
			j.res, j.err = c.runner.Probe(ctx, j.req)
			j.ran = j.err == nil
		}()
	}
	wg.Wait()
}

// judge decides every outcome.
func (c *NetworkChecker) judge(ctx context.Context, t Target, d destination, outs []*netOutcome, jobs []*probeJob, deadline time.Time) {
	neutral := &neutralDNS{c: c, ctx: ctx, t: t, d: d, deadline: deadline}
	for _, o := range outs {
		if o.exp.allow {
			c.judgeAllowOutcome(o, jobs, neutral, d)
		} else {
			c.judgeDenyOutcome(o, jobs, neutral, d)
		}
	}
}

func unknownFrom(o *netOutcome, j *probeJob, text string) {
	o.state, o.text, o.err = stateUnknown, text, j.err
	if j.err == nil && j.res.FailureClass == FailureProbeError {
		o.err = ErrProbeFailed
	}
}

func (c *NetworkChecker) judgeAllowOutcome(o *netOutcome, jobs []*probeJob, n *neutralDNS, d destination) {
	j := o.main
	if text, bad := j.bad(); bad {
		unknownFrom(o, j, text)
		return
	}
	if j.res.FailureClass == FailureDNS {
		c.judgeDNS(o, j, jobs, n, d)
		return
	}
	o.state, o.text = judgeAllowResult(j.res, j.http)
}

// judgeAllowResult judges a probe that was expected to connect. Only a
// refusal or a timeout is evidence of a fault of the allowed path; any other
// failure does not show whose fault it is (spec 8.1: ERROR).
func judgeAllowResult(r ProbeResult, h *httpSpec) (outcomeState, string) {
	switch {
	case !r.Connected:
		switch r.FailureClass {
		case FailureTimeout, FailureRefused:
			return stateFails, fmt.Sprintf("the allowed path did not connect (%s)", r.FailureClass)
		}
		return stateUnknown, fmt.Sprintf("the allowed path failed in an unclassified way (%s), which does not show whose fault it is", classOrUnknown(r.FailureClass))
	case h == nil:
		return stateHolds, ""
	case r.HTTPStatus != h.status:
		if r.HTTPStatus == 0 {
			return stateFails, "connected, but the server sent no HTTP answer (http-mismatch)"
		}
		return stateFails, fmt.Sprintf("answered HTTP %d, expected %d (http-mismatch)", r.HTTPStatus, h.status)
	case r.BodyMarker != h.marker:
		return stateFails, fmt.Sprintf("answered HTTP %d but the body lacks the expected application marker: a different backend answered (http-mismatch)", r.HTTPStatus)
	}
	return stateHolds, ""
}

func classOrUnknown(class string) string {
	if class == "" {
		return "no reason reported"
	}
	return class
}

// judgeDenyOutcome decides a deny expectation. It holds only if, for EVERY
// ready pod, the denied source timed out while an allowed source connected
// to that same pod and port (the pod is alive and listening, so the silence
// is the policy). A refusal is not accepted as a block: a pod answers it
// itself when nothing listens, and kind's policy enforcement drops instead.
func (c *NetworkChecker) judgeDenyOutcome(o *netOutcome, jobs []*probeJob, n *neutralDNS, d destination) {
	if len(o.pods) == 0 {
		o.state, o.text = stateUnknown, "no ready pod was probed, so nothing shows the block"
		return
	}
	var fails, unknowns []string
	for _, pc := range o.pods {
		for _, j := range []*probeJob{pc.deny, pc.ctrl} {
			if text, bad := j.bad(); bad {
				unknownFrom(o, j, fmt.Sprintf("pod %s: %s", pc.pod, text))
				unknowns = append(unknowns, o.text)
			}
		}
	}
	if len(unknowns) > 0 {
		o.text = strings.Join(unknowns, "; ")
		return
	}
	for _, pc := range o.pods {
		if pc.deny.res.FailureClass == FailureDNS {
			c.judgeDNS(o, pc.deny, jobs, n, d)
			return
		}
	}
	for _, pc := range o.pods {
		cs, ct := judgeAllowResult(pc.ctrl.res, pc.ctrl.http)
		r := pc.deny.res
		var ds outcomeState
		var dt string
		switch {
		case r.Connected:
			ds, dt = stateFails, "connected although this source must be denied"
		case r.FailureClass == FailureTimeout:
			ds = stateHolds
		case r.FailureClass == FailureRefused:
			ds, dt = stateUnknown, "the connection was refused, which is not how a NetworkPolicy blocks (it drops); a refusal comes from the pod or the network path, so it proves no block"
		default:
			ds, dt = stateUnknown, fmt.Sprintf("the connection failed (%s), which does not show a block", classOrUnknown(r.FailureClass))
		}
		if cs == stateFails {
			fails = append(fails, fmt.Sprintf("pod %s: the allowed control did not succeed (%s), so the pod is not shown to be healthy and a block there proves nothing", pc.pod, ct))
		} else if cs == stateUnknown {
			unknowns = append(unknowns, fmt.Sprintf("pod %s: allowed control: %s", pc.pod, ct))
		}
		switch ds {
		case stateFails:
			fails = append(fails, fmt.Sprintf("pod %s: %s", pc.pod, dt))
		case stateUnknown:
			unknowns = append(unknowns, fmt.Sprintf("pod %s: %s", pc.pod, dt))
		}
	}
	switch {
	case len(fails) > 0:
		o.state, o.text = stateFails, strings.Join(fails, "; ")
	case len(unknowns) > 0:
		o.state, o.text = stateUnknown, strings.Join(unknowns, "; ")
	default:
		o.state = stateHolds // every pod: denied source silent, allowed control answered
	}
}

// judgeDNS decides an expectation whose probe could not resolve the name.
// A DNS failure is never an allow and never a deny; whose fault it is depends
// on whether anyone else can resolve the name.
func (c *NetworkChecker) judgeDNS(o *netOutcome, self *probeJob, all []*probeJob, n *neutralDNS, d destination) {
	for _, a := range all {
		if a != self && a.ran && a.err == nil && a.resolves() &&
			a.res.FailureClass != FailureDNS && a.res.FailureClass != FailureProbeError {
			o.state, o.text = stateFails, fmt.Sprintf("this source could not resolve %s although another source could (egress or DNS settings of the source)", d.dial)
			return
		}
	}
	switch ok, err := n.resolves(); {
	case err != nil:
		o.state, o.text, o.err = stateUnknown, "cluster DNS could not be verified: "+strings.TrimPrefix(err.Error(), ErrProbeFailed.Error()+": "), err
	case ok:
		o.state, o.text = stateFails, fmt.Sprintf("this source could not resolve %s although a neutral pod can", d.dial)
	default:
		o.state, o.text = stateUnknown, fmt.Sprintf("%s does not resolve from any pod; cluster DNS looks unhealthy", d.dial)
	}
}

// neutralDNS resolves the destination from a pod without any source labels,
// once, only when a source failed to resolve.
type neutralDNS struct {
	c        *NetworkChecker
	ctx      context.Context
	t        Target
	d        destination
	deadline time.Time
	once     sync.Once
	ok       bool
	err      error
}

func (n *neutralDNS) resolves() (bool, error) {
	n.once.Do(func() {
		ns := n.c.set.controlNamespace
		if ns == "" {
			n.err = errors.New("no control namespace is configured")
			return
		}
		if n.c.budgetLeft(n.deadline) < probeFloor {
			n.err = errBudget
			return
		}
		res, err := n.c.runner.Probe(n.ctx, ProbeRequest{
			Protocol: ProbeDNS, Namespace: ns, Host: n.d.dial, Attempts: ProbeAttempts, Timeout: ProbeAttemptTimeout,
			Deadline: n.deadline, EnvironmentID: n.t.Identity.ID, ProblemID: n.t.Problem.Definition.ID,
		})
		switch {
		case err != nil:
			n.err = err
		case res.FailureClass == FailureProbeError:
			n.err = fmt.Errorf("%w: the control probe could not run", ErrProbeFailed)
		default:
			n.ok = res.FailureClass != FailureDNS && res.Connected
		}
	})
	return n.ok, n.err
}
