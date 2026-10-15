package grader_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/JunYupK/JunKloud/internal/grader"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

const canary = "secret-canary-value"

var secretRef = refOf("v1", "Secret", "jk-p1", "creds")

func secretDoc(t testing.TB) map[string]any {
	t.Helper()
	enc := base64.StdEncoding.EncodeToString([]byte(canary))
	return obj(t, `{"apiVersion":"v1","kind":"Secret","type":"Opaque",
	 "metadata":{"name":"creds","namespace":"jk-p1","uid":"uid-s",
	   "annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{\"stringData\":{\"token\":\"`+canary+`\"}}"}},
	 "data":{"token":"`+enc+`","other":"`+base64.StdEncoding.EncodeToString([]byte("another-private-thing"))+`"}}`)
}

func encode(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	testutil.NoError(t, err)
	return string(b)
}

func TestEvidenceRedaction(t *testing.T) {
	ctx := context.Background()
	for name, expected := range map[string]string{"matching value": canary, "wrong value": "not-it"} {
		t.Run(name, func(t *testing.T) {
			red := grader.NewRedactor()
			c := crit("s", model.CriterionField, 2, secretRef, map[string]any{"fields": []any{
				map[string]any{"path": "data.token", "equals": expected},
				map[string]any{"path": "data", "exists": true},
				map[string]any{"path": "metadata.annotations", "exists": true},
				map[string]any{"path": "type", "equals": "Opaque"},
			}})
			r := newReader().put(secretRef, secretDoc(t))
			checker := grader.NewFieldChecker(r, grader.WithClock(newClock()), grader.WithRedactor(red))
			e := grader.NewEngine(map[string]grader.Checker{model.CriterionField: checker}, grader.WithClock(newClock()), grader.WithRedactor(red))
			ev, err := e.Evaluate(ctx, target(c))
			testutil.NoError(t, err)
			wantStatus := "PASS"
			if expected != canary {
				wantStatus = "FAIL"
			}
			testutil.Equal(t, resultFor(t, ev, "s").Status, wantStatus)

			encodedEvidence := encode(t, ev)
			testutil.Equal(t, strings.Contains(encodedEvidence, canary), false)
			testutil.Equal(t, strings.Contains(encodedEvidence, "another-private-thing"), false)
			testutil.Equal(t, strings.Contains(encodedEvidence, base64.StdEncoding.EncodeToString([]byte(canary))), false)
			if wantStatus == "FAIL" && strings.Contains(encodedEvidence, "not-it") {
				t.Fatalf("expected Secret values are redacted too: %s", encodedEvidence)
			}
			// Non-secret facts are still reported.
			if !strings.Contains(encodedEvidence, "Opaque") || !strings.Contains(encodedEvidence, "uid-s") {
				t.Fatalf("evidence lost the non-secret facts: %s", encodedEvidence)
			}
		})
	}
}

func TestEvidenceRedactionOfLearnedValues(t *testing.T) {
	// A later checker (logs, http) may put a secret-derived value anywhere in
	// its evidence. The engine scrubs every learned value as a backstop.
	red := grader.NewRedactor()
	red.Learn(canary)
	leaky := checkerFunc(func(_ context.Context, _ grader.Target, c model.Criterion) (model.CheckResult, error) {
		return model.CheckResult{CriterionID: c.ID, Status: model.StatusFail,
			Reason: "log line was token=" + canary,
			Evidence: model.Evidence{
				Expected: map[string]any{"line": "token=" + canary},
				Actual:   []any{"x", map[string]string{"k": "prefix-" + canary + "-suffix"}},
			}}, nil
	})
	e := grader.NewEngine(map[string]grader.Checker{model.CriterionLogs: leaky}, grader.WithClock(newClock()), grader.WithRedactor(red))
	ev, err := e.Evaluate(context.Background(), target(crit("l", model.CriterionLogs, 1, deployRef, nil)))
	testutil.NoError(t, err)
	encodedEvidence := encode(t, ev)
	testutil.Equal(t, strings.Contains(encodedEvidence, canary), false)
	if !strings.Contains(encodedEvidence, "[REDACTED]") || !strings.Contains(encodedEvidence, "prefix-") {
		t.Fatalf("scrub should replace only the secret part: %s", encodedEvidence)
	}
}

func TestRedactorLearnsSecretObjects(t *testing.T) {
	red := grader.NewRedactor()
	red.LearnObject(u(secretDoc(t)))
	out := red.Scrub(map[string]any{"a": "x " + canary, "b": []any{"another-private-thing"}, "n": 7})
	testutil.Equal(t, out, map[string]any{"a": "x [REDACTED]", "b": []any{"[REDACTED]"}, "n": 7})
	// Short values are not scrubbed by substring: it would mangle everything.
	red.Learn("ab")
	testutil.Equal(t, red.Scrub("abcab"), "abcab")
}

func TestEvidenceIsBounded(t *testing.T) {
	huge := strings.Repeat("x", 100000)
	doc := obj(t, deployDoc)
	doc["metadata"].(map[string]any)["annotations"] = map[string]any{"big": huge}
	c := fieldCrit(map[string]any{"path": "metadata.annotations", "exists": true})
	res := checkField(t, mustJSON(t, doc), c)
	testutil.Equal(t, res.Status, "PASS")
	if n := len(encode(t, res.Evidence)); n > 20000 {
		t.Fatalf("evidence is %d bytes", n)
	}
}

func mustJSON(t testing.TB, v any) string {
	t.Helper()
	return encode(t, v)
}
