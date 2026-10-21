package grader

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
)

type membershipReader struct {
	objects map[model.ObjectRef]object
	slices  []object
	failure error
}

func (r *membershipReader) Get(_ context.Context, ref model.ObjectRef) (object, error) {
	if r.failure != nil {
		return nil, r.failure
	}
	o := r.objects[ref]
	if o == nil {
		return nil, ErrNotFound
	}
	return o, nil
}
func (r *membershipReader) List(_ context.Context, q ListQuery) ([]object, error) {
	if r.failure != nil {
		return nil, r.failure
	}
	if q.Kind == "EndpointSlice" {
		return r.slices, nil
	}
	var out []object
	for ref, o := range r.objects {
		if ref.Kind == q.Kind && ref.Namespace == q.Namespace {
			out = append(out, o)
		}
	}
	return out, nil
}

type membershipProbes struct {
	mu    sync.Mutex
	calls []ProbeRequest
	fn    func(ProbeRequest) (ProbeResult, error)
}

func (p *membershipProbes) Probe(_ context.Context, q ProbeRequest) (ProbeResult, error) {
	p.mu.Lock()
	p.calls = append(p.calls, q)
	p.mu.Unlock()
	if p.fn != nil {
		return p.fn(q)
	}
	return ProbeResult{Connected: true, HTTPStatus: 200, BodyMarker: q.BodyMarker}, nil
}
func memberRef(kind, name string) model.ObjectRef {
	api := "apps/v1"
	if kind == "Pod" || kind == "Service" || kind == "Namespace" || kind == "ConfigMap" {
		api = "v1"
	}
	ns := "member"
	if kind == "Namespace" {
		ns = ""
	}
	return model.ObjectRef{APIVersion: api, Kind: kind, Namespace: ns, Name: name}
}
func memberObject(kind, name, uid string) object {
	return object{"apiVersion": memberRef(kind, name).APIVersion, "kind": kind, "metadata": object{"name": name, "namespace": "member", "uid": uid}}
}
func memberOwner(api, kind, name, uid string) []any {
	return []any{object{"apiVersion": api, "kind": kind, "name": name, "uid": uid, "controller": true}}
}
func memberFixture() (*membershipReader, *membershipProbes, Target, model.Criterion) {
	r := &membershipReader{objects: map[model.ObjectRef]object{}}
	ns := memberObject("Namespace", "member", "namespace-uid")
	ns["metadata"].(object)["labels"] = object{"junkloud.io/environment-id": "env", "junkloud.io/session-id": "session", "junkloud.io/problem-id": "release"}
	r.objects[memberRef("Namespace", "member")] = ns
	svc := memberObject("Service", "web", "service-uid")
	svc["spec"] = object{"selector": object{"app": "release"}, "ports": []any{object{"port": 80, "targetPort": 8080, "protocol": "TCP"}}}
	r.objects[memberRef("Service", "web")] = svc
	var eps []any
	for i, name := range []string{"stable", "canary"} {
		d := memberObject("Deployment", name, name+"-uid")
		d["spec"] = object{"replicas": 1, "template": object{"metadata": object{"labels": object{"app": "release"}}, "spec": object{"containers": []any{object{"name": "web", "image": "busybox:1.37.0", "envFrom": []any{object{"configMapRef": object{"name": name + "-config"}}}}}}}}
		r.objects[memberRef("Deployment", name)] = d
		rs := memberObject("ReplicaSet", name+"-rs", name+"-rs-uid")
		rs["metadata"].(object)["ownerReferences"] = memberOwner("apps/v1", "Deployment", name, name+"-uid")
		r.objects[memberRef("ReplicaSet", name+"-rs")] = rs
		p := memberObject("Pod", name+"-pod", name+"-pod-uid")
		p["metadata"].(object)["ownerReferences"] = memberOwner("apps/v1", "ReplicaSet", name+"-rs", name+"-rs-uid")
		p["metadata"].(object)["labels"] = object{"app": "release"}
		ip := fmt.Sprintf("10.0.0.%d", i+1)
		p["spec"] = object{"containers": []any{object{"name": "web"}}}
		p["status"] = object{"phase": "Running", "podIP": ip, "podIPs": []any{object{"ip": ip}}, "conditions": []any{object{"type": "Ready", "status": "True"}}}
		r.objects[memberRef("Pod", name+"-pod")] = p
		eps = append(eps, object{"addresses": []any{ip}, "conditions": object{"ready": true}, "targetRef": object{"apiVersion": "v1", "kind": "Pod", "namespace": "member", "name": name + "-pod", "uid": name + "-pod-uid"}})
		cm := memberObject("ConfigMap", name+"-config", name+"-config-uid")
		cm["data"] = object{"marker": name}
		r.objects[model.ObjectRef{APIVersion: "v1", Kind: "ConfigMap", Namespace: "member", Name: name + "-config"}] = cm
	}
	s := object{"apiVersion": endpointSliceAPI, "kind": "EndpointSlice", "metadata": object{"name": "web-slice", "namespace": "member", "uid": "slice-uid", "labels": object{serviceNameLabel: "web"}, "ownerReferences": memberOwner("v1", "Service", "web", "service-uid")}, "addressType": "IPv4", "ports": []any{object{"port": 8080, "protocol": "TCP"}}, "endpoints": eps}
	r.slices = []object{s}
	crit := model.Criterion{ID: "members", Kind: "endpoint-membership", Target: memberRef("Service", "web"), Points: 1, Timeout: 90 * time.Second, Args: map[string]any{"source": object{"labels": object{"role": "client"}}, "port": 80, "path": "/", "backends": []any{object{"deployment": "stable", "body_marker": "STABLE"}, object{"deployment": "canary", "body_marker": "CANARY"}}}}
	target := Target{Identity: model.Identity{ID: "env"}, SessionID: "session", Problem: model.FrozenProblem{Definition: model.Problem{ID: "release", Namespace: "member", Criteria: []model.Criterion{crit}}}}
	return r, &membershipProbes{}, target, crit
}
func TestEndpointMembership(t *testing.T) {
	cases := []struct {
		name, want string
		mutate     func(*membershipReader, *membershipProbes, *Target, *model.Criterion)
	}{
		{"both deterministic backends", "PASS", nil},
		{"missing canary", "FAIL", func(r *membershipReader, _ *membershipProbes, _ *Target, _ *model.Criterion) {
			r.slices[0]["endpoints"] = listAt(r.slices[0], "endpoints")[:1]
		}},
		{"missing stable", "FAIL", func(r *membershipReader, _ *membershipProbes, _ *Target, _ *model.Criterion) {
			r.slices[0]["endpoints"] = listAt(r.slices[0], "endpoints")[1:]
		}},
		{"empty endpoints", "FAIL", func(r *membershipReader, _ *membershipProbes, _ *Target, _ *model.Criterion) { r.slices = nil }},
		{"unready endpoint", "FAIL", func(r *membershipReader, _ *membershipProbes, _ *Target, _ *model.Criterion) {
			asMap(listAt(r.slices[0], "endpoints")[1])["conditions"] = object{"ready": false}
		}},
		{"wrong selector", "FAIL", func(r *membershipReader, _ *membershipProbes, _ *Target, _ *model.Criterion) {
			r.objects[memberRef("Service", "web")]["spec"].(object)["selector"] = object{"app": "wrong"}
		}},
		{"forged slice owner", "ERROR", func(r *membershipReader, _ *membershipProbes, _ *Target, _ *model.Criterion) {
			asMap(listAt(r.slices[0], "metadata", "ownerReferences")[0])["uid"] = "forged"
		}},
		{"forged pod uid", "ERROR", func(r *membershipReader, _ *membershipProbes, _ *Target, _ *model.Criterion) {
			asMap(asMap(listAt(r.slices[0], "endpoints")[0])["targetRef"])["uid"] = "forged"
		}},
		{"forged RS uid", "ERROR", func(r *membershipReader, _ *membershipProbes, _ *Target, _ *model.Criterion) {
			asMap(listAt(r.objects[memberRef("Pod", "stable-pod")], "metadata", "ownerReferences")[0])["uid"] = "forged"
		}},
		{"forged deployment uid", "ERROR", func(r *membershipReader, _ *membershipProbes, _ *Target, _ *model.Criterion) {
			asMap(listAt(r.objects[memberRef("ReplicaSet", "stable-rs")], "metadata", "ownerReferences")[0])["uid"] = "forged"
		}},
		{"stale IP", "ERROR", func(r *membershipReader, _ *membershipProbes, _ *Target, _ *model.Criterion) {
			asMap(listAt(r.slices[0], "endpoints")[0])["addresses"] = []any{"10.0.0.99"}
		}},
		{"unproven port", "ERROR", func(r *membershipReader, _ *membershipProbes, _ *Target, _ *model.Criterion) {
			asMap(listAt(r.slices[0], "ports")[0])["port"] = 9090
		}},
		{"wrong backend behavior", "FAIL", func(_ *membershipReader, p *membershipProbes, _ *Target, _ *model.Criterion) {
			p.fn = func(q ProbeRequest) (ProbeResult, error) {
				if q.Host == "10.0.0.2" {
					return ProbeResult{Connected: true, HTTPStatus: 200, FailureClass: FailureHTTPMismatch}, nil
				}
				return ProbeResult{Connected: true, HTTPStatus: 200, BodyMarker: q.BodyMarker}, nil
			}
		}},
		{"API failure", "ERROR", func(r *membershipReader, _ *membershipProbes, _ *Target, _ *model.Criterion) {
			r.failure = errors.New("API down")
		}},
		{"probe failure", "ERROR", func(_ *membershipReader, p *membershipProbes, _ *Target, _ *model.Criterion) {
			p.fn = func(ProbeRequest) (ProbeResult, error) { return ProbeResult{}, ErrProbeFailed }
		}},
		{"missing expected session", "ERROR", func(_ *membershipReader, _ *membershipProbes, t *Target, _ *model.Criterion) { t.SessionID = "" }},
		{"mismatching session", "ERROR", func(_ *membershipReader, _ *membershipProbes, t *Target, _ *model.Criterion) { t.SessionID = "other" }},
		{"foreign source", "ERROR", func(_ *membershipReader, _ *membershipProbes, _ *Target, c *model.Criterion) {
			c.Args["source"] = object{"namespace": "foreign"}
		}},
		{"unknown args", "ERROR", func(_ *membershipReader, _ *membershipProbes, _ *Target, c *model.Criterion) {
			c.Args["random_ratio"] = 0.5
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, p, target, c := memberFixture()
			if tc.mutate != nil {
				tc.mutate(r, p, &target, &c)
			}
			check := NewNetworkChecker(r, p)
			res, err := check.Check(context.Background(), target, c)
			if tc.want == "ERROR" {
				if err == nil {
					t.Fatalf("want ERROR, got %+v", res)
				}
				if tc.name == "missing expected session" || tc.name == "mismatching session" || tc.name == "foreign source" || tc.name == "unknown args" {
					if len(p.calls) != 0 {
						t.Fatal("unauthorized probes")
					}
				}
				return
			}
			if err != nil || res.Status != tc.want {
				t.Fatalf("got %s %v (%s), want %s", res.Status, err, res.Reason, tc.want)
			}
			if tc.want == "PASS" {
				hosts := map[string]ProbeRequest{}
				for _, q := range p.calls {
					hosts[q.Host] = q
				}
				if len(hosts) != 3 || hosts["10.0.0.1"].BodyMarker != "STABLE" || hosts["10.0.0.2"].BodyMarker != "CANARY" || hosts["web.member.svc.cluster.local"].Protocol != ProbeTCP {
					t.Fatalf("non deterministic/control probes: %+v", p.calls)
				}
			}
		})
	}
}
func TestEndpointMembershipCaptureExcludedBackend(t *testing.T) {
	r, _, target, _ := memberFixture()
	r.objects[memberRef("Service", "web")]["spec"].(object)["selector"] = object{"app": "wrong"}
	a := NewArtifactChecker(ArtifactConfig{Reader: r})
	before, err := a.CaptureInputs(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"stable", "canary"} {
		if before[objectKey(memberRef("Deployment", name))] == "" {
			t.Fatal("excluded backend missing", name)
		}
	}
	r.objects[memberRef("Deployment", "canary")]["spec"].(object)["replicas"] = 0
	after, err := a.CaptureInputs(context.Background(), target)
	if err != nil || reflect.DeepEqual(before, after) {
		t.Fatal("backend change invisible", err)
	}
	if before["object:v1/ConfigMap/member/canary-config"] == "" {
		t.Fatal("referenced config invisible")
	}
}
func TestEndpointMembershipRegistration(t *testing.T) {
	r, p, _, c := memberFixture()
	checks := DefaultCheckers(r)
	n := NewNetworkChecker(r, p)
	AddNetworkCheckers(checks, n)
	if checks[c.Kind] != n || !n.Mutates(c) {
		t.Fatal("membership must use serialized native NetworkChecker")
	}
}

