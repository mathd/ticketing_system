package consumer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
		{http.StatusConflict, true, true},            // seat not in current version → deterministic
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
		// ai-review F2: a single json.Decode accepts a valid first value followed by anything.
		// A truncated or spliced 200 body must never reach a delete decision.
		{"trailing garbage after a valid page", `{"pins":[` + p1 + `]} not-json-at-all`},
		{"truncated second value", `{"pins":[` + p1 + `]}{"pins":[`},
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

// TestListSeatPinsGuardsTheSizeLimitBoundary closes the hole the FIRST fix for the
// exactly-one-value check opened (ai-review pass 2). Reading through io.LimitReader(body, N)
// manufactures an EOF at N even when the socket has more to give, so a page padded through the
// limit and then followed by garbage made the second Decode return io.EOF — the page was
// accepted and its dead entries could reach UnpinSeats. The size bound and the
// exactly-one-value check cancelled each other out.
func TestListSeatPinsGuardsTheSizeLimitBoundary(t *testing.T) {
	const p1 = `{"id":"00000000-0000-0000-0000-000000000001","organizer_id":"11111111-1111-1111-1111-111111111111","seat_map_id":"22222222-2222-2222-2222-222222222222","seat_identity":"Orchestra/A/1","pinned_by":"hold:h1"}`
	page := `{"pins":[` + p1 + `]}`

	t.Run("exactly at the limit is accepted", func(t *testing.T) {
		body := page + strings.Repeat(" ", maxSeatPinPageBytes-len(page))
		if len(body) != maxSeatPinPageBytes {
			t.Fatalf("fixture is %d bytes, want exactly %d", len(body), maxSeatPinPageBytes)
		}
		r, _ := pinListServer(t, http.StatusOK, body)
		pins, err := r.ListSeatPins(context.Background(), uuid.Nil, 100)
		if err != nil {
			t.Fatalf("a page exactly at the limit must be accepted: %v", err)
		}
		if len(pins) != 1 {
			t.Fatalf("pins = %d want 1", len(pins))
		}
	})

	t.Run("one byte over the limit is rejected", func(t *testing.T) {
		body := page + strings.Repeat(" ", maxSeatPinPageBytes-len(page)) + " "
		r, _ := pinListServer(t, http.StatusOK, body)
		if _, err := r.ListSeatPins(context.Background(), uuid.Nil, 100); err == nil {
			t.Fatal("a body over the size limit must be rejected, not silently truncated")
		}
	})

	t.Run("garbage hidden beyond the limit is rejected", func(t *testing.T) {
		body := page + strings.Repeat(" ", maxSeatPinPageBytes) + `{"pins":[`
		r, _ := pinListServer(t, http.StatusOK, body)
		if _, err := r.ListSeatPins(context.Background(), uuid.Nil, 100); err == nil {
			t.Fatal("trailing content past the limit must not be invisible to the one-value check")
		}
	})
}

// --- TKT-181 / ADR-041: the adjacency projection's boundary validation ---
//
// These go through a real httptest server rather than a double, because the whole risk
// is in DECODING geometry the projection then treats as authoritative. A double would
// hand back a Go slice that is already well-formed and prove nothing (ai-review noted
// the test doubles never exercised this path at all).

func geometryServer(t *testing.T, status int, body string) *CatalogResolver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewCatalogResolver(srv.URL, "tok", srv.Client())
}

