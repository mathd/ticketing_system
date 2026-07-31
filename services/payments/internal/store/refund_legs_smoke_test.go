//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Partial-refund legs (TKT-156). The existing payment_compensations row means "the ONE
// whole compensation for a failed checkout" — its primary key is (organizer, source
// operation, kind), which by construction cannot express a second refund. Post-purchase
// partial refunds get their own durable identity in payment_refund_legs, and the whole
// point of these tests is the two ceilings that identity has to respect: never more than
// the captured money, and never alongside a whole refund.
//
// Real PostgreSQL, for the same reason as journal_smoke_test.go: every assertion here
// depends on the row lock, the primary key, or transaction visibility.

// seedCaptured writes a payment_operations row carrying the durable "captured" evidence a
// refund leg is decided from — the shape CompleteOperation leaves behind on the charge path.
func seedCaptured(t *testing.T, db *sql.DB, ctx context.Context, org uuid.UUID, key string, captured int64) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO payment_operations(organizer_id,idempotency_key,request_fingerprint,status,order_id,buyer_id,
		                               request_amount,request_currency,provider_payment_ref,provider_state,
		                               authorized_amount,captured_amount,provider_state_at)
		VALUES($1,$2,'fingerprint','captured',$3,$4,$5,'EUR','pi_test','captured',$5,$5,now())`,
		org, key, uuid.New(), uuid.New(), captured); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM payment_refund_legs WHERE organizer_id=$1`, org)
		_, _ = db.Exec(`DELETE FROM payment_compensations WHERE organizer_id=$1`, org)
		_, _ = db.Exec(`DELETE FROM payment_operations WHERE organizer_id=$1`, org)
	})
}

// The convergence property the whole design rests on: two binds under the same refund key
// produce ONE row and therefore ONE deterministic provider idempotency key. Without it a
// commerce retry issues a second provider refund.
func TestBindRefundLegConvergesOnOneProviderKey(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org, key := uuid.New(), "charge-converge"
	seedCaptured(t, db, ctx, org, key, 2500)

	first, err := j.BindRefundLeg(ctx, org, key, "refund-1", 1250, "EUR")
	if err != nil {
		t.Fatalf("bind first leg: %v", err)
	}
	second, err := j.BindRefundLeg(ctx, org, key, "refund-1", 1250, "EUR")
	if err != nil {
		t.Fatalf("re-bind same leg: %v", err)
	}
	if first.ProviderKey != second.ProviderKey {
		t.Fatalf("provider key must be deterministic: %q vs %q", first.ProviderKey, second.ProviderKey)
	}
	if first.ProviderKey != RefundLegKey(org, key, "refund-1") {
		t.Fatalf("provider key %q is not the derived key", first.ProviderKey)
	}
	if !first.BoundAt.Equal(second.BoundAt) {
		t.Fatalf("bound_at must be stable across re-bind: %s vs %s", first.BoundAt, second.BoundAt)
	}

	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM payment_refund_legs WHERE organizer_id=$1`, org).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d, want 1", rows)
	}

	// A DIFFERENT refund key against the same charge is a different leg with a different
	// provider key — that is what makes two partial refunds two provider operations.
	other, err := j.BindRefundLeg(ctx, org, key, "refund-2", 500, "EUR")
	if err != nil {
		t.Fatalf("bind second leg: %v", err)
	}
	if other.ProviderKey == first.ProviderKey {
		t.Fatal("distinct refund keys must not share a provider idempotency key")
	}
}

// The captured-money ceiling. Legs are cumulative and BOUND legs count: releasing an
// unresolved leg's allowance is how you refund more than you captured.
func TestBindRefundLegRejectsOverCapture(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org, key := uuid.New(), "charge-ceiling"
	seedCaptured(t, db, ctx, org, key, 2500)

	if _, err := j.BindRefundLeg(ctx, org, key, "refund-1", 2000, "EUR"); err != nil {
		t.Fatalf("bind within capture: %v", err)
	}
	_, err := j.BindRefundLeg(ctx, org, key, "refund-2", 600, "EUR")
	if !errors.Is(err, ErrRefundExceedsCapture) {
		t.Fatalf("err = %v, want ErrRefundExceedsCapture", err)
	}
	// Exactly the remainder still fits — the ceiling is a ceiling, not a margin.
	if _, err := j.BindRefundLeg(ctx, org, key, "refund-2", 500, "EUR"); err != nil {
		t.Fatalf("bind exact remainder: %v", err)
	}
}

// Two individually-valid legs that together exceed the capture, raced. Exactly one must
// commit. This is the test the FOR UPDATE on payment_operations exists for.
func TestConcurrentRefundLegBindingsCannotExceedCapture(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org, key := uuid.New(), "charge-race"
	seedCaptured(t, db, ctx, org, key, 1000)

	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, refundKey := range []string{"race-a", "race-b"} {
		wg.Add(1)
		go func(i int, rk string) {
			defer wg.Done()
			start.Wait()
			_, errs[i] = j.BindRefundLeg(ctx, org, key, rk, 700, "EUR")
		}(i, refundKey)
	}
	start.Done()
	wg.Wait()

	var ok int
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrRefundExceedsCapture):
		default:
			t.Fatalf("leg %d failed unexpectedly: %v", i, err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d legs bound, want exactly 1", ok)
	}
	var total sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT sum(amount) FROM payment_refund_legs WHERE organizer_id=$1`, org).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total.Int64 > 1000 {
		t.Fatalf("bound legs total %d exceeds captured 1000", total.Int64)
	}
}

