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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	apispec "ticketing/services/access/api"
	"ticketing/services/access/internal/lifecycle"
	"ticketing/services/access/internal/store"
	"ticketing/services/access/internal/testdb"
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

// A revocation_refused item must reach the refusal record, never the admission
// path: the scanner did NOT let this holder in. The observable difference is the
// ticket itself. Had the handler reconciled the item as an admission, the ticket
// would be redeemed and the live scan after it would answer already_redeemed.
func TestReconcileRevocationRefusalIsNotAnAdmissionOnTheWire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	srv, st, qr := newSmokeServer(t, ctx)
	payload, organizer := issueSmokeTicket(t, ctx, st, qr)
	router := enrolled(srv, organizer).Router(nil, true)
	refused, live := uuid.NewString(), uuid.NewString()
	body := `{"occurrences":[{"qr_payload":` + mustJSON(t, payload) + `,"occurrence_id":"` + refused + `","occurred_at":"2026-07-17T09:00:00Z","local_decision":"revocation_refused"}]}`

	for _, want := range []string{"recorded", "synced"} {
		code, response := postJSON(t, router, "/scans/reconciliations", body)
		results, _ := response["results"].([]any)
		if code != http.StatusOK || len(results) != 1 {
			t.Fatalf("reconcile = %d %v", code, response)
		}
		entry, _ := results[0].(map[string]any)
		if entry["occurrence_id"] != refused || entry["result"] != want {
			t.Fatalf("entry = %v, want %s for %s", entry, want, refused)
		}
	}
	code, scan := postJSON(t, router, "/scans", `{"qr_payload":`+mustJSON(t, payload)+`,"occurrence_id":"`+live+`","occurred_at":"2026-07-17T09:10:00Z"}`)
	if code != http.StatusOK || scan["decision"] != "accepted" {
		t.Fatalf("live scan after a recorded refusal = %d %v, want accepted (the refusal admitted no one)", code, scan)
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

// --- TKT-557: the Router over a real store, a real driver and real faults ---
//
// ADR-085 §8 ticket 0. The store smoke tests prove the application type,
// site, class and cause. These prove what a scanner receives through
// Router(log, true) for the same real producers, and what the database holds
// afterwards, read back through the fixture's separate healthy handle.

const (
	smokePolicySQL     = "FROM slot_re_entry_policies"
	smokeTicketLockSQL = "FROM tickets WHERE id=$1 FOR UPDATE"
	smokeDeviceSQL     = "FROM scanner_devices"
	smokeTouchSQL      = "SET last_seen_at = now()"
)

type routerHarness struct {
	f         *testdb.Fixture
	st, seed  *store.Postgres
	qr        *ticket.Signer
	verifier  *ticket.Verifier
	organizer uuid.UUID
	device    store.ScannerDevice
	token     string
}

func newRouterHarness(t *testing.T, ctx context.Context, opts testdb.Options) *routerHarness {
	t.Helper()
	opts.Migrate = store.Migrate
	f := testdb.New(t, ctx, opts)
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
	cfg := store.Config{Signer: signer, Keyring: keyring, Policy: store.DefaultPolicy()}
	qrPub, qrPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	qr, err := ticket.New(base64.RawStdEncoding.EncodeToString(qrPriv.Seed()), "access-qr/test-v1")
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := ticket.NewVerifier("access-qr/test-v1="+base64.RawStdEncoding.EncodeToString(qrPub), "access-qr/test-v1")
	if err != nil {
		t.Fatal(err)
	}
	h := &routerHarness{f: f, st: store.New(f.DB, cfg), seed: store.New(f.Admin, cfg), qr: qr, verifier: verifier, organizer: uuid.New()}
	h.device, h.token, err = h.seed.EnrolScannerDevice(ctx, h.organizer, "gate")
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// server is the PRODUCTION construction: New wires the real store into the
// device port and the admission port.
func (h *routerHarness) server() *Server {
	return New(ServerConfig{Store: h.st, Verifier: h.verifier})
}

type smokeTicket struct {
	id, order, slot uuid.UUID
	payload         string
}

func (h *routerHarness) issue(t *testing.T, ctx context.Context, pass bool) smokeTicket {
	t.Helper()
	tk := smokeTicket{id: uuid.New(), order: uuid.New(), slot: uuid.New()}
	issuedAt := time.Now().UTC()
	var err error
	tk.payload, err = h.qr.Payload(tk.id, tk.order, h.organizer, tk.slot, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.seed.Issue(ctx, store.IssueInput{EventID: uuid.New(), Tickets: []store.Ticket{{
		ID: tk.id, OrderID: tk.order, GuestOrderRef: uuid.New(), OrganizerID: h.organizer,
		BuyerID: uuid.New(), SlotID: tk.slot, TicketTypeID: uuid.New(), Payload: tk.payload, IssuedAt: issuedAt,
	}}}); err != nil {
		t.Fatal(err)
	}
	if pass {
		if err := h.seed.UpsertSlotPolicy(ctx, uuid.New(), store.SlotPolicy{SlotID: tk.slot, OrganizerID: h.organizer, Policy: store.ReEntryPolicy{Mode: "multi"}}); err != nil {
			t.Fatal(err)
		}
	}
	return tk
}

func scanBody(t *testing.T, tk smokeTicket, occ string) string {
	t.Helper()
	return `{"qr_payload":` + mustJSON(t, tk.payload) + `,"occurrence_id":"` + occ + `","occurred_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
}

func (h *routerHarness) request(method, path, body string) *http.Request {
	var reader *bytes.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(scannerDeviceHeader, h.token)
	return req
}

func (h *routerHarness) serve(router http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// admissions counts admission facts for one ticket through the healthy
// handle: redeemed or entry events, and quarantine-side records.
func (h *routerHarness) admissions(t *testing.T, ctx context.Context, tk smokeTicket) int {
	t.Helper()
	var events, quarantined int
	if err := h.f.Admin.QueryRowContext(ctx, `SELECT count(*) FROM lifecycle_events WHERE ticket_id=$1 AND event_type IN ('redeemed','entry','exit','duplicate_admit')`, tk.id).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := h.f.Admin.QueryRowContext(ctx, `SELECT count(*) FROM lifecycle_integrity_quarantine WHERE ticket_id=$1`, tk.id).Scan(&quarantined); err != nil {
		t.Fatal(err)
	}
	return events + quarantined
}

func (h *routerHarness) occurrenceRecorded(t *testing.T, ctx context.Context, occ string) int {
	t.Helper()
	var n int
	if err := h.f.Admin.QueryRowContext(ctx, `SELECT count(*) FROM lifecycle_events WHERE id=$1`, occ).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

var smokeRoutes = []struct{ name, method, path, operationID string }{
	{"S", http.MethodPost, "/scans", "scanTicket"},
	{"R", http.MethodPost, "/scans/reconciliations", "reconcileScans"},
	{"F", http.MethodGet, "/scans/voided-tickets", "listVoidedTickets"},
}

func (h *routerHarness) routeBody(t *testing.T, route string, tk smokeTicket) string {
	t.Helper()
	switch route {
	case "S":
		return scanBody(t, tk, uuid.NewString())
	case "R":
		return `{"occurrences":[{"qr_payload":` + mustJSON(t, tk.payload) + `,"occurrence_id":"` + uuid.NewString() + `","occurred_at":"2026-08-10T10:00:00Z"}]}`
	}
	return ""
}

// The recovery-held PostgreSQL answers the device lookup's connect with a
// real 57P03: every protected route answers the lookup 503, nothing past the
// lookup runs, and the capture is a declared response.
func TestRouterLookupUnavailableOnEveryRouteThroughTheRealStore(t *testing.T) {
	for _, route := range smokeRoutes {
		t.Run(route.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newRouterHarness(t, ctx, testdb.Options{})
			testdb.RequireRecoveryRejects(t, ctx)
			tk := h.issue(t, ctx, false)
			router := h.server().Router(nil, true)
			h.f.Arm(testdb.FaultRecovery)
			rec := h.serve(router, h.request(route.method, route.path, h.routeBody(t, route.name, tk)))
			h.f.Disarm()
			requireExactJSON(t, rec, 503, map[string]any{"error": "scanner device lookup unavailable"})
			if err := validateCaptured(t, apispec.Spec, route.method, route.path, rec); err != nil {
				t.Errorf("capture violates %s: %v", route.operationID, err)
			}
			// The failed connect ran no statement at all: no Touch, no
			// decision, no feed read.
			if q := h.f.Queries(); len(q) != 0 {
				t.Fatalf("%d statements ran after an unavailable lookup: %v", len(q), q[0].SQL)
			}
			if n := h.admissions(t, ctx, tk); n != 0 {
				t.Fatalf("%d admission facts written", n)
			}
		})
	}
}

// The real 57P03 at the live policy read and at both acquisitions: the scan
// answers 503 "scanner store unavailable" and writes nothing.
func TestRouterStoreUnavailableAtPolicyAndBegin(t *testing.T) {
	for _, tc := range []struct {
		site, after string
		pass        bool
	}{
		{"policy", smokeTouchSQL, false},
		{"single begin", smokePolicySQL, false},
		{"pass begin", smokePolicySQL, true},
	} {
		t.Run(tc.site, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newRouterHarness(t, ctx, testdb.Options{})
			testdb.RequireRecoveryRejects(t, ctx)
			tk := h.issue(t, ctx, tc.pass)
			h.f.ArmAfter(tc.after, testdb.FaultRecovery)
			rec := h.serve(h.server().Router(nil, true), h.request(http.MethodPost, "/scans", scanBody(t, tk, uuid.NewString())))
			h.f.Disarm()
			requireExactJSON(t, rec, 503, map[string]any{"error": "scanner store unavailable"})
			if err := validateCaptured(t, apispec.Spec, http.MethodPost, "/scans", rec); err != nil {
				t.Errorf("capture violates scanTicket: %v", err)
			}
			if n := h.f.CountQueries(smokeTicketLockSQL); n != 0 {
				t.Fatalf("%d ticket decision statements ran", n)
			}
			if n := h.admissions(t, ctx, tk); n != 0 {
				t.Fatalf("%d admission facts written", n)
			}
		})
	}
}

// D39 expiry at each site through the Router: a real backlog-full dial
// blocks, the production step timer fires, and the fixed 503 answers.
func TestRouterStepExpiryAtEverySite(t *testing.T) {
	for _, tc := range []struct {
		site, after string
		pass        bool
		body        map[string]any
	}{
		{"device", "", false, map[string]any{"error": "scanner device lookup unavailable"}},
		{"policy", smokeTouchSQL, false, map[string]any{"error": "scanner store unavailable"}},
		{"single begin", smokePolicySQL, false, map[string]any{"error": "scanner store unavailable"}},
		{"pass begin", smokePolicySQL, true, map[string]any{"error": "scanner store unavailable"}},
	} {
		t.Run(tc.site, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newRouterHarness(t, ctx, testdb.Options{})
			tk := h.issue(t, ctx, tc.pass)
			if tc.after == "" {
				h.f.Arm(testdb.FaultBlockedDial)
			} else {
				h.f.ArmAfter(tc.after, testdb.FaultBlockedDial)
			}
			start := time.Now()
			rec := h.serve(h.server().Router(nil, true), h.request(http.MethodPost, "/scans", scanBody(t, tk, uuid.NewString())))
			elapsed := time.Since(start)
			h.f.Disarm()
			requireExactJSON(t, rec, 503, tc.body)
			if elapsed >= 3*time.Second {
				t.Fatalf("the 503 took %s", elapsed)
			}
			if h.f.BlockedDials() < 1 {
				t.Fatal("the blocked dial was never entered")
			}
			if n := h.admissions(t, ctx, tk); n != 0 {
				t.Fatalf("%d admission facts written", n)
			}
			t.Logf("TKT557-LATENCY site=router-%s class=step_expired ms=%.1f", strings.ReplaceAll(tc.site, " ", "_"), float64(elapsed.Microseconds())/1000)
		})
	}
}

// Negative control: a wrong database password is a configuration fault, not
// availability. At every site the real 28P01 answers 500, never 503, and no
// ticket decision runs.
func TestRouterWrongDatabasePasswordIs500(t *testing.T) {
	for _, tc := range []struct {
		site, after string
		pass        bool
		body        map[string]any
	}{
		{"device", "", false, map[string]any{"error": "scanner device lookup failed"}},
		{"policy", smokeTouchSQL, false, map[string]any{"error": "redeem ticket"}},
		{"single begin", smokePolicySQL, false, map[string]any{"error": "redeem ticket"}},
		{"pass begin", smokePolicySQL, true, map[string]any{"error": "redeem ticket"}},
	} {
		t.Run(tc.site, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newRouterHarness(t, ctx, testdb.Options{})
			tk := h.issue(t, ctx, tc.pass)
			if tc.after == "" {
				h.f.Arm(testdb.FaultWrongPassword)
			} else {
				h.f.ArmAfter(tc.after, testdb.FaultWrongPassword)
			}
			rec := h.serve(h.server().Router(nil, true), h.request(http.MethodPost, "/scans", scanBody(t, tk, uuid.NewString())))
			h.f.Disarm()
			requireExactJSON(t, rec, 500, tc.body)
			if err := validateCaptured(t, apispec.Spec, http.MethodPost, "/scans", rec); err != nil {
				t.Errorf("capture violates scanTicket: %v", err)
			}
			if s := h.f.Stats(); s.Faulted < 1 {
				t.Fatal("no connect attempt met the wrong password")
			}
			if n := h.f.CountQueries(smokeTicketLockSQL); n != 0 {
				t.Fatalf("%d ticket decision statements ran", n)
			}
			if n := h.admissions(t, ctx, tk); n != 0 {
				t.Fatalf("%d admission facts written", n)
			}
		})
	}
}

func holdRowLock(t *testing.T, ctx context.Context, admin *sql.DB, query string, args ...any) *sql.Tx {
	t.Helper()
	tx, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

// D14 (a): the decision's FOR UPDATE waits on a held ticket row AFTER
// handoff, then the request deadline passes. That deadline is after the
// decision began: exact 500, never 503.
func TestRouterLaterTicketLockDeadlineIs500(t *testing.T) {
	for _, pass := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "pass"}[pass], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newRouterHarness(t, ctx, testdb.Options{})
			tk := h.issue(t, ctx, pass)
			lock := holdRowLock(t, ctx, h.f.Admin, `SELECT 1 FROM tickets WHERE id=$1 FOR UPDATE`, tk.id)
			reqCtx, reqCancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer reqCancel()
			done := make(chan *httptest.ResponseRecorder, 1)
			router := h.server().Router(nil, true)
			go func() {
				done <- h.serve(router, h.request(http.MethodPost, "/scans", scanBody(t, tk, uuid.NewString())).WithContext(reqCtx))
			}()
			if !h.f.WaitForLockWait(ctx, "for update", time.Second) {
				t.Fatal("the decision's FOR UPDATE was never seen waiting")
			}
			rec := <-done
			_ = lock.Rollback()
			requireExactJSON(t, rec, 500, map[string]any{"error": "redeem ticket"})
			if n := h.admissions(t, ctx, tk); n != 0 {
				t.Fatalf("%d admission facts written", n)
			}
		})
	}
}

// D14 (b): the transaction's backend is terminated while it waits at the
// ticket read. Its real 57P01 is availability at a pre-decision site; here,
// after handoff, it is an exact 500.
func TestRouterLaterBackendLossIs500(t *testing.T) {
	for _, pass := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "pass"}[pass], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newRouterHarness(t, ctx, testdb.Options{})
			tk := h.issue(t, ctx, pass)
			lock := holdRowLock(t, ctx, h.f.Admin, `SELECT 1 FROM tickets WHERE id=$1 FOR UPDATE`, tk.id)
			var pid uint32
			var mu sync.Mutex
			h.f.Once(testdb.AtStart, smokeTicketLockSQL, func(e testdb.QueryEvent) { mu.Lock(); pid = e.PID; mu.Unlock() })
			done := make(chan *httptest.ResponseRecorder, 1)
			router := h.server().Router(nil, true)
			go func() {
				done <- h.serve(router, h.request(http.MethodPost, "/scans", scanBody(t, tk, uuid.NewString())))
			}()
			if !h.f.WaitForLockWait(ctx, "for update", 2*time.Second) {
				t.Fatal("the decision's FOR UPDATE was never seen waiting")
			}
			mu.Lock()
			target := pid
			mu.Unlock()
			h.f.Terminate(ctx, int(target))
			rec := <-done
			_ = lock.Rollback()
			requireExactJSON(t, rec, 500, map[string]any{"error": "redeem ticket"})
			if n := h.admissions(t, ctx, tk); n != 0 {
				t.Fatalf("%d admission facts written", n)
			}
		})
	}
}

// A later statement fails (an immediate test-only trigger on the admission
// insert), and a commit fails (a deferred test-only constraint trigger): both
// are after handoff, both exact 500, and nothing is kept.
func TestRouterLaterStatementAndCommitFailuresAre500(t *testing.T) {
	for _, tc := range []struct{ name, timing string }{
		{"later statement", ""},
		{"commit", "DEFERRABLE INITIALLY DEFERRED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newRouterHarness(t, ctx, testdb.Options{})
			tk := h.issue(t, ctx, false)
			if _, err := h.f.Admin.ExecContext(ctx, `CREATE FUNCTION tkt557_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'tkt557 test-only failure'; END $$`); err != nil {
				t.Fatal(err)
			}
			if _, err := h.f.Admin.ExecContext(ctx, `CREATE CONSTRAINT TRIGGER tkt557_fail AFTER INSERT ON lifecycle_events `+tc.timing+
				` FOR EACH ROW WHEN (NEW.ticket_id = '`+tk.id.String()+`' AND NEW.event_type = 'redeemed') EXECUTE FUNCTION tkt557_fail()`); err != nil {
				t.Fatal(err)
			}
			rec := h.serve(h.server().Router(nil, true), h.request(http.MethodPost, "/scans", scanBody(t, tk, uuid.NewString())))
			requireExactJSON(t, rec, 500, map[string]any{"error": "redeem ticket"})
			if tc.timing != "" {
				reached := false
				for _, q := range h.f.Queries() {
					if strings.EqualFold(strings.TrimSpace(q.SQL), "commit") && q.Err != nil {
						reached = true
					}
				}
				if !reached {
					t.Fatal("the commit was never reached: this fixture did not fail at commit")
				}
			}
			if n := h.admissions(t, ctx, tk); n != 0 {
				t.Fatalf("%d admission facts kept", n)
			}
		})
	}
}

// A reconciliation batch commits row 1 in its own transaction, then row 2
// fails at the store: exact 500, no batch-wide 503, and row 1 stays.
func TestRouterPartialReconcileBatchIs500WithRowOneCommitted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h := newRouterHarness(t, ctx, testdb.Options{})
	first, second := h.issue(t, ctx, false), h.issue(t, ctx, false)
	if _, err := h.f.Admin.ExecContext(ctx, `CREATE FUNCTION tkt557_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'tkt557 test-only failure'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.f.Admin.ExecContext(ctx, `CREATE TRIGGER tkt557_fail BEFORE INSERT ON lifecycle_events FOR EACH ROW WHEN (NEW.ticket_id = '`+second.id.String()+`') EXECUTE FUNCTION tkt557_fail()`); err != nil {
		t.Fatal(err)
	}
	occ1, occ2 := uuid.NewString(), uuid.NewString()
	body := `{"occurrences":[` +
		`{"qr_payload":` + mustJSON(t, first.payload) + `,"occurrence_id":"` + occ1 + `","occurred_at":"2026-08-10T10:00:00Z"},` +
		`{"qr_payload":` + mustJSON(t, second.payload) + `,"occurrence_id":"` + occ2 + `","occurred_at":"2026-08-10T10:01:00Z"}]}`
	rec := h.serve(h.server().Router(nil, true), h.request(http.MethodPost, "/scans/reconciliations", body))
	requireExactJSON(t, rec, 500, map[string]any{"error": "reconcile admissions"})
	if n := h.occurrenceRecorded(t, ctx, occ1); n != 1 {
		t.Fatalf("row 1 occurrence events = %d, want its committed one", n)
	}
	if n := h.occurrenceRecorded(t, ctx, occ2); n != 0 {
		t.Fatalf("row 2 occurrence events = %d, want none", n)
	}
}

// panicAfterScan calls the REAL store, keeps what it decided, then panics
// before the handler can map it.
type panicAfterScan struct {
	inner  scannerAdmissionStore
	mu     sync.Mutex
	result store.RedeemResult
	err    error
	called bool
}

func (p *panicAfterScan) Scan(ctx context.Context, in store.ScanInput) (store.RedeemResult, error) {
	result, err := p.inner.Scan(ctx, in)
	p.mu.Lock()
	p.result, p.err, p.called = result, err, true
	p.mu.Unlock()
	panic("after the real store decided")
}

func (p *panicAfterScan) ReconcileAdmission(ctx context.Context, in store.ReconcileOccurrence) (store.ReconcileResult, error) {
	return p.inner.ReconcileAdmission(ctx, in)
}

func (p *panicAfterScan) RecordRevocationRefusal(ctx context.Context, ticketID, orderID, organizerID, slotID, occurrenceID uuid.UUID, occurredAt time.Time, decision string) (store.ReconcileResult, error) {
	return p.inner.RecordRevocationRefusal(ctx, ticketID, orderID, organizerID, slotID, occurrenceID, occurredAt, decision)
}

// A real committed acceptance and a real decided refusal, each followed by a
// panic: the scanner gets the route-neutral 500, and the database still
// holds exactly what the store decided. A 500 here does not mean "nothing
// happened".
func TestRouterRecoversAPanicAfterARealDecision(t *testing.T) {
	t.Run("committed acceptance", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		h := newRouterHarness(t, ctx, testdb.Options{})
		tk := h.issue(t, ctx, false)
		wrapper := &panicAfterScan{inner: h.st}
		occ := uuid.NewString()
		rec := h.serve(h.server().WithAdmissions(wrapper).Router(nil, true), h.request(http.MethodPost, "/scans", scanBody(t, tk, occ)))
		requireExactJSON(t, rec, 500, map[string]any{"error": "request failed"})
		if err := validateCaptured(t, apispec.Spec, http.MethodPost, "/scans", rec); err != nil {
			t.Errorf("capture violates scanTicket: %v", err)
		}
		if !wrapper.called || wrapper.err != nil || !wrapper.result.Accepted {
			t.Fatalf("the real store did not accept first: called=%v result=%+v err=%v", wrapper.called, wrapper.result, wrapper.err)
		}
		if n := h.occurrenceRecorded(t, ctx, occ); n != 1 {
			t.Fatalf("committed occurrence events = %d, want exactly the one the store committed", n)
		}
		if n := h.admissions(t, ctx, tk); n != 1 {
			t.Fatalf("admission facts = %d, want 1", n)
		}
	})
	t.Run("decided refusal", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		h := newRouterHarness(t, ctx, testdb.Options{})
		tk := h.issue(t, ctx, false)
		first := uuid.NewString()
		if rec := h.serve(h.server().Router(nil, true), h.request(http.MethodPost, "/scans", scanBody(t, tk, first))); rec.Code != http.StatusOK {
			t.Fatalf("first scan = %d %s", rec.Code, rec.Body.String())
		}
		wrapper := &panicAfterScan{inner: h.st}
		second := uuid.NewString()
		rec := h.serve(h.server().WithAdmissions(wrapper).Router(nil, true), h.request(http.MethodPost, "/scans", scanBody(t, tk, second)))
		requireExactJSON(t, rec, 500, map[string]any{"error": "request failed"})
		if !wrapper.called || wrapper.err != nil || wrapper.result.Accepted || wrapper.result.Decision != store.DecisionAlreadyRedeemed {
			t.Fatalf("the real store did not refuse first: called=%v result=%+v err=%v", wrapper.called, wrapper.result, wrapper.err)
		}
		if n := h.occurrenceRecorded(t, ctx, second); n != 0 {
			t.Fatalf("the refused occurrence wrote %d events", n)
		}
		if n := h.admissions(t, ctx, tk); n != 1 {
			t.Fatalf("admission facts = %d, want only the first", n)
		}
	})
}

// panicAfterLookup runs the REAL device lookup, keeps it, then panics.
type panicAfterLookup struct {
	inner   scannerDeviceStore
	mu      sync.Mutex
	found   store.ScannerDevice
	touches int
}

func (p *panicAfterLookup) AuthenticateScannerDevice(ctx context.Context, token string) (store.ScannerDevice, error) {
	device, err := p.inner.AuthenticateScannerDevice(ctx, token)
	if err != nil {
		return device, err
	}
	p.mu.Lock()
	p.found = device
	p.mu.Unlock()
	panic("after the real lookup")
}

func (p *panicAfterLookup) TouchScannerDevice(ctx context.Context, id uuid.UUID) {
	p.mu.Lock()
	p.touches++
	p.mu.Unlock()
	p.inner.TouchScannerDevice(ctx, id)
}

// The security hook panics after a real successful lookup on every route: the
// 500 is recovered and nothing past the hook runs.
func TestRouterRecoversAHookPanicAfterARealLookup(t *testing.T) {
	for _, route := range smokeRoutes {
		t.Run(route.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h := newRouterHarness(t, ctx, testdb.Options{})
			tk := h.issue(t, ctx, false)
			devices := &panicAfterLookup{inner: h.st}
			admissions := &countingAdmissions{}
			rec := h.serve(h.server().WithScannerDevices(devices).WithAdmissions(admissions).Router(nil, true),
				h.request(route.method, route.path, h.routeBody(t, route.name, tk)))
			requireExactJSON(t, rec, 500, map[string]any{"error": "request failed"})
			if err := validateCaptured(t, apispec.Spec, route.method, route.path, rec); err != nil {
				t.Errorf("capture violates %s: %v", route.operationID, err)
			}
			if devices.found.ID != h.device.ID {
				t.Fatalf("the real lookup did not complete before the panic: %+v", devices.found)
			}
			if devices.touches != 0 {
				t.Fatal("Touch ran after the hook panic")
			}
			if scans, reconciles, refusals := admissions.counts(); scans+reconciles+refusals != 0 {
				t.Fatal("a handler ran after the hook panic")
			}
			for _, q := range h.f.Queries() {
				if !strings.Contains(q.SQL, smokeDeviceSQL) {
					t.Fatalf("a statement after the lookup ran: %s", q.SQL)
				}
			}
		})
	}
}

