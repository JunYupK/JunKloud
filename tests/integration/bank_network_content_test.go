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

var networkIDs = []string{"service-001", "ingress-001", "networkpolicy-001"}

// networkPending lists the networking packages that a later task adds. A
// pending package may be missing; every other package must exist. The task
// that adds a package removes its ID here (TestNetworkTablesHavePackages
// fails while an existing package is still listed as pending).
var networkPending = []string{}

// networkPoints pins the points of each networking problem.
var networkPoints = map[string]int{
	"service-001":       6,
	"ingress-001":       6,
	"networkpolicy-001": 8,
}

// networkCases maps problem ID -> case ID -> expected score. Each package adds
// its row with its task; the table pins the exact case set of each package.
var networkCases = map[string]map[string]int{
	"service-001": {
		"pods-relabeled":                2,
		"wrong-target-port":             4,
		"clusterip-only":                5,
		"empty-service":                 3,
		"pods-labelled-directly":        5,
		"alternative-named-target-port": 6,
	},
	"ingress-001": {
		"all-to-web":                4,
		"missing-api-host":          4,
		"wrong-backend-port":        4,
		"wrong-class":               2,
		"alternative-default-class": 6,
		"alternative-port-name":     6,
	},
	// Predictions for an enforcing host: not certified (the sandbox kernel
	// lacks nft_queue, so NetworkPolicy is not enforced here).
	"networkpolicy-001": {
		"deny-all":                  3,
		"allow-all-ingress":         5,
		"batch-relabeled":           7,
		"missing-egress":            7,
		"and-or-split":              7,
		"alternative-single-policy": 8,
	},
}

// networkCapabilities and networkInitialScores pin each package's declared
// capabilities and the score of the untouched environment.
var networkCapabilities = map[string][]string{
	"service-001":       nil,
	"ingress-001":       {"ingress"},
	"networkpolicy-001": {"network-policy"},
}

var networkInitialScores = map[string]int{
	"service-001": 3,
	"ingress-001": 2,
	// Prediction, not certified: with no policy every denied source connects.
	"networkpolicy-001": 4,
}

// networkCompetencies are the three official items of spec section 3.1 that
// the networking domain covers, used exactly.
var networkCompetencies = []string{
	"NetworkPolicy",
	"Service access and troubleshooting",
	"Ingress",
}

// presentNetworkIDs returns the networking IDs whose package exists, so the
// bank tests grow with the packages instead of failing before they are added.
func presentNetworkIDs(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, id := range networkIDs {
		_, err := os.Stat(filepath.Join(bankRoot, "problems", id))
		switch {
		case err == nil:
			out = append(out, id)
		case !os.IsNotExist(err):
			t.Fatal(err)
		case !slices.Contains(networkPending, id):
			// Only a pending package may be missing.
			t.Errorf("problems/%s is missing and is not pending", id)
		}
	}
	return out
}

// A pending package may be missing; a package that is not pending must exist,
// and a pending one that exists must be removed from networkPending.
func TestNetworkTablesHavePackages(t *testing.T) {
	for _, id := range networkIDs {
		_, err := os.Stat(filepath.Join(bankRoot, "problems", id))
		pending := slices.Contains(networkPending, id)
		switch {
		case err == nil && pending:
			t.Errorf("problems/%s exists: remove it from networkPending", id)
		case err != nil && !pending:
			t.Errorf("problems/%s: %v", id, err)
		}
	}
	for _, id := range networkPending {
		if !slices.Contains(networkIDs, id) {
			t.Errorf("networkPending names %s which is not in networkIDs", id)
		}
	}
	for id := range networkCases {
		if !slices.Contains(networkIDs, id) {
			t.Errorf("networkCases has a row for %s which is not in networkIDs", id)
		}
	}
	for _, id := range networkIDs {
		if _, ok := networkPoints[id]; !ok {
			t.Errorf("networkPoints has no row for %s", id)
		}
		if _, ok := networkCapabilities[id]; !ok {
			t.Errorf("networkCapabilities has no row for %s", id)
		}
	}
	testutil.Equal(t, len(networkPoints), len(networkIDs))
	testutil.Equal(t, len(networkCapabilities), len(networkIDs))
}

// Every present networking package is a networking problem with the points of
// its row and uses only the official competency strings.
func TestNetworkCompetencies(t *testing.T) {
	for _, p := range loadGroup(t, presentNetworkIDs(t)) {
		if len(p.Competencies) == 0 {
			t.Errorf("%s declares no competency", p.ID)
		}
		for _, c := range p.Competencies {
			if !slices.Contains(networkCompetencies, c) {
				t.Errorf("%s uses competency %q which is not one of the 3 official items", p.ID, c)
			}
		}
	}
}

