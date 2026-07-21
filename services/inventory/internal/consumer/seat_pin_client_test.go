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