// Touch meets a held device row lock: authentication succeeds, Touch skips
// the row at once, and the scan answers normally.
func TestRouterTouchSkipsAHeldRowLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h := newRouterHarness(t, ctx, testdb.Options{})
	tk := h.issue(t, ctx, false)
	seeded := time.Date(2026, 10, 17, 3, 4, 5, 123456000, time.UTC)
	if _, err := h.f.Admin.ExecContext(ctx, `UPDATE scanner_devices SET last_seen_at=$1 WHERE id=$2`, seeded, h.device.ID); err != nil {
		t.Fatal(err)
	}
	lock := holdRowLock(t, ctx, h.f.Admin, `SELECT id FROM scanner_devices WHERE id=$1 FOR UPDATE`, h.device.ID)
	occ := uuid.NewString()
	rec := h.serve(h.server().Router(nil, true), h.request(http.MethodPost, "/scans", scanBody(t, tk, occ)))
	_ = lock.Rollback()
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"decision":"accepted"`) {
		t.Fatalf("scan = %d %s, want the normal acceptance", rec.Code, rec.Body.String())
	}
	var touch *testdb.QueryEvent
	for _, q := range h.f.Queries() {
		if strings.Contains(q.SQL, smokeTouchSQL) {
			q := q
			touch = &q
		}
	}
	if touch == nil || !touch.Done || touch.Err != nil || touch.Rows != 0 {
		t.Fatalf("Touch = %+v, want one finished statement with zero rows and no error", touch)
	}
	if took := touch.End.Sub(touch.Start); took >= 350*time.Millisecond {
		t.Fatalf("Touch took %s under a held row lock", took)
	}
	var seen time.Time
	if err := h.f.Admin.QueryRowContext(ctx, `SELECT last_seen_at FROM scanner_devices WHERE id=$1`, h.device.ID).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	if !seen.Equal(seeded) {
		t.Fatalf("last_seen_at = %v, want the exact seeded %v", seen, seeded)
	}
}

// timedTouch measures the REAL Touch and runs a hook as soon as it returns.
type timedTouch struct {
	scannerDeviceStore
	after   func()
	elapsed time.Duration
}

func (t *timedTouch) TouchScannerDevice(ctx context.Context, id uuid.UUID) {
	start := time.Now()
	t.scannerDeviceStore.TouchScannerDevice(ctx, id)
	t.elapsed = time.Since(start)
	t.after()
}

// POOL-WAIT FIXTURE (MaxOpen=1). Real authentication first; from the
// post-lookup barrier a control session holds the only connection until
// Touch returns, then releases it before the policy read. Touch returns
// inside an independent 750 ms bound and the scan result is unchanged.
func TestRouterTouchPoolWaitIsBoundedAndTheScanIsUnchanged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h := newRouterHarness(t, ctx, testdb.Options{MaxOpen: 1})
	tk := h.issue(t, ctx, false)
	release := make(chan struct{})
	var once sync.Once
	releaseHold := func() { once.Do(func() { close(release) }) }
	defer releaseHold()
	var held sync.WaitGroup
	h.f.Once(testdb.AtEnd, smokeDeviceSQL, func(testdb.QueryEvent) {
		before := h.f.DB.Stats().WaitCount
		held.Add(1)
		go func() {
			defer held.Done()
			conn, err := h.f.DB.Conn(context.Background())
			if err != nil {
				t.Errorf("hold the pool: %v", err)
				return
			}
			<-release
			_ = conn.Close()
		}()
		deadline := time.Now().Add(2 * time.Second)
		for h.f.DB.Stats().WaitCount == before && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	})
	touch := &timedTouch{scannerDeviceStore: h.st, after: releaseHold}
	rec := h.serve(h.server().WithScannerDevices(touch).Router(nil, true), h.request(http.MethodPost, "/scans", scanBody(t, tk, uuid.NewString())))
	held.Wait()
	if touch.elapsed >= 750*time.Millisecond || touch.elapsed < 500*time.Millisecond {
		t.Fatalf("Touch took %s, want within [500ms, 750ms): bounded by its step, and it did wait", touch.elapsed)
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"decision":"accepted"`) {
		t.Fatalf("scan = %d %s, want the unchanged acceptance", rec.Code, rec.Body.String())
	}
	t.Logf("TKT557-TOUCH router-pool-wait elapsed_ms=%d", touch.elapsed.Milliseconds())
}

