package grader

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/JunYupK/JunKloud/internal/model"
)

// Probe protocols.
const (
	// ProbeHTTP sends one HTTP/1.0 GET and reports the status and whether
	// the expected marker is in the body.
	ProbeHTTP = "http"
	// ProbeTCP only completes a TCP handshake.
	ProbeTCP = "tcp"
	// ProbeDNS only resolves the name (the neutral DNS control).
	ProbeDNS = "dns"
)

// FailureClass values of a ProbeResult. An empty class means the probe
// succeeded.
const (
	// FailureDNS: the name did not resolve from the probe pod.
	FailureDNS = "dns"
	// FailureRefused: the destination actively rejected the connection.
	FailureRefused = "refused"
	// FailureTimeout: nothing answered within the attempt timeout (what a
	// dropping NetworkPolicy looks like).
	FailureTimeout = "timeout"
	// FailureHTTPMismatch: a server answered, but not with the expected
	// status or application marker (or not with HTTP at all).
	FailureHTTPMismatch = "http-mismatch"
	// FailureProbeError: the probe itself could not run or report. The
	// criterion is ERROR, never FAIL.
	FailureProbeError = "probe-error"
	// FailureNetwork: the connection failed in a way that is neither a clean
	// refusal nor a timeout. It proves nothing about whose fault it is.
	FailureNetwork = "network-error"
)

// Network probing bounds (spec 8.3): every attempt waits at most
// ProbeAttemptTimeout, and a probe makes at most ProbeAttempts attempts.
const (
	ProbeAttempts       = 3
	ProbeAttemptTimeout = 3 * time.Second
)

const (
	clusterDomain     = "svc.cluster.local"
	maxExpectations   = 8
	maxMarkerLen      = 256
	maxConcurrentProb = 4
	reservedLabelNS   = "junkloud.io/"
	ingressClassOwn   = "cloud-provider-kind"
	endpointSliceAPI  = "discovery.k8s.io/v1"
	serviceNameLabel  = "kubernetes.io/service-name"
)

// ErrProbeFailed marks a probe that could not run or report (the probe pod
// could not be scheduled or started, the probe image is missing, the result
// was unreadable). It is an environment fault: graded ERROR.
var ErrProbeFailed = errors.New("probe could not run")

// ProbeRequest is one connection attempt series from a declared source
// identity: a pod in Namespace carrying Labels (so the learner's
// NetworkPolicy selectors apply to it) dials Host:Port.
type ProbeRequest struct {
	Protocol  string // ProbeHTTP, ProbeTCP or ProbeDNS
	Namespace string
	Labels    map[string]string
	Host      string // name or IP to dial
	Port      int
	// HTTP only.
	HTTPHost     string // Host header
	Path         string
	ExpectStatus int
	BodyMarker   string // expected application marker in the body
	// Bounds. Attempts <= ProbeAttempts, Timeout <= ProbeAttemptTimeout.
	Attempts int
	Timeout  time.Duration
	// Ownership labels of the probe pod.
	EnvironmentID string
	ProblemID     string
}

// ProbeResult is what one probe observed. Connected means a connection (and
// for HTTP a response) was established at all. HTTPStatus is the status of the
// last response (0 if none). BodyMarker is the request's marker when the body
// contained it, otherwise empty. FailureClass is empty for a probe that
// reached its goal, otherwise one of the Failure* values; FailureDNS means
// name resolution failed, any other class means the name resolved.
type ProbeResult struct {
	Connected    bool   `json:"connected"`
	HTTPStatus   int    `json:"http_status"`
	BodyMarker   string `json:"body_marker"`
	FailureClass string `json:"failure_class"`
}

// ProbeRunner runs one probe. A probe that ran and observed a failure
// returns it in the result; an error (wrapping ErrProbeFailed) means the
// probe itself could not run and is graded ERROR.
type ProbeRunner interface {
	Probe(ctx context.Context, req ProbeRequest) (ProbeResult, error)
}

