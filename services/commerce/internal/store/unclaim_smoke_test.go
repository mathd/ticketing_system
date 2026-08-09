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

// attributionOf lives in attribution_smoke_test.go — same package, and the read
// it does is the same one this file needs.

func TestDetachRestoresTheOrderToUnattributedAndRecordsIt(t *testing.T) {
	db, ctx := outboxDB(t)
	customer := registerClaimant(t, db, ctx, "detach-owner")
	order, _ := seedClaimable(t, db, ctx, "completed", uuid.NullUUID{UUID: customer, Valid: true})

	detached, err := DetachOrderAttribution(ctx, db, order, "key-"+uuid.NewString(), "staff:amy", "claimed by the wrong account")
	if err != nil {
		t.Fatal(err)
	}
	if detached != customer {
		t.Fatalf("detached from %s, want %s — the audit trail would name the wrong account", detached, customer)
	}
	if got := attributionOf(t, db, ctx, order); got.Valid {
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

	if _, err := DetachOrderAttribution(ctx, db, order, "key-"+uuid.NewString(), "staff:amy", "wrong account"); err != nil {
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

	if _, err := DetachOrderAttribution(ctx, db, order, "key-"+uuid.NewString(), "staff:amy", "nothing to detach"); err != ErrOrderNotDetachable {
		t.Fatalf("err = %v, want ErrOrderNotDetachable", err)
	}
	if records := detachmentsFor(t, db, order); len(records) != 0 {
		t.Fatalf("recorded %d detachments for an order that was never attributed: %+v", len(records), records)
	}
}

// `status = 'completed'` mirrors the claim's own predicate: the two operations
// bracket the same window, and an order that was never claimable is not one to
// un-claim.
//
// Both directions of "not completed" are covered — an order still in flight
// (`confirmation_pending`) and one whose life has moved past a purchase
// (`refunded`) — because a single fixture on one side would pass against a
// predicate that tested the wrong bound.
func TestDetachRefusesAnOrderThatIsNotCompleted(t *testing.T) {
	db, ctx := outboxDB(t)
	for _, status := range []string{"confirmation_pending", "refunded"} {
		t.Run(status, func(t *testing.T) {
			customer := registerClaimant(t, db, ctx, "detach-"+status)
			order, _ := seedClaimable(t, db, ctx, status, uuid.NullUUID{UUID: customer, Valid: true})

			if _, err := DetachOrderAttribution(ctx, db, order, "key-"+uuid.NewString(), "staff:amy", "wrong state"); err != ErrOrderNotDetachable {
				t.Fatalf("err = %v, want ErrOrderNotDetachable", err)
			}
			if got := attributionOf(t, db, ctx, order); !got.Valid || got.UUID != customer {
				t.Fatalf("attribution changed on an order whose status is %q", status)
			}
			if records := detachmentsFor(t, db, order); len(records) != 0 {
				t.Fatalf("a refused detach wrote %d audit rows", len(records))
			}
		})
	}
}

func TestDetachRefusesAnOrderThatDoesNotExist(t *testing.T) {
	db, ctx := outboxDB(t)
	if _, err := DetachOrderAttribution(ctx, db, uuid.New(), "key-"+uuid.NewString(), "staff:amy", "no such order"); err != ErrOrderNotDetachable {
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
			if _, err := DetachOrderAttribution(ctx, db, order, "key-"+uuid.NewString(), tc.actor, tc.reason); err != ErrDetachmentNotDescribed {
				t.Fatalf("err = %v, want ErrDetachmentNotDescribed", err)
			}
		})
	}
	// And the order is untouched by any of them.
	if got := attributionOf(t, db, ctx, order); !got.Valid || got.UUID != customer {
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

	if _, err := DetachOrderAttribution(ctx, db, order, "key-"+uuid.NewString(), "staff:amy", "claimed by the wrong account"); err != nil {
		t.Fatal(err)
	}

	// The rightful buyer, who could not claim while it was attributed to someone
	// else, now can.
	if _, err := ClaimGuestOrder(ctx, db, ref, rightful); err != nil {
		t.Fatalf("the rightful buyer cannot claim a detached order: %v", err)
	}
	if got := attributionOf(t, db, ctx, order); !got.Valid || got.UUID != rightful {
		t.Fatalf("attribution = %+v, want %s", got, rightful)
	}
}

// Detaching twice: the second finds nothing to detach and adds no second record.
func TestDetachingTwiceRecordsOnlyTheDetachmentThatHappened(t *testing.T) {
	db, ctx := outboxDB(t)
	customer := registerClaimant(t, db, ctx, "detach-twice")
	order, _ := seedClaimable(t, db, ctx, "completed", uuid.NullUUID{UUID: customer, Valid: true})

	if _, err := DetachOrderAttribution(ctx, db, order, "key-"+uuid.NewString(), "staff:amy", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := DetachOrderAttribution(ctx, db, order, "key-"+uuid.NewString(), "staff:bo", "second"); err != ErrOrderNotDetachable {
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

// The race that made the Idempotency-Key necessary (ai-review [high], TKT-225).
//
// A detached order is immediately re-claimable by design (ADR-052 § 4), so a
// detach is NOT naturally idempotent. Without a key: detach A, the HTTP response
// is lost, B claims the now-free order, the caller retries the identical
// request — and the retry detaches B. A customer the operator never reviewed
// loses their purchase, recorded under the reason written about someone else, and
// retry timing decides who.
//
// This drives that exact sequence. With the key, the retry is a replay: it returns
// the customer the FIRST call detached, and B keeps the order.
func TestARetryAfterAnInterveningClaimDoesNotDetachTheNewOwner(t *testing.T) {
	db, ctx := outboxDB(t)
	first := registerClaimant(t, db, ctx, "retry-first")
	intervening := registerClaimant(t, db, ctx, "retry-intervening")
	order, ref := seedClaimable(t, db, ctx, "completed", uuid.NullUUID{UUID: first, Valid: true})

	const key = "support-ticket-4471"

	detached, err := DetachOrderAttribution(ctx, db, order, key, "staff:amy", "claimed by the wrong account")
	if err != nil {
		t.Fatal(err)
	}
	if detached != first {
		t.Fatalf("detached %s, want %s", detached, first)
	}

	// The response is lost. Meanwhile somebody claims the freed order.
	if _, err := ClaimGuestOrder(ctx, db, ref, intervening); err != nil {
		t.Fatal(err)
	}

	// The operator's client retries the identical request.
	replayed, err := DetachOrderAttribution(ctx, db, order, key, "staff:amy", "claimed by the wrong account")
	if err != nil {
		t.Fatalf("the retry failed instead of replaying: %v", err)
	}
	if replayed != first {
		t.Fatalf("the retry reported %s; a replay must report the customer the FIRST call detached (%s)",
			replayed, first)
	}

	// The intervening claimant still has the order, and no second row was written.
	if got := attributionOf(t, db, ctx, order); !got.Valid || got.UUID != intervening {
		t.Fatalf("attribution = %+v; the retry took the order from a customer the operator never reviewed", got)
	}
	if records := detachmentsFor(t, db, order); len(records) != 1 {
		t.Fatalf("recorded %d detachments, want 1 — a replay must not add evidence of a second act", len(records))
	}
}

// A DIFFERENT key on the same order is a new decision, not a replay: the operator
// looked again and asked for another detach. That must work, or support cannot fix
// a second mis-claim on the same order.
func TestADifferentKeyOnTheSameOrderIsANewDetachment(t *testing.T) {
	db, ctx := outboxDB(t)
	first := registerClaimant(t, db, ctx, "twokeys-first")
	second := registerClaimant(t, db, ctx, "twokeys-second")
	order, ref := seedClaimable(t, db, ctx, "completed", uuid.NullUUID{UUID: first, Valid: true})

	if _, err := DetachOrderAttribution(ctx, db, order, "support-1", "staff:amy", "first mistake"); err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimGuestOrder(ctx, db, ref, second); err != nil {
		t.Fatal(err)
	}
	detached, err := DetachOrderAttribution(ctx, db, order, "support-2", "staff:amy", "second mistake")
	if err != nil {
		t.Fatal(err)
	}
	if detached != second {
		t.Fatalf("the second detachment reported %s, want the CURRENT owner %s", detached, second)
	}
	if records := detachmentsFor(t, db, order); len(records) != 2 {
		t.Fatalf("recorded %d detachments, want 2 — each key is its own decision", len(records))
	}
}

func TestDetachRefusesABlankIdempotencyKey(t *testing.T) {
	db, ctx := outboxDB(t)
	customer := registerClaimant(t, db, ctx, "blank-key")
	order, _ := seedClaimable(t, db, ctx, "completed", uuid.NullUUID{UUID: customer, Valid: true})

	for _, key := range []string{"", "   "} {
		if _, err := DetachOrderAttribution(ctx, db, order, key, "staff:amy", "a reason"); err != ErrDetachmentNotDescribed {
			t.Fatalf("key %q: err = %v, want ErrDetachmentNotDescribed", key, err)
		}
	}
	if got := attributionOf(t, db, ctx, order); !got.Valid || got.UUID != customer {
		t.Fatal("a refused detach still changed the attribution")
	}
}

// The two recovery branches, forced deterministically (ai-review pass 3 [medium]).
//
// The first version of this test just started two goroutines and hoped. It passed
// with the ErrNoRows recovery DELETED — verified — because a scheduler that
// serializes the two calls takes the ordinary fast-replay path and proves nothing
// about either branch. That is the fixture-too-small trap, one level up: the test
// exercised the happy path and was named for the race.
//
// So these drive real transactions by hand, in a fixed order, so each branch is
// entered by construction rather than by timing.

// Branch 1: both attempts miss the replay read; the winner commits; the loser's
// UPDATE finds an already-NULL row. It must report the winner's customer, not
// ErrOrderNotDetachable — the caller's KEY succeeded even though this attempt did
// no work.
func TestALoserWhoseUpdateFindsNothingReplaysTheWinner(t *testing.T) {
	db, ctx := outboxDB(t)
	customer := registerClaimant(t, db, ctx, "loser-norows")
	order, _ := seedClaimable(t, db, ctx, "completed", uuid.NullUUID{UUID: customer, Valid: true})
	const key = "support-norows"

	// The "loser" opens its transaction and performs the replay read FIRST, while
	// the row is still attributed — so it misses, exactly as it would in the race.
	loser, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = loser.Rollback() }()
	if _, found, err := lookupDetachment(ctx, loser, order, key); err != nil || found {
		t.Fatalf("the loser's replay read should miss: found=%v err=%v", found, err)
	}

	// The winner completes the whole operation and commits.
	if _, err := DetachOrderAttribution(ctx, db, order, key, "staff:amy", "winner"); err != nil {
		t.Fatal(err)
	}
	_ = loser.Rollback()

	// Now the loser's request proceeds. Its UPDATE finds nothing to detach.
	replayed, err := DetachOrderAttribution(ctx, db, order, key, "staff:amy", "winner")
	if err != nil {
		t.Fatalf("the loser reported failure for a key whose operation succeeded: %v", err)
	}
	if replayed != customer {
		t.Fatalf("the loser reported %s, want the winner's customer %s", replayed, customer)
	}
	if records := detachmentsFor(t, db, order); len(records) != 1 {
		t.Fatalf("recorded %d detachments, want exactly 1", len(records))
	}
}

// Branch 2: the loser's UPDATE SUCCEEDS — because the order was re-claimed in
// between — and it then collides on the unique index. Its whole transaction must
// roll back, leaving the new claimant's attribution intact, and it must report the
// winner's customer.
//
// This is the branch the goroutine test could not reach at all: without an
// intervening claim the loser's UPDATE finds NULL and takes branch 1.
func TestALoserWhoCollidesOnTheKeyRollsBackAndReplays(t *testing.T) {
	db, ctx := outboxDB(t)
	first := registerClaimant(t, db, ctx, "collide-first")
	second := registerClaimant(t, db, ctx, "collide-second")
	order, ref := seedClaimable(t, db, ctx, "completed", uuid.NullUUID{UUID: first, Valid: true})
	const key = "support-collide"

	// The winner detaches and commits.
	if _, err := DetachOrderAttribution(ctx, db, order, key, "staff:amy", "winner"); err != nil {
		t.Fatal(err)
	}
	// Somebody claims the freed order.
	if _, err := ClaimGuestOrder(ctx, db, ref, second); err != nil {
		t.Fatal(err)
	}

	// The stale attempt arrives with the SAME key. Its UPDATE now succeeds — the
	// order is attributed again — and the INSERT collides.
	replayed, err := DetachOrderAttribution(ctx, db, order, key, "staff:amy", "winner")
	if err != nil {
		t.Fatalf("the colliding attempt errored instead of replaying: %v", err)
	}
	if replayed != first {
		t.Fatalf("reported %s, want the customer the FIRST call detached (%s)", replayed, first)
	}

	// The rollback is the point: the new claimant must still hold the order.
	if got := attributionOf(t, db, ctx, order); !got.Valid || got.UUID != second {
		t.Fatalf("attribution = %+v; the colliding attempt took the order from a customer "+
			"the operator never reviewed", got)
	}
	if records := detachmentsFor(t, db, order); len(records) != 1 {
		t.Fatalf("recorded %d detachments for this key, want exactly 1", len(records))
	}
}

// Two concurrent FIRST attempts with the same key (ai-review pass 2).
//
// The replay SELECT is a fast path, not the authority: under READ COMMITTED both
// transactions read no row and both proceed, and the unique index is what
// serializes them. The loser must report what the winner did — the same answer a
// replay gives — rather than a raw constraint violation, and exactly one
// detachment must be recorded.
func TestTwoConcurrentDetachesWithTheSameKeyProduceOneDetachment(t *testing.T) {
	db, ctx := outboxDB(t)
	customer := registerClaimant(t, db, ctx, "concurrent-key")
	order, _ := seedClaimable(t, db, ctx, "completed", uuid.NullUUID{UUID: customer, Valid: true})

	const key = "support-concurrent"
	type outcome struct {
		detached uuid.UUID
		err      error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			d, err := DetachOrderAttribution(ctx, db, order, key, "staff:amy", "same key, same instant")
			results <- outcome{d, err}
		}()
	}
	close(start)

	var succeeded int
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Errorf("a concurrent attempt failed instead of agreeing with the winner: %v", got.err)
			continue
		}
		if got.detached != customer {
			t.Errorf("reported %s, want %s — both attempts must name the customer that lost the order",
				got.detached, customer)
		}
		succeeded++
	}
	if succeeded != 2 {
		t.Fatalf("%d of 2 attempts succeeded; both must, one doing the work and one replaying", succeeded)
	}

	if records := detachmentsFor(t, db, order); len(records) != 1 {
		t.Fatalf("recorded %d detachments, want exactly 1", len(records))
	}
	if got := attributionOf(t, db, ctx, order); got.Valid {
		t.Fatalf("order still attributed to %s", got.UUID)
	}
}
