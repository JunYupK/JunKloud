package grader

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/JunYupK/JunKloud/internal/model"
)

// securityContextArgs are the Args of a "security-context" criterion. Target
// is a Pod or a workload with a pod template (Deployment, StatefulSet,
// DaemonSet, ReplicaSet, Job, CronJob).
//
//	containers: [ {name?, init?, run_as_user?, run_as_group?, run_as_non_root?,
//	               read_only_root_filesystem?, allow_privilege_escalation?,
//	               privileged?, capabilities? {drop?: [..], add?: [..]},
//	               seccomp? RuntimeDefault|Localhost|Unconfined} ]
//	    EFFECTIVE values, compared semantically: a container-level field
//	    overrides the pod-level one; allowPrivilegeEscalation defaults to true
//	    and privileged to false as the API does; capabilities.drop must be
//	    dropped (ALL covers any), capabilities.add is the exact set added;
//	    names are case-insensitive with or without CAP_. Without name the
//	    entry applies to every regular container (and init containers when
//	    init is true).
//	pod:        declared pod-level {run_as_user?, run_as_group?, run_as_non_root?,
//	            fs_group?, supplemental_groups? (each must be listed), seccomp?}
//	behavior:   what a running pod does, read by exec (no shell):
//	            {container?, uid?, gid?, non_root?, caps_absent?, caps_present?,
//	            no_capabilities?, read_only_root?}. `id -u`, `id -g`,
//	            /proc/self/status (CapEff, CapBnd) and /proc/self/mountinfo are read
//	            from up to 3 running pods of the target; a pod that cannot be
//	            examined makes the criterion ERROR.
//
// caps_present / caps_absent are judged on the bounding set (CapBnd, what the
// process can ever hold) together with the effective set: a non-root process
// has an empty effective set even when the pod adds NET_BIND_SERVICE, and
// dropping a capability removes it from the bounding set. no_capabilities
// means nothing is effective now (CapEff is empty).
//
// A declared field does not prove behaviour: give both when the problem
// cares about the process, not the manifest.
type securityContextArgs struct {
	Containers []scContainer `json:"containers"`
	Pod        *scPod        `json:"pod"`
	Behavior   *scBehavior   `json:"behavior"`
}

type scContainer struct {
	Name                     string  `json:"name"`
	Init                     bool    `json:"init"`
	RunAsUser                *int64  `json:"run_as_user"`
	RunAsGroup               *int64  `json:"run_as_group"`
	RunAsNonRoot             *bool   `json:"run_as_non_root"`
	ReadOnlyRootFilesystem   *bool   `json:"read_only_root_filesystem"`
	AllowPrivilegeEscalation *bool   `json:"allow_privilege_escalation"`
	Privileged               *bool   `json:"privileged"`
	Capabilities             *scCaps `json:"capabilities"`
	Seccomp                  *string `json:"seccomp"`
}

type scCaps struct {
	Drop []string  `json:"drop"`
	Add  *[]string `json:"add"`
}

type scPod struct {
	RunAsUser          *int64  `json:"run_as_user"`
	RunAsGroup         *int64  `json:"run_as_group"`
	RunAsNonRoot       *bool   `json:"run_as_non_root"`
	FSGroup            *int64  `json:"fs_group"`
	SupplementalGroups []int64 `json:"supplemental_groups"`
	Seccomp            *string `json:"seccomp"`
}

type scBehavior struct {
	Container      string   `json:"container"`
	UID            *int64   `json:"uid"`
	GID            *int64   `json:"gid"`
	NonRoot        bool     `json:"non_root"`
	CapsAbsent     []string `json:"caps_absent"`
	CapsPresent    []string `json:"caps_present"`
	NoCapabilities bool     `json:"no_capabilities"`
	ReadOnlyRoot   *bool    `json:"read_only_root"`
}

// podSpecPath is where each target kind keeps its pod spec.
var podSpecPath = map[string][]string{
	"Pod":         {"spec"},
	"Deployment":  {"spec", "template", "spec"},
	"StatefulSet": {"spec", "template", "spec"},
	"DaemonSet":   {"spec", "template", "spec"},
	"ReplicaSet":  {"spec", "template", "spec"},
	"Job":         {"spec", "template", "spec"},
	"CronJob":     {"spec", "jobTemplate", "spec", "template", "spec"},
}

