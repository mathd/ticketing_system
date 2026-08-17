package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	apispec "ticketing/services/commerce/api"
	commerceevents "ticketing/services/commerce/internal/events"
	"ticketing/services/commerce/internal/refunds"
	commercestore "ticketing/services/commerce/internal/store"
	"ticketing/shared/contract"
	"ticketing/shared/fakepsp"
	"ticketing/shared/httpx"
)

var errCheckoutConflict = errors.New("checkout request conflicts with existing order")

// errRecoveryInProgress reports that the recovery runner holds this order under a live
// lease. Unlike errCheckoutConflict this is transient: the lease lapses, and the buyer's
// retry then either finds the order resolved or claims it cleanly.
var errRecoveryInProgress = errors.New("order is being recovered; retry shortly")

type Server struct {
	db                                           *sql.DB
	client                                       *http.Client
	catalogURL, inventoryURL, paymentsURL, token string
	// The back office's commerce credential (TKT-194); see staff_credential.go.
	staffWriteToken string
	// paymentsToken opens payments' internal surface, and ONLY payments'
	// (ai-review S8). `token` above still opens catalog's and inventory's; the
	// money surface was split off it because one value shared by five services
	// meant a compromise anywhere reached charge, void and refund.
	//
	// Empty falls back to `token`, which keeps every existing New caller and every
	// test construction working unchanged. main.go never leaves it empty — a
	// deployment that did would simply be back where it started, not broken.
	paymentsToken string
	// The HMAC key for customer checkout assertions (TKT-221); see assertion.go.
	// Commerce-only, and main.go refuses to start when it equals either other
	// credential.
	assertionKey customerAssertionKey
	// accessURL drives the ticket-voiding half of a refund reversal (TKT-157). Empty
	// leaves the obligation outstanding rather than failing the refund — the money has
	// already moved by then.
	accessURL string
	// publicURL is the buyer-facing origin password-reset links are built from
	// (TKT-226). Server-configured and never derived from a request: a link base taken
	// from the Host header lets a caller mail a victim a genuine reset link pointing at
	// the attacker's site. Mirrors access's PUBLIC_BASE_URL.
	publicURL string
	publisher commerceevents.Publisher
	// refunds is the one unit of work for refunding an order, shared with the
	// event-cancellation bulk runner (TKT-159). Rebuilt by WithAccess because the access
	// URL arrives after New.
	refunds *refunds.Service
	// limiters bound the public, credential-free customer surface (TKT-224,
	// ADR-051). In-process and per-replica — see shared/go/ratelimit's package doc
	// for exactly what that does and does not bound.
	//
	// Reached through lim(), never directly. Plenty of tests build `&Server{}` as a
	// literal, and a nil check that treated "no limiter" as "allow" would make every
	// such construction silently unlimited — a guard that cannot see, which is the
	// failure mode this repo has already written down twice. lim() builds the real
	// thing instead, so the default is enforcement.
	limiters *customerLimiters
	limOnce  sync.Once
}

func New(db *sql.DB, client *http.Client, catalog, inventory, payments, token string, publishers ...commerceevents.Publisher) *Server {
	var publisher commerceevents.Publisher
	if len(publishers) > 0 {
		publisher = publishers[0]
	}
	s := &Server{db: db, client: client, catalogURL: strings.TrimSuffix(catalog, "/"), inventoryURL: strings.TrimSuffix(inventory, "/"), paymentsURL: strings.TrimSuffix(payments, "/"), token: token, publisher: publisher}
	s.refunds = refunds.New(db, s.call, s.paymentsURL, s.accessURL, s.inventoryURL)
	s.limiters = newCustomerLimiters(nil)
	return s
}

// internalTokenFor picks the credential the destination accepts.
//
// Decided from the URL rather than at the call site, and that is deliberate:
// `call` has seventeen callers across four files, several of which reach two
// different services from adjacent lines. A per-call-site choice would be
// seventeen chances to send the wrong credential, and the failure — the shared
// token presented to payments — is a 401 on the money path, discovered in
// production. One rule, applied once, cannot drift.
//
// Prefix match against the CONFIGURED payments base URL, which is the same
// trimmed string every payments call is built from (s.paymentsURL + "/internal/…"),
// so the match and the request cannot disagree.
func (s *Server) internalTokenFor(url string) string {
	if s.paymentsToken != "" && s.paymentsURL != "" && strings.HasPrefix(url, s.paymentsURL) {
		return s.paymentsToken
	}
	return s.token
}

// WithPaymentsToken supplies the payments-only credential (ai-review S8). An
// option rather than a New parameter for the same reason WithStaffWriteCredential
// is one: New already takes six positional strings.
func (s *Server) WithPaymentsToken(token string) *Server {
	s.paymentsToken = token
	return s
}

// lim returns the rate limiters, building the real ones on first use if the
// Server was constructed as a literal rather than through New. Never returns nil —
// see the field comment for why "nil means allow" is not an option.
func (s *Server) lim() *customerLimiters {
	s.limOnce.Do(func() {
		if s.limiters == nil {
			s.limiters = newCustomerLimiters(nil)
		}
	})
	return s.limiters
}

// WithClock replaces the limiters' time source. Tests only — production passes
// nil to New and gets time.Now. It rebuilds rather than mutates, so a test always
// starts from empty buckets, and it consumes limOnce so a later lim() cannot
// discard the injected clock.
func (s *Server) WithClock(now func() time.Time) *Server {
	s.limiters = newCustomerLimiters(now)
	s.limOnce.Do(func() {})
	return s
}

// Refunds exposes the shared refund unit of work so the event-cancellation bulk runner
// (TKT-159) refunds through exactly the same protocol the staff endpoint does, rather than
// composing a second money path.
func (s *Server) Refunds() *refunds.Service { return s.refunds }

// WithAccess supplies the access base URL for refund ticket voiding. A separate setter
// rather than a seventh positional argument: every existing New caller keeps compiling,
// and a server without it degrades to leaving reversals outstanding instead of failing.
func (s *Server) WithAccess(access string) *Server {
	s.accessURL = strings.TrimSuffix(access, "/")
	// Rebuild the refund unit: it captured an empty access URL at New, and a coordinator
	// that cannot reach access would leave every ticket-voiding obligation outstanding.
	s.refunds = refunds.New(s.db, s.call, s.paymentsURL, s.accessURL, s.inventoryURL)
	return s
}

// registerRoutes is separate from Router so a test can WALK the real route
// inventory (TKT-194). The credential enumeration proving that the back
// office's token opens exactly one internal operation is only as good as its
// knowledge of which operations exist, and a hand-maintained list cannot detect
// a route added after it was written.
func (s *Server) registerRoutes(r chi.Router) {
	// The on-sale write path (ai-review S7). Grouped for the same reason the
	// customer surface below is: a limiter attached route-by-route is a list
	// someone has to remember to extend, and the next write added here would
	// silently be the unlimited one.
	r.Group(func(r chi.Router) {
		r.Use(s.limitCheckoutSource)
		r.Post("/reservations", s.reserve)
		r.Post("/orders", s.checkout)
	})
	r.Get("/orders/{id}", s.getOrder)
	// Public, credential-free, and deliberately NOT under /internal/ (TKT-220,
	// ADR-049): the storefront that renders these forms holds no service token,
	// and the gateway edge-denies /api/commerce/internal/ by construction.
	//
	// Grouped so the per-source limiter (TKT-224, ADR-051) wraps ALL of them at
	// once. A limiter attached route-by-route is a list someone has to remember to
	// extend, and the next public customer operation would silently be the
	// unlimited one — the hand-maintained-inventory defect this file already
	// carries a note about at registerRoutes. The per-SUBJECT half cannot live
	// here: it needs the decoded body, so it runs inside each handler.
	r.Group(func(r chi.Router) {
		r.Use(s.limitSource)
		r.Post("/customers", s.registerCustomer)
		r.Post("/customers/authenticate", s.authenticateCustomer)
		// Password recovery (TKT-226). Public for the same reason the two above are: the
		// caller is by definition someone who cannot sign in, so no credential exists to
		// present. STATIC segments under /customers — chi prefers static over a parameter,
		// so neither is read as a customer id, and a routing test asserts it.
		r.Post("/customers/password-reset", s.requestPasswordReset)
		r.Post("/customers/password-reset/complete", s.completePasswordReset)
		// TKT-223's claim. In scope for TKT-224 because the ONLY proof of ownership it
		// takes is an order reference (ADR-049 § TKT-223 amendment), so unbounded
		// attempts are a guessing surface. It moved into this group for that reason
		// alone; it is still a STATIC segment under the same prefix as
		// `GET /orders/{id}` above, chi still prefers static over a parameter, and the
		// routing test that asserts so — rather than assuming it — still covers it
		// from here.
		r.Post("/orders/claim", s.claimGuestOrder)
	})
	// The wallet (TKT-222). Public surface, identified by the assertion — the
	// storefront still holds no service credential.
	r.Get("/customers/{id}/orders", s.listCustomerOrders)
	// The partner surface (TKT-240). NOT under /internal/: a reseller is an
	// external caller and reaches these from the edge, which is exactly why the
	// guard is a declared `security:` the validator enforces rather than a header
	// compared in a handler (ADR-043).
	r.Get("/partners/availability", s.partnerAvailability)
	r.Post("/partners/reservations", s.partnerReserve)
	r.Post("/internal/orders/{id}/refunds", s.refundOrder)
	r.Post("/internal/slots/{id}/cancellation-refunds", s.createCancellationRefundRun)
	r.Get("/internal/cancellation-refunds/{id}", s.getCancellationRefundReport)
	r.Post("/internal/orders/{id}/unclaim", s.unclaimOrder)
	r.Post("/internal/orders/{id}/exchanges", s.exchangeOrder)
	r.Post("/internal/exchanges/{id}/tickets-switched", s.exchangeTicketsSwitched)
	r.Get("/internal/buyers/{id}/delivery-email", s.deliveryEmail)
	r.Post("/internal/operational-holds/{id}/convert", s.convertOperational)
	r.Post("/internal/group-reservations/{id}/draw-down", s.drawDownGroupReservation)
}

