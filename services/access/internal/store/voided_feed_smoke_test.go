//go:build smoke

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// The voided-ticket feed (TKT-162, ADR-066). An offline scanner cannot learn that
// a ticket was refunded or exchanged, so it admits the holder; TKT-157 refuses one
// at a LIVE gate and TKT-269 makes the offline admit visible afterwards, but
// neither stops it. This feed is how the revocation state reaches the device
// before the scan.
//
// Two properties carry the ticket, and ADR-019 insists both are proved, because
// either alone is satisfiable by a wrong implementation:
//
//  1. the RESULT is scoped — another organizer's voided ticket never appears;
//  2. the SCAN is scoped — the plan reaches rows through an organizer-leading
//     index rather than reading everyone's voided rows and discarding them.
//
// A read that gets (1) right and (2) wrong returns correct answers at a cost
// proportional to the whole table. That is the defect ADR-019 exists to catch and
// it is invisible to every assertion about the returned rows.

// voidOneTicketOfItsOwnOrder refunds the single-ticket order issueTicket minted,
// so the `refunded` event is written by the real path and participates in the
// signed chain. A hand-written INSERT would read as tampering (ADR-021).
func voidOneTicketOfItsOwnOrder(t *testing.T, ctx context.Context, st *Postgres, s seeded) {
	t.Helper()
	if _, err := st.RefundOrderTickets(ctx, s.id.OrganizerID, s.id.OrderID, uuid.New(), 1); err != nil {
		t.Fatal(err)
	}
}

func feedIDs(t *testing.T, page []VoidedTicket) []uuid.UUID {
	t.Helper()
	out := make([]uuid.UUID, 0, len(page))
	for _, v := range page {
		out = append(out, v.TicketID)
	}
	return out
}

func containsID(ids []uuid.UUID, want uuid.UUID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// RESULT SCOPE (ADR-019 half 1). The poison row is another organizer's voided
// ticket, placed so it would fall inside the page if the tenant predicate were
// missing. Deleting `t.organizer_id = $1` must surface it.
func TestVoidedFeedIsScopedToTheCallersOrganizer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))

	orgA, orgB := uuid.New(), uuid.New()
	refunded := issueTicket(t, ctx, st, orgA)
	live := issueTicket(t, ctx, st, orgA)
	poison := issueTicket(t, ctx, st, orgB)

	voidOneTicketOfItsOwnOrder(t, ctx, st, refunded)
	voidOneTicketOfItsOwnOrder(t, ctx, st, poison)

	page, _, err := st.VoidedTickets(ctx, orgA, VoidedCursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	ids := feedIDs(t, page)

	if !containsID(ids, refunded.ticketID) {
		t.Fatalf("the organizer's own refunded ticket is missing from its feed: %v", ids)
	}
	if containsID(ids, poison.ticketID) {
		t.Fatalf("another organizer's voided ticket appeared in this feed — the tenant predicate is not doing its job: %v", ids)
	}
	if containsID(ids, live.ticketID) {
		t.Fatalf("a live ticket appeared in the voided feed: %v", ids)
	}
	if len(ids) != 1 {
		t.Fatalf("feed = %v, want exactly the one refunded ticket", ids)
	}
}

