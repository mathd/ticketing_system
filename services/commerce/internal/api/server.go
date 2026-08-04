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
	// accessURL drives the ticket-voiding half of a refund reversal (TKT-157). Empty
	// leaves the obligation outstanding rather than failing the refund — the money has
	// already moved by then.
	accessURL string
	publisher commerceevents.Publisher
	// refunds is the one unit of work for refunding an order, shared with the
	// event-cancellation bulk runner (TKT-159). Rebuilt by WithAccess because the access
	// URL arrives after New.
	refunds *refunds.Service
}

func New(db *sql.DB, client *http.Client, catalog, inventory, payments, token string, publishers ...commerceevents.Publisher) *Server {
	var publisher commerceevents.Publisher
	if len(publishers) > 0 {
		publisher = publishers[0]
	}
	s := &Server{db: db, client: client, catalogURL: strings.TrimSuffix(catalog, "/"), inventoryURL: strings.TrimSuffix(inventory, "/"), paymentsURL: strings.TrimSuffix(payments, "/"), token: token, publisher: publisher}
	s.refunds = refunds.New(db, s.call, s.paymentsURL, s.accessURL, s.inventoryURL)
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
func (s *Server) Router(log *slog.Logger, validateResponses bool) http.Handler {
	r := chi.NewRouter()
	r.Get("/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Cache-Control", "public, max-age=300, s-maxage=300")
		_, _ = w.Write(apispec.Spec)
	})
	r.Post("/reservations", s.reserve)
	r.Post("/orders", s.checkout)
	r.Get("/orders/{id}", s.getOrder)
	r.Post("/internal/orders/{id}/refunds", s.refundOrder)
	r.Post("/internal/slots/{id}/cancellation-refunds", s.createCancellationRefundRun)
	r.Get("/internal/cancellation-refunds/{id}", s.getCancellationRefundReport)
	r.Post("/internal/orders/{id}/exchanges", s.exchangeOrder)
	r.Post("/internal/exchanges/{id}/tickets-switched", s.exchangeTicketsSwitched)
	r.Get("/internal/buyers/{id}/delivery-email", s.deliveryEmail)
	r.Post("/internal/operational-holds/{id}/convert", s.convertOperational)
	r.Post("/internal/group-reservations/{id}/draw-down", s.drawDownGroupReservation)
	validated, err := contract.RequestValidator(apispec.Spec, r, log, validateResponses)
	if err != nil {
		panic(err)
	}
	return validated
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
		req.Header.Set("X-Internal-Token", s.token)
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

