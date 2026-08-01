//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Exchange settlement (TKT-158, ADR-039). An exchange is a reversal AND a sale, and the
// thing that makes it different from "a refund plus a checkout" is that exactly ONE net
// provider movement settles the difference while BOTH gross legs are journalled.
//
// This ticket stops at `switch_pending`: the delta is settled and the replacement is
// confirmed, and the buyer still holds VALID OLD TICKETS. Switching the entitlement is
// TKT-166. That state is the safe one — it under-sells, cannot oversell, and never leaves
// the buyer with nothing.

func exchangeRequest(c Completion, key string, target uuid.UUID) ExchangeRequest {
	return ExchangeRequest{
		SourceOrderID: c.OrderID, OrganizerID: c.OrganizerID, TargetTicketTypeID: target,
		IdempotencyKey: key, Actor: "support@example.test", Reason: "wrong ticket type",
	}
}

// The identity every downstream key derives from must be stable across a retry that lost
// its response — otherwise a replay settles the difference twice.
func TestExchangeIDIsDeterministic(t *testing.T) {
	org, other := uuid.New(), uuid.New()
	// Bound first: `f(x) != f(x)` inline reads as a tautology to staticcheck, and it has a
	// point — the assertion that matters is that two SEPARATE derivations agree, which is
	// what a retry actually performs.
	first := ExchangeID(org, "k")
	second := ExchangeID(org, "k")
	if first != second {
		t.Fatalf("exchange identity is not deterministic: %s vs %s", first, second)
	}
	if first == ExchangeID(org, "k2") {
		t.Fatal("distinct idempotency keys must not collide")
	}
	if first == ExchangeID(other, "k") {
		t.Fatal("distinct organizers must not collide")
	}
}

// Only a completed order can be exchanged, and only once. The vocabulary is walked so a
// new orders.status token cannot silently become exchangeable.
func TestBindOrderExchangeRejectsNonCompletedOrder(t *testing.T) {
	db, ctx := outboxDB(t)
	for _, status := range []string{
		"created", "payment_unknown", "confirmation_pending", "release_pending",
		"declined", "timeout", "reconciliation_required", "refunded",
	} {
		t.Run(status, func(t *testing.T) {
			c, _ := seedCompleted(t, db, ctx, "exch-status-"+status, 2, 1000)
			if _, err := db.ExecContext(ctx, `UPDATE orders SET status=$2 WHERE id=$1`, c.OrderID, status); err != nil {
				t.Fatal(err)
			}
			if _, err := BindOrderExchange(ctx, db, exchangeRequest(c, "x-1", uuid.New())); !errors.Is(err, ErrOrderNotExchangeable) {
				t.Fatalf("err = %v, want ErrOrderNotExchangeable", err)
			}
		})
	}
}

