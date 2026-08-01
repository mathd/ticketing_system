//go:build smoke

package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// The entitlement switch (TKT-166). One property carries the whole ticket: the void and
// the issue happen TOGETHER. Everything else here exists to pin the ways that can be
// quietly false.

// replacementTickets builds n replacement tickets for an exchange, the way the consumer
// derives them.
func replacementTickets(order, org, slot uuid.UUID, n int) []Ticket {
	guestRef, buyer, ticketType := uuid.New(), uuid.New(), uuid.New()
	out := make([]Ticket, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Ticket{
			ID: uuid.New(), OrderID: order, GuestOrderRef: guestRef, OrganizerID: org,
			BuyerID: buyer, SlotID: slot, TicketTypeID: ticketType,
			Payload: "replacement-credential", IssuedAt: time.Now().UTC(),
		})
	}
	return out
}

// AC1. The switch voids every source ticket and issues the replacement set, and both
// halves are visible together or not at all.
func TestSwitchExchangeVoidsSourceAndIssuesReplacement(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	source, sourceIDs, _ := issueOrder(t, ctx, st, org, 2)
	replacementOrder, exchangeID := uuid.New(), uuid.New()
	tickets := replacementTickets(replacementOrder, org, uuid.New(), 2)

	if err := st.SwitchExchange(ctx, SwitchExchangeInput{
		EventID: uuid.New(), ExchangeID: exchangeID, SourceOrderID: source, OrganizerID: org, Tickets: tickets,
	}); err != nil {
		t.Fatalf("switch: %v", err)
	}

	for _, id := range sourceIDs {
		if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id='`+id.String()+`' AND event_type='exchanged'`); n != 1 {
			t.Fatalf("source ticket %s has %d exchanged events, want 1", id, n)
		}
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM tickets WHERE order_id='`+replacementOrder.String()+`'`); n != 2 {
		t.Fatalf("replacement order has %d tickets, want 2", n)
	}
	// Every replacement ticket carries its own `issued` event — the switch does not get
	// to skip the append just because it wrote the row itself (ADR-021).
	for _, tk := range tickets {
		if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id='`+tk.ID.String()+`' AND event_type='issued'`); n != 1 {
			t.Fatalf("replacement ticket %s has %d issued events, want 1", tk.ID, n)
		}
	}
}

// AC6, and the reason this function is one transaction rather than two.
//
// The failure is injected DETERMINISTICALLY — a duplicate replacement ticket id, which
// the primary key rejects partway through the second loop, after every source ticket has
// already been voided in this transaction. No goroutines, no timing: the test either
// observes a full rollback or it does not.
//
// Against a two-transaction implementation this is the neither-admits window made
// permanent, and the assertion below is what fails.
func TestSwitchExchangeRollsBackTheVoidsWhenIssuingFails(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	source, sourceIDs, seeds := issueOrder(t, ctx, st, org, 2)
	eventID := uuid.New()

	tickets := replacementTickets(uuid.New(), org, uuid.New(), 2)
	tickets[1].ID = tickets[0].ID // the deterministic failure: same id twice

	if err := st.SwitchExchange(ctx, SwitchExchangeInput{
		EventID: eventID, ExchangeID: uuid.New(), SourceOrderID: source, OrganizerID: org, Tickets: tickets,
	}); err == nil {
		t.Fatal("switch with a duplicate replacement ticket id must fail")
	}

	// The old entitlement is intact — the buyer at the gate still gets in.
	for _, id := range sourceIDs {
		if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE ticket_id='`+id.String()+`' AND event_type='exchanged'`); n != 0 {
			t.Fatalf("source ticket %s kept %d exchanged events after a failed switch — the void did not roll back", id, n)
		}
	}
	result, err := st.Redeem(ctx, seeds[0].redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted {
		t.Fatalf("a source ticket must still admit after a failed switch: %+v", result)
	}
	// And the receipt rolled back with everything else, so the event is still owed. A
	// receipt that survived would make the retry a silent no-op and strand the exchange.
	if n := countRows(t, ctx, db, `SELECT count(*) FROM consumed_events WHERE event_id='`+eventID.String()+`'`); n != 0 {
		t.Fatalf("the dedupe receipt survived a rolled-back switch (%d rows) — the retry would do nothing", n)
	}
}

