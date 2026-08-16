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
	"net/http"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/google/uuid"
)

// organizerScopeKey carries the verified scope from the request validator to the
// handler (TKT-245).
//
// The value behind it is a POINTER to a slot rather than the scope itself,
// because an AuthenticationFunc is handed the request, not the chain: a context
// is immutable and the func cannot replace the request the handler will later
// see. So the router installs an empty slot on the way in and the auth func fills
// it — one slot per request, so two staff members writing at once cannot read
// each other's organizer. (Same shape as commerce's partnerScopeKey, ADR-056.)
type organizerScopeKey struct{}

// withOrganizerScopeSlot installs the empty slot. It must wrap the validator, not
// sit inside the chi router, because the validator runs before any middleware the
// router could carry — and the validator is where the slot gets filled.
func withOrganizerScopeSlot(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), organizerScopeKey{}, new(organizerScope))))
	})
}

// organizerFromContext returns the verified organizer. The second result is false
// when there is none, and every caller must treat that as a REFUSAL rather than
// as an empty scope: a zero-value organizerScope names organizer uuid.Nil, and a
// handler that carried on would write rows belonging to nobody, or trip a foreign
// key as a 500 that reads like an outage.
func organizerFromContext(ctx context.Context) (organizerScope, bool) {
	slot, ok := ctx.Value(organizerScopeKey{}).(*organizerScope)
	if !ok || slot == nil || slot.OrganizerID == uuid.Nil {
		return organizerScope{}, false
	}
	return *slot, true
}

// authenticateOrganizerAssertion verifies the assertion and records its scope.
//
// Fails CLOSED at every step, including when the server has no key: a catalog
// that cannot check an assertion must refuse the writes that depend on one,
// because the alternative is that the one configuration nobody exercises is the
// one that admits everybody.
func (s *Server) authenticateOrganizerAssertion(_ context.Context, input *openapi3filter.AuthenticationInput) error {
	if input.SecuritySchemeName != organizerAssertionSecurityScheme {
		return fmt.Errorf("unauthorized")
	}
	req := input.RequestValidationInput.Request
	scope, err := verifyOrganizerAssertion(s.organizerAssertionKey,
		req.Header.Get(organizerAssertionHeader), time.Now())
	if err != nil {
		// Deliberately uninformative, and identical to the staff-credential
		// refusal: a caller that can tell "no assertion" from "expired" from
		// "signed by the wrong key" learns which of its guesses was structurally
		// right.
		return fmt.Errorf("unauthorized")
	}
	// Hand the VERIFIED scope to the handler. Resolving an assertion and then
	// discarding what it was minted for is how the organizer would quietly come
	// back from the request body; the handlers take it from here and nowhere else.
	if slot, ok := req.Context().Value(organizerScopeKey{}).(*organizerScope); ok && slot != nil {
		*slot = scope
	}
	return nil
}

// organizerFor resolves the verified organizer for a write, or refuses.
//
// Every converted handler goes through this and takes the organizer from nowhere
// else. It exists so the refusal is one behaviour rather than fifteen: a handler
// that forgot to check would otherwise write with uuid.Nil, and a handler that
// invented its own refusal would answer differently from its neighbours.
//
// Reaching here without a scope means the validator did not fill the slot, which
// means the operation did not declare the assertion — a contract bug, not a
// caller's mistake. It is still a 401 rather than a 500: the caller cannot tell
// the difference and must not be able to, and the contract test is what catches
// the real cause.
func (s *Server) organizerFor(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	scope, ok := organizerFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, Error{Error: "unauthorized"})
		return uuid.Nil, false
	}
	return scope.OrganizerID, true
}

// authenticateCatalogRequest dispatches on the scheme the contract declared.
//
// One func rather than two registrations because openapi3filter takes a single
// AuthenticationFunc; the dispatch is on SecuritySchemeName, and an unknown name
// is refused rather than treated as satisfied — that is how a renamed or mistyped
// scheme becomes an open door.
func (s *Server) authenticateCatalogRequest(ctx context.Context, input *openapi3filter.AuthenticationInput) error {
	switch input.SecuritySchemeName {
	case staffWriteSecurityScheme:
		return s.authenticateStaffWrite(ctx, input)
	case organizerAssertionSecurityScheme:
		return s.authenticateOrganizerAssertion(ctx, input)
	default:
		return fmt.Errorf("unauthorized")
	}
}

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
