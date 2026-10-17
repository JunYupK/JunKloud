package grader

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/JunYupK/JunKloud/internal/model"
)

// Probe protocols.
const (
	// ProbeHTTP sends one HTTP/1.1 GET (Connection: close) and reports the status and whether
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
	clusterDomain    = "svc.cluster.local"
	maxExpectations  = 8
	maxMarkerLen     = 256
	reservedLabelNS  = "junkloud.io/"
	ingressClassOwn  = "cloud-provider-kind"
	endpointSliceAPI = "discovery.k8s.io/v1"
	serviceNameLabel = "kubernetes.io/service-name"
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
	// ResolveName, when set, is a name the probe must also resolve from its
	// pod before dialing Host (an IP): the DNS evidence for probes that dial a
	// pod address directly. A failure is reported as FailureDNS.
	ResolveName string
	// Deadline (checker clock) is the end of the criterion's time budget; the
	// runner never waits for the pod beyond it. Zero means only the runner's
	// own bound.
	Deadline time.Time
	// Ownership labels of the probe pod.
	EnvironmentID string
	ProblemID     string
}

// Time budget of a network criterion. Every probe of a criterion must end
// before start+timeout (ProbeRequest.Deadline), so the criterion's declared
// timeout has to cover the work: probeWave is the planning figure for one
// wave of concurrent probe pods (pod start plus three 3 s attempts with their
// pauses), the neutral DNS control may need a second wave, and an Ingress
// address is awaited up to ingressWait first.
const (
	probeWave    = 20 * time.Second
	networkSlack = 5 * time.Second
	ingressWait  = 15 * time.Second
)

// MinNetworkTimeout is the smallest criterion timeout a network criterion
// may declare: a probe wave, a neutral DNS control wave and slack (45 s),
// plus the wait for an Ingress address (60 s). Network criteria therefore
// always declare an explicit timeout; ValidateCriterion rejects less.
func MinNetworkTimeout(ingress bool) time.Duration {
	d := 2*probeWave + networkSlack
	if ingress {
		d += ingressWait
	}
	return d
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
// destination is healthy. kube-proxy picks a backend per connection, so a
// deny expectation is never judged through the Service name: it is checked
// against EVERY ready pod (EndpointSlice address and target port, which is
// where NetworkPolicy applies). It passes only when, for every pod,
//
//  1. an allowed source connected to that same pod and port in this run (the
//     pod is alive and listening; a hung or dead pod blocks everybody),
//  2. the denied source resolved the destination name (a DNS failure is never
//     a deny), and
//  3. the denied source's connection TIMED OUT (a drop). A refusal is no
//     proof: a pod where nothing listens refuses by itself.
//
// An allow expectation is probed through the Service name (routing and DNS).
// Failures that are neither refusal nor timeout (network-error) prove nothing
// about whose fault they are and are ERROR on both paths.
//
// Time: every probe must end before the criterion's timeout
// (ProbeRequest.Deadline); ValidateCriterion rejects timeouts below
// MinNetworkTimeout (45 s, 60 s for an Ingress), so network criteria always
// declare one. The checker and the engine must share the default timeout.
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
	eff := crit.Timeout
	if eff <= 0 {
		eff = c.set.defaultTimeout
	}
	if min := MinNetworkTimeout(p.ingress); eff < min {
		return p, invalid("timeout %s is below the %s minimum of a %s criterion (set the criterion's timeout; probes must finish within it)", eff, min, crit.Kind)
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
