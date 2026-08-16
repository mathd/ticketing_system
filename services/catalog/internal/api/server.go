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
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	oapimiddleware "github.com/oapi-codegen/nethttp-middleware"
	openapi_types "github.com/oapi-codegen/runtime/types"

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
		Options: openapi3filter.Options{AuthenticationFunc: s.authenticateStaffWrite},
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
	return contract.ResponseValidator(apispec.Spec, guardInternalSurface(s, handler), s.log, validateResponses)
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

func (s *Server) CreateVenue(w http.ResponseWriter, r *http.Request) {
	var in VenueCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	v, err := s.store.CreateVenue(r.Context(), store.VenueInput{
		OrganizerID: in.OrganizerId,
		Name:        in.Name,
		GACapacity:  in.GaCapacity,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, Venue{
		Id: v.ID, OrganizerId: v.OrganizerID, Name: v.Name,
		GaCapacity: v.GACapacity, CreatedAt: v.CreatedAt,
	})
}

func (s *Server) CreateEvent(w http.ResponseWriter, r *http.Request) {
	var in EventCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	if err := validateLocalized("name", in.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: err.Error()})
		return
	}
	var desc store.LocalizedText
	if in.Description != nil {
		desc = store.LocalizedText(*in.Description)
	}
	ev, err := s.store.CreateEvent(r.Context(), store.EventInput{
		OrganizerID: in.OrganizerId,
		Name:        store.LocalizedText(in.Name),
		Description: desc,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, eventToAPI(ev))
}

func eventToAPI(ev store.Event) Event {
	out := Event{
		Id: ev.ID, OrganizerId: ev.OrganizerID,
		Name: LocalizedString(ev.Name), CreatedAt: ev.CreatedAt,
	}
	if len(ev.Description) > 0 {
		d := LocalizedString(ev.Description)
		out.Description = &d
	}
	return out
}

func seriesToAPI(s store.Series) Series {
	members := make([]SeriesMember, 0, len(s.Members))
	for _, m := range s.Members {
		members = append(members, SeriesMember{PerformanceId: m.PerformanceID, Position: m.Position})
	}
	return Series{Id: s.ID, OrganizerId: s.OrganizerID, EventId: s.EventID, Name: LocalizedString(s.Name), Members: members, CreatedAt: s.CreatedAt}
}

func seasonToAPI(s store.Season) Season {
	return Season{Id: s.ID, OrganizerId: s.OrganizerID, Name: LocalizedString(s.Name), SeriesIds: s.SeriesIDs, EventIds: s.EventIDs, CreatedAt: s.CreatedAt}
}

func festivalToAPI(f store.Festival) Festival {
	return Festival{
		Id: f.ID, OrganizerId: f.OrganizerID, Name: LocalizedString(f.Name),
		SharedCapacity: f.SharedCapacity, Status: FestivalStatus(f.Status),
		MemberIds: f.MemberIDs, CreatedAt: f.CreatedAt,
	}
}

func (s *Server) CreateSeries(w http.ResponseWriter, r *http.Request) {
	var in SeriesCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	if err := validateLocalized("name", in.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: err.Error()})
		return
	}
	out, err := s.store.CreateSeries(r.Context(), store.SeriesInput{OrganizerID: in.OrganizerId, EventID: in.EventId, Name: store.LocalizedText(in.Name)})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, seriesToAPI(out))
}

func (s *Server) AttachPerformanceToSeries(w http.ResponseWriter, r *http.Request, seriesId SeriesId) {
	var in SeriesPerformanceAttach
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	out, err := s.store.AttachPerformanceToSeries(r.Context(), seriesId, in.PerformanceId, in.Position)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, seriesToAPI(out))
}

func (s *Server) CreateSeason(w http.ResponseWriter, r *http.Request) {
	var in SeasonCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	if err := validateLocalized("name", in.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: err.Error()})
		return
	}
	out, err := s.store.CreateSeason(r.Context(), store.SeasonInput{OrganizerID: in.OrganizerId, Name: store.LocalizedText(in.Name)})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, seasonToAPI(out))
}

func (s *Server) AttachSeriesToSeason(w http.ResponseWriter, r *http.Request, seasonId SeasonId) {
	var in SeasonSeriesAttach
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	out, err := s.store.AttachSeriesToSeason(r.Context(), seasonId, in.SeriesId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, seasonToAPI(out))
}

func (s *Server) AttachEventToSeason(w http.ResponseWriter, r *http.Request, seasonId SeasonId) {
	var in SeasonEventAttach
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	out, err := s.store.AttachEventToSeason(r.Context(), seasonId, in.EventId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, seasonToAPI(out))
}

func (s *Server) CreateFestival(w http.ResponseWriter, r *http.Request) {
	var in FestivalCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	if err := validateLocalized("name", in.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: err.Error()})
		return
	}
	if in.SharedCapacity <= 0 {
		writeJSON(w, http.StatusBadRequest, Error{Error: "shared_capacity must be positive"})
		return
	}
	out, err := s.store.CreateFestival(r.Context(), store.FestivalInput{
		OrganizerID: in.OrganizerId, Name: store.LocalizedText(in.Name), SharedCapacity: in.SharedCapacity,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, festivalToAPI(out))
}

func (s *Server) AttachDayToFestival(w http.ResponseWriter, r *http.Request, festivalId FestivalId) {
	var in FestivalDayAttach
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	out, err := s.store.AttachDayToFestival(r.Context(), festivalId, in.PerformanceId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, festivalToAPI(out))
}

