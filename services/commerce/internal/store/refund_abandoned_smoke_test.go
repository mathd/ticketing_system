//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// TKT-525 S2 (ADR-087): the Commerce half of `abandoned`.
//
// Commerce reserves the abandonment before payments is called, and appends the outcome and
// finalizes the reservation after payments committed its marker. The outcome changes nothing on
// the refund row: the refund stays pending, the order projection, the tickets and the capacity
// keep their state. It releases ONLY that refund's quantity allowance, because the quantity sum
// excludes a refund with a matching `abandoned` outcome.

const abandonedProviderRef = "re_failed_abandoned"

func abandonedRequest(c Completion, refund uuid.UUID) RefundOutcomeRequest {
	return RefundOutcomeRequest{
		OrganizerID: c.OrganizerID, RefundID: refund, Kind: OutcomeAbandoned,
		OperatorName: "Ada Operator", Reason: "buyer confirmed nobody will refund this attempt",
	}
}

// recordAbandoned is the whole Commerce half with payments' answer assumed: reserve, then finish.
func recordAbandoned(t *testing.T, ctx context.Context, db *sql.DB, in RefundOutcomeRequest) (RefundOutcome, bool, error) {
	t.Helper()
	target, err := InspectRefundOutcomeTarget(ctx, db, in.OrganizerID, in.RefundID)
	if err != nil {
		return RefundOutcome{}, false, err
	}
	if _, existing, err := ReserveAbandonment(ctx, db, in, target); err != nil || existing != nil {
		if err != nil {
			return RefundOutcome{}, false, err
		}
		return *existing, true, nil
	}
	return FinishAbandonment(ctx, db, in, target, abandonedProviderRef)
}

func reserve(t *testing.T, ctx context.Context, db *sql.DB, in RefundOutcomeRequest) (AbandonmentDisposition, RefundOutcomeTarget) {
	t.Helper()
	target, err := InspectRefundOutcomeTarget(ctx, db, in.OrganizerID, in.RefundID)
	if err != nil {
		t.Fatal(err)
	}
	d, existing, err := ReserveAbandonment(ctx, db, in, target)
	if err != nil || existing != nil {
		t.Fatalf("reserve: existing=%v err=%v", existing, err)
	}
	return d, target
}

// leaveReversalQueue discharges a completed refund's reversal when the test ends. These tests
// complete refunds only to reach a refund state; left in the shared database, the rows would sit
// in the reversal queue, where the reconciler tests lease them and the claim-plan test then
// counts them as filtered rows.
func leaveReversalQueue(t *testing.T, db *sql.DB, org, refund uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(), `
			UPDATE order_refunds SET tickets_voided_at=coalesce(tickets_voided_at, now()), capacity_returned_at=coalesce(capacity_returned_at, now())
			WHERE organizer_id=$1 AND id=$2 AND status='completed'`, org, refund); err != nil {
			t.Error(err)
		}
	})
}

type dispositionRow struct {
	count int
	state string
}

