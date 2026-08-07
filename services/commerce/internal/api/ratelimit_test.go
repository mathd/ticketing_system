package api

// TKT-224 / ADR-051. What is under test here is the HTTP behaviour of the limit:
// the bucket arithmetic itself is proven in shared/go/ratelimit, and re-proving it
// through a handler would only test the same thing more slowly.

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	commercestore "ticketing/services/commerce/internal/store"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// limitedServer is a Server whose credential store always REFUSES. Every test
// below drives the refusal path on purpose: that is the path an enumerator walks,
// and it is the one where a limit that only applied to successes would be useless.
func limitedServer(t *testing.T, c *testClock) *Server {
	t.Helper()
	swapCustomerStore(t,
		func(_ context.Context, _, _ string) (commercestore.CustomerAccount, error) {
			return commercestore.CustomerAccount{}, commercestore.ErrCustomerEmailTaken
		},
		func(_ context.Context, _, _ string) (commercestore.CustomerAccount, error) {
			return commercestore.CustomerAccount{}, commercestore.ErrCustomerCredentialsInvalid
		})
	return (&Server{}).WithClock(c.now)
}

// postAs drives a handler with an explicit client IP, so the per-source limiter
// can be steered independently of the per-subject one.
func postAs(h http.HandlerFunc, ip, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip + ":51000"
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// throughSource wraps a handler in the real per-source middleware, which is how
// the route group mounts it. Calling the bare handler would exercise only the
// per-subject half and quietly report the source limiter as untested.
func throughSource(s *Server, h http.HandlerFunc) http.HandlerFunc {
	wrapped := s.limitSource(h)
	return func(w http.ResponseWriter, r *http.Request) { wrapped.ServeHTTP(w, r) }
}

func signIn(s *Server, ip, email string) *httptest.ResponseRecorder {
	return postAs(s.authenticateCustomer, ip, "/customers/authenticate",
		`{"email":"`+email+`","password":"whatever"}`)
}

// AC: the limiter refuses under the intended volume.
//
// The two boundaries are asserted together on purpose. "It refuses eventually" is
// satisfied by a limiter that refuses on call 2, which would break every real
// buyer; only pinning BOTH edges says the budget is the one that was designed.
func TestTheSubjectBudgetIsSpendableInFullAndThenRefuses(t *testing.T) {
	c := newTestClock()
	s := limitedServer(t, c)

	for i := range customerSubjectBurst {
		if rec := signIn(s, "203.0.113.1", "buyer@example.test"); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("call %d of %d was rate limited inside the budget", i+1, customerSubjectBurst)
		}
	}
	rec := signIn(s, "203.0.113.1", "buyer@example.test")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d after spending the whole budget, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("a 429 with no Retry-After leaves a legitimate buyer guessing")
	}
}

// AC: a normal buyer flow is never refused.
//
// Four subject-scoped calls, which is what the budget was sized against. If this
// ever fails the threshold moved under a real buyer, not just under a test.
func TestTheOrdinaryBuyerFlowIsNeverRefused(t *testing.T) {
	c := newTestClock()
	swapCustomerStore(t,
		func(_ context.Context, email, _ string) (commercestore.CustomerAccount, error) {
			return commercestore.CustomerAccount{ID: uuid.New(), Email: email}, nil
		},
		func(_ context.Context, email, password string) (commercestore.CustomerAccount, error) {
			if password == "mistyped" {
				return commercestore.CustomerAccount{}, commercestore.ErrCustomerCredentialsInvalid
			}
			return commercestore.CustomerAccount{ID: uuid.New(), Email: email}, nil
		})
	s := (&Server{}).WithClock(c.now)

	const ip, email = "203.0.113.7", "buyer@example.test"
	steps := []struct {
		what string
		rec  *httptest.ResponseRecorder
	}{
		{"register", postAs(s.registerCustomer, ip, "/customers", `{"email":"`+email+`","password":"correct horse battery"}`)},
		{"sign in", signIn(s, ip, email)},
		{"mistype once", postAs(s.authenticateCustomer, ip, "/customers/authenticate", `{"email":"`+email+`","password":"mistyped"}`)},
		{"sign in again", signIn(s, ip, email)},
	}
	for _, step := range steps {
		if step.rec.Code == http.StatusTooManyRequests {
			t.Fatalf("%q was rate limited; the budget does not fit an ordinary buyer", step.what)
		}
	}
}

