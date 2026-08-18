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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/inventory/internal/store"
)

// bestAvailableAPIStore is seatAPIStore's sibling for an ADR-061-capable pool: rule on,
// adjacency present, and ordering metadata on every seat. A separate helper rather than a
// flag on the existing one, so the pre-ADR-061 pool that seatAPIStore builds stays exactly
// what it is — the shape the unsupported-refusal test needs.
func bestAvailableAPIStore(t *testing.T, seats int) (*store.Postgres, uuid.UUID, uuid.UUID, func(int) string) {
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
	schema := "inventory_ba_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
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
	seat := func(i int) string { return "A/1/" + strconv.Itoa(i) }
	rowKey := "A/1"
	adjacency := make([]store.SeatAdjacencyRow, 0, seats)
	for i := 1; i <= seats; i++ {
		pos := int32(i)
		key := rowKey
		row := store.SeatAdjacencyRow{SeatIdentity: seat(i), RowKey: &key, Position: &pos}
		if i > 1 {
			left := seat(i - 1)
			row.Left = &left
		}
		if i < seats {
			right := seat(i + 1)
			row.Right = &right
		}
		adjacency = append(adjacency, row)
	}
	if err = st.ProvisionSeated(ctx, uuid.New(), slot, org, seatMap, 1000, true, adjacency); err != nil {
		t.Fatal(err)
	}
	return st, org, slot, seat
}

func postBestAvailable(t *testing.T, srv http.Handler, org, slot uuid.UUID, count int, key string) *httptest.ResponseRecorder {
	t.Helper()
	return postBestAvailableTT(t, srv, org, slot, uuid.New(), count, key)
}

func postBestAvailableTT(t *testing.T, srv http.Handler, org, slot, ticketType uuid.UUID, count int, key string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"organizer_id": org, "slot_id": slot, "seat_count": count,
		"ticket_type_id": ticketType, "unit_amount": 4500, "currency": "EUR",
	})
	req := httptest.NewRequest(http.MethodPost, "/holds/seats/best-available", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	res := httptest.NewRecorder()
	srv.ServeHTTP(res, req)
	return res
}

