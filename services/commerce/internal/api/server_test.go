package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	commercestore "ticketing/services/commerce/internal/store"
	"ticketing/shared/fakepsp"
)

func TestPaymentFailureResponse(t *testing.T) {
	tests := []struct {
		name, body, fallback, wantStatus string
		wantReplay                       bool
	}{
		{name: "valid object", body: `{"status":"declined","replay":true,"reason":"card_declined","payment_id":"provider-secret"}`, fallback: "fallback", wantStatus: "declined", wantReplay: true},
		{name: "empty", body: ``, fallback: "timeout", wantStatus: "timeout"},
		{name: "malformed", body: `{`, fallback: "declined", wantStatus: "declined"},
		{name: "non object", body: `[]`, fallback: "timeout", wantStatus: "timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := paymentFailureResponse([]byte(tt.body), tt.fallback)
			if got["status"] != tt.wantStatus {
				t.Fatalf("status = %v, want %s", got["status"], tt.wantStatus)
			}
			if replay, ok := got["replay"]; ok != tt.wantReplay || (ok && replay != true) {
				t.Fatalf("replay = %v, present = %t, want replay = %t", replay, ok, tt.wantReplay)
			}
			if _, ok := got["reason"]; ok {
				t.Fatal("reason must not be exposed")
			}
			if _, ok := got["payment_id"]; ok {
				t.Fatal("payment_id must not be exposed")
			}
		})
	}
}

func TestReserveRejectsNonStrictJSON(t *testing.T) {
	s := New(nil, http.DefaultClient, "", "", "", "")
	valid := `{"organizer_id":"00000000-0000-0000-0000-000000000001","ticket_type_id":"00000000-0000-0000-0000-000000000002","quantity":1}`
	for name, body := range map[string]string{
		"unknown field":  strings.TrimSuffix(valid, "}") + `,"amount":1}`,
		"trailing value": valid + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/reservations", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "strict-json")
			res := httptest.NewRecorder()
			s.Router(nil, true).ServeHTTP(res, req)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want=%d", res.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestCheckoutClaimProblemDoesNotLeakDetails(t *testing.T) {
	code, message := checkoutClaimProblem(errCheckoutConflict)
	if code != http.StatusConflict || message != "checkout conflicts with an existing request" {
		t.Fatalf("conflict mapping = %d %q", code, message)
	}
	code, message = checkoutClaimProblem(errors.New("duplicate key value violates unique constraint orders_pkey"))
	if code != http.StatusInternalServerError || message != "persist checkout" {
		t.Fatalf("unexpected mapping = %d %q", code, message)
	}
}

func TestPaymentOutcomeProblem(t *testing.T) {
	code, _, active := paymentOutcomeProblem(http.StatusConflict)
	if !active || code != http.StatusConflict {
		t.Fatalf("conflict must be retryable: code=%d active=%t", code, active)
	}
	if _, _, active = paymentOutcomeProblem(http.StatusBadRequest); active {
		t.Fatal("bad request must not be treated as an active operation")
	}
}

func TestTerminalCheckoutCode(t *testing.T) {
	if got := terminalCheckoutCode("declined"); got != http.StatusPaymentRequired {
		t.Fatalf("declined code = %d, want %d", got, http.StatusPaymentRequired)
	}
	if got := terminalCheckoutCode("timeout"); got != http.StatusRequestTimeout {
		t.Fatalf("timeout code = %d, want %d", got, http.StatusRequestTimeout)
	}
	// A refunded order replays as a payment failure: the charge was captured then
	// compensated (TKT-115) — from the buyer's side the checkout did not buy a seat.
	if got := terminalCheckoutCode("refunded"); got != http.StatusPaymentRequired {
		t.Fatalf("refunded code = %d, want %d", got, http.StatusPaymentRequired)
	}
}

func TestPersistenceReadProblem(t *testing.T) {
	if code, message := persistenceReadProblem(sql.ErrNoRows); code != http.StatusNotFound || message != "not found" {
		t.Fatalf("not found mapping = %d %q", code, message)
	}
	if code, message := persistenceReadProblem(errors.New("database unavailable")); code != http.StatusServiceUnavailable || message != "temporarily unavailable" {
		t.Fatalf("database mapping = %d %q", code, message)
	}
}

func TestCheckoutRejectsUnknownPaymentToken(t *testing.T) {
	if fakepsp.ValidToken("not-a-token") {
		t.Fatal("unknown token accepted")
	}
}

func TestConvertOperationalRequiresInternalToken(t *testing.T) {
	body := `{"organizer_id":"00000000-0000-0000-0000-000000000001","ticket_type_id":"00000000-0000-0000-0000-000000000002","quantity":1,"actor":"staff:amy","reason":"walk-up"}`
	for name, s := range map[string]*Server{
		"wrong token": New(nil, http.DefaultClient, "", "", "", "secret"),
		// An unconfigured token must fail closed, never open.
		"empty configured token": New(nil, http.DefaultClient, "", "", "", ""),
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/internal/operational-holds/00000000-0000-0000-0000-000000000003/convert", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "k")
			req.Header.Set("X-Internal-Token", "wrong")
			res := httptest.NewRecorder()
			s.Router(nil, true).ServeHTTP(res, req)
			if res.Code != http.StatusNotFound {
				t.Fatalf("status=%d want=%d", res.Code, http.StatusNotFound)
			}
		})
	}
}

