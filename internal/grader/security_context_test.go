package grader_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func scCrit(ref model.ObjectRef, args map[string]any) model.Criterion {
	return model.Criterion{ID: "c", Kind: model.CriterionSecurityContext, Target: ref, Points: 4, Args: args}
}

func deployWithSecurity(t testing.TB, podCtx, containerCtx string) map[string]any {
	pc, cc := "", ""
	if podCtx != "" {
		pc = fmt.Sprintf(`,"securityContext":%s`, podCtx)
	}
	if containerCtx != "" {
		cc = fmt.Sprintf(`,"securityContext":%s`, containerCtx)
	}
	return obj(t, fmt.Sprintf(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"jk-p1","uid":"dep-1","generation":1},
	 "spec":{"selector":{"matchLabels":{"app":"web"}},"template":{"spec":{"containers":[
	   {"name":"app","image":"busybox:1.37.0"%s},{"name":"sidecar","image":"busybox:1.37.0"%s}]%s}}}}`, cc, cc, pc))
}

func scGrade(t testing.TB, doc map[string]any, args map[string]any) model.CheckResult {
	r := newReader().put(deployRef, doc)
	return gradeSecurity(t, grader.SecurityConfig{Reader: r}, scCrit(deployRef, args))
}

func TestSecurityContextDeclaredFields(t *testing.T) {
	hardened := `{"runAsNonRoot":true,"runAsUser":10001,"readOnlyRootFilesystem":true,"allowPrivilegeEscalation":false,
	 "privileged":false,"capabilities":{"drop":["ALL"],"add":["NET_BIND_SERVICE"]},"seccompProfile":{"type":"RuntimeDefault"}}`
	all := func(extra map[string]any) map[string]any {
		c := map[string]any{"run_as_non_root": true, "run_as_user": 10001, "read_only_root_filesystem": true,
			"allow_privilege_escalation": false, "privileged": false, "seccomp": "RuntimeDefault",
			"capabilities": map[string]any{"drop": []any{"NET_RAW"}, "add": []any{"NET_BIND_SERVICE"}}}
		for k, v := range extra {
			c[k] = v
		}
		return map[string]any{"containers": []any{c}}
	}
	cases := []struct {
		name   string
		doc    map[string]any
		args   map[string]any
		want   string
		reason []string
	}{
		{name: "fully hardened container", doc: deployWithSecurity(t, "", hardened), args: all(nil), want: "PASS"},
		{name: "ALL dropped covers a specific drop", doc: deployWithSecurity(t, "", hardened), args: all(map[string]any{"capabilities": map[string]any{"drop": []any{"NET_RAW", "SYS_ADMIN"}}}), want: "PASS"},
		{name: "capability names are normalised", doc: deployWithSecurity(t, "", `{"capabilities":{"drop":["ALL"],"add":["CAP_NET_BIND_SERVICE"]}}`),
			args: map[string]any{"containers": []any{map[string]any{"capabilities": map[string]any{"add": []any{"net_bind_service"}}}}}, want: "PASS"},
		{name: "nothing declared", doc: deployWithSecurity(t, "", ""), args: all(nil), want: "FAIL", reason: []string{"run_as_non_root"}},
		{name: "privilege escalation defaults to allowed", doc: deployWithSecurity(t, "", `{"runAsNonRoot":true}`),
			args: map[string]any{"containers": []any{map[string]any{"allow_privilege_escalation": false}}}, want: "FAIL", reason: []string{"allow_privilege_escalation"}},
		{name: "privileged", doc: deployWithSecurity(t, "", `{"privileged":true}`),
			args: map[string]any{"containers": []any{map[string]any{"privileged": false}}}, want: "FAIL", reason: []string{"privileged"}},
		{name: "extra added capability", doc: deployWithSecurity(t, "", `{"capabilities":{"add":["NET_ADMIN","NET_BIND_SERVICE"]}}`),
			args: map[string]any{"containers": []any{map[string]any{"capabilities": map[string]any{"add": []any{"NET_BIND_SERVICE"}}}}}, want: "FAIL", reason: []string{"capabilities.add", "NET_ADMIN"}},
		{name: "not dropped", doc: deployWithSecurity(t, "", `{"capabilities":{"drop":["NET_RAW"]}}`),
			args: map[string]any{"containers": []any{map[string]any{"capabilities": map[string]any{"drop": []any{"ALL"}}}}}, want: "FAIL", reason: []string{"drop", "ALL"}},
		{name: "pod level value is inherited", doc: deployWithSecurity(t, `{"runAsNonRoot":true,"runAsUser":10001,"seccompProfile":{"type":"RuntimeDefault"}}`, ""),
			args: map[string]any{"containers": []any{map[string]any{"run_as_non_root": true, "run_as_user": 10001, "seccomp": "RuntimeDefault"}}}, want: "PASS"},
		{name: "container overrides the pod", doc: deployWithSecurity(t, `{"runAsNonRoot":true}`, `{"runAsNonRoot":false}`),
			args: map[string]any{"containers": []any{map[string]any{"run_as_non_root": true}}}, want: "FAIL", reason: []string{"run_as_non_root"}},
		{name: "every container must satisfy by default", doc: func() map[string]any {
			d := deployWithSecurity(t, "", "")
			cs := d["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
			cs[0].(map[string]any)["securityContext"] = map[string]any{"runAsNonRoot": true}
			return d
		}(), args: map[string]any{"containers": []any{map[string]any{"run_as_non_root": true}}}, want: "FAIL", reason: []string{"sidecar"}},
		{name: "a named container only", doc: func() map[string]any {
			d := deployWithSecurity(t, "", "")
			cs := d["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
			cs[0].(map[string]any)["securityContext"] = map[string]any{"runAsNonRoot": true}
			return d
		}(), args: map[string]any{"containers": []any{map[string]any{"name": "app", "run_as_non_root": true}}}, want: "PASS"},
		{name: "container that does not exist", doc: deployWithSecurity(t, "", `{"runAsNonRoot":true}`),
			args: map[string]any{"containers": []any{map[string]any{"name": "ghost", "run_as_non_root": true}}}, want: "FAIL", reason: []string{"ghost"}},
		{name: "pod level fields", doc: deployWithSecurity(t, `{"fsGroup":2000,"runAsUser":1000,"supplementalGroups":[3000,4000]}`, ""),
			args: map[string]any{"pod": map[string]any{"fs_group": 2000, "run_as_user": 1000, "supplemental_groups": []any{4000}}}, want: "PASS"},
		{name: "wrong fsGroup", doc: deployWithSecurity(t, `{"fsGroup":1}`, ""), args: map[string]any{"pod": map[string]any{"fs_group": 2000}}, want: "FAIL", reason: []string{"fs_group"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := scGrade(t, tc.doc, tc.args)
			testutil.Equal(t, res.Status, tc.want)
			reasonHas(t, res, tc.reason...)
		})
	}
}

func TestSecurityContextTargets(t *testing.T) {
	cases := map[string]struct {
		ref model.ObjectRef
		doc string
	}{
		"pod":       {refOf("v1", "Pod", "jk-p1", "x"), `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"x","namespace":"jk-p1","uid":"u"},"spec":{"containers":[{"name":"app","image":"i","securityContext":{"runAsNonRoot":true}}]}}`},
		"job":       {refOf("batch/v1", "Job", "jk-p1", "x"), `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"x","namespace":"jk-p1","uid":"u"},"spec":{"template":{"spec":{"containers":[{"name":"app","image":"i","securityContext":{"runAsNonRoot":true}}]}}}}`},
		"cronjob":   {refOf("batch/v1", "CronJob", "jk-p1", "x"), `{"apiVersion":"batch/v1","kind":"CronJob","metadata":{"name":"x","namespace":"jk-p1","uid":"u"},"spec":{"jobTemplate":{"spec":{"template":{"spec":{"containers":[{"name":"app","image":"i","securityContext":{"runAsNonRoot":true}}]}}}}}}`},
		"daemonset": {refOf("apps/v1", "DaemonSet", "jk-p1", "x"), `{"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"x","namespace":"jk-p1","uid":"u"},"spec":{"template":{"spec":{"containers":[{"name":"app","image":"i","securityContext":{"runAsNonRoot":true}}]}}}}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newReader().put(tc.ref, obj(t, tc.doc))
			res := gradeSecurity(t, grader.SecurityConfig{Reader: r}, scCrit(tc.ref, map[string]any{"containers": []any{map[string]any{"run_as_non_root": true}}}))
			testutil.Equal(t, res.Status, "PASS")
		})
	}
	t.Run("missing target", func(t *testing.T) {
		res := gradeSecurity(t, grader.SecurityConfig{Reader: newReader()}, scCrit(deployRef, map[string]any{"containers": []any{map[string]any{"run_as_non_root": true}}}))
		testutil.Equal(t, res.Status, "FAIL")
	})
}

