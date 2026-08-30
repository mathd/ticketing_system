//go:build smoke

package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TKT-301: the payment token is OPAQUE to commerce.
//
// Payments' port declares it so — "for Stripe it is a PaymentMethod/PaymentIntent
// reference" (psp/port.go:68-70) — and commerce's own contract declares it so:
// `payment_token: {type: string, minLength: 1}`. But commerce's handler judged it against
// `fakepsp.ValidToken`, the four-token vocabulary of the LOCAL SIMULATOR, so no real
// provider's token could survive checkout. ADR-032's provider neutrality was foreclosed by
// a different service, through `shared/`.
//
// THE ASSERTION IS THAT PAYMENTS WAS ASKED, not that the request was refused. Both the old
// and the new behaviour can answer 400 for a bad token; what distinguishes them is WHO
// decided. A test asserting only the status cannot tell "payments rejected it" from
// "commerce rejected it locally" — which is the entire subject of this ticket — and would
// pass with the local check restored.
func TestCheckoutTreatsThePaymentTokenAsOpaque(t *testing.T) {
	// A token no fake vocabulary contains and that looks like a real provider reference.
	// If commerce judges tokens at all, this one never reaches payments.
	const stripeLike = "pm_1QcXyZ2eZvKYlo2CkQhF9xYz"

	db, ctx := exchangeAPIDB(t)
	_, reservation := seedCheckoutable(t, db, ctx)

	var chargedToken string
	payments := newCountingStub(t, func(c *countingStub, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Scoped to the charge endpoint. The payments base URL also serves the evidence
		// reads commerce makes around a checkout, and counting those would make the
		// assertion below about traffic volume rather than about the charge.
		if !strings.HasSuffix(r.URL.Path, "/internal/charges") {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		c.hit("charges")
		chargedToken = bodyField(r, "payment_token")
		_, _ = w.Write([]byte(`{"status":"captured","fact_id":"` + uuid.NewString() + `"}`))
	})
	inventory := newCountingStub(t, func(c *countingStub, w http.ResponseWriter, r *http.Request) {
		c.hit("inventory")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})

	srv := New(db, http.DefaultClient, "", inventory.server.URL, payments.server.URL, "tok")
	r := chi.NewRouter()
	r.Post("/reservations/{id}/checkout", srv.checkout)

	body := fmt.Sprintf(`{"reservation_id":%q,"name":"Opaque Buyer","email":"opaque@example.test","payment_token":%q}`,
		reservation, stripeLike)
	req := httptest.NewRequest(http.MethodPost, "/reservations/"+reservation.String()+"/checkout",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "opaque-"+uuid.NewString())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// The load-bearing assertion. Restoring commerce's local fakepsp check drops this to 0
	// while the response stays a 400 — which is why the count, not the status, is what this
	// test is about.
	if got := payments.count("charges"); got != 1 {
		t.Fatalf("payments received %d charge requests, want 1: commerce must forward an opaque "+
			"token and let payments judge it. Response was %d %s", got, rec.Code, rec.Body.String())
	}
	// And it must arrive UNCHANGED — forwarding a token commerce rewrote would be the same
	// defect wearing a different hat.
	if chargedToken != stripeLike {
		t.Fatalf("payments received token %q, want %q verbatim", chargedToken, stripeLike)
	}
}

// The complement, and it is what stops the test above from being satisfied by deleting all
// validation: commerce still refuses a token it can judge WITHOUT knowing any provider's
// vocabulary. Empty is not a provider question.
//
// Asserted at the same seam, by the same measure: payments is never asked.
func TestCheckoutStillRefusesAnEmptyPaymentTokenLocally(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	_, reservation := seedCheckoutable(t, db, ctx)

	payments := newCountingStub(t, func(c *countingStub, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/internal/charges") {
			c.hit("charges")
		}
		_, _ = w.Write([]byte(`{"status":"captured"}`))
	})

	srv := New(db, http.DefaultClient, "", "", payments.server.URL, "tok")
	r := chi.NewRouter()
	r.Post("/reservations/{id}/checkout", srv.checkout)

	body := fmt.Sprintf(`{"reservation_id":%q,"name":"Empty Buyer","email":"empty@example.test","payment_token":""}`,
		reservation)
	req := httptest.NewRequest(http.MethodPost, "/reservations/"+reservation.String()+"/checkout",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "empty-"+uuid.NewString())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an empty token: presence is not a provider question",
			rec.Code)
	}
	if got := payments.count("charges"); got != 0 {
		t.Fatalf("payments was asked %d times about an empty token, want 0", got)
	}
}