func TestConvertOperationalRejectsNonStrictJSON(t *testing.T) {
	s := New(nil, http.DefaultClient, "", "", "", "secret")
	valid := `{"organizer_id":"00000000-0000-0000-0000-000000000001","ticket_type_id":"00000000-0000-0000-0000-000000000002","quantity":1,"actor":"staff:amy","reason":"walk-up"}`
	for name, body := range map[string]string{
		"unknown field":  strings.TrimSuffix(valid, "}") + `,"unit_amount":1}`,
		"trailing value": valid + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/internal/operational-holds/00000000-0000-0000-0000-000000000003/convert", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "k")
			req.Header.Set("X-Internal-Token", "secret")
			res := httptest.NewRecorder()
			s.Router(nil, true).ServeHTTP(res, req)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want=%d body=%s", res.Code, http.StatusBadRequest, res.Body.String())
			}
		})
	}
}

func TestDrawDownGroupReservationRequiresInternalToken(t *testing.T) {
	body := `{"organizer_id":"00000000-0000-0000-0000-000000000001","ticket_type_id":"00000000-0000-0000-0000-000000000002","quantity":1,"actor":"staff:amy","reason":"batch"}`
	for name, s := range map[string]*Server{
		"wrong token":            New(nil, http.DefaultClient, "", "", "", "secret"),
		"empty configured token": New(nil, http.DefaultClient, "", "", "", ""),
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/internal/group-reservations/00000000-0000-0000-0000-000000000003/draw-down", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "k")
			req.Header.Set("X-Internal-Token", "wrong")
			res := httptest.NewRecorder()
			s.Router(nil, true).ServeHTTP(res, req)
			if res.Code != http.StatusNotFound {
				t.Fatalf("status=%d want=%d", res.Code, http.StatusNotFound)
			}
		})
	}
}

// The draw-down rides the same orchestration as the operational convert: catalog decides
// slot and price (the client cannot supply them), and the offer's slot is forwarded as
// inventory's locked precondition.
func TestDrawDownForwardsSlotPreconditionToInventory(t *testing.T) {
	org := "00000000-0000-0000-0000-000000000001"
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"00000000-0000-0000-0000-000000000002","organizer_id":"` + org + `","performance_id":"00000000-0000-0000-0000-000000000009","price":{"amount":2500,"currency":"EUR"}}`))
	}))
	defer catalog.Close()
	var forwardedSlot, calledPath string
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledPath = r.URL.Path
		var body struct {
			SlotID string `json:"slot_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		forwardedSlot = body.SlotID
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":"conflicting terminal state"}`))
	}))
	defer inventory.Close()
	s := New(nil, http.DefaultClient, catalog.URL, inventory.URL, "", "secret")
	body := `{"organizer_id":"` + org + `","ticket_type_id":"00000000-0000-0000-0000-000000000002","quantity":1,"actor":"staff:amy","reason":"batch"}`
	req := httptest.NewRequest(http.MethodPost, "/internal/group-reservations/00000000-0000-0000-0000-000000000003/draw-down", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "k")
	req.Header.Set("X-Internal-Token", "secret")
	res := httptest.NewRecorder()
	s.Router(nil, true).ServeHTTP(res, req)
	if res.Code != http.StatusConflict {
		t.Fatalf("status=%d want=%d body=%s", res.Code, http.StatusConflict, res.Body.String())
	}
	if calledPath != "/internal/group-reservations/00000000-0000-0000-0000-000000000003/draw-down" {
		t.Fatalf("inventory path = %q, want the draw-down operation", calledPath)
	}
	if forwardedSlot != "00000000-0000-0000-0000-000000000009" {
		t.Fatalf("forwarded slot_id = %q, want the offer's performance id", forwardedSlot)
	}
}