func TestSeatMapAdjacencyDerivesNeighboursFromPosition(t *testing.T) {
	id := uuid.New()
	// Deliberately out of order, and with position GAPS: gaps are spacing, not missing
	// seats, so 10/20/40 is still three adjacent seats.
	row := uuid.New()
	body := `{"map":{"id":"` + id.String() + `","status":"published"},"sections":[{"rows":[{"id":"` + row.String() + `","seats":[
		{"seat_identity":"A/1/3","position":40},
		{"seat_identity":"A/1/1","position":10},
		{"seat_identity":"A/1/2","position":20}]}]}]}`

	got, err := geometryServer(t, 200, body).SeatMapAdjacency(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d seats want 3", len(got))
	}
	if got[0].SeatIdentity != "A/1/1" || got[0].Left != nil || got[0].Right == nil || *got[0].Right != "A/1/2" {
		t.Fatalf("first seat: %+v — a row end has NO left neighbour, which is an answer, not missing data", got[0])
	}
	// The middle seat is the one that matters for the orphan rule: a derivation that
	// emitted every seat with NO neighbours would still be internally reciprocal, so
	// nothing downstream in inventory could tell it apart from a row of one-seat rows.
	// Fidelity to the geometry is established HERE and nowhere else (ADR-041).
	if got[1].Left == nil || *got[1].Left != "A/1/1" || got[1].Right == nil || *got[1].Right != "A/1/3" {
		t.Fatalf("middle seat: %+v — both neighbours must be named", got[1])
	}
	if got[2].SeatIdentity != "A/1/3" || got[2].Right != nil || *got[2].Left != "A/1/2" {
		t.Fatalf("last seat: %+v", got[2])
	}
	// The ordering half (TKT-81), derived from the same sort and pinned here for the same
	// reason: this is the only place the geometry exists, so it is the only place fidelity
	// can be established.
	//
	// Positions are RE-BASED to 1..n, and this fixture is why. Catalog's raw positions here
	// are 10, 20 and 40 — legal, since positions must ascend and be unique but need not be
	// contiguous ("gaps are spacing, not missing seats", above). Carrying those through
	// would make the selection query, which groups runs by CONSECUTIVE positions, read this
	// row as three separate one-seat runs and never offer the party of three that is
	// plainly sitting there.
	for i, want := range []int32{1, 2, 3} {
		if got[i].RowKey != row.String() {
			t.Fatalf("seat %d row key = %q want %q — the row's catalog id, not its label", i, got[i].RowKey, row)
		}
		if got[i].Position != want {
			t.Fatalf("seat %d position = %d want %d — positions are re-based to 1..n so adjacency and consecutiveness agree", i, got[i].Position, want)
		}
	}
}

// TestSeatMapAdjacencyRefusesARowWithNoId: a row that cannot be keyed cannot be projected
// (TKT-81). Keying on anything else available — the label, the index in the response —
// merges rows across sections ("row A" exists in every one) or renumbers them on the next
// publication, and either produces a projection that offers runs spanning a gangway.
// Deterministically unusable, so it terminates rather than parking for ever.
func TestSeatMapAdjacencyRefusesARowWithNoId(t *testing.T) {
	id := uuid.New()
	body := `{"map":{"id":"` + id.String() + `","status":"published"},"sections":[{"rows":[{"seats":[
		{"seat_identity":"A/1/1","position":1},
		{"seat_identity":"A/1/2","position":2}]}]}]}`

	_, err := geometryServer(t, 200, body).SeatMapAdjacency(context.Background(), id)
	if !errors.Is(err, ErrGeometryInvalid) {
		t.Fatalf("err = %v want ErrGeometryInvalid — an unkeyable row must terminate, not retry", err)
	}
}

// TestSeatMapAdjacencyKeepsRowsDistinctWhenLabelsRepeat is the reason RowKey is the row's
// id. Two sections each hold a "row A" at the same positions; a label-keyed or
// position-keyed projection collapses them into one row, and best-available then offers a
// party seats on both sides of a gangway and calls them adjacent.
func TestSeatMapAdjacencyKeepsRowsDistinctWhenLabelsRepeat(t *testing.T) {
	id, r1, r2 := uuid.New(), uuid.New(), uuid.New()
	body := `{"map":{"id":"` + id.String() + `","status":"published"},"sections":[
		{"rows":[{"id":"` + r1.String() + `","label":"A","seats":[
			{"seat_identity":"L/A/1","position":1},{"seat_identity":"L/A/2","position":2}]}]},
		{"rows":[{"id":"` + r2.String() + `","label":"A","seats":[
			{"seat_identity":"R/A/1","position":1},{"seat_identity":"R/A/2","position":2}]}]}]}`

	got, err := geometryServer(t, 200, body).SeatMapAdjacency(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]struct{}{}
	for _, a := range got {
		keys[a.RowKey] = struct{}{}
	}
	if len(keys) != 2 {
		t.Fatalf("distinct row keys = %d want 2 — two sections' \"row A\" are different rows", len(keys))
	}
	// And the two rows get DISTINCT ranks, in the order catalog listed them (TKT-81). The
	// key keeps them apart; the rank puts them in the venue's order, and a uuid cannot do
	// the second job — sorting by it would seat a buyer in a row chosen at random.
	for _, a := range got {
		want := int32(1)
		if strings.HasPrefix(a.SeatIdentity, "R/") {
			want = 2
		}
		if a.RowRank != want {
			t.Fatalf("%s rank = %d want %d — rows rank in the order catalog lists them", a.SeatIdentity, a.RowRank, want)
		}
	}
}

