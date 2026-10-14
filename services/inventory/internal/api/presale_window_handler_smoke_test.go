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

// presaleCodeLockStatement is the store test's LIKE pattern for the code-lock
// statement in redeemPresaleCode: the table, the key predicate and FOR UPDATE, never
// the projection.
const presaleCodeLockStatement = `SELECT % FROM presale_codes%WHERE organizer_id=$1 AND channel_code=$2 AND code=$3 FOR UPDATE%`

// The uniform refusal, byte for byte (ADR-064 §4).
const presaleCodeInvalidBody = `{"code":"presale_code_invalid","error":"invalid presale code"}`

// presaleWindowAPIStore is a migrated, empty inventory schema, with the database
// handle the forced schedule needs for its blocker and its handshake.
func presaleWindowAPIStore(t *testing.T) (context.Context, *store.Postgres, *sql.DB) {
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
	schema := "inventory_pw_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
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
	return ctx, store.New(db, time.Hour), db
}

// TKT-544 D9 at the tier that decides the wire body: the real router, with
// response validation ON, against PostgreSQL. The store test
// (TestPresaleRefusalIsUniformAcrossTheCodeLockWait) proves the four refusals are
// one sentinel; this proves a client cannot tell them apart.
//
// The schedule is the production-like oracle: an EXHAUSTED code's row is held by a
// transaction that never writes (another redemption, in production), and a
// POST /holds with that code waits on it across the channel's closes_at. While it
// waits, POST /holds with an unknown code, no code, and a code of another channel,
// each on its own pool, finish before closes_at: they have no row to wait on. All
// four must answer 409 with the identical body. With D3 the one that waited
// answered channel_window_closed, which named the code that exists.
func TestPresaleRefusalBodyIsUniformAcrossTheCodeLockWait(t *testing.T) {
	ctx, st, db := presaleWindowAPIStore(t)
	org := uuid.New()
	pool := func() uuid.UUID {
		p := uuid.New()
		if err := st.Provision(ctx, uuid.New(), p, org, 100); err != nil {
			t.Fatal(err)
		}
		return p
	}
	exhaustedPool, unknownPool, emptyPool, otherPool := pool(), pool(), pool(), pool()
	pools := []uuid.UUID{exhaustedPool, unknownPool, emptyPool, otherPool}
	replace := func(closes *time.Time) {
		for _, p := range pools {
			if _, err := st.ReplaceChannelAllocations(ctx, org, p, []store.ChannelAllocation{
				{Channel: "presale", Cap: 100, RequiresCode: true, ClosesAt: closes}}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	replace(nil)
	const exhausted, other = "EXHAUSTED-544", "OTHER-CHANNEL-544"
	capTwo := int32(2)
	for _, c := range []store.PresaleCode{
		{OrganizerID: org, Channel: "presale", Code: exhausted, MaxRedemptions: &capTwo},
		{OrganizerID: org, Channel: "reseller", Code: other},
	} {
		if _, err := st.IssuePresaleCode(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	srv := New(st, "", nil).Router(nil, true)
	post := func(slot uuid.UUID, code, key string) (int, string) {
		body := map[string]any{"organizer_id": org, "slot_id": slot, "quantity": 2, "channel": "presale"}
		// "No code" on the wire is an ABSENT field: the schema refuses an empty
		// string with a 400 before the store is reached.
		if code != "" {
			body["presale_code"] = code
		}
		b, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/holds", bytes.NewReader(b)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}
	// Spend the cap through the same route, so the code is exhausted for real.
	if code, body := post(exhaustedPool, exhausted, "spend"); code != http.StatusCreated {
		t.Fatalf("spend the exhausted code: %d %s", code, body)
	}

	// Last setup step, from the database clock: every pool's channel closes at once.
	var closes time.Time
	if err := db.QueryRowContext(ctx, `SELECT clock_timestamp() + interval '3 seconds'`).Scan(&closes); err != nil {
		t.Fatal(err)
	}
	replace(&closes)
	dbBefore := func(at time.Time) bool {
		var before bool
		if err := db.QueryRowContext(ctx, `SELECT clock_timestamp() < $1`, at).Scan(&before); err != nil {
			t.Fatal(err)
		}
		return before
	}

	// The blocker: the exhausted code's row, FOR UPDATE, never written.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	btx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	type answer struct {
		status int
		body   string
	}
	var waited answer
	finished := make(chan struct{})
	started := false
	t.Cleanup(func() {
		// Before the schema drop: DROP SCHEMA would queue behind the blocker.
		_ = btx.Rollback()
		_ = conn.Close()
		if started {
			<-finished
		}
	})
	var bpid int
	if err = btx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&bpid); err != nil {
		t.Fatal(err)
	}
	var locked string
	if err = btx.QueryRowContext(ctx,
		`SELECT code FROM presale_codes WHERE code=$1 AND channel_code=$2 AND organizer_id=$3 FOR UPDATE`,
		exhausted, "presale", org).Scan(&locked); err != nil {
		t.Fatalf("setup: the blocker could not lock the code row: %v", err)
	}

	started = true
	go func() {
		defer close(finished)
		waited.status, waited.body = post(exhaustedPool, exhausted, "exhausted")
	}()

	// Handshake: exactly one backend, in a Lock wait on exactly the blocker, in the
	// code-lock statement, seen before closes_at.
	var waiterPID int
	for waiterPID == 0 {
		select {
		case <-finished:
			t.Fatalf("setup: the waiting request returned (%d %s) without waiting in the code-lock "+
				"statement; the schedule was not forced", waited.status, waited.body)
		default:
		}
		var pids []int
		rows, err := db.QueryContext(ctx, `SELECT pid FROM pg_stat_activity
			WHERE wait_event_type='Lock' AND pg_blocking_pids(pid) = ARRAY[$1::integer] AND query LIKE $2`,
			bpid, presaleCodeLockStatement)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var pid int
			if err = rows.Scan(&pid); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			pids = append(pids, pid)
		}
		_ = rows.Close()
		switch {
		case len(pids) > 1:
			t.Fatalf("setup: %d backends wait on the blocker in a code-lock statement, want 1", len(pids))
		case len(pids) == 1:
			if !dbBefore(closes) {
				t.Fatalf("setup: the request was seen waiting only after closes_at")
			}
			waiterPID = pids[0]
		default:
			if !dbBefore(closes) {
				t.Fatalf("setup: no request waited on blocker pid %d before closes_at", bpid)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// While it waits: the three requests with no row, each on its own pool.
	noRow := []struct {
		name, code, key string
		pool            uuid.UUID
		got             answer
	}{
		{"unknown code", "NEVER-ISSUED-544", "unknown", unknownPool, answer{}},
		{"no code", "", "empty", emptyPool, answer{}},
		{"code of another channel", other, "other-channel", otherPool, answer{}},
	}
	for i := range noRow {
		noRow[i].got.status, noRow[i].got.body = post(noRow[i].pool, noRow[i].code, noRow[i].key)
	}
	if !dbBefore(closes) {
		t.Fatalf("setup: the three no-row requests finished only after closes_at")
	}

	// Past closes_at by the database clock, still waiting; then release.
	for dbBefore(closes.Add(200 * time.Millisecond)) {
		time.Sleep(20 * time.Millisecond)
	}
	var stillWaiting bool
	if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
		WHERE pid=$1 AND wait_event_type='Lock' AND pg_blocking_pids(pid) = ARRAY[$2::integer])`,
		waiterPID, bpid).Scan(&stillWaiting); err != nil {
		t.Fatal(err)
	}
	if !stillWaiting {
		t.Fatalf("setup: the waiting request stopped waiting before the blocker released")
	}
	if err = btx.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatalf("the waiting request did not finish after the release: %v", ctx.Err())
	}

	report := []string{"exhausted code that waited on its row across closes_at: " +
		http.StatusText(waited.status) + " " + waited.body}
	uniform := waited.status == http.StatusConflict && waited.body == presaleCodeInvalidBody
	for _, r := range noRow {
		report = append(report, r.name+" (no row, no wait): "+http.StatusText(r.got.status)+" "+r.got.body)
		if r.got.status != http.StatusConflict || r.got.body != presaleCodeInvalidBody {
			uniform = false
		}
	}
	if !uniform {
		t.Fatalf("the four POST /holds answers differ; each must be 409 %s:\n  %s\nA distinct body for "+
			"the request that waited tells the caller which code has a row (ADR-064 §4).",
			presaleCodeInvalidBody, strings.Join(report, "\n  "))
	}
}
