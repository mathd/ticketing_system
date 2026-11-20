//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
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

// lifecycleStep is one event of a ticket's history, in chain order.
type lifecycleStep struct {
	ID   uuid.UUID
	Type string
}

// allHistories reads every ticket in the test's schema with its complete history in
// chain order. A ticket with no event still has an entry. The tests compare the whole
// map, so an extra event, a missing event, a changed event id, or an extra ticket all
// show as a difference.
func allHistories(t *testing.T, ctx context.Context, db *sql.DB) map[uuid.UUID][]lifecycleStep {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT t.id, e.id, e.event_type
		FROM tickets t
		LEFT JOIN lifecycle_events e ON e.ticket_id = t.id
		LEFT JOIN lifecycle_event_integrity i ON i.event_id = e.id
		ORDER BY t.id, i.sequence NULLS LAST, e.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[uuid.UUID][]lifecycleStep{}
	for rows.Next() {
		var ticketID uuid.UUID
		var eventID uuid.NullUUID
		var eventType sql.NullString
		if err := rows.Scan(&ticketID, &eventID, &eventType); err != nil {
			t.Fatal(err)
		}
		steps := out[ticketID]
		if steps == nil {
			steps = []lifecycleStep{}
		}
		if eventID.Valid {
			steps = append(steps, lifecycleStep{ID: eventID.UUID, Type: eventType.String})
		}
		out[ticketID] = steps
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// refundBinding is one row of ticket_refund_batches, without its timestamp.
type refundBinding struct {
	OrganizerID, RefundID, OrderID uuid.UUID
	Quantity                       int32
}

// allRefundBindings reads the complete ticket_refund_batches relation in the test's
// schema.
func allRefundBindings(t *testing.T, ctx context.Context, db *sql.DB) []refundBinding {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT organizer_id, refund_id, order_id, quantity FROM ticket_refund_batches ORDER BY organizer_id, refund_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := []refundBinding{}
	for rows.Next() {
		var b refundBinding
		if err := rows.Scan(&b.OrganizerID, &b.RefundID, &b.OrderID, &b.Quantity); err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// requireStepTypes fails unless the history has exactly these event types, in order.
// It checks the seed that a test's claim depends on.
func requireStepTypes(t *testing.T, ticketID uuid.UUID, history []lifecycleStep, types ...string) {
	t.Helper()
	got := make([]string, 0, len(history))
	for _, step := range history {
		got = append(got, step.Type)
	}
	if !reflect.DeepEqual(got, types) {
		t.Fatalf("seeded state is wrong: ticket %s has %v, want %v", ticketID, got, types)
	}
}

// requireTrailState compares the complete ticket histories and the complete binding
// relation with the expected values. It reports with Errorf, so that a test that
// already saw the wrong result still reports every row the writer committed.
func requireTrailState(t *testing.T, ctx context.Context, db *sql.DB, wantHistories map[uuid.UUID][]lifecycleStep, wantBindings []refundBinding) {
	t.Helper()
	if got := allHistories(t, ctx, db); !reflect.DeepEqual(got, wantHistories) {
		t.Errorf("ticket histories changed:\n got  %v\n want %v", got, wantHistories)
	}
	if got := allRefundBindings(t, ctx, db); !reflect.DeepEqual(got, wantBindings) {
		t.Errorf("refund bindings = %v, want %v", got, wantBindings)
	}
}

// TKT-527. An exchanged ticket is already void, so a new refund must not select it.
// The sequence is the one in the ticket: the real switch voids every source ticket,
// then a refund arrives for the SOURCE order. No source ticket is eligible, so Access
// refuses with the exchange sentinel and writes nothing. Before TKT-527 the writer
// selected the lowest source ticket and committed a refund event and a binding.
func TestRefundOrderTicketsRefusesWhenExchangedTicketsLeaveTooFew(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	source, _, sourceIDs, replacementIDs := exchangeUnderSourceRef(t, ctx, db, st, org)

	before := allHistories(t, ctx, db)
	if len(before) != len(sourceIDs)+len(replacementIDs) {
		t.Fatalf("seeded state is wrong: %d tickets, want %d", len(before), len(sourceIDs)+len(replacementIDs))
	}
	for _, id := range sourceIDs {
		requireStepTypes(t, id, before[id], "issued", "exchanged")
	}
	for _, id := range replacementIDs {
		requireStepTypes(t, id, before[id], "issued")
	}
	if got := allRefundBindings(t, ctx, db); len(got) != 0 {
		t.Fatalf("seeded state is wrong: refund bindings %v before any refund", got)
	}

	_, err := st.RefundOrderTickets(ctx, org, source, uuid.New(), 1)
	if !errors.Is(err, ErrRefundTicketsExchanged) || errors.Is(err, ErrTicketsNotIssued) {
		t.Errorf("err = %v, want ErrRefundTicketsExchanged and not ErrTicketsNotIssued — "+
			"the exchanged tickets are already void, and this is not an issuance delay", err)
	}
	requireTrailState(t, ctx, db, before, []refundBinding{})

	// The failure path is organizer scoped too (engineering rules, write boundaries).
	// Another organizer sees no ticket of this order, so the answer is the issuance
	// sentinel. The exchange sentinel here would disclose the owner's exchange.
	after := allHistories(t, ctx, db)
	afterBindings := allRefundBindings(t, ctx, db)
	_, err = st.RefundOrderTickets(ctx, uuid.New(), source, uuid.New(), 1)
	if !errors.Is(err, ErrTicketsNotIssued) || errors.Is(err, ErrRefundTicketsExchanged) {
		t.Errorf("another organizer: err = %v, want ErrTicketsNotIssued and not ErrRefundTicketsExchanged", err)
	}
	requireTrailState(t, ctx, db, after, afterBindings)
}

// TKT-527. Eligibility is per ticket, not per order. Whole-order exchange is current
// policy (ADR-039, TKT-158), not a property of the trail, so the lowest ticket is
// exchanged here with a signed direct append. The refund must take the next eligible
// ticket. An order-wide refusal fails this test, and so does a selector that takes the
// lowest id without checking eligibility.
func TestRefundOrderTicketsSkipsAnExchangedTicketWhenEnoughOthersQualify(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	order, sorted, _ := issueOrder(t, ctx, st, org, 3)
	if _, err := st.appendLifecycleForTest(ctx, sorted[0], order, org, "exchanged"); err != nil {
		t.Fatal(err)
	}

	before := allHistories(t, ctx, db)
	if len(before) != 3 {
		t.Fatalf("seeded state is wrong: %d tickets, want 3", len(before))
	}
	requireStepTypes(t, sorted[0], before[sorted[0]], "issued", "exchanged")
	requireStepTypes(t, sorted[1], before[sorted[1]], "issued")
	requireStepTypes(t, sorted[2], before[sorted[2]], "issued")

	refundID := uuid.New()
	batch, err := st.RefundOrderTickets(ctx, org, order, refundID, 1)
	if err != nil {
		t.Errorf("refund: %v — one eligible ticket is enough for quantity 1", err)
	} else if batch.Replay || !reflect.DeepEqual(batch.TicketIDs, []uuid.UUID{sorted[1]}) {
		t.Errorf("selected %v (replay=%t), want only the lowest ELIGIBLE ticket %s", batch.TicketIDs, batch.Replay, sorted[1])
	}

	want := map[uuid.UUID][]lifecycleStep{}
	for id, history := range before {
		want[id] = append([]lifecycleStep{}, history...)
	}
	want[sorted[1]] = append(want[sorted[1]], lifecycleStep{ID: refundEventID(refundID, sorted[1]), Type: "refunded"})
	requireTrailState(t, ctx, db, want, []refundBinding{{OrganizerID: org, RefundID: refundID, OrderID: order, Quantity: 1}})
}

// seedHistoricalRefund writes what an older Access writer committed for a refund of an
// already exchanged ticket: the binding and the refund event under the derived id, in
// one transaction under the ticket lock. The event goes through appendLifecycle, so the
// chain stays verifiable.
func seedHistoricalRefund(t *testing.T, ctx context.Context, st *Postgres, org, order, refundID, ticketID uuid.UUID) {
	t.Helper()
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var id TicketIdentity
	if err := tx.QueryRowContext(ctx, `SELECT order_id,organizer_id,slot_id FROM tickets WHERE id=$1 FOR UPDATE`, ticketID).
		Scan(&id.OrderID, &id.OrganizerID, &id.SlotID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ticket_refund_batches(organizer_id,refund_id,order_id,quantity) VALUES($1,$2,$3,1)`,
		org, refundID, order); err != nil {
		t.Fatal(err)
	}
	if _, err := st.appendLifecycle(ctx, tx, appendInput{
		TicketID: ticketID, OrderID: id.OrderID, OrganizerID: id.OrganizerID, SlotID: id.SlotID,
		EventID: refundEventID(refundID, ticketID), Type: "refunded",
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TKT-527. A refund that an older writer committed on an exchanged ticket still
// replays. Replay resolves before the exchange exclusion: a classification that skips
// exchanged tickets first would not find this refund's own event, and would answer
// with the exchange refusal instead. The ordinary replay test cannot see that, because
// its tickets are not exchanged.
func TestRefundOrderTicketsReplaysAHistoricalRefundOfAnExchangedTicket(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	source, _, sourceIDs, _ := exchangeUnderSourceRef(t, ctx, db, st, org)
	refundID := uuid.New()
	seedHistoricalRefund(t, ctx, st, org, source, refundID, sourceIDs[0])

	before := allHistories(t, ctx, db)
	requireStepTypes(t, sourceIDs[0], before[sourceIDs[0]], "issued", "exchanged", "refunded")
	if got := before[sourceIDs[0]][2].ID; got != refundEventID(refundID, sourceIDs[0]) {
		t.Fatalf("seeded state is wrong: refund event id %s, want the derived id", got)
	}
	requireStepTypes(t, sourceIDs[1], before[sourceIDs[1]], "issued", "exchanged")
	wantBindings := []refundBinding{{OrganizerID: org, RefundID: refundID, OrderID: source, Quantity: 1}}
	if got := allRefundBindings(t, ctx, db); !reflect.DeepEqual(got, wantBindings) {
		t.Fatalf("seeded state is wrong: refund bindings %v, want %v", got, wantBindings)
	}

	batch, err := st.RefundOrderTickets(ctx, org, source, refundID, 1)
	if err != nil {
		t.Errorf("replay of a historical refund: %v", err)
	} else if !batch.Replay || batch.RefundID != refundID || batch.OrderID != source ||
		!reflect.DeepEqual(batch.TicketIDs, []uuid.UUID{sourceIDs[0]}) {
		t.Errorf("replay = %+v, want replay of %s on order %s with ticket %s", batch, refundID, source, sourceIDs[0])
	}
	requireTrailState(t, ctx, db, before, wantBindings)
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

// The same reuse, with the interleaving PINNED rather than raced.
//
// A two-goroutine version was written first and the review was right about it: nothing
// forces both transactions to read "no binding" before either inserts, so one can commit
// first and the other returns the conflict from the ordinary binding CHECK — passing even
// with the 23505 mapping deleted. That is the same defect as TKT-156's racing test, which
// passed against deliberately broken code. When the fix is a specific error path, drive
// that path.
//
// Here the binding row is held uncommitted by an outer transaction, so RefundOrderTickets
// gets past its own binding check (which cannot see the uncommitted row), blocks on the
// primary key, and receives 23505 the instant the outer transaction commits. That is
// exactly the production interleaving, and it happens every run.
func TestCrossOrderRefundIDLoserGetsAConflictNotAnInternalError(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	orderA, _, _ := issueOrder(t, ctx, st, org, 2)
	orderB, _, _ := issueOrder(t, ctx, st, org, 2)
	refundID := uuid.New()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ticket_refund_batches(organizer_id,refund_id,order_id,quantity) VALUES($1,$2,$3,1)`,
		org, refundID, orderA); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := st.RefundOrderTickets(ctx, org, orderB, refundID, 1)
		done <- err
	}()

	// It must be blocked on the primary key, not answering from the binding check: an
	// uncommitted row is invisible to that check by construction.
	select {
	case err := <-done:
		t.Fatalf("returned (%v) without contending on the binding primary key", err)
	case <-time.After(750 * time.Millisecond):
	}

	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrRefundBatchConflict) {
		t.Fatalf("err = %v, want ErrRefundBatchConflict — a raw 23505 becomes an API 500", err)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE event_type='refunded'`); n != 0 {
		t.Fatalf("the losing request voided %d tickets", n)
	}
}