var seccompTypes = map[string]bool{"RuntimeDefault": true, "Localhost": true, "Unconfined": true}

// capabilityNames is Linux's capability numbering (capability.h).
var capabilityNames = []string{
	"CHOWN", "DAC_OVERRIDE", "DAC_READ_SEARCH", "FOWNER", "FSETID", "KILL", "SETGID", "SETUID", "SETPCAP",
	"LINUX_IMMUTABLE", "NET_BIND_SERVICE", "NET_BROADCAST", "NET_ADMIN", "NET_RAW", "IPC_LOCK", "IPC_OWNER",
	"SYS_MODULE", "SYS_RAWIO", "SYS_CHROOT", "SYS_PTRACE", "SYS_PACCT", "SYS_ADMIN", "SYS_BOOT", "SYS_NICE",
	"SYS_RESOURCE", "SYS_TIME", "SYS_TTY_CONFIG", "MKNOD", "LEASE", "AUDIT_WRITE", "AUDIT_CONTROL", "SETFCAP",
	"MAC_OVERRIDE", "MAC_ADMIN", "SYSLOG", "WAKE_ALARM", "BLOCK_SUSPEND", "AUDIT_READ", "PERFMON", "BPF", "CHECKPOINT_RESTORE",
}

func normalizeCap(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	return strings.TrimPrefix(s, "CAP_")
}

func knownCap(s string) bool {
	for _, n := range capabilityNames {
		if n == s {
			return true
		}
	}
	return false
}

func parseSecurityContextArgs(crit model.Criterion) (securityContextArgs, error) {
	var a securityContextArgs
	if err := checkRef(crit.Target); err != nil {
		return a, err
	}
	if _, ok := podSpecPath[crit.Target.Kind]; !ok {
		return a, invalidf("security-context cannot read %s: target must be a Pod, Deployment, StatefulSet, DaemonSet, ReplicaSet, Job or CronJob", crit.Target.Kind)
	}
	if err := decodeArgs(crit.Args, &a); err != nil {
		return a, err
	}
	if len(a.Containers) == 0 && a.Pod == nil && a.Behavior == nil {
		return a, invalidf("security-context needs args.containers, args.pod or args.behavior")
	}
	for i := range a.Containers {
		c := &a.Containers[i]
		bad := func(f string, v ...any) error { return invalidf("args.containers[%d]: %s", i, fmt.Sprintf(f, v...)) }
		if c.RunAsUser == nil && c.RunAsGroup == nil && c.RunAsNonRoot == nil && c.ReadOnlyRootFilesystem == nil &&
			c.AllowPrivilegeEscalation == nil && c.Privileged == nil && c.Capabilities == nil && c.Seccomp == nil {
			return a, bad("no setting to check")
		}
		for name, v := range map[string]*int64{"run_as_user": c.RunAsUser, "run_as_group": c.RunAsGroup} {
			if v != nil && *v < 0 {
				return a, bad("%s cannot be negative", name)
			}
		}
		if c.Seccomp != nil && !seccompTypes[*c.Seccomp] {
			return a, bad("seccomp %q is not RuntimeDefault, Localhost or Unconfined", *c.Seccomp)
		}
		if c.Capabilities != nil {
			c.Capabilities.Drop = normalizeCaps(c.Capabilities.Drop)
			if c.Capabilities.Add != nil {
				n := normalizeCaps(*c.Capabilities.Add)
				c.Capabilities.Add = &n
			}
			for _, n := range append(append([]string(nil), c.Capabilities.Drop...), derefStrings(c.Capabilities.Add)...) {
				if n != "ALL" && !knownCap(n) {
					return a, bad("unknown capability %q", n)
				}
			}
		}
	}
	if p := a.Pod; p != nil {
		if p.RunAsUser == nil && p.RunAsGroup == nil && p.RunAsNonRoot == nil && p.FSGroup == nil && len(p.SupplementalGroups) == 0 && p.Seccomp == nil {
			return a, invalidf("args.pod has no setting to check")
		}
		if p.Seccomp != nil && !seccompTypes[*p.Seccomp] {
			return a, invalidf("args.pod.seccomp %q is not RuntimeDefault, Localhost or Unconfined", *p.Seccomp)
		}
	}
	if b := a.Behavior; b != nil {
		if b.UID == nil && b.GID == nil && !b.NonRoot && len(b.CapsAbsent) == 0 && len(b.CapsPresent) == 0 && !b.NoCapabilities && b.ReadOnlyRoot == nil {
			return a, invalidf("args.behavior has nothing to check")
		}
		if b.NonRoot && b.UID != nil {
			return a, invalidf("args.behavior: give non_root or uid, not both")
		}
		if b.NoCapabilities && len(b.CapsPresent) > 0 {
			return a, invalidf("args.behavior: no_capabilities excludes caps_present")
		}
		b.CapsAbsent, b.CapsPresent = normalizeCaps(b.CapsAbsent), normalizeCaps(b.CapsPresent)
		for _, n := range append(append([]string(nil), b.CapsAbsent...), b.CapsPresent...) {
			if !knownCap(n) {
				return a, invalidf("args.behavior: unknown capability %q", n)
			}
		}
	}
	return a, nil
}