// Mutual exclusion with the recovery path. A whole refund and a partial leg against the
// same charge would each be counted against a ceiling the other does not see.
func TestRefundLegAndWholeRefundAreMutuallyExclusive(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))

	org, key := uuid.New(), "charge-whole-first"
	seedCaptured(t, db, ctx, org, key, 2500)
	if _, err := j.BindCompensation(ctx, org, key, "refund", 2500, "EUR"); err != nil {
		t.Fatalf("bind whole refund: %v", err)
	}
	if _, err := j.BindRefundLeg(ctx, org, key, "refund-1", 100, "EUR"); !errors.Is(err, ErrWholeRefundBound) {
		t.Fatalf("err = %v, want ErrWholeRefundBound", err)
	}

	org2, key2 := uuid.New(), "charge-leg-first"
	seedCaptured(t, db, ctx, org2, key2, 2500)
	if _, err := j.BindRefundLeg(ctx, org2, key2, "refund-1", 100, "EUR"); err != nil {
		t.Fatalf("bind leg: %v", err)
	}
	if _, err := j.BindCompensation(ctx, org2, key2, "refund", 2500, "EUR"); !errors.Is(err, ErrRefundLegsBound) {
		t.Fatalf("err = %v, want ErrRefundLegsBound", err)
	}
	// A VOID is unaffected: the exclusion is about refund money, not about the operation.
	if _, err := j.BindCompensation(ctx, org2, key2, "void", 2500, "EUR"); err != nil {
		t.Fatalf("a void must not be blocked by refund legs: %v", err)
	}
}

// The race the sequential test above cannot see (ai-review, critical). Before the fix the
// whole-refund path checked "no legs exist" and inserted its compensation as two separate
// autocommit statements, so a leg could bind in between: both rows exist, both derive
// distinct provider keys, and the partial amount PLUS the entire captured amount are both
// refunded.
//
// It is asserted DETERMINISTICALLY, by holding the lock, rather than by racing two
// goroutines. A twelve-iteration racing version was written first and PASSED against the
// reverted, broken implementation — the losing interleaving is too narrow to hit by luck,
// so that test proved nothing while looking like it proved everything. The fix is a lock;
// the test holds the lock.
func TestBindCompensationDecidesUnderTheOperationRowLock(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org, key := uuid.New(), "charge-exclusion-lock"
	seedCaptured(t, db, ctx, org, key, 2500)

	// Stand in for a leg bind in progress: hold the row BindRefundLeg locks, then insert
	// the leg inside that same transaction so it is invisible to any uncommitted read.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM payment_operations WHERE organizer_id=$1 AND idempotency_key=$2 FOR UPDATE`, org, key).Scan(&one); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := j.BindCompensation(ctx, org, key, "refund", 2500, "EUR")
		done <- err
	}()

	// It must BLOCK. Pre-fix it never touched payment_operations and returned immediately
	// — which is precisely the window the leg bound in.
	select {
	case err := <-done:
		t.Fatalf("BindCompensation returned (%v) without waiting for the operation row lock", err)
	case <-time.After(750 * time.Millisecond):
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO payment_refund_legs(organizer_id,source_idempotency_key,refund_idempotency_key,provider_idempotency_key,amount,currency)
		VALUES($1,$2,'refund-1',$3,1250,'EUR')`, org, key, RefundLegKey(org, key, "refund-1")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Unblocked, it now observes the leg that committed while it waited.
	if err := <-done; !errors.Is(err, ErrRefundLegsBound) {
		t.Fatalf("err = %v, want ErrRefundLegsBound", err)
	}
	var whole int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM payment_compensations WHERE organizer_id=$1 AND kind='refund'`, org).Scan(&whole); err != nil {
		t.Fatal(err)
	}
	if whole != 0 {
		t.Fatalf("a whole refund bound alongside a partial leg: %d rows", whole)
	}
}

