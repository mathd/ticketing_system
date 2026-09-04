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

	"ticketing/services/catalog/internal/store"
)

// stubPublicReader answers with a fixed age so the handler's Age emission can be
// tested without driving a real cache through five minutes.
type stubPublicReader struct {
	age     time.Duration
	calls   int
	enabled bool
	entries int
}

func (s *stubPublicReader) SetEnabled(v bool) { s.enabled = v }

func (s *stubPublicReader) Status() publicReadStatus {
	return publicReadStatus{Enabled: s.enabled, Entries: s.entries}
}

func (s *stubPublicReader) ListPublishedEvents(context.Context) (cached[[]store.EventAggregate], error) {
	s.calls++
	return cached[[]store.EventAggregate]{Value: nil, Age: s.age}, nil
}

func (s *stubPublicReader) GetPublishedEvent(_ context.Context, id uuid.UUID) (cached[store.EventAggregate], error) {
	s.calls++
	return cached[store.EventAggregate]{}, store.ErrNotFound
}

func (s *stubPublicReader) GetPublishedSeason(_ context.Context, id uuid.UUID) (cached[store.SeasonAggregate], error) {
	s.calls++
	return cached[store.SeasonAggregate]{}, store.ErrNotFound
}

func (s *stubPublicReader) GetPublishedFestival(_ context.Context, id uuid.UUID) (cached[store.FestivalAggregate], error) {
	s.calls++
	return cached[store.FestivalAggregate]{}, store.ErrNotFound
}

// TestPublicListEmitsAgeWithinTheTier is the anti-stacking guarantee at the HTTP
// boundary, and it is contract-enforced: the spec declares Age required and
// bounded to [0,300], so ADR-028's validator turns a missing or out-of-range
// value into a 500 with the payload withheld.
func TestPublicListEmitsAgeWithinTheTier(t *testing.T) {
	for _, tc := range []struct {
		name string
		age  time.Duration
		want string
	}{
		{"a miss is age 0", 0, "0"},
		{"a fresh hit rounds up", 10 * time.Millisecond, "1"},
		{"a hit rounds up, never down", 90500 * time.Millisecond, "91"},
		{"an almost-expired hit reports most of the tier", 299 * time.Second, "299"},
		{"age is clamped to the tier", 10 * time.Minute, "300"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rd := &stubPublicReader{age: tc.age}
			srv := newServerWithPublicReader(&fakeStore{}, &fakePublisher{}, nil, "t", testStaffWriteToken, rd)
			rec := httptest.NewRecorder()
			srv.ListPublicEvents(rec, httptest.NewRequest(http.MethodGet,
				"http://catalog.local/public/events?locale=en", nil), ListPublicEventsParams{Locale: "en"})

			if rec.Code != http.StatusOK {
				t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Age"); got != tc.want {
				t.Fatalf("Age = %q, want %q", got, tc.want)
			}
			if got := rec.Header().Get("Cache-Control"); got != CacheControlPublicReads {
				t.Fatalf("Cache-Control = %q, want the unchanged minutes tier", got)
			}
			if rd.calls != 1 {
				t.Fatalf("the handler called the display reader %d times, want 1 — it must not bypass the cache", rd.calls)
			}
		})
	}
}

// TestOnlyTheMinuteTierPublicReadsUseTheCache is COS 3 and the write-path
// separation together, checked structurally because that is the form the
// guarantee takes: not "no other handler happened to use it in this test", but
// "no other handler can".
//
// Two things depend on it. Writes must never read a cached number. And the
// seat-map reads must keep calling s.seatMaps directly: their tier is decided per
// response by cacheControlForSeatMaps (ADR-004 § TKT-107), and a draft-bearing
// payload is no-store — which has to mean not stored HERE either, not merely
// not stored downstream.
func TestOnlyTheMinuteTierPublicReadsUseTheCache(t *testing.T) {
	allowed := map[string]bool{
		"ListPublicEvents": true, "GetPublicEvent": true,
		"GetPublicSeason": true, "GetPublicFestival": true,
		"newServer": true, "NewServer": true, "newServerWithPublicReader": true,
		// The kill-switch handlers legitimately reach the collaborator — routing
		// the switch through the same object the read path uses is the point
		// (TKT-210). Named individually; no wildcard.
		"cacheControlStatus": true, "cacheControlSet": true, "writeCacheState": true,
	}

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
		f, perr := parser.ParseFile(token.NewFileSet(), filepath.Join(".", name), nil, 0)
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
				if !ok || sel.Sel.Name != "public" {
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
	if len(users) == 0 {
		t.Fatal("nothing references s.public — the cache is not wired to any handler")
	}
	for _, fn := range users {
		if !allowed[fn] {
			t.Errorf("%s reads s.public — only the four minute-tier public reads may. "+
				"Writes take truth from the store, and the seat-map reads decide their tier per "+
				"response (no-store on a draft), which must mean not cached here either.", fn)
		}
	}
}
