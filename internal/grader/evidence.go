package grader

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/JunYupK/JunKloud/internal/model"
)

const (
	// Redacted replaces every secret value in evidence and reasons.
	Redacted = "[REDACTED]"
	// minScrubLen is the shortest learned value that is removed from free
	// text by substring: shorter ones would mangle unrelated text. Secret
	// objects' own data is always redacted structurally regardless.
	minScrubLen = 4
	// maxEvidenceValue bounds one stored Expected/Actual value.
	maxEvidenceValue = 4096
)

// Redactor keeps Secret plaintext out of everything the grader stores or
// prints (spec 8.3). Checkers call LearnObject for every object they read and
// build evidence with redacted values; the engine runs Scrub over every
// result as a backstop for values a checker did not expect to be secret.
// A Redactor is safe for concurrent use.
type Redactor struct {
	mu     sync.RWMutex
	values map[string]struct{}
}

// NewRedactor returns an empty Redactor.
func NewRedactor() *Redactor { return &Redactor{values: map[string]struct{}{}} }

// Learn registers secret values to be removed wherever they appear.
func (r *Redactor) Learn(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range values {
		if len(v) >= minScrubLen {
			r.values[v] = struct{}{}
		}
	}
}

// LearnObject registers every data/stringData value of a Secret (plaintext
// and its base64 form). Other kinds are ignored.
func (r *Redactor) LearnObject(obj *unstructured.Unstructured) {
	if !IsSecret(obj) {
		return
	}
	for _, field := range []string{"data", "stringData"} {
		m, _ := obj.Object[field].(map[string]any)
		for _, v := range m {
			s, ok := v.(string)
			if !ok {
				continue
			}
			r.Learn(s)
			if field == "data" {
				if dec, err := base64.StdEncoding.DecodeString(s); err == nil {
					r.Learn(string(dec))
				}
			}
		}
	}
}

// IsSecret reports whether obj is a core/v1 Secret.
func IsSecret(obj *unstructured.Unstructured) bool {
	return obj != nil && obj.GetAPIVersion() == "v1" && obj.GetKind() == "Secret"
}

// Scrub returns a copy of v with every learned secret replaced by Redacted.
// Structure is preserved; v itself is not modified.
func (r *Redactor) Scrub(v any) any {
	r.mu.RLock()
	secrets := make([]string, 0, len(r.values))
	for s := range r.values {
		secrets = append(secrets, s)
	}
	r.mu.RUnlock()
	if len(secrets) == 0 {
		return v
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	return scrubValue(v, secrets)
}

func scrubString(s string, secrets []string) string {
	for _, sec := range secrets {
		if strings.Contains(s, sec) {
			s = strings.ReplaceAll(s, sec, Redacted)
		}
	}
	return s
}

func scrubValue(v any, secrets []string) any {
	switch x := v.(type) {
	case nil, bool, int, int32, int64, float32, float64, json.Number:
		return v
	case string:
		return scrubString(x, secrets)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[scrubString(k, secrets)] = scrubValue(e, secrets)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = scrubValue(e, secrets)
		}
		return out
	default:
		// Typed values (structs, map[string]string, ...) are scrubbed in
		// their JSON form.
		raw, err := json.Marshal(v)
		if err != nil {
			return "<unprintable>"
		}
		var generic any
		if err := json.Unmarshal(raw, &generic); err != nil {
			return "<unprintable>"
		}
		return scrubValue(generic, secrets)
	}
}

// ScrubResult scrubs a result's reason and evidence values.
func (r *Redactor) ScrubResult(res model.CheckResult) model.CheckResult {
	res.Reason = r.Scrub(res.Reason).(string)
	res.Evidence.Expected = r.Scrub(res.Evidence.Expected)
	res.Evidence.Actual = r.Scrub(res.Evidence.Actual)
	return res
}

// InputHash identifies the part of an object a result depends on: its spec,
// or for kinds without one (ConfigMap, Secret, RBAC objects) every top-level
// field except apiVersion, kind, metadata and status. The hash is over
// canonical JSON, so map order does not matter.
func InputHash(obj *unstructured.Unstructured) string {
	if obj == nil {
		return ""
	}
	subject := map[string]any{}
	if spec, ok := obj.Object["spec"]; ok {
		subject["spec"] = spec
	} else {
		for k, v := range obj.Object {
			switch k {
			case "apiVersion", "kind", "metadata", "status":
			default:
				subject[k] = v
			}
		}
	}
	raw, err := json.Marshal(subject)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// baseEvidence fills the facts every observation has: when, which object
// (UID, generation) and the hash of what was graded.
func baseEvidence(now time.Time, start time.Time, obj *unstructured.Unstructured) model.Evidence {
	ev := model.Evidence{ObservedAt: now.UTC(), Duration: now.Sub(start)}
	if obj != nil {
		ev.UID = string(obj.GetUID())
		ev.Generation = obj.GetGeneration()
		ev.InputHash = InputHash(obj)
	}
	return ev
}

// clampValue bounds a stored evidence value: large values are replaced by a
// truncated JSON rendering.
func clampValue(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return "<unprintable>"
	}
	if len(raw) <= maxEvidenceValue {
		return v
	}
	return string(raw[:maxEvidenceValue]) + "...(truncated)"
}
