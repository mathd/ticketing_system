package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func catalogCacheControl(t *testing.T, srv *Server, method, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	h, err := NewRouter(srv, true)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	req := httptest.NewRequest(method, "http://catalog.local/internal/cache-control", strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Internal-Token", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func cacheControlServer(rd publicReader) *Server {
	return newServerWithPublicReader(&fakeStore{}, &fakePublisher{},
		slog.New(slog.NewTextHandler(io.Discard, nil)), "secret", testStaffWriteToken, rd)
}

// TestCatalogCacheControlRequiresTheInternalCredential.
//
// Bodies are deliberately well-formed: a malformed body would be refused before
// the guard runs, and the test would pass while proving nothing about
// authentication.
func TestCatalogCacheControlRequiresTheInternalCredential(t *testing.T) {
	for _, tc := range []struct{ name, method, token, body string }{
		{"GET without a token", http.MethodGet, "", ""},
		{"GET with the wrong token", http.MethodGet, "wrong", ""},
		{"PUT without a token", http.MethodPut, "", `{"enabled":false}`},
		{"PUT with the wrong token", http.MethodPut, "wrong", `{"enabled":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rd := &stubPublicReader{enabled: true}
			rec := catalogCacheControl(t, cacheControlServer(rd), tc.method, tc.token, tc.body)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("got %d %s, want 401 — matching catalog's other internal routes", rec.Code, rec.Body.String())
			}
			if !rd.enabled {
				t.Fatal("a refused request still changed the cache state")
			}
		})
	}
}

// TestCatalogCacheControlDoesNotNeedTheStaffWriteToken: the staff-write
// credential guards authoring operations and is held by a public-facing SSR
// process (ADR-042/TKT-191). An operator toggling a cache is a different
// principal, and must not need it.
func TestCatalogCacheControlDoesNotNeedTheStaffWriteToken(t *testing.T) {
	rd := &stubPublicReader{enabled: true, entries: 7}
	rec := catalogCacheControl(t, cacheControlServer(rd), http.MethodGet, "secret", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d %s, want 200 with only the internal token", rec.Code, rec.Body.String())
	}
}

// TestCatalogCacheControlReportsAndTogglesTheLiveCollaborator pins that the
// switch addresses the same object the four public reads use.
func TestCatalogCacheControlReportsAndTogglesTheLiveCollaborator(t *testing.T) {
	rd := &stubPublicReader{enabled: true, entries: 7}
	srv := cacheControlServer(rd)

	rec := catalogCacheControl(t, srv, http.MethodGet, "secret", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `"enabled":true`) || !strings.Contains(body, `"entries":7`) {
		t.Fatalf("GET reported %s, want the collaborator's live state", body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}

	rec = catalogCacheControl(t, srv, http.MethodPut, "secret", `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	if rd.enabled {
		t.Fatal("PUT did not reach the collaborator the read path uses")
	}
	rec = catalogCacheControl(t, srv, http.MethodPut, "secret", `{"enabled":true}`)
	if rec.Code != http.StatusOK || !rd.enabled {
		t.Fatalf("re-enable: %d, enabled=%v", rec.Code, rd.enabled)
	}
}

// TestCatalogCacheControlRejectsAnAmbiguousBody: a missing `enabled` must not be
// read as "disable".
func TestCatalogCacheControlRejectsAnAmbiguousBody(t *testing.T) {
	for _, body := range []string{`{}`, `{"enabled":null}`, `{"enabled":"false"}`, `{"nope":true}`, `not json`} {
		rd := &stubPublicReader{enabled: true}
		rec := catalogCacheControl(t, cacheControlServer(rd), http.MethodPut, "secret", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %s: got %d, want 400", body, rec.Code)
		}
		if !rd.enabled {
			t.Errorf("PUT %s changed the cache state despite being refused", body)
		}
	}
}