// Same lifecycle-not-timestamp replay rule as the operational convert (ADR-023); the
// shared helper must keep it for draw-downs.
func TestDrawDownReplayJudgedByChildLifecycle(t *testing.T) {
	org := "00000000-0000-0000-0000-000000000001"
	cases := map[string]struct {
		hold string
		want int
	}{
		"held past deadline": {hold: `"status":"held","expires_at":"2026-07-16T11:00:00Z","server_time":"2026-07-16T11:50:00Z"`, want: http.StatusConflict},
		"expired":            {hold: `"status":"expired","expires_at":"2026-07-16T11:00:00Z","server_time":"2026-07-16T11:50:00Z"`, want: http.StatusConflict},
		"unknown status":     {hold: `"status":"parked","expires_at":"2026-07-16T12:00:00Z","server_time":"2026-07-16T11:50:00Z"`, want: http.StatusBadGateway},
	}
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"00000000-0000-0000-0000-000000000002","organizer_id":"` + org + `","performance_id":"00000000-0000-0000-0000-000000000009","price":{"amount":2500,"currency":"EUR"}}`))
	}))
	defer catalog.Close()
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200) // replay
				_, _ = w.Write([]byte(`{"hold":{"hold_id":"00000000-0000-0000-0000-000000000007","organizer_id":"` + org + `","slot_id":"00000000-0000-0000-0000-000000000009","quantity":1,` + tc.hold + `},"source_id":"00000000-0000-0000-0000-000000000003","source_remaining":4,"source_status":"held"}`))
			}))
			defer inventory.Close()
			s := New(nil, http.DefaultClient, catalog.URL, inventory.URL, "", "secret")
			body := `{"organizer_id":"` + org + `","ticket_type_id":"00000000-0000-0000-0000-000000000002","quantity":1,"actor":"staff:amy","reason":"batch"}`
			req := httptest.NewRequest(http.MethodPost, "/internal/group-reservations/00000000-0000-0000-0000-000000000003/draw-down", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "k")
			req.Header.Set("X-Internal-Token", "secret")
			res := httptest.NewRecorder()
			s.Router(nil, true).ServeHTTP(res, req)
			if res.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", res.Code, tc.want, res.Body.String())
			}
		})
	}
}

func TestConvertOperationalForwardsSlotPreconditionToInventory(t *testing.T) {
	org := "00000000-0000-0000-0000-000000000001"
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The offer belongs to performance ...0009.
		_, _ = w.Write([]byte(`{"id":"00000000-0000-0000-0000-000000000002","organizer_id":"` + org + `","performance_id":"00000000-0000-0000-0000-000000000009","price":{"amount":2500,"currency":"EUR"}}`))
	}))
	defer catalog.Close()
	var forwardedSlot string
	// Behaves like real inventory: the hold's pool is ...0008, so any other expected
	// slot rejects BEFORE mutating — commerce must surface that as 409.
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SlotID string `json:"slot_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		forwardedSlot = body.SlotID
		w.Header().Set("Content-Type", "application/json")
		if body.SlotID != "00000000-0000-0000-0000-000000000008" {
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"error":"conflicting terminal state"}`))
			return
		}
		w.WriteHeader(201)
	}))
	defer inventory.Close()
	s := New(nil, http.DefaultClient, catalog.URL, inventory.URL, "", "secret")
	body := `{"organizer_id":"` + org + `","ticket_type_id":"00000000-0000-0000-0000-000000000002","quantity":1,"actor":"staff:amy","reason":"walk-up"}`
	req := httptest.NewRequest(http.MethodPost, "/internal/operational-holds/00000000-0000-0000-0000-000000000003/convert", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "k")
	req.Header.Set("X-Internal-Token", "secret")
	res := httptest.NewRecorder()
	s.Router(nil, true).ServeHTTP(res, req)
	if res.Code != http.StatusConflict {
		t.Fatalf("status=%d want=%d body=%s", res.Code, http.StatusConflict, res.Body.String())
	}
	if forwardedSlot != "00000000-0000-0000-0000-000000000009" {
		t.Fatalf("forwarded slot_id = %q, want the offer's performance id", forwardedSlot)
	}
}

func TestConvertOperationalReplayJudgedByChildLifecycle(t *testing.T) {
	// The guard must key on the child's status, not its timestamp: a confirmed claim
	// keeps its elapsed expires_at forever (409 would instruct a double carve), and a
	// released child with a future timestamp must not become a dead reservation.
	// The accept path for a confirmed child is exercised end-to-end in the smoke suite
	// (replay after checkout), where a real database exists.
	org := "00000000-0000-0000-0000-000000000001"
	cases := map[string]struct {
		hold string
		want int
	}{
		"held past deadline":        {hold: `"status":"held","expires_at":"2026-07-16T11:00:00Z","server_time":"2026-07-16T11:50:00Z"`, want: http.StatusConflict},
		"released, future deadline": {hold: `"status":"released","expires_at":"2026-07-16T12:00:00Z","server_time":"2026-07-16T11:50:00Z"`, want: http.StatusConflict},
		"expired":                   {hold: `"status":"expired","expires_at":"2026-07-16T11:00:00Z","server_time":"2026-07-16T11:50:00Z"`, want: http.StatusConflict},
		// Unknown is not terminal: a version-skew status must not advise re-conversion
		// (it could double-carve a still-live child) — it is an invalid response.
		"empty status":   {hold: `"status":"","expires_at":"2026-07-16T12:00:00Z","server_time":"2026-07-16T11:50:00Z"`, want: http.StatusBadGateway},
		"unknown status": {hold: `"status":"parked","expires_at":"2026-07-16T12:00:00Z","server_time":"2026-07-16T11:50:00Z"`, want: http.StatusBadGateway},
	}
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"00000000-0000-0000-0000-000000000002","organizer_id":"` + org + `","performance_id":"00000000-0000-0000-0000-000000000009","price":{"amount":2500,"currency":"EUR"}}`))
	}))
	defer catalog.Close()
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200) // replay
				_, _ = w.Write([]byte(`{"hold":{"hold_id":"00000000-0000-0000-0000-000000000007","organizer_id":"` + org + `","slot_id":"00000000-0000-0000-0000-000000000009","quantity":1,` + tc.hold + `},"source_id":"00000000-0000-0000-0000-000000000003","source_remaining":4,"source_status":"held"}`))
			}))
			defer inventory.Close()
			s := New(nil, http.DefaultClient, catalog.URL, inventory.URL, "", "secret")
			body := `{"organizer_id":"` + org + `","ticket_type_id":"00000000-0000-0000-0000-000000000002","quantity":1,"actor":"staff:amy","reason":"walk-up"}`
			req := httptest.NewRequest(http.MethodPost, "/internal/operational-holds/00000000-0000-0000-0000-000000000003/convert", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "k")
			req.Header.Set("X-Internal-Token", "secret")
			res := httptest.NewRecorder()
			s.Router(nil, true).ServeHTTP(res, req)
			if res.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", res.Code, tc.want, res.Body.String())
			}
		})
	}
}

