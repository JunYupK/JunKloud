//go:build linux

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/testutil"
	"k8s.io/apimachinery/pkg/api/resource"
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
	"resources-001": {
		"quota-deleted":                  3,
		"quota-raised":                   3,
		"oversized-requests":             2,
		"limitrange-missing":             4,
		"alternative-decimal-quantities": 5,
	},
}

// securityCapabilities and securityInitialScores pin each package's declared
// capabilities and the score of the untouched environment.
var securityCapabilities = map[string][]string{
	"custom-resource-001": nil,
	"access-001":          nil,
	"resources-001":       nil,
}

var securityInitialScores = map[string]int{
	"custom-resource-001": 0,
	"access-001":          2,
	"resources-001":       1,
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
	// Every key the alternative sets, at any depth the schema describes, is
	// declared in the CRD schema: a strict-decoding error or silent pruning
	// would otherwise turn the "correct" alternative into a trap.
	assertDeclared(t, "alternative-extra-fields spec", nestedMap(t, alt, "spec"), spec)

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

// assertDeclared fails for every key of value that the object schema does not
// list under properties, and recurses into nested objects the schema describes.
func assertDeclared(t *testing.T, where string, value, schema map[string]any) {
	t.Helper()
	declared, _ := schema["properties"].(map[string]any)
	for k, v := range value {
		sub, ok := declared[k].(map[string]any)
		if !ok {
			t.Errorf("%s: key %q is not declared in the CRD schema properties", where, k)
			continue
		}
		if m, isMap := v.(map[string]any); isMap {
			assertDeclared(t, where+"."+k, m, sub)
		}
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
			// The Roles must not overlap: the union has no duplicate tuple.
			// Compact a clone, so the comparison sees the uncompacted length.
			slices.Sort(union)
			testutil.Equal(t, slices.Compact(slices.Clone(union)), canonical)
			testutil.Equal(t, len(union), len(canonical))
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

// ---------------------------------------------------------- resources-001

// resourcesGroup is what a group of actions does: the objects it applies,
// keyed by kind (at most one per kind), and the objects it deletes first.
type resourcesGroup struct {
	objs    map[string]map[string]any
	deleted []model.ObjectRef
}

func resourcesActions(t *testing.T, p model.Problem, f model.FrozenProblem, label string, actions []model.Action) resourcesGroup {
	t.Helper()
	built, err := author.BuildActions(p.Files, f.Parameters, actions)
	testutil.NoError(t, err)
	g := resourcesGroup{objs: map[string]map[string]any{}}
	for _, a := range built {
		switch a.Kind {
		case model.ActionApply:
			for _, o := range a.Objects {
				kind, _ := o["kind"].(string)
				if _, dup := g.objs[kind]; dup {
					t.Fatalf("%s: two %s objects", label, kind)
				}
				g.objs[kind] = o
			}
		case model.ActionDeleteOwned:
			g.deleted = append(g.deleted, a.Targets...)
		default:
			t.Fatalf("%s: resources-001 uses only apply and delete-owned, got %s", label, a.Kind)
		}
	}
	return g
}

func (g resourcesGroup) kinds() []string {
	var out []string
	for k := range g.objs {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// workerContainer returns the single container of a worker Deployment.
func workerContainer(t *testing.T, label string, deploy map[string]any) map[string]any {
	t.Helper()
	cs, ok := nestedMap(t, deploy, "spec", "template", "spec")["containers"].([]any)
	if !ok || len(cs) != 1 {
		t.Fatalf("%s: want exactly one container", label)
	}
	return cs[0].(map[string]any)
}

// quantityMap parses a {name: quantity} object into semantic quantities.
func quantityMap(t *testing.T, label string, m map[string]any) map[string]resource.Quantity {
	t.Helper()
	out := map[string]resource.Quantity{}
	for k, v := range m {
		text := fmt.Sprint(v)
		if n, isFloat := v.(float64); isFloat {
			text = strconv.FormatFloat(n, 'f', -1, 64)
		}
		q, err := resource.ParseQuantity(text)
		if err != nil {
			t.Fatalf("%s: %s = %v is not a quantity: %v", label, k, v, err)
		}
		out[k] = q
	}
	return out
}

// quantityStrings renders quantities in one canonical notation (milli-units),
// so that 64Mi and 67108864 compare equal.
func quantityStrings(m map[string]resource.Quantity) map[string]string {
	out := map[string]string{}
	for k, q := range m {
		out[k] = strconv.FormatInt(q.MilliValue(), 10)
	}
	return out
}

// canonQuantities canonicalises a {name: "100m"} literal the same way.
func canonQuantities(t *testing.T, m map[string]string) map[string]string {
	t.Helper()
	parsed := map[string]resource.Quantity{}
	for k, v := range m {
		parsed[k] = resource.MustParse(v)
	}
	return quantityStrings(parsed)
}

// resources-001: the quota, the Deployment and the LimitRange carry the
// literals the question and the criteria state, the correct answer fits the
// quota while the oversized one does not, and each wrong case differs from the
// solution only in the mistake it is named after.
func TestSecurityResourcesContracts(t *testing.T) {
	p := loadGroup(t, []string{"resources-001"})[0]
	f, err := problem.Freeze(p, 1)
	testutil.NoError(t, err)
	ns := f.Namespace
	q := string(p.Files[problem.QuestionFile])

	// No cluster-scoped object belongs to this problem.
	testutil.Equal(t, p.Scope.Namespaces == nil, true)
	testutil.Equal(t, len(p.Scope.ClusterResources), 0)

	// Setup: the quota and the Deployment without resources.
	setup := resourcesActions(t, p, f, "setup", p.Setup)
	testutil.Equal(t, setup.kinds(), []string{"Deployment", "ResourceQuota"})
	quota := setup.objs["ResourceQuota"]
	testutil.Equal(t, nestedString(t, quota, "metadata", "name"), "team-quota")
	testutil.Equal(t, nestedString(t, quota, "metadata", "namespace"), ns)
	hard := map[string]string{"requests.cpu": "500m", "limits.cpu": "1", "requests.memory": "256Mi", "limits.memory": "512Mi", "pods": "4"}
	testutil.Equal(t, quantityStrings(quantityMap(t, "setup quota", nestedMap(t, quota, "spec", "hard"))), canonQuantities(t, hard))
	deploy := setup.objs["Deployment"]
	testutil.Equal(t, nestedString(t, deploy, "metadata", "name"), "worker")
	testutil.Equal(t, nestedString(t, deploy, "metadata", "namespace"), ns)
	testutil.Equal(t, nestedString(t, deploy, "spec", "replicas"), "2")
	c0 := workerContainer(t, "setup", deploy)
	testutil.Equal(t, c0["name"], any("worker"))
	testutil.Equal(t, c0["image"], any("docker.io/library/busybox:1.37.0"))
	testutil.Equal(t, c0["command"], any([]any{"sh", "-c", "sleep 3600"}))
	if _, ok := c0["resources"]; ok {
		t.Error("the provided worker must declare no resources: that is what the quota rejects")
	}

	// Criteria: ids, kinds, dependencies, timeouts.
	crit := map[string]model.Criterion{}
	var order []string
	for _, c := range p.Criteria {
		crit[c.ID] = c
		order = append(order, c.ID)
	}
	testutil.Equal(t, order, []string{"worker-resources", "quota-intact", "worker-ready", "quota-used", "limitrange-defaults"})
	fieldsOf := func(id string) map[string]string {
		t.Helper()
		c := crit[id]
		testutil.Equal(t, c.Kind, model.CriterionField)
		out := map[string]string{}
		for _, fld := range c.Args["fields"].([]any) {
			m := fld.(map[string]any)
			out[m["path"].(string)] = fmt.Sprint(m["equals"])
		}
		return out
	}
	target := func(id, apiVersion, kind, name string) {
		t.Helper()
		c := crit[id]
		testutil.Equal(t, c.Target.APIVersion, apiVersion)
		testutil.Equal(t, c.Target.Kind, kind)
		testutil.Equal(t, c.Target.Name, name)
		testutil.Equal(t, c.Target.Namespace, "${namespace}")
	}
	target("worker-resources", "apps/v1", "Deployment", "worker")
	target("quota-intact", "v1", "ResourceQuota", "team-quota")
	target("worker-ready", "apps/v1", "Deployment", "worker")
	target("quota-used", "v1", "ResourceQuota", "team-quota")
	target("limitrange-defaults", "v1", "LimitRange", "defaults")

	const cpath = "spec.template.spec.containers[name=worker].resources."
	wantRes := map[string]string{
		cpath + "requests.cpu": "100m", cpath + "requests.memory": "64Mi",
		cpath + "limits.cpu": "200m", cpath + "limits.memory": "128Mi",
	}
	testutil.Equal(t, fieldsOf("worker-resources"), wantRes)
	testutil.Equal(t, crit["worker-resources"].DependsOn == nil, true)
	testutil.Equal(t, fieldsOf("quota-intact"), map[string]string{
		"spec.hard['requests.cpu']": "500m", "spec.hard['limits.cpu']": "1",
		"spec.hard['requests.memory']": "256Mi", "spec.hard['limits.memory']": "512Mi", "spec.hard.pods": "4",
	})
	testutil.Equal(t, crit["quota-intact"].DependsOn == nil, true)
	ready := crit["worker-ready"]
	testutil.Equal(t, ready.Kind, model.CriterionReady)
	testutil.Equal(t, fmt.Sprint(ready.Args["replicas"]), "2")
	testutil.Equal(t, ready.Timeout, 90*time.Second)
	// A blocked Pod is "no Pods", which `ready` reports as ERROR: the
	// dependency on the declared resources turns it into a prerequisite FAIL.
	testutil.Equal(t, ready.DependsOn, []string{"worker-resources"})
	used := crit["quota-used"]
	testutil.Equal(t, fieldsOf("quota-used"), map[string]string{"status.used['requests.cpu']": "200m"})
	testutil.Equal(t, used.Args["wait"], any(true))
	testutil.Equal(t, used.Timeout, 60*time.Second)
	testutil.Equal(t, used.DependsOn, []string{"worker-ready", "quota-intact"})
	// status.used is not an auto-quantity path, so the comparison is explicit.
	for _, fld := range used.Args["fields"].([]any) {
		testutil.Equal(t, fld.(map[string]any)["compare"], any("quantity"))
	}
	wantLR := map[string]string{
		"spec.limits[type=Container].defaultRequest.cpu": "50m", "spec.limits[type=Container].defaultRequest.memory": "32Mi",
		"spec.limits[type=Container].default.cpu": "100m", "spec.limits[type=Container].default.memory": "64Mi",
	}
	testutil.Equal(t, fieldsOf("limitrange-defaults"), wantLR)
	testutil.Equal(t, crit["limitrange-defaults"].DependsOn == nil, true)
	for _, fld := range crit["limitrange-defaults"].Args["fields"].([]any) {
		testutil.Equal(t, fld.(map[string]any)["compare"], any("quantity"))
	}

	// Every pinned literal and dependency is stated in the question.
	for _, lit := range []string{
		"`requests.cpu` `500m`", "`limits.cpu` `1`", "`requests.memory` `256Mi`", "`limits.memory` `512Mi`", "`pods` `4`",
		"requests `cpu: 100m`, `memory: 64Mi`", "limits `cpu: 200m`, `memory: 128Mi`",
		"LimitRange `defaults`", "`defaultRequest` is `cpu: 50m`, `memory: 32Mi`", "`default` (the default limit) is `cpu: 100m`, `memory: 64Mi`",
		"Do not delete or change `team-quota`", "its five hard values must stay as they are",
		"`0.1` for `100m`", "`67108864` for `64Mi`",
		"2 Ready replicas", "10-second stability window", "90-second observation limit",
		"this point needs the first one", "`200m`", "up to 60 seconds", "this point needs the third and the second one",
	} {
		if !strings.Contains(q, lit) {
			t.Errorf("question must state %q", lit)
		}
	}

	// Solution: the provided Deployment plus the resources, and the LimitRange.
	sol := resourcesActions(t, p, f, "solution", p.Solution)
	testutil.Equal(t, sol.kinds(), []string{"Deployment", "LimitRange"})
	testutil.Equal(t, len(sol.deleted), 0)
	solDeploy, solLR := sol.objs["Deployment"], sol.objs["LimitRange"]
	solC := workerContainer(t, "solution", solDeploy)
	resOf := func(c map[string]any) map[string]any { return c["resources"].(map[string]any) }
	requests := quantityMap(t, "solution requests", resOf(solC)["requests"].(map[string]any))
	limits := quantityMap(t, "solution limits", resOf(solC)["limits"].(map[string]any))
	testutil.Equal(t, quantityStrings(requests), canonQuantities(t, map[string]string{"cpu": "100m", "memory": "64Mi"}))
	testutil.Equal(t, quantityStrings(limits), canonQuantities(t, map[string]string{"cpu": "200m", "memory": "128Mi"}))
	withoutRes := deepCopyObject(solDeploy)
	unstructured.RemoveNestedField(withoutRes, "spec", "template", "spec", "containers")
	provided := deepCopyObject(deploy)
	unstructured.RemoveNestedField(provided, "spec", "template", "spec", "containers")
	testutil.Equal(t, withoutRes, provided)
	provC := deepCopyObject(c0)
	solNoRes := deepCopyObject(solC)
	delete(solNoRes, "resources")
	testutil.Equal(t, solNoRes, provC)

	// LimitRange: one Container entry with exactly the four defaults.
	testutil.Equal(t, nestedString(t, solLR, "metadata", "name"), "defaults")
	testutil.Equal(t, nestedString(t, solLR, "metadata", "namespace"), ns)
	lrEntries := nestedMap(t, solLR, "spec")["limits"].([]any)
	testutil.Equal(t, len(lrEntries), 1)
	lr0 := lrEntries[0].(map[string]any)
	testutil.Equal(t, lr0["type"], any("Container"))
	lrOf := func(e map[string]any) map[string]string {
		out := map[string]string{}
		for _, key := range []string{"defaultRequest", "default"} {
			for res, qty := range quantityStrings(quantityMap(t, "LimitRange "+key, e[key].(map[string]any))) {
				out["spec.limits[type=Container]."+key+"."+res] = qty
			}
		}
		return out
	}
	testutil.Equal(t, lrOf(lr0), canonQuantities(t, wantLR))
	testutil.Equal(t, len(lr0), 3) // type, defaultRequest, default

	// The correct answer fits the quota with the stated stability margin;
	// the used CPU the criterion expects is replicas x the CPU request.
	replicas := int64(2)
	fit := func(label string, rep int64, req, lim map[string]resource.Quantity) bool {
		t.Helper()
		ok := true
		for key, qty := range map[string]resource.Quantity{
			"requests.cpu": req["cpu"], "requests.memory": req["memory"], "limits.cpu": lim["cpu"], "limits.memory": lim["memory"],
		} {
			total := qty.DeepCopy()
			total.Set(0)
			for range rep {
				total.Add(qty)
			}
			h := resource.MustParse(hard[key])
			if total.Cmp(h) > 0 {
				ok = false
				t.Logf("%s: %d x %s %s exceeds hard %s", label, rep, key, qty.String(), h.String())
			}
		}
		return ok
	}
	if !fit("solution", replicas, requests, limits) {
		t.Error("the solution must fit the quota")
	}
	cpuUsed := requests["cpu"].DeepCopy()
	cpuUsed.Add(requests["cpu"])
	wantUsed := resource.MustParse(fieldsOf("quota-used")["status.used['requests.cpu']"])
	testutil.Equal(t, cpuUsed.Cmp(wantUsed), 0)

	// Cases.
	resWith := func(label string, mutate func(res map[string]any)) map[string]any {
		d := deepCopyObject(solDeploy)
		res := workerContainer(t, label, d)["resources"].(map[string]any)
		mutate(res)
		return d
	}
	for _, c := range p.Cases {
		g := resourcesActions(t, p, f, c.ID, c.Actions)
		switch c.ID {
		case "quota-deleted":
			// The setup-created quota is deleted; the rest is the solution.
			testutil.Equal(t, g.deleted, []model.ObjectRef{{APIVersion: "v1", Kind: "ResourceQuota", Namespace: ns, Name: "team-quota"}})
			testutil.Equal(t, g.kinds(), []string{"Deployment", "LimitRange"})
			testutil.Equal(t, g.objs["Deployment"], solDeploy)
			testutil.Equal(t, g.objs["LimitRange"], solLR)
		case "quota-raised":
			// The quota differs from the setup quota only in requests.cpu.
			testutil.Equal(t, len(g.deleted), 0)
			testutil.Equal(t, g.kinds(), []string{"Deployment", "LimitRange", "ResourceQuota"})
			testutil.Equal(t, g.objs["Deployment"], solDeploy)
			testutil.Equal(t, g.objs["LimitRange"], solLR)
			raised := deepCopyObject(quota)
			nestedMap(t, raised, "spec", "hard")["requests.cpu"] = g.objs["ResourceQuota"]["spec"].(map[string]any)["hard"].(map[string]any)["requests.cpu"]
			testutil.Equal(t, g.objs["ResourceQuota"], raised)
			rc := resource.MustParse(nestedString(t, g.objs["ResourceQuota"], "spec", "hard", "requests.cpu"))
			orig := resource.MustParse(hard["requests.cpu"])
			if rc.Cmp(orig) <= 0 {
				t.Errorf("quota-raised must raise requests.cpu above %s, got %s", orig.String(), rc.String())
			}
		case "oversized-requests":
			// Only the CPU request grows (the limit follows so that
			// request <= limit stays valid); two such Pods exceed the quota
			// while one fits, so which Pod the scheduler admits is irrelevant.
			testutil.Equal(t, len(g.deleted), 0)
			testutil.Equal(t, g.kinds(), []string{"Deployment", "LimitRange"})
			testutil.Equal(t, g.objs["LimitRange"], solLR)
			co := workerContainer(t, c.ID, g.objs["Deployment"])
			oreq := quantityMap(t, c.ID+" requests", resOf(co)["requests"].(map[string]any))
			olim := quantityMap(t, c.ID+" limits", resOf(co)["limits"].(map[string]any))
			oreqCPU, olimCPU := oreq["cpu"], olim["cpu"]
			testutil.Equal(t, oreqCPU.String(), "300m")
			testutil.Equal(t, olimCPU.String(), "300m")
			want := resWith(c.ID, func(res map[string]any) {
				res["requests"].(map[string]any)["cpu"] = resOf(co)["requests"].(map[string]any)["cpu"]
				res["limits"].(map[string]any)["cpu"] = resOf(co)["limits"].(map[string]any)["cpu"]
			})
			testutil.Equal(t, g.objs["Deployment"], want)
			if fit(c.ID, replicas, oreq, olim) {
				t.Error("two oversized Pods must not fit the quota")
			}
			if !fit(c.ID+" (one Pod)", 1, oreq, olim) {
				t.Error("one oversized Pod must fit the quota, so that the case holds whichever Pod is admitted")
			}
		case "limitrange-missing":
			// Exactly the solution without the LimitRange.
			testutil.Equal(t, len(g.deleted), 0)
			testutil.Equal(t, g.kinds(), []string{"Deployment"})
			testutil.Equal(t, g.objs["Deployment"], solDeploy)
		case "alternative-decimal-quantities":
			// Same amounts in other notations, for the Deployment and the
			// LimitRange; every graded literal is spelled differently.
			testutil.Equal(t, len(g.deleted), 0)
			testutil.Equal(t, g.kinds(), []string{"Deployment", "LimitRange"})
			ac := workerContainer(t, c.ID, g.objs["Deployment"])
			for _, key := range []string{"requests", "limits"} {
				got := resOf(ac)[key].(map[string]any)
				ref := resOf(solC)[key].(map[string]any)
				testutil.Equal(t, quantityStrings(quantityMap(t, c.ID+" "+key, got)), quantityStrings(quantityMap(t, "solution "+key, ref)))
				for res, v := range got {
					if fmt.Sprint(v) == fmt.Sprint(ref[res]) {
						t.Errorf("%s: %s.%s is written like the solution (%v); the alternative must use another notation", c.ID, key, res, v)
					}
				}
			}
			same := deepCopyObject(g.objs["Deployment"])
			workerContainer(t, c.ID, same)["resources"] = resOf(solC)
			testutil.Equal(t, same, solDeploy)
			alt := g.objs["LimitRange"]
			altEntries := nestedMap(t, alt, "spec")["limits"].([]any)
			testutil.Equal(t, len(altEntries), 1)
			altE := altEntries[0].(map[string]any)
			testutil.Equal(t, lrOf(altE), canonQuantities(t, wantLR))
			for _, key := range []string{"defaultRequest", "default"} {
				for res, v := range altE[key].(map[string]any) {
					if fmt.Sprint(v) == fmt.Sprint(lr0[key].(map[string]any)[res]) {
						t.Errorf("%s: LimitRange %s.%s is written like the solution (%v)", c.ID, key, res, v)
					}
				}
			}
			delete(alt, "spec")
			delete(solLR, "spec")
			testutil.Equal(t, alt, solLR)
		default:
			t.Errorf("resources-001 case %s has no contract", c.ID)
		}
	}

	// The explanation covers the trap and the wrong answers in Korean.
	for _, lit := range []string{"FailedCreate", "ERROR", "team-quota"} {
		if !strings.Contains(string(p.Files["explanation.ko.md"]), lit) {
			t.Errorf("the explanation must mention %q", lit)
		}
	}
}
