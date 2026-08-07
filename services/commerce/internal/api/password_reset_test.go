package api

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	commercestore "ticketing/services/commerce/internal/store"
)

// Password-reset handlers (TKT-226, ADR-050).
//
// The store seams are package-level function values, so these tests never need a
// database: the token logic is proven against real Postgres in the store package, and
// what is under test HERE is the mapping from a store verdict to an HTTP answer — which
// is exactly where an enumeration oracle gets reintroduced by accident.

func swapResetStore(
	t *testing.T,
	request func(ctx context.Context, email string, compose func(commercestore.IssuedResetToken) (string, string, string)) (bool, error),
	complete func(ctx context.Context, token, password string) (uuid.UUID, error),
) {
	t.Helper()
	prevRequest, prevComplete := requestPasswordResetFn, completePasswordResetFn
	if request != nil {
		requestPasswordResetFn = func(ctx context.Context, _ *sql.DB, email string, compose func(commercestore.IssuedResetToken) (string, string, string)) (bool, error) {
			return request(ctx, email, compose)
		}
	}
	if complete != nil {
		completePasswordResetFn = func(ctx context.Context, _ *sql.DB, token, password string) (uuid.UUID, error) {
			return complete(ctx, token, password)
		}
	}
	t.Cleanup(func() { requestPasswordResetFn, completePasswordResetFn = prevRequest, prevComplete })
}