func (s *Server) CreatePerformance(w http.ResponseWriter, r *http.Request) {
	var in PerformanceCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	if _, err := time.LoadLocation(in.Timezone); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: fmt.Sprintf("unknown timezone %q", in.Timezone)})
		return
	}
	kind := store.KindPerformance
	if in.Kind != nil {
		kind = string(*in.Kind)
	}
	// Per-kind temporal invariant: the spec can't express "required-if-kind",
	// so it is enforced here (the DB CHECK is the backstop). A performance
	// carries an instant; a day kind carries the operating window.
	switch kind {
	case store.KindPerformance:
		if in.StartsAt == nil {
			writeJSON(w, http.StatusBadRequest, Error{Error: "kind 'performance' requires starts_at"})
			return
		}
		if in.OperatingDate != nil || in.OpensAt != nil || in.ClosesAt != nil {
			writeJSON(w, http.StatusBadRequest, Error{Error: "kind 'performance' must not carry an operating window"})
			return
		}
	case store.KindFestivalDay, store.KindOperatingDay:
		if in.StartsAt != nil {
			writeJSON(w, http.StatusBadRequest, Error{Error: "day kinds must not carry starts_at"})
			return
		}
		if in.OperatingDate == nil || in.OpensAt == nil || in.ClosesAt == nil {
			writeJSON(w, http.StatusBadRequest, Error{Error: "day kinds require operating_date, opens_at and closes_at"})
			return
		}
	}
	re := store.ReEntryPolicy{Mode: "single"}
	if in.ReEntry != nil {
		re = store.ReEntryPolicy{Mode: string(in.ReEntry.Mode), MaxEntries: in.ReEntry.MaxEntries, RequiresExit: in.ReEntry.RequiresExit}
	}
	if re.Mode == "count_limited" && re.MaxEntries == nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "re_entry mode 'count_limited' requires max_entries"})
		return
	}
	if re.Mode != "count_limited" && re.MaxEntries != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "max_entries is only valid for re_entry mode 'count_limited'"})
		return
	}
	input := store.PerformanceInput{
		OrganizerID: in.OrganizerId,
		EventID:     in.EventId,
		VenueID:     in.VenueId,
		Kind:        kind,
		StartsAt:    in.StartsAt,
		OpensAt:     in.OpensAt,
		ClosesAt:    in.ClosesAt,
		Timezone:    in.Timezone,
		ReEntry:     re,
		SeatMapID:   in.SeatMapId,
	}
	if in.OperatingDate != nil {
		d := in.OperatingDate.Time
		input.OperatingDate = &d
	}
	p, err := s.store.CreatePerformance(r.Context(), input)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, performanceToAPI(p))
}

func performanceToAPI(p store.Performance) Performance {
	out := Performance{
		Id: p.ID, OrganizerId: p.OrganizerID, EventId: p.EventID, VenueId: p.VenueID,
		Kind: SlotKind(p.Kind), StartsAt: p.StartsAt, OpensAt: p.OpensAt, ClosesAt: p.ClosesAt,
		Timezone: p.Timezone,
		ReEntry: ReEntryPolicy{
			Mode: ReEntryPolicyMode(p.ReEntry.Mode), MaxEntries: p.ReEntry.MaxEntries,
			RequiresExit: p.ReEntry.RequiresExit,
		},
		Closure: Closure{
			Status: ClosureStatus(p.Closure.Status), ClosedAt: p.Closure.ClosedAt, Reason: p.Closure.Reason,
		},
		Status: PerformanceStatus(p.Status), PublishedAt: p.PublishedAt,
		ArchivedAt: p.ArchivedAt, CapacityGroupId: p.CapacityGroupID, SeatMapId: p.SeatMapID,
		CreatedAt: p.CreatedAt,
	}
	if p.OperatingDate != nil {
		out.OperatingDate = &openapi_types.Date{Time: *p.OperatingDate}
	}
	return out
}