// NetworkChecker grades model.CriterionHTTP, model.CriterionTCP and
// model.CriterionNetworkPolicy by real connections from declared source
// identities (spec 8.2).
//
// Targets: a Service (v1) for tcp and network-policy; a Service or an Ingress
// (networking.k8s.io/v1) for http. Service names are dialed as
// <name>.<namespace>.svc.cluster.local; an Ingress is dialed at its load
// balancer address with the Host header of args.host (default: the first
// rule's host). Ingress and its address are polled for up to half of the
// criterion's timeout.
//
// Args (strict; "mutates" is accepted everywhere). source is the identity of
// the probe pod: {namespace: default the problem namespace, labels: pod
// labels}. Labels under "junkloud.io/" are reserved.
//
//	http:           source, port (Service: required; Ingress: default 80),
//	                host, path (default "/"), status (default 200),
//	                body_marker (required: the application identity)
//	tcp:            source, port (required)
//	network-policy: expectations: [ {name?, expect: allow|deny, source, port,
//	                http?: {host, path, status, body_marker}} ]
//
// A network-policy criterion holds only if EVERY expectation holds together.
// It needs at least one allow and one deny, and every deny port needs an
// allow on the same port: that allowed request is the control that the
// destination is healthy. A deny expectation passes only when
//
//  1. the destination has ready endpoints and an allowed source reached the
//     same port in this run (a dead or hung server blocks everybody),
//  2. the denied source resolved the destination name (a DNS failure is never
//     a deny), and
//  3. the denied path timed out or was refused while the allow connected.
//
// A deny expectation is checked at connection level (TCP).
//
// Probes may not run in parallel with any other criterion (Mutates): they
// create pods in the problem's namespaces.
type NetworkChecker struct {
	observer
	runner ProbeRunner
}

// NewNetworkChecker returns the checker for http, tcp and network-policy.
// Use AddNetworkCheckers to register it. WithProbeControlNamespace names the
// namespace of the neutral DNS control probes; without it a DNS failure of a
// source can never be attributed and is ERROR.
func NewNetworkChecker(reader ObjectReader, runner ProbeRunner, opts ...Option) *NetworkChecker {
	return &NetworkChecker{observer: observer{reader: reader, set: newSettings(opts)}, runner: runner}
}

// AddNetworkCheckers registers c for the http, tcp and network-policy kinds.
func AddNetworkCheckers(checkers map[string]Checker, c *NetworkChecker) {
	checkers[model.CriterionHTTP] = c
	checkers[model.CriterionTCP] = c
	checkers[model.CriterionNetworkPolicy] = c
}

// Mutates implements MutatingChecker: probe pods are created and removed.
func (c *NetworkChecker) Mutates(model.Criterion) bool { return true }

// ---------------------------------------------------------------- arguments

type netSource struct {
	Namespace string            `json:"namespace"`
	Labels    map[string]string `json:"labels"`
}

type netHTTPArgs struct {
	Host       string `json:"host"`
	Path       string `json:"path"`
	Status     *int   `json:"status"`
	BodyMarker string `json:"body_marker"`
}

type httpCriterionArgs struct {
	Source netSource `json:"source"`
	Port   *int      `json:"port"`
	netHTTPArgs
}

type tcpCriterionArgs struct {
	Source netSource `json:"source"`
	Port   *int      `json:"port"`
}

type expectationArgs struct {
	Name   string       `json:"name"`
	Expect string       `json:"expect"`
	Source netSource    `json:"source"`
	Port   *int         `json:"port"`
	HTTP   *netHTTPArgs `json:"http"`
}

type policyCriterionArgs struct {
	Expectations []expectationArgs `json:"expectations"`
}

// httpSpec is a validated HTTP expectation.
type httpSpec struct {
	host, path, marker string
	status             int
}

// netExpect is one validated expectation.
type netExpect struct {
	name   string
	allow  bool
	source netSource
	port   int // 0: the Ingress default
	http   *httpSpec
}

type netPlan struct {
	expects []netExpect
	ingress bool
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidCriterion, fmt.Sprintf(format, a...))
}

func isService(ref model.ObjectRef) bool { return ref.APIVersion == "v1" && ref.Kind == "Service" }

func isIngress(ref model.ObjectRef) bool {
	return ref.APIVersion == "networking.k8s.io/v1" && ref.Kind == "Ingress"
}

