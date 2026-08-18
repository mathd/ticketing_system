//go:build smoke

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
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
		{"upgrade", 2000, 999},     // 3×2000 = 6000, delta +999
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
	// A real replacement order owes NO `order.completed` (ADR-039 §4) — persistExchangeReplacement
	// writes it without an outbox row, deliberately, so that settlement's own
	// `order.exchanged` row is the one owed event that issues its tickets (TKT-166).
	// seedCompleted is not representative on that point; `completion_outbox.order_id` is
	// UNIQUE, so leaving the seeded row would make this fixture, not the code, the conflict.
	if _, err := db.ExecContext(ctx, `DELETE FROM completion_outbox WHERE order_id=$1`, replacement); err != nil {
		t.Fatal(err)
	}

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
	if _, recorded, err := RecordExchangeBasis(ctx, db, ex.OrganizerID, ex.ID, ExchangeBasis{
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
	if _, _, err := RecordExchangeBasis(ctx, db, ex.OrganizerID, ex.ID, basis); err == nil {
		t.Fatal("the database accepted a delta that is not target - source")
	}
	// And the total must be the product, which is the other half of the same discipline.
	basis.DeltaAmount, basis.TargetUnitAmount = -1000, 999
	if _, _, err := RecordExchangeBasis(ctx, db, ex.OrganizerID, ex.ID, basis); err == nil {
		t.Fatal("the database accepted a total that is not quantity × unit")
	}
	basis.TargetUnitAmount = 500 // 2 × 500 = 1000 ✓, delta 1000 - 2000 = -1000 ✓
	if _, recorded, err := RecordExchangeBasis(ctx, db, ex.OrganizerID, ex.ID, basis); err != nil || !recorded {
		t.Fatalf("a consistent basis must be accepted: %v recorded=%t", err, recorded)
	}
	// Second writer: the row is taken, and it must be TOLD so rather than receiving nil and
	// continuing on a basis the money does not use (ai-review pass 3).
	if _, recorded, err := RecordExchangeBasis(ctx, db, ex.OrganizerID, ex.ID, basis); err != nil || recorded {
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

// TKT-166. A settled exchange must OWE the switch event, and must owe it in the same
// transaction that settles.
//
// This is ADR-016 §Decision 6's rule applied to a second subject. It matters more here
// than for `order.completed`: the replacement order deliberately owes no completion event
// (ADR-039 §4), so this row is the ONLY thing that will ever issue its tickets. A
// settlement that committed without it would leave the buyer paid-up, holding tickets to
// an event they exchanged away, with no retry able to notice.
func TestExchangeSettlementOwesTheSwitchEvent(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "exch-owes", 2, 1000)
	ex, err := BindOrderExchange(ctx, db, exchangeRequest(c, "owe-1", uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	rep, _ := seedCompleted(t, db, ctx, "exch-owes-replacement", 2, 2000)
	if _, err := db.ExecContext(ctx, `DELETE FROM completion_outbox WHERE order_id=$1`, rep.OrderID); err != nil {
		t.Fatal(err)
	}
	if _, recorded, err := RecordExchangeBasis(ctx, db, ex.OrganizerID, ex.ID, ExchangeBasis{
		TargetHoldID: uuid.New(), ReplacementReservationID: uuid.New(), TargetSlotID: uuid.New(),
		TargetTotal: 4000, DeltaAmount: 2000, TargetUnitAmount: 2000,
	}); err != nil || !recorded {
		t.Fatalf("record basis: %v recorded=%t", err, recorded)
	}
	if err := CompleteExchangeSettlement(ctx, db, ex.OrganizerID, ex.ID, rep.OrderID); err != nil {
		t.Fatal(err)
	}

	var subject string
	var envelope []byte
	var settledAt sql.NullTime
	if err := db.QueryRowContext(ctx, `
		SELECT x.subject, x.envelope, e.settled_at
		FROM completion_outbox x JOIN order_exchanges e ON e.replacement_order_id = x.order_id
		WHERE e.id=$1`, ex.ID).Scan(&subject, &envelope, &settledAt); err != nil {
		t.Fatalf("a settled exchange owes no switch event: %v", err)
	}
	if subject != "platform.commerce.order.exchanged" {
		t.Fatalf("subject = %q", subject)
	}

	// The frozen bytes describe the PERSISTED settlement, and occurred_at is the stored
	// settled_at — so a republish cannot produce different bytes under one deterministic id.
	var env struct {
		ID         uuid.UUID `json:"id"`
		Type       string    `json:"type"`
		Schema     int       `json:"schema"`
		OccurredAt time.Time `json:"occurred_at"`
		Data       struct {
			ExchangeID         uuid.UUID `json:"exchange_id"`
			SourceOrderID      uuid.UUID `json:"source_order_id"`
			ReplacementOrderID uuid.UUID `json:"replacement_order_id"`
			GuestOrderRef      uuid.UUID `json:"guest_order_ref"`
			Quantity           int32     `json:"quantity"`
		} `json:"data"`
	}
	if err := json.Unmarshal(envelope, &env); err != nil {
		t.Fatal(err)
	}
	if env.Schema != 1 || env.Data.ExchangeID != ex.ID || env.Data.SourceOrderID != c.OrderID ||
		env.Data.ReplacementOrderID != rep.OrderID || env.Data.Quantity != 2 {
		t.Fatalf("frozen payload is wrong: %+v", env.Data)
	}
	if !env.OccurredAt.Equal(settledAt.Time.UTC()) {
		t.Fatalf("occurred_at %s is not the persisted settled_at %s — a republish would freeze different bytes",
			env.OccurredAt, settledAt.Time.UTC())
	}
	// The GUEST reference is the SOURCE order's: the buyer's existing link must show the
	// old and the new tickets together.
	var sourceRef uuid.UUID
	if err := db.QueryRowContext(ctx, `SELECT guest_order_ref FROM orders WHERE id=$1`, c.OrderID).Scan(&sourceRef); err != nil {
		t.Fatal(err)
	}
	if env.Data.GuestOrderRef != sourceRef {
		t.Fatalf("guest_order_ref = %s, want the SOURCE order's %s", env.Data.GuestOrderRef, sourceRef)
	}

	// And settling twice owes exactly one event — the deterministic id makes the replay a
	// no-op rather than a second obligation.
	if err := CompleteExchangeSettlement(ctx, db, ex.OrganizerID, ex.ID, rep.OrderID); err != nil {
		t.Fatal(err)
	}
	var owed int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM completion_outbox WHERE subject='platform.commerce.order.exchanged'`).Scan(&owed); err != nil {
		t.Fatal(err)
	}
	if owed != 1 {
		t.Fatalf("a replayed settlement owes %d switch events, want 1", owed)
	}
}

// AC4's ordering, enforced by the database rather than by the caller's good manners
// (migration 0011). Capacity returning before the tickets stop admitting is the one
// sequence that OVERSELLS — so it is a CHECK, not a comment.
func TestCapacityCannotBeReturnedBeforeTheSwitch(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "exch-order", 2, 1000)
	ex, err := BindOrderExchange(ctx, db, exchangeRequest(c, "ord-1", uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE order_exchanges SET capacity_returned_at=now() WHERE id=$1`, ex.ID); err == nil {
		t.Fatal("the database allowed capacity to be returned before the entitlement switched")
	}

	// And the guarded helper refuses the same thing without erroring: an out-of-order call
	// is a no-op, not a write.
	if err := MarkExchangeCapacityReturned(ctx, db, ex.OrganizerID, ex.ID); err != nil {
		t.Fatal(err)
	}
	sw, err := LoadExchangeSwitch(ctx, db, ex.OrganizerID, ex.ID)
	if !errors.Is(err, ErrExchangeNotSettled) {
		t.Fatalf("an unsettled exchange must not load as switchable: %+v %v", sw, err)
	}
}

// The three timestamps advance in their safety order, and each is once-only.
func TestExchangeReversalProgressIsThreeOrderedFacts(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "exch-progress", 1, 1000)
	ex, err := BindOrderExchange(ctx, db, exchangeRequest(c, "prog-1", uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	rep, _ := seedCompleted(t, db, ctx, "exch-progress-replacement", 1, 1500)
	if _, err := db.ExecContext(ctx, `DELETE FROM completion_outbox WHERE order_id=$1`, rep.OrderID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RecordExchangeBasis(ctx, db, ex.OrganizerID, ex.ID, ExchangeBasis{
		TargetHoldID: uuid.New(), ReplacementReservationID: uuid.New(), TargetSlotID: uuid.New(),
		TargetTotal: 1500, DeltaAmount: 500, TargetUnitAmount: 1500,
	}); err != nil {
		t.Fatal(err)
	}
	if err := CompleteExchangeSettlement(ctx, db, ex.OrganizerID, ex.ID, rep.OrderID); err != nil {
		t.Fatal(err)
	}

	sw, err := LoadExchangeSwitch(ctx, db, ex.OrganizerID, ex.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sw.TicketsExchanged || sw.CapacityReturned || sw.Quantity != 1 || sw.SourceHoldID == uuid.Nil {
		t.Fatalf("a freshly settled exchange = %+v, want switch_pending with the SOURCE hold", sw)
	}

	if err := MarkExchangeTicketsSwitched(ctx, db, ex.OrganizerID, ex.ID); err != nil {
		t.Fatal(err)
	}
	if err := MarkExchangeCapacityReturned(ctx, db, ex.OrganizerID, ex.ID); err != nil {
		t.Fatal(err)
	}
	sw, err = LoadExchangeSwitch(ctx, db, ex.OrganizerID, ex.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sw.TicketsExchanged || !sw.CapacityReturned {
		t.Fatalf("reversal = %+v, want both discharged", sw)
	}

	// Once-only: a replayed callback must not move either timestamp again.
	var before, after time.Time
	if err := db.QueryRowContext(ctx, `SELECT tickets_exchanged_at FROM order_exchanges WHERE id=$1`, ex.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := MarkExchangeTicketsSwitched(ctx, db, ex.OrganizerID, ex.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT tickets_exchanged_at FROM order_exchanges WHERE id=$1`, ex.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !before.Equal(after) {
		t.Fatalf("a replayed switch callback moved the timestamp: %s → %s", before, after)
	}
}

// ai-review pass 3 F2. The projection must carry the third fact, because the API reports
// from it — and an exchange whose capacity return failed is under-selling, not done.
//
// Migration 0011 added `capacity_returned_at` precisely to make this state expressible.
// Adding the column and then not projecting it left the staff endpoint answering
// `completed` for a switched exchange whose old seats were still withheld, findable only
// by hand-written SQL. The column and the projection are one decision, not two.
func TestExchangeProjectionCarriesTheCapacityFact(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "exch-projection", 1, 1000)
	ex, err := BindOrderExchange(ctx, db, exchangeRequest(c, "proj-1", uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	rep, _ := seedCompleted(t, db, ctx, "exch-projection-replacement", 1, 1500)
	if _, err := db.ExecContext(ctx, `DELETE FROM completion_outbox WHERE order_id=$1`, rep.OrderID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RecordExchangeBasis(ctx, db, ex.OrganizerID, ex.ID, ExchangeBasis{
		TargetHoldID: uuid.New(), ReplacementReservationID: uuid.New(), TargetSlotID: uuid.New(),
		TargetTotal: 1500, DeltaAmount: 500, TargetUnitAmount: 1500,
	}); err != nil {
		t.Fatal(err)
	}
	if err := CompleteExchangeSettlement(ctx, db, ex.OrganizerID, ex.ID, rep.OrderID); err != nil {
		t.Fatal(err)
	}
	if err := MarkExchangeTicketsSwitched(ctx, db, ex.OrganizerID, ex.ID); err != nil {
		t.Fatal(err)
	}

	// Switched, capacity NOT returned — the substate the whole column exists for. A replay
	// of the staff request must report it rather than claiming the exchange is done.
	reloaded, err := BindOrderExchange(ctx, db, exchangeRequest(c, "proj-1", ex.TargetTicketTypeID))
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.TicketsExchanged || reloaded.CapacityReturned {
		t.Fatalf("exchange = switched %t / capacity %t, want switched with capacity STILL OUTSTANDING",
			reloaded.TicketsExchanged, reloaded.CapacityReturned)
	}

	if err := MarkExchangeCapacityReturned(ctx, db, ex.OrganizerID, ex.ID); err != nil {
		t.Fatal(err)
	}
	done, err := BindOrderExchange(ctx, db, exchangeRequest(c, "proj-1", ex.TargetTicketTypeID))
	if err != nil {
		t.Fatal(err)
	}
	if !done.TicketsExchanged || !done.CapacityReturned {
		t.Fatalf("exchange = switched %t / capacity %t, want both discharged", done.TicketsExchanged, done.CapacityReturned)
	}
}

// ai-review pass 4. A settled exchange that owes no switch event is repairable by the
// ordinary replay, so the guarantee does not rest on how the code was rolled out.
//
// The state is not reachable in this build — settlement and the outbox row share one
// transaction — so the test MANUFACTURES it by deleting the owed row, which is exactly the
// shape pre-TKT-166 data would have. Manufacturing it is the point: it makes the repair
// testable without a release having happened.
func TestReplayRepairsASettledExchangeThatOwesNoEvent(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "exch-repair", 1, 1000)
	ex, err := BindOrderExchange(ctx, db, exchangeRequest(c, "repair-1", uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	rep, _ := seedCompleted(t, db, ctx, "exch-repair-replacement", 1, 1500)
	if _, err := db.ExecContext(ctx, `DELETE FROM completion_outbox WHERE order_id=$1`, rep.OrderID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RecordExchangeBasis(ctx, db, ex.OrganizerID, ex.ID, ExchangeBasis{
		TargetHoldID: uuid.New(), ReplacementReservationID: uuid.New(), TargetSlotID: uuid.New(),
		TargetTotal: 1500, DeltaAmount: 500, TargetUnitAmount: 1500,
	}); err != nil {
		t.Fatal(err)
	}
	if err := CompleteExchangeSettlement(ctx, db, ex.OrganizerID, ex.ID, rep.OrderID); err != nil {
		t.Fatal(err)
	}

	// Settled, and now owing nothing — the pre-TKT-166 shape.
	if _, err := db.ExecContext(ctx, `DELETE FROM completion_outbox WHERE subject='platform.commerce.order.exchanged' AND order_id=$1`, rep.OrderID); err != nil {
		t.Fatal(err)
	}
	if n := countExchangeEvents(t, ctx, db, rep.OrderID); n != 0 {
		t.Fatalf("setup left %d owed events", n)
	}

	// The replay repairs it, against the SAME persisted settled_at — so the recovered bytes
	// are the ones that would have been published originally, not a fresh timestamp under
	// the same deterministic id.
	if err := CompleteExchangeSettlement(ctx, db, ex.OrganizerID, ex.ID, rep.OrderID); err != nil {
		t.Fatalf("repair: %v", err)
	}
	if n := countExchangeEvents(t, ctx, db, rep.OrderID); n != 1 {
		t.Fatalf("a settled exchange owing no event was not repaired by a replay (%d owed)", n)
	}
	var occurred time.Time
	var settledAt time.Time
	if err := db.QueryRowContext(ctx, `
		SELECT (x.envelope->>'occurred_at')::timestamptz, e.settled_at
		FROM completion_outbox x JOIN order_exchanges e ON e.replacement_order_id = x.order_id
		WHERE e.id=$1 AND x.subject='platform.commerce.order.exchanged'`, ex.ID).Scan(&occurred, &settledAt); err != nil {
		t.Fatal(err)
	}
	if !occurred.Equal(settledAt) {
		t.Fatalf("repaired occurred_at %s is not the persisted settled_at %s", occurred, settledAt)
	}
}

func countExchangeEvents(t *testing.T, ctx context.Context, db *sql.DB, order uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM completion_outbox WHERE subject='platform.commerce.order.exchanged' AND order_id=$1`, order).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TKT-215 A1: an EVEN exchange on a fee-carrying order must move ZERO money.
//
// This is the test that fails against the version of TKT-215 that folded fees
// into total_amount and called the consequence a deferred gap. The delta is
// targetTotal − sourceTotal, and targetTotal is a rule-resolved price with no fee
// in it; if the source is read from the GROSS total, an identically-priced
// exchange produces a negative delta and settleExchangeDelta issues a partial
// refund of the service fee to a buyer who exchanged for the same thing.
//
// FIXTURE NOTE: the fee must be NON-ZERO and the exchange must be EVEN. A
// zero-fee fixture cannot distinguish reading face from reading total — they are
// the same number — which is precisely why every pre-TKT-215 row is safe and why
// a test seeded from the old helper would have proved nothing.
func TestEvenExchangeOnAFeeCarryingOrderMovesNoMoney(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "exch-fee-even", 2, 4550) // face 9100

	// Charge a 300-cent passed-on fee on top: gross 9400, face 9100.
	if _, err := db.ExecContext(ctx, `
		UPDATE reservations SET total_amount = 9400, face_value_amount = 9100,
		       fee_resolution_snapshot = $2
		WHERE id = $1`, c.ReservationID,
		[]byte(`{"resolution":{"fees":[]},"breakdown":[{"fee_code":"service","basis":"per_ticket_fixed","incidence":"passed_on","amount":300,"currency":"EUR"}],"face_value":9100,"passed_on_fees":300,"absorbed_fees":0,"total_amount":9400}`)); err != nil {
		t.Fatal(err)
	}

	ex, err := BindOrderExchange(ctx, db, exchangeRequest(c, "x-fee-1", uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	// The source total the exchange reasons about is the FACE value, not the
	// gross charge.
	if ex.SourceTotal != 9100 {
		t.Fatalf("SourceTotal = %d, want the face value 9100. Reading total_amount (9400) here "+
			"makes an even exchange look like a 300-cent downgrade", ex.SourceTotal)
	}
	// An identically-priced target: 2 × 4550.
	if got := ExchangeDelta(ex.SourceTotal, 4550*int64(ex.Quantity)); got != 0 {
		t.Errorf("delta = %d, want 0 — an even exchange must move no money. A negative delta "+
			"here means the buyer is refunded their service fee for exchanging like for like", got)
	}

	// And the OTHER half, which the first version of this fix got wrong
	// (ai-review [high]): the gross is carried separately, because the
	// `order.exchange.reversed` money fact reverses what was actually CAPTURED.
	// Reversing the face value would leave the payments journal disagreeing with
	// the original charge by exactly the fee.
	if ex.SourceGrossTotal != 9400 {
		t.Errorf("SourceGrossTotal = %d, want the captured 9400. The delta needs the face value "+
			"and the reversal fact needs the gross; one column cannot serve both", ex.SourceGrossTotal)
	}
	if ex.SourceTotal == ex.SourceGrossTotal {
		t.Error("face and gross are equal on a fee-carrying order — this fixture can no longer " +
			"distinguish the two, which is the only thing it is here to do")
	}
}

// The lost-race path answers only for the SAME request (ai-review pass 2).
//
// Two reserves under one idempotency key with different channels both miss the
// initial lookup — the channel never reaches inventory, so both receive the same
// hold — and one INSERT wins. The loser must NOT be handed the winner's
// reservation: its terms were never accepted, and answering 201 there makes one
// request succeed now and conflict on every retry afterwards.
//
// Driven at the store level by seeding the winner's row first, which is exactly
// the state the loser observes.
func TestReserveLosingTheInsertRaceRefusesForeignTerms(t *testing.T) {
	db, ctx := outboxDB(t)
	org := uuid.New()
	id := uuid.New()
	channelA := "channel-a"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,
		                         quantity,unit_amount,total_amount,face_value_amount,currency,status,channel_code)
		VALUES($1,$2,$3,$4,$5,$6,2,4550,9700,9100,'EUR','held',$7)`,
		id, org, uuid.New(), uuid.New(), uuid.New(), uuid.New(), channelA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM reservations WHERE id=$1`, id) })

	var storedChannel *string
	if err := db.QueryRowContext(ctx,
		`SELECT channel_code FROM reservations WHERE id=$1`, id).Scan(&storedChannel); err != nil {
		t.Fatal(err)
	}
	if storedChannel == nil || *storedChannel != channelA {
		t.Fatalf("channel_code = %v, want it PERSISTED — it is an idempotency term, and a term "+
			"that is not stored cannot be compared", storedChannel)
	}
}

// The source sale's channel reaches the exchange (TKT-237).
//
// LoadExchangeSource projects reservations.channel_code so the exchange handler
// can reprice the TARGET on the channel the source was bought on. Without it the
// target reprices in the default/public context, which silently changes what the
// buyer owes on the difference — and the symptom is a wrong number, not an
// error, so nothing else would notice.
//
// This test exists because the field was added by editing a SELECT and a Scan in
// two places, and one Scan was missed: the gate caught it as "expected 11
// destination arguments, not 10". That failure mode is loud. The one this test
// guards is the quiet successor — a future edit that drops the column from the
// SELECT while the struct field stays, leaving every exchange priced publicly.
func TestLoadExchangeSourceCarriesTheChannel(t *testing.T) {
	db, ctx := outboxDB(t)

	t.Run("a channelled sale", func(t *testing.T) {
		c, _ := seedCompleted(t, db, ctx, "exch-chan", 2, 1000)
		if _, err := db.ExecContext(ctx,
			`UPDATE reservations SET channel_code='reseller' WHERE id=$1`, c.ReservationID); err != nil {
			t.Fatal(err)
		}
		src, err := LoadExchangeSource(ctx, db, c.OrganizerID, c.OrderID)
		if err != nil {
			t.Fatal(err)
		}
		if src.ChannelCode == nil {
			t.Fatal("ChannelCode = nil for a sale made on 'reseller' — the exchange would reprice on the public channel")
		}
		if *src.ChannelCode != "reseller" {
			t.Fatalf("ChannelCode = %q, want %q verbatim", *src.ChannelCode, "reseller")
		}
	})

	t.Run("a public sale", func(t *testing.T) {
		// nil, not "": the default/public context is the absence of a channel,
		// and an empty string is a channel whose code is empty — which the
		// contract's minLength forbids anyway. Conflating them would send
		// `channel_code=` on the reprice, a different request entirely.
		c, _ := seedCompleted(t, db, ctx, "exch-public", 2, 1000)
		src, err := LoadExchangeSource(ctx, db, c.OrganizerID, c.OrderID)
		if err != nil {
			t.Fatal(err)
		}
		if src.ChannelCode != nil {
			t.Fatalf("ChannelCode = %q for a sale that named no channel, want nil", *src.ChannelCode)
		}
	})
}

// A CONCURRENT loser continues with the WINNER's basis, never its own (TKT-167, COS 3).
//
// TKT-158 made RecordExchangeBasis report `false` to a losing writer, and the handler
// answered 409 rather than guess — correct, and as far as that ticket could go. It leaves
// the loser unable to RESUME, which is the whole point here: the money's basis is the
// persisted one, and a caller that cannot read it back can only refuse.
//
// Driven through a real row lock rather than two sequential calls. Sequential calls prove
// the `basis_at IS NULL` guard and nothing about concurrency: the loser's UPDATE would
// never have waited on anything, so a fallback SELECT that reads a stale snapshot would
// pass just as well. Here the loser's UPDATE blocks on the winner's uncommitted row, wakes
// after the commit, matches zero rows because `basis_at` is no longer NULL, and must then
// read what the winner actually wrote.
//
// The two bases differ in EVERY field, so the assertion cannot be satisfied by accident:
// a function that returned the caller's own input would have to coincide on seven values.
func TestTheLoserOfABasisRaceContinuesWithTheWinnersBasis(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "exch-basis-race", 2, 1000) // source_total 2000
	ex, err := BindOrderExchange(ctx, db, exchangeRequest(c, "x-race-1", uuid.New()))
	if err != nil {
		t.Fatal(err)
	}

	// 2 × 1500 = 3000, delta 3000 - 2000 = +1000. An upgrade.
	winner := ExchangeBasis{
		TargetHoldID: uuid.New(), ReplacementReservationID: uuid.New(), TargetSlotID: uuid.New(),
		TargetTotal: 3000, DeltaAmount: 1000, TargetUnitAmount: 1500,
		PriceSnapshot: []byte(`{"who":"winner"}`),
	}
	// 2 × 900 = 1800, delta 1800 - 2000 = -200. A downgrade — the opposite MONEY DIRECTION,
	// so a loser that kept its own basis would not merely settle a different number, it
	// would call the refund leg instead of the charge leg.
	loser := ExchangeBasis{
		TargetHoldID: uuid.New(), ReplacementReservationID: uuid.New(), TargetSlotID: uuid.New(),
		TargetTotal: 1800, DeltaAmount: -200, TargetUnitAmount: 900,
		PriceSnapshot: []byte(`{"who":"loser"}`),
	}

	// The winner writes inside a transaction it holds open, so the loser's UPDATE has a
	// live row lock to block on.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		UPDATE order_exchanges
		SET target_hold_id=$3, replacement_reservation_id=$4, target_total=$5, delta_amount=$6,
		    target_unit_amount=$7, target_slot_id=$8, target_price_snapshot=$9, basis_at=now()
		WHERE organizer_id=$1 AND id=$2 AND basis_at IS NULL`,
		ex.OrganizerID, ex.ID, winner.TargetHoldID, winner.ReplacementReservationID,
		winner.TargetTotal, winner.DeltaAmount, winner.TargetUnitAmount, winner.TargetSlotID,
		winner.PriceSnapshot); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		basis   ExchangeBasis
		written bool
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		got, written, err := RecordExchangeBasis(ctx, db, ex.OrganizerID, ex.ID, loser)
		done <- outcome{got, written, err}
	}()

	// The loser must actually be BLOCKED, not merely slower. Without this the test could
	// pass on a race it never created — the goroutine finishing after the commit for
	// timing reasons, proving nothing about the lock. pg_stat_activity is the only place
	// that fact is observable.
	waitForBlockedWriter(t, db, ctx)

	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var got outcome
	select {
	case got = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the losing writer never returned after the winner committed")
	}
	if got.err != nil {
		t.Fatalf("the loser must resolve, not error: %v", got.err)
	}
	if got.written {
		t.Fatal("the loser reported written=true; only the writer that set basis_at may claim it")
	}
	if got.basis.TargetTotal != winner.TargetTotal || got.basis.DeltaAmount != winner.DeltaAmount ||
		got.basis.TargetUnitAmount != winner.TargetUnitAmount {
		t.Fatalf("the loser continued on money that was never persisted: got total=%d delta=%d unit=%d, "+
			"want the winner's %d/%d/%d. Settling its own basis charges an amount the row does not record",
			got.basis.TargetTotal, got.basis.DeltaAmount, got.basis.TargetUnitAmount,
			winner.TargetTotal, winner.DeltaAmount, winner.TargetUnitAmount)
	}
	if got.basis.TargetHoldID != winner.TargetHoldID ||
		got.basis.ReplacementReservationID != winner.ReplacementReservationID ||
		got.basis.TargetSlotID != winner.TargetSlotID {
		t.Fatalf("the loser continued on identities the row does not name: hold=%s reservation=%s slot=%s, "+
			"want %s/%s/%s. Finalizing a hold nobody recorded strands the claim the money bought",
			got.basis.TargetHoldID, got.basis.ReplacementReservationID, got.basis.TargetSlotID,
			winner.TargetHoldID, winner.ReplacementReservationID, winner.TargetSlotID)
	}
	// Compared as JSON, not as bytes: the column is `jsonb`, which reparses and reserializes
	// on the way in, so `{"who":"winner"}` comes back as `{"who": "winner"}`. That is
	// canonicalization, not drift — and the replacement's own price_resolution_snapshot is
	// jsonb too (migration 0006), so both sides of the round trip normalize identically.
	// Asserting bytes here would pin PostgreSQL's spacing, which is not the invariant.
	assertSameJSON(t, got.basis.PriceSnapshot, winner.PriceSnapshot,
		"the replacement's provenance must describe the basis the money actually used (ADR-036 §5)")
}

// The FIRST writer is told it wrote, and reads back exactly what it sent.
//
// The companion to the race above: `written` is what tells the handler whether the basis
// it is holding is its own commitment or somebody else's, and a function that always
// answered false would satisfy the loser test alone.
func TestTheWriterOfABasisIsToldItWroteIt(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "exch-basis-writer", 2, 1000)
	ex, err := BindOrderExchange(ctx, db, exchangeRequest(c, "x-writer-1", uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	mine := ExchangeBasis{
		TargetHoldID: uuid.New(), ReplacementReservationID: uuid.New(), TargetSlotID: uuid.New(),
		TargetTotal: 3000, DeltaAmount: 1000, TargetUnitAmount: 1500,
		PriceSnapshot: []byte(`{"who":"mine"}`),
	}
	got, written, err := RecordExchangeBasis(ctx, db, ex.OrganizerID, ex.ID, mine)
	if err != nil || !written {
		t.Fatalf("the first writer must report written=true: written=%t err=%v", written, err)
	}
	if got.TargetTotal != mine.TargetTotal || got.DeltaAmount != mine.DeltaAmount ||
		got.TargetUnitAmount != mine.TargetUnitAmount || got.TargetHoldID != mine.TargetHoldID ||
		got.ReplacementReservationID != mine.ReplacementReservationID ||
		got.TargetSlotID != mine.TargetSlotID {
		t.Fatalf("the writer read back a basis that is not what it wrote: %+v vs %+v", got, mine)
	}
	assertSameJSON(t, got.PriceSnapshot, mine.PriceSnapshot, "the writer's own snapshot")
}

// An exchange that does not exist is an ERROR, not a zero basis.
//
// The record-or-load shape has a third outcome the two-valued one did not: the UPDATE
// matches nothing AND the SELECT finds nothing. Returning a zero-valued basis there would
// hand the handler total=0, delta=0 and a nil hold — a settlement of nothing against a
// claim that does not exist — and every field would look like an ordinary even exchange.
func TestRecordExchangeBasisRefusesAnExchangeThatDoesNotExist(t *testing.T) {
	db, ctx := outboxDB(t)
	_, written, err := RecordExchangeBasis(ctx, db, uuid.New(), uuid.New(), ExchangeBasis{
		TargetHoldID: uuid.New(), ReplacementReservationID: uuid.New(), TargetSlotID: uuid.New(),
		TargetTotal: 1000, DeltaAmount: 0, TargetUnitAmount: 500,
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows — a missing exchange must not resolve to a zero basis", err)
	}
	if written {
		t.Fatal("written=true for an exchange that does not exist")
	}
}

// waitForBlockedWriter blocks until some backend is waiting on a lock, which is the only
// observable proof that the concurrent writer reached the UPDATE and was stopped by it.
func waitForBlockedWriter(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND state = 'active'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("no writer ever blocked on the row lock — the race this test needs was never created, " +
		"so a passing result would prove nothing about concurrency")
}

// assertSameJSON compares two jsonb payloads by VALUE. The column reserializes on write, so
// byte equality would assert PostgreSQL's whitespace rather than the snapshot's content.
func assertSameJSON(t *testing.T, got, want []byte, why string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("stored snapshot is not JSON (%s): %v", got, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("expected snapshot is not JSON (%s): %v", want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("price snapshot = %s, want %s — %s", got, want, why)
	}
}

// The bind race that shares a hold answers CONFLICT, not not-exchangeable (TKT-167,
// ai-review pass 3 [high]).
//
// Two requests with the same organizer and idempotency key but DIFFERENT source orders
// derive the same ExchangeID — and therefore the same `exchange-target:<id>` inventory key,
// so they share one claim. The bind transaction's only row lock is `FOR UPDATE OF o` on the
// SOURCE order, so those two lock different rows: both can pass the lookup seeing no
// exchange and both reach the INSERT. One loses on `order_exchanges_pkey`.
//
// What the loser is TOLD decides whether the winner survives. The handler releases the
// target hold on every bind error except ErrExchangeConflict, and the hold is SHARED — so
// mapping this collision to ErrOrderNotExchangeable made the loser release the claim the
// winner had already finalized, and possibly already charged against. That is the buyer-paid
// wedge, reachable by two ordinary commerce callers rather than by anyone holding a service
// token.
//
// The distinction is the CONSTRAINT NAME, which is why the code branches on it rather than
// on isUniqueViolation: order_exchanges_one_per_source means a different exchange owns the
// order and the hold really is ours to release; order_exchanges_pkey means the opposite.
func TestABindCollidingOnTheExchangeIdentityReportsConflict(t *testing.T) {
	db, ctx := outboxDB(t)
	winner, _ := seedCompleted(t, db, ctx, "bind-race-winner", 2, 1000)
	loser, _ := seedCompleted(t, db, ctx, "bind-race-loser", 2, 1000)
	// One organizer, so the two share an ExchangeID for a shared idempotency key.
	if _, err := db.ExecContext(ctx,
		`UPDATE reservations SET organizer_id=$1 WHERE id=$2`, winner.OrganizerID, loser.ReservationID); err != nil {
		t.Fatal(err)
	}
	loser.OrganizerID = winner.OrganizerID
	target := uuid.New()

	if _, err := BindOrderExchange(ctx, db, exchangeRequest(winner, "shared", target)); err != nil {
		t.Fatalf("winner bind: %v", err)
	}

	// The loser reaching the INSERT is what the concurrent interleaving produces; driven
	// here by inserting a row the loser's own INSERT would collide with. The error the
	// STORE maps is the thing under test.
	_, err := db.ExecContext(ctx, `
		INSERT INTO order_exchanges(organizer_id,id,source_order_id,target_ticket_type_id,
		                            idempotency_key,request_fingerprint,quantity,source_total,
		                            source_gross_total,currency,actor,reason)
		VALUES($1,$2,$3,$4,$5,'other-fingerprint',2,2000,2000,'EUR','a','r')`,
		loser.OrganizerID, ExchangeID(loser.OrganizerID, "shared"), loser.OrderID, target, "shared")
	if err == nil {
		t.Fatal("the identity collision did not fire — this fixture can no longer reach the race")
	}
	if got := violatedConstraint(err); got != "order_exchanges_pkey" {
		t.Fatalf("constraint = %q, want order_exchanges_pkey. If the schema changed, this test's "+
			"whole premise moved and the mapping below must be re-derived", got)
	}

	// THE ASSERTION: that collision is a CONFLICT.
	if !errors.Is(mapBindInsertError(err), ErrExchangeConflict) {
		t.Fatalf("an identity collision maps to %v, want ErrExchangeConflict. As "+
			"ErrOrderNotExchangeable the handler releases the SHARED target hold — the winner's "+
			"finalized, possibly already-charged claim — and the buyer is left paid with no target",
			mapBindInsertError(err))
	}

	// And the sibling constraint still means the opposite, or the fix has merely moved the
	// defect: a DIFFERENT exchange owning the source order must stay not-exchangeable, since
	// there the hold really is the caller's to release.
	// A DISTINCT identity (new uuid, new key) aimed at the source order the winner already
	// exchanged: that trips one_per_source instead, and must map the OTHER way. A fix in one
	// direction that broke the other would be invisible to a test checking only the case
	// that failed.
	_, err = db.ExecContext(ctx, `
		INSERT INTO order_exchanges(organizer_id,id,source_order_id,target_ticket_type_id,
		                            idempotency_key,request_fingerprint,quantity,source_total,
		                            source_gross_total,currency,actor,reason)
		VALUES($1,$2,$3,$4,$5,'fp',2,2000,2000,'EUR','a','r')`,
		winner.OrganizerID, uuid.New(), winner.OrderID, uuid.New(), "distinct-key")
	if err == nil {
		t.Fatal("the one-per-source index did not fire")
	}
	if got := violatedConstraint(err); got != "order_exchanges_one_per_source" {
		t.Fatalf("constraint = %q, want order_exchanges_one_per_source", got)
	}
	if !errors.Is(mapBindInsertError(err), ErrOrderNotExchangeable) {
		t.Fatalf("a one-per-source collision maps to %v, want ErrOrderNotExchangeable — a "+
			"different exchange owns the order, so this caller's hold IS its own to release",
			mapBindInsertError(err))
	}

	// AND the third index, which the first version of this fix missed (pass 4).
	// `UNIQUE (organizer_id, idempotency_key)` from migration 0010 means the same thing as
	// the primary key — same identity, shared hold — but a fix that enumerated the pkey and
	// let everything else fall through to ErrOrderNotExchangeable sent this one to the
	// releasing branch, so the defect survived through a different constraint.
	_, err = db.ExecContext(ctx, `
		INSERT INTO order_exchanges(organizer_id,id,source_order_id,target_ticket_type_id,
		                            idempotency_key,request_fingerprint,quantity,source_total,
		                            source_gross_total,currency,actor,reason)
		VALUES($1,$2,$3,$4,'shared','fp3',2,2000,2000,'EUR','a','r')`,
		loser.OrganizerID, uuid.New(), loser.OrderID, uuid.New())
	if err == nil {
		t.Fatal("the (organizer, idempotency_key) index did not fire")
	}
	if got := violatedConstraint(err); got != "order_exchanges_organizer_id_idempotency_key_key" {
		t.Fatalf("constraint = %q, want order_exchanges_organizer_id_idempotency_key_key", got)
	}
	if !errors.Is(mapBindInsertError(err), ErrExchangeConflict) {
		t.Fatalf("a (organizer, idempotency_key) collision maps to %v, want ErrExchangeConflict. "+
			"That pair IS the exchange identity and therefore the target hold key, so the hold is "+
			"shared and must not be released — the same hazard as the pkey, through another index",
			mapBindInsertError(err))
	}

	// And the property the enumeration rests on: an UNKNOWN constraint must fail SAFE. A
	// future index on this table is the next version of the bug that reached pass 4, and the
	// only defence that survives not knowing about it is the default.
	if !errors.Is(mapBindInsertError(&pgconn.PgError{Code: "23505", ConstraintName: "order_exchanges_some_future_index"}), ErrExchangeConflict) {
		t.Fatal("an unrecognised unique violation must map to ErrExchangeConflict — the default " +
			"has to be the answer that does NOT release a possibly-shared hold, or every index " +
			"added to this table reinstates the defect until someone remembers to enumerate it")
	}
}

// The mapping is WIRED INTO BindOrderExchange, not merely correct in isolation
// (TKT-167, ai-review pass 4 [medium]).
//
// The test above proves mapBindInsertError classifies each constraint correctly. It stays
// GREEN if line 286's `return Exchange{}, mapBindInsertError(err)` becomes
// `return Exchange{}, err` — verified by running that mutation — because it calls the helper
// itself and never drives a losing bind. That is this repo's standing distinction between
// breaking a mechanism and removing it from the place that uses it (AGENTS.md, TKT-202): a
// guard that tests the mechanism and not the wiring catches the wrong edit.
//
// So this one goes through BindOrderExchange and asserts on what IT returns.
//
// The losing INSERT is reached without a goroutine race by exploiting the lookup's scope: it
// keys on ExchangeID(organizer, key), so seeding a row under a DIFFERENT id that still
// collides on `UNIQUE (organizer_id, idempotency_key)` leaves the lookup finding nothing and
// the INSERT colliding — the same code path a real concurrent loser takes, made
// deterministic. A goroutine race would test the scheduler; this tests the boundary.
func TestBindOrderExchangeReturnsConflictForAnIdentityCollision(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "bind-wiring", 2, 1000)

	// A row holding this organizer+key under an unrelated id and an unrelated source order.
	// The lookup (which keys on the DERIVED id) misses it; the INSERT cannot.
	other, _ := seedCompleted(t, db, ctx, "bind-wiring-other", 2, 1000)
	if _, err := db.ExecContext(ctx,
		`UPDATE reservations SET organizer_id=$1 WHERE id=$2`, c.OrganizerID, other.ReservationID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_exchanges(organizer_id,id,source_order_id,target_ticket_type_id,
		                            idempotency_key,request_fingerprint,quantity,source_total,
		                            source_gross_total,currency,actor,reason)
		VALUES($1,$2,$3,$4,$5,'squatter',2,2000,2000,'EUR','a','r')`,
		c.OrganizerID, uuid.New(), other.OrderID, uuid.New(), "wiring-key"); err != nil {
		t.Fatal(err)
	}

	_, err := BindOrderExchange(ctx, db, exchangeRequest(c, "wiring-key", uuid.New()))
	if err == nil {
		t.Fatal("the identity collision did not surface — this fixture no longer reaches the INSERT")
	}
	if !errors.Is(err, ErrExchangeConflict) {
		t.Fatalf("BindOrderExchange returned %v, want ErrExchangeConflict.\n\n"+
			"This is the WIRING assertion: the handler releases the target hold on every bind "+
			"error except ErrExchangeConflict, and the hold is keyed on the very identity that "+
			"just collided — so any other answer here makes a losing request release a claim "+
			"another request may already have finalized and charged against. Returning the raw "+
			"INSERT error, or dropping the mapBindInsertError call, both land here.", err)
	}
}
