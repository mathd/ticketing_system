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
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// schemaDB gives each test its own schema on the migration-test database, mirroring the
// access/catalog migration-upgrade harnesses: upgrades are proven against data seeded at
// the OLD version, which the always-fully-migrated store-test database cannot express.
func schemaDB(t *testing.T, ctx context.Context) (*sql.DB, *goose.Provider) {
	t.Helper()
	dsn := os.Getenv("COMMERCE_MIGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("COMMERCE_MIGRATION_TEST_DATABASE_URL is not set")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	schema := "commerce_migration_" + uuid.NewString()[:8]
	if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") })

	db, err := sql.Open("pgx", dsn+"?search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	migrations, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations)
	if err != nil {
		t.Fatal(err)
	}
	return db, provider
}

// hasFaceValue reports whether migration 0014 has been applied to this schema.
//
// seedV4Order is called from tests at TWO different schema versions — one stops
// at 0004, the other applies everything — so the insert has to fit both. The
// alternative was giving face_value_amount a column DEFAULT, and that was
// rejected: there is no honest default (0 violates the bounds CHECK for any
// non-zero total, and "the total" is not expressible as one), and a default
// would silently paper over a real insert site that forgot to state the face
// value, which is the exact bug the column exists to prevent.
func hasFaceValue(t *testing.T, ctx context.Context, db *sql.DB) bool {
	t.Helper()
	var present bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'reservations'
		  AND column_name = 'face_value_amount')`).Scan(&present); err != nil {
		t.Fatal(err)
	}
	return present
}

func faceValueColumns(t *testing.T, ctx context.Context, db *sql.DB) string {
	if hasFaceValue(t, ctx, db) {
		return ",face_value_amount"
	}
	return ""
}

// A fee-free seed, so the face value IS the total.
func faceValueValues(t *testing.T, ctx context.Context, db *sql.DB) string {
	if hasFaceValue(t, ctx, db) {
		return ",1000"
	}
	return ""
}

// seedV4Order inserts a reservation + order pair against the version-4 schema.
func seedV4Order(t *testing.T, ctx context.Context, db *sql.DB, status string) uuid.UUID {
	t.Helper()
	resID, orderID := uuid.New(), uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,currency,status`+faceValueColumns(t, ctx, db)+`)
		VALUES($1,$2,$3,$4,$5,$6,1,1000,1000,'EUR','finalizing'`+faceValueValues(t, ctx, db)+`)`,
		resID, uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint)
		VALUES($1,$2,$3,$4,'fp')`,
		orderID, resID, status, "mig-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	return orderID
}

// Migration 0005's backfill re-opens EXACTLY the two populations that were parked
// awaiting the capabilities TKT-115 ships — and nothing else. Rows parked for attempt
// exhaustion or manual reconciliation have different semantics and must stay parked.
func TestMigration0005BackfillReopensOnlyTheTKT56Populations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)

	if _, err := provider.UpTo(ctx, 4); err != nil {
		t.Fatalf("apply migrations through 0004: %v", err)
	}

	park := func(orderID uuid.UUID, reason string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `
			UPDATE orders SET status='reconciliation_required',recovery_parked_at=now(),
			    recovery_last_error=$2,recovery_attempts=7,
			    updated_at=now()-interval '10 minutes' WHERE id=$1`, orderID, reason); err != nil {
			t.Fatal(err)
		}
	}
	needsStatus := seedV4Order(t, ctx, db, "created")
	park(needsStatus, "payment result unknown; needs PSP status (TKT-56)")
	needsRefund := seedV4Order(t, ctx, db, "created")
	park(needsRefund, "captured payment whose claim is gone; needs void/refund (TKT-56)")
	exhausted := seedV4Order(t, ctx, db, "created")
	park(exhausted, "claim is confirmed but payment did not capture; needs manual reconciliation")
	unparked := seedV4Order(t, ctx, db, "created")

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply migration 0005: %v", err)
	}

	assertRow := func(orderID uuid.UUID, wantParked, wantErrRetained bool, wantAttempts int) {
		t.Helper()
		var parked *time.Time
		var attempts int
		var lastErr *string
		if err := db.QueryRowContext(ctx, `SELECT recovery_parked_at,recovery_attempts,recovery_last_error FROM orders WHERE id=$1`, orderID).Scan(&parked, &attempts, &lastErr); err != nil {
			t.Fatal(err)
		}
		if (parked != nil) != wantParked {
			t.Fatalf("order %s parked=%v, want parked=%v", orderID, parked != nil, wantParked)
		}
		if attempts != wantAttempts {
			t.Fatalf("order %s attempts=%d, want %d", orderID, attempts, wantAttempts)
		}
		if wantErrRetained && lastErr == nil {
			t.Fatalf("order %s: the backfill must retain recovery_last_error as operator context", orderID)
		}
	}
	assertRow(needsStatus, false, true, 0)
	assertRow(needsRefund, false, true, 0)
	assertRow(exhausted, true, true, 7)
	assertRow(unparked, false, false, 0)

	// The re-opened rows are immediately claimable through the recreated partial index.
	claimed, err := ClaimStuckOrders(ctx, db, 50, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	found := map[uuid.UUID]bool{}
	for _, c := range claimed {
		found[c.OrderID] = true
	}
	if !found[needsStatus] || !found[needsRefund] {
		t.Fatalf("re-opened TKT-56 rows must be claimable: needsStatus=%v needsRefund=%v", found[needsStatus], found[needsRefund])
	}
	if found[exhausted] {
		t.Fatal("a row parked for manual reconciliation must not be re-opened by the backfill")
	}
}

// 0005 widens the vocabulary CHECKs: no_side_effect joins terminal_outcome and refunded
// joins orders.status. Rejected values stay rejected.
func TestMigration0005VocabularyChecks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	orderID := seedV4Order(t, ctx, db, "created")
	if _, err := db.ExecContext(ctx, `UPDATE orders SET terminal_outcome='no_side_effect' WHERE id=$1`, orderID); err != nil {
		t.Fatalf("no_side_effect must be an accepted terminal_outcome after 0005: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE orders SET status='refunded' WHERE id=$1`, orderID); err != nil {
		t.Fatalf("refunded must be an accepted order status after 0005: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE orders SET terminal_outcome='captured' WHERE id=$1`, orderID); err == nil {
		t.Fatal("terminal_outcome must still reject values that do not prove absence of a side effect")
	}
}

