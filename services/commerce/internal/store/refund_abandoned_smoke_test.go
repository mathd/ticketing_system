//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// TKT-525 S2 (ADR-087): the Commerce half of `abandoned`.
//
// The outcome is appended after payments has committed its marker. It changes nothing on the
// refund row: the refund stays pending, the order projection, the tickets and the capacity keep
// their state. It releases ONLY that refund's quantity allowance, because the quantity sum
// excludes a refund with a matching `abandoned` outcome.

const abandonedProviderRef = "re_failed_abandoned"

func abandonedRequest(c Completion, refund uuid.UUID) RefundOutcomeRequest {
	return RefundOutcomeRequest{
		OrganizerID: c.OrganizerID, RefundID: refund, Kind: OutcomeAbandoned,
		OperatorName: "Ada Operator", Reason: "buyer confirmed nobody will refund this attempt",
	}
}

func recordAbandoned(t *testing.T, ctx context.Context, db *sql.DB, in RefundOutcomeRequest) (RefundOutcome, bool, error) {
	t.Helper()
	target, err := InspectRefundOutcomeTarget(ctx, db, in.OrganizerID, in.RefundID)
	if err != nil {
		return RefundOutcome{}, false, err
	}
	return RecordAbandoned(ctx, db, in, target, abandonedProviderRef)
}