// BOTH voiding facts, not just the one the ticket was originally filed about.
// An exchanged ticket is the sharper offline case: its replacement is live
// elsewhere, so admitting the original admits the exchange twice.
func TestVoidedFeedCarriesRefundedAndExchanged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))

	org := uuid.New()
	refunded := issueTicket(t, ctx, st, org)
	live := issueTicket(t, ctx, st, org)

	voidOneTicketOfItsOwnOrder(t, ctx, st, refunded)

	// The exchanged fact, through the real switch so the `exchanged` event is
	// written by the path production uses and chains like any other.
	exchangedOrder, exchangedIDs, _ := issueOrder(t, ctx, st, org, 1)
	replacements := replacementTickets(uuid.New(), org, uuid.New(), 1)
	if err := st.SwitchExchange(ctx, SwitchExchangeInput{
		EventID: uuid.New(), ExchangeID: uuid.New(), SourceOrderID: exchangedOrder, OrganizerID: org,
		Tickets: replacements,
	}); err != nil {
		t.Fatal(err)
	}

	page, _, err := st.VoidedTickets(ctx, org, VoidedCursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	ids := feedIDs(t, page)

	if !containsID(ids, refunded.ticketID) {
		t.Fatalf("refunded ticket missing: %v", ids)
	}
	if !containsID(ids, exchangedIDs[0]) {
		t.Fatalf("exchanged ticket missing — the feed carries BOTH voiding facts, and an exchanged ticket's replacement is live elsewhere: %v", ids)
	}
	if containsID(ids, live.ticketID) {
		t.Fatalf("a live ticket appeared: %v", ids)
	}
	// The exchange's REPLACEMENT ticket is live and must not be in the feed:
	// voiding the source is exactly what makes the replacement the valid one.
	for _, tk := range replacements {
		if containsID(ids, tk.ID) {
			t.Fatalf("a replacement ticket appeared in the voided feed — voiding the source is what makes the replacement valid: %v", ids)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("feed = %v, want exactly the refunded and the exchanged source ticket", ids)
	}
}

// KEYSET pagination. Four voided tickets — three of them sharing one instant at
// the head of the feed — read two at a time, so a page boundary falls INSIDE the
// tie. See the fixture comment: the odd tie against the even limit is what makes
// this test able to fail.
func TestVoidedFeedPagesWithoutGapOrOverlap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))

	org := uuid.New()
	want := make(map[uuid.UUID]bool, 4)

	// One singly-voided ticket first, then THREE tickets of one order voided by a
	// single refund. Those three `refunded` events are appended inside one
	// transaction and share `now()` exactly — a real timestamp tie, produced the
	// way production produces it rather than forced afterwards. (It cannot be
	// forced afterwards: lifecycle_events is immutable by trigger and an UPDATE
	// raises.)
	//
	// THE SHAPE OF THIS FIXTURE IS THE TEST, and it took two mutation runs to get
	// right. THREE tied rows, newest, read TWO at a time. Both numbers matter:
	//
	//   - tied LAST, so the tie sits at the head of a newest-first feed;
	//   - an ODD tie against an even limit, so the page boundary falls INSIDE the
	//     tied instant rather than after it.
	//
	// With a tie of two and a limit of two the boundary lands cleanly after the
	// pair, and a cursor that has lost its tie-break entirely still returns all
	// four rows — the test passes while proving nothing. That is what the first
	// two versions of this fixture did, and only a mutation run distinguished
	// them: a fixture that names the tie-break is not the same as one that can
	// observe it.
	//
	// Why it matters: a cursor over the timestamp alone cannot resume between two
	// rows sharing one. It either skips a ticket or repeats it, and a skipped
	// ticket in a revocation feed is a voided holder walking through a gate.
	s := issueTicket(t, ctx, st, org)
	voidOneTicketOfItsOwnOrder(t, ctx, st, s)
	want[s.ticketID] = false

	tiedOrder, tiedIDs, _ := issueOrder(t, ctx, st, org, 3)
	if _, err := st.RefundOrderTickets(ctx, org, tiedOrder, uuid.New(), 3); err != nil {
		t.Fatal(err)
	}
	for _, id := range tiedIDs {
		want[id] = false
	}

	// The fixture's own precondition, asserted rather than assumed: the two newest
	// voided events must share an instant. If they do not, the page boundary at
	// limit=2 does not fall inside a tie and this test passes without exercising
	// the tie-break at all — proving nothing while looking like it proves the
	// thing its name claims.
	var newestTied bool
	if err := db.QueryRowContext(ctx, `
		SELECT count(DISTINCT occurred_at) = 1 FROM (
			SELECT e.occurred_at FROM lifecycle_events e
			  JOIN tickets t ON t.id = e.ticket_id
			 WHERE t.organizer_id = $1 AND e.event_type = 'refunded'
			 ORDER BY e.occurred_at DESC, e.id DESC
			 LIMIT 3) AS newest`, org).Scan(&newestTied); err != nil {
		t.Fatal(err)
	}
	if !newestTied {
		t.Fatal("fixture cannot exercise the tie-break: the three newest voided events do not share an instant, so a limit=2 page boundary never falls INSIDE the tie and a cursor with no tie-break would still pass")
	}

	seen := make([]uuid.UUID, 0, 4)
	var cursor VoidedCursor
	for page := 0; page < 6; page++ {
		got, next, err := st.VoidedTickets(ctx, org, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			break
		}
		if len(got) > 2 {
			t.Fatalf("page %d returned %d rows, want at most the limit of 2 — the limit+1 probe row must not be emitted", page, len(got))
		}
		seen = append(seen, feedIDs(t, got)...)
		if next.IsZero() {
			break
		}
		cursor = next
	}

	if len(seen) != 4 {
		t.Fatalf("paging saw %d ids, want 4 — a gap or a premature stop: %v", len(seen), seen)
	}
	for _, id := range seen {
		already, known := want[id]
		if !known {
			t.Fatalf("paging returned an id that was never voided: %s", id)
		}
		if already {
			t.Fatalf("paging returned %s twice — the cursor overlapped a page boundary", id)
		}
		want[id] = true
	}
}