// TKT-153: the reservation carries the provenance of the price it was created
// with, as a snapshot rather than a reference — so closing or superseding the
// rule later cannot rewrite what a buyer was charged.
func TestPriceResolutionSnapshotColumns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all migrations: %v", err)
	}

	res := uuid.New()
	seed := func(snapshot any) error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,
			                         quantity,unit_amount,total_amount,face_value_amount,currency,status,
			                         price_resolution_snapshot)
			VALUES($1,$2,$3,$4,$5,$6,1,900,900,900,'EUR','held',$7)
			ON CONFLICT(id) DO UPDATE SET price_resolution_snapshot = EXCLUDED.price_resolution_snapshot`,
			res, uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), snapshot)
		return err
	}

	// A legacy row — priced before this migration — stays NULL. Backfilling
	// 'no_eligible_rule' would fabricate a resolution that never happened.
	if err := seed(nil); err != nil {
		t.Fatalf("a NULL snapshot must be accepted (legacy and staff-path rows): %v", err)
	}

	winner := `{"resolver_version":2,"winner":{"rule_id":"` + uuid.New().String() +
		`","scope_level":"event"},"candidates":[]}`
	if err := seed([]byte(winner)); err != nil {
		t.Fatalf("a winner snapshot must be accepted: %v", err)
	}
	var (
		version int32
		ruleID  uuid.UUID
		scope   string
	)
	if err := db.QueryRowContext(ctx,
		`SELECT price_resolver_version, price_rule_id, price_rule_scope_level FROM reservations WHERE id=$1`,
		res).Scan(&version, &ruleID, &scope); err != nil {
		t.Fatalf("the generated trace projections must be readable without a JSON path: %v", err)
	}
	if version != 2 || scope != "event" || ruleID == uuid.Nil {
		t.Errorf("projections = %d/%v/%q, want them derived from the snapshot", version, ruleID, scope)
	}

	fallback := `{"resolver_version":2,"winner":null,"candidates":[],"fallback_reason":"no_eligible_rule"}`
	if err := seed([]byte(fallback)); err != nil {
		t.Fatalf("a fallback snapshot must be accepted: %v", err)
	}

	// The XOR the application enforces before persisting, enforced again here:
	// a document claiming both a winner and a fallback is incoherent, and a
	// snapshot is the one record that must stay interpretable years later.
	both := `{"resolver_version":2,"winner":{"rule_id":"` + uuid.New().String() +
		`","scope_level":"event"},"candidates":[],"fallback_reason":"no_eligible_rule"}`
	if err := seed([]byte(both)); err == nil {
		t.Error("a snapshot with BOTH a winner and a fallback_reason must be rejected")
	}
	neither := `{"resolver_version":2,"winner":null,"candidates":[]}`
	if err := seed([]byte(neither)); err == nil {
		t.Error("a snapshot with NEITHER a winner nor a fallback_reason must be rejected")
	}
	if err := seed([]byte(`[]`)); err == nil {
		t.Error("a non-object snapshot must be rejected")
	}
}

// 0012's Down must refuse to run once a cancellation refund run exists. Silently dropping
// the record of who was repaid on a cancelled event is worse than refusing to roll back —
// it is the only place that record lives per-order.
func TestMigration0012DownRefusesToDestroyCancellationHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}

	org, run := uuid.New(), uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO cancellation_refund_runs(organizer_id,id,slot_id,idempotency_key,request_fingerprint,actor,reason,cutoff_at)
		VALUES($1,$2,$3,'k','fp','ops@example.test','event cancelled',now())`,
		org, run, uuid.New()); err != nil {
		t.Fatal(err)
	}
	// Unwind everything above 0012 first. provider.Down() rolls back exactly ONE
	// migration, so this assertion silently stopped testing 0012 the moment 0013
	// landed on top of it (TKT-173) — it rolled back the new migration, saw that
	// succeed, and reported a missing guard. DownTo pins the target explicitly so the
	// next migration cannot move this test's aim again.
	if _, err := provider.DownTo(ctx, 12); err != nil {
		t.Fatalf("unwind to 0012: %v", err)
	}
	if _, err := provider.Down(ctx); err == nil {
		t.Fatal("0012 rolled back over an existing cancellation refund run — the guard is missing")
	}

	// And with the history cleared it rolls back cleanly, so the guard is a guard and not
	// a permanently broken Down.
	if _, err := db.ExecContext(ctx, `DELETE FROM cancellation_refund_runs`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("0012 down on an empty history: %v", err)
	}
}

// TKT-215 / migration 0014. Two columns, and the second one — face_value_amount —
// exists because of a defect found at plan review: once total_amount became the
// GROSS charge, the exchange delta compared it against a price-only target and
// refunded the service fee on an EVEN exchange.
func TestMigration0014FeeCompositionColumns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all migrations: %v", err)
	}

	insert := func(id uuid.UUID, total, face int64, snapshot any) error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,
			                         quantity,unit_amount,total_amount,face_value_amount,currency,status,
			                         fee_resolution_snapshot)
			VALUES($1,$2,$3,$4,$5,$6,1,900,$7,$8,'EUR','held',$9)
			ON CONFLICT(id) DO UPDATE SET fee_resolution_snapshot = EXCLUDED.fee_resolution_snapshot`,
			id, uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), total, face, snapshot)
		return err
	}

	// A fee-free sale: the two numbers agree, and the snapshot is NULL — the same
	// state every pre-migration row is in.
	if err := insert(uuid.New(), 900, 900, nil); err != nil {
		t.Fatalf("a fee-free reservation must be accepted: %v", err)
	}
	// A sale carrying a passed-on fee: gross above face.
	env := `{"resolution":{"resolver_version":1,"fees":[]},"breakdown":[],` +
		`"face_value":900,"passed_on_fees":300,"absorbed_fees":0,"total_amount":1200}`
	if err := insert(uuid.New(), 1200, 900, []byte(env)); err != nil {
		t.Fatalf("a fee-carrying reservation must be accepted: %v", err)
	}

	// face > total is unrepresentable: total = face + passed_on and passed_on is
	// never negative, so a row claiming otherwise is corrupt.
	if err := insert(uuid.New(), 900, 1200, nil); err == nil {
		t.Error("a face value above the total must be rejected")
	}

	// The envelope must AGREE with the columns it explains. A provenance
	// document that contradicts the row is worse than none — it is a record that
	// lies, and TKT-217 settles real money from it.
	for name, bad := range map[string]string{
		"a face value disagreeing with the column": `{"resolution":{},"breakdown":[],"face_value":111,"passed_on_fees":300,"total_amount":1200}`,
		"a total disagreeing with the column":      `{"resolution":{},"breakdown":[],"face_value":900,"passed_on_fees":300,"total_amount":999}`,
		"no resolution document":                   `{"breakdown":[],"face_value":900,"passed_on_fees":300,"total_amount":1200}`,
		"a breakdown that is not an array":         `{"resolution":{},"breakdown":{},"face_value":900,"passed_on_fees":300,"total_amount":1200}`,
		"no totals at all":                         `{"resolution":{},"breakdown":[]}`,
		// ai-review [medium]: presence was checked, agreement was not. A snapshot
		// claiming fees the columns do not show is a provenance document that
		// lies, and TKT-217 settles money from it.
		"passed-on fees that contradict the columns": `{"resolution":{},"breakdown":[],"face_value":900,"passed_on_fees":999,"total_amount":1200}`,
		"a non-numeric total":                        `{"resolution":{},"breakdown":[],"face_value":900,"passed_on_fees":"garbage","total_amount":1200}`,
		"a negative fee total":                       `{"resolution":{},"breakdown":[],"face_value":1500,"passed_on_fees":-300,"total_amount":1200}`,
		"a null where a number belongs":              `{"resolution":{},"breakdown":[],"face_value":null,"passed_on_fees":300,"total_amount":1200}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := insert(uuid.New(), 1200, 900, []byte(bad)); err == nil {
				t.Error("the shape CHECK must reject this envelope")
			}
		})
	}
}

// The backfill is exact rather than approximate, and that is the whole argument
// for it: before 0014 no reservation carries a fee, so face = total is TRUE for
// every existing row and exchange behaviour is UNCHANGED rather than newly
// corrected. This test seeds a row under the old schema and proves it.
func TestMigration0014BackfillsFaceValueFromTheTotal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	// Stop BELOW 0014 so the row is written by the pre-fee schema, exactly as a
	// production row would have been.
	if _, err := provider.UpTo(ctx, 13); err != nil {
		t.Fatalf("apply migrations up to 0013: %v", err)
	}
	legacy := uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,
		                         quantity,unit_amount,total_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,3,900,2700,'EUR','held')`,
		legacy, uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply 0014: %v", err)
	}
	var face, total int64
	if err := db.QueryRowContext(ctx,
		`SELECT face_value_amount, total_amount FROM reservations WHERE id=$1`, legacy).
		Scan(&face, &total); err != nil {
		t.Fatal(err)
	}
	if face != 2700 || total != 2700 {
		t.Errorf("face=%d total=%d, want both 2700 — a pre-fee row's total IS its face value, "+
			"so the backfill is exact and the exchange delta it feeds is unchanged", face, total)
	}
}