// A redelivery of the same event must not switch twice. The receipt is the whole
// mechanism; there is no per-ticket replay resolution the way refunds have one.
func TestSwitchExchangeIsIdempotentOnTheEventID(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	source, _, _ := issueOrder(t, ctx, st, org, 2)
	eventID, replacementOrder := uuid.New(), uuid.New()
	in := SwitchExchangeInput{
		EventID: eventID, ExchangeID: uuid.New(), SourceOrderID: source, OrganizerID: org,
		Tickets: replacementTickets(replacementOrder, org, uuid.New(), 2),
	}
	if err := st.SwitchExchange(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := st.SwitchExchange(ctx, in); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM tickets WHERE order_id='`+replacementOrder.String()+`'`); n != 2 {
		t.Fatalf("redelivery produced %d replacement tickets, want 2", n)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE event_type='exchanged'`); n != 2 {
		t.Fatalf("redelivery appended %d exchanged events, want 2", n)
	}
}

// An exchange that outruns issuance must not report success. Issuance is asynchronous, so
// this genuinely happens; switching an empty set would issue the replacement beside a
// source order whose tickets arrive live moments later — both-admit by another route.
func TestSwitchExchangeRefusesBeforeTheSourceTicketsExist(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()

	err := st.SwitchExchange(ctx, SwitchExchangeInput{
		EventID: uuid.New(), ExchangeID: uuid.New(), SourceOrderID: uuid.New(), OrganizerID: org,
		Tickets: replacementTickets(uuid.New(), org, uuid.New(), 2),
	})
	if !errors.Is(err, ErrExchangeTicketsNotIssued) {
		t.Fatalf("err = %v, want ErrExchangeTicketsNotIssued", err)
	}
	if n := countRows(t, ctx, db, `SELECT count(*) FROM tickets`); n != 0 {
		t.Fatalf("a refused switch issued %d replacement tickets", n)
	}
}

// A source order already voided by a refund is refused rather than voided twice. Commerce
// will not authorise this, so reaching it means a race or a repair — and appending a
// second void would collide on the singleton index halfway through the loop.
func TestSwitchExchangeRefusesAlreadyVoidedSourceTickets(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	source, _, _ := issueOrder(t, ctx, st, org, 2)
	if _, err := st.RefundOrderTickets(ctx, org, source, uuid.New(), 2); err != nil {
		t.Fatal(err)
	}

	err := st.SwitchExchange(ctx, SwitchExchangeInput{
		EventID: uuid.New(), ExchangeID: uuid.New(), SourceOrderID: source, OrganizerID: org,
		Tickets: replacementTickets(uuid.New(), org, uuid.New(), 2),
	})
	if !errors.Is(err, ErrSourceTicketsAlreadyVoided) {
		t.Fatalf("err = %v, want ErrSourceTicketsAlreadyVoided", err)
	}
}