// insertOutcome writes an outcome row directly, for fixtures that need an outcome the command
// path would not produce (another organizer's, or one for a refund that does not exist).
func insertForeignOutcome(t *testing.T, ctx context.Context, db *sql.DB, org, subject uuid.UUID, kind string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO refund_outcomes(organizer_id,subject_kind,subject_id,kind,amount,currency,external_ref,operator_name,session_user_name,reason)
		VALUES($1,'order_refund',$2,$3,1000,'EUR','re_fixture','Ada',session_user,'fixture')`, org, subject, kind); err != nil {
		t.Fatal(err)
	}
}

type refundRow struct {
	status  string
	outside sql.NullString
	voided  bool
}

func readRefundRow(t *testing.T, ctx context.Context, db *sql.DB, org, refund uuid.UUID) refundRow {
	t.Helper()
	var r refundRow
	if err := db.QueryRowContext(ctx, `
		SELECT status, outside_completion_kind, tickets_voided_at IS NOT NULL FROM order_refunds WHERE organizer_id=$1 AND id=$2`, org, refund).
		Scan(&r.status, &r.outside, &r.voided); err != nil {
		t.Fatal(err)
	}
	return r
}

// The independent-ceilings fixture, Commerce half: two units at 1000. Two bound units fill the
// order. Abandoning one leaves one counted: one fresh unit fits and the next is refused.
func TestAbandonedOutcomeReleasesOnlyItsQuantity(t *testing.T) {
	db, ctx := outboxDB(t)
	c, order := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 2, 1000)
	a := bindPending(t, ctx, db, c, "abandon-ceiling-a", 1)
	bindPending(t, ctx, db, c, "abandon-ceiling-b", 1)
	if _, err := BindOrderRefund(ctx, db, request(c, "abandon-ceiling-c", 1)); !errors.Is(err, ErrRefundExceedsOrder) {
		t.Fatalf("before the outcome: err = %v, want ErrRefundExceedsOrder", err)
	}

	if _, replay, err := recordAbandoned(t, ctx, db, abandonedRequest(c, a.ID)); err != nil || replay {
		t.Fatalf("record abandoned: replay=%v err=%v", replay, err)
	}
	if _, err := BindOrderRefund(ctx, db, request(c, "abandon-ceiling-c", 1)); err != nil {
		t.Fatalf("after abandoning one unit, a fresh unit must fit: %v", err)
	}
	if _, err := BindOrderRefund(ctx, db, request(c, "abandon-ceiling-d", 1)); !errors.Is(err, ErrRefundExceedsOrder) {
		t.Fatalf("one more unit: err = %v, want ErrRefundExceedsOrder", err)
	}
	// The abandoned row is still there, still pending, and the projection never moved.
	if r := readRefundRow(t, ctx, db, c.OrganizerID, a.ID); r != (refundRow{"pending", sql.NullString{}, false}) {
		t.Fatalf("abandoned refund row = %+v", r)
	}
	if p := readProjection(t, ctx, db, order); p != (projection{0, 0, "none"}) {
		t.Fatalf("projection = %+v, want untouched", p)
	}
}

// Only a MATCHING abandoned outcome releases: same organizer, same refund. An outcome row for
// the same raw refund id under another organizer releases nothing here.
func TestAbandonedOutcomeOfAnotherOrganizerReleasesNothing(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 1, 1000)
	a := bindPending(t, ctx, db, c, "abandon-foreign-a", 1)
	insertForeignOutcome(t, ctx, db, uuid.New(), a.ID, OutcomeAbandoned)
	if _, err := BindOrderRefund(ctx, db, request(c, "abandon-foreign-b", 1)); !errors.Is(err, ErrRefundExceedsOrder) {
		t.Fatalf("a foreign outcome released this refund's unit: err = %v", err)
	}
	got, found, err := LookupRefundByID(ctx, db, c.OrganizerID, a.ID)
	if err != nil || !found || got.Abandoned {
		t.Fatalf("lookup = %+v found=%v err=%v, want not abandoned", got, found, err)
	}
}

// The refund lookup carries the flag, for the bulk runner and the shared refund service: true
// for this organizer's abandoned outcome only. A replay through BindOrderRefund carries it too.
func TestRefundLookupReportsTheAbandonedFlag(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 3, 1000)
	plain := bindPending(t, ctx, db, c, "flag-plain", 1)
	abandoned := bindPending(t, ctx, db, c, "flag-abandoned", 1)
	outside := bindPending(t, ctx, db, c, "flag-outside", 1)
	if _, _, err := recordAbandoned(t, ctx, db, abandonedRequest(c, abandoned.ID)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := recordOutside(t, ctx, db, outsideRequest(c, outside.ID)); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[uuid.UUID]bool{plain.ID: false, abandoned.ID: true, outside.ID: false} {
		got, found, err := LookupRefundByID(ctx, db, c.OrganizerID, name)
		if err != nil || !found || got.Abandoned != want {
			t.Fatalf("lookup %s = abandoned %v found=%v err=%v, want %v", name, got.Abandoned, found, err, want)
		}
	}
	replay, err := BindOrderRefund(ctx, db, request(c, "flag-abandoned", 1))
	if err != nil || !replay.Abandoned || replay.ID != abandoned.ID {
		t.Fatalf("bind replay = %+v err=%v, want the abandoned refund flagged", replay, err)
	}
}

// The full S2 Commerce write: one outcome with the payments reference, attribution from the
// command and session_user from the connection, and nothing else changed.
func TestRecordAbandonedAppendsAndLeavesTheRefundPending(t *testing.T) {
	db, ctx := outboxDB(t)
	c, order := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 2, 1000)
	r := bindPending(t, ctx, db, c, "abandon-write", 2)
	in := abandonedRequest(c, r.ID)
	got, replay, err := recordAbandoned(t, ctx, db, in)
	if err != nil || replay {
		t.Fatalf("replay=%v err=%v", replay, err)
	}
	var sessionUser string
	if err := db.QueryRowContext(ctx, `SELECT session_user`).Scan(&sessionUser); err != nil {
		t.Fatal(err)
	}
	if got.Kind != OutcomeAbandoned || got.Amount != 2000 || got.Currency != "EUR" || got.ExternalRef != abandonedProviderRef ||
		got.OperatorName != in.OperatorName || got.Reason != in.Reason || got.SessionUser != sessionUser || got.RecordedAt.IsZero() {
		t.Fatalf("outcome = %+v", got)
	}
	if r := readRefundRow(t, ctx, db, c.OrganizerID, r.ID); r != (refundRow{"pending", sql.NullString{}, false}) {
		t.Fatalf("refund row = %+v, want pending with no outside evidence", r)
	}
	if p := readProjection(t, ctx, db, order); p != (projection{0, 0, "none"}) {
		t.Fatalf("projection = %+v, want untouched", p)
	}
	if n := countFacts(t, ctx, db, order); n != 0 {
		t.Fatalf("order.refunded facts = %d", n)
	}
}

// Replay returns the original record; a different operator or reason conflicts. The external
// reference is payments', never part of the command, so it plays no part in the comparison.
// After abandoned, refunded_outside is a conflict.
func TestRecordAbandonedReplayAndConflict(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 1, 1000)
	r := bindPending(t, ctx, db, c, "abandon-replay", 1)
	in := abandonedRequest(c, r.ID)
	first, _, err := recordAbandoned(t, ctx, db, in)
	if err != nil {
		t.Fatal(err)
	}
	target, err := InspectRefundOutcomeTarget(ctx, db, c.OrganizerID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if existing, found, err := target.Replay(in); err != nil || !found || !existing.RecordedAt.Equal(first.RecordedAt) {
		t.Fatalf("inspected replay = %+v found=%v err=%v", existing, found, err)
	}
	again, replay, err := RecordAbandoned(ctx, db, in, target, "re_another_lookup")
	if err != nil || !replay || again.ExternalRef != first.ExternalRef || !again.RecordedAt.Equal(first.RecordedAt) {
		t.Fatalf("locked replay = %+v replay=%v err=%v, want the original record", again, replay, err)
	}
	for name, mutate := range map[string]func(*RefundOutcomeRequest){
		"reason":   func(x *RefundOutcomeRequest) { x.Reason += "!" },
		"operator": func(x *RefundOutcomeRequest) { x.OperatorName = "Grace Operator" },
	} {
		changed := in
		mutate(&changed)
		if _, _, err := RecordAbandoned(ctx, db, changed, target, abandonedProviderRef); !errors.Is(err, ErrRefundOutcomeConflict) {
			t.Fatalf("a repeat with a different %s: err = %v, want conflict", name, err)
		}
		if _, found, err := target.Replay(changed); !found || !errors.Is(err, ErrRefundOutcomeConflict) {
			t.Fatalf("inspected repeat with a different %s: err = %v, want conflict", name, err)
		}
	}
	if _, _, err := recordOutside(t, ctx, db, outsideRequest(c, r.ID)); !errors.Is(err, ErrRefundOutcomeConflict) {
		t.Fatalf("refunded_outside after abandoned: err = %v, want conflict", err)
	}
	var outcomes int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM refund_outcomes WHERE organizer_id=$1 AND subject_id=$2`, c.OrganizerID, r.ID).Scan(&outcomes); err != nil || outcomes != 1 {
		t.Fatalf("outcomes = %d err=%v", outcomes, err)
	}
}

