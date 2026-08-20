// Package api implements the generated ServerInterface (openapi_gen.go —
// regenerate with `make generate`, never edit) against the store and event
// ports. Shape validation is the spec middleware's job; this layer owns
// business rules (locale completeness, timezone validity, tenancy mapping)
// and the ADR-004 cache tier on every public read.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	oapimiddleware "github.com/oapi-codegen/nethttp-middleware"

	"ticketing/shared/cachetier"
	"ticketing/shared/contract"

	apispec "ticketing/services/catalog/api"
	"ticketing/services/catalog/internal/events"
	"ticketing/services/catalog/internal/store"

	"ticketing/shared/httpx"
)

// SupportedLocales is the platform's live locale set — data, not schema
// (TKT-36): extending it is a code change here, not a contract change.
var SupportedLocales = []string{"en", "fr"}

// CacheControlPublicReads is the ADR-004 minutes tier carried by both
// aggregated public reads (event list, event detail).
//
// Derived from the tier registry rather than written out (TKT-204): the tier's
// lifetime and the header advertising it are now one number, so a cache built on
// the tier cannot outlive what the response promises. A var rather than a const
// only because Go cannot make a function-derived string constant; no call site
// needs a constant expression.
var CacheControlPublicReads = cachetier.Minutes.CacheControl()

// CacheControlPublicVenueReads is the ADR-004 hours tier: venue/seat-map
// geometry is long-lived, so the back-office venue read (US-018) is cacheable
// far longer than the events minutes tier. Seat-map reads earn it only when the
// whole payload is published — see cacheControlForSeatMaps (TKT-107).
var CacheControlPublicVenueReads = cachetier.Hours.CacheControl()

// cacheControlNever is ADR-004's never tier, rendered once at init rather than
// per request. The rendering is safe on any path for a registered tier, but
// keeping every cachetier call at package level means the panic that guards
// against an unregistered tier stays a process-start failure — and these routers
// install no panic-recovery middleware.
var cacheControlNever = cachetier.Never.CacheControl()

// cacheControlForSeatMaps is TKT-107's tier rule for the three public seat-map
// reads: the hours tier only when the response is non-empty and every seat map in
// it is published, otherwise no-store. Draft geometry is mutable, so an hour of
// shared-cache lifetime would make an authoring write look lost; published
// versions are immutable (ADR-029 — an edit inserts a new version rather than
// mutating one), which is what makes the hours branch correct rather than merely
// inherited. One response carries one header, so for a list the least-cacheable
// member decides. Any other status ('archived', or a future one — migration 0009
// constrains the column to draft/published/archived) fails closed to no-store.
//
// The emptiness guard is for ListVenueSeatMaps, the only one of the three that can
// return zero rows; ListSeatMapVersions returns ErrNotFound instead. Caching "this
// venue has no seat maps" for an hour would hide the venue's first map.
//
// no-store closes the *shared-cache* vector only. It is not access control: a
// reader who knows the id still gets the draft (ADR-004 § TKT-107 amendment).
func cacheControlForSeatMaps(maps ...store.SeatMap) string {
	if len(maps) == 0 {
		return cacheControlNever
	}
	for _, m := range maps {
		if m.Status != "published" {
			return cacheControlNever
		}
	}
	return CacheControlPublicVenueReads
}

type Server struct {
	store store.Store
	// public serves the four minute-tier public reads from memory (TKT-206).
	// Deliberately separate from `store`: every write and the seat-map reads keep
	// using `store` directly, so nothing but those four handlers can reach a
	// cached value. Asserted structurally, not by convention.
	public publicReader
	pub    events.Publisher
	log    *slog.Logger
	// internalCredential guards the hand-mounted /internal/* routes. It is the
	// SHARED service token: whatever holds it reaches every service.
	internalCredential string
	// staffWriteCredential guards every unsafe operation in the contract
	// (TKT-191). Deliberately a different, catalog-only value — see the
	// CatalogStaffWriteCredential security scheme and ADR-042.
	staffWriteCredential string
	// organizerAssertionKey signs the organizer assertion (TKT-245, ADR-058). It
	// answers a different question from staffWriteCredential, which is why it is a
	// second value rather than a reuse: that credential says "the back office is
	// calling", this key says "for organizer O". Set through
	// WithOrganizerAssertionKey; a server without it verifies nothing rather than
	// verifying everything (see assertion.go's empty-key check).
	organizerAssertionKey organizerAssertionKey
	// limiters bound the public staff-login surface (TKT-195). Reached through
	// lim(), never directly: a nil here must mean "build the real ones", not
	// "allow everything". See ratelimit.go.
	limiters *staffAuthLimiters
	limOnce  sync.Once
}

