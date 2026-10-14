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
