//go:build linux

package integration

import (
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
	"sigs.k8s.io/yaml"
)

var observabilityIDs = []string{"api-migration-001", "probes-001", "diagnostics-001"}

// observabilityCases maps problem ID -> case ID -> expected score. Every bank
// problem has a row; the table pins the exact case set of each package.
var observabilityCases = map[string]map[string]int{
	"api-migration-001": {
		"apiversion-only":               0,
		"fixed-not-applied":             2,
		"wrong-hpa-target":              3,
		"stale-scale-target":            3,
		"selector-drops-tier":           4,
		"alternative-match-expressions": 5,
	},
	"probes-001": {
		"probes-deleted":            2,
		"readiness-port-kept":       3,
		"liveness-delay-no-startup": 4,
		"command-shortened":         4,
		"liveness-wrong-port":       3,
		"alternative-named-port":    5,
	},
	"diagnostics-001": {
		"sleep-instead":                      1,
		"answer-only":                        2,
		"hardcoded-endpoint":                 4,
		"wrong-pod-answer":                   4,
		"alternative-items-mapping":          5,
		"alternative-configmap-edit-restart": 5,
	},
}

// observabilityCapabilities and observabilityInitialScores pin each package's declared capabilities and the score
// of the untouched environment.
var observabilityCapabilities = map[string][]string{
	"api-migration-001": nil,
	"probes-001":        nil,
	"diagnostics-001":   {"metrics"},
}

var observabilityInitialScores = map[string]int{
	"api-migration-001": 0,
	"probes-001":        1,
	"diagnostics-001":   1,
}

func caseScore(p model.Problem, id string) int {
	for _, c := range p.Cases {
		if c.ID == id {
			return c.ExpectedScore
		}
	}
	return -1
}

func initialBelowMax(p model.Problem) bool {
	return p.InitialScore < sumPoints([]model.Problem{p})
}

func TestObservabilityProblems(t *testing.T) {
	for _, p := range loadGroup(t, observabilityIDs) {
		t.Run(p.ID, func(t *testing.T) {
			testutil.Equal(t, p.Domain, model.DomainObservability)
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
			for id, want := range observabilityCases[p.ID] {
				testutil.Equal(t, caseScore(p, id), want)
			}
			testutil.Equal(t, len(p.Cases), len(observabilityCases[p.ID]))
			testutil.Equal(t, initialBelowMax(p), true)
			testutil.Equal(t, p.InitialScore, observabilityInitialScores[p.ID])
			if want := observabilityCapabilities[p.ID]; !slices.Equal(p.Capabilities, want) {
				t.Errorf("capabilities = %v, want %v", p.Capabilities, want)
			}
			for id, want := range observabilityCases[p.ID] {
				if !strings.HasPrefix(id, "alternative-") && want >= 5 {
					t.Fatalf("wrong case %s earns full credit", id)
				}
			}
			for _, c := range p.Cases {
				testutil.Equal(t, len(c.Expected), len(p.Criteria))
			}
			// Ruling R1: every http criterion observes for 60 seconds.
			for _, c := range p.Criteria {
				if c.Kind == model.CriterionHTTP {
					testutil.Equal(t, c.Timeout, 60*time.Second)
				}
			}
			// Private file sources are copied raw, so they must not rely on
			// parameter rendering. Every apply manifest, wherever it is used
			// (setup, solution or case), must also avoid the unserved legacy
			// GroupVersions as an object's own apiVersion. (A scaleTargetRef
			// string is data, not a served GroupVersion, and is checked by
			// TestObservabilityAPIMigrationContracts.)
			frozen, err := problem.Freeze(p, seeds[0])
			testutil.NoError(t, err)
			groups := [][]model.Action{p.Setup, p.Solution}
			for _, c := range p.Cases {
				groups = append(groups, c.Actions)
			}
			for _, g := range groups {
				for _, a := range g {
					switch a.Kind {
					case model.ActionFile:
						src, _ := a.Args["source"].(string)
						b, ok := p.Files[src]
						if !ok {
							t.Fatalf("file source %q is not part of the package", src)
						}
						if strings.Contains(string(b), "${") {
							t.Errorf("file source %s contains an unrendered ${param}", src)
						}
					case model.ActionApply:
						path, _ := a.Args["path"].(string)
						if _, ok := p.Files[path]; !ok {
							t.Fatalf("apply manifest %q is not part of the package", path)
						}
						for _, o := range applyObjects(t, p, frozen, a) {
							av, _ := o["apiVersion"].(string)
							for _, legacy := range []string{"extensions/v1beta1", "v1beta1", "v2beta1", "v2beta2"} {
								if strings.HasSuffix(av, legacy) {
									t.Errorf("apply manifest %s has an object with unserved API version %q", path, av)
								}
							}
						}
					}
				}
			}
		})
	}
}

