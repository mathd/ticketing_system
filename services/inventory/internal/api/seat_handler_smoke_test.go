//go:build smoke

package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"ticketing/services/inventory/internal/consumer"
	"ticketing/services/inventory/internal/store"
)

// recordingPinner is a fake SeatPinner: it records pin/unpin calls and can be told to
// fail the pin (deterministically or transiently) so the handler's compensation is tested
// without a running catalog.
type recordingPinner struct {
	pins, unpins int
	pinErr       error
	lastPinnedBy string
	lastSeats    []string
}

func (p *recordingPinner) PinSeats(_ context.Context, _, _ uuid.UUID, seats []string, pinnedBy string) error {
	p.pins++
	p.lastSeats = seats
	p.lastPinnedBy = pinnedBy
	return p.pinErr
}

func (p *recordingPinner) UnpinSeats(_ context.Context, _, _ uuid.UUID, _ []string, _ string) error {
	p.unpins++
	return nil
}

func seatAPIStore(t *testing.T) (*store.Postgres, uuid.UUID, uuid.UUID) {
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
	schema := "inventory_api_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
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
	org, slot, seatMap := uuid.New(), uuid.New(), uuid.New()
	if err = st.ProvisionSeated(ctx, uuid.New(), slot, org, seatMap, 100); err != nil {
		t.Fatal(err)
	}
	return st, org, slot
}

func postSeatHold(t *testing.T, srv http.Handler, org, slot uuid.UUID, seats []string, key string) *httptest.ResponseRecorder {
	return postSeatHoldTT(t, srv, org, slot, uuid.New(), seats, key)
}

func postSeatHoldTT(t *testing.T, srv http.Handler, org, slot, ticketType uuid.UUID, seats []string, key string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"organizer_id": org, "slot_id": slot, "seat_identities": seats,
		"ticket_type_id": ticketType, "unit_amount": 4500, "currency": "EUR",
	})
	req := httptest.NewRequest(http.MethodPost, "/holds/seats", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	res := httptest.NewRecorder()
	srv.ServeHTTP(res, req)
	return res
}

func TestCreateSeatHoldHappyPathPins(t *testing.T) {
	st, org, slot := seatAPIStore(t)
	pin := &recordingPinner{}
	srv := New(st, "", pin).Router(nil)

	res := postSeatHold(t, srv, org, slot, []string{"A/1/1", "A/1/2"}, "k1")
	if res.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s want 201", res.Code, res.Body.String())
	}
	var out struct {
		HoldID string   `json:"hold_id"`
		Seats  []string `json:"seats"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Seats) != 2 {
		t.Fatalf("seats = %v", out.Seats)
	}
	if pin.pins != 1 {
		t.Fatalf("pin calls = %d want 1", pin.pins)
	}
	if pin.lastPinnedBy != "hold:"+out.HoldID {
		t.Fatalf("pinned_by = %q want hold:%s", pin.lastPinnedBy, out.HoldID)
	}
}

func TestCreateSeatHoldDeterministicRejectReleasesAndBlocksReplay(t *testing.T) {
	st, org, slot := seatAPIStore(t)
	pin := &recordingPinner{pinErr: fmt.Errorf("nope: %w", consumer.ErrSeatPinRejected)}
	srv := New(st, "", pin).Router(nil)
	tt := uuid.New()

	res := postSeatHoldTT(t, srv, org, slot, tt, []string{"A/1/1"}, "k1")
	if res.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s want 409", res.Code, res.Body.String())
	}
	// The hold was released. A same-key + same-request retry finds a TERMINAL claim and is
	// rejected (409), never re-pinning a released hold as a false success.
	if res2 := postSeatHoldTT(t, srv, org, slot, tt, []string{"A/1/1"}, "k1"); res2.Code != http.StatusConflict {
		t.Fatalf("terminal replay = %d body=%s want 409", res2.Code, res2.Body.String())
	}
	// And the seat is genuinely free: a fresh key holds it (pinner now ok).
	pin.pinErr = nil
	if res3 := postSeatHold(t, srv, org, slot, []string{"A/1/1"}, "k2"); res3.Code != http.StatusCreated {
		t.Fatalf("re-hold after release = %d body=%s want 201", res3.Code, res3.Body.String())
	}
}

func TestCreateSeatHoldTransientFailKeepsHoldForRetry(t *testing.T) {
	st, org, slot := seatAPIStore(t)
	pin := &recordingPinner{pinErr: errors.New("connection reset")}
	srv := New(st, "", pin).Router(nil)
	tt := uuid.New()

	res := postSeatHoldTT(t, srv, org, slot, tt, []string{"A/1/1"}, "k1")
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s want 503", res.Code, res.Body.String())
	}
	// The hold is NOT released on a transient failure: a same-key retry re-pins it (the
	// replay-re-pin invariant) and returns 200, rather than freeing seats a concurrent
	// retry may have already pinned.
	pin.pinErr = nil
	res2 := postSeatHoldTT(t, srv, org, slot, tt, []string{"A/1/1"}, "k1")
	if res2.Code != http.StatusOK {
		t.Fatalf("same-key retry after transient = %d body=%s want 200", res2.Code, res2.Body.String())
	}
	if pin.pins != 2 {
		t.Fatalf("retry must re-pin the surviving hold: pin calls = %d want 2", pin.pins)
	}
}

// A seat that survives the OpenAPI minLength:1 check as whitespace is rejected by
// canonicalSeats and must surface as 400, not 500.
func TestCreateSeatHoldWhitespaceSeatIs400(t *testing.T) {
	st, org, slot := seatAPIStore(t)
	srv := New(st, "", &recordingPinner{}).Router(nil)
	res := postSeatHold(t, srv, org, slot, []string{" "}, "k1")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("whitespace seat = %d body=%s want 400", res.Code, res.Body.String())
	}
}

func TestCreateSeatHoldReplayRepins(t *testing.T) {
	st, org, slot := seatAPIStore(t)
	pin := &recordingPinner{}
	srv := New(st, "", pin).Router(nil)

	tt := uuid.New()
	if res := postSeatHoldTT(t, srv, org, slot, tt, []string{"A/1/1"}, "same"); res.Code != http.StatusCreated {
		t.Fatalf("first = %d want 201", res.Code)
	}
	// Same key + same request → replay (200); the pins are re-asserted (replay-re-pin).
	res := postSeatHoldTT(t, srv, org, slot, tt, []string{"A/1/1"}, "same")
	if res.Code != http.StatusOK {
		t.Fatalf("replay = %d body=%s want 200", res.Code, res.Body.String())
	}
	if pin.pins != 2 {
		t.Fatalf("pin calls = %d want 2 (replay must re-pin)", pin.pins)
	}
}