// PublishPerformance is idempotent on the resource and at-least-once on the
// event: the domain event is emitted only while unacknowledged
// (event_emitted_at null), so a failed emission is retried by re-POSTing
// publish. Crash between DB commit and ack remains the recorded US-004
// deferral (ADR-009).
//
// **Not organizer-scoped — TKT-199, deferred.** This and ArchivePerformance
// take only a slot id, while every sibling write scopes by (id, organizer_id).
// Any holder of the staff-write credential can therefore transition ANY
// organizer's slot by naming its id. The deferral rests on one precondition:
// `organizers` has exactly one row (migration 0002) and nothing outside a
// migration inserts into it, so there is no second tenant to cross into.
// Nothing enforces that precondition — seed a second organizer and this becomes
// a live cross-tenant write with no test, no startup check and no signal.
// Whoever does so owns TKT-199 first. (TKT-22 refactor: triage re-confirmed.)
func (s *Server) PublishPerformance(w http.ResponseWriter, r *http.Request, performanceId PerformanceId) {
	p, needsEmit, err := s.store.PublishPerformance(r.Context(), performanceId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if needsEmit {
		if err := s.pub.PerformancePublished(r.Context(), p); err != nil {
			s.log.ErrorContext(r.Context(), "domain event emission failed; re-POST publish to retry",
				"performance_id", p.ID, "err", err)
			writeJSON(w, http.StatusInternalServerError,
				Error{Error: "performance is published but the domain event was not emitted; retry publish"})
			return
		}
		if err := s.store.MarkPerformanceEventEmitted(r.Context(), p.ID); err != nil {
			// Ack'd but unmarked: the next publish retry may re-emit — that
			// is the at-least-once contract, consumers de-duplicate on id.
			s.log.ErrorContext(r.Context(), "event emitted but not marked", "performance_id", p.ID, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, performanceToAPI(p))
}

// ArchivePerformance is resource-idempotent and event-at-least-once. If the
// publication marker is still null, publication is emitted and marked before
// the archive event so the lifecycle cannot silently drop a domain event.
func (s *Server) ArchivePerformance(w http.ResponseWriter, r *http.Request, performanceId PerformanceId) {
	p, publishNeedsEmit, archiveNeedsEmit, err := s.store.ArchivePerformance(r.Context(), performanceId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if publishNeedsEmit {
		if err := s.pub.PerformancePublished(r.Context(), p); err != nil {
			s.log.ErrorContext(r.Context(), "owed publication event emission failed", "performance_id", p.ID, "err", err)
			writeJSON(w, http.StatusInternalServerError, Error{Error: "performance is archived but its publication event was not emitted; retry archive"})
			return
		}
	}
	if archiveNeedsEmit {
		if err := s.pub.PerformanceArchived(r.Context(), p); err != nil {
			s.log.ErrorContext(r.Context(), "archive event emission failed", "performance_id", p.ID, "err", err)
			writeJSON(w, http.StatusInternalServerError, Error{Error: "performance is archived but the archive event was not emitted; retry archive"})
			return
		}
	}
	// Mark only after every owed event has been emitted. A failure between
	// emissions therefore retries the already-emitted publication too; its
	// deterministic id makes that safe at the stream.
	if publishNeedsEmit {
		if err := s.store.MarkPerformanceEventEmitted(r.Context(), p.ID); err != nil {
			s.log.ErrorContext(r.Context(), "publication event emitted but not marked", "performance_id", p.ID, "err", err)
		}
	}
	if archiveNeedsEmit {
		if err := s.store.MarkPerformanceArchiveEmitted(r.Context(), p.ID); err != nil {
			s.log.ErrorContext(r.Context(), "archive event emitted but not marked", "performance_id", p.ID, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, performanceToAPI(p))
}

func (s *Server) PublishSeries(w http.ResponseWriter, r *http.Request, seriesId SeriesId) {
	items, err := s.store.PublishSeries(r.Context(), seriesId)
	if err != nil {
		s.writeSeriesTransitionError(w, r, err)
		return
	}
	for _, item := range items {
		if item.PublishNeedsEmit {
			if err = s.pub.PerformancePublished(r.Context(), item.Performance); err != nil {
				writeJSON(w, http.StatusInternalServerError, Error{Error: "series is published but a member event is still owed; retry publish"})
				return
			}
			if markErr := s.store.MarkPerformanceEventEmitted(r.Context(), item.Performance.ID); markErr != nil {
				s.log.ErrorContext(r.Context(), "series publication emitted but not marked", "performance_id", item.Performance.ID, "err", markErr)
			}
		}
	}
	s.writeSeriesResult(w, seriesId, items)
}

func (s *Server) ArchiveSeries(w http.ResponseWriter, r *http.Request, seriesId SeriesId) {
	items, err := s.store.ArchiveSeries(r.Context(), seriesId)
	if err != nil {
		s.writeSeriesTransitionError(w, r, err)
		return
	}
	for _, item := range items {
		if item.PublishNeedsEmit {
			if err = s.pub.PerformancePublished(r.Context(), item.Performance); err != nil {
				writeJSON(w, http.StatusInternalServerError, Error{Error: "series is archived but a member publication event is still owed; retry archive"})
				return
			}
			if markErr := s.store.MarkPerformanceEventEmitted(r.Context(), item.Performance.ID); markErr != nil {
				s.log.ErrorContext(r.Context(), "series publication emitted but not marked", "performance_id", item.Performance.ID, "err", markErr)
			}
		}
		if item.ArchiveNeedsEmit {
			if err = s.pub.PerformanceArchived(r.Context(), item.Performance); err != nil {
				writeJSON(w, http.StatusInternalServerError, Error{Error: "series is archived but a member archive event is still owed; retry archive"})
				return
			}
			if markErr := s.store.MarkPerformanceArchiveEmitted(r.Context(), item.Performance.ID); markErr != nil {
				s.log.ErrorContext(r.Context(), "series archive emitted but not marked", "performance_id", item.Performance.ID, "err", markErr)
			}
		}
	}
	s.writeSeriesResult(w, seriesId, items)
}

func (s *Server) writeSeriesResult(w http.ResponseWriter, id uuid.UUID, items []store.SeriesTransition) {
	performances := make([]Performance, 0, len(items))
	for _, item := range items {
		performances = append(performances, performanceToAPI(item.Performance))
	}
	writeJSON(w, http.StatusOK, SeriesLifecycleResult{SeriesId: id, Performances: performances})
}

func (s *Server) writeSeriesTransitionError(w http.ResponseWriter, r *http.Request, err error) {
	var conflict *store.SeriesTransitionConflict
	if errors.As(err, &conflict) {
		id := conflict.PerformanceID
		writeJSON(w, http.StatusConflict, SeriesTransitionConflict{Error: "series transition blocked", Reason: conflict.Reason, BlockingPerformanceId: &id})
		return
	}
	if errors.Is(err, store.ErrEmptySeries) {
		writeJSON(w, http.StatusConflict, SeriesTransitionConflict{Error: "series transition blocked", Reason: "series has no members"})
		return
	}
	s.writeStoreError(w, r, err)
}

func (s *Server) PublishFestival(w http.ResponseWriter, r *http.Request, festivalId FestivalId) {
	items, err := s.store.PublishFestival(r.Context(), festivalId)
	if err != nil {
		s.writeFestivalTransitionError(w, r, err)
		return
	}
	for _, item := range items {
		if item.PublishNeedsEmit {
			if err = s.pub.PerformancePublished(r.Context(), item.Performance); err != nil {
				writeJSON(w, http.StatusInternalServerError, Error{Error: "festival is published but a member event is still owed; retry publish"})
				return
			}
			if markErr := s.store.MarkPerformanceEventEmitted(r.Context(), item.Performance.ID); markErr != nil {
				s.log.ErrorContext(r.Context(), "festival publication emitted but not marked", "performance_id", item.Performance.ID, "err", markErr)
			}
		}
	}
	s.writeFestivalResult(w, festivalId, items)
}

func (s *Server) ArchiveFestival(w http.ResponseWriter, r *http.Request, festivalId FestivalId) {
	items, err := s.store.ArchiveFestival(r.Context(), festivalId)
	if err != nil {
		s.writeFestivalTransitionError(w, r, err)
		return
	}
	for _, item := range items {
		if item.PublishNeedsEmit {
			if err = s.pub.PerformancePublished(r.Context(), item.Performance); err != nil {
				writeJSON(w, http.StatusInternalServerError, Error{Error: "festival is archived but a member publication event is still owed; retry archive"})
				return
			}
			if markErr := s.store.MarkPerformanceEventEmitted(r.Context(), item.Performance.ID); markErr != nil {
				s.log.ErrorContext(r.Context(), "festival publication emitted but not marked", "performance_id", item.Performance.ID, "err", markErr)
			}
		}
		if item.ArchiveNeedsEmit {
			if err = s.pub.PerformanceArchived(r.Context(), item.Performance); err != nil {
				writeJSON(w, http.StatusInternalServerError, Error{Error: "festival is archived but a member archive event is still owed; retry archive"})
				return
			}
			if markErr := s.store.MarkPerformanceArchiveEmitted(r.Context(), item.Performance.ID); markErr != nil {
				s.log.ErrorContext(r.Context(), "festival archive emitted but not marked", "performance_id", item.Performance.ID, "err", markErr)
			}
		}
	}
	s.writeFestivalResult(w, festivalId, items)
}

func (s *Server) writeFestivalResult(w http.ResponseWriter, id uuid.UUID, items []store.SeriesTransition) {
	performances := make([]Performance, 0, len(items))
	for _, item := range items {
		performances = append(performances, performanceToAPI(item.Performance))
	}
	writeJSON(w, http.StatusOK, FestivalLifecycleResult{FestivalId: id, Performances: performances})
}

func (s *Server) writeFestivalTransitionError(w http.ResponseWriter, r *http.Request, err error) {
	var conflict *store.FestivalTransitionConflict
	if errors.As(err, &conflict) {
		id := conflict.PerformanceID
		writeJSON(w, http.StatusConflict, SeriesTransitionConflict{Error: "festival transition blocked", Reason: conflict.Reason, BlockingPerformanceId: &id})
		return
	}
	if errors.Is(err, store.ErrEmptyFestival) {
		writeJSON(w, http.StatusConflict, SeriesTransitionConflict{Error: "festival transition blocked", Reason: "festival has no members"})
		return
	}
	s.writeStoreError(w, r, err)
}

// CloseSlot sets the orthogonal closure attribute to closed and emits the
// closed event at least once while owed (deterministic id per closure_version,
// so retried or raced emissions de-duplicate). Any publication event still owed
// for this slot is emitted first, so a closure never overtakes the slot's
// publication. Resource-idempotent: closing an already-closed slot returns 200
// and only re-emits while an event is still owed.
func (s *Server) CloseSlot(w http.ResponseWriter, r *http.Request, performanceId PerformanceId) {
	var in SlotCloseRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	p, publishNeedsEmit, closureNeedsEmit, err := s.store.CloseSlot(r.Context(), performanceId, in.Reason)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.emitClosure(w, r, p, publishNeedsEmit, closureNeedsEmit, s.pub.SlotClosed, "close")
}

// ReopenSlot mirrors CloseSlot for the reverse transition.
func (s *Server) ReopenSlot(w http.ResponseWriter, r *http.Request, performanceId PerformanceId) {
	p, publishNeedsEmit, closureNeedsEmit, err := s.store.ReopenSlot(r.Context(), performanceId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.emitClosure(w, r, p, publishNeedsEmit, closureNeedsEmit, s.pub.SlotReopened, "reopen")
}

// emitClosure emits any owed publication first, then the closure event, marking
// each only after it is emitted — the publication-before-closure ordering and
// at-least-once discipline that ArchivePerformance already uses. A failure
// between emissions retries the already-emitted publication too; its
// deterministic id makes that safe at the stream.
func (s *Server) emitClosure(w http.ResponseWriter, r *http.Request, p store.Performance,
	publishNeedsEmit, closureNeedsEmit bool, emitClosureEvent func(context.Context, store.Performance) error, verb string) {
	if publishNeedsEmit {
		if err := s.pub.PerformancePublished(r.Context(), p); err != nil {
			s.log.ErrorContext(r.Context(), "owed publication event emission failed", "performance_id", p.ID, "err", err)
			writeJSON(w, http.StatusInternalServerError,
				Error{Error: "slot state changed but its publication event was not emitted; retry " + verb})
			return
		}
		if err := s.store.MarkPerformanceEventEmitted(r.Context(), p.ID); err != nil {
			s.log.ErrorContext(r.Context(), "publication event emitted but not marked", "performance_id", p.ID, "err", err)
		}
	}
	if closureNeedsEmit {
		if err := emitClosureEvent(r.Context(), p); err != nil {
			s.log.ErrorContext(r.Context(), "closure event emission failed; retry to re-emit",
				"performance_id", p.ID, "verb", verb, "err", err)
			writeJSON(w, http.StatusInternalServerError,
				Error{Error: "slot state changed but the closure event was not emitted; retry " + verb})
			return
		}
		if err := s.store.MarkClosureEmitted(r.Context(), p.ID, p.Closure.Version); err != nil {
			s.log.ErrorContext(r.Context(), "closure event emitted but not marked", "performance_id", p.ID, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, performanceToAPI(p))
}

func (s *Server) CreateTicketType(w http.ResponseWriter, r *http.Request) {
	var in TicketTypeCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	if err := validateLocalized("name", in.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: err.Error()})
		return
	}
	tt, err := s.store.CreateTicketType(r.Context(), store.TicketTypeInput{
		OrganizerID:   in.OrganizerId,
		PerformanceID: in.PerformanceId,
		Name:          store.LocalizedText(in.Name),
		PriceAmount:   in.Price.Amount,
		Currency:      in.Price.Currency,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, TicketType{
		Id: tt.ID, OrganizerId: tt.OrganizerID, PerformanceId: tt.PerformanceID,
		Name:      LocalizedString(tt.Name),
		Price:     Money{Amount: tt.PriceAmount, Currency: tt.Currency},
		CreatedAt: tt.CreatedAt,
	})
}

// publicStartsAt is the slot's representative instant on the public path: the
// store COALESCEs a day kind's operating-window opening moment into StartsAt,
// so it is always set here; the guard is defensive against a nil.
func publicStartsAt(p store.Performance) time.Time {
	if p.StartsAt != nil {
		return *p.StartsAt
	}
	return time.Time{}
}

func localeSupported(locale string) bool {
	return slices.Contains(SupportedLocales, locale)
}

// resolve picks the requested locale with defaultLocale fallback for
// optional fields (required fields are complete by construction at create).
func resolve(text store.LocalizedText, locale string) string {
	if v := text[locale]; v != "" {
		return v
	}
	return text[SupportedLocales[0]]
}

func (s *Server) ListPublicEvents(w http.ResponseWriter, r *http.Request, params ListPublicEventsParams) {
	if !localeSupported(params.Locale) {
		writeJSON(w, http.StatusBadRequest, Error{Error: fmt.Sprintf("unsupported locale %q", params.Locale)})
		return
	}
	read, err := s.public.ListPublishedEvents(r.Context())
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	aggs := read.Value
	out := PublicEventList{Events: make([]PublicEventSummary, 0, len(aggs))}
	for _, agg := range aggs {
		out.Events = append(out.Events, eventSummary(agg, params.Locale))
	}
	w.Header().Set("Cache-Control", CacheControlPublicReads)
	setPublicReadAge(w, read.Age)
	writeJSON(w, http.StatusOK, out)
}

// ListPublicVenues serves the back-office venue list (US-018): an organizer's
// venues, name-ordered, at the ADR-004 hours tier. organizer_id is a required
// query param (parsed + validated by the generated wrapper); scoping is a store
// predicate (ADR-002). The response is the full Venue payload so the contract
// (ADR-028) is satisfied without hand-shaping.
func (s *Server) ListPublicVenues(w http.ResponseWriter, r *http.Request, params ListPublicVenuesParams) {
	venues, err := s.store.ListVenues(r.Context(), params.OrganizerId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	out := PublicVenueList{Venues: make([]Venue, 0, len(venues))}
	for _, v := range venues {
		out.Venues = append(out.Venues, Venue{
			Id:          v.ID,
			OrganizerId: v.OrganizerID,
			Name:        v.Name,
			GaCapacity:  v.GACapacity,
			CreatedAt:   v.CreatedAt,
		})
	}
	w.Header().Set("Cache-Control", CacheControlPublicVenueReads)
	writeJSON(w, http.StatusOK, out)
}

func eventSummary(agg store.EventAggregate, locale string) PublicEventSummary {
	sum := PublicEventSummary{
		Id:           agg.Event.ID,
		Name:         resolve(agg.Event.Name, locale),
		Performances: make([]PublicPerformanceSummary, 0, len(agg.Performances)),
	}
	if d := resolve(agg.Event.Description, locale); d != "" {
		sum.Description = &d
	}
	for _, pa := range agg.Performances {
		from := pa.TicketTypes[0]
		for _, tt := range pa.TicketTypes[1:] {
			if tt.PriceAmount < from.PriceAmount {
				from = tt
			}
		}
		sum.Performances = append(sum.Performances, PublicPerformanceSummary{
			Id: pa.Performance.ID, StartsAt: publicStartsAt(pa.Performance),
			Timezone: pa.Performance.Timezone, VenueName: pa.Venue.Name,
			FromPrice: Money{Amount: from.PriceAmount, Currency: from.Currency},
		})
	}
	return sum
}

func (s *Server) GetPublicEvent(w http.ResponseWriter, r *http.Request, eventId EventId, params GetPublicEventParams) {
	if !localeSupported(params.Locale) {
		writeJSON(w, http.StatusBadRequest, Error{Error: fmt.Sprintf("unsupported locale %q", params.Locale)})
		return
	}
	read, err := s.public.GetPublishedEvent(r.Context(), eventId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	agg := read.Value
	detail := publicEventDetail(agg, params.Locale)
	w.Header().Set("Cache-Control", CacheControlPublicReads)
	setPublicReadAge(w, read.Age)
	writeJSON(w, http.StatusOK, detail)
}

func publicEventDetail(agg store.EventAggregate, locale string) PublicEventDetail {
	detail := PublicEventDetail{
		Id:           agg.Event.ID,
		OrganizerId:  agg.Event.OrganizerID,
		Name:         resolve(agg.Event.Name, locale),
		Series:       make([]PublicSeriesContext, 0, len(agg.Series)),
		Performances: make([]PublicPerformanceDetail, 0, len(agg.Performances)),
	}
	if d := resolve(agg.Event.Description, locale); d != "" {
		detail.Description = &d
	}
	for _, sa := range agg.Series {
		detail.Series = append(detail.Series, PublicSeriesContext{Id: sa.Series.ID, Name: resolve(sa.Series.Name, locale), PerformanceIds: sa.PerformanceIDs})
	}
	for _, pa := range agg.Performances {
		pd := PublicPerformanceDetail{
			Id: pa.Performance.ID, StartsAt: publicStartsAt(pa.Performance),
			Timezone:    pa.Performance.Timezone,
			Venue:       PublicVenue{Id: pa.Venue.ID, Name: pa.Venue.Name},
			SeatMapId:   pa.Performance.SeatMapID,
			TicketTypes: make([]PublicTicketType, 0, len(pa.TicketTypes)),
		}
		for _, tt := range pa.TicketTypes {
			pd.TicketTypes = append(pd.TicketTypes, PublicTicketType{
				Id: tt.ID, Name: resolve(tt.Name, locale),
				Price: Money{Amount: tt.PriceAmount, Currency: tt.Currency},
			})
		}
		detail.Performances = append(detail.Performances, pd)
	}
	return detail
}

func (s *Server) GetPublicSeason(w http.ResponseWriter, r *http.Request, seasonId SeasonId, params GetPublicSeasonParams) {
	if !localeSupported(params.Locale) {
		writeJSON(w, http.StatusBadRequest, Error{Error: fmt.Sprintf("unsupported locale %q", params.Locale)})
		return
	}
	read, err := s.public.GetPublishedSeason(r.Context(), seasonId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	agg := read.Value
	out := PublicSeasonDetail{Id: agg.Season.ID, OrganizerId: agg.Season.OrganizerID, Name: resolve(agg.Season.Name, params.Locale), Events: make([]PublicEventDetail, 0, len(agg.Events))}
	for _, event := range agg.Events {
		out.Events = append(out.Events, publicEventDetail(event, params.Locale))
	}
	w.Header().Set("Cache-Control", CacheControlPublicReads)
	setPublicReadAge(w, read.Age)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) GetPublicFestival(w http.ResponseWriter, r *http.Request, festivalId FestivalId, params GetPublicFestivalParams) {
	if !localeSupported(params.Locale) {
		writeJSON(w, http.StatusBadRequest, Error{Error: fmt.Sprintf("unsupported locale %q", params.Locale)})
		return
	}
	read, err := s.public.GetPublishedFestival(r.Context(), festivalId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	agg := read.Value
	out := PublicFestivalDetail{
		Id: agg.Festival.ID, OrganizerId: agg.Festival.OrganizerID,
		Name: resolve(agg.Festival.Name, params.Locale), Days: make([]PublicPerformanceDetail, 0, len(agg.Performances)),
	}
	for _, pa := range agg.Performances {
		day := PublicPerformanceDetail{
			Id: pa.Performance.ID, StartsAt: publicStartsAt(pa.Performance), Timezone: pa.Performance.Timezone,
			Venue: PublicVenue{Id: pa.Venue.ID, Name: pa.Venue.Name}, TicketTypes: make([]PublicTicketType, 0, len(pa.TicketTypes)),
		}
		for _, tt := range pa.TicketTypes {
			day.TicketTypes = append(day.TicketTypes, PublicTicketType{
				Id: tt.ID, Name: resolve(tt.Name, params.Locale), Price: Money{Amount: tt.PriceAmount, Currency: tt.Currency},
			})
		}
		out.Days = append(out.Days, day)
	}
	w.Header().Set("Cache-Control", CacheControlPublicReads)
	setPublicReadAge(w, read.Age)
	writeJSON(w, http.StatusOK, out)
}

// GetOpenAPISpec serves the committed contract byte-identical (ADR-009 §4).
func (s *Server) GetOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(apispec.Spec)
}

// --- Seat-map authoring (US-019 / TKT-102). Draft-only writes at the trusted
// root; geometry + summary reads under /public at the ADR-004 hours tier. ---

func seatMapPayload(m store.SeatMap) SeatMap {
	return SeatMap{
		Id: m.ID, OrganizerId: m.OrganizerID, VenueId: m.VenueID, Name: m.Name,
		Version: m.Version, Status: SeatMapStatus(m.Status), PublishedAt: m.PublishedAt,
		OrphanPreventionEnabled: m.OrphanPreventionEnabled,
		CreatedAt:               m.CreatedAt,
	}
}

// PublishSeatMap is idempotent on the resource and at-least-once on the event
// (TKT-103), the same emit-after-commit owed-marker contract as
// PublishPerformance: the seat_map.published event is emitted only while
// unacknowledged (event_emitted_at null), so a failed emission is retried by
// re-POSTing publish; consumers de-duplicate on the deterministic id.
func (s *Server) PublishSeatMap(w http.ResponseWriter, r *http.Request, seatMapId SeatMapId) {
	m, needsEmit, err := s.store.PublishSeatMap(r.Context(), seatMapId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if needsEmit {
		if err := s.pub.SeatMapPublished(r.Context(), m); err != nil {
			s.log.ErrorContext(r.Context(), "seat-map domain event emission failed; re-POST publish to retry",
				"seat_map_id", m.ID, "err", err)
			writeJSON(w, http.StatusInternalServerError,
				Error{Error: "seat map is published but the domain event was not emitted; retry publish"})
			return
		}
		if err := s.store.MarkSeatMapEventEmitted(r.Context(), m.ID); err != nil {
			// Ack'd but unmarked: a publish retry may re-emit — the at-least-once
			// contract, consumers de-duplicate on the deterministic id.
			s.log.ErrorContext(r.Context(), "seat-map event emitted but not marked", "seat_map_id", m.ID, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, seatMapPayload(m))
}

// EditSeatMap surfaces the TKT-104 safe-edit contract (ADR-029) over HTTP
// (TKT-105). It is a thin wrapper: the store re-resolves the family's current
// published version under a family advisory lock, validates that every pinned
// seat identity survives, and INSERTs a new published version — the HTTP layer
// re-implements none of that. An orphaning edit surfaces as
// ErrSeatMapEditOrphansPinned -> 409 via writeStoreError. The new version owes
// its own seat_map.published event, so this mirrors PublishSeatMap's
// emit-after-commit owed-marker discipline (a failed emission -> 500; recovery
// is re-POSTing publish of the NEW version id, NOT retrying the edit, which
// would mint yet another version).
//
// The 500 is declared in the spec (TKT-108), so the recovery hint reaches the
// client through the ADR-028 response validator. The new version is intact and
// event-owed; operators recover via the owed-event retry the same way as for
// publish.
func (s *Server) EditSeatMap(w http.ResponseWriter, r *http.Request, seatMapId SeatMapId) {
	var in SeatMapEdit
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	m, needsEmit, err := s.store.EditSeatMap(r.Context(), editInput(seatMapId, in))
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if needsEmit {
		if err := s.pub.SeatMapPublished(r.Context(), m); err != nil {
			s.log.ErrorContext(r.Context(), "edited seat-map event emission failed; re-POST publish of the new version to retry",
				"seat_map_id", m.ID, "version", m.Version, "err", err)
			writeJSON(w, http.StatusInternalServerError,
				Error{Error: "the new version is published but its domain event was not emitted; retry by publishing the new version"})
			return
		}
		if err := s.store.MarkSeatMapEventEmitted(r.Context(), m.ID); err != nil {
			s.log.ErrorContext(r.Context(), "edited seat-map event emitted but not marked", "seat_map_id", m.ID, "err", err)
		}
	}
	writeJSON(w, http.StatusCreated, seatMapPayload(m))
}

// editInput maps the wire SeatMapEdit (any version id + full geometry tree) to
// the store's EditSeatMapInput. Seat identity is composed server-side from the
// labels, so no id plumbing is needed.
func editInput(seatMapID SeatMapId, in SeatMapEdit) store.EditSeatMapInput {
	sections := make([]store.EditSectionInput, 0, len(in.Sections))
	for _, sec := range in.Sections {
		rows := make([]store.EditRowInput, 0, len(sec.Rows))
		for _, row := range sec.Rows {
			seats := make([]store.EditSeatInput, 0, len(row.Seats))
			for _, seat := range row.Seats {
				seats = append(seats, store.EditSeatInput{Label: seat.Label, Position: seat.Position})
			}
			rows = append(rows, store.EditRowInput{Label: row.Label, Position: row.Position, Seats: seats})
		}
		sections = append(sections, store.EditSectionInput{Name: sec.Name, Position: sec.Position, Rows: rows})
	}
	// nil INHERITS the edited version's setting; a value applies to the new version
	// only. The pointer survives the mapping deliberately — collapsing it to a bool
	// here would turn "the staffer said nothing" into "the staffer said off"
	// (ADR-041).
	return store.EditSeatMapInput{
		OrganizerID: in.OrganizerId, SeatMapID: seatMapID, Sections: sections,
		OrphanPreventionEnabled: in.OrphanPreventionEnabled,
	}
}

// ListSeatMapVersions is the TKT-105 version-history read (COS-3): the family's
// versions newest-first, current_version = highest published. Status-driven cache
// tier (cacheControlForSeatMaps, TKT-107), catalog-owned.
func (s *Server) ListSeatMapVersions(w http.ResponseWriter, r *http.Request, seatMapId SeatMapId) {
	versions, err := s.store.ListSeatMapVersions(r.Context(), seatMapId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	out := SeatMapVersionHistory{Versions: make([]SeatMap, 0, len(versions))}
	for _, v := range versions {
		out.Versions = append(out.Versions, seatMapPayload(v))
		// versions are newest-first, so the first published row is the current one.
		if out.CurrentVersion == nil && v.Status == "published" {
			cv := v.Version
			out.CurrentVersion = &cv
		}
	}
	w.Header().Set("Cache-Control", cacheControlForSeatMaps(versions...))
	writeJSON(w, http.StatusOK, out)
}

// UpdateVenueGaCapacity sets a venue's GA capacity (TKT-105 COS-5). Write ->
// no-store.
func (s *Server) UpdateVenueGaCapacity(w http.ResponseWriter, r *http.Request, venueId VenueId) {
	var in VenueGaCapacityUpdate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	v, err := s.store.UpdateVenueGACapacity(r.Context(), store.VenueGACapacityInput{
		OrganizerID: in.OrganizerId, VenueID: venueId, GACapacity: in.GaCapacity,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, Venue{
		Id: v.ID, OrganizerId: v.OrganizerID, Name: v.Name,
		GaCapacity: v.GACapacity, CreatedAt: v.CreatedAt,
	})
}

func (s *Server) CreateSeatMap(w http.ResponseWriter, r *http.Request, venueId VenueId) {
	var in SeatMapCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	m, err := s.store.CreateSeatMap(r.Context(), store.SeatMapInput{
		OrganizerID: in.OrganizerId, VenueID: venueId, Name: in.Name,
		// Absent means false: a caller that has never heard of the rule creates
		// exactly the map it created before (ADR-041).
		OrphanPreventionEnabled: in.OrphanPreventionEnabled != nil && *in.OrphanPreventionEnabled,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, seatMapPayload(m))
}

func (s *Server) AddSeatMapSection(w http.ResponseWriter, r *http.Request, seatMapId SeatMapId) {
	var in SeatMapSectionCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	sec, err := s.store.AddSeatMapSection(r.Context(), store.SeatMapSectionInput{
		OrganizerID: in.OrganizerId, SeatMapID: seatMapId, Name: in.Name, Position: in.Position,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, SeatSection{Id: sec.ID, Name: sec.Name, Position: sec.Position})
}

func (s *Server) AddSeatMapRow(w http.ResponseWriter, r *http.Request, seatMapId SeatMapId) {
	var in SeatMapRowCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	row, err := s.store.AddSeatMapRow(r.Context(), store.SeatMapRowInput{
		OrganizerID: in.OrganizerId, SeatMapID: seatMapId, SectionID: in.SectionId,
		Label: in.Label, Position: in.Position,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, SeatRow{Id: row.ID, Label: row.Label, Position: row.Position})
}

func (s *Server) AddSeatMapSeat(w http.ResponseWriter, r *http.Request, seatMapId SeatMapId) {
	var in SeatMapSeatCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	seat, err := s.store.AddSeatMapSeat(r.Context(), store.SeatMapSeatInput{
		OrganizerID: in.OrganizerId, SeatMapID: seatMapId, RowID: in.RowId,
		Label: in.Label, Position: in.Position,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, Seat{
		Id: seat.ID, SeatIdentity: seat.SeatIdentity, Label: seat.Label, Position: seat.Position,
	})
}

func (s *Server) GetPublicSeatMapGeometry(w http.ResponseWriter, r *http.Request, seatMapId SeatMapId) {
	g, err := s.store.GetSeatMapGeometry(r.Context(), seatMapId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	out := SeatMapGeometry{Map: seatMapPayload(g.Map), Sections: make([]SeatSection, 0, len(g.Sections))}
	for _, sec := range g.Sections {
		rows := make([]SeatRow, 0, len(sec.Rows))
		for _, row := range sec.Rows {
			seats := make([]Seat, 0, len(row.Seats))
			for _, st := range row.Seats {
				seats = append(seats, Seat{
					Id: st.ID, SeatIdentity: st.SeatIdentity, Label: st.Label, Position: st.Position,
				})
			}
			outRow := SeatRow{Id: row.ID, Label: row.Label, Position: row.Position, Seats: &seats}
			rows = append(rows, outRow)
		}
		out.Sections = append(out.Sections, SeatSection{
			Id: sec.ID, Name: sec.Name, Position: sec.Position, Rows: &rows,
		})
	}
	w.Header().Set("Cache-Control", cacheControlForSeatMaps(g.Map))
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) ListVenueSeatMaps(w http.ResponseWriter, r *http.Request, venueId VenueId) {
	maps, err := s.store.ListVenueSeatMaps(r.Context(), venueId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	out := SeatMapList{SeatMaps: make([]SeatMap, 0, len(maps))}
	for _, m := range maps {
		out.SeatMaps = append(out.SeatMaps, seatMapPayload(m))
	}
	w.Header().Set("Cache-Control", cacheControlForSeatMaps(maps...))
	writeJSON(w, http.StatusOK, out)
}