// applyObjects renders one apply action into its objects with the frozen
// parameters, exactly like the validator does.
func applyObjects(t *testing.T, p model.Problem, f model.FrozenProblem, a model.Action) []map[string]any {
	t.Helper()
	built, err := author.BuildActions(p.Files, f.Parameters, []model.Action{a})
	testutil.NoError(t, err)
	var out []map[string]any
	for _, step := range built {
		out = append(out, step.Objects...)
	}
	return out
}

// Group totals: three 5-point problems, every ID present. Removing a package
// or a criterion must fail before spending time on a cluster.
func TestObservabilityBankContent(t *testing.T) {
	ps := loadGroup(t, observabilityIDs)
	testutil.Equal(t, len(ps), 3)
	testutil.Equal(t, sumPoints(ps), 15)
	got := map[string]bool{}
	for _, p := range ps {
		got[p.ID] = true
		testutil.Equal(t, p.Domain, model.DomainObservability)
	}
	for _, id := range observabilityIDs {
		if !got[id] {
			t.Errorf("missing %s", id)
		}
	}
}

func TestObservabilityCoverage(t *testing.T) {
	var cov struct {
		Groups []struct {
			ID       string   `json:"id"`
			Domain   string   `json:"domain"`
			Count    int      `json:"count"`
			Points   int      `json:"points"`
			Problems []string `json:"problems"`
		} `json:"groups"`
	}
	b, err := os.ReadFile(filepath.Join(bankRoot, "curriculum", "coverage.yaml"))
	testutil.NoError(t, err)
	testutil.NoError(t, yaml.UnmarshalStrict(b, &cov))
	found := 0
	for _, g := range cov.Groups {
		if g.Domain != model.DomainObservability {
			continue
		}
		found++
		testutil.Equal(t, g.ID, "observability")
		testutil.Equal(t, g.Count, 3)
		testutil.Equal(t, g.Points, 15)
		got, want := slices.Clone(g.Problems), slices.Clone(observabilityIDs)
		slices.Sort(got)
		slices.Sort(want)
		testutil.Equal(t, got, want)
	}
	testutil.Equal(t, found, 1)
}

func TestObservabilityBankPreflight(t *testing.T) {
	preflightBank(t, loadGroup(t, observabilityIDs))
}

