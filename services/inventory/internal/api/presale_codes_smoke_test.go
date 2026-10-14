//go:build smoke

package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/inventory/internal/store"
	"ticketing/shared/httpx"
)

const presaleAPICredential = "presale-api-internal-token"

// presaleAPIStore is a migrated, empty inventory schema.
func presaleAPIStore(t *testing.T) (context.Context, *store.Postgres, *sql.DB) {
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
	schema := "inventory_pc_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
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
	return ctx, store.New(db, 10*time.Minute), db
}

// TKT-543 at the tier that decides the wire body: the real router, with response
// validation ON, against PostgreSQL, on both admission endpoints.
//
// A cap refusal now runs a cross-pool cleanup and may retry the admission, and the store
// tells the two admission wrappers about it with an unexported signal (D5). If that signal
// ever left a wrapper, problem() would not recognise it and the exhausted cause alone would
// answer differently from the other four: the enumeration oracle ADR-064 §4 exists to
// prevent. The store tests assert the sentinel; only this tier sees the bytes a scraper
// reads.
//
// Each subtest first runs a reclamation through the endpoint and checks that its expiry
// committed, so the refusals below are asserted on a path where the cleanup is live, then
// exhausts the code with a confirmed claim.
func TestPresaleRefusalsStayUniformAfterCrossPoolCleanup(t *testing.T) {
	const want = `{"code":"presale_code_invalid","error":"invalid presale code"}`
	for _, tc := range []struct {
		name string
		path string
		body func(org, slot uuid.UUID, code string) map[string]any
	}{
		{
			name: "POST /holds",
			path: "/holds",
			body: func(org, slot uuid.UUID, code string) map[string]any {
				b := map[string]any{"organizer_id": org, "slot_id": slot, "quantity": 1, "channel": "presale"}
				if code != "" {
					b["presale_code"] = code
				}
				return b
			},
		},
		{
			name: "POST /internal/group-reservations",
			path: "/internal/group-reservations",
			body: func(org, slot uuid.UUID, code string) map[string]any {
				b := map[string]any{"organizer_id": org, "slot_id": slot, "quantity": 1, "channel": "presale",
					"counterparty": "agency", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
					"actor": "staff", "reason": "group"}
				if code != "" {
					b["presale_code"] = code
				}
				return b
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, st, db := presaleAPIStore(t)
			srv := New(st, presaleAPICredential, nil).Router(nil, true)
			post := func(slot uuid.UUID, org uuid.UUID, code, key string) (int, string) {
				t.Helper()
				b, err := json.Marshal(tc.body(org, slot, code))
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(b))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Idempotency-Key", key)
				req.Header.Set(httpx.InternalToken, presaleAPICredential)
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, req)
				return rec.Code, strings.TrimSpace(rec.Body.String())
			}

			org, p1, p2 := uuid.New(), uuid.New(), uuid.New()
			for _, p := range []uuid.UUID{p1, p2} {
				if err := st.Provision(ctx, uuid.New(), p, org, 50); err != nil {
					t.Fatal(err)
				}
				if _, err := st.ReplaceChannelAllocations(ctx, org, p, []store.ChannelAllocation{
					{Channel: "presale", Cap: 20, RequiresCode: true},
					{Channel: "reseller", Cap: 20, RequiresCode: true},
				}, nil); err != nil {
					t.Fatal(err)
				}
			}
			two := int32(2)
			past := time.Now().Add(-time.Hour)
			for _, c := range []store.PresaleCode{
				{OrganizerID: org, Channel: "presale", Code: "CAP", MaxRedemptions: &two},
				{OrganizerID: org, Channel: "presale", Code: "CLOSED", ClosesAt: &past},
				{OrganizerID: org, Channel: "reseller", Code: "OTHER-CHANNEL"},
			} {
				if _, err := st.IssuePresaleCode(ctx, c); err != nil {
					t.Fatal(err)
				}
			}

			// Reclamation through the endpoint: CAP's only other usage is an expired,
			// unswept hold on a quiet P2.
			h, _, err := st.CreateHold(ctx, org, p2, uuid.Nil, 2, 0, "", "presale", "h", store.WithPresaleCode("CAP"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.ExecContext(ctx, `UPDATE claims SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, h.ID); err != nil {
				t.Fatal(err)
			}
			if code, body := post(p1, org, "CAP", "reclaim"); code != http.StatusCreated {
				t.Fatalf("reclaiming admission: %d %s, want 201", code, body)
			}
			var status string
			if err = db.QueryRowContext(ctx, `SELECT status FROM claims WHERE id=$1`, h.ID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != "expired" {
				t.Fatalf("the expired hold on P2 is %q after the reclaiming admission, want expired", status)
			}

			// Exhaust CAP (cap 2) with a confirmed claim: 1 from the reclaiming admission
			// plus 1 confirmed here.
			c, _, err := st.CreateHold(ctx, org, p2, uuid.Nil, 1, 0, "", "presale", "spend", store.WithPresaleCode("CAP"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = st.Transition(ctx, org, c.ID, "confirmed"); err != nil {
				t.Fatal(err)
			}

			var before int
			if err = db.QueryRowContext(ctx, `SELECT count(*) FROM claims WHERE organizer_id=$1`, org).Scan(&before); err != nil {
				t.Fatal(err)
			}
			for cause, code := range map[string]string{
				"no code at all":          "",
				"unknown code":            "NEVER-ISSUED",
				"code of another channel": "OTHER-CHANNEL",
				"exhausted code":          "CAP",
				"out-of-window code":      "CLOSED",
			} {
				if status, body := post(p1, org, code, "refused-"+uuid.NewString()); status != http.StatusConflict || body != want {
					t.Errorf("%s: %d %s, want 409 %s", cause, status, body, want)
				}
			}
			var after int
			if err = db.QueryRowContext(ctx, `SELECT count(*) FROM claims WHERE organizer_id=$1`, org).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("refused requests created %d claims, want 0", after-before)
			}
		})
	}
}

// waiterBehind polls until a backend running a statement that contains stmt waits on a
// lock that holder blocks, and returns its pid and transaction start. A queued waiter is
// blocked by the waiter ahead of it, so holder is that waiter where there is one.
func waiterBehind(t *testing.T, ctx context.Context, db *sql.DB, holder int, stmt string, returned <-chan struct{}) (int, time.Time) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var pid int
		var xactStart time.Time
		err := db.QueryRowContext(ctx, `SELECT pid, xact_start FROM pg_stat_activity
			WHERE $1 = ANY(pg_blocking_pids(pid)) AND wait_event_type = 'Lock'
			  AND strpos(query, $2) > 0 AND pid <> pg_backend_pid()`, holder, stmt).Scan(&pid, &xactStart)
		if err == nil {
			return pid, xactStart
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		select {
		case <-returned:
			t.Fatalf("reach: the call returned before a backend running %q waited behind pid %d", stmt, holder)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("reach: no backend running %q waited behind pid %d within 10s", stmt, holder)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TKT-543 D17 at the tier that decides the wire body, on POST /holds.
//
// Review pass 3's schedule (the store tier has it for both admission paths in
// TestPresaleRetryAnswersOnlySuccessOrTheUniformRefusal). A finalize queued on P2 before
// H's expiry keeps H; the admission's cleanup waits behind it; P1's channel window closes
// during that wait; the cleanup commits an empty transaction and the admission runs again.
// The exhausted code must answer exactly what an unknown code answers at the same moment:
// a different body is the enumeration oracle ADR-064 §4 forbids.
func TestPresaleRetryRefusalIsUniformOnTheWire(t *testing.T) {
	const want = `{"code":"presale_code_invalid","error":"invalid presale code"}`
	ctx, st, db := presaleAPIStore(t)
	srv := New(st, presaleAPICredential, nil).Router(nil, true)
	org, p1, p2 := uuid.New(), uuid.New(), uuid.New()
	for _, p := range []uuid.UUID{p1, p2} {
		if err := st.Provision(ctx, uuid.New(), p, org, 50); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ReplaceChannelAllocations(ctx, org, p, []store.ChannelAllocation{{Channel: "presale", Cap: 20, RequiresCode: true}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	one := int32(1)
	if _, err := st.IssuePresaleCode(ctx, store.PresaleCode{OrganizerID: org, Channel: "presale", Code: "CAP", MaxRedemptions: &one}); err != nil {
		t.Fatal(err)
	}
	h, _, err := st.CreateHold(ctx, org, p2, uuid.Nil, 1, 0, "", "presale", "h", store.WithPresaleCode("CAP"))
	if err != nil {
		t.Fatal(err)
	}
	post := func(ctx context.Context, code, key string) (int, string) {
		b, err := json.Marshal(map[string]any{"organizer_id": org, "slot_id": p1, "quantity": 1, "channel": "presale", "presale_code": code})
		if err != nil {
			t.Error(err)
			return 0, ""
		}
		req := httptest.NewRequest(http.MethodPost, "/holds", bytes.NewReader(b)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}
	// Goroutines are cancelled and drained on every exit path.
	runCtx, cancel := context.WithCancel(ctx)
	var running sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		running.Wait()
	})

	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Rollback() })
	var blockerPID int
	if err = blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	if _, err = blocker.ExecContext(ctx, `SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE`, p2); err != nil {
		t.Fatal(err)
	}
	finalized := make(chan error, 1)
	finDone := make(chan struct{})
	running.Add(1)
	go func() {
		defer running.Done()
		defer close(finDone)
		_, err := st.Transition(runCtx, org, h.ID, "finalizing")
		finalized <- err
	}()
	finPID, finStart := waiterBehind(t, ctx, db, blockerPID, "SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE", finDone)
	var expiry time.Time
	if err = db.QueryRowContext(ctx, `UPDATE claims SET expires_at=clock_timestamp() WHERE id=$1 AND status='held' RETURNING expires_at`, h.ID).Scan(&expiry); err != nil {
		t.Fatal(err)
	}
	if !finStart.Before(expiry) {
		t.Fatalf("reach: the finalize's transaction began at %v, not before H's expiry %v", finStart, expiry)
	}
	for past := false; !past; time.Sleep(5 * time.Millisecond) {
		if err = db.QueryRowContext(ctx, `SELECT clock_timestamp() > $1`, expiry).Scan(&past); err != nil {
			t.Fatal(err)
		}
	}

	type response struct {
		status int
		body   string
	}
	answered := make(chan response, 1)
	admDone := make(chan struct{})
	running.Add(1)
	go func() {
		defer running.Done()
		defer close(admDone)
		s, b := post(runCtx, "CAP", "exhausted")
		answered <- response{s, b}
	}()
	waiterBehind(t, ctx, db, finPID, "/* presale-cleanup pool lock */", admDone)

	// The control, at the same moment: an unknown code is refused by the code, at once.
	if s, b := post(ctx, "NEVER-ISSUED", "control"); s != http.StatusConflict || b != want {
		t.Fatalf("an unknown code at the same moment: %d %s, want 409 %s", s, b, want)
	}
	if _, err = db.ExecContext(ctx, `UPDATE channel_allocations SET opens_at=clock_timestamp()+interval '1 hour'
		WHERE pool_id=$1 AND channel_code='presale'`, p1); err != nil {
		t.Fatal(err)
	}
	if err = blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	var r response
	select {
	case r = <-answered:
	case <-time.After(10 * time.Second):
		t.Fatal("the exhausted-code request did not answer within 10s")
	}
	select {
	case err = <-finalized:
		if err != nil {
			t.Fatalf("reach: the finalize failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reach: the finalize did not return within 10s")
	}
	var status string
	if err = db.QueryRowContext(ctx, `SELECT status FROM claims WHERE id=$1`, h.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "finalizing" {
		t.Fatalf("reach: H is %q, want finalizing: the cleanup must have found nothing to expire", status)
	}
	if r.status != http.StatusConflict || r.body != want {
		t.Fatalf("the exhausted code whose retry met a closed window answered %d %s, want 409 %s: "+
			"an unknown code at the same moment answered the uniform body", r.status, r.body, want)
	}
}