func normalizeCaps(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = normalizeCap(s)
	}
	return out
}

func derefStrings(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}

func (c *SecurityChecker) checkSecurityContext(ctx context.Context, t Target, crit model.Criterion) (model.CheckResult, error) {
	a, err := parseSecurityContextArgs(crit)
	if err != nil {
		return model.CheckResult{}, err
	}
	if c.cfg.Reader == nil {
		return model.CheckResult{}, notConfigured("security-context checks need an object reader")
	}
	if a.Behavior != nil && c.cfg.Pods == nil {
		return model.CheckResult{}, notConfigured("behaviour checks need pod access (exec)")
	}
	ref, err := resolveTarget(t, crit)
	if err != nil {
		return model.CheckResult{}, err
	}
	start := c.set.clock.Now()
	expected := map[string]any{"target": describeRef(ref)}
	obj, err := c.obs.get(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return verdictAt(c.set, start, false, fmt.Sprintf("%s %q not found", ref.Kind, ref.Name), expected, map[string]any{"exists": false}), nil
	}
	if err != nil {
		return model.CheckResult{}, err
	}
	actual := map[string]any{}
	var failures []string

	podSpec := mapAt(obj, podSpecPath[ref.Kind]...)
	if len(a.Containers) > 0 || a.Pod != nil {
		failures = append(failures, declaredFailures(podSpec, a, actual)...)
		expected["declared"] = true
	}
	if a.Behavior != nil {
		expected["behavior"] = a.Behavior
		bf, info, err := c.checkBehavior(ctx, ref, obj, a.Behavior)
		if err != nil {
			return model.CheckResult{}, err
		}
		actual["behavior"] = info
		failures = append(failures, bf...)
	}
	res := verdictAt(c.set, start, len(failures) == 0, strings.Join(failures, "; "), expected, actual)
	res.Evidence.UID, res.Evidence.Generation, res.Evidence.InputHash = uidOf(obj), generationOf(obj), inputHash(obj)
	return res, nil
}

// ------------------------------------------------------------- declared

func boolAt(m map[string]any, key string) (bool, bool) { v, ok := m[key].(bool); return v, ok }

func intAt(m map[string]any, key string) (int64, bool) {
	if r, ok := ratOf(m[key]); ok && r.IsInt() {
		return r.Num().Int64(), true
	}
	return 0, false
}

