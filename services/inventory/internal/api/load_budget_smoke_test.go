//go:build smoke

package api

import (
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
	_ "github.com/jackc/pgx/v5/stdlib"

	"ticketing/services/inventory/internal/availability"
	"ticketing/services/inventory/internal/seatoccupancy"
	"ticketing/services/inventory/internal/store"
)

// The two display reads answer 503 when their cache's OWN load budget expires (TKT-211,
// ADR-044). The fixture makes the read block INSIDE Postgres: a second transaction holds
// an ACCESS EXCLUSIVE lock on inventory_pools, which both reads query first, so the load
// is still in the database when the budget fires. That is the path production takes, and
// the one a pre-expired context would never reach — pgx may report an in-query deadline
// as a server-side cancellation rather than as context.DeadlineExceeded.
const testLoadBudget = 200 * time.Millisecond

type budgetFixture struct {
	db                  *sql.DB
	st                  *store.Postgres
	org, gaSlot, seated uuid.UUID
}

func loadBudgetFixture(t *testing.T) budgetFixture {
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
	schema := "inventory_budget_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
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
	f := budgetFixture{db: db, st: st, org: uuid.New(), gaSlot: uuid.New(), seated: uuid.New()}
	if err = st.Provision(ctx, uuid.New(), f.gaSlot, f.org, 100); err != nil {
		t.Fatal(err)
	}
	if err = st.ProvisionSeated(ctx, uuid.New(), f.seated, f.org, uuid.New(), 100, false, nil); err != nil {
		t.Fatal(err)
	}
	return f
}

