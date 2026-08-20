package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"ticketing/shared/httpx"
)

// The client's timeout bounds how LONG a sibling call may take, not how MANY
// bytes it answers with, so before maxUpstreamResponseBytes a sibling streaming
// steadily inside its deadline allocated without a ceiling — on checkout and on
// recovery, in a process holding other orders' claims.
//
// bodyServer answers every request with exactly `size` bytes.
func bodyServer(t *testing.T, size int) *httptest.Server {
	t.Helper()
	payload := strings.Repeat("x", size)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The boundary, tested as a boundary. Exactly the limit is read in full; one byte
// more is refused. A test that only sent a hugely oversized body would pass against
// an off-by-one ceiling, and one that only asserted the refusal would not notice a
// limit that had started rejecting legitimate sibling responses.
func TestUpstreamCallBodyLimitBoundary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		size    int64
		wantErr bool
	}{
		{"one byte under the limit", maxUpstreamResponseBytes - 1, false},
		{"exactly the limit", maxUpstreamResponseBytes, false},
		{"one byte over the limit", maxUpstreamResponseBytes + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := bodyServer(t, int(tc.size))
			s := New(nil, http.DefaultClient, srv.URL, srv.URL, srv.URL, "secret")

			code, body, err := s.call(context.Background(), http.MethodGet, srv.URL+"/anything", "", nil, true)
			if tc.wantErr {
				if !errors.Is(err, httpx.ErrResponseTooLarge) {
					t.Fatalf("want ErrResponseTooLarge, got %v", err)
				}
				// An oversize body must not come back truncated. A caller that
				// decoded a clipped body would classify on partial evidence,
				// which is worse than not answering at all.
				if body != nil {
					t.Fatalf("want no body on refusal, got %d bytes", len(body))
				}
				return
			}
			if err != nil {
				t.Fatalf("a response within the limit must be read: %v", err)
			}
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
			if int64(len(body)) != tc.size {
				t.Fatalf("read %d bytes, want %d", len(body), tc.size)
			}
		})
	}
}

// The semantics at a real seam, which is what the ceiling is FOR. An oversize
// price resolution must land on errResolveUnavailable — "we could not get an
// answer" — and abort before inventory. The dangerous alternative is not an
// error at all: it is degrading to the base price, which sells at the wrong
// number and looks like nothing happened (ADR-028 fail-closed).
func TestAnOversizePriceResolutionIsUnavailableNotAWrongPrice(t *testing.T) {
	catalog := bodyServer(t, int(maxUpstreamResponseBytes)+1)
	s := New(nil, http.DefaultClient, catalog.URL, catalog.URL, "", "secret")

	org, tt := uuid.MustParse(pricingOrg), uuid.MustParse(pricingTT)
	_, err := s.resolveTicketTypePrice(context.Background(), tt, org, 2, nil)
	if !errors.Is(err, errResolveUnavailable) {
		t.Fatalf("want errResolveUnavailable, got %v", err)
	}
	// Specifically NOT errResolveUnusable: that means catalog answered and we
	// judged the answer. Here we never read one.
	if errors.Is(err, errResolveUnusable) {
		t.Fatal("an unread body must not be reported as a body we judged")
	}
	// The seam flattens the cause with %v rather than %w, so errors.Is cannot
	// reach ErrResponseTooLarge from here; the classification, not the sentinel,
	// is what the sale path acts on. The message is still asserted so a refusal
	// cannot become indistinguishable from a transport failure in an incident.
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("the refusal must be legible in the message, got %v", err)
	}
}