func TestEndpointMembershipPortAndIdentityContracts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		edit      func(*membershipReader)
		wantError bool
	}{
		{"omitted core target defaults", func(r *membershipReader) {
			for _, v := range listAt(r.slices[0], "endpoints") {
				tr := mapAt(asMap(v), "targetRef")
				delete(tr, "apiVersion")
				delete(tr, "namespace")
			}
		}, false},
		{"numeric port needs no container declaration", func(*membershipReader) {}, false},
		{"named TCP target", func(r *membershipReader) {
			asMap(listAt(r.objects[memberRef("Service", "web")], "spec", "ports")[0])["targetPort"] = "http"
			for _, name := range []string{"stable", "canary"} {
				asMap(listAt(r.objects[memberRef("Pod", name+"-pod")], "spec", "containers")[0])["ports"] = []any{object{"name": "http", "containerPort": 8080, "protocol": "TCP"}}
			}
		}, false},
		{"missing named target", func(r *membershipReader) {
			asMap(listAt(r.objects[memberRef("Service", "web")], "spec", "ports")[0])["targetPort"] = "http"
		}, true},
		{"Slice missing UID", func(r *membershipReader) { delete(mapAt(r.slices[0], "metadata"), "uid") }, true},
		{"foreign target namespace", func(r *membershipReader) {
			mapAt(asMap(listAt(r.slices[0], "endpoints")[0]), "targetRef")["namespace"] = "foreign"
		}, true},
		{"unknown readiness", func(r *membershipReader) {
			delete(mapAt(asMap(listAt(r.slices[0], "endpoints")[0]), "conditions"), "ready")
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, p, target, c := memberFixture()
			tc.edit(r)
			got, err := NewNetworkChecker(r, p).Check(context.Background(), target, c)
			if tc.wantError {
				if err == nil {
					t.Fatalf("want ERROR, got %+v", got)
				}
			} else if err != nil || got.Status != model.StatusPass {
				t.Fatalf("got %+v %v", got, err)
			}
		})
	}
}