// staffWriteSecurityScheme is the name in the contract; staffWriteHeader is the
// header it declares. Both are read by write_credential_test.go, which asserts
// the contract and this server agree — a scheme naming a header nobody reads is
// documentation, not a guard.
const (
	staffWriteSecurityScheme = "CatalogStaffWriteCredential"
	staffWriteHeader         = "X-Catalog-Staff-Write-Token"
)

func NewServer(st store.Store, pub events.Publisher, log *slog.Logger, internalCredential, staffWriteCredential string) *Server {
	return newServer(st, pub, log, internalCredential, staffWriteCredential, newPublicReadCache(st))
}

// newServerWithPublicReader injects the display-read collaborator. Tests use it
// to count store loads and to prove no other handler can reach it.
func newServerWithPublicReader(st store.Store, pub events.Publisher, log *slog.Logger, internalCredential, staffWriteCredential string, pr publicReader) *Server {
	return newServer(st, pub, log, internalCredential, staffWriteCredential, pr)
}

func newServer(st store.Store, pub events.Publisher, log *slog.Logger, internalCredential, staffWriteCredential string, pr publicReader) *Server {
	return &Server{store: st, pub: pub, log: log, public: pr,
		internalCredential: internalCredential, staffWriteCredential: staffWriteCredential}
}

// WithOrganizerAssertionKey supplies the signing key (TKT-245). A setter rather
// than another positional argument to NewServer, for the same reason commerce's
// WithCustomerAssertionKey is one: every existing caller keeps compiling, and a
// server constructed without it verifies nothing rather than verifying everything.
func (s *Server) WithOrganizerAssertionKey(key string) *Server {
	s.organizerAssertionKey = organizerAssertionKey(key)
	return s
}

// publicReader is the narrow display-read collaborator — the four minute-tier
// reads and nothing else. No write is on it, so the write path cannot acquire a
// cached number even by accident.
type publicReader interface {
	// SetEnabled and Status are the operator kill-switch (TKT-210). On THIS
	// interface, not a separate controller, so the switch cannot address a
	// different object than the read path consults.
	SetEnabled(bool)
	Status() publicReadStatus
	ListPublishedEvents(ctx context.Context) (cached[[]store.EventAggregate], error)
	GetPublishedEvent(ctx context.Context, id uuid.UUID) (cached[store.EventAggregate], error)
	GetPublishedSeason(ctx context.Context, id uuid.UUID) (cached[store.SeasonAggregate], error)
	GetPublishedFestival(ctx context.Context, id uuid.UUID) (cached[store.FestivalAggregate], error)
}

