package grader_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

const (
	netNS     = "jk-net"
	netMarker = "APP-WEB"
	ctlNS     = "junkloud-platform"
)

var (
	netSvc = refOf("v1", "Service", netNS, "web")
	netIng = refOf("networking.k8s.io/v1", "Ingress", netNS, "web")

	// Results a probe pod can report.
	rHTTP     = grader.ProbeResult{Connected: true, HTTPStatus: 200, BodyMarker: netMarker}
	rTCP      = grader.ProbeResult{Connected: true}
	rTimeout  = grader.ProbeResult{FailureClass: grader.FailureTimeout}
	rRefused  = grader.ProbeResult{FailureClass: grader.FailureRefused}
	rDNS      = grader.ProbeResult{FailureClass: grader.FailureDNS}
	rNetError = grader.ProbeResult{FailureClass: grader.FailureNetwork}
)

// fakeProbes is a ProbeRunner driven by a function and recording its calls.
type fakeProbes struct {
	mu       sync.Mutex
	reqs     []grader.ProbeRequest
	fn       func(grader.ProbeRequest) (grader.ProbeResult, error)
	inflight map[string]bool
	maxHosts int
}

func (f *fakeProbes) Probe(_ context.Context, req grader.ProbeRequest) (grader.ProbeResult, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	if f.inflight == nil {
		f.inflight = map[string]bool{}
	}
	f.inflight[req.Host] = true
	f.maxHosts = max(f.maxHosts, len(f.inflight))
	f.mu.Unlock()
	time.Sleep(time.Millisecond) // let a second criterion overlap if the engine allowed it
	defer func() {
		f.mu.Lock()
		delete(f.inflight, req.Host)
		f.mu.Unlock()
	}()
	return f.fn(req)
}

func (f *fakeProbes) calls() []grader.ProbeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]grader.ProbeRequest(nil), f.reqs...)
}

// role is the source identity a request was made from.
func role(req grader.ProbeRequest) string { return req.Labels["role"] }

// world answers like a cluster: frontend may connect, others are denied (or
// not, depending on the knobs).
type world struct {
	frontend grader.ProbeResult // what the allowed source sees
	other    grader.ProbeResult // what the denied source sees
	neutral  grader.ProbeResult // what the neutral control sees (DNS)
}

func healthyWorld() world { return world{frontend: rHTTP, other: rTimeout, neutral: rTCP} }

func (w world) probes() *fakeProbes {
	return &fakeProbes{fn: func(req grader.ProbeRequest) (grader.ProbeResult, error) {
		switch {
		case req.Protocol == grader.ProbeDNS:
			return w.neutral, nil
		case role(req) == "frontend":
			return w.frontend, nil
		default:
			return w.other, nil
		}
	}}
}

func svcObject(t testing.TB) map[string]any {
	return obj(t, `{"apiVersion":"v1","kind":"Service","metadata":{"name":"web","namespace":"jk-net","uid":"svc-uid"},
		"spec":{"selector":{"app":"web"},"ports":[{"port":80,"targetPort":8080}]}}`)
}

func sliceObject(t testing.TB, ready string) map[string]any {
	return obj(t, fmt.Sprintf(`{"apiVersion":"discovery.k8s.io/v1","kind":"EndpointSlice",
		"metadata":{"name":"web-abc","namespace":"jk-net","labels":{"kubernetes.io/service-name":"web"}},
		"ports":[{"port":8080,"protocol":"TCP"}],
		"endpoints":[{"addresses":["10.0.0.5"],"conditions":{"ready":%s},"targetRef":{"kind":"Pod","name":"web-1"}}]}`, ready))
}

// sliceWith serves the given ready endpoints (address, pod name) on port 8080.
func sliceWith(t testing.TB, eps ...[2]string) map[string]any {
	var list []string
	for _, e := range eps {
		list = append(list, fmt.Sprintf(`{"addresses":["%s"],"conditions":{"ready":true},"targetRef":{"kind":"Pod","name":"%s"}}`, e[0], e[1]))
	}
	return obj(t, `{"metadata":{"name":"web-abc","namespace":"jk-net","labels":{"kubernetes.io/service-name":"web"}},
		"ports":[{"port":8080,"protocol":"TCP"}],"endpoints":[`+strings.Join(list, ",")+`]}`)
}

// netCluster has Service web with one ready endpoint.
func netCluster(t testing.TB) *fakeReader {
	return newReader().put(netSvc, svcObject(t)).putList("EndpointSlice", netNS, sliceObject(t, "true"))
}

func netEngine(r *fakeReader, p grader.ProbeRunner, opts ...grader.Option) *grader.Engine {
	// Network criteria need a generous budget (MinNetworkTimeout).
	opts = append([]grader.Option{grader.WithClock(newClock()), grader.WithDefaultTimeout(60 * time.Second)}, opts...)
	checkers := grader.DefaultCheckers(r, opts...)
	grader.AddNetworkCheckers(checkers, grader.NewNetworkChecker(r, p, opts...))
	return grader.NewEngine(checkers, opts...)
}

func src(role string) map[string]any {
	return map[string]any{"labels": map[string]any{"role": role}}
}

// policyCriterion: the frontend must reach web and everybody else must not.
func policyCriterion() model.Criterion {
	return crit("np", model.CriterionNetworkPolicy, 5, netSvc, map[string]any{"expectations": []any{
		map[string]any{"name": "frontend-allowed", "expect": "allow", "port": 80, "source": src("frontend"),
			"http": map[string]any{"body_marker": netMarker}},
		map[string]any{"name": "other-denied", "expect": "deny", "port": 80, "source": src("other")},
	}})
}

func httpCriterion() model.Criterion {
	return crit("web", model.CriterionHTTP, 3, netSvc, map[string]any{
		"source": src("frontend"), "port": 80, "path": "/healthz", "host": "shop.example.com", "status": 200, "body_marker": netMarker})
}