// The legacy manifests are only ever workspace files: the server rejects both
// unserved GVKs, and apply of one would be an author error.
func TestObservabilityAPIMigrationContracts(t *testing.T) {
	p := loadGroup(t, []string{"api-migration-001"})[0]
	legacy := string(p.Files["setup/provided/legacy/app.yaml"])
	for _, want := range []string{"extensions/v1beta1", "autoscaling/v2beta1", "targetAverageUtilization"} {
		if !strings.Contains(legacy, want) {
			t.Errorf("legacy manifest lacks %q", want)
		}
	}
	if strings.Contains(legacy, "namespace:") {
		t.Error("legacy manifest must not hard-code a namespace")
	}
	for _, a := range append(append([]model.Action{}, p.Setup...), p.Solution...) {
		if a.Kind == model.ActionApply && a.Args["path"] == "setup/provided/legacy/app.yaml" {
			t.Error("legacy manifest must never be applied")
		}
	}
	f, err := problem.Freeze(p, 1)
	testutil.NoError(t, err)
	// The HPA is graded on the scale target's API (file and live) and, live,
	// on the metric target type; each graded value is stated in the question.
	wantFields := map[string]map[string]any{
		"manifest-hpa": {"spec.scaleTargetRef.apiVersion": "apps/v1", "spec.scaleTargetRef.kind": "Deployment",
			"spec.metrics[type=Resource].resource.name": "cpu"},
		"hpa-live": {"spec.scaleTargetRef.apiVersion": "apps/v1", "spec.scaleTargetRef.kind": "Deployment",
			"spec.metrics[type=Resource].resource.name":        "cpu",
			"spec.metrics[type=Resource].resource.target.type": "Utilization"},
	}
	for _, c := range p.Criteria {
		for path, value := range wantFields[c.ID] {
			found := false
			for _, field := range c.Args["fields"].([]any) {
				if m := field.(map[string]any); m["path"] == path && m["equals"] == value {
					found = true
				}
			}
			if !found {
				t.Errorf("criterion %s does not require %s == %v", c.ID, path, value)
			}
		}
	}
	if q := string(p.Files[problem.QuestionFile]); !strings.Contains(q, "`apps/v1` Deployment `inventory`") {
		t.Error("question must say that the HPA targets the apps/v1 Deployment inventory")
	}
	// The graders select the first Resource metric, so the question must say
	// that the CPU metric has to be the (first) Resource metric.
	if q := string(p.Files[problem.QuestionFile]); !strings.Contains(q, "first `Resource` metric") || strings.Contains(q, "Equivalent selectors and extra HPA settings that keep these values are accepted") {
		t.Error("question must say the CPU metric is the first Resource metric and name the accepted extras")
	}
	// scaleTargetRef.apiVersion of every applied HPA, per case/solution. Only
	// the stale-scale-target case may keep the legacy value; no object may use
	// an unserved GroupVersion of its own.
	targets := func(actions []model.Action) []string {
		var out []string
		for _, a := range actions {
			if a.Kind != model.ActionApply {
				continue
			}
			for _, o := range applyObjects(t, p, f, a) {
				if av, _ := o["apiVersion"].(string); strings.Contains(av, "beta") {
					t.Errorf("applied %v object uses unserved API %q", o["kind"], av)
				}
				if o["kind"] != "HorizontalPodAutoscaler" {
					continue
				}
				ref, _ := o["spec"].(map[string]any)["scaleTargetRef"].(map[string]any)
				ver, _ := ref["apiVersion"].(string)
				out = append(out, ver)
			}
		}
		return out
	}
	testutil.Equal(t, targets(p.Solution), []string{"apps/v1"})
	for _, c := range p.Cases {
		want := []string{"apps/v1"}
		if c.ID == "stale-scale-target" {
			want = []string{"extensions/v1beta1"}
		}
		got := targets(c.Actions)
		if len(got) == 0 && c.ID != "stale-scale-target" {
			continue // file-only case: nothing reaches the cluster
		}
		testutil.Equal(t, got, want)
	}
}