// NewRouter mounts the generated routes wrapped in spec request validation
// (ADR-009 §3) on a fresh chi router. /healthz stays outside, in main.
func NewRouter(s *Server, validateResponses bool) (http.Handler, error) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(apispec.Spec)
	if err != nil {
		return nil, fmt.Errorf("load openapi spec: %w", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		return nil, fmt.Errorf("invalid openapi spec: %w", err)
	}
	validator := oapimiddleware.OapiRequestValidatorWithOptions(doc, &oapimiddleware.Options{
		ErrorHandler: func(w http.ResponseWriter, message string, statusCode int) {
			// A 401 is the staff-write guard refusing (TKT-191). Answer with a
			// fixed body rather than the validator's own phrasing ("security
			// requirements failed: …"), which is internal wording that would
			// reach an unauthenticated caller and vary with the library version.
			// Validation messages on other statuses stay: a 400 is telling a
			// legitimate caller what it got wrong.
			if statusCode == http.StatusUnauthorized {
				writeJSON(w, statusCode, Error{Error: "unauthorized"})
				return
			}
			writeJSON(w, statusCode, Error{Error: message})
		},
		// The contract's security requirement is enforced HERE rather than in 26
		// handlers (TKT-191). The document declares the requirement at the top
		// level and public reads opt out with `security: []`, so a newly added
		// operation is closed by construction — the failure mode this replaces is
		// an endpoint that ships unguarded because someone forgot a line.
		// Two schemes now (TKT-245): the staff-write credential says the back
		// office is calling, the organizer assertion says which tenant it is
		// calling for. authenticateCatalogRequest dispatches on the declared
		// scheme name and refuses an unknown one.
		Options: openapi3filter.Options{AuthenticationFunc: s.authenticateCatalogRequest},
	})
	r := chi.NewRouter()
	r.Get("/internal/ticket-types/{id}", s.getTicketType)
	r.Get("/internal/performances/{id}", s.getPublishedPerformance)
	r.Get("/internal/pools/{id}/offer-state", s.getPoolOfferState)
	r.Get("/internal/cache-control", s.cacheControlStatus)
	r.Put("/internal/cache-control", s.cacheControlSet)
	// TKT-80: inventory pins/unpins a seat-hold's seats here (ADR-029 contract). Hand-mounted
	// internal routes, like the reads above — service-to-service, not part of the public
	// OpenAPI contract; the response validator skips undeclared paths.
	r.Post("/internal/seat-maps/{id}/pins", s.pinSeats)
	r.Post("/internal/seat-maps/{id}/unpins", s.unpinSeats)
	// TKT-112: the read side of the same contract — inventory's one-shot reconcile-pins
	// drains this to find pins left behind by holds that expired on a pool nobody touched
	// again. Same hand-mounted, credential-guarded, out-of-contract convention.
	r.Get("/internal/seat-map-pins", s.listSeatMapPins)
	// TKT-235 operator channel reads. Undeclared and hand-mounted because
	// catalog's contract cannot express a staff-authenticated GET — see
	// channels.go for why.
	r.Get("/internal/channels", s.listChannels)
	r.Get("/internal/channels/{id}", s.getChannel)
	// Unauthenticated public surface: bound request bodies before any read.
	limitBody := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req.Body = http.MaxBytesReader(w, req.Body, 1<<20)
			next.ServeHTTP(w, req)
		})
	}
	handler := HandlerWithOptions(s, ChiServerOptions{
		BaseRouter:  r,
		Middlewares: []MiddlewareFunc{validator, limitBody},
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			writeJSON(w, http.StatusBadRequest, Error{Error: err.Error()})
		},
	})
	// Response drift fails closed (ADR-028): hand-built payloads are checked
	// against the committed spec at runtime, same as the non-codegen services.
	//
	// withOrganizerScopeSlot wraps EVERYTHING (TKT-245), outside both the internal
	// guard and the validator, because both fill or read it: the validator's
	// AuthenticationFunc fills the slot for contract operations, and the
	// hand-mounted /internal/channels read fills it inline. Installed here rather
	// than as a chi middleware because the validator runs before the router.
	validated, err := contract.ResponseValidator(apispec.Spec, guardInternalSurface(s, handler), s.log, validateResponses)
	if err != nil {
		return nil, err
	}
	return withOrganizerScopeSlot(validated), nil
}

