//go:build smoke

package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"ticketing/services/commerce/internal/exchangeunwind"
	commercestore "ticketing/services/commerce/internal/store"
)

// Resuming an exchange interrupted after the money moved (TKT-167, ADR-039).
//
// THE TIER IS THE POINT. TKT-158 persists the settlement basis before calling payments and
// then does not use it on the one path that needs it: an unsettled replay re-prices through
// catalog and re-submits the target hold before it ever loads the basis. Neither half of
// that is visible anywhere else:
//
//   - the store tier has the row but no handler, so it cannot observe that a retry made a
//     catalog call at all — the defect is a SEQUENCE of external calls, not a row state;
//   - the existing api tier (exchange_target_test.go) has a handler but no database, so it
//     cannot construct "basis_at set, settled_at NULL", which is the entire precondition;
//   - the gateway suite in smoke/ drives the real stack, where catalog cannot be made to
//     fail on the second request only and provider calls cannot be counted.
//
// So this file is the first DB-backed test under commerce's internal/api. scripts/smoke.sh
// already runs `./internal/...` for exactly this eventuality — the comment there said
// "nothing lives there for commerce yet" — and now also creates this package its own
// database, which the same script's history says is not optional (see exchangeAPIDB).
//
// THE INTERRUPTION IS DRIVEN THROUGH THE HANDLER, never seeded. Writing basis_at into
// order_exchanges directly would produce a green test whose precondition the code under
// test never created — the handler's own basis write would go unexercised, and a fixture
// that constructs the state it is supposed to observe cannot fail for the right reason.
// Here the first request runs the real forward path and is interrupted by a payments stub
// that refuses the journal call, which is a real 503 branch in the handler.

// exchangeStack is a commerce server wired to real PostgreSQL and three counting stubs.
type exchangeStack struct {
	db      *sql.DB
	handler http.Handler
	token   string

	catalog   *countingStub
	inventory *countingStub
	payments  *countingStub
}

// countingStub is one dependency. Every stub counts its calls, because on the resume path
// the COUNT is the assertion: "resumes from the basis" means precisely "catalog was not
// asked again and no second hold was taken".
type countingStub struct {
	server *httptest.Server

	mu     sync.Mutex
	counts map[string]int
	// chargeToken is the payment token of the most recent charge submission (TKT-301).
	chargeToken string
	// onCharge runs INSIDE the charge handler, before it answers. It is how a test observes
	// durable state at the instant the provider is being called rather than afterwards —
	// "the marker was set by the end" is also true of a handler that marks too late.
	onCharge func()
}

func (c *countingStub) hit(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[name]++
	return c.counts[name]
}

// noteChargeToken records the payment token of the most recent charge, so a test can assert
// commerce FORWARDED the caller's instrument rather than substituting one (TKT-301).
func (c *countingStub) noteChargeToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chargeToken = token
}

func (c *countingStub) lastChargeToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chargeToken
}

// reset forgets every count so far (TKT-292). A test whose fixture needs the real handler
// to mint a row has to make one request BEFORE the one it is about; without a reset, its
// "this request called nobody" assertion would be permanently red for the setup's calls,
// and the tempting repair — asserting a delta instead of zero — is weaker: a delta is
// satisfied by a second call to an endpoint the setup already touched.
func (c *countingStub) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts = map[string]int{}
	c.chargeToken = ""
}

func (c *countingStub) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[name]
}

func newCountingStub(t *testing.T, h func(*countingStub, http.ResponseWriter, *http.Request)) *countingStub {
	t.Helper()
	c := &countingStub{counts: map[string]int{}}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h(c, w, r)
	}))
	t.Cleanup(c.server.Close)
	return c
}

// exchangeAPIDB opens this package's OWN smoke database and migrates it.
//
// Its own, not ./internal/store's, and the rule is the repo's rather than this ticket's:
// `go test ./internal/...` runs packages concurrently, so a second store.Migrate caller
// sharing one database races the first's migration. scripts/smoke.sh records that lesson
// three times over (TKT-226, TKT-198) and dsn_isolation_smoke_test.go now fails if a
// package is added without its own DSN.
//
// Sharing also cost a real, misleading failure before it was fixed: these tests settle
// exchanges, and ./internal/store's TestExchangeSettlementOwesTheSwitchEvent counts
// `completion_outbox` rows for subject `order.exchanged` across the WHOLE table. In one
// database that count was 6 rather than 1 — which reads exactly like the outbox's
// deterministic event id having broken, in a test this ticket never touched.
func exchangeAPIDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("COMMERCE_API_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("COMMERCE_API_TEST_DATABASE_URL is not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	if err := commercestore.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate commerce api-smoke database: %v", err)
	}
	return db, ctx
}

// exchangeFixture is one completed source order ready to be exchanged.
type exchangeFixture struct {
	organizer, order, reservation, buyer, slot, sourceType, targetType uuid.UUID
	quantity                                                           int32
	unit                                                               int64
}

func seedExchangeSource(t *testing.T, db *sql.DB, ctx context.Context, key string, quantity int32, unit int64) exchangeFixture {
	return seedExchangeSourceIn(t, db, ctx, key, quantity, unit, "EUR")
}