// An order already reversed by a refund cannot also be exchanged — it would be reversed
// twice. Both paths take the same order row lock; this pins that they actually contend
// (plan-review A5).
func TestExchangeAndRefundCannotBothReverseTheSameOrder(t *testing.T) {
	db, ctx := outboxDB(t)

	// Refund first, then exchange.
	c, _ := seedCompleted(t, db, ctx, "exch-after-refund", 2, 1000)
	if _, err := BindOrderRefund(ctx, db, RefundRequest{
		OrderID: c.OrderID, OrganizerID: c.OrganizerID, Quantity: 1,
		IdempotencyKey: "r-1", Actor: "a", Reason: "r",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := BindOrderExchange(ctx, db, exchangeRequest(c, "x-1", uuid.New())); !errors.Is(err, ErrOrderNotExchangeable) {
		t.Fatalf("err = %v, want ErrOrderNotExchangeable for a partly refunded order", err)
	}

	// Exchange first, then refund.
	c2, _ := seedCompleted(t, db, ctx, "refund-after-exch", 2, 1000)
	if _, err := BindOrderExchange(ctx, db, exchangeRequest(c2, "x-2", uuid.New())); err != nil {
		t.Fatal(err)
	}
	if _, err := BindOrderRefund(ctx, db, RefundRequest{
		OrderID: c2.OrderID, OrganizerID: c2.OrganizerID, Quantity: 1,
		IdempotencyKey: "r-2", Actor: "a", Reason: "r",
	}); !errors.Is(err, ErrOrderNotRefundable) {
		t.Fatalf("err = %v, want ErrOrderNotRefundable for an exchanged order", err)
	}
}

// AC8: the replay returns the same exchange identity and writes one row.
func TestBindOrderExchangeReplayReturnsTheSameExchange(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "exch-replay", 2, 1000)
	target := uuid.New()

	first, err := BindOrderExchange(ctx, db, exchangeRequest(c, "x-1", target))
	if err != nil {
		t.Fatal(err)
	}
	second, err := BindOrderExchange(ctx, db, exchangeRequest(c, "x-1", target))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || !first.CreatedAt.Equal(second.CreatedAt) {
		t.Fatalf("replay must return the same exchange: %+v vs %+v", first, second)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM order_exchanges WHERE source_order_id=$1`, c.OrderID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d, want 1", rows)
	}
	// Same key, different target: a conflict, not a silent replay of a different exchange.
	if _, err := BindOrderExchange(ctx, db, exchangeRequest(c, "x-1", uuid.New())); !errors.Is(err, ErrExchangeConflict) {
		t.Fatalf("err = %v, want ErrExchangeConflict", err)
	}
}

// AC1 + AC3: the delta is computed from persisted integer minor units, in both directions,
// and an equal-price exchange settles nothing. The source totals come from the reservation,
// never from a re-read of a mutable catalog row.
func TestExchangeDeltaIsSignedAndDerivedFromPersistedAmounts(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "exch-delta", 3, 1667) // total 5001

	ex, err := BindOrderExchange(ctx, db, exchangeRequest(c, "x-1", uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	if ex.SourceTotal != 5001 || ex.Quantity != 3 || ex.Currency != "EUR" {
		t.Fatalf("source line = %+v, want 3 × 1667 = 5001 EUR", ex)
	}
	for _, tc := range []struct {
		name      string
		newUnit   int64
		wantDelta int64
	}{
		{"upgrade", 2000, 999},    // 3×2000 = 6000, delta +999
		{"downgrade", 1000, -2001}, // 3×1000 = 3000, delta -2001
		{"equal", 1667, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExchangeDelta(ex.SourceTotal, tc.newUnit*int64(ex.Quantity)); got != tc.wantDelta {
				t.Fatalf("delta = %d, want %d", got, tc.wantDelta)
			}
		})
	}
}

// AC3: currencies must match. No FX inside an order (PRD / TKT-10), and the refusal must
// land before anything downstream is touched.
func TestSettleExchangeRefusesCurrencyMismatch(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "exch-currency", 2, 1000)
	ex, err := BindOrderExchange(ctx, db, exchangeRequest(c, "x-1", uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateExchangeTarget(ex, 2000, "USD"); !errors.Is(err, ErrExchangeCurrencyMismatch) {
		t.Fatalf("err = %v, want ErrExchangeCurrencyMismatch", err)
	}
	if err := ValidateExchangeTarget(ex, 2000, "EUR"); err != nil {
		t.Fatalf("matching currency must pass: %v", err)
	}
}

// AC4's settlement half: the exchange records its own progress, once. A replayed
// completion must not move it twice, and `switch_pending` must be distinguishable from
// done — the buyer still holds valid old tickets in that state.
func TestCompleteExchangeSettlementIsOnceOnly(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "exch-settle", 2, 1000)
	ex, err := BindOrderExchange(ctx, db, exchangeRequest(c, "x-1", uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	// A real order: replacement_order_id is a foreign key, and the exchange flow creates
	// the replacement before recording it.
	rep, _ := seedCompleted(t, db, ctx, "exch-settle-replacement", 2, 2000)
	replacement := rep.OrderID

	// Settlement cannot precede the basis — the constraint refuses it, which is what stops
	// money being recorded against numbers nobody committed to first.
	if err := CompleteExchangeSettlement(ctx, db, ex.OrganizerID, ex.ID, replacement); err != nil {
		t.Fatal(err)
	}
	if reloaded, _, err := lookupExchangeForTest(ctx, db, ex.OrganizerID, ex.ID); err != nil {
		t.Fatal(err)
	} else if reloaded.Settled {
		t.Fatal("settlement landed without a basis")
	}

	// source_total is 2000 (2 × 1000), so a target of 4000 makes the delta +2000 — and the
	// CHECK enforces exactly that relationship (ai-review F4).
	if recorded, err := RecordExchangeBasis(ctx, db, ex.OrganizerID, ex.ID, ExchangeBasis{
		TargetHoldID: uuid.New(), ReplacementReservationID: uuid.New(), TargetSlotID: uuid.New(),
		TargetTotal: 4000, DeltaAmount: 2000, TargetUnitAmount: 2000,
	}); err != nil || !recorded {
		t.Fatalf("record basis: %v recorded=%t", err, recorded)
	}
	if err := CompleteExchangeSettlement(ctx, db, ex.OrganizerID, ex.ID, replacement); err != nil {
		t.Fatal(err)
	}
	other, _ := seedCompleted(t, db, ctx, "exch-settle-other", 1, 1)
	if err := CompleteExchangeSettlement(ctx, db, ex.OrganizerID, ex.ID, other.OrderID); err != nil {
		t.Fatal(err)
	}

	var storedReplacement uuid.NullUUID
	var target, delta sql.NullInt64
	var settledAt sql.NullTime
	if err := db.QueryRowContext(ctx, `SELECT replacement_order_id,target_total,delta_amount,settled_at FROM order_exchanges WHERE id=$1`, ex.ID).
		Scan(&storedReplacement, &target, &delta, &settledAt); err != nil {
		t.Fatal(err)
	}
	if storedReplacement.UUID != replacement || target.Int64 != 4000 || delta.Int64 != 2000 || !settledAt.Valid {
		t.Fatalf("settlement must be once-only: replacement=%v target=%v delta=%v settled=%v",
			storedReplacement.UUID, target.Int64, delta.Int64, settledAt.Valid)
	}

	// And the source order is now visibly mid-exchange, which is what stops a refund.
	reloaded, err := BindOrderExchange(ctx, db, exchangeRequest(c, "x-1", ex.TargetTicketTypeID))
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Settled || reloaded.TicketsExchanged {
		t.Fatalf("exchange = settled %t / switched %t, want settled and NOT switched (switch_pending)",
			reloaded.Settled, reloaded.TicketsExchanged)
	}
}

// ADR-002.
func TestBindOrderExchangeIsOrganizerScoped(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "exch-scope", 2, 1000)
	in := exchangeRequest(c, "x-1", uuid.New())
	in.OrganizerID = uuid.New()
	if _, err := BindOrderExchange(ctx, db, in); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want the order to be invisible to another organizer", err)
	}
}


// lookupExchangeForTest reaches the unexported reader so a test can assert on stored state
// without exporting it for production callers that do not need it.
func lookupExchangeForTest(ctx context.Context, db *sql.DB, org, id uuid.UUID) (Exchange, bool, error) {
	s, found, err := lookupExchange(ctx, db, org, id)
	return s.exchange, found, err
}

// ai-review F4: the database refuses a settled row whose delta is not the difference. An
// application regression or a repair cannot persist an internally contradictory money
// record that the row still claims is settled.
func TestExchangeBasisRefusesAnInconsistentDelta(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "exch-bad-delta", 2, 1000) // source_total 2000
	ex, err := BindOrderExchange(ctx, db, exchangeRequest(c, "x-1", uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	// target 1000 against source 2000 is a delta of -1000; claiming +9000 must be refused.
	basis := ExchangeBasis{
		TargetHoldID: uuid.New(), ReplacementReservationID: uuid.New(), TargetSlotID: uuid.New(),
		TargetTotal: 1000, DeltaAmount: 9000, TargetUnitAmount: 500,
	}
	if _, err := RecordExchangeBasis(ctx, db, ex.OrganizerID, ex.ID, basis); err == nil {
		t.Fatal("the database accepted a delta that is not target - source")
	}
	// And the total must be the product, which is the other half of the same discipline.
	basis.DeltaAmount, basis.TargetUnitAmount = -1000, 999
	if _, err := RecordExchangeBasis(ctx, db, ex.OrganizerID, ex.ID, basis); err == nil {
		t.Fatal("the database accepted a total that is not quantity × unit")
	}
	basis.TargetUnitAmount = 500 // 2 × 500 = 1000 ✓, delta 1000 - 2000 = -1000 ✓
	if recorded, err := RecordExchangeBasis(ctx, db, ex.OrganizerID, ex.ID, basis); err != nil || !recorded {
		t.Fatalf("a consistent basis must be accepted: %v recorded=%t", err, recorded)
	}
	// Second writer: the row is taken, and it must be TOLD so rather than receiving nil and
	// continuing on a basis the money does not use (ai-review pass 3).
	if recorded, err := RecordExchangeBasis(ctx, db, ex.OrganizerID, ex.ID, basis); err != nil || recorded {
		t.Fatalf("a second basis write reported recorded=%t (err %v); it must report false", recorded, err)
	}
}

// ai-review F2: a REFUSED exchange must leave nothing behind. Eligibility is judged before
// anything durable is written, so a sold-out or seated or mistyped attempt cannot make the
// order permanently unreversible.
func TestLoadExchangeSourceWritesNothing(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "exch-readonly", 2, 1000)

	src, err := LoadExchangeSource(ctx, db, c.OrganizerID, c.OrderID)
	if err != nil {
		t.Fatal(err)
	}
	if src.Quantity != 2 || src.Total != 2000 || src.Currency != "EUR" || src.PaymentSourceKey != "exch-readonly" {
		t.Fatalf("source = %+v", src)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM order_exchanges WHERE source_order_id=$1`, c.OrderID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("a read-only eligibility load wrote %d exchange rows", rows)
	}
	// And the order is still refundable, which is the property a refused exchange used to
	// destroy.
	if _, err := BindOrderRefund(ctx, db, RefundRequest{
		OrderID: c.OrderID, OrganizerID: c.OrganizerID, Quantity: 1,
		IdempotencyKey: "r-after-refused-exchange", Actor: "a", Reason: "r",
	}); err != nil {
		t.Fatalf("an order whose exchange was refused must stay refundable: %v", err)
	}
}


// ai-review pass 3: an idempotency key names ONE request. A settled replay carrying a
// different order or target must conflict, not answer 200 with somebody else's exchange.
func TestLookupExchangeForRefusesADifferentRequestUnderTheSameKey(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "exch-fingerprint", 2, 1000)
	target := uuid.New()
	in := exchangeRequest(c, "x-1", target)

	if _, err := BindOrderExchange(ctx, db, in); err != nil {
		t.Fatal(err)
	}
	if got, found, err := LookupExchangeFor(ctx, db, in); err != nil || !found || got.TargetTicketTypeID != target {
		t.Fatalf("the same request must resolve: %v found=%t", err, found)
	}
	other := in
	other.TargetTicketTypeID = uuid.New()
	if _, _, err := LookupExchangeFor(ctx, db, other); !errors.Is(err, ErrExchangeConflict) {
		t.Fatalf("err = %v, want ErrExchangeConflict for a different target under the same key", err)
	}
	other = in
	other.Reason = "something else"
	if _, _, err := LookupExchangeFor(ctx, db, other); !errors.Is(err, ErrExchangeConflict) {
		t.Fatalf("err = %v, want ErrExchangeConflict for a different reason under the same key", err)
	}
}