func grade1(t *testing.T, e *grader.Engine, c model.Criterion) model.CheckResult {
	t.Helper()
	ev, err := e.Evaluate(context.Background(), target(c))
	testutil.NoError(t, err)
	return resultFor(t, ev, c.ID)
}

func TestAllowAndDeny(t *testing.T) {
	cases := []struct {
		name   string
		w      world
		status string
		reason string // substring
	}{
		{"correct policy: allowed connects, denied times out", healthyWorld(), "PASS", ""},
		{"a refusal is not proof of a block", world{rHTTP, rRefused, rTCP}, "ERROR", ""},
		{"no policy: the denied source connects", world{rHTTP, rHTTP, rTCP}, "FAIL", "other-denied"},
		{"denied source reaches the port without HTTP", world{rHTTP, rTCP, rTCP}, "FAIL", "connected"},
		{"everything blocked", world{rTimeout, rTimeout, rTCP}, "FAIL", "frontend-allowed"},
		{"allowed source refused", world{rRefused, rTimeout, rTCP}, "FAIL", "refused"},
		{"deny failure of an unknown kind is not proof", world{rHTTP, rNetError, rTCP}, "ERROR", ""},
		{"unclassified failure on the allow path proves nothing", world{rNetError, rTimeout, rTCP}, "ERROR", "unclassified"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.w.probes()
			res := grade1(t, netEngine(netCluster(t), p), policyCriterion())
			testutil.Equal(t, res.Status, tc.status)
			if !strings.Contains(res.Reason, tc.reason) {
				t.Fatalf("reason %q lacks %q", res.Reason, tc.reason)
			}
			if tc.status == "PASS" {
				testutil.Equal(t, res.Points, 5)
				testutil.Equal(t, res.Evidence.UID, "svc-uid")
			} else {
				testutil.Equal(t, res.Points, 0)
			}
		})
	}
}

func TestAllowAndDenyRequests(t *testing.T) {
	clk := newClock()
	start := clk.Now()
	p := healthyWorld().probes()
	res := grade1(t, netEngine(netCluster(t), p, grader.WithClock(clk)), policyCriterion())
	testutil.Equal(t, res.Status, "PASS")
	// One routing probe through the Service name, plus the deny check and its
	// allow control against the single ready pod address.
	reqs := p.calls()
	testutil.Equal(t, len(reqs), 3)
	var svcAllow, podDeny, podControl grader.ProbeRequest
	for _, r := range reqs {
		switch {
		case r.Host != "10.0.0.5":
			svcAllow = r
		case role(r) == "other":
			podDeny = r
		default:
			podControl = r
		}
	}
	want := "web.jk-net.svc.cluster.local"
	testutil.Equal(t, svcAllow.Protocol, grader.ProbeHTTP)
	testutil.Equal(t, svcAllow.Namespace, "jk-p1") // defaults to the problem namespace, not the destination's
	testutil.Equal(t, svcAllow.Host, want)
	testutil.Equal(t, svcAllow.HTTPHost, want)
	testutil.Equal(t, svcAllow.Path, "/")
	testutil.Equal(t, svcAllow.Port, 80)
	testutil.Equal(t, svcAllow.ExpectStatus, 200)
	testutil.Equal(t, svcAllow.BodyMarker, netMarker)
	testutil.Equal(t, svcAllow.Attempts, 3)
	testutil.Equal(t, svcAllow.Timeout, 3*time.Second)
	testutil.Equal(t, svcAllow.EnvironmentID, "0123456789ab")
	testutil.Equal(t, svcAllow.ProblemID, "p1")
	// NetworkPolicy is evaluated on the pod address and target port.
	testutil.Equal(t, podDeny.Protocol, grader.ProbeTCP)
	testutil.Equal(t, podDeny.Host, "10.0.0.5")
	testutil.Equal(t, podDeny.Port, 8080)
	testutil.Equal(t, podDeny.ResolveName, want) // DNS is still proven from the denied source
	testutil.Equal(t, podDeny.Namespace, "jk-p1")
	testutil.Equal(t, podDeny.Attempts, 3)
	testutil.Equal(t, podDeny.Timeout, 3*time.Second)
	testutil.Equal(t, podControl.Protocol, grader.ProbeHTTP)
	testutil.Equal(t, podControl.Port, 8080)
	testutil.Equal(t, podControl.HTTPHost, want)
	testutil.Equal(t, podControl.BodyMarker, netMarker)
	for _, r := range reqs { // every probe shares the criterion's budget
		testutil.Equal(t, r.Deadline, start.Add(60*time.Second))
	}
}

func TestAllowAndDenyFromAnotherNamespace(t *testing.T) {
	c := crit("np", model.CriterionNetworkPolicy, 5, netSvc, map[string]any{"expectations": []any{
		map[string]any{"expect": "allow", "port": 80, "source": src("frontend"), "http": map[string]any{"body_marker": netMarker}},
		map[string]any{"expect": "deny", "port": 80, "source": map[string]any{"namespace": "jk-other", "labels": map[string]any{"role": "other"}}},
	}})
	p := healthyWorld().probes()
	res := grade1(t, netEngine(netCluster(t), p), c)
	testutil.Equal(t, res.Status, "PASS")
	for _, r := range p.calls() {
		if role(r) == "other" {
			testutil.Equal(t, r.Namespace, "jk-other")
		}
	}
}