// lockPools holds an ACCESS EXCLUSIVE lock on inventory_pools until the test ends. The
// lock transaction has its own lock_timeout so a stuck test cannot wedge the package, and
// the cleanup rolls it back before the schema is dropped.
func lockPools(t *testing.T, db *sql.DB) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.Exec(`SET LOCAL lock_timeout = '5s'`); err != nil {
		t.Fatal(err)
	}
	// lock_timeout bounds how long taking the lock may wait, not how long it is HELD. If a
	// regression left the read hanging, the cleanup below would never run; the server ends
	// an idle-in-transaction session itself, so the lock cannot outlive a stuck test
	// (review F4).
	if _, err := tx.Exec(`SET LOCAL idle_in_transaction_session_timeout = '15s'`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`LOCK TABLE inventory_pools IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
}

func budgetServer(f budgetFixture) http.Handler {
	return NewWithReaders(f.st, "", nil,
		availability.New(f.st, availability.WithLoadTimeout(testLoadBudget)),
		seatoccupancy.New(f.st, seatoccupancy.WithLoadTimeout(testLoadBudget)),
	).Router(nil, true)
}

func getRead(t *testing.T, srv http.Handler, path string) (*httptest.ResponseRecorder, time.Duration) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	res := httptest.NewRecorder()
	start := time.Now()
	srv.ServeHTTP(res, req)
	return res, time.Since(start)
}

func assertBudget503(t *testing.T, res *httptest.ResponseRecorder, elapsed time.Duration, wantError, wantCode string) {
	t.Helper()
	// The fixture must really have blocked: an answer faster than the budget means the
	// read never waited in Postgres, and whatever status it returned proves nothing here.
	if elapsed < testLoadBudget {
		t.Fatalf("read answered in %v, before its %v budget: the lock did not block it", elapsed, testLoadBudget)
	}
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", res.Code, res.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("503 body is not JSON: %v (%s)", err, res.Body.String())
	}
	if body["error"] != wantError || body["code"] != wantCode || len(body) != 2 {
		t.Fatalf("503 body = %v, want exactly error=%q code=%q", body, wantError, wantCode)
	}
	lower := strings.ToLower(res.Body.String())
	if strings.Contains(lower, "deadline") || strings.Contains(lower, "context") || strings.Contains(lower, "cancel") {
		t.Fatalf("503 body leaks internal error text: %s", res.Body.String())
	}
}

func TestAvailabilityPastItsBudgetAnswers503(t *testing.T) {
	f := loadBudgetFixture(t)
	srv := budgetServer(f)
	lockPools(t, f.db)
	res, elapsed := getRead(t, srv, "/slots/"+f.gaSlot.String()+"/availability?organizer_id="+f.org.String())
	assertBudget503(t, res, elapsed, "availability temporarily unavailable, retry", "availability_unavailable")
}

func TestSeatOccupancyPastItsBudgetAnswers503(t *testing.T) {
	f := loadBudgetFixture(t)
	srv := budgetServer(f)
	lockPools(t, f.db)
	res, elapsed := getRead(t, srv, "/slots/"+f.seated.String()+"/seat-occupancy?organizer_id="+f.org.String())
	assertBudget503(t, res, elapsed, "seat occupancy temporarily unavailable, retry", "seat_occupancy_unavailable")
}

// A caller that leaves, or whose own deadline passes, is not a slow dependency. Its read
// ends with the CALLER's context error from the wait (context.Canceled or the caller's
// context.DeadlineExceeded), which must stay on the existing path. The deadline case is
// the one a naive errors.Is(err, context.DeadlineExceeded) mapping would get wrong: only
// the cache's own load budget is evidence that the dependency was slow.
//
// Each case gets its OWN fixture (review pass 4): the cache's load outlives a caller that
// left, so a shared schema would carry one case's waiting query into the next, and a
// coalesced read would issue no query at all. With a fresh schema, the only backend that
// can wait on its inventory_pools is this case's read.
func TestCallerCancellationOrDeadlineIsNot503(t *testing.T) {
	type caller struct {
		name     string
		deadline time.Duration // zero: cancel explicitly once the read is seen waiting
	}
	for _, c := range []caller{{name: "canceled"}, {name: "caller deadline", deadline: 300 * time.Millisecond}} {
		for _, read := range []string{"availability", "seat-occupancy"} {
			t.Run(c.name+"/"+read, func(t *testing.T) {
				f := loadBudgetFixture(t)
				srv := NewWithReaders(f.st, "", nil,
					availability.New(f.st, availability.WithLoadTimeout(5*time.Second)),
					seatoccupancy.New(f.st, seatoccupancy.WithLoadTimeout(5*time.Second)),
				).Router(nil, true)
				lockPools(t, f.db)
				slot := f.gaSlot
				if read == "seat-occupancy" {
					slot = f.seated
				}
				path := "/slots/" + slot.String() + "/" + read + "?organizer_id=" + f.org.String()

				ctx, cancel := context.WithCancel(context.Background())
				if c.deadline > 0 {
					ctx, cancel = context.WithTimeout(context.Background(), c.deadline)
				}
				defer cancel()
				res := httptest.NewRecorder()
				done := make(chan struct{})
				go func() {
					defer close(done)
					srv.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
				}()
				// Evidence, not timing, that THIS read reached Postgres and is waiting on the
				// lock. Bounded, and each probe has its own timeout, so a stuck observation
				// fails the test instead of hanging it.
				if !waitForLockWaiter(t, f.db, 3*time.Second) {
					t.Fatalf("%s: no backend was seen waiting on inventory_pools: the read never reached the lock", path)
				}
				if c.deadline == 0 {
					cancel()
				}
				<-done
				// The existing path: problem()'s default, a fixed 500 body. Pinned exactly, so a
				// read that ignored the caller's end and answered 200 cannot pass either.
				if res.Code != http.StatusInternalServerError || strings.TrimSpace(res.Body.String()) != `{"error":"internal error"}` {
					t.Fatalf("%s: got %d %s, want the existing 500 internal error; only the cache's own budget means a slow dependency",
						path, res.Code, res.Body.String())
				}
			})
		}
	}
}

// waitForLockWaiter polls pg_locks until some backend waits on inventory_pools, or the
// bound passes. Every probe carries its own short timeout.
func waitForLockWaiter(t *testing.T, db *sql.DB, bound time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		var waiting int
		err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks WHERE NOT granted AND relation = 'inventory_pools'::regclass`).Scan(&waiting)
		cancel()
		if err == nil && waiting > 0 {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}