// guardInternalSurface authenticates catalog's WHOLE /internal/ surface before
// routing, parameter binding or request validation (TKT-214 ai-review).
//
// Why it cannot live in the handler, or even in ChiServerOptions.Middlewares:
// the generated wrapper binds and validates path and query parameters BEFORE it
// applies HandlerMiddlewares (openapi_gen.go — the BindStyledParameterWithOptions
// / ErrorHandlerFunc block precedes the middleware loop). So a handler-level
// check answers 401 for a well-formed request and the VALIDATOR answers 400,
// with details, for a malformed one — handing an unauthenticated caller a
// schema oracle on the internal surface and making "the credential check is the
// first thing it does" false. Wrapping the finished handler is what makes that
// sentence true.
//
// It is a prefix guard rather than a per-route one on purpose: a newly declared
// /internal/ operation is then closed BY CONSTRUCTION, which is the same
// argument TKT-191 made for expressing the staff-write requirement once at the
// document level instead of in 26 handlers. The per-handler checks on the
// hand-mounted routes stay: they are cheap, and a guard that can be removed in
// one place should not be the only thing standing there.
//
// Side effect, named rather than discovered: an UNKNOWN /internal/ path now
// answers 401 instead of chi's 404. That is a strict improvement in the
// direction ADR-043 argues for — "enumerating the internal surface is the
// caller's problem to solve, not ours to help with" — and it is why this guard
// answers before routing rather than after it.
func guardInternalSurface(s *Server, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// r.URL.Path is the DECODED path, so an escaped spelling such as
		// internal%2Fx resolves here even where chi would route it elsewhere.
		// That direction is fail-closed: it can only guard more, never less.
		if !strings.HasPrefix(r.URL.Path, "/internal/") {
			next.ServeHTTP(w, r)
			return
		}
		if s.internalAuthorizedRequest(r) || s.staffMayReadOperatorChannels(r) {
			next.ServeHTTP(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, Error{Error: "unauthorized"})
	})
}

// internalAuthorizedRequest is the shared-token check, as a predicate. Split out
// of the guard so a second accepted credential can be expressed as an OR without
// duplicating the comparison.
func (s *Server) internalAuthorizedRequest(r *http.Request) bool {
	return httpx.HeaderCredentialMatches(r, httpx.InternalToken, s.internalCredential)
}

// staffMayReadOperatorChannels is TKT-236's single, deliberately narrow
// exception (ADR-053): the back office's catalog staff-write credential also
// opens `GET /internal/channels`, and nothing else.
//
// WHAT IT COSTS, corrected. An earlier version of this comment claimed the added
// blast radius was NIL, on the argument that a holder of the write credential
// could already learn which channels exist. **That was false, and ai-review
// caught it.** Before this read, the credential could only PROBE: a create
// against a guessed code returns 409 if it is taken, one code per request, and
// it never yields an id. This read returns every channel for a caller-supplied
// organizer in one call — ids, codes, kinds, and disabled rows that appear
// nowhere public. That is a real capability increase, and calling it nil was the
// kind of confident wrong claim ADR-021 exists to stop.
//
// WHAT THE ORGANIZER PREDICATE DOES AND DOES NOT DO. A second review pass caught
// this comment claiming more than the code delivers, for the SECOND time, so it
// is now written to what is demonstrable and nothing beyond it.
//
// `UpdateChannel` gained `AND organizer_id = $2` in the same pass. That predicate
// defends ONE adversary: a forged or mistaken request through the back-office
// form, where the page supplies its session's organizer and the channel id comes
// from a form field. Against that caller the predicate is a real boundary — a
// wrong id lands on no row.
//
// It does NOT defend against a stolen credential, and an earlier version of this
// comment said it did. Both the list's `organizer_id` and the update's are
// CALLER-SUPPLIED, so an attacker holding this token can list a victim's channels
// and then update the ids it just learned, naming the victim's organizer in both
// calls. The predicate matches and the write succeeds. That chain was executed
// against this code, not reasoned about: list returns 200 with the victim's
// channels, update returns 200 and mutates them.
//
// SO, PRECISELY:
//   - This read ADDS bulk enumeration of any organizer's channel configuration
//     — ids, codes, kinds, and disabled rows that appear nowhere public — to a
//     credential that previously had to probe one guessed code at a time.
//   - It does NOT add cross-tenant WRITE capability, because the same credential
//     already had it: `createChannel` and `updateChannel` take the organizer from
//     the request body and catalog cannot check it. Enumeration makes that
//     existing capability far easier to aim, which is a real amplification and
//     not a new power.
//   - The whole thing rests on one assumption, which is the actual security
//     property: **the back office is not compromised.** Catalog authenticates the
//     PROCESS (ADR-021 — name the adversary), and no predicate in this file can
//     change that.
//
// TKT-245 owns the fix — an organizer identity catalog can verify independently
// of the request body. Until it lands, ADR-053 states this assumption rather than
// implying a boundary that is not there.
//
// WHY IT DOES NOT GENERALIZE. The same reasoning fails for inventory's
// `channel-allocations` (TKT-244): no staff credential exists there, the back
// office holds nothing for that service, and the surface is a capacity write on
// the contention hot path. This is an allowlist entry, not a precedent for
// handing an SSR process an internal surface.
//
// WHY IT IS METHOD+PATH EXACT rather than a prefix. `/internal/channels/{id}`
// sits one character away and is NOT opened — the page does not need it, and an
// allowance is only narrow if something refuses the things next to it. Matching
// on a prefix would have quietly included the sibling read the moment it was
// added.
//
// WHAT IT IS NOT: tenant isolation. `organizer_id` is caller-supplied and this
// credential authenticates the DEPUTY PROCESS, not the staff member behind it
// (ADR-021 — name the adversary). The back office passes its session's organizer
// and never one taken from the request; catalog cannot enforce that, and neither
// this function nor ADR-053 claims it does.
func (s *Server) staffMayReadOperatorChannels(r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Path != "/internal/channels" {
		return false
	}
	if s.staffWriteCredential == "" {
		return false
	}
	// Constant-time: the comparison target is a secret, and an early-exit
	// compare leaks its prefix to anyone willing to measure. Same discipline as
	// authenticateStaffWrite.
	return subtle.ConstantTimeCompare(
		[]byte(r.Header.Get(staffWriteHeader)), []byte(s.staffWriteCredential)) == 1
}

