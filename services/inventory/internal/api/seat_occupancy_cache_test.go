package api

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/inventory/internal/seatoccupancy"
	"ticketing/services/inventory/internal/store"
)

type countingOccupancyReader struct {
	calls   int
	age     time.Duration
	unavail []string
	enabled bool
	entries int
}

func (c *countingOccupancyReader) SetEnabled(v bool) { c.enabled = v }

func (c *countingOccupancyReader) Status() seatoccupancy.Status {
	return seatoccupancy.Status{Enabled: c.enabled, Entries: c.entries}
}

func (c *countingOccupancyReader) Read(_ context.Context, _, slot uuid.UUID) (seatoccupancy.Read, error) {
	c.calls++
	unavail := c.unavail
	if unavail == nil {
		unavail = []string{}
	}
	return seatoccupancy.Read{
		Value: store.SeatOccupancy{
			SlotID:            slot,
			SeatMapID:         uuid.Nil,
			OfferingStatus:    "open",
			RemainingCapacity: 10,
			Unavailable:       unavail,
		},
		Age: c.age,
	}, nil
}

func getSeatOccupancy(t *testing.T, h http.Handler, slot, org uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"http://inventory.local/slots/"+slot.String()+"/seat-occupancy?organizer_id="+org.String(), nil))
	return rec
}

// TestSeatOccupancyHandlerReadsThroughTheCache pins the wiring: the public seat occupancy
// read goes through the display collaborator, not straight to the store.
func TestSeatOccupancyHandlerReadsThroughTheCache(t *testing.T) {
	rd := &countingOccupancyReader{unavail: []string{"A1"}}
	h := NewWithReaders(nil, "", nil, nil, rd).Router(nil, true)
	slot, org := uuid.New(), uuid.New()

	for range 3 {
		rec := getSeatOccupancy(t, h, slot, org)
		if rec.Code != http.StatusOK {
			t.Fatalf("seat-occupancy: %d %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"unavailable_seat_identities":["A1"]`) {
			t.Fatalf("body did not come from the display read: %s", rec.Body.String())
		}
	}
	if rd.calls != 3 {
		t.Fatalf("display read called %d times for 3 requests, want 3 — the handler must not bypass it", rd.calls)
	}
}

// TestSeatOccupancyEmitsAgeWithinTheTier is the anti-stacking guarantee, and it is
// contract-enforced: the spec declares SeatOccupancyAge required, an integer in [0,5].
func TestSeatOccupancyEmitsAgeWithinTheTier(t *testing.T) {
	for _, tc := range []struct {
		name string
		age  time.Duration
		want string
	}{
		{"a miss is age 0", 0, "0"},
		{"a fresh hit rounds up", 10 * time.Millisecond, "1"},
		{"a hit rounds up, never down", 2500 * time.Millisecond, "3"},
		{"an almost-expired hit reports the whole tier", 4900 * time.Millisecond, "5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rd := &countingOccupancyReader{age: tc.age}
			h := NewWithReaders(nil, "", nil, nil, rd).Router(nil, true)
			rec := getSeatOccupancy(t, h, uuid.New(), uuid.New())
			if rec.Code != http.StatusOK {
				t.Fatalf("seat-occupancy: %d %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Age"); got != tc.want {
				t.Fatalf("Age = %q, want %q", got, tc.want)
			}
			if got := rec.Header().Get("Cache-Control"); got != CacheControlPublicSeatOccupancy {
				t.Fatalf("Cache-Control = %q, want %q", got, CacheControlPublicSeatOccupancy)
			}
		})
	}
}

// TestOnlyTheDisplayReadTouchesTheOccupancyCache is COS 7 / COS 5 structural scan:
// only seatOccupancy, New, NewWithReaders, and the cache-control handlers may reach s.occupancy.
func TestOnlyTheDisplayReadTouchesTheOccupancyCache(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	var users []string
	var scanned int
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++
		f, perr := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "occupancy" {
					return true
				}
				if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "s" {
					users = append(users, fn.Name.Name)
				}
				return true
			})
		}
	}

	if scanned < 3 {
		t.Fatalf("scanned only %d production files — the walk is not reaching this package", scanned)
	}
	for _, fn := range users {
		if fn != "seatOccupancy" && fn != "New" && fn != "NewWithReaders" &&
			fn != "cacheControlStatus" && fn != "cacheControlSet" {
			t.Errorf("%s reads s.occupancy — only the public display read may.", fn)
		}
	}
	if len(users) == 0 {
		t.Fatal("nothing references s.occupancy at all — the cache is not wired to the handler")
	}
}
