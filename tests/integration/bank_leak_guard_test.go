//go:build linux

package integration

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/JunYupK/JunKloud/internal/model"
)

// evaluationLeaks reports where an evaluation shows any forbidden string, in
// plain text or base64. It looks at two renderings: the JSON the evaluation is
// stored and logged as, and the text the CLI prints for every result (the
// expected / actual / reason line of internal/cli/grade.go, for PASS results
// too, which is stricter than the CLI). A finding names the place and the index
// of the forbidden string, never the value itself, so that a failing run does
// not spread the secret into the log.
func evaluationLeaks(ev model.Evaluation, forbidden []string) []string {
	if len(forbidden) == 0 {
		return nil
	}
	encoded, err := json.Marshal(ev)
	if err != nil {
		return []string{fmt.Sprintf("evaluation does not marshal: %v", err)}
	}
	var cli strings.Builder
	for _, r := range ev.Results {
		fmt.Fprintf(&cli, "%s %s (%d)\n", r.CriterionID, r.Status, r.Points)
		fmt.Fprintf(&cli, "  expected: %v | actual: %v\n", r.Evidence.Expected, r.Evidence.Actual)
		fmt.Fprintf(&cli, "  reason: %s\n", r.Reason)
	}
	renderings := []struct{ name, text string }{{"JSON", string(encoded)}, {"CLI text", cli.String()}}
	var findings []string
	for i, f := range forbidden {
		if f == "" {
			continue
		}
		forms := map[string]string{"plain": f, "base64": base64.StdEncoding.EncodeToString([]byte(f))}
		for _, rendering := range renderings {
			for form, needle := range forms {
				if strings.Contains(rendering.text, needle) {
					findings = append(findings, fmt.Sprintf("forbidden string #%d (%s) appears in the evaluation %s", i, form, rendering.name))
				}
			}
		}
	}
	return findings
}
