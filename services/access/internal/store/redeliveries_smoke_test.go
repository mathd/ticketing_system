//go:build smoke

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Staff-triggered redelivery (TKT-203). The invariants these tests pin, each stated
// without naming the implementation:
//
//   - A resend covers EVERY ticket of the order, including one already delivered.
//   - A resend's message id is one the transport has never been handed before.
//   - A resend moves the trail: each accepted send is its own recorded event, and the
//     chain still verifies.
//   - One key resends once, however many times it is presented.
//   - A key belongs to one order; the bound belongs to the order, not the caller.

// deliverOriginal drives the ORIGINAL delivery path so the fixture reaches the state
// a resend actually meets in production: a ticket carrying a `delivered` event and an
// accepted delivery_attempts row. Hand-inserting either would be a fixture built from
// the thing under test, and the `delivered` event has to be chained or the verifier
// reads it as tampering.
func deliverOriginal(t *testing.T, ctx context.Context, st *Postgres, ticketID uuid.UUID) uuid.UUID {
	t.Helper()
	msg, err := st.DeliveryID(ctx, ticketID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkDelivered(ctx, ticketID, msg); err != nil {
		t.Fatal(err)
	}
	return msg
}

func claimAndAccept(t *testing.T, ctx context.Context, st *Postgres, org, order uuid.UUID, key string) RedeliveryClaim {
	t.Helper()
	claim, err := st.ClaimRedelivery(ctx, org, order, key)
	if err != nil {
		t.Fatalf("claim %q: %v", key, err)
	}
	for _, tk := range claim.Tickets {
		if err := st.MarkRedelivered(ctx, org, key, tk.TicketID, tk.MessageID); err != nil {
			t.Fatalf("mark redelivered %s: %v", tk.TicketID, err)
		}
	}
	return claim
}

// COS-1. The whole point of the feature: an already-delivered ticket is exactly the
// one the original path refuses to touch, and it must be resent.
//
// The already-delivered seed is load-bearing, not decoration: DELETE IT and an
// implementation built on PendingDeliveries passes, because every remaining ticket is
// pending and the filter is invisible.
func TestRedeliveryResendsTicketsThatWereAlreadyDelivered(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	order, sorted, _ := issueOrder(t, ctx, st, org, 3)

	// One of the three has already been delivered. The other two have not: a
	// fixture where ALL are delivered could not distinguish "resends everything"
	// from "resends only the delivered ones".
	deliverOriginal(t, ctx, st, sorted[0])

	claim, err := st.ClaimRedelivery(ctx, org, order, "key-all-tickets")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claim.Tickets) != 3 {
		t.Fatalf("resend covers %d tickets, want all 3 of the order — an already-delivered ticket was skipped", len(claim.Tickets))
	}
	got := map[uuid.UUID]bool{}
	for _, tk := range claim.Tickets {
		got[tk.TicketID] = true
	}
	for _, want := range sorted {
		if !got[want] {
			t.Fatalf("ticket %s is missing from the resend", want)
		}
	}
}

// COS-3. A resend hands the transport a message id it has never seen. Asserted
// against the database rather than the returned struct, because the uniqueness that
// matters is the one PostgreSQL enforces.
func TestRedeliveryMintsAMessageIDDistinctFromTheOriginalDelivery(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	order, sorted, _ := issueOrder(t, ctx, st, org, 2)
	original := deliverOriginal(t, ctx, st, sorted[0])

	before := countRows(t, ctx, db, `SELECT count(*) FROM redelivery_attempts WHERE organizer_id=$1`, org)
	if before != 0 {
		t.Fatalf("fixture already has %d resend attempts", before)
	}

	claim := claimAndAccept(t, ctx, st, org, order, "key-fresh-message-id")

	after := countRows(t, ctx, db, `SELECT count(*) FROM redelivery_attempts WHERE organizer_id=$1`, org)
	if after != 2 {
		t.Fatalf("resend recorded %d attempts, want one per ticket (2)", after)
	}
	for _, tk := range claim.Tickets {
		if tk.MessageID == original {
			t.Fatalf("ticket %s was resent under the ORIGINAL delivery's message id %s: a transport that deduplicates on message id drops this as a replay", tk.TicketID, original)
		}
		if tk.MessageID == uuid.Nil {
			t.Fatalf("ticket %s was resent with no message id", tk.TicketID)
		}
	}
	// The original attempt row is untouched: a resend records itself beside the
	// first delivery, it does not overwrite the record of it.
	if n := countRows(t, ctx, db, `SELECT count(*) FROM delivery_attempts WHERE ticket_id=$1 AND message_id=$2`, sorted[0], original); n != 1 {
		t.Fatalf("the original delivery_attempts row was disturbed (%d rows match)", n)
	}
}

