package api

// Password recovery handlers (TKT-226). See ADR-050 for the mail path and
// ADR-049 § TKT-226 amendment for what this changes about customer identity.

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	commercestore "ticketing/services/commerce/internal/store"
)

// resetAccepted is the ONE answer a reset request gets, for every address.
//
// A constant rather than two call sites, for the reason customerCredentialsInvalid is
// one constant: two places building "the same" acknowledgement are two places one of
// which eventually says something slightly different, and the difference is what an
// enumerator reads.
const resetAccepted = "accepted"

// resetTokenUnusable is the ONE refusal a redemption gets. Unknown, expired and
// already-redeemed are indistinguishable here because they are indistinguishable in the
// store — telling them apart would hand a prober an oracle for which tokens were real.
const resetTokenUnusable = "the reset link is invalid or has expired"

// The store seams, package-level so the handler tests can exercise the verdict-to-HTTP
// mapping without a database. Typed explicitly: a change to the store's signature fails
// here rather than being silently absorbed.
var (
	requestPasswordResetFn  = commercestore.RequestPasswordReset
	completePasswordResetFn = commercestore.CompletePasswordReset
)

type passwordResetRequestBody struct {
	Email string `json:"email"`
}

type passwordResetCompletionBody struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// requestPasswordReset answers 202 whether or not the address holds an account.
//
// There is no branch on `ok` and there must never be one. `ok` exists so the store can
// report what it did without this handler being able to accidentally disclose it — the
// value is deliberately discarded rather than logged, counted, or returned.
func (s *Server) requestPasswordReset(w http.ResponseWriter, r *http.Request) {
	var in passwordResetRequestBody
	if !decode(w, r, &in) {
		return
	}
	// Before the store call, for the reason registerCustomer states. This is also
	// the operation with an OUTBOUND cost — every accepted request enqueues a
	// message — so the subject budget here is what stops one address being mail
	// bombed (ADR-050 § Consequences names this as the surface TKT-226 added).
	if !s.allowSubject(w, customerEmailSubject(subjectScopeRecovery, in.Email)) {
		return
	}

	if _, err := requestPasswordResetFn(r.Context(), s.db, in.Email, s.composeResetMail); err != nil {
		// An outage is NOT reported as a refusal, and it is not reported as success
		// either. A buyer told "we sent you a link" when nothing was enqueued waits
		// forever for mail that does not exist; a 500 tells them to try again.
		logCustomerFailure(r.Context(), "password reset request failed", err)
		write(w, http.StatusInternalServerError, map[string]string{"error": customerUnavailable})
		return
	}
	write(w, http.StatusAccepted, map[string]string{"status": resetAccepted})
}

// completePasswordReset redeems a token and reports whose sessions to destroy.
func (s *Server) completePasswordReset(w http.ResponseWriter, r *http.Request) {
	var in passwordResetCompletionBody
	if !decode(w, r, &in) {
		return
	}
	// NO per-subject limit here, deliberately (TKT-224). The only candidate key is
	// the submitted token, and keying on it would be worse than nothing twice over:
	// a grinder varies the token, so every guess would land in a FRESH bucket and
	// the budget would never bind — and each guess would insert a map entry, which
	// turns the limiter into the key-exhaustion vector its cap exists to prevent.
	// The per-source limiter on this route is the real bound. Grinding the token
	// itself is not the threat the limiter answers: it is 32 random bytes (ADR-050
	// §4), so the search space, not the rate, is what makes it infeasible.
	customer, err := completePasswordResetFn(r.Context(), s.db, in.Token, in.Password)
	switch {
	case errors.Is(err, commercestore.ErrResetTokenUnusable):
		write(w, http.StatusBadRequest, map[string]string{"error": resetTokenUnusable})
		return
	case errors.Is(err, commercestore.ErrCustomerPasswordUnusable),
		errors.Is(err, commercestore.ErrCustomerInvalidInput):
		// The contract already refuses these shapes, so reaching here means a caller
		// bypassed the validator or the two disagree. The input is the problem, and
		// this is a DIFFERENT message from the token refusal on purpose: a buyer whose
		// new password was rejected must not be told their link expired and sent back
		// to a mailbox for a link that still works.
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	case err != nil:
		logCustomerFailure(r.Context(), "password reset completion failed", err)
		write(w, http.StatusInternalServerError, map[string]string{"error": customerUnavailable})
		return
	}
	write(w, http.StatusOK, map[string]string{"customer_id": customer.String()})
}

// composeResetMail builds the message. It is passed to the store so that the copy lives
// here, beside the other customer-facing strings, and the store never learns what a
// password reset email says.
//
// THE LINK'S BASE IS SERVER-CONFIGURED, NEVER TAKEN FROM THE REQUEST. Building it from
// the Host header (or from any caller-supplied field) turns this endpoint into a
// phishing generator: an attacker submits a victim's address with their own host, and
// the victim receives a genuine reset link pointing at the attacker's site. That is the
// classic host-header injection and it is why nothing here reads r.
func (s *Server) composeResetMail(tok commercestore.IssuedResetToken) (recipient, subject, body string) {
	link := s.publicURL + "/en/account/reset-password?token=" + url.QueryEscape(tok.Raw)
	subject = "Reset your password"
	body = fmt.Sprintf(`Someone asked to reset the password for this address.

Open this link to choose a new one:

%s

The link works once and expires in %s. If you did not ask for this, nothing has
changed and you can ignore this message.
`, link, commercestore.ResetTokenTTL)
	return tok.Email, subject, body
}

// WarnIfResetMailUnconfigured is logged once at startup rather than per request. A
// commerce with no PUBLIC_BASE_URL still serves every other operation, so refusing to
// start would be a large blast radius for a feature an operator may not have enabled —
// but a reset link with no origin is a broken link, so it must not be silent either.
func WarnIfResetMailUnconfigured(log *slog.Logger, publicURL string) {
	if publicURL == "" {
		log.Warn("PUBLIC_BASE_URL is unset; password-reset links cannot be built and reset mail will be undeliverable")
	}
}