// capHex renders a capability set as /proc/self/status shows it.
func capHex(bits ...uint) string {
	var v uint64
	for _, b := range bits {
		v |= 1 << b
	}
	return fmt.Sprintf("%016x", v)
}

func procStatus(capEff string) string {
	return procStatusBnd(capEff, capEff)
}

func procStatusBnd(capEff, capBnd string) string {
	return fmt.Sprintf("Name:\tapp\nUid:\t1000\t1000\t1000\t1000\nCapInh:\t0000000000000000\nCapPrm:\t%s\nCapEff:\t%s\nCapBnd:\t%s\nCapAmb:\t0000000000000000\nSeccomp:\t2\n", capEff, capEff, capBnd)
}

const mountsRW = "22 1 0:21 / / rw,relatime - overlay overlay rw,lowerdir=/x\n23 22 0:5 / /proc rw - proc proc rw\n"
const mountsRO = "22 1 0:21 / / ro,relatime - overlay overlay ro,lowerdir=/x\n23 22 0:5 / /proc rw - proc proc rw\n"

func behaviorPods(uid, gid, capEff, mounts string) *fakePods {
	p := newPods()
	p.execFn = func(key string, cmd []string) (grader.ExecResult, error) {
		switch strings.Join(cmd, " ") {
		case "id -u":
			return grader.ExecResult{Stdout: uid + "\n"}, nil
		case "id -g":
			return grader.ExecResult{Stdout: gid + "\n"}, nil
		case "cat /proc/self/status":
			return grader.ExecResult{Stdout: procStatus(capEff)}, nil
		case "cat /proc/self/mountinfo":
			return grader.ExecResult{Stdout: mounts}, nil
		}
		return grader.ExecResult{}, fmt.Errorf("unexpected command %v", cmd)
	}
	return p
}