// The Down guard, on both halves. The snapshot half is the obvious one; the
// face-value half is the one that matters, because dropping that column once a
// fee has been charged silently re-breaks the exchange delta.
func TestMigration0014DownRefusesToDestroyFeeComposition(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	res := uuid.New()
	env := `{"resolution":{"fees":[]},"breakdown":[],"face_value":900,"passed_on_fees":300,` +
		`"absorbed_fees":0,"total_amount":1200}`
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,
		                         quantity,unit_amount,total_amount,face_value_amount,currency,status,
		                         fee_resolution_snapshot)
		VALUES($1,$2,$3,$4,$5,$6,1,900,1200,900,'EUR','held',$7)`,
		res, uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), []byte(env)); err != nil {
		t.Fatal(err)
	}
	// Unwind everything above 0014 first, for exactly the reason recorded on the
	// 0012 test above: provider.Down() rolls back ONE migration, so this assertion
	// stopped testing 0014 the moment 0015 landed on top of it (TKT-220) — it
	// rolled back the new migration, saw that succeed, and reported a missing
	// guard on a guard that was still there. Second occurrence of the same defect;
	// DownTo pins the aim so the next migration cannot move it again.
	if _, err := provider.DownTo(ctx, 14); err != nil {
		t.Fatalf("unwind to 0014: %v", err)
	}
	if _, err := provider.Down(ctx); err == nil {
		t.Fatal("0014 rolled back over a stored fee snapshot — the guard is missing")
	}

	// Clearing the snapshot is not enough while the face value still differs from
	// the total: that row's exchange delta depends on the column.
	if _, err := db.ExecContext(ctx, `UPDATE reservations SET fee_resolution_snapshot = NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(ctx); err == nil {
		t.Fatal("0014 rolled back over a reservation whose face value differs from its total — " +
			"dropping the column there silently re-breaks the exchange delta")
	}

	// With both cleared it rolls back cleanly, so the guard is a guard rather
	// than a permanently broken Down.
	if _, err := db.ExecContext(ctx, `UPDATE reservations SET total_amount = face_value_amount`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("0014 down on fee-free data: %v", err)
	}
}