// The last page says so. `next_cursor` distinguishes "more" from "done", and a
// feed that never reports done makes a scanner poll forever.
func TestVoidedFeedReportsTheLastPage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))

	org := uuid.New()
	for i := 0; i < 2; i++ {
		s := issueTicket(t, ctx, st, org)
		voidOneTicketOfItsOwnOrder(t, ctx, st, s)
	}

	page, next, err := st.VoidedTickets(ctx, org, VoidedCursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 {
		t.Fatalf("page = %d rows, want 2", len(page))
	}
	if !next.IsZero() {
		t.Fatalf("next cursor = %+v on a complete page, want the zero cursor — there is nothing after this", next)
	}

	full, next, err := st.VoidedTickets(ctx, org, VoidedCursor{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 2 || !next.IsZero() {
		t.Fatalf("a page exactly filling the limit with nothing beyond it must still report done: rows=%d next=%+v", len(full), next)
	}
}

// SCAN SCOPE (ADR-019 half 2) — the assertion the result tests cannot make.
//
// A feed that reads every organizer's voided rows and filters in Go returns the
// right answer, so TestVoidedFeedIsScopedToTheCallersOrganizer stays green. What
// separates the two is the PLAN, and it must be read under a generic plan: a
// value-bound custom plan is built knowing the parameter and will choose the
// index whether or not the predicate is sound, so it would pass against a
// deliberately widened predicate (ADR-019).
func TestVoidedFeedScanIsOrganizerScoped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)

	// Enough foreign rows that a sequential scan is genuinely the expensive
	// option. With too few, the planner rightly ignores the index and the test
	// fails for a reason that has nothing to do with the predicate.
	org := uuid.New()
	seedFeedVolume(t, ctx, db, org, 4000)

	plan := explainVoidedFeedGenericPlan(t, ctx, db, org)

	// Naming the index is NOT enough, and this is the ai-review [medium] finding.
	// A full index scan or a bitmap path can MENTION tickets_organizer_feed_idx
	// while reading every organizer's rows and filtering afterwards — which
	// satisfies both "the index appears" and "no Seq Scan on tickets" while being
	// exactly the unscoped read ADR-019 is about. What proves the scan is narrowed
	// is the index CONDITION carrying the tenant parameter.
	if !strings.Contains(plan, feedOrganizerIndex) {
		t.Fatalf("the voided feed does not reach rows through %s, so the read is scoped in its RESULT but not in its SCAN (ADR-019):\n%s", feedOrganizerIndex, plan)
	}
	if strings.Contains(plan, "Seq Scan on tickets") {
		t.Fatalf("the plan sequentially scans tickets — every organizer's rows are read and discarded:\n%s", plan)
	}
	cond := indexCondFor(t, plan, feedOrganizerIndex)
	if !strings.Contains(cond, "organizer_id") || !strings.Contains(cond, "$1") {
		t.Fatalf("%s is used, but its Index Cond is %q — it does not constrain organizer_id to $1, so the scan reads rows this organizer may not see and discards them afterwards (ADR-019):\n%s",
			feedOrganizerIndex, cond, plan)
	}
}

