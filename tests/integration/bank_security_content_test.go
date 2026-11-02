//go:build linux

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/testutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

var securityIDs = []string{"custom-resource-001", "access-001", "resources-001", "configuration-001", "security-001"}

// securityCases maps problem ID -> case ID -> expected score. Each package adds
// its row with its task; the table pins the exact case set of each package.
var securityCases = map[string]map[string]int{
	"custom-resource-001": {
		"configmap-substitute":     1,
		"missing-answer":           4,
		"wrong-target":             4,
		"wrong-retention":          4,
		"alternative-extra-fields": 5,
	},
	"access-001": {
		"bound-to-default":        1,
		"edit-rolebinding":        4,
		"cluster-wide-binding":    4,
		"sa-not-used":             4,
		"alternative-split-roles": 5,
	},
}

// securityCapabilities and securityInitialScores pin each package's declared
// capabilities and the score of the untouched environment.
var securityCapabilities = map[string][]string{
	"custom-resource-001": nil,
	"access-001":          nil,
}

var securityInitialScores = map[string]int{
	"custom-resource-001": 0,
	"access-001":          2,
}

// presentSecurityIDs returns the security IDs whose package exists, so the
// bank tests grow with the packages instead of failing before they are added.
func presentSecurityIDs(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, id := range securityIDs {
		_, err := os.Stat(filepath.Join(bankRoot, "problems", id))
		switch {
		case err == nil:
			out = append(out, id)
		case !os.IsNotExist(err):
			t.Fatal(err)
		}
	}
	return out
}

// A row in the case table without a package is a package that was announced
// but never authored.
func TestSecurityTablesHavePackages(t *testing.T) {
	present := presentSecurityIDs(t)
	for id := range securityCases {
		if !slices.Contains(present, id) {
			t.Errorf("securityCases has a row for %s but problems/%s does not exist", id, id)
		}
	}
}

func TestSecurityProblems(t *testing.T) {
	for _, p := range loadGroup(t, presentSecurityIDs(t)) {
		t.Run(p.ID, func(t *testing.T) {
			testutil.Equal(t, p.Domain, model.DomainEnvironmentSecurity)
			testutil.Equal(t, sumPoints([]model.Problem{p}), 5)
			if p.Difficulty == "" || p.Type == "" {
				t.Fatal("explicit difficulty/type required")
			}
			seeds, err := author.CoveringSeeds(p)
			testutil.NoError(t, err)
			testutil.Equal(t, len(seeds) >= 2, true)
			for _, seed := range seeds {
				f, err := problem.Freeze(p, seed)
				testutil.NoError(t, err)
				if f.Question == "" || len(f.Assets) == 0 {
					t.Fatal("public question and scaffold required")
				}
			}
			want, ok := securityCases[p.ID]
			if !ok {
				t.Fatalf("securityCases has no row for %s", p.ID)
			}
			for id, score := range want {
				testutil.Equal(t, caseScore(p, id), score)
			}
			testutil.Equal(t, len(p.Cases), len(want))
			testutil.Equal(t, initialBelowMax(p), true)
			testutil.Equal(t, p.InitialScore, securityInitialScores[p.ID])
			if want := securityCapabilities[p.ID]; !slices.Equal(p.Capabilities, want) {
				t.Errorf("capabilities = %v, want %v", p.Capabilities, want)
			}
			for id, score := range want {
				if !strings.HasPrefix(id, "alternative-") && score >= 5 {
					t.Fatalf("wrong case %s earns full credit", id)
				}
			}
			// Full status vectors: every case states every criterion.
			for _, c := range p.Cases {
				testutil.Equal(t, len(c.Expected), len(p.Criteria))
				for _, crit := range p.Criteria {
					switch c.Expected[crit.ID] {
					case model.StatusPass, model.StatusFail, model.StatusError:
					default:
						t.Errorf("case %s has no status for criterion %s", c.ID, crit.ID)
					}
				}
			}
			// Ruling R1: every http criterion observes for 60 seconds.
			for _, c := range p.Criteria {
				if c.Kind == model.CriterionHTTP {
					testutil.Equal(t, c.Timeout, 60*time.Second)
				}
			}
			// Private file sources are copied raw, so they must not rely on
			// parameter rendering.
			groups := [][]model.Action{p.Setup, p.Solution}
			for _, c := range p.Cases {
				groups = append(groups, c.Actions)
			}
			for _, g := range groups {
				for _, a := range g {
					if a.Kind != model.ActionFile {
						continue
					}
					src, _ := a.Args["source"].(string)
					b, ok := p.Files[src]
					if !ok {
						t.Fatalf("file source %q is not part of the package", src)
					}
					if strings.Contains(string(b), "${") {
						t.Errorf("file source %s contains an unrendered ${param}", src)
					}
				}
			}
		})
	}
}