func TestNetworkProblems(t *testing.T) {
	for _, p := range loadGroup(t, presentNetworkIDs(t)) {
		t.Run(p.ID, func(t *testing.T) {
			testutil.Equal(t, p.Domain, model.DomainNetworking)
			testutil.Equal(t, sumPoints([]model.Problem{p}), networkPoints[p.ID])
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
			want, ok := networkCases[p.ID]
			if !ok {
				t.Fatalf("networkCases has no row for %s", p.ID)
			}
			for id, score := range want {
				testutil.Equal(t, caseScore(p, id), score)
			}
			testutil.Equal(t, len(p.Cases), len(want))
			testutil.Equal(t, initialBelowMax(p), true)
			testutil.Equal(t, p.InitialScore, networkInitialScores[p.ID])
			if want := networkCapabilities[p.ID]; !slices.Equal(p.Capabilities, want) {
				t.Errorf("capabilities = %v, want %v", p.Capabilities, want)
			}
			full := networkPoints[p.ID]
			for id, score := range want {
				if !strings.HasPrefix(id, "alternative-") && score >= full {
					t.Fatalf("wrong case %s earns full credit", id)
				}
				if strings.HasPrefix(id, "alternative-") && score != full {
					t.Fatalf("alternative case %s must earn full credit, has %d of %d", id, score, full)
				}
			}
			// Full status vectors: the initial state and every case state
			// every criterion, and only PASS or FAIL is an acceptable
			// expectation (a validated stage never ends in ERROR).
			vectors := map[string]map[string]string{"initial": p.InitialExpected}
			for _, c := range p.Cases {
				m := map[string]string{}
				for id, st := range c.Expected {
					m[id] = string(st)
				}
				vectors["case "+c.ID] = m
			}
			for name, vec := range vectors {
				testutil.Equal(t, len(vec), len(p.Criteria))
				for _, crit := range p.Criteria {
					switch vec[crit.ID] {
					case string(model.StatusPass), string(model.StatusFail):
					default:
						t.Errorf("%s: expected status of criterion %s is %q, want PASS or FAIL (ERROR is never an expected status)", name, crit.ID, vec[crit.ID])
					}
				}
			}
			// Ruling R1: every network criterion declares 60 seconds (the
			// minimum is 45 s; 60 s for an Ingress) and depends on the
			// ready criterion of its target workload, because EndpointSlices
			// are read once and not waited for.
			byID := map[string]model.Criterion{}
			for _, c := range p.Criteria {
				byID[c.ID] = c
			}
			for _, c := range p.Criteria {
				switch c.Kind {
				case model.CriterionHTTP, model.CriterionTCP, model.CriterionNetworkPolicy:
					testutil.Equal(t, c.Timeout, 60*time.Second)
					if !dependsOnKind(byID, c, model.CriterionReady) {
						t.Errorf("network criterion %s does not depend on a ready criterion", c.ID)
					}
					if bad := reservedSourceLabels(c.Args); len(bad) > 0 {
						t.Errorf("network criterion %s uses reserved source labels %v", c.ID, bad)
					}
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

// dependsOnKind reports whether c depends, directly or through other
// criteria, on a criterion of the given kind.
func dependsOnKind(byID map[string]model.Criterion, c model.Criterion, kind string) bool {
	seen := map[string]bool{}
	var walk func(model.Criterion) bool
	walk = func(c model.Criterion) bool {
		for _, d := range c.DependsOn {
			if seen[d] {
				continue
			}
			seen[d] = true
			dep, ok := byID[d]
			if !ok {
				continue
			}
			if dep.Kind == kind || walk(dep) {
				return true
			}
		}
		return false
	}
	return walk(c)
}

// reservedSourceLabels returns the junkloud.io/ source label keys of a network
// criterion's args (single source or expectations).
func reservedSourceLabels(args map[string]any) []string {
	var bad []string
	check := func(v any) {
		src, _ := v.(map[string]any)
		labels, _ := src["labels"].(map[string]any)
		for k := range labels {
			if strings.HasPrefix(k, "junkloud.io/") {
				bad = append(bad, k)
			}
		}
	}
	check(args["source"])
	exps, _ := args["expectations"].([]any)
	for _, e := range exps {
		em, _ := e.(map[string]any)
		check(em["source"])
	}
	return bad
}

func TestNetworkBankPreflight(t *testing.T) {
	preflightBank(t, loadGroup(t, presentNetworkIDs(t)))
}

// smokeNetworkCriteria returns the criteria the networking smoke evaluates and
// the IDs it leaves out. The smoke keeps every criterion of a networking
// problem; only when network-policy is unenforced does it drop the
// network-policy criteria and, transitively, every criterion that depends on
// one of them.
func smokeNetworkCriteria(all []model.Criterion, policyUnenforced bool) (kept []model.Criterion, dropped []string) {
	if !policyUnenforced {
		return all, nil
	}
	drop := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, c := range all {
			if drop[c.ID] {
				continue
			}
			isDropped := c.Kind == model.CriterionNetworkPolicy
			for _, d := range c.DependsOn {
				isDropped = isDropped || drop[d]
			}
			if isDropped {
				drop[c.ID] = true
				changed = true
			}
		}
	}
	for _, c := range all {
		if drop[c.ID] {
			dropped = append(dropped, c.ID)
		} else {
			kept = append(kept, c)
		}
	}
	return kept, dropped
}

func TestNetworkSmokeCriteriaFilter(t *testing.T) {
	ids := func(cs []model.Criterion) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.ID)
		}
		return out
	}
	all := []model.Criterion{
		{ID: "ready", Kind: model.CriterionReady},
		{ID: "allowed", Kind: model.CriterionHTTP, DependsOn: []string{"ready"}},
		{ID: "blocked", Kind: model.CriterionNetworkPolicy, DependsOn: []string{"ready"}},
		{ID: "after-blocked", Kind: model.CriterionField, DependsOn: []string{"blocked"}},
		{ID: "after-after", Kind: model.CriterionLogs, DependsOn: []string{"after-blocked", "ready"}},
		{ID: "independent", Kind: model.CriterionField},
	}
	kept, dropped := smokeNetworkCriteria(all, false)
	testutil.Equal(t, ids(kept), ids(all))
	testutil.Equal(t, len(dropped), 0)
	kept, dropped = smokeNetworkCriteria(all, true)
	testutil.Equal(t, ids(kept), []string{"ready", "allowed", "independent"})
	testutil.Equal(t, dropped, []string{"blocked", "after-blocked", "after-after"})
}

// ----------------------------------------------------------- service-001

// networkObjects renders a group of actions: the objects it applies in order,
// keyed "Kind/name", and the objects it deletes (delete-owned) in order.
type networkGroup struct {
	order   []string
	objs    map[string]map[string]any
	deleted []string
}

func networkActions(t *testing.T, p model.Problem, f model.FrozenProblem, label string, actions []model.Action) networkGroup {
	t.Helper()
	built, err := author.BuildActions(p.Files, f.Parameters, actions)
	testutil.NoError(t, err)
	g := networkGroup{objs: map[string]map[string]any{}}
	for _, a := range built {
		switch a.Kind {
		case model.ActionApply:
			for _, o := range a.Objects {
				key := fmt.Sprint(o["kind"]) + "/" + nestedString(t, o, "metadata", "name")
				if _, dup := g.objs[key]; dup {
					t.Fatalf("%s: two %s objects", label, key)
				}
				g.objs[key] = o
				g.order = append(g.order, key)
			}
		case model.ActionDeleteOwned:
			for _, ref := range a.Targets {
				g.deleted = append(g.deleted, ref.Kind+"/"+ref.Name)
			}
		default:
			t.Fatalf("%s: service-001 uses only apply and delete-owned, got %s", label, a.Kind)
		}
	}
	return g
}

func TestNetworkServiceContracts(t *testing.T) {
	p := loadGroup(t, []string{"service-001"})[0]
	f, err := problem.Freeze(p, 1)
	testutil.NoError(t, err)
	ns := f.Namespace
	q := string(p.Files[problem.QuestionFile])
	const (
		image          = "docker.io/library/busybox:1.37.0"
		catalogScript  = "mkdir -p /www && printf catalog-ok > /www/index.html && exec httpd -f -p 8080 -h /www"
		frontendScript = `while true; do if b=$(wget -qO- -T 2 http://catalog/ 2>/dev/null); then echo "frontend fetched $b"; else echo "frontend fetch failed"; fi; sleep 3; done`
	)

	testutil.Equal(t, p.Type, "troubleshoot")
	testutil.Equal(t, p.Difficulty, "medium")
	testutil.Equal(t, p.Competencies, []string{"Service access and troubleshooting"})
	testutil.Equal(t, p.Scope.Namespaces == nil, true)
	testutil.Equal(t, len(p.Scope.ClusterResources), 0)
	testutil.Equal(t, fmt.Sprint(p.Parameters["namespace"]), "[jk-service-001-a jk-service-001-b]")

	// Setup: the catalog Deployment, the broken Service and the frontend.
	setup := networkActions(t, p, f, "setup", p.Setup)
	testutil.Equal(t, setup.order, []string{"Deployment/catalog", "Service/catalog", "Deployment/frontend"})
	testutil.Equal(t, len(setup.deleted), 0)
	catalog := setup.objs["Deployment/catalog"]
	testutil.Equal(t, nestedString(t, catalog, "metadata", "namespace"), ns)
	testutil.Equal(t, nestedString(t, catalog, "spec", "replicas"), "2")
	sameJSON(t, "catalog selector", nestedMap(t, catalog, "spec", "selector")["matchLabels"], map[string]any{"app": "catalog"})
	sameJSON(t, "catalog template labels", nestedMap(t, catalog, "spec", "template", "metadata")["labels"], map[string]any{"app": "catalog", "tier": "backend"})
	cc := appContainer(t, "catalog", catalog)
	testutil.Equal(t, cc["name"], any("catalog"))
	testutil.Equal(t, cc["image"], any(image))
	testutil.Equal(t, cc["imagePullPolicy"], any("Never"))
	testutil.Equal(t, cc["command"], any([]any{"sh", "-c", catalogScript}))
	sameJSON(t, "catalog ports", cc["ports"], []any{map[string]any{"name": "http", "containerPort": 8080}})
	svc := setup.objs["Service/catalog"]
	testutil.Equal(t, nestedString(t, svc, "metadata", "namespace"), ns)
	sameJSON(t, "setup Service spec", svc["spec"], map[string]any{
		"type":     "ClusterIP",
		"selector": map[string]any{"app": "catalog-api"},
		"ports":    []any{map[string]any{"name": "http", "port": 80, "targetPort": 9090, "protocol": "TCP"}},
	})
	frontend := setup.objs["Deployment/frontend"]
	testutil.Equal(t, nestedString(t, frontend, "metadata", "namespace"), ns)
	testutil.Equal(t, nestedString(t, frontend, "spec", "replicas"), "1")
	sameJSON(t, "frontend selector", nestedMap(t, frontend, "spec", "selector")["matchLabels"], map[string]any{"app": "frontend"})
	sameJSON(t, "frontend template labels", nestedMap(t, frontend, "spec", "template", "metadata")["labels"], map[string]any{"app": "frontend"})
	fc := appContainer(t, "frontend", frontend)
	testutil.Equal(t, fc["name"], any("frontend"))
	testutil.Equal(t, fc["image"], any(image))
	testutil.Equal(t, fc["imagePullPolicy"], any("Never"))
	testutil.Equal(t, fc["command"], any([]any{"sh", "-c", frontendScript}))

	// Criteria: six, one point each, in the pinned order and shape.
	crit := map[string]model.Criterion{}
	var order []string
	for _, c := range p.Criteria {
		crit[c.ID] = c
		order = append(order, c.ID)
		testutil.Equal(t, c.Points, 1)
	}
	testutil.Equal(t, order, []string{"catalog-ready", "workloads-unchanged", "service-routes", "service-shape", "frontend-ready", "frontend-reaches"})
	cr := crit["catalog-ready"]
	testutil.Equal(t, cr.Kind, model.CriterionReady)
	testutil.Equal(t, cr.Target.APIVersion+" "+cr.Target.Kind+" "+cr.Target.Name, "apps/v1 Deployment catalog")
	testutil.Equal(t, fmt.Sprint(cr.Args["replicas"]), "2")
	testutil.Equal(t, cr.Timeout, 90*time.Second)
	wu := crit["workloads-unchanged"]
	testutil.Equal(t, wu.Kind, model.CriterionField)
	testutil.Equal(t, wu.Target.Kind+" "+wu.Target.Name, "Deployment catalog")
	sameJSON(t, "workloads-unchanged args", wu.Args, map[string]any{"fields": []any{
		map[string]any{"path": "spec.template.metadata.labels.app", "equals": "catalog"},
		map[string]any{"path": "spec.template.metadata.labels.tier", "equals": "backend"},
		map[string]any{"path": "spec.template.spec.containers[name=catalog].image", "equals": image},
		map[string]any{"path": "spec.template.spec.containers[name=catalog].command", "equals": []any{"sh", "-c", catalogScript}},
	}})
	sr := crit["service-routes"]
	testutil.Equal(t, sr.Kind, model.CriterionHTTP)
	testutil.Equal(t, sr.Target, model.ObjectRef{APIVersion: "v1", Kind: "Service", Namespace: "${namespace}", Name: "catalog"})
	sameJSON(t, "service-routes args", sr.Args, map[string]any{"port": 80, "path": "/", "body_marker": "catalog-ok"})
	testutil.Equal(t, sr.Timeout, 60*time.Second)
	testutil.Equal(t, sr.DependsOn, []string{"catalog-ready"})
	st := crit["service-shape"]
	testutil.Equal(t, st.Kind, model.CriterionField)
	testutil.Equal(t, st.Target, model.ObjectRef{APIVersion: "v1", Kind: "Service", Namespace: "${namespace}", Name: "catalog"})
	// The selector is folded into this criterion: keeping the broken selector
	// app: catalog-api and labelling Pods by hand must lose a point even
	// though the orphaned Pods answer the routing check.
	sameJSON(t, "service-shape args", st.Args, map[string]any{"fields": []any{
		map[string]any{"path": "spec.selector.app", "equals": "catalog"},
		map[string]any{"path": "spec.type", "equals": "NodePort"},
		map[string]any{"path": "spec.ports[name=http].nodePort", "equals": 30080},
	}})
	fr := crit["frontend-ready"]
	testutil.Equal(t, fr.Kind, model.CriterionReady)
	testutil.Equal(t, fr.Target.APIVersion+" "+fr.Target.Kind+" "+fr.Target.Name, "apps/v1 Deployment frontend")
	testutil.Equal(t, fmt.Sprint(fr.Args["replicas"]), "1")
	testutil.Equal(t, fr.Timeout, 90*time.Second)
	re := crit["frontend-reaches"]
	testutil.Equal(t, re.Kind, model.CriterionLogs)
	testutil.Equal(t, re.Target.APIVersion+" "+re.Target.Kind+" "+re.Target.Name, "apps/v1 Deployment frontend")
	sameJSON(t, "frontend-reaches args", re.Args, map[string]any{"container": "frontend", "matches": []any{"^frontend fetched catalog-ok$"}, "wait": true})
	testutil.Equal(t, re.Timeout, 60*time.Second)
	testutil.Equal(t, re.DependsOn, []string{"service-routes", "frontend-ready"})
	// The window: the loop fetches every 3 seconds and the check observes for
	// 60, so a fix is seen well inside the window.
	testutil.Equal(t, strings.Contains(frontendScript, "sleep 3"), true)

	// Solution: one apply of the repaired Service.
	solution := networkActions(t, p, f, "solution", p.Solution)
	testutil.Equal(t, solution.order, []string{"Service/catalog"})
	testutil.Equal(t, len(solution.deleted), 0)
	if len(p.Solution) != 1 {
		t.Errorf("the solution must be one apply, got %d actions", len(p.Solution))
	}
	wantSvc := func(mut func(spec map[string]any)) map[string]any {
		out := deepCopyObject(svc)
		spec := nestedMap(t, out, "spec")
		spec["type"] = "NodePort"
		spec["selector"] = map[string]any{"app": "catalog", "tier": "backend"}
		spec["ports"] = []any{map[string]any{"name": "http", "port": 80, "targetPort": 8080, "protocol": "TCP", "nodePort": 30080}}
		if mut != nil {
			mut(spec)
		}
		return out
	}
	solSvc := solution.objs["Service/catalog"]
	sameJSON(t, "solution Service", solSvc, wantSvc(nil))

	// Cases: each differs from the solution only in its named mistake.
	for _, c := range p.Cases {
		g := networkActions(t, p, f, c.ID, c.Actions)
		switch c.ID {
		case "pods-relabeled":
			// The Deployment is recreated with the other label (its selector
			// is immutable); the Service keeps its wrong port and is not touched.
			testutil.Equal(t, g.deleted, []string{"Deployment/catalog"})
			testutil.Equal(t, g.order, []string{"Deployment/catalog"})
			want := deepCopyObject(catalog)
			nestedMap(t, want, "spec", "selector")["matchLabels"] = map[string]any{"app": "catalog-api"}
			nestedMap(t, want, "spec", "template", "metadata")["labels"] = map[string]any{"app": "catalog-api", "tier": "backend"}
			sameJSON(t, c.ID+" Deployment", g.objs["Deployment/catalog"], want)
		case "wrong-target-port":
			testutil.Equal(t, len(g.deleted), 0)
			testutil.Equal(t, g.order, []string{"Service/catalog"})
			want := wantSvc(func(spec map[string]any) {
				spec["ports"].([]any)[0].(map[string]any)["targetPort"] = 9090
			})
			sameJSON(t, c.ID+" Service", g.objs["Service/catalog"], want)
		case "clusterip-only":
			testutil.Equal(t, len(g.deleted), 0)
			testutil.Equal(t, g.order, []string{"Service/catalog"})
			want := wantSvc(func(spec map[string]any) {
				spec["type"] = "ClusterIP"
				delete(spec["ports"].([]any)[0].(map[string]any), "nodePort")
			})
			sameJSON(t, c.ID+" Service", g.objs["Service/catalog"], want)
		case "empty-service":
			// Removing a field from a setup-created Service: delete-owned,
			// then apply (a server-side apply cannot remove it).
			testutil.Equal(t, g.deleted, []string{"Service/catalog"})
			testutil.Equal(t, g.order, []string{"Service/catalog"})
			want := wantSvc(func(spec map[string]any) { delete(spec, "selector") })
			sameJSON(t, c.ID+" Service", g.objs["Service/catalog"], want)
		case "pods-labelled-directly":
			// The direct-relabel route: two bare Pods outside the Deployment,
			// labelled with the old selector, built from the catalog Pod
			// template (same container); the Service keeps the broken selector
			// and is otherwise the solution. Nothing is deleted, no scope entry
			// is needed (Pods live in the problem namespace).
			testutil.Equal(t, len(g.deleted), 0)
			testutil.Equal(t, g.order, []string{"Pod/catalog-orphan-1", "Pod/catalog-orphan-2", "Service/catalog"})
			podSpec := nestedMap(t, catalog, "spec", "template", "spec")
			for _, name := range []string{"catalog-orphan-1", "catalog-orphan-2"} {
				wantPod := map[string]any{
					"apiVersion": "v1", "kind": "Pod",
					"metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]any{"app": "catalog-api", "tier": "backend"}},
					"spec":     deepCopyObject(podSpec),
				}
				sameJSON(t, c.ID+" "+name, g.objs["Pod/"+name], wantPod)
			}
			want := wantSvc(func(spec map[string]any) { spec["selector"] = map[string]any{"app": "catalog-api"} })
			sameJSON(t, c.ID+" Service", g.objs["Service/catalog"], want)
		case "alternative-named-target-port":
			testutil.Equal(t, len(g.deleted), 0)
			testutil.Equal(t, g.order, []string{"Service/catalog"})
			want := wantSvc(func(spec map[string]any) {
				spec["selector"] = map[string]any{"app": "catalog"}
				spec["ports"].([]any)[0].(map[string]any)["targetPort"] = "http"
			})
			sameJSON(t, c.ID+" Service", g.objs["Service/catalog"], want)
		default:
			t.Errorf("service-001 case %s has no contract", c.ID)
		}
	}
	// The pods-relabeled case is the only one that touches a workload: the
	// frontend and the Service of every other case stay as setup created them.
	for _, c := range p.Cases {
		g := networkActions(t, p, f, c.ID, c.Actions)
		if _, has := g.objs["Deployment/frontend"]; has {
			t.Errorf("case %s must not touch the frontend", c.ID)
		}
		if c.ID != "pods-relabeled" {
			if _, has := g.objs["Deployment/catalog"]; has {
				t.Errorf("case %s must not touch the catalog Deployment", c.ID)
			}
		}
	}

	// The question states every graded literal and nothing ungraded is
	// forbidden: exactly one "do not change" sentence, about the catalog
	// Pod template, which workloads-unchanged grades.
	for _, lit := range []string{
		"`catalog`", "`frontend`", "`${namespace}`", "2 replicas", "`8080`", "`http`", "`80`", "`NodePort`", "`30080`",
		"every 3 seconds", "`frontend fetched catalog-ok`", "`docker.io/library/busybox:1.37.0`", "the provided command",
		"`app: catalog`", "`tier: backend`", "Do not change the labels `app: catalog` and `tier: backend`, the image or the command of the Pod template of Deployment `catalog`",
		"must select the Pods of Deployment `catalog` by their label `app: catalog`", "`selector` key `app` must have the value `catalog`", "labelling Pods by hand to match the old selector is not the fix",
		"has the `selector` key `app` with the value `catalog`, the type `NodePort`", "any other selector value, type or node port loses the point",
		"2 Ready replicas", "1 Ready replica", "10-second stability window, 90-second observation limit",
		"60-second limit", "this point needs the first one, because it judges the Pods of that Deployment",
		"this point needs the third and the fifth one",
	} {
		if !strings.Contains(q, lit) {
			t.Errorf("question must state %q", lit)
		}
	}
	testutil.Equal(t, strings.Count(q, "not change"), 1)
	for _, ungraded := range []string{"Ingress", "replicas of `frontend`", "ClusterIP", "port-forward", "curl", "kubectl"} {
		if strings.Contains(q, ungraded) {
			t.Errorf("the question must not mention %q: nothing grades it", ungraded)
		}
	}

	// Handout and explanation.
	readme := string(p.Files["setup/provided/README.txt"])
	for _, lit := range []string{
		"Deployment catalog", "Service catalog", "Deployment frontend", "port 80", "port 8080", "NodePort", "30080", "named http",
		"Do not change the labels app: catalog and tier: backend, the image or the command of the Pod template of Deployment catalog", "frontend fetched catalog-ok",
		"label app: catalog", "labelling Pods by hand to match the old selector is not the fix",
	} {
		if !strings.Contains(readme, lit) {
			t.Errorf("the handout must state %q", lit)
		}
	}
	testutil.Equal(t, strings.Count(readme, "not change"), 1)
	for _, ungraded := range []string{"Ingress", "ClusterIP", "kubectl", "curl"} {
		if strings.Contains(readme, ungraded) {
			t.Errorf("the handout must not mention %q: nothing grades it", ungraded)
		}
	}
	expl := string(p.Files["explanation.ko.md"])
	if strings.Contains(expl, "service-type") || strings.Contains(expl, "kubectl get endpoints catalog") {
		t.Error("the explanation uses a stale criterion name or the deprecated kubectl get endpoints")
	}
	for _, lit := range []string{
		"EndpointSlice", "selector", "targetPort", "NodePort", "30080", "kubectl get endpointslices", "kubectl get pods --show-labels",
		"pods-relabeled", "wrong-target-port", "clusterip-only", "empty-service", "pods-labelled-directly", "alternative-named-target-port",
		"workloads-unchanged", "service-routes", "service-shape", "conditions.ready", "kubectl label pods", "frontend-reaches", "frontend-ready", "catalog-ready",
		"selector를 바꿀 수 없", "https://kubernetes.io/docs/concepts/services-networking/service/",
	} {
		if !strings.Contains(expl, lit) {
			t.Errorf("the explanation must mention %q", lit)
		}
	}
}

// ----------------------------------------------------------- ingress-001

// ingressRule is one host rule of the shop Ingress: path -> Service name.
type ingressRule struct {
	host  string
	paths [][2]string // {path, service}
}

// buildIngress renders the shop Ingress of the solution with the given class
// ("" omits it), rules and backend port (number or name).
func buildIngress(ns, class string, rules []ingressRule, port any) map[string]any {
	var rs []any
	for _, r := range rules {
		var ps []any
		for _, p := range r.paths {
			ps = append(ps, map[string]any{
				"path": p[0], "pathType": "Prefix",
				"backend": map[string]any{"service": map[string]any{"name": p[1], "port": port}},
			})
		}
		rs = append(rs, map[string]any{"host": r.host, "http": map[string]any{"paths": ps}})
	}
	spec := map[string]any{"rules": rs}
	if class != "" {
		spec["ingressClassName"] = class
	}
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
		"metadata": map[string]any{"name": "shop", "namespace": ns},
		"spec":     spec,
	}
}

func TestNetworkIngressContracts(t *testing.T) {
	p := loadGroup(t, []string{"ingress-001"})[0]
	f, err := problem.Freeze(p, 1)
	testutil.NoError(t, err)
	ns := f.Namespace
	q := string(p.Files[problem.QuestionFile])
	const (
		image     = "docker.io/library/busybox:1.37.0"
		webScript = "mkdir -p /www/api && printf web-ok > /www/index.html && printf web-ok > /www/api/index.html && exec httpd -f -p 8080 -h /www"
		apiScript = "mkdir -p /www/api && printf api-ok > /www/index.html && printf api-ok > /www/api/index.html && exec httpd -f -p 8080 -h /www"
		class     = "cloud-provider-kind"
	)
	shopHost := "shop." + ns + ".example"
	apiHost := "api." + ns + ".example"

	testutil.Equal(t, p.Type, "build")
	testutil.Equal(t, p.Difficulty, "medium")
	testutil.Equal(t, p.Competencies, []string{"Ingress"})
	testutil.Equal(t, p.Scope.Namespaces == nil, true)
	testutil.Equal(t, len(p.Scope.ClusterResources), 0)
	testutil.Equal(t, fmt.Sprint(p.Parameters["namespace"]), "[jk-ingress-001-a jk-ingress-001-b]")

	// Setup: two backends and their Services; the Ingress is the learner's.
	setup := networkActions(t, p, f, "setup", p.Setup)
	testutil.Equal(t, setup.order, []string{"Deployment/shop-web", "Service/shop-web", "Deployment/shop-api", "Service/shop-api"})
	testutil.Equal(t, len(setup.deleted), 0)
	for _, b := range []struct{ name, script string }{{"shop-web", webScript}, {"shop-api", apiScript}} {
		d := setup.objs["Deployment/"+b.name]
		testutil.Equal(t, nestedString(t, d, "metadata", "namespace"), ns)
		testutil.Equal(t, nestedString(t, d, "spec", "replicas"), "1")
		sameJSON(t, b.name+" selector", nestedMap(t, d, "spec", "selector")["matchLabels"], map[string]any{"app": b.name})
		sameJSON(t, b.name+" template labels", nestedMap(t, d, "spec", "template", "metadata")["labels"], map[string]any{"app": b.name})
		c := appContainer(t, b.name, d)
		testutil.Equal(t, c["name"], any("app"))
		testutil.Equal(t, c["image"], any(image))
		testutil.Equal(t, c["imagePullPolicy"], any("Never"))
		testutil.Equal(t, c["command"], any([]any{"sh", "-c", b.script}))
		sameJSON(t, b.name+" ports", c["ports"], []any{map[string]any{"name": "http", "containerPort": 8080}})
		s := setup.objs["Service/"+b.name]
		testutil.Equal(t, nestedString(t, s, "metadata", "namespace"), ns)
		sameJSON(t, b.name+" Service spec", s["spec"], map[string]any{
			"type":     "ClusterIP",
			"selector": map[string]any{"app": b.name},
			"ports":    []any{map[string]any{"name": "http", "port": 80, "targetPort": 8080, "protocol": "TCP"}},
		})
	}
	// The markers differ, so a wrong backend that answers 200 loses the point:
	// both backends answer 200 on / and on /api/, each with its own marker
	// (the pinned scripts above serve web-ok / api-ok on both paths).

	// Criteria: six, one point each, in the pinned order and shape.
	crit := map[string]model.Criterion{}
	var order []string
	for _, c := range p.Criteria {
		crit[c.ID] = c
		order = append(order, c.ID)
		testutil.Equal(t, c.Points, 1)
	}
	testutil.Equal(t, order, []string{"web-ready", "api-ready", "root-route", "api-path-route", "api-host-route", "ingress-shape"})
	for _, r := range []struct{ id, name string }{{"web-ready", "shop-web"}, {"api-ready", "shop-api"}} {
		c := crit[r.id]
		testutil.Equal(t, c.Kind, model.CriterionReady)
		testutil.Equal(t, c.Target.APIVersion+" "+c.Target.Kind+" "+c.Target.Name, "apps/v1 Deployment "+r.name)
		testutil.Equal(t, fmt.Sprint(c.Args["replicas"]), "1")
		testutil.Equal(t, c.Timeout, 90*time.Second)
	}
	ingressRef := model.ObjectRef{APIVersion: "networking.k8s.io/v1", Kind: "Ingress", Namespace: "${namespace}", Name: "shop"}
	for _, r := range []struct {
		id, host, path, marker, dep string
	}{
		{"root-route", "shop.${namespace}.example", "/", "web-ok", "web-ready"},
		{"api-path-route", "shop.${namespace}.example", "/api/", "api-ok", "api-ready"},
		{"api-host-route", "api.${namespace}.example", "/", "api-ok", "api-ready"},
	} {
		c := crit[r.id]
		testutil.Equal(t, c.Kind, model.CriterionHTTP)
		testutil.Equal(t, c.Target, ingressRef)
		// Each route criterion sets its own Host header and path.
		sameJSON(t, r.id+" args", c.Args, map[string]any{"host": r.host, "path": r.path, "body_marker": r.marker})
		testutil.Equal(t, c.Timeout, 60*time.Second)
		testutil.Equal(t, c.DependsOn, []string{r.dep})
	}
	// The shape criterion closes the routes the three requests cannot see: a
	// rule without host or a default backend answers both hosts by itself.
	sh := crit["ingress-shape"]
	testutil.Equal(t, sh.Kind, model.CriterionField)
	testutil.Equal(t, sh.Target, ingressRef)
	sameJSON(t, "ingress-shape args", sh.Args, map[string]any{"fields": []any{
		map[string]any{"path": "spec.ingressClassName", "equals": class},
		map[string]any{"path": "spec.rules[host=shop.${namespace}.example].host", "exists": true},
		map[string]any{"path": "spec.rules[host=api.${namespace}.example].host", "exists": true},
	}})

	// Solution: one apply of the Ingress.
	solution := networkActions(t, p, f, "solution", p.Solution)
	testutil.Equal(t, solution.order, []string{"Ingress/shop"})
	testutil.Equal(t, len(solution.deleted), 0)
	if len(p.Solution) != 1 {
		t.Errorf("the solution must be one apply, got %d actions", len(p.Solution))
	}
	shopRules := []ingressRule{
		{shopHost, [][2]string{{"/", "shop-web"}, {"/api", "shop-api"}}},
		{apiHost, [][2]string{{"/", "shop-api"}}},
	}
	sameJSON(t, "solution Ingress", solution.objs["Ingress/shop"], buildIngress(ns, class, shopRules, map[string]any{"number": 80}))

	// Cases: each differs from the solution only in its named mistake. The
	// Ingress is not part of the setup, so every case is a plain apply.
	for _, c := range p.Cases {
		g := networkActions(t, p, f, c.ID, c.Actions)
		testutil.Equal(t, len(g.deleted), 0)
		testutil.Equal(t, g.order, []string{"Ingress/shop"})
		var want map[string]any
		switch c.ID {
		case "all-to-web":
			want = buildIngress(ns, class, []ingressRule{
				{shopHost, [][2]string{{"/", "shop-web"}, {"/api", "shop-web"}}},
				{apiHost, [][2]string{{"/", "shop-web"}}},
			}, map[string]any{"number": 80})
		case "missing-api-host":
			want = buildIngress(ns, class, shopRules[:1], map[string]any{"number": 80})
		case "wrong-backend-port":
			want = buildIngress(ns, class, shopRules, map[string]any{"number": 80})
			for _, r := range nestedSlice(t, want, "spec", "rules") {
				for _, pp := range r.(map[string]any)["http"].(map[string]any)["paths"].([]any) {
					svc := pp.(map[string]any)["backend"].(map[string]any)["service"].(map[string]any)
					if svc["name"] == "shop-api" {
						svc["port"] = map[string]any{"number": 8080}
					}
				}
			}
		case "wrong-class":
			// A class no controller of this environment serves: nothing
			// answers, the Ingress never gets an address.
			want = buildIngress(ns, "nginx", shopRules, map[string]any{"number": 80})
		case "alternative-default-class":
			// The cluster's default IngressClass (cloud-provider-kind) is
			// set by admission, so omitting the class ends in the same object.
			want = buildIngress(ns, "", shopRules, map[string]any{"number": 80})
		case "alternative-port-name":
			want = buildIngress(ns, class, shopRules, map[string]any{"name": "http"})
			want["metadata"].(map[string]any)["annotations"] = map[string]any{"example.com/owner": "shop-team"}
		default:
			t.Errorf("ingress-001 case %s has no contract", c.ID)
			continue
		}
		sameJSON(t, c.ID+" Ingress", g.objs["Ingress/shop"], want)
	}

	// The question states every graded literal. It has no "do not change"
	// sentence: nothing grades the provided workloads.
	for _, lit := range []string{
		"`shop-web`", "`shop-api`", "`${namespace}`", "port `80`", "port `8080`", "`web-ok`", "`api-ok`",
		"Ingress `shop`", "`cloud-provider-kind`", "`spec.ingressClassName`", "`Prefix`",
		"`shop.${namespace}.example`", "`api.${namespace}.example`", "`/api`", "`/api/`", "`Host`",
		"1 Ready replica", "10-second stability window, 90-second observation limit",
		"60-second limit", "this point needs the first one", "this point needs the second one",
		"a rule for each of the hosts", "HTTP 200",
		"default IngressClass of the cluster is `cloud-provider-kind`", "any other class is not served",
		"created without a class", "an update never fills in a class", "each on both of the paths `/` and `/api/`",
		"and the path `/` returns HTTP 200 with a body containing `api-ok`",
	} {
		if !strings.Contains(q, lit) {
			t.Errorf("question must state %q", lit)
		}
	}
	testutil.Equal(t, strings.Count(q, "not change"), 0)
	for _, ungraded := range []string{"kubectl", "curl", "port-forward", "NetworkPolicy"} {
		if strings.Contains(q, ungraded) {
			t.Errorf("the question must not mention %q: nothing grades it", ungraded)
		}
	}
	readme := string(p.Files["setup/provided/README.txt"])
	for _, lit := range []string{
		"Deployment shop-web", "Deployment shop-api", "Service shop-web", "Service shop-api", "port 80", "port 8080", "web-ok", "api-ok",
		"Ingress shop", "cloud-provider-kind", "ingressClassName", "Prefix", "shop.${namespace}.example", "api.${namespace}.example", "/api",
	} {
		if !strings.Contains(readme, lit) {
			t.Errorf("the handout must state %q", lit)
		}
	}
	testutil.Equal(t, strings.Count(readme, "not change"), 0)

	expl := string(p.Files["explanation.ko.md"])
	for _, lit := range []string{
		"IngressClass", "ingressClassName", "pathType", "Prefix", "Host", "kubectl get ingress", "kubectl describe ingress", "ADDRESS",
		"all-to-web", "missing-api-host", "wrong-backend-port", "wrong-class", "alternative-default-class", "alternative-port-name",
		"is-default-class", "admission", "만들 때", "수정(update)", "<error: ...>", "<none>", "Linux Docker 호스트",
		"root-route", "api-path-route", "api-host-route", "ingress-shape", "web-ready", "api-ready", "web-ok", "api-ok",
		"https://kubernetes.io/docs/concepts/services-networking/ingress/",
	} {
		if !strings.Contains(expl, lit) {
			t.Errorf("the explanation must mention %q", lit)
		}
	}
}

func nestedSlice(t *testing.T, o map[string]any, path ...string) []any {
	t.Helper()
	v, found, err := unstructured.NestedFieldNoCopy(o, path...)
	s, ok := v.([]any)
	if err != nil || !found || !ok {
		t.Fatalf("%v is not a list in %v (%v)", path, o, err)
	}
	return s
}

// ------------------------------------------------------ networkpolicy-001

// npLabels builds a label map from key/value pairs.
func npLabels(kv ...string) map[string]any {
	m := map[string]any{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func npSel(kv ...string) map[string]any { return map[string]any{"matchLabels": npLabels(kv...)} }

func npPorts(ps ...[2]any) []any {
	var out []any
	for _, p := range ps {
		out = append(out, map[string]any{"protocol": p[0], "port": p[1]})
	}
	return out
}

// npPolicy renders a NetworkPolicy the way the package files spell it.
func npPolicy(ns, name string, podSel map[string]any, types []any, ingress, egress []any) map[string]any {
	spec := map[string]any{"podSelector": podSel, "policyTypes": types}
	if ingress != nil {
		spec["ingress"] = ingress
	}
	if egress != nil {
		spec["egress"] = egress
	}
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec":     spec,
	}
}

// npSolution renders the three policies of the reference solution:
// api-from-web, api-from-monitor and web-egress.
func npSolution(ns string) map[string]map[string]any {
	mon := map[string]any{"namespaceSelector": npSel("kubernetes.io/metadata.name", ns+"-mon"), "podSelector": npSel("app", "monitor")}
	return map[string]map[string]any{
		"api-from-web": npPolicy(ns, "api-from-web", npSel("role", "api"), []any{"Ingress"}, []any{
			map[string]any{"from": []any{map[string]any{"podSelector": npSel("role", "web")}}, "ports": npPorts([2]any{"TCP", 8080})},
		}, nil),
		"api-from-monitor": npPolicy(ns, "api-from-monitor", npSel("role", "api"), []any{"Ingress"}, []any{
			map[string]any{"from": []any{mon}, "ports": npPorts([2]any{"TCP", 8080})},
		}, nil),
		"web-egress": npPolicy(ns, "web-egress", npSel("role", "web"), []any{"Egress"}, nil, []any{
			map[string]any{"to": []any{map[string]any{"podSelector": npSel("role", "api")}}, "ports": npPorts([2]any{"TCP", 8080})},
			map[string]any{"to": []any{map[string]any{"namespaceSelector": npSel("kubernetes.io/metadata.name", "kube-system")}}, "ports": npPorts([2]any{"UDP", 53}, [2]any{"TCP", 53})},
		}),
	}
}

func TestNetworkPolicyContracts(t *testing.T) {
	p := loadGroup(t, []string{"networkpolicy-001"})[0]
	f, err := problem.Freeze(p, 1)
	testutil.NoError(t, err)
	ns := f.Namespace
	mon := ns + "-mon"
	q := string(p.Files[problem.QuestionFile])
	const (
		image       = "docker.io/library/busybox:1.37.0"
		sleepScript = "exec sleep 86400"
	)
	serveScript := func(marker string) string {
		return "mkdir -p /www && printf " + marker + " > /www/index.html && exec httpd -f -p 8080 -h /www"
	}

	testutil.Equal(t, p.Type, "behavioral")
	testutil.Equal(t, p.Difficulty, "hard")
	testutil.Equal(t, p.Competencies, []string{"NetworkPolicy"})
	testutil.Equal(t, p.Capabilities, []string{"network-policy"})
	testutil.Equal(t, fmt.Sprint(p.Parameters["namespace"]), "[jk-networkpolicy-001-a jk-networkpolicy-001-b]")
	// The monitor and intruder live in a second namespace that the problem owns.
	testutil.Equal(t, p.Scope.Namespaces, []string{"${namespace}-mon"})
	testutil.Equal(t, f.Definition.Scope.Namespaces, []string{mon})
	testutil.Equal(t, len(p.Scope.ClusterResources), 0)

	// Setup: four workloads and two Services in the problem namespace, two
	// workloads in the -mon namespace. No NetworkPolicy exists.
	setup := networkActions(t, p, f, "setup", p.Setup)
	testutil.Equal(t, setup.order, []string{
		"Deployment/api", "Service/api", "Deployment/ledger", "Service/ledger", "Deployment/web", "Deployment/batch",
		"Deployment/monitor", "Deployment/intruder",
	})
	testutil.Equal(t, len(setup.deleted), 0)
	type workload struct {
		name, ns, key, val, script string
	}
	workloads := []workload{
		{"api", ns, "app", "api", serveScript("api-ok")},
		{"ledger", ns, "app", "ledger", serveScript("ledger-ok")},
		{"web", ns, "app", "web", sleepScript},
		{"batch", ns, "app", "batch", sleepScript},
		{"monitor", mon, "app", "monitor", sleepScript},
		{"intruder", mon, "component", "intruder", sleepScript},
	}
	for _, w := range workloads {
		d := setup.objs["Deployment/"+w.name]
		testutil.Equal(t, nestedString(t, d, "metadata", "namespace"), w.ns)
		testutil.Equal(t, nestedString(t, d, "spec", "replicas"), "1")
		sameJSON(t, w.name+" selector", nestedMap(t, d, "spec", "selector")["matchLabels"], npLabels(w.key, w.val))
		wantLabels := npLabels(w.key, w.val)
		if w.ns == ns {
			wantLabels["role"] = w.name // the role label the policies select
		}
		sameJSON(t, w.name+" template labels", nestedMap(t, d, "spec", "template", "metadata")["labels"], wantLabels)
		c := appContainer(t, w.name, d)
		testutil.Equal(t, c["name"], any(w.name))
		testutil.Equal(t, c["image"], any(image))
		testutil.Equal(t, c["imagePullPolicy"], any("Never"))
		testutil.Equal(t, c["command"], any([]any{"sh", "-c", w.script}))
		if w.name == "api" || w.name == "ledger" {
			sameJSON(t, w.name+" ports", c["ports"], []any{map[string]any{"name": "http", "containerPort": 8080}})
			// No kubelet probe: a default-deny policy must not be able to
			// flip the Pod to NotReady (node-to-Pod traffic is CNI-dependent),
			// because the deny-all prediction relies on api and ledger staying Ready.
			if _, has := c["readinessProbe"]; has {
				t.Errorf("%s must not declare a readinessProbe", w.name)
			}
		}
	}
	for _, s := range []string{"api", "ledger"} {
		svc := setup.objs["Service/"+s]
		testutil.Equal(t, nestedString(t, svc, "metadata", "namespace"), ns)
		sameJSON(t, s+" Service spec", svc["spec"], map[string]any{
			"type":     "ClusterIP",
			"selector": npLabels("app", s),
			"ports":    []any{map[string]any{"name": "http", "port": 80, "targetPort": 8080, "protocol": "TCP"}},
		})
	}
	// Criteria: eight, one point each, in the pinned order and shape.
	crit := map[string]model.Criterion{}
	var order []string
	for _, c := range p.Criteria {
		crit[c.ID] = c
		order = append(order, c.ID)
		testutil.Equal(t, c.Points, 1)
	}
	testutil.Equal(t, order, []string{"api-ready", "ledger-ready", "web-reaches-api", "batch-blocked", "monitor-allowed", "unlabeled-blocked", "web-egress-blocked", "workloads-unchanged"})
	for _, r := range []struct{ id, name string }{{"api-ready", "api"}, {"ledger-ready", "ledger"}} {
		c := crit[r.id]
		testutil.Equal(t, c.Kind, model.CriterionReady)
		testutil.Equal(t, c.Target, model.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "${namespace}", Name: r.name})
		testutil.Equal(t, fmt.Sprint(c.Args["replicas"]), "1")
		testutil.Equal(t, c.Timeout, 90*time.Second)
	}
	svcRef := func(name string) model.ObjectRef {
		return model.ObjectRef{APIVersion: "v1", Kind: "Service", Namespace: "${namespace}", Name: name}
	}
	web := map[string]any{"labels": map[string]any{"role": "web"}}
	batch := map[string]any{"labels": map[string]any{"role": "batch"}}
	allowAPI := func(name string, source map[string]any) map[string]any {
		return map[string]any{"name": name, "expect": "allow", "source": source, "port": 80, "http": map[string]any{"path": "/", "body_marker": "api-ok"}}
	}
	deny := func(name string, source map[string]any) map[string]any {
		return map[string]any{"name": name, "expect": "deny", "source": source, "port": 80}
	}
	wr := crit["web-reaches-api"]
	testutil.Equal(t, wr.Kind, model.CriterionHTTP)
	testutil.Equal(t, wr.Target, svcRef("api"))
	sameJSON(t, "web-reaches-api args", wr.Args, map[string]any{"source": web, "port": 80, "path": "/", "body_marker": "api-ok"})
	testutil.Equal(t, wr.Timeout, 60*time.Second)
	testutil.Equal(t, wr.DependsOn, []string{"api-ready"})
	for _, r := range []struct {
		id    string
		exps  []any
		dep   string
		other string
	}{
		{"batch-blocked", []any{allowAPI("web-allowed", web), deny("batch-denied", batch)}, "api-ready", "api"},
		{"monitor-allowed", []any{
			allowAPI("monitor-allowed", map[string]any{"namespace": "${namespace}-mon", "labels": map[string]any{"app": "monitor"}}),
			deny("intruder-denied", map[string]any{"namespace": "${namespace}-mon", "labels": map[string]any{"component": "intruder"}}),
			deny("monitor-lookalike-denied", map[string]any{"labels": map[string]any{"app": "monitor"}}),
		}, "api-ready", "api"},
		{"unlabeled-blocked", []any{allowAPI("web-allowed", web), deny("unlabeled-denied", map[string]any{})}, "api-ready", "api"},
		{"web-egress-blocked", []any{
			map[string]any{"name": "batch-allowed", "expect": "allow", "source": batch, "port": 80, "http": map[string]any{"path": "/", "body_marker": "ledger-ok"}},
			deny("web-denied", web),
		}, "ledger-ready", "ledger"},
	} {
		c := crit[r.id]
		testutil.Equal(t, c.Kind, model.CriterionNetworkPolicy)
		testutil.Equal(t, c.Target, svcRef(r.other))
		sameJSON(t, r.id+" args", c.Args, map[string]any{"expectations": r.exps})
		testutil.Equal(t, c.Timeout, 60*time.Second)
		testutil.Equal(t, c.DependsOn, []string{r.dep})
	}
	wu := crit["workloads-unchanged"]
	testutil.Equal(t, wu.Kind, model.CriterionField)
	testutil.Equal(t, wu.Target, model.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "${namespace}", Name: "batch"})
	sameJSON(t, "workloads-unchanged args", wu.Args, map[string]any{"fields": []any{
		map[string]any{"path": "spec.template.metadata.labels.app", "equals": "batch"},
		map[string]any{"path": "spec.template.metadata.labels.role", "equals": "batch"},
		map[string]any{"path": "spec.template.spec.containers[name=batch].image", "equals": image},
		map[string]any{"path": "spec.template.spec.containers[name=batch].command", "equals": []any{"sh", "-c", sleepScript}},
	}})
	testutil.Equal(t, len(wu.DependsOn), 0)

	// Solution: one apply of the three policies, in the problem namespace only.
	solution := networkActions(t, p, f, "solution", p.Solution)
	testutil.Equal(t, solution.order, []string{"NetworkPolicy/api-from-web", "NetworkPolicy/api-from-monitor", "NetworkPolicy/web-egress"})
	testutil.Equal(t, len(solution.deleted), 0)
	if len(p.Solution) != 1 {
		t.Errorf("the solution must be one apply, got %d actions", len(p.Solution))
	}
	sol := npSolution(ns)
	for name, want := range sol {
		sameJSON(t, "solution "+name, solution.objs["NetworkPolicy/"+name], want)
	}

	// Cases: each differs from the solution only in its named mistake.
	for _, c := range p.Cases {
		g := networkActions(t, p, f, c.ID, c.Actions)
		testutil.Equal(t, len(g.deleted), 0)
		switch c.ID {
		case "deny-all":
			testutil.Equal(t, g.order, []string{"NetworkPolicy/default-deny"})
			sameJSON(t, c.ID+" policy", g.objs["NetworkPolicy/default-deny"],
				npPolicy(ns, "default-deny", map[string]any{}, []any{"Ingress", "Egress"}, nil, nil))
		case "allow-all-ingress":
			// Ingress to api from everywhere, with the correct egress policy.
			testutil.Equal(t, g.order, []string{"NetworkPolicy/api-allow-all", "NetworkPolicy/web-egress"})
			sameJSON(t, c.ID+" ingress", g.objs["NetworkPolicy/api-allow-all"],
				npPolicy(ns, "api-allow-all", npSel("role", "api"), []any{"Ingress"}, []any{map[string]any{}}, nil))
			sameJSON(t, c.ID+" egress", g.objs["NetworkPolicy/web-egress"], sol["web-egress"])
		case "batch-relabeled":
			// The correct policies, and batch Pods that carry role: web. The
			// probe Pods carry the declared labels, so only workloads-unchanged moves.
			testutil.Equal(t, g.order, []string{"NetworkPolicy/api-from-web", "NetworkPolicy/api-from-monitor", "NetworkPolicy/web-egress", "Deployment/batch"})
			for name, want := range sol {
				sameJSON(t, c.ID+" "+name, g.objs["NetworkPolicy/"+name], want)
			}
			want := deepCopyObject(setup.objs["Deployment/batch"])
			nestedMap(t, want, "spec", "template", "metadata")["labels"] = npLabels("app", "batch", "role", "web")
			sameJSON(t, c.ID+" Deployment", g.objs["Deployment/batch"], want)
		case "missing-egress":
			testutil.Equal(t, g.order, []string{"NetworkPolicy/api-from-web", "NetworkPolicy/api-from-monitor"})
			for _, name := range g.order {
				sameJSON(t, c.ID+" "+name, g.objs[name], sol[strings.TrimPrefix(name, "NetworkPolicy/")])
			}
		case "and-or-split":
			// The monitor peer written as two separate from entries
			// (namespaceSelector OR podSelector) instead of one ANDed peer:
			// every Pod of the -mon namespace (the intruder too) and every
			// app=monitor Pod of the problem namespace (the lookalike) pass.
			testutil.Equal(t, g.order, []string{"NetworkPolicy/api-from-web", "NetworkPolicy/api-from-monitor", "NetworkPolicy/web-egress"})
			sameJSON(t, c.ID+" api-from-web", g.objs["NetworkPolicy/api-from-web"], sol["api-from-web"])
			sameJSON(t, c.ID+" api-from-monitor", g.objs["NetworkPolicy/api-from-monitor"],
				npPolicy(ns, "api-from-monitor", npSel("role", "api"), []any{"Ingress"}, []any{
					map[string]any{"from": []any{
						map[string]any{"namespaceSelector": npSel("kubernetes.io/metadata.name", mon)},
						map[string]any{"podSelector": npSel("app", "monitor")},
					}, "ports": npPorts([2]any{"TCP", 8080})},
				}, nil))
			sameJSON(t, c.ID+" web-egress", g.objs["NetworkPolicy/web-egress"], sol["web-egress"])
		case "alternative-single-policy":
			// One policy with two from entries (the second ANDs the namespace
			// and the Pod selector), and a separate egress policy that names
			// the DNS Pods and the api Pods differently.
			testutil.Equal(t, g.order, []string{"NetworkPolicy/api-ingress", "NetworkPolicy/web-egress"})
			sameJSON(t, c.ID+" ingress", g.objs["NetworkPolicy/api-ingress"],
				npPolicy(ns, "api-ingress", npSel("role", "api"), []any{"Ingress"}, []any{
					map[string]any{"from": []any{
						map[string]any{"podSelector": npSel("role", "web")},
						map[string]any{"namespaceSelector": npSel("kubernetes.io/metadata.name", mon), "podSelector": npSel("app", "monitor")},
					}, "ports": npPorts([2]any{"TCP", 8080})},
				}, nil))
			sameJSON(t, c.ID+" egress", g.objs["NetworkPolicy/web-egress"],
				npPolicy(ns, "web-egress", npSel("role", "web"), []any{"Egress"}, nil, []any{
					map[string]any{"to": []any{map[string]any{"podSelector": npSel("app", "api")}}, "ports": npPorts([2]any{"TCP", 8080})},
					map[string]any{"to": []any{map[string]any{"namespaceSelector": npSel("kubernetes.io/metadata.name", "kube-system"), "podSelector": npSel("k8s-app", "kube-dns")}},
						"ports": npPorts([2]any{"UDP", 53}, [2]any{"TCP", 53})},
				}))
		default:
			t.Errorf("networkpolicy-001 case %s has no contract", c.ID)
		}
		// web, api, ledger, monitor and intruder stay as setup created them in
		// every case; only batch-relabeled touches batch. No case or solution
		// touches a Service, and every object stays in the problem namespace.
		for key, o := range g.objs {
			testutil.Equal(t, nestedString(t, o, "metadata", "namespace"), ns)
			switch {
			case strings.HasPrefix(key, "NetworkPolicy/"):
			case key == "Deployment/batch" && c.ID == "batch-relabeled":
			default:
				t.Errorf("case %s must not apply %s", c.ID, key)
			}
		}
	}

	// Predicted status vectors (enforcing host): the criteria that FAIL in
	// the untouched environment and in each case; every other one is PASS. The
	// written trace is in the task report; nothing here was run against an
	// enforcing cluster.
	wantFail := map[string][]string{
		"initial":                {"batch-blocked", "monitor-allowed", "unlabeled-blocked", "web-egress-blocked"},
		"case deny-all":          {"web-reaches-api", "batch-blocked", "monitor-allowed", "unlabeled-blocked", "web-egress-blocked"},
		"case allow-all-ingress": {"batch-blocked", "monitor-allowed", "unlabeled-blocked"},
		"case batch-relabeled":   {"workloads-unchanged"},
		"case missing-egress":    {"web-egress-blocked"},
		// Trace: the lookalike (app=monitor in the problem namespace) and the
		// intruder (any -mon Pod) connect, so only monitor-allowed FAILs.
		"case and-or-split":              {"monitor-allowed"},
		"case alternative-single-policy": nil,
	}
	vectors := map[string]map[string]string{"initial": p.InitialExpected}
	for _, c := range p.Cases {
		vectors["case "+c.ID] = c.Expected
	}
	testutil.Equal(t, len(vectors), len(wantFail))
	for name, fails := range wantFail {
		for _, c := range p.Criteria {
			want := model.StatusPass
			if slices.Contains(fails, c.ID) {
				want = model.StatusFail
			}
			if got := vectors[name][c.ID]; got != want {
				t.Errorf("%s: criterion %s is %s, predicted %s", name, c.ID, got, want)
			}
		}
	}

	// The question states every graded literal, exactly one "do not change"
	// sentence (about batch, which workloads-unchanged grades), the blocked
	// definition and the scoring windows.
	for _, lit := range []string{
		"`api`", "`ledger`", "`web`", "`batch`", "`monitor`", "`intruder`", "`${namespace}`", "`${namespace}-mon`",
		"`role: api`", "`role: ledger`", "`role: web`", "`role: batch`", "`app: monitor`", "`component: intruder`",
		"`api-ok`", "`ledger-ok`", "port `80`", "port `8080`", "`53`", "UDP and TCP", "kube-system", "NetworkPolicies in `${namespace}` only",
		"Blocked means that the connection times out while an allowed client reaches the same Pod",
		"a client without the `role` or `app` labels in `${namespace}` is blocked", "and the server answers `api-ok`", "and the server answers `ledger-ok`",
		"Do not change the labels `app: batch` and `role: batch`, the image or the command of the Pod template of Deployment `batch`",
		"1 Ready replica", "10-second stability window, 90-second observation limit",
		"60-second limit", "this point needs the first one", "this point needs the second one",
		"`batch` must keep reaching `ledger`",
	} {
		if !strings.Contains(q, lit) {
			t.Errorf("question must state %q", lit)
		}
	}
	testutil.Equal(t, strings.Count(q, "not change"), 1)
	for _, ungraded := range []string{"kubectl", "curl", "Ingress ", "port-forward"} {
		if strings.Contains(q, ungraded) {
			t.Errorf("the question must not mention %q: nothing grades it", ungraded)
		}
	}
	readme := string(p.Files["setup/provided/README.txt"])
	for _, lit := range []string{
		"Deployment api", "Deployment ledger", "Deployment web", "Deployment batch", "Deployment monitor", "Deployment intruder",
		"role: web", "role: batch", "app: monitor", "api-ok", "ledger-ok", "port 80", "port 8080", "port 53", "kube-system",
		"Do not change the labels app: batch and role: batch, the image or the command of the Pod template of Deployment batch",
	} {
		if !strings.Contains(readme, lit) {
			t.Errorf("the handout must state %q", lit)
		}
	}
	testutil.Equal(t, strings.Count(readme, "not change"), 1)

	expl := string(p.Files["explanation.ko.md"])
	for _, lit := range []string{
		"NetworkPolicy", "podSelector", "namespaceSelector", "policyTypes", "kubernetes.io/metadata.name", "kube-system", "53",
		"timeout", "kubectl get networkpolicy", "kubectl describe networkpolicy", "kubectl get pods --show-labels",
		"deny-all", "allow-all-ingress", "batch-relabeled", "missing-egress", "and-or-split", "alternative-single-policy",
		"api-ready", "ledger-ready", "web-reaches-api", "batch-blocked", "monitor-allowed", "unlabeled-blocked", "web-egress-blocked", "workloads-unchanged",
		"https://kubernetes.io/docs/concepts/services-networking/network-policies/",
		// J2, J3: deny-all fails DNS in-namespace; the gating sentence.
		"Service 이름을 찾는 단계(DNS)에서 실패", "(기준 3~6)", "`workloads-unchanged`는 전제가 없습니다",
	} {
		if !strings.Contains(expl, lit) {
			t.Errorf("the explanation must mention %q", lit)
		}
	}
}