func TestDeadServerNotPolicyPass(t *testing.T) {
	t.Run("no ready endpoints: fail without probing", func(t *testing.T) {
		for name, r := range map[string]*fakeReader{
			"no slices":     newReader().put(netSvc, svcObject(t)),
			"not ready":     newReader().put(netSvc, svcObject(t)).putList("EndpointSlice", netNS, sliceObject(t, "false")),
			"foreign slice": newReader().put(netSvc, svcObject(t)).putList("EndpointSlice", netNS, obj(t, `{"metadata":{"name":"x","labels":{"kubernetes.io/service-name":"other"}},"endpoints":[{"conditions":{"ready":true}}]}`)),
		} {
			p := healthyWorld().probes()
			res := grade1(t, netEngine(r, p), policyCriterion())
			testutil.Equal(t, res.Status, "FAIL")
			if !strings.Contains(res.Reason, "no ready endpoints") {
				t.Fatalf("%s: reason %q", name, res.Reason)
			}
			testutil.Equal(t, len(p.calls()), 0)
		}
	})
	t.Run("server hangs behind a ready endpoint: the block proves nothing", func(t *testing.T) {
		p := world{rTimeout, rTimeout, rTCP}.probes()
		res := grade1(t, netEngine(netCluster(t), p), policyCriterion())
		testutil.Equal(t, res.Status == "PASS", false)
		testutil.Equal(t, res.Status, "FAIL")
		if !strings.Contains(res.Reason, "healthy") {
			t.Fatalf("the reason must say the deny has no healthy-server control: %q", res.Reason)
		}
	})
	t.Run("server refuses everybody", func(t *testing.T) {
		p := world{rRefused, rRefused, rTCP}.probes()
		res := grade1(t, netEngine(netCluster(t), p), policyCriterion())
		testutil.Equal(t, res.Status, "FAIL")
	})
	t.Run("a healthy port does not vouch for a hung one", func(t *testing.T) {
		svc := obj(t, `{"apiVersion":"v1","kind":"Service","metadata":{"name":"web","namespace":"jk-net","uid":"svc-uid"},
			"spec":{"ports":[{"name":"web","port":80,"targetPort":8080},{"name":"admin","port":8081,"targetPort":9090}]}}`)
		slice := obj(t, `{"metadata":{"name":"web-abc","labels":{"kubernetes.io/service-name":"web"}},
			"ports":[{"name":"web","port":8080},{"name":"admin","port":9090}],
			"endpoints":[{"addresses":["10.0.0.5"],"conditions":{"ready":true}}]}`)
		r := newReader().put(netSvc, svc).putList("EndpointSlice", netNS, slice)
		p := &fakeProbes{fn: func(req grader.ProbeRequest) (grader.ProbeResult, error) {
			if req.Port == 80 || req.Port == 8080 {
				if role(req) == "frontend" {
					return rHTTP, nil
				}
				return rTimeout, nil
			}
			return rTimeout, nil // the admin port is hung for everybody
		}}
		c := crit("np", model.CriterionNetworkPolicy, 5, netSvc, map[string]any{"expectations": []any{
			map[string]any{"name": "web-allowed", "expect": "allow", "port": 80, "source": src("frontend"), "http": map[string]any{"body_marker": netMarker}},
			map[string]any{"name": "admin-allowed", "expect": "allow", "port": 8081, "source": src("frontend")},
			map[string]any{"name": "admin-denied", "expect": "deny", "port": 8081, "source": src("other")},
		}})
		res := grade1(t, netEngine(r, p), c)
		testutil.Equal(t, res.Status, "FAIL")
		if !strings.Contains(res.Reason, "admin-denied") || !strings.Contains(res.Reason, "healthy") {
			t.Fatalf("reason %q", res.Reason)
		}
		var sawAdmin bool
		for _, q := range p.calls() {
			if q.Port == 9090 { // the admin target port, not the Service port
				sawAdmin = true
			}
		}
		testutil.Equal(t, sawAdmin, true)
	})
	t.Run("service is missing", func(t *testing.T) {
		p := healthyWorld().probes()
		res := grade1(t, netEngine(newReader(), p), policyCriterion())
		testutil.Equal(t, res.Status, "FAIL")
		if !strings.Contains(res.Reason, "prerequisite not satisfied") {
			t.Fatalf("reason %q", res.Reason)
		}
		testutil.Equal(t, len(p.calls()), 0)
	})
	t.Run("service lacks the port", func(t *testing.T) {
		c := crit("np", model.CriterionNetworkPolicy, 5, netSvc, map[string]any{"expectations": []any{
			map[string]any{"expect": "allow", "port": 8080, "source": src("frontend")},
			map[string]any{"expect": "deny", "port": 8080, "source": src("other")},
		}})
		p := healthyWorld().probes()
		res := grade1(t, netEngine(netCluster(t), p), c)
		testutil.Equal(t, res.Status, "FAIL")
		testutil.Equal(t, len(p.calls()), 0)
	})
	t.Run("API trouble is an error, not a fail", func(t *testing.T) {
		r := newReader().put(netSvc, errors.New("connection refused"))
		res := grade1(t, netEngine(r, healthyWorld().probes()), policyCriterion())
		testutil.Equal(t, res.Status, "ERROR")
		r = newReader().put(netSvc, svcObject(t))
		r.listErr = errors.New("etcd timeout")
		res = grade1(t, netEngine(r, healthyWorld().probes()), policyCriterion())
		testutil.Equal(t, res.Status, "ERROR")
	})
}