func (s *Server) Router(log *slog.Logger, validateResponses bool) http.Handler {
	r := chi.NewRouter()
	r.Get("/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Cache-Control", "public, max-age=300, s-maxage=300")
		_, _ = w.Write(apispec.Spec)
	})
	s.registerRoutes(r)
	// WithSecurity, because the partner surface's guard is DECLARED in the
	// contract and enforced here rather than compared in a handler (ADR-043,
	// TKT-240). The customer surface is unaffected: it declares no security, so
	// the authentication func is never consulted for it.
	validated, err := contract.RequestValidatorWithSecurity(apispec.Spec, r, log, validateResponses,
		func(w http.ResponseWriter, req *http.Request, msg string, status int) {
			// A refused partner credential gets the 401 the contract declares, with
			// a body that says nothing about which failure it was. Everything else
			// keeps the shared helper's representation.
			//
			// Matching on the PATH rather than on the status alone: 401 is also the
			// customer assertion's refusal on /orders, and that operation declares
			// its own meaning for it (a forged assertion is not a missing partner
			// credential). Only the partner surface is rewritten here.
			if status == http.StatusUnauthorized && strings.HasPrefix(req.URL.Path, "/partners/") {
				write(w, http.StatusUnauthorized, map[string]string{"error": "partner credential is not recognised"})
				return
			}
			// Restore 405 for a wrong method. Supplying ANY error handler switches
			// the shared helper from its legacy hook to ErrorHandlerWithOpts, and
			// that hook hard-codes 404 for EVERY route-lookup failure where the
			// legacy one distinguishes routers.ErrMethodNotAllowed as 405
			// (shared/go/contract/http.go documents the trap; this ticket walked
			// into it anyway). Without this, adding the partner surface silently
			// changed wrong-method responses on every pre-existing commerce
			// operation -- a platform-wide regression, found by ai-review and
			// confirmed by running both revisions: GET /reservations answered 405
			// on origin/main and 404 here.
			//
			// Matched on the MESSAGE rather than on routers.ErrMethodNotAllowed,
			// and that is a limitation of the shared helper rather than a choice:
			// contract.RequestValidatorWithSecurity flattens the error to
			// err.Error() before this callback sees it (shared/go/contract/http.go),
			// so errors.Is is not available here. The upstream cause is
			// nethttp-middleware's performRequestValidationForErrorHandlerWithOpts,
			// which hard-codes StatusNotFound whenever the route is nil and thereby
			// discards the ErrMethodNotAllowed the router did report.
			//
			// The string is kin-openapi's routers.ErrMethodNotAllowed.Error(), a
			// package-level sentinel rather than a formatted message, so it is as
			// stable as that dependency's public API. It is still a string compare,
			// and the honest mitigation is that TestWrongMethodStillAnswers405
			// drives the REAL router: a dependency bump that changes the text turns
			// that test red rather than silently restoring the 404 regression.
			//
			// The better fix is to widen the helper to pass the error through, so
			// every service can use errors.Is. That is a shared-package change and
			// does not belong in this ticket.
			if status == http.StatusNotFound && strings.Contains(msg, "method not allowed") {
				write(w, http.StatusMethodNotAllowed, map[string]string{"error": msg})
				return
			}
			// Everything else keeps the shape the shared helper's own default
			// produces, byte for byte -- {"error": <validator message>} with
			// Cache-Control: no-store.
			write(w, status, map[string]string{"error": msg})
		}, s.authenticatePartner)
	if err != nil {
		panic(err)
	}
	// The slot the authentication func fills. Installed OUTSIDE the validator
	// because the validator runs before any middleware the chi router carries.
	return withPartnerScopeSlot(validated)
}
func write(w http.ResponseWriter, c int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(c)
	_ = json.NewEncoder(w).Encode(v)
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if httpx.DecodeJSON(w, r, v, 1<<20) != nil {
		write(w, 400, map[string]string{"error": "invalid body"})
		return false
	}
	return true
}
func (s *Server) call(ctx context.Context, method, url, key string, in any, internal bool) (int, []byte, error) {
	var body io.Reader
	if in != nil {
		b, e := json.Marshal(in)
		if e != nil {
			return 0, nil, e
		}
		body = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, url, body)
	if e != nil {
		return 0, nil, e
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if internal {
		req.Header.Set("X-Internal-Token", s.internalTokenFor(url))
	}
	resp, e := s.client.Do(req)
	if e != nil {
		return 0, nil, e
	}
	defer func() { _ = resp.Body.Close() }()
	out, e := io.ReadAll(resp.Body)
	return resp.StatusCode, out, e
}

type price struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}
type offer struct {
	ID            uuid.UUID `json:"id"`
	OrganizerID   uuid.UUID `json:"organizer_id"`
	PerformanceID uuid.UUID `json:"performance_id"`
	Price         price     `json:"price"`
}
type reserveRequest struct {
	OrganizerID  uuid.UUID `json:"organizer_id"`
	TicketTypeID uuid.UUID `json:"ticket_type_id"`
	Quantity     int32     `json:"quantity"`
	// SeatIdentities makes this a SEATED reservation (TKT-173) and is mutually
	// exclusive with Quantity. Forwarded to inventory verbatim: canonicalisation
	// (sort + de-duplicate) is inventory's, because its idempotency fingerprint is
	// computed over the canonical form and a second canonicaliser here would be a
	// second definition of "the same request".
	SeatIdentities []string `json:"seat_identities,omitempty"`
	// ChannelCode selects which fee rules (TKT-215 / ADR-046 §4) and, since
	// TKT-237, which PRICE rules apply. A POINTER, not a string: nil is the
	// default/public context in which only channel-agnostic rules are eligible, and
	// that is NOT the same as a caller sending an empty channel. Omitting it is not
	// a wildcard.
	//
	// IT CANNOT BE SET BY A PUBLIC CALLER (TKT-248, ADR-060). `channel_code` is gone
	// from ReservationCreate, so a public body naming one is refused by the contract
	// and again by reserveWithScope below. The field survives on this struct because
	// the PARTNER path fills it -- from the credential, never from a body -- and both
	// paths share one reserve implementation.
	//
	// The history, because the shape of the fix matters more than the fix:
	//
	// TKT-240 tried to close the commerce->inventory seam by forwarding the channel
	// here, and the closure was REVERTED after its own adversarial review. Forwarding
	// is necessary but not sufficient while the route is UNAUTHENTICATED and takes
	// the channel from the body: it lets any caller name a reseller's channel and
	// consume its allocation with no credential -- executed and confirmed, not
	// theorised. TKT-246 then closed the inventory half by making the allocation say
	// who may sell it, judged under the pool row lock (ADR-055's requires_code
	// shape), and left the PRICING half open because forwarding a body-supplied
	// channel was still the same bypass.
	//
	// TKT-248 closed that half by removing the capability rather than checking it:
	// with no field, there is no body-supplied channel to price on. A channelled sale
	// now exists only behind a credential.
	//
	// Do not re-add this field to the public contract -- see channel_seam_test.go,
	// which pins the route as channel-free, and ADR-060.
	ChannelCode *string `json:"channel_code,omitempty"`
}

// seated reports whether this request is for named seats. The XOR itself is checked
// separately — this only answers which branch a valid request took.
func (in reserveRequest) seated() bool { return len(in.SeatIdentities) > 0 }

// canonicalSeatSet mirrors inventory's canonicalSeats — trim, de-duplicate, sort —
// for two local purposes, neither of which is rewriting the request. Commerce
// forwards the caller's array verbatim; inventory owns canonicalisation because its
// idempotency fingerprint is computed over its own canonical form, and a second
// authority for "the same request" is how two services quietly disagree.
//
// The two purposes:
//
//   - PRICING. Rules can be quantity-tiered (ADR-036), so resolving "3 seats" for a
//     request of [A,A,B] would quote a tier the buyer never reaches.
//   - IDEMPOTENT TERMS. Same key, different seats must be refused, and comparing
//     counts alone does not do it: [1,2] and [2,3] are both two seats, and the replay
//     would answer with the ORIGINAL claim's seats while the caller asked for
//     different ones. Caught end to end by TestSeatedReservationAndCheckout.
//
// Because this mirrors rather than delegates, the claimed set is verified against it
// after the call — so the two canonicalisations agreeing is an assertion, not an
// assumption.
func (in reserveRequest) canonicalSeatSet() []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in.SeatIdentities))
	for _, seat := range in.SeatIdentities {
		seat = strings.TrimSpace(seat)
		if _, dup := seen[seat]; dup {
			continue
		}
		seen[seat] = struct{}{}
		out = append(out, seat)
	}
	sort.Strings(out)
	return out
}

// units is how many tickets this request is FOR — the number priced against.
func (in reserveRequest) units() int32 {
	if !in.seated() {
		return in.Quantity
	}
	return int32(len(in.canonicalSeatSet()))
}

// uniqueSeats reports whether a forwarded identity list has no repeats. Weaker than
// subsetOf on purpose: an orphaned-seat list is NOT drawn from the request.
func uniqueSeats(seats []string) bool {
	seen := make(map[string]struct{}, len(seats))
	for _, s := range seats {
		if strings.TrimSpace(s) == "" || len(s) > 200 {
			return false
		}
		if _, dup := seen[s]; dup {
			return false
		}
		seen[s] = struct{}{}
	}
	return true
}

// disjointFrom reports that none of got appears in want. An orphaned seat is by
// definition one the buyer did NOT request.
func disjointFrom(got, want []string) bool {
	asked := make(map[string]struct{}, len(want))
	for _, s := range want {
		asked[s] = struct{}{}
	}
	for _, s := range got {
		if _, ok := asked[s]; ok {
			return false
		}
	}
	return true
}

// subsetOf reports whether every identity in got appears in want, with no duplicates.
// want is canonical (sorted, de-duplicated); got is whatever the far service sent.
func subsetOf(got, want []string) bool {
	allowed := make(map[string]struct{}, len(want))
	for _, seat := range want {
		allowed[seat] = struct{}{}
	}
	for _, seat := range got {
		if _, ok := allowed[seat]; !ok {
			return false
		}
		delete(allowed, seat) // a repeat is not a subset either
	}
	return true
}

// sameSeats reports whether a persisted (already canonical) set is the set this
// request names.
func sameSeats(persisted, requested []string) bool {
	if len(persisted) != len(requested) {
		return false
	}
	for i := range persisted {
		if persisted[i] != requested[i] {
			return false
		}
	}
	return true
}

// validReservationShape enforces the exactly-one-of rule in Go, independently of the
// contract's minProperties/maxProperties expression of it. Two enforcers on purpose:
// the schema is what a client reads, and this is what protects a handler invoked
// directly (every commerce handler test does exactly that). "Both" is the dangerous
// input — a handler that silently preferred one would charge for a quantity while
// claiming seats, or the reverse.
func validReservationShape(in reserveRequest) bool {
	if in.OrganizerID == uuid.Nil || in.TicketTypeID == uuid.Nil {
		return false
	}
	hasQty, hasSeats := in.Quantity != 0, len(in.SeatIdentities) > 0
	if hasQty == hasSeats {
		return false // both, or neither
	}
	if hasSeats {
		if len(in.SeatIdentities) > 50 {
			return false
		}
		for _, seat := range in.SeatIdentities {
			if strings.TrimSpace(seat) == "" || len(seat) > 200 {
				return false
			}
		}
		return true
	}
	return in.Quantity >= 1 && in.Quantity <= 50
}