// The diagnostics application command is graded literally by command-unchanged
// and must be identical in the handed-out setup and in every correct answer;
// the symptom-hiding cases must differ from it.
func TestObservabilityDiagnosticsContracts(t *testing.T) {
	p := loadGroup(t, []string{"diagnostics-001"})[0]
	f, err := problem.Freeze(p, 1)
	testutil.NoError(t, err)
	var want []any
	for _, c := range p.Criteria {
		if c.ID != "command-unchanged" {
			continue
		}
		for _, field := range c.Args["fields"].([]any) {
			m := field.(map[string]any)
			if m["path"] == "spec.template.spec.containers[name=app].command" {
				want = m["equals"].([]any)
			}
		}
	}
	if len(want) != 3 {
		t.Fatalf("command-unchanged criterion lacks the expected command list: %v", want)
	}
	command := func(actions []model.Action) [][]any {
		var out [][]any
		for _, a := range actions {
			if a.Kind != model.ActionApply {
				continue
			}
			for _, o := range applyObjects(t, p, f, a) {
				if o["kind"] != "Deployment" || o["metadata"].(map[string]any)["name"] != "orders" {
					continue
				}
				for _, c := range o["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any) {
					if c.(map[string]any)["name"] == "app" {
						out = append(out, c.(map[string]any)["command"].([]any))
					}
				}
			}
		}
		return out
	}
	same := func(label string, actions []model.Action) {
		got := command(actions)
		if len(got) != 1 || !slices.Equal(got[0], want) {
			t.Errorf("%s: orders command %v differs from the command-unchanged criterion", label, got)
		}
	}
	same("setup", p.Setup)
	same("solution", p.Solution)
	for _, c := range p.Cases {
		switch c.ID {
		case "alternative-items-mapping", "alternative-configmap-edit-restart":
			same("case "+c.ID, c.Actions)
		case "sleep-instead", "hardcoded-endpoint":
			got := command(c.Actions)
			if len(got) != 1 || slices.Equal(got[0], want) {
				t.Errorf("case %s must change the command, got %v", c.ID, got)
			}
		}
	}
}

// A correct liveness probe must survive one slow probe on a loaded host, and
// the question must tell a learner that a crashed Pod needs a restart.
func TestObservabilityRobustnessContracts(t *testing.T) {
	probes := loadGroup(t, []string{"probes-001"})[0]
	f, err := problem.Freeze(probes, 1)
	testutil.NoError(t, err)
	correct := append([]model.Action{}, probes.Solution...)
	for _, c := range probes.Cases {
		if c.ID == "alternative-named-port" {
			correct = append(correct, c.Actions...)
		}
	}
	checked := 0
	for _, a := range correct {
		if a.Kind != model.ActionApply {
			continue
		}
		for _, o := range applyObjects(t, probes, f, a) {
			if o["kind"] != "Deployment" {
				continue
			}
			for _, c := range o["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any) {
				live := c.(map[string]any)["livenessProbe"].(map[string]any)
				testutil.Equal(t, live["failureThreshold"], any(float64(3)))
				checked++
			}
		}
	}
	testutil.Equal(t, checked, 2)

	diag := loadGroup(t, []string{"diagnostics-001"})[0]
	if q := string(diag.Files[problem.QuestionFile]); !strings.Contains(q, "kubectl rollout restart deployment/orders") {
		t.Error("diagnostics question must name the restart command for an already crashed Pod")
	}
}

func probeContainers(t *testing.T, p model.Problem, f model.FrozenProblem, actions []model.Action) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, a := range actions {
		if a.Kind != model.ActionApply {
			continue
		}
		for _, o := range applyObjects(t, p, f, a) {
			if o["kind"] != "Deployment" {
				continue
			}
			for _, c := range o["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any) {
				out = append(out, c.(map[string]any))
			}
		}
	}
	return out
}