func TestDNSFailureNotDenyPass(t *testing.T) {
	ctl := grader.WithProbeControlNamespace(ctlNS)
	t.Run("denied source cannot resolve while a neutral pod can: fail", func(t *testing.T) {
		p := world{rHTTP, rDNS, rTCP}.probes()
		res := grade1(t, netEngine(netCluster(t), p, ctl), policyCriterion())
		testutil.Equal(t, res.Status, "FAIL")
		if !strings.Contains(res.Reason, "other-denied") || !strings.Contains(res.Reason, "resolve") {
			t.Fatalf("reason %q", res.Reason)
		}
		// The allowed source resolved the name, so no neutral pod was needed.
		for _, r := range p.calls() {
			testutil.Equal(t, r.Protocol == grader.ProbeDNS, false)
		}
	})
	t.Run("nobody can resolve and the neutral pod cannot either: error", func(t *testing.T) {
		p := world{rDNS, rDNS, rDNS}.probes()
		res := grade1(t, netEngine(netCluster(t), p, ctl), policyCriterion())
		testutil.Equal(t, res.Status, "ERROR")
		var neutral []grader.ProbeRequest
		for _, r := range p.calls() {
			if r.Protocol == grader.ProbeDNS {
				neutral = append(neutral, r)
			}
		}
		testutil.Equal(t, len(neutral), 1)
		testutil.Equal(t, neutral[0].Namespace, ctlNS)
		testutil.Equal(t, len(neutral[0].Labels), 0)
		testutil.Equal(t, neutral[0].Host, "web.jk-net.svc.cluster.local")
	})
	t.Run("nobody can resolve but a neutral pod can: fail", func(t *testing.T) {
		p := world{rDNS, rDNS, rTCP}.probes()
		res := grade1(t, netEngine(netCluster(t), p, ctl), policyCriterion())
		testutil.Equal(t, res.Status, "FAIL")
	})
	t.Run("no control namespace configured: error, never pass", func(t *testing.T) {
		p := world{rDNS, rDNS, rTCP}.probes()
		res := grade1(t, netEngine(netCluster(t), p), policyCriterion())
		testutil.Equal(t, res.Status, "ERROR")
	})
	t.Run("a failed control probe is an error", func(t *testing.T) {
		p := &fakeProbes{fn: func(req grader.ProbeRequest) (grader.ProbeResult, error) {
			if req.Protocol == grader.ProbeDNS {
				return grader.ProbeResult{}, fmt.Errorf("%w: control pod unschedulable", grader.ErrProbeFailed)
			}
			return rDNS, nil
		}}
		res := grade1(t, netEngine(netCluster(t), p, ctl), policyCriterion())
		testutil.Equal(t, res.Status, "ERROR")
	})
	t.Run("an http criterion with a DNS failure never passes", func(t *testing.T) {
		p := world{rDNS, rDNS, rDNS}.probes()
		res := grade1(t, netEngine(netCluster(t), p, ctl), httpCriterion())
		testutil.Equal(t, res.Status, "ERROR")
	})
}

func TestWrongBackend(t *testing.T) {
	cases := []struct {
		name   string
		result grader.ProbeResult
		status string
		reason string
	}{
		{"correct backend", grader.ProbeResult{Connected: true, HTTPStatus: 200, BodyMarker: netMarker}, "PASS", ""},
		{"wrong backend answers 200 without the marker", grader.ProbeResult{Connected: true, HTTPStatus: 200}, "FAIL", "marker"},
		{"right marker, wrong status", grader.ProbeResult{Connected: true, HTTPStatus: 503, BodyMarker: netMarker}, "FAIL", "503"},
		{"connected but no HTTP answer", grader.ProbeResult{Connected: true, FailureClass: grader.FailureHTTPMismatch}, "FAIL", "http-mismatch"},
		{"timeout", rTimeout, "FAIL", "timeout"},
		{"refused", rRefused, "FAIL", "refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakeProbes{fn: func(grader.ProbeRequest) (grader.ProbeResult, error) { return tc.result, nil }}
			res := grade1(t, netEngine(netCluster(t), p), httpCriterion())
			testutil.Equal(t, res.Status, tc.status)
			if !strings.Contains(res.Reason, tc.reason) {
				t.Fatalf("reason %q lacks %q", res.Reason, tc.reason)
			}
			reqs := p.calls()
			testutil.Equal(t, len(reqs), 1)
			testutil.Equal(t, reqs[0].HTTPHost, "shop.example.com")
			testutil.Equal(t, reqs[0].Path, "/healthz")
		})
	}
	t.Run("selector matches nothing", func(t *testing.T) {
		r := newReader().put(netSvc, svcObject(t))
		p := world{rHTTP, rTimeout, rTCP}.probes()
		res := grade1(t, netEngine(r, p), httpCriterion())
		testutil.Equal(t, res.Status, "FAIL")
		if !strings.Contains(res.Reason, "no ready endpoints") {
			t.Fatalf("reason %q", res.Reason)
		}
	})
}

func TestTCPCriterion(t *testing.T) {
	c := crit("db", model.CriterionTCP, 2, netSvc, map[string]any{"source": src("frontend"), "port": 80})
	p := world{frontend: rTCP}.probes()
	res := grade1(t, netEngine(netCluster(t), p), c)
	testutil.Equal(t, res.Status, "PASS")
	testutil.Equal(t, p.calls()[0].Protocol, grader.ProbeTCP)
	res = grade1(t, netEngine(netCluster(t), world{frontend: rRefused}.probes()), c)
	testutil.Equal(t, res.Status, "FAIL")
}

func TestNetworkProbeFaultsAreErrors(t *testing.T) {
	for name, fn := range map[string]func(grader.ProbeRequest) (grader.ProbeResult, error){
		"runner error": func(grader.ProbeRequest) (grader.ProbeResult, error) {
			return grader.ProbeResult{}, fmt.Errorf("%w: image not present", grader.ErrProbeFailed)
		},
		"probe-error class": func(grader.ProbeRequest) (grader.ProbeResult, error) {
			return grader.ProbeResult{FailureClass: grader.FailureProbeError}, nil
		},
	} {
		for _, c := range []model.Criterion{policyCriterion(), httpCriterion()} {
			res := grade1(t, netEngine(netCluster(t), &fakeProbes{fn: fn}), c)
			testutil.Equal(t, res.Status, "ERROR")
			if name == "runner error" && !strings.Contains(res.Reason, "image not present") {
				t.Fatalf("reason %q", res.Reason)
			}
		}
	}
	// A definite learner fault is not hidden by a fault elsewhere: ERROR here
	// would only delay the learner's feedback, but FAIL must stay FAIL.
	p := &fakeProbes{fn: func(req grader.ProbeRequest) (grader.ProbeResult, error) {
		if role(req) == "frontend" {
			return rRefused, nil
		}
		return grader.ProbeResult{}, fmt.Errorf("%w: boom", grader.ErrProbeFailed)
	}}
	res := grade1(t, netEngine(netCluster(t), p), policyCriterion())
	testutil.Equal(t, res.Status, "FAIL")
}