func (s *Server) getTicketType(w http.ResponseWriter, r *http.Request) {
	if !httpx.HeaderCredentialMatches(r, httpx.InternalToken, s.internalCredential) {
		writeJSON(w, http.StatusUnauthorized, Error{Error: "unauthorized"})
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid ticket type id"})
		return
	}
	tt, err := s.store.GetTicketType(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": tt.ID, "organizer_id": tt.OrganizerID, "performance_id": tt.PerformanceID,
		"price": map[string]any{"amount": tt.PriceAmount, "currency": tt.Currency},
	})
}

func (s *Server) getPublishedPerformance(w http.ResponseWriter, r *http.Request) {
	if !httpx.HeaderCredentialMatches(r, httpx.InternalToken, s.internalCredential) {
		writeJSON(w, http.StatusUnauthorized, Error{Error: "unauthorized"})
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid performance id"})
		return
	}
	perf, err := s.store.GetPublishedPerformance(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	out := map[string]any{"id": perf.ID, "organizer_id": perf.OrganizerID, "capacity": perf.Capacity}
	if perf.CapacityGroupID != nil && perf.SharedCapacity != nil {
		out["capacity_group_id"] = perf.CapacityGroupID
		out["shared_capacity"] = perf.SharedCapacity
	}
	writeJSON(w, http.StatusOK, out)
}

// getPoolOfferState is the reconciliation read (TKT-90): a positive per-id
// answer whatever the id is — a performance in ANY lifecycle (archived included;
// the published-only lookup above 404s those by design), or a festival capacity
// group, which inventory skips rather than mistakes for a dead slot.
func (s *Server) getPoolOfferState(w http.ResponseWriter, r *http.Request) {
	if !httpx.HeaderCredentialMatches(r, httpx.InternalToken, s.internalCredential) {
		writeJSON(w, http.StatusUnauthorized, Error{Error: "unauthorized"})
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid pool id"})
		return
	}
	state, err := s.store.GetPoolOfferState(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if state.Kind == "festival" {
		writeJSON(w, http.StatusOK, map[string]any{"kind": "festival"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind":            "performance",
		"lifecycle":       state.Lifecycle,
		"closure_status":  state.Closure.Status,
		"closure_version": state.Closure.Version,
	})
}

// reconcileDefaultPinPage is the page size used when the caller names none. Callers may ask
// for less or more, up to store.MaxSeatMapPinPage; an unbounded page is not on offer.
const reconcileDefaultPinPage = 100

// listSeatMapPins is the reconciliation read (TKT-112): one keyset page of the pin table,
// cursor-driven so an operator run drains it without ever holding the whole table. Returns
// EVERY pin namespace — the reconciler decides what `hold:*` means, catalog does not.
func (s *Server) listSeatMapPins(w http.ResponseWriter, r *http.Request) {
	if !httpx.HeaderCredentialMatches(r, httpx.InternalToken, s.internalCredential) {
		writeJSON(w, http.StatusUnauthorized, Error{Error: "unauthorized"})
		return
	}
	after := uuid.Nil
	if raw := r.URL.Query().Get("after"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, Error{Error: "invalid cursor"})
			return
		}
		after = parsed
	}
	limit := reconcileDefaultPinPage
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > store.MaxSeatMapPinPage {
			writeJSON(w, http.StatusBadRequest, Error{Error: "invalid limit"})
			return
		}
		limit = parsed
	}
	pins, err := s.store.ListSeatMapPins(r.Context(), after, limit)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(pins))
	for _, pin := range pins {
		out = append(out, map[string]any{
			"id": pin.ID, "organizer_id": pin.OrganizerID, "seat_map_id": pin.SeatMapID,
			"seat_identity": pin.SeatIdentity, "pinned_by": pin.PinnedBy,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"pins": out})
}