// TestBestAvailableHandlerPinsWhatItSelected is the hold-then-pin obligation on a path
// where the caller never named the seats: the pin has to carry the seats the STORE chose,
// and the response has to report the same ones. Nothing upstream can check that for us —
// on the named-seat path the request itself is the cross-check, and here there is none.
func TestBestAvailableHandlerPinsWhatItSelected(t *testing.T) {
	st, org, slot, seat := bestAvailableAPIStore(t, 10)
	pin := &recordingPinner{}
	srv := New(st, "", pin).Router(nil, true)

	res := postBestAvailable(t, srv, org, slot, 3, "k1")
	if res.Code != 201 {
		t.Fatalf("status = %d body = %s", res.Code, res.Body.String())
	}
	var out struct {
		HoldID uuid.UUID `json:"hold_id"`
		Seats  []string  `json:"seats"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	want := []string{seat(1), seat(2), seat(3)}
	if strings.Join(out.Seats, ",") != strings.Join(want, ",") {
		t.Fatalf("response seats = %v want %v", out.Seats, want)
	}
	if pin.pins != 1 {
		t.Fatalf("pins = %d want 1", pin.pins)
	}
	if strings.Join(pin.lastSeats, ",") != strings.Join(out.Seats, ",") {
		t.Fatalf("pinned %v but answered %v — the pin must name the seats the response promises", pin.lastSeats, out.Seats)
	}
	if pin.lastPinnedBy != "hold:"+out.HoldID.String() {
		t.Fatalf("pinned_by = %q want hold:%s", pin.lastPinnedBy, out.HoldID)
	}
}

// TestBestAvailableHandlerSeparatesTheTwoRefusals is amendment A4 at the wire, which is the
// only tier where it means anything: the codes exist so a CALLER can tell a sellout from a
// pool that cannot serve this endpoint at all.
func TestBestAvailableHandlerSeparatesTheTwoRefusals(t *testing.T) {
	t.Run("unsupported pool", func(t *testing.T) {
		// seatAPIStore builds the pre-ADR-061 shape: seated, no ordering projection.
		st, org, slot := seatAPIStore(t)
		srv := New(st, "", &recordingPinner{}).Router(nil, true)

		res := postBestAvailable(t, srv, org, slot, 2, "k1")
		if res.Code != 409 {
			t.Fatalf("status = %d body = %s", res.Code, res.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out["code"] != "best_available_unsupported" {
			t.Fatalf("code = %v want best_available_unsupported — a pool with no projection is not a sellout", out["code"])
		}
		if _, named := out["seat_identities"]; named {
			t.Fatal("no seats were chosen, so none may be named")
		}
	})

	t.Run("no run long enough", func(t *testing.T) {
		st, org, slot, seat := bestAvailableAPIStore(t, 6)
		srv := New(st, "", &recordingPinner{}).Router(nil, true)
		// Break the row so the longest free run is 2.
		if res := postSeatHold(t, srv, org, slot, []string{seat(3)}, "seed"); res.Code != 201 {
			t.Fatalf("seeding: %d %s", res.Code, res.Body.String())
		}

		res := postBestAvailable(t, srv, org, slot, 4, "k1")
		if res.Code != 409 {
			t.Fatalf("status = %d body = %s", res.Code, res.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out["code"] != "best_available_unavailable" {
			t.Fatalf("code = %v want best_available_unavailable", out["code"])
		}
	})
}

// TestBestAvailableHandlerReplayRePinsTheOriginalSeats: the replay-re-pin invariant
// (ADR-031) on this path. A retry must return 200 with the SAME seats and pin them again —
// re-running selection would hand out a second run, and skipping the pin would let commerce
// hold a claim whose seats are unpinned.
func TestBestAvailableHandlerReplayRePinsTheOriginalSeats(t *testing.T) {
	st, org, slot, _ := bestAvailableAPIStore(t, 10)
	pin := &recordingPinner{}
	srv := New(st, "", pin).Router(nil, true)
	tt := uuid.New()

	first := postBestAvailableTT(t, srv, org, slot, tt, 2, "same")
	if first.Code != 201 {
		t.Fatalf("first: %d %s", first.Code, first.Body.String())
	}
	again := postBestAvailableTT(t, srv, org, slot, tt, 2, "same")
	if again.Code != 200 {
		t.Fatalf("replay status = %d want 200; body = %s", again.Code, again.Body.String())
	}
	var a, b struct {
		HoldID uuid.UUID `json:"hold_id"`
		Seats  []string  `json:"seats"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(again.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if a.HoldID != b.HoldID || strings.Join(a.Seats, ",") != strings.Join(b.Seats, ",") {
		t.Fatalf("replay returned %s/%v, original was %s/%v", b.HoldID, b.Seats, a.HoldID, a.Seats)
	}
	if pin.pins != 2 {
		t.Fatalf("pins = %d want 2 — a replay re-asserts the pin (ADR-031)", pin.pins)
	}
}

// TestBestAvailableHandlerRejectsAMalformedRequest pins the validation band at the edge.
// Each case is its own request, because an earlier refusal short-circuits the rest and a
// single request carrying several faults proves only that one of them was caught.
func TestBestAvailableHandlerRejectsAMalformedRequest(t *testing.T) {
	st, org, slot, _ := bestAvailableAPIStore(t, 10)
	srv := New(st, "", &recordingPinner{}).Router(nil, true)

	base := func() map[string]any {
		return map[string]any{"organizer_id": org, "slot_id": slot, "seat_count": 2,
			"ticket_type_id": uuid.New(), "unit_amount": 4500, "currency": "EUR"}
	}
	cases := map[string]func(map[string]any){
		"zero seats":      func(m map[string]any) { m["seat_count"] = 0 },
		"negative seats":  func(m map[string]any) { m["seat_count"] = -1 },
		"over the band":   func(m map[string]any) { m["seat_count"] = store.MaxSeatsPerHold + 1 },
		"no organizer":    func(m map[string]any) { m["organizer_id"] = uuid.Nil },
		"no slot":         func(m map[string]any) { m["slot_id"] = uuid.Nil },
		"no ticket type":  func(m map[string]any) { m["ticket_type_id"] = uuid.Nil },
		"wrong currency":  func(m map[string]any) { m["currency"] = "USD" },
		"negative amount": func(m map[string]any) { m["unit_amount"] = -1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := base()
			mutate(m)
			body, _ := json.Marshal(m)
			req := httptest.NewRequest(http.MethodPost, "/holds/seats/best-available", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "k-"+name)
			res := httptest.NewRecorder()
			srv.ServeHTTP(res, req)
			if res.Code != 400 {
				t.Fatalf("status = %d want 400; body = %s", res.Code, res.Body.String())
			}
		})
	}

	t.Run("no idempotency key", func(t *testing.T) {
		body, _ := json.Marshal(base())
		req := httptest.NewRequest(http.MethodPost, "/holds/seats/best-available", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		srv.ServeHTTP(res, req)
		if res.Code != 400 {
			t.Fatalf("status = %d want 400; body = %s", res.Code, res.Body.String())
		}
	})
}
