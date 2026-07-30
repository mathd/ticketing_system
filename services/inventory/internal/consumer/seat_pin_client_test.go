package consumer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// TestPinSeatsClassifiesStatus pins the client's status→error classification (TKT-80):
// only 409 is a deterministic seat rejection (ErrSeatPinRejected, the caller releases);
// every other non-200 is transient (an error, but NOT ErrSeatPinRejected, so the caller
// keeps the hold and retries).
func TestPinSeatsClassifiesStatus(t *testing.T) {
	cases := []struct {
		status       int
		wantErr      bool
		wantRejected bool
	}{
		{http.StatusOK, false, false},
		{http.StatusConflict, true, true},      // seat not in current version → deterministic
		{http.StatusServiceUnavailable, true, false}, // transient
		{http.StatusUnauthorized, true, false},       // token rotation → transient, must not release
		{http.StatusNotFound, true, false},           // map lookup → transient
		{http.StatusTooManyRequests, true, false},    // throttle → transient
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.status)
		}))
		r := NewCatalogResolver(srv.URL, "tok", srv.Client())
		err := r.PinSeats(context.Background(), uuid.New(), uuid.New(), []string{"A/1/1"}, "hold:x")
		srv.Close()
		if (err != nil) != c.wantErr {
			t.Fatalf("status %d: err=%v wantErr=%v", c.status, err, c.wantErr)
		}
		if errors.Is(err, ErrSeatPinRejected) != c.wantRejected {
			t.Fatalf("status %d: rejected=%v want %v (err=%v)", c.status, errors.Is(err, ErrSeatPinRejected), c.wantRejected, err)
		}
	}
}

// An empty seat set is a no-op (no HTTP call, no error).
func TestPinSeatsEmptyIsNoop(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true }))
	defer srv.Close()
	r := NewCatalogResolver(srv.URL, "tok", srv.Client())
	if err := r.PinSeats(context.Background(), uuid.New(), uuid.New(), nil, "hold:x"); err != nil {
		t.Fatalf("empty pin: %v", err)
	}
	if called {
		t.Fatal("empty seat set must not make an HTTP call")
	}
}

// pinListServer serves one canned body and records what was asked for.
func pinListServer(t *testing.T, status int, body string) (*CatalogResolver, *[]string) {
	t.Helper()
	asked := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Internal-Token") != "tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		*asked = append(*asked, r.URL.Path+"?"+r.URL.RawQuery)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewCatalogResolver(srv.URL, "tok", srv.Client()), asked
}

// TestListSeatPinsDecodesAndPassesCursor pins the wire contract with a HAND-WRITTEN body —
// not one encoded from SeatPin — so the test can express a payload the type would never
// produce. A fixture built from the decoder's own type cannot fail on a field-name change.
func TestListSeatPinsDecodesAndPassesCursor(t *testing.T) {
	r, asked := pinListServer(t, http.StatusOK, `{"pins":[
		{"id":"00000000-0000-0000-0000-000000000001","organizer_id":"11111111-1111-1111-1111-111111111111",
		 "seat_map_id":"22222222-2222-2222-2222-222222222222","seat_identity":"Orchestra/A/1",
		 "pinned_by":"hold:33333333-3333-3333-3333-333333333333"},
		{"id":"00000000-0000-0000-0000-000000000002","organizer_id":"11111111-1111-1111-1111-111111111111",
		 "seat_map_id":"22222222-2222-2222-2222-222222222222","seat_identity":"Orchestra/A/2",
		 "pinned_by":"sale:44444444-4444-4444-4444-444444444444"}]}`)

	cursor := uuid.MustParse("00000000-0000-0000-0000-000000000000")
	pins, err := r.ListSeatPins(context.Background(), cursor, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pins) != 2 {
		t.Fatalf("pins = %+v want 2", pins)
	}
	if pins[0].SeatIdentity != "Orchestra/A/1" || pins[0].PinnedBy != "hold:33333333-3333-3333-3333-333333333333" {
		t.Fatalf("pin 0 = %+v", pins[0])
	}
	if pins[1].PinnedBy != "sale:44444444-4444-4444-4444-444444444444" {
		t.Fatalf("pin 1 = %+v — the sale pin must survive the decode, the CALLER classifies", pins[1])
	}
	if pins[0].OrganizerID != uuid.MustParse("11111111-1111-1111-1111-111111111111") ||
		pins[0].SeatMapID != uuid.MustParse("22222222-2222-2222-2222-222222222222") {
		t.Fatalf("pin 0 ids = %+v", pins[0])
	}
	// uuid.Nil cursor is the first page and must NOT be sent as a literal zero uuid.
	if len(*asked) != 1 || (*asked)[0] != "/internal/seat-map-pins?limit=100" {
		t.Fatalf("request = %v", *asked)
	}

	// A non-nil cursor rides the query string. Its own server: the drained page is empty,
	// which is how the caller learns the table is exhausted.
	next := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	r2, asked2 := pinListServer(t, http.StatusOK, `{"pins":[]}`)
	page, err := r2.ListSeatPins(context.Background(), next, 50)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(page) != 0 {
		t.Fatalf("drained page = %+v want empty", page)
	}
	if len(*asked2) != 1 || (*asked2)[0] != "/internal/seat-map-pins?after="+next.String()+"&limit=50" {
		t.Fatalf("second request = %v", *asked2)
	}
}

