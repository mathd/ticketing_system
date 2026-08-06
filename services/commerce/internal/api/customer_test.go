package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	commercestore "ticketing/services/commerce/internal/store"
)

// Customer account handlers (TKT-220 / US-A1, ADR-049).
//
// The store seams are package-level function values so these tests never need a
// database: the credential logic itself is proven in the store package, and what
// is under test HERE is the mapping from a store verdict to an HTTP answer —
// which is where an enumeration oracle gets reintroduced by accident.

func swapCustomerStore(t *testing.T, register func(context.Context, string, string) (commercestore.CustomerAccount, error), authenticate func(context.Context, string, string) (commercestore.CustomerAccount, error)) {
	t.Helper()
	prevRegister, prevAuthenticate := registerCustomerFn, authenticateCustomerFn
	if register != nil {
		registerCustomerFn = func(ctx context.Context, _ *sql.DB, email, password string) (commercestore.CustomerAccount, error) {
			return register(ctx, email, password)
		}
	}
	if authenticate != nil {
		authenticateCustomerFn = func(ctx context.Context, _ *sql.DB, email, password string) (commercestore.CustomerAccount, error) {
			return authenticate(ctx, email, password)
		}
	}
	t.Cleanup(func() { registerCustomerFn, authenticateCustomerFn = prevRegister, prevAuthenticate })
}