func TestSecurityBankPreflight(t *testing.T) {
	ids := presentSecurityIDs(t)
	if len(ids) == 0 {
		t.Skip("no environment-security package yet")
	}
	preflightBank(t, loadGroup(t, ids))
}

// customResourceObjects renders the Backup objects (and the ConfigMaps) a
// group of actions applies, keyed by kind.
func customResourceObjects(t *testing.T, p model.Problem, f model.FrozenProblem, actions []model.Action) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, a := range actions {
		if a.Kind != model.ActionApply {
			continue
		}
		for _, o := range applyObjects(t, p, f, a) {
			kind, _ := o["kind"].(string)
			if _, dup := out[kind]; dup {
				t.Fatalf("two %s objects in one action group", kind)
			}
			out[kind] = o
		}
	}
	return out
}

func nestedString(t *testing.T, o map[string]any, path ...string) string {
	t.Helper()
	v, found, err := unstructured.NestedFieldNoCopy(o, path...)
	if err != nil || !found {
		t.Fatalf("%v is missing in %v (%v)", path, o, err)
	}
	return fmt.Sprint(v)
}

// custom-resource-001: the CRD is owned through the scope, its schema is the
// one the criteria and the question rely on, every correct Backup carries the
// graded literals and each wrong case differs from the correct one in exactly
// the field it is named after.
func TestSecurityCustomResourceContracts(t *testing.T) {
	p := loadGroup(t, []string{"custom-resource-001"})[0]
	f, err := problem.Freeze(p, 1)
	testutil.NoError(t, err)

	// Scope: exactly the CRD, and the setup applies it.
	testutil.Equal(t, p.Scope.Namespaces == nil, true)
	testutil.Equal(t, p.Scope.ClusterResources, []model.ObjectRef{{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition", Name: "backups.ops.junkloud.dev"}})
	setup := map[string]map[string]any{}
	for _, a := range p.Setup {
		for _, o := range applyObjects(t, p, f, a) {
			setup[o["kind"].(string)] = o
		}
	}
	testutil.Equal(t, len(setup), 2)
	crd, deploy := setup["CustomResourceDefinition"], setup["Deployment"]
	if crd == nil || deploy == nil {
		t.Fatalf("setup must apply the CRD and Deployment ledger, got %v", setup)
	}
	testutil.Equal(t, nestedString(t, crd, "metadata", "name"), "backups.ops.junkloud.dev")
	testutil.Equal(t, nestedString(t, deploy, "metadata", "name"), "ledger")
	testutil.Equal(t, nestedString(t, deploy, "metadata", "namespace"), f.Namespace)

	// CRD shape: group, kind, scope, short name, and the schema rules.
	plural := nestedString(t, crd, "spec", "names", "plural")
	group := nestedString(t, crd, "spec", "group")
	testutil.Equal(t, plural+"."+group, "backups.ops.junkloud.dev")
	testutil.Equal(t, nestedString(t, crd, "spec", "names", "kind"), "Backup")
	testutil.Equal(t, nestedString(t, crd, "spec", "scope"), "Namespaced")
	testutil.Equal(t, nestedString(t, crd, "spec", "names", "shortNames"), "[bk]")
	versions := crd["spec"].(map[string]any)["versions"].([]any)
	testutil.Equal(t, len(versions), 1)
	v := versions[0].(map[string]any)
	testutil.Equal(t, v["name"], any("v1"))
	testutil.Equal(t, v["served"], any(true))
	testutil.Equal(t, v["storage"], any(true))
	spec := nestedMap(t, v, "schema", "openAPIV3Schema", "properties", "spec")
	testutil.Equal(t, spec["required"], any([]any{"schedule", "target", "retentionDays"}))
	props := spec["properties"].(map[string]any)
	testutil.Equal(t, nestedMap(t, props, "schedule")["type"], any("string"))
	testutil.Equal(t, nestedMap(t, props, "description")["type"], any("string"))
	retention := nestedMap(t, props, "retentionDays")
	testutil.Equal(t, retention["type"], any("integer"))
	testutil.Equal(t, retention["minimum"], any(float64(1)))
	testutil.Equal(t, retention["maximum"], any(float64(30)))
	target := nestedMap(t, props, "target")
	testutil.Equal(t, target["required"], any([]any{"kind", "name"}))
	testutil.Equal(t, nestedMap(t, target, "properties", "kind")["enum"], any([]any{"Deployment", "StatefulSet"}))
	if strings.Contains(string(p.Files["setup/crd.yaml"]), "preserve-unknown-fields") {
		t.Error("the schema must be strict: no x-kubernetes-preserve-unknown-fields")
	}
	if slices.Contains(spec["required"].([]any), any("description")) {
		t.Error("description must stay optional")
	}

	// Criteria pin the literals the question states.
	want := map[string]string{"spec.schedule": "0 2 * * *", "spec.target.kind": "Deployment", "spec.target.name": "ledger", "spec.retentionDays": "7"}
	got := map[string]string{}
	for _, c := range p.Criteria {
		switch c.ID {
		case "crd-discovered":
			testutil.Equal(t, c.Args["path"], any("answers/backup-crd.txt"))
			testutil.Equal(t, c.Args["matches"], any([]any{`^\s*backups\.ops\.junkloud\.dev\s*$`}))
		case "backup-exists":
			testutil.Equal(t, c.Kind, model.CriterionResource)
			testutil.Equal(t, c.Target.APIVersion, "ops.junkloud.dev/v1")
			testutil.Equal(t, c.Target.Kind, "Backup")
			testutil.Equal(t, c.Target.Name, "nightly")
		case "backup-schedule", "backup-target", "backup-retention":
			testutil.Equal(t, c.Kind, model.CriterionField)
			testutil.Equal(t, c.Target.APIVersion, "ops.junkloud.dev/v1")
			testutil.Equal(t, c.Target.Kind, "Backup")
			testutil.Equal(t, c.Target.Name, "nightly")
			testutil.Equal(t, c.DependsOn, []string{"backup-exists"})
			for _, fld := range c.Args["fields"].([]any) {
				m := fld.(map[string]any)
				got[m["path"].(string)] = fmt.Sprint(m["equals"])
			}
		}
	}
	testutil.Equal(t, got, want)
	q := string(p.Files[problem.QuestionFile])
	for _, lit := range []string{"answers/backup-crd.txt", "`Backup`", "`nightly`", "`0 2 * * *`", "`Deployment`", "`ledger`", "7 retention days", "ConfigMap"} {
		if !strings.Contains(q, lit) {
			t.Errorf("question must state %s", lit)
		}
	}

	// Backups: correct ones carry every graded literal; each wrong case
	// differs in exactly its own field.
	fields := func(o map[string]any) map[string]string {
		return map[string]string{
			"spec.schedule":      nestedString(t, o, "spec", "schedule"),
			"spec.target.kind":   nestedString(t, o, "spec", "target", "kind"),
			"spec.target.name":   nestedString(t, o, "spec", "target", "name"),
			"spec.retentionDays": nestedString(t, o, "spec", "retentionDays"),
		}
	}
	check := func(label string, actions []model.Action, differs string) {
		o := customResourceObjects(t, p, f, actions)["Backup"]
		if o == nil {
			t.Fatalf("%s: no Backup applied", label)
		}
		testutil.Equal(t, nestedString(t, o, "apiVersion"), "ops.junkloud.dev/v1")
		testutil.Equal(t, nestedString(t, o, "metadata", "name"), "nightly")
		testutil.Equal(t, nestedString(t, o, "metadata", "namespace"), f.Namespace)
		for path, w := range want {
			if g := fields(o)[path]; (g != w) != (path == differs) {
				t.Errorf("%s: %s = %q (want %q, differs=%v)", label, path, g, w, path == differs)
			}
		}
	}
	check("solution", p.Solution, "")
	for _, c := range p.Cases {
		switch c.ID {
		case "configmap-substitute":
			objs := customResourceObjects(t, p, f, c.Actions)
			testutil.Equal(t, len(objs), 1)
			cm := objs["ConfigMap"]
			if cm == nil {
				t.Fatal("configmap-substitute must apply only a ConfigMap")
			}
			testutil.Equal(t, nestedString(t, cm, "metadata", "name"), "nightly")
			testutil.Equal(t, nestedString(t, cm, "data", "schedule"), "0 2 * * *")
			testutil.Equal(t, nestedString(t, cm, "data", "target-kind"), "Deployment")
			testutil.Equal(t, nestedString(t, cm, "data", "target-name"), "ledger")
			testutil.Equal(t, nestedString(t, cm, "data", "retention-days"), "7")
		case "wrong-target":
			check(c.ID, c.Actions, "spec.target.kind")
		case "wrong-retention":
			check(c.ID, c.Actions, "spec.retentionDays")
		default:
			check(c.ID, c.Actions, "")
		}
	}
	// The alternative adds only the optional field the schema declares.
	alt := customResourceObjects(t, p, f, caseActions(p, "alternative-extra-fields"))["Backup"]
	testutil.Equal(t, nestedString(t, alt, "spec", "description") != "", true)

	// Answer files: the written name is the CRD's own <plural>.<group>.
	answerOf := func(actions []model.Action) string {
		var out string
		for _, a := range actions {
			if a.Kind == model.ActionFile && a.Args["path"] == "answers/backup-crd.txt" {
				out = string(p.Files[a.Args["source"].(string)])
			}
		}
		return out
	}
	if answerOf(p.Solution) == "" {
		t.Error("solution writes no answer")
	}
	testutil.Equal(t, strings.TrimSpace(answerOf(p.Solution)), plural+"."+group)
	for _, c := range p.Cases {
		a := answerOf(c.Actions)
		switch c.ID {
		case "missing-answer":
			testutil.Equal(t, a, "")
		case "alternative-extra-fields":
			testutil.Equal(t, strings.TrimSpace(a), plural+"."+group)
			if a == strings.TrimSpace(a) {
				t.Error("the alternative answer must have surrounding whitespace")
			}
		default:
			testutil.Equal(t, strings.TrimSpace(a), plural+"."+group)
		}
	}
	// The handout ships the answers directory.
	if _, ok := p.Files["setup/provided/answers/README.txt"]; !ok {
		t.Error("answers/ must be shipped in setup/provided")
	}
}

func caseActions(p model.Problem, id string) []model.Action {
	for _, c := range p.Cases {
		if c.ID == id {
			return c.Actions
		}
	}
	return nil
}

func nestedMap(t *testing.T, o map[string]any, path ...string) map[string]any {
	t.Helper()
	v, found, err := unstructured.NestedFieldNoCopy(o, path...)
	m, ok := v.(map[string]any)
	if err != nil || !found || !ok {
		t.Fatalf("%v is not an object in %v (%v)", path, o, err)
	}
	return m
}

// ------------------------------------------------------------- access-001

type accessCheck struct {
	Verb, Resource, Group, Name, Namespace string
	ClusterScope, Allowed                  bool
}

func accessChecksOf(t *testing.T, c model.Criterion) []accessCheck {
	t.Helper()
	raw, ok := c.Args["checks"].([]any)
	if !ok {
		t.Fatalf("criterion %s has no checks", c.ID)
	}
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	var out []accessCheck
	for _, r := range raw {
		m := r.(map[string]any)
		allowed, ok := m["allowed"].(bool)
		if !ok {
			t.Fatalf("criterion %s has a check without explicit allowed: %v", c.ID, m)
		}
		cs, _ := m["cluster_scope"].(bool)
		out = append(out, accessCheck{Verb: str(m, "verb"), Resource: str(m, "resource"), Group: str(m, "group"), Name: str(m, "name"), Namespace: str(m, "namespace"), ClusterScope: cs, Allowed: allowed})
	}
	return out
}

// accessObjects renders every object a group of actions applies, keyed by kind.
func accessObjects(t *testing.T, p model.Problem, f model.FrozenProblem, actions []model.Action) map[string][]map[string]any {
	t.Helper()
	out := map[string][]map[string]any{}
	for _, a := range actions {
		if a.Kind != model.ActionApply {
			t.Fatalf("access-001 uses only apply actions, got %s", a.Kind)
		}
		for _, o := range applyObjects(t, p, f, a) {
			kind, _ := o["kind"].(string)
			out[kind] = append(out[kind], o)
		}
	}
	return out
}

// onlyObject returns the single object of a kind.
func onlyObject(t *testing.T, label string, objs map[string][]map[string]any, kind string) map[string]any {
	t.Helper()
	if len(objs[kind]) != 1 {
		t.Fatalf("%s: want exactly one %s, got %d", label, kind, len(objs[kind]))
	}
	return objs[kind][0]
}

func kindsOf(objs map[string][]map[string]any) []string {
	var out []string
	for k := range objs {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// ruleTuples flattens RBAC rules to "group|resource|resourceName|verb"
// tuples (an empty resourceName means every name), so differently laid out
// Roles with the same rights compare equal.
func ruleTuples(t *testing.T, o map[string]any) []string {
	t.Helper()
	rules, _ := o["rules"].([]any)
	strs := func(m map[string]any, k string) []string {
		var out []string
		for _, v := range m[k].([]any) {
			out = append(out, v.(string))
		}
		return out
	}
	var out []string
	for _, r := range rules {
		m := r.(map[string]any)
		names := []string{""}
		if _, ok := m["resourceNames"]; ok {
			names = strs(m, "resourceNames")
		}
		for _, g := range strs(m, "apiGroups") {
			for _, res := range strs(m, "resources") {
				for _, n := range names {
					for _, v := range strs(m, "verbs") {
						out = append(out, g+"|"+res+"|"+n+"|"+v)
					}
				}
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func deepCopyObject(o map[string]any) map[string]any { return runtime.DeepCopyJSON(o) }

// access-001: the allowed and forbidden action lists are the ones the
// question states, the cluster-scoped objects of the cluster-wide case are
// owned through the scope, the provided Deployment is the one the solution
// changes in exactly one field, and every wrong case differs from the solution
// in exactly the mistake it is named after.
func TestSecurityAccessContracts(t *testing.T) {
	p := loadGroup(t, []string{"access-001"})[0]
	f, err := problem.Freeze(p, 1)
	testutil.NoError(t, err)
	ns := f.Namespace
	q := string(p.Files[problem.QuestionFile])

	// Scope: exactly the ClusterRole and ClusterRoleBinding of the
	// cluster-wide case.
	const clusterName = "jk-report-reader"
	testutil.Equal(t, p.Scope.Namespaces == nil, true)
	testutil.Equal(t, p.Scope.ClusterResources, []model.ObjectRef{
		{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: clusterName},
		{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding", Name: clusterName},
	})

	// Setup: ConfigMap report-config and Deployment reporter running as the
	// default ServiceAccount with a mounted token.
	setup := accessObjects(t, p, f, p.Setup)
	testutil.Equal(t, kindsOf(setup), []string{"ConfigMap", "Deployment"})
	testutil.Equal(t, nestedString(t, onlyObject(t, "setup", setup, "ConfigMap"), "metadata", "name"), "report-config")
	provided := onlyObject(t, "setup", setup, "Deployment")
	testutil.Equal(t, nestedString(t, provided, "metadata", "name"), "reporter")
	testutil.Equal(t, nestedString(t, provided, "metadata", "namespace"), ns)
	testutil.Equal(t, nestedString(t, provided, "spec", "replicas"), "1")
	podSpec := nestedMap(t, provided, "spec", "template", "spec")
	for _, banned := range []string{"serviceAccountName", "serviceAccount", "automountServiceAccountToken"} {
		if _, ok := podSpec[banned]; ok {
			t.Errorf("the provided Deployment must not set %s", banned)
		}
	}
	containers := podSpec["containers"].([]any)
	testutil.Equal(t, len(containers), 1)
	c0 := containers[0].(map[string]any)
	testutil.Equal(t, c0["name"], any("reporter"))
	testutil.Equal(t, c0["image"], any("docker.io/library/busybox:1.37.0"))
	testutil.Equal(t, c0["command"], any([]any{"sh", "-c", "sleep 3600"}))

	// Criteria: ids, kinds and dependencies.
	crit := map[string]model.Criterion{}
	var order []string
	for _, c := range p.Criteria {
		crit[c.ID] = c
		order = append(order, c.ID)
	}
	testutil.Equal(t, order, []string{"reporter-ready", "can-read", "least-privilege", "runs-as-sa", "default-sa-unchanged"})
	ready := crit["reporter-ready"]
	testutil.Equal(t, ready.Kind, model.CriterionReady)
	testutil.Equal(t, ready.Target.Name, "reporter")
	testutil.Equal(t, fmt.Sprint(ready.Args["replicas"]), "1")
	testutil.Equal(t, ready.Timeout, 90*time.Second)
	for _, lit := range []string{"1 Ready replica", "10-second stability window", "90-second observation limit"} {
		if !strings.Contains(q, lit) {
			t.Errorf("question must state %q", lit)
		}
	}
	for id, c := range crit {
		if id == "reporter-ready" {
			continue
		}
		testutil.Equal(t, c.Kind, model.CriterionAuthorization)
		testutil.Equal(t, c.Target.APIVersion, "v1")
		testutil.Equal(t, c.Target.Kind, "ServiceAccount")
		testutil.Equal(t, c.Target.Namespace, "${namespace}")
		want := "report-reader"
		if id == "default-sa-unchanged" {
			want = "default"
		}
		testutil.Equal(t, c.Target.Name, want)
	}

	// The allowed / forbidden lists are exactly these, and each check is
	// stated in the question.
	type pinned struct {
		check  accessCheck
		phrase string
	}
	assertPinned := func(id string, want []pinned) {
		t.Helper()
		got := accessChecksOf(t, crit[id])
		var wantChecks []accessCheck
		for _, w := range want {
			wantChecks = append(wantChecks, w.check)
			if !strings.Contains(q, w.phrase) {
				t.Errorf("%s: question must state %q (for %+v)", id, w.phrase, w.check)
			}
		}
		testutil.Equal(t, got, wantChecks)
	}
	assertPinned("can-read", []pinned{
		{accessCheck{Verb: "get", Resource: "pods", Allowed: true}, "get, list and watch Pods in `${namespace}`"},
		{accessCheck{Verb: "list", Resource: "pods", Allowed: true}, "get, list and watch Pods in `${namespace}`"},
		{accessCheck{Verb: "watch", Resource: "pods", Allowed: true}, "get, list and watch Pods in `${namespace}`"},
		{accessCheck{Verb: "get", Resource: "configmaps", Name: "report-config", Allowed: true}, "get the ConfigMap `report-config` in `${namespace}`"},
	})
	assertPinned("least-privilege", []pinned{
		{accessCheck{Verb: "delete", Resource: "pods"}, "delete Pods"},
		{accessCheck{Verb: "get", Resource: "secrets"}, "get or list Secrets"},
		{accessCheck{Verb: "list", Resource: "secrets"}, "get or list Secrets"},
		{accessCheck{Verb: "create", Resource: "deployments", Group: "apps"}, "create Deployments"},
		{accessCheck{Verb: "list", Resource: "pods", ClusterScope: true}, "list Pods in all namespaces"},
		{accessCheck{Verb: "list", Resource: "pods", Namespace: "kube-system"}, "list Pods in the namespace `kube-system`"},
		{accessCheck{Verb: "get", Resource: "configmaps", Name: "other"}, "get any other ConfigMap (for example one named `other`)"},
	})
	assertPinned("default-sa-unchanged", []pinned{
		{accessCheck{Verb: "get", Resource: "pods"}, "it must still not be able to get or list Pods or to get the ConfigMap `report-config`"},
		{accessCheck{Verb: "list", Resource: "pods"}, "it must still not be able to get or list Pods or to get the ConfigMap `report-config`"},
		{accessCheck{Verb: "get", Resource: "configmaps", Name: "report-config"}, "it must still not be able to get or list Pods or to get the ConfigMap `report-config`"},
	})
	assertPinned("runs-as-sa", []pinned{
		{accessCheck{Verb: "get", Resource: "pods", Allowed: true}, "run Deployment `reporter` as `report-reader`"},
	})
	runs := crit["runs-as-sa"]
	testutil.Equal(t, runs.DependsOn, []string{"reporter-ready"})
	testutil.Equal(t, runs.Args["runs_as"], any(map[string]any{"api_version": "apps/v1", "kind": "Deployment", "name": "reporter", "identity": "token"}))
	for _, lit := range []string{"ServiceAccount token mounted", "automountServiceAccountToken: false", "`cat`", "this point needs the first one", "ClusterRole", "`edit`"} {
		if !strings.Contains(q, lit) {
			t.Errorf("question must state %q", lit)
		}
	}
	for id, c := range crit {
		if id != "runs-as-sa" && c.DependsOn != nil {
			t.Errorf("%s must not depend on another criterion", id)
		}
	}

	// Solution: ServiceAccount, Role with the two stated rules, RoleBinding,
	// and the provided Deployment with only serviceAccountName added.
	sol := accessObjects(t, p, f, p.Solution)
	testutil.Equal(t, kindsOf(sol), []string{"Deployment", "Role", "RoleBinding", "ServiceAccount"})
	solSA := onlyObject(t, "solution", sol, "ServiceAccount")
	solRole := onlyObject(t, "solution", sol, "Role")
	solRB := onlyObject(t, "solution", sol, "RoleBinding")
	solDeploy := onlyObject(t, "solution", sol, "Deployment")
	testutil.Equal(t, nestedString(t, solSA, "metadata", "name"), "report-reader")
	testutil.Equal(t, nestedString(t, solSA, "metadata", "namespace"), ns)
	canonical := []string{"|configmaps|report-config|get", "|pods||get", "|pods||list", "|pods||watch"}
	testutil.Equal(t, ruleTuples(t, solRole), canonical)
	testutil.Equal(t, nestedString(t, solRB, "subjects"), fmt.Sprint([]any{map[string]any{"kind": "ServiceAccount", "name": "report-reader", "namespace": ns}}))
	testutil.Equal(t, nestedString(t, solRB, "roleRef"), fmt.Sprint(map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": nestedString(t, solRole, "metadata", "name")}))
	testutil.Equal(t, nestedString(t, solDeploy, "spec", "template", "spec", "serviceAccountName"), "report-reader")
	unchanged := deepCopyObject(solDeploy)
	unstructured.RemoveNestedField(unchanged, "spec", "template", "spec", "serviceAccountName")
	testutil.Equal(t, unchanged, provided)

	// Cluster-scoped objects appear only in the cluster-wide case, with the
	// scoped names.
	clusterKinds := []string{"ClusterRole", "ClusterRoleBinding"}
	for _, group := range append([][]model.Action{p.Setup, p.Solution}, func() [][]model.Action {
		var gs [][]model.Action
		for _, c := range p.Cases {
			if c.ID != "cluster-wide-binding" {
				gs = append(gs, c.Actions)
			}
		}
		return gs
	}()...) {
		objs := accessObjects(t, p, f, group)
		for _, k := range clusterKinds {
			if len(objs[k]) != 0 {
				t.Errorf("only cluster-wide-binding may apply a %s", k)
			}
		}
	}

	// Cases.
	rbWith := func(mutate func(rb map[string]any)) map[string]any {
		rb := deepCopyObject(solRB)
		mutate(rb)
		return rb
	}
	for _, c := range p.Cases {
		objs := accessObjects(t, p, f, c.Actions)
		switch c.ID {
		case "bound-to-default":
			// Same Role, same RoleBinding, only the subject is `default`;
			// no ServiceAccount report-reader and no Deployment change.
			testutil.Equal(t, kindsOf(objs), []string{"Role", "RoleBinding"})
			testutil.Equal(t, onlyObject(t, c.ID, objs, "Role"), solRole)
			want := rbWith(func(rb map[string]any) { rb["subjects"].([]any)[0].(map[string]any)["name"] = "default" })
			testutil.Equal(t, onlyObject(t, c.ID, objs, "RoleBinding"), want)
		case "edit-rolebinding":
			// Same ServiceAccount and Deployment; the RoleBinding points at
			// the built-in ClusterRole edit instead of the Role.
			testutil.Equal(t, kindsOf(objs), []string{"Deployment", "RoleBinding", "ServiceAccount"})
			testutil.Equal(t, onlyObject(t, c.ID, objs, "ServiceAccount"), solSA)
			testutil.Equal(t, onlyObject(t, c.ID, objs, "Deployment"), solDeploy)
			want := rbWith(func(rb map[string]any) {
				rb["roleRef"] = map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "edit"}
			})
			testutil.Equal(t, onlyObject(t, c.ID, objs, "RoleBinding"), want)
		case "cluster-wide-binding":
			// Same rules and subject, but as ClusterRole + ClusterRoleBinding
			// with the scoped names.
			testutil.Equal(t, kindsOf(objs), []string{"ClusterRole", "ClusterRoleBinding", "Deployment", "ServiceAccount"})
			testutil.Equal(t, onlyObject(t, c.ID, objs, "ServiceAccount"), solSA)
			testutil.Equal(t, onlyObject(t, c.ID, objs, "Deployment"), solDeploy)
			cr := onlyObject(t, c.ID, objs, "ClusterRole")
			crb := onlyObject(t, c.ID, objs, "ClusterRoleBinding")
			testutil.Equal(t, nestedString(t, cr, "metadata", "name"), clusterName)
			testutil.Equal(t, nestedString(t, crb, "metadata", "name"), clusterName)
			testutil.Equal(t, cr["rules"], solRole["rules"])
			testutil.Equal(t, crb["subjects"], solRB["subjects"])
			testutil.Equal(t, crb["roleRef"], any(map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": clusterName}))
			if _, ok := crb["metadata"].(map[string]any)["namespace"]; ok {
				t.Error("a ClusterRoleBinding has no namespace")
			}
		case "sa-not-used":
			// Exactly the solution's RBAC; the Deployment keeps `default`.
			testutil.Equal(t, kindsOf(objs), []string{"Role", "RoleBinding", "ServiceAccount"})
			testutil.Equal(t, onlyObject(t, c.ID, objs, "ServiceAccount"), solSA)
			testutil.Equal(t, onlyObject(t, c.ID, objs, "Role"), solRole)
			testutil.Equal(t, onlyObject(t, c.ID, objs, "RoleBinding"), solRB)
		case "alternative-split-roles":
			// Two Roles whose rules add up to the solution's rights, each
			// bound to report-reader; the Deployment is the solution's.
			testutil.Equal(t, kindsOf(objs), []string{"Deployment", "Role", "RoleBinding", "ServiceAccount"})
			testutil.Equal(t, onlyObject(t, c.ID, objs, "ServiceAccount"), solSA)
			testutil.Equal(t, onlyObject(t, c.ID, objs, "Deployment"), solDeploy)
			testutil.Equal(t, len(objs["Role"]), 2)
			testutil.Equal(t, len(objs["RoleBinding"]), 2)
			var union, roleNames, bound []string
			for _, r := range objs["Role"] {
				tuples := ruleTuples(t, r)
				if len(tuples) == 0 || slices.Equal(tuples, canonical) {
					t.Errorf("each split Role must hold part of the rights, got %v", tuples)
				}
				union = append(union, tuples...)
				roleNames = append(roleNames, nestedString(t, r, "metadata", "name"))
			}
			slices.Sort(union)
			testutil.Equal(t, slices.Compact(union), canonical)
			testutil.Equal(t, len(slices.Compact(slices.Clone(union))), len(union))
			for _, rb := range objs["RoleBinding"] {
				testutil.Equal(t, rb["subjects"], solRB["subjects"])
				ref := rb["roleRef"].(map[string]any)
				testutil.Equal(t, ref["kind"], any("Role"))
				testutil.Equal(t, ref["apiGroup"], any("rbac.authorization.k8s.io"))
				bound = append(bound, ref["name"].(string))
			}
			slices.Sort(roleNames)
			slices.Sort(bound)
			testutil.Equal(t, bound, roleNames)
			if slices.Contains(roleNames, "report-reader") {
				t.Error("the split Roles must be named differently from the solution Role")
			}
		default:
			t.Errorf("access-001 case %s has no contract", c.ID)
		}
	}

	// The explanation spells out the reset limit and the edit mistake.
	if !strings.Contains(string(p.Files["explanation.ko.md"]), "reset") {
		t.Error("the explanation must mention that cluster-scoped objects survive reset")
	}
	if strings.Contains(q, "automountServiceAccountToken: true") {
		t.Error("the question must not demand an explicit automount value")
	}
}
