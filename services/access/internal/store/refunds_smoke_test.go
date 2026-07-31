//go:build smoke

package store

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Ticket voiding on refund (TKT-157). Two properties carry the whole ticket:
// WHICH tickets get voided must be deterministic and stable across a replay, and a
// voided ticket must be refused at the gate BEFORE any path that can fail open.

// issueOrder issues n tickets against one order and returns them sorted by id, which
// is the selection order the implementation must use.
func issueOrder(t *testing.T, ctx context.Context, st *Postgres, org uuid.UUID, n int) (uuid.UUID, []uuid.UUID, []seeded) {
	t.Helper()
	order, slot, guestRef := uuid.New(), uuid.New(), uuid.New()
	tickets := make([]Ticket, 0, n)
	seeds := make([]seeded, 0, n)
	for i := 0; i < n; i++ {
		id := uuid.New()
		tickets = append(tickets, Ticket{
			ID: id, OrderID: order, GuestOrderRef: guestRef, OrganizerID: org,
			BuyerID: uuid.New(), SlotID: slot, TicketTypeID: uuid.New(),
			Payload: "signed-credential", IssuedAt: time.Now().UTC(),
		})
		seeds = append(seeds, seeded{ticketID: id, id: TicketIdentity{OrderID: order, OrganizerID: org, SlotID: slot}})
	}
	if err := st.Issue(ctx, IssueInput{EventID: uuid.New(), Tickets: tickets}); err != nil {
		t.Fatal(err)
	}
	ids := make([]uuid.UUID, 0, n)
	for _, tk := range tickets {
		ids = append(ids, tk.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return order, ids, seeds
}

// AC5: the lowest ticket UUIDs ascending, and the SECOND refund takes the next ones —
// not the same ones again.
func TestRefundOrderTicketsSelectsLowestUnrefundedIDs(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	order, sorted, _ := issueOrder(t, ctx, st, org, 5)

	first, err := st.RefundOrderTickets(ctx, org, order, uuid.New(), 2)
	if err != nil {
		t.Fatalf("first refund: %v", err)
	}
	if len(first.TicketIDs) != 2 || first.TicketIDs[0] != sorted[0] || first.TicketIDs[1] != sorted[1] {
		t.Fatalf("selected %v, want the two lowest %v", first.TicketIDs, sorted[:2])
	}
	second, err := st.RefundOrderTickets(ctx, org, order, uuid.New(), 2)
	if err != nil {
		t.Fatalf("second refund: %v", err)
	}
	if second.TicketIDs[0] != sorted[2] || second.TicketIDs[1] != sorted[3] {
		t.Fatalf("second refund selected %v, want the NEXT two %v", second.TicketIDs, sorted[2:4])
	}
}

// AC5's other half: a replay must void the SAME tickets. Recomputing "the first q
// unrefunded" on replay would pick the next q and void the whole order.
func TestRefundOrderTicketsReplayReturnsTheSameTickets(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	order, sorted, _ := issueOrder(t, ctx, st, org, 4)
	refundID := uuid.New()

	first, err := st.RefundOrderTickets(ctx, org, order, refundID, 2)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := st.RefundOrderTickets(ctx, org, order, refundID, 2)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Replay {
		t.Fatal("second call must report itself as a replay")
	}
	if len(replay.TicketIDs) != 2 || replay.TicketIDs[0] != first.TicketIDs[0] || replay.TicketIDs[1] != first.TicketIDs[1] {
		t.Fatalf("replay selected %v, want the original %v", replay.TicketIDs, first.TicketIDs)
	}
	// And no second lifecycle event: appendLifecycle must not be reached at all.
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE event_type='refunded'`); n != 2 {
		t.Fatalf("refunded events = %d, want 2 (the replay appended nothing)", n)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_event_integrity i JOIN lifecycle_events e ON e.id=i.event_id WHERE e.event_type='refunded'`); n != 2 {
		t.Fatalf("integrity rows for refunded events = %d, want 2", n)
	}
	_ = sorted
}

// AC4: the events go through appendLifecycle, so the chain still verifies with
// coverage required — the assertion that would fail on a direct INSERT.
func TestRefundedEventsKeepTheLifecycleChainVerifiable(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)
	org := uuid.New()
	order, _, _ := issueOrder(t, ctx, st, org, 3)

	if _, err := st.RefundOrderTickets(ctx, org, order, uuid.New(), 2); err != nil {
		t.Fatal(err)
	}
	verifier := New(db, verifyOnlyConfig(t, cfg))
	if err := verifier.VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatalf("chain must verify after voiding tickets: %v", err)
	}
}