// addSellerScope puts the AUTHENTICATED channel and reseller on an inventory hold
// body, and is the single place a channel may reach inventory (TKT-246).
//
// It takes a *partnerScope, not a channel string, and that signature is the guard.
// The channel inventory acts on can only come from a credential, because that is the
// only thing this function accepts -- there is no argument to pass a body field in.
//
// WHY THE PUBLIC ROUTE FORWARDS NOTHING. `POST /reservations` is unauthenticated and
// takes channel_code from the request body. TKT-240 forwarded that value and was
// reverted: any caller could then name a reseller's channel and consume its
// allocation with no credential (executed, not argued). Binding allocations to a
// seller does not rescue the public forward either, because every allocation that
// exists today is UNBOUND and an unbound allocation admits anyone -- so on the day
// this ships the bypass would be live for exactly the allocations it is meant to
// protect. The public route is therefore kept unable to reach the decision at all.
//
// The residual was: a public sale naming a reseller channel still PRICES under that
// channel's fee rules while consuming public stock -- a fee-attribution defect, not
// an inventory-theft one, since moving inventory already required a credential.
// (This comment said "TKT-247's", which was wrong: TKT-247 is a scanner-device
// flake. It was TKT-248's.)
//
// CLOSED by TKT-248 / ADR-060, and closed by removing the capability rather than
// checking it: `channel_code` no longer exists on ReservationCreate, so a public
// caller cannot name a channel at all. A channel now reaches pricing only from an
// authenticated partner credential -- which is also why this function's nil-scope
// early return is the whole public story.
func addSellerScope(body map[string]any, scope *partnerScope) {
	if scope == nil {
		return
	}
	body["channel"] = scope.ChannelCode
	body["reseller_id"] = scope.ResellerID
}

// holdEndpoint picks WHICH inventory hold route a request goes to, and returns whether
// it must be called as an internal service (TKT-246, ai-review pass 3).
//
// A hold naming a reseller goes to `/internal/holds`, with the service credential. The
// public `POST /holds` does not accept `reseller_id` at all — its contract refuses the
// field and its handler passes uuid.Nil regardless — because that route is proxied to
// the edge, and an authorization input taken from an unauthenticated body is the exact
// defect TKT-240 was reverted for.
//
// Returned as a pair rather than set at each call site: there are three GA hold paths
// (first attempt, persisted replay, exchange target), and the previous two rounds of
// this ticket were both defects of one path being updated and the others not.
func holdEndpoint(base string, body map[string]any) (string, bool) {
	if _, named := body["reseller_id"]; named {
		return base + "/internal/holds", true
	}
	return base + "/holds", false
}

// reservationID derives the reservation a given idempotency key names.
//
// organizer + key for a public sale, and organizer + RESELLER + key for a partner one
// (TKT-246). Without the reseller segment, two partners of the same organizer that
// happened to choose the same key derived the SAME id, and the second received the
// first's reservation -- another reseller's buyer, seats and money, attributed to
// them. Keys are caller-chosen and frequently sequential ("1", "order-1"), so that is
// a collision waiting rather than an attack.
//
// The public basis is unchanged BYTE FOR BYTE, and must stay so: every reservation
// that exists was written under it, so a caller mid-retry would otherwise derive a new
// id, miss its persisted row and place a SECOND hold. The reseller segment is only
// ever added for a scope that did not exist before this ticket, so no existing row
// changes identity.
//
// A function rather than an expression at the call site so the test asserts THIS
// derivation instead of re-implementing it — a test that recomputes the rule it is
// checking passes whatever the code does (AGENTS.md).
func reservationID(org uuid.UUID, key string, scope *partnerScope) uuid.UUID {
	basis := "reservation:" + org.String() + ":" + key
	if scope != nil {
		basis = "reservation:" + org.String() + ":" + scope.ResellerID.String() + ":" + key
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(basis))
}

// reserve is the PUBLIC, unauthenticated reserve. It carries no seller identity,
// and therefore forwards no channel to inventory -- see reserveWithScope.
func (s *Server) reserve(w http.ResponseWriter, r *http.Request) {
	s.reserveWithScope(w, r, nil)
}

