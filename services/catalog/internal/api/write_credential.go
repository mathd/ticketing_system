package api

// The staff-write guard (TKT-191 / US-B2a). See ADR-042 § the amendment.
//
// Before this, every catalog write in the contract was reachable unauthenticated
// by anyone who could reach the gateway: TKT-190 gated the back-office UI, not
// the API behind it.
//
// The guard is expressed as an OpenAPI security requirement rather than a check
// in each handler, so that the contract, the runtime and the invariant test read
// the same declaration. A new operation inherits the document-level requirement
// and is therefore closed by default; going public takes a visible `security: []`.

import (
	"context"
	"crypto/subtle"
	"fmt"

	"github.com/getkin/kin-openapi/openapi3filter"
)

// authenticateStaffWrite is the AuthenticationFunc the request validator calls
// for any operation whose effective security requires a scheme.
//
// It returns a deliberately uninformative error. The validator turns that into
// the response, so anything said here reaches an unauthenticated caller — and
// "header missing" versus "header wrong" tells them whether a credential is
// configured at all.
func (s *Server) authenticateStaffWrite(_ context.Context, input *openapi3filter.AuthenticationInput) error {
	if input.SecuritySchemeName != staffWriteSecurityScheme {
		// An unknown scheme must not be silently treated as satisfied: that is how
		// a renamed or mistyped scheme becomes an open door.
		return fmt.Errorf("unauthorized")
	}
	// Fail closed when the server has no credential configured. Startup already
	// refuses that (runtimecfg.RequiredCredential), so this is defence against a
	// future construction path that forgets, not against normal operation —
	// without it, an empty configured value would match an empty header.
	if s.staffWriteCredential == "" {
		return fmt.Errorf("unauthorized")
	}
	presented := input.RequestValidationInput.Request.Header.Get(staffWriteHeader)
	// Constant-time: the comparison target is a secret, and an early-exit compare
	// leaks its prefix to anyone willing to measure. Cheap here, unlike the
	// deliberate cost on the password path.
	if subtle.ConstantTimeCompare([]byte(presented), []byte(s.staffWriteCredential)) != 1 {
		return fmt.Errorf("unauthorized")
	}
	return nil
}
