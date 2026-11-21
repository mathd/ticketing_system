//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TKT-525 S2 (ADR-087): an abandonment marker releases ONLY its own leg's captured-money
// allowance. The whole-refund exclusion still counts every leg, marked or not, and the marked
// leg itself stays bound with its provider key.

func markAbandoned(t *testing.T, ctx context.Context, db *sql.DB, org uuid.UUID, sourceKey, refundKey string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `INSERT INTO payment_refund_leg_abandonments(organizer_id,source_idempotency_key,refund_idempotency_key,provider_ref)
		VALUES($1,$2,$3,'re_failed_marked')`, org, sourceKey, refundKey); err != nil {
		t.Fatal(err)
	}
}

// Two units at 1000 against a 2000 capture. Abandoning one of two bound legs leaves 1000
// counted: one fresh 1000 leg fits and the next unit is refused.
func TestAbandonedLegReleasesOnlyItsCapturedMoney(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org, key := uuid.New(), "charge-abandon-ceiling"
	seedCapturedKeepingMarkedLegs(t, db, ctx, org, key, 2000)

	a, err := j.BindRefundLeg(ctx, org, key, "refund-a", 1000, "EUR")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.BindRefundLeg(ctx, org, key, "refund-b", 1000, "EUR"); err != nil {
		t.Fatal(err)
	}
	if _, err := j.BindRefundLeg(ctx, org, key, "refund-c", 1000, "EUR"); !errors.Is(err, ErrRefundExceedsCapture) {
		t.Fatalf("before the marker: err = %v, want ErrRefundExceedsCapture", err)
	}

	markAbandoned(t, ctx, db, org, key, "refund-a")
	if _, err := j.BindRefundLeg(ctx, org, key, "refund-c", 1000, "EUR"); err != nil {
		t.Fatalf("after abandoning refund-a, a fresh 1000 leg must fit: %v", err)
	}
	// The bound leg refund-b and the fresh refund-c still count: nothing else was released.
	if _, err := j.BindRefundLeg(ctx, org, key, "refund-d", 1, "EUR"); !errors.Is(err, ErrRefundExceedsCapture) {
		t.Fatalf("one more unit: err = %v, want ErrRefundExceedsCapture", err)
	}

	// The marked leg is untouched: still bound, same provider key.
	again, found, err := j.LookupRefundLeg(ctx, org, key, "refund-a")
	if err != nil || !found {
		t.Fatalf("lookup marked leg: found=%v err=%v", found, err)
	}
	if again.Status != "bound" || again.ProviderKey != a.ProviderKey || again.Amount != 1000 {
		t.Fatalf("marked leg = %+v, want bound with its original provider key", again)
	}
}

// A completed leg keeps its allowance whatever the marker table holds for OTHER legs.
func TestAbandonedLegDoesNotReleaseACompletedLeg(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org, key := uuid.New(), "charge-abandon-completed"
	seedCapturedKeepingMarkedLegs(t, db, ctx, org, key, 2000)
	if _, err := j.BindRefundLeg(ctx, org, key, "refund-a", 1000, "EUR"); err != nil {
		t.Fatal(err)
	}
	if err := j.CompleteRefundLeg(ctx, org, key, "refund-a", "re_ok", uuid.New(), ConfirmedRefund{Amount: 1000, Currency: "EUR"}); err != nil {
		t.Fatal(err)
	}
	if _, err := j.BindRefundLeg(ctx, org, key, "refund-b", 1000, "EUR"); err != nil {
		t.Fatal(err)
	}
	markAbandoned(t, ctx, db, org, key, "refund-b")
	if _, err := j.BindRefundLeg(ctx, org, key, "refund-c", 1000, "EUR"); err != nil {
		t.Fatalf("refund-b's 1000 is released: %v", err)
	}
	if _, err := j.BindRefundLeg(ctx, org, key, "refund-d", 1, "EUR"); !errors.Is(err, ErrRefundExceedsCapture) {
		t.Fatalf("the completed leg and refund-c fill the capture: err = %v", err)
	}
}

// The whole-refund exclusion counts EVERY leg. A charge whose only leg is marked is still not
// eligible for a whole refund: the leg is bound history, and the recovery path must not refund
// the entire capture beside it.
func TestMarkedLegStillExcludesAWholeRefund(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org, key := uuid.New(), "charge-abandon-whole"
	seedCapturedKeepingMarkedLegs(t, db, ctx, org, key, 2000)
	if _, err := j.BindRefundLeg(ctx, org, key, "refund-a", 1000, "EUR"); err != nil {
		t.Fatal(err)
	}
	markAbandoned(t, ctx, db, org, key, "refund-a")
	if _, err := j.BindCompensation(ctx, org, key, "refund", 2000, "EUR"); !errors.Is(err, ErrRefundLegsBound) {
		t.Fatalf("whole refund beside a marked leg: err = %v, want ErrRefundLegsBound", err)
	}
}