// Migration 0023's Down refuses while operator unpark evidence exists (TKT-146).
//
// The evidence is the only commerce-local record that a human ever intervened in recovery,
// and there is no honest way to translate it into the pre-0023 schema — so the Down fails
// loudly rather than dropping it, the way 0005 refuses its own durable recovery evidence and
// 0012 refuses cancellation refund runs.
//
// DownTo(22) then Down(), NOT DownTo(23): provider.Down rolls back exactly one migration, so
// aiming at 23 would work only while 0023 happens to be last. That is precisely the drift
// TKT-173 recorded when the 0012 test silently stopped testing 0012.
func TestMigration0023DownRefusesToDestroyUnparkEvidence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all migrations: %v", err)
	}
	orderID := seedV4Order(t, ctx, db, "release_pending")
	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_recovery_unparks
		  (id,order_id,reason,pre_recovery_attempts,pre_recovery_parked_at,pre_recovery_last_error)
		VALUES($1,$2,'psp restored; re-driving',10,now(),'psp unreachable')`,
		uuid.New(), orderID); err != nil {
		t.Fatal(err)
	}

	if _, err := provider.DownTo(ctx, 22); err == nil {
		t.Fatal("0023 rolled back over existing operator unpark evidence — the guard is missing")
	}

	// And with the evidence cleared it rolls back cleanly, so the guard is a guard rather
	// than a permanently broken Down.
	if _, err := db.ExecContext(ctx, `DELETE FROM order_recovery_unparks`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 22); err != nil {
		t.Fatalf("0023 down on an empty history: %v", err)
	}
}

// The database-tier constraints on the unpark evidence table are live (TKT-146).
//
// pre_recovery_parked_at NOT NULL is the one that matters most and is easiest to mistake for
// incidental nullability housekeeping: it is a second enforcement of the store guard's "is it
// parked?" predicate, and it is what makes "an unpark row proves the order was parked" true at
// the schema level rather than by Go convention. A blank reason is refused for the same kind of
// reason — the reason is the only part of the record a later reader cannot reconstruct.
func TestMigration0023ConstrainsUnparkEvidence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all migrations: %v", err)
	}
	orderID := seedV4Order(t, ctx, db, "release_pending")

	insert := func(reason string, attempts int, parkedAt any) error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO order_recovery_unparks
			  (id,order_id,reason,pre_recovery_attempts,pre_recovery_parked_at)
			VALUES($1,$2,$3,$4,$5)`, uuid.New(), orderID, reason, attempts, parkedAt)
		return err
	}
	if err := insert("   ", 10, time.Now()); err == nil {
		t.Fatal("a whitespace-only reason was accepted; the evidence row would look complete and say nothing")
	}
	if err := insert("", 10, time.Now()); err == nil {
		t.Fatal("an empty reason was accepted")
	}
	if err := insert("psp restored", -1, time.Now()); err == nil {
		t.Fatal("a negative pre_recovery_attempts was accepted")
	}
	if err := insert("psp restored", 10, nil); err == nil {
		t.Fatal("a NULL pre_recovery_parked_at was accepted; an unpark row must prove the order was parked")
	}
	// The control: the same insert with every value valid succeeds, so the four refusals
	// above are the constraints firing and not a broken statement.
	if err := insert("psp restored; re-driving", 10, time.Now()); err != nil {
		t.Fatalf("a valid unpark evidence row was refused: %v", err)
	}
	// A row must name a real order.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_recovery_unparks
		  (id,order_id,reason,pre_recovery_attempts,pre_recovery_parked_at)
		VALUES($1,$2,'orphan',1,now())`, uuid.New(), uuid.New()); err == nil {
		t.Fatal("unpark evidence was accepted for an order that does not exist")
	}
}

// 0024's Down refuses to destroy operator unwind evidence (TKT-255).
//
// DownTo(23), not DownTo(24) — N−1, the same correction TKT-173 and TKT-220 each had to make
// on this file. `DownTo(N)` migrates down TO version N, which leaves N applied and tests
// nothing about it.
//
// This evidence matters more than the unpark table's, and the guard is the same shape for a
// stronger reason: an unpark row describes an order that still exists and whose state can be
// re-read, but an unwind row describes an `order_exchanges` row that has been DELETED. It is
// the only account of it anywhere in commerce. Rolling the table away does not lose a
// duplicate — it loses the record entirely.
func TestMigration0024DownRefusesToDestroyUnwindEvidence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all migrations: %v", err)
	}
	orderID := seedV4Order(t, ctx, db, "completed")
	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_exchange_unwinds
		  (id,organizer_id,exchange_id,source_order_id,reason,idempotency_key,actor,
		   pre_source_total,currency,pre_basis_recorded)
		VALUES($1,$2,$3,$4,'target claim released; order stuck','buyer-key','support@example.test',
		       2000,'EUR',false)`,
		uuid.New(), uuid.New(), uuid.New(), orderID); err != nil {
		t.Fatal(err)
	}

	if _, err := provider.DownTo(ctx, 23); err == nil {
		t.Fatal("0024 rolled back over existing operator unwind evidence — the guard is missing. " +
			"The exchange rows these describe are gone, so this table is the only record that " +
			"anyone ever abandoned them")
	}

	// And with the evidence cleared it rolls back cleanly, so the guard is a guard rather
	// than a permanently broken Down. Without this half, a Down that always raised would
	// pass the assertion above and break every future rollback.
	if _, err := db.ExecContext(ctx, `DELETE FROM order_exchange_unwinds`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 23); err != nil {
		t.Fatalf("0024 down on an empty history: %v", err)
	}
}