func declaredFailures(podSpec map[string]any, a securityContextArgs, actual map[string]any) []string {
	podCtx := asMap(podSpec["securityContext"])
	var failures []string
	if p := a.Pod; p != nil {
		add := func(f string, v ...any) { failures = append(failures, "pod: "+fmt.Sprintf(f, v...)) }
		wantInt := func(name string, want *int64, key string) {
			if want == nil {
				return
			}
			if got, ok := intAt(podCtx, key); !ok || got != *want {
				add("%s is %s, want %d", name, intText(podCtx, key), *want)
			}
		}
		wantInt("run_as_user", p.RunAsUser, "runAsUser")
		wantInt("run_as_group", p.RunAsGroup, "runAsGroup")
		wantInt("fs_group", p.FSGroup, "fsGroup")
		if p.RunAsNonRoot != nil {
			if got, _ := boolAt(podCtx, "runAsNonRoot"); got != *p.RunAsNonRoot {
				add("run_as_non_root is %t, want %t", got, *p.RunAsNonRoot)
			}
		}
		for _, g := range p.SupplementalGroups {
			found := false
			for _, e := range listAt(podCtx, "supplementalGroups") {
				if r, ok := ratOf(e); ok && r.IsInt() && r.Num().Int64() == g {
					found = true
				}
			}
			if !found {
				add("supplemental_groups lacks %d", g)
			}
		}
		if p.Seccomp != nil && seccompOf(podCtx, nil) != *p.Seccomp {
			add("seccomp is %q, want %q", seccompOf(podCtx, nil), *p.Seccomp)
		}
	}
	checked := 0
	names := map[string]bool{}
	for _, group := range []struct {
		key  string
		init bool
	}{{"containers", false}, {"initContainers", true}} {
		for _, raw := range listAt(podSpec, group.key) {
			cm, _ := raw.(map[string]any)
			name, _ := cm["name"].(string)
			names[name] = true
			for _, want := range a.Containers {
				if (want.Name != "" && want.Name != name) || (want.Name == "" && group.init && !want.Init) {
					continue
				}
				checked++
				failures = append(failures, containerFailures(name, cm, podCtx, want)...)
			}
		}
	}
	for _, want := range a.Containers {
		if want.Name != "" && !names[want.Name] {
			failures = append(failures, fmt.Sprintf("container %q does not exist", want.Name))
		}
	}
	if len(a.Containers) > 0 {
		actual["containers_checked"] = checked
	}
	return failures
}

func intText(m map[string]any, key string) string {
	if v, ok := intAt(m, key); ok {
		return strconv.FormatInt(v, 10)
	}
	return "unset"
}

func seccompOf(podCtx, ctrCtx map[string]any) string {
	if t, _ := asMap(ctrCtx["seccompProfile"])["type"].(string); t != "" {
		return t
	}
	t, _ := asMap(podCtx["seccompProfile"])["type"].(string)
	if t == "" {
		return "unset"
	}
	return t
}

func containerFailures(name string, c, podCtx map[string]any, want scContainer) []string {
	ctr := asMap(c["securityContext"])
	var failures []string
	add := func(f string, v ...any) { failures = append(failures, "container "+name+": "+fmt.Sprintf(f, v...)) }
	effInt := func(key string) (int64, bool) {
		if v, ok := intAt(ctr, key); ok {
			return v, true
		}
		return intAt(podCtx, key)
	}
	for _, w := range []struct {
		name string
		want *int64
		key  string
	}{{"run_as_user", want.RunAsUser, "runAsUser"}, {"run_as_group", want.RunAsGroup, "runAsGroup"}} {
		if w.want == nil {
			continue
		}
		if got, ok := effInt(w.key); !ok || got != *w.want {
			txt := "unset"
			if ok {
				txt = strconv.FormatInt(got, 10)
			}
			add("%s is %s, want %d", w.name, txt, *w.want)
		}
	}
	if want.RunAsNonRoot != nil {
		got, ok := boolAt(ctr, "runAsNonRoot")
		if !ok {
			got, _ = boolAt(podCtx, "runAsNonRoot")
		}
		if got != *want.RunAsNonRoot {
			add("run_as_non_root is %t, want %t", got, *want.RunAsNonRoot)
		}
	}
	if want.ReadOnlyRootFilesystem != nil {
		if got, _ := boolAt(ctr, "readOnlyRootFilesystem"); got != *want.ReadOnlyRootFilesystem {
			add("read_only_root_filesystem is %t, want %t", got, *want.ReadOnlyRootFilesystem)
		}
	}
	privileged, _ := boolAt(ctr, "privileged")
	if want.Privileged != nil && privileged != *want.Privileged {
		add("privileged is %t, want %t", privileged, *want.Privileged)
	}
	if want.AllowPrivilegeEscalation != nil {
		got, ok := boolAt(ctr, "allowPrivilegeEscalation")
		if !ok || privileged {
			got = true // the API default; a privileged container always may
		}
		if got != *want.AllowPrivilegeEscalation {
			add("allow_privilege_escalation is %t, want %t", got, *want.AllowPrivilegeEscalation)
		}
	}
	if want.Seccomp != nil {
		if got := seccompOf(podCtx, ctr); got != *want.Seccomp {
			add("seccomp is %q, want %q", got, *want.Seccomp)
		}
	}
	if caps := want.Capabilities; caps != nil {
		have := asMap(ctr["capabilities"])
		dropped := capSet(have["drop"])
		for _, d := range caps.Drop {
			if !dropped["ALL"] && !dropped[d] {
				add("capabilities.drop lacks %s (dropped: %s)", d, setText(dropped))
			}
		}
		if caps.Add != nil {
			added := capSet(have["add"])
			wantSet := map[string]bool{}
			for _, n := range *caps.Add {
				wantSet[n] = true
			}
			if setText(added) != setText(wantSet) {
				add("capabilities.add is [%s], want [%s]", setText(added), setText(wantSet))
			}
		}
	}
	return failures
}

