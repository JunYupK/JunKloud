package problem_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/testutil"
	"github.com/JunYupK/JunKloud/schemas"
)

func TestLoadValid(t *testing.T) {
	p, err := problem.Load(validFixture)
	testutil.NoError(t, err)

	testutil.Equal(t, p.SchemaVersion, 1)
	testutil.Equal(t, p.ID, "service-test")
	testutil.Equal(t, p.Version, "1.0.0")
	testutil.Equal(t, p.Domain, model.DomainNetworking)
	testutil.Equal(t, p.Difficulty, model.DifficultyEasy)
	testutil.Equal(t, p.Type, model.TypeBuild)
	testutil.Equal(t, p.Namespace, "${namespace}")
	// Quantity strings are parsed: 250m CPU and 128Mi memory.
	testutil.Equal(t, p.Budget, model.ResourceBudget{CPUMillis: 250, MemoryBytes: 128 << 20})
	// Parameter values keep their scalar type: ports are ints.
	testutil.Equal(t, p.Parameters["port"], []any{8080, 9090})
	testutil.Equal(t, p.Parameters["namespace"], []any{"svc-alpha", "svc-bravo", "svc-charlie"})

	testutil.Equal(t, len(p.Criteria), 3)
	reach := p.Criteria[2]
	testutil.Equal(t, reach.ID, "service-reachable")
	testutil.Equal(t, reach.Kind, model.CriterionHTTP)
	// Duration strings are parsed with time.ParseDuration; absent means 0.
	testutil.Equal(t, reach.Timeout, 30*time.Second)
	testutil.Equal(t, p.Criteria[0].Timeout, time.Duration(0))
	testutil.Equal(t, reach.DependsOn, []string{"service-port"})
	testutil.Equal(t, reach.Target, model.ObjectRef{APIVersion: "v1", Kind: "Service", Namespace: "${namespace}", Name: "${service_name}"})
	// Integer literals in Args decode as int, not float64.
	testutil.Equal(t, reach.Args["status"], 200)
	// Every criterion carries its Korean feedback (spec 8.1).
	testutil.Equal(t, reach.Feedback, "Service가 ${port} 포트로 HTTP 200 응답을 돌려주는지 확인하세요.")
	for _, c := range p.Criteria {
		if c.Feedback == "" {
			t.Errorf("criterion %s has no feedback", c.ID)
		}
	}

	testutil.Equal(t, p.Solution, []model.Action{{Kind: model.ActionApply, Args: map[string]any{"path": "solution/answer.yaml"}}})
	testutil.Equal(t, p.InitialScore, 0)
	testutil.Equal(t, p.Cases[0].ExpectedScore, 2)
	testutil.Equal(t, p.Cases[0].Expected["service-exists"], model.StatusPass)

	// Every package file except problem.yaml is loaded, keyed by slash path.
	keys := make([]string, 0, len(p.Files))
	for k := range p.Files {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	testutil.Equal(t, keys, []string{
		"cases/wrong-port.yaml",
		"explanation.ko.md",
		"question.en.md",
		"setup/deployment.yaml",
		"setup/provided/client-pod.yaml",
		"solution/answer.yaml",
	})
}

// TestInvalidDefinition loads the valid package with problem.yaml replaced
// by each testdata/invalid fixture and checks it is rejected for the stated
// reason.
func TestInvalidDefinition(t *testing.T) {
	cases := map[string]string{
		"duplicate-yaml-key":     `"title"`,
		"unknown-criterion-kind": "/criteria/0/kind",
		"unknown-action-kind":    "/setup/0/kind",
		"unknown-domain":         "/domain",
		"unknown-field":          "script",
		"wrong-schema-version":   "/schema_version",
		"dependency-cycle":       "dependency cycle",
		"unknown-dependency":     `unknown criterion "service-missing"`,
		"zero-points":            "/criteria/0/points",
		"initial-score-mismatch": "initial_score 3",
		"case-score-mismatch":    `case "wrong-port": expected_score 5`,
		"incomplete-expected":    `initial_expected misses criterion "service-reachable"`,
		"port-out-of-range":      "70000",
		"duplicate-criterion-id": `duplicate criterion id "service-exists"`,
		"duplicate-case-id":      `duplicate case id "wrong-port"`,
		"undefined-parameter":    `undefined parameter "service"`,
		"mixed-parameter-types":  `parameter "port"`,
		"bad-timeout":            "/criteria/2/timeout",
		"bad-quantity":           "memory",
		"bad-namespace":          "Svc_Bravo",
		// Each criterion has Korean feedback (spec 8.1).
		"missing-feedback":          "/criteria/1",
		"non-korean-feedback":       `criterion "service-port": feedback must be written in Korean`,
		"undefined-ref-in-feedback": `undefined parameter "nope"`,
		// Resource scope (spec §6.2, ruling R13).
		"scope-problem-namespace":           `scope namespace "svc-alpha" is the problem namespace`,
		"scope-duplicate-namespace":         `duplicate scope namespace "svc-alpha-client"`,
		"scope-bad-namespace":               `scope namespace "Client_NS" is not a DNS-1123 label`,
		"scope-namespaced-cluster-resource": "/scope/cluster_resources/0",
		// Directories overlay package files instead of replacing problem.yaml:
		// references in every package text file must be declared parameters.
		"undeclared-ref-in-setup":    `setup/deployment.yaml: undefined parameter "servce_name"`,
		"undeclared-ref-in-provided": `setup/provided/client-pod.yaml: undefined parameter "clinet"`,
	}
	entries, err := os.ReadDir("testdata/invalid")
	testutil.NoError(t, err)
	testutil.Equal(t, len(entries), len(cases))

	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			dir := copyFixture(t, t.TempDir())
			src := filepath.Join("testdata/invalid", name)
			if info, err := os.Stat(src); err == nil && info.IsDir() {
				overlay(t, src, dir)
			} else {
				def, err := os.ReadFile(src + ".yaml")
				testutil.NoError(t, err)
				writeFile(t, filepath.Join(dir, "problem.yaml"), string(def))
			}

			_, err := problem.Load(dir)
			testutil.ErrorIs(t, err, problem.ErrInvalidDefinition)
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not mention %q", err, want)
			}
		})
	}
}

