package api

// Back-office staff authentication (TKT-190 / US-B1). See ADR-042 for why staff
// identity lives in catalog and what this endpoint does and does not constrain.

import (
	"encoding/json"
	"errors"
	"net/http"

	"ticketing/services/catalog/internal/store"
)

// invalidStaffCredentials is the ONE refusal body. Built once, as a constant, so
// the unknown-identifier and wrong-password paths cannot drift apart later: two
// call sites constructing "the same" message are two call sites one of which
// eventually says something slightly different, and the difference is exactly
// what an account enumerator is looking for.
const invalidStaffCredentials = "invalid credentials"

// AuthenticateStaff verifies a sign-in attempt and returns the principal.
//
// The endpoint is public (no X-Internal-Token): the login form in front of it is
// necessarily anonymous, so an internal-only endpoint would move the front door
// without locking it — while handing the public-facing back-office process the
// shared credential that also opens commerce's refunds and inventory's
// operational holds. ADR-042 records the trade and names TKT-195 (rate limiting)
// as the control that actually addresses submission volume.
func (s *Server) AuthenticateStaff(w http.ResponseWriter, r *http.Request) {
	var in StaffCredentials
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid request body"})
		return
	}

	principal, err := s.store.AuthenticateStaff(r.Context(), in.Identifier, in.Password)
	switch {
	case errors.Is(err, store.ErrStaffCredentialsInvalid):
		writeJSON(w, http.StatusUnauthorized, Error{Error: invalidStaffCredentials})
		return
	case err != nil:
		// Log for the operator, say nothing to the caller: a lookup failure is
		// not a credential verdict, and answering 401 here would tell someone
		// their correct password is wrong while an outage went unreported.
		s.log.ErrorContext(r.Context(), "staff authentication failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, Error{Error: "authentication unavailable"})
		return
	}

	writeJSON(w, http.StatusOK, StaffPrincipal{
		StaffId:     principal.ID,
		OrganizerId: principal.OrganizerID,
	})
}
