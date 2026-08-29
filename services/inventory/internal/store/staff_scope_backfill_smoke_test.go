//go:build smoke

package store

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

// Migration 0019's BACKFILL, exercised against rows written by the OLD writers.
//
// This test exists because the namespace tests could not see the backfill at all
// (ai-review finding, TKT-296). They create their claims AFTER migrating to head,
// so the new writers set staff_scope=true directly and both UPDATE statements in
// 0019 could be DELETED with every one of those tests still green — while every
// historical staff row stayed public-scoped and the collision this ticket closes
// stayed open for existing data. A fixture that cannot reach the failing state.
//
// So this one stops at 0018, seeds the exact shapes the four pre-fix writers
// produced (prefixed key, no staff_scope column yet, claim_history recording the
// action), then applies 0019 and asserts what each row became.
//
// MUTATION, per branch, each caught ONLY here:
//   - delete the place/reserve UPDATE  -> the operational hold and the group
//     reservation stay NULL; their assertions go red.
//   - delete the convert/draw_down UPDATE -> the two BUYER-KIND children stay
//     NULL; their assertions go red. This is the branch a claim_kind-based
//     backfill would have got wrong, which is why the children are seeded with
//     claim_kind='buyer' exactly as the real writers do.
//   - change either UPDATE to key on claim_kind -> the children go red while the
//     first two stay green, which is the failure this test exists to expose.
func TestMigration0019BackfillsHistoricalStaffClaims(t *testing.T) {
	dsn := os.Getenv("INVENTORY_MIGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("INVENTORY_MIGRATION_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	schema := "inv_backfill_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") })

	db, err := sql.Open("pgx", dsn+"?search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, sub)
	if err != nil {
		t.Fatal(err)
	}

	// Stop BEFORE the migration under test. Seeding after it would defeat the point.
	if _, err = provider.UpTo(ctx, 18); err != nil {
		t.Fatalf("migrating to 0018: %v", err)
	}
	var hasColumn bool
	if err = db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM information_schema.columns
		WHERE table_schema=current_schema() AND table_name='claims' AND column_name='staff_scope')`).Scan(&hasColumn); err != nil {
		t.Fatal(err)
	}
	if hasColumn {
		t.Fatal("staff_scope already exists at 0018 — this test is not seeding a pre-migration schema")
	}

	org, slot := uuid.New(), uuid.New()
	if _, err = db.ExecContext(ctx, `INSERT INTO inventory_pools(slot_id,organizer_id,capacity,confirmed_quantity,source_event_id)
		VALUES($1,$2,50,0,$3)`, slot, org, uuid.New()); err != nil {
		t.Fatal(err)
	}

	// The four pre-fix staff shapes, written exactly as the old code wrote them:
	// a DECORATED key, and claim_history carrying the action that identifies them.
	// Two SOURCES and two CHILDREN, seeded as the real writers would leave them.
	//
	// FIDELITY MATTERS HERE and a first version of this test got it wrong (ai-review
	// pass 2): it left both sources 'held' with quantity_after 1 after a full convert,
	// and gave the children no ticket_type_id/unit_amount/currency. Those states are
	// unreachable -- ConvertOperational sets the source 'released' with remaining 0
	// when qty == c.Quantity (operational.go), and both child writers always populate
	// the money columns. A backfill wrongly narrowed to status='held' or to
	// ticket_type_id IS NULL would have passed against impossible rows while missing
	// every real released claim.
	//
	// So: opHold is FULLY converted (released, remaining 0) and groupRes is PARTIALLY
	// drawn down (still held, quantity decremented), covering both branches.
	opHold, groupRes := uuid.New(), uuid.New()
	convertChild, drawChild := uuid.New(), uuid.New()
	publicHold, resellerHold := uuid.New(), uuid.New()
	reseller, ticketType := uuid.New(), uuid.New()

	// Source 1: an operational hold, fully converted -> released, quantity 0.
	if _, err = db.ExecContext(ctx, `INSERT INTO claims(id,organizer_id,pool_id,quantity,status,expires_at,idempotency_key,request_fingerprint,claim_kind,operational_purpose,operational_label)
		VALUES($1,$2,$3,2,'released',NULL,'op-place:K1','fp-k1','operational','house','row A')`, opHold, org, slot); err != nil {
		t.Fatal(err)
	}
	// Source 2: a group reservation, partially drawn down -> still held, decremented.
	if _, err = db.ExecContext(ctx, `INSERT INTO claims(id,organizer_id,pool_id,quantity,status,expires_at,idempotency_key,request_fingerprint,claim_kind,reservation_counterparty)
		VALUES($1,$2,$3,8,'held',now()+interval '1 hour','grp-place:K2','fp-k2','reservation','School Group')`, groupRes, org, slot); err != nil {
		t.Fatal(err)
	}
	// The two CHILDREN: claim_kind='buyer' by design, and fully shaped -- the money
	// columns the real writers always populate.
	for _, c := range []struct {
		id  uuid.UUID
		key string
	}{
		{convertChild, "convert:" + opHold.String() + ":K3"},
		{drawChild, "grp-draw:" + groupRes.String() + ":K4"},
	} {
		if _, err = db.ExecContext(ctx, `INSERT INTO claims(id,organizer_id,pool_id,ticket_type_id,quantity,unit_amount,currency,status,expires_at,idempotency_key,request_fingerprint,claim_kind)
			VALUES($1,$2,$3,$4,2,4500,'EUR','held',now()+interval '10 min',$5,$6,'buyer')`,
			c.id, org, slot, ticketType, c.key, "fp-"+c.key); err != nil {
			t.Fatalf("seeding child %s: %v", c.key, err)
		}
	}
	// CONTROLS that must stay NULL: an ordinary public buyer hold, and a reseller one.
	if _, err = db.ExecContext(ctx, `INSERT INTO claims(id,organizer_id,pool_id,ticket_type_id,quantity,unit_amount,currency,status,expires_at,idempotency_key,request_fingerprint,claim_kind)
		VALUES($1,$2,$3,$4,1,4500,'EUR','held',now()+interval '10 min','PUBLIC-K5','fp-k5','buyer')`, publicHold, org, slot, ticketType); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO claims(id,organizer_id,pool_id,ticket_type_id,quantity,unit_amount,currency,status,expires_at,idempotency_key,request_fingerprint,claim_kind,reseller_scope)
		VALUES($1,$2,$3,$4,1,4500,'EUR','held',now()+interval '10 min','RESELLER-K6','fp-k6','buyer',$5)`, resellerHold, org, slot, ticketType, reseller); err != nil {
		t.Fatal(err)
	}

	// claim_history is what the backfill classifies from, and the quantity_after /
	// status_after values match what each writer actually records.
	hist := func(claim uuid.UUID, related *uuid.UUID, action string, qty, after int, status string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `INSERT INTO claim_history(id,organizer_id,claim_id,related_claim_id,action,actor,reason,quantity,quantity_after,status_after)
			VALUES($1,$2,$3,$4,$5,'ops@example.test','seed',$6,$7,$8)`,
			uuid.New(), org, claim, related, action, qty, after, status); err != nil {
			t.Fatalf("seeding history %s: %v", action, err)
		}
	}
	hist(opHold, nil, "place", 2, 2, "held")
	hist(groupRes, nil, "reserve", 8, 8, "held")
	// Full convert: source released, nothing remaining.
	hist(opHold, &convertChild, "convert", 2, 0, "released")
	// Partial draw-down: source still held, decremented.
	hist(groupRes, &drawChild, "draw_down", 2, 6, "held")
	hist(publicHold, nil, "create", 1, 1, "held")

	// Now the migration under test.
	if _, err = provider.UpTo(ctx, 19); err != nil {
		t.Fatalf("applying 0019: %v", err)
	}

	scope := func(id uuid.UUID) *bool {
		t.Helper()
		var b *bool
		if err := db.QueryRowContext(ctx, `SELECT staff_scope FROM claims WHERE id=$1`, id).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	staff := func(name string, id uuid.UUID) {
		t.Helper()
		got := scope(id)
		if got == nil || !*got {
			t.Errorf("%s was NOT backfilled to staff_scope=true (got %v).\n"+
				"Its historical row stays in the public namespace, so the collision "+
				"TKT-296 closes is still open for every claim written before 0019.", name, got)
		}
	}
	public := func(name string, id uuid.UUID) {
		t.Helper()
		if got := scope(id); got != nil {
			t.Errorf("%s was wrongly marked staff_scope=%v — the backfill is over-broad "+
				"and has moved a non-staff row out of the public namespace", name, *got)
		}
	}

	staff("the operational hold (action=place)", opHold)
	staff("the group reservation (action=reserve)", groupRes)
	staff("the CONVERT child, claim_kind='buyer' (action=convert, via related_claim_id)", convertChild)
	staff("the DRAW_DOWN child, claim_kind='buyer' (action=draw_down, via related_claim_id)", drawChild)
	public("the ordinary public buyer hold", publicHold)
	public("the reseller-scoped hold", resellerHold)

	// The backfill sets SCOPE only. Rewriting a historical key would change which
	// row an in-flight retry finds, which is the one thing this migration must not do.
	var storedKey string
	if err = db.QueryRowContext(ctx, `SELECT idempotency_key FROM claims WHERE id=$1`, opHold).Scan(&storedKey); err != nil {
		t.Fatal(err)
	}
	if storedKey != "op-place:K1" {
		t.Errorf("the migration REWROTE a historical key to %q; it must set scope only", storedKey)
	}
}