// seedCheckoutable inserts a reservation checkout can actually claim: HELD, with no order
// against it. seedExchangeSource deliberately builds a COMPLETED reservation with an order,
// which checkout answers 409 for — correct behaviour, and the wrong fixture for this test.
func seedCheckoutable(t *testing.T, db *sql.DB, ctx context.Context) (organizer, reservation uuid.UUID) {
	t.Helper()
	organizer, reservation = uuid.New(), uuid.New()
	const unit, quantity = 2500, 1
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,
		                         unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,'EUR','held')`,
		reservation, organizer, uuid.New(), uuid.New(), uuid.New(), uuid.New(),
		quantity, int64(unit), int64(unit*quantity)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM order_facts WHERE organizer_id=$1`, organizer)
		_, _ = db.Exec(`DELETE FROM completion_outbox WHERE order_id IN
			(SELECT o.id FROM orders o JOIN reservations r ON r.id = o.reservation_id
			 WHERE r.organizer_id=$1)`, organizer)
		_, _ = db.Exec(`DELETE FROM orders WHERE reservation_id IN
			(SELECT id FROM reservations WHERE organizer_id=$1)`, organizer)
		_, _ = db.Exec(`DELETE FROM reservations WHERE organizer_id=$1`, organizer)
	})
	return organizer, reservation
}

// TKT-301, the other half: an exchange UPGRADE has no payment instrument, so it is refused
// rather than charged against a token commerce invented.
//
// `settleExchangeDelta`'s upgrade arm submitted `"payment_token": "fake-ok"` — the local
// simulator's approve token. The charge therefore succeeded by construction of the fake and
// **no buyer instrument was ever collected for an upgrade**. Against a real provider that
// literal is not a token at all, so upgrades would silently stop charging anyone. The
// decision was recorded in no ADR; ADR-069 records it now.
//
// What this asserts, and why each part is here:
//
//   - the refusal is 409, NOT the 502 every other settlement failure answers. A permanent
//     refusal answered as "unresolved" invites a retry loop against something that can
//     never succeed, and 502 is what the surrounding code returns for genuinely transient
//     provider trouble.
//   - payments is never asked. A refusal that still sent the charge would be the defect
//     with an error message on top.
//
// Downgrades and equal exchanges are unaffected and are covered by the existing suite; only
// the arm that needs an instrument is refused.
func TestAnExchangeUpgradeIsRefusedRatherThanChargedAgainstAFakeToken(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "upgrade-refused-"+uuid.NewString(), 2, 1000)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(1500) // target 2 × 1500 = 3000 against a 2000 source: delta +1000
	s := exchangeStackFor(t, db, f, policy)

	// Posted WITHOUT payment_token — the shared helper supplies one, and the whole point
	// here is its absence.
	reqBody := fmt.Sprintf(`{"organizer_id":%q,"target_ticket_type_id":%q,
		"actor":"support@example.test","reason":"wrong ticket type"}`, f.organizer, f.targetType)
	req := httptest.NewRequest(http.MethodPost, "/internal/orders/"+f.order.String()+"/exchanges",
		strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", s.token)
	req.Header.Set("Idempotency-Key", "upgrade-refused-1")
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	code, body := rec.Code, rec.Body.String()

	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: an upgrade needs an instrument nobody collected, which "+
			"is a permanent refusal — 502 would say 'unresolved' and invite a retry that can "+
			"never succeed. body=%s", code, body)
	}
	if got := s.payments.count("charge-submissions"); got != 0 {
		t.Fatalf("payments received %d charge submissions, want 0: a refused upgrade must not "+
			"reach the provider at all", got)
	}

	// AND IT LEAVES NOTHING BEHIND. This is the half a status-only assertion misses, and it
	// was a real defect: refusing at settlement alone meant the target hold was taken and
	// finalized, the exchange row bound, the basis recorded and `settling_at` set before
	// the 409 — so the source order was blocked from another exchange or refund, inside
	// ADR-067's settling grace window, by a request that was always going to be refused.
	// A refusal that wedges the thing it refuses is not a refusal.
	if n := countExchangeRows(t, ctx, db, f.organizer); n != 0 {
		t.Fatalf("order_exchanges rows = %d, want 0: a refused upgrade must bind nothing", n)
	}
}