func TestLoadScope(t *testing.T) {
	dir := copyFixture(t, t.TempDir())
	def, err := os.ReadFile(filepath.Join(dir, "problem.yaml"))
	testutil.NoError(t, err)
	writeFile(t, filepath.Join(dir, "problem.yaml"), strings.Replace(string(def), "budget:\n", `scope:
  namespaces: ["${namespace}-client"]
  cluster_resources:
    - {api_version: rbac.authorization.k8s.io/v1, kind: ClusterRole, name: "${service_name}-reader"}
budget:
`, 1))

	p, err := problem.Load(dir)
	testutil.NoError(t, err)
	testutil.Equal(t, p.Scope, model.ResourceScope{
		Namespaces:       []string{"${namespace}-client"},
		ClusterResources: []model.ObjectRef{{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: "${service_name}-reader"}},
	})

	f, err := problem.Freeze(p, 38129)
	testutil.NoError(t, err)
	testutil.Equal(t, f.Definition.Scope, model.ResourceScope{
		Namespaces:       []string{goldenNamespace + "-client"},
		ClusterResources: []model.ObjectRef{{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: goldenServiceName + "-reader"}},
	})
	// Freeze leaves the input's scope unrendered.
	testutil.Equal(t, p.Scope.Namespaces, []string{"${namespace}-client"})
}

func TestLoadDefaultScope(t *testing.T) {
	// Absent scope: only the problem's own namespace, no cluster objects.
	p, err := problem.Load(validFixture)
	testutil.NoError(t, err)
	testutil.Equal(t, p.Scope, model.ResourceScope{})
}

func TestLoadSkipsBinaryFilesForReferences(t *testing.T) {
	dir := copyFixture(t, t.TempDir())
	writeFile(t, filepath.Join(dir, "setup", "provided", "blob.bin"), "\x00${not_a_param}")
	p, err := problem.Load(dir)
	testutil.NoError(t, err)
	f, err := problem.Freeze(p, 1)
	testutil.NoError(t, err)
	testutil.Equal(t, string(f.Assets["blob.bin"]), "\x00${not_a_param}")
}

func TestLoadRequiresPackageFiles(t *testing.T) {
	for _, name := range []string{"problem.yaml", "question.en.md", "explanation.ko.md"} {
		t.Run(name, func(t *testing.T) {
			dir := copyFixture(t, t.TempDir())
			testutil.NoError(t, os.Remove(filepath.Join(dir, name)))
			_, err := problem.Load(dir)
			testutil.ErrorIs(t, err, problem.ErrInvalidDefinition)
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("error %q does not mention %q", err, name)
			}
		})
	}
}

func TestLoadRejectsEscapingSymlink(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside.yaml")
	writeFile(t, outside, "kind: Secret\n")
	dir := copyFixture(t, filepath.Join(t.TempDir(), "pkg"))
	testutil.NoError(t, os.Symlink(outside, filepath.Join(dir, "setup", "provided", "leak.yaml")))

	_, err := problem.Load(dir)
	testutil.ErrorIs(t, err, problem.ErrInvalidDefinition)
	testutil.ErrorIs(t, err, problem.ErrAssetEscape)
}

func TestLoadFollowsInternalSymlink(t *testing.T) {
	dir := copyFixture(t, filepath.Join(t.TempDir(), "pkg"))
	testutil.NoError(t, os.Symlink("../deployment.yaml", filepath.Join(dir, "setup", "provided", "deployment.yaml")))

	p, err := problem.Load(dir)
	testutil.NoError(t, err)
	want, err := os.ReadFile(filepath.Join(dir, "setup", "deployment.yaml"))
	testutil.NoError(t, err)
	testutil.Equal(t, p.Files["setup/provided/deployment.yaml"], want)
}

func TestLoadKoreanAndSpacePath(t *testing.T) {
	dir := copyFixture(t, filepath.Join(t.TempDir(), "문제 은행", "서비스 테스트"))
	p, err := problem.Load(dir)
	testutil.NoError(t, err)
	testutil.Equal(t, p.ID, "service-test")
}

// TestSchemaEnumsMatchModel keeps the schema's enumerations and the exported
// model constants in lock-step.
func TestSchemaEnumsMatchModel(t *testing.T) {
	var s struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
		Defs map[string]struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	testutil.NoError(t, json.Unmarshal(schemas.ProblemV1, &s))
	testutil.Equal(t, s.Properties["domain"].Enum, model.Domains())
	testutil.Equal(t, s.Properties["type"].Enum, model.Types())
	testutil.Equal(t, s.Properties["difficulty"].Enum, model.Difficulties())
	testutil.Equal(t, s.Defs["criterion"].Properties["kind"].Enum, model.CriterionKinds())
	testutil.Equal(t, s.Defs["action"].Properties["kind"].Enum, model.ActionKinds())
}