// reserveWithScope is the one reserve implementation. `scope` is nil for the public
// route and the authenticated reseller for the partner route (TKT-246).
//
// ONE implementation, deliberately. The alternative -- a second partner-shaped
// reserve beside this one -- is how the replay path and the exchange target came to
// omit the channel in the first place: TKT-240's post-mortem named "every layer
// reasoned about the path being changed" as the root cause. A fork here would mean
// every future fix to pricing, fees, idempotency or seat handling has two homes and
// one of them will be missed.
//
// The scope is the ONLY source of channel and reseller for a partner sale. Nothing
// downstream reads in.ChannelCode for the inventory hold; see sellerIdentity.
func (s *Server) reserveWithScope(w http.ResponseWriter, r *http.Request, scope *partnerScope) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		write(w, 400, map[string]string{"error": "Idempotency-Key required"})
		return
	}
	var in reserveRequest
	if !decode(w, r, &in) {
		return
	}
	if scope != nil {
		// The credential is scoped to ONE organizer (ADR-056). organizer_id stays in
		// the body because the shared path needs it, so it is COMPARED here rather
		// than trusted -- the same reasoning as the channel, one tenant up. Refusing
		// rather than overwriting: silently rewriting the caller's organizer would
		// answer a question they did not ask.
		if in.OrganizerID != scope.OrganizerID {
			write(w, 403, map[string]string{"error": "credential is not scoped to this organizer"})
			return
		}
		// A partner's channel comes from its credential and NOTHING else. Overwriting
		// rather than rejecting a body value because the contract already refuses the
		// field (additionalProperties: false) -- this is the second line of defence
		// for the day that contract is edited, and it must not depend on the first.
		channel := scope.ChannelCode
		in.ChannelCode = &channel
	} else if in.ChannelCode != nil {
		// A PUBLIC caller may not name a channel at all (TKT-248, ADR-060).
		//
		// The real guard is that the field no longer exists on ReservationCreate,
		// so with `additionalProperties: false` the contract refuses it at the edge
		// and it is UNSUBMITTABLE rather than validated. This is the second line of
		// defence, for the day that contract is edited -- the same reasoning as the
		// partner overwrite above, and it must not depend on the first.
		//
		// Why it is refused rather than silently cleared: this route is
		// unauthenticated, and catalog resolves BOTH fee rules (ADR-046 §4) and,
		// since TKT-237, PRICE rules on the channel. So a body-supplied channel is a
		// caller choosing their own price basis, and an `absorbed` fee rule makes
		// that a smaller charge with the organizer eating the difference -- a
		// revenue leak, not a reporting artifact. Clearing it silently would price
		// them correctly while telling them nothing; a partner integrator who lost
		// their credential would then be quietly under-billed instead of told.
		write(w, 400, map[string]string{"error": "channel_code is not accepted on a public reservation"})
		return
	}
	if !validReservationShape(in) {
		write(w, 400, map[string]string{"error": "invalid reservation"})
		return
	}
	// The quote is pinned to the hold, and the pin has to be structural rather
	// than hopeful. The reservation id is derived from the idempotency key, so a
	// replayed reserve lands on the same row -- but re-resolving first would send
	// inventory a DIFFERENT unit_amount after a rule change, and inventory
	// fingerprints its idempotency on that amount. The replay would be rejected
	// with a 409 before commerce ever reached its ON CONFLICT DO NOTHING.
	//
	// So: look for the reservation BEFORE pricing anything. If it exists, answer
	// from what was persisted and never call catalog. That is what makes "the
	// price you were quoted is the price you are charged" true across a retry.
	id := reservationID(in.OrganizerID, key, scope)
	if s.db != nil {
		var pin struct {
			hold, buyer, slot, ticket uuid.UUID
			qty                       int32
			unit, total, face         int64
			currency                  string
			seats                     []byte // jsonb: NULL for a GA reservation
			feeSnapshot               []byte // jsonb: NULL for a fee-free reservation
			channel                   *string
		}
		err := s.db.QueryRowContext(r.Context(),
			`SELECT hold_id,buyer_id,slot_id,ticket_type_id,quantity,unit_amount,total_amount,face_value_amount,currency,seat_identities,fee_resolution_snapshot,channel_code
			 FROM reservations WHERE id=$1 AND organizer_id=$2`, id, in.OrganizerID).
			Scan(&pin.hold, &pin.buyer, &pin.slot, &pin.ticket, &pin.qty, &pin.unit, &pin.total, &pin.face, &pin.currency, &pin.seats, &pin.feeSnapshot, &pin.channel)
		switch {
		case err == nil:
			var pinnedSeats []string
			if pin.seats != nil {
				if json.Unmarshal(pin.seats, &pinnedSeats) != nil {
					write(w, 500, map[string]string{"error": "persist reservation"})
					return
				}
			}
			// Same key, different terms: the caller reused an idempotency key for
			// something else. Refuse rather than answer with the old quote. A
			// GA↔seated switch under one key is the same reuse and is caught here —
			// the counts would often match, so the KIND has to be compared too.
			// The SET, not just the count. [1,2] and [2,3] are both two seats, and
			// comparing counts would replay the original claim's seats back to a
			// caller who asked for different ones — a 201 that looks like success and
			// hands over seats nobody requested. The kind is compared too: a GA↔seated
			// switch under one key is the same reuse and the counts would often match.
			// The CHANNEL is a term too (ai-review): it selects which fee rules
			// apply, so the same key in two channels is two different sales, and
			// answering the second with the first's quote hands back a total
			// computed under fees that do not apply to it.
			if !sameTerms(in, pin.qty, pin.ticket, pin.channel, pinnedSeats) {
				write(w, 409, map[string]string{"error": "idempotency key reused with different terms"})
				return
			}
			// Replay inventory with the PERSISTED amount, not a re-resolved one.
			// That is the whole point: inventory fingerprints its idempotency on
			// unit_amount, so replaying with today's price after a rule change
			// would be rejected as a conflicting request. Replaying with the
			// pinned amount returns the same hold, and its expiry is what the
			// caller actually needs on a retry.
			seatedReplayURL := s.inventoryURL + "/holds/seats"
			replayBody := map[string]any{
				"organizer_id": in.OrganizerID, "slot_id": pin.slot, "ticket_type_id": pin.ticket,
				"quantity": pin.qty, "unit_amount": pin.unit, "currency": pin.currency}
			// The replay carries the same seller scope as the first attempt (TKT-246).
			//
			// Inventory fingerprints idempotency over the request, so a replay that
			// omits the channel the first attempt sent is a DIFFERENT request: it is
			// refused with a 409 rather than replayed, and a partner retrying a
			// timeout gets a hard failure on a hold it already owns. One of the two
			// sibling paths TKT-240 missed, and missed for the reason its post-mortem
			// names -- every layer reasoned about the path it was changing.
			//
			// From the SCOPE, not from the persisted row, and the two agree by
			// construction: the id derivation includes the reseller, so a scope that
			// found this row is the scope that wrote it.
			addSellerScope(replayBody, scope)
			if len(pinnedSeats) > 0 {
				// Replay with the PERSISTED seat set, not the incoming one. Inventory
				// fingerprints seat-hold idempotency over the canonical set, and the
				// persisted array IS that canonical set — replaying it is what makes a
				// retry land on the original claim instead of being refused as a
				// conflicting request. This is the only reader of the column, which is
				// why a write-only text[] would have looked fine until it mattered.
				replayBody = map[string]any{"organizer_id": in.OrganizerID, "slot_id": pin.slot,
					"ticket_type_id": pin.ticket, "seat_identities": pinnedSeats,
					"unit_amount": pin.unit, "currency": pin.currency}
			}
			replayURL, replayInternal := holdEndpoint(s.inventoryURL, replayBody)
			if len(pinnedSeats) > 0 {
				replayURL, replayInternal = seatedReplayURL, false
			}
			code, body, err := s.call(r.Context(), http.MethodPost, replayURL, key, replayBody, replayInternal)
			if err != nil || (code != 200 && code != 201) {
				if len(pinnedSeats) > 0 {
					seatedInventoryRefusal(w, code, body, pinnedSeats)
					return
				}
				write(w, 409, map[string]string{"error": "inventory unavailable"})
				return
			}
			var replayed struct {
				ID         uuid.UUID `json:"hold_id"`
				ExpiresAt  time.Time `json:"expires_at"`
				ServerTime time.Time `json:"server_time"`
			}
			if json.Unmarshal(body, &replayed) != nil {
				write(w, 502, map[string]string{"error": "invalid inventory response"})
				return
			}
			// A different hold under the same key means the two sides disagree
			// about what this reservation is. Fail rather than answer.
			if replayed.ID != pin.hold {
				write(w, 409, map[string]string{"error": "hold no longer matches the reservation"})
				return
			}
			// 201, not 200: createReservation declares exactly one success
			// status, and a replay was already indistinguishable from a first
			// call before this change (ON CONFLICT DO NOTHING then returned 201
			// too). Answering 200 would be an undeclared response, which the
			// fail-closed validator turns into a 500 (ADR-028) — widening the
			// contract to legalise it would be changing an API to fit an
			// implementation detail.
			out := map[string]any{"reservation_id": id, "hold_id": pin.hold, "buyer_id": pin.buyer,
				"amount": pin.total, "currency": pin.currency,
				"expires_at": replayed.ExpiresAt, "server_time": replayed.ServerTime}
			if len(pinnedSeats) > 0 {
				out["seats"] = pinnedSeats
			}
			// The breakdown comes from the PERSISTED snapshot, never from a
			// re-resolution. Same reasoning as the amount itself: a rule change
			// between the original reserve and the retry must not alter what the
			// caller is told they are buying. A reservation written before this
			// ticket has no snapshot, and then the fee fields are simply absent —
			// which is why they are optional in the contract.
			if err := addStoredFeeFields(out, pin.face, pin.feeSnapshot); err != nil {
				write(w, 500, map[string]string{"error": "persist reservation"})
				return
			}
			write(w, 201, out)
			return
		case !errors.Is(err, sql.ErrNoRows):
			// 500, not 503: createReservation does not declare 503, and an
			// undeclared status is an outage under the response validator.
			write(w, 500, map[string]string{"error": "persist reservation"})
			return
		}
	}

	// The price comes from catalog's RULE RESOLUTION, not from the ticket type's
	// raw column (TKT-153 / ADR-036 §6). One read: it carries the organizer, the
	// slot, the money and the provenance together.
	//
	// Every failure here aborts BEFORE inventory. "No rule matched" is not a
	// failure — it is a successful resolution answering with the base price, and
	// that distinction is the whole point of the fail-closed rule (ADR-028): a
	// silent fall back to the base price would sell at the wrong price and look
	// like nothing happened.
	resolution, err := s.resolveTicketTypePrice(r.Context(), in.TicketTypeID, in.OrganizerID, in.units(), in.ChannelCode)
	if err != nil {
		if errors.Is(err, errResolveUnavailable) {
			write(w, 502, map[string]string{"error": "catalog unavailable"})
			return
		}
		write(w, 500, map[string]string{"error": "price resolution unusable"})
		return
	}
	// Fees, on the same terms and for the same reasons (TKT-215 / ADR-046). This
	// read happens BEFORE the hold so that a document we cannot store or trust
	// aborts while there is nothing to orphan -- the lesson TKT-153 encoded for
	// the price snapshot. "No rule matched" is a successful resolution with an
	// empty fee set; it is not a failure and must never be conflated with one.
	fees, err := s.resolveTicketTypeFees(r.Context(), in.TicketTypeID, in.OrganizerID,
		resolution.PerformanceID, in.ChannelCode)
	if err != nil {
		if errors.Is(err, errResolveUnavailable) {
			write(w, 502, map[string]string{"error": "catalog unavailable"})
			return
		}
		write(w, 500, map[string]string{"error": "fee resolution unusable"})
		return
	}
	if fees.Currency != resolution.ResolvedPrice.Currency {
		// A fee in a different currency from the thing it is a fee on cannot be
		// added to it. Catalog validates this against the ticket type; commerce
		// checks it against the price it is actually charging.
		write(w, 500, map[string]string{"error": "fee resolution unusable"})
		return
	}
	o := offer{OrganizerID: resolution.OrganizerID, PerformanceID: resolution.PerformanceID,
		Price: price{Amount: resolution.ResolvedPrice.Amount, Currency: resolution.ResolvedPrice.Currency}}
	// Prove the ARITHMETIC fits before the hold, not just the document
	// (TKT-215 ai-review, [high]). A perfectly valid rule — catalog bounds a fee
	// amount at the same Money cap a price uses — can still overflow when
	// multiplied by quantity, and doing that check after the hold answered 400
	// with a hold left behind: an orphan, and a buyer told their order was
	// malformed by their own request.
	//
	// The REQUESTED quantity is the right basis to check, and that is not an
	// approximation: every fee basis is non-decreasing in quantity, and inventory
	// can only ever REDUCE the count (seated canonicalisation de-duplicates, it
	// never invents a seat). So a composition that fits at the requested quantity
	// fits at the canonical one, and this check is a sound upper bound rather
	// than a guess.
	if err := checkCompositionFits(fees.Fees, o.Price, resolution.total(in.units()), in.units()); err != nil {
		write(w, 400, map[string]string{"error": "order total out of range"})
		return
	}
	holdBody := map[string]any{"organizer_id": in.OrganizerID,
		"slot_id": o.PerformanceID, "ticket_type_id": in.TicketTypeID, "quantity": in.Quantity,
		"unit_amount": o.Price.Amount, "currency": o.Price.Currency}
	addSellerScope(holdBody, scope)
	seatedHoldURL := s.inventoryURL + "/holds/seats"
	if in.seated() {
		holdBody = map[string]any{"organizer_id": in.OrganizerID, "slot_id": o.PerformanceID,
			"ticket_type_id": in.TicketTypeID, "seat_identities": in.SeatIdentities,
			"unit_amount": o.Price.Amount, "currency": o.Price.Currency}
	}
	// A reseller-bearing hold goes to the INTERNAL route, with the service credential.
	holdURL, holdInternal := holdEndpoint(s.inventoryURL, holdBody)
	if in.seated() {
		holdURL, holdInternal = seatedHoldURL, false
	}
	code, body, err := s.call(r.Context(), http.MethodPost, holdURL, key, holdBody, holdInternal)
	if err != nil || (code != 200 && code != 201) {
		if in.seated() {
			seatedInventoryRefusal(w, code, body, in.canonicalSeatSet())
			return
		}
		// A GA quantity claim against a SEATED pool is refused by inventory under
		// the pool row lock (store.go: kind == "seated" -> ErrPoolKindMismatch),
		// and that refusal is worth keeping distinguishable here (TKT-240).
		// Flattening it into "inventory unavailable" tells a caller to retry, and
		// this is the one 409 that will never succeed however long they wait: the
		// pool sells seat by seat and a quantity claim can never fit it.
		//
		// It matters most on the partner surface, where the caller is a machine
		// with a retry loop, and where the honest answer also has to say that a
		// seated claim carries no channel at all -- so this refusal cites TKT-176
		// rather than implying the partner could hold seats some other way.
		if bytes.Contains(body, []byte("claim kind does not match pool kind")) {
			write(w, 409, map[string]string{
				"error": "this slot is seated and cannot be sold by quantity. A seated claim carries " +
					"no channel and does not consume a channel allocation -- TKT-176 owns that seam.",
				"code": "seated_pool_unsupported",
			})
			return
		}
		write(w, 409, map[string]string{"error": "inventory unavailable"})
		return
	}
	var hold struct {
		ID         uuid.UUID `json:"hold_id"`
		ExpiresAt  time.Time `json:"expires_at"`
		ServerTime time.Time `json:"server_time"`
		Seats      []string  `json:"seats"`
		Quantity   int32     `json:"quantity"`
	}
	if json.Unmarshal(body, &hold) != nil {
		write(w, 502, map[string]string{"error": "invalid inventory response"})
		return
	}
	// The CLAIM is authoritative for what was reserved, not the request. Inventory
	// canonicalises (sorts, de-duplicates), so a request naming the same seat twice
	// claims one seat — and the money must follow the claim or the buyer is charged
	// for a seat nobody holds.
	quantity := in.Quantity
	if in.seated() {
		// Fail closed on the whole invariant, not just the count (ai-review). A
		// response that is schema-valid but inconsistent — different seats of the same
		// count, or a quantity that disagrees with its own seat list — would otherwise
		// be persisted, billed from, and later refunded against. That needs an
		// inventory defect or version skew to happen, which is precisely when a
		// cross-service check earns its keep: the failure mode is a wrong-seat sale.
		//
		// The set must be EQUAL, not merely the same size, and it is compared against
		// the locally canonicalised request — the same function the idempotent-terms
		// check uses, so "what we asked for" has one definition here.
		if len(hold.Seats) == 0 ||
			hold.Quantity != int32(len(hold.Seats)) ||
			!sameSeats(hold.Seats, in.canonicalSeatSet()) {
			write(w, 502, map[string]string{"error": "invalid inventory response"})
			return
		}
		quantity = int32(len(hold.Seats))
	}
	// The face value is the rule-resolved price times the CLAIMED quantity, and
	// the fees are composed on that same canonical number -- inventory
	// canonicalises a seated claim, so a request naming one seat twice claims one
	// seat and must be charged one fee.
	faceValue := resolution.total(quantity)
	composition, err := computeFeeBreakdown(fees.Fees, o.Price.Amount, quantity, o.Price.Currency)
	if err != nil {
		// Unreachable by the monotonicity argument above: this same computation
		// already succeeded at a quantity >= this one, before the hold. Kept
		// because "unreachable" is a claim about today's bases, and a future
		// basis that is not monotonic in quantity would make it false — at which
		// point failing here beats charging a wrong total.
		write(w, 500, map[string]string{"error": "fee composition failed after the hold"})
		return
	}
	total, err := composedTotal(faceValue, composition.PassedOnTotal)
	if err != nil {
		write(w, 500, map[string]string{"error": "fee composition failed after the hold"})
		return
	}
	feeSnapshot, err := feeSnapshotEnvelope(fees, composition, faceValue, total)
	if err != nil {
		write(w, 500, map[string]string{"error": "persist reservation"})
		return
	}
	buyer := uuid.NewSHA1(uuid.NameSpaceOID, []byte("buyer:"+id.String()))
	// The provenance snapshot is stored as a document, not as a rule reference: a
	// rule can later be closed or superseded, and a foreign key would let that
	// rewrite what a buyer was charged. Copying the document keeps the record
	// true. Honest-writer consistency, not tamper-evidence (ADR-021) — anyone
	// with commerce DB access can still replace it.
	// jsonb, not text[]: the pgx/v5 stdlib driver behind database/sql writes a text[]
	// and cannot scan one back, and the only reader is the replay path — so the
	// failure would surface exactly where it hurts. NULL for a GA reservation.
	var seatsColumn any
	if in.seated() {
		encoded, marshalErr := json.Marshal(hold.Seats)
		if marshalErr != nil {
			write(w, 500, map[string]string{"error": "persist reservation"})
			return
		}
		seatsColumn = encoded
	}
	// Per-reseller attribution (TKT-240). NULL for every non-partner sale, which is
	// what a customer sale is. It comes from the authenticated credential in the
	// request context -- never from the body -- so a customer cannot claim to be a
	// reseller by sending a field.
	var resellerColumn any
	if scope, ok := partnerScopeFrom(r.Context()); ok {
		resellerColumn = scope.ResellerID
	}
	res, err := s.db.ExecContext(r.Context(), `INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status,price_resolution_snapshot,seat_identities,fee_resolution_snapshot,channel_code,reseller_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'held',$12,$13,$14,$15,$16) ON CONFLICT(id) DO NOTHING`,
		id, in.OrganizerID, hold.ID, o.PerformanceID, in.TicketTypeID, buyer, quantity, o.Price.Amount, total, faceValue, o.Price.Currency, []byte(resolution.raw), seatsColumn, feeSnapshot, in.ChannelCode, resellerColumn)
	if err != nil {
		write(w, 500, map[string]string{"error": "persist reservation"})
		return
	}
	// ON CONFLICT DO NOTHING can LOSE, and the loser must not answer with the
	// total it computed locally (ai-review, [high]). Two concurrent reserves
	// under one key both miss the lookup above; if catalog's fees changed between
	// their resolutions they compute different totals, inventory returns the same
	// hold to both, and one INSERT wins. The loser would otherwise quote a total
	// the database does not hold — and checkout charges what the database holds.
	//
	// Re-read rather than fail: the winner's row IS the answer, and a retry-shaped
	// error for a reservation that exists would be worse than telling the caller
	// what they actually bought.
	if affected, affErr := res.RowsAffected(); affErr == nil && affected == 0 {
		// The INSERT lost a race. Answer from what is PERSISTED — but only after
		// checking the winner's request was the SAME request (ai-review pass 2).
		//
		// The first version of this branch re-read the totals and returned 201
		// unconditionally, which is worse than the bug it fixed: two concurrent
		// requests under one key with DIFFERENT channels both miss the lookup
		// above (the channel never reaches inventory, so both get the same hold),
		// and the loser was handed the winner's reservation as if its own terms
		// had been accepted. A later retry of that same request then reaches the
		// replay path, compares the channel, and answers 409 — so one request
		// succeeded once and conflicted afterwards.
		var stored struct {
			total, face int64
			qty         int32
			ticket      uuid.UUID
			channel     *string
			snapshot    []byte
			seats       []byte
		}
		if err = s.db.QueryRowContext(r.Context(),
			`SELECT total_amount,face_value_amount,quantity,ticket_type_id,channel_code,
			        fee_resolution_snapshot,seat_identities
			 FROM reservations WHERE id=$1 AND organizer_id=$2`, id, in.OrganizerID).
			Scan(&stored.total, &stored.face, &stored.qty, &stored.ticket, &stored.channel,
				&stored.snapshot, &stored.seats); err != nil {
			write(w, 500, map[string]string{"error": "persist reservation"})
			return
		}
		var storedSeats []string
		if stored.seats != nil && json.Unmarshal(stored.seats, &storedSeats) != nil {
			write(w, 500, map[string]string{"error": "persist reservation"})
			return
		}
		// The SAME comparison the replay path uses — one definition of "the same
		// request", so the two answers cannot disagree about it.
		if !sameTerms(in, stored.qty, stored.ticket, stored.channel, storedSeats) {
			write(w, 409, map[string]string{"error": "idempotency key reused with different terms"})
			return
		}
		out := map[string]any{"reservation_id": id, "hold_id": hold.ID, "buyer_id": buyer,
			"amount": stored.total, "currency": o.Price.Currency,
			"expires_at": hold.ExpiresAt, "server_time": hold.ServerTime}
		if len(storedSeats) > 0 {
			out["seats"] = storedSeats
		}
		if err = addStoredFeeFields(out, stored.face, stored.snapshot); err != nil {
			write(w, 500, map[string]string{"error": "persist reservation"})
			return
		}
		write(w, 201, out)
		return
	}
	out := map[string]any{"reservation_id": id, "hold_id": hold.ID, "buyer_id": buyer, "amount": total, "currency": o.Price.Currency, "expires_at": hold.ExpiresAt, "server_time": hold.ServerTime}
	if in.seated() {
		out["seats"] = hold.Seats
	}
	addFeeFields(out, faceValue, composition)
	write(w, 201, out)
}

