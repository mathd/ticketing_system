//go:build smoke

package smoke_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TKT-285: a zero-total checkout skips the PSP. These tests drive it through the composed stack
// — real catalog price, real inventory, real payments with its real journal, real recovery
// runner — because what they prove spans services. The durable tables prove that payments holds
// no operation, no settlement row and exactly the order's two journal facts for the order, and that
// access issued the tickets (a fact about a third service). They do NOT prove that no charge
// REQUEST was made: a refused charge leaves no row. That is proven at the api tier
// (zero_total_checkout_smoke_test.go in services/commerce/internal/api), whose payments stub
// records every request.

// zeroOrderJournal is what payments recorded for one order, re-read from its tables.
type zeroOrderJournal struct {
	facts      []journalFact
	operations int // payment_operations rows for the order
	settlement int // settlement_entries rows for the order
}

type journalFact struct {
	FactID      uuid.UUID
	OrganizerID uuid.UUID
	BuyerID     uuid.UUID
	FactType    string
	Amount      int64
	Currency    string
	OccurredAt  time.Time
}

func readZeroOrderJournal(t *testing.T, ctx context.Context, orderID string) zeroOrderJournal {
	t.Helper()
	pay, err := pgx.Connect(ctx, dsn("payments", "payments"))
	if err != nil {
		t.Fatalf("connect payments db: %v", err)
	}
	defer func() { _ = pay.Close(context.Background()) }()
	var out zeroOrderJournal
	// Every journal row about this order, whatever its type: a fabricated payment.* fact or a
	// zero-amount money fact must show up here, so nothing is filtered by type.
	rows, err := pay.Query(ctx, `SELECT fact_id, organizer_id, buyer_id, fact_type, amount, currency, occurred_at FROM journal_entries
		WHERE payload->>'order_id' = $1 ORDER BY sequence`, orderID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var f journalFact
		if err := rows.Scan(&f.FactID, &f.OrganizerID, &f.BuyerID, &f.FactType, &f.Amount, &f.Currency, &f.OccurredAt); err != nil {
			t.Fatal(err)
		}
		out.facts = append(out.facts, f)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := pay.QueryRow(ctx, `SELECT count(*) FROM payment_operations WHERE order_id=$1`, orderID).Scan(&out.operations); err != nil {
		t.Fatal(err)
	}
	if err := pay.QueryRow(ctx, `SELECT count(*) FROM settlement_entries WHERE order_id=$1`, orderID).Scan(&out.settlement); err != nil {
		t.Fatal(err)
	}
	return out
}

// assertZeroOrderJournal is COS4, re-read EXACTLY: two rows, in order, amount 0, EUR, with the
// timestamps commerce stored; no payment.* fact, no operation, no settlement row. A comparison
// against commerce's order_facts and not a non-null check, so a truncated or substituted
// timestamp fails.
func assertZeroOrderJournal(t *testing.T, ctx context.Context, orderID string) zeroOrderJournal {
	t.Helper()
	got := readZeroOrderJournal(t, ctx, orderID)
	if len(got.facts) != 2 || got.facts[0].FactType != "order.created" || got.facts[1].FactType != "order.completed" {
		t.Fatalf("payments journal for the order = %+v, want exactly [order.created order.completed]", got.facts)
	}
	com := commerceDB(t, ctx)
	for _, f := range got.facts {
		if f.Amount != 0 || f.Currency != "EUR" {
			t.Errorf("%s carries %d %s, want 0 EUR", f.FactType, f.Amount, f.Currency)
		}
		var stored time.Time
		var storedID, storedOrganizer, storedBuyer uuid.UUID
		if err := com.QueryRow(ctx, `SELECT occurred_at, fact_id, organizer_id, buyer_id FROM order_facts WHERE order_id=$1 AND fact_type=$2`,
			orderID, f.FactType).Scan(&stored, &storedID, &storedOrganizer, &storedBuyer); err != nil {
			t.Fatalf("commerce has no %s fact for the order: %v", f.FactType, err)
		}
		// Identity, not only content: the journal row must be THE fact commerce recorded, for the
		// same organizer and buyer. A fact submitted under a fresh id, or another buyer's, fails.
		if f.FactID != storedID || f.OrganizerID != storedOrganizer || f.BuyerID != storedBuyer {
			t.Errorf("%s journal row (fact %s organizer %s buyer %s) != commerce order_facts (fact %s organizer %s buyer %s)",
				f.FactType, f.FactID, f.OrganizerID, f.BuyerID, storedID, storedOrganizer, storedBuyer)
		}
		if !f.OccurredAt.Equal(stored) {
			t.Errorf("%s journal time %s != commerce order_facts time %s", f.FactType, f.OccurredAt, stored)
		}
	}
	if got.operations != 0 {
		t.Errorf("payments holds %d operation(s) for a zero-total order, want 0", got.operations)
	}
	if got.settlement != 0 {
		t.Errorf("payments holds %d settlement row(s) for a zero-total order, want 0", got.settlement)
	}
	return got
}

func reserveAt(t *testing.T, ticketType, key string, want int64) map[string]any {
	t.Helper()
	code, body := postWithKey(t, gatewayURL+"/api/commerce/reservations", key,
		map[string]any{"organizer_id": organizerID, "ticket_type_id": ticketType, "quantity": 2})
	if code != http.StatusCreated {
		t.Fatalf("reserve %d %s", code, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out["amount"] != float64(want) || out["currency"] != "EUR" {
		t.Fatalf("authoritative total = %v %v, want %d EUR", out["amount"], out["currency"], want)
	}
	return out
}

func waitForTickets(t *testing.T, guestRef string, want int) {
	t.Helper()
	retry(t, 30*time.Second, func() error {
		code, body, _ := getWithHeaders(t, gatewayURL+"/api/access/orders/"+guestRef+"/tickets")
		if code != http.StatusOK {
			return fmt.Errorf("ticket bundle %d %s", code, body)
		}
		var bundle struct {
			Tickets []struct{} `json:"tickets"`
		}
		if err := json.Unmarshal(body, &bundle); err != nil {
			return err
		}
		if len(bundle.Tickets) != want {
			return fmt.Errorf("issued tickets = %d, want %d", len(bundle.Tickets), want)
		}
		return nil
	})
}

// COS1 and COS4: a ticket priced at zero checks out end to end with NO payment operation, and
// payments' journal holds exactly the order's two facts.
func TestAZeroPricedTicketChecksOutWithoutTouchingThePSP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	slot, ticketType := setupCheckoutOfferAt(t, "zerototal", 0)
	reservation := reserveAt(t, ticketType, "zero-reserve-"+uuid.NewString(), 0)
	key := "zero-order-" + uuid.NewString()
	body := map[string]any{"reservation_id": reservation["reservation_id"], "name": "Free Buyer",
		"email": "free@example.test", "payment_token": "fake-ok"}

	code, raw := postWithKey(t, gatewayURL+"/api/commerce/orders", key, body)
	if code != http.StatusOK {
		t.Fatalf("zero-total checkout = %d %s, want 200 completed (it used to answer 202 payment_unknown)", code, raw)
	}
	var order struct {
		OrderID  string `json:"order_id"`
		GuestRef string `json:"guest_order_ref"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(raw, &order); err != nil || order.Status != "completed" || order.GuestRef == "" {
		t.Fatalf("zero-total checkout = %s (%v), want completed with a guest reference", raw, err)
	}

	// Tickets are issued from order.completed, consumed by access — the whole point of a comp.
	waitForTickets(t, order.GuestRef, 2)
	if _, _, confirmed, _ := staffAvailability(t, slot); confirmed != 2 {
		t.Errorf("confirmed seats = %d, want 2: the completion tail must confirm the claim", confirmed)
	}

	first := assertZeroOrderJournal(t, ctx, order.OrderID)

	// A byte-identical replay answers the same order and changes nothing: not a second
	// ticket set, not a second journal row, and still no operation.
	code, raw = postWithKey(t, gatewayURL+"/api/commerce/orders", key, body)
	var replay struct {
		OrderID  string `json:"order_id"`
		GuestRef string `json:"guest_order_ref"`
	}
	if err := json.Unmarshal(raw, &replay); err != nil || code != http.StatusOK ||
		replay.OrderID != order.OrderID || replay.GuestRef != order.GuestRef {
		t.Fatalf("replay = %d %s, want the same completed order", code, raw)
	}
	waitForTickets(t, order.GuestRef, 2)
	second := assertZeroOrderJournal(t, ctx, order.OrderID)
	if fmt.Sprint(first.facts) != fmt.Sprint(second.facts) {
		t.Errorf("a replay changed the journal: %+v then %+v", first.facts, second.facts)
	}
}

// D4 control. The discriminator is the persisted GROSS total. A ticket priced at zero whose
// only price is a fixed fee the buyer pays has a positive total and is CHARGED: keying the skip
// on the face value would complete it for free.
func TestAZeroFaceValueTicketWithAPassedOnFeeIsStillCharged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, ticketType := setupCheckoutOfferAt(t, "zeroface", 0)

	cat, err := pgx.Connect(ctx, dsn("catalog", "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cat.Close(ctx) }()
	var eventID string
	if err := cat.QueryRow(ctx, `SELECT p.event_id FROM ticket_types t
		JOIN performances p ON p.id = t.performance_id WHERE t.id = $1`, ticketType).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	// A public (channel-agnostic) fixed fee, passed on to the buyer: 300 per ticket.
	if _, err := cat.Exec(ctx, `INSERT INTO fee_rules
		(organizer_id, scope_level, scope_id, fee_code, basis, amount, currency, incidence, channel_code)
		VALUES($1,'event',$2,'service','per_ticket_fixed',300,'EUR','passed_on',NULL)`,
		organizerID, eventID); err != nil {
		t.Fatal(err)
	}

	reservation := reserveAt(t, ticketType, "zeroface-reserve-"+uuid.NewString(), 600)
	if reservation["face_value"] != float64(0) {
		t.Fatalf("face_value = %v, want 0: the fixture must be a zero FACE value with a positive total", reservation["face_value"])
	}
	key := "zeroface-order-" + uuid.NewString()
	code, raw := postWithKey(t, gatewayURL+"/api/commerce/orders", key,
		map[string]any{"reservation_id": reservation["reservation_id"], "name": "Fee Only Buyer",
			"email": "feeonly@example.test", "payment_token": "fake-ok"})
	if code != http.StatusOK {
		t.Fatalf("checkout = %d %s, want 200", code, raw)
	}
	var order struct {
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal(raw, &order); err != nil {
		t.Fatal(err)
	}
	got := readZeroOrderJournal(t, ctx, order.OrderID)
	if got.operations != 1 {
		t.Fatalf("payments holds %d operation(s), want 1: a positive total must be charged (facts: %+v)", got.operations, got.facts)
	}
	var captured int64
	for _, f := range got.facts {
		if f.FactType == "payment.captured" {
			captured = f.Amount
		}
	}
	if captured != 600 {
		t.Fatalf("captured %d, want the 600 the buyer owed (facts: %+v)", captured, got.facts)
	}
}

// COS4 for a RECOVERED order, in both windows D11 names. The checkout died after its intent
// fact — before the finalize in one case, after it in the other — and the real runner must
// COMPLETE the order, never release it. The stuck order is seeded directly (the crash cannot be
// staged deterministically through the stack; pattern of
// TestRecoveryRefundsCapturedMoneyWithGoneClaim) with buyer_pii, a live inventory claim and the
// order.created fact in commerce's order_facts.
//
// Payments' journal is asserted EMPTY for the order before recovery runs. That is what makes
// the test about recovery: the facts in the journal afterwards were submitted by the runner,
// because nothing else could have put them there.
func TestRecoveryCompletesAZeroTotalOrderAndTheJournalHoldsOnlyItsFacts(t *testing.T) {
	for _, window := range []struct {
		name     string
		finalize bool
	}{
		{"before the finalize", false},
		{"after the finalize", true},
	} {
		t.Run(window.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			slot, ticketType := setupCheckoutOfferAt(t, "zerorecov", 0)
			reservation := reserveAt(t, ticketType, "zero-recov-reserve-"+uuid.NewString(), 0)
			reservationID, holdID := fmt.Sprint(reservation["reservation_id"]), fmt.Sprint(reservation["hold_id"])
			buyerID := fmt.Sprint(reservation["buyer_id"])
			orderID := uuid.NewString()
			key := "zero-recov-order-" + uuid.NewString()

			if window.finalize {
				code, raw := internalJSON(t, http.MethodPost,
					fmt.Sprintf("%s/internal/holds/%s/finalize?organizer_id=%s", inventoryURL, holdID, organizerID), "", nil)
				if code != http.StatusOK {
					t.Fatalf("finalize the claim: %d %s", code, raw)
				}
			}
			status := "held"
			if window.finalize {
				status = "finalizing"
			}
			db := commerceDB(t, ctx)
			if _, err := db.Exec(ctx, `UPDATE reservations SET status=$2 WHERE id=$1`, reservationID, status); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(ctx, `INSERT INTO buyer_pii(buyer_id,name,email) VALUES($1,'Recovered Buyer','recovered@example.test')
				ON CONFLICT(buyer_id) DO UPDATE SET name=EXCLUDED.name,email=EXCLUDED.email`, buyerID); err != nil {
				t.Fatal(err)
			}
			// The intent fact checkout wrote before it died, under the deterministic id commerce derives.
			factID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(orderID+":order.created"))
			if _, err := db.Exec(ctx, `INSERT INTO order_facts(fact_id,order_id,organizer_id,buyer_id,fact_type,amount,currency)
				VALUES($1,$2,$3,$4,'order.created',0,'EUR')`, factID, orderID, organizerID, buyerID); err != nil {
				t.Fatalf("seed the intent fact: %v", err)
			}

			// BEFORE recovery: payments knows nothing about this order.
			before := readZeroOrderJournal(t, ctx, orderID)
			if len(before.facts) != 0 || before.operations != 0 || before.settlement != 0 {
				t.Fatalf("payments already holds %+v for the order before recovery ran", before)
			}

			// The commerce side died here: an order row, aged past the recovery grace period.
			if _, err := db.Exec(ctx, `INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,updated_at)
				VALUES($1,$2,'created',$3,'smoke-crash-fp',now()-interval '10 minutes')`,
				orderID, reservationID, key); err != nil {
				t.Fatalf("stage the crashed order: %v", err)
			}

			retry(t, 60*time.Second, func() error {
				var got string
				if err := db.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1`, orderID).Scan(&got); err != nil {
					return err
				}
				switch got {
				case "declined", "timeout", "refunded":
					t.Fatalf("order status = %q: recovery RELEASED a zero-total comp instead of completing it", got)
				}
				if got != "completed" {
					return fmt.Errorf("order status = %q, want completed", got)
				}
				return nil
			})

			assertZeroOrderJournal(t, ctx, orderID)
			var guestRef string
			if err := db.QueryRow(ctx, `SELECT guest_order_ref::text FROM orders WHERE id=$1`, orderID).Scan(&guestRef); err != nil {
				t.Fatal(err)
			}
			waitForTickets(t, guestRef, 2)
			if _, _, confirmed, _ := staffAvailability(t, slot); confirmed != 2 {
				t.Errorf("confirmed seats = %d, want 2", confirmed)
			}
		})
	}
}