// The mirror image: BindRefundLeg must contend on the same row, or the exclusion only
// holds in one direction.
func TestBindRefundLegDecidesUnderTheOperationRowLock(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org, key := uuid.New(), "charge-leg-lock"
	seedCaptured(t, db, ctx, org, key, 2500)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM payment_operations WHERE organizer_id=$1 AND idempotency_key=$2 FOR UPDATE`, org, key).Scan(&one); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := j.BindRefundLeg(ctx, org, key, "refund-1", 1250, "EUR")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("BindRefundLeg returned (%v) without waiting for the operation row lock", err)
	case <-time.After(750 * time.Millisecond):
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO payment_compensations(organizer_id,source_idempotency_key,kind,provider_idempotency_key,amount,currency)
		VALUES($1,$2,'refund',$3,2500,'EUR')`, org, key, CompensationKey(org, key, "refund")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrWholeRefundBound) {
		t.Fatalf("err = %v, want ErrWholeRefundBound", err)
	}
}

// Completion is once-only and carries the journalled fact, so a replay after a crash
// between the provider call and the completion keeps the original result.
func TestCompleteRefundLegIsOnceOnly(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org, key := uuid.New(), "charge-complete"
	seedCaptured(t, db, ctx, org, key, 2500)

	if _, err := j.BindRefundLeg(ctx, org, key, "refund-1", 1250, "EUR"); err != nil {
		t.Fatal(err)
	}
	first, second := uuid.New(), uuid.New()
	if err := j.CompleteRefundLeg(ctx, org, key, "refund-1", "re_first", first); err != nil {
		t.Fatal(err)
	}
	if err := j.CompleteRefundLeg(ctx, org, key, "refund-1", "re_second", second); err != nil {
		t.Fatal(err)
	}
	leg, found, err := j.LookupRefundLeg(ctx, org, key, "refund-1")
	if err != nil || !found {
		t.Fatalf("lookup: %v found=%t", err, found)
	}
	if !leg.Completed || leg.FactID != first || leg.ProviderRef != "re_first" {
		t.Fatalf("completion must be once-only: %+v", leg)
	}
}

// A completed leg still counts against the ceiling — the money is gone, so its allowance
// is gone with it.
func TestCompletedRefundLegsCountAgainstTheCeiling(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org, key := uuid.New(), "charge-completed-ceiling"
	seedCaptured(t, db, ctx, org, key, 1000)

	if _, err := j.BindRefundLeg(ctx, org, key, "refund-1", 900, "EUR"); err != nil {
		t.Fatal(err)
	}
	if err := j.CompleteRefundLeg(ctx, org, key, "refund-1", "re_1", uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := j.BindRefundLeg(ctx, org, key, "refund-2", 200, "EUR"); !errors.Is(err, ErrRefundExceedsCapture) {
		t.Fatalf("err = %v, want ErrRefundExceedsCapture", err)
	}
}

// Two distinct refund legs append two distinct payment.refunded facts and the chain still
// verifies — the ADR-003 property this whole ticket is measured against.
func TestRefundLegFactsKeepTheJournalVerifiable(t *testing.T) {
	db, ctx := journalDB(t)
	ring := fullRing(t)
	j := New(db, ring)
	org, key := uuid.New(), "charge-chain"
	seedCaptured(t, db, ctx, org, key, 2500)
	buyer := uuid.New()

	for _, leg := range []struct {
		refundKey string
		amount    int64
	}{{"refund-1", 1250}, {"refund-2", 750}} {
		bound, err := j.BindRefundLeg(ctx, org, key, leg.refundKey, leg.amount, "EUR")
		if err != nil {
			t.Fatalf("bind %s: %v", leg.refundKey, err)
		}
		factID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("payment-leg:"+org.String()+":"+key+":"+leg.refundKey))
		if _, _, err := j.Append(ctx, Fact{
			ID: factID, OrganizerID: org, Type: "payment.refunded", OccurredAt: bound.BoundAt,
			BuyerID: buyer, Amount: leg.amount, Currency: "EUR",
			Payload: map[string]string{"order_id": uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)).String()},
		}); err != nil {
			t.Fatalf("append %s: %v", leg.refundKey, err)
		}
	}
	if err := j.Verify(ctx); err != nil {
		t.Fatalf("journal must still verify after two refund legs: %v", err)
	}
}