// seatedInventoryRefusal translates a refused seat claim. A bare 409 tells a picker
// that something went wrong and not which seats to re-render, so a `seat_taken`
// carrying identities is forwarded with them intact.
//
// It never SYNTHESISES identities. Echoing the request would name seats that were
// never contended — plausible-looking and false, and precisely the lie AC4 exists to
// prevent. An inventory `seat_taken` with no usable list is a broken upstream: 502.
func seatedInventoryRefusal(w http.ResponseWriter, code int, body []byte, requested []string) {
	var refusal struct {
		Error string   `json:"error"`
		Code  string   `json:"code"`
		Seats []string `json:"seat_identities"`
	}
	if code == 409 && json.Unmarshal(body, &refusal) == nil && refusal.Code == "orphaned_seats" {
		// The identities here are seats the buyer did NOT ask for — the ones the
		// selection would strand. The subset rule below is therefore exactly wrong for
		// them: applying it would turn every valid orphan refusal into a 502 (ADR-041).
		// They still must be non-empty and unique; commerce never invents identities.
		// Non-empty, unique, contract-shaped, bounded, and DISJOINT from the request.
		// The last one is the real check: a requested seat cannot be a free unrequested
		// orphan, so accepting one would have the picker propose an impossible repair —
		// "add the seat you already asked for" (ai-review). The subset rule that guards
		// seat_taken is exactly inverted here, which is why both exist.
		if len(refusal.Seats) == 0 || len(refusal.Seats) > 200 || !uniqueSeats(refusal.Seats) ||
			!disjointFrom(refusal.Seats, requested) {
			write(w, 502, map[string]string{"error": "invalid inventory response"})
			return
		}
		write(w, 409, map[string]any{
			"error": "the selection would leave a seat with no neighbour",
			"code":  "orphaned_seats", "seat_identities": refusal.Seats,
		})
		return
	}
	if code == 409 && json.Unmarshal(body, &refusal) == nil && refusal.Code == "seat_taken" {
		// Non-empty, and a SUBSET of what this buyer asked for. Forwarding verbatim
		// would let an inventory defect or a version skew name seats this request
		// never mentioned, which the response schema cannot catch — it is a semantic
		// mismatch, not a shape one — and a picker would then grey out somebody
		// else's seats. Commerce's contract promises only requested, actually
		// contended identities; this is what makes that true rather than hopeful.
		if len(refusal.Seats) == 0 || !subsetOf(refusal.Seats, requested) {
			write(w, 502, map[string]string{"error": "invalid inventory response"})
			return
		}
		write(w, 409, map[string]any{
			"error": "one or more of the requested seats are no longer available",
			"code":  "seat_taken", "seat_identities": refusal.Seats,
		})
		return
	}
	// Everything else keeps the GA path's existing collapsed shape. Widening that is
	// a public error-contract change with its own consumers and is not this ticket's.
	write(w, 409, map[string]string{"error": "inventory unavailable"})
}

type checkoutRequest struct {
	ReservationID uuid.UUID `json:"reservation_id"`
	Name          string    `json:"name"`
	Email         string    `json:"email"`
	PaymentToken  string    `json:"payment_token"`
}

func paymentFailureResponse(body []byte, fallbackStatus string) map[string]any {
	out := map[string]any{"status": fallbackStatus}
	var decoded map[string]any
	if json.Unmarshal(body, &decoded) == nil && decoded != nil {
		if status, ok := decoded["status"].(string); ok {
			out["status"] = status
		}
		if replay, ok := decoded["replay"].(bool); ok {
			out["replay"] = replay
		}
	}
	return out
}

type reservation struct {
	ID, OrganizerID, HoldID, BuyerID, SlotID, TicketTypeID uuid.UUID
	Quantity                                               int32
	// Amount is the GROSS charge; FaceValue is the face alone. TKT-215 split
	// them because the exchange delta needs one and the PSP needs the other.
	Amount, FaceValue int64
	Currency, Status  string
	// FeeSnapshot is the verbatim fee resolution persisted at reserve time.
	// NULL on a pre-TKT-215 or staff-created reservation.
	FeeSnapshot []byte
}

func (s *Server) load(ctx context.Context, id uuid.UUID) (reservation, error) {
	var x reservation
	err := s.db.QueryRowContext(ctx, `SELECT id,organizer_id,hold_id,buyer_id,slot_id,ticket_type_id,quantity,total_amount,face_value_amount,currency,status,fee_resolution_snapshot FROM reservations WHERE id=$1`, id).Scan(&x.ID, &x.OrganizerID, &x.HoldID, &x.BuyerID, &x.SlotID, &x.TicketTypeID, &x.Quantity, &x.Amount, &x.FaceValue, &x.Currency, &x.Status, &x.FeeSnapshot)
	return x, err
}

// publishOwed sends an order's committed envelope inline, for promptness only. It is
// best-effort by design: the outbox already owes the event, so every failure path here
// is recoverable by the drainer and none of them may fail the buyer's request.
//
// It publishes the FROZEN bytes rather than rebuilding the envelope. Rebuilding would
// put a different payload on the wire than the drainer's retry would send, under the
// same deterministic id — defeating the point of freezing it at commit.
func (s *Server) publishOwed(ctx context.Context, order uuid.UUID) {
	if s.publisher == nil {
		return // the drainer owns delivery
	}
	eventID, subject, envelope, ok, err := commercestore.FrozenEnvelope(ctx, s.db, order)
	if err != nil || !ok {
		if err != nil {
			slog.Default().WarnContext(ctx, "read owed completion envelope; left to the outbox drainer",
				"order_id", order, "err", err)
		}
		return // already published, or unreadable: either way the drainer reconciles
	}
	if err := s.publisher.PublishRaw(ctx, subject, eventID, envelope); err != nil {
		slog.Default().WarnContext(ctx, "inline publish of owed completion event failed; left to the outbox drainer",
			"order_id", order, "err", err)
		return
	}
	// Retire the row so the drainer does not republish what just went out. Only
	// retires an unleased row: if a drainer holds it, that drainer owns the outcome.
	if err := commercestore.MarkPublishedByOrder(ctx, s.db, order); err != nil {
		slog.Default().WarnContext(ctx, "retire owed completion event after inline publish",
			"order_id", order, "err", err)
	}
}