func checkSource(s netSource) error {
	if s.Namespace != "" {
		if errs := validation.IsDNS1123Label(s.Namespace); len(errs) > 0 {
			return invalid("source.namespace %q: %s", s.Namespace, errs[0])
		}
	}
	for k, v := range s.Labels {
		if strings.HasPrefix(k, reservedLabelNS) {
			return invalid("source label %q is reserved for JunKloud", k)
		}
		if errs := validation.IsQualifiedName(k); len(errs) > 0 {
			return invalid("source label key %q: %s", k, errs[0])
		}
		if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
			return invalid("source label %q value %q: %s", k, v, errs[0])
		}
	}
	return nil
}

func checkPort(p *int, defaultPort int) (int, error) {
	if p == nil {
		if defaultPort == 0 {
			return 0, invalid("args.port is required")
		}
		return defaultPort, nil
	}
	if *p < 1 || *p > 65535 {
		return 0, invalid("args.port %d is not a port number", *p)
	}
	return *p, nil
}

func cleanHeaderValue(what, v string) error {
	if strings.ContainsAny(v, "\r\n\x00 \t") {
		return invalid("%s %q must not contain whitespace or control characters", what, v)
	}
	return nil
}

func buildHTTP(a netHTTPArgs) (*httpSpec, error) {
	h := &httpSpec{host: a.Host, path: a.Path, marker: a.BodyMarker, status: 200}
	if err := cleanHeaderValue("host", h.host); err != nil {
		return nil, err
	}
	if h.path == "" {
		h.path = "/"
	}
	if !strings.HasPrefix(h.path, "/") {
		return nil, invalid("path %q must start with /", h.path)
	}
	if err := cleanHeaderValue("path", h.path); err != nil {
		return nil, err
	}
	if a.Status != nil {
		if *a.Status < 100 || *a.Status > 599 {
			return nil, invalid("status %d is not an HTTP status", *a.Status)
		}
		h.status = *a.Status
	}
	if h.marker == "" {
		return nil, invalid("body_marker is required: an HTTP check must identify the application, not only a status code")
	}
	if len(h.marker) > maxMarkerLen || strings.ContainsAny(h.marker, "\r\n\x00") {
		return nil, invalid("body_marker must be a single line of at most %d characters", maxMarkerLen)
	}
	return h, nil
}

// plan validates a criterion's definition without touching the cluster.
func (c *NetworkChecker) plan(crit model.Criterion) (netPlan, error) {
	var p netPlan
	if err := checkRef(crit.Target); err != nil {
		return p, err
	}
	svc, ing := isService(crit.Target), isIngress(crit.Target)
	defaultPort := 0
	switch crit.Kind {
	case model.CriterionHTTP:
		if !svc && !ing {
			return p, invalid("http targets a Service or an Ingress, not %s %s", crit.Target.APIVersion, crit.Target.Kind)
		}
		if ing {
			defaultPort = 80
		}
	case model.CriterionTCP, model.CriterionNetworkPolicy:
		if !svc {
			return p, invalid("%s targets a Service, not %s %s", crit.Kind, crit.Target.APIVersion, crit.Target.Kind)
		}
	default:
		return p, invalid("network checker cannot grade kind %q", crit.Kind)
	}
	p.ingress = ing

	switch crit.Kind {
	case model.CriterionHTTP:
		var a httpCriterionArgs
		if err := decodeArgs(crit.Args, &a); err != nil {
			return p, err
		}
		port, err := checkPort(a.Port, defaultPort)
		if err != nil {
			return p, err
		}
		h, err := buildHTTP(a.netHTTPArgs)
		if err != nil {
			return p, err
		}
		if err := checkSource(a.Source); err != nil {
			return p, err
		}
		p.expects = []netExpect{{name: "http", allow: true, source: a.Source, port: port, http: h}}
	case model.CriterionTCP:
		var a tcpCriterionArgs
		if err := decodeArgs(crit.Args, &a); err != nil {
			return p, err
		}
		port, err := checkPort(a.Port, 0)
		if err != nil {
			return p, err
		}
		if err := checkSource(a.Source); err != nil {
			return p, err
		}
		p.expects = []netExpect{{name: "tcp", allow: true, source: a.Source, port: port}}
	default:
		var a policyCriterionArgs
		if err := decodeArgs(crit.Args, &a); err != nil {
			return p, err
		}
		if err := c.planPolicy(&p, a); err != nil {
			return p, err
		}
	}
	return p, nil
}

