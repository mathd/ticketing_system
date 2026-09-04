//go:build smoke

package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"ticketing/services/access/internal/lifecycle"
	"ticketing/services/access/internal/store"
	"ticketing/services/access/internal/ticket"
)

// Handler-through-store round trip for the occurrence protocol: the store
// tests prove the semantics; this proves the HTTP mapping — the replay marker
// on the wire, and the per-occurrence reconcile results.
func newSmokeServer(t *testing.T, ctx context.Context) (*Server, *store.Postgres, *ticket.Signer) {
	t.Helper()
	dsn := os.Getenv("ACCESS_MIGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ACCESS_MIGRATION_TEST_DATABASE_URL is not set")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	schema := "access_api_" + uuid.NewString()[:8]
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

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := lifecycle.NewSigner(base64.RawStdEncoding.EncodeToString(priv.Seed()), "access-lifecycle/test")
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := lifecycle.NewKeyring("access-lifecycle/test=" + base64.RawStdEncoding.EncodeToString(pub))
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(db, store.Config{Signer: signer, Keyring: keyring, Policy: store.DefaultPolicy()})

	qrPub, qrPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	qrSigner, err := ticket.New(base64.RawStdEncoding.EncodeToString(qrPriv.Seed()), "access-qr/test-v1")
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := ticket.NewVerifier("access-qr/test-v1="+base64.RawStdEncoding.EncodeToString(qrPub), "access-qr/test-v1")
	if err != nil {
		t.Fatal(err)
	}
	return newTestServer(st, verifier), st, qrSigner
}

// issueSmokeTicket issues one ticket and returns its credential AND the
// organizer it belongs to. The organizer is returned because these routes are
// device-authenticated (ai-review S1): a caller has to enrol a device for the
// SAME organizer or the scope check refuses, and a helper that hid the organizer
// made that impossible to do — which is how three tests in this file came to be
// asserting 401 responses (TKT-162).
func issueSmokeTicket(t *testing.T, ctx context.Context, st *store.Postgres, qr *ticket.Signer) (payload string, organizer uuid.UUID) {
	t.Helper()
	ticketID, orderID := uuid.New(), uuid.New()
	organizerID, slotID := uuid.New(), uuid.New()
	issuedAt := time.Now().UTC()
	payload, err := qr.Payload(ticketID, orderID, organizerID, slotID, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Issue(ctx, store.IssueInput{EventID: uuid.New(), Tickets: []store.Ticket{{
		ID: ticketID, OrderID: orderID, GuestOrderRef: uuid.New(), OrganizerID: organizerID,
		BuyerID: uuid.New(), SlotID: slotID, TicketTypeID: uuid.New(), Payload: payload, IssuedAt: issuedAt,
	}}}); err != nil {
		t.Fatal(err)
	}
	return payload, organizerID
}

// postJSON carries the device credential, for the same reason scanRequest does in
// server_test.go: a test that means to exercise a scan must not silently exercise
// the enrolment guard instead. Pair the router with `enrolled(srv, organizer)`.
func postJSON(t *testing.T, router http.Handler, path, body string) (int, map[string]any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(scannerDeviceHeader, testDeviceToken)
	router.ServeHTTP(recorder, request)
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode %s response %q: %v", path, recorder.Body.String(), err)
	}
	return recorder.Code, response
}

func TestScanReplayMarkerOnTheWire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	srv, st, qr := newSmokeServer(t, ctx)
	payload, organizer := issueSmokeTicket(t, ctx, st, qr)
	router := enrolled(srv, organizer).Router(nil, true)
	occ := uuid.NewString()
	body := `{"qr_payload":` + mustJSON(t, payload) + `,"occurrence_id":"` + occ + `","occurred_at":"2026-07-17T09:00:00Z"}`

	code, first := postJSON(t, router, "/scans", body)
	if code != http.StatusOK || first["decision"] != "accepted" {
		t.Fatalf("first scan = %d %v", code, first)
	}
	if _, present := first["replay"]; present {
		t.Fatalf("first-time acceptance carries a replay marker: %v", first)
	}
	code, retry := postJSON(t, router, "/scans", body)
	if code != http.StatusOK || retry["decision"] != "accepted" || retry["replay"] != true {
		t.Fatalf("retry = %d %v, want accepted with replay:true (never bare accepted, ADR-025 §D3)", code, retry)
	}
}

