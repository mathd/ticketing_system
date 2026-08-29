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
	opHold, groupRes := uuid.New(), uuid.New()
	convertChild, drawChild := uuid.New(), uuid.New()
	publicHold, resellerHold := uuid.New(), uuid.New()
	reseller := uuid.New()

	seed := func(id uuid.UUID, kind, key string, extra string, args ...any) {
		t.Helper()
		base := `INSERT INTO claims(id,organizer_id,pool_id,quantity,status,expires_at,idempotency_key,request_fingerprint,claim_kind` + extra
		if _, err := db.ExecContext(ctx, base, append([]any{id, org, slot, 1, key, "fp-" + key, kind}, args...)...); err != nil {
			t.Fatalf("seeding %s: %v", key, err)
		}
	}
	// operational hold: claim_kind='operational', needs purpose+label, no expiry.
	seed(opHold, "operational", "op-place:K1",
		`,operational_purpose,operational_label) VALUES($1,$2,$3,$4,'held',NULL,$5,$6,$7,'house','row A')`)
	// group reservation: claim_kind='reservation', needs counterparty and an expiry.
	seed(groupRes, "reservation", "grp-place:K2",
		`,reservation_counterparty) VALUES($1,$2,$3,$4,'held',now()+interval '1 hour',$5,$6,$7,'School Group')`)
	// convert + draw_down CHILDREN: claim_kind='buyer' BY DESIGN. This is the trap.
	seed(convertChild, "buyer", "convert:"+opHold.String()+":K3",
		`) VALUES($1,$2,$3,$4,'held',now()+interval '10 min',$5,$6,$7)`)
	seed(drawChild, "buyer", "grp-draw:"+groupRes.String()+":K4",
		`) VALUES($1,$2,$3,$4,'held',now()+interval '10 min',$5,$6,$7)`)
	// CONTROLS that must stay NULL: an ordinary public buyer hold, and a reseller one.
	seed(publicHold, "buyer", "PUBLIC-K5",
		`) VALUES($1,$2,$3,$4,'held',now()+interval '10 min',$5,$6,$7)`)
	if _, err = db.ExecContext(ctx, `INSERT INTO claims(id,organizer_id,pool_id,quantity,status,expires_at,idempotency_key,request_fingerprint,claim_kind,reseller_scope)
		VALUES($1,$2,$3,1,'held',now()+interval '10 min','RESELLER-K6','fp-r','buyer',$4)`, resellerHold, org, slot, reseller); err != nil {
		t.Fatal(err)
	}

	// claim_history is what the backfill classifies from. The children are reached
	// through related_claim_id, which is the whole reason claim_kind cannot be used.
	hist := func(claim uuid.UUID, related *uuid.UUID, action string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `INSERT INTO claim_history(id,organizer_id,claim_id,related_claim_id,action,actor,reason,quantity,quantity_after,status_after)
			VALUES($1,$2,$3,$4,$5,'ops@example.test','seed',1,1,'held')`,
			uuid.New(), org, claim, related, action); err != nil {
			t.Fatalf("seeding history %s: %v", action, err)
		}
	}
	hist(opHold, nil, "place")
	hist(groupRes, nil, "reserve")
	hist(opHold, &convertChild, "convert")
	hist(groupRes, &drawChild, "draw_down")
	hist(publicHold, nil, "create")

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
