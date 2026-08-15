package api

import (
	"net/http"

	"ticketing/shared/httpx"
)

// staffWriteHeader carries the back office's inventory credential (TKT-244, ADR-057).
//
// Distinct from X-Internal-Token on purpose. That one value opens every service's
// internal surface, and TKT-191 deliberately withheld it from the back office so an
// internet-facing SSR process could not spend it. This one opens exactly two
// operations — the channel-allocation editor's read and its save — and the enumeration
// in staff_credential_test.go is what keeps that true.
//
// Why a NEW credential rather than reusing catalog's, which ADR-053 chose three days
// earlier for a similar-looking question: ADR-053's case rested on the catalog token
// ALREADY holding create/update power over the very channels it was then allowed to
// read, so the allowance added amplification but no new capability class. The back
// office holds nothing for inventory, so any reuse would grant a new capability across
// a service boundary and make a catalog-token compromise an inventory-write compromise.
const staffWriteHeader = "X-Inventory-Staff-Write-Token"

// WithStaffWriteCredential supplies the back office's credential.
//
// An option rather than a New parameter: New already takes the store, the shared
// credential and the pinner, and a fourth positional string would be one more thing for
// a call site to pass in the wrong order — with a credential, silently.
func (s *Server) WithStaffWriteCredential(token string) *Server {
	s.staffWriteToken = token
	return s
}

// staffOrInternal reports whether the caller may drive one of the two staff-reachable
// operations ADR-057 grants: either the shared internal token, or the back office's
// inventory credential.
//
// An inline check rather than a contract `security:` declaration is decided by ADR-043:
// declared security guards a service's public contract, an inline check guards its
// internal surface. This ticket adds a CREDENTIAL, not a new guard PLACEMENT — the
// guard stays exactly where internalOnly already put it.
//
// One helper, so no call site can invent a second answer — and both arms fail closed on
// an unconfigured value, because a service started without a credential must refuse
// everyone rather than admit anyone presenting nothing. Constant-time throughout: these
// are bearer credentials, and a comparison that returns early on the first wrong byte
// answers a different question than the one it was asked.
func (s *Server) staffOrInternal(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpx.HeaderCredentialMatches(r, httpx.InternalToken, s.credential) &&
			!httpx.HeaderCredentialMatches(r, staffWriteHeader, s.staffWriteToken) {
			write(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		h(w, r)
	}
}