func TestNetworkArgsAreValidatedWithoutTheCluster(t *testing.T) {
	deny := map[string]any{"expect": "deny", "port": 80, "source": src("other")}
	allow := map[string]any{"expect": "allow", "port": 80, "source": src("frontend")}
	np := func(exp ...any) map[string]any { return map[string]any{"expectations": exp} }
	bad := map[string]model.Criterion{
		"unknown key":              crit("c", model.CriterionHTTP, 1, netSvc, map[string]any{"port": 80, "body_marker": "x", "bogus": 1}),
		"http without marker":      crit("c", model.CriterionHTTP, 1, netSvc, map[string]any{"port": 80}),
		"http without port":        crit("c", model.CriterionHTTP, 1, netSvc, map[string]any{"body_marker": "x"}),
		"port out of range":        crit("c", model.CriterionHTTP, 1, netSvc, map[string]any{"port": 70000, "body_marker": "x"}),
		"status out of range":      crit("c", model.CriterionHTTP, 1, netSvc, map[string]any{"port": 80, "body_marker": "x", "status": 99}),
		"path without slash":       crit("c", model.CriterionHTTP, 1, netSvc, map[string]any{"port": 80, "body_marker": "x", "path": "x"}),
		"host with a newline":      crit("c", model.CriterionHTTP, 1, netSvc, map[string]any{"port": 80, "body_marker": "x", "host": "a\r\nb"}),
		"marker with a newline":    crit("c", model.CriterionHTTP, 1, netSvc, map[string]any{"port": 80, "body_marker": "a\nb"}),
		"tcp with http fields":     crit("c", model.CriterionTCP, 1, netSvc, map[string]any{"port": 80, "path": "/"}),
		"tcp without port":         crit("c", model.CriterionTCP, 1, netSvc, map[string]any{}),
		"wrong target kind":        crit("c", model.CriterionTCP, 1, refOf("apps/v1", "Deployment", netNS, "web"), map[string]any{"port": 80}),
		"ingress for tcp":          crit("c", model.CriterionTCP, 1, netIng, map[string]any{"port": 80}),
		"reserved label":           crit("c", model.CriterionTCP, 1, netSvc, map[string]any{"port": 80, "source": map[string]any{"labels": map[string]any{"junkloud.io/x": "1"}}}),
		"bad label value":          crit("c", model.CriterionTCP, 1, netSvc, map[string]any{"port": 80, "source": map[string]any{"labels": map[string]any{"a": "b c"}}}),
		"bad namespace":            crit("c", model.CriterionTCP, 1, netSvc, map[string]any{"port": 80, "source": map[string]any{"namespace": "Not_Valid"}}),
		"mutates not bool":         crit("c", model.CriterionTCP, 1, netSvc, map[string]any{"port": 80, "mutates": "yes"}),
		"policy without list":      crit("c", model.CriterionNetworkPolicy, 1, netSvc, map[string]any{}),
		"policy without deny":      crit("c", model.CriterionNetworkPolicy, 1, netSvc, np(allow)),
		"policy without allow":     crit("c", model.CriterionNetworkPolicy, 1, netSvc, np(deny)),
		"deny port has no control": crit("c", model.CriterionNetworkPolicy, 1, netSvc, np(allow, map[string]any{"expect": "deny", "port": 8080, "source": src("other")})),
		"bad expect":               crit("c", model.CriterionNetworkPolicy, 1, netSvc, np(allow, deny, map[string]any{"expect": "block", "port": 80})),
		"duplicate names":          crit("c", model.CriterionNetworkPolicy, 1, netSvc, np(map[string]any{"name": "a", "expect": "allow", "port": 80}, map[string]any{"name": "a", "expect": "deny", "port": 80})),
		"expectation unknown key":  crit("c", model.CriterionNetworkPolicy, 1, netSvc, np(allow, deny, map[string]any{"expect": "allow", "port": 80, "protocol": "udp"})),
		"timeout below the minimum": func() model.Criterion {
			c := crit("c", model.CriterionTCP, 1, netSvc, map[string]any{"port": 80})
			c.Timeout = grader.MinNetworkTimeout(false) - time.Second
			return c
		}(),
		"ingress timeout below its minimum": func() model.Criterion {
			c := crit("c", model.CriterionHTTP, 1, netIng, map[string]any{"body_marker": "x"})
			c.Timeout = grader.MinNetworkTimeout(true) - time.Second
			return c
		}(),
		"policy on ingress": crit("c", model.CriterionNetworkPolicy, 1, netIng, np(allow, deny)),
		"allow http no marker": crit("c", model.CriterionNetworkPolicy, 1, netSvc, np(deny,
			map[string]any{"expect": "allow", "port": 80, "http": map[string]any{"path": "/"}})),
	}
	for name, c := range bad {
		t.Run(name, func(t *testing.T) {
			r, p := netCluster(t), healthyWorld().probes()
			e := netEngine(r, p)
			res := grade1(t, e, c)
			testutil.Equal(t, res.Status, "ERROR")
			if !strings.Contains(res.Reason, "invalid criterion") {
				t.Fatalf("reason %q", res.Reason)
			}
			testutil.Equal(t, len(p.calls()), 0)
			testutil.Equal(t, r.callsFor(netSvc), 0)
			testutil.Equal(t, len(r.listQuery), 0)
			if err := e.Validate([]model.Criterion{c}); !errors.Is(err, grader.ErrInvalidCriterion) {
				t.Fatalf("Validate = %v", err)
			}
		})
	}
	good := []model.Criterion{policyCriterion(), httpCriterion(),
		crit("t", model.CriterionTCP, 1, netSvc, map[string]any{"port": 80, "mutates": true})}
	testutil.NoError(t, netEngine(netCluster(t), healthyWorld().probes()).Validate(good))
}

