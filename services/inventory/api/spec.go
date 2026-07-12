// Package apispec embeds the inventory public contract (ADR-009).
package apispec

import _ "embed"

//go:embed openapi.yaml
var Spec []byte