// The orders status vocabulary, copied from the CHECK constraint in
// services/commerce/internal/store/migrations/0005_psp_recovery.sql. Pinned here on
// purpose: a status added to the constraint without a decision about what checkout tells
// a buyer whose guarded write lost the race to recovery is the F3 defect returning. The
// first cut answered only refunded/reconciliation_required and silently told buyers
// "payment unknown" for orders recovery had already completed or terminally failed.
func TestClassifyRecoveredCoversTheStatusVocabulary(t *testing.T) {
	want := map[string]recoveredClass{
		// Recovery reached a buyer-final answer — checkout must report it.
		"completed":               recoveredCompleted,
		"declined":                recoveredTerminal,
		"timeout":                 recoveredTerminal,
		"refunded":                recoveredTerminal,
		"reconciliation_required": recoveredReconciling,
		// The terminal outcome is already durable here — RecordTerminalOutcome writes it in
		// the same statement that sets the status — so calling this payment_unknown would
		// deny evidence the database holds (ai-review pass 3, P3-2).
		"release_pending": recoveredPending,
		// Genuinely still unresolved: the optimistic 202 is honest. These are exactly the
		// statuses the guarded write itself targets.
		"created":              recoveredOptimistic,
		"payment_unknown":      recoveredOptimistic,
		"confirmation_pending": recoveredOptimistic,
	}
	for status, class := range want {
		if got := classifyRecovered(status); got != class {
			t.Errorf("classifyRecovered(%q) = %d, want %d", status, got, class)
		}
	}
	// Guard the pin itself: if the vocabulary grows, this list must grow with it. Applied
	// goose migrations are immutable (ADR-022), so the vocabulary can only ever grow in a
	// NEW file — reading 0005 alone would keep passing forever while 0006 added a status
	// no one taught checkout to answer (ai-review pass 3, P3-4). Scan every migration in
	// order and take the LAST constraint definition, which is the one in force.
	migrations, err := filepath.Glob("../store/migrations/*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatalf("glob migrations: %v (%d found)", err, len(migrations))
	}
	// goose applies migrations in NUMERIC version order, so sorting the filenames as
	// strings would visit an unpadded 10_ before 9_ and read a superseded constraint as
	// the one in force (ai-review pass 4). Today's names are zero-padded, which hides
	// that — order by the version goose itself would use.
	version := func(path string) int {
		digits := regexp.MustCompile(`^\d+`).FindString(filepath.Base(path))
		n, _ := strconv.Atoi(digits)
		return n
	}
	sort.Slice(migrations, func(i, j int) bool { return version(migrations[i]) < version(migrations[j]) })

	// Track DROP as well as ADD, in the order they appear: a migration whose Up only drops
	// the constraint leaves NO vocabulary in force, and remembering the previous one would
	// validate a schema state the database no longer has (ai-review pass 4).
	constraint := regexp.MustCompile(`(?s)DROP CONSTRAINT orders_status_check|ADD CONSTRAINT orders_status_check CHECK \((.*?)\);`)
	var inForce string
	for _, file := range migrations {
		sql, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		// Up and Down both redefine the constraint; the Up block is everything before the
		// `-- +goose Down` marker, and only that is the forward vocabulary.
		up, _, _ := strings.Cut(string(sql), "-- +goose Down")
		for _, m := range constraint.FindAllStringSubmatch(up, -1) {
			if strings.HasPrefix(m[0], "DROP") {
				inForce = ""
				continue
			}
			inForce = m[1]
		}
	}
	if inForce == "" {
		t.Fatal("no orders_status_check constraint found in any migration; the guard below would be vacuous")
	}
	found := regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(inForce, -1)
	// Without this the loop below goes vacuous the moment the constraint is renamed,
	// and a test that cannot fail is worse than no test.
	if len(found) != len(want) {
		t.Fatalf("extracted %d statuses from the CHECK constraint in force, want %d", len(found), len(want))
	}
	for _, status := range found {
		if _, ok := want[status[1]]; !ok {
			t.Errorf("order status %q is in the CHECK constraint but has no checkout answer; "+
				"decide what a buyer racing recovery is told before adding it", status[1])
		}
	}
}