// batchPinRequest is the body inventory sends to pin/unpin a seat-hold's seats (TKT-80).
type batchPinRequest struct {
	OrganizerID    uuid.UUID `json:"organizer_id"`
	SeatIdentities []string  `json:"seat_identities"`
	PinnedBy       string    `json:"pinned_by"`
}

func (s *Server) decodeBatchPin(w http.ResponseWriter, r *http.Request) (store.BatchPinInput, bool) {
	if !httpx.HeaderCredentialMatches(r, httpx.InternalToken, s.internalCredential) {
		writeJSON(w, http.StatusUnauthorized, Error{Error: "unauthorized"})
		return store.BatchPinInput{}, false
	}
	seatMapID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid seat map id"})
		return store.BatchPinInput{}, false
	}
	var body batchPinRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil ||
		body.OrganizerID == uuid.Nil || len(body.SeatIdentities) == 0 || strings.TrimSpace(body.PinnedBy) == "" {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid pin request"})
		return store.BatchPinInput{}, false
	}
	return store.BatchPinInput{OrganizerID: body.OrganizerID, SeatMapID: seatMapID, SeatIdentities: body.SeatIdentities, PinnedBy: body.PinnedBy}, true
}

func (s *Server) pinSeats(w http.ResponseWriter, r *http.Request) {
	in, ok := s.decodeBatchPin(w, r)
	if !ok {
		return
	}
	if err := s.store.PinSeats(r.Context(), in); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "pinned"})
}

