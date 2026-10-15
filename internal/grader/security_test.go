package grader_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// fakeAccess is an authorizer: rules decide per request, tokens map to users.
type fakeAccess struct {
	mu       sync.Mutex
	allow    func(s grader.AccessSubject, a grader.AccessAttributes) bool
	tokens   map[string]string // token -> user
	reviews  []grader.AccessSubject
	attrs    []grader.AccessAttributes
	reviewFn func() error
}

func (f *fakeAccess) Review(_ context.Context, s grader.AccessSubject, a grader.AccessAttributes) (grader.AccessDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reviews, f.attrs = append(f.reviews, s), append(f.attrs, a)
	if f.reviewFn != nil {
		if err := f.reviewFn(); err != nil {
			return grader.AccessDecision{}, err
		}
	}
	ok := f.allow(s, a)
	return grader.AccessDecision{Allowed: ok, Reason: fmt.Sprintf("fake: %s", map[bool]string{true: "allowed", false: "no RBAC policy matched"}[ok])}, nil
}

func (f *fakeAccess) TokenUser(_ context.Context, token string) (string, bool, error) {
	u, ok := f.tokens[token]
	return u, ok, nil
}

const saToken = "eyJhbGciOiJSUzI1NiJ9.reader-token-payload.sig"

func saUser(ns, name string) string { return "system:serviceaccount:" + ns + ":" + name }

// readerRules: the SA "reader" may get/list pods in jk-p1 and nothing else.
func readerRules(s grader.AccessSubject, a grader.AccessAttributes) bool {
	return s.User == saUser("jk-p1", "reader") && a.Namespace == "jk-p1" && a.Resource == "pods" && (a.Verb == "get" || a.Verb == "list")
}

var saRef = refOf("v1", "ServiceAccount", "jk-p1", "reader")

func saDoc(t testing.TB, name string) map[string]any {
	return obj(t, fmt.Sprintf(`{"apiVersion":"v1","kind":"ServiceAccount","metadata":{"name":%q,"namespace":"jk-p1","uid":"sa-%s"}}`, name, name))
}