// AC3, stated as an ordering because that is what it is. A corrupt chain takes the
// degraded fail-open posture (ADR-021 §D6) and admits ONCE. If the exchange check ran
// after it, the exchanged ticket would get in exactly once — and its replacement would
// get in too. One paid seat, two admissions.
func TestExchangedRefusalPrecedesIntegrityFailOpen(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	source, sourceIDs, seeds := issueOrder(t, ctx, st, org, 1)

	if err := st.SwitchExchange(ctx, SwitchExchangeInput{
		EventID: uuid.New(), ExchangeID: uuid.New(), SourceOrderID: source, OrganizerID: org,
		Tickets: replacementTickets(uuid.New(), org, uuid.New(), 1),
	}); err != nil {
		t.Fatal(err)
	}
	corruptChain(t, ctx, db, sourceIDs[0])

	result, err := st.Redeem(ctx, seeds[0].redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted || result.Decision != DecisionExchanged {
		t.Fatalf("an exchanged ticket with a corrupt chain: accepted=%t decision=%q — must be refused as exchanged, never admitted_degraded",
			result.Accepted, result.Decision)
	}
}

// The verdict is `exchanged`, not `refunded`. Folding them would send a buyer holding a
// live replacement off to look for a refund that does not exist.
func TestExchangedIsItsOwnVerdictNotRefunded(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	source, _, seeds := issueOrder(t, ctx, st, org, 1)

	if err := st.SwitchExchange(ctx, SwitchExchangeInput{
		EventID: uuid.New(), ExchangeID: uuid.New(), SourceOrderID: source, OrganizerID: org,
		Tickets: replacementTickets(uuid.New(), org, uuid.New(), 1),
	}); err != nil {
		t.Fatal(err)
	}
	result, err := st.Redeem(ctx, seeds[0].redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != DecisionExchanged {
		t.Fatalf("decision = %q, want %q", result.Decision, DecisionExchanged)
	}
}

// The replacement admits. Obvious, and worth pinning: a switch that voided the old
// tickets and issued replacements that do not scan is the neither-admits failure with
// extra steps.
func TestReplacementTicketsAdmitAfterTheSwitch(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	source, _, _ := issueOrder(t, ctx, st, org, 1)
	replacementOrder, slot := uuid.New(), uuid.New()
	tickets := replacementTickets(replacementOrder, org, slot, 1)

	if err := st.SwitchExchange(ctx, SwitchExchangeInput{
		EventID: uuid.New(), ExchangeID: uuid.New(), SourceOrderID: source, OrganizerID: org, Tickets: tickets,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := st.Redeem(ctx, RedeemInput{
		TicketID: tickets[0].ID, OrderID: replacementOrder, OrganizerID: org, SlotID: slot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted {
		t.Fatalf("the replacement ticket must admit: %+v", result)
	}
}

// AC2. The trail verifies clean after a switch — every `exchanged` and every replacement
// `issued` went through appendLifecycle, so the verifier's one-to-one coverage holds. A
// direct INSERT anywhere in the switch path reads as tampering and fails here.
func TestSwitchExchangeLeavesTheTrailVerifiable(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	cfg := testConfig(t)
	st := New(db, cfg)
	org := uuid.New()
	source, _, _ := issueOrder(t, ctx, st, org, 3)

	if err := st.SwitchExchange(ctx, SwitchExchangeInput{
		EventID: uuid.New(), ExchangeID: uuid.New(), SourceOrderID: source, OrganizerID: org,
		Tickets: replacementTickets(uuid.New(), org, uuid.New(), 3),
	}); err != nil {
		t.Fatal(err)
	}
	if err := New(db, verifyOnlyConfig(t, cfg)).VerifyLifecycle(ctx, VerifyOptions{RequireCoverage: true}); err != nil {
		t.Fatalf("the trail does not verify after a switch: %v", err)
	}
}

// AC2. `exchanged` joins the once-per-ticket set, and the head migration stays
// irreversible — the trail is signed and immutable, so 0008 refuses to roll back exactly
// as 0004 through 0007 do. A conditional guard ("only if no exchanged rows exist") would
// be weaker than the convention and would quietly make the head reversible on any
// database that simply had not exchanged anything yet.
func TestExchangedLifecycleMigrationIsIrreversible(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(ctx); err == nil {
		t.Fatal("migration 0008 rolled back; lifecycle history is not protected")
	}
	// The singleton index still names `exchanged`, so the failed attempt changed nothing.
	var def string
	if err := db.QueryRowContext(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname='lifecycle_events_singleton_type_uidx'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(def, "exchanged") {
		t.Fatalf("a failed down attempt altered the singleton index: %s", def)
	}
}

// The guard admits `exchanged` and still refuses an invented type. Widening a CHECK is
// easy to widen too far.
func TestExchangedIsAdmittedAndUnknownTypesAreNot(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	source, sourceIDs, _ := issueOrder(t, ctx, st, org, 1)

	if err := st.SwitchExchange(ctx, SwitchExchangeInput{
		EventID: uuid.New(), ExchangeID: uuid.New(), SourceOrderID: source, OrganizerID: org,
		Tickets: replacementTickets(uuid.New(), org, uuid.New(), 1),
	}); err != nil {
		t.Fatal(err)
	}
	// A second `exchanged` on the same ticket is refused by the singleton index — the
	// event is once-per-ticket, like issued/delivered/redeemed/refunded.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO lifecycle_events(id,ticket_id,order_id,organizer_id,slot_id,event_type,occurred_at)
		VALUES($1,$2,$3,$4,$5,'exchanged',now())`,
		uuid.New(), sourceIDs[0], source, org, uuid.New()); err == nil {
		t.Fatal("a second exchanged event on one ticket was accepted")
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO lifecycle_events(id,ticket_id,order_id,organizer_id,slot_id,event_type,occurred_at)
		VALUES($1,$2,$3,$4,$5,'teleported',now())`,
		uuid.New(), sourceIDs[0], source, org, uuid.New()); err == nil {
		t.Fatal("the widened CHECK admits an unknown event type")
	}
}

// ai-review F1. The double admission this ticket would otherwise have introduced.
//
// The buyer scans in on the old ticket during `switch_pending`, and the switch then voids
// that used ticket and issues a fresh unredeemed one. Two admissions, one paid
// entitlement. TKT-158 could not produce this — it never switched anything — so it is new
// here, and a follow-up ticket does not make it un-shipped.
//
// The refusal costs a settled exchange that never switches, which is precisely the state
// TKT-158 shipped for EVERY exchange. Degrading to the previously-safe behaviour for the
// one unsafe case is the trade.
func TestSwitchExchangeRefusesAnAlreadyAdmittedSourceTicket(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	source, _, seeds := issueOrder(t, ctx, st, org, 2)

	// The buyer goes through the door on one of the old tickets.
	result, err := st.Redeem(ctx, seeds[0].redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted {
		t.Fatalf("the source ticket must admit before the exchange: %+v", result)
	}

	replacement := replacementTickets(uuid.New(), org, uuid.New(), 2)
	err = st.SwitchExchange(ctx, SwitchExchangeInput{
		EventID: uuid.New(), ExchangeID: uuid.New(), SourceOrderID: source, OrganizerID: org,
		Tickets: replacement,
	})
	if !errors.Is(err, ErrSourceTicketsAlreadyAdmitted) {
		t.Fatalf("err = %v, want ErrSourceTicketsAlreadyAdmitted", err)
	}
	// No fresh credential exists, so there is nothing to admit a second time.
	for _, tk := range replacement {
		if n := countRows(t, ctx, db, `SELECT count(*) FROM tickets WHERE id='`+tk.ID.String()+`'`); n != 0 {
			t.Fatalf("a refused switch issued replacement ticket %s", tk.ID)
		}
	}
	// And the other source ticket — never used — is untouched and still admits.
	if n := countRows(t, ctx, db, `SELECT count(*) FROM lifecycle_events WHERE event_type='exchanged'`); n != 0 {
		t.Fatalf("a refused switch voided %d source tickets", n)
	}
	live, err := st.Redeem(ctx, seeds[1].redeemInput())
	if err != nil {
		t.Fatal(err)
	}
	if !live.Accepted {
		t.Fatalf("the unused source ticket must still admit: %+v", live)
	}
}

// A pass `entry` counts as admission too — the pass vocabulary is not `redeemed`
// (ADR-005), and checking only one of the two would leave the hole open for passes.
func TestAdmissionCheckCoversBothVocabularies(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	source, sourceIDs, _ := issueOrder(t, ctx, st, org, 1)

	// Appended through the store's own path so the chain stays verifiable.
	if _, err := st.appendLifecycleForTest(ctx, sourceIDs[0], source, org, "entry"); err != nil {
		t.Fatal(err)
	}
	err := st.SwitchExchange(ctx, SwitchExchangeInput{
		EventID: uuid.New(), ExchangeID: uuid.New(), SourceOrderID: source, OrganizerID: org,
		Tickets: replacementTickets(uuid.New(), org, uuid.New(), 1),
	})
	if !errors.Is(err, ErrSourceTicketsAlreadyAdmitted) {
		t.Fatalf("err = %v, want a pass `entry` to count as admission", err)
	}
}

// appendLifecycleForTest appends one event through the real append path, so the chain and
// its coverage stay verifiable. A direct INSERT would read as tampering.
func (p *Postgres) appendLifecycleForTest(ctx context.Context, ticketID, orderID, org uuid.UUID, eventType string) (uuid.UUID, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return uuid.Nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var id TicketIdentity
	if err := tx.QueryRowContext(ctx, `SELECT order_id,organizer_id,slot_id FROM tickets WHERE id=$1 FOR UPDATE`, ticketID).
		Scan(&id.OrderID, &id.OrganizerID, &id.SlotID); err != nil {
		return uuid.Nil, err
	}
	eventID := uuid.New()
	if _, err := p.appendLifecycle(ctx, tx, appendInput{
		TicketID: ticketID, OrderID: id.OrderID, OrganizerID: id.OrganizerID, SlotID: id.SlotID,
		EventID: eventID, Type: eventType,
	}); err != nil {
		return uuid.Nil, err
	}
	return eventID, tx.Commit()
}