func TestNetworkCriteriaRunAlone(t *testing.T) {
	nc := grader.NewNetworkChecker(netCluster(t), healthyWorld().probes())
	for _, c := range []model.Criterion{policyCriterion(), httpCriterion()} {
		testutil.Equal(t, nc.Mutates(c), true)
	}
	// Two network criteria of one problem never overlap in the engine.
	svc2 := refOf("v1", "Service", netNS, "api")
	api := svcObject(t)
	api["metadata"].(map[string]any)["name"] = "api"
	r := netCluster(t).put(svc2, api).putList("EndpointSlice", netNS, sliceObject(t, "true"),
		func() map[string]any {
			s := sliceObject(t, "true")
			s["metadata"].(map[string]any)["labels"] = map[string]any{"kubernetes.io/service-name": "api"}
			return s
		}())
	p := healthyWorld().probes()
	e := netEngine(r, p, grader.WithParallelism(4))
	c1 := httpCriterion()
	c2 := crit("api", model.CriterionHTTP, 3, svc2, map[string]any{"source": src("frontend"), "port": 80, "body_marker": netMarker})
	ev, err := e.Evaluate(context.Background(), target(c1, c2))
	testutil.NoError(t, err)
	testutil.Equal(t, statusOf(ev.Results)["web"], "PASS")
	testutil.Equal(t, statusOf(ev.Results)["api"], "PASS")
	testutil.Equal(t, p.maxHosts, 1)
}

func TestIngressTarget(t *testing.T) {
	ing := func(status string) map[string]any {
		return obj(t, `{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"name":"web","namespace":"jk-net","uid":"ing-uid"},
			"spec":{"rules":[{"host":"shop.example.com","http":{"paths":[{"path":"/","pathType":"Prefix"}]}}]},"status":`+status+`}`)
	}
	c := crit("ing", model.CriterionHTTP, 3, netIng, map[string]any{"source": src("frontend"), "body_marker": netMarker, "path": "/shop"})
	t.Run("dials the load balancer address with the rule's host", func(t *testing.T) {
		r := newReader().put(netIng, ing(`{"loadBalancer":{"ingress":[{"ip":"172.18.0.9"}]}}`))
		p := world{frontend: rHTTP}.probes()
		res := grade1(t, netEngine(r, p), c)
		testutil.Equal(t, res.Status, "PASS")
		req := p.calls()[0]
		testutil.Equal(t, req.Host, "172.18.0.9")
		testutil.Equal(t, req.Port, 80)
		testutil.Equal(t, req.HTTPHost, "shop.example.com")
		testutil.Equal(t, req.Path, "/shop")
		testutil.Equal(t, res.Evidence.UID, "ing-uid")
	})
	t.Run("wrong path on an ingress is a failed marker", func(t *testing.T) {
		r := newReader().put(netIng, ing(`{"loadBalancer":{"ingress":[{"hostname":"lb.local"}]}}`))
		p := &fakeProbes{fn: func(grader.ProbeRequest) (grader.ProbeResult, error) {
			return grader.ProbeResult{Connected: true, HTTPStatus: 404}, nil
		}}
		res := grade1(t, netEngine(r, p), c)
		testutil.Equal(t, res.Status, "FAIL")
		testutil.Equal(t, p.calls()[0].Host, "lb.local")
	})
	t.Run("no address", func(t *testing.T) {
		r := newReader().put(netIng, ing(`{}`))
		p := world{frontend: rHTTP}.probes()
		res := grade1(t, netEngine(r, p), c)
		testutil.Equal(t, res.Status, "ERROR") // our controller may just be slow
		testutil.Equal(t, len(p.calls()), 0)
		other := ing(`{}`)
		other["spec"].(map[string]any)["ingressClassName"] = "nginx"
		r = newReader().put(netIng, other)
		res = grade1(t, netEngine(r, p), c)
		testutil.Equal(t, res.Status, "FAIL") // nobody serves that class
	})
	t.Run("missing ingress", func(t *testing.T) {
		res := grade1(t, netEngine(newReader(), world{}.probes()), c)
		testutil.Equal(t, res.Status, "FAIL")
		if !strings.Contains(res.Reason, "prerequisite not satisfied") {
			t.Fatalf("reason %q", res.Reason)
		}
	})
}

func TestNetworkTimeoutMinimum(t *testing.T) {
	testutil.Equal(t, grader.MinNetworkTimeout(false), 45*time.Second)
	testutil.Equal(t, grader.MinNetworkTimeout(true), 60*time.Second)
	with := func(c model.Criterion, d time.Duration) model.Criterion { c.Timeout = d; return c }
	ing := crit("ing", model.CriterionHTTP, 1, netIng, map[string]any{"body_marker": "x"})
	tcp := crit("t", model.CriterionTCP, 1, netSvc, map[string]any{"port": 80})
	r, p := netCluster(t), healthyWorld().probes()
	e := netEngine(r, p)
	for name, tc := range map[string]struct {
		c  model.Criterion
		ok bool
	}{
		"exactly the minimum":           {with(tcp, 45*time.Second), true},
		"one second below":              {with(tcp, 44*time.Second), false},
		"ingress minimum":               {with(ing, 60*time.Second), true},
		"ingress one second below":      {with(ing, 59*time.Second), false},
		"zero uses the configured 60s":  {tcp, true},
		"policy needs the same minimum": {with(policyCriterion(), 30*time.Second), false},
	} {
		err := e.Validate([]model.Criterion{tc.c})
		if tc.ok {
			testutil.NoError(t, err)
			continue
		}
		if !errors.Is(err, grader.ErrInvalidCriterion) || !strings.Contains(err.Error(), "minimum") {
			t.Fatalf("%s: Validate = %v", name, err)
		}
		// Graded as an author error with no cluster read and no pod.
		res := grade1(t, e, tc.c)
		testutil.Equal(t, res.Status, "ERROR")
	}
	testutil.Equal(t, len(p.calls()), 0)
	// A zero timeout means the engine default: 30s is too little.
	short := netEngine(netCluster(t), healthyWorld().probes(), grader.WithDefaultTimeout(30*time.Second))
	err := short.Validate([]model.Criterion{tcp})
	if !errors.Is(err, grader.ErrInvalidCriterion) || !strings.Contains(err.Error(), "45s") {
		t.Fatalf("Validate with a 30s default = %v", err)
	}
}