func podAs(t testing.TB, name, sa string) map[string]any {
	return obj(t, fmt.Sprintf(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":%q,"namespace":"jk-p1","uid":"u-%s"},
	 "spec":{"serviceAccountName":%q,"containers":[{"name":"app","image":"busybox:1.37.0"}]},"status":{"phase":"Running"}}`, name, name, sa))
}

func authCrit(args map[string]any) model.Criterion {
	return model.Criterion{ID: "c", Kind: model.CriterionAuthorization, Target: saRef, Points: 4, Args: args}
}

func check(verb, resource string, allowed bool, extra ...any) map[string]any {
	m := map[string]any{"verb": verb, "resource": resource, "allowed": allowed}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i].(string)] = extra[i+1]
	}
	return m
}

func gradeSecurity(t testing.TB, cfg grader.SecurityConfig, c model.Criterion) model.CheckResult {
	t.Helper()
	opts := []grader.Option{grader.WithClock(newClock()), grader.WithRedactor(grader.NewRedactor())}
	checkers := grader.DefaultCheckers(cfg.Reader, opts...)
	grader.AddSecurityCheckers(checkers, grader.NewSecurityChecker(cfg, opts...))
	ev, err := grader.NewEngine(checkers, opts...).Evaluate(context.Background(), target(c))
	testutil.NoError(t, err)
	return ev.Results[0]
}

func TestAuthorizationLeastPrivilege(t *testing.T) {
	cfg := func(allow func(grader.AccessSubject, grader.AccessAttributes) bool) (grader.SecurityConfig, *fakeAccess) {
		acc := &fakeAccess{allow: allow}
		return grader.SecurityConfig{Reader: newReader().put(saRef, saDoc(t, "reader")), Access: acc}, acc
	}
	cases := []struct {
		name   string
		rules  func(grader.AccessSubject, grader.AccessAttributes) bool
		checks []any
		want   string
		reason []string
	}{
		{name: "exactly what is needed", rules: readerRules, checks: []any{
			check("get", "pods", true), check("list", "pods", true), check("delete", "pods", false), check("get", "secrets", false)}, want: "PASS"},
		{name: "missing permission", rules: func(s grader.AccessSubject, a grader.AccessAttributes) bool { return false },
			checks: []any{check("get", "pods", true)}, want: "FAIL", reason: []string{"get pods", "allowed"}},
		{name: "overprivileged", rules: func(s grader.AccessSubject, a grader.AccessAttributes) bool { return true },
			checks: []any{check("get", "pods", true), check("delete", "pods", false)}, want: "FAIL", reason: []string{"delete pods", "must not be allowed"}},
		{name: "cluster admin binding", rules: func(s grader.AccessSubject, a grader.AccessAttributes) bool { return true },
			checks: []any{check("*", "*", false, "group", "*", "cluster_scope", true)}, want: "FAIL", reason: []string{"must not be allowed"}},
		{name: "no access to other namespaces", rules: readerRules, checks: []any{check("get", "pods", false, "namespace", "kube-system")}, want: "PASS"},
		{name: "read across all namespaces", rules: readerRules, checks: []any{check("list", "pods", false, "cluster_scope", true)}, want: "PASS"},
		{name: "subresource and name", rules: func(s grader.AccessSubject, a grader.AccessAttributes) bool {
			return a.Subresource == "log" && a.Name == "web-0" && s.User == saUser("jk-p1", "reader")
		}, checks: []any{check("get", "pods", true, "subresource", "log", "name", "web-0"), check("get", "pods", false, "subresource", "exec")}, want: "PASS"},
		{name: "non-resource url", rules: func(s grader.AccessSubject, a grader.AccessAttributes) bool { return a.Path == "/healthz" },
			checks: []any{map[string]any{"verb": "get", "path": "/healthz", "allowed": true}, map[string]any{"verb": "get", "path": "/metrics", "allowed": false}}, want: "PASS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, acc := cfg(tc.rules)
			res := gradeSecurity(t, c, authCrit(map[string]any{"checks": tc.checks}))
			testutil.Equal(t, res.Status, tc.want)
			reasonHas(t, res, tc.reason...)
			// The declared ServiceAccount identity is what is reviewed, with the
			// groups the API server gives a service account.
			s := acc.reviews[0]
			testutil.Equal(t, s.User, saUser("jk-p1", "reader"))
			testutil.Equal(t, strings.Join(s.Groups, ","), "system:serviceaccounts,system:serviceaccounts:jk-p1,system:authenticated")
		})
	}
	t.Run("missing service account", func(t *testing.T) {
		acc := &fakeAccess{allow: readerRules}
		res := gradeSecurity(t, grader.SecurityConfig{Reader: newReader(), Access: acc}, authCrit(map[string]any{"checks": []any{check("get", "pods", true)}}))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "ServiceAccount", "not found")
		testutil.Equal(t, len(acc.reviews), 0)
	})
	t.Run("authorizer failure is an error", func(t *testing.T) {
		c, acc := cfg(readerRules)
		acc.reviewFn = func() error { return errors.New("apiserver unavailable") }
		res := gradeSecurity(t, c, authCrit(map[string]any{"checks": []any{check("get", "pods", true)}}))
		testutil.Equal(t, res.Status, "ERROR")
	})
	t.Run("no reviewer configured is an error", func(t *testing.T) {
		res := gradeSecurity(t, grader.SecurityConfig{Reader: newReader().put(saRef, saDoc(t, "reader"))}, authCrit(map[string]any{"checks": []any{check("get", "pods", true)}}))
		testutil.Equal(t, res.Status, "ERROR")
	})
}

// TestEffectiveIdentity: a ServiceAccount can have exactly the right
// permissions and still be the wrong answer, because the pod does not run as
// it. The original fixture is a pod whose token is the reader's; the wrong
// answers are a pod on the default SA and a pod whose spec says "reader" but
// whose credential belongs to another identity.
func TestEffectiveIdentity(t *testing.T) {
	podRef := refOf("v1", "Pod", "jk-p1", "app")
	runsAs := func(identity string) map[string]any {
		return map[string]any{"checks": []any{check("get", "pods", true), check("delete", "pods", false)},
			"runs_as": map[string]any{"kind": "Pod", "api_version": "v1", "name": "app", "identity": identity}}
	}
	setup := func(sa string, tokenUser string) (grader.SecurityConfig, *fakePods, *fakeAccess) {
		pods := newPods()
		pods.execs["jk-p1/app/app cat /var/run/secrets/kubernetes.io/serviceaccount/token"] = grader.ExecResult{Stdout: saToken}
		acc := &fakeAccess{allow: readerRules, tokens: map[string]string{saToken: tokenUser}}
		r := newReader().put(saRef, saDoc(t, "reader")).put(podRef, podAs(t, "app", sa))
		return grader.SecurityConfig{Reader: r, Access: acc, Pods: pods}, pods, acc
	}
	t.Run("pod runs as the service account", func(t *testing.T) {
		c, _, acc := setup("reader", saUser("jk-p1", "reader"))
		res := gradeSecurity(t, c, authCrit(runsAs("token")))
		testutil.Equal(t, res.Status, "PASS")
		// The review used the identity the cluster authenticated from the token.
		testutil.Equal(t, acc.reviews[0].User, saUser("jk-p1", "reader"))
	})
	t.Run("spec level identity", func(t *testing.T) {
		c, pods, _ := setup("reader", saUser("jk-p1", "reader"))
		res := gradeSecurity(t, c, authCrit(runsAs("spec")))
		testutil.Equal(t, res.Status, "PASS")
		testutil.Equal(t, len(pods.ran), 0) // nothing is exec'd for a spec-level check
	})
	t.Run("pod on the default service account", func(t *testing.T) {
		c, _, _ := setup("default", saUser("jk-p1", "default"))
		for _, mode := range []string{"spec", "token"} {
			res := gradeSecurity(t, c, authCrit(runsAs(mode)))
			testutil.Equal(t, res.Status, "FAIL")
			reasonHas(t, res, "pod app", "default", "reader")
		}
	})
	t.Run("spec says reader but the credential is another identity", func(t *testing.T) {
		c, _, _ := setup("reader", saUser("jk-p1", "default"))
		res := gradeSecurity(t, c, authCrit(runsAs("token")))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "credential", saUser("jk-p1", "default"))
		// The spec-level check alone cannot see this: that is why token exists.
		testutil.Equal(t, gradeSecurity(t, c, authCrit(runsAs("spec"))).Status, "PASS")
	})
	t.Run("token never reaches the evidence", func(t *testing.T) {
		c, _, _ := setup("reader", saUser("jk-p1", "default"))
		res := gradeSecurity(t, c, authCrit(runsAs("token")))
		if strings.Contains(evidenceJSON(t, res), "reader-token-payload") {
			t.Errorf("token in the evidence: %s", evidenceJSON(t, res))
		}
	})
	t.Run("an unauthenticated token", func(t *testing.T) {
		c, pods, _ := setup("reader", saUser("jk-p1", "reader"))
		pods.execs["jk-p1/app/app cat /var/run/secrets/kubernetes.io/serviceaccount/token"] = grader.ExecResult{Stdout: "forged.token.value"}
		res := gradeSecurity(t, c, authCrit(runsAs("token")))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "not authenticated")
	})
	t.Run("pod without a mounted credential", func(t *testing.T) {
		c, pods, _ := setup("reader", saUser("jk-p1", "reader"))
		pods.execs["jk-p1/app/app cat /var/run/secrets/kubernetes.io/serviceaccount/token"] = grader.ExecResult{ExitCode: 1, Stderr: "cat: can't open '/var/run/secrets/kubernetes.io/serviceaccount/token': No such file or directory"}
		res := gradeSecurity(t, c, authCrit(runsAs("token")))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "no service account credential")
	})
	t.Run("image without cat cannot be shown", func(t *testing.T) {
		c, pods, _ := setup("reader", saUser("jk-p1", "reader"))
		delete(pods.execs, "jk-p1/app/app cat /var/run/secrets/kubernetes.io/serviceaccount/token")
		res := gradeSecurity(t, c, authCrit(runsAs("token")))
		testutil.Equal(t, res.Status, "ERROR")
	})
	t.Run("workload pods are all checked", func(t *testing.T) {
		pods := newPods()
		r := deployLogReader(t, podAs(t, "web-a", "reader"), podAs(t, "web-b", "default"))
		for _, p := range []string{"web-a", "web-b"} {
			r.putList("Pod", "jk-p1", podOwned(t, "web-a", "reader"), podOwned(t, "web-b", "default"))
			_ = p
		}
		r.put(saRef, saDoc(t, "reader"))
		cfg := grader.SecurityConfig{Reader: r, Access: &fakeAccess{allow: readerRules}, Pods: pods}
		args := map[string]any{"checks": []any{check("get", "pods", true)}, "runs_as": map[string]any{"kind": "Deployment", "api_version": "apps/v1", "name": "web", "identity": "spec"}}
		res := gradeSecurity(t, cfg, authCrit(args))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "web-b", "default")
	})
	t.Run("no running pod", func(t *testing.T) {
		c, _, _ := setup("reader", saUser("jk-p1", "reader"))
		c.Reader.(*fakeReader).put(podRef, nil)
		res := gradeSecurity(t, c, authCrit(runsAs("spec")))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "Pod", "not found")
	})
}

func podOwned(t testing.TB, name, sa string) map[string]any {
	p := podAs(t, name, sa)
	p["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"uid": "rs-1", "controller": true}}
	p["metadata"].(map[string]any)["labels"] = map[string]any{"app": "web"}
	return p
}

func TestAuthorizationArgsAreStrict(t *testing.T) {
	ck := grader.NewSecurityChecker(grader.SecurityConfig{})
	good := []any{check("get", "pods", true)}
	bad := map[string]model.Criterion{
		"unknown key":                 authCrit(map[string]any{"checks": good, "bogus": 1}),
		"no checks":                   authCrit(map[string]any{}),
		"no verb":                     authCrit(map[string]any{"checks": []any{map[string]any{"resource": "pods", "allowed": true}}}),
		"no resource":                 authCrit(map[string]any{"checks": []any{map[string]any{"verb": "get", "allowed": true}}}),
		"allowed missing":             authCrit(map[string]any{"checks": []any{map[string]any{"verb": "get", "resource": "pods"}}}),
		"resource and path":           authCrit(map[string]any{"checks": []any{map[string]any{"verb": "get", "resource": "pods", "path": "/x", "allowed": true}}}),
		"path without slash":          authCrit(map[string]any{"checks": []any{map[string]any{"verb": "get", "path": "x", "allowed": true}}}),
		"cluster scope and namespace": authCrit(map[string]any{"checks": []any{check("get", "pods", true, "cluster_scope", true, "namespace", "x")}}),
		"bad identity":                authCrit(map[string]any{"checks": good, "runs_as": map[string]any{"kind": "Pod", "api_version": "v1", "name": "p", "identity": "magic"}}),
		"runs_as kind":                authCrit(map[string]any{"checks": good, "runs_as": map[string]any{"kind": "ConfigMap", "api_version": "v1", "name": "p"}}),
		"not a service account": func() model.Criterion {
			c := authCrit(map[string]any{"checks": good})
			c.Target = refOf("v1", "Pod", "ns", "x")
			return c
		}(),
		"too many checks": authCrit(map[string]any{"checks": func() []any {
			var l []any
			for i := 0; i < 40; i++ {
				l = append(l, check("get", "pods", true))
			}
			return l
		}()}),
	}
	for name, c := range bad {
		if err := ck.ValidateCriterion(c); !isInvalid(err) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	testutil.NoError(t, ck.ValidateCriterion(authCrit(map[string]any{"checks": good})))
}
