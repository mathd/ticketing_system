package recovery

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// Inventory's transition handler answers 200 when the claim already IS the target
// (services/inventory/internal/store/store.go: `if c.Status == target`). So on the
// release path an already-released claim is a 200, and a 409 can only mean a terminal
// state that is NOT released — in practice `confirmed`.
//
// The two verbs are therefore asymmetric, and reading 409 as "already gone" on both is
// what makes it dangerous: on confirm it means gone, on release it means sold.
func TestInventoryStatusMapping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		verb   string
		status int
		want   error
	}{
		{"release already released", "release", http.StatusOK, nil},
		{"release of a missing claim", "release", http.StatusNotFound, nil},
		// The one that matters: a confirmed claim must not read as a successful release.
		{"release of a confirmed claim", "release", http.StatusConflict, ErrClaimNotReleasable},
		{"confirm ok", "confirm", http.StatusOK, nil},
		{"confirm of a released claim", "confirm", http.StatusConflict, ErrClaimGone},
		{"confirm of a missing claim", "confirm", http.StatusNotFound, ErrClaimGone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			c := HTTPClients{Client: srv.Client(), InventoryURL: srv.URL, Token: "t"}
			var err error
			if tc.verb == "release" {
				err = c.Release(context.Background(), uuid.New(), uuid.New())
			} else {
				err = c.Confirm(context.Background(), uuid.New(), uuid.New())
			}

			if tc.want == nil {
				if err != nil {
					t.Fatalf("%s %d = %v, want nil", tc.verb, tc.status, err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("%s %d = %v, want %v", tc.verb, tc.status, err, tc.want)
			}
		})
	}
}

// An unexpected status must not be read as success: the claim's state is unknown, and
// assuming it was released is exactly the inference ADR-016 §Decision 2 forbids.
func TestUnexpectedInventoryStatusIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := HTTPClients{Client: srv.Client(), InventoryURL: srv.URL, Token: "t"}
	if err := c.Release(context.Background(), uuid.New(), uuid.New()); err == nil {
		t.Fatal("release must not treat a 500 as success")
	}
	if err := c.Confirm(context.Background(), uuid.New(), uuid.New()); err == nil {
		t.Fatal("confirm must not treat a 500 as success")
	}
}

// LookupOperation's 404 is load-bearing evidence: it is what the runner reads as
// "payments never bound a charge". It must be distinguishable from a transport failure.
func TestLookupOperationDistinguishesAbsenceFromFailure(t *testing.T) {
	t.Run("404 is absence, not an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		c := HTTPClients{Client: srv.Client(), PaymentsURL: srv.URL, Token: "t"}
		_, found, err := c.LookupOperation(context.Background(), uuid.New(), "k")
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if found {
			t.Fatal("found = true for a 404")
		}
	})

	t.Run("500 is a failure, never absence", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		c := HTTPClients{Client: srv.Client(), PaymentsURL: srv.URL, Token: "t"}
		if _, _, err := c.LookupOperation(context.Background(), uuid.New(), "k"); err == nil {
			t.Fatal("a 500 must not be reported as 'no operation exists' — that would release a seat whose money may have captured")
		}
	})
}