// probes-001: the image and command are graded through the startup-probe
// criterion (a shortened command hides the startup delay), a wrong liveness
// port must outlast the stability window with Kubernetes default timings, and
// the wrong-probe cases keep their point.
func TestObservabilityProbesContracts(t *testing.T) {
	p := loadGroup(t, []string{"probes-001"})[0]
	f, err := problem.Freeze(p, 1)
	testutil.NoError(t, err)
	var wantCommand []any
	var wantImage any
	for _, c := range p.Criteria {
		switch c.ID {
		case "startup-probe":
			for _, field := range c.Args["fields"].([]any) {
				m := field.(map[string]any)
				switch m["path"] {
				case "spec.template.spec.containers[name=web].command":
					wantCommand, _ = m["equals"].([]any)
				case "spec.template.spec.containers[name=web].image":
					wantImage = m["equals"]
				}
			}
		case "rollout-stable":
			testutil.Equal(t, c.Args["stable_for"], any("40s"))
			testutil.Equal(t, c.Timeout, 120*time.Second)
		}
	}
	if len(wantCommand) != 3 || wantImage != "docker.io/library/busybox:1.37.0" {
		t.Fatalf("startup-probe criterion must pin the provided image and command, got %v %v", wantImage, wantCommand)
	}
	for _, name := range []string{problem.QuestionFile, problem.ExplanationFile} {
		if !strings.Contains(string(p.Files[name]), "40") {
			t.Errorf("%s must state the 40-second stability window", name)
		}
	}
	if q := string(p.Files[problem.QuestionFile]); !strings.Contains(q, "image and command") {
		t.Error("question must say the image and command are part of the startup point")
	}
	check := func(label string, actions []model.Action, sameCommand bool) []map[string]any {
		cs := probeContainers(t, p, f, actions)
		if len(cs) != 1 {
			if len(actions) > 0 && label != "case probes-deleted" {
				t.Fatalf("%s: want one web container, got %d", label, len(cs))
			}
		}
		for _, c := range cs {
			got, _ := c["command"].([]any)
			if sameCommand != slices.Equal(got, wantCommand) {
				t.Errorf("%s: command %v, same as the criterion want %v", label, got, sameCommand)
			}
			if sameCommand && c["image"] != wantImage {
				t.Errorf("%s: image %v", label, c["image"])
			}
		}
		return cs
	}
	check("setup", p.Setup, true)
	check("solution", p.Solution, true)
	for _, c := range p.Cases {
		switch c.ID {
		case "command-shortened":
			cs := check("case "+c.ID, c.Actions, false)
			startup := cs[0]["startupProbe"].(map[string]any)
			testutil.Equal(t, startup["httpGet"].(map[string]any)["path"], any("/healthz"))
			testutil.Equal(t, startup["failureThreshold"], any(float64(1)))
		case "liveness-wrong-port":
			cs := check("case "+c.ID, c.Actions, true)
			live := cs[0]["livenessProbe"].(map[string]any)
			testutil.Equal(t, live["httpGet"].(map[string]any)["path"], any("/healthz"))
			testutil.Equal(t, live["httpGet"].(map[string]any)["port"], any(float64(8081)))
			for _, key := range []string{"periodSeconds", "failureThreshold", "initialDelaySeconds", "timeoutSeconds"} {
				if _, ok := live[key]; ok {
					t.Errorf("liveness-wrong-port must keep the default %s", key)
				}
			}
			for _, probe := range []string{"readinessProbe", "startupProbe"} {
				testutil.Equal(t, cs[0][probe].(map[string]any)["httpGet"].(map[string]any)["port"], any(float64(8080)))
			}
		case "liveness-delay-no-startup":
			cs := check("case "+c.ID, c.Actions, true)
			if _, ok := cs[0]["startupProbe"]; ok {
				t.Error("liveness-delay-no-startup must not have a startup probe")
			}
			testutil.Equal(t, cs[0]["livenessProbe"].(map[string]any)["failureThreshold"], any(float64(3)))
		default:
			check("case "+c.ID, c.Actions, true)
		}
	}
}

// diagnostics-001: the answers directory is handed out, so a learner does not
// need to create it, and the stability window is disclosed.
func TestObservabilityDiagnosticsHandout(t *testing.T) {
	p := loadGroup(t, []string{"diagnostics-001"})[0]
	if _, ok := p.Files["setup/provided/answers/README.txt"]; !ok {
		t.Error("answers/ directory must be shipped in setup/provided")
	}
	q := string(p.Files[problem.QuestionFile])
	if !strings.Contains(q, "10-second stability window") {
		t.Error("question must state the orders-ready stability window")
	}
	if strings.Contains(q, "args") {
		t.Error("question must not mention the ungraded args")
	}
}