// TKT-156: the refund handler is thin because every DB-dependent decision lives in
// store.BindOrderRefund. What is left in the handler is this mapping, and it is worth a
// table because an unmapped store error would become a 500 for something the caller could
// have acted on — and because an UNDECLARED status becomes a 500 anyway under the
// fail-closed response validator (ADR-028), so the mapping must only produce statuses the
// contract declares.
func TestRefundProblemMapsEveryStoreError(t *testing.T) {
	declared := map[int]bool{400: true, 404: true, 409: true, 500: true, 502: true, 503: true}
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"not completed", commercestore.ErrOrderNotRefundable, http.StatusConflict},
		{"over refund", commercestore.ErrRefundExceedsOrder, http.StatusConflict},
		{"no money", commercestore.ErrRefundNoMoney, http.StatusConflict},
		{"key reused", commercestore.ErrRefundConflict, http.StatusConflict},
		{"unknown order", sql.ErrNoRows, http.StatusNotFound},
		{"anything else", errors.New("connection reset"), http.StatusInternalServerError},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			code, message := refundProblem(tt.err)
			if code != tt.code {
				t.Fatalf("code = %d, want %d", code, tt.code)
			}
			if !declared[code] {
				t.Fatalf("status %d is not declared by refundOrder; the validator turns it into a 500", code)
			}
			if strings.Contains(message, "connection reset") {
				t.Fatalf("message leaks the underlying error: %q", message)
			}
		})
	}
}

