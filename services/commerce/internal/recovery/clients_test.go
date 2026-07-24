package recovery

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// The PSP status mapping is the recovery decision table's front door (TKT-115): each
// non-200 carries a distinct meaning — 404 evidence-of-inconsistency, 409 the expired
// replay window (park, never retry), 502 still-ambiguous (retry, never terminal). A
// generic error for any of them would collapse "park" and "retry later" into one path.
func TestPSPStatusMapping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"expired replay window parks", http.StatusConflict, `{"error":"status replay window expired"}`, ErrReplayWindowExpired},
		{"still ambiguous retries", http.StatusBadGateway, `{"error":"provider status unresolved"}`, ErrProviderUnresolved},
		{"missing operation is inconsistent state", http.StatusNotFound, `{"error":"operation not found"}`, ErrOperationNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := HTTPClients{Client: srv.Client(), PaymentsURL: srv.URL, Token: "t"}
			_, err := c.Status(context.Background(), uuid.New(), "k1")
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d = %v, want %v", tc.status, err, tc.want)
			}
		})
	}
}

// A decoded 200 carries the full provider-neutral evidence, integer minor units intact.
func TestPSPStatusDecodesEvidence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/psp/status" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("X-Internal-Token") != "t" {
			t.Error("internal token missing")
		}
		if r.URL.Query().Get("idempotency_key") != "k1" {
			t.Errorf("idempotency_key = %q", r.URL.Query().Get("idempotency_key"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"outcome":"captured","terminal_no_side_effect":false,"captured":true,"authorized":true,"authorized_amount":1250,"captured_amount":1250,"currency":"EUR"}`))
	}))
	defer srv.Close()
	c := HTTPClients{Client: srv.Client(), PaymentsURL: srv.URL, Token: "t"}
	got, err := c.Status(context.Background(), uuid.New(), "k1")
	if err != nil {
		t.Fatal(err)
	}
	want := PSPStatus{Outcome: "captured", Captured: true, Authorized: true, AuthorizedAmount: 1250, CapturedAmount: 1250, Currency: "EUR"}
	if got != want {
		t.Fatalf("status = %+v, want %+v", got, want)
	}
}

// A malformed 200 is a transport failure, not evidence: the caller must get an error it
// retries on, never a zero-valued PSPStatus it might read as terminal.
func TestPSPStatusMalformed200IsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{`))
	}))
	defer srv.Close()
	c := HTTPClients{Client: srv.Client(), PaymentsURL: srv.URL, Token: "t"}
	if _, err := c.Status(context.Background(), uuid.New(), "k1"); err == nil {
		t.Fatal("a malformed 200 must be an error")
	}
}

// Void and refund share the compensation mapping: 409 = wrong compensation for the
// stored evidence (re-derive, not failure, not success), 502 = bound and recoverable.
// The POST body carries only the operation identity — amounts live in payments.
func TestCompensationMapping(t *testing.T) {
	org := uuid.New()
	for _, verb := range []string{"void", "refund"} {
		t.Run(verb, func(t *testing.T) {
			var gotPath, gotBody string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				b := make([]byte, 1024)
				n, _ := r.Body.Read(b)
				gotBody = string(b[:n])
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"` + verb + `ed","fact_id":"` + uuid.NewString() + `","replay":false}`))
			}))
			defer srv.Close()
			c := HTTPClients{Client: srv.Client(), PaymentsURL: srv.URL, Token: "t"}
			call := c.Void
			if verb == "refund" {
				call = c.Refund
			}
			res, err := call(context.Background(), org, "k1")
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != verb+"ed" || res.Replay {
				t.Fatalf("result = %+v", res)
			}
			if gotPath != "/internal/psp/"+verb {
				t.Fatalf("path = %s", gotPath)
			}
			if !strings.Contains(gotBody, org.String()) || !strings.Contains(gotBody, `"k1"`) {
				t.Fatalf("body must carry the operation identity, got %s", gotBody)
			}
		})
	}
	// A 200 whose body does not SAY the compensation completed is not proof of anything
	// — an empty or wrong-status answer (version skew, an upstream bug) must fail
	// closed as unresolved, because callers release seats and mark orders refunded on
	// this result (ai-review B1).
	for _, body := range []string{`{}`, `{"status":"pending","replay":false}`, `{"status":"voided","replay":false}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		c := HTTPClients{Client: srv.Client(), PaymentsURL: srv.URL, Token: "t"}
		if _, err := c.Refund(context.Background(), org, "k1"); !errors.Is(err, ErrProviderUnresolved) {
			t.Fatalf("refund 200 with body %s = %v, want ErrProviderUnresolved", body, err)
		}
		srv.Close()
	}
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusConflict, ErrWrongCompensation},
		{http.StatusBadGateway, ErrProviderUnresolved},
		{http.StatusNotFound, ErrOperationNotFound},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
		}))
		c := HTTPClients{Client: srv.Client(), PaymentsURL: srv.URL, Token: "t"}
		if _, err := c.Refund(context.Background(), org, "k1"); !errors.Is(err, tc.want) {
			t.Fatalf("refund %d = %v, want %v", tc.status, err, tc.want)
		}
		srv.Close()
	}
}