func countExchangeRows(t *testing.T, ctx context.Context, db *sql.DB, org uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM order_exchanges WHERE organizer_id=$1`, org).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The complement, and it is what stops the refusal above from being satisfied by simply
// breaking upgrades: given an instrument, an upgrade charges it — and charges THAT token,
// not a literal commerce chose.
//
// The two together are the rule: an upgrade either collects an instrument or is refused,
// never silently charged against a fake. Asserting only the refusal would leave "upgrades
// no longer work at all" indistinguishable from the fix.
func TestAnExchangeUpgradeChargesTheSuppliedInstrument(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	f := seedExchangeSource(t, db, ctx, "upgrade-charged-"+uuid.NewString(), 2, 1000)
	policy := &stubPolicy{}
	policy.catalogUnit.Store(1500) // delta +1000, an upgrade
	s := exchangeStackFor(t, db, f, policy)

	code, _ := s.exchange(t, f, "upgrade-charged-1")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: an upgrade carrying an instrument must settle", code)
	}
	if got := s.payments.count("charge-submissions"); got != 1 {
		t.Fatalf("charge submissions = %d, want 1", got)
	}
	// The token payments received is the one the request supplied, verbatim. A commerce
	// that substituted its own would be the original defect with an extra step.
	if got := s.payments.lastChargeToken(); got != "pm_exchange_instrument" {
		t.Fatalf("payments was given token %q, want the request's own %q",
			got, "pm_exchange_instrument")
	}
}

// When payments refuses the token, the caller learns THAT — not that something unknown
// happened (TKT-301).
//
// This is the other half of moving the judge. Commerce used to answer its own 400 before
// payments was asked; now payments answers, and its provider-neutral 400 has to survive the
// trip back. Left to fall through it did not: a 400 matched no arm in the outcome switch,
// reached the confirm call, and the request came back 202 `payment_unknown` — an order
// parked for a recovery runner to retry, over a token that will be just as invalid every
// time. A caller error dressed as an operational one, filling a recovery queue with work
// that can never succeed.
//
// The reservation stays usable on purpose. A refused token is not a decline: nothing was
// submitted to a provider, so there is nothing to compensate, and the caller can retry with
// a token that works — which is exactly what the gateway smoke suite has always asserted.
func TestAPaymentsTokenRefusalIsAnswered400NotParkedForRecovery(t *testing.T) {
	db, ctx := exchangeAPIDB(t)
	_, reservation := seedCheckoutable(t, db, ctx)

	payments := newCountingStub(t, func(c *countingStub, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/internal/charges") {
			c.hit("charges")
			// Payments' own provider-neutral refusal, verbatim.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid payment token"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	inventory := newCountingStub(t, func(c *countingStub, w http.ResponseWriter, r *http.Request) {
		c.hit("inventory")
		if strings.Contains(r.URL.Path, "/release") {
			c.hit("release")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})

	srv := New(db, http.DefaultClient, "", inventory.server.URL, payments.server.URL, "tok")
	r := chi.NewRouter()
	r.Post("/reservations/{id}/checkout", srv.checkout)
	body := fmt.Sprintf(`{"reservation_id":%q,"name":"Bad Token","email":"bad@example.test","payment_token":"pm_not_a_real_one"}`,
		reservation)
	req := httptest.NewRequest(http.MethodPost, "/reservations/"+reservation.String()+"/checkout",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "bad-"+uuid.NewString())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: payments refused the token, which is a permanent caller "+
			"error — 202 payment_unknown would queue a recovery retry that can never succeed. body=%s",
			rec.Code, rec.Body.String())
	}
	// And payments really was the one that decided, which is the whole point of the ticket.
	if got := payments.count("charges"); got != 1 {
		t.Fatalf("payments received %d charge requests, want 1", got)
	}
	// The capacity comes back. A refusal that failed the order without releasing the hold
	// would strand the seats until the hold expired on its own.
	if got := inventory.count("release"); got != 1 {
		t.Fatalf("inventory releases = %d, want 1: a refused token must return the capacity", got)
	}
	// TERMINAL, like a decline. By this point the order is claimed under a fingerprint that
	// INCLUDES the token, the hold is finalized and order.created is journalled — so a
	// retry with a corrected token is a different fingerprint and is refused as a conflict.
	// Leaving the row live would strand a reservation nobody can complete, so the refusal
	// fails it and releases the capacity.
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM orders WHERE reservation_id=$1`, reservation).Scan(&status); err != nil {
		t.Fatal(err)
	}
	// NOT "declined", and not any other terminal outcome. Every value the column admits
	// would be a lie here: `declined`/`timeout` are provider answers and no provider saw
	// this token; `not_attempted` means payments bound no charge, and payments binds before
	// it validates. Recording a decline would also make the idempotent REPLAY answer 402 to
	// a request that first answered 400.
	if status == "declined" || status == "timeout" {
		t.Fatalf("order status = %q: a token the provider never saw must not be recorded as a "+
			"provider decision — the replay would then answer 402 to a request that answered 400",
			status)
	}
	if status == "payment_unknown" {
		t.Fatalf("order status = %q: a refused token must not park the order for recovery to "+
			"retry something permanently invalid", status)
	}

	// AND THE REPLAY AGREES. One idempotent request must not answer 400 once and something
	// else the next time — recording a decline would have made the second answer 402, for a
	// provider decision that never happened.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/reservations/"+reservation.String()+"/checkout",
		strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Idempotency-Key", req.Header.Get("Idempotency-Key"))
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("replay status = %d, want 400 — the same request must get the same "+
			"classification. body=%s", rec2.Code, rec2.Body.String())
	}
}