func (s *Server) unpinSeats(w http.ResponseWriter, r *http.Request) {
	in, ok := s.decodeBatchPin(w, r)
	if !ok {
		return
	}
	if err := s.store.UnpinSeats(r.Context(), in); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "unpinned"})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, Error{Error: "referenced entity not found"})
	case errors.Is(err, store.ErrOrganizerMismatch):
		writeJSON(w, http.StatusBadRequest, Error{Error: "entities must belong to the same organizer"})
	case errors.Is(err, store.ErrNotSellable):
		writeJSON(w, http.StatusConflict, Error{Error: "performance has no ticket type; create one before publishing"})
	case errors.Is(err, store.ErrIllegalTransition):
		writeJSON(w, http.StatusConflict, Error{Error: "illegal performance lifecycle transition"})
	case errors.Is(err, store.ErrClosurePending):
		writeJSON(w, http.StatusConflict, Error{Error: "previous closure event still owed; retry that transition first"})
	case errors.Is(err, store.ErrMembershipConflict):
		writeJSON(w, http.StatusConflict, Error{Error: "group membership conflicts with an existing member or position"})
	case errors.Is(err, store.ErrMembershipFrozen):
		writeJSON(w, http.StatusConflict, Error{Error: "series membership is frozen after launch"})
	case errors.Is(err, store.ErrEmptySeries):
		writeJSON(w, http.StatusConflict, Error{Error: "series has no members"})
	case errors.Is(err, store.ErrSlotKindMismatch):
		writeJSON(w, http.StatusConflict, Error{Error: "only festival_day slots can join a festival"})
	case errors.Is(err, store.ErrAlreadyGrouped):
		writeJSON(w, http.StatusConflict, Error{Error: "slot already belongs to a festival"})
	case errors.Is(err, store.ErrGroupedSlotLifecycle):
		writeJSON(w, http.StatusConflict, Error{Error: "grouped festival day must be published/archived via its festival"})
	case errors.Is(err, store.ErrFestivalNotDraft):
		writeJSON(w, http.StatusConflict, Error{Error: "festival is not draft"})
	case errors.Is(err, store.ErrEmptyFestival):
		writeJSON(w, http.StatusConflict, Error{Error: "festival has no members"})
	case errors.Is(err, store.ErrSeatMapConflict):
		// Covers both authoring (duplicate section/row name or position) and an
		// edit that submits a duplicate seat identity (TKT-105) — one sentinel, so
		// the message names both causes rather than misdescribing an edit conflict.
		writeJSON(w, http.StatusConflict, Error{Error: "duplicate seat identity, or duplicate name or position within the seat map"})
	case errors.Is(err, store.ErrSeatMapNotPublished):
		writeJSON(w, http.StatusConflict, Error{Error: "seat map must be published before a slot can be seated against it"})
	case errors.Is(err, store.ErrSeatMapEditOrphansPinned):
		// TKT-105/ADR-029: an edit that drops a seat identity pinned by a sale or
		// hold is a conflict, not a 500. The message is actionable for the UI.
		writeJSON(w, http.StatusConflict, Error{Error: "edit would orphan a seat identity pinned by a sale or hold; the new geometry must keep every pinned seat (same section/row/seat labels)"})
	case errors.Is(err, store.ErrChannelCodeTaken):
		writeJSON(w, http.StatusConflict, Error{Error: "this organizer already has a channel with that code"})
	case errors.Is(err, store.ErrChannelCodeImmutable):
		// TKT-235: a channel code is immutable. Renaming would orphan the code
		// already recorded on live claims, fee rules and split schedules, none of
		// which reference the registry (ADR-024), so nothing would cascade and
		// nothing would complain. The message says what to do instead.
		writeJSON(w, http.StatusConflict, Error{Error: "channel code is immutable; create a new channel and disable this one instead of renaming"})
	case errors.Is(err, store.ErrChannelInvalidInput):
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid channel: code 1..100 bytes, display_name 1..200, kind one of web/pos/presale/reseller"})
	case errors.Is(err, store.ErrSeatIdentityNotFound):
		// Defensive: never let this store sentinel fall through to 500. No HTTP
		// path triggers it today (PinSeat is store-only), but if one does, it is a
		// conflict against the current published version, not a missing resource.
		writeJSON(w, http.StatusConflict, Error{Error: "seat identity is not present in the current published version"})
	default:
		s.log.ErrorContext(r.Context(), "store error", "err", err)
		writeJSON(w, http.StatusInternalServerError, Error{Error: "internal error"})
	}
}

// validateLocalized enforces the i18n-from-birth rule (owner decision,
// 2026-07-12): required localized fields carry every supported locale.
func validateLocalized(field string, text LocalizedString) error {
	for _, loc := range SupportedLocales {
		if text[loc] == "" {
			return fmt.Errorf("%s must include non-empty %q text", field, loc)
		}
	}
	return nil
}