// LookupOperation now surfaces the durable bind time and, when the provider bounds
// same-key replay, the deadline — the runner's park-before-call check reads these.
func TestLookupOperationParsesDeadline(t *testing.T) {
	occurred := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	deadline := occurred.Add(24 * time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resolved":false,"occurred_at":"2026-07-22T12:00:00Z","status_replay_deadline_at":"2026-07-23T12:00:00Z"}`))
	}))
	defer srv.Close()
	c := HTTPClients{Client: srv.Client(), PaymentsURL: srv.URL, Token: "t"}
	op, found, err := c.LookupOperation(context.Background(), uuid.New(), "k1")
	if err != nil || !found {
		t.Fatalf("lookup: found=%v err=%v", found, err)
	}
	if !op.OccurredAt.Equal(occurred) {
		t.Fatalf("occurred_at = %v, want %v", op.OccurredAt, occurred)
	}
	if op.StatusReplayDeadlineAt == nil || !op.StatusReplayDeadlineAt.Equal(deadline) {
		t.Fatalf("deadline = %v, want %v", op.StatusReplayDeadlineAt, deadline)
	}
}

// json.Decoder.Decode stops at the first complete value, so a 200 body with trailing
// bytes decoded cleanly and — since the B1 status check only reads the decoded prefix —
// was accepted as proof a refund completed. The callers release seats and mark orders
// refunded on that answer (ai-review pass 2, F1).
func TestCompensationRejectsTrailingContent(t *testing.T) {
	for _, body := range []string{
		`{"status":"refunded"}garbage`,
		`{"status":"refunded"}{"status":"voided"}`,
	} {
		t.Run(body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()

			c := HTTPClients{Client: srv.Client(), PaymentsURL: srv.URL, Token: "t"}
			got, err := c.Refund(context.Background(), uuid.New(), "key")
			if err == nil {
				t.Fatalf("Refund accepted a body with trailing content: %+v", got)
			}
			if got.Status != "" {
				t.Fatalf("Refund returned a result alongside the error: %+v", got)
			}
		})
	}
}

// actOnProviderStatus dispatches on Outcome alone: `declined` goes straight to
// recordAndRelease. A body whose outcome contradicts its money flags would therefore
// free the seats against money the same body says is captured — the release-on-unproven
// case ADR-016 §D3 forbids. Fail closed at the boundary (ai-review pass 2, F2).
func TestStatusRejectsContradictoryEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		ok   bool
	}{
		// The dangerous shapes: a terminal answer carrying live money.
		{"declined but captured", `{"outcome":"declined","terminal_no_side_effect":true,"captured":true}`, false},
		{"timeout but authorized", `{"outcome":"timeout","terminal_no_side_effect":true,"authorized":true}`, false},
		{"voided but captured", `{"outcome":"voided","terminal_no_side_effect":true,"captured":true}`, false},
		{"declined without no-side-effect", `{"outcome":"declined"}`, false},
		{"captured but flagged no-side-effect", `{"outcome":"captured","captured":true,"terminal_no_side_effect":true}`, false},
		{"captured without captured money", `{"outcome":"captured"}`, false},
		{"authorized but captured", `{"outcome":"authorized","authorized":true,"captured":true}`, false},
		// A refund's money already came back: no live money, NOT no-side-effect.
		{"refunded flagged no-side-effect", `{"outcome":"refunded","terminal_no_side_effect":true}`, false},
		// The shapes payments actually emits must keep passing.
		{"captured", `{"outcome":"captured","captured":true,"authorized":true}`, true},
		{"authorized", `{"outcome":"authorized","authorized":true}`, true},
		{"declined", `{"outcome":"declined","terminal_no_side_effect":true}`, true},
		{"timeout", `{"outcome":"timeout","terminal_no_side_effect":true}`, true},
		{"voided", `{"outcome":"voided","terminal_no_side_effect":true}`, true},
		{"refunded", `{"outcome":"refunded"}`, true},
		// An unknown outcome proves nothing on its own; the runner retries it.
		{"unknown", `{"outcome":"unknown"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := HTTPClients{Client: srv.Client(), PaymentsURL: srv.URL, Token: "t"}
			_, err := c.Status(context.Background(), uuid.New(), "key")
			if tc.ok {
				if err != nil {
					t.Fatalf("Status rejected a body payments emits: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrProviderUnresolved) {
				t.Fatalf("Status(%s) = %v, want ErrProviderUnresolved", tc.body, err)
			}
		})
	}
}
