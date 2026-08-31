//go:build smoke

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// TKT-292. A parked order in a RESUMABLE status re-entered the money path.
//
// `checkout` read `recoveryParked` from claimOrder and consulted it in exactly one place:
// inside the `release_pending` branch. A parked `created`, `payment_unknown` or
// `confirmation_pending` order matched no replay branch and fell through to the
// orchestration — buyer PII, the `order.created` fact, the inventory finalize, and
// `POST /internal/charges`. ClaimStuckOrders excludes parked rows, so nothing would ever
// reconcile whatever that charge did.
//
// THE ASSERTION IS THE ABSENCE OF THE CALLS, NOT THE STATUS CODE. This is the ticket's own
// COS and it is not pedantry: a refusal written as an extension of the parked normalisation
// in answerRecovered would answer 409 too, and would answer it AFTER charging. A test that
// checked only `rec.Code == 409` would pass against exactly the defect this closes. So each
// parked case counts what inventory and payments received, and reads back the two rows the
// orchestration would have left behind.
//
// WHY THE THREE STATUSES ARE ONE TEST AND NOT ONE ASSERTION. The guard is position-based —
// it fires on anything that reaches the orchestration parked — so a single status going
// green proves the position, not the coverage. Each status gets its own subtest and its own
// fixture, and the mutation was re-run against each (see the ticket's coding comment).
//
// WHY THE UNPARKED CONTROLS ARE NOT DECORATION. A guard written as "refuse every order in
// these three statuses" satisfies every parked assertion below and destroys recovery: those
// three statuses are precisely the ones a normal checkout resumes through. The controls are
// the regression, and per AGENTS.md each seed must have an answer to "what goes red if I
// delete it" — delete the `recovery_parked_at=now()` from a parked case and its no-call
// assertions go red; delete a control and a whole-status refusal ships unnoticed.
//
// TIER. Only a DB-backed API test sees both halves: the precondition
// ("status IN the three AND recovery_parked_at set") exists only in the database, and what
// a caller is told exists only at the handler. The store tier has the row and no handler;
// the nil-db API tests have the handler and cannot construct the row.
//
// NOTE ON RUNNING IT: exchangeAPIDB skips when COMMERCE_API_TEST_DATABASE_URL is unset, so
// a plain `go test ./services/commerce/internal/api/` reports ok WITHOUT running this.

// parkableResumableStatuses are the statuses ClaimStuckOrders admits
// (store/recovery.go) that have NO earlier replay branch in checkout, and so reach the
// orchestration. `release_pending` and `reconciliation_required` are admitted too and are
// deliberately absent: both return from their own branches above the guard, and
// `release_pending`'s parked answer is pinned by
// TestParkedReleasePendingGetsTheSameAnswerFromBothPaths.
var parkableResumableStatuses = []string{"created", "payment_unknown", "confirmation_pending"}