// Migration 0026 (TKT-268): the backfill derives source_organizer_id from the SOURCE
// RESERVATION, and rows seeded at 0025 are upgraded correctly.
//
// The upgrade is the half a fully-migrated database cannot express: the store-test database
// is always at head, so a backfill bug there is invisible. Seeding at 0025 and stepping over
// 0026 is the only way to see it.
//
// The malformed row matters most. A backfill that copied the queue row's own organizer_id
// would produce source_organizer_id = organizer_id for EVERY row, which reads as success and
// makes the claim predicate true by construction — a precondition that cannot fail. So this
// asserts the mismatch SURVIVES the backfill.
func TestMigration0026BackfillsFromTheSourceReservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.UpTo(ctx, 25); err != nil {
		t.Fatalf("apply migrations through 0025: %v", err)
	}

	sourceOrg := uuid.New()
	order := seedV4OrderForOrganizer(t, ctx, db, sourceOrg)

	// Two refunds on that order: one whose queue organizer agrees with the source, one whose
	// does not. Only the second can tell a source-derived backfill from a self-copy.
	agreeing, malformed := uuid.New(), uuid.New()
	otherOrg := uuid.New()
	for _, r := range []struct {
		id  uuid.UUID
		org uuid.UUID
	}{{agreeing, sourceOrg}, {malformed, otherOrg}} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO order_refunds(id,order_id,organizer_id,idempotency_key,request_fingerprint,
			                          quantity,unit_amount,amount,currency,actor,reason,status,
			                          completed_at,payment_fact_id)
			VALUES($1,$2,$3,$4,'fp',1,100,100,'EUR','ops@example.test','backfill test','completed',now(),$5)`,
			r.id, order, r.org, "mk-"+r.id.String(), uuid.New()); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := provider.UpTo(ctx, 26); err != nil {
		t.Fatalf("apply 0026: %v", err)
	}

	var src, own uuid.UUID
	read := func(id uuid.UUID) {
		t.Helper()
		if err := db.QueryRowContext(ctx,
			`SELECT source_organizer_id, organizer_id FROM order_refunds WHERE id=$1`, id).
			Scan(&src, &own); err != nil {
			t.Fatal(err)
		}
	}

	read(agreeing)
	if src != sourceOrg {
		t.Errorf("the agreeing row's source_organizer_id is %s, want the source reservation's %s", src, sourceOrg)
	}

	read(malformed)
	if src != sourceOrg {
		t.Errorf("the malformed row's source_organizer_id is %s, want the SOURCE reservation's %s. "+
			"A backfill that copies the queue row's own organizer makes every row agree with "+
			"itself, so the claim predicate can never refuse anything", src, sourceOrg)
	}
	if src == own {
		t.Error("the malformed row's source and queue organizers agree after the backfill, so the " +
			"mismatch this fixture created did not survive it and nothing here can fail")
	}

	// The columns are NOT NULL on both queue tables.
	for _, table := range []string{"order_refunds", "order_exchanges"} {
		var nullable string
		if err := db.QueryRowContext(ctx, `
			SELECT is_nullable FROM information_schema.columns
			WHERE table_name=$1 AND column_name='source_organizer_id'`, table).Scan(&nullable); err != nil {
			t.Fatal(err)
		}
		if nullable != "NO" {
			t.Errorf("%s.source_organizer_id is nullable; a NULL is a row the queue index silently drops", table)
		}
	}

	// The equality reached the partial index predicates, which is the half that makes the
	// scan bounded. Asserting the column exists proves nothing on its own.
	for _, idx := range []string{"order_refunds_reversal_queue_idx", "order_exchanges_reversal_queue_idx"} {
		var def string
		if err := db.QueryRowContext(ctx,
			`SELECT pg_get_indexdef(oid) FROM pg_class WHERE relname=$1`, idx).Scan(&def); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(def, "source_organizer_id = organizer_id") {
			t.Errorf("%s does not carry the source-organizer equality, so malformed rows are still "+
				"IN the index and the claim reads and discards them:\n%s", idx, def)
		}
	}
}

// The trigger OVERWRITES a submitted source_organizer_id (TKT-268).
//
// Without this the column would be caller-supplied, and a writer could hand the queue a value
// matching its own organizer, satisfying the claim predicate while the real source belongs to
// someone else. That is the defect the predicate exists to catch, so the derivation has to be
// the database's rather than the caller's.
//
// ADR-021: this constrains writers who go THROUGH the trigger. Anyone with commerce database
// access can drop it. Honest-writer consistency, not tamper-evidence.
func TestMigration0026TriggerOverwritesASubmittedSourceOrganizer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all migrations: %v", err)
	}

	sourceOrg := uuid.New()
	order := seedV4OrderForOrganizer(t, ctx, db, sourceOrg)
	liar := uuid.New()

	// The writer claims the source is its own organizer. Both columns agree in the INSERT.
	id := uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_refunds(id,order_id,organizer_id,source_organizer_id,idempotency_key,
		                          request_fingerprint,quantity,unit_amount,amount,currency,actor,
		                          reason,status,completed_at,payment_fact_id)
		VALUES($1,$2,$3,$3,'k','fp',1,100,100,'EUR','ops@example.test','trigger test','completed',now(),$4)`,
		id, order, liar, uuid.New()); err != nil {
		t.Fatal(err)
	}

	var src uuid.UUID
	if err := db.QueryRowContext(ctx,
		`SELECT source_organizer_id FROM order_refunds WHERE id=$1`, id).Scan(&src); err != nil {
		t.Fatal(err)
	}
	if src != sourceOrg {
		t.Fatalf("source_organizer_id is %s — the submitted value was trusted. It must be %s, "+
			"derived from the source reservation, or a writer can make the claim predicate "+
			"pass on a row whose source belongs to another organizer", src, sourceOrg)
	}
}

