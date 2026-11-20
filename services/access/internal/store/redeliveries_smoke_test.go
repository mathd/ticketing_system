//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
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
		t.Errorf("another organizer's order answered %v, want ErrTicketsNotIssued (it must look like an order with nothing to resend)", err)
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

	// TKT-526. The failure paths stay scoped once the victim's order is void. The
	// eligibility refusal would disclose the victim's refund, so the attacker's claim
	// still reads as an order with no tickets, and the organizer check refuses the mark
	// before the void check can answer success.
	if _, err := st.RefundOrderTickets(ctx, victim, order, uuid.New(), 1); err != nil {
		t.Fatal(err)
	}
	before := readRedeliveryState(t, ctx, db)
	requireStepTypes(t, sorted[0], before.Histories[sorted[0]], "issued", "refunded")
	if _, err := st.ClaimRedelivery(ctx, attacker, order, "key-cross-tenant-refunded"); !errors.Is(err, ErrTicketsNotIssued) || errors.Is(err, ErrRedeliveryNoEligibleTickets) {
		t.Errorf("another organizer's refunded order answered %v, want ErrTicketsNotIssued and not ErrRedeliveryNoEligibleTickets", err)
	}
	if err := st.MarkRedelivered(ctx, attacker, "key-own", sorted[0], claim.Tickets[0].MessageID); !errors.Is(err, ErrTicketCredential) {
		t.Errorf("marking another organizer's refunded ticket returned %v, want ErrTicketCredential", err)
	}
	requireRedeliveryState(t, ctx, db, before)
}

// ai-review F2, at the tier the mechanism lives at.
//
// A claim COMMITS before the caller sends anything, so a request that dies partway
// leaves attempt rows with nothing sent against them. The replay must report those as
// OUTSTANDING — if it reported them accepted, the caller would return 200 for an order
// whose tickets never left, permanently.
//
// This is a store-tier test because the mechanism is a SQL read of accepted_at. The
// API-tier tests assert the handler resends what it is told is outstanding; only this
// one proves the store tells it the truth. Asserting it against a Go fake would prove
// the fake and the handler agree, and nothing else.
func TestAReplayReportsUnacceptedAttemptsAsOutstanding(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	order, sorted, _ := issueOrder(t, ctx, st, org, 2)
	const key = "key-partial-send"

	first, err := st.ClaimRedelivery(ctx, org, order, key)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(first.Outstanding()); n != 2 {
		t.Fatalf("a fresh claim reports %d outstanding, want both tickets (2): nothing has "+
			"been sent yet", n)
	}

	// The caller sends ONE ticket and dies — the partial state F2 describes.
	var sent RedeliveryTicket
	for _, tk := range first.Tickets {
		if tk.TicketID == sorted[0] {
			sent = tk
		}
	}
	if err := st.MarkRedelivered(ctx, org, key, sent.TicketID, sent.MessageID); err != nil {
		t.Fatal(err)
	}

	replay, err := st.ClaimRedelivery(ctx, org, order, key)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replay {
		t.Fatal("the retry was not recognised as the same request")
	}
	outstanding := replay.Outstanding()
	if len(outstanding) != 1 {
		t.Fatalf("the replay reports %d tickets outstanding, want exactly the 1 that was "+
			"never sent — reporting 0 tells the caller a half-sent order is complete, and "+
			"the unsent ticket never leaves", len(outstanding))
	}
	if outstanding[0].TicketID == sent.TicketID {
		t.Fatal("the replay reports the ALREADY-SENT ticket as outstanding and the unsent " +
			"one as done: the accepted_at read is inverted")
	}
	// And the resume reuses the original message id, which is what lets a deduplicating
	// transport avoid a second delivery.
	for _, tk := range first.Tickets {
		for _, r := range replay.Tickets {
			if r.TicketID == tk.TicketID && r.MessageID != tk.MessageID {
				t.Fatalf("ticket %s was re-minted a new message id on replay", tk.TicketID)
			}
		}
	}

	// Finishing the job clears it: nothing outstanding once both are accepted.
	if err := st.MarkRedelivered(ctx, org, key, outstanding[0].TicketID, outstanding[0].MessageID); err != nil {
		t.Fatal(err)
	}
	done, err := st.ClaimRedelivery(ctx, org, order, key)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(done.Outstanding()); n != 0 {
		t.Fatalf("a fully accepted request still reports %d outstanding", n)
	}
}

