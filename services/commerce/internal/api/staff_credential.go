package api

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"ticketing/shared/httpx"
)

// staffWriteHeader carries the back office's commerce credential (TKT-194).
//
// Distinct from X-Internal-Token on purpose. That one value opens every
// service's internal surface, and TKT-191 deliberately withheld it from the
// back office so an internet-facing SSR process could not spend it. This one
// opens three — the staff refund, the void that reverses a comped order
// (TKT-171), and the staff order read (TKT-201) — and only together with a
// verified organizer assertion (TKT-287). The enumeration in
// staff_credential_test.go is what keeps the set from growing unnoticed.
//
// Each addition is a deliberate widening of what an internet-facing SSR process
// can spend, so each one is argued in its own ticket rather than inherited.
const staffWriteHeader = "X-Commerce-Staff-Write-Token"

// organizerAssertionHeader carries catalog's organizer assertion. The same header
// name catalog reads (TKT-287 D6): the back office forwards one session value to
// both services.
const organizerAssertionHeader = "X-Catalog-Organizer-Assertion"

// staffOrganizer authenticates a staff-reachable internal operation and returns
// the organizer it may act for (TKT-287). Both must hold, in one decision:
//
//   - the back office's commerce credential (X-Commerce-Staff-Write-Token), which
//     authenticates the DEPUTY process and names no tenant; and
//   - a valid organizer assertion (X-Catalog-Organizer-Assertion), which catalog
//     minted at staff sign-in and commerce verifies with catalog's PUBLIC key
//     (shared/go/organizerassertion, ADR-058). Its organizer is the scope.
//
// The organizer comes from the verified assertion and from nowhere else: the
// three request contracts no longer carry organizer_id at all, so there is
// nothing to compare it with.
//
// The shared internal token does NOT open these operations, even with a valid
// assertion. That token names no tenant either, and it is held by every service;
// leaving it as an alternative would keep the gap open for the widest credential
// while closing it for the narrowest (ADR-058 § 5's argument, applied here).
//
// Inline, and every refusal is the same 404 (ADR-043, TKT-287 D3): a compromised
// back office probing with the staff credential learns nothing about which half
// it lacks. Fails closed on an unconfigured credential or verifier.
func (s *Server) staffOrganizer(r *http.Request) (uuid.UUID, bool) {
	// HeaderCredentialMatches is constant-time and refuses an empty configured
	// value, so an unconfigured credential admits nobody.
	if !httpx.HeaderCredentialMatches(r, staffWriteHeader, s.staffWriteToken) {
		return uuid.Nil, false
	}
	// A nil verifier refuses every token (organizerassertion.Verifier.Verify).
	scope, err := s.organizerAssertions.Verify(r.Header.Get(organizerAssertionHeader), time.Now())
	if err != nil {
		return uuid.Nil, false
	}
	return scope.OrganizerID, true
}
