//go:build smoke

package store

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
)

// Detaching a claimed order, against real PostgreSQL (TKT-225 / ADR-052).
//
// Real Postgres rather than a fake: every claim this operation makes is a property
// of the SQL — the conditional UPDATE's predicates, the transaction that binds the
// attribution change to its audit row, and the CHECK constraints that refuse a
// blank reason. A fake store can be wrong about all of them.

// detachmentsFor reads the audit trail for one order, newest first.
func detachmentsFor(t *testing.T, db *sql.DB, order uuid.UUID) []struct {
	Customer uuid.UUID
	Reason   string
	Actor    string
} {
	t.Helper()
	rows, err := db.Query(`
		SELECT customer_id, reason, actor
		  FROM order_attribution_detachments
		 WHERE order_id = $1
		 ORDER BY detached_at DESC`, order)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []struct {
		Customer uuid.UUID
		Reason   string
		Actor    string
	}
	for rows.Next() {
		var r struct {
			Customer uuid.UUID
			Reason   string
			Actor    string
		}
		if err := rows.Scan(&r.Customer, &r.Reason, &r.Actor); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func attributionOf(t *testing.T, db *sql.DB, order uuid.UUID) uuid.NullUUID {
	t.Helper()
	var got uuid.NullUUID
	if err := db.QueryRow(`SELECT customer_id FROM orders WHERE id = $1`, order).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestDetachRestoresTheOrderToUnattributedAndRecordsIt(t *testing.T) {
	db, ctx := outboxDB(t)
	customer := registerClaimant(t, db, ctx, "detach-owner")
	order, _ := seedClaimable(t, db, ctx, "completed", uuid.NullUUID{UUID: customer, Valid: true})

	detached, err := DetachOrderAttribution(ctx, db, order, "staff:amy", "claimed by the wrong account")
	if err != nil {
		t.Fatal(err)
	}
	if detached != customer {
		t.Fatalf("detached from %s, want %s — the audit trail would name the wrong account", detached, customer)
	}
	if got := attributionOf(t, db, order); got.Valid {
		t.Fatalf("order still attributed to %s after a detach", got.UUID)
	}

	records := detachmentsFor(t, db, order)
	if len(records) != 1 {
		t.Fatalf("recorded %d detachments, want exactly 1", len(records))
	}
	if records[0].Customer != customer || records[0].Actor != "staff:amy" ||
		records[0].Reason != "claimed by the wrong account" {
		t.Fatalf("audit row = %+v; it must name who lost the order, who took it and why", records[0])
	}
}

// The order is otherwise untouched. `updated_at` in particular means "when did
// this order's CHECKOUT last move" — recovery reads it to decide what is stale —
// and a support action months later is not checkout activity.
func TestDetachTouchesNothingButAttribution(t *testing.T) {
	db, ctx := outboxDB(t)
	customer := registerClaimant(t, db, ctx, "detach-untouched")
	order, _ := seedClaimable(t, db, ctx, "completed", uuid.NullUUID{UUID: customer, Valid: true})

	var beforeStatus string
	var beforeUpdated, beforeCreated any
	if err := db.QueryRow(`SELECT status, updated_at, created_at FROM orders WHERE id = $1`, order).
		Scan(&beforeStatus, &beforeUpdated, &beforeCreated); err != nil {
		t.Fatal(err)
	}

	if _, err := DetachOrderAttribution(ctx, db, order, "staff:amy", "wrong account"); err != nil {
		t.Fatal(err)
	}

	var afterStatus string
	var afterUpdated, afterCreated any
	if err := db.QueryRow(`SELECT status, updated_at, created_at FROM orders WHERE id = $1`, order).
		Scan(&afterStatus, &afterUpdated, &afterCreated); err != nil {
		t.Fatal(err)
	}
	if afterStatus != beforeStatus {
		t.Fatalf("status moved %q -> %q; a detach is not a state transition (ADR-016)", beforeStatus, afterStatus)
	}
	if afterUpdated != beforeUpdated {
		t.Fatal("updated_at moved; recovery reads it as checkout staleness and a support action is not checkout activity")
	}
	if afterCreated != beforeCreated {
		t.Fatal("created_at moved")
	}
}

// The predicate that stops the audit trail lying. Without `customer_id IS NOT
// NULL` this reports success and writes a row describing a detachment that did
// not happen.
func TestDetachingAnUnattributedOrderIsRefusedAndRecordsNothing(t *testing.T) {
	db, ctx := outboxDB(t)
	order, _ := seedClaimable(t, db, ctx, "completed", uuid.NullUUID{})

	if _, err := DetachOrderAttribution(ctx, db, order, "staff:amy", "nothing to detach"); err != ErrOrderNotDetachable {
		t.Fatalf("err = %v, want ErrOrderNotDetachable", err)
	}
	if records := detachmentsFor(t, db, order); len(records) != 0 {
		t.Fatalf("recorded %d detachments for an order that was never attributed: %+v", len(records), records)
	}
}

func TestDetachRefusesAnOrderThatIsNotCompleted(t *testing.T) {
	db, ctx := outboxDB(t)
	customer := registerClaimant(t, db, ctx, "detach-pending")
	order, _ := seedClaimable(t, db, ctx, "payment_pending", uuid.NullUUID{UUID: customer, Valid: true})

	if _, err := DetachOrderAttribution(ctx, db, order, "staff:amy", "too early"); err != ErrOrderNotDetachable {
		t.Fatalf("err = %v, want ErrOrderNotDetachable", err)
	}
	if got := attributionOf(t, db, order); !got.Valid || got.UUID != customer {
		t.Fatal("attribution changed on an order that is not completed")
	}
}

func TestDetachRefusesAnOrderThatDoesNotExist(t *testing.T) {
	db, ctx := outboxDB(t)
	if _, err := DetachOrderAttribution(ctx, db, uuid.New(), "staff:amy", "no such order"); err != ErrOrderNotDetachable {
		t.Fatalf("err = %v, want ErrOrderNotDetachable", err)
	}
}

// A detach that describes nothing is refused BEFORE the order is looked at, so
// the refusal is identical whether or not the order exists — a blank reason
// cannot be used to probe.
func TestDetachRefusesABlankActorOrReason(t *testing.T) {
	db, ctx := outboxDB(t)
	customer := registerClaimant(t, db, ctx, "detach-blank")
	order, _ := seedClaimable(t, db, ctx, "completed", uuid.NullUUID{UUID: customer, Valid: true})

	for _, tc := range []struct{ name, actor, reason string }{
		{"no actor", "", "wrong account"},
		{"no reason", "staff:amy", ""},
		{"whitespace actor", "   ", "wrong account"},
		{"whitespace reason", "staff:amy", "\t\n "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DetachOrderAttribution(ctx, db, order, tc.actor, tc.reason); err != ErrDetachmentNotDescribed {
				t.Fatalf("err = %v, want ErrDetachmentNotDescribed", err)
			}
		})
	}
	// And the order is untouched by any of them.
	if got := attributionOf(t, db, order); !got.Valid || got.UUID != customer {
		t.Fatal("a refused detach still changed the attribution")
	}
	if records := detachmentsFor(t, db, order); len(records) != 0 {
		t.Fatalf("a refused detach wrote %d audit rows", len(records))
	}
}

// The behaviour ADR-052 chose, pinned so a later "hardening" that blocks re-claim
// has to argue with a test rather than silently reverse the decision.
//
// The rightful buyer claiming after support fixed a mis-claim is the PRIMARY case
// this operation exists for. An attacker re-claiming is the same path, and what
// bounds them is ADR-051's rate limiting plus fixing the leak itself (TKT-202) —
// not a block that would also refuse the buyer.
func TestADetachedOrderCanBeClaimedAgain(t *testing.T) {
	db, ctx := outboxDB(t)
	wrong := registerClaimant(t, db, ctx, "detach-wrong")
	rightful := registerClaimant(t, db, ctx, "detach-rightful")
	order, ref := seedClaimable(t, db, ctx, "completed", uuid.NullUUID{UUID: wrong, Valid: true})

	if _, err := DetachOrderAttribution(ctx, db, order, "staff:amy", "claimed by the wrong account"); err != nil {
		t.Fatal(err)
	}

	// The rightful buyer, who could not claim while it was attributed to someone
	// else, now can.
	if _, err := ClaimGuestOrder(ctx, db, ref, rightful); err != nil {
		t.Fatalf("the rightful buyer cannot claim a detached order: %v", err)
	}
	if got := attributionOf(t, db, order); !got.Valid || got.UUID != rightful {
		t.Fatalf("attribution = %+v, want %s", got, rightful)
	}
}

// Detaching twice: the second finds nothing to detach and adds no second record.
func TestDetachingTwiceRecordsOnlyTheDetachmentThatHappened(t *testing.T) {
	db, ctx := outboxDB(t)
	customer := registerClaimant(t, db, ctx, "detach-twice")
	order, _ := seedClaimable(t, db, ctx, "completed", uuid.NullUUID{UUID: customer, Valid: true})

	if _, err := DetachOrderAttribution(ctx, db, order, "staff:amy", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := DetachOrderAttribution(ctx, db, order, "staff:bo", "second"); err != ErrOrderNotDetachable {
		t.Fatalf("err = %v, want ErrOrderNotDetachable on the second detach", err)
	}
	records := detachmentsFor(t, db, order)
	if len(records) != 1 || records[0].Actor != "staff:amy" {
		t.Fatalf("records = %+v, want exactly the first detachment", records)
	}
}

// The database refuses a blank reason even when Go does not ask it to. The Go
// check and the CHECK constraint are two different adversaries: a caller bug, and
// a psql session.
func TestTheDatabaseItselfRefusesABlankReasonOrActor(t *testing.T) {
	db, ctx := outboxDB(t)
	customer := registerClaimant(t, db, ctx, "detach-constraint")
	order, _ := seedClaimable(t, db, ctx, "completed", uuid.NullUUID{UUID: customer, Valid: true})

	for _, tc := range []struct{ name, actor, reason string }{
		{"blank reason", "staff:amy", "   "},
		{"blank actor", "", "a reason"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, `
				INSERT INTO order_attribution_detachments(id, order_id, customer_id, reason, actor)
				VALUES($1,$2,$3,$4,$5)`, uuid.New(), order, customer, tc.reason, tc.actor)
			if err == nil {
				t.Fatal("the database accepted a detachment record that says nothing about itself")
			}
		})
	}
}