// The token is OPTIONAL, and only an upgrade needs it (TKT-301, ADR-069).
//
// The shared exchange helper supplies one to every request, which is convenient and hides
// exactly one regression: a handler that required a token for ALL exchanges would satisfy
// the entire pre-existing suite while contradicting both the OpenAPI declaration (the field
// is optional) and this ADR. These two post WITHOUT one.
//
// A downgrade refunds against the ORIGINAL charge by its idempotency key and an equal
// exchange moves no money, so neither has anything to charge and neither may be refused for
// lacking an instrument.
func TestOnlyAnUpgradeNeedsAnInstrument(t *testing.T) {
	for _, tc := range []struct {
		name string
		unit int64 // source is 2 × 1000 = 2000
	}{
		{name: "a downgrade refunds against the original charge", unit: 600},
		{name: "an equal exchange moves no money", unit: 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := exchangeAPIDB(t)
			f := seedExchangeSource(t, db, ctx, "no-instrument-"+uuid.NewString(), 2, 1000)
			policy := &stubPolicy{}
			policy.catalogUnit.Store(tc.unit)
			s := exchangeStackFor(t, db, f, policy)

			reqBody := fmt.Sprintf(`{"organizer_id":%q,"target_ticket_type_id":%q,
				"actor":"support@example.test","reason":"wrong ticket type"}`, f.organizer, f.targetType)
			req := httptest.NewRequest(http.MethodPost, "/internal/orders/"+f.order.String()+"/exchanges",
				strings.NewReader(reqBody))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Internal-Token", s.token)
			req.Header.Set("Idempotency-Key", "no-instrument-1")
			rec := httptest.NewRecorder()
			s.handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: this exchange owes the buyer nothing, so it needs "+
					"no instrument. body=%s", rec.Code, rec.Body.String())
			}
			// And it never charged: the refund leg addresses the original payment, and an
			// equal exchange calls nobody.
			if got := s.payments.count("charge-submissions"); got != 0 {
				t.Fatalf("charge submissions = %d, want 0", got)
			}
			if n := countExchangeRows(t, ctx, db, f.organizer); n != 1 {
				t.Fatalf("order_exchanges rows = %d, want 1 — the exchange must have settled", n)
			}
		})
	}
}