func postTo(t *testing.T, h http.HandlerFunc, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// THE test for this endpoint. A known and an unknown address must produce the same
// status and the same bytes — if this ever fails, the membership oracle ADR-049 §2
// regrets on registration has been rebuilt on recovery.
func TestRequestPasswordResetAnswersIdenticallyForKnownAndUnknownAddresses(t *testing.T) {
	s := &Server{}
	var seen []string

	swapResetStore(t, func(_ context.Context, email string, _ func(commercestore.IssuedResetToken) (string, string, string)) (bool, error) {
		seen = append(seen, email)
		// The known address resolves, the unknown one does not — and the handler must
		// not be able to tell.
		return email == "known@example.test", nil
	}, nil)

	known := postTo(t, s.requestPasswordReset, "/customers/password-reset", `{"email":"known@example.test"}`)
	unknown := postTo(t, s.requestPasswordReset, "/customers/password-reset", `{"email":"nobody@example.test"}`)

	if len(seen) != 2 {
		t.Fatalf("both addresses must reach the store, got %d", len(seen))
	}
	if known.Code != http.StatusAccepted || unknown.Code != http.StatusAccepted {
		t.Fatalf("statuses differ or are not 202: known=%d unknown=%d", known.Code, unknown.Code)
	}
	if !bytes.Equal(known.Body.Bytes(), unknown.Body.Bytes()) {
		t.Fatalf("response bodies differ — this is an account-existence oracle:\n known:   %s\n unknown: %s",
			known.Body.String(), unknown.Body.String())
	}
	// And the headers a caller can read must not differ either.
	if known.Header().Get("Content-Type") != unknown.Header().Get("Content-Type") {
		t.Fatal("Content-Type differs between a known and an unknown address")
	}
}

// A store outage must not be reported as success. "We sent you a link" when nothing was
// enqueued leaves a locked-out buyer waiting forever for mail that does not exist.
func TestRequestPasswordResetReportsAnOutageRatherThanClaimingSuccess(t *testing.T) {
	s := &Server{}
	swapResetStore(t, func(context.Context, string, func(commercestore.IssuedResetToken) (string, string, string)) (bool, error) {
		return false, errors.New("database is down")
	}, nil)

	rec := postTo(t, s.requestPasswordReset, "/customers/password-reset", `{"email":"a@b.test"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), resetAccepted) {
		t.Fatalf("an outage answered like an acceptance: %s", rec.Body.String())
	}
}

// The submitted address must not reach a log. A log is a durable, widely-readable
// record, so writing the addresses someone is probing into it is the same disclosure as
// echoing them back, with a longer half-life.
func TestRequestPasswordResetDoesNotLogTheSubmittedAddress(t *testing.T) {
	const probed = "victim@example.test"
	var captured strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&captured, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s := &Server{}
	swapResetStore(t, func(context.Context, string, func(commercestore.IssuedResetToken) (string, string, string)) (bool, error) {
		return false, errors.New("database is down")
	}, nil)
	postTo(t, s.requestPasswordReset, "/customers/password-reset", `{"email":"`+probed+`"}`)

	out := captured.String()
	if out == "" {
		t.Fatal("nothing was logged at all; this test would pass vacuously")
	}
	if strings.Contains(out, probed) {
		t.Fatalf("the probed address reached the log:\n%s", out)
	}
}

func TestCompletePasswordResetMapsEveryStoreVerdict(t *testing.T) {
	customer := uuid.New()
	for _, tc := range []struct {
		name       string
		storeErr   error
		wantStatus int
		wantBody   string
	}{
		{"redeemed", nil, http.StatusOK, customer.String()},
		{"unknown or expired or used", commercestore.ErrResetTokenUnusable, http.StatusBadRequest, resetTokenUnusable},
		{"unusable password", commercestore.ErrCustomerPasswordUnusable, http.StatusBadRequest, "invalid request"},
		{"outage", errors.New("database is down"), http.StatusInternalServerError, customerUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			swapResetStore(t, nil, func(context.Context, string, string) (uuid.UUID, error) {
				if tc.storeErr != nil {
					return uuid.Nil, tc.storeErr
				}
				return customer, nil
			})
			rec := postTo(t, s.completePasswordReset, "/customers/password-reset/complete",
				`{"token":"t","password":"a new password"}`)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("body = %s, want it to contain %q", rec.Body.String(), tc.wantBody)
			}
		})
	}
}

// A rejected NEW password must not be reported as a dead link. Telling a buyer their
// link expired when it still works sends them back to a mailbox for a message that will
// never arrive — they already used their one request.
func TestARejectedPasswordIsNotReportedAsADeadLink(t *testing.T) {
	s := &Server{}
	swapResetStore(t, nil, func(context.Context, string, string) (uuid.UUID, error) {
		return uuid.Nil, commercestore.ErrCustomerPasswordUnusable
	})
	rec := postTo(t, s.completePasswordReset, "/customers/password-reset/complete",
		`{"token":"t","password":"short"}`)
	if strings.Contains(rec.Body.String(), resetTokenUnusable) {
		t.Fatalf("a password problem was reported as a token problem: %s", rec.Body.String())
	}
}

// The completion response carries the customer id and NOTHING else — specifically not an
// assertion. Completing a reset is not signing in, and returning a bearer credential to
// whoever redeemed a mailed token would make the link a sign-in bypass.
func TestCompletionReturnsNoAssertion(t *testing.T) {
	customer := uuid.New()
	s := &Server{}
	swapResetStore(t, nil, func(context.Context, string, string) (uuid.UUID, error) { return customer, nil })
	rec := postTo(t, s.completePasswordReset, "/customers/password-reset/complete",
		`{"token":"t","password":"a new password"}`)
	body := rec.Body.String()
	if !strings.Contains(body, customer.String()) {
		t.Fatalf("the customer id must be returned so the caller can destroy sessions: %s", body)
	}
	if strings.Contains(body, "assertion") {
		t.Fatalf("completing a reset must not mint a credential: %s", body)
	}
}

// Host-header injection. The reset link's origin is server-configured, and building it
// from the request would let a caller submit a victim's address with their own Host and
// have the victim receive a GENUINE reset link pointing at the attacker's site.
func TestTheResetLinkUsesTheConfiguredOriginNotTheRequestHost(t *testing.T) {
	s := (&Server{}).WithPublicURL("https://tickets.example.test/")

	var body string
	swapResetStore(t, func(_ context.Context, _ string, compose func(commercestore.IssuedResetToken) (string, string, string)) (bool, error) {
		_, _, body = compose(commercestore.IssuedResetToken{Raw: "the-token", Email: "buyer@example.test"})
		return true, nil
	}, nil)

	req := httptest.NewRequest(http.MethodPost, "/customers/password-reset", strings.NewReader(`{"email":"buyer@example.test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Host = "attacker.example.test"
	req.Header.Set("X-Forwarded-Host", "attacker.example.test")
	s.requestPasswordReset(httptest.NewRecorder(), req)

	if !strings.Contains(body, "https://tickets.example.test/en/account/reset-password?token=the-token") {
		t.Fatalf("the link is not built from the configured origin:\n%s", body)
	}
	if strings.Contains(body, "attacker.example.test") {
		t.Fatalf("the request host reached the mailed link — this is a phishing generator:\n%s", body)
	}
}

// The raw token must survive into the link intact, and it must be query-escaped so a
// base64url value cannot be reinterpreted. base64url has no characters that need
// escaping, which is why it was chosen — this pins that the escaping does not corrupt it.
func TestTheRawTokenSurvivesIntoTheLink(t *testing.T) {
	s := (&Server{}).WithPublicURL("https://tickets.example.test")
	const raw = "abcDEF-123_xyz"

	var body, recipient string
	swapResetStore(t, func(_ context.Context, _ string, compose func(commercestore.IssuedResetToken) (string, string, string)) (bool, error) {
		recipient, _, body = compose(commercestore.IssuedResetToken{Raw: raw, Email: "Buyer@Example.test"})
		return true, nil
	}, nil)
	postTo(t, s.requestPasswordReset, "/customers/password-reset", `{"email":"buyer@example.test"}`)

	if !strings.Contains(body, "token="+raw) {
		t.Fatalf("the token did not survive into the link:\n%s", body)
	}
	// The message goes to the address as REGISTERED, not as typed — the store resolves
	// the display spelling, and mailing the typed form would let a caller choose the
	// recipient's spelling for a message they never see.
	if recipient != "Buyer@Example.test" {
		t.Fatalf("recipient = %q, want the registered spelling", recipient)
	}
}

// A reset link with no origin is a broken link, so an unconfigured PUBLIC_BASE_URL must
// be loud at startup. It must NOT refuse to start: every other commerce operation is
// unaffected, and that blast radius is wrong for one optional feature.
func TestAnUnconfiguredPublicOriginWarnsRatherThanFailing(t *testing.T) {
	var captured strings.Builder
	log := slog.New(slog.NewTextHandler(&captured, &slog.HandlerOptions{Level: slog.LevelDebug}))

	WarnIfResetMailUnconfigured(log, "")
	if !strings.Contains(captured.String(), "PUBLIC_BASE_URL") {
		t.Fatalf("an unset origin must warn, got %q", captured.String())
	}

	captured.Reset()
	WarnIfResetMailUnconfigured(log, "https://tickets.example.test")
	if captured.Len() != 0 {
		t.Fatalf("a configured origin must be silent, got %q", captured.String())
	}
}

// chi prefers a static segment over a parameter, so `/customers/password-reset` must not
// be read as a customer id by `/customers/{id}/orders`. Asserted rather than assumed —
// the same care TKT-223 took for `/orders/claim`.
func TestResetRoutesAreNotSwallowedByTheCustomerIdRoute(t *testing.T) {
	s := &Server{}
	swapResetStore(t, func(context.Context, string, func(commercestore.IssuedResetToken) (string, string, string)) (bool, error) {
		return true, nil
	}, func(context.Context, string, string) (uuid.UUID, error) { return uuid.New(), nil })

	h := s.Router(slog.New(slog.NewTextHandler(&strings.Builder{}, nil)), false)
	for _, tc := range []struct{ path, body string }{
		{"/customers/password-reset", `{"email":"a@b.test"}`},
		{"/customers/password-reset/complete", `{"token":"t","password":"a new password"}`},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
			t.Fatalf("%s did not reach its handler (status %d)", tc.path, rec.Code)
		}
	}
}