// AC6: a refunded ticket is refused at the gate, and nothing is appended.
func TestScanOfRefundedTicketIsRefused(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	order, sorted, seeds := issueOrder(t, ctx, st, org, 2)

	if _, err := st.RefundOrderTickets(ctx, org, order, uuid.New(), 1); err != nil {
		t.Fatal(err)
	}
	var refunded, live seeded
	for _, s := range seeds {
		if s.ticketID == sorted[0] {
			refunded = s
		} else {
			live = s
		}
	}

	result, err := st.Redeem(ctx, refunded.redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted || result.Decision != DecisionRefunded {
		t.Fatalf("refunded ticket: accepted=%t decision=%q, want refused as refunded", result.Accepted, result.Decision)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type='redeemed'`, refunded.ticketID); n != 0 {
		t.Fatalf("a refused scan appended %d redeemed events", n)
	}

	// The ticket that was NOT refunded still works — the refusal is per ticket, not
	// per order.
	live_result, err := st.Redeem(ctx, live.redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if !live_result.Accepted {
		t.Fatalf("the unrefunded ticket must still admit: %+v", live_result)
	}
}

// AC6's ordering, and the reason it is stated as an ordering: a corrupt chain takes
// the degraded fail-open posture (ADR-021 §D6) and admits once. If the refund check
// ran after it, a refunded ticket would get in exactly once — which is the failure
// this AC exists to prevent.
func TestRefundedRefusalPrecedesIntegrityFailOpen(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	order, sorted, seeds := issueOrder(t, ctx, st, org, 1)

	if _, err := st.RefundOrderTickets(ctx, org, order, uuid.New(), 1); err != nil {
		t.Fatal(err)
	}
	corruptChain(t, ctx, db, sorted[0])

	result, err := st.Redeem(ctx, seeds[0].redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted || result.Decision != DecisionRefunded {
		t.Fatalf("a refunded ticket with a corrupt chain: accepted=%t decision=%q — must be refused as refunded, never admitted_degraded",
			result.Accepted, result.Decision)
	}
}

// A refund cannot void more tickets than the order has left. Issuance is
// asynchronous (outbox → JetStream), so a refund can genuinely arrive first, and
// answering "voided zero" would silently drop the obligation.
func TestRefundOrderTicketsRefusesWhenTooFewTicketsExist(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	order, _, _ := issueOrder(t, ctx, st, org, 2)

	if _, err := st.RefundOrderTickets(ctx, org, order, uuid.New(), 3); !errors.Is(err, ErrTicketsNotIssued) {
		t.Fatalf("err = %v, want ErrTicketsNotIssued", err)
	}
	// And an order with no tickets at all is the same answer, not a silent success.
	if _, err := st.RefundOrderTickets(ctx, org, uuid.New(), uuid.New(), 1); !errors.Is(err, ErrTicketsNotIssued) {
		t.Fatalf("err = %v, want ErrTicketsNotIssued for an unissued order", err)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE event_type='refunded'`); n != 0 {
		t.Fatalf("a refused refund appended %d events", n)
	}
}

// Two different refunds on one order must not select the same ticket. They serialize
// on the order's ticket rows.
func TestConcurrentRefundsSelectDisjointTickets(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	order, _, _ := issueOrder(t, ctx, st, org, 4)

	type outcome struct {
		batch TicketRefundBatch
		err   error
	}
	results := make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		go func() {
			b, err := st.RefundOrderTickets(ctx, org, order, uuid.New(), 2)
			results <- outcome{b, err}
		}()
	}
	seen := map[uuid.UUID]bool{}
	for i := 0; i < 2; i++ {
		got := <-results
		if got.err != nil {
			t.Fatalf("concurrent refund failed: %v", got.err)
		}
		for _, id := range got.batch.TicketIDs {
			if seen[id] {
				t.Fatalf("ticket %s selected by both refunds", id)
			}
			seen[id] = true
		}
	}
	if len(seen) != 4 {
		t.Fatalf("selected %d distinct tickets, want 4", len(seen))
	}
}