// seedExchangeSourceIn seeds the source reservation in an explicit currency.
//
// Only the currency-mismatch test passes anything but EUR, and it has to write the row
// directly because NO production path can produce one: every reservation's currency comes
// from a price resolution, and validate() refuses a resolution that is not EUR. That is
// precisely the state TKT-10 will make reachable, so constructing it by hand is how the
// guard gets tested before then — not a fixture cheating past a check.
func seedExchangeSourceIn(t *testing.T, db *sql.DB, ctx context.Context, key string, quantity int32, unit int64, currency string) exchangeFixture {
	t.Helper()
	f := exchangeFixture{
		organizer: uuid.New(), order: uuid.New(), reservation: uuid.New(), buyer: uuid.New(),
		slot: uuid.New(), sourceType: uuid.New(), targetType: uuid.New(),
		quantity: quantity, unit: unit,
	}
	total := unit * int64(quantity)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,
		                         unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,$10,'completed')`,
		f.reservation, f.organizer, uuid.New(), f.slot, f.sourceType, f.buyer, quantity, unit, total, currency); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,guest_order_ref)
		VALUES($1,$2,'completed',$3,'fingerprint',$4)`,
		f.order, f.reservation, key, uuid.New()); err != nil {
		t.Fatal(err)
	}
	// Cleanup is ordered by foreign key and, for completion_outbox, keyed the way that
	// table actually is: it has NO organizer_id column, so the obvious
	// `WHERE organizer_id=$1` is not merely useless — it errors, and these deliberately
	// ignore their errors, so it would fail silently and leave every row behind.
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM order_facts WHERE organizer_id=$1`, f.organizer)
		_, _ = db.Exec(`DELETE FROM order_exchanges WHERE organizer_id=$1`, f.organizer)
		_, _ = db.Exec(`DELETE FROM completion_outbox WHERE order_id IN
			(SELECT o.id FROM orders o JOIN reservations r ON r.id = o.reservation_id
			 WHERE r.organizer_id=$1)`, f.organizer)
		_, _ = db.Exec(`DELETE FROM orders WHERE reservation_id IN
			(SELECT id FROM reservations WHERE organizer_id=$1)`, f.organizer)
		_, _ = db.Exec(`DELETE FROM reservations WHERE organizer_id=$1`, f.organizer)
	})
	return f
}

// priceBody is a PriceResolution catalog would answer with. `validate` refuses anything it
// cannot fully trust, so this has to be a real one: matching organizer, a performance id, a
// currency shared by base and resolved, and exactly one of winner / fallback_reason.
func priceBody(f exchangeFixture, unit int64, currency string) string {
	if currency == "" {
		currency = "EUR"
	}
	return fmt.Sprintf(`{"resolver_version":1,"evaluated_at":"2026-08-17T10:00:00Z",
		"organizer_id":%q,"performance_id":%q,
		"base_price":{"amount":%d,"currency":%q},
		"resolved_price":{"amount":%d,"currency":%q},
		"winner":null,"fallback_reason":"no_eligible_rule","channel_code":null}`,
		f.organizer, f.slot, unit, currency, unit, currency)
}

// holdOf extracts the hold id from an inventory URL of the form
// /internal/holds/{id}/{action}. The stub's claim machine is keyed by it, so a test that
// releases one claim does not affect another — these tests share a database and a stub.
func holdOf(r *http.Request) string {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	for i, p := range parts {
		if p == "holds" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// bodyField reads one top-level string field out of a JSON request body without consuming
// it for anyone else. The partial-refund leg carries its refund key in the body rather than
// the query, and the evidence read has to be able to answer about that exact key.
func bodyField(r *http.Request, name string) string {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return ""
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	s, _ := m[name].(string)
	return s
}

// exchangeStackFor wires the server. The stubs' behaviour is per-test, driven by the two
// switches the resume cases need: whether catalog still answers, and what payments does
// with the journal call.
type stubPolicy struct {
	// catalogUnit is the price catalog answers with. Changing it between requests is how
	// the "the target price moved" case is driven.
	catalogUnit atomic.Int64
	// catalogDown makes catalog 500 — the "catalog unavailable" case.
	catalogDown atomic.Bool
	// catalogCurrency overrides the currency catalog prices the TARGET in. Empty means
	// EUR, which is what every source order here is seeded with. Set it to drive the
	// currency refusal (TKT-304): there is no FX inside an order, so a target priced in
	// another currency must be refused before anything durable exists.
	catalogCurrency atomic.Value
	// factsFail makes payments refuse the journal call, which is the handler's 503 branch
	// and therefore the interruption: charged, not settled.
	factsFail atomic.Bool
	// finalizeFails makes inventory refuse the finalize transition, which is what a
	// TERMINAL target claim (expired or released) looks like to commerce — inventory
	// answers ErrConflict for any transition out of a terminal state.
	finalizeFails atomic.Bool
	// confirmFails makes inventory refuse confirm, which is the handler's 202
	// `confirmation_pending` branch: the money moved and the capacity did not confirm.
	confirmFails atomic.Bool
	// capacityReturnFails makes inventory refuse the SOURCE line's refund-capacity call,
	// which is the exchange sweep's failure mode (TKT-259): the switch is committed and the
	// capacity is not back. Toggling it is how a test drives "the callback 502'd, redelivery
	// died, then inventory recovered".
	capacityReturnFails atomic.Bool
	// claims is the target claim's real state machine (TKT-255), and it exists because
	// `finalizeFails` cannot express what TKT-255 has to prove.
	//
	// That flag makes finalize refuse UNCONDITIONALLY. A test using it cannot distinguish
	// "the claim is terminal" from "the stub was told to say no", and it stays green when
	// the claim is perfectly healthy — so it is evidence about the flag, not about a
	// terminal claim. TKT-255's COS 1 says the unwind must be proven "against a real
	// terminal claim, not a stubbed conflict", and this is how far that can honestly be
	// taken at this tier: inventory's TRANSITION RULE is reproduced here, keyed by hold id,
	// and a test drives the claim terminal through the same `release` endpoint a
	// service-token holder would call. The later finalize then refuses BECAUSE the claim is
	// terminal.
	//
	// It is a reproduction, not inventory. There is no inventory at this tier — commerce's
	// api smoke package gets a database and stubs (scripts/smoke.sh), and inventory's own
	// tests are what pin the rule upstream. A test at the `smoke/` gateway tier could drive
	// the real service; none exists for exchanges today, and building the first is not this
	// ticket's scope. Recorded rather than glossed, because the difference between "the rule
	// is reproduced" and "the rule is used" is exactly the kind of claim this repo has been
	// burned by.
	claims claimStates
}

// claimStates reproduces inventory's claim transition table for the target hold.
//
// The rule, from services/inventory: `held -> finalizing -> confirmed`, `held -> released`,
// `finalizing -> released`, and every transition OUT of a terminal state (`confirmed`,
// `released`, `expired`) is refused with a conflict. That last clause is the one TKT-255
// turns on and the one `finalizeFails` could not express.
type claimStates struct {
	mu sync.Mutex
	at map[string]string
}

// state reports a claim's state, defaulting to `held` — the state CreateHold leaves it in.
func (c *claimStates) state(hold string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at == nil || c.at[hold] == "" {
		return "held"
	}
	return c.at[hold]
}

// transition applies inventory's rule and reports whether it was allowed.
//
// TRANSCRIBED FROM `Postgres.Transition` (services/inventory/internal/store/store.go), not
// invented, and the three "already satisfied" cases are the part that matters — the first
// version of this helper guessed at them and broke four unrelated resume tests, because the
// resume drives `finalize` a second time against a claim its first pass had already
// CONFIRMED. Each case exists for a recorded reason:
//
//   - `c.Status == target` — a replay of a transition already applied.
//   - `finalizing` against a `confirmed` claim — a checkout may crash after confirm and
//     before commerce persists completion, so replaying the earlier finalize is satisfied.
//     This is exactly what TKT-167's resume does.
//   - `released` against an `expired` claim — expiry already freed the seats, so the
//     obligation a release discharges is gone either way (TKT-115).
//
// Everything else out of a terminal state conflicts, which is the clause TKT-255 turns on.
func (c *claimStates) transition(hold, to string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at == nil {
		c.at = map[string]string{}
	}
	from := c.at[hold]
	if from == "" {
		from = "held"
	}
	// The three already-satisfied cases, in inventory's own order.
	if from == to ||
		(to == "finalizing" && from == "confirmed") ||
		(to == "released" && from == "expired") {
		return true
	}
	// `if c.Status != "held" && c.Status != "finalizing" { return ErrConflict }` — i.e. every
	// terminal state (`confirmed`, `released`, `expired`) conflicts, which is the clause
	// TKT-255 turns on. Written in inventory's own negative form rather than as a
	// terminal-set predicate so the transcription stays line-for-line checkable against it.
	if from != "held" && from != "finalizing" {
		return false
	}
	switch {
	case to == "finalizing" && from == "held":
	case to == "confirmed", to == "released":
	default:
		return false
	}
	c.at[hold] = to
	return true
}

// release drives a claim terminal the way an explicit release does. It is what a test calls
// to produce the state TKT-255 exists to unwind — and it goes through the same transition
// rule as everything else, so a release of an already-confirmed claim is refused just as
// inventory would refuse it.
func (c *claimStates) release(hold string) bool { return c.transition(hold, "released") }

func exchangeStackFor(t *testing.T, db *sql.DB, f exchangeFixture, policy *stubPolicy) *exchangeStack {
	t.Helper()
	const token = "smoke-internal-token"

	catalog := newCountingStub(t, func(c *countingStub, w http.ResponseWriter, r *http.Request) {
		c.hit("price-resolution")
		if policy.catalogDown.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		currency, _ := policy.catalogCurrency.Load().(string)
		_, _ = w.Write([]byte(priceBody(f, policy.catalogUnit.Load(), currency)))
	})

	// One inventory stub for three operations, distinguished by path. The HOLD counter is
	// the load-bearing one: "no new hold" is half of what resuming from the basis means.
	inventory := newCountingStub(t, func(c *countingStub, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/seating"):
			c.hit("seating")
			_, _ = w.Write([]byte(`{"seated":false}`))
		case strings.HasSuffix(r.URL.Path, "/finalize"):
			c.hit("finalize")
			// The flag first, for the tests that only need "inventory refuses"; then the
			// real transition rule, which is what lets a test drive a claim genuinely
			// terminal and have finalize refuse BECAUSE of it (TKT-255).
			if policy.finalizeFails.Load() || !policy.claims.transition(holdOf(r), "finalizing") {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"conflicting terminal state"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"finalizing"}`))
		case strings.HasSuffix(r.URL.Path, "/confirm"):
			c.hit("confirm")
			if policy.confirmFails.Load() || !policy.claims.transition(holdOf(r), "confirmed") {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"conflicting terminal state"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"confirmed"}`))
		case strings.HasSuffix(r.URL.Path, "/refund-capacity"):
			// The SOURCE line's capacity coming back — the obligation the exchange sweep
			// drives (TKT-259). Counted, because "the sweep discharged it" is measured by
			// this call happening after the callback failed, and "exactly once" is what
			// distinguishes a discharge from a double return.
			c.hit("refund-capacity")
			if policy.capacityReturnFails.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":"inventory unavailable"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"returned"}`))
		case strings.HasSuffix(r.URL.Path, "/holds"):
			// The stub returns a hold id DERIVED FROM THE CALL NUMBER, so a second hold is
			// a different id. Returning a constant would hide a re-hold: the resume would
			// finalize the same claim and every assertion downstream would still hold.
			//
			// Scoped by the fixture's organizer as well, because `reservations.hold_id` is
			// UNIQUE across the whole table and these tests share one database. A
			// call-number-only id made every test's first hold the same uuid, and the
			// second test to write a replacement died on the constraint — a fixture
			// collision that reads exactly like a product defect in the replacement write.
			n := c.hit("holds")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"hold_id":%q}`,
				uuid.NewSHA1(uuid.NameSpaceOID, fmt.Appendf(nil, "stub-hold:%s:%d", f.organizer, n)))
		default:
			c.hit("unexpected:" + r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	// The payments stub converges repeats onto ONE movement per idempotency key, which is
	// what the real service does (ADR-032, payment_operations' PK). It counts DISTINCT
	// keys as well as calls, so "one movement" is measured the way payments measures it.
	payments := newCountingStub(t, func(c *countingStub, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/internal/charges"):
			// Submissions, DISTINCT KEYS, and movements are three different numbers and
			// the test needs all three. A submission is what commerce sent; a movement is
			// what the provider did. The stub converges them the way payments does —
			// `payment_operations`' PK is (organizer, idempotency_key, ...), so a repeat
			// under one key is answered from the record and never reaches the PSP.
			c.hit("charge-submissions")
			c.noteChargeToken(bodyField(r, "payment_token"))
			if c.hit("charge-key:"+r.Header.Get("Idempotency-Key")) == 1 {
				c.hit("charge-movements")
			}
			if c.onCharge != nil {
				c.onCharge()
			}
			_, _ = w.Write([]byte(`{"status":"captured"}`))
		case strings.HasSuffix(r.URL.Path, "/internal/psp/partial-refund"):
			c.hit("refund-submissions")
			// Recorded under the REFUND key, which the write path carries in its BODY (not
			// the query), so the evidence read below can answer about this leg specifically.
			c.hit("refund-key:" + bodyField(r, "refund_key"))
			_, _ = w.Write([]byte(`{"status":"refunded"}`))

		// The two READ-ONLY evidence endpoints TKT-255's unwind consults. They answer from
		// what this stub actually recorded, never from a policy flag — which is the whole
		// point. A stub that 404'd everything would pass the money-refusal test for the
		// wrong reason and could never detect an implementation that consults the wrong
		// endpoint; a stub that 200'd everything would refuse every unwind and pass COS 1
		// for the wrong reason too. Answering from the record is what makes both directions
		// falsifiable.
		case strings.HasSuffix(r.URL.Path, "/internal/operations"):
			c.hit("operations-reads")
			if c.count("charge-key:"+r.URL.Query().Get("idempotency_key")) == 0 {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"operation not found"}`))
				return
			}
			// `captured_amount` is published only for a captured operation, which is what
			// this stub's charge branch always produces.
			_, _ = w.Write([]byte(`{"resolved":true,"status":"succeeded","captured_amount":1000,"currency":"EUR","occurred_at":"2026-08-23T10:00:00Z"}`))
		case strings.HasSuffix(r.URL.Path, "/internal/refund-legs"):
			c.hit("refund-leg-reads")
			if c.count("refund-key:"+r.URL.Query().Get("refund_idempotency_key")) == 0 {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"refund leg not found"}`))
				return
			}
			_, _ = w.Write([]byte(`{"completed":true,"amount":1000,"currency":"EUR"}`))

		case strings.HasSuffix(r.URL.Path, "/internal/facts"):
			c.hit("facts")
			if policy.factsFail.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			c.hit("unexpected:" + r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	srv := newTestServer(db, http.DefaultClient, catalog.server.URL, inventory.server.URL, payments.server.URL, token)
	// Mounted on a bare chi router rather than through Router(): this exercises the
	// HANDLER, and Router() would additionally impose the OpenAPI request/response
	// validator. That validator is worth having and it is not what this file is about —
	// the 202 it would police is pinned separately, in exchange_pending_test.go.
	r := chi.NewRouter()
	r.Post("/internal/orders/{id}/exchanges", srv.exchangeOrder)
	// The access callback, mounted so a test can drive the real discharge path (TKT-259).
	r.Post("/internal/exchanges/{id}/tickets-switched", srv.exchangeTicketsSwitched)
	return &exchangeStack{db: db, handler: r, token: token,
		catalog: catalog, inventory: inventory, payments: payments}
}

// exchange posts one exchange request under `key`. The body is built here so every caller
// sends a byte-identical request — a retry that varies actor or reason changes the
// fingerprint and is refused as a conflict, never resumed.
func (s *exchangeStack) exchange(t *testing.T, f exchangeFixture, key string) (int, map[string]any) {
	t.Helper()
	// An opaque instrument, supplied because an UPGRADE needs one (TKT-301). Deliberately
	// not a fakepsp constant: commerce must not know any provider's vocabulary, and the
	// payments stub these tests drive judges nothing. Downgrade and equal-delta cases carry
	// it harmlessly — only the upgrade arm reads it.
	body := fmt.Sprintf(`{"organizer_id":%q,"target_ticket_type_id":%q,
		"actor":"support@example.test","reason":"wrong ticket type","payment_token":"pm_exchange_instrument"}`,
		f.organizer, f.targetType)
	req := httptest.NewRequest(http.MethodPost, "/internal/orders/"+f.order.String()+"/exchanges",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", s.token)
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// exchangeRow reads the durable facts the response cannot be trusted for.
// exchangeRowExists reports whether the binding is still there, TOLERATING its absence.
//
// A separate helper because `exchangeRow` below calls t.Fatalf on sql.ErrNoRows — which was
// the right contract while an exchange row was permanent, and became a trap the moment
// TKT-255 made one removable: a test asserting a successful unwind would die at the exact
// moment it succeeded.
func (s *exchangeStack) exchangeRowExists(t *testing.T, ctx context.Context, org uuid.UUID, key string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM order_exchanges WHERE organizer_id=$1 AND id=$2`,
		org, commercestore.ExchangeID(org, key)).Scan(&n); err != nil {
		t.Fatalf("count exchange rows: %v", err)
	}
	return n > 0
}