func readDisposition(t *testing.T, ctx context.Context, db *sql.DB, org, refund uuid.UUID) dispositionRow {
	t.Helper()
	var d dispositionRow
	if err := db.QueryRowContext(ctx, `
		SELECT count(*), coalesce(max(state), '') FROM order_refund_abandonment_dispositions WHERE organizer_id=$1 AND refund_id=$2`,
		org, refund).Scan(&d.count, &d.state); err != nil {
		t.Fatal(err)
	}
	return d
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
	leaveReversalQueue(t, db, c.OrganizerID, outside.ID)
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
func TestAbandonmentReplayAndConflict(t *testing.T) {
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
	// A concurrent finish of the same reservation answers with the original outcome.
	again, replay, err := FinishAbandonment(ctx, db, in, target, "re_another_lookup")
	if err != nil || !replay || again.ExternalRef != first.ExternalRef || !again.RecordedAt.Equal(first.RecordedAt) {
		t.Fatalf("locked replay = %+v replay=%v err=%v, want the original record", again, replay, err)
	}
	if _, existing, err := ReserveAbandonment(ctx, db, in, target); err != nil || existing == nil || !existing.RecordedAt.Equal(first.RecordedAt) {
		t.Fatalf("reserve after the finish: existing=%+v err=%v, want the original record", existing, err)
	}
	for name, mutate := range map[string]func(*RefundOutcomeRequest){
		"reason":   func(x *RefundOutcomeRequest) { x.Reason += "!" },
		"operator": func(x *RefundOutcomeRequest) { x.OperatorName = "Grace Operator" },
	} {
		changed := in
		mutate(&changed)
		if _, _, err := FinishAbandonment(ctx, db, changed, target, abandonedProviderRef); !errors.Is(err, ErrRefundOutcomeConflict) {
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

// An ordinarily completed refund is not eligible; a foreign or missing one is not found; a
// changed row is stale. None writes an outcome or a reservation.
func TestAbandonmentRefusals(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 4, 1000)
	completed := bindPending(t, ctx, db, c, "abandon-completed", 1)
	leaveReversalQueue(t, db, c.OrganizerID, completed.ID)
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
		if _, _, err := ReserveAbandonment(ctx, db, abandonedRequest(c, foreign.ID), forged); !errors.Is(err, ErrRefundOutcomeNotFound) {
			t.Fatalf("locked reservation: err = %v, want ErrRefundOutcomeNotFound", err)
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
		if _, _, err := ReserveAbandonment(ctx, db, abandonedRequest(c, malformed), forged); !errors.Is(err, ErrRefundOutcomeNotFound) {
			t.Fatalf("locked reservation: err = %v, want ErrRefundOutcomeNotFound", err)
		}
	})
	t.Run("stale", func(t *testing.T) {
		r := bindPending(t, ctx, db, c, "abandon-stale", 1)
		target, err := InspectRefundOutcomeTarget(ctx, db, c.OrganizerID, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		target.ChargeKey += "-elsewhere"
		if _, _, err := ReserveAbandonment(ctx, db, abandonedRequest(c, r.ID), target); !errors.Is(err, ErrRefundOutcomeStale) {
			t.Fatalf("err = %v, want ErrRefundOutcomeStale", err)
		}
	})
	t.Run("not a provider reference", func(t *testing.T) {
		r := bindPending(t, ctx, db, c, "abandon-bad-ref", 1)
		_, target := reserve(t, ctx, db, abandonedRequest(c, r.ID))
		for _, ref := range []string{"SEPA-typed-by-hand", "re_", "re_" + strings.Repeat("x", MaxProviderRefundRef-2)} {
			if _, _, err := FinishAbandonment(ctx, db, abandonedRequest(c, r.ID), target, ref); err == nil {
				t.Fatalf("an abandoned outcome took the reference %q", ref)
			}
		}
	})
	t.Run("finish without a reservation", func(t *testing.T) {
		r := bindPending(t, ctx, db, c, "abandon-unreserved", 1)
		target, err := InspectRefundOutcomeTarget(ctx, db, c.OrganizerID, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := FinishAbandonment(ctx, db, abandonedRequest(c, r.ID), target, abandonedProviderRef); !errors.Is(err, ErrRefundOutcomeStale) {
			t.Fatalf("err = %v, want ErrRefundOutcomeStale", err)
		}
	})
	var outcomes, reservations int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM refund_outcomes WHERE organizer_id=$1),
		(SELECT count(*) FROM order_refund_abandonment_dispositions WHERE organizer_id=$1 AND state='finalized')`, c.OrganizerID).Scan(&outcomes, &reservations); err != nil || outcomes != 0 || reservations != 0 {
		t.Fatalf("outcomes = %d finalized reservations = %d err=%v, want none", outcomes, reservations, err)
	}
}

// The command writes lock the refund row and then its order, CompleteOrderRefund's order
// (0027's lock invariant). With the order row held elsewhere, each writer must already hold its
// refund row while it waits on the order. The finish's order lock is also what a fresh
// cancellation bind relies on to see an abandonment entirely or not at all.
func TestCommandWritesLockTheRefundBeforeTheOrder(t *testing.T) {
	for name, write := range map[string]func(ctx context.Context, db *sql.DB, c Completion, refund uuid.UUID, target RefundOutcomeTarget) error{
		"outside completion": func(ctx context.Context, db *sql.DB, c Completion, refund uuid.UUID, target RefundOutcomeTarget) error {
			_, _, err := RecordRefundedOutside(ctx, db, outsideRequest(c, refund), target)
			return err
		},
		"abandonment reservation": func(ctx context.Context, db *sql.DB, c Completion, refund uuid.UUID, target RefundOutcomeTarget) error {
			_, _, err := ReserveAbandonment(ctx, db, abandonedRequest(c, refund), target)
			return err
		},
		"abandonment finish": func(ctx context.Context, db *sql.DB, c Completion, refund uuid.UUID, target RefundOutcomeTarget) error {
			_, _, err := FinishAbandonment(ctx, db, abandonedRequest(c, refund), target, abandonedProviderRef)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			db, ctx := outboxDB(t)
			c, _ := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 1, 1000)
			r := bindPending(t, ctx, db, c, "lock-order", 1)
			leaveReversalQueue(t, db, c.OrganizerID, r.ID)
			if name == "abandonment finish" {
				reserve(t, ctx, db, abandonedRequest(c, r.ID))
			}
			target, err := InspectRefundOutcomeTarget(ctx, db, c.OrganizerID, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			assertLocksRefundBeforeOrder(t, ctx, db, c, r.ID, func() error { return write(ctx, db, c, r.ID, target) })
		})
	}
}

// assertLocksRefundBeforeOrder holds the order row, starts write, waits until write blocks on
// the order lock statement, and requires the refund row to be locked by then.
func assertLocksRefundBeforeOrder(t *testing.T, ctx context.Context, db *sql.DB, c Completion, refund uuid.UUID, write func() error) {
	t.Helper()
	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx, `SELECT 1 FROM orders WHERE id=$1 FOR UPDATE`, c.OrderID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- write() }()
	waitForLockWait(t, ctx, db, outsideOrderLockSQL, done)
	probe, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, probeErr := probe.ExecContext(ctx, `SELECT 1 FROM order_refunds WHERE organizer_id=$1 AND id=$2 FOR UPDATE NOWAIT`, c.OrganizerID, refund)
	_ = probe.Rollback()
	var pg *pgconn.PgError
	if !errors.As(probeErr, &pg) || pg.Code != "55P03" {
		t.Fatalf("the refund row was lockable while the writer waited for the order (err=%v). "+
			"It locks the order first, against CompleteOrderRefund's refund-then-order order", probeErr)
	}
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the write failed once the order was released: %v", err)
	}
}

// The reservation: inserted pending with the refund's stored charge key and money and the
// command's attribution; resumed only by the exact same command; refused for an ineligible
// refund. A reservation on a refund that ordinary completion has since completed is inert.
func TestReserveAbandonmentInsertsResumesAndConflicts(t *testing.T) {
	db, ctx := outboxDB(t)
	c, order := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 2, 1000)
	r := bindPending(t, ctx, db, c, "reserve", 2)
	leaveReversalQueue(t, db, c.OrganizerID, r.ID)
	in := abandonedRequest(c, r.ID)
	d, target := reserve(t, ctx, db, in)
	if d.State != DispositionPending || d.ChargeKey != target.ChargeKey || d.Amount != 2000 || d.Currency != "EUR" ||
		d.OperatorName != in.OperatorName || d.Reason != in.Reason || d.ReservedAt.IsZero() {
		t.Fatalf("reservation = %+v", d)
	}
	again, existing, err := ReserveAbandonment(ctx, db, in, target)
	if err != nil || existing != nil || again != d {
		t.Fatalf("exact resume = %+v existing=%v err=%v, want the same reservation", again, existing, err)
	}
	for name, mutate := range map[string]func(*RefundOutcomeRequest){
		"reason":   func(x *RefundOutcomeRequest) { x.Reason += "!" },
		"operator": func(x *RefundOutcomeRequest) { x.OperatorName = "Grace Operator" },
	} {
		changed := in
		mutate(&changed)
		if _, _, err := ReserveAbandonment(ctx, db, changed, target); !errors.Is(err, ErrRefundOutcomeConflict) {
			t.Fatalf("a resume with a different %s: err = %v, want conflict", name, err)
		}
	}
	if got := readDisposition(t, ctx, db, c.OrganizerID, r.ID); got != (dispositionRow{1, DispositionPending}) {
		t.Fatalf("reservations = %+v, want one pending", got)
	}
	// The reservation alone releases nothing: the quantity stays reserved.
	if _, err := BindOrderRefund(ctx, db, request(c, "reserve-probe", 1)); !errors.Is(err, ErrRefundExceedsOrder) {
		t.Fatalf("a pending reservation released quantity: err = %v", err)
	}
	if p := readProjection(t, ctx, db, order); p != (projection{0, 0, "none"}) {
		t.Fatalf("projection = %+v, want untouched", p)
	}

	// Ordinary completion lands after the reservation: the reservation is inert.
	if err := CompleteOrderRefund(ctx, db, c.OrganizerID, r.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReserveAbandonment(ctx, db, in, target); !errors.Is(err, ErrRefundOutcomeIneligible) {
		t.Fatalf("resume after ordinary completion: err = %v, want ErrRefundOutcomeIneligible", err)
	}
	if _, _, err := FinishAbandonment(ctx, db, in, target, abandonedProviderRef); !errors.Is(err, ErrRefundOutcomeIneligible) {
		t.Fatalf("finish after ordinary completion: err = %v, want ErrRefundOutcomeIneligible", err)
	}
	if n := countRefundOutcomes(t, ctx, db, c.OrganizerID, r.ID); n != 0 {
		t.Fatalf("outcomes = %d", n)
	}
}

func countRefundOutcomes(t *testing.T, ctx context.Context, db *sql.DB, org, refund uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM refund_outcomes WHERE organizer_id=$1 AND subject_id=$2`, org, refund).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The finish appends the outcome with the reserved attribution and finalizes the reservation in
// the same commit. A 255-byte provider reference, the longest payments accepts, is stored.
func TestFinishAbandonmentFinalizesWithTheOutcome(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 1, 1000)
	r := bindPending(t, ctx, db, c, "finish", 1)
	in := abandonedRequest(c, r.ID)
	_, target := reserve(t, ctx, db, in)
	long := "re_" + strings.Repeat("x", MaxProviderRefundRef-3)
	got, replay, err := FinishAbandonment(ctx, db, in, target, long)
	if err != nil || replay || got.ExternalRef != long || got.OperatorName != in.OperatorName || got.Reason != in.Reason {
		t.Fatalf("finish = %+v replay=%v err=%v", got, replay, err)
	}
	if d := readDisposition(t, ctx, db, c.OrganizerID, r.ID); d != (dispositionRow{1, DispositionFinalized}) {
		t.Fatalf("reservation after the finish = %+v, want one finalized", d)
	}
	if _, err := BindOrderRefund(ctx, db, request(c, "finish-probe", 1)); err != nil {
		t.Fatalf("the finished abandonment did not release its unit: %v", err)
	}
}

// Outside completion refuses a refund with a reservation, pending or finalized, under its locks:
// the target here was inspected BEFORE the reservation committed, as when the proof was in
// flight. Nothing changes.
func TestRecordRefundedOutsideRefusesAReservedRefund(t *testing.T) {
	db, ctx := outboxDB(t)
	c, order := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 1, 1000)
	r := bindPending(t, ctx, db, c, "outside-after-reserve", 1)
	inspectedBefore, err := InspectRefundOutcomeTarget(ctx, db, c.OrganizerID, r.ID)
	if err != nil || inspectedBefore.Disposition != nil {
		t.Fatalf("inspect = %+v err=%v", inspectedBefore, err)
	}
	reserve(t, ctx, db, abandonedRequest(c, r.ID))
	if _, _, err := RecordRefundedOutside(ctx, db, outsideRequest(c, r.ID), inspectedBefore); !errors.Is(err, ErrAbandonmentReserved) {
		t.Fatalf("outside completion over a pending reservation: err = %v, want ErrAbandonmentReserved", err)
	}
	if n := countRefundOutcomes(t, ctx, db, c.OrganizerID, r.ID); n != 0 {
		t.Fatalf("outcomes = %d", n)
	}
	if p := readProjection(t, ctx, db, order); p != (projection{0, 0, "none"}) {
		t.Fatalf("projection = %+v, want untouched", p)
	}
	inspectedAfter, err := InspectRefundOutcomeTarget(ctx, db, c.OrganizerID, r.ID)
	if err != nil || inspectedAfter.Disposition == nil || inspectedAfter.Disposition.State != DispositionPending {
		t.Fatalf("inspection does not report the reservation: %+v err=%v", inspectedAfter, err)
	}
}

// The reservation refuses a refund an outside completion already recorded, under its locks: the
// target here was inspected BEFORE the outside completion committed. No reservation is written.
func TestReserveAbandonmentRefusesAnOutsideOutcome(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 1, 1000)
	r := bindPending(t, ctx, db, c, "reserve-after-outside", 1)
	leaveReversalQueue(t, db, c.OrganizerID, r.ID)
	inspectedBefore, err := InspectRefundOutcomeTarget(ctx, db, c.OrganizerID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := recordOutside(t, ctx, db, outsideRequest(c, r.ID)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReserveAbandonment(ctx, db, abandonedRequest(c, r.ID), inspectedBefore); !errors.Is(err, ErrRefundOutcomeConflict) {
		t.Fatalf("reservation after an outside outcome: err = %v, want ErrRefundOutcomeConflict", err)
	}
	if d := readDisposition(t, ctx, db, c.OrganizerID, r.ID); d.count != 0 {
		t.Fatalf("reservations = %+v, want none", d)
	}
}

// A FRESH cancellation bind (a new key in the reserved namespace) on an order with any recorded
// abandoned outcome is refused, whatever the remaining quantity: three units, an earlier
// cancellation refund of one, staff R1 of one abandoned, and staff R2 of one replacing it, first
// pending and then completed. The refusal comes before the ceiling. A pending reservation alone
// does not trigger it, a valid same-key cancellation replay keeps its behavior, and a staff key
// gets only the ordinary ceiling.
func TestFreshCancellationBindRefusesAnOrderWithAnAbandonedRefund(t *testing.T) {
	db, ctx := outboxDB(t)
	c, _ := seedCompleted(t, db, ctx, "charge-"+uuid.NewString(), 3, 1000)
	slot := uuid.New()
	earlierCancel := bindPending(t, ctx, db, c, CancellationRefundKey(slot, c.OrderID), 1)
	r1 := bindPending(t, ctx, db, c, "staff-r1", 1)

	reserve(t, ctx, db, abandonedRequest(c, r1.ID))
	if _, err := BindOrderRefund(ctx, db, request(c, CancellationRefundKey(uuid.New(), c.OrderID), 2)); !errors.Is(err, ErrRefundExceedsOrder) {
		t.Fatalf("with only a pending reservation: err = %v, want the ordinary ceiling", err)
	}
	if _, _, err := recordAbandoned(t, ctx, db, abandonedRequest(c, r1.ID)); err != nil {
		t.Fatal(err)
	}

	r2 := bindPending(t, ctx, db, c, "staff-r2", 1)
	leaveReversalQueue(t, db, c.OrganizerID, r2.ID)
	fresh := CancellationRefundKey(uuid.New(), c.OrderID)
	// One unit remains: the ceiling would admit this bind.
	if _, err := BindOrderRefund(ctx, db, request(c, fresh, 1)); !errors.Is(err, ErrOrderHasAbandonedRefund) {
		t.Fatalf("fresh cancellation bind with R2 pending: err = %v, want ErrOrderHasAbandonedRefund", err)
	}
	// Two units would exceed the ceiling: the abandoned refusal still answers first.
	if _, err := BindOrderRefund(ctx, db, request(c, fresh, 2)); !errors.Is(err, ErrOrderHasAbandonedRefund) {
		t.Fatalf("fresh cancellation bind over the ceiling: err = %v, want ErrOrderHasAbandonedRefund", err)
	}
	if err := CompleteOrderRefund(ctx, db, c.OrganizerID, r2.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := BindOrderRefund(ctx, db, request(c, fresh, 1)); !errors.Is(err, ErrOrderHasAbandonedRefund) {
		t.Fatalf("fresh cancellation bind after the staff replacement: err = %v, want ErrOrderHasAbandonedRefund", err)
	}
	if replay, err := BindOrderRefund(ctx, db, request(c, CancellationRefundKey(slot, c.OrderID), 1)); err != nil || replay.ID != earlierCancel.ID {
		t.Fatalf("same-key cancellation replay = %+v err=%v, want the earlier refund", replay, err)
	}
	if _, err := BindOrderRefund(ctx, db, request(c, "staff-r3", 1)); err != nil {
		t.Fatalf("a staff refund of the remaining unit: %v", err)
	}
	var cancels int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM order_refunds WHERE order_id=$1 AND idempotency_key LIKE 'cancel:%'`, c.OrderID).Scan(&cancels); err != nil || cancels != 1 {
		t.Fatalf("cancellation refunds = %d err=%v, want only the earlier one", cancels, err)
	}
}