func capSet(v any) map[string]bool {
	out := map[string]bool{}
	l, _ := v.([]any)
	for _, e := range l {
		if s, ok := e.(string); ok {
			out[normalizeCap(s)] = true
		}
	}
	return out
}

func setText(s map[string]bool) string {
	var l []string
	for k := range s {
		l = append(l, k)
	}
	sort.Strings(l)
	return strings.Join(l, " ")
}

// ------------------------------------------------------------- behaviour

type procFacts struct {
	uid, gid int64
	capEff   uint64
	capBnd   uint64
	rootRO   bool
}

func (c *SecurityChecker) checkBehavior(ctx context.Context, ref model.ObjectRef, obj object, b *scBehavior) ([]string, []any, error) {
	pods, err := ownedPods(ctx, c.cfg.Reader, ref.Kind, obj)
	if err != nil {
		return nil, nil, err
	}
	running := runningPods(pods)
	if len(running) == 0 {
		reason, err := c.explainNoRunningPods(ctx, pods, ref.Kind+" "+ref.Name)
		if err != nil {
			return nil, nil, err
		}
		return []string{reason}, nil, nil
	}
	var failures []string
	var info []any
	for _, p := range running[:min(len(running), maxBehaviorPods)] {
		container, ok := pickContainer(p, b.Container)
		if !ok {
			failures = append(failures, fmt.Sprintf("pod %s has no container %q", nameOf(p), b.Container))
			continue
		}
		facts, err := c.probePod(ctx, p, container, b)
		if err != nil {
			return nil, nil, err
		}
		entry := map[string]any{"pod": nameOf(p)}
		if b.UID != nil || b.NonRoot {
			entry["uid"] = facts.uid
		}
		if b.GID != nil {
			entry["gid"] = facts.gid
		}
		if needsCaps(b) {
			entry["effective_capabilities"] = capNames(facts.capEff)
			entry["bounding_capabilities"] = capNames(facts.capBnd)
		}
		if b.ReadOnlyRoot != nil {
			entry["root_read_only"] = facts.rootRO
		}
		info = append(info, entry)
		failures = append(failures, behaviorFailures(nameOf(p), facts, b)...)
	}
	return failures, info, nil
}

func needsCaps(b *scBehavior) bool {
	return len(b.CapsAbsent) > 0 || len(b.CapsPresent) > 0 || b.NoCapabilities
}