func TestEndpointMembershipReplacementDuringProbe(t *testing.T) {
	for _, kind := range []string{"Namespace", "Service", "Deployment", "ReplicaSet", "Pod", "EndpointSlice"} {
		t.Run(kind, func(t *testing.T) {
			r, p, target, c := memberFixture()
			var once sync.Once
			p.fn = func(q ProbeRequest) (ProbeResult, error) {
				once.Do(func() {
					var o object
					switch kind {
					case "Namespace":
						o = r.objects[memberRef(kind, "member")]
					case "Service":
						o = r.objects[memberRef(kind, "web")]
					case "Deployment":
						o = r.objects[memberRef(kind, "stable")]
					case "ReplicaSet":
						o = r.objects[memberRef(kind, "stable-rs")]
					case "Pod":
						o = r.objects[memberRef(kind, "stable-pod")]
					case "EndpointSlice":
						o = r.slices[0]
					}
					mapAt(o, "metadata")["uid"] = "replacement"
				})
				return ProbeResult{Connected: true, HTTPStatus: 200, BodyMarker: q.BodyMarker}, nil
			}
			_, err := NewNetworkChecker(r, p).Check(context.Background(), target, c)
			if !errors.Is(err, ErrTargetChanged) {
				t.Fatalf("replacement accepted: %v", err)
			}
		})
	}
}