func (c *NetworkChecker) planPolicy(p *netPlan, a policyCriterionArgs) error {
	if len(a.Expectations) == 0 || len(a.Expectations) > maxExpectations {
		return invalid("args.expectations needs between 1 and %d entries", maxExpectations)
	}
	names := map[string]bool{}
	allowPorts, denyPorts := map[int]bool{}, []int{}
	for i, e := range a.Expectations {
		if e.Expect != "allow" && e.Expect != "deny" {
			return invalid("expectations[%d].expect must be allow or deny", i)
		}
		ne := netExpect{name: e.Name, allow: e.Expect == "allow", source: e.Source}
		if ne.name == "" {
			ne.name = fmt.Sprintf("%s-%d", e.Expect, i+1)
		}
		if names[ne.name] {
			return invalid("expectations[%d]: duplicate name %q", i, ne.name)
		}
		names[ne.name] = true
		port, err := checkPort(e.Port, 0)
		if err != nil {
			return invalid("expectations[%d]: %v", i, strings.TrimPrefix(err.Error(), ErrInvalidCriterion.Error()+": "))
		}
		ne.port = port
		if err := checkSource(e.Source); err != nil {
			return err
		}
		if e.HTTP != nil {
			if !ne.allow {
				return invalid("expectations[%d]: a deny expectation is checked at connection level and takes no http", i)
			}
			if ne.http, err = buildHTTP(*e.HTTP); err != nil {
				return err
			}
		}
		if ne.allow {
			allowPorts[port] = true
		} else {
			denyPorts = append(denyPorts, port)
		}
		p.expects = append(p.expects, ne)
	}
	if len(allowPorts) == 0 || len(denyPorts) == 0 {
		return invalid("a network-policy criterion needs at least one allow and one deny expectation")
	}
	for _, port := range denyPorts {
		if !allowPorts[port] {
			return invalid("deny expectation on port %d has no allow expectation on the same port to prove the server is healthy", port)
		}
	}
	return nil
}

// ValidateCriterion implements ArgsValidator.
func (c *NetworkChecker) ValidateCriterion(crit model.Criterion) error {
	_, err := c.plan(crit)
	return err
}

// ------------------------------------------------------------------- checking

type outcomeState int

const (
	stateHolds outcomeState = iota
	stateFails
	stateUnknown
)

// netOutcome is the judgement of one expectation.
type netOutcome struct {
	exp    netExpect
	state  outcomeState
	text   string
	result ProbeResult
	ran    bool
	err    error
}