func TestSeatMapAdjacencyNeverConnectsRowsOrSections(t *testing.T) {
	id := uuid.New()
	body := `{"map":{"id":"` + id.String() + `","status":"published"},"sections":[
		{"rows":[{"id":"` + uuid.New().String() + `","seats":[{"seat_identity":"A/1/1","position":1}]}]},
		{"rows":[{"id":"` + uuid.New().String() + `","seats":[{"seat_identity":"B/1/1","position":1}]}]}]}`

	got, err := geometryServer(t, 200, body).SeatMapAdjacency(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		if a.Left != nil || a.Right != nil {
			t.Fatalf("%+v — a one-seat row has no neighbours, and seats never adjoin across rows or sections", a)
		}
	}
}

// Every one of these produces a plausible-looking partial projection if it is not
// refused — and a partial projection is worse than none, because the rule then
// silently permits orphans exactly where the data was missing, or invents neighbours
// and refuses legal selections.
func TestSeatMapAdjacencyFailsClosedOnMalformedGeometry(t *testing.T) {
	id, other := uuid.New(), uuid.New()
	pub := `"status":"published"`
	for name, body := range map[string]string{
		"wrong map version": `{"map":{"id":"` + other.String() + `",` + pub + `},"sections":[{"rows":[{"seats":[{"seat_identity":"A/1/1","position":1}]}]}]}`,
		"draft geometry":    `{"map":{"id":"` + id.String() + `","status":"draft"},"sections":[{"rows":[{"seats":[{"seat_identity":"A/1/1","position":1}]}]}]}`,
		"no seats at all":   `{"map":{"id":"` + id.String() + `",` + pub + `},"sections":[]}`,
		// A zero position is what an OMITTED position decodes to, so this also covers
		// a seat whose position the producer forgot to send.
		"zero position": `{"map":{"id":"` + id.String() + `",` + pub + `},"sections":[{"rows":[{"seats":[
			{"seat_identity":"A/1/1","position":0},{"seat_identity":"A/1/2","position":1}]}]}]}`,
		// Equal positions make sort order arbitrary — the derived neighbours would
		// differ run to run.
		"duplicate position in a row": `{"map":{"id":"` + id.String() + `",` + pub + `},"sections":[{"rows":[{"seats":[
			{"seat_identity":"A/1/1","position":1},{"seat_identity":"A/1/2","position":1}]}]}]}`,
		"duplicate identity": `{"map":{"id":"` + id.String() + `",` + pub + `},"sections":[{"rows":[{"seats":[
			{"seat_identity":"A/1/1","position":1}]},{"seats":[{"seat_identity":"A/1/1","position":1}]}]}]}`,
		// "A/1/1" and " A/1/1" are the same seat to catalog and to a human; two rows
		// on one seat would make the claim-path lookup depend on which one it matched.
		"whitespace-variant identity": `{"map":{"id":"` + id.String() + `",` + pub + `},"sections":[{"rows":[{"seats":[
			{"seat_identity":"A/1/1","position":1},{"seat_identity":" A/1/1","position":2}]}]}]}`,
		"blank identity": `{"map":{"id":"` + id.String() + `",` + pub + `},"sections":[{"rows":[{"seats":[
			{"seat_identity":"   ","position":1}]}]}]}`,
		// A valid prefix followed by garbage: one Decode would accept the prefix and
		// commit it as authoritative, permanently, since the same transaction consumes
		// the event.
		"trailing bytes after a valid document": `{"map":{"id":"` + id.String() + `",` + pub + `},"sections":[{"rows":[{"seats":[
			{"seat_identity":"A/1/1","position":1}]}]}]} {"map":{"id":"junk"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := geometryServer(t, 200, body).SeatMapAdjacency(context.Background(), id)
			if err == nil {
				t.Fatal("malformed geometry must be refused, never projected as authoritative")
			}
			// And refused DETERMINISTICALLY. Classifying these as transient would park
			// them for ever — the mirror of terminating a catalog blip.
			if !errors.Is(err, ErrGeometryInvalid) {
				t.Fatalf("err = %v, want ErrGeometryInvalid: retrying corrupt geometry changes nothing", err)
			}
		})
	}
}

func TestSeatMapAdjacencyFailsOnUpstreamError(t *testing.T) {
	id := uuid.New()
	for _, status := range []int{404, 500, 503} {
		if _, err := geometryServer(t, status, `{}`).SeatMapAdjacency(context.Background(), id); err == nil {
			t.Fatalf("status %d must be an error", status)
		}
	}
}

// Only catalog's SETTLED answer about this map is deterministic. A blanket 4xx sweep
// is wrong: 408, 425 and 429 are explicitly retryable, 401 can be transient during a
// credential or proxy change, and an unknown status must never terminate. A needless
// retry costs a delay; a wrong terminate costs the publication (ai-review).
func TestSeatMapAdjacencyStatusClassification(t *testing.T) {
	id := uuid.New()
	for _, tc := range []struct {
		status        int
		deterministic bool
	}{
		{404, true}, {410, true},
		{401, false}, {408, false}, {425, false}, {429, false},
		{500, false}, {502, false}, {503, false}, {418, false},
	} {
		_, err := geometryServer(t, tc.status, `{}`).SeatMapAdjacency(context.Background(), id)
		if err == nil {
			t.Fatalf("status %d must be an error", tc.status)
		}
		if got := errors.Is(err, ErrGeometryInvalid); got != tc.deterministic {
			t.Fatalf("status %d: ErrGeometryInvalid=%v want %v", tc.status, got, tc.deterministic)
		}
	}
}

// A transport that succeeds through the headers and then fails mid-body must be
// RETRIED, not terminated: the content was never seen, so calling it invalid geometry
// deletes a publication because a connection dropped (ai-review).
func TestSeatMapAdjacencyTreatsAnInterruptedBodyAsTransient(t *testing.T) {
	id := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "4096") // promise more than we send
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"map":{"id":"` + id.String() + `"`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Return without completing the body: the client sees an unexpected EOF.
	}))
	defer srv.Close()

	_, err := NewCatalogResolver(srv.URL, "tok", srv.Client()).SeatMapAdjacency(context.Background(), id)
	if err == nil {
		t.Fatal("an interrupted body must be an error")
	}
	if errors.Is(err, ErrGeometryInvalid) {
		t.Fatalf("err = %v — a dropped connection is transient; terminating on it loses the publication", err)
	}
}