// A different organizer cannot void another's tickets (ADR-002).
func TestRefundOrderTicketsIsOrganizerScoped(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	order, _, _ := issueOrder(t, ctx, st, org, 2)

	if _, err := st.RefundOrderTickets(ctx, uuid.New(), order, uuid.New(), 1); !errors.Is(err, ErrTicketsNotIssued) {
		t.Fatalf("err = %v, want the order to be invisible to another organizer", err)
	}
}

// ai-review F2: a refund id belongs to ONE order. The event-id derivation cannot say so
// — against a different order it derives different ids, finds nothing of its own, and
// would happily void a second batch — so the binding is stored and checked.
func TestRefundIDCannotVoidTicketsInASecondOrder(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	orderA, _, _ := issueOrder(t, ctx, st, org, 2)
	orderB, _, _ := issueOrder(t, ctx, st, org, 2)
	refundID := uuid.New()

	if _, err := st.RefundOrderTickets(ctx, org, orderA, refundID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RefundOrderTickets(ctx, org, orderB, refundID, 1); !errors.Is(err, ErrRefundBatchConflict) {
		t.Fatalf("err = %v, want ErrRefundBatchConflict — one refund id, one order", err)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE event_type='refunded'`); n != 1 {
		t.Fatalf("refunded events = %d, want 1 (the second order must be untouched)", n)
	}
	// The same refund id with a different QUANTITY against its own order is equally a
	// conflict — a replay must be the same request or no request.
	if _, err := st.RefundOrderTickets(ctx, org, orderA, refundID, 2); !errors.Is(err, ErrRefundBatchConflict) {
		t.Fatalf("err = %v, want ErrRefundBatchConflict for a changed quantity", err)
	}
}

// The same reuse, raced. The two requests lock DISJOINT ticket rows, so nothing in the
// per-order locking serializes them — only the binding's primary key does.
func TestConcurrentCrossOrderRefundIDReuseVoidsOneBatch(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	orderA, _, _ := issueOrder(t, ctx, st, org, 2)
	orderB, _, _ := issueOrder(t, ctx, st, org, 2)
	refundID := uuid.New()

	errs := make(chan error, 2)
	for _, order := range []uuid.UUID{orderA, orderB} {
		go func(o uuid.UUID) {
			_, err := st.RefundOrderTickets(ctx, org, o, refundID, 1)
			errs <- err
		}(order)
	}
	// Counting successes is not enough: the loser must fail with the CONFLICT, not with a
	// raw unique violation the API would map to 500 (ai-review pass 2 caught exactly that,
	// because the first version of this test only counted).
	var ok, conflicts int
	for i := 0; i < 2; i++ {
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.Is(err, ErrRefundBatchConflict):
			conflicts++
		default:
			t.Fatalf("loser failed with %v, want ErrRefundBatchConflict", err)
		}
	}
	if ok != 1 || conflicts != 1 {
		t.Fatalf("%d succeeded and %d conflicted, want exactly 1 of each", ok, conflicts)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE event_type='refunded'`); n != 1 {
		t.Fatalf("refunded events = %d, want 1", n)
	}
}