func TestEndpointMembershipRejectsUnknownNestedAndTimeout(t *testing.T) {
	for _, edit := range []func(*model.Criterion){func(c *model.Criterion) { c.Timeout = 0 }, func(c *model.Criterion) { c.Timeout = 89 * time.Second }, func(c *model.Criterion) { asMap(c.Args["source"])["unknown"] = true }, func(c *model.Criterion) { asMap(c.Args["backends"].([]any)[0])["unknown"] = true }, func(c *model.Criterion) { asMap(c.Args["backends"].([]any)[1])["deployment"] = "stable" }, func(c *model.Criterion) { c.Args["backends"] = c.Args["backends"].([]any)[:1] }} {
		r, p, target, c := memberFixture()
		edit(&c)
		_, err := NewNetworkChecker(r, p).Check(context.Background(), target, c)
		if !errors.Is(err, ErrInvalidCriterion) || len(p.calls) != 0 {
			t.Fatalf("invalid definition mutated: %v", err)
		}
	}
}

func TestEndpointMembershipCancellation(t *testing.T) {
	r, p, target, c := memberFixture()
	ctx, cancel := context.WithCancel(context.Background())
	p.fn = func(q ProbeRequest) (ProbeResult, error) {
		cancel()
		return ProbeResult{Connected: true, HTTPStatus: 200, BodyMarker: q.BodyMarker}, nil
	}
	_, err := NewNetworkChecker(r, p).Check(ctx, target, c)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestEndpointMembershipBoundAndUnlistedBackend(t *testing.T) {
	for _, extra := range []bool{false, true} {
		r, p, target, c := memberFixture()
		for i := 0; i < 3; i++ {
			name := fmt.Sprintf("extra-%d", i)
			pod := memberObject("Pod", name, name+"-uid")
			pod["metadata"].(object)["labels"] = object{"app": "release"}
			pod["metadata"].(object)["ownerReferences"] = memberOwner("apps/v1", "ReplicaSet", "stable-rs", "stable-rs-uid")
			ip := fmt.Sprintf("10.0.1.%d", i+1)
			pod["status"] = object{"phase": "Running", "podIP": ip, "conditions": []any{object{"type": "Ready", "status": "True"}}}
			r.objects[memberRef("Pod", name)] = pod
			r.slices[0]["endpoints"] = append(listAt(r.slices[0], "endpoints"), object{"conditions": object{"ready": true}, "addresses": []any{ip}, "targetRef": object{"kind": "Pod", "name": name, "uid": name + "-uid"}})
		}
		if extra {
			d := memberObject("Deployment", "other", "other-uid")
			r.objects[memberRef("Deployment", "other")] = d
			mapAt(r.objects[memberRef("ReplicaSet", "stable-rs")], "metadata")["ownerReferences"] = memberOwner("apps/v1", "Deployment", "other", "other-uid")
		}
		res, err := NewNetworkChecker(r, p).Check(context.Background(), target, c)
		if extra {
			if err != nil || res.Status != model.StatusFail {
				t.Fatalf("unlisted member accepted: %+v %v", res, err)
			}
		} else if err == nil {
			t.Fatal("unbounded endpoint observation accepted")
		}
		if len(p.calls) != 0 {
			t.Fatal("structurally invalid membership was probed")
		}
	}
}

func TestEndpointMembershipControllerChurnIsNotReplacement(t *testing.T) {
	r, p, target, c := memberFixture()
	var once sync.Once
	p.fn = func(q ProbeRequest) (ProbeResult, error) {
		once.Do(func() {
			for _, o := range r.objects {
				mapAt(o, "metadata")["resourceVersion"] = "next"
				if stringAt(o, "kind") == "Deployment" {
					o["status"] = object{"readyReplicas": 1}
				}
			}
		})
		return ProbeResult{Connected: true, HTTPStatus: 200, BodyMarker: q.BodyMarker}, nil
	}
	res, err := NewNetworkChecker(r, p).Check(context.Background(), target, c)
	if err != nil || res.Status != model.StatusPass {
		t.Fatalf("controller churn altered verdict: %+v %v", res, err)
	}
}
