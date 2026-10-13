//go:build smoke

package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/inventory/internal/store"
)

// TKT-295 D2 at the tier an operator uses: the PRODUCTION command registry, the real env
// var and a real migrated schema. A correct store guard that is never wired to the command
// (or wired to the wrong callback) fails here, not in the store suite.
func sequenceSchema(t *testing.T) (context.Context, *sql.DB, string) {
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
	schema := "inventory_seq_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
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
	// A restored numbered row (a pool-shaped one needs only a pool) at 50, with the sequence
	// left at its start: what a value-preserving restore leaves behind.
	org, slot := uuid.New(), uuid.New()
	if err := store.New(db, time.Minute).Provision(ctx, uuid.New(), slot, org, 10); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`ALTER TABLE claim_history DISABLE TRIGGER claim_history_set_append_order`,
		`INSERT INTO claim_history(id,organizer_id,pool_id,action,actor,reason,quantity,quantity_after,status_after,idempotency_key,request_fingerprint,append_order)
		 VALUES('` + uuid.NewString() + `','` + org.String() + `','` + slot.String() + `','adjust_capacity','staff','restored',10,10,'applied','k','fp',50)`,
		`ALTER TABLE claim_history ENABLE TRIGGER claim_history_set_append_order`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	return ctx, db, scoped
}

func TestCheckClaimHistorySequenceCommandRefusesThenRepairs(t *testing.T) {
	ctx, db, dsn := sequenceSchema(t)
	t.Setenv("DATABASE_URL", dsn)
	noServe := func() error { t.Fatal("server ran"); return nil }
	seq := func() (int64, bool) {
		var v int64
		var called bool
		if err := db.QueryRowContext(ctx, `SELECT last_value, is_called FROM claim_history_append_order_seq`).Scan(&v, &called); err != nil {
			t.Fatal(err)
		}
		return v, called
	}
	v0, c0 := seq()

	got := execute([]string{"check-claim-history-sequence"}, productionCommandCallbacks(), noServe)
	if got.ExitCode == 0 || !errors.Is(got.Err, store.ErrAppendOrderSequenceBehind) {
		t.Fatalf("check on a behind sequence = %+v, want a non-zero exit with ErrAppendOrderSequenceBehind", got)
	}
	if v, c := seq(); v != v0 || c != c0 {
		t.Fatalf("a check without --repair moved the sequence: (%d,%v) → (%d,%v)", v0, c0, v, c)
	}

	got = execute([]string{"check-claim-history-sequence", "--repair"}, productionCommandCallbacks(), noServe)
	if got.ExitCode != 0 || got.Err != nil {
		t.Fatalf("--repair = %+v, want exit 0", got)
	}
	if v, c := seq(); v != 50 || !c {
		t.Fatalf("after --repair the sequence is (%d,%v), want (50,true)", v, c)
	}
	if got = execute([]string{"check-claim-history-sequence"}, productionCommandCallbacks(), noServe); got.ExitCode != 0 {
		t.Fatalf("the check after repair = %+v, want exit 0", got)
	}
}

// The output an operator reads: the observed values, and the remedy on a refusal.
func TestCheckClaimHistorySequenceOutput(t *testing.T) {
	_, _, dsn := sequenceSchema(t)
	var out bytes.Buffer
	err := runCheckClaimHistorySequence(nil, dsn, &out)
	if err == nil || !strings.Contains(err.Error(), "check-claim-history-sequence --repair") {
		t.Fatalf("refusal error = %v, want it to name the --repair remedy", err)
	}
	if want := "status=needs-repair last_value=1 is_called=false max_append_order=50 changed=false"; !strings.Contains(out.String(), want) {
		t.Fatalf("refusal output = %q, want it to contain %q", out.String(), want)
	}
	out.Reset()
	if err := runCheckClaimHistorySequence([]string{"--repair"}, dsn, &out); err != nil {
		t.Fatal(err)
	}
	if want := "status=repaired before_last_value=1 before_is_called=false last_value=50 is_called=true max_append_order=50 changed=true"; !strings.Contains(out.String(), want) {
		t.Fatalf("repair output = %q, want it to contain %q", out.String(), want)
	}
	if strings.Contains(out.String(), dsn) {
		t.Fatal("the output echoes the connection string")
	}
}
