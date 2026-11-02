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
}

// securityCapabilities and securityInitialScores pin each package's declared
// capabilities and the score of the untouched environment.
var securityCapabilities = map[string][]string{
	"custom-resource-001": nil,
}

var securityInitialScores = map[string]int{
	"custom-resource-001": 0,
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