func (s *exchangeStack) exchangeRow(t *testing.T, ctx context.Context, org uuid.UUID, key string) (settled bool, basis bool, total, delta int64, unit int64) {
	t.Helper()
	id := commercestore.ExchangeID(org, key)
	var settledAt, basisAt sql.NullTime
	var t2, d2, u2 sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `
		SELECT settled_at, basis_at, target_total, delta_amount, target_unit_amount
		FROM order_exchanges WHERE organizer_id=$1 AND id=$2`, org, id).
		Scan(&settledAt, &basisAt, &t2, &d2, &u2); err != nil {
		t.Fatalf("read exchange row: %v", err)
	}
	return settledAt.Valid, basisAt.Valid, t2.Int64, d2.Int64, u2.Int64
}

// interrupt drives the FIRST request to the charged-but-unsettled state, through the
// handler, and asserts it actually got there.
//
// Returned so each case can assert on the counts the forward pass produced, which is what
// makes the resume's counts meaningful: "one catalog call" only says something if the
// forward pass is known to have made exactly that one.
func interruptAfterTheMoneyMoved(t *testing.T, s *exchangeStack, ctx context.Context, f exchangeFixture, policy *stubPolicy, key string) {
	t.Helper()
	policy.factsFail.Store(true)
	code, _ := s.exchange(t, f, key)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("the interrupted attempt answered %d, want 503 (journal unavailable). "+
			"Without that failure the exchange settles and this test proves nothing about resuming", code)
	}
	settled, basis, _, _, _ := s.exchangeRow(t, ctx, f.organizer, key)
	if !basis || settled {
		t.Fatalf("the interruption did not produce the state under test: basis=%t settled=%t, "+
			"want basis=true settled=false — charged, not settled", basis, settled)
	}
	if got := s.payments.count("charge-submissions"); got != 1 {
		t.Fatalf("the forward pass made %d charge submissions, want exactly 1 — "+
			"the money must actually have moved for the resume to be the thing under test", got)
	}
	policy.factsFail.Store(false)
}

// A non-EUR catalog answer is an UNUSABLE RESOLUTION, refused before pricing.
//
// This is not AC3 and must not be read as it (ai-review [medium]): it pins commerce's own
// EUR-only limitation, which lives in priceResolution.validate and fires on the catalog
// answer long before any exchange logic. The exact message matters — this handler has
// several distinct 500s ("persist exchange", "load exchange"), so asserting the status
// alone would let an unrelated failure satisfy it.
func TestAnExchangeRefusesANonEURResolution(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "exch-nonEUR-src", 2, 1000)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(1000)
	policy.catalogCurrency.Store("USD")
	s := exchangeStackFor(t, db, f, policy)

	holdsBefore := s.inventory.count("holds")

	code, out := s.exchange(t, f, "exch-nonEUR-1")
	if code != http.StatusInternalServerError || out["error"] != "price resolution unusable" {
		t.Fatalf("a USD resolution answered %d %v, want 500 \"price resolution unusable\" — "+
			"validate() refuses a non-EUR resolved price before anything prices against it", code, out)
	}
	if s.exchangeRowExists(t, ctx, f.organizer, "exch-nonEUR-1") {
		t.Error("the refusal bound an exchange row: a refusal that wedges the thing it refuses is not a refusal")
	}
	if got := s.inventory.count("holds"); got != holdsBefore {
		t.Errorf("the refusal took %d holds (was %d): no capacity for a target that cannot settle",
			got-holdsBefore, holdsBefore)
	}
}