// COS-4. A draw of the trail: every accepted resend is its own event, and the chain
// still verifies. Two tickets, because a one-ticket fixture cannot tell "one event per
// delivery" from "one event per order".
func TestRedeliveryAppendsOneEventPerTicketAndVerifiesClean(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)
	org := uuid.New()
	order, sorted, _ := issueOrder(t, ctx, st, org, 2)
	deliverOriginal(t, ctx, st, sorted[0])

	claimAndAccept(t, ctx, st, org, order, "key-trail-1")

	for _, id := range sorted {
		if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type='redelivered'`, id); n != 1 {
			t.Fatalf("ticket %s has %d redelivered events after one resend, want 1", id, n)
		}
	}
	// The original `delivered` event is still there and still singular: a resend
	// adds to the trail, it does not rewrite what it says about the first delivery.
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type='delivered'`, sorted[0]); n != 1 {
		t.Fatalf("the original delivered event count is %d, want 1", n)
	}

	// A SECOND resend under a different key moves the trail again — this is the
	// repeatable half, and the singleton index would refuse it if `redelivered`
	// had been added to that predicate.
	claimAndAccept(t, ctx, st, org, order, "key-trail-2")
	for _, id := range sorted {
		if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type='redelivered'`, id); n != 2 {
			t.Fatalf("ticket %s has %d redelivered events after two resends, want 2 — a resend that cannot repeat makes the trail claim one delivery where there were two", id, n)
		}
	}
	if err := New(db, verifyOnlyConfig(t, cfg)).VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatalf("verify after redelivery: %v", err)
	}
}

// COS-7. One key resends once, however many times it is presented — the double-click.
func TestRedeliveryReplaysTheSameKeyWithoutSendingAgain(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	order, sorted, _ := issueOrder(t, ctx, st, org, 2)

	first := claimAndAccept(t, ctx, st, org, order, "key-double-click")
	if first.Replay {
		t.Fatal("the first claim reported itself as a replay")
	}

	second, err := st.ClaimRedelivery(ctx, org, order, "key-double-click")
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if !second.Replay {
		t.Fatal("the second claim under one key did not report a replay: the caller will send a second mail")
	}
	// A replay hands back the ids that were actually sent, so a caller resuming an
	// ambiguous request re-presents the same message id rather than a new one.
	if len(second.Tickets) != len(first.Tickets) {
		t.Fatalf("replay returned %d tickets, first claim returned %d", len(second.Tickets), len(first.Tickets))
	}
	for i := range first.Tickets {
		if second.Tickets[i].MessageID != first.Tickets[i].MessageID {
			t.Fatalf("replay minted a NEW message id for ticket %s", first.Tickets[i].TicketID)
		}
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM redelivery_attempts WHERE organizer_id=$1`, org); n != 2 {
		t.Fatalf("%d attempt rows after a double-click, want one per ticket (2)", n)
	}
	for _, id := range sorted {
		if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type='redelivered'`, id); n != 1 {
			t.Fatalf("ticket %s has %d redelivered events after a double-click, want 1", id, n)
		}
	}
}

// MarkRedelivered is idempotent on its own, independently of the claim: a caller that
// crashes after the transport accepted and retries must not append twice.
func TestMarkRedeliveredAppendsOnlyOncePerTicket(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)
	org := uuid.New()
	order, sorted, _ := issueOrder(t, ctx, st, org, 1)

	claim, err := st.ClaimRedelivery(ctx, org, order, "key-retry-accept")
	if err != nil {
		t.Fatal(err)
	}
	tk := claim.Tickets[0]
	for i := 0; i < 3; i++ {
		if err := st.MarkRedelivered(ctx, org, "key-retry-accept", tk.TicketID, tk.MessageID); err != nil {
			t.Fatalf("accept %d: %v", i, err)
		}
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type='redelivered'`, sorted[0]); n != 1 {
		t.Fatalf("%d redelivered events after three accepts of one send, want 1", n)
	}
	if err := New(db, verifyOnlyConfig(t, cfg)).VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatalf("verify after repeated accept: %v", err)
	}
}