func TestSecurityContextBehavior(t *testing.T) {
	const netBindService, netRaw, setuid = 10, 13, 7
	podRef := refOf("v1", "Pod", "jk-p1", "app")
	run := func(pods *fakePods, behavior map[string]any) model.CheckResult {
		r := newReader().put(podRef, podAs(t, "app", "default"))
		return gradeSecurity(t, grader.SecurityConfig{Reader: r, Pods: pods}, scCrit(podRef, map[string]any{"behavior": behavior}))
	}
	cases := []struct {
		name     string
		pods     *fakePods
		behavior map[string]any
		want     string
		reason   []string
	}{
		{name: "runs as the declared user", pods: behaviorPods("10001", "10001", capHex(), mountsRW), behavior: map[string]any{"uid": 10001, "gid": 10001}, want: "PASS"},
		{name: "runs as root although non-root was expected", pods: behaviorPods("0", "0", capHex(), mountsRW), behavior: map[string]any{"uid": 10001}, want: "FAIL", reason: []string{"uid 0", "10001"}},
		{name: "must not be root", pods: behaviorPods("0", "0", capHex(), mountsRW), behavior: map[string]any{"non_root": true}, want: "FAIL", reason: []string{"root"}},
		{name: "non root", pods: behaviorPods("1000", "1000", capHex(), mountsRW), behavior: map[string]any{"non_root": true}, want: "PASS"},
		{name: "wrong gid", pods: behaviorPods("1000", "0", capHex(), mountsRW), behavior: map[string]any{"gid": 1000}, want: "FAIL", reason: []string{"gid 0"}},
		{name: "capabilities dropped", pods: behaviorPods("1000", "1000", capHex(netBindService), mountsRW),
			behavior: map[string]any{"caps_absent": []any{"NET_RAW", "CAP_SETUID"}, "caps_present": []any{"NET_BIND_SERVICE"}}, want: "PASS"},
		{name: "non-root keeps an added capability only in the bounding set", pods: func() *fakePods {
			p := behaviorPods("10001", "10001", capHex(), mountsRW)
			inner := p.execFn
			p.execFn = func(key string, cmd []string) (grader.ExecResult, error) {
				if strings.Join(cmd, " ") == "cat /proc/self/status" {
					return grader.ExecResult{Stdout: procStatusBnd(capHex(), capHex(netBindService))}, nil
				}
				return inner(key, cmd)
			}
			return p
		}(), behavior: map[string]any{"caps_present": []any{"NET_BIND_SERVICE"}, "caps_absent": []any{"NET_RAW"}, "no_capabilities": false}, want: "PASS"},
		{name: "capability still effective", pods: behaviorPods("1000", "1000", capHex(netRaw, setuid), mountsRW),
			behavior: map[string]any{"caps_absent": []any{"NET_RAW"}}, want: "FAIL", reason: []string{"NET_RAW", "still available"}},
		{name: "no effective capability at all", pods: behaviorPods("1000", "1000", capHex(), mountsRW), behavior: map[string]any{"no_capabilities": true}, want: "PASS"},
		{name: "some effective capability", pods: behaviorPods("1000", "1000", capHex(netBindService), mountsRW), behavior: map[string]any{"no_capabilities": true}, want: "FAIL", reason: []string{"NET_BIND_SERVICE"}},
		{name: "read-only root filesystem", pods: behaviorPods("1000", "1000", capHex(), mountsRO), behavior: map[string]any{"read_only_root": true}, want: "PASS"},
		{name: "writable root filesystem", pods: behaviorPods("1000", "1000", capHex(), mountsRW), behavior: map[string]any{"read_only_root": true}, want: "FAIL", reason: []string{"writable"}},
		{name: "exec failure is an error", pods: func() *fakePods {
			p := newPods()
			p.execFn = func(string, []string) (grader.ExecResult, error) {
				return grader.ExecResult{}, fmt.Errorf("executable file not found in $PATH")
			}
			return p
		}(), behavior: map[string]any{"uid": 1}, want: "ERROR"},
		{name: "command that exits non-zero is an error", pods: func() *fakePods {
			p := newPods()
			p.execFn = func(string, []string) (grader.ExecResult, error) {
				return grader.ExecResult{ExitCode: 126, Stderr: "id: not found"}, nil
			}
			return p
		}(), behavior: map[string]any{"uid": 1}, want: "ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := run(tc.pods, tc.behavior)
			testutil.Equal(t, res.Status, tc.want)
			reasonHas(t, res, tc.reason...)
		})
	}
	t.Run("workload pod is chosen from its own pods", func(t *testing.T) {
		pods := behaviorPods("10001", "10001", capHex(), mountsRW)
		r := deployLogReader(t, podOwned(t, "web-a", "default"))
		res := gradeSecurity(t, grader.SecurityConfig{Reader: r, Pods: pods},
			scCrit(deployRef, map[string]any{"behavior": map[string]any{"uid": 10001}}))
		testutil.Equal(t, res.Status, "PASS")
		testutil.Equal(t, len(pods.ran) > 0, true)
	})
	t.Run("pod that is not running cannot be exec'd", func(t *testing.T) {
		pod := podAs(t, "app", "default")
		pod["status"] = map[string]any{"phase": "Pending"}
		pods := behaviorPods("1", "1", capHex(), mountsRW)
		res := gradeSecurity(t, grader.SecurityConfig{Reader: newReader().put(podRef, pod), Pods: pods}, scCrit(podRef, map[string]any{"behavior": map[string]any{"uid": 1}}))
		testutil.Equal(t, res.Status, "ERROR")
		testutil.Equal(t, len(pods.ran), 0)
	})
	t.Run("no pod access configured", func(t *testing.T) {
		res := gradeSecurity(t, grader.SecurityConfig{Reader: newReader().put(podRef, podAs(t, "app", "default"))}, scCrit(podRef, map[string]any{"behavior": map[string]any{"uid": 1}}))
		testutil.Equal(t, res.Status, "ERROR")
	})
	t.Run("declared and observed together", func(t *testing.T) {
		pod := podAs(t, "app", "default")
		pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["securityContext"] = map[string]any{"runAsUser": int64(10001)}
		pods := behaviorPods("0", "0", capHex(), mountsRW)
		// The spec says 10001 but the process is root: the declaration alone
		// would pass, the behaviour does not.
		res := gradeSecurity(t, grader.SecurityConfig{Reader: newReader().put(podRef, pod), Pods: pods}, scCrit(podRef,
			map[string]any{"containers": []any{map[string]any{"run_as_user": 10001}}, "behavior": map[string]any{"uid": 10001}}))
		testutil.Equal(t, res.Status, "FAIL")
		reasonHas(t, res, "uid 0")
	})
}