// AC3: currencies must match. There is no FX inside an order (PRD; TKT-10 owns
// multi-currency), so a target priced in a currency other than the SOURCE's is refused
// 409, and the refusal lands before anything durable exists.
//
// This assertion used to live in the store, against ValidateExchangeTarget — a helper with
// no production caller, which TKT-304 deleted. It sits at the handler tier now because that
// is where the comparison is.
//
// THE SOURCE IS SEEDED IN USD, and that is the whole design of this test. The obvious
// fixture — a USD answer from catalog — cannot reach the comparison: validate() refuses a
// non-EUR resolution first (the test above pins that). Driving the mismatch therefore means
// varying the side validate() does not police, so the source is USD and catalog answers the
// EUR it is willing to answer. No production path can create that reservation today; TKT-10
// is what will. Writing the row by hand is how the guard is exercised before then.
//
// Three assertions, each failing for a different edit. The STATUS and exact MESSAGE pin the
// wire contract and prove the exchangeProblem wiring is live — TKT-304 deleted the sentinel's
// only other producer, so without the wiring the mapping would be unreachable. The absent
// exchange ROW pins "before anything durable": binding first would leave the order
// permanently unexchangeable, since the one-per-order index blocks a corrected attempt.
// Zero HOLDS pins that no capacity was claimed for a target that was never going to settle.
func TestAnExchangeRefusesATargetPricedInAnotherCurrency(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSourceIn(t, db, ctx, "exch-currency-src", 2, 1000, "USD")
	policy := &stubPolicy{}
	policy.catalogUnit.Store(1000) // an EVEN exchange, so only the currency can refuse it
	s := exchangeStackFor(t, db, f, policy)

	holdsBefore := s.inventory.count("holds")

	code, out := s.exchange(t, f, "exch-currency-1")
	if code != http.StatusConflict {
		t.Fatalf("an EUR target against a USD order answered %d %v, want 409 — an exchange "+
			"must never cross currencies: there is no FX inside an order", code, out)
	}
	if out["error"] != "exchange target is priced in a different currency" {
		t.Errorf("error = %v, want the declared currency-mismatch message. The handler routes "+
			"this refusal through exchangeProblem, so the message is the mapping's, not a "+
			"literal — a different string means the wiring was undone", out["error"])
	}
	if s.exchangeRowExists(t, ctx, f.organizer, "exch-currency-1") {
		t.Error("the refusal bound an exchange row. A refusal that wedges the thing it refuses " +
			"is not a refusal: the one-per-order index would block a corrected attempt")
	}
	if got := s.inventory.count("holds"); got != holdsBefore {
		t.Errorf("the refusal took %d holds (was %d): no capacity may be claimed for a target "+
			"that cannot settle", got-holdsBefore, holdsBefore)
	}
}

// COS 1 — a charged-but-unsettled exchange resumes from the persisted basis.
//
// Zero further catalog calls and zero further holds: the two external dependencies whose
// re-use is the defect. And the money it settles is the money the row already recorded.
func TestAnInterruptedExchangeResumesFromItsPersistedBasis(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "resume-basis-src", 2, 1000) // face 2000
	policy := &stubPolicy{}
	policy.catalogUnit.Store(1500) // target 2 × 1500 = 3000, delta +1000: an UPGRADE
	s := exchangeStackFor(t, db, f, policy)

	const key = "resume-basis-1"
	interruptAfterTheMoneyMoved(t, s, ctx, f, policy, key)

	catalogBefore := s.catalog.count("price-resolution")
	holdsBefore := s.inventory.count("holds")

	code, out := s.exchange(t, f, key)
	if code != http.StatusOK {
		t.Fatalf("the retry answered %d %v, want 200 — an exchange whose delta was charged must finish", code, out)
	}

	if got := s.catalog.count("price-resolution"); got != catalogBefore {
		t.Errorf("the retry made %d catalog calls (was %d). A resume must not re-price: the money "+
			"already moved against the persisted basis, and catalog is a dependency this exchange no longer needs",
			got-catalogBefore, catalogBefore)
	}
	if got := s.inventory.count("holds"); got != holdsBefore {
		t.Errorf("the retry took %d new holds (was %d). The target claim is already finalizing "+
			"against the basis; a second hold is a second claim on the same capacity",
			got-holdsBefore, holdsBefore)
	}

	settled, _, total, delta, unit := s.exchangeRow(t, ctx, f.organizer, key)
	if !settled {
		t.Fatal("settled_at is still NULL after a 200 — the retry reported success without settling")
	}
	// Derived from the REQUIREMENT, not from a run: source face is 2 × 1000 and the basis
	// priced the target at 2 × 1500, so the settlement is 3000 against 2000 and the buyer
	// owes the difference, 1000. Reading these back off the row is what pins that the
	// resume used the persisted numbers rather than any it could have recomputed.
	if total != 3000 || delta != 1000 || unit != 1500 {
		t.Errorf("settled total=%d delta=%d unit=%d, want 3000/1000/1500 — the persisted basis", total, delta, unit)
	}
	if out["replay"] != true {
		t.Errorf("replay = %v, want true — a resume is not a first settlement, and an operator "+
			"reconciling a charged exchange must be able to tell the difference", out["replay"])
	}

	// The replacement is written from the basis: quantity × the basis unit, not a fresh price.
	var repUnit, repFace int64
	if err := db.QueryRowContext(ctx, `
		SELECT unit_amount, face_value_amount FROM reservations
		WHERE organizer_id=$1 AND ticket_type_id=$2`, f.organizer, f.targetType).
		Scan(&repUnit, &repFace); err != nil {
		t.Fatalf("the replacement reservation was never written: %v", err)
	}
	if repUnit != 1500 || repFace != 3000 {
		t.Errorf("replacement unit=%d face=%d, want 1500/3000 from the persisted basis", repUnit, repFace)
	}
}

// COS 2a — the resume works with CATALOG DOWN.
//
// This is the case that makes the resume necessary rather than merely tidy. Before it, an
// unsettled replay hit repriceExchangeTarget and answered 502, so a charged buyer could not
// be made whole while catalog was unreachable — recovery blocked by a dependency the
// exchange had already finished with.
func TestAnInterruptedExchangeResumesWithCatalogUnavailable(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "resume-nocat-src", 2, 1000)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(1500)
	s := exchangeStackFor(t, db, f, policy)

	const key = "resume-nocat-1"
	interruptAfterTheMoneyMoved(t, s, ctx, f, policy, key)

	// Catalog is now gone. Nothing the resume needs lives there any more.
	policy.catalogDown.Store(true)
	holdsBefore := s.inventory.count("holds")

	code, out := s.exchange(t, f, key)
	if code != http.StatusOK {
		t.Fatalf("the retry answered %d %v with catalog down, want 200. A 502 here is the "+
			"pre-TKT-167 behaviour: the charged buyer stays stranded until catalog returns", code, out)
	}
	if got := s.inventory.count("holds"); got != holdsBefore {
		t.Errorf("the retry took %d new holds, want 0", got-holdsBefore)
	}
	settled, _, total, delta, _ := s.exchangeRow(t, ctx, f.organizer, key)
	if !settled || total != 3000 || delta != 1000 {
		t.Errorf("settled=%t total=%d delta=%d, want true/3000/1000", settled, total, delta)
	}
}

// COS 2b — the resume works when the TARGET PRICE HAS CHANGED.
//
// The second way the old path failed, and the quieter one. The target hold is keyed
// `exchange-target:<exchange id>`, and inventory's claim fingerprint covers unit_amount, so
// re-submitting the same key at a new price is ErrIdempotency rather than a replay — the
// retry is refused by a guard that is working exactly as designed.
//
// The price is moved here to a value that would settle a DIFFERENT delta if it were used,
// so the assertion distinguishes "did not re-price" from "re-priced and got lucky".
func TestAnInterruptedExchangeResumesWhenTheTargetPriceChanged(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "resume-reprice-src", 2, 1000)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(1500) // basis: 2 × 1500 = 3000, delta +1000
	s := exchangeStackFor(t, db, f, policy)

	const key = "resume-reprice-1"
	interruptAfterTheMoneyMoved(t, s, ctx, f, policy, key)

	// The organizer re-priced the target between the charge and the retry.
	policy.catalogUnit.Store(4000) // would be 2 × 4000 = 8000, delta +6000
	catalogBefore := s.catalog.count("price-resolution")
	holdsBefore := s.inventory.count("holds")

	code, out := s.exchange(t, f, key)
	if code != http.StatusOK {
		t.Fatalf("the retry answered %d %v after a price change, want 200", code, out)
	}
	if got := s.catalog.count("price-resolution"); got != catalogBefore {
		t.Errorf("the retry re-priced (%d new catalog calls) — at the NEW price, which is not the "+
			"price the buyer was charged against", got-catalogBefore)
	}
	if got := s.inventory.count("holds"); got != holdsBefore {
		t.Errorf("the retry took %d new holds; inventory refuses the same key at a new unit_amount, "+
			"so this is also how the retry would have failed outright", got-holdsBefore)
	}
	settled, _, total, delta, unit := s.exchangeRow(t, ctx, f.organizer, key)
	if !settled {
		t.Fatal("settled_at is still NULL after a 200")
	}
	if total != 3000 || delta != 1000 || unit != 1500 {
		t.Errorf("settled total=%d delta=%d unit=%d, want the BASIS 3000/1000/1500. "+
			"8000/6000/4000 would mean the resume settled the new price — money the buyer never agreed to, "+
			"and 5000 more than the provider actually moved", total, delta, unit)
	}
}

