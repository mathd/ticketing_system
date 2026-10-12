//go:build smoke

package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/inventory/internal/store"
)

// soldOutAPIStore is a migrated, empty inventory schema with one GA pool.
func soldOutAPIStore(t *testing.T, capacity int32) (*store.Postgres, uuid.UUID, uuid.UUID) {
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
	schema := "inventory_so_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") })
	db, err := sql.Open("pgx", dsn+"?search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	st := store.New(db, 10*time.Minute)
	org, slot := uuid.New(), uuid.New()
	if err = st.Provision(ctx, uuid.New(), slot, org, capacity); err != nil {
		t.Fatal(err)
	}
	return st, org, slot
}

// TKT-341 / ADR-082 at the tier that decides the wire body: the real router, with
// response validation ON, against PostgreSQL. The store test proves the
// classification; this proves that the classification reaches a client as a valid
// 409 with `sold_out`. Without the enum entry the fail-closed validator answers 500
// (ADR-028), and only a validated router can see that.
func TestAPublicHoldOnAnExhaustedPoolAnswersSoldOutOnTheWire(t *testing.T) {
	st, org, slot := soldOutAPIStore(t, 3)
	srv := New(st, "", nil).Router(nil, true)
	post := func(qty int, key string) (int, string) {
		t.Helper()
		b, err := json.Marshal(map[string]any{"organizer_id": org, "slot_id": slot, "quantity": qty})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/holds", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}

	if code, body := post(2, "first"); code != http.StatusCreated {
		t.Fatalf("first hold: %d %s", code, body)
	}
	// One unit left, two requested: a partial shortage stays the code-less refusal.
	if code, body := post(2, "partial"); code != http.StatusConflict || body != `{"error":"insufficient capacity"}` {
		t.Fatalf("partial shortage: %d %s, want 409 with no code", code, body)
	}
	if code, body := post(1, "last"); code != http.StatusCreated {
		t.Fatalf("last unit: %d %s", code, body)
	}
	// Zero left.
	if code, body := post(1, "after"); code != http.StatusConflict || body != `{"code":"sold_out","error":"insufficient capacity: sold out"}` {
		t.Fatalf("exhausted pool: %d %s, want 409 sold_out", code, body)
	}
}
