package problem_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

func TestFreezeStable(t *testing.T) {
	p, err := problem.Load(validFixture)
	testutil.NoError(t, err)

	a, err := problem.Freeze(p, 38129)
	testutil.NoError(t, err)
	b, err := problem.Freeze(p, 38129)
	testutil.NoError(t, err)
	testutil.Equal(t, a, b)

	// Golden: seed-v1 with seed 38129 picks these values for service-test.
	// Changing this means changing the seed algorithm, which needs a new
	// algorithm version string.
	testutil.Equal(t, a.SeedAlgorithm, "seed-v1")
	testutil.Equal(t, a.Seed, uint64(38129))
	testutil.Equal(t, a.Parameters, map[string]any{"namespace": goldenNamespace, "port": goldenPort, "service_name": goldenServiceName})
	testutil.Equal(t, a.Namespace, goldenNamespace)
	testutil.Equal(t, a.Context, "")

	// The definition is rendered structurally: whole references keep the
	// parameter's type, embedded ones are interpolated.
	d := a.Definition
	testutil.Equal(t, d.Namespace, goldenNamespace)
	testutil.Equal(t, d.Criteria[0].Target, model.ObjectRef{APIVersion: "v1", Kind: "Service", Namespace: goldenNamespace, Name: goldenServiceName})
	testutil.Equal(t, d.Criteria[1].Args["equals"], goldenPort)
	testutil.Equal(t, d.Criteria[2].Args["port"], goldenPort)
	testutil.Equal(t, d.Criteria[2].Args["path"], "/")
	// Freeze does not mutate its input.
	testutil.Equal(t, p.Criteria[2].Args["port"], "${port}")
	testutil.Equal(t, p.Namespace, "${namespace}")

	// Only the English question and setup/provided/ files are public.
	if !strings.Contains(a.Question, "named `"+goldenServiceName+"`") || strings.Contains(a.Question, "${") {
		t.Fatalf("question not rendered: %s", a.Question)
	}
	testutil.Equal(t, len(a.Assets), 1)
	// Provided text assets are rendered with the same frozen values.
	asset := string(a.Assets["client-pod.yaml"])
	if !strings.Contains(asset, `namespace: "`+goldenNamespace+`"`) || strings.Contains(asset, "${") {
		t.Fatalf("provided asset not rendered: %s", asset)
	}
	testutil.Equal(t, string(p.Files["setup/provided/client-pod.yaml"]) != asset, true)
	for name, content := range a.Assets {
		for _, marker := range []string{"REFERENCE-ANSWER-MARKER", "WRONG-CASE-MARKER", "해설"} {
			if strings.Contains(string(content), marker) || strings.Contains(a.Question, marker) {
				t.Fatalf("public content %q leaks private marker %q", name, marker)
			}
		}
	}

	if !strings.HasPrefix(a.Hash, "sha256:") || len(a.Hash) != len("sha256:")+64 {
		t.Fatalf("hash %q is not sha256:<hex>", a.Hash)
	}
	h, err := problem.Hash(p)
	testutil.NoError(t, err)
	testutil.Equal(t, a.Hash, h)
}

func TestFreezeOtherSeedsStayInRange(t *testing.T) {
	p, err := problem.Load(validFixture)
	testutil.NoError(t, err)
	seen := map[any]bool{}
	for seed := uint64(0); seed < 64; seed++ {
		f, err := problem.Freeze(p, seed)
		testutil.NoError(t, err)
		for name, v := range f.Parameters {
			allowed := false
			for _, a := range p.Parameters[name] {
				allowed = allowed || a == v
			}
			if !allowed {
				t.Fatalf("seed %d: parameter %s=%v not in %v", seed, name, v, p.Parameters[name])
			}
		}
		testutil.Equal(t, f.Namespace, f.Parameters["namespace"])
		seen[f.Parameters["namespace"]] = true
	}
	testutil.Equal(t, len(seen), 3)
}