func postJSON(t *testing.T, h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/customers", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestRegisterCustomerReturnsThePrincipalAndNoPasswordMaterial(t *testing.T) {
	id := uuid.New()
	swapCustomerStore(t, func(_ context.Context, email, password string) (commercestore.CustomerAccount, error) {
		if password != "correct horse battery" {
			t.Fatalf("password reached the store mangled: %q", password)
		}
		return commercestore.CustomerAccount{ID: id, Email: strings.TrimSpace(email)}, nil
	}, nil)

	s := &Server{}
	rec := postJSON(t, s.registerCustomer, `{"email":"  Buyer@Example.TEST  ","password":"correct horse battery"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store — a credential response must not be cached", got)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["customer_id"] != id.String() {
		t.Fatalf("customer_id = %v, want %s", out["customer_id"], id)
	}
	// The ORIGINAL spelling, trimmed — not the normalized key, which is never exposed.
	if out["email"] != "Buyer@Example.TEST" {
		t.Fatalf("email = %v, want the original spelling", out["email"])
	}
	for _, forbidden := range []string{"password", "password_hash", "email_key"} {
		if _, present := out[forbidden]; present {
			t.Fatalf("the response carries %q", forbidden)
		}
	}
	if strings.Contains(rec.Body.String(), "correct horse battery") {
		t.Fatal("the response echoes the submitted password")
	}
}

// 409, and the body says only that the address is taken. This is a deliberate,
// documented disclosure (ADR-049) — the test exists so that it stays the ONLY
// one, and so that a later change to "invalid request" style errors does not
// silently turn it into something else.
func TestRegisterCustomerAnswers409ForADuplicateAddress(t *testing.T) {
	swapCustomerStore(t, func(context.Context, string, string) (commercestore.CustomerAccount, error) {
		return commercestore.CustomerAccount{}, commercestore.ErrCustomerEmailTaken
	}, nil)

	s := &Server{}
	rec := postJSON(t, s.registerCustomer, `{"email":"taken@example.test","password":"correct horse battery"}`)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), customerEmailTaken) {
		t.Fatalf("body = %s, want the single duplicate message", rec.Body.String())
	}
}

// A store outage is not a credential verdict, and it is not a duplicate either.
// Answering 409 on a database failure would tell a buyer they already have an
// account they do not have; leaking the error text hands out internals.
func TestRegisterCustomerAnswers500OnAStoreFailureWithoutLeakingIt(t *testing.T) {
	swapCustomerStore(t, func(context.Context, string, string) (commercestore.CustomerAccount, error) {
		return commercestore.CustomerAccount{}, errors.New("dial tcp 10.1.2.3:5432: connection refused")
	}, nil)

	s := &Server{}
	rec := postJSON(t, s.registerCustomer, `{"email":"buyer@example.test","password":"correct horse battery"}`)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "10.1.2.3") || strings.Contains(rec.Body.String(), "connection refused") {
		t.Fatalf("the response leaks the underlying error: %s", rec.Body.String())
	}
}

// The whole point of the store's constant-shape comparison is undone if the
// handler answers differently. Asserting equal STATUS is not enough — the bodies
// must be byte-identical, because a one-word difference is exactly what an
// enumerator reads.
func TestAuthenticateCustomerAnswersIdenticallyForUnknownAndWrongPassword(t *testing.T) {
	swapCustomerStore(t, nil, func(context.Context, string, string) (commercestore.CustomerAccount, error) {
		return commercestore.CustomerAccount{}, commercestore.ErrCustomerCredentialsInvalid
	})

	s := &Server{}
	unknown := postJSON(t, s.authenticateCustomer, `{"email":"nobody@example.test","password":"whatever"}`)
	wrong := postJSON(t, s.authenticateCustomer, `{"email":"buyer@example.test","password":"whatever"}`)

	if unknown.Code != http.StatusUnauthorized || wrong.Code != http.StatusUnauthorized {
		t.Fatalf("statuses = %d and %d, want 401 for both", unknown.Code, wrong.Code)
	}
	if !bytes.Equal(unknown.Body.Bytes(), wrong.Body.Bytes()) {
		t.Fatalf("bodies differ:\n unknown: %s wrong:   %s", unknown.Body.String(), wrong.Body.String())
	}
	// And neither echoes the address back — a reflection surface puts the
	// addresses someone is probing into anything that captures the response.
	if strings.Contains(wrong.Body.String(), "buyer@example.test") {
		t.Fatalf("the refusal echoes the submitted address: %s", wrong.Body.String())
	}
}

func TestAuthenticateCustomerReturnsThePrincipal(t *testing.T) {
	id := uuid.New()
	swapCustomerStore(t, nil, func(_ context.Context, email, _ string) (commercestore.CustomerAccount, error) {
		return commercestore.CustomerAccount{ID: id, Email: "Buyer@Example.TEST"}, nil
	})

	s := &Server{}
	rec := postJSON(t, s.authenticateCustomer, `{"email":"buyer@example.test","password":"correct horse battery"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["customer_id"] != id.String() || out["email"] != "Buyer@Example.TEST" {
		t.Fatalf("principal = %v", out)
	}
}

// The error path is where the submitted address gets attached to a log line by
// reflex. A log is a durable, widely-readable record; putting the addresses
// someone is probing into it is the same disclosure as echoing them, with a
// longer half-life. The back-office login page refuses the equivalent for the
// same reason (web/backoffice/src/pages/login.astro).
//
// What this proves and what it does not: the injected error deliberately does
// NOT contain the address, so the assertion isolates one property — the handler
// adds no address or password ATTRIBUTE of its own. It cannot speak for what a
// driver puts inside an error string. That side is argued rather than tested, in
// logCustomerFailure's comment: pgconn renders message + SQLSTATE and leaves the
// offending value in Detail, which it does not print.
func TestCustomerHandlersNeverLogTheSubmittedAddressOrPassword(t *testing.T) {
	var captured bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&captured, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	boom := errors.New("store is down")
	swapCustomerStore(t,
		func(context.Context, string, string) (commercestore.CustomerAccount, error) {
			return commercestore.CustomerAccount{}, boom
		},
		func(context.Context, string, string) (commercestore.CustomerAccount, error) {
			return commercestore.CustomerAccount{}, boom
		})

	s := &Server{}
	postJSON(t, s.registerCustomer, `{"email":"probe@example.test","password":"hunter2hunter2"}`)
	postJSON(t, s.authenticateCustomer, `{"email":"probe@example.test","password":"hunter2hunter2"}`)

	logged := captured.String()
	if logged == "" {
		t.Fatal("nothing was logged at all — an outage must still be reportable to an operator")
	}
	for _, secret := range []string{"probe@example.test", "hunter2hunter2"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("the logs contain %q:\n%s", secret, logged)
		}
	}
}

func TestCustomerHandlersRefuseAMalformedBody(t *testing.T) {
	s := &Server{}
	for _, h := range []http.HandlerFunc{s.registerCustomer, s.authenticateCustomer} {
		rec := postJSON(t, h, `{`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	}
}
