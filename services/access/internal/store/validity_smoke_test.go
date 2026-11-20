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
	// else. That is the shape this test supports, and for it "the literals" is the same set
	// as "the admitted values". Any
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

	// TKT-526. The store's commercial-void predicate and the page verdict must agree on
	// every admitted type: a staff resend skips exactly the tickets the guest page shows
	// as voided. One ticket per type. `issued` is the issuance itself, because a second
	// issued fact is structurally refused; every other type is a signed append. The data
	// is quiescent, so History is the production reader and the predicate runs in its
	// own short transaction.
	st := New(db, testConfig(t))
	org := uuid.New()
	types := make([]string, 0, len(admitted))
	for typ := range admitted {
		types = append(types, typ)
	}
	sort.Strings(types)
	for _, typ := range types {
		order, ids, _ := issueOrder(t, ctx, st, org, 1)
		id := ids[0]
		if typ != "issued" {
			if _, err := st.appendLifecycleForTest(ctx, id, order, org, typ); err != nil {
				t.Fatalf("seed %s: %v", typ, err)
			}
		}
		history, err := st.History(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !hasType(typesOf(history), typ) {
			t.Fatalf("seeded state is wrong: ticket %s has %v, want %s", id, typesOf(history), typ)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		void, err := ticketCommerciallyVoid(ctx, tx, id)
		_ = tx.Rollback()
		if err != nil {
			t.Fatal(err)
		}
		if page := DeriveTicketValidity(history).Status == ValidityVoided; void != page {
			t.Errorf("type %s (ticket %s, history %v): ticketCommerciallyVoid = %t, page verdict voided = %t",
				typ, id, typesOf(history), void, page)
		}
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

// Exchange, then refund: a HISTORICAL history. Commerce refuses this sequence
// (BindOrderRefund refuses an exchanged order). Older Access writers could still append
// a refund after an exchange through a direct internal call. Access now excludes
// exchanged tickets from new refunds (TKT-527), so the refund is seeded with a signed
// append on one known exchanged source. The precedence still decides such a history,
// and it must match the gate: refund first.
func TestValidityAfterExchangeThenRefund(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()
	source, ref, sourceIDs, replacementIDs := exchangeUnderSourceRef(t, ctx, db, st, org)
	both := sourceIDs[0]
	if _, err := st.appendLifecycleForTest(ctx, both, source, org, "refunded"); err != nil {
		t.Fatalf("seed the historical refund after exchange: %v", err)
	}

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

// Facts recorded after a void do not restore it. A first delivery can land after a fast
// refund, and an offline gate can report an admission that already happened; both real
// writers accept a refunded ticket. A staff resend no longer selects a voided ticket
// (TKT-526), but a history written before that can hold a `redelivered` fact after a
// refund, so the resend fact is a signed historical append. The kept ticket gets the same
// facts and must stay valid, so the test fails for "any delivery voids" as well as for
// "the newest event decides".
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
	for _, id := range ids {
		if _, err := st.appendLifecycleForTest(ctx, id, order, org, "redelivered"); err != nil {
			t.Fatalf("seed the historical resend of %s: %v", id, err)
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
	history := allHistories(t, ctx, db)
	requireStepTypes(t, refunded, history[refunded], "issued", "refunded", "delivered", "redelivered", "entry")
	requireStepTypes(t, kept, history[kept], "issued", "delivered", "redelivered", "entry")
	assertVerdicts(t, got, map[uuid.UUID]TicketValidity{refunded: voidedRefunded, kept: stillValid}, types)
}
