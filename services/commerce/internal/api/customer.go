package api

// Customer account operations (TKT-220 / US-A1). See ADR-049 for why buyer
// identity lives in commerce and why these two operations are public.

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"

	commercestore "ticketing/services/commerce/internal/store"
)

// customerCredentialsInvalid is the ONE refusal body for a sign-in. Built once,
// as a constant, so the unknown-address and wrong-password paths cannot drift
// apart later: two call sites constructing "the same" message are two call sites
// one of which eventually says something slightly different, and the difference
// is exactly what an account enumerator is looking for.
const customerCredentialsInvalid = "invalid credentials"

// customerEmailTaken is the deliberate disclosure. ADR-049 records it as an
// unauthenticated membership oracle and names TKT-224 (rate limiting) as the
// control; it is not softened here, because a vaguer message would still be a
// distinguishable answer while being useless to the buyer who typed it.
const customerEmailTaken = "an account already exists for that address"

// customerUnavailable is what an outage says. Deliberately NOT a credential
// verdict: telling a buyer their correct password is wrong sends them to reset
// it — which this system cannot even do — while the real fault goes unreported.
const customerUnavailable = "customer accounts are temporarily unavailable"

// customerStoreFn is the shape both customer store operations share.
type customerStoreFn func(ctx context.Context, db *sql.DB, email, password string) (commercestore.CustomerAccount, error)

// The store seams. Package-level values rather than direct calls so the handler
// tests can exercise the verdict-to-HTTP mapping without a database; the
// credential logic itself is proven in the store package. Typed explicitly, so a
// change to the store's signature fails here rather than being absorbed.
var (
	registerCustomerFn     customerStoreFn = commercestore.RegisterCustomer
	authenticateCustomerFn customerStoreFn = commercestore.AuthenticateCustomer
)

// customerAccountRequest is both operations' body. They are separate schemas in
// the contract (registration has a minimum password length that sign-in must not
// apply) but identical on the wire, and the validator has already enforced the
// difference by the time a handler runs.
type customerAccountRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) registerCustomer(w http.ResponseWriter, r *http.Request) {
	var in customerAccountRequest
	if !decode(w, r, &in) {
		return
	}
	// BEFORE the store call, so the bucket fills identically whether or not the
	// address exists. Refusing after the lookup would make a 429 mean "this address
	// is real" — a sharper oracle than the 409 this limiter exists to blunt.
	if !s.allowSubject(w, customerEmailSubject(subjectScopeCredential, in.Email)) {
		return
	}

	account, err := registerCustomerFn(r.Context(), s.db, in.Email, in.Password)
	switch {
	case errors.Is(err, commercestore.ErrCustomerEmailTaken):
		write(w, http.StatusConflict, map[string]string{"error": customerEmailTaken})
		return
	case errors.Is(err, commercestore.ErrCustomerPasswordUnusable),
		errors.Is(err, commercestore.ErrCustomerInvalidInput):
		// The contract already refuses these shapes, so reaching here means a
		// caller bypassed the validator or the two disagree. Answer 400, not
		// 500: the input is the problem.
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	case err != nil:
		logCustomerFailure(r.Context(), "customer registration failed", err)
		write(w, http.StatusInternalServerError, map[string]string{"error": customerUnavailable})
		return
	}
	write(w, http.StatusCreated, s.principalResponse(account))
}

func (s *Server) authenticateCustomer(w http.ResponseWriter, r *http.Request) {
	var in customerAccountRequest
	if !decode(w, r, &in) {
		return
	}
	// Before the store call, for the reason registerCustomer states — and here it
	// also keeps the bcrypt comparison behind the limit, which is what makes
	// unbounded attempts a CPU-exhaustion vector and not only a guessing one
	// (ADR-049 §3 makes that cost deliberate on BOTH paths).
	if !s.allowSubject(w, customerEmailSubject(subjectScopeCredential, in.Email)) {
		return
	}

	account, err := authenticateCustomerFn(r.Context(), s.db, in.Email, in.Password)
	switch {
	case errors.Is(err, commercestore.ErrCustomerCredentialsInvalid):
		write(w, http.StatusUnauthorized, map[string]string{"error": customerCredentialsInvalid})
		return
	case err != nil:
		// A lookup failure is not a credential verdict. Answering 401 here would
		// tell someone their correct password is wrong while an outage went
		// unreported.
		logCustomerFailure(r.Context(), "customer authentication failed", err)
		write(w, http.StatusInternalServerError, map[string]string{"error": customerUnavailable})
		return
	}
	write(w, http.StatusOK, s.principalResponse(account))
}

// principalResponse is the only shape either operation returns. `email` is the
// buyer's original spelling; the normalized lookup key is never exposed, and no
// password material exists above the store to leak.
//
// `customer_assertion` is a BEARER CREDENTIAL in a public response body, and that
// is worth saying out loud (ADR-049 § TKT-221): anyone holding it can attribute a
// checkout to this customer until it expires. It is returned only to a caller who
// has just proven the password, and the storefront keeps it server-side inside its
// in-process session — never in the cookie, never in a rendered prop, never in a
// log. It is empty when no signing key is configured, which is a state startup
// refuses.
func (s *Server) principalResponse(a commercestore.CustomerAccount) map[string]any {
	return map[string]any{
		"customer_id":        a.ID,
		"email":              a.Email,
		"customer_assertion": s.mintForPrincipal(a.ID),
	}
}

// logCustomerFailure reports an outage to the operator and carries NOTHING the
// caller submitted.
//
// The submitted address is the reflex thing to attach, and it must not be: a log
// is a durable, widely-readable record, so putting the addresses someone is
// probing into it is the same disclosure as echoing them back, with a longer
// half-life (web/backoffice/src/pages/login.astro refuses the equivalent). There
// is no customer id to log either — on both paths the failure happened while
// trying to resolve one.
//
// The wrapped error IS logged, and that is safe with this driver rather than by
// luck: `pgconn.PgError.Error()` renders severity, message and SQLSTATE only.
// The offending VALUE lives in the error's Detail field, which that method does
// not include — so a CHECK or unique violation on customer_accounts cannot carry
// an address into the log through here. If the driver or the wrapping ever
// changes, this comment is the thing that has stopped being true.
func logCustomerFailure(ctx context.Context, msg string, err error) {
	slog.Default().ErrorContext(ctx, msg, "err", err)
}