func (s *Server) guestReference(ctx context.Context, order uuid.UUID) (uuid.UUID, error) {
	var ref uuid.UUID
	err := s.db.QueryRowContext(ctx, `SELECT guest_order_ref FROM orders WHERE id=$1 AND status='completed'`, order).Scan(&ref)
	return ref, err
}
func (s *Server) fact(ctx context.Context, x reservation, order uuid.UUID, typ string) error {
	fid := uuid.NewSHA1(uuid.NameSpaceOID, []byte(order.String()+":"+typ))
	_, e := s.db.ExecContext(ctx, `INSERT INTO order_facts(fact_id,order_id,organizer_id,buyer_id,fact_type,amount,currency) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, fid, order, x.OrganizerID, x.BuyerID, typ, x.Amount, x.Currency)
	if e != nil {
		return e
	}
	var occurred time.Time
	if e := s.db.QueryRowContext(ctx, `SELECT occurred_at FROM order_facts WHERE fact_id=$1`, fid).Scan(&occurred); e != nil {
		return e
	}
	code, _, e := s.call(ctx, http.MethodPost, s.paymentsURL+"/internal/facts", "", map[string]any{"fact_id": fid, "organizer_id": x.OrganizerID, "fact_type": typ, "buyer_id": x.BuyerID, "amount": x.Amount, "currency": x.Currency, "occurred_at": occurred, "payload": map[string]string{"order_id": order.String()}}, true)
	if e != nil || code != 200 {
		return errors.New("journal unavailable")
	}
	return nil
}

// claimOrder returns the order id, its current status, and whether recovery has PARKED it.
// Parked is read here rather than re-queried because this is the only place that already
// holds the row lock, and a replay branch that answers from durable evidence needs to know
// whether any worker can still act on that evidence (ai-review F2).
// claimOrder takes `customer` — the verified attribution, or an invalid NullUUID
// for a guest — and writes it on the INSERT that creates the order row.
//
// Written HERE and nowhere else, deliberately. The attribution has to survive the
// request that established it: an order can be completed minutes later by the
// recovery runner (ADR-016), long after the assertion expired and the storefront
// session is gone. Carrying it forward from the handler would mean recovery has no
// idea who bought, and there is no second source to ask.
//
// A replay finds the existing row and does NOT update it. That is what makes
// attribution immutable under idempotency: a second request bearing a different
// (or absent) assertion cannot repoint a completed purchase at someone else, and
// cannot promote a guest order into an attributed one. The first claim decides.
func (s *Server) claimOrder(ctx context.Context, x reservation, key, fingerprint string, customer uuid.NullUUID) (uuid.UUID, string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return uuid.Nil, "", false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, x.ID.String()); err != nil {
		return uuid.Nil, "", false, err
	}
	var id uuid.UUID
	var storedKey, storedFingerprint, status string
	var recoveryClaim uuid.NullUUID
	var recoveryLease, recoveryParked sql.NullTime
	// FOR UPDATE, and the recovery columns are read here rather than ignored: the
	// recovery runner decides from a payments lookup that is only durable evidence while
	// no one else can charge this order. Reading the row without locking it would let
	// this replay bind a charge in the window between recovery's lookup and its release.
	err = tx.QueryRowContext(ctx, `SELECT id,idempotency_key,request_fingerprint,status,recovery_claim_id,recovery_lease_until,recovery_parked_at FROM orders WHERE reservation_id=$1 FOR UPDATE`, x.ID).
		Scan(&id, &storedKey, &storedFingerprint, &status, &recoveryClaim, &recoveryLease, &recoveryParked)
	if errors.Is(err, sql.ErrNoRows) {
		id = uuid.NewSHA1(uuid.NameSpaceOID, []byte("order:"+x.OrganizerID.String()+":"+key))
		// Channel and reseller attribution are COPIED FROM THE RESERVATION rather
		// than from the request (TKT-240). The reservation is where the credential's
		// scope was applied, so this survives a replay and a recovery identically --
		// and there is no request field a caller could set to attribute a sale to a
		// reseller that did not make it. Settlement (ADR-048) splits by these.
		_, err = tx.ExecContext(ctx, `INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,customer_id,channel_code,reseller_id) SELECT $1,$2,'created',$3,$4,$5,r.channel_code,r.reseller_id FROM reservations r WHERE r.id=$2`, id, x.ID, key, fingerprint, customer)
		status = "created"
	} else if err == nil {
		if storedKey != key || storedFingerprint != fingerprint {
			return uuid.Nil, "", false, errCheckoutConflict
		}
		// A recovery pass holds this order under an unexpired lease. It may already have
		// asked payments whether a charge exists and been told no; binding one now would
		// turn that true answer into a false one, and the seat would be released out from
		// under a captured payment. Recovery's decision is bounded by its lease, so the
		// buyer can retry once it lapses.
		if recoveryClaim.Valid && recoveryLease.Valid && recoveryLease.Time.After(time.Now()) {
			return uuid.Nil, "", false, errRecoveryInProgress
		}
		// Reopening an existing order is fresh activity on it. Without this the row keeps
		// the timestamp of the checkout that died, stays past recovery's grace period,
		// and recovery can claim it while this request is live — the grace period only
		// protects orders whose updated_at actually moves.
		//
		// Scoped to the statuses that actually RESUME orchestration (TKT-116). A retry
		// landing on release_pending or reconciliation_required returns from a replay
		// branch below without touching anything downstream, so there is no in-flight work
		// to protect — while refreshing updated_at would push the order back inside
		// recovery's 2-minute grace window on every retry, leaving the release that IS
		// outstanding permanently unclaimable and the buyer looping on the same answer.
		if _, err = tx.ExecContext(ctx, `UPDATE orders SET updated_at=now() WHERE id=$1 AND status IN ('created','payment_unknown','confirmation_pending')`, id); err != nil {
			return uuid.Nil, "", false, err
		}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return uuid.Nil, "", false, errCheckoutConflict
	}
	if err != nil {
		return uuid.Nil, "", false, err
	}
	return id, status, recoveryParked.Valid, tx.Commit()
}

func checkoutClaimProblem(err error) (int, string) {
	if errors.Is(err, errCheckoutConflict) {
		return http.StatusConflict, "checkout conflicts with an existing request"
	}
	if errors.Is(err, errRecoveryInProgress) {
		// 409 like the conflict above, but the distinct message matters: this one clears
		// on its own once the recovery lease lapses.
		return http.StatusConflict, "this order is being recovered; retry shortly"
	}
	return http.StatusInternalServerError, "persist checkout"
}

func persistenceReadProblem(err error) (int, string) {
	if errors.Is(err, sql.ErrNoRows) {
		return http.StatusNotFound, "not found"
	}
	return http.StatusServiceUnavailable, "temporarily unavailable"
}

func paymentOutcomeProblem(code int) (int, string, bool) {
	if code == http.StatusConflict {
		return http.StatusConflict, "payment operation in progress; retry with the same idempotency key", true
	}
	return 0, "", false
}

// markUnknown reports whether the guarded order write landed. A zero-row update means
// the order moved on underneath a slow checkout — since TKT-115 the recovery runner
// actively transitions rows to reconciliation_required/refunded, so a swallowed miss
// would answer an optimistic 202 for money that is being reconciled (ai-review A1).
func (s *Server) markUnknown(ctx context.Context, reservationID, orderID uuid.UUID) bool {
	_, _ = s.db.ExecContext(ctx, `UPDATE reservations SET status='unknown' WHERE id=$1 AND status IN ('held','finalizing','unknown')`, reservationID)
	res, err := s.db.ExecContext(ctx, `UPDATE orders SET status='payment_unknown',updated_at=now() WHERE id=$1 AND status IN ('created','payment_unknown','confirmation_pending')`, orderID)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

// answerRecovered re-reads an order a guarded status write missed and answers the
// recovery truth when there is one; false means the caller's optimistic answer stands
// (the miss was a plain write failure, and 202-with-a-hint remains the honest default).
func (s *Server) answerRecovered(ctx context.Context, w http.ResponseWriter, x reservation, order uuid.UUID) bool {
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM orders WHERE id=$1`, order).Scan(&status); err != nil {
		return false
	}
	switch classifyRecovered(status) {
	case recoveredCompleted:
		// Recovery resolved the payment as captured and completed the order — the tickets
		// exist. Answering the optimistic 202 here would tell a buyer their payment is
		// unknown while their tickets are issued (ai-review pass 2, F3).
		ref, e := s.guestReference(ctx, order)
		if e != nil {
			write(w, 503, map[string]string{"error": "ticket issuance pending; retry checkout"})
			return true
		}
		s.publishOwed(ctx, order)
		write(w, 200, map[string]any{"order_id": order, "guest_order_ref": ref, "status": "completed"})
		return true
	case recoveredTerminal:
		// Report the terminal truth, not payment_unknown — but journal it first. Not every
		// terminal status arrives with its fact already written: markTerminalFailure commits
		// the status BEFORE s.fact and answers 503 when the journal is down, precisely so the
		// buyer retries into the replay branch that re-asserts the fact before giving a
		// buyer-final answer. Answering 402/408 here without that replay would end the
		// retry loop on a race and strand the order.failed gap forever (ai-review pass 3,
		// P3-1). The fact is idempotent, so replaying a recovery-journalled one is free.
		if err := s.fact(ctx, x, order, "order.failed"); err != nil {
			write(w, 503, map[string]string{"error": "journal unavailable"})
			return true
		}
		write(w, terminalCheckoutCode(status), map[string]any{"order_id": order, "status": status, "replay": true})
		return true
	case recoveredPending:
		// The outcome IS decided here — RecordTerminalOutcome writes terminal_outcome and
		// status='release_pending' in one statement, and the runner treats a
		// release_pending row without an outcome as a bug. Only the inventory release is
		// outstanding. Echo that state, as the checkout's own release_pending path does;
		// calling it payment_unknown would deny durable evidence (ai-review pass 3, P3-2).
		write(w, 202, map[string]any{"order_id": order, "status": status})
		return true
	case recoveredReconciling:
		write(w, 409, map[string]any{"error": "order awaiting payment reconciliation", "order_id": order, "status": status})
		return true
	}
	return false
}

// The answer a checkout owes a buyer when the recovery runner won the race for its order.
type recoveredClass int