// A table lock that blocks only writers reaches the Touch UPDATE itself:
// Touch waits, its step ends it, and the scan continues with its normal
// answer. The device row is read, not written, so the lookup succeeds.
func TestRouterTouchTableLockIsAdvisory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h := newRouterHarness(t, ctx, testdb.Options{})
	tk := h.issue(t, ctx, false)
	lock := holdRowLock(t, ctx, h.f.Admin, `LOCK TABLE scanner_devices IN EXCLUSIVE MODE`)
	done := make(chan *httptest.ResponseRecorder, 1)
	router := h.server().Router(nil, true)
	go func() {
		done <- h.serve(router, h.request(http.MethodPost, "/scans", scanBody(t, tk, uuid.NewString())))
	}()
	if !h.f.WaitForLockWait(ctx, "scanner_devices", 450*time.Millisecond) {
		t.Fatal("Touch was never seen waiting on the table lock")
	}
	rec := <-done
	_ = lock.Rollback()
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"decision":"accepted"`) {
		t.Fatalf("scan = %d %s, want the normal acceptance after an advisory Touch expiry", rec.Code, rec.Body.String())
	}
	var seen *time.Time
	if err := h.f.Admin.QueryRowContext(ctx, `SELECT last_seen_at FROM scanner_devices WHERE id=$1`, h.device.ID).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	if seen != nil {
		t.Fatalf("an expired Touch updated last_seen_at: %v", seen)
	}
}

// Credential mismatch and occurrence collision keep their exact 422s through
// the real store.
func TestRouterCredentialAndCollisionStay422(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h := newRouterHarness(t, ctx, testdb.Options{})
	router := h.server().Router(nil, true)
	// A signed credential for this organizer whose ticket was never issued.
	ghost, err := h.qr.Payload(uuid.New(), uuid.New(), h.organizer, uuid.New(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	rec := h.serve(router, h.request(http.MethodPost, "/scans", scanBody(t, smokeTicket{payload: ghost}, uuid.NewString())))
	requireExactJSON(t, rec, 422, map[string]any{"decision": "rejected", "reason": "invalid_credential"})
	// One occurrence id on two different tickets.
	a, b := h.issue(t, ctx, false), h.issue(t, ctx, false)
	occ := uuid.NewString()
	if rec := h.serve(router, h.request(http.MethodPost, "/scans", scanBody(t, a, occ))); rec.Code != http.StatusOK {
		t.Fatalf("first scan = %d %s", rec.Code, rec.Body.String())
	}
	rec = h.serve(router, h.request(http.MethodPost, "/scans", scanBody(t, b, occ)))
	requireExactJSON(t, rec, 422, map[string]any{"decision": "rejected", "reason": "occurrence_collision"})
}
