//go:build smoke

package store

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func TestRedeemedLifecycleMigrationPreservesHistory(t *testing.T) {
	dsn := os.Getenv("ACCESS_MIGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ACCESS_MIGRATION_TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()

	schema := "access_migration_" + uuid.NewString()[:8]
	if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") }()

	db, err := sql.Open("pgx", dsn+"?search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	migrations, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.UpTo(ctx, 1); err != nil {
		t.Fatalf("apply migration 0001: %v", err)
	}

	ticketID, orderID := uuid.New(), uuid.New()
	organizerID, slotID := uuid.New(), uuid.New()
	issuedID, deliveredID := uuid.New(), uuid.New()
	issuedAt := time.Date(2026, time.July, 12, 14, 30, 0, 0, time.UTC)
	deliveredAt := issuedAt.Add(2 * time.Minute)
	_, err = db.ExecContext(ctx, `
		INSERT INTO tickets(id,order_id,guest_order_ref,organizer_id,buyer_id,slot_id,ticket_type_id,qr_payload,issued_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,'signed-credential',$8)`,
		ticketID, orderID, uuid.New(), organizerID, uuid.New(), slotID, uuid.New(), issuedAt)
	if err != nil {
		t.Fatalf("seed pre-upgrade ticket: %v", err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO lifecycle_events(id,ticket_id,event_type,occurred_at)
		VALUES($1,$2,'issued',$3),($4,$2,'delivered',$5)`,
		issuedID, ticketID, issuedAt, deliveredID, deliveredAt)
	if err != nil {
		t.Fatalf("seed pre-upgrade history: %v", err)
	}

	st := New(db)
	before, err := st.History(ctx, ticketID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.Up(ctx); err != nil {
		t.Fatalf("apply migration 0002: %v", err)
	}
	after, err := st.History(ctx, ticketID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("history changed during upgrade: before=%+v after=%+v", before, after)
	}

	result, err := st.Redeem(ctx, RedeemInput{
		TicketID: ticketID, OrderID: orderID, OrganizerID: organizerID, SlotID: slotID,
	})
	if err != nil {
		t.Fatalf("redeem upgraded ticket: %v", err)
	}
	if !result.Accepted || result.OccurredAt.IsZero() {
		t.Fatalf("redemption result = %+v", result)
	}
	history, err := st.History(ctx, ticketID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 || history[0].ID != issuedID || history[1].ID != deliveredID || history[2].Type != "redeemed" {
		t.Fatalf("post-redemption history = %+v", history)
	}
	if !history[0].OccurredAt.Equal(issuedAt) || !history[1].OccurredAt.Equal(deliveredAt) {
		t.Fatalf("pre-upgrade timestamps changed: %+v", history[:2])
	}

	if _, err = db.ExecContext(ctx, `UPDATE lifecycle_events SET occurred_at=now() WHERE id=$1`, issuedID); err == nil {
		t.Fatal("upgraded lifecycle history is no longer immutable")
	}
	current, target, err := provider.GetVersions(ctx)
	if err != nil || current != 2 || target != 2 {
		t.Fatalf("migration versions current=%d target=%d err=%v", current, target, err)
	}

	t.Logf("migration 0001 -> 0002 preserved %d events and appended redemption", len(before))
}
