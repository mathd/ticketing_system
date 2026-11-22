//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TKT-498. The Commerce store layer of the abandonment command: the scoped load, the locked
// reservation, the progress writes, the terminal transaction, the backlog predicate and the
// refund binder's marker check. The fixture is seedAbandonable (exchange_abandonment_smoke_test.go).

const (
	adaOperator = "Ada Operator"
	adaReason   = "source admitted before the switch"
)

func adaAttribution(t *testing.T) AbandonmentAttribution {
	t.Helper()
	a, err := NewAbandonmentAttribution(adaOperator, adaReason)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func loadSettled(t *testing.T, db *sql.DB, ctx context.Context, f abandonFixture) SettledExchange {
	t.Helper()
	e, err := LoadSettledExchange(ctx, db, f.Org, f.ExchangeID)
	if err != nil {
		t.Fatalf("load exchange %s: %v", f.ExchangeID, err)
	}
	return e
}

// reserveAbandonment loads the exchange and reserves it with no replacement refunds inspected.
func reserveAbandonment(t *testing.T, db *sql.DB, ctx context.Context, f abandonFixture) SettledExchange {
	t.Helper()
	e, fresh, err := ReserveExchangeAbandonment(ctx, db, AbandonmentReservation{
		Expected: loadSettled(t, db, ctx, f), Attribution: adaAttribution(t)})
	if err != nil || !fresh {
		t.Fatalf("reserve exchange %s: fresh=%v err=%v", f.ExchangeID, fresh, err)
	}
	return e
}

// startedRow reads the reservation columns directly, so the assertions do not depend on the
// code under test to read them back.
type startedRow struct {
	StartedAt                     sql.NullTime
	Operator, SessionUser, Reason sql.NullString
	RefundFact                    uuid.NullUUID
	FailedRef                     sql.NullString
	ReturnedAt, AbandonedAt       sql.NullTime
	WrittenOff                    sql.NullInt64
}

func readStarted(t *testing.T, db *sql.DB, ctx context.Context, org, id uuid.UUID) startedRow {
	t.Helper()
	var r startedRow
	if err := db.QueryRowContext(ctx, `
		SELECT abandonment_started_at, abandonment_operator_name, abandonment_session_user, abandonment_reason,
		       abandonment_refund_fact_id, abandonment_refund_failed_ref, abandonment_target_returned_at,
		       abandoned_at, abandonment_written_off_amount
		FROM order_exchanges WHERE organizer_id=$1 AND id=$2`, org, id).
		Scan(&r.StartedAt, &r.Operator, &r.SessionUser, &r.Reason, &r.RefundFact, &r.FailedRef,
			&r.ReturnedAt, &r.AbandonedAt, &r.WrittenOff); err != nil {
		t.Fatal(err)
	}
	return r
}

// ---------------------------------------------------------------------------------------------
// Load and fresh eligibility.

// The load is scoped by (organizer, exchange): each side of a same-ID pair reads its own row,
// and an organizer with no such exchange reads nothing.
func TestLoadSettledExchangeIsOrganizerScoped(t *testing.T) {
	db, ctx := outboxDB(t)
	shared := uuid.New()
	a := seedAbandonable(t, db, ctx, abandonSeedOpts{ExchangeID: shared})
	b := seedAbandonable(t, db, ctx, abandonSeedOpts{ExchangeID: shared, TargetUnit: 1000})
	for _, f := range []abandonFixture{a, b} {
		e := loadSettled(t, db, ctx, f)
		if e.SourceOrderID != f.SourceOrder || e.ReplacementOrderID != f.ReplacementOrder || e.DeltaAmount != f.Delta {
			t.Fatalf("organizer %s loaded source %s replacement %s delta %d, want its own %s %s %d",
				f.Org, e.SourceOrderID, e.ReplacementOrderID, e.DeltaAmount, f.SourceOrder, f.ReplacementOrder, f.Delta)
		}
		if err := e.FreshEligibility(); err != nil {
			t.Fatalf("a consistent candidate is not eligible: %v", err)
		}
	}
	if _, err := LoadSettledExchange(ctx, db, uuid.New(), shared); !errors.Is(err, ErrExchangeNotFound) {
		t.Fatalf("an organizer with no such exchange loaded err=%v, want ErrExchangeNotFound", err)
	}
}

// Fresh eligibility refuses an unsettled exchange, a switched one and each inconsistency, each
// with its own error.
func TestFreshEligibilityRefusesEachIneligibleShape(t *testing.T) {
	db, ctx := outboxDB(t)
	unsettled := seedAbandonable(t, db, ctx, abandonSeedOpts{Unsettled: true})
	if err := loadSettled(t, db, ctx, unsettled).FreshEligibility(); !errors.Is(err, ErrExchangeNotSettled) {
		t.Fatalf("unsettled: err=%v, want ErrExchangeNotSettled", err)
	}
	switched := seedAbandonable(t, db, ctx, abandonSeedOpts{})
	if err := MarkExchangeTicketsSwitched(ctx, db, switched.Org, switched.ExchangeID); err != nil {
		t.Fatal(err)
	}
	if err := loadSettled(t, db, ctx, switched).FreshEligibility(); !errors.Is(err, ErrExchangeAlreadySwitched) {
		t.Fatalf("switched: err=%v, want ErrExchangeAlreadySwitched", err)
	}
	for _, c := range inconsistencyCases {
		t.Run(c.name, func(t *testing.T) {
			f := seedAbandonable(t, db, ctx, abandonSeedOpts{})
			c.apply(t, db, ctx, f)
			if err := loadSettled(t, db, ctx, f).FreshEligibility(); !errors.Is(err, ErrExchangeAbandonmentInconsistent) {
				t.Fatalf("err=%v, want ErrExchangeAbandonmentInconsistent", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------
// Attribution.

func TestAbandonmentAttributionIsTrimmedAndBounded(t *testing.T) {
	a, err := NewAbandonmentAttribution("  Ada Operator\t", "\n source admitted  ")
	if err != nil || a.Operator != "Ada Operator" || a.Reason != "source admitted" {
		t.Fatalf("attribution = %+v err=%v, want the trimmed pair", a, err)
	}
	// Lengths count characters, as the database CHECK does: 200 two-byte characters fit.
	if _, err := NewAbandonmentAttribution(strings.Repeat("é", 200), strings.Repeat("é", 500)); err != nil {
		t.Fatalf("200 and 500 characters were refused: %v", err)
	}
	for _, c := range []struct{ operator, reason, want string }{
		{"   ", "r", "operator"},
		{strings.Repeat("a", 201), "r", "operator"},
		{"Ada", "\t", "reason"},
		{"Ada", strings.Repeat("r", 501), "reason"},
	} {
		_, err := NewAbandonmentAttribution(c.operator, c.reason)
		if !errors.Is(err, ErrAbandonmentAttributionInvalid) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("operator %d chars, reason %d chars: err=%v, want ErrAbandonmentAttributionInvalid naming %s",
				len(c.operator), len(c.reason), err, c.want)
		}
	}
}

// ---------------------------------------------------------------------------------------------
// The reservation.

// The reservation records the trimmed pair and the database login of the writing connection.
func TestReserveRecordsTheTrimmedPairAndTheSessionUser(t *testing.T) {
	db, ctx := outboxDB(t)
	f := seedAbandonable(t, db, ctx, abandonSeedOpts{})
	a, err := NewAbandonmentAttribution("  "+adaOperator+" ", adaReason+"\n")
	if err != nil {
		t.Fatal(err)
	}
	e, fresh, err := ReserveExchangeAbandonment(ctx, db, AbandonmentReservation{Expected: loadSettled(t, db, ctx, f), Attribution: a})
	if err != nil || !fresh {
		t.Fatalf("fresh=%v err=%v, want a fresh reservation", fresh, err)
	}
	var login string
	if err := db.QueryRowContext(ctx, `SELECT session_user`).Scan(&login); err != nil {
		t.Fatal(err)
	}
	r := readStarted(t, db, ctx, f.Org, f.ExchangeID)
	if !r.StartedAt.Valid || r.Operator.String != adaOperator || r.Reason.String != adaReason || r.SessionUser.String != login {
		t.Fatalf("stored started=%v operator=%q reason=%q session_user=%q, want %q %q %q",
			r.StartedAt.Valid, r.Operator.String, r.Reason.String, r.SessionUser.String, adaOperator, adaReason, login)
	}
	if !e.AbandonmentStarted || e.AbandonmentOperator != adaOperator || e.AbandonmentSessionUser != login {
		t.Fatalf("returned row started=%v operator=%q session_user=%q", e.AbandonmentStarted, e.AbandonmentOperator, e.AbandonmentSessionUser)
	}
}

// The same trimmed pair resumes without rewriting anything; a different operator or a different
// reason conflicts, each on its own.
func TestReserveResumesTheSamePairAndRefusesAnother(t *testing.T) {
	db, ctx := outboxDB(t)
	f := seedAbandonable(t, db, ctx, abandonSeedOpts{})
	reserveAbandonment(t, db, ctx, f)
	before := readStarted(t, db, ctx, f.Org, f.ExchangeID)

	same, err := NewAbandonmentAttribution(adaOperator+"  ", "  "+adaReason)
	if err != nil {
		t.Fatal(err)
	}
	e, fresh, err := ReserveExchangeAbandonment(ctx, db, AbandonmentReservation{Expected: loadSettled(t, db, ctx, f), Attribution: same})
	if err != nil || fresh || !e.AbandonmentStartedAt.Equal(before.StartedAt.Time) {
		t.Fatalf("same pair: fresh=%v err=%v started=%s, want a resume of the reservation at %s",
			fresh, err, e.AbandonmentStartedAt, before.StartedAt.Time)
	}
	for _, other := range []AbandonmentAttribution{
		{Operator: "Grace Operator", Reason: adaReason},
		{Operator: adaOperator, Reason: "another reason"},
	} {
		_, _, err := ReserveExchangeAbandonment(ctx, db, AbandonmentReservation{Expected: loadSettled(t, db, ctx, f), Attribution: other})
		if !errors.Is(err, ErrAbandonmentAttributionConflict) {
			t.Fatalf("pair %+v: err=%v, want ErrAbandonmentAttributionConflict", other, err)
		}
	}
	if after := readStarted(t, db, ctx, f.Org, f.ExchangeID); after != before {
		t.Fatalf("a resume or a conflict changed the reservation: %+v -> %+v", before, after)
	}
}

// A switch marker committed after the load and before the reservation is seen under the lock.
func TestReserveRechecksTheSwitchUnderTheLock(t *testing.T) {
	db, ctx := outboxDB(t)
	f := seedAbandonable(t, db, ctx, abandonSeedOpts{})
	expected := loadSettled(t, db, ctx, f)
	if err := expected.FreshEligibility(); err != nil {
		t.Fatal(err)
	}
	if err := MarkExchangeTicketsSwitched(ctx, db, f.Org, f.ExchangeID); err != nil {
		t.Fatal(err)
	}
	_, _, err := ReserveExchangeAbandonment(ctx, db, AbandonmentReservation{Expected: expected, Attribution: adaAttribution(t)})
	if !errors.Is(err, ErrExchangeAlreadySwitched) {
		t.Fatalf("err=%v, want ErrExchangeAlreadySwitched from the locked recheck", err)
	}
	if r := readStarted(t, db, ctx, f.Org, f.ExchangeID); r.StartedAt.Valid {
		t.Fatal("a switched exchange was reserved")
	}
}

// Each inconsistency introduced after the load is refused under the lock.
func TestReserveRechecksConsistencyUnderTheLock(t *testing.T) {
	db, ctx := outboxDB(t)
	for _, c := range inconsistencyCases {
		t.Run(c.name, func(t *testing.T) {
			f := seedAbandonable(t, db, ctx, abandonSeedOpts{})
			expected := loadSettled(t, db, ctx, f)
			c.apply(t, db, ctx, f)
			_, _, err := ReserveExchangeAbandonment(ctx, db, AbandonmentReservation{Expected: expected, Attribution: adaAttribution(t)})
			if !errors.Is(err, ErrExchangeAbandonmentInconsistent) {
				t.Fatalf("err=%v, want ErrExchangeAbandonmentInconsistent", err)
			}
			if r := readStarted(t, db, ctx, f.Org, f.ExchangeID); r.StartedAt.Valid {
				t.Fatal("an inconsistent exchange was reserved")
			}
		})
	}
}

// The Access proofs and the refund inspection were made for the loaded identities. A change of
// the target hold after the load, kept consistent on both rows, is refused as stale evidence.
func TestReserveRefusesIdentitiesThatChangedSinceTheLoad(t *testing.T) {
	db, ctx := outboxDB(t)
	f := seedAbandonable(t, db, ctx, abandonSeedOpts{})
	expected := loadSettled(t, db, ctx, f)
	moved := uuid.New()
	mustExec(t, db, ctx, `UPDATE reservations SET hold_id=$2 WHERE id=$1`, f.ReplacementReservation, moved)
	mustExec(t, db, ctx, `UPDATE order_exchanges SET target_hold_id=$3 WHERE organizer_id=$1 AND id=$2`, f.Org, f.ExchangeID, moved)
	_, _, err := ReserveExchangeAbandonment(ctx, db, AbandonmentReservation{Expected: expected, Attribution: adaAttribution(t)})
	if !errors.Is(err, ErrAbandonmentEvidenceStale) {
		t.Fatalf("err=%v, want ErrAbandonmentEvidenceStale", err)
	}
}

// bindReplacementRefund binds a staff refund of the replacement order through the real binder.
func bindReplacementRefund(t *testing.T, db *sql.DB, ctx context.Context, f abandonFixture, key string) Refund {
	t.Helper()
	r, err := BindOrderRefund(ctx, db, RefundRequest{OrderID: f.ReplacementOrder, OrganizerID: f.Org, Quantity: 1,
		IdempotencyKey: key, Actor: "support@example.test", Reason: "target slot cancelled"})
	if err != nil {
		t.Fatalf("bind replacement refund: %v", err)
	}
	return r
}

// The caller inspected the replacement refunds against payments before reserving. Under the
// lock the set must be exactly the one inspected, with the same states.
func TestReserveRefusesAReplacementRefundSetThatChanged(t *testing.T) {
	db, ctx := outboxDB(t)

	t.Run("the inspected pending refund is still the whole set", func(t *testing.T) {
		f := seedAbandonable(t, db, ctx, abandonSeedOpts{})
		r := bindReplacementRefund(t, db, ctx, f, "cancel-1")
		_, fresh, err := ReserveExchangeAbandonment(ctx, db, AbandonmentReservation{
			Expected: loadSettled(t, db, ctx, f), Attribution: adaAttribution(t),
			InspectedRefunds: []ReplacementRefund{{ID: r.ID, Status: "pending"}}})
		if err != nil || !fresh {
			t.Fatalf("fresh=%v err=%v, want the reservation", fresh, err)
		}
	})
	t.Run("a refund bound after the inspection", func(t *testing.T) {
		f := seedAbandonable(t, db, ctx, abandonSeedOpts{})
		expected := loadSettled(t, db, ctx, f)
		bindReplacementRefund(t, db, ctx, f, "cancel-1")
		_, _, err := ReserveExchangeAbandonment(ctx, db, AbandonmentReservation{Expected: expected, Attribution: adaAttribution(t)})
		if !errors.Is(err, ErrReplacementRefundSetChanged) {
			t.Fatalf("err=%v, want ErrReplacementRefundSetChanged", err)
		}
	})
	t.Run("an inspected refund that completed", func(t *testing.T) {
		f := seedAbandonable(t, db, ctx, abandonSeedOpts{})
		r := bindReplacementRefund(t, db, ctx, f, "cancel-1")
		mustExec(t, db, ctx, `UPDATE order_refunds SET status='completed', completed_at=now(), payment_fact_id=gen_random_uuid()
			WHERE organizer_id=$1 AND id=$2`, f.Org, r.ID)
		_, _, err := ReserveExchangeAbandonment(ctx, db, AbandonmentReservation{
			Expected: loadSettled(t, db, ctx, f), Attribution: adaAttribution(t),
			InspectedRefunds: []ReplacementRefund{{ID: r.ID, Status: "pending"}}})
		if !errors.Is(err, ErrReplacementRefundSetChanged) {
			t.Fatalf("err=%v, want ErrReplacementRefundSetChanged", err)
		}
	})
	t.Run("a completed refund inspected as completed", func(t *testing.T) {
		f := seedAbandonable(t, db, ctx, abandonSeedOpts{})
		r := bindReplacementRefund(t, db, ctx, f, "cancel-1")
		mustExec(t, db, ctx, `UPDATE order_refunds SET status='completed', completed_at=now(), payment_fact_id=gen_random_uuid()
			WHERE organizer_id=$1 AND id=$2`, f.Org, r.ID)
		_, _, err := ReserveExchangeAbandonment(ctx, db, AbandonmentReservation{
			Expected: loadSettled(t, db, ctx, f), Attribution: adaAttribution(t),
			InspectedRefunds: []ReplacementRefund{{ID: r.ID, Status: "completed"}}})
		if !errors.Is(err, ErrReplacementRefundCompleted) {
			t.Fatalf("err=%v, want ErrReplacementRefundCompleted", err)
		}
	})
	t.Run("a refund row under another organizer is not this organizer's", func(t *testing.T) {
		f := seedAbandonable(t, db, ctx, abandonSeedOpts{})
		mustExec(t, db, ctx, `
			INSERT INTO order_refunds(organizer_id,id,order_id,idempotency_key,request_fingerprint,quantity,unit_amount,amount,currency,actor,reason)
			VALUES($1,$2,$3,'foreign','fp',1,2000,2000,'EUR','a','r')`, uuid.New(), uuid.New(), f.ReplacementOrder)
		refunds, err := ListReplacementRefunds(ctx, db, f.Org, f.ReplacementOrder)
		if err != nil || len(refunds) != 0 {
			t.Fatalf("listed %v err=%v, want no refund of this organizer", refunds, err)
		}
		if _, fresh, err := ReserveExchangeAbandonment(ctx, db, AbandonmentReservation{
			Expected: loadSettled(t, db, ctx, f), Attribution: adaAttribution(t)}); err != nil || !fresh {
			t.Fatalf("fresh=%v err=%v, want the reservation", fresh, err)
		}
	})
}

// The order locks are FOR NO KEY UPDATE. A transaction holding a key-share lock on the
// replacement order (what the completion outbox and order_refunds foreign keys take) must not
// block the reservation; FOR UPDATE would wait on it, and a settlement replay holding the
// exchange row would then deadlock with the reservation.
func TestReserveDoesNotWaitOnAKeyShareLockOfTheReplacementOrder(t *testing.T) {
	db, ctx := outboxDB(t)
	f := seedAbandonable(t, db, ctx, abandonSeedOpts{})
	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx, `SELECT 1 FROM orders WHERE id=$1 FOR KEY SHARE`, f.ReplacementOrder); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, fresh, err := ReserveExchangeAbandonment(short, db, AbandonmentReservation{
		Expected: loadSettled(t, db, ctx, f), Attribution: adaAttribution(t)}); err != nil || !fresh {
		t.Fatalf("the reservation did not complete while a key-share lock was held: fresh=%v err=%v", fresh, err)
	}
}

// Two identical reservations queued behind the source order lock: exactly one reserves, the
// other resumes it.
func TestConcurrentIdenticalReservationsReserveOnce(t *testing.T) {
	db, ctx := outboxDB(t)
	f := seedAbandonable(t, db, ctx, abandonSeedOpts{})
	expected := loadSettled(t, db, ctx, f)
	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback() }()
	if _, err := blocker.ExecContext(ctx, `SELECT 1 FROM orders WHERE id=$1 FOR UPDATE`, f.SourceOrder); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, fresh, err := ReserveExchangeAbandonment(ctx, db, AbandonmentReservation{Expected: expected, Attribution: adaAttribution(t)})
			results <- fresh
			errs <- err
		}()
	}
	waitForReservationWaiters(t, db, ctx, 2)
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a queued identical reservation failed: %v", err)
		}
	}
	fresh := 0
	for r := range results {
		if r {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("%d reservations reported fresh, want exactly 1", fresh)
	}
}

func waitForReservationWaiters(t *testing.T, db *sql.DB, ctx context.Context, n int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND state = 'active'
			  AND query LIKE '%FOR NO KEY UPDATE%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fewer than %d reservations ever waited on the source order lock", n)
}

// ---------------------------------------------------------------------------------------------
// Progress writes.

func TestRecordAbandonmentRefundFillsOnceAndReadsBackTheSameFact(t *testing.T) {
	db, ctx := outboxDB(t)
	f := seedAbandonable(t, db, ctx, abandonSeedOpts{TargetUnit: 2000})
	fact := uuid.New()
	if err := RecordExchangeAbandonmentRefund(ctx, db, f.Org, f.ExchangeID, fact); !errors.Is(err, ErrAbandonmentNotStarted) {
		t.Fatalf("before the reservation: err=%v, want ErrAbandonmentNotStarted", err)
	}
	reserveAbandonment(t, db, ctx, f)
	if err := RecordExchangeAbandonmentRefund(ctx, db, f.Org, f.ExchangeID, fact); err != nil {
		t.Fatalf("record the refund: %v", err)
	}
	if err := RecordExchangeAbandonmentRefund(ctx, db, f.Org, f.ExchangeID, fact); err != nil {
		t.Fatalf("the same fact again: %v, want the identical result read back", err)
	}
	if err := RecordExchangeAbandonmentRefund(ctx, db, f.Org, f.ExchangeID, uuid.New()); !errors.Is(err, ErrAbandonmentProgressConflict) {
		t.Fatalf("another fact: err=%v, want ErrAbandonmentProgressConflict", err)
	}
	if err := RecordExchangeAbandonmentRefundFailure(ctx, db, f.Org, f.ExchangeID, "re_123"); !errors.Is(err, ErrAbandonmentProgressConflict) {
		t.Fatalf("a failure after the refund: err=%v, want ErrAbandonmentProgressConflict", err)
	}
	if r := readStarted(t, db, ctx, f.Org, f.ExchangeID); r.RefundFact.UUID != fact || r.FailedRef.Valid {
		t.Fatalf("stored refund %v failure %v, want %s and none", r.RefundFact, r.FailedRef, fact)
	}
	if err := RecordExchangeAbandonmentRefund(ctx, db, f.Org, f.ExchangeID, uuid.Nil); err == nil {
		t.Fatal("a nil refund fact was accepted")
	}
}

func TestRecordAbandonmentRefundFailureFillsOnce(t *testing.T) {
	db, ctx := outboxDB(t)
	f := seedAbandonable(t, db, ctx, abandonSeedOpts{TargetUnit: 2000})
	reserveAbandonment(t, db, ctx, f)
	for _, bad := range []string{"", "pi_123", "re_", "re_" + strings.Repeat("x", 253)} {
		if err := RecordExchangeAbandonmentRefundFailure(ctx, db, f.Org, f.ExchangeID, bad); !errors.Is(err, ErrInvalidProviderRefundRef) {
			t.Fatalf("reference %q: err=%v, want ErrInvalidProviderRefundRef", bad, err)
		}
	}
	if err := RecordExchangeAbandonmentRefundFailure(ctx, db, f.Org, f.ExchangeID, "re_3Pq"); err != nil {
		t.Fatalf("record the failure: %v", err)
	}
	if err := RecordExchangeAbandonmentRefundFailure(ctx, db, f.Org, f.ExchangeID, "re_3Pq"); err != nil {
		t.Fatalf("the same failure again: %v", err)
	}
	if err := RecordExchangeAbandonmentRefundFailure(ctx, db, f.Org, f.ExchangeID, "re_other"); !errors.Is(err, ErrAbandonmentProgressConflict) {
		t.Fatalf("another reference: err=%v, want ErrAbandonmentProgressConflict", err)
	}
	if err := RecordExchangeAbandonmentRefund(ctx, db, f.Org, f.ExchangeID, uuid.New()); !errors.Is(err, ErrAbandonmentRefundFailed) {
		t.Fatalf("a refund after the failure: err=%v, want ErrAbandonmentRefundFailed", err)
	}
	if _, err := RecordExchangeAbandonmentTargetReturned(ctx, db, f.Org, f.ExchangeID); !errors.Is(err, ErrAbandonmentRefundFailed) {
		t.Fatalf("a target return after the failure: err=%v, want ErrAbandonmentRefundFailed", err)
	}
	if r := readStarted(t, db, ctx, f.Org, f.ExchangeID); r.FailedRef.String != "re_3Pq" || r.RefundFact.Valid || r.ReturnedAt.Valid {
		t.Fatalf("stored failure %q refund %v returned %v, want re_3Pq, none, none", r.FailedRef.String, r.RefundFact, r.ReturnedAt)
	}
}

// Refund evidence belongs to an upgrade only.
func TestRefundEvidenceIsRefusedWithoutAnUpgradeDelta(t *testing.T) {
	db, ctx := outboxDB(t)
	for _, unit := range []int64{1000, 1500} {
		f := seedAbandonable(t, db, ctx, abandonSeedOpts{TargetUnit: unit})
		reserveAbandonment(t, db, ctx, f)
		if err := RecordExchangeAbandonmentRefund(ctx, db, f.Org, f.ExchangeID, uuid.New()); !errors.Is(err, ErrAbandonmentNoRefund) {
			t.Fatalf("delta %d refund: err=%v, want ErrAbandonmentNoRefund", f.Delta, err)
		}
		if err := RecordExchangeAbandonmentRefundFailure(ctx, db, f.Org, f.ExchangeID, "re_3Pq"); !errors.Is(err, ErrAbandonmentNoRefund) {
			t.Fatalf("delta %d failure: err=%v, want ErrAbandonmentNoRefund", f.Delta, err)
		}
	}
}

func TestRecordTargetReturnedNeedsTheUpgradeRefundAndKeepsTheFirstInstant(t *testing.T) {
	db, ctx := outboxDB(t)
	f := seedAbandonable(t, db, ctx, abandonSeedOpts{TargetUnit: 2000})
	if _, err := RecordExchangeAbandonmentTargetReturned(ctx, db, f.Org, f.ExchangeID); !errors.Is(err, ErrAbandonmentNotStarted) {
		t.Fatalf("before the reservation: err=%v, want ErrAbandonmentNotStarted", err)
	}
	reserveAbandonment(t, db, ctx, f)
	if _, err := RecordExchangeAbandonmentTargetReturned(ctx, db, f.Org, f.ExchangeID); !errors.Is(err, ErrAbandonmentRefundPending) {
		t.Fatalf("upgrade without its refund: err=%v, want ErrAbandonmentRefundPending", err)
	}
	if err := RecordExchangeAbandonmentRefund(ctx, db, f.Org, f.ExchangeID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	first, err := RecordExchangeAbandonmentTargetReturned(ctx, db, f.Org, f.ExchangeID)
	if err != nil {
		t.Fatalf("record the target return: %v", err)
	}
	again, err := RecordExchangeAbandonmentTargetReturned(ctx, db, f.Org, f.ExchangeID)
	if err != nil || !again.Equal(first) {
		t.Fatalf("a second record returned %s err=%v, want the first instant %s", again, err, first)
	}
	if r := readStarted(t, db, ctx, f.Org, f.ExchangeID); !r.ReturnedAt.Time.Equal(first) {
		t.Fatalf("stored %s, returned %s", r.ReturnedAt.Time, first)
	}
}

// ---------------------------------------------------------------------------------------------
// The compensating fact and the terminal transaction.

// The fact is built from the write-once exchange fields alone: its amount is the target gross
// (the target face plus the retained source fee), its payload the source order, the exchange,
// the source gross, the signed delta and the sign's evidence.
func TestTheAbandonedFactIsBuiltFromTheExchangeFields(t *testing.T) {
	db, ctx := outboxDB(t)
	for _, c := range []struct {
		name string
		unit int64
	}{{"upgrade", 2000}, {"downgrade", 1000}, {"even", 1500}} {
		t.Run(c.name, func(t *testing.T) {
			f := seedAbandonable(t, db, ctx, abandonSeedOpts{TargetUnit: c.unit})
			reserveAbandonment(t, db, ctx, f)
			refund := uuid.New()
			if f.Delta > 0 {
				if err := RecordExchangeAbandonmentRefund(ctx, db, f.Org, f.ExchangeID, refund); err != nil {
					t.Fatal(err)
				}
			}
			returned, err := RecordExchangeAbandonmentTargetReturned(ctx, db, f.Org, f.ExchangeID)
			if err != nil {
				t.Fatal(err)
			}
			fact, err := BuildExchangeAbandonedFact(loadSettled(t, db, ctx, f))
			if err != nil {
				t.Fatal(err)
			}
			wantID := ExchangeAbandonedFactID(f.Org, f.ExchangeID)
			if fact.ID != wantID || fact.OrganizerID != f.Org || fact.OrderID != f.SourceOrder || fact.BuyerID != f.Buyer ||
				fact.Amount != f.TargetGross || fact.Currency != "EUR" || !fact.OccurredAt.Equal(returned) {
				t.Fatalf("fact envelope %+v, want id %s source order %s buyer %s amount %d occurred %s",
					fact, wantID, f.SourceOrder, f.Buyer, f.TargetGross, returned)
			}
			want := map[string]string{
				"order_id": f.SourceOrder.String(), "exchange_id": f.ExchangeID.String(),
				"source_gross": strconv.FormatInt(f.SourceGross, 10), "delta_amount": strconv.FormatInt(f.Delta, 10),
			}
			switch {
			case f.Delta > 0:
				want["refund_fact_id"] = refund.String()
			case f.Delta < 0:
				want["written_off_amount"] = strconv.FormatInt(-f.Delta, 10)
			}
			got := fact.Payload()
			if len(got) != len(want) {
				t.Fatalf("payload %v, want %v", got, want)
			}
			for k, v := range want {
				if got[k] != v {
					t.Fatalf("payload %v, want %v", got, want)
				}
			}
		})
	}
}

func TestTheAbandonedFactNeedsTheTargetReturn(t *testing.T) {
	db, ctx := outboxDB(t)
	f := seedAbandonable(t, db, ctx, abandonSeedOpts{TargetUnit: 1500})
	reserveAbandonment(t, db, ctx, f)
	if _, err := BuildExchangeAbandonedFact(loadSettled(t, db, ctx, f)); !errors.Is(err, ErrAbandonmentTargetNotReturned) {
		t.Fatalf("err=%v, want ErrAbandonmentTargetNotReturned", err)
	}
}

// abandonThroughReturn reserves, records the refund for an upgrade, records the target return
// and returns the fact the terminal transaction expects.
func abandonThroughReturn(t *testing.T, db *sql.DB, ctx context.Context, f abandonFixture) ExchangeAbandonedFact {
	t.Helper()
	reserveAbandonment(t, db, ctx, f)
	if f.Delta > 0 {
		if err := RecordExchangeAbandonmentRefund(ctx, db, f.Org, f.ExchangeID, uuid.New()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RecordExchangeAbandonmentTargetReturned(ctx, db, f.Org, f.ExchangeID); err != nil {
		t.Fatal(err)
	}
	fact, err := BuildExchangeAbandonedFact(loadSettled(t, db, ctx, f))
	if err != nil {
		t.Fatal(err)
	}
	return fact
}

type factRow struct {
	OrderID, OrganizerID, BuyerID uuid.UUID
	Type, Currency                string
	Amount                        int64
	OccurredAt                    time.Time
}

func readFacts(t *testing.T, db *sql.DB, ctx context.Context, id uuid.UUID) []factRow {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT order_id, organizer_id, buyer_id, fact_type, currency, amount, occurred_at
		FROM order_facts WHERE fact_id=$1`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []factRow
	for rows.Next() {
		var r factRow
		if err := rows.Scan(&r.OrderID, &r.OrganizerID, &r.BuyerID, &r.Type, &r.Currency, &r.Amount, &r.OccurredAt); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// The terminal transaction appends the identical fact with the existing order_facts columns and
// sets abandoned_at and the applicable write-off together. A second call is a replay: no second
// fact, no new timestamp.
func TestFinishAppendsTheFactAndTheTerminalMarkerTogether(t *testing.T) {
	db, ctx := outboxDB(t)
	for _, c := range []struct {
		name string
		unit int64
	}{{"upgrade", 2000}, {"downgrade", 1000}, {"even", 1500}} {
		t.Run(c.name, func(t *testing.T) {
			f := seedAbandonable(t, db, ctx, abandonSeedOpts{TargetUnit: c.unit})
			fact := abandonThroughReturn(t, db, ctx, f)
			e, replay, err := FinishExchangeAbandonment(ctx, db, fact)
			if err != nil || replay || !e.Abandoned {
				t.Fatalf("finish: replay=%v abandoned=%v err=%v", replay, e.Abandoned, err)
			}
			rows := readFacts(t, db, ctx, fact.ID)
			want := factRow{OrderID: f.SourceOrder, OrganizerID: f.Org, BuyerID: f.Buyer, Type: "order.exchange.abandoned",
				Currency: "EUR", Amount: f.TargetGross}
			if len(rows) != 1 || rows[0].OrderID != want.OrderID || rows[0].OrganizerID != want.OrganizerID ||
				rows[0].BuyerID != want.BuyerID || rows[0].Type != want.Type || rows[0].Currency != want.Currency ||
				rows[0].Amount != want.Amount || !rows[0].OccurredAt.Equal(fact.OccurredAt) {
				t.Fatalf("order_facts rows %+v, want one %+v at %s", rows, want, fact.OccurredAt)
			}
			r := readStarted(t, db, ctx, f.Org, f.ExchangeID)
			wantOff := sql.NullInt64{}
			if f.Delta < 0 {
				wantOff = sql.NullInt64{Int64: -f.Delta, Valid: true}
			}
			if !r.AbandonedAt.Valid || r.WrittenOff != wantOff {
				t.Fatalf("abandoned=%v write-off=%v, want terminal with write-off %v", r.AbandonedAt.Valid, r.WrittenOff, wantOff)
			}

			again, replay, err := FinishExchangeAbandonment(ctx, db, fact)
			if err != nil || !replay || !again.AbandonedAt.Equal(r.AbandonedAt.Time) {
				t.Fatalf("second finish: replay=%v abandoned_at=%s err=%v, want a replay at %s", replay, again.AbandonedAt, err, r.AbandonedAt.Time)
			}
			if n := len(readFacts(t, db, ctx, fact.ID)); n != 1 {
				t.Fatalf("a replay left %d facts, want 1", n)
			}
		})
	}
}

// A fact row already under the derived id with other content is a conflicting identity: the
// terminal transaction fails and leaves the exchange pending.
func TestFinishRefusesAConflictingFactIdentity(t *testing.T) {
	db, ctx := outboxDB(t)
	f := seedAbandonable(t, db, ctx, abandonSeedOpts{TargetUnit: 1500})
	fact := abandonThroughReturn(t, db, ctx, f)
	mustExec(t, db, ctx, `INSERT INTO order_facts(fact_id,order_id,organizer_id,buyer_id,fact_type,amount,currency,occurred_at)
		VALUES($1,$2,$3,$4,'order.exchange.abandoned',$5,'EUR',$6)`,
		fact.ID, fact.OrderID, fact.OrganizerID, fact.BuyerID, fact.Amount+1, fact.OccurredAt)
	if _, _, err := FinishExchangeAbandonment(ctx, db, fact); !errors.Is(err, ErrAbandonmentFactConflict) {
		t.Fatalf("err=%v, want ErrAbandonmentFactConflict", err)
	}
	if r := readStarted(t, db, ctx, f.Org, f.ExchangeID); r.AbandonedAt.Valid {
		t.Fatal("the exchange went terminal over a conflicting fact")
	}
}

// R1-5. The stored fact is read in the exchange's organizer only. A row under the derived id in
// another organizer is a scoped miss after the insert conflict, and it answers the same generic
// conflict without reading that organizer's row.
func TestFinishRefusesAFactIdentityHeldByAnotherOrganizer(t *testing.T) {
	db, ctx := outboxDB(t)
	f := seedAbandonable(t, db, ctx, abandonSeedOpts{TargetUnit: 1500})
	fact := abandonThroughReturn(t, db, ctx, f)
	other := uuid.New()
	mustExec(t, db, ctx, `INSERT INTO order_facts(fact_id,order_id,organizer_id,buyer_id,fact_type,amount,currency,occurred_at)
		VALUES($1,$2,$3,$4,'order.exchange.abandoned',$5,'EUR',$6)`,
		fact.ID, fact.OrderID, other, fact.BuyerID, fact.Amount, fact.OccurredAt)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM order_facts WHERE organizer_id=$1`, other) })
	if _, _, err := FinishExchangeAbandonment(ctx, db, fact); !errors.Is(err, ErrAbandonmentFactConflict) {
		t.Fatalf("err=%v, want ErrAbandonmentFactConflict", err)
	}
	if r := readStarted(t, db, ctx, f.Org, f.ExchangeID); r.AbandonedAt.Valid {
		t.Fatal("the exchange went terminal over another organizer's fact")
	}
}

// R1-5. The loader projects an order's fields only through a reservation in the exchange's
// organizer. A source or replacement reservation moved to another organizer leaves that order's
// checkout key unread, and the row reads as inconsistent.
func TestTheLoaderReadsOrderFieldsOnlyThroughAnInOrganizerReservation(t *testing.T) {
	db, ctx := outboxDB(t)
	source := seedAbandonable(t, db, ctx, abandonSeedOpts{})
	mustExec(t, db, ctx, `UPDATE reservations SET organizer_id=$2 WHERE id=$1`, source.SourceReservation, uuid.New())
	if e := loadSettled(t, db, ctx, source); e.PaymentSourceKey != "" || e.FreshEligibility() == nil {
		t.Fatalf("source out of organizer: payment source key %q, eligibility %v; want no key and a refusal", e.PaymentSourceKey, e.FreshEligibility())
	}
	replacement := seedAbandonable(t, db, ctx, abandonSeedOpts{})
	mustExec(t, db, ctx, `UPDATE reservations SET organizer_id=$2 WHERE id=$1`, replacement.ReplacementReservation, uuid.New())
	if e := loadSettled(t, db, ctx, replacement); e.ReplacementPaymentKey != "" || e.FreshEligibility() == nil {
		t.Fatalf("replacement out of organizer: replacement key %q, eligibility %v; want no key and a refusal", e.ReplacementPaymentKey, e.FreshEligibility())
	}
}

// The fact submitted to payments must be the one the locked row describes.
func TestFinishRefusesAFactTheRowDoesNotDescribe(t *testing.T) {
	db, ctx := outboxDB(t)
	f := seedAbandonable(t, db, ctx, abandonSeedOpts{TargetUnit: 1000})
	fact := abandonThroughReturn(t, db, ctx, f)
	for name, mutate := range map[string]func(*ExchangeAbandonedFact){
		"amount":     func(x *ExchangeAbandonedFact) { x.Amount++ },
		"write-off":  func(x *ExchangeAbandonedFact) { x.WrittenOffAmount++ },
		"occurred":   func(x *ExchangeAbandonedFact) { x.OccurredAt = x.OccurredAt.Add(time.Second) },
		"buyer":      func(x *ExchangeAbandonedFact) { x.BuyerID = uuid.New() },
		"fact id":    func(x *ExchangeAbandonedFact) { x.ID = uuid.New() },
		"organizer":  func(x *ExchangeAbandonedFact) { x.OrganizerID = uuid.New() },
		"refund":     func(x *ExchangeAbandonedFact) { x.RefundFactID = uuid.New() },
		"source sum": func(x *ExchangeAbandonedFact) { x.SourceGross++ },
	} {
		t.Run(name, func(t *testing.T) {
			bad := fact
			mutate(&bad)
			_, _, err := FinishExchangeAbandonment(ctx, db, bad)
			if !errors.Is(err, ErrAbandonmentFactMismatch) && (name != "organizer" || !errors.Is(err, ErrExchangeNotFound)) {
				t.Fatalf("err=%v, want ErrAbandonmentFactMismatch", err)
			}
		})
	}
	if r := readStarted(t, db, ctx, f.Org, f.ExchangeID); r.AbandonedAt.Valid {
		t.Fatal("the exchange went terminal over a fact it does not describe")
	}
	if n := len(readFacts(t, db, ctx, fact.ID)); n != 0 {
		t.Fatalf("a refused finish left %d facts", n)
	}
}

// After completion the same pair replays the terminal row and another pair still conflicts.
func TestAFinishedAbandonmentKeepsItsOwner(t *testing.T) {
	db, ctx := outboxDB(t)
	f := seedAbandonable(t, db, ctx, abandonSeedOpts{TargetUnit: 1500})
	fact := abandonThroughReturn(t, db, ctx, f)
	if _, _, err := FinishExchangeAbandonment(ctx, db, fact); err != nil {
		t.Fatal(err)
	}
	e, fresh, err := ReserveExchangeAbandonment(ctx, db, AbandonmentReservation{Expected: loadSettled(t, db, ctx, f), Attribution: adaAttribution(t)})
	if err != nil || fresh || !e.Abandoned {
		t.Fatalf("same pair after completion: fresh=%v abandoned=%v err=%v", fresh, e.Abandoned, err)
	}
	_, _, err = ReserveExchangeAbandonment(ctx, db, AbandonmentReservation{Expected: loadSettled(t, db, ctx, f),
		Attribution: AbandonmentAttribution{Operator: "Grace Operator", Reason: adaReason}})
	if !errors.Is(err, ErrAbandonmentAttributionConflict) {
		t.Fatalf("another pair after completion: err=%v, want ErrAbandonmentAttributionConflict", err)
	}
}

// ---------------------------------------------------------------------------------------------
// The original gross facts.

// The two gross facts the settlement journalled must agree with the exchange: organizer, source
// order, buyer, type, currency, the source gross and the target gross.
func TestVerifyExchangeGrossFactsRefusesEachDisagreement(t *testing.T) {
	db, ctx := outboxDB(t)
	f := seedAbandonable(t, db, ctx, abandonSeedOpts{GrossFacts: true})
	if err := VerifyExchangeGrossFacts(ctx, db, loadSettled(t, db, ctx, f)); err != nil {
		t.Fatalf("consistent gross facts were refused: %v", err)
	}
	reversed, sold := ExchangeReversedFactID(f.ExchangeID), ExchangeSoldFactID(f.ExchangeID)
	for _, c := range []struct {
		name, set string
		fact      uuid.UUID
	}{
		{"reversed fact missing", "fact_id=gen_random_uuid()", reversed},
		{"sold fact in another organizer", "organizer_id=gen_random_uuid()", sold},
		{"reversed fact on another order", "order_id=gen_random_uuid()", reversed},
		{"sold fact for another buyer", "buyer_id=gen_random_uuid()", sold},
		{"reversed fact of another type", "fact_type='order.refunded'", reversed},
		{"sold fact in another currency", "currency='USD'", sold},
		{"reversed amount is not the source gross", "amount=amount+1", reversed},
		{"sold amount is not the target gross", "amount=amount-1", sold},
	} {
		t.Run(c.name, func(t *testing.T) {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.ExecContext(ctx, `UPDATE order_facts SET `+c.set+` WHERE fact_id=$1`, c.fact); err != nil {
				t.Fatal(err)
			}
			e := loadSettled(t, db, ctx, f)
			if err := VerifyExchangeGrossFacts(ctx, tx, e); !errors.Is(err, ErrExchangeGrossFactsInconsistent) {
				t.Fatalf("err=%v, want ErrExchangeGrossFactsInconsistent", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------
// Write scope.

// A same-ID row in another organizer is never written by any step of this organizer's
// abandonment. Each step has its own foreign row in exactly the state that step's write
// matches, so an unscoped write would change it.
func TestEveryAbandonmentWriteIsOrganizerScoped(t *testing.T) {
	db, ctx := outboxDB(t)
	shared := uuid.New()
	seed := func() abandonFixture {
		return seedAbandonable(t, db, ctx, abandonSeedOpts{ExchangeID: shared, TargetUnit: 2000})
	}
	// The foreign rows are advanced with direct SQL, not with the functions under test, so that
	// an unscoped write in one of those functions cannot touch them before the snapshot.
	advance := func(f abandonFixture, stmts ...string) {
		t.Helper()
		for _, stmt := range stmts {
			mustExec(t, db, ctx, stmt, f.Org, shared)
		}
	}
	unstarted := seed()
	started := seed()
	advance(started, startStmt)
	refunded := seed()
	advance(refunded, startStmt, setRefund)
	returned := seed()
	advance(returned, startStmt, setRefund, setReturned)
	foreign := []abandonFixture{unstarted, started, refunded, returned}
	before := make([]startedRow, len(foreign))
	for i, f := range foreign {
		before[i] = readStarted(t, db, ctx, f.Org, shared)
	}
	check := func(step string) {
		t.Helper()
		for i, f := range foreign {
			if after := readStarted(t, db, ctx, f.Org, shared); after != before[i] {
				t.Fatalf("%s changed organizer %s's exchange: %+v -> %+v", step, f.Org, before[i], after)
			}
		}
	}

	mine := seed()
	if _, fresh, err := ReserveExchangeAbandonment(ctx, db, AbandonmentReservation{Expected: loadSettled(t, db, ctx, mine),
		Attribution: AbandonmentAttribution{Operator: "Grace Operator", Reason: "mine"}}); err != nil || !fresh {
		t.Fatalf("reserve mine: fresh=%v err=%v", fresh, err)
	}
	check("the reservation")
	if err := RecordExchangeAbandonmentRefund(ctx, db, mine.Org, shared, uuid.New()); err != nil {
		t.Fatal(err)
	}
	check("the refund record")
	if _, err := RecordExchangeAbandonmentTargetReturned(ctx, db, mine.Org, shared); err != nil {
		t.Fatal(err)
	}
	check("the target return")
	fact, err := BuildExchangeAbandonedFact(loadSettled(t, db, ctx, mine))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := FinishExchangeAbandonment(ctx, db, fact); err != nil {
		t.Fatal(err)
	}
	check("the terminal transaction")
	failing := seed()
	advance(failing, startStmt)
	if err := RecordExchangeAbandonmentRefundFailure(ctx, db, failing.Org, shared, "re_mine"); err != nil {
		t.Fatal(err)
	}
	check("the failure record")
}

// ---------------------------------------------------------------------------------------------
// The reversal backlog.

// Started and provider-failed abandonments still count as outstanding and awaiting the switch;
// a terminal one leaves both counts and the oldest age.
func TestTheBacklogDropsTerminalAbandonmentsOnly(t *testing.T) {
	db, ctx := outboxDB(t)
	before, err := ReadExchangeReversalBacklog(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	pending := seedAbandonable(t, db, ctx, abandonSeedOpts{TargetUnit: 1500})
	reserveAbandonment(t, db, ctx, pending)
	failed := seedAbandonable(t, db, ctx, abandonSeedOpts{TargetUnit: 2000})
	reserveAbandonment(t, db, ctx, failed)
	if err := RecordExchangeAbandonmentRefundFailure(ctx, db, failed.Org, failed.ExchangeID, "re_3Pq"); err != nil {
		t.Fatal(err)
	}
	// The terminal row is far older than anything else, so it would own the oldest age if it
	// were still counted.
	terminal := seedAbandonable(t, db, ctx, abandonSeedOpts{TargetUnit: 1500, SettledAt: time.Now().Add(-90 * 24 * time.Hour)})
	fact := abandonThroughReturn(t, db, ctx, terminal)
	if _, _, err := FinishExchangeAbandonment(ctx, db, fact); err != nil {
		t.Fatal(err)
	}

	after, err := ReadExchangeReversalBacklog(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if d := after.Outstanding - before.Outstanding; d != 2 {
		t.Fatalf("outstanding delta = %d, want 2 (pending and failed, not terminal)", d)
	}
	if d := after.AwaitingSwitch - before.AwaitingSwitch; d != 2 {
		t.Fatalf("awaiting-switch delta = %d, want 2 (pending and failed, not terminal)", d)
	}
	if after.OldestAgeSeconds >= int64(30*24*time.Hour/time.Second) {
		t.Fatalf("oldest age = %ds: the terminal abandonment still owns the oldest obligation", after.OldestAgeSeconds)
	}
}

// ---------------------------------------------------------------------------------------------
// The refund binder's marker check.

// Once the abandonment of an exchange has started, its replacement order cannot bind a new
// refund. The refusal is the ordinary not-refundable one, so staff answer 409 and bulk reports
// `not_refundable`.
func TestBindOrderRefundRefusesAFreshBindOnAnAbandoningReplacement(t *testing.T) {
	db, ctx := outboxDB(t)
	f := seedAbandonable(t, db, ctx, abandonSeedOpts{})
	reserveAbandonment(t, db, ctx, f)
	_, err := BindOrderRefund(ctx, db, RefundRequest{OrderID: f.ReplacementOrder, OrganizerID: f.Org, Quantity: 1,
		IdempotencyKey: "staff-1", Actor: "support@example.test", Reason: "target slot cancelled"})
	if !errors.Is(err, ErrExchangeReplacementAbandoned) || !errors.Is(err, ErrOrderNotRefundable) {
		t.Fatalf("err=%v, want ErrExchangeReplacementAbandoned wrapping ErrOrderNotRefundable", err)
	}
	if n := countAbandonRows(t, db, ctx, `SELECT count(*) FROM order_refunds WHERE order_id=$1`, f.ReplacementOrder); n != 0 {
		t.Fatalf("%d refund rows bound after the reservation", n)
	}
}

// A refund bound BEFORE the reservation is not replayed after it: the marker check runs before
// the same-key replay.
func TestBindOrderRefundRefusesAReplayOnAnAbandoningReplacement(t *testing.T) {
	db, ctx := outboxDB(t)
	f := seedAbandonable(t, db, ctx, abandonSeedOpts{})
	r := bindReplacementRefund(t, db, ctx, f, "cancel-1")
	_, fresh, err := ReserveExchangeAbandonment(ctx, db, AbandonmentReservation{Expected: loadSettled(t, db, ctx, f),
		Attribution: adaAttribution(t), InspectedRefunds: []ReplacementRefund{{ID: r.ID, Status: "pending"}}})
	if err != nil || !fresh {
		t.Fatalf("reserve: fresh=%v err=%v", fresh, err)
	}
	_, err = BindOrderRefund(ctx, db, RefundRequest{OrderID: f.ReplacementOrder, OrganizerID: f.Org, Quantity: 1,
		IdempotencyKey: "cancel-1", Actor: "support@example.test", Reason: "target slot cancelled"})
	if !errors.Is(err, ErrExchangeReplacementAbandoned) {
		t.Fatalf("replay after the reservation: err=%v, want ErrExchangeReplacementAbandoned", err)
	}
}

// The marker is read for THIS organizer's exchange of THIS replacement, and only once started:
// an unstarted exchange, and a started one in another organizer naming the same reservation,
// leave the bind alone.
func TestBindOrderRefundReadsOnlyAStartedMarkerOfItsOwnOrganizer(t *testing.T) {
	db, ctx := outboxDB(t)
	unstarted := seedAbandonable(t, db, ctx, abandonSeedOpts{})
	bindReplacementRefund(t, db, ctx, unstarted, "staff-1")

	f := seedAbandonable(t, db, ctx, abandonSeedOpts{})
	// Another organizer's started exchange whose replacement reservation is f's.
	foreign := seedAbandonable(t, db, ctx, abandonSeedOpts{})
	mustExec(t, db, ctx, `UPDATE order_exchanges SET replacement_reservation_id=$3 WHERE organizer_id=$1 AND id=$2`,
		foreign.Org, foreign.ExchangeID, f.ReplacementReservation)
	mustExec(t, db, ctx, startStmt, foreign.Org, foreign.ExchangeID)
	bindReplacementRefund(t, db, ctx, f, "staff-1")
}

// The void binder reads the same marker (D23): only a started marker of its own organizer
// refuses a comped replacement's void. An unstarted exchange, and a started one in another
// organizer naming the same reservation, leave the void alone.
func TestBindOrderVoidRefusesOnlyAStartedMarkerOfItsOwnOrganizer(t *testing.T) {
	db, ctx := outboxDB(t)
	comped := func(o abandonSeedOpts) abandonFixture {
		f := seedAbandonable(t, db, ctx, o)
		mustExec(t, db, ctx, `UPDATE reservations SET unit_amount=0, total_amount=0, face_value_amount=0
			WHERE organizer_id=$1 AND id=$2`, f.Org, f.ReplacementReservation)
		t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM order_voids WHERE organizer_id=$1`, f.Org) })
		return f
	}
	void := func(f abandonFixture) error {
		_, err := BindOrderVoid(ctx, db, VoidRequest{OrderID: f.ReplacementOrder, OrganizerID: f.Org,
			IdempotencyKey: "staff-void", Actor: "support@example.test", Reason: "comp withdrawn"})
		return err
	}
	if err := void(comped(abandonSeedOpts{})); err != nil {
		t.Fatalf("void of an unstarted exchange's replacement: %v", err)
	}
	f := comped(abandonSeedOpts{})
	foreign := seedAbandonable(t, db, ctx, abandonSeedOpts{})
	mustExec(t, db, ctx, `UPDATE order_exchanges SET replacement_reservation_id=$3 WHERE organizer_id=$1 AND id=$2`,
		foreign.Org, foreign.ExchangeID, f.ReplacementReservation)
	mustExec(t, db, ctx, startStmt, foreign.Org, foreign.ExchangeID)
	if err := void(f); err != nil {
		t.Fatalf("void beside another organizer's started exchange: %v", err)
	}
	started := comped(abandonSeedOpts{})
	reserveAbandonment(t, db, ctx, started)
	if err := void(started); !errors.Is(err, ErrExchangeReplacementVoidAbandoned) || !errors.Is(err, ErrOrderNotVoidable) {
		t.Fatalf("void after the reservation: err=%v, want ErrExchangeReplacementVoidAbandoned wrapping ErrOrderNotVoidable", err)
	}
}

// TKT-598. A void bound BEFORE the abandonment started cannot be replayed after it (D23): the
// marker check runs under the order lock and before the existing-row return, so a replay is
// refused like a fresh bind, and the existing void row stays as history, unchanged.
//
// The fixture is a settled, unswitched, fee-free downgrade to a zero-gross replacement, so the
// replacement is a comped order that the void path accepts and every exchange amount is
// consistent.
func TestBindOrderVoidRefusesAReplayOnAnAbandoningReplacement(t *testing.T) {
	db, ctx := outboxDB(t)
	f := seedAbandonable(t, db, ctx, abandonSeedOpts{})
	// Registered after the seed, so it runs before the seed deletes the orders.
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM order_voids WHERE organizer_id=$1 AND order_id=$2`, f.Org, f.ReplacementOrder)
	})
	mustExec(t, db, ctx, `UPDATE reservations SET total_amount=face_value_amount WHERE organizer_id=$1 AND id=$2`,
		f.Org, f.SourceReservation)
	mustExec(t, db, ctx, `UPDATE reservations SET unit_amount=0, total_amount=0, face_value_amount=0
		WHERE organizer_id=$1 AND id=$2`, f.Org, f.ReplacementReservation)
	mustExec(t, db, ctx, `
		UPDATE order_exchanges
		SET source_gross_total=source_total, target_total=0, target_unit_amount=0, delta_amount=-source_total
		WHERE organizer_id=$1 AND id=$2`, f.Org, f.ExchangeID)

	in := VoidRequest{OrderID: f.ReplacementOrder, OrganizerID: f.Org,
		IdempotencyKey: "staff-void", Actor: "support@example.test", Reason: "comp withdrawn"}
	v, err := BindOrderVoid(ctx, db, in)
	if err != nil {
		t.Fatalf("void before the abandonment started: %v", err)
	}
	if v.ID != VoidID(f.Org, f.ReplacementOrder) || v.Quantity != f.Quantity || v.TicketsVoided || v.CapacityReturned {
		t.Fatalf("void = %+v, want id %s, quantity %d and both obligations open",
			v, VoidID(f.Org, f.ReplacementOrder), f.Quantity)
	}
	readVoid := func() string {
		t.Helper()
		var row string
		if err := db.QueryRowContext(ctx, `SELECT to_jsonb(v)::text FROM order_voids v WHERE organizer_id=$1 AND order_id=$2`,
			f.Org, f.ReplacementOrder).Scan(&row); err != nil {
			t.Fatalf("read the void row: %v", err)
		}
		return row
	}
	bound := readVoid()

	reserveAbandonment(t, db, ctx, f)
	if started := readStarted(t, db, ctx, f.Org, f.ExchangeID); !started.StartedAt.Valid {
		t.Fatal("the reservation did not persist abandonment_started_at")
	}

	_, err = BindOrderVoid(ctx, db, in)
	if !errors.Is(err, ErrExchangeReplacementVoidAbandoned) || !errors.Is(err, ErrOrderNotVoidable) {
		t.Fatalf("replay after the reservation: err=%v, want ErrExchangeReplacementVoidAbandoned wrapping ErrOrderNotVoidable", err)
	}
	if after := readVoid(); after != bound {
		t.Fatalf("the refused replay changed the void row:\n before %s\n after  %s", bound, after)
	}
}

func countAbandonRows(t *testing.T, db *sql.DB, ctx context.Context, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ---------------------------------------------------------------------------------------------
// The listing after 0028.

// A started and a provider-failed abandonment stay listed with their progress and the failure
// reference; a terminal one leaves the listing.
func TestSettledUnswitchedListingShowsUnresolvedAbandonmentsAndDropsTerminalOnes(t *testing.T) {
	db, ctx := outboxDB(t)
	org := uuid.New()
	pending := seedAbandonable(t, db, ctx, abandonSeedOpts{Org: org, TargetUnit: 1500})
	reserveAbandonment(t, db, ctx, pending)
	failed := seedAbandonable(t, db, ctx, abandonSeedOpts{Org: org, TargetUnit: 2000})
	reserveAbandonment(t, db, ctx, failed)
	if err := RecordExchangeAbandonmentRefundFailure(ctx, db, org, failed.ExchangeID, "re_3Pq"); err != nil {
		t.Fatal(err)
	}
	terminal := seedAbandonable(t, db, ctx, abandonSeedOpts{Org: org, TargetUnit: 1500})
	if _, _, err := FinishExchangeAbandonment(ctx, db, abandonThroughReturn(t, db, ctx, terminal)); err != nil {
		t.Fatal(err)
	}

	got := map[uuid.UUID]SettledExchange{}
	for _, r := range listedIDs(t, db, ctx, org) {
		got[r.ID] = r
	}
	if _, listed := got[terminal.ExchangeID]; listed || len(got) != 2 {
		t.Fatalf("listed %v, want the pending and failed abandonments only", exchangeIDsOf(got))
	}
	p, f := got[pending.ExchangeID], got[failed.ExchangeID]
	if !p.AbandonmentStarted || p.AbandonmentOperator != adaOperator || p.AbandonmentRefundFailedRef != "" {
		t.Fatalf("pending row started=%v operator=%q failure=%q", p.AbandonmentStarted, p.AbandonmentOperator, p.AbandonmentRefundFailedRef)
	}
	if !f.AbandonmentStarted || f.AbandonmentRefundFailedRef != "re_3Pq" {
		t.Fatalf("failed row started=%v failure=%q, want re_3Pq", f.AbandonmentStarted, f.AbandonmentRefundFailedRef)
	}
}

func exchangeIDsOf(m map[uuid.UUID]SettledExchange) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	return ids
}