func TestHashCoversPackage(t *testing.T) {
	dir := copyFixture(t, t.TempDir())
	p, err := problem.Load(dir)
	testutil.NoError(t, err)
	base, err := problem.Hash(p)
	testutil.NoError(t, err)

	// Reloading the same content gives the same hash.
	again, err := problem.Load(dir)
	testutil.NoError(t, err)
	h, err := problem.Hash(again)
	testutil.NoError(t, err)
	testutil.Equal(t, h, base)

	// A comment-only edit of problem.yaml does not change the normalized
	// definition, so it does not change the hash.
	def, err := os.ReadFile(filepath.Join(dir, "problem.yaml"))
	testutil.NoError(t, err)
	writeFile(t, filepath.Join(dir, "problem.yaml"), "# reformatted\n"+string(def))
	commented, err := problem.Load(dir)
	testutil.NoError(t, err)
	h, err = problem.Hash(commented)
	testutil.NoError(t, err)
	testutil.Equal(t, h, base)

	// A private file change (the reference solution) changes the hash.
	writeFile(t, filepath.Join(dir, "solution", "answer.yaml"), "changed: true\n")
	changed, err := problem.Load(dir)
	testutil.NoError(t, err)
	h, err = problem.Hash(changed)
	testutil.NoError(t, err)
	if h == base {
		t.Fatal("hash did not change after solution/answer.yaml changed")
	}

	// A definition change changes the hash.
	p.Criteria[0].Points = 9
	h, err = problem.Hash(p)
	testutil.NoError(t, err)
	if h == base {
		t.Fatal("hash did not change after the definition changed")
	}
}

func TestRenderValue(t *testing.T) {
	params := map[string]any{"name": "web", "port": 8080, "debug": true}
	got, err := problem.RenderValue(map[string]any{
		"metadata": map[string]any{"name": "${name}", "labels": map[string]any{"${name}": "app-${name}"}},
		"ports":    []any{map[string]any{"port": "${port}", "url": "http://${name}:${port}/"}},
		"debug":    "${debug}",
		"count":    3,
		"shell":    "${HOME}",
	}, params)
	testutil.NoError(t, err)
	testutil.Equal(t, got, map[string]any{
		// Map keys are not rendered: only scalar values are.
		"metadata": map[string]any{"name": "web", "labels": map[string]any{"${name}": "app-web"}},
		"ports":    []any{map[string]any{"port": 8080, "url": "http://web:8080/"}},
		"debug":    true,
		"count":    3,
		// Only lower-case parameter names are references.
		"shell": "${HOME}",
	})

	_, err = problem.RenderValue(map[string]any{"name": "${missing}"}, params)
	testutil.ErrorIs(t, err, problem.ErrUndefinedParameter)

	text, err := problem.RenderText("Expose ${name} on ${port}.", params)
	testutil.NoError(t, err)
	testutil.Equal(t, text, "Expose web on 8080.")
	_, err = problem.RenderText("Use ${missing}.", params)
	testutil.ErrorIs(t, err, problem.ErrUndefinedParameter)
}

// Golden seed-v1 parameter choice for service-test with seed 38129,
// computed independently of this package (SplitMix64 over sorted names).
const (
	goldenNamespace   = "svc-bravo"
	goldenServiceName = "frontend"
	goldenPort        = 9090
)

// TestFreezeRendersFeedback: a criterion's Korean feedback is rendered with
// the frozen parameters like the rest of the definition.
func TestFreezeRendersFeedback(t *testing.T) {
	p, err := problem.Load(validFixture)
	testutil.NoError(t, err)
	for _, seed := range []uint64{1, 2, 3, 4} {
		f, err := problem.Freeze(p, seed)
		testutil.NoError(t, err)
		for _, c := range f.Definition.Criteria {
			if strings.Contains(c.Feedback, "${") {
				t.Fatalf("seed %d: criterion %s feedback not rendered: %q", seed, c.ID, c.Feedback)
			}
		}
		testutil.Equal(t, f.Definition.Criteria[1].Feedback, fmt.Sprintf("Service의 첫 번째 포트가 %v인지 확인하세요.", f.Parameters["port"]))
		testutil.Equal(t, f.Definition.Criteria[0].Feedback,
			fmt.Sprintf("Service %v이(가) %v namespace에 있는지 확인하세요.", f.Parameters["service_name"], f.Parameters["namespace"]))
	}
	// The unrendered problem keeps the references (it is what is hashed).
	testutil.Equal(t, strings.Contains(p.Criteria[1].Feedback, "${port}"), true)
}