func TestReconcileResultsOnTheWire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	srv, st, qr := newSmokeServer(t, ctx)
	payload, organizer := issueSmokeTicket(t, ctx, st, qr)
	router := enrolled(srv, organizer).Router(nil, true)
	occA, occB := uuid.NewString(), uuid.NewString()

	// Live redemption first, then an offline occurrence syncs: conflict.
	if code, r := postJSON(t, router, "/scans", `{"qr_payload":`+mustJSON(t, payload)+`,"occurrence_id":"`+occA+`","occurred_at":"2026-07-17T09:00:00Z"}`); code != http.StatusOK {
		t.Fatalf("live scan = %d %v", code, r)
	}
	code, response := postJSON(t, router, "/scans/reconciliations",
		`{"occurrences":[{"qr_payload":`+mustJSON(t, payload)+`,"occurrence_id":"`+occB+`","occurred_at":"2026-07-17T09:05:00Z"}]}`)
	if code != http.StatusOK {
		t.Fatalf("reconcile = %d %v", code, response)
	}
	results, _ := response["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results = %v", response)
	}
	entry, _ := results[0].(map[string]any)
	if entry["occurrence_id"] != occB || entry["result"] != "conflict" {
		t.Fatalf("entry = %v, want an explicit conflict result for %s (ADR-025 §D6)", entry, occB)
	}

	// Retrying the sync: synced, not a second conflict.
	code, response = postJSON(t, router, "/scans/reconciliations",
		`{"occurrences":[{"qr_payload":`+mustJSON(t, payload)+`,"occurrence_id":"`+occB+`","occurred_at":"2026-07-17T09:05:00Z"}]}`)
	if code != http.StatusOK {
		t.Fatalf("reconcile retry = %d %v", code, response)
	}
	results, _ = response["results"].([]any)
	entry, _ = results[0].(map[string]any)
	if entry["result"] != "synced" {
		t.Fatalf("retry entry = %v, want synced", entry)
	}
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TKT-87 on the wire: the direction field maps to the pass path, policy
// denials surface as distinguishable 409 reasons, and a reconciled pass
// occurrence records factually. Store tests own the semantics; this pins the
// HTTP mapping.
func TestPassScanFlowOnTheWire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	srv, st, qr := newSmokeServer(t, ctx)
	ticketID, orderID := uuid.New(), uuid.New()
	organizerID, slotID := uuid.New(), uuid.New()
	// Enrolled for THIS ticket's organizer: the scope check compares the device's
	// organizer to the credential's, so a device paired to anyone else is refused.
	router := enrolled(srv, organizerID).Router(nil, true)
	issuedAt := time.Now().UTC()
	payload, err := qr.Payload(ticketID, orderID, organizerID, slotID, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Issue(ctx, store.IssueInput{EventID: uuid.New(), Tickets: []store.Ticket{{
		ID: ticketID, OrderID: orderID, GuestOrderRef: uuid.New(), OrganizerID: organizerID,
		BuyerID: uuid.New(), SlotID: slotID, TicketTypeID: uuid.New(), Payload: payload, IssuedAt: issuedAt,
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSlotPolicy(ctx, uuid.New(), store.SlotPolicy{
		SlotID: slotID, OrganizerID: organizerID,
		Policy: store.ReEntryPolicy{Mode: "multi", RequiresExit: true},
	}); err != nil {
		t.Fatal(err)
	}
	at := func(offset time.Duration) string { return time.Now().UTC().Add(offset).Format(time.RFC3339) }
	scan := func(direction string, occ string, offset time.Duration) (int, map[string]any) {
		body := `{"qr_payload":` + mustJSON(t, payload) + `,"occurrence_id":"` + occ + `","occurred_at":"` + at(offset) + `"`
		if direction != "" {
			body += `,"direction":"` + direction + `"`
		}
		return postJSON(t, router, "/scans", body+`}`)
	}

	if code, r := scan("", uuid.NewString(), -time.Hour); code != http.StatusOK || r["decision"] != "accepted" {
		t.Fatalf("pass entry = %d %v", code, r)
	}
	if code, r := scan("entry", uuid.NewString(), -50*time.Minute); code != http.StatusConflict || r["reason"] != "exit_required" {
		t.Fatalf("re-entry while inside = %d %v, want 409 exit_required", code, r)
	}
	if code, r := scan("exit", uuid.NewString(), -40*time.Minute); code != http.StatusOK || r["decision"] != "accepted" {
		t.Fatalf("exit = %d %v", code, r)
	}
	if code, r := scan("exit", uuid.NewString(), -35*time.Minute); code != http.StatusConflict || r["reason"] != "not_inside" {
		t.Fatalf("exit while outside = %d %v, want 409 not_inside", code, r)
	}
	// No occurrence id on a pass slot: the distinguishable 409, nothing stored.
	code, r := postJSON(t, router, "/scans", `{"qr_payload":`+mustJSON(t, payload)+`}`)
	if code != http.StatusConflict || r["reason"] != "occurrence_required" {
		t.Fatalf("occurrence-less pass scan = %d %v, want 409 occurrence_required", code, r)
	}

	// A reconciled pass occurrence records factually — result recorded, never
	// conflict (pass conflicts are derived projections, ADR-025 §D2).
	code, response := postJSON(t, router, "/scans/reconciliations",
		`{"occurrences":[{"qr_payload":`+mustJSON(t, payload)+`,"occurrence_id":"`+uuid.NewString()+`","occurred_at":"`+at(-30*time.Minute)+`","event_type":"entry"}]}`)
	if code != http.StatusOK {
		t.Fatalf("pass reconcile = %d %v", code, response)
	}
	results, _ := response["results"].([]any)
	entry, _ := results[0].(map[string]any)
	if entry["result"] != "recorded" {
		t.Fatalf("pass reconcile entry = %v, want recorded", entry)
	}
}

// The organizer must be UNSUBMITTABLE, not merely validated.
//
// Caught by a mutation rather than by review: teaching the handler to read an
// `organizer_id` query parameter left every other test in this file green. A
// device could then read any organizer's revocations — a map of which of a
// competitor's tickets have been refunded — on a route any enrolled device can
// reach.
//
// And the obvious defence is NOT there: OpenAPI query validation ignores
// undeclared parameters, so `?organizer_id=…` reaches the handler as ordinary
// noise. Nothing refuses it. What makes the value unsubmittable is that the
// handler never looks for it — the organizer has exactly one source, the context
// slot the authentication func filled.
//
// So this test asserts the source rather than the refusal: the same request with
// and without a foreign `organizer_id` must produce the SAME feed. If a future
// edit gives that parameter meaning, these two answers diverge and this goes red.
func TestVoidedFeedIgnoresACallerSuppliedOrganizer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	srv, st, qr := newSmokeServer(t, ctx)

	mine := uuid.New()
	theirs := uuid.New()
	// A voided ticket for each organizer. The device below is enrolled for
	// `mine`, so a handler that honoured the parameter would return theirs.
	seedVoidedSmokeTicket(t, ctx, st, qr, mine)
	seedVoidedSmokeTicket(t, ctx, st, qr, theirs)

	router := enrolled(srv, mine).Router(nil, true)

	plain := getFeed(t, router, "/scans/voided-tickets")
	spoofed := getFeed(t, router, "/scans/voided-tickets?organizer_id="+theirs.String())

	if len(plain) != 1 {
		t.Fatalf("the device's own feed = %v, want exactly its one voided ticket", plain)
	}
	if !reflect.DeepEqual(plain, spoofed) {
		t.Fatalf("a caller-supplied organizer_id changed the feed:\n plain    = %v\n spoofed  = %v\nthe organizer has one source, the device token", plain, spoofed)
	}
}

func getFeed(t *testing.T, router http.Handler, target string) []any {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, scanRequest(http.MethodGet, target, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", target, recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	ids, _ := body["ticket_ids"].([]any)
	return ids
}

// seedVoidedSmokeTicket issues a ticket for the organizer and refunds it, so the
// `refunded` event is written by the real path.
func seedVoidedSmokeTicket(t *testing.T, ctx context.Context, st *store.Postgres, qr *ticket.Signer, organizer uuid.UUID) uuid.UUID {
	t.Helper()
	ticketID, orderID, slotID := uuid.New(), uuid.New(), uuid.New()
	issuedAt := time.Now().UTC()
	payload, err := qr.Payload(ticketID, orderID, organizer, slotID, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Issue(ctx, store.IssueInput{EventID: uuid.New(), Tickets: []store.Ticket{{
		ID: ticketID, OrderID: orderID, GuestOrderRef: uuid.New(), OrganizerID: organizer,
		BuyerID: uuid.New(), SlotID: slotID, TicketTypeID: uuid.New(), Payload: payload, IssuedAt: issuedAt,
	}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RefundOrderTickets(ctx, organizer, orderID, uuid.New(), 1); err != nil {
		t.Fatal(err)
	}
	return ticketID
}
