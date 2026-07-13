// Package apispec embeds the Access OpenAPI contract for byte-identical serving.
package apispec

import _ "embed"

//go:embed openapi.yaml
var Spec []byte