const (
	// The optimistic 202 payment_unknown stands.
	recoveredOptimistic recoveredClass = iota
	recoveredCompleted
	recoveredTerminal
	recoveredPending
	recoveredReconciling
)

// classifyRecovered maps an order status a guarded checkout write lost the race to onto the
// answer the buyer is owed. Every status in the orders CHECK vocabulary must land here
// deliberately: the first cut answered only refunded/reconciliation_required and silently
// dropped the rest into the optimistic 202, telling buyers their payment was unknown while
// recovery had already completed or terminally failed the order (ai-review pass 2, F3).
func classifyRecovered(status string) recoveredClass {
	switch status {
	case "completed":
		return recoveredCompleted
	case "declined", "timeout", "refunded":
		return recoveredTerminal
	case "release_pending":
		return recoveredPending
	case "reconciliation_required":
		return recoveredReconciling
	}
	// Only `created`, `payment_unknown` and `confirmation_pending` are left, and those are
	// what the guarded write itself targets — reaching them means it lost no race and the
	// payment genuinely IS unresolved, so the optimistic 202 is honest.
	return recoveredOptimistic
}

func (s *Server) markTerminalFailure(ctx context.Context, reservationID, orderID uuid.UUID, status string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE reservations SET status='failed' WHERE id=$1 AND status IN ('held','finalizing','unknown')`, reservationID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE orders SET status=$2,updated_at=now() WHERE id=$1 AND status IN ('created','payment_unknown','confirmation_pending')`, orderID, status); err != nil {
		return err
	}
	return tx.Commit()
}

func terminalCheckoutCode(status string) int {
	if status == "timeout" {
		return http.StatusRequestTimeout
	}
	return http.StatusPaymentRequired
}

func (s *Server) checkout(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		write(w, 400, map[string]string{"error": "Idempotency-Key required"})
		return
	}
	var in checkoutRequest
	if !decode(w, r, &in) {
		return
	}
	if in.ReservationID == uuid.Nil || strings.TrimSpace(in.Name) == "" || !strings.Contains(in.Email, "@") || !fakepsp.ValidToken(in.PaymentToken) {
		write(w, 400, map[string]string{"error": "invalid checkout"})
		return
	}
	x, err := s.load(r.Context(), in.ReservationID)
	if err != nil {
		code, message := persistenceReadProblem(err)
		if code != http.StatusNotFound {
			slog.Default().ErrorContext(r.Context(), "load checkout reservation", "err", err)
			write(w, code, map[string]string{"error": message})
			return
		}
		write(w, code, map[string]string{"error": "reservation " + message})
		return
	}
	// The attribution, resolved BEFORE any order, payment or inventory work: a
	// forged assertion must cost nothing and leave nothing behind.
	//
	// Deliberately NOT part of the fingerprint below. The fingerprint decides
	// whether a replay is the same request, and an assertion carries an expiry —
	// so including it would turn a legitimate retry after a re-sign-in into a 409
	// conflict, which is not what the buyer changed.
	customer, err := customerFromRequest(s.assertionKey, r.Header.Get(assertionHeader), time.Now())
	if err != nil {
		// 401, and no order. NOT a silent downgrade to a guest checkout: that
		// would hide a failed attribution from the buyer, who would then not find
		// the purchase in their account with no error to point at. The message
		// says nothing about which part failed (assertion.go).
		write(w, http.StatusUnauthorized, map[string]string{"error": "invalid customer assertion"})
		return
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%s\n%s", in.ReservationID, strings.TrimSpace(in.Name), strings.ToLower(strings.TrimSpace(in.Email)), in.PaymentToken))))
	order, orderStatus, recoveryParked, err := s.claimOrder(r.Context(), x, key, fingerprint, customer)
	if err != nil {
		code, message := checkoutClaimProblem(err)
		if code == http.StatusInternalServerError {
			slog.Default().ErrorContext(r.Context(), "claim checkout order", "err", err)
		}
		write(w, code, map[string]string{"error": message})
		return
	}
	if orderStatus == "completed" {
		ref, e := s.guestReference(r.Context(), order)
		if e != nil {
			write(w, 503, map[string]string{"error": "ticket issuance pending; retry checkout"})
			return
		}
		// The original completion owed this event; if it is still unpublished, send the
		// frozen bytes. If it is already published (or backfilled and drained), this is
		// a no-op — a replay must not mint a second, differently-timestamped copy.
		s.publishOwed(r.Context(), order)
		write(w, 200, map[string]any{"order_id": order, "guest_order_ref": ref, "status": "completed"})
		return
	}
	if orderStatus == "declined" || orderStatus == "timeout" {
		// A previous attempt may have persisted the terminal local outcome while
		// payments was unavailable. Replay the idempotent journal fact before
		// returning the buyer-visible terminal result.
		if err := s.fact(r.Context(), x, order, "order.failed"); err != nil {
			write(w, 503, map[string]string{"error": "journal unavailable"})
			return
		}
		write(w, terminalCheckoutCode(orderStatus), map[string]any{"order_id": order, "status": orderStatus, "replay": true})
		return
	}
	if orderStatus == "refunded" {
		// Recovery refunded this order's captured money (TKT-115). Without this branch a
		// byte-identical replay would fall through, re-journal order.created and finalize
		// against a hold recovery already released. The fact replay mirrors the declined/
		// timeout branches' defense-in-depth (ai-review A3): recovery journalled
		// order.failed before MarkRefunded, so this collapses idempotently — but the
		// symmetry means a future path that sets `refunded` without the fact gets caught
		// here instead of leaving a journal gap.
		if err := s.fact(r.Context(), x, order, "order.failed"); err != nil {
			write(w, 503, map[string]string{"error": "journal unavailable"})
			return
		}
		write(w, terminalCheckoutCode(orderStatus), map[string]any{"order_id": order, "status": orderStatus, "replay": true})
		return
	}
	if orderStatus == "release_pending" {
		// A terminal outcome is already decided and durable: RecordTerminalOutcome writes
		// terminal_outcome and this status in ONE statement, and the runner treats a
		// release_pending row without an outcome as a bug. Only the inventory release is
		// outstanding. Falling through would re-journal order.created and finalize a claim
		// recovery is concurrently releasing — or, once inventory had released it, hand the
		// buyer a misleading 409 "hold expired" (TKT-116). claimOrder narrows this but
		// cannot close it: errRecoveryInProgress fires only while the lease is LIVE.
		//
		// 202 echoing the durable status, not 402/408 from terminal_outcome: from here the
		// release can still find a CONFIRMED claim and park the order for reconciliation
		// (recovery.releaseAndFail), so the buyer-visible outcome is not final yet. It is
		// also exactly what answerRecovered already returns for this state, so the guarded
		// -write loser and this replay agree without either changing. terminal_outcome does
		// prove no money was captured, and 202-with-status says that without over-claiming.
		//
		// Unless recovery has PARKED the row (ai-review F2). 202 promises that something
		// will advance this order, and once ReleaseStuckOrder has exhausted its attempts
		// that promise is false: it sets recovery_parked_at and deliberately leaves the
		// status alone, and ClaimStuckOrders excludes parked rows, so no worker will ever
		// pick it up again. Parked means a human must act — the same thing
		// reconciliation_required tells buyers, so it gets the same answer.
		if recoveryParked {
			write(w, 409, map[string]any{"error": "order awaiting payment reconciliation", "order_id": order, "status": orderStatus})
			return
		}
		write(w, 202, map[string]any{"order_id": order, "status": orderStatus})
		return
	}
	if orderStatus == "reconciliation_required" {
		// Captured money mid-compensation (or awaiting a human). Neither completed nor
		// terminally failed — falling through would re-drive a checkout whose money is
		// being reconciled. The distinct message is the buyer-facing state.
		write(w, 409, map[string]any{"error": "order awaiting payment reconciliation", "order_id": order, "status": orderStatus})
		return
	}
	if _, err = s.db.ExecContext(r.Context(), `INSERT INTO buyer_pii(buyer_id,name,email) VALUES($1,$2,$3) ON CONFLICT(buyer_id) DO UPDATE SET name=EXCLUDED.name,email=EXCLUDED.email`, x.BuyerID, in.Name, in.Email); err != nil {
		write(w, 500, map[string]string{"error": "persist buyer"})
		return
	}
	if err := s.fact(r.Context(), x, order, "order.created"); err != nil {
		write(w, 503, map[string]string{"error": "journal unavailable"})
		return
	}
	code, _, err := s.call(r.Context(), http.MethodPost, fmt.Sprintf("%s/internal/holds/%s/finalize?organizer_id=%s", s.inventoryURL, x.HoldID, x.OrganizerID), "", nil, true)
	if err != nil || code != 200 {
		write(w, 409, map[string]string{"error": "hold expired"})
		return
	}
	if _, err = s.db.ExecContext(r.Context(), `UPDATE reservations SET status='finalizing' WHERE id=$1 AND status IN ('held','finalizing')`, x.ID); err != nil {
		write(w, 500, map[string]string{"error": "persist checkout"})
		return
	}
	// The settlement plan comes from the PERSISTED snapshot, never a fresh
	// catalog read (TKT-217 / ADR-048). A schedule edited between the sale and
	// the capture must not change who gets paid for that sale — the same reason
	// the snapshot exists at all.
	plan, err := settlementPlanFromSnapshot(x)
	if err != nil {
		write(w, 500, map[string]string{"error": "settlement plan unavailable"})
		return
	}
	charge := map[string]any{"order_id": order, "organizer_id": x.OrganizerID, "buyer_id": x.BuyerID, "amount": x.Amount, "currency": x.Currency, "payment_token": in.PaymentToken, "settlement": plan}
	code, body, err := s.call(r.Context(), http.MethodPost, s.paymentsURL+"/internal/charges", key, charge, true)
	if err != nil {
		if !s.markUnknown(r.Context(), x.ID, order) && s.answerRecovered(r.Context(), w, x, order) {
			return
		}
		write(w, 202, map[string]any{"order_id": order, "status": "payment_unknown"})
		return
	}
	if problemCode, message, active := paymentOutcomeProblem(code); active {
		write(w, problemCode, map[string]any{"order_id": order, "status": "payment_in_progress", "error": message})
		return
	}
	if code == 402 || code == 408 {
		releaseCode, _, releaseErr := s.call(r.Context(), http.MethodPost, fmt.Sprintf("%s/internal/holds/%s/release?organizer_id=%s", s.inventoryURL, x.HoldID, x.OrganizerID), "", nil, true)
		if releaseErr != nil || releaseCode != 200 {
			write(w, 202, map[string]any{"order_id": order, "status": "release_pending"})
			return
		}
		terminalStatus := "declined"
		if code == http.StatusRequestTimeout {
			terminalStatus = "timeout"
		}
		if err := s.markTerminalFailure(r.Context(), x.ID, order, terminalStatus); err != nil {
			write(w, 500, map[string]string{"error": "persist failure"})
			return
		}
		if err := s.fact(r.Context(), x, order, "order.failed"); err != nil {
			write(w, 503, map[string]string{"error": "journal unavailable"})
			return
		}
		out := paymentFailureResponse(body, terminalStatus)
		out["order_id"] = order
		write(w, code, out)
		return
	}
	if code != 200 {
		if !s.markUnknown(r.Context(), x.ID, order) && s.answerRecovered(r.Context(), w, x, order) {
			return
		}
		write(w, 202, map[string]any{"order_id": order, "status": "payment_unknown"})
		return
	}
	code, _, err = s.call(r.Context(), http.MethodPost, fmt.Sprintf("%s/internal/holds/%s/confirm?organizer_id=%s", s.inventoryURL, x.HoldID, x.OrganizerID), "", nil, true)
	if err != nil || code != 200 {
		res, execErr := s.db.ExecContext(r.Context(), `UPDATE orders SET status='confirmation_pending',updated_at=now() WHERE id=$1 AND status IN ('created','payment_unknown','confirmation_pending')`, order)
		if execErr == nil {
			if n, _ := res.RowsAffected(); n != 1 && s.answerRecovered(r.Context(), w, x, order) {
				return
			}
		}
		write(w, 202, map[string]any{"order_id": order, "status": "confirmation_pending"})
		return
	}
	if err := s.fact(r.Context(), x, order, "order.completed"); err != nil {
		write(w, 503, map[string]string{"error": "journal unavailable"})
		return
	}
	guestRef, err := uuid.NewRandom()
	if err != nil {
		write(w, 500, map[string]string{"error": "mint guest order reference"})
		return
	}
	guestRef, err = commercestore.CompleteOrder(r.Context(), s.db, commercestore.Completion{
		ReservationID: x.ID, OrderID: order, OrganizerID: x.OrganizerID, BuyerID: x.BuyerID,
		SlotID: x.SlotID, TicketTypeID: x.TicketTypeID, Quantity: x.Quantity,
	}, guestRef)
	if err != nil {
		slog.Default().ErrorContext(r.Context(), "persist order completion", "err", err)
		write(w, 500, map[string]string{"error": "persist completion"})
		return
	}
	// The completion commit owes the event, so issuance no longer depends on this
	// publish succeeding: a failure here (or a crash) leaves a claimable outbox row
	// the drainer picks up. Publish inline anyway to keep the happy path prompt — the
	// buyer's ticket should not wait a drain interval — but never fail the checkout
	// on it. The old 503 told a buyer whose money was taken and whose order was
	// committed to "retry checkout", which was both alarming and unnecessary.
	s.publishOwed(r.Context(), order)
	write(w, 200, map[string]any{"order_id": order, "guest_order_ref": guestRef, "status": "completed"})
}

