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

// TKT-115 (ADR-032 Slice 3): commerce recovery drains the states TKT-114's endpoints
// exist for, against the composed stack — real PostgreSQL (commerce migration 0005),
// the real recovery runner (RECOVERY_INTERVAL=2s in the smoke override), real payments,
// fake PSP. The fake-auth-hold token is the offline crashed-provider-flow testbed: the
// charge 500s, the operation stays bound-unresolved, and the runner must resolve it via
// status → void without a human.

func commerceDB(t *testing.T, ctx context.Context) *pgx.Conn {
	t.Helper()
	db, err := pgx.Connect(ctx, fmt.Sprintf("postgres://commerce:commerce@%s/commerce", pgHostPort))
	if err != nil {
		t.Fatalf("connect commerce db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close(context.Background()) })
	return db
}

// A payment_unknown order (auth-hold charge died mid-protocol) is resolved end to end:
// the runner asks PSP status (authorized), voids the hold, records `no_side_effect`,
// releases the seat, and the order presents to the buyer as a timed-out checkout while
// the audit column keeps the real resolution (COS1).
func TestRecoveryDrainsPaymentUnknownViaStatusAndVoid(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, ticketType := setupCheckoutOffer(t, "psprecov")
	reservation := reserveCheckout(t, ticketType, "psp-recov-reserve-"+uuid.NewString())

	code, body := postWithKey(t, gatewayURL+"/api/commerce/orders", "psp-recov-order-"+uuid.NewString(),
		map[string]any{"reservation_id": reservation["reservation_id"], "name": "Hold Buyer",
			"email": "hold@example.test", "payment_token": "fake-auth-hold"})
	if code != 202 {
		t.Fatalf("auth-hold checkout = %d %s, want 202 (recovery pending)", code, body)
	}
	var pending struct {
		OrderID string `json:"order_id"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(body, &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Status != "payment_unknown" {
		t.Fatalf("checkout status = %q, want payment_unknown", pending.Status)
	}

	// Age the order past the in-flight grace period so the 2s-interval runner claims it
	// now instead of two minutes from now. The runner's own predicates stay untouched.
	db := commerceDB(t, ctx)
	tag, err := db.Exec(ctx, `UPDATE orders SET updated_at=now()-interval '10 minutes',
		recovery_next_attempt_at=now() WHERE id=$1`, pending.OrderID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("age order: %v (rows=%d)", err, tag.RowsAffected())
	}

	retry(t, 45*time.Second, func() error {
		code, body, _ := getWithHeaders(t, gatewayURL+"/api/commerce/orders/"+pending.OrderID)
		if code != http.StatusOK {
			return fmt.Errorf("order state = %d %s", code, body)
		}
		var state struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(body, &state); err != nil {
			return err
		}
		if state.Status != "timeout" {
			return fmt.Errorf("order status = %q, want timeout (recovery not done)", state.Status)
		}
		return nil
	})

	// The audit column keeps the PSP-status-proven resolution distinct (G1/COS1) …
	var outcome string
	var idempotencyKey string
	var organizerID string
	if err := db.QueryRow(ctx, `SELECT o.terminal_outcome, o.idempotency_key, r.organizer_id::text
		FROM orders o JOIN reservations r ON r.id=o.reservation_id WHERE o.id=$1`,
		pending.OrderID).Scan(&outcome, &idempotencyKey, &organizerID); err != nil {
		t.Fatal(err)
	}
	if outcome != "no_side_effect" {
		t.Fatalf("terminal_outcome = %q, want no_side_effect", outcome)
	}
	// … the seat obligation is discharged …
	var resStatus string
	if err := db.QueryRow(ctx, `SELECT r.status FROM reservations r JOIN orders o ON o.reservation_id=r.id
		WHERE o.id=$1`, pending.OrderID).Scan(&resStatus); err != nil {
		t.Fatal(err)
	}
	if resStatus != "failed" {
		t.Fatalf("reservation status = %q, want failed", resStatus)
	}
	// … and payments' durable evidence shows the completed void superseding the stale
	// authorized state (payment.voided was appended by payments, not commerce).
	statusURL := paymentsURL + "/internal/psp/status?organizer_id=" + organizerID + "&idempotency_key=" + idempotencyKey
	code, body = internalJSON(t, http.MethodGet, statusURL, "", nil)
	if code != http.StatusOK {
		t.Fatalf("psp status = %d: %s", code, body)
	}
	var st struct {
		Outcome              string `json:"outcome"`
		TerminalNoSideEffect bool   `json:"terminal_no_side_effect"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	if st.Outcome != "voided" || !st.TerminalNoSideEffect {
		t.Fatalf("psp status = %+v, want voided/terminal-no-side-effect", st)
	}
}

// COS2 composed (ai-review B7): captured money whose inventory claim is gone is
// REFUNDED by the real runner through the real payments compensation surface — not a
// fabricated status flip. The mid-protocol crash (commerce died between capture and
// confirm) is staged by construction: a real reservation + a real captured payments
// operation under the order's idempotency key + a fabricated confirmation_pending order
// row + an expired hold. The runner must confirm → ErrClaimGone → queue → status →
// refund → payment.refunded (payments-owned) → order.failed → refunded.
func TestRecoveryRefundsCapturedMoneyWithGoneClaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, ticketType := setupCheckoutOffer(t, "psprefund")
	reservation := reserveCheckout(t, ticketType, "psp-refund-reserve-"+uuid.NewString())
	orderID := uuid.NewString()
	chargeKey := "psp-refund-order-" + uuid.NewString()

	// The money side really happened: a captured operation with durable evidence.
	code, body := internalJSON(t, http.MethodPost, paymentsURL+"/internal/charges", chargeKey,
		map[string]any{"order_id": orderID, "organizer_id": organizerID,
			"buyer_id": reservation["buyer_id"], "amount": 2500, "currency": "EUR",
			"payment_token": "fake-ok"})
	if code != http.StatusOK {
		t.Fatalf("charge = %d: %s", code, body)
	}

	// The commerce side died after the capture 200, before confirm: exactly the row
	// state the crash leaves behind (aged past the grace period).
	db := commerceDB(t, ctx)
	if _, err := db.Exec(ctx, `INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,updated_at)
		VALUES($1,$2,'confirmation_pending',$3,'smoke-crash-fp',now()-interval '10 minutes')`,
		orderID, reservation["reservation_id"], chargeKey); err != nil {
		t.Fatalf("stage crashed order: %v", err)
	}
	// The seat is gone: the hold expired while the order was stuck.
	expireInventoryHold(t, fmt.Sprint(reservation["hold_id"]))

	retry(t, 45*time.Second, func() error {
		code, body, _ := getWithHeaders(t, gatewayURL+"/api/commerce/orders/"+orderID)
		if code != http.StatusOK {
			return fmt.Errorf("order state = %d %s", code, body)
		}
		var state struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(body, &state); err != nil {
			return err
		}
		if state.Status != "refunded" {
			return fmt.Errorf("order status = %q, want refunded", state.Status)
		}
		return nil
	})

	// Payments owns the compensating fact: its evidence now answers refunded.
	statusURL := paymentsURL + "/internal/psp/status?organizer_id=" + organizerID + "&idempotency_key=" + chargeKey
	code, body = internalJSON(t, http.MethodGet, statusURL, "", nil)
	if code != http.StatusOK {
		t.Fatalf("psp status = %d: %s", code, body)
	}
	var st struct {
		Outcome              string `json:"outcome"`
		TerminalNoSideEffect bool   `json:"terminal_no_side_effect"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	if st.Outcome != "refunded" || st.TerminalNoSideEffect {
		t.Fatalf("psp status = %+v, want refunded and NOT terminal-no-side-effect", st)
	}
	// The seat obligation is discharged, never sold.
	var resStatus string
	if err := db.QueryRow(ctx, `SELECT status FROM reservations WHERE id=$1`,
		reservation["reservation_id"]).Scan(&resStatus); err != nil {
		t.Fatal(err)
	}
	if resStatus != "failed" {
		t.Fatalf("reservation status = %q, want failed", resStatus)
	}
}

// COS3: past the provider's idempotency-key retention (injected as 24h for the fake via
// PAYMENTS_STATUS_REPLAY_RETENTION), a ref-less unresolved operation's status answers
// 409 — never a replay that could mint a second PaymentIntent — and the operation
// lookup exposes the deadline commerce's pre-check reads.
func TestStatusReplayWindowExpiryAnswers409(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	organizer := uuid.NewString()
	chargeKey := "psp-expiry-smoke-" + uuid.NewString()

	// An auth-hold charge 500s and leaves the operation bound-unresolved, ref-less.
	code, body := internalJSON(t, http.MethodPost, paymentsURL+"/internal/charges", chargeKey,
		map[string]any{"order_id": uuid.NewString(), "organizer_id": organizer, "buyer_id": uuid.NewString(),
			"amount": 1250, "currency": "EUR", "payment_token": "fake-auth-hold"})
	if code != http.StatusInternalServerError {
		t.Fatalf("auth-hold charge = %d %s, want 500 (bound, unresolved)", code, body)
	}

	// Within the window: the lookup exposes the deadline, and status resolves.
	lookupURL := paymentsURL + "/internal/operations?organizer_id=" + organizer + "&idempotency_key=" + chargeKey
	code, body = internalJSON(t, http.MethodGet, lookupURL, "", nil)
	if code != http.StatusOK {
		t.Fatalf("lookup = %d: %s", code, body)
	}
	var op struct {
		Resolved               bool       `json:"resolved"`
		OccurredAt             *time.Time `json:"occurred_at"`
		StatusReplayDeadlineAt *time.Time `json:"status_replay_deadline_at"`
	}
	if err := json.Unmarshal(body, &op); err != nil {
		t.Fatal(err)
	}
	if op.Resolved || op.OccurredAt == nil || op.StatusReplayDeadlineAt == nil {
		t.Fatalf("unresolved ref-less operation must expose occurred_at and the replay deadline: %s", body)
	}

	// Expire it: backdate the durable bind time past the 24h retention.
	db, err := pgx.Connect(ctx, fmt.Sprintf("postgres://payments:payments@%s/payments", pgHostPort))
	if err != nil {
		t.Fatalf("connect payments db: %v", err)
	}
	defer func() { _ = db.Close(context.Background()) }()
	tag, err := db.Exec(ctx, `UPDATE payment_operations SET occurred_at=now()-interval '25 hours'
		WHERE organizer_id=$1 AND idempotency_key=$2`, organizer, chargeKey)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("backdate operation: %v (rows=%d)", err, tag.RowsAffected())
	}

	statusURL := paymentsURL + "/internal/psp/status?organizer_id=" + organizer + "&idempotency_key=" + chargeKey
	code, body = internalJSON(t, http.MethodGet, statusURL, "", nil)
	if code != http.StatusConflict {
		t.Fatalf("expired status = %d %s, want 409", code, body)
	}
}

// F3/R4 (plan-final): a byte-identical checkout replay against an order recovery has
// already resolved answers the terminal truth — it must never fall through to
// re-journal order.created and finalize against a hold recovery released.
func TestCheckoutReplayAgainstRecoveredOrders(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, ticketType := setupCheckoutOffer(t, "pspreplay")
	reservation := reserveCheckout(t, ticketType, "psp-replay-reserve-"+uuid.NewString())
	checkoutKey := "psp-replay-order-" + uuid.NewString()
	checkoutBody := map[string]any{"reservation_id": reservation["reservation_id"], "name": "Replay Buyer",
		"email": "replay@example.test", "payment_token": "fake-decline"}

	code, body := postWithKey(t, gatewayURL+"/api/commerce/orders", checkoutKey, checkoutBody)
	if code != 402 {
		t.Fatalf("declined checkout = %d %s, want 402", code, body)
	}
	var declined struct {
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal(body, &declined); err != nil {
		t.Fatal(err)
	}

	// Recovery later refunded this order (fabricated directly: manufacturing a real
	// captured-then-claim-gone order needs a mid-protocol crash the composed stack
	// cannot stage deterministically; the runner path is pinned by unit + store tests).
	db := commerceDB(t, ctx)
	if _, err := db.Exec(ctx, `UPDATE orders SET status='refunded', terminal_outcome=NULL WHERE id=$1`, declined.OrderID); err != nil {
		t.Fatalf("fabricate refunded order: %v", err)
	}
	code, body = postWithKey(t, gatewayURL+"/api/commerce/orders", checkoutKey, checkoutBody)
	if code != 402 {
		t.Fatalf("replay against refunded = %d %s, want 402", code, body)
	}
	var replay struct {
		Status string `json:"status"`
		Replay bool   `json:"replay"`
	}
	if err := json.Unmarshal(body, &replay); err != nil {
		t.Fatal(err)
	}
	if replay.Status != "refunded" || !replay.Replay {
		t.Fatalf("replay = %+v, want status=refunded replay=true", replay)
	}

	// Mid-compensation, the same replay is a conflict, not a re-drive.
	if _, err := db.Exec(ctx, `UPDATE orders SET status='reconciliation_required' WHERE id=$1`, declined.OrderID); err != nil {
		t.Fatalf("fabricate reconciliation_required order: %v", err)
	}
	code, body = postWithKey(t, gatewayURL+"/api/commerce/orders", checkoutKey, checkoutBody)
	if code != 409 {
		t.Fatalf("replay against reconciliation_required = %d %s, want 409", code, body)
	}
}
