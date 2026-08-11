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
// as the control that actually addresses submission volume — installed in
// ratelimit.go, and until ai-review S4 caught it, named here and nowhere else.
func (s *Server) AuthenticateStaff(w http.ResponseWriter, r *http.Request) {
	var in StaffCredentials
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid request body"})
		return
	}

	// After the decode, because the identifier is the subject key; before the
	// store call, because a bucket that fills only for accounts that EXIST turns
	// the 429 into the account oracle the shared 401 exists to prevent.
	if !s.allowStaffAuth(w, r, in.Identifier) {
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

	role, ok := recognisedStaffRole(principal.Role)
	if !ok {
		// A stored role outside the contract's vocabulary is a DATA problem, not
		// a credential one, and the distinction decides where an operator looks.
		// Answering 401 would say "your password is wrong" about a password that
		// is right. Answering 200 with the unknown value would hand the back
		// office a role its matrix cannot classify — and an unclassifiable role
		// reaching a session is the fail-open TKT-197 exists to prevent.
		//
		// So it is a 500: something is wrong on this side, not the caller's. The
		// operator symptom is distinctive — one account 500s at sign-in while
		// everyone else works — and docs/development.md names it.
		s.log.ErrorContext(r.Context(), "staff account has a role outside the contract vocabulary",
			"staff_id", principal.ID, "role", principal.Role)
		writeJSON(w, http.StatusInternalServerError, Error{Error: "authentication unavailable"})
		return
	}

	writeJSON(w, http.StatusOK, StaffPrincipal{
		StaffId:     principal.ID,
		OrganizerId: principal.OrganizerID,
		Role:        role,
	})
}

// recognisedStaffRole maps a stored string onto the contract's vocabulary.
//
// Enumerated against the GENERATED constants rather than string literals, so the
// contract enum stays the single source: adding a role to openapi.yaml without
// adding it here fails to compile, which is the point.
func recognisedStaffRole(stored string) (StaffRole, bool) { return RecognisedStaffRole(stored) }

// RecognisedStaffRole is the exported form, so `provision-staff` validates an
// operator's --role against the same generated constants the service does.
// One vocabulary, two callers — not two vocabularies.
func RecognisedStaffRole(stored string) (StaffRole, bool) {
	switch StaffRole(stored) {
	case Admin:
		return Admin, true
	case BoxOffice:
		return BoxOffice, true
	case Finance:
		return Finance, true
	}
	return "", false
}

// StaffRoleNames lists the vocabulary for operator-facing messages. Derived from
// the generated constants, so a role added to the contract shows up in the CLI's
// help without anyone remembering to update a string.
func StaffRoleNames() []string {
	roles := []StaffRole{Admin, BoxOffice, Finance}
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, string(r))
	}
	return out
}