func (o netOutcome) evidence() map[string]any {
	m := map[string]any{"expectation": o.exp.name, "expect": map[bool]string{true: "allow", false: "deny"}[o.exp.allow],
		"port": o.exp.port, "state": [...]string{"holds", "fails", "unknown"}[o.state]}
	if o.ran {
		m["connected"] = o.result.Connected
		if o.result.HTTPStatus != 0 {
			m["http_status"] = o.result.HTTPStatus
		}
		if o.result.FailureClass != "" {
			m["failure_class"] = o.result.FailureClass
		}
	}
	if o.text != "" {
		m["note"] = o.text
	}
	return m
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
	start := c.set.clock.Now()
	dst, obj, fail, err := c.destination(ctx, t, crit, ref, plan)
	if err != nil {
		return model.CheckResult{}, err
	}
	if fail != "" {
		res := model.CheckResult{Status: model.StatusFail, Reason: fail, Evidence: baseEvidence(c.set.clock.Now(), start, obj)}
		res.Evidence.Expected = describePlan(plan, ref)
		return res, nil
	}

	outcomes := c.probeAll(ctx, t, plan, dst)
	if err := ctx.Err(); err != nil {
		return model.CheckResult{}, err
	}
	c.judge(ctx, t, plan, dst, outcomes)
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
	res.Evidence.Actual = map[string]any{"destination": dst.dial, "ready_endpoints": dst.ready, "probes": probes}
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

// destination is what the probes dial.
type destination struct {
	dial     string // name or IP
	httpHost string // default Host header
	ready    int    // ready endpoints (Service)
}

// destination reads the target. fail is a learner-visible reason for a FAIL
// that needs no probe.
func (c *NetworkChecker) destination(ctx context.Context, t Target, crit model.Criterion, ref model.ObjectRef, p netPlan) (destination, object, string, error) {
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
	d := destination{dial: fmt.Sprintf("%s.%s.%s", ref.Name, ref.Namespace, clusterDomain)}
	d.httpHost = d.dial
	have := map[int]bool{}
	for _, raw := range listAt(obj, "spec", "ports") {
		pm, _ := raw.(map[string]any)
		if n, ok := ratOf(pm["port"]); ok && n.IsInt() {
			have[int(n.Num().Int64())] = true
		}
	}
	for _, e := range p.expects {
		if !have[e.port] {
			return d, obj, fmt.Sprintf("Service %s/%s does not expose port %d", ref.Namespace, ref.Name, e.port), nil
		}
	}
	slices, err := c.listSlices(ctx, ref)
	if err != nil {
		return d, obj, "", err
	}
	for _, s := range slices {
		if stringAt(s, "metadata", "labels", serviceNameLabel) != ref.Name {
			continue
		}
		for _, raw := range listAt(s, "endpoints") {
			ep, _ := raw.(map[string]any)
			if r, set := mapAt(ep, "conditions")["ready"]; !set || r != false {
				d.ready++
			}
		}
	}
	if d.ready == 0 {
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
		ref: ref, timeout: timeoutFor(crit, c.set) / 2, poll: true, waitForCreate: false,
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

func (c *NetworkChecker) request(t Target, e netExpect, d destination) ProbeRequest {
	ns := e.source.Namespace
	if ns == "" {
		ns = t.Problem.Namespace
		if ns == "" {
			ns = t.Problem.Definition.Namespace
		}
	}
	req := ProbeRequest{
		Protocol: ProbeTCP, Namespace: ns, Labels: e.source.Labels, Host: d.dial, Port: e.port,
		Attempts: ProbeAttempts, Timeout: ProbeAttemptTimeout,
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

// probeAll runs every expectation's probe, a few at a time.
func (c *NetworkChecker) probeAll(ctx context.Context, t Target, p netPlan, d destination) []netOutcome {
	outs := make([]netOutcome, len(p.expects))
	sem := make(chan struct{}, maxConcurrentProb)
	var wg sync.WaitGroup
	for i, e := range p.expects {
		outs[i].exp = e
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					outs[i].err = fmt.Errorf("%w: probe panic: %v", ErrProbeFailed, r)
				}
			}()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				outs[i].err = ctx.Err()
				return
			}
			outs[i].result, outs[i].err = c.runner.Probe(ctx, c.request(t, e, d))
			outs[i].ran = outs[i].err == nil
		}()
	}
	wg.Wait()
	return outs
}

// judge decides every outcome: allow expectations first, then deny
// expectations against the allow controls and DNS evidence.
func (c *NetworkChecker) judge(ctx context.Context, t Target, p netPlan, d destination, outs []netOutcome) {
	neutral := &neutralDNS{c: c, ctx: ctx, t: t, d: d}
	for i := range outs {
		o := &outs[i]
		switch {
		case o.err != nil:
			o.state, o.text = stateUnknown, o.err.Error()
		case o.result.FailureClass == FailureProbeError:
			o.state, o.text = stateUnknown, "the probe could not run or report"
			o.err = ErrProbeFailed
		case o.result.FailureClass == FailureDNS:
			c.judgeDNS(o, outs, neutral, d)
		case o.exp.allow:
			judgeAllow(o)
		}
	}
	for i := range outs {
		o := &outs[i]
		if o.exp.allow || o.err != nil || o.state != stateHolds || !o.ran || o.result.FailureClass == FailureDNS {
			continue
		}
		judgeDeny(o, outs)
	}
}

func judgeAllow(o *netOutcome) {
	r, h := o.result, o.exp.http
	switch {
	case !r.Connected:
		o.state, o.text = stateFails, fmt.Sprintf("the allowed path did not connect (%s)", classOrUnknown(r.FailureClass))
	case h == nil:
	case r.HTTPStatus != h.status:
		o.state = stateFails
		if r.HTTPStatus == 0 {
			o.text = "connected, but the server sent no HTTP answer (http-mismatch)"
		} else {
			o.text = fmt.Sprintf("answered HTTP %d, expected %d (http-mismatch)", r.HTTPStatus, h.status)
		}
	case r.BodyMarker != h.marker:
		o.state, o.text = stateFails, fmt.Sprintf("answered HTTP %d but the body lacks the expected application marker: a different backend answered (http-mismatch)", r.HTTPStatus)
	}
}

func classOrUnknown(class string) string {
	if class == "" {
		return "no reason reported"
	}
	return class
}

func judgeDeny(o *netOutcome, all []netOutcome) {
	r := o.result
	switch {
	case r.Connected:
		o.state, o.text = stateFails, "connected although this source must be denied"
		return
	case r.FailureClass != FailureTimeout && r.FailureClass != FailureRefused:
		o.state, o.text = stateUnknown, fmt.Sprintf("the connection failed (%s), which does not show a block", classOrUnknown(r.FailureClass))
		return
	}
	// The block counts only beside a healthy server: an allowed source
	// reached the same port in this run.
	var controls []*netOutcome
	for i := range all {
		if a := &all[i]; a.exp.allow && a.exp.port == o.exp.port {
			controls = append(controls, a)
		}
	}
	for _, a := range controls {
		if a.state == stateHolds {
			return // healthy: the block is real
		}
	}
	for _, a := range controls {
		if a.state == stateUnknown {
			o.state, o.text = stateUnknown, "the allowed control could not be decided, so the block proves nothing yet"
			return
		}
	}
	o.state, o.text = stateFails, fmt.Sprintf("the %s was observed, but no allowed source reached port %d in this run, so the destination is not shown to be healthy and the block proves nothing", r.FailureClass, o.exp.port)
}

// judgeDNS decides an expectation whose probe could not resolve the name.
// A DNS failure is never an allow and never a deny; whose fault it is
// depends on whether anyone else can resolve the name.
func (c *NetworkChecker) judgeDNS(o *netOutcome, all []netOutcome, n *neutralDNS, d destination) {
	for i := range all {
		a := &all[i]
		if a != o && a.ran && a.result.FailureClass != FailureDNS && a.result.FailureClass != FailureProbeError {
			o.state, o.text = stateFails, fmt.Sprintf("this source could not resolve %s although another source could (egress or DNS settings of the source)", d.dial)
			return
		}
	}
	switch ok, err := n.resolves(); {
	case err != nil:
		o.state, o.text = stateUnknown, "cluster DNS could not be verified: "+err.Error()
	case ok:
		o.state, o.text = stateFails, fmt.Sprintf("this source could not resolve %s although a neutral pod can", d.dial)
	default:
		o.state, o.text = stateUnknown, fmt.Sprintf("%s does not resolve from any pod; cluster DNS looks unhealthy", d.dial)
	}
}

// neutralDNS resolves the destination from a pod without any source labels,
// once, only when a source failed to resolve.
type neutralDNS struct {
	c    *NetworkChecker
	ctx  context.Context
	t    Target
	d    destination
	once sync.Once
	ok   bool
	err  error
}

func (n *neutralDNS) resolves() (bool, error) {
	n.once.Do(func() {
		ns := n.c.set.controlNamespace
		if ns == "" {
			n.err = errors.New("no control namespace is configured")
			return
		}
		res, err := n.c.runner.Probe(n.ctx, ProbeRequest{
			Protocol: ProbeDNS, Namespace: ns, Host: n.d.dial, Attempts: ProbeAttempts, Timeout: ProbeAttemptTimeout,
			EnvironmentID: n.t.Identity.ID, ProblemID: n.t.Problem.Definition.ID,
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

// WithProbeControlNamespace names the namespace where neutral control probes
// (a pod without any source labels, only resolving the destination name) run.
// Composition passes the platform namespace. Without it a source's DNS
// failure cannot be attributed and its criterion is ERROR.
func WithProbeControlNamespace(ns string) Option {
	return func(s *settings) { s.controlNamespace = ns }
}

// WithProbeImage sets the probe pod image (NewPodProbeRunner). It must be a
// pinned image the environment has preloaded: probe pods never pull.
func WithProbeImage(ref string) Option { return func(s *settings) { s.probeImage = ref } }