// THE ORACLE TEST, and the reason the limiter refuses before the store call.
//
// If the bucket only filled for addresses that EXIST, a 429 would mean "this
// address is real" — a sharper disclosure than the 409 this limiter was added to
// blunt (ADR-049 §2). The refusals must be byte-identical, so this compares the
// whole response, not just the status.
func TestARateLimitedKnownAddressIsIndistinguishableFromAnUnknownOne(t *testing.T) {
	c := newTestClock()
	// The store answers DIFFERENTLY for the two addresses — 409 for the known one,
	// 401 for the unknown. That difference is the oracle this test hunts: if any of
	// it survives into the rate-limited answer, the assertion below fails.
	swapCustomerStore(t, func(_ context.Context, email, _ string) (commercestore.CustomerAccount, error) {
		if strings.Contains(email, "known") {
			return commercestore.CustomerAccount{}, commercestore.ErrCustomerEmailTaken
		}
		return commercestore.CustomerAccount{}, commercestore.ErrCustomerCredentialsInvalid
	}, nil)
	s := (&Server{}).WithClock(c.now)

	spend := func(email string) *httptest.ResponseRecorder {
		var last *httptest.ResponseRecorder
		for range customerSubjectBurst + 1 {
			last = postAs(s.registerCustomer, "203.0.113.2", "/customers",
				`{"email":"`+email+`","password":"correct horse battery"}`)
		}
		return last
	}

	known := spend("known@example.test")
	unknown := spend("stranger@example.test")

	if known.Code != http.StatusTooManyRequests || unknown.Code != http.StatusTooManyRequests {
		t.Fatalf("both should be limited: known=%d unknown=%d", known.Code, unknown.Code)
	}
	if known.Body.String() != unknown.Body.String() {
		t.Fatalf("the refusals differ, which is an account-existence oracle:\n known=%s\n unknown=%s",
			known.Body.String(), unknown.Body.String())
	}
	if known.Header().Get("Retry-After") != unknown.Header().Get("Retry-After") {
		t.Fatal("Retry-After differs between a known and an unknown address — still an oracle")
	}
}

// Budgets are per subject, or one buyer exhausting theirs would refuse everyone.
func TestOneAddressExhaustingItsBudgetDoesNotRefuseAnother(t *testing.T) {
	c := newTestClock()
	s := limitedServer(t, c)

	for range customerSubjectBurst + 1 {
		signIn(s, "203.0.113.3", "victim@example.test")
	}
	if rec := signIn(s, "203.0.113.3", "someone-else@example.test"); rec.Code == http.StatusTooManyRequests {
		t.Fatal("a second address was refused because the first spent its own budget")
	}
}

// Case and surrounding whitespace must not buy a fresh budget. This is why the
// subject key delegates to the store's normalizer instead of having its own idea
// of one — two normalizers is the bypass.
func TestVaryingTheCaseOfAnAddressDoesNotResetItsBudget(t *testing.T) {
	c := newTestClock()
	s := limitedServer(t, c)

	for range customerSubjectBurst {
		signIn(s, "203.0.113.4", "buyer@example.test")
	}
	if rec := signIn(s, "203.0.113.4", "  BuYeR@Example.TEST  "); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d for a case-varied spelling of an exhausted address, want 429", rec.Code)
	}
}

// The per-source half. Rotating the ADDRESS gives a fresh subject budget every
// time — that is inherent to a per-subject key — so without a source limit an
// enumerator walking a list would never be refused at all. This is the test that
// says the second limiter earns its place.
func TestRotatingTheAddressDoesNotEscapeTheSourceBudget(t *testing.T) {
	c := newTestClock()
	s := limitedServer(t, c)

	var limited bool
	for i := range customerSourceBurst + 1 {
		rec := postAs(throughSource(s, s.registerCustomer), "203.0.113.5", "/customers",
			`{"email":"walk-`+strconv.Itoa(i)+`@example.test","password":"correct horse battery"}`)
		if rec.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatalf("an enumerator rotating addresses was never refused within %d calls", customerSourceBurst+1)
	}
}

// Sources are independent of each other, so one noisy egress does not refuse a
// different one.
func TestOneSourceExhaustingItsBudgetDoesNotRefuseAnother(t *testing.T) {
	c := newTestClock()
	s := limitedServer(t, c)

	for i := range customerSourceBurst + 1 {
		postAs(throughSource(s, s.registerCustomer), "203.0.113.6", "/customers",
			`{"email":"walk-`+strconv.Itoa(i)+`@example.test","password":"correct horse battery"}`)
	}
	rec := postAs(throughSource(s, s.registerCustomer), "198.51.100.1", "/customers",
		`{"email":"fresh@example.test","password":"correct horse battery"}`)
	if rec.Code == http.StatusTooManyRequests {
		t.Fatal("a different source was refused because the first spent its budget")
	}
}

// The budget recovers, or a shared NAT egress that trips the limit once is barred
// for the rest of the process's life.
func TestTheBudgetRecoversOverTime(t *testing.T) {
	c := newTestClock()
	s := limitedServer(t, c)

	for range customerSubjectBurst + 1 {
		signIn(s, "203.0.113.8", "buyer@example.test")
	}
	c.advance(customerSubjectWindow)
	if rec := signIn(s, "203.0.113.8", "buyer@example.test"); rec.Code == http.StatusTooManyRequests {
		t.Fatal("a full window did not restore the budget")
	}
}