// A marker for the same refund key under ANOTHER organizer or charge releases nothing here.
func TestAbandonmentMarkerIsScopedToItsExactLeg(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	org, key := uuid.New(), "charge-abandon-scope"
	seedCapturedKeepingMarkedLegs(t, db, ctx, org, key, 1000)
	other, otherKey := uuid.New(), "charge-abandon-scope-other"
	seedCapturedKeepingMarkedLegs(t, db, ctx, other, otherKey, 1000)
	if _, err := j.BindRefundLeg(ctx, org, key, "refund-a", 1000, "EUR"); err != nil {
		t.Fatal(err)
	}
	// Same refund key, another organizer's charge. Same refund key, another charge of ours.
	if _, err := j.BindRefundLeg(ctx, other, otherKey, "refund-a", 1000, "EUR"); err != nil {
		t.Fatal(err)
	}
	markAbandoned(t, ctx, db, other, otherKey, "refund-a")
	seedCapturedKeepingMarkedLegs(t, db, ctx, org, key+"-2", 1000)
	if _, err := j.BindRefundLeg(ctx, org, key+"-2", "refund-a", 1000, "EUR"); err != nil {
		t.Fatal(err)
	}
	markAbandoned(t, ctx, db, org, key+"-2", "refund-a")
	if _, err := j.BindRefundLeg(ctx, org, key, "refund-b", 1, "EUR"); !errors.Is(err, ErrRefundExceedsCapture) {
		t.Fatalf("a foreign or other-charge marker released this charge's leg: err = %v", err)
	}
}

// seedCapturedKeepingMarkedLegs is seedCaptured without the leg cleanup: a marked leg cannot
// be deleted (the marker's foreign key and append-only trigger hold it), and every fixture
// uses a fresh organizer, so the rows stay and nothing else reads them.
func seedCapturedKeepingMarkedLegs(t *testing.T, db *sql.DB, ctx context.Context, org uuid.UUID, key string, captured int64) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO payment_operations(organizer_id,idempotency_key,request_fingerprint,status,order_id,buyer_id,
		                               request_amount,request_currency,provider_payment_ref,provider_state,
		                               authorized_amount,captured_amount,provider_state_at)
		VALUES($1,$2,'fingerprint','captured',$3,$4,$5,'EUR','pi_test','captured',$5,$5,now())`,
		org, key, uuid.New(), uuid.New(), captured); err != nil {
		t.Fatal(err)
	}
}

// The marker write and the marker read are scoped to the exact composite leg key, organizer
// included. Two organizers can hold the same charge key and refund key.
func TestAbandonRefundLegIsScopedToTheOrganizer(t *testing.T) {
	db, ctx := journalDB(t)
	j := New(db, fullRing(t))
	ours, theirs, empty := uuid.New(), uuid.New(), uuid.New()
	key, refundKey := "charge-abandon-org-scope-"+uuid.NewString(), uuid.NewString()
	seedCapturedKeepingMarkedLegs(t, db, ctx, ours, key, 1000)
	seedCapturedKeepingMarkedLegs(t, db, ctx, theirs, key, 1000)
	seedCapturedKeepingMarkedLegs(t, db, ctx, empty, key, 1000)
	for _, org := range []uuid.UUID{ours, theirs} {
		if _, err := j.BindRefundLeg(ctx, org, key, refundKey, 1000, "EUR"); err != nil {
			t.Fatal(err)
		}
	}
	state, found, err := j.LookupRefundLegProofState(ctx, ours, key, refundKey)
	if err != nil || !found {
		t.Fatalf("proof state: found=%v err=%v", found, err)
	}
	if _, replay, err := j.AbandonRefundLeg(ctx, ours, key, refundKey, state, "re_ours"); err != nil || replay {
		t.Fatalf("abandon ours: replay=%v err=%v", replay, err)
	}
	if _, found, err := j.LookupRefundLegAbandonment(ctx, theirs, key, refundKey); err != nil || found {
		t.Fatalf("their identical leg reads as abandoned: found=%v err=%v", found, err)
	}
	// An organizer with the same charge key and no leg: not found, nothing written.
	if _, _, err := j.AbandonRefundLeg(ctx, empty, key, refundKey, state, "re_ours"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("abandon with no leg in this organizer: err = %v, want sql.ErrNoRows", err)
	}
	var markers int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM payment_refund_leg_abandonments WHERE source_idempotency_key=$1`, key).Scan(&markers); err != nil || markers != 1 {
		t.Fatalf("markers = %d err=%v, want exactly ours", markers, err)
	}
	// Their leg still counts in their ceiling.
	if _, err := j.BindRefundLeg(ctx, theirs, key, uuid.NewString(), 1, "EUR"); !errors.Is(err, ErrRefundExceedsCapture) {
		t.Fatalf("our marker released their leg: err = %v", err)
	}
}
