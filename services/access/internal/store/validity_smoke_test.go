//go:build smoke

package store

import (
	"context"
	"database/sql"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TKT-355. The forcing function for the verdict table: every lifecycle type the
// database admits must be classified, and the table must name no type the database
// refuses. It reads the live CHECK after every migration, so a migration that adds a
// type fails the gate until DeriveTicketValidity says what the type does to a ticket's
// verdict. That is why the derivation can keep the verdict for an unknown type instead
// of refusing the whole bundle: no migration can introduce one silently.
//
// Both directions, on purpose. With only "every CHECK type is classified", a change to
// pg_get_constraintdef's output that the pattern no longer matches would yield zero
// types and pass. The reverse direction then fails on every classified type.
func TestEveryLifecycleTypeHasAValidityEffect(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	rows, err := db.QueryContext(ctx, `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid='lifecycle_events'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%event_type%'`)
	if err != nil {
		t.Fatal(err)
	}
	var defs []string
	for rows.Next() {
		var def string
		if err := rows.Scan(&def); err != nil {
			t.Fatal(err)
		}
		defs = append(defs, def)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if len(defs) != 1 {
		t.Fatalf("want exactly one event_type CHECK on lifecycle_events, got %d: %v", len(defs), defs)
	}

	// The CHECK must have exactly the shape PostgreSQL deparses for
	// `event_type IN ('a', 'b', ...)`: one ANY over an ARRAY of text literals and nothing
	// else. Only for that shape is "the literals" the same set as "the admitted values". Any
	// other shape (a concatenation, a second branch, another cast) fails here, so the test
	// is updated with the CHECK instead of passing on a set it did not read. A literal is any
	// SQL string, not only [a-z_] ('revoked_v2', 'revoked-v2'); '' is an escaped quote.
	const lit = `'(?:[^']|'')*'::text`
	shape := regexp.MustCompile(`^CHECK \(\(event_type = ANY \(ARRAY\[` + lit + `(?:, ` + lit + `)*\]\)\)\)$`)
	if !shape.MatchString(defs[0]) {
		t.Fatalf("the event_type CHECK is not a plain IN list of text literals; update this test with the CHECK: %s", defs[0])
	}
	admitted := map[string]bool{}
	for _, m := range regexp.MustCompile(`'((?:[^']|'')*)'::text`).FindAllStringSubmatch(defs[0], -1) {
		admitted[strings.ReplaceAll(m[1], "''", "'")] = true
	}
	var unclassified, unknown []string
	for typ := range admitted {
		if _, ok := lifecycleEffects[typ]; !ok {
			unclassified = append(unclassified, typ)
		}
	}
	for typ := range lifecycleEffects {
		if !admitted[typ] {
			unknown = append(unknown, typ)
		}
	}
	sort.Strings(unclassified)
	sort.Strings(unknown)
	if len(unclassified) > 0 {
		t.Errorf("lifecycle types the CHECK admits but lifecycleEffects does not classify: %v (CHECK: %s)", unclassified, defs[0])
	}
	if len(unknown) > 0 {
		t.Errorf("lifecycleEffects classifies types the CHECK does not admit: %v (CHECK: %s)", unknown, defs[0])
	}
}

// verdicts reads a guest reference the way the bundle handler does — Tickets, then each
// ticket's History — and derives each verdict from that same slice. It also returns the
// event types, so a test can assert that the void it seeded is really in the history
// before it trusts a verdict about it.
func verdicts(t *testing.T, ctx context.Context, st *Postgres, ref uuid.UUID) (map[uuid.UUID]TicketValidity, map[uuid.UUID][]string) {
	t.Helper()
	tickets, err := st.Tickets(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	got, types := map[uuid.UUID]TicketValidity{}, map[uuid.UUID][]string{}
	for _, tk := range tickets {
		history, err := st.History(ctx, tk.ID)
		if err != nil {
			t.Fatal(err)
		}
		got[tk.ID] = DeriveTicketValidity(history)
		types[tk.ID] = typesOf(history)
	}
	return got, types
}

func guestRefOf(t *testing.T, ctx context.Context, db *sql.DB, ticketID uuid.UUID) uuid.UUID {
	t.Helper()
	var ref uuid.UUID
	if err := db.QueryRowContext(ctx, `SELECT guest_order_ref FROM tickets WHERE id=$1`, ticketID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	return ref
}

func hasType(types []string, want string) bool {
	for _, typ := range types {
		if typ == want {
			return true
		}
	}
	return false
}

// assertVerdicts compares by ticket id and requires the exact set: a ticket missing from
// the bundle, or one the test did not expect, fails as loudly as a wrong verdict.
func assertVerdicts(t *testing.T, got map[uuid.UUID]TicketValidity, want map[uuid.UUID]TicketValidity, types map[uuid.UUID][]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("bundle has %d tickets, want %d", len(got), len(want))
	}
	for id, w := range want {
		g, ok := got[id]
		if !ok {
			t.Fatalf("ticket %s is not in the bundle", id)
		}
		if g != w {
			t.Errorf("ticket %s (history %v) verdict = %+v, want %+v", id, types[id], g, w)
		}
	}
}

// A mixed bundle through the real refund writer. The refunded ticket is the one the
// writer RETURNS: it picks the lowest ids, and the bundle orders by issue time, so its
// position in the bundle changes from run to run and an index would be a coin toss.
func TestValidityAfterPartialRefund(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	order, ids, _ := issueOrder(t, ctx, st, org, 2)
	batch, err := st.RefundOrderTickets(ctx, org, order, uuid.New(), 1)
	if err != nil {
		t.Fatal(err)
	}
	refunded := batch.TicketIDs[0]
	kept := ids[0]
	if kept == refunded {
		kept = ids[1]
	}

	got, types := verdicts(t, ctx, st, guestRefOf(t, ctx, db, refunded))
	if !hasType(types[refunded], "refunded") || hasType(types[kept], "refunded") {
		t.Fatalf("seeded state is wrong: refunded %v, kept %v", types[refunded], types[kept])
	}
	assertVerdicts(t, got, map[uuid.UUID]TicketValidity{refunded: voidedRefunded, kept: stillValid}, types)
}

// An exchange under ONE reference, as the consumer writes it: the replacement tickets
// carry the source order's guest reference (consumer.go). replacementTickets mints a
// fresh reference by default, which would split the bundle in two and prove nothing
// about a mixed one, so the reference is overridden here.
func exchangeUnderSourceRef(t *testing.T, ctx context.Context, db *sql.DB, st *Postgres, org uuid.UUID) (source uuid.UUID, ref uuid.UUID, sourceIDs, replacementIDs []uuid.UUID) {
	t.Helper()
	source, sourceIDs, _ = issueOrder(t, ctx, st, org, 2)
	ref = guestRefOf(t, ctx, db, sourceIDs[0])
	replacements := replacementTickets(uuid.New(), org, uuid.New(), 2)
	for i := range replacements {
		replacements[i].GuestOrderRef = ref
		replacementIDs = append(replacementIDs, replacements[i].ID)
	}
	if err := st.SwitchExchange(ctx, SwitchExchangeInput{
		EventID: uuid.New(), ExchangeID: uuid.New(), SourceOrderID: source, OrganizerID: org, Tickets: replacements,
	}); err != nil {
		t.Fatal(err)
	}
	return source, ref, sourceIDs, replacementIDs
}

func TestValidityAfterExchangeUnderOneReference(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	_, ref, sourceIDs, replacementIDs := exchangeUnderSourceRef(t, ctx, db, st, org)

	got, types := verdicts(t, ctx, st, ref)
	want := map[uuid.UUID]TicketValidity{}
	for _, id := range sourceIDs {
		if !hasType(types[id], "exchanged") {
			t.Fatalf("source ticket %s was not exchanged: %v", id, types[id])
		}
		want[id] = voidedExchanged
	}
	for _, id := range replacementIDs {
		want[id] = stillValid
	}
	assertVerdicts(t, got, want, types)
}

// Exchange, then refund, through Access's own writers. Commerce refuses this sequence
// today (BindOrderRefund refuses an exchanged order), so it is reachable only here. The
// precedence still decides it, and it must match the gate: refund first.
func TestValidityAfterExchangeThenRefund(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	source, ref, sourceIDs, replacementIDs := exchangeUnderSourceRef(t, ctx, db, st, org)
	batch, err := st.RefundOrderTickets(ctx, org, source, uuid.New(), 1)
	if err != nil {
		t.Fatalf("refund after exchange: %v", err)
	}
	both := batch.TicketIDs[0]

	got, types := verdicts(t, ctx, st, ref)
	if !hasType(types[both], "exchanged") || !hasType(types[both], "refunded") {
		t.Fatalf("seeded state is wrong: %s has %v, want exchanged and refunded", both, types[both])
	}
	want := map[uuid.UUID]TicketValidity{both: voidedRefunded}
	for _, id := range sourceIDs {
		if id != both {
			want[id] = voidedExchanged
		}
	}
	for _, id := range replacementIDs {
		want[id] = stillValid
	}
	assertVerdicts(t, got, want, types)
}

// Facts recorded after a void do not restore it. Each of the three writers below accepts
// a refunded ticket: a delivery can land after a fast refund, a staff resend selects
// every ticket of the order, and an offline gate reports an admission that already
// happened. The kept ticket gets the same facts and must stay valid, so the test fails
// for "any delivery voids" as well as for "the newest event decides".
func TestValidityIsNotRestoredByLaterFacts(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	order, ids, seeds := issueOrder(t, ctx, st, org, 2)
	batch, err := st.RefundOrderTickets(ctx, org, order, uuid.New(), 1)
	if err != nil {
		t.Fatal(err)
	}
	refunded := batch.TicketIDs[0]
	kept := ids[0]
	if kept == refunded {
		kept = ids[1]
	}

	for _, id := range ids {
		message, err := st.DeliveryID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.MarkDelivered(ctx, id, message); err != nil {
			t.Fatalf("deliver %s: %v", id, err)
		}
	}
	claim, err := st.ClaimRedelivery(ctx, org, order, "validity-resend")
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range claim.Tickets {
		if err := st.MarkRedelivered(ctx, org, "validity-resend", tk.TicketID, tk.MessageID); err != nil {
			t.Fatalf("redeliver %s: %v", tk.TicketID, err)
		}
	}
	for _, s := range seeds {
		if _, err := st.RecordAdmission(ctx, RecordAdmissionInput{
			TicketID: s.ticketID, OrderID: s.id.OrderID, OrganizerID: s.id.OrganizerID, SlotID: s.id.SlotID,
			OccurrenceID: uuid.New(), Type: AdmissionEntry, OccurredAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("record entry %s: %v", s.ticketID, err)
		}
	}

	got, types := verdicts(t, ctx, st, guestRefOf(t, ctx, db, refunded))
	for _, id := range ids {
		for _, typ := range []string{"delivered", "redelivered", "entry"} {
			if !hasType(types[id], typ) {
				t.Fatalf("seeded state is wrong: %s has %v, want %s after the refund", id, types[id], typ)
			}
		}
	}
	if last := types[refunded][len(types[refunded])-1]; last == "refunded" {
		t.Fatalf("seeded state is wrong: the refund is still the newest event of %s: %v", refunded, types[refunded])
	}
	assertVerdicts(t, got, map[uuid.UUID]TicketValidity{refunded: voidedRefunded, kept: stillValid}, types)
}