// endpointCluster serves Service web with the given ready pods.
func endpointCluster(t testing.TB, eps ...[2]string) *fakeReader {
	return newReader().put(netSvc, svcObject(t)).putList("EndpointSlice", netNS, sliceWith(t, eps...))
}

var (
	podV1 = [2]string{"10.0.0.5", "web-v1"}
	podV2 = [2]string{"10.0.0.6", "web-v2"}
)

// byPod answers per destination address: the Service name picks any pod, a
// pod address reaches exactly that pod (kube-proxy's choice is invisible).
type podBehavior struct{ allowed, denied, control grader.ProbeResult }

func perPod(pods map[string]podBehavior) *fakeProbes {
	return &fakeProbes{fn: func(req grader.ProbeRequest) (grader.ProbeResult, error) {
		b, ok := pods[req.Host]
		if !ok { // the Service name: any healthy pod answers
			return rHTTP, nil
		}
		if role(req) == "frontend" {
			return b.control, nil
		}
		return b.denied, nil
	}}
}

func TestDenyIsCheckedAgainstEveryEndpoint(t *testing.T) {
	healthy := podBehavior{control: rHTTP, denied: rTimeout}
	t.Run("both pods denied and healthy", func(t *testing.T) {
		p := perPod(map[string]podBehavior{podV1[0]: healthy, podV2[0]: healthy})
		res := grade1(t, netEngine(endpointCluster(t, podV1, podV2), p), policyCriterion())
		testutil.Equal(t, res.Status, "PASS")
		seen := map[string]int{}
		for _, q := range p.calls() {
			if q.Host != "web.jk-net.svc.cluster.local" {
				testutil.Equal(t, q.Port, 8080)
				seen[q.Host+"/"+role(q)]++
			}
		}
		testutil.Equal(t, len(seen), 4) // deny and control against each pod
	})
	t.Run("canary: the policy covers only one of two backends", func(t *testing.T) {
		// podSelector version=v1 protects v1 only: the intruder reaches v2.
		// Through the Service name the deny would hit v1 with probability
		// 1/2 per attempt and could pass; per pod it cannot.
		p := perPod(map[string]podBehavior{podV1[0]: healthy, podV2[0]: {control: rHTTP, denied: rHTTP}})
		res := grade1(t, netEngine(endpointCluster(t, podV1, podV2), p), policyCriterion())
		testutil.Equal(t, res.Status, "FAIL")
		if !strings.Contains(res.Reason, "10.0.0.6") || !strings.Contains(res.Reason, "connected") {
			t.Fatalf("reason %q", res.Reason)
		}
	})
	t.Run("one backend is up but not listening, no policy at all", func(t *testing.T) {
		dead := podBehavior{control: rRefused, denied: rRefused}
		p := perPod(map[string]podBehavior{podV1[0]: {control: rHTTP, denied: rHTTP}, podV2[0]: dead})
		res := grade1(t, netEngine(endpointCluster(t, podV1, podV2), p), policyCriterion())
		testutil.Equal(t, res.Status, "FAIL")
	})
	t.Run("one backend is not listening and the deny looks like a block", func(t *testing.T) {
		// v1 is properly denied; v2 refuses everybody. A refusal from the
		// dead pod must not be read as the policy working.
		dead := podBehavior{control: rRefused, denied: rRefused}
		p := perPod(map[string]podBehavior{podV1[0]: healthy, podV2[0]: dead})
		res := grade1(t, netEngine(endpointCluster(t, podV1, podV2), p), policyCriterion())
		testutil.Equal(t, res.Status == "PASS", false)
		testutil.Equal(t, res.Status, "FAIL")
		if !strings.Contains(res.Reason, "10.0.0.6") || !strings.Contains(res.Reason, "healthy") {
			t.Fatalf("reason %q", res.Reason)
		}
	})
	t.Run("a hung backend that times out for everybody", func(t *testing.T) {
		hung := podBehavior{control: rTimeout, denied: rTimeout}
		p := perPod(map[string]podBehavior{podV1[0]: healthy, podV2[0]: hung})
		res := grade1(t, netEngine(endpointCluster(t, podV1, podV2), p), policyCriterion())
		testutil.Equal(t, res.Status, "FAIL")
	})
	t.Run("a refusal while the allowed source connects is not a drop", func(t *testing.T) {
		p := perPod(map[string]podBehavior{podV1[0]: {control: rHTTP, denied: rRefused}})
		res := grade1(t, netEngine(endpointCluster(t, podV1), p), policyCriterion())
		testutil.Equal(t, res.Status, "ERROR")
	})
	t.Run("an unclassified control failure is undecided", func(t *testing.T) {
		p := perPod(map[string]podBehavior{podV1[0]: {control: rNetError, denied: rTimeout}})
		res := grade1(t, netEngine(endpointCluster(t, podV1), p), policyCriterion())
		testutil.Equal(t, res.Status, "ERROR")
	})
	t.Run("a pod probe that cannot run is an error", func(t *testing.T) {
		p := &fakeProbes{fn: func(req grader.ProbeRequest) (grader.ProbeResult, error) {
			if req.Host == podV1[0] {
				return grader.ProbeResult{}, fmt.Errorf("%w: unschedulable", grader.ErrProbeFailed)
			}
			return rHTTP, nil
		}}
		res := grade1(t, netEngine(endpointCluster(t, podV1), p), policyCriterion())
		testutil.Equal(t, res.Status, "ERROR")
	})
	t.Run("too many endpoints to verify one by one", func(t *testing.T) {
		var eps [][2]string
		for i := 0; i < 5; i++ {
			eps = append(eps, [2]string{fmt.Sprintf("10.0.1.%d", i), fmt.Sprintf("web-%d", i)})
		}
		p := healthyWorld().probes()
		res := grade1(t, netEngine(endpointCluster(t, eps...), p), policyCriterion())
		testutil.Equal(t, res.Status, "ERROR")
		testutil.Equal(t, len(p.calls()), 0)
	})
	t.Run("an endpoint without the target port is undecided", func(t *testing.T) {
		slice := sliceWith(t, podV1)
		slice["ports"] = []any{}
		r := newReader().put(netSvc, svcObject(t)).putList("EndpointSlice", netNS, slice)
		p := healthyWorld().probes()
		res := grade1(t, netEngine(r, p), policyCriterion())
		testutil.Equal(t, res.Status, "ERROR")
		testutil.Equal(t, len(p.calls()), 0)
	})
	t.Run("only ready pods are probed", func(t *testing.T) {
		slice := sliceWith(t, podV1, podV2)
		slice["endpoints"].([]any)[1].(map[string]any)["conditions"] = map[string]any{"ready": false}
		r := newReader().put(netSvc, svcObject(t)).putList("EndpointSlice", netNS, slice)
		p := perPod(map[string]podBehavior{podV1[0]: healthy})
		res := grade1(t, netEngine(r, p), policyCriterion())
		testutil.Equal(t, res.Status, "PASS")
		for _, q := range p.calls() {
			testutil.Equal(t, q.Host == podV2[0], false)
		}
	})
}