// exec runs one command; a command that cannot run or fails is not a
// verdict on the learner (it may be a minimal image): insufficient evidence.
func (c *SecurityChecker) exec(ctx context.Context, p object, container string, cmd ...string) (string, error) {
	res, err := c.cfg.Pods.Exec(ctx, stringAt(p, "metadata", "namespace"), nameOf(p), container, cmd)
	if err != nil {
		return "", fmt.Errorf("%w: cannot run %q in pod %s (the image needs it): %v", ErrInsufficientEvidence, cmd[0], nameOf(p), err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("%w: %q in pod %s exited with %d: %s", ErrInsufficientEvidence, strings.Join(cmd, " "), nameOf(p), res.ExitCode, shortenLine(res.Stderr))
	}
	return res.Stdout, nil
}

func (c *SecurityChecker) probePod(ctx context.Context, p object, container string, b *scBehavior) (procFacts, error) {
	var f procFacts
	num := func(cmd ...string) (int64, error) {
		out, err := c.exec(ctx, p, container, cmd...)
		if err != nil {
			return 0, err
		}
		n, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		if perr != nil {
			return 0, fmt.Errorf("%w: %q in pod %s printed %q", ErrInsufficientEvidence, strings.Join(cmd, " "), nameOf(p), shortenLine(out))
		}
		return n, nil
	}
	var err error
	if b.UID != nil || b.NonRoot {
		if f.uid, err = num("id", "-u"); err != nil {
			return f, err
		}
	}
	if b.GID != nil {
		if f.gid, err = num("id", "-g"); err != nil {
			return f, err
		}
	}
	if needsCaps(b) {
		out, err := c.exec(ctx, p, container, "cat", "/proc/self/status")
		if err != nil {
			return f, err
		}
		if f.capEff, f.capBnd, err = parseCaps(out); err != nil {
			return f, fmt.Errorf("%w: pod %s: %v", ErrInsufficientEvidence, nameOf(p), err)
		}
	}
	if b.ReadOnlyRoot != nil {
		out, err := c.exec(ctx, p, container, "cat", "/proc/self/mountinfo")
		if err != nil {
			return f, err
		}
		ro, err := rootIsReadOnly(out)
		if err != nil {
			return f, fmt.Errorf("%w: pod %s: %v", ErrInsufficientEvidence, nameOf(p), err)
		}
		f.rootRO = ro
	}
	return f, nil
}

// parseCaps reads CapEff (effective now) and CapBnd (bounding: what the
// process can ever acquire) from /proc/self/status.
func parseCaps(status string) (eff, bnd uint64, err error) {
	found := 0
	for _, line := range strings.Split(status, "\n") {
		for _, f := range []struct {
			prefix string
			dst    *uint64
		}{{"CapEff:", &eff}, {"CapBnd:", &bnd}} {
			if v, ok := strings.CutPrefix(line, f.prefix); ok {
				n, perr := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
				if perr != nil {
					return 0, 0, fmt.Errorf("unreadable %s %q", strings.TrimSuffix(f.prefix, ":"), strings.TrimSpace(v))
				}
				*f.dst = n
				found++
			}
		}
	}
	if found != 2 {
		return 0, 0, errors.New("/proc/self/status lacks CapEff or CapBnd")
	}
	return eff, bnd, nil
}

func capNames(set uint64) []string {
	var out []string
	for bit := 0; bit < 64; bit++ {
		if set&(1<<bit) == 0 {
			continue
		}
		if bit < len(capabilityNames) {
			out = append(out, capabilityNames[bit])
		} else {
			out = append(out, "CAP_"+strconv.Itoa(bit))
		}
	}
	return out
}

func capBit(name string) uint64 {
	for i, n := range capabilityNames {
		if n == name {
			return 1 << i
		}
	}
	return 0
}

// rootIsReadOnly reads the mount options of "/" from mountinfo.
func rootIsReadOnly(mountinfo string) (bool, error) {
	for _, line := range strings.Split(mountinfo, "\n") {
		f := strings.Fields(line)
		if len(f) > 5 && f[4] == "/" {
			for _, o := range strings.Split(f[5], ",") {
				if o == "ro" {
					return true, nil
				}
			}
			return false, nil
		}
	}
	return false, errors.New("/proc/self/mountinfo has no entry for /")
}

func behaviorFailures(pod string, f procFacts, b *scBehavior) []string {
	var failures []string
	add := func(s string, v ...any) { failures = append(failures, "pod "+pod+": "+fmt.Sprintf(s, v...)) }
	if b.UID != nil && f.uid != *b.UID {
		add("the process runs as uid %d, want %d", f.uid, *b.UID)
	}
	if b.NonRoot && f.uid == 0 {
		add("the process runs as root (uid 0)")
	}
	if b.GID != nil && f.gid != *b.GID {
		add("the process runs with gid %d, want %d", f.gid, *b.GID)
	}
	for _, n := range b.CapsAbsent {
		if (f.capEff|f.capBnd)&capBit(n) != 0 {
			add("capability %s is still available to the process", n)
		}
	}
	for _, n := range b.CapsPresent {
		if f.capBnd&capBit(n) == 0 {
			add("capability %s is not available to the process", n)
		}
	}
	if b.NoCapabilities && f.capEff != 0 {
		add("capabilities are still effective: %s", strings.Join(capNames(f.capEff), ", "))
	}
	if b.ReadOnlyRoot != nil && f.rootRO != *b.ReadOnlyRoot {
		if *b.ReadOnlyRoot {
			add("the root filesystem is writable")
		} else {
			add("the root filesystem is read-only")
		}
	}
	return failures
}
