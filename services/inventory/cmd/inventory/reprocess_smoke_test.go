//go:build smoke

package main

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/inventory/internal/store"
	"ticketing/shared/runtimecfg"
)

// TKT-326 at the tier an operator uses: the PRODUCTION command registry, the real environment
// variables and a real migrated schema, with NATS_URL unset. The command must apply a
// quarantined row in-process, with no broker at all, and leave every row it cannot apply pending.
// Every row is a handwritten JSON literal, never built from the Go struct under test (ADR-017).
func reprocessSchema(t *testing.T) (context.Context, *sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("INVENTORY_MIGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("INVENTORY_MIGRATION_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	schema := "inventory_rq_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") })
	scoped := dsn + "?search_path=" + schema
	db, err := sql.Open("pgx", scoped)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return ctx, db, scoped
}

// captureStdout runs f and returns what it printed. The command reports its counts on stdout.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	func() {
		defer func() { os.Stdout = saved }()
		f()
	}()
	_ = w.Close()
	return <-done
}

func TestReprocessQuarantineCommandAppliesInProcessWithoutNATS(t *testing.T) {
	ctx, db, dsn := reprocessSchema(t)

	// A schema-2 publication and a schema-2 archive need no catalog call. The catalog server is
	// there so the command's catalog configuration is valid, and it counts calls so the test
	// sees none were made.
	var catalogCalls atomic.Int32
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		catalogCalls.Add(1)
		http.Error(w, "unexpected catalog call", http.StatusInternalServerError)
	}))
	t.Cleanup(catalog.Close)
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("CATALOG_URL", catalog.URL)
	t.Setenv("INTERNAL_SERVICE_TOKEN", strings.Repeat("b2", runtimecfg.CredentialMinBytes/2))
	t.Setenv("NATS_URL", "")
	if err := os.Unsetenv("NATS_URL"); err != nil {
		t.Fatal(err)
	}

	org, slot, archivedSlot := uuid.New(), uuid.New(), uuid.New()
	supportedID, futureID, archiveID := uuid.New(), uuid.New(), uuid.New()
	const published = "platform.catalog.performance.published"
	const archived = "platform.catalog.performance.archived"
	supported := `{"id":"` + supportedID.String() + `","type":"` + published + `","schema":2,"data":{"performance_id":"` +
		slot.String() + `","organizer_id":"` + org.String() + `","capacity":120}}`
	future := `{"id":"` + futureID.String() + `","type":"` + published + `","schema":9,"data":{"slot_ref":{"weird":[1,2]}}}`
	// An archive whose pool does not exist and has no publication moot record: it must wait.
	archive := `{"id":"` + archiveID.String() + `","type":"` + archived + `","schema":2,"data":{"performance_id":"` +
		archivedSlot.String() + `","organizer_id":"` + org.String() + `"}}`
	// The future row is oldest, so a run that stopped at it would leave the others untouched.
	t0 := time.Now().Add(-time.Hour)
	for i, r := range []struct {
		subject  string
		id       uuid.UUID
		schema   int
		envelope string
	}{
		{published, futureID, 9, future},
		{published, supportedID, 2, supported},
		{archived, archiveID, 2, archive},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO catalog_event_quarantine(subject, event_id, schema, envelope, first_seen_at)
			VALUES($1,$2,$3,$4,$5)`, r.subject, r.id, r.schema, []byte(r.envelope), t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	out := captureStdout(t, func() {
		got := execute([]string{"reprocess-quarantine"}, productionCommandCallbacks(), func() error {
			t.Error("server ran")
			return nil
		})
		if got.ExitCode != 0 || got.Err != nil {
			t.Errorf("reprocess-quarantine = %+v, want exit 0: unsupported and deferred rows are not failures", got)
		}
	})
	if t.Failed() {
		t.FailNow()
	}
	if want := "applied=1 terminal=0 unsupported=1 deferred=1"; !strings.Contains(out, want) {
		t.Fatalf("output = %q, want it to contain %q", out, want)
	}
	if !strings.Contains(out, "readiness stays down after a restart") {
		t.Fatalf("output = %q, want the deferred line to say readiness stays down", out)
	}

	// The supported publication is projected: the exact pool, from this event.
	var gotOrg, gotSource uuid.UUID
	var capacity, confirmed int32
	var kind, lifecycle, closure string
	var seatMap *uuid.UUID
	if err := db.QueryRowContext(ctx, `SELECT organizer_id, capacity, confirmed_quantity, source_event_id, inventory_kind,
		seat_map_id, lifecycle_status, closure_status FROM inventory_pools WHERE slot_id=$1`, slot).
		Scan(&gotOrg, &capacity, &confirmed, &gotSource, &kind, &seatMap, &lifecycle, &closure); err != nil {
		t.Fatalf("read the projected pool: %v", err)
	}
	if gotOrg != org || capacity != 120 || confirmed != 0 || gotSource != supportedID || kind != "ga" ||
		seatMap != nil || lifecycle != "published" || closure != "open" {
		t.Fatalf("pool = (org %s, capacity %d, confirmed %d, source %s, kind %s, seat map %v, %s, %s), "+
			"want (org %s, capacity 120, confirmed 0, source %s, kind ga, no seat map, published, open)",
			gotOrg, capacity, confirmed, gotSource, kind, seatMap, lifecycle, closure, org, supportedID)
	}
	var pools int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM inventory_pools`).Scan(&pools); err != nil {
		t.Fatal(err)
	}
	if pools != 1 {
		t.Fatalf("%d pools exist, want only the supported publication's", pools)
	}
	// Only the applied event is consumed. The deferred archive is not.
	consumed := map[uuid.UUID]bool{}
	rows, err := db.QueryContext(ctx, `SELECT event_id FROM consumed_events`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		consumed[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if len(consumed) != 1 || !consumed[supportedID] {
		t.Fatalf("consumed_events = %v, want exactly the supported publication %s", consumed, supportedID)
	}

	// The applied row is resolved and keeps its exact bytes. The other two stay pending.
	for _, r := range []struct {
		subject  string
		id       uuid.UUID
		resolved bool
		envelope string
	}{
		{published, supportedID, true, supported},
		{published, futureID, false, future},
		{archived, archiveID, false, archive},
	} {
		var resolved bool
		var envelope []byte
		if err := db.QueryRowContext(ctx, `SELECT reinjected_at IS NOT NULL, envelope FROM catalog_event_quarantine
			WHERE subject=$1 AND event_id=$2`, r.subject, r.id).Scan(&resolved, &envelope); err != nil {
			t.Fatalf("read quarantine row %s: %v", r.id, err)
		}
		if resolved != r.resolved {
			t.Errorf("row %s %s resolved = %v, want %v", r.subject, r.id, resolved, r.resolved)
		}
		if string(envelope) != r.envelope {
			t.Errorf("row %s envelope changed: %q, want %q", r.id, envelope, r.envelope)
		}
	}
	var parked int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM catalog_event_parked`).Scan(&parked); err != nil {
		t.Fatal(err)
	}
	if parked != 0 {
		t.Fatalf("%d events parked; reprocess never parks (it has no delivery count)", parked)
	}
	if n := catalogCalls.Load(); n != 0 {
		t.Fatalf("catalog was called %d times; neither row needs it", n)
	}
	// The exact predicate startup readiness reads (COS3): rows remain pending, so a restart stays
	// unready.
	pending, err := store.New(db, time.Minute).HasPendingCatalogQuarantine(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("HasPendingCatalogQuarantine = false with a future row and a deferred row pending")
	}
}
