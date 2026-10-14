// Package schemas embeds the JSON Schemas that define JunKloud's declarative
// file formats, so the binary validates against exactly the committed schema.
package schemas

import _ "embed"

// ProblemV1 is problem-v1.schema.json (JSON Schema 2020-12) for problem.yaml
// schema_version 1.
//
//go:embed problem-v1.schema.json
var ProblemV1 []byte