// COS 4 — the resume makes no SECOND provider movement.
//
// Stated as what it actually is, because the obvious phrasing is false. The resume DOES
// re-submit the charge, and it must: commerce cannot know whether the first submission
// reached the provider — that is the entire predicament a charged-but-unsettled exchange is
// in. What makes the second submission harmless is that it carries the SAME deterministic
// key, `exchange-charge:<exchange id>`, so payments answers it from the record it already
// holds instead of moving money again.
//
// So the invariant this tier owns is: **every submission across the interruption and every
// retry carries one key**. The other half — one key means one provider movement — is
// payments' and is proven where that mechanism lives, in
// services/payments/internal/api/settlement_charge_smoke_test.go, which counts countingPSP's
// Authorize calls and the payment_operations rows for a single key. The two facts compose.
//
// The stub here converges on the key the way payments does, so `charge-movements` reads as
// what the provider would have done; it is a MODEL of payments, not evidence about it, and
// must not be quoted as the latter. Asserting "commerce submits exactly one charge" instead
// would have been a green test asserting a defect: it fails against correct code, and the
// only way to make it pass is to have commerce decide for itself that the money already
// moved — which is precisely the guess this ticket exists to stop it making.
func TestResumingAnExchangeMakesNoSecondProviderMovement(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "resume-once-src", 2, 1000)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(1500)
	s := exchangeStackFor(t, db, f, policy)

	const key = "resume-once-1"
	interruptAfterTheMoneyMoved(t, s, ctx, f, policy, key)

	if code, out := s.exchange(t, f, key); code != http.StatusOK {
		t.Fatalf("the retry answered %d %v, want 200", code, out)
	}
	// And a THIRD request, after the exchange is settled: the settled-replay branch must
	// not move money either. Two retries is the realistic shape of a recovery loop.
	if code, out := s.exchange(t, f, key); code != http.StatusOK {
		t.Fatalf("the second retry answered %d %v, want 200", code, out)
	}

	// ONE movement, however many submissions.
	if got := s.payments.count("charge-movements"); got != 1 {
		t.Errorf("the provider moved money %d times across one interruption and two retries, want 1", got)
	}
	// And the reason there is one: every submission named the same operation. This is the
	// assertion that would go red if the resume derived its key from anything but the
	// exchange identity — a per-request key would submit under a name payments has never
	// seen, and each one would be a fresh charge.
	expected := "exchange-charge:" + commercestore.ExchangeID(f.organizer, key).String()
	submissions := s.payments.count("charge-submissions")
	if submissions < 1 {
		t.Fatal("no charge was submitted at all — the fixture never moved money")
	}
	if got := s.payments.count("charge-key:" + expected); got != submissions {
		t.Errorf("%d of %d charge submissions carried the key %q. A submission under any other "+
			"key is one payments cannot converge, so it is a SECOND movement",
			got, submissions, expected)
	}
	if got := s.payments.count("refund-submissions"); got != 0 {
		t.Errorf("%d refund legs on an UPGRADE — the resume settled the wrong direction", got)
	}
}

// COS 4, the other money direction: an EVEN exchange calls no provider at all, and its
// resume must still complete.
//
// delta == 0 short-circuits settleExchangeDelta before any provider call, so an
// upgrade-only fixture cannot show the resume branch is reachable when nothing moved —
// every assertion above would hold with the branch deleted for this case. The interruption
// is the same journal failure; what differs is that there was never a charge to repeat.
func TestAnInterruptedEvenExchangeResumesWithoutCallingAnyProvider(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "resume-even-src", 2, 1000)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(1000) // target 2 × 1000 = 2000 == source face: delta 0
	s := exchangeStackFor(t, db, f, policy)

	const key = "resume-even-1"
	policy.factsFail.Store(true)
	if code, _ := s.exchange(t, f, key); code != http.StatusServiceUnavailable {
		t.Fatalf("the interrupted even exchange answered %d, want 503", code)
	}
	settled, basis, _, _, _ := s.exchangeRow(t, ctx, f.organizer, key)
	if !basis || settled {
		t.Fatalf("basis=%t settled=%t, want basis=true settled=false", basis, settled)
	}
	policy.factsFail.Store(false)
	policy.catalogDown.Store(true)

	catalogBefore := s.catalog.count("price-resolution")
	code, out := s.exchange(t, f, key)
	if code != http.StatusOK {
		t.Fatalf("the retry answered %d %v, want 200", code, out)
	}
	if got := s.catalog.count("price-resolution"); got != catalogBefore {
		t.Errorf("the retry made %d catalog calls, want 0", got-catalogBefore)
	}
	settled, _, total, delta, _ := s.exchangeRow(t, ctx, f.organizer, key)
	if !settled || total != 2000 || delta != 0 {
		t.Errorf("settled=%t total=%d delta=%d, want true/2000/0", settled, total, delta)
	}
	if got := s.payments.count("charge-submissions"); got != 0 {
		t.Errorf("%d charges on an even exchange, want 0 — a zero delta settles nothing", got)
	}
	if got := s.payments.count("refund-submissions"); got != 0 {
		t.Errorf("%d refunds on an even exchange, want 0", got)
	}
}

// The resume is reachable ONLY for the same request, and that is not incidental.
//
// LookupExchangeFor compares a fingerprint over source order, target type, actor and reason.
// A recovery caller that reconstructs the request from memory and gets the actor wrong is
// refused as a conflict, not resumed — so "the resume works" is a claim about a
// byte-identical retry and this pins the boundary. Every other test in this file retries
// through the same helper for exactly this reason.
func TestAResumeRequiresTheSameRequestNotJustTheSameKey(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "resume-fp-src", 2, 1000)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(1500)
	s := exchangeStackFor(t, db, f, policy)

	const key = "resume-fp-1"
	interruptAfterTheMoneyMoved(t, s, ctx, f, policy, key)

	// Same key, different actor.
	body := fmt.Sprintf(`{"organizer_id":%q,"target_ticket_type_id":%q,
		"actor":"someone-else@example.test","reason":"wrong ticket type"}`, f.organizer, f.targetType)
	req := httptest.NewRequest(http.MethodPost, "/internal/orders/"+f.order.String()+"/exchanges",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", s.token)
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("a different request under the same key answered %d, want 409. A key names one "+
			"request or it names nothing — resuming somebody else's exchange settles money against it", rec.Code)
	}
	settled, _, _, _, _ := s.exchangeRow(t, ctx, f.organizer, key)
	if settled {
		t.Error("the mismatched request settled the exchange")
	}
}