// TestListSeatPinsFailsClosedOnBadPage: every malformed page must abort before any liveness
// decision is made. A reconciler that accepted a page it could not fully trust would be
// deciding what to DELETE from partial data.
func TestListSeatPinsFailsClosedOnBadPage(t *testing.T) {
	const (
		p1 = `{"id":"00000000-0000-0000-0000-000000000001","organizer_id":"11111111-1111-1111-1111-111111111111","seat_map_id":"22222222-2222-2222-2222-222222222222","seat_identity":"Orchestra/A/1","pinned_by":"hold:h1"}`
		p2 = `{"id":"00000000-0000-0000-0000-000000000002","organizer_id":"11111111-1111-1111-1111-111111111111","seat_map_id":"22222222-2222-2222-2222-222222222222","seat_identity":"Orchestra/A/2","pinned_by":"hold:h2"}`
	)
	for _, tc := range []struct{ name, body string }{
		{"not json", `{`},
		{"missing pins key", `{"rows":[]}`},
		{"nil pin id", `{"pins":[{"id":"00000000-0000-0000-0000-000000000000","organizer_id":"11111111-1111-1111-1111-111111111111","seat_map_id":"22222222-2222-2222-2222-222222222222","seat_identity":"A/1","pinned_by":"hold:h"}]}`},
		{"nil organizer", `{"pins":[{"id":"00000000-0000-0000-0000-000000000001","organizer_id":"00000000-0000-0000-0000-000000000000","seat_map_id":"22222222-2222-2222-2222-222222222222","seat_identity":"A/1","pinned_by":"hold:h"}]}`},
		{"nil seat map", `{"pins":[{"id":"00000000-0000-0000-0000-000000000001","organizer_id":"11111111-1111-1111-1111-111111111111","seat_map_id":"00000000-0000-0000-0000-000000000000","seat_identity":"A/1","pinned_by":"hold:h"}]}`},
		{"blank seat identity", `{"pins":[{"id":"00000000-0000-0000-0000-000000000001","organizer_id":"11111111-1111-1111-1111-111111111111","seat_map_id":"22222222-2222-2222-2222-222222222222","seat_identity":"","pinned_by":"hold:h"}]}`},
		{"blank pinned_by", `{"pins":[{"id":"00000000-0000-0000-0000-000000000001","organizer_id":"11111111-1111-1111-1111-111111111111","seat_map_id":"22222222-2222-2222-2222-222222222222","seat_identity":"A/1","pinned_by":""}]}`},
		{"duplicate id", `{"pins":[` + p1 + `,` + p1 + `]}`},
		{"out of order", `{"pins":[` + p2 + `,` + p1 + `]}`},
		{"oversized page", `{"pins":[` + p1 + `,` + p2 + `]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limit := 100
			if tc.name == "oversized page" {
				limit = 1 // the server returned more rows than were asked for
			}
			r, _ := pinListServer(t, http.StatusOK, tc.body)
			if _, err := r.ListSeatPins(context.Background(), uuid.Nil, limit); err == nil {
				t.Fatal("a page that cannot be fully trusted must be an error")
			}
		})
	}

	t.Run("row at or before the cursor", func(t *testing.T) {
		r, _ := pinListServer(t, http.StatusOK, `{"pins":[`+p1+`]}`)
		// Asking after id ...0001 and being handed ...0001 back would spin the drain loop.
		if _, err := r.ListSeatPins(context.Background(), uuid.MustParse("00000000-0000-0000-0000-000000000001"), 100); err == nil {
			t.Fatal("a row at or before the cursor must be rejected, not looped on")
		}
	})

	t.Run("non-200", func(t *testing.T) {
		for _, status := range []int{http.StatusUnauthorized, http.StatusInternalServerError, http.StatusNotFound} {
			r, _ := pinListServer(t, status, `{"pins":[]}`)
			if _, err := r.ListSeatPins(context.Background(), uuid.Nil, 100); err == nil {
				t.Fatalf("status %d must be an error", status)
			}
		}
	})

	t.Run("bad limit", func(t *testing.T) {
		r, _ := pinListServer(t, http.StatusOK, `{"pins":[]}`)
		if _, err := r.ListSeatPins(context.Background(), uuid.Nil, 0); err == nil {
			t.Fatal("limit 0 must be a usage error")
		}
	})
}