// TKT-526. A staff resend sends only tickets that are still commercially valid. A
// refund or an exchange ends a ticket's life, and a resend of a dead ticket re-emits a
// capability the buyer must not use (owner decision (a), 2026-10-12).
//
// The helpers below read the complete resend relations and every ticket history, so a
// test compares the whole state and not only the row it expects to change.

// redeliveryRequestRow is one redelivery_requests row without its timestamp.
type redeliveryRequestRow struct {
	OrganizerID uuid.UUID
	Key         string
	OrderID     uuid.UUID
	Count       int
}

// redeliveryAttemptRow is one complete redelivery_attempts row.
type redeliveryAttemptRow struct {
	OrganizerID uuid.UUID
	Key         string
	TicketID    uuid.UUID
	MessageID   uuid.UUID
	AcceptedAt  sql.NullTime
}

// redeliveryState is every resend row and every ticket history in the test's schema.
type redeliveryState struct {
	Histories map[uuid.UUID][]lifecycleStep
	Requests  []redeliveryRequestRow
	Attempts  []redeliveryAttemptRow
}

func readRedeliveryState(t *testing.T, ctx context.Context, db *sql.DB) redeliveryState {
	t.Helper()
	state := redeliveryState{
		Histories: allHistories(t, ctx, db),
		Requests:  []redeliveryRequestRow{},
		Attempts:  []redeliveryAttemptRow{},
	}
	rows, err := db.QueryContext(ctx, `
		SELECT organizer_id, idempotency_key, order_id, ticket_count FROM redelivery_requests
		ORDER BY organizer_id, idempotency_key`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r redeliveryRequestRow
		if err := rows.Scan(&r.OrganizerID, &r.Key, &r.OrderID, &r.Count); err != nil {
			t.Fatal(err)
		}
		state.Requests = append(state.Requests, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	rows, err = db.QueryContext(ctx, `
		SELECT organizer_id, idempotency_key, ticket_id, message_id, accepted_at FROM redelivery_attempts
		ORDER BY organizer_id, idempotency_key, ticket_id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a redeliveryAttemptRow
		if err := rows.Scan(&a.OrganizerID, &a.Key, &a.TicketID, &a.MessageID, &a.AcceptedAt); err != nil {
			t.Fatal(err)
		}
		state.Attempts = append(state.Attempts, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	return state
}

// requireRedeliveryState compares every part of the state and reports each difference
// with Errorf, so a test that already saw a wrong result still reports what was written.
func requireRedeliveryState(t *testing.T, ctx context.Context, db *sql.DB, want redeliveryState) {
	t.Helper()
	got := readRedeliveryState(t, ctx, db)
	if !reflect.DeepEqual(got.Histories, want.Histories) {
		t.Errorf("ticket histories changed:\n got  %v\n want %v", got.Histories, want.Histories)
	}
	if !reflect.DeepEqual(got.Requests, want.Requests) {
		t.Errorf("redelivery requests:\n got  %+v\n want %+v", got.Requests, want.Requests)
	}
	if !reflect.DeepEqual(got.Attempts, want.Attempts) {
		t.Errorf("redelivery attempts:\n got  %+v\n want %+v", got.Attempts, want.Attempts)
	}
}

// cloneHistories copies a history map so that a test can add the events it expects.
func cloneHistories(in map[uuid.UUID][]lifecycleStep) map[uuid.UUID][]lifecycleStep {
	out := make(map[uuid.UUID][]lifecycleStep, len(in))
	for id, h := range in {
		out[id] = append([]lifecycleStep{}, h...)
	}
	return out
}

// claimedTicket is what a claim tells the caller about one ticket.
type claimedTicket struct {
	TicketID, MessageID uuid.UUID
	Accepted            bool
}

func claimedTickets(c RedeliveryClaim) []claimedTicket {
	out := []claimedTicket{}
	for _, tk := range c.Tickets {
		out = append(out, claimedTicket{tk.TicketID, tk.MessageID, tk.Accepted})
	}
	return out
}

func outstandingIDs(c RedeliveryClaim) []uuid.UUID {
	out := []uuid.UUID{}
	for _, tk := range c.Outstanding() {
		out = append(out, tk.TicketID)
	}
	return out
}

// partialVoid is one commercial void fact applied to named tickets of a live order.
//
// The refund goes through the real writer, which voids the lowest unrefunded ids, so a
// caller passes the lowest ids. The current exchange writer switches a whole order
// (ADR-039), so a mixed exchange is a signed `exchanged` append on the named tickets:
// the store can represent it, and the predicate reads it like any other.
type partialVoid struct {
	name  string
	event string
	void  func(t *testing.T, ctx context.Context, st *Postgres, org, order uuid.UUID, ids []uuid.UUID)
}

func partialVoids() []partialVoid {
	return []partialVoid{
		{name: "refund", event: "refunded", void: func(t *testing.T, ctx context.Context, st *Postgres, org, order uuid.UUID, ids []uuid.UUID) {
			t.Helper()
			batch, err := st.RefundOrderTickets(ctx, org, order, uuid.New(), int32(len(ids)))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(batch.TicketIDs, ids) {
				t.Fatalf("seeded state is wrong: the refund voided %v, want %v", batch.TicketIDs, ids)
			}
		}},
		{name: "exchange", event: "exchanged", void: func(t *testing.T, ctx context.Context, st *Postgres, org, order uuid.UUID, ids []uuid.UUID) {
			t.Helper()
			for _, id := range ids {
				if _, err := st.appendLifecycleForTest(ctx, id, order, org, "exchanged"); err != nil {
					t.Fatal(err)
				}
			}
		}},
	}
}

// wholeOrderVoid voids every ticket of an order with the real writer of each kind: a
// full refund, or the whole-order exchange switch with replacements in a new order.
type wholeOrderVoid struct {
	name  string
	event string
	void  func(t *testing.T, ctx context.Context, st *Postgres, org, order uuid.UUID, n int)
}

func wholeOrderVoids() []wholeOrderVoid {
	return []wholeOrderVoid{
		{name: "refund", event: "refunded", void: func(t *testing.T, ctx context.Context, st *Postgres, org, order uuid.UUID, n int) {
			t.Helper()
			if _, err := st.RefundOrderTickets(ctx, org, order, uuid.New(), int32(n)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "exchange", event: "exchanged", void: func(t *testing.T, ctx context.Context, st *Postgres, org, order uuid.UUID, n int) {
			t.Helper()
			if err := st.SwitchExchange(ctx, SwitchExchangeInput{
				EventID: uuid.New(), ExchangeID: uuid.New(), SourceOrderID: order, OrganizerID: org,
				Tickets: replacementTickets(uuid.New(), org, uuid.New(), n),
			}); err != nil {
				t.Fatal(err)
			}
		}},
	}
}

// COS-1. A fresh claim on a partly voided order returns exactly the valid tickets,
// stores their count, and mints one attempt per returned ticket. The lower id is the
// voided one, so a claim that keeps the order's first ticket cannot pass by accident.
func TestClaimRedeliverySkipsVoidedTickets(t *testing.T) {
	for _, kind := range partialVoids() {
		t.Run(kind.name, func(t *testing.T) {
			ctx := context.Background()
			db := migratedDB(t, ctx)
			st := New(db, testConfig(t))
			org := uuid.New()
			order, sorted, _ := issueOrder(t, ctx, st, org, 2)
			kind.void(t, ctx, st, org, order, sorted[:1])

			before := readRedeliveryState(t, ctx, db)
			requireStepTypes(t, sorted[0], before.Histories[sorted[0]], "issued", kind.event)
			requireStepTypes(t, sorted[1], before.Histories[sorted[1]], "issued")
			if len(before.Histories) != 2 || len(before.Requests) != 0 || len(before.Attempts) != 0 {
				t.Fatalf("seeded state is wrong: %+v", before)
			}

			const key = "key-skip-voided"
			msg := redeliveryMessageID(org, key, sorted[1])
			claim, err := st.ClaimRedelivery(ctx, org, order, key)
			if err != nil {
				t.Errorf("claim: %v — the order still has one valid ticket", err)
			} else {
				want := []claimedTicket{{TicketID: sorted[1], MessageID: msg}}
				if got := claimedTickets(claim); claim.Replay || claim.OrderID != order || !reflect.DeepEqual(got, want) {
					t.Errorf("claim = order %s replay %t tickets %+v, want order %s, a fresh claim, and only the valid ticket %+v",
						claim.OrderID, claim.Replay, got, order, want)
				}
			}
			requireRedeliveryState(t, ctx, db, redeliveryState{
				Histories: before.Histories,
				Requests:  []redeliveryRequestRow{{OrganizerID: org, Key: key, OrderID: order, Count: 1}},
				Attempts:  []redeliveryAttemptRow{{OrganizerID: org, Key: key, TicketID: sorted[1], MessageID: msg}},
			})
		})
	}
}

// COS-3. A replay rechecks eligibility. Four tickets cover every combination of
// accepted or pending and voided or valid. Only the valid attempts come back, with their
// original message ids and acceptance, and the stored request keeps its first count.
func TestClaimRedeliveryReplaySkipsVoidedAttempts(t *testing.T) {
	for _, kind := range partialVoids() {
		t.Run(kind.name, func(t *testing.T) {
			ctx := context.Background()
			db := migratedDB(t, ctx)
			st := New(db, testConfig(t))
			org := uuid.New()
			order, sorted, _ := issueOrder(t, ctx, st, org, 4)
			const key = "key-replay-voided"

			first, err := st.ClaimRedelivery(ctx, org, order, key)
			if err != nil {
				t.Fatal(err)
			}
			if len(first.Tickets) != 4 {
				t.Fatalf("seeded state is wrong: the live claim returned %d tickets, want 4", len(first.Tickets))
			}
			msgs := map[uuid.UUID]uuid.UUID{}
			for _, tk := range first.Tickets {
				msgs[tk.TicketID] = tk.MessageID
			}
			// sorted[0] will be voided after acceptance, sorted[2] stays valid after it.
			for _, id := range []uuid.UUID{sorted[0], sorted[2]} {
				if err := st.MarkRedelivered(ctx, org, key, id, msgs[id]); err != nil {
					t.Fatal(err)
				}
			}
			kind.void(t, ctx, st, org, order, sorted[:2])

			before := readRedeliveryState(t, ctx, db)
			requireStepTypes(t, sorted[0], before.Histories[sorted[0]], "issued", "redelivered", kind.event)
			requireStepTypes(t, sorted[1], before.Histories[sorted[1]], "issued", kind.event)
			requireStepTypes(t, sorted[2], before.Histories[sorted[2]], "issued", "redelivered")
			requireStepTypes(t, sorted[3], before.Histories[sorted[3]], "issued")
			if !reflect.DeepEqual(before.Requests, []redeliveryRequestRow{{OrganizerID: org, Key: key, OrderID: order, Count: 4}}) {
				t.Fatalf("seeded state is wrong: requests %+v", before.Requests)
			}
			accepted := map[uuid.UUID]bool{}
			for _, a := range before.Attempts {
				accepted[a.TicketID] = a.AcceptedAt.Valid
			}
			if len(before.Attempts) != 4 || !accepted[sorted[0]] || accepted[sorted[1]] || !accepted[sorted[2]] || accepted[sorted[3]] {
				t.Fatalf("seeded state is wrong: attempts %+v", before.Attempts)
			}

			replay, err := st.ClaimRedelivery(ctx, org, order, key)
			if err != nil {
				t.Errorf("replay: %v — two valid attempts remain under this key", err)
			} else {
				want := []claimedTicket{
					{TicketID: sorted[2], MessageID: msgs[sorted[2]], Accepted: true},
					{TicketID: sorted[3], MessageID: msgs[sorted[3]]},
				}
				if got := claimedTickets(replay); !replay.Replay || !reflect.DeepEqual(got, want) {
					t.Errorf("replay = replay %t tickets %+v, want a replay of only the valid attempts %+v", replay.Replay, got, want)
				}
				if got := outstandingIDs(replay); !reflect.DeepEqual(got, []uuid.UUID{sorted[3]}) {
					t.Errorf("outstanding = %v, want only the valid pending ticket %s", got, sorted[3])
				}
			}
			requireRedeliveryState(t, ctx, db, before)
		})
	}
}

// COS-2. An order whose issued tickets are all void is refused before key lookup, quota
// and insertion, and nothing is written. The voids come from the real writers. The
// second fixture reaches the quota with live claims first, so the refusal must come
// before the quota check for a new key, and before the replay for a bound key.
func TestClaimRedeliveryRefusesAllVoidedTickets(t *testing.T) {
	for _, kind := range wholeOrderVoids() {
		t.Run(kind.name+"/below quota", func(t *testing.T) {
			ctx := context.Background()
			db := migratedDB(t, ctx)
			st := New(db, testConfig(t))
			org := uuid.New()
			order, sorted, _ := issueOrder(t, ctx, st, org, 2)
			kind.void(t, ctx, st, org, order, 2)

			before := readRedeliveryState(t, ctx, db)
			for _, id := range sorted {
				requireStepTypes(t, id, before.Histories[id], "issued", kind.event)
			}
			if len(before.Requests) != 0 || len(before.Attempts) != 0 {
				t.Fatalf("seeded state is wrong: %+v", before)
			}

			_, err := st.ClaimRedelivery(ctx, org, order, "key-all-voided")
			if !errors.Is(err, ErrRedeliveryNoEligibleTickets) || errors.Is(err, ErrTicketsNotIssued) {
				t.Errorf("err = %v, want ErrRedeliveryNoEligibleTickets and not ErrTicketsNotIssued", err)
			}
			requireRedeliveryState(t, ctx, db, before)
		})
		t.Run(kind.name+"/at quota", func(t *testing.T) {
			ctx := context.Background()
			db := migratedDB(t, ctx)
			st := New(db, testConfig(t))
			org := uuid.New()
			order, sorted, _ := issueOrder(t, ctx, st, org, 2)
			keys := make([]string, 0, RedeliveryBound)
			for i := 0; i < RedeliveryBound; i++ {
				key := "key-live-" + uuid.NewString()
				keys = append(keys, key)
				if _, err := st.ClaimRedelivery(ctx, org, order, key); err != nil {
					t.Fatalf("live claim %d: %v", i+1, err)
				}
			}
			kind.void(t, ctx, st, org, order, 2)

			before := readRedeliveryState(t, ctx, db)
			for _, id := range sorted {
				requireStepTypes(t, id, before.Histories[id], "issued", kind.event)
			}
			if len(before.Requests) != RedeliveryBound || len(before.Attempts) != 2*RedeliveryBound {
				t.Fatalf("seeded state is wrong: %d requests, %d attempts", len(before.Requests), len(before.Attempts))
			}

			for _, key := range []string{"key-fresh-at-quota", keys[0]} {
				_, err := st.ClaimRedelivery(ctx, org, order, key)
				if !errors.Is(err, ErrRedeliveryNoEligibleTickets) {
					t.Errorf("key %q: err = %v, want ErrRedeliveryNoEligibleTickets", key, err)
				}
			}
			requireRedeliveryState(t, ctx, db, before)
		})
	}
}

// COS-2, the guard order. Key K is bound to eligible order B. A claim for the fully
// refunded order A under K must report that A has nothing to send, not that K is bound
// elsewhere: the refusal comes before the key lookup.
func TestClaimRedeliveryRefusesAllVoidedBeforeKeyLookup(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	const key = "key-bound-to-b"
	orderB, _, _ := issueOrder(t, ctx, st, org, 1)
	if _, err := st.ClaimRedelivery(ctx, org, orderB, key); err != nil {
		t.Fatal(err)
	}
	orderA, sortedA, _ := issueOrder(t, ctx, st, org, 2)
	if _, err := st.RefundOrderTickets(ctx, org, orderA, uuid.New(), 2); err != nil {
		t.Fatal(err)
	}

	before := readRedeliveryState(t, ctx, db)
	for _, id := range sortedA {
		requireStepTypes(t, id, before.Histories[id], "issued", "refunded")
	}
	if len(before.Histories) != 3 || len(before.Requests) != 1 || before.Requests[0].OrderID != orderB || len(before.Attempts) != 1 {
		t.Fatalf("seeded state is wrong: %+v", before)
	}

	_, err := st.ClaimRedelivery(ctx, org, orderA, key)
	if !errors.Is(err, ErrRedeliveryNoEligibleTickets) || errors.Is(err, ErrRedeliveryKeyConflict) {
		t.Errorf("err = %v, want ErrRedeliveryNoEligibleTickets and not ErrRedeliveryKeyConflict", err)
	}
	requireRedeliveryState(t, ctx, db, before)
}

// COS-3, the empty intersection. The key's only attempt is on a ticket that is now
// refunded, and the order has since gained a valid ticket outside the key. The store
// can represent that growth; current Commerce issuance cannot produce it. A replay must
// not mint an attempt for the new ticket, and it must not report an empty success.
func TestClaimRedeliveryRefusesEmptyReplayWithOtherEligibleTickets(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	order, sorted, seeds := issueOrder(t, ctx, st, org, 1)
	const key = "key-empty-replay"
	if _, err := st.ClaimRedelivery(ctx, org, order, key); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RefundOrderTickets(ctx, org, order, uuid.New(), 1); err != nil {
		t.Fatal(err)
	}
	added := uuid.New()
	if err := st.Issue(ctx, IssueInput{EventID: uuid.New(), Tickets: []Ticket{{
		ID: added, OrderID: order, GuestOrderRef: uuid.New(), OrganizerID: org,
		BuyerID: uuid.New(), SlotID: seeds[0].id.SlotID, TicketTypeID: uuid.New(),
		Payload: "signed-credential", IssuedAt: time.Now().UTC(),
	}}}); err != nil {
		t.Fatal(err)
	}

	before := readRedeliveryState(t, ctx, db)
	requireStepTypes(t, sorted[0], before.Histories[sorted[0]], "issued", "refunded")
	requireStepTypes(t, added, before.Histories[added], "issued")
	if len(before.Requests) != 1 || before.Requests[0].Count != 1 || len(before.Attempts) != 1 || before.Attempts[0].TicketID != sorted[0] {
		t.Fatalf("seeded state is wrong: %+v", before)
	}

	claim, err := st.ClaimRedelivery(ctx, org, order, key)
	if !errors.Is(err, ErrRedeliveryNoEligibleTickets) {
		t.Errorf("err = %v (claim %+v), want ErrRedeliveryNoEligibleTickets", err, claimedTickets(claim))
	}
	requireRedeliveryState(t, ctx, db, before)
}

// holdUncommittedVoid locks a ticket on a dedicated connection and appends a signed void
// event on that transaction without committing it. Both real void writers lock the same
// ticket row and append the same way; this fixture lets the test decide when the void
// commits.
func holdUncommittedVoid(t *testing.T, ctx context.Context, st *Postgres, db *sql.DB, ticketID uuid.UUID, eventType string) (*sql.Tx, int, uuid.UUID) {
	t.Helper()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	var pid int
	if err := conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	var id TicketIdentity
	if err := tx.QueryRowContext(ctx, `SELECT order_id,organizer_id,slot_id FROM tickets WHERE id=$1 FOR UPDATE`, ticketID).
		Scan(&id.OrderID, &id.OrganizerID, &id.SlotID); err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	if _, err := st.appendLifecycle(ctx, tx, appendInput{
		TicketID: ticketID, OrderID: id.OrderID, OrganizerID: id.OrganizerID, SlotID: id.SlotID,
		EventID: eventID, Type: eventType,
	}); err != nil {
		t.Fatal(err)
	}
	return tx, pid, eventID
}

// waitForTicketLockWait returns once a backend blocked by blockerPID waits on the
// ticket-lock statement. It fails if the call under test returns first, or waits on
// anything else until the deadline.
func waitForTicketLockWait(t *testing.T, ctx context.Context, db *sql.DB, blockerPID int, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("MarkRedelivered returned %v before it waited on the ticket lock: the lock window was not exercised", err)
		default:
		}
		var waiting int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE $1 = ANY(pg_blocking_pids(pid)) AND wait_event_type='Lock' AND query LIKE $2`,
			blockerPID, "%FROM tickets WHERE id=$1 FOR UPDATE%").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("MarkRedelivered never waited on the ticket lock held by the void")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// COS-4. A void that commits while mark waits for the ticket lock wins. Mark returns
// success as a no-op: the attempt stays pending and the trail gains only the void. A
// read before the lock would see the ticket as valid and record a resend of a dead
// ticket.
func TestMarkRedeliveredSkipsTicketsVoidedWhileWaiting(t *testing.T) {
	for _, kind := range partialVoids() {
		t.Run(kind.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			db := migratedDB(t, ctx)
			st := New(db, testConfig(t))
			org := uuid.New()
			order, sorted, _ := issueOrder(t, ctx, st, org, 1)
			const key = "key-void-while-waiting"
			claim, err := st.ClaimRedelivery(ctx, org, order, key)
			if err != nil {
				t.Fatal(err)
			}
			before := readRedeliveryState(t, ctx, db)
			requireStepTypes(t, sorted[0], before.Histories[sorted[0]], "issued")
			msg := redeliveryMessageID(org, key, sorted[0])
			if !reflect.DeepEqual(claimedTickets(claim), []claimedTicket{{TicketID: sorted[0], MessageID: msg}}) ||
				!reflect.DeepEqual(before.Attempts, []redeliveryAttemptRow{{OrganizerID: org, Key: key, TicketID: sorted[0], MessageID: msg}}) {
				t.Fatalf("seeded state is wrong: claim %+v, attempts %+v", claimedTickets(claim), before.Attempts)
			}

			voidTx, blockerPID, voidEvent := holdUncommittedVoid(t, ctx, st, db, sorted[0], kind.event)
			done := make(chan error, 1)
			go func() { done <- st.MarkRedelivered(ctx, org, key, sorted[0], msg) }()
			waitForTicketLockWait(t, ctx, db, blockerPID, done)
			if err := voidTx.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("mark after the void won the lock: %v, want a successful no-op", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("MarkRedelivered did not return after the void committed")
			}

			want := redeliveryState{Histories: cloneHistories(before.Histories), Requests: before.Requests, Attempts: before.Attempts}
			want.Histories[sorted[0]] = append(want.Histories[sorted[0]], lifecycleStep{ID: voidEvent, Type: kind.event})
			requireRedeliveryState(t, ctx, db, want)
			if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_event_integrity WHERE ticket_id=$1`, sorted[0]); n != 2 {
				t.Errorf("ticket %s has %d integrity rows, want 2 (issued and the void)", sorted[0], n)
			}
			if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_event_integrity WHERE event_id=$1`, voidEvent); n != 1 {
				t.Errorf("the void event has %d integrity rows, want 1", n)
			}
			if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_event_integrity WHERE event_id=$1`, redeliveryEventID(org, key, sorted[0])); n != 0 {
				t.Errorf("the resend event has %d integrity rows, want 0", n)
			}
		})
	}
}