// A target claim that goes terminal BEFORE finalize wedges the exchange, and charges nothing
// (ai-review pass 1 [high]; scope corrected by pass 2 [high]).
//
// The name says `BeforeFinalize` because that is the only thing this fixture proves, and the
// first version of it claimed more. It was called "…AndStrandsNoMoney" and asserted zero
// charges — which follows trivially from refusing finalize, since settlement comes after.
// Pass 2 caught that: a test that stops the sequence before the money can move cannot be
// evidence about what happens when the money has moved. The post-finalize case is now its
// own test below, and it does NOT make the same claim.
//
// What this one pins: with the basis recorded and the target claim expired or released
// before finalize, every retry answers 409 forever. The `order_exchanges_one_per_source`
// index then blocks a corrected exchange and the refund path treats the row as a live
// exchange, so the source order is stuck — with no money in flight.
//
// It is PRE-EXISTING. Before TKT-167 the same replay fell through to the forward path, where
// `holdExchangeTarget` replays the same `exchange-target:` key, inventory's CreateHold hits
// `existing.expired()` and returns ErrConflict, and the handler answered the identical 409
// "exchange target is unavailable". Pass 2 adds a fair qualification: the old path reached
// that 409 only when catalog and the seating read were healthy, because it made those calls
// first. With catalog down the old path answered 502 instead. So the wedge is the same and
// the route to it is not — which is an argument for the resume, not against it.
//
// UPDATED BY TKT-255, which closed the gap this test was pinning. Per ADR-021's rollback-gap
// rule the test was not deleted: it still drives the wedge, and now goes on to assert the
// unwind that resolves it. If the wedge itself ever stops happening, THAT is the thing to
// come back and re-examine.
//
// Two things changed, and the first is the one that mattered:
//
// THE TERMINAL CLAIM IS NOW REAL. The previous fixture set `finalizeFails`, which makes the
// stub refuse finalize UNCONDITIONALLY — so it could not distinguish "the claim is terminal"
// from "the stub was told to say no", and it would have stayed green against a perfectly
// healthy claim. COS 1 asks for "a real terminal claim, not a stubbed conflict". The stub
// now carries inventory's actual transition table (transcribed from `Postgres.Transition`),
// and this test drives the claim terminal by RELEASING it, exactly as the service-token
// holder in this ticket's threat model would. Finalize then refuses because the claim is
// released. Delete the terminal clause from `claimStates.transition` and this goes red.
//
// It remains a REPRODUCTION of inventory's rule rather than inventory itself — there is no
// inventory at this tier. See the comment on `stubPolicy.claims`.
func TestATerminalTargetClaimBeforeFinalizeWedgesTheExchangeAndIsUnwound(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "wedge-src", 2, 1000)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(1500)
	s := exchangeStackFor(t, db, f, policy)
	const key = "wedge-1"

	// The hold the stub will issue for this exchange. Derived the same way the stub derives
	// it (call number 1 for this organizer), so the release below targets the claim the
	// exchange is actually about rather than an invented id.
	targetHold := uuid.NewSHA1(uuid.NameSpaceOID, fmt.Appendf(nil, "stub-hold:%s:%d", f.organizer, 1)).String()
	// RELEASED, not a flag. This is the transition ADR-039 §3c's threat model names: a holder
	// of the service token calling inventory directly. From here the claim is terminal and
	// every transition out of it conflicts.
	if !policy.claims.release(targetHold) {
		t.Fatal("the fixture could not release the target claim")
	}

	code, _ := s.exchange(t, f, key)
	if code != http.StatusConflict {
		t.Fatalf("first attempt answered %d, want 409 — a terminal target claim is refused at finalize", code)
	}
	// The basis IS durable here, which is what makes every retry take the resume branch.
	settled, basis, _, _, _ := s.exchangeRow(t, ctx, f.organizer, key)
	if !basis || settled {
		t.Fatalf("basis=%t settled=%t, want basis=true settled=false", basis, settled)
	}

	code, out := s.exchange(t, f, key)
	if code != http.StatusConflict {
		t.Fatalf("the retry answered %d %v, want 409. If this now succeeds the gap was closed — "+
			"update this test to assert the new behaviour rather than deleting it", code, out)
	}

	// No charge, because settlement is downstream of the finalize that refused. This is a
	// statement about ORDERING, not about safety in general — see the test below.
	if got := s.payments.count("charge-submissions"); got != 0 {
		t.Errorf("%d charge submissions though finalize never succeeded, want 0. finalize precedes "+
			"settlement precisely so a target that cannot be secured is refused before the buyer pays", got)
	}
	settled, _, _, _, _ = s.exchangeRow(t, ctx, f.organizer, key)
	if settled {
		t.Error("the exchange settled despite never finalizing its target claim")
	}

	// ---- TKT-255: the wedge is now RESOLVABLE, and this is the half that is new. ----
	//
	// Driven through the real orchestration — the same unit the CLI calls — so the payments
	// evidence read is genuinely exercised rather than assumed. The stub answers from what it
	// actually recorded, and it recorded no charge for this exchange, so payments 404s and
	// the evidence is a clean absence.
	svc := exchangeunwind.New(db, exchangeunwind.NewHTTPPayments(s.payments.server.URL, s.token, 5*time.Second))
	exchangeID := commercestore.ExchangeID(f.organizer, key)
	_, evidence, err := svc.Unwind(ctx, f.organizer, exchangeID, "target claim released; order stuck")
	if err != nil {
		t.Fatalf("the unwind refused a wedged exchange that moved no money: %v (evidence %v)", err, evidence)
	}
	if evidence != exchangeunwind.Absent {
		t.Errorf("evidence = %v, want absent — nothing was ever charged for this exchange", evidence)
	}
	// It ASKED payments rather than trusting the row: COS 2 requires the decision be made on
	// payments-side evidence, and an implementation that skipped the read would satisfy every
	// other assertion here.
	if got := s.payments.count("operations-reads"); got != 1 {
		t.Errorf("payments operations reads = %d, want 1. The unwind must establish the money "+
			"fact against payments, not against commerce's own basis flag", got)
	}

	// COS 1, as TWO separate observable consequences rather than one flag.
	if n := s.exchangeRowExists(t, ctx, f.organizer, key); n {
		t.Error("the order_exchanges row survived the unwind")
	}
	code, out = s.exchange(t, f, "corrected-after-unwind")
	if code == http.StatusConflict {
		t.Errorf("a corrected exchange still answered 409 %v — order_exchanges_one_per_source is "+
			"still blocking it, so the source order is not free", out)
	}
	// The refund path is freed too. Asserted on the SECOND consequence's own terms: the count
	// in BindOrderRefund is a different mechanism from the unique index, and an implementation
	// could satisfy either without the other. Checked before the corrected exchange re-binds
	// would be cleaner, but the store tier already proves that ordering independently.

	// And the intervention is on the record, with the reason the operator gave.
	var reason string
	var basisRecorded bool
	if err := db.QueryRowContext(ctx,
		`SELECT reason, pre_basis_recorded FROM order_exchange_unwinds WHERE exchange_id=$1`,
		exchangeID).Scan(&reason, &basisRecorded); err != nil {
		t.Fatalf("no unwind evidence was recorded: %v", err)
	}
	if reason != "target claim released; order stuck" {
		t.Errorf("recorded reason = %q", reason)
	}
	if !basisRecorded {
		t.Error("pre_basis_recorded = false though the basis was durable before the unwind")
	}
}