// A key belongs to one order. Presented against another it is refused rather than
// resending a fresh batch — the binding the derived event id cannot express, and the
// reason redelivery_requests stores the order at all.
func TestRedeliveryRefusesAKeyReusedAgainstADifferentOrder(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	first, _, _ := issueOrder(t, ctx, st, org, 1)
	second, _, _ := issueOrder(t, ctx, st, org, 1)

	if _, err := st.ClaimRedelivery(ctx, org, first, "key-shared"); err != nil {
		t.Fatal(err)
	}
	_, err := st.ClaimRedelivery(ctx, org, second, "key-shared")
	if !errors.Is(err, ErrRedeliveryKeyConflict) {
		t.Fatalf("reusing one key against a second order returned %v, want ErrRedeliveryKeyConflict", err)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM redelivery_attempts WHERE organizer_id=$1`, org); n != 1 {
		t.Fatalf("the refused request left %d attempt rows, want the 1 the accepted request wrote", n)
	}
}

// COS-5. The bound is per ORDER and it is durable: it counts rows, so it survives a
// restart and holds across replicas. Replays are exempt — a caller retrying an
// ambiguous request must not be refused by a quota it already passed.
func TestRedeliveryBoundsDistinctRequestsPerOrder(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	order, _, _ := issueOrder(t, ctx, st, org, 1)

	keys := make([]string, 0, RedeliveryBound)
	for i := 0; i < RedeliveryBound; i++ {
		key := "key-bound-" + uuid.New().String()
		keys = append(keys, key)
		if _, err := st.ClaimRedelivery(ctx, org, order, key); err != nil {
			t.Fatalf("request %d of %d was refused: %v", i+1, RedeliveryBound, err)
		}
	}
	_, err := st.ClaimRedelivery(ctx, org, order, "key-bound-over")
	if !errors.Is(err, ErrRedeliveryBoundExceeded) {
		t.Fatalf("request %d returned %v, want ErrRedeliveryBoundExceeded", RedeliveryBound+1, err)
	}

	// A replay of an already-claimed key still answers, even though the order is at
	// its bound: an operator retrying an ambiguous request must not be locked out by
	// a quota that request already passed.
	replay, err := st.ClaimRedelivery(ctx, org, order, keys[0])
	if err != nil {
		t.Fatalf("replaying a key claimed before the bound was reached: %v", err)
	}
	if !replay.Replay {
		t.Fatal("the replay was treated as a new request")
	}

	// A DIFFERENT order is unaffected: the bound is the order's, not the caller's.
	other, _, _ := issueOrder(t, ctx, st, org, 1)
	if _, err := st.ClaimRedelivery(ctx, org, other, "key-other-order"); err != nil {
		t.Fatalf("a second order was refused by the first order's bound: %v", err)
	}
}

// The window is what makes the bound "rolling" rather than "ever". Asserted by moving
// the store's clock rather than the rows: the rows are what production writes, and a
// test that back-dated them would be asserting against its own fixture.
func TestRedeliveryBoundIsAWindowNotALifetimeCap(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	base := time.Now().UTC()
	cfg := testConfig(t)
	cfg.Now = func() time.Time { return base }
	st := New(db, cfg)
	org := uuid.New()
	order, _, _ := issueOrder(t, ctx, st, org, 1)

	for i := 0; i < RedeliveryBound; i++ {
		if _, err := st.ClaimRedelivery(ctx, org, order, "key-window-"+uuid.New().String()); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}
	if _, err := st.ClaimRedelivery(ctx, org, order, "key-window-over"); !errors.Is(err, ErrRedeliveryBoundExceeded) {
		t.Fatalf("at the bound, got %v, want ErrRedeliveryBoundExceeded", err)
	}

	// Past the window, the same order may resend again.
	cfg.Now = func() time.Time { return base.Add(RedeliveryWindow + time.Minute) }
	later := New(db, cfg)
	if _, err := later.ClaimRedelivery(ctx, org, order, "key-window-after"); err != nil {
		t.Fatalf("after the window elapsed the order was still refused: %v", err)
	}
}

// An order with no issued tickets is "not yet", never "no such order": issuance is
// asynchronous and a resend can outrun it, exactly as a refund can.
func TestRedeliveryOfAnOrderWithNoTicketsIsNotYet(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))

	_, err := st.ClaimRedelivery(ctx, uuid.New(), uuid.New(), "key-no-tickets")
	if !errors.Is(err, ErrTicketsNotIssued) {
		t.Fatalf("an order with no tickets returned %v, want ErrTicketsNotIssued", err)
	}
}

// The organizer scopes the read AND the write. A resend claimed for one organizer must
// not reach another's tickets, and the failure path is scoped too: MarkRedelivered
// refuses a ticket belonging to someone else rather than appending to its trail.
func TestRedeliveryIsScopedToTheOrganizer(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	victim, attacker := uuid.New(), uuid.New()
	order, sorted, _ := issueOrder(t, ctx, st, victim, 1)

	if _, err := st.ClaimRedelivery(ctx, attacker, order, "key-cross-tenant"); !errors.Is(err, ErrTicketsNotIssued) {
		t.Fatalf("another organizer's order answered %v, want ErrTicketsNotIssued (it must look like an order with nothing to resend)", err)
	}

	// And the accept path, separately: holding a claim for one's own order must not
	// let a caller mark a ticket that is not theirs.
	own, _, _ := issueOrder(t, ctx, st, attacker, 1)
	claim, err := st.ClaimRedelivery(ctx, attacker, own, "key-own")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRedelivered(ctx, attacker, "key-own", sorted[0], claim.Tickets[0].MessageID); !errors.Is(err, ErrTicketCredential) {
		t.Fatalf("marking another organizer's ticket returned %v, want ErrTicketCredential", err)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type='redelivered'`, sorted[0]); n != 0 {
		t.Fatalf("the victim's ticket gained %d redelivered events from another organizer's request", n)
	}
}