func TestProbeBudgetFollowsTheCriterionTimeout(t *testing.T) {
	ctl := grader.WithProbeControlNamespace(ctlNS)
	t.Run("the neutral DNS control is not started without time", func(t *testing.T) {
		clk := newClock()
		start := clk.Now()
		p := &fakeProbes{fn: func(req grader.ProbeRequest) (grader.ProbeResult, error) {
			// The first wave eats all but 3s of the 60s budget.
			_ = clk.Sleep(context.Background(), 57*time.Second)
			return rDNS, nil
		}}
		res := grade1(t, netEngine(netCluster(t), p, grader.WithClock(clk), ctl), httpCriterion())
		testutil.Equal(t, res.Status, "ERROR")
		if !strings.Contains(res.Reason, "time budget") {
			t.Fatalf("reason %q", res.Reason)
		}
		for _, q := range p.calls() {
			testutil.Equal(t, q.Protocol == grader.ProbeDNS, false)
			testutil.Equal(t, q.Deadline, start.Add(60*time.Second))
		}
	})
	t.Run("the control gets the same deadline when time remains", func(t *testing.T) {
		clk := newClock()
		start := clk.Now()
		p := world{rDNS, rDNS, rTCP}.probes()
		res := grade1(t, netEngine(netCluster(t), p, grader.WithClock(clk), ctl), policyCriterion())
		testutil.Equal(t, res.Status, "FAIL")
		var n int
		for _, q := range p.calls() {
			if q.Protocol == grader.ProbeDNS {
				n++
			}
			testutil.Equal(t, q.Deadline, start.Add(60*time.Second))
		}
		testutil.Equal(t, n, 1)
	})
	t.Run("a criterion timeout longer than the default sets the deadline", func(t *testing.T) {
		clk := newClock()
		start := clk.Now()
		c := httpCriterion()
		c.Timeout = 100 * time.Second
		p := world{frontend: rHTTP}.probes()
		res := grade1(t, netEngine(netCluster(t), p, grader.WithClock(clk)), c)
		testutil.Equal(t, res.Status, "PASS")
		testutil.Equal(t, p.calls()[0].Deadline, start.Add(100*time.Second))
	})
	t.Run("a probe error caused by an expired budget is an error", func(t *testing.T) {
		p := &fakeProbes{fn: func(grader.ProbeRequest) (grader.ProbeResult, error) {
			return grader.ProbeResult{}, fmt.Errorf("%w: did not finish within the criterion's time budget", grader.ErrProbeFailed)
		}}
		res := grade1(t, netEngine(netCluster(t), p), httpCriterion())
		testutil.Equal(t, res.Status, "ERROR")
	})
}

// slowList burns budget while the EndpointSlices are read.
type slowList struct {
	*fakeReader
	clk *fakeClock
}

func (s slowList) List(ctx context.Context, q grader.ListQuery) ([]map[string]any, error) {
	_ = s.clk.Sleep(ctx, 40*time.Second)
	return s.fakeReader.List(ctx, q)
}

func TestNoProbeStartsWithoutBudget(t *testing.T) {
	clk := newClock()
	p := healthyWorld().probes()
	c := httpCriterion()
	c.Timeout = grader.MinNetworkTimeout(false)
	r := slowList{netCluster(t), clk}
	opts := []grader.Option{grader.WithClock(clk)}
	checkers := grader.DefaultCheckers(r, opts...)
	grader.AddNetworkCheckers(checkers, grader.NewNetworkChecker(r, p, opts...))
	ev, err := grader.NewEngine(checkers, opts...).Evaluate(context.Background(), target(c))
	testutil.NoError(t, err)
	res := resultFor(t, ev, "web")
	testutil.Equal(t, res.Status, "ERROR")
	if !strings.Contains(res.Reason, "time budget") {
		t.Fatalf("reason %q", res.Reason)
	}
	testutil.Equal(t, len(p.calls()), 0)
}
