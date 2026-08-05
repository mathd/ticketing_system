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

	"ticketing/services/inventory/internal/availability"
	"ticketing/services/inventory/internal/store"
)

type countingReader struct {
	calls int
	age   time.Duration
	avail int32
}

func (c *countingReader) Read(_ context.Context, _, slot uuid.UUID, _ string) (availability.Read, error) {
	c.calls++
	return availability.Read{
		Value: store.Availability{SlotID: slot, Available: c.avail, OfferingStatus: "open"},
		Age:   c.age,
	}, nil
}

func getAvailability(t *testing.T, h http.Handler, slot, org uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"http://inventory.local/slots/"+slot.String()+"/availability?organizer_id="+org.String(), nil))
	return rec
}

// TestAvailabilityHandlerReadsThroughTheCache pins the wiring: the public read
// goes through the display collaborator, not straight to the store. Without it
// the cache could be constructed, registered and never consulted — and every
// cache-package test would still pass.
func TestAvailabilityHandlerReadsThroughTheCache(t *testing.T) {
	rd := &countingReader{avail: 9}
	h := NewWithAvailability(nil, "", nil, rd).Router(nil, true)
	slot, org := uuid.New(), uuid.New()

	for range 3 {
		rec := getAvailability(t, h, slot, org)
		if rec.Code != http.StatusOK {
			t.Fatalf("availability: %d %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"available":9`) {
			t.Fatalf("body did not come from the display read: %s", rec.Body.String())
		}
	}
	if rd.calls != 3 {
		t.Fatalf("display read called %d times for 3 requests, want 3 — the handler must not bypass it", rd.calls)
	}
}

// TestAvailabilityEmitsAgeWithinTheTier is the anti-stacking guarantee, and it is
// contract-enforced: the spec declares Age required, an integer in [0,5], so
// ADR-028's validator turns a missing or out-of-range value into a 500 with the
// payload withheld. A response served from a nearly-expired entry must say so,
// or a conformant client grants itself another full tier of freshness on top.
func TestAvailabilityEmitsAgeWithinTheTier(t *testing.T) {
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
			rd := &countingReader{age: tc.age}
			h := NewWithAvailability(nil, "", nil, rd).Router(nil, true)
			rec := getAvailability(t, h, uuid.New(), uuid.New())
			if rec.Code != http.StatusOK {
				t.Fatalf("availability: %d %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Age"); got != tc.want {
				t.Fatalf("Age = %q, want %q", got, tc.want)
			}
			// The tier itself must be untouched by any of this.
			if got := rec.Header().Get("Cache-Control"); got != CacheControlPublicAvailability {
				t.Fatalf("Cache-Control = %q, want the unchanged seconds tier", got)
			}
		})
	}
}

// TestOnlyTheDisplayReadTouchesTheCache is COS 7, checked structurally because
// that is the form the guarantee actually takes: not "the claim path happened not
// to hit the cache in this test", but "no other handler can reach it at all".
//
// ADR-002 and ADR-010 put correctness in the claim transaction. A claim that
// consulted a cached number could oversell, and it would do so intermittently,
// under load, which is the worst possible way to find out. The staff read is in
// the same position for a different reason: it exists for operators reconciling
// against truth, so serving it the cache would make the endpoint used to CHECK
// the cache report the cache's own answer.
func TestOnlyTheDisplayReadTouchesTheCache(t *testing.T) {
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
				if !ok || sel.Sel.Name != "avail" {
					return true
				}
				if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "s" {
					users = append(users, fn.Name.Name)
				}
				return true
			})
		}
	}

	// A walk that reaches nothing is a test that cannot fail.
	if scanned < 3 {
		t.Fatalf("scanned only %d production files — the walk is not reaching this package", scanned)
	}
	for _, fn := range users {
		// New/NewWithAvailability assign it; availability is the one reader.
		if fn != "availability" && fn != "New" && fn != "NewWithAvailability" {
			t.Errorf("%s reads s.avail — only the public display read may. "+
				"The claim path takes truth from the store under ADR-010's transaction, and the "+
				"staff read exists to check the cache, not to be served by it.", fn)
		}
	}
	if len(users) == 0 {
		t.Fatal("nothing references s.avail at all — the cache is not wired to the handler")
	}
}