// An ordinarily completed refund and an outside-completed one are not eligible; a foreign or
// missing one is not found; a changed row is stale. None writes.
func TestRecordAbandonedRefusals(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 3, 1000)
	completed := bindPending(t, ctx, db, c, "abandon-completed", 1)
	if err := CompleteOrderRefund(ctx, db, c.OrganizerID, completed.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	t.Run("completed", func(t *testing.T) {
		if _, _, err := recordAbandoned(t, ctx, db, abandonedRequest(c, completed.ID)); !errors.Is(err, ErrRefundOutcomeIneligible) {
			t.Fatalf("err = %v, want ErrRefundOutcomeIneligible", err)
		}
	})
	t.Run("foreign", func(t *testing.T) {
		other, _ := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 1, 1000)
		foreign := bindPending(t, ctx, db, other, "abandon-foreign", 1)
		if _, _, err := recordAbandoned(t, ctx, db, abandonedRequest(c, foreign.ID)); !errors.Is(err, ErrRefundOutcomeNotFound) {
			t.Fatalf("inspect: err = %v, want ErrRefundOutcomeNotFound", err)
		}
		forged := RefundOutcomeTarget{OrganizerID: c.OrganizerID, RefundID: foreign.ID, OrderID: other.OrderID, Amount: 1000, Currency: "EUR", ChargeKey: "x"}
		if _, _, err := RecordAbandoned(ctx, db, abandonedRequest(c, foreign.ID), forged, abandonedProviderRef); !errors.Is(err, ErrRefundOutcomeNotFound) {
			t.Fatalf("locked write: err = %v, want ErrRefundOutcomeNotFound", err)
		}
	})
	// A refund row under THIS organizer whose order's source reservation is another
	// organizer's. The forged target matches the row's order, charge key and money exactly, so
	// only the locked recheck of the source organizer can refuse it.
	t.Run("source reservation elsewhere", func(t *testing.T) {
		_, otherOrder := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 1, 1000)
		malformed := uuid.New()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO order_refunds(organizer_id,id,order_id,idempotency_key,request_fingerprint,quantity,unit_amount,amount,currency,actor,reason)
			VALUES($1,$2,$3,'malformed','fp',1,1000,1000,'EUR','ops@example.test','malformed')`, c.OrganizerID, malformed, otherOrder); err != nil {
			t.Fatal(err)
		}
		var chargeKey string
		if err := db.QueryRowContext(ctx, `SELECT idempotency_key FROM orders WHERE id=$1`, otherOrder).Scan(&chargeKey); err != nil {
			t.Fatal(err)
		}
		forged := RefundOutcomeTarget{OrganizerID: c.OrganizerID, RefundID: malformed, OrderID: otherOrder, Amount: 1000, Currency: "EUR", ChargeKey: chargeKey}
		if _, _, err := RecordAbandoned(ctx, db, abandonedRequest(c, malformed), forged, abandonedProviderRef); !errors.Is(err, ErrRefundOutcomeNotFound) {
			t.Fatalf("locked write: err = %v, want ErrRefundOutcomeNotFound", err)
		}
	})
	t.Run("stale", func(t *testing.T) {
		r := bindPending(t, ctx, db, c, "abandon-stale", 1)
		target, err := InspectRefundOutcomeTarget(ctx, db, c.OrganizerID, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		target.ChargeKey += "-elsewhere"
		if _, _, err := RecordAbandoned(ctx, db, abandonedRequest(c, r.ID), target, abandonedProviderRef); !errors.Is(err, ErrRefundOutcomeStale) {
			t.Fatalf("err = %v, want ErrRefundOutcomeStale", err)
		}
	})
	t.Run("not a provider reference", func(t *testing.T) {
		r := bindPending(t, ctx, db, c, "abandon-bad-ref", 1)
		target, err := InspectRefundOutcomeTarget(ctx, db, c.OrganizerID, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := RecordAbandoned(ctx, db, abandonedRequest(c, r.ID), target, "SEPA-typed-by-hand"); err == nil {
			t.Fatal("an abandoned outcome took a reference that is not the provider's")
		}
	})
	var outcomes int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM refund_outcomes WHERE organizer_id=$1`, c.OrganizerID).Scan(&outcomes); err != nil || outcomes != 0 {
		t.Fatalf("outcomes = %d err=%v, want none", outcomes, err)
	}
}

// Same lock order as the outside completion and CompleteOrderRefund: refund, then order.
func TestRecordAbandonedLocksTheRefundBeforeTheOrder(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 1, 1000)
	r := bindPending(t, ctx, db, c, "abandon-lock-order", 1)
	target, err := InspectRefundOutcomeTarget(ctx, db, c.OrganizerID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx, `SELECT 1 FROM orders WHERE id=$1 FOR UPDATE`, c.OrderID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := RecordAbandoned(ctx, db, abandonedRequest(c, r.ID), target, abandonedProviderRef)
		done <- err
	}()
	waitForLockWait(t, ctx, db, outsideOrderLockSQL, done)
	probe, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, probeErr := probe.ExecContext(ctx, `SELECT 1 FROM order_refunds WHERE organizer_id=$1 AND id=$2 FOR UPDATE NOWAIT`, c.OrganizerID, r.ID)
	_ = probe.Rollback()
	var pg *pgconn.PgError
	if !errors.As(probeErr, &pg) || pg.Code != "55P03" {
		t.Fatalf("the refund row was lockable while the writer waited for the order (err=%v)", probeErr)
	}
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the abandonment failed once the order was released: %v", err)
	}
}