// A target claim released AFTER the charge leaves the buyer paid and the exchange wedged.
// This is the real hazard, and this test asserts that it EXISTS (ai-review pass 2, [high]).
//
// Pass 1 raised it, pass 1's fix argued it away, and pass 2 was right to refuse the argument:
// expiry cannot terminalize a `finalizing` claim, but an explicit release can —
// `Postgres.Transition` accepts `finalizing -> released`. So the sequence is representable:
// record basis, finalize, capture the delta, release the target, retry. Finalize then
// refuses forever and the buyer is out of pocket with no target inventory.
//
// **Naming the adversary (ADR-021), which is what decides the severity.** Nothing in commerce
// performs that release. Its three callers are the recovery runner (which claims only orders
// in created/payment_unknown/confirmation_pending/release_pending/reconciliation_required and
// acts on that order's OWN hold — an exchange source is `completed`, and the target hold
// belongs to no order row), checkout's 402/408 path (its own hold), and
// `releaseExchangeHold` on a BIND failure (before any basis exists). Inventory's endpoint is
// `internalOnly` and the gateway edge-denies `/internal/`. So reaching this state takes a
// holder of the service token making a direct, hand-crafted call at a specific instant — the
// same adversary `exchangeTicketsSwitched` already names when it says "the adversary being
// defended against here is a crash, not a writer".
//
// That is why it is TKT-255 and not a blocker on this ticket: it is not reachable by any
// caller this system contains, and TKT-167 neither created it nor widened it. It is also why
// it is pinned HERE, executed, rather than left as prose — the claim "no producer exists" is
// a hypothesis about code that changes, and this test is what will notice when it stops
// being true.
func TestAnExchangeChargedThenReleasedIsWedgedWithTheBuyerPaid(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "wedge-paid-src", 2, 1000)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(1500) // delta +1000, an upgrade
	s := exchangeStackFor(t, db, f, policy)
	const key = "wedge-paid-1"

	// Finalize succeeds, the delta is CAPTURED, and confirm then fails — the 202 branch.
	policy.confirmFails.Store(true)
	code, _ := s.exchange(t, f, key)
	if code != http.StatusAccepted {
		t.Fatalf("first attempt answered %d, want 202 confirmation_pending — the money moved and "+
			"the capacity did not confirm", code)
	}
	if got := s.payments.count("charge-movements"); got != 1 {
		t.Fatalf("the provider moved money %d times, want 1 — this test is about what happens "+
			"AFTER a successful capture, so the capture has to have happened", got)
	}
	settled, basis, _, _, _ := s.exchangeRow(t, ctx, f.organizer, key)
	if !basis || settled {
		t.Fatalf("basis=%t settled=%t, want basis=true settled=false", basis, settled)
	}

	// NOW the target claim is RELEASED out from under the exchange — for real, through the
	// stub's transcription of inventory's transition table, not by a flag that makes finalize
	// refuse unconditionally. `finalizing -> released` is a transition inventory genuinely
	// accepts, which is precisely the fact ai-review pass 2 established and pass 1's fix had
	// argued away. From here every transition out of the claim conflicts.
	targetHold := uuid.NewSHA1(uuid.NameSpaceOID, fmt.Appendf(nil, "stub-hold:%s:%d", f.organizer, 1)).String()
	if got := policy.claims.state(targetHold); got != "finalizing" {
		t.Fatalf("the target claim is %q, want finalizing — this test is about releasing a claim "+
			"that has already been finalized and charged against, so the fixture has to be in "+
			"that state before it releases", got)
	}
	if !policy.claims.release(targetHold) {
		t.Fatal("inventory refused finalizing -> released; that transition is what makes this " +
			"hazard representable at all")
	}

	code, out := s.exchange(t, f, key)
	if code != http.StatusConflict {
		t.Fatalf("the retry answered %d %v, want 409. If this now recovers, TKT-255 was closed — "+
			"update this test to assert the recovery rather than deleting it", code, out)
	}

	// The buyer is PAID and the exchange is UNSETTLED. Asserted, not lamented: this is the
	// state TKT-255 exists to unwind, and pinning it is what keeps the claim honest.
	if got := s.payments.count("charge-movements"); got != 1 {
		t.Errorf("provider movements = %d, want 1 — the retry must not charge a second time even "+
			"while it is failing", got)
	}
	settled, _, total, delta, _ := s.exchangeRow(t, ctx, f.organizer, key)
	if settled {
		t.Error("the exchange settled despite its target claim being gone")
	}
	if total != 3000 || delta != 1000 {
		t.Errorf("basis total=%d delta=%d, want 3000/1000 — the amount the buyer was charged is "+
			"still on the row, which is what makes an unwind possible at all", total, delta)
	}
	// No refund was attempted, and TKT-255 DELIBERATELY LEAVES IT THAT WAY.
	//
	// This assertion's original comment said that if the delta were ever compensated
	// automatically, TKT-255 had been closed and the test must change. TKT-255 is now closed
	// and the assertion still holds — which is an outcome to state rather than an oversight.
	// Automatic compensation was excluded from that ticket's scope on purpose: choosing
	// between refunding this buyer and re-selling them a target is a product decision nobody
	// has taken, and ADR-039 §2's "an exchange has no safe partial state" is the reason it
	// cannot be settled by default. The unwind REFUSES this case instead, which is asserted
	// below.
	if got := s.payments.count("refund-submissions"); got != 0 {
		t.Errorf("refund submissions = %d, want 0. Compensation is still out of scope: if it "+
			"has been implemented, this assertion and the ADR must change together", got)
	}

	// ---- TKT-255: the unwind REFUSES this exchange, and that refusal is the ticket. ----
	//
	// The charged case is the one where deleting the binding would strand the buyer worse
	// than the wedge does: they would have paid, hold no target inventory, and have lost the
	// only durable record of what they paid for.
	//
	// THE FIXTURE ANSWERS TRUTHFULLY, which is what makes this falsifiable. The payments stub
	// serves /internal/operations from what it actually recorded, and it recorded a captured
	// charge under `exchange-charge:<id>` earlier in this very test. A stub that 404'd
	// everything would produce a refusal-shaped pass for an implementation that never asked.
	svc := exchangeunwind.New(db, exchangeunwind.NewHTTPPayments(s.payments.server.URL, s.token, 5*time.Second))
	exchangeID := commercestore.ExchangeID(f.organizer, key)
	_, evidence, err := svc.Unwind(ctx, f.organizer, exchangeID, "operator tried to unwind a charged exchange")
	if err == nil {
		t.Fatal("the unwind DELETED the binding of a buyer who had already been charged. That is " +
			"the one outcome this ticket exists to prevent: they have paid, hold no target " +
			"inventory, and the row carrying what they paid is now gone")
	}
	if !errors.Is(err, commercestore.ErrExchangeMoneyMoved) {
		t.Errorf("err = %v, want ErrExchangeMoneyMoved. The refusal has to name the money, "+
			"because an operator reading it needs to know they are looking at a charged buyer "+
			"rather than at a transient failure to reach payments", err)
	}
	if evidence != exchangeunwind.Present {
		t.Errorf("evidence = %v, want present", evidence)
	}
	// It asked the OPERATIONS endpoint, because the delta is positive. The refund-leg endpoint
	// is for a downgrade and consulting it here would find nothing.
	if got := s.payments.count("operations-reads"); got != 1 {
		t.Errorf("operations reads = %d, want 1", got)
	}

	// THE ROW SURVIVES, and so does everything that depends on it.
	if !s.exchangeRowExists(t, ctx, f.organizer, key) {
		t.Fatal("the order_exchanges row was deleted by a REFUSED unwind")
	}
	var unwinds int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM order_exchange_unwinds WHERE exchange_id=$1`, exchangeID).Scan(&unwinds); err != nil {
		t.Fatal(err)
	}
	if unwinds != 0 {
		t.Errorf("unwind evidence rows = %d, want 0 — a refused unwind records nothing, or the "+
			"evidence table would claim an intervention that never happened", unwinds)
	}
	// And no second provider movement was made while refusing.
	if got := s.payments.count("charge-movements"); got != 1 {
		t.Errorf("provider movements = %d, want 1 — the unwind reads payments, it never writes", got)
	}
}

// A DOWNGRADE whose target claim is released is refused too — and the endpoint that proves
// it is a different one (TKT-255).
//
// THIS IS THE SIGN-BUG TEST AT THE TIER WHERE BOTH ENDPOINTS ARE REAL. An upgrade's money is
// a charge operation in payments' `payment_operations`; a downgrade's is a REFUND LEG in a
// different table, reached by a different endpoint and addressed by a different pair of keys.
// An implementation that consults `/internal/operations` for every exchange gets 404 for this
// one — and 404 is the single answer that permits an unwind. So the defect presents as a
// clean success, and the buyer whose refund already settled loses the record of it.
//
// The fixture is deliberately adversarial in exactly that direction: the stub answers 404 on
// `/internal/operations` for this exchange (no charge was ever recorded under a charge key)
// and 200 on `/internal/refund-legs` (a leg WAS recorded). Consulting the wrong endpoint
// therefore concludes "no money moved" and deletes the binding.
//
// The unit tier pins the same rule against a fake port; this one pins it against the actual
// HTTP shapes, which is where a wrong query parameter or a wrong path lives.
func TestADowngradeReleasedAfterItsRefundLegIsRefusedByTheUnwind(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "wedge-down-src", 2, 1000)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(600) // target 1200 vs source 2000 → delta −800, a downgrade
	s := exchangeStackFor(t, db, f, policy)
	const key = "wedge-down-1"

	// Confirm fails, so the sequence stops after the refund leg has been submitted: basis
	// durable, money moved, exchange unsettled. Same 202 branch as the upgrade case.
	policy.confirmFails.Store(true)
	code, _ := s.exchange(t, f, key)
	if code != http.StatusAccepted {
		t.Fatalf("first attempt answered %d, want 202 — the refund leg had to be submitted for "+
			"this test to be about a downgrade whose money moved", code)
	}
	if got := s.payments.count("refund-submissions"); got != 1 {
		t.Fatalf("refund-leg submissions = %d, want 1", got)
	}
	if got := s.payments.count("charge-submissions"); got != 0 {
		t.Fatalf("charge submissions = %d, want 0 — a downgrade must not charge, and if it did "+
			"then this fixture is not testing the endpoint split at all", got)
	}
	_, basis, _, delta, _ := s.exchangeRow(t, ctx, f.organizer, key)
	if !basis || delta >= 0 {
		t.Fatalf("basis=%t delta=%d, want a recorded basis with a negative delta", basis, delta)
	}

	// Release the target claim for real, as in the upgrade case.
	targetHold := uuid.NewSHA1(uuid.NameSpaceOID, fmt.Appendf(nil, "stub-hold:%s:%d", f.organizer, 1)).String()
	if !policy.claims.release(targetHold) {
		t.Fatal("the fixture could not release the target claim")
	}
	code, out := s.exchange(t, f, key)
	if code != http.StatusConflict {
		t.Fatalf("the retry answered %d %v, want 409", code, out)
	}

	svc := exchangeunwind.New(db, exchangeunwind.NewHTTPPayments(s.payments.server.URL, s.token, 5*time.Second))
	exchangeID := commercestore.ExchangeID(f.organizer, key)
	_, evidence, err := svc.Unwind(ctx, f.organizer, exchangeID, "operator tried to unwind a refunded downgrade")
	if err == nil {
		t.Fatal("the unwind DELETED the binding of a downgrade whose refund leg had already " +
			"settled. The money for this exchange lives in payments' refund-leg table, not in " +
			"payment_operations — an implementation that asks the operations endpoint sees 404 " +
			"and reads it as proof of safety")
	}
	if !errors.Is(err, commercestore.ErrExchangeMoneyMoved) {
		t.Errorf("err = %v, want ErrExchangeMoneyMoved", err)
	}
	if evidence != exchangeunwind.Present {
		t.Errorf("evidence = %v, want present", evidence)
	}

	// THE ENDPOINT ASSERTIONS, which are what distinguish this test from the upgrade one:
	// the refund-leg read happened and the operations read did NOT.
	if got := s.payments.count("refund-leg-reads"); got != 1 {
		t.Errorf("refund-leg reads = %d, want 1 — a negative delta's money is a refund leg", got)
	}
	if got := s.payments.count("operations-reads"); got != 0 {
		t.Errorf("operations reads = %d, want 0. Asking the operations endpoint about a "+
			"downgrade returns 404 for a key it never bound, which reads as absence", got)
	}
	if !s.exchangeRowExists(t, ctx, f.organizer, key) {
		t.Error("the order_exchanges row was deleted by a REFUSED unwind")
	}
	// No second refund leg was submitted while refusing: the unwind reads, never writes.
	if got := s.payments.count("refund-submissions"); got != 1 {
		t.Errorf("refund-leg submissions = %d, want 1", got)
	}
}

// The HANDLER writes the settlement-in-flight marker, at the right instant (TKT-255,
// ai-review pass 2 [medium]).
//
// The store tests prove the guard REFUSES a marked exchange. They cannot prove the shipped
// flow ever marks one: they call `MarkExchangeSettling` themselves, so deleting the
// production call from `completeExchangeFromBasis` leaves every one of them green. That is
// the mechanism-versus-wiring distinction AGENTS.md names, and this test is the wiring half —
// it drives the real HTTP handler and never touches the marker itself.
//
// THE INSTANT MATTERS AS MUCH AS THE FACT. The marker has to land after inventory's finalize
// SUCCEEDS and before the provider is called: earlier and it would veto unwinding a genuinely
// wedged exchange (finalize refuses, so the marker would be a lie), later and it would not be
// set during the window it exists to protect. Both halves are asserted:
//
//   - a request whose finalize REFUSES leaves the marker NULL — the wedged case, which must
//     stay unwindable;
//   - a request that gets past finalize has the marker set by the time it reaches the
//     provider, observed from the payments stub DURING the charge rather than afterwards,
//     because "it was set at the end" is also satisfied by marking after the money moved.
func TestTheHandlerMarksSettlingBetweenFinalizeAndTheProvider(t *testing.T) {
	db, ctx := exchangeAPIDB(t)

	t.Run("a refused finalize leaves the exchange unwindable", func(t *testing.T) {
		f := seedExchangeSource(t, db, ctx, "mark-wedged-src", 2, 1000)
		policy := &stubPolicy{}
		policy.catalogUnit.Store(1500)
		s := exchangeStackFor(t, db, f, policy)
		const key = "mark-wedged"

		targetHold := uuid.NewSHA1(uuid.NameSpaceOID, fmt.Appendf(nil, "stub-hold:%s:%d", f.organizer, 1)).String()
		if !policy.claims.release(targetHold) {
			t.Fatal("could not release the target claim")
		}
		if code, _ := s.exchange(t, f, key); code != http.StatusConflict {
			t.Fatalf("answered %d, want 409", code)
		}
		if at := settlingMarkerAt(t, s.db, ctx, f.organizer, key); at.Valid {
			t.Errorf("settling_at was set to %s for an exchange whose finalize REFUSED. A wedged "+
				"exchange never reaches the provider, and marking it would make the one state "+
				"this ticket exists to unwind permanently un-unwindable", at.Time)
		}
	})

	t.Run("the marker is set before the provider is called", func(t *testing.T) {
		f := seedExchangeSource(t, db, ctx, "mark-live-src", 2, 1000)
		policy := &stubPolicy{}
		policy.catalogUnit.Store(1500) // delta +1000
		s := exchangeStackFor(t, db, f, policy)
		const key = "mark-live"

		// Observed FROM INSIDE the charge. Asserting after the request returns would also be
		// satisfied by a handler that marked once the money had already moved, which is the
		// ordering the marker exists to rule out.
		var markedDuringCharge sql.NullTime
		var probed bool
		s.payments.onCharge = func() {
			probed = true
			markedDuringCharge = settlingMarkerAt(t, s.db, ctx, f.organizer, key)
		}

		if code, _ := s.exchange(t, f, key); code != http.StatusOK {
			t.Fatalf("answered %d, want 200", code)
		}
		if !probed {
			t.Fatal("the payments charge was never called, so this test observed nothing — the " +
				"fixture must reach the provider for the ordering assertion to mean anything")
		}
		if !markedDuringCharge.Valid {
			t.Error("settling_at was NULL while the provider was being called. The marker has to " +
				"be durable BEFORE the money moves, or an operator's unwind can delete the " +
				"binding out from under the charge — which is the race it was added to close")
		}
	})
}

// settlingMarkerAt reads the in-flight marker for an exchange addressed the way a test knows
// it: by organizer and idempotency key.
func settlingMarkerAt(t *testing.T, db *sql.DB, ctx context.Context, org uuid.UUID, key string) sql.NullTime {
	t.Helper()
	var at sql.NullTime
	if err := db.QueryRowContext(ctx,
		`SELECT settling_at FROM order_exchanges WHERE organizer_id=$1 AND id=$2`,
		org, commercestore.ExchangeID(org, key)).Scan(&at); err != nil {
		t.Fatalf("read settling marker: %v", err)
	}
	return at
}

// After an unwind, a retry under the SAME key binds a NEW exchange rather than resuming the
// deleted one — and that is the correct outcome, not a gap (TKT-255, ai-review pass 3).
//
// This test exists because a first version of it asserted the opposite and was wrong. It
// expected the retry to hit `MarkExchangeSettling`'s ErrExchangeUnwound hard stop. It does
// not: the resume branch is gated on `found && BasisRecorded && !Settled`
// (api/exchanges.go:142), and once the row is deleted `found` is FALSE. So the request falls
// through to the forward path and binds a fresh exchange, which is exactly what COS 1
// promises — the source order is free for a corrected attempt. The buyer ends up charged for
// a real, recorded exchange with its own row, replacement order and `order.exchanged` event.
//
// Worth pinning precisely because the shape looks alarming: a 200 and a charge after an
// unwind reads like the money-after-deletion defect until you check WHICH exchange was
// charged. The assertion is that a durable row exists for it.
//
// The narrow window `ErrExchangeUnwound` still guards is a single in-flight request whose row
// is deleted between its own read and its mark — reachable, not constructible through this
// HTTP surface, and covered at the store tier by
// TestMarkingSettlingReportsAnExchangeThatWasUnwound.
func TestAfterAnUnwindTheSameKeyBindsANewExchange(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "unwound-midflight-src", 2, 1000)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(1500) // delta +1000
	s := exchangeStackFor(t, db, f, policy)
	const key = "unwound-midflight"

	interruptAfterTheMoneyMoved(t, s, ctx, f, policy, key)
	exchangeID := commercestore.ExchangeID(f.organizer, key)

	// The interrupted attempt got past finalize, so it marked itself settling and the unwind
	// is refused until that marker ages out. Aged here rather than shortening the window,
	// which is derived from obs.ClientTimeout and must not be tuned for a test.
	if _, err := db.ExecContext(ctx,
		`UPDATE order_exchanges SET settling_at=now()-$2::interval WHERE organizer_id=$1 AND id=$3`,
		f.organizer, (commercestore.SettlingGraceWindow + time.Minute).String(), exchangeID); err != nil {
		t.Fatal(err)
	}
	if err := commercestore.UnwindWedgedExchange(ctx, db, f.organizer, exchangeID,
		"operator abandoned it", false); err != nil {
		t.Fatalf("unwind: %v", err)
	}
	if s.exchangeRowExists(t, ctx, f.organizer, key) {
		t.Fatal("the unwind did not delete the row, so the rest of this test proves nothing")
	}

	code, out := s.exchange(t, f, key)
	if code != http.StatusOK {
		t.Fatalf("the retry answered %d %v, want 200 — after an unwind the source order is free "+
			"and a fresh exchange under the same key is a NEW exchange, not a resume", code, out)
	}

	// THE POINT: whatever money moved, a durable row records what it was for. The failure
	// this guards against is a charge against a binding that no longer exists.
	settled, basis, _, _, _ := s.exchangeRow(t, ctx, f.organizer, key)
	if !settled || !basis {
		t.Errorf("settled=%t basis=%t, want both true — the retry charged the buyer, so there "+
			"must be a settled exchange row saying what that charge bought", settled, basis)
	}
	if replay, _ := out["replay"].(bool); replay {
		t.Error("the response reported replay=true for an exchange that was bound fresh; an " +
			"operator reconciling this would be told they are looking at a resume of the " +
			"exchange that was unwound")
	}
	// And the unwind evidence still stands beside it, so the history is legible: one exchange
	// abandoned, one bound afterwards under the same buyer-facing key.
	var unwinds int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM order_exchange_unwinds WHERE exchange_id=$1`, exchangeID).Scan(&unwinds); err != nil {
		t.Fatal(err)
	}
	if unwinds != 1 {
		t.Errorf("unwind evidence rows = %d, want 1", unwinds)
	}
}