// TestSeatMapAdjacencyRanksRowsByDeclaredPositionNotArrivalOrder pins the ranking against the
// producer's iteration order rather than with it (TKT-81).
//
// Catalog's geometry read orders rows by position across the WHOLE map and then files each
// into its section, so today arrival order and (section, row) order agree. That is the
// producer's SQL, not a contract this consumer can check, and inventory holds no geometry to
// notice the day it changes. The response carries the positions explicitly, so the rank is
// computed from them.
//
// The fixture arrives deliberately out of order: section 2 first, and within each section the
// higher row position first. The ranks must still follow (section position, row position).
func TestSeatMapAdjacencyRanksRowsByDeclaredPositionNotArrivalOrder(t *testing.T) {
	id := uuid.New()
	r := func() string { return uuid.New().String() }
	s2r2, s2r1, s1r2, s1r1 := r(), r(), r(), r()
	body := `{"map":{"id":"` + id.String() + `","status":"published"},"sections":[
		{"position":2,"rows":[
			{"id":"` + s2r2 + `","position":2,"seats":[{"seat_identity":"B/2/1","position":1}]},
			{"id":"` + s2r1 + `","position":1,"seats":[{"seat_identity":"B/1/1","position":1}]}]},
		{"position":1,"rows":[
			{"id":"` + s1r2 + `","position":2,"seats":[{"seat_identity":"A/2/1","position":1}]},
			{"id":"` + s1r1 + `","position":1,"seats":[{"seat_identity":"A/1/1","position":1}]}]}]}`

	got, err := geometryServer(t, 200, body).SeatMapAdjacency(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int32{"A/1/1": 1, "A/2/1": 2, "B/1/1": 3, "B/2/1": 4}
	for _, a := range got {
		if a.RowRank != want[a.SeatIdentity] {
			t.Fatalf("%s rank = %d want %d — rows rank by (section position, row position), not by the order the response happened to list them",
				a.SeatIdentity, a.RowRank, want[a.SeatIdentity])
		}
	}
	if len(got) != 4 {
		t.Fatalf("got %d seats want 4", len(got))
	}
}