func TestAParkedResumableOrderIsRefusedBeforeTheMoneyPath(t *testing.T) {
	for _, status := range parkableResumableStatuses {
		t.Run(status, func(t *testing.T) {
			db, ctx := exchangeAPIDB(t)
			organizer, reservation := seedCheckoutable(t, db, ctx)

			// Both stubs count EVERY request, not only the endpoint of interest. The
			// journal (`s.fact`) posts to payments /internal/facts and the orchestration
			// calls inventory /finalize then /confirm — a per-endpoint counter alone would
			// let a side effect through any path the test did not think to name.
			inventory := newCountingStub(t, func(c *countingStub, w http.ResponseWriter, r *http.Request) {
				c.hit("any")
				if strings.HasSuffix(r.URL.Path, "/finalize") {
					c.hit("finalize")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			})
			payments := newCountingStub(t, func(c *countingStub, w http.ResponseWriter, r *http.Request) {
				c.hit("any")
				if strings.HasSuffix(r.URL.Path, "/internal/charges") {
					c.hit("charges")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"captured","fact_id":"` + uuid.NewString() + `"}`))
			})

			srv := New(db, http.DefaultClient, "", inventory.server.URL, payments.server.URL, "tok")
			r := chi.NewRouter()
			r.Post("/reservations/{id}/checkout", srv.checkout)

			const name, email, token = "Parked Resumer", "parked-resume@example.test", "pm_parked_resume"
			key := "checkout-parked-" + status + "-" + uuid.NewString()

			// The order is minted by the REAL claimOrder on a first request rather than
			// hand-written, so its idempotency key, fingerprint and attribution columns are
			// whatever production writes. A hand-built row is how a fixture ends up
			// repairing its own precondition (AGENTS.md).
			//
			// That first request runs the orchestration against the stubs and completes.
			// It is setup, not the subject: the counters are read only after the SECOND
			// request, and `reset` below draws the line.
			mint := submitCheckout(t, r, reservation, name, email, token, key)
			if mint.Code != http.StatusOK {
				t.Fatalf("minting request did not complete: %d %s", mint.Code, mint.Body.String())
			}
			order := orderIDFor(t, db, ctx, reservation)

			// Now drive the row into the state under test: the resumable status, parked,
			// with the recovery lease released the way ReleaseStuckOrder leaves it after
			// exhausting its attempts.
			//
			// The reservation goes back to `finalizing`: a `completed` one makes checkout
			// answer 409 from a different branch entirely, which would make every
			// assertion below pass for the wrong reason.
			if _, err := db.ExecContext(ctx,
				`UPDATE reservations SET status='finalizing' WHERE id=$1`, reservation); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `
				UPDATE orders SET status=$2,
				    recovery_attempts=10, recovery_last_error='attempts exhausted',
				    recovery_claim_id=NULL, recovery_lease_until=NULL,
				    recovery_parked_at=now(),
				    updated_at=now()-interval '10 minutes'
				WHERE id=$1`, order, status); err != nil {
				t.Fatal(err)
			}
			// Clear the facts and PII the minting request legitimately wrote, so the
			// absence assertions below are about THIS request. Without this they would be
			// green-by-accident in reverse: permanently red, and the temptation would be to
			// weaken them to a delta.
			if _, err := db.ExecContext(ctx, `DELETE FROM order_facts WHERE order_id=$1`, order); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `DELETE FROM buyer_pii WHERE buyer_id=(SELECT buyer_id FROM reservations WHERE id=$1)`, reservation); err != nil {
				t.Fatal(err)
			}

			// Prove the fixture reached the state it claims BEFORE asserting anything about
			// it. A silently-failed UPDATE would make every no-call assertion below pass
			// because the request 409'd for an unrelated reason.
			assertOrderState(t, db, ctx, order, status, true)

			inventory.reset()
			payments.reset()

			// The request under test: byte-identical to the minting one, so claimOrder
			// returns the existing order rather than answering 409 on a fingerprint clash.
			rec := submitCheckout(t, r, reservation, name, email, token, key)

			// THE LOAD-BEARING ASSERTIONS. Counted at the wire, before the status code is
			// looked at, because a late refusal produces the same status code as an early
			// one and differs only here.
			if got := payments.count("any"); got != 0 {
				t.Fatalf("payments received %d requests (%d of them charges) for a PARKED %s order: "+
					"the refusal must precede the money path, not follow it. Response was %d %s",
					got, payments.count("charges"), status, rec.Code, rec.Body.String())
			}
			if got := inventory.count("any"); got != 0 {
				t.Fatalf("inventory received %d requests (%d of them finalize) for a PARKED %s order: "+
					"the refusal must precede the hold finalize. Response was %d %s",
					got, inventory.count("finalize"), status, rec.Code, rec.Body.String())
			}

			// And the two rows the orchestration would have left behind. These assert the
			// REQUIREMENT rather than the transport: `s.fact` happens to reach payments
			// today, so the counter above catches the journal — but only while it travels
			// that way. ADR-003 is the rule being kept: the journal records what happened,
			// and an aborted resume did not create an order.
			var facts int
			if err := db.QueryRowContext(ctx,
				`SELECT count(*) FROM order_facts WHERE order_id=$1 AND fact_type='order.created'`,
				order).Scan(&facts); err != nil {
				t.Fatal(err)
			}
			if facts != 0 {
				t.Fatalf("a refused parked %s order journalled %d order.created facts; ADR-003: "+
					"the journal records what happened", status, facts)
			}
			var pii int
			if err := db.QueryRowContext(ctx, `
				SELECT count(*) FROM buyer_pii
				WHERE buyer_id=(SELECT buyer_id FROM reservations WHERE id=$1)`,
				reservation).Scan(&pii); err != nil {
				t.Fatal(err)
			}
			if pii != 0 {
				t.Fatalf("a refused parked %s order wrote %d buyer_pii rows", status, pii)
			}

			// The answer, derived from what the state MEANS rather than from what the code
			// returns: a parked row is one no worker will advance, which is the same thing
			// reconciliation_required tells a buyer, so it gets the same answer. 202 here
			// would be a promise nothing can keep.
			if rec.Code != http.StatusConflict {
				t.Fatalf("a parked %s order must be answered 409, got %d: %s",
					status, rec.Code, rec.Body.String())
			}
			var answer map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
				t.Fatalf("response body is not JSON: %s", rec.Body.String())
			}
			if got := answer["error"]; got != "order awaiting payment reconciliation" {
				t.Fatalf("a parked %s order must tell the buyer a human must act, got %v: %s",
					status, got, rec.Body.String())
			}
			if got := answer["status"]; got != status {
				t.Fatalf("the answer must echo the durable status %q, got %v: %s",
					status, got, rec.Body.String())
			}
			if got, _ := answer["order_id"].(string); got != order.String() {
				t.Fatalf("the answer must name the order %s, got %v: %s",
					order, answer["order_id"], rec.Body.String())
			}

			// The refusal changed nothing. A guard that answered correctly and still
			// advanced the row would be a subtler version of the same defect.
			assertOrderState(t, db, ctx, order, status, true)
			_ = organizer
		})
	}
}

// The control, and it is the regression that matters. The three statuses above are exactly
// the ones a NORMAL checkout resumes through — an unparked `payment_unknown` order is a
// buyer retrying after a transport failure, and refusing it would strand every such retry
// while satisfying every assertion in the test above.
//
// Asserted at the same seam and by the same measure: the calls the parked case must not
// make are the calls this one must.
func TestAnUnparkedResumableOrderStillResumesCheckout(t *testing.T) {
	for _, status := range parkableResumableStatuses {
		t.Run(status, func(t *testing.T) {
			db, ctx := exchangeAPIDB(t)
			_, reservation := seedCheckoutable(t, db, ctx)

			inventory := newCountingStub(t, func(c *countingStub, w http.ResponseWriter, r *http.Request) {
				c.hit("any")
				switch {
				case strings.HasSuffix(r.URL.Path, "/finalize"):
					c.hit("finalize")
				case strings.HasSuffix(r.URL.Path, "/confirm"):
					c.hit("confirm")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			})
			payments := newCountingStub(t, func(c *countingStub, w http.ResponseWriter, r *http.Request) {
				c.hit("any")
				if strings.HasSuffix(r.URL.Path, "/internal/charges") {
					c.hit("charges")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"captured","fact_id":"` + uuid.NewString() + `"}`))
			})

			srv := New(db, http.DefaultClient, "", inventory.server.URL, payments.server.URL, "tok")
			r := chi.NewRouter()
			r.Post("/reservations/{id}/checkout", srv.checkout)

			const name, email, token = "Unparked Resumer", "unparked-resume@example.test", "pm_unparked_resume"
			key := "checkout-unparked-" + status + "-" + uuid.NewString()

			mint := submitCheckout(t, r, reservation, name, email, token, key)
			if mint.Code != http.StatusOK {
				t.Fatalf("minting request did not complete: %d %s", mint.Code, mint.Body.String())
			}
			order := orderIDFor(t, db, ctx, reservation)

			// The same fixture as the parked case in every respect BUT the marker. That is
			// deliberate: it is what makes this a control rather than a second, unrelated
			// test — the only difference between the two outcomes must be the one column
			// the guard reads.
			if _, err := db.ExecContext(ctx,
				`UPDATE reservations SET status='finalizing' WHERE id=$1`, reservation); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `
				UPDATE orders SET status=$2,
				    recovery_attempts=10, recovery_last_error='attempts exhausted',
				    recovery_claim_id=NULL, recovery_lease_until=NULL,
				    recovery_parked_at=NULL,
				    updated_at=now()-interval '10 minutes'
				WHERE id=$1`, order, status); err != nil {
				t.Fatal(err)
			}
			assertOrderState(t, db, ctx, order, status, false)

			inventory.reset()
			payments.reset()

			rec := submitCheckout(t, r, reservation, name, email, token, key)

			if rec.Code != http.StatusOK {
				t.Fatalf("an UNPARKED %s order must still resume to completion, got %d: %s",
					status, rec.Code, rec.Body.String())
			}
			// The calls are the point. A guard that returned 200 without resuming would
			// satisfy the status assertion and still have broken recovery.
			if got := payments.count("charges"); got != 1 {
				t.Fatalf("an UNPARKED %s order must reach the money path: payments saw %d charges",
					status, got)
			}
			if got := inventory.count("finalize"); got != 1 {
				t.Fatalf("an UNPARKED %s order must finalize its hold: inventory saw %d finalizes",
					status, got)
			}
			if got := inventory.count("confirm"); got != 1 {
				t.Fatalf("an UNPARKED %s order must confirm its hold: inventory saw %d confirms",
					status, got)
			}
			assertOrderState(t, db, ctx, order, "completed", false)
		})
	}
}

