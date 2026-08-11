package api

// Partner credential authentication (TKT-240 / ADR-056).
//
// This is a CONTRACT operation's guard, not an internal route's. ADR-043 draws
// that line: an internal route is 404'd at the edge and may compare a header in a
// handler, but a surface external callers reach declares `security:` and is
// enforced by the OpenAPI validator. The difference is not ceremony — it is what
// stops a newly added partner-shaped operation inheriting the declaration without
// the check, because the validator refuses on the DECLARATION rather than on
// whether someone remembered to call a guard.

import (
	"context"
	"errors"
	"net/http"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/google/uuid"

	store "ticketing/services/commerce/internal/store"
)

// partnerCredentialScheme is the securityScheme name in the contract, and
// partnerCredentialHeader is the header it declares. They are compared against
// what the document says by TestPartnerSchemeIsADeclaredHeaderKey: a scheme
// renamed in the spec and not here would stop matching and the guard would refuse
// everything, which is at least loud — but a HEADER renamed in the spec and not
// here would silently read a header nobody sends.
const (
	partnerCredentialScheme = "PartnerCredential"
	partnerCredentialHeader = "X-Partner-Credential"
)

// partnerScope is what a resolved credential authorises. It is filled by the
// authentication func and read by the handlers; nothing in it ever comes from the
// request body.
type partnerScope struct {
	CredentialID uuid.UUID
	ResellerID   uuid.UUID
	OrganizerID  uuid.UUID
	ChannelCode  string
}

// partnerScopeKey carries the authenticated scope from the request validator to
// the handler.
//
// The value behind it is a POINTER to a slot rather than the scope itself,
// because an AuthenticationFunc is handed the request, not the chain: a context
// is immutable and the func cannot replace the request the handler will later
// see. So the router installs an empty slot on the way in and the auth func fills
// it — one slot per request, so two partners selling at once cannot read each
// other's credential. (Same shape as access's scannerOrganizerKey.)
type partnerScopeKey struct{}

// withPartnerScopeSlot installs the empty slot. It must wrap the validator, not
// sit inside the router, because the validator runs before any middleware the chi
// router could carry.
func withPartnerScopeSlot(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), partnerScopeKey{}, new(partnerScope))))
	})
}

// partnerScopeFrom returns the authenticated scope. The second result is false
// when there is none, and every caller must treat that as a refusal rather than
// as an empty scope: a zero-value partnerScope names organizer uuid.Nil and
// channel "", and a handler that carried on would be running unauthenticated with
// a scope that compares equal to nothing.
func partnerScopeFrom(ctx context.Context) (partnerScope, bool) {
	slot, ok := ctx.Value(partnerScopeKey{}).(*partnerScope)
	if !ok || slot == nil || slot.CredentialID == uuid.Nil {
		return partnerScope{}, false
	}
	return *slot, true
}

// authenticatePartner resolves the presented credential and records its scope.
//
// Fails CLOSED at every step, including when the server has no database to look
// in: a commerce that cannot check a credential must refuse partner traffic,
// because the alternative is that the one configuration nobody exercises is the
// one that admits everybody.
//
// It returns one error for every failure. The refusal reaches the error handler
// as 401 and is rendered identically whatever went wrong — unknown, revoked,
// malformed and absent are the same answer, because a partner integration that
// can tell them apart can enumerate other partners' credentials.
func (s *Server) authenticatePartner(ctx context.Context, input *openapi3filter.AuthenticationInput) error {
	if input.SecuritySchemeName != partnerCredentialScheme {
		// An unknown scheme must not be silently treated as satisfied: that is how
		// a renamed or mistyped scheme becomes an open door.
		return errors.New("unauthorized")
	}
	if s.db == nil {
		return errors.New("unauthorized")
	}
	cred, err := store.AuthenticateResellerCredential(ctx,
		s.db, input.RequestValidationInput.Request.Header.Get(partnerCredentialHeader))
	if err != nil {
		return errors.New("unauthorized")
	}
	// Hand the credential's SCOPE to the handler. Resolving a credential and then
	// discarding what it was issued for is how access's scanner enrolment was
	// platform-wide while looking per-organizer (and how ADR-053's staff
	// credential reaches across tenants). The handlers below take organizer and
	// channel from here and from nowhere else.
	if slot, ok := input.RequestValidationInput.Request.Context().Value(partnerScopeKey{}).(*partnerScope); ok && slot != nil {
		*slot = partnerScope{
			CredentialID: cred.ID,
			ResellerID:   cred.ResellerID,
			OrganizerID:  cred.OrganizerID,
			ChannelCode:  cred.ChannelCode,
		}
	}
	return nil
}