func TestSecurityContextArgsAreStrict(t *testing.T) {
	ck := grader.NewSecurityChecker(grader.SecurityConfig{})
	one := []any{map[string]any{"run_as_non_root": true}}
	bad := map[string]model.Criterion{
		"unknown key":                  scCrit(deployRef, map[string]any{"containers": one, "bogus": 1}),
		"nothing to check":             scCrit(deployRef, map[string]any{}),
		"empty container":              scCrit(deployRef, map[string]any{"containers": []any{map[string]any{}}}),
		"unknown container key":        scCrit(deployRef, map[string]any{"containers": []any{map[string]any{"bogus": true}}}),
		"bad seccomp":                  scCrit(deployRef, map[string]any{"containers": []any{map[string]any{"seccomp": "Magic"}}}),
		"negative uid":                 scCrit(deployRef, map[string]any{"containers": []any{map[string]any{"run_as_user": -1}}}),
		"empty behavior":               scCrit(deployRef, map[string]any{"behavior": map[string]any{}}),
		"unknown capability":           scCrit(deployRef, map[string]any{"behavior": map[string]any{"caps_absent": []any{"MAGIC"}}}),
		"non_root with uid":            scCrit(deployRef, map[string]any{"behavior": map[string]any{"non_root": true, "uid": 5}}),
		"no_capabilities with present": scCrit(deployRef, map[string]any{"behavior": map[string]any{"no_capabilities": true, "caps_present": []any{"CHOWN"}}}),
		"unsupported target":           scCrit(refOf("v1", "ConfigMap", "ns", "x"), map[string]any{"containers": one}),
		"no target":                    scCrit(model.ObjectRef{}, map[string]any{"containers": one}),
	}
	for name, c := range bad {
		if err := ck.ValidateCriterion(c); !isInvalid(err) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	testutil.NoError(t, ck.ValidateCriterion(scCrit(deployRef, map[string]any{"containers": one})))
}
