package api

import (
	"net/http"

	"ticketing/shared/httpx"
)

// staffWriteHeader carries the back office's commerce credential (TKT-194).
//
// Distinct from X-Internal-Token on purpose. That one value opens every
// service's internal surface, and TKT-191 deliberately withheld it from the
// back office so an internet-facing SSR process could not spend it. This one
// opens three — the staff refund, the void that reverses a comped order
// (TKT-171), and the staff order read (TKT-201) — and the enumeration in
// staff_credential_test.go is what keeps the set from growing unnoticed.
//
// Each addition is a deliberate widening of what an internet-facing SSR process
// can spend, so each one is argued in its own ticket rather than inherited.
const staffWriteHeader = "X-Commerce-Staff-Write-Token"

// WithStaffWriteCredential supplies the back office's credential.
//
// An option rather than a New parameter: New already takes six positional
// strings, and a seventh would be one more thing for a call site to pass in the
// wrong order — with a credential, silently.
func (s *Server) WithStaffWriteCredential(token string) *Server {
	s.staffWriteToken = token
	return s
}

// staffOrInternal reports whether the caller may drive a staff-reachable
// internal operation: either the shared internal token, or the back office's
// commerce credential.
//
// An inline check rather than a contract `security:` declaration, and a 404
// rather than a 401, are both decided in ADR-043: declared security guards a
// service's public contract, an inline check guards its internal surface, and
// the 404 keeps commerce's refusal indistinguishable from the gateway's own
// edge deny on the same path. Read it before replacing either.
//
// One helper, so no call site can invent a second answer — and both arms fail
// closed on an unconfigured value, because a service started without a
// credential must refuse everyone rather than admit anyone presenting nothing.
// Constant-time throughout: these are bearer credentials, and a comparison that
// returns early on the first wrong byte answers a different question than the
// one it was asked.
func (s *Server) staffOrInternal(r *http.Request) bool {
	return credentialMatches(r.Header.Get("X-Internal-Token"), s.token) ||
		credentialMatches(r.Header.Get(staffWriteHeader), s.staffWriteToken)
}

// The body moved to shared/go/httpx (ai-review S9): five services were comparing
// this same class of credential with `==`, and the fix was to give them all the
// one implementation that was already right rather than a sixth copy of it.
func credentialMatches(presented, configured string) bool {
	return httpx.CredentialMatches(presented, configured)
}
