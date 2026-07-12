// Package apispec embeds the catalog OpenAPI contract so the binary can
// serve it byte-identical to the committed file (ADR-009 §4) and validate
// requests against it at runtime.
package apispec

import _ "embed"

//go:embed openapi.yaml
var Spec []byte