// indexCondFor pulls the Index Cond of the plan node that uses the named index.
//
// Reads the JSON plan rather than grepping the text form: in text output the
// condition is a sibling LINE of the node, so a naive "does the plan contain
// organizer_id" check passes when the condition belongs to a different node
// entirely — including a post-scan Filter, which is precisely the unscoped case.
func indexCondFor(t *testing.T, planJSON, index string) string {
	t.Helper()
	var doc []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(planJSON), &doc); err != nil {
		t.Fatalf("plan is not JSON: %v\n%s", err, planJSON)
	}
	if len(doc) == 0 {
		t.Fatalf("empty plan document:\n%s", planJSON)
	}
	var found string
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		if name, _ := node["Index Name"].(string); name == index {
			cond, _ := node["Index Cond"].(string)
			found = cond
			return
		}
		children, _ := node["Plans"].([]any)
		for _, c := range children {
			if child, ok := c.(map[string]any); ok {
				walk(child)
			}
		}
	}
	walk(doc[0].Plan)
	return found
}

// seedFeedVolume writes one voided lifecycle row per ticket directly. The chain
// is irrelevant to a query plan and the real path would take minutes at this
// volume; nothing in this test reads the chain.
func seedFeedVolume(t *testing.T, ctx context.Context, db *sql.DB, org uuid.UUID, n int) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tickets(id, order_id, guest_order_ref, organizer_id, buyer_id, slot_id, ticket_type_id, qr_payload, issued_at)
		SELECT gen_random_uuid(), gen_random_uuid(), gen_random_uuid(),
		       CASE WHEN g % 50 = 0 THEN $1::uuid ELSE gen_random_uuid() END,
		       gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'seed', now()
		  FROM generate_series(1, $2) AS g`, org, n); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO lifecycle_events(id, ticket_id, event_type, occurred_at)
		SELECT gen_random_uuid(), id, 'refunded', now() - (random() * interval '30 days')
		  FROM tickets WHERE qr_payload = 'seed'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ANALYZE tickets, lifecycle_events`); err != nil {
		t.Fatal(err)
	}
}

// explainVoidedFeedGenericPlan EXPLAINs the EXACT statement the store executes,
// under a forced generic plan.
//
// Two traps this closes, both from ADR-019. First, sending `EXPLAIN <query>`
// through the driver does not give a generic plan — the driver's prepared
// statement IS the EXPLAIN, so the inner query is planned with the value bound.
// It needs a server-side PREPARE and EXPLAIN EXECUTE. Second, a substituted
// parameter means the plan is custom after all and every assertion below it is
// vacuous, so every $n is checked for survival.
func explainVoidedFeedGenericPlan(t *testing.T, ctx context.Context, db *sql.DB, org uuid.UUID) string {
	t.Helper()
	stmt := "voided_feed_plan_probe_" + strconv.FormatInt(time.Now().UnixNano(), 10)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err = tx.ExecContext(ctx, fmt.Sprintf(
		`PREPARE %s(uuid, timestamptz, uuid, int) AS %s`, stmt, voidedFeedQuery)); err != nil {
		t.Fatal(err)
	}
	// Set before the first EXECUTE: the cached plan is built then.
	if _, err = tx.ExecContext(ctx, `SET LOCAL plan_cache_mode = force_generic_plan`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(
		`EXPLAIN (FORMAT JSON) EXECUTE %s('%s'::uuid, '9999-12-31 23:59:59Z'::timestamptz, '%s'::uuid, 100)`,
		stmt, org, uuid.Max))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	var plan strings.Builder
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}

	got := plan.String()
	// $1..$3 must survive. $4 is deliberately NOT checked: it is the LIMIT, and
	// Postgres folds a LIMIT parameter into the plan's cost estimate even under
	// force_generic_plan, so requiring it would fail against a perfectly generic
	// plan. The parameters that matter are the ones inside the predicates — those
	// are what a widened filter would let the planner see through, and $1 is the
	// tenant scope this whole assertion is about.
	for i := 1; i <= 3; i++ {
		marker := "$" + strconv.Itoa(i)
		if !strings.Contains(got, marker) {
			t.Fatalf("not a generic plan — %s was substituted, so plan_cache_mode did not apply and every index assertion here proves nothing.\nplan:\n%s", marker, got)
		}
	}
	return got
}

// The four-test set migration 0010 owes, matching what every migration in this
// service carries: statement order and banned keywords, the destination schema,
// representative volume against the fail-fast bound, and irreversibility.