// Moving a parent identity link moves the derived value with it (TKT-268).
//
// Production does not rewrite orders.reservation_id or reservations.organizer_id today.
// Without this half the guarantee would rest on that convention holding forever, and the
// mismatch would be reachable from the parent side while every queue-row test stayed green.
func TestMigration0026ParentIdentityChangesMoveTheDerivedOrganizer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all migrations: %v", err)
	}

	sourceOrg := uuid.New()
	order := seedV4OrderForOrganizer(t, ctx, db, sourceOrg)
	id := uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_refunds(id,order_id,organizer_id,idempotency_key,request_fingerprint,
		                          quantity,unit_amount,amount,currency,actor,reason,status,
		                          completed_at,payment_fact_id)
		VALUES($1,$2,$3,'k','fp',1,100,100,'EUR','ops@example.test','reseat test','completed',now(),$4)`,
		id, order, sourceOrg, uuid.New()); err != nil {
		t.Fatal(err)
	}

	// The reservation changes hands.
	moved := uuid.New()
	if _, err := db.ExecContext(ctx, `
		UPDATE reservations SET organizer_id=$1
		WHERE id=(SELECT reservation_id FROM orders WHERE id=$2)`, moved, order); err != nil {
		t.Fatal(err)
	}

	var src uuid.UUID
	if err := db.QueryRowContext(ctx,
		`SELECT source_organizer_id FROM order_refunds WHERE id=$1`, id).Scan(&src); err != nil {
		t.Fatal(err)
	}
	if src != moved {
		t.Fatalf("source_organizer_id is still %s after the reservation moved to %s. The queue row's "+
			"derived value is stale, so a mismatch is reachable from the parent side and the "+
			"claim predicate would keep passing it", src, moved)
	}
}

// 0026's Down fails closed while a malformed row exists, and rolls back cleanly otherwise.
//
// BOTH directions. A refusal-only test is satisfied by an unconditional RAISE, which would
// make the migration permanently irreversible rather than conditionally so.
func TestMigration0026DownRefusesOverAMalformedQueueRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all migrations: %v", err)
	}

	sourceOrg := uuid.New()
	order := seedV4OrderForOrganizer(t, ctx, db, sourceOrg)
	id := uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO order_refunds(id,order_id,organizer_id,idempotency_key,request_fingerprint,
		                          quantity,unit_amount,amount,currency,actor,reason,status,
		                          completed_at,payment_fact_id)
		VALUES($1,$2,$3,'k','fp',1,100,100,'EUR','ops@example.test','down test','completed',now(),$4)`,
		id, order, sourceOrg, uuid.New()); err != nil {
		t.Fatal(err)
	}
	// Make it malformed: the queue organizer leaves its source behind.
	if _, err := db.ExecContext(ctx,
		`UPDATE order_refunds SET organizer_id=$1 WHERE id=$2`, uuid.New(), id); err != nil {
		t.Fatal(err)
	}

	// DownTo pins the aim: provider.Down() rolls back exactly one migration, so without this
	// the test would silently retarget the moment 0027 lands (TKT-173's lesson, same file).
	if _, err := provider.DownTo(ctx, 25); err == nil {
		t.Fatal("0026 rolled back while a malformed queue row exists. Rolling back restores the " +
			"correlated EXISTS, whose cost is linear in exactly those rows — the guard is missing")
	}

	// Repaired, it rolls back cleanly, so the guard is a guard rather than a broken Down.
	if _, err := db.ExecContext(ctx,
		`UPDATE order_refunds SET organizer_id=source_organizer_id WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 25); err != nil {
		t.Fatalf("0026 down on a clean queue: %v", err)
	}
}

// seedV4OrderForOrganizer is seedV4Order with a caller-chosen organizer, which is what the
// 0026 tests need: the whole point is that the SOURCE reservation's organizer differs from
// the queue row's.
func seedV4OrderForOrganizer(t *testing.T, ctx context.Context, db *sql.DB, org uuid.UUID) uuid.UUID {
	t.Helper()
	resID, orderID := uuid.New(), uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,currency,status`+faceValueColumns(t, ctx, db)+`)
		VALUES($1,$2,$3,$4,$5,$6,1,1000,1000,'EUR','completed'`+faceValueValues(t, ctx, db)+`)`,
		resID, org, uuid.New(), uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint)
		VALUES($1,$2,'completed',$3,'fp')`,
		orderID, resID, "mig-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	return orderID
}