// clientIP is the source key, so what it trusts is a security property.
func TestClientIPPrefersTheLastForwardedHopAndFallsBackToThePeer(t *testing.T) {
	for name, tc := range map[string]struct {
		xff, remote, want string
	}{
		"no header falls back to the peer":   {"", "203.0.113.9:44444", "203.0.113.9"},
		"a single hop is the gateway's":      {"203.0.113.9", "10.0.0.1:5", "203.0.113.9"},
		"the LAST hop wins, never the first": {"1.2.3.4, 203.0.113.9", "10.0.0.1:5", "203.0.113.9"},
		"whitespace around the hop":          {"1.2.3.4,   203.0.113.9  ", "10.0.0.1:5", "203.0.113.9"},
		"a peer with no port is used whole":  {"", "not-a-host-port", "not-a-host-port"},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/customers", nil)
			req.RemoteAddr = tc.remote
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := clientIP(req); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// The source budget is a cap on the WHOLE SITE's form traffic, not on one
// client's, because every storefront-originated call reaches commerce with the
// storefront container's address (the gateway replaces X-Forwarded-For with its
// peer on that hop). See the constant's comment and ADR-051 §3.
//
// This test exists to fail when someone tightens it toward a per-person figure.
// That change looks obviously correct in isolation — "60 sign-in attempts per
// quarter hour is plenty for one buyer" — and would throttle the entire
// storefront during an on-sale. The ratio is the guard rail.
func TestTheSourceBudgetIsSizedForTheWholeSiteNotOnePerson(t *testing.T) {
	if customerSourceBurst < customerSubjectBurst*20 {
		t.Fatalf("customerSourceBurst = %d, which is only %.1fx the per-subject budget of %d.\n"+
			"Every storefront form submission shares ONE source key, so this is a cap on the site's\n"+
			"aggregate traffic. Sizing it per-person throttles every buyer at once. ADR-051 §3.",
			customerSourceBurst, float64(customerSourceBurst)/float64(customerSubjectBurst), customerSubjectBurst)
	}
	if customerSourceWindow != customerSubjectWindow {
		t.Fatal("the two windows have drifted apart; Retry-After advertises the subject rate and would mislead")
	}
}

// Exhausting the CREDENTIAL budget must not refuse account recovery.
//
// This is the flaw the browser gate caught (test/browser/rate-limit.mjs). One
// bucket per address across both looked obviously right: it is the same subject,
// and separate budgets let an enumerator probe twice as much. But the buyer who
// mistypes their password until the budget is gone is precisely the buyer who
// then clicks "forgot password", and a shared bucket refuses them the way back
// in — a lockout built out of a limiter, on the one path that exists to undo it.
func TestSpendingTheSignInBudgetDoesNotRefusePasswordRecovery(t *testing.T) {
	c := newTestClock()
	s := limitedServer(t, c)

	prev := requestPasswordResetFn
	requestPasswordResetFn = func(_ context.Context, _ *sql.DB, _ string,
		_ func(commercestore.IssuedResetToken) (string, string, string)) (bool, error) {
		return true, nil
	}
	t.Cleanup(func() { requestPasswordResetFn = prev })

	const email = "locked-out@example.test"
	for range customerSubjectBurst + 5 {
		signIn(s, "203.0.113.9", email)
	}
	// The precondition, asserted rather than assumed: without it a passing test
	// below would prove nothing, because nothing would have been exhausted.
	if rec := signIn(s, "203.0.113.9", email); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("sign-in status = %d; the credential budget was not exhausted, so this test proves nothing", rec.Code)
	}

	rec := postAs(s.requestPasswordReset, "203.0.113.9", "/customers/password-reset",
		`{"email":"`+email+`"}`)
	if rec.Code == http.StatusTooManyRequests {
		t.Fatal("a locked-out buyer could not request a reset — the limiter has become a lockout")
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("reset request status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
}

// ...and the recovery budget still binds on its own, or the split above would
// have quietly removed the mail-bombing bound ADR-050 asked for.
func TestTheRecoveryBudgetStillLimitsOnItsOwn(t *testing.T) {
	c := newTestClock()
	s := limitedServer(t, c)

	prev := requestPasswordResetFn
	requestPasswordResetFn = func(_ context.Context, _ *sql.DB, _ string,
		_ func(commercestore.IssuedResetToken) (string, string, string)) (bool, error) {
		return true, nil
	}
	t.Cleanup(func() { requestPasswordResetFn = prev })

	const email = "mailbombed@example.test"
	body := `{"email":"` + email + `"}`
	for i := range customerSubjectBurst {
		if rec := postAs(s.requestPasswordReset, "203.0.113.10", "/customers/password-reset", body); rec.Code != http.StatusAccepted {
			t.Fatalf("reset %d of %d refused inside the budget: %d", i+1, customerSubjectBurst, rec.Code)
		}
	}
	if rec := postAs(s.requestPasswordReset, "203.0.113.10", "/customers/password-reset", body); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d after spending the recovery budget, want 429 — one address can be mail bombed", rec.Code)
	}
}