func (s *Server) reserve(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		write(w, 400, map[string]string{"error": "Idempotency-Key required"})
		return
	}
	var in reserveRequest
	if !decode(w, r, &in) {
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
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte("reservation:"+in.OrganizerID.String()+":"+key))
	if s.db != nil {
		var pin struct {
			hold, buyer, slot, ticket uuid.UUID
			qty                       int32
			unit, total               int64
			currency                  string
			seats                     []byte // jsonb: NULL for a GA reservation
		}
		err := s.db.QueryRowContext(r.Context(),
			`SELECT hold_id,buyer_id,slot_id,ticket_type_id,quantity,unit_amount,total_amount,currency,seat_identities
			 FROM reservations WHERE id=$1 AND organizer_id=$2`, id, in.OrganizerID).
			Scan(&pin.hold, &pin.buyer, &pin.slot, &pin.ticket, &pin.qty, &pin.unit, &pin.total, &pin.currency, &pin.seats)
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
			if pin.qty != in.units() || pin.ticket != in.TicketTypeID ||
				(len(pinnedSeats) > 0) != in.seated() ||
				(in.seated() && !sameSeats(pinnedSeats, in.canonicalSeatSet())) {
				write(w, 409, map[string]string{"error": "idempotency key reused with different terms"})
				return
			}
			// Replay inventory with the PERSISTED amount, not a re-resolved one.
			// That is the whole point: inventory fingerprints its idempotency on
			// unit_amount, so replaying with today's price after a rule change
			// would be rejected as a conflicting request. Replaying with the
			// pinned amount returns the same hold, and its expiry is what the
			// caller actually needs on a retry.
			replayURL, replayBody := s.inventoryURL+"/holds", map[string]any{
				"organizer_id": in.OrganizerID, "slot_id": pin.slot, "ticket_type_id": pin.ticket,
				"quantity": pin.qty, "unit_amount": pin.unit, "currency": pin.currency}
			if len(pinnedSeats) > 0 {
				// Replay with the PERSISTED seat set, not the incoming one. Inventory
				// fingerprints seat-hold idempotency over the canonical set, and the
				// persisted array IS that canonical set — replaying it is what makes a
				// retry land on the original claim instead of being refused as a
				// conflicting request. This is the only reader of the column, which is
				// why a write-only text[] would have looked fine until it mattered.
				replayURL = s.inventoryURL + "/holds/seats"
				replayBody = map[string]any{"organizer_id": in.OrganizerID, "slot_id": pin.slot,
					"ticket_type_id": pin.ticket, "seat_identities": pinnedSeats,
					"unit_amount": pin.unit, "currency": pin.currency}
			}
			code, body, err := s.call(r.Context(), http.MethodPost, replayURL, key, replayBody, false)
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
	resolution, err := s.resolveTicketTypePrice(r.Context(), in.TicketTypeID, in.OrganizerID, in.units())
	if err != nil {
		if errors.Is(err, errResolveUnavailable) {
			write(w, 502, map[string]string{"error": "catalog unavailable"})
			return
		}
		write(w, 500, map[string]string{"error": "price resolution unusable"})
		return
	}
	o := offer{OrganizerID: resolution.OrganizerID, PerformanceID: resolution.PerformanceID,
		Price: price{Amount: resolution.ResolvedPrice.Amount, Currency: resolution.ResolvedPrice.Currency}}
	holdURL, holdBody := s.inventoryURL+"/holds", map[string]any{"organizer_id": in.OrganizerID,
		"slot_id": o.PerformanceID, "ticket_type_id": in.TicketTypeID, "quantity": in.Quantity,
		"unit_amount": o.Price.Amount, "currency": o.Price.Currency}
	if in.seated() {
		holdURL = s.inventoryURL + "/holds/seats"
		holdBody = map[string]any{"organizer_id": in.OrganizerID, "slot_id": o.PerformanceID,
			"ticket_type_id": in.TicketTypeID, "seat_identities": in.SeatIdentities,
			"unit_amount": o.Price.Amount, "currency": o.Price.Currency}
	}
	code, body, err := s.call(r.Context(), http.MethodPost, holdURL, key, holdBody, false)
	if err != nil || (code != 200 && code != 201) {
		if in.seated() {
			seatedInventoryRefusal(w, code, body, in.canonicalSeatSet())
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
	total := resolution.total(quantity)
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
	_, err = s.db.ExecContext(r.Context(), `INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,currency,status,price_resolution_snapshot,seat_identities) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'held',$11,$12) ON CONFLICT(id) DO NOTHING`,
		id, in.OrganizerID, hold.ID, o.PerformanceID, in.TicketTypeID, buyer, quantity, o.Price.Amount, total, o.Price.Currency, []byte(resolution.raw), seatsColumn)
	if err != nil {
		write(w, 500, map[string]string{"error": "persist reservation"})
		return
	}
	out := map[string]any{"reservation_id": id, "hold_id": hold.ID, "buyer_id": buyer, "amount": total, "currency": o.Price.Currency, "expires_at": hold.ExpiresAt, "server_time": hold.ServerTime}
	if in.seated() {
		out["seats"] = hold.Seats
	}
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
	Amount                                                 int64
	Currency, Status                                       string
}

func (s *Server) load(ctx context.Context, id uuid.UUID) (reservation, error) {
	var x reservation
	err := s.db.QueryRowContext(ctx, `SELECT id,organizer_id,hold_id,buyer_id,slot_id,ticket_type_id,quantity,total_amount,currency,status FROM reservations WHERE id=$1`, id).Scan(&x.ID, &x.OrganizerID, &x.HoldID, &x.BuyerID, &x.SlotID, &x.TicketTypeID, &x.Quantity, &x.Amount, &x.Currency, &x.Status)
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
func (s *Server) claimOrder(ctx context.Context, x reservation, key, fingerprint string) (uuid.UUID, string, bool, error) {
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
		_, err = tx.ExecContext(ctx, `INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint) VALUES($1,$2,'created',$3,$4)`, id, x.ID, key, fingerprint)
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
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%s\n%s", in.ReservationID, strings.TrimSpace(in.Name), strings.ToLower(strings.TrimSpace(in.Email)), in.PaymentToken))))
	order, orderStatus, recoveryParked, err := s.claimOrder(r.Context(), x, key, fingerprint)
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
	charge := map[string]any{"order_id": order, "organizer_id": x.OrganizerID, "buyer_id": x.BuyerID, "amount": x.Amount, "currency": x.Currency, "payment_token": in.PaymentToken}
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
	if s.token == "" || r.Header.Get("X-Internal-Token") != s.token {
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
	_, err = s.db.ExecContext(r.Context(), `INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,currency,status) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'held') ON CONFLICT(id) DO NOTHING`,
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
	if r.Header.Get("X-Internal-Token") != s.token {
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
