//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Claiming a guest order against real Postgres (TKT-223 / US-A4).

func seedClaimable(t *testing.T, db *sql.DB, ctx context.Context, status string, customer uuid.NullUUID) (uuid.UUID, uuid.UUID) {
	t.Helper()
	reservation, order, ref := uuid.New(), uuid.New(), uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,1,4550,4550,4550,'EUR','completed')`,
		reservation, uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,guest_order_ref,customer_id)
		VALUES($1,$2,$3,$4,'fingerprint',$5,$6)`,
		order, reservation, status, "claim-"+uuid.NewString(), ref, customer); err != nil {
		t.Fatal(err)
	}
	return order, ref
}

func registerClaimant(t *testing.T, db *sql.DB, ctx context.Context, local string) uuid.UUID {
	t.Helper()
	account, err := RegisterCustomer(ctx, db, uniqueEmail(local), "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	return account.ID
}

func TestClaimAttributesACompletedGuestOrder(t *testing.T) {
	db, ctx := outboxDB(t)
	customer := registerClaimant(t, db, ctx, "claimant")
	order, ref := seedClaimable(t, db, ctx, "completed", uuid.NullUUID{})

	got, err := ClaimGuestOrder(ctx, db, ref, customer)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got != order {
		t.Fatalf("claimed %s, want %s", got, order)
	}
	if attribution := attributionOf(t, db, ctx, order); !attribution.Valid || attribution.UUID != customer {
		t.Fatalf("attribution = %v, want %s", attribution, customer)
	}

	// It now appears in that customer's wallet — the whole point, and an
	// inference until asserted (the wallet also requires guest_order_ref, which a
	// claimed order has by construction).
	page, _, err := CustomerOrders(ctx, db, customer, WalletCursor{}, WalletPageLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].OrderID != order {
		t.Fatalf("wallet = %+v, want the claimed order %s", page, order)
	}
}

// Idempotent by predicate, not by replay detection: the second claim takes the
// same branch and gets the same answer, so a browser retry is safe.
func TestClaimingTwiceAsTheSameCustomerSucceedsBothTimes(t *testing.T) {
	db, ctx := outboxDB(t)
	customer := registerClaimant(t, db, ctx, "twice")
	_, ref := seedClaimable(t, db, ctx, "completed", uuid.NullUUID{})

	first, err := ClaimGuestOrder(ctx, db, ref, customer)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ClaimGuestOrder(ctx, db, ref, customer)
	if err != nil {
		t.Fatalf("the same customer claiming again must succeed, not error: %v", err)
	}
	if first != second {
		t.Fatalf("two claims returned different orders: %s and %s", first, second)
	}
}

// Every refused case is the SAME error, because the store cannot be the place the
// distinction leaks either.
func TestClaimRefusesEveryUnclaimableOrder(t *testing.T) {
	db, ctx := outboxDB(t)
	mine := registerClaimant(t, db, ctx, "mine")
	theirs := registerClaimant(t, db, ctx, "theirs")

	_, incomplete := seedClaimable(t, db, ctx, "created", uuid.NullUUID{})
	_, taken := seedClaimable(t, db, ctx, "completed", uuid.NullUUID{UUID: theirs, Valid: true})

	for _, tc := range []struct {
		name string
		ref  uuid.UUID
	}{
		{"no such order", uuid.New()},
		{"not completed", incomplete},
		{"already claimed by somebody else", taken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ClaimGuestOrder(ctx, db, tc.ref, mine); !errors.Is(err, ErrOrderNotClaimable) {
				t.Fatalf("want ErrOrderNotClaimable, got %v", err)
			}
		})
	}
}

// A claim must not perturb the checkout's own timeline. `updated_at` means "when
// did this order's CHECKOUT last move" — recovery reads it to decide what is
// stale — and a claim months later is not checkout activity.
func TestClaimDoesNotTouchUpdatedAt(t *testing.T) {
	db, ctx := outboxDB(t)
	customer := registerClaimant(t, db, ctx, "timeline")
	order, ref := seedClaimable(t, db, ctx, "completed", uuid.NullUUID{})

	var before time.Time
	if err := db.QueryRowContext(ctx, `SELECT updated_at FROM orders WHERE id=$1`, order).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimGuestOrder(ctx, db, ref, customer); err != nil {
		t.Fatal(err)
	}
	var after time.Time
	if err := db.QueryRowContext(ctx, `SELECT updated_at FROM orders WHERE id=$1`, order).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) {
		t.Fatalf("updated_at moved from %s to %s — a claim is not checkout activity, and recovery "+
			"reads this column to decide what is stale", before, after)
	}
}

// The contention proof, and it FORCES the interleaving rather than looping.
//
// Repetition cannot falsify a race fix
// (docs/learnings/2026-07-29-force-the-interleaving-repetition-cannot-falsify-a-race-fix.md):
// two goroutines racing usually serialize by luck and the test passes against
// code that would lose under real contention. So a blocker transaction holds the
// order row, both claimants are launched and observed to be WAITING on it, and
// only then is the blocker released — which puts both UPDATEs in the lock queue
// with the outcome genuinely undecided.
//
// Under READ COMMITTED the loser's UPDATE re-evaluates its predicate against the
// winner's committed row, finds customer_id already set to somebody else, matches
// nothing, and reports zero rows. That is the property: exactly one winner, and
// the loser gets the ordinary refusal rather than an error.
func TestTwoCustomersClaimingAtOnceProduceExactlyOneWinner(t *testing.T) {
	db, ctx := outboxDB(t)
	alice := registerClaimant(t, db, ctx, "race-alice")
	bob := registerClaimant(t, db, ctx, "race-bob")
	order, ref := seedClaimable(t, db, ctx, "completed", uuid.NullUUID{})

	// The blocker: hold the row so neither claimant can proceed.
	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback() }()
	if _, err := blocker.ExecContext(ctx, `SELECT 1 FROM orders WHERE id=$1 FOR UPDATE`, order); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		id  uuid.UUID
		err error
	}
	results := make(chan outcome, 2)
	for _, claimant := range []uuid.UUID{alice, bob} {
		go func(customer uuid.UUID) {
			id, err := ClaimGuestOrder(ctx, db, ref, customer)
			results <- outcome{id, err}
		}(claimant)
	}

	// Wait until BOTH are genuinely queued on the row, so releasing the blocker
	// starts a real race. Polling pg_locks rather than sleeping: a sleep is a
	// guess that turns a race test into a slow non-test.
	deadline := time.Now().Add(20 * time.Second)
	for {
		var waiting int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM pg_stat_activity
			 WHERE wait_event_type = 'Lock' AND state = 'active' AND query LIKE '%UPDATE orders%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d claimant(s) reached the lock queue; the interleaving was never "+
				"forced and this test would prove nothing", waiting)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}

	var winners, refusals int
	for range 2 {
		got := <-results
		switch {
		case got.err == nil && got.id == order:
			winners++
		case errors.Is(got.err, ErrOrderNotClaimable):
			refusals++
		default:
			t.Fatalf("unexpected outcome: id=%s err=%v", got.id, got.err)
		}
	}
	if winners != 1 || refusals != 1 {
		t.Fatalf("winners=%d refusals=%d, want exactly one of each — both winning means the row "+
			"was repointed, both losing means nobody can ever claim", winners, refusals)
	}

	// And the row names one of them, not a mixture.
	attribution := attributionOf(t, db, ctx, order)
	if !attribution.Valid || (attribution.UUID != alice && attribution.UUID != bob) {
		t.Fatalf("attribution = %v, want exactly one of the two claimants", attribution)
	}
}
