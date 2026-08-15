package api

import (
	"context"
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
		internal := httpx.HeaderCredentialMatches(r, httpx.InternalToken, s.credential)
		staff := httpx.HeaderCredentialMatches(r, staffWriteHeader, s.staffWriteToken)
		if !internal && !staff {
			write(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		// Both arms are evaluated before either is consulted, deliberately: a
		// short-circuit would skip a constant-time comparison and make the
		// unauthorized path's timing depend on which credential was presented.
		//
		// The class travels on the request context because the handler needs it
		// (TKT-250): the allocation-set revision is REQUIRED of a staff caller and
		// optional for the shared internal token, and until now the guard collapsed
		// both into one boolean and told the handler nothing.
		//
		// Internal wins when both are presented. That is the conservative order: the
		// shared token is the broader credential, so a caller holding it is treated
		// as the service-to-service path it already was, and presenting a staff
		// header alongside it cannot tighten or loosen anything.
		class := credentialStaff
		if internal {
			class = credentialInternal
		}
		h(w, r.WithContext(context.WithValue(r.Context(), credentialClassKey{}, class)))
	}
}

// credentialClass is which of ADR-057's two credentials opened the request.
//
// Deliberately not a bare bool on the request: the two are not opposites — a future
// third credential would be a third value, and `!internal` would silently reclassify it
// as staff.
type credentialClass int

const (
	// credentialStaff is the back office's own inventory credential.
	credentialStaff credentialClass = iota
	// credentialInternal is the shared X-Internal-Token, held by services and by the
	// smoke suite, and deliberately NOT by the back office (ADR-057).
	credentialInternal
)

// credentialClassKey types the context key so nothing else can collide with it.
type credentialClassKey struct{}

// callerCredential reports which credential opened this request.
//
// FAILS CLOSED to staff: a handler reached without the guard having run has no class on
// its context, and staff is the arm with the STRICTER requirement (it must present a
// revision). Defaulting to internal would silently drop the precondition for any route
// wired up without the guard — the failure would be a missing check, which is invisible,
// rather than a refusal, which is not.
func callerCredential(r *http.Request) credentialClass {
	if c, ok := r.Context().Value(credentialClassKey{}).(credentialClass); ok {
		return c
	}
	return credentialStaff
}