// convertOperational is the staff entry point for selling out of an operational hold
// (TKT-77 / ADR-023): resolve the offer through the existing catalog seam (no price
// override), have inventory carve a buyer hold out of the operational hold atomically,
// then persist the same reservation shape the public reserve path produces so the
// existing checkout completes it. Deterministic reservation identity + the forwarded
// idempotency key make a crash between the inventory commit and the reservation insert
// repairable by replaying the same request.
func (s *Server) convertOperational(w http.ResponseWriter, r *http.Request) {
	s.staffSale(w, r, "/internal/operational-holds/", "/convert", "reservation:op-convert:")
}

// drawDownGroupReservation is the staff entry point for selling out of a group/agency
// reservation (TKT-79 / ADR-027) — the same orchestration as convertOperational against
// inventory's draw-down operation, with its own reservation identity namespace.
func (s *Server) drawDownGroupReservation(w http.ResponseWriter, r *http.Request) {
	s.staffSale(w, r, "/internal/group-reservations/", "/draw-down", "reservation:group-draw-down:")
}

func (s *Server) staffSale(w http.ResponseWriter, r *http.Request, invPrefix, invAction, ns string) {
	// Fail closed on an unconfigured token; a bad token reads as 404 like deliveryEmail.
	if !httpx.HeaderCredentialMatches(r, httpx.InternalToken, s.token) {
		write(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		write(w, 400, map[string]string{"error": "Idempotency-Key required"})
		return
	}
	sourceID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid hold id"})
		return
	}
	var in struct {
		OrganizerID  uuid.UUID `json:"organizer_id"`
		TicketTypeID uuid.UUID `json:"ticket_type_id"`
		Quantity     int32     `json:"quantity"`
		Actor        string    `json:"actor"`
		Reason       string    `json:"reason"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.OrganizerID == uuid.Nil || in.TicketTypeID == uuid.Nil || in.Quantity < 1 || in.Quantity > 50 {
		write(w, 400, map[string]string{"error": "invalid conversion request"})
		return
	}
	code, body, err := s.call(r.Context(), http.MethodGet, s.catalogURL+"/internal/ticket-types/"+in.TicketTypeID.String(), "", nil, true)
	if err != nil || code != 200 {
		write(w, 409, map[string]string{"error": "unknown ticket type"})
		return
	}
	var o offer
	if json.Unmarshal(body, &o) != nil || o.OrganizerID != in.OrganizerID || o.Price.Currency != "EUR" || o.Price.Amount < 0 || o.Price.Amount > math.MaxInt64/int64(in.Quantity) {
		write(w, 409, map[string]string{"error": "offer not sellable in EUR"})
		return
	}
	// slot_id makes ticket-type/slot agreement a precondition inside inventory's locked
	// transaction: a mismatch rejects with the operational hold untouched, instead of
	// discovering it after the carve has committed.
	convBody := map[string]any{"organizer_id": in.OrganizerID, "slot_id": o.PerformanceID, "quantity": in.Quantity, "ticket_type_id": in.TicketTypeID, "unit_amount": o.Price.Amount, "currency": o.Price.Currency, "actor": in.Actor, "reason": in.Reason}
	code, body, err = s.call(r.Context(), http.MethodPost, s.inventoryURL+invPrefix+sourceID.String()+invAction, key, convBody, true)
	if err != nil {
		write(w, 409, map[string]string{"error": "inventory unavailable"})
		return
	}
	if code == 404 || code == 409 {
		write(w, code, map[string]string{"error": "conversion rejected"})
		return
	}
	if code != 200 && code != 201 {
		write(w, 502, map[string]string{"error": "invalid inventory response"})
		return
	}
	var conv struct {
		Hold struct {
			ID         uuid.UUID `json:"hold_id"`
			SlotID     uuid.UUID `json:"slot_id"`
			Status     string    `json:"status"`
			ExpiresAt  time.Time `json:"expires_at"`
			ServerTime time.Time `json:"server_time"`
		} `json:"hold"`
		SourceRemaining int32 `json:"source_remaining"`
	}
	if json.Unmarshal(body, &conv) != nil || conv.Hold.ID == uuid.Nil {
		write(w, 502, map[string]string{"error": "invalid inventory response"})
		return
	}
	// A replay must be judged by the child's lifecycle status, not its timestamp: a
	// confirmed claim keeps its elapsed expires_at forever (a 409 here would instruct
	// staff to carve the same seats twice), finalizing is live regardless of deadline,
	// and a terminal expired/released child must not become a reservation no checkout
	// can complete — that capacity is already back in the public pool (ADR-023).
	if code == 200 {
		switch conv.Hold.Status {
		case "confirmed", "finalizing":
			// live or already sold — the replay is legitimate; persist/return below
		case "held":
			if !conv.Hold.ExpiresAt.After(conv.Hold.ServerTime) {
				write(w, 409, map[string]string{"error": "converted hold expired; place a new conversion"})
				return
			}
		case "expired", "released":
			// Terminal: that capacity is public again; a reservation here could never
			// check out, and re-carving is an explicit new staff operation.
			write(w, 409, map[string]string{"error": "converted hold is no longer usable; place a new conversion"})
			return
		default:
			// Empty or future status (version skew): unknown is not terminal — advising
			// a re-conversion here could double-carve a still-live child (ADR-017's
			// rule: never judge a future variant with today's semantics).
			write(w, 502, map[string]string{"error": "invalid inventory response"})
			return
		}
	}
	total := o.Price.Amount * int64(in.Quantity)
	// Namespaced so a staff key can never collide with a public reserve key — and the
	// two staff families can never collide with each other.
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(ns+in.OrganizerID.String()+":"+key))
	buyer := uuid.NewSHA1(uuid.NameSpaceOID, []byte("buyer:"+id.String()))
	// face_value_amount = total: the staff paths are deliberately fee-free (they
	// keep writing a NULL fee snapshot for the same reason they write a NULL
	// price snapshot), so the two numbers are equal by construction here.
	_, err = s.db.ExecContext(r.Context(), `INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,$10,'held') ON CONFLICT(id) DO NOTHING`,
		id, in.OrganizerID, conv.Hold.ID, o.PerformanceID, in.TicketTypeID, buyer, in.Quantity, o.Price.Amount, total, o.Price.Currency)
	if err != nil {
		write(w, 500, map[string]string{"error": "persist reservation"})
		return
	}
	status := 201
	if code == 200 {
		status = 200
	}
	write(w, status, map[string]any{"reservation_id": id, "hold_id": conv.Hold.ID, "buyer_id": buyer, "amount": total, "currency": o.Price.Currency, "expires_at": conv.Hold.ExpiresAt, "server_time": conv.Hold.ServerTime, "source_remaining": conv.SourceRemaining})
}

func (s *Server) deliveryEmail(w http.ResponseWriter, r *http.Request) {
	if !httpx.HeaderCredentialMatches(r, httpx.InternalToken, s.token) {
		write(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid buyer"})
		return
	}
	var email string
	if err := s.db.QueryRowContext(r.Context(), `SELECT email FROM buyer_pii WHERE buyer_id=$1`, id).Scan(&email); err != nil {
		code, message := persistenceReadProblem(err)
		write(w, code, map[string]string{"error": message})
		return
	}
	write(w, 200, map[string]string{"email": email})
}
func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	id, e := uuid.Parse(chi.URLParam(r, "id"))
	if e != nil {
		write(w, 400, map[string]string{"error": "invalid order id"})
		return
	}
	var status string
	// customer_id is deliberately NOT selected, not merely omitted from the
	// response: this read is public and answers for any order id, and order ids
	// are derived from the organizer plus the checkout idempotency key, so the
	// field told anyone who could recompute an id whether that order has an
	// account behind it (ai-review S3). Nothing consumed it. The endpoint's
	// remaining disclosure — status, for a guessed id — is TKT-222's, which
	// makes the whole read authenticated and ownership-scoped.
	e = s.db.QueryRowContext(r.Context(), `SELECT status FROM orders WHERE id=$1`, id).Scan(&status)
	if e != nil {
		code, message := persistenceReadProblem(e)
		if code != http.StatusNotFound {
			slog.Default().ErrorContext(r.Context(), "load order", "err", e)
		}
		write(w, code, map[string]string{"error": message})
		return
	}
	write(w, 200, map[string]any{"order_id": id, "status": status})
}