func TestVoidedFeedMigrationStatementOrder(t *testing.T) {
	raw, err := fs.ReadFile(migrationsFS, "migrations/0010_voided_ticket_feed.sql")
	if err != nil {
		t.Fatalf("migration 0010 is missing: %v", err)
	}
	sql := string(raw)
	if !strings.Contains(sql, "CREATE INDEX "+feedOrganizerIndex+" ON tickets (organizer_id, id)") {
		t.Fatalf("migration 0010 does not create %s over (organizer_id, id) — the query, the index and the plan assertion must name the same thing", feedOrganizerIndex)
	}
	for _, banned := range []string{"CONCURRENTLY", "NOT VALID", "NO TRANSACTION"} {
		if strings.Contains(sql, banned) {
			t.Fatalf("migration 0010 contains %q (ADR-020/ADR-022 forbid it here)", banned)
		}
	}
	// An index migration has no business touching data or structure.
	for _, banned := range []string{"ALTER TABLE", "INSERT INTO", "UPDATE ", "DELETE FROM"} {
		if strings.Contains(sql, banned) {
			t.Fatalf("migration 0010 contains %q — it creates one index and nothing else", banned)
		}
	}
}

// The destination schema. Column ORDER is the assertion that matters: an index on
// (id, organizer_id) exists, is valid, and is useless to this query, because the
// tenant filter needs the leading column.
func TestVoidedFeedMigrationSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)

	var def string
	if err := db.QueryRowContext(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname = $1`, feedOrganizerIndex).Scan(&def); err != nil {
		t.Fatalf("%s is absent after migrating: %v", feedOrganizerIndex, err)
	}
	if !strings.Contains(def, "(organizer_id, id)") {
		t.Fatalf("%s = %q, want a leading organizer_id — a trailing one cannot serve the tenant filter", feedOrganizerIndex, def)
	}
	var valid bool
	if err := db.QueryRowContext(ctx,
		`SELECT i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relname = $1`,
		feedOrganizerIndex).Scan(&valid); err != nil {
		t.Fatal(err)
	}
	if !valid {
		t.Fatalf("%s exists but is not valid, so the planner will ignore it", feedOrganizerIndex)
	}
}

// The measured-migration obligation (ADR-008/ADR-022's 30-second bound). Opt-in
// like its siblings: seeding representative volume takes minutes and does not
// belong in every `make check`.
func TestVoidedFeedMigrationRepresentativeVolume(t *testing.T) {
	nStr := os.Getenv("ACCESS_FEED_MIGRATION_MEASUREMENT_TICKETS")
	if nStr == "" {
		t.Skip("ACCESS_FEED_MIGRATION_MEASUREMENT_TICKETS is not set")
	}
	n, err := strconv.Atoi(nStr)
	if err != nil || n <= 0 {
		t.Fatalf("ACCESS_FEED_MIGRATION_MEASUREMENT_TICKETS=%q is not a positive integer", nStr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.UpTo(ctx, 9); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tickets(id, order_id, guest_order_ref, organizer_id, buyer_id, slot_id, ticket_type_id, qr_payload, issued_at)
		SELECT gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(),
		       gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'volume', now()
		  FROM generate_series(1, $1) AS g`, n); err != nil {
		t.Fatal(err)
	}

	// A FRESH context bounded exactly as the one-shot migrate job is, so the
	// measurement is against the bound the job actually enforces.
	bounded, cancelBounded := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelBounded()
	start := time.Now()
	if _, err := provider.UpTo(bounded, 10); err != nil {
		t.Fatalf("migration 0010 did not complete inside the 30s fail-fast bound at %d tickets: %v", n, err)
	}
	elapsed := time.Since(start)

	var size string
	_ = db.QueryRowContext(ctx, `SELECT pg_size_pretty(pg_relation_size('tickets'))`).Scan(&size)
	var version string
	_ = db.QueryRowContext(ctx, `SHOW server_version`).Scan(&version)
	t.Logf("migration 0010: %v to index %d tickets (%s, postgres %s)", elapsed, n, size, version)
	if elapsed > 15*time.Second {
		t.Logf("WARNING: above the 15s engineering target — ship only with the reduced margin explicitly accepted")
	}
}