// submitCheckout submits one checkout through the real router. The idempotency key and the
// buyer fields are the caller's, so two calls with the same values are the same request as
// far as claimOrder's fingerprint is concerned — which is how a replay is expressed here.
func submitCheckout(t *testing.T, r chi.Router, reservation uuid.UUID, name, email, token, key string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"reservation_id":%q,"name":%q,"email":%q,"payment_token":%q}`,
		reservation, name, email, token)
	req := httptest.NewRequest(http.MethodPost,
		"/reservations/"+reservation.String()+"/checkout", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// orderIDFor returns the order the real claimOrder minted against this reservation. Read
// rather than computed: claimOrder derives the id from the organizer and the idempotency
// key (uuid.NewSHA1), and recomputing it here would make the test agree with a copy of the
// implementation instead of with the row.
func orderIDFor(t *testing.T, db *sql.DB, ctx context.Context, reservation uuid.UUID) uuid.UUID {
	t.Helper()
	var order uuid.UUID
	if err := db.QueryRowContext(ctx,
		`SELECT id FROM orders WHERE reservation_id=$1`, reservation).Scan(&order); err != nil {
		t.Fatalf("no order was minted for reservation %s: %v", reservation, err)
	}
	return order
}

// assertOrderState reads the row back and fails if it is not exactly the state the caller
// says it is. Used both to prove a fixture landed and to prove a refusal changed nothing.
func assertOrderState(t *testing.T, db *sql.DB, ctx context.Context, order uuid.UUID, wantStatus string, wantParked bool) {
	t.Helper()
	var status string
	var parked sql.NullTime
	if err := db.QueryRowContext(ctx,
		`SELECT status, recovery_parked_at FROM orders WHERE id=$1`, order).Scan(&status, &parked); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || parked.Valid != wantParked {
		t.Fatalf("order %s is status=%q parked=%v, want status=%q parked=%v",
			order, status, parked.Valid, wantStatus, wantParked)
	}
}