// The refund is internal-only: the gateway denies /internal/* at the edge, and commerce
// fails closed to 404 (not 401) exactly as staffSale and deliveryEmail do. An unconfigured
// token must not open the endpoint.
func TestRefundOrderRequiresInternalToken(t *testing.T) {
	for name, token := range map[string]string{"no token configured": "", "wrong token": "expected"} {
		t.Run(name, func(t *testing.T) {
			s := New(nil, http.DefaultClient, "", "", "", token)
			req := httptest.NewRequest(http.MethodPost, "/internal/orders/"+uuid.Nil.String()+"/refunds",
				bytes.NewBufferString(`{"organizer_id":"00000000-0000-0000-0000-000000000001","quantity":1,"actor":"a","reason":"r"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "refund-auth")
			res := httptest.NewRecorder()
			s.Router(nil, true).ServeHTTP(res, req)
			if res.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", res.Code)
			}
		})
	}
}

// TKT-158: same discipline as refundProblem — the handler stays thin, so what is left is
// this mapping, and every status it can produce must be one the contract declares (an
// undeclared status becomes a 500 under the fail-closed validator, ADR-028).
func TestExchangeProblemMapsEveryStoreError(t *testing.T) {
	declared := map[int]bool{400: true, 404: true, 409: true, 500: true, 502: true, 503: true}
	for name, tc := range map[string]struct {
		err  error
		code int
	}{
		"not exchangeable":  {commercestore.ErrOrderNotExchangeable, http.StatusConflict},
		"key reused":        {commercestore.ErrExchangeConflict, http.StatusConflict},
		"currency mismatch": {commercestore.ErrExchangeCurrencyMismatch, http.StatusConflict},
		"unknown order":     {sql.ErrNoRows, http.StatusNotFound},
		"anything else":     {errors.New("connection reset"), http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			code, message := exchangeProblem(tc.err)
			if code != tc.code {
				t.Fatalf("code = %d, want %d", code, tc.code)
			}
			if !declared[code] {
				t.Fatalf("status %d is not declared by exchangeOrder", code)
			}
			if strings.Contains(message, "connection reset") {
				t.Fatalf("message leaks the underlying error: %q", message)
			}
		})
	}
}

// The signed delta is the whole money story: one movement, or none. Pure arithmetic on
// persisted minor units, so it is worth pinning without a database.
func TestExchangeDeltaDirections(t *testing.T) {
	for name, tc := range map[string]struct{ source, target, want int64 }{
		"upgrade":   {5001, 6000, 999},
		"downgrade": {5001, 3000, -2001},
		"equal":     {5001, 5001, 0},
		"to free":   {5001, 0, -5001},
	} {
		t.Run(name, func(t *testing.T) {
			if got := commercestore.ExchangeDelta(tc.source, tc.target); got != tc.want {
				t.Fatalf("delta = %d, want %d", got, tc.want)
			}
		})
	}
}

// ai-review pass 4. The exchange hold is keyed on the exchange IDENTITY, so two concurrent
// requests sharing an idempotency key and target share the hold. On ErrExchangeConflict the
// OTHER request owns that exchange and is about to finalize this very claim — releasing it
// would break the winner and leave a durable exchange bound to a released claim. Every
// other bind error means nothing bound under this identity, so the hold is ours to return.
func TestShouldReleaseHoldOnBindError(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"conflict — the winner shares this hold": {commercestore.ErrExchangeConflict, false},
		"order already exchanged":                {commercestore.ErrOrderNotExchangeable, true},
		"unknown order":                          {sql.ErrNoRows, true},
		"anything else":                          {errors.New("connection reset"), true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := shouldReleaseHoldOnBindError(tc.err); got != tc.want {
				t.Fatalf("shouldRelease(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}