// Irreversibility, asserted against 0010 SPECIFICALLY.
//
// `UpTo(10)` then `Down()` rather than `Up()` then `Down()`, and the difference
// is not cosmetic: goose's Down rolls back exactly ONE migration, the current
// head. Every irreversibility test in this service used Up-to-head, so each was
// really asserting that whatever happened to be newest refused — three tests
// named for three migrations, all exercising the same one. Pinning the version
// is what makes this test about 0010.
func TestVoidedFeedMigrationIsIrreversible(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.UpTo(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(ctx); err == nil {
		t.Fatal("migration 0010 rolled back; the feed would keep answering correctly while scanning every organizer's tickets (ADR-019)")
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_class WHERE relname = $1`, feedOrganizerIndex).Scan(&n); err != nil || n != 1 {
		t.Fatalf("a failed down attempt altered the schema (n=%d err=%v)", n, err)
	}
}

// A walk down the feed terminates and does not lose rows that existed when it
// began — and a void created DURING the walk is deferred to the next pull.
//
// This test replaced one that claimed a snapshot and could not fail, and the
// history is worth keeping because it is the ticket's main lesson. Two review
// passes and three fixture attempts established what is actually true:
//
//   - The walk is strictly descending, so its cursor only moves backwards. A void
//     created during the walk is NEWER than the cursor and is excluded by the
//     keyset predicate alone.
//   - An explicit "ceiling" bound was added to make that guarantee look
//     deliberate. It was DEAD CODE: on a descending walk the cursor is always at
//     or below any ceiling taken from page one, so the keyset predicate is
//     strictly stronger and the ceiling can never change a result. No test could
//     make it fail, which is exactly how it was found — the test written to prove
//     it stayed green with the predicate deleted. It was removed rather than kept
//     with an unfalsifiable test beside it.
//   - What remains uncovered, and is ACCEPTED in ADR-066 §4b: `occurred_at`
//     defaults to Postgres `now()`, which is transaction start time, so a voiding
//     transaction that began before page one and commits after it can land at a
//     position the walk has already passed. No timestamp bound fixes that; it
//     needs commit ordering the table does not carry. The scanner's next pull is
//     the completeness mechanism.
func TestVoidedFeedWalkTerminatesAndDefersLateVoids(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := migratedDB(t, ctx)
	st := New(db, testConfig(t))
	org := uuid.New()

	before := make(map[uuid.UUID]bool, 4)
	for i := 0; i < 4; i++ {
		s := issueTicket(t, ctx, st, org)
		voidOneTicketOfItsOwnOrder(t, ctx, st, s)
		before[s.ticketID] = false
	}

	page1, cursor, err := st.VoidedTickets(ctx, org, VoidedCursor{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.IsZero() {
		t.Fatal("fixture cannot exercise a multi-page walk: the first page already finished it")
	}

	// Voided after the walk began. The keyset excludes these from THIS walk.
	late := make(map[uuid.UUID]bool, 2)
	for i := 0; i < 2; i++ {
		s := issueTicket(t, ctx, st, org)
		voidOneTicketOfItsOwnOrder(t, ctx, st, s)
		late[s.ticketID] = false
	}

	seen := make(map[uuid.UUID]bool, 6)
	for _, v := range page1 {
		seen[v.TicketID] = true
	}
	pages := 1
	for page := 0; page < 8; page++ {
		got, next, err := st.VoidedTickets(ctx, org, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, v := range got {
			seen[v.TicketID] = true
		}
		if next.IsZero() {
			break
		}
		cursor = next
	}

	for id := range before {
		if !seen[id] {
			t.Fatalf("ticket %s was voided BEFORE the walk started and never appeared in it", id)
		}
	}
	for id := range late {
		if seen[id] {
			t.Fatalf("ticket %s was voided AFTER the walk began but appeared in it — a descending walk must not chase rows added beneath it", id)
		}
	}
	// The walk TERMINATED in the pages its row count implies. Under continuous
	// voiding an unbounded walk would keep finding work; this is what says it does
	// not.
	if pages > 4 {
		t.Fatalf("the walk took %d pages for 4 rows — it is chasing rows added during the walk", pages)
	}
	// Deferred, not lost. This is the mechanism ADR-066 §4b relies on in place of
	// a snapshot, and it is the assertion that makes the accepted race tolerable.
	nextWalk, _, err := st.VoidedTickets(ctx, org, VoidedCursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	for id := range late {
		if !containsID(feedIDs(t, nextWalk), id) {
			t.Fatalf("ticket %s was excluded from the walk running when it was voided AND is absent from the next walk — it is lost, not deferred", id)
		}
	}
}
