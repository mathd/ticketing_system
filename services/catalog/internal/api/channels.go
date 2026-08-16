package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"ticketing/services/catalog/internal/store"
)

// Handlers for the sales-channel registry (TKT-235 / epic TKT-17).
//
// The registry is a LOOKUP, NOT A CONSTRAINT: nothing here validates a channel
// code against the registry on any sale path, and nothing may start to. Four
// columns in three services store channel codes with no foreign key to this
// table (ADR-024, so historical attribution survives a channel being retired),
// which means an unregistered code sells exactly as it did before.
//
// The public list is served straight from the store, deliberately outside
// catalog's public-read cache. See listPublicChannels below.

func channelResponse(c store.Channel) Channel {
	return Channel{
		Id:          c.ID,
		OrganizerId: c.OrganizerID,
		Code:        c.Code,
		DisplayName: c.DisplayName,
		Kind:        ChannelKind(c.Kind),
		Enabled:     c.Enabled,
		CreatedAt:   c.CreatedAt,
		UpdatedAt:   c.UpdatedAt,
	}
}

func (s *Server) CreateChannel(w http.ResponseWriter, r *http.Request) {
	var in ChannelCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	// Omitted means enabled: a channel is created sellable unless said
	// otherwise.
	//
	// The DEFAULT IS THE CONTRACT'S, not this handler's. `enabled` is declared
	// `default: true`, and kin-openapi's request validator materializes schema
	// defaults into the body before the handler runs — so `in.Enabled` is never
	// nil here on a validated request. A `if in.Enabled == nil { enabled = true }`
	// branch beside it would be unreachable, and worse than merely dead: it
	// reads as the thing enforcing the default, so a later edit to the contract's
	// default would look covered by a handler that never runs.
	//
	// Verified by mutation rather than assumed — flipping this to false leaves
	// the create-with-omitted-enabled test green ONLY if the validator is
	// supplying the value, which is what makes the nil case impossible.
	// The nil check that remains is a fail-safe for an unvalidated construction
	// path, and it defaults the same way the contract does.
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	c, err := s.store.CreateChannel(r.Context(), store.ChannelInput{
		OrganizerID: organizerID,
		Code:        in.Code,
		DisplayName: in.DisplayName,
		Kind:        store.ChannelKind(in.Kind),
		Enabled:     enabled,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, channelResponse(c))
}

// The operator reads — hand-mounted on /internal/, not declared in the contract.
//
// This is forced, not preferred, and the constraint is worth stating because the
// obvious design does not compile against it. Catalog's contract holds a derived
// invariant (TKT-191, write_credential_test.go): every UNSAFE operation requires
// CatalogStaffWriteCredential and every SAFE one opts out with `security: []`.
// The credential is a *write* credential, so "a staff-authenticated GET" is not
// expressible in this document at all — declaring these reads with `security:`
// fails that test, and declaring them public would publish the disabled-channel
// list.
//
// So they follow catalog's other guarded reads (getTicketType,
// getPublishedPerformance, getPoolOfferState, listSeatMapPins): hand-mounted,
// undeclared, 401 on refusal, behind guardInternalSurface. cache_control.go
// records why catalog keeps its internal surface out of the contract —
// declaring one route would make it the only declared internal route, a new
// inconsistency rather than a fix.
//
// The cost, stated: no generated types and no ADR-028 response validation on
// these two, and the back office (TKT-236) must reach them through the same
// path catalog's other internal reads use rather than through the gateway,
// which denies /api/catalog/internal/ at the edge.

func (s *Server) listChannels(w http.ResponseWriter, r *http.Request) {
	// Two accepted credentials on this ONE route (TKT-236 / ADR-053): the shared
	// internal token that every other internal route takes, and the back office's
	// catalog staff-write token. See staffMayReadOperatorChannels in server.go
	// for why the second is safe here and why it does not generalize.
	//
	// The per-handler check stays even though guardInternalSurface already
	// refused an unauthorized request before routing — the prefix guard is the
	// one that must not be the only thing standing there, and the two must agree
	// about this route or the defence-in-depth becomes a 401 on a request the
	// guard admitted. A test pins that agreement.
	if !s.internalAuthorizedRequest(r) && !s.staffMayReadOperatorChannels(r) {
		writeJSON(w, http.StatusUnauthorized, Error{Error: "unauthorized"})
		return
	}
	// The organizer comes from the assertion, not from the query string (TKT-245).
	// This read is exactly the enumeration ADR-053 recorded: it returns every
	// channel of a CALLER-NAMED organizer, ids and disabled rows included, which is
	// what turned a credential that could only probe one guessed code at a time
	// into one that could list a victim's whole configuration.
	//
	// The check is INLINE rather than declared, because this route is hand-mounted
	// and outside the contract — the validator never sees it, so there is no
	// AuthenticationFunc to fill the slot. ADR-043 draws that line: contract
	// operation, declared security; internal route, inline check.
	//
	// Verified before removing the parameter: the back office is the ONLY caller
	// repo-wide (web/backoffice/src/lib/catalog.ts), so no service-to-service
	// caller loses the ability to name an organizer it is not.
	scope, err := verifyOrganizerAssertion(s.organizerAssertionKey,
		r.Header.Get(organizerAssertionHeader), time.Now())
	if err != nil {
		// Same uninformative refusal as everywhere else: absent, expired, forged
		// and malformed are one answer.
		writeJSON(w, http.StatusUnauthorized, Error{Error: "unauthorized"})
		return
	}
	organizerID := scope.OrganizerID
	channels, err := s.store.ListChannels(r.Context(), organizerID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	out := ChannelList{Channels: make([]Channel, 0, len(channels))}
	for _, c := range channels {
		out.Channels = append(out.Channels, channelResponse(c))
	}
	// Carries disabled channels — organizer configuration, never shared-cacheable.
	w.Header().Set("Cache-Control", cacheControlNever)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getChannel(w http.ResponseWriter, r *http.Request) {
	if !s.internalAuthorized(w, r) {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid channel id"})
		return
	}
	c, err := s.store.GetChannel(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", cacheControlNever)
	writeJSON(w, http.StatusOK, channelResponse(c))
}

func (s *Server) UpdateChannel(w http.ResponseWriter, r *http.Request, channelId ChannelId) {
	var in ChannelUpdate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	// The organizer predicate TKT-236 added to this query is now fed by the
	// VERIFIED organizer rather than a caller-supplied one. That predicate always
	// scoped the write; what it could not do was tell whether the organizer it
	// scoped to was the caller's to name (ADR-053).
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	c, err := s.store.UpdateChannel(r.Context(), organizerID, channelId, store.ChannelUpdate{
		Code:        in.Code,
		DisplayName: in.DisplayName,
		Kind:        store.ChannelKind(in.Kind),
		Enabled:     in.Enabled,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", cacheControlNever)
	writeJSON(w, http.StatusOK, channelResponse(c))
}

// ListPublicChannels serves the storefront's channel selector.
//
// NOT fronted by catalog's public-read cache (public_read_cache.go), and that is
// a decision rather than an omission. The cache exists for the four aggregated
// storefront reads, which take a caller-supplied UUID on an unauthenticated
// route at on-sale volume; a registry list is low-volume organizer
// configuration. The repo already classified this shape twice the same way —
// public_read_invalidation_test.go records CreateFeeRule and CreateSplitSchedule
// as affecting no cached public payload — and adding a cache later is additive
// while retracting invalidation semantics is not.
//
// Cache-Control still declares the ADR-004 minutes tier, so shared caches and
// the storefront behave exactly as they do for the cached reads. No Age header:
// nothing here ages in memory, and Age is required only on responses that can
// come back already stale (see the PublicReadAge header's own rationale).
func (s *Server) ListPublicChannels(w http.ResponseWriter, r *http.Request, params ListPublicChannelsParams) {
	channels, err := s.store.ListEnabledChannels(r.Context(), params.OrganizerId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	out := PublicChannelList{Channels: make([]PublicChannel, 0, len(channels))}
	for _, c := range channels {
		out.Channels = append(out.Channels, PublicChannel{Code: c.Code, DisplayName: c.DisplayName})
	}
	w.Header().Set("Cache-Control", CacheControlPublicReads)
	writeJSON(w, http.StatusOK, out)
}
