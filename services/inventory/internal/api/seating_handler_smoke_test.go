//go:build smoke

package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"ticketing/services/inventory/internal/store"
)

// Absent versus broken, on the seating lookup (TKT-305).
//
// `holdSeating` mapped EVERY store error to 404, so commerce asking "is this claim
// seated" during a database outage was told "no such claim" — a fact, about a claim
// that exists. Every other handler in this service routes through `problem()`, which
// answers 404 only for store.ErrNotFound and 500 for anything else.
//
// The consumer is the exchange refusal (TKT-158): an exchange must refuse a SEATED
// source before money moves. A 404 during an outage currently fails safe by accident
// — commerce treats it as "not seated" and proceeds — which is the wrong reason to be
// correct, and one caller away from an exchange settling against a seated line.
//
// The distinction is only visible with a REAL database, because it is the difference
// between two store return paths (ErrNotFound versus a driver error) that no fake can
// produce faithfully. Hence a smoke test rather than a handler unit test.
func seatingAPIStore(t *testing.T) (*store.Postgres, *sql.DB, uuid.UUID) {
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
	schema := "inventory_seating_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
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
	return store.New(db, 10*time.Minute), db, uuid.New()
}

// seatingRequest drives holdSeating through a chi router so the {id} URL param binds
// the way it does in production.
func seatingRequest(t *testing.T, st *store.Postgres, org, hold uuid.UUID) (int, string) {
	t.Helper()
	srv := New(st, "internal-token", nil)
	r := chi.NewRouter()
	r.Get("/internal/holds/{id}/seating", srv.holdSeating)
	req := httptest.NewRequest(http.MethodGet,
		"/internal/holds/"+hold.String()+"/seating?organizer_id="+org.String(), nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// A claim that genuinely does not exist is 404. This is the behaviour that must
// SURVIVE the fix — without it, "route everything through problem()" could be
// satisfied by answering 500 for everything.
func TestSeatingLookupAnswers404ForAClaimThatDoesNotExist(t *testing.T) {
	st, _, org := seatingAPIStore(t)
	code, body := seatingRequest(t, st, org, uuid.New())
	if code != http.StatusNotFound {
		t.Fatalf("an absent claim answered %d %s, want 404: it really is not there", code, body)
	}
}

// A store FAILURE is 500, not 404.
//
// The outage is driven by closing the pool the store holds, which makes every query
// return a driver error rather than sql.ErrNoRows — exactly the branch
// ClaimIsSeated distinguishes (refund_returns.go: ErrNotFound for no rows, the raw
// error otherwise) and the branch the handler used to flatten.
//
// Asserting 500 rather than "not 404" is deliberate: it is what problem() produces
// for an unrecognised error, so this pins the routing decision and not merely the
// absence of the old answer.
func TestSeatingLookupAnswers500WhenTheStoreFails(t *testing.T) {
	st, db, org := seatingAPIStore(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	code, body := seatingRequest(t, st, org, uuid.New())
	if code != http.StatusInternalServerError {
		t.Fatalf("a store failure answered %d %s, want 500 — reporting an outage as 404 tells "+
			"commerce a claim is absent when inventory simply could not look, and the exchange "+
			"refusal that consumes this then decides on a fact nobody established", code, body)
	}
}
