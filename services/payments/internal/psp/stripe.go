package psp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Stripe is the test-mode Stripe adapter behind the psp.PSP port (ADR-032). It talks to the
// Stripe REST API over an INJECTED *http.Client and base URL, so tests replay recorded JSON
// against an httptest.Server and the gate never touches api.stripe.com. It holds no durable
// state: the payments store owns the operation/compensation rows.
type Stripe struct {
	secretKey string
	baseURL   string // e.g. https://api.stripe.com (no trailing slash) or the httptest URL
	client    *http.Client
}

// NewStripe builds the adapter. secretKey is the Basic-auth username (password empty).
func NewStripe(secretKey, baseURL string, client *http.Client) *Stripe {
	if client == nil {
		client = http.DefaultClient
	}
	return &Stripe{secretKey: secretKey, baseURL: strings.TrimRight(baseURL, "/"), client: client}
}

// stripePI is the subset of a PaymentIntent we read. Unknown fields are ignored by
// encoding/json, which is what lets a hand-written fixture carry extra keys.
type stripePI struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	Currency     string `json:"currency"`
	LatestCharge string `json:"latest_charge"`
}

type stripeRefund struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type stripeError struct {
	Err struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// unknown is a transport/ambiguity failure: no proof of side effect. NEVER terminal —
// recovery must not release a claim on it (ADR-016 §Dec3).
func unknown(err error) (Result, error) { return Result{Outcome: Unknown}, err }

// do performs one Stripe HTTP call. A transport failure (no response) returns a non-nil
// error AND leaves the caller to map it to Unknown. A non-2xx returns the decoded error
// envelope. A 2xx returns the raw body for the caller to parse.
func (s *Stripe) do(ctx context.Context, method, path, idempotencyKey string, form url.Values) (int, []byte, *stripeError, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, body)
	if err != nil {
		return 0, nil, nil, err
	}
	req.SetBasicAuth(s.secretKey, "")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, nil, err // transport failure -> Unknown at the call site
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var se stripeError
		_ = json.Unmarshal(raw, &se) // best effort; may be empty
		return resp.StatusCode, raw, &se, nil
	}
	return resp.StatusCode, raw, nil, nil
}

// classifyError maps a non-2xx Stripe response to a Result. A card_error / decline is the
// only TERMINAL mapping (Declined, no side effect). Everything else — rate limits, 5xx,
// auth/config errors, malformed envelopes — is Unknown or a hard error, never terminal
// (plan-final A1/A2, ADR-016 §Dec3).
func classifyError(status int, se *stripeError) (Result, error) {
	if se != nil && se.Err.Type == "card_error" {
		return Result{Outcome: Declined, TerminalNoSideEffect: true}, nil
	}
	// 429 / 5xx: the request may have reached Stripe — ambiguous, not terminal.
	// 401/403: configuration error — hard, non-terminal. 4xx invalid_request: hard, but the
	// durable operation stays recoverable rather than being invented as terminal.
	msg := "stripe error"
	if se != nil && se.Err.Message != "" {
		msg = se.Err.Message
	}
	return Result{Outcome: Unknown}, fmt.Errorf("stripe status %d: %s", status, msg)
}

// mapPIStatus maps a PaymentIntent status to a normalized Result. Known-but-not-terminal
// statuses (processing, requires_action, requires_confirmation, requires_payment_method)
// map to Unknown, never Declined (plan-final A2).
func mapPIStatus(pi stripePI) (Result, error) {
	switch pi.Status {
	case "requires_capture":
		return Result{Outcome: Authorized, Authorized: true, ProviderRef: pi.ID}, nil
	case "succeeded":
		return Result{Outcome: Captured, Captured: true, Authorized: true, ProviderRef: pi.ID, ProviderChargeRef: pi.LatestCharge}, nil
	case "canceled":
		return Result{Outcome: Voided, TerminalNoSideEffect: true, ProviderRef: pi.ID}, nil
	case "processing", "requires_action", "requires_confirmation", "requires_payment_method":
		return Result{Outcome: Unknown, ProviderRef: pi.ID}, fmt.Errorf("stripe payment intent not settled: %s", pi.Status)
	default:
		// Unknown/future status: fail closed as Unknown, never Declined.
		return Result{Outcome: Unknown, ProviderRef: pi.ID}, fmt.Errorf("stripe payment intent unknown status %q", pi.Status)
	}
}

func lc(currency string) string { return strings.ToLower(currency) }

// createIntent creates+confirms a manual-capture PaymentIntent under idempotencyKey.
func (s *Stripe) createIntent(ctx context.Context, req AuthorizeRequest) (stripePI, *stripeError, int, error) {
	form := url.Values{}
	form.Set("amount", strconv.FormatInt(req.Amount, 10))
	form.Set("currency", lc(req.Currency))
	form.Set("capture_method", "manual")
	form.Set("payment_method", req.PaymentToken)
	form.Set("confirm", "true")
	status, raw, se, err := s.do(ctx, http.MethodPost, "/v1/payment_intents", req.IdempotencyKey, form)
	if err != nil {
		return stripePI{}, nil, status, err
	}
	if se != nil {
		return stripePI{}, se, status, nil
	}
	var pi stripePI
	if err := json.Unmarshal(raw, &pi); err != nil {
		return stripePI{}, nil, status, err
	}
	return pi, nil, status, nil
}

// Authorize creates a manual-capture PaymentIntent and, on the immediate-capture charge
// path, captures it internally — returning Captured, exactly like the fake, so the charge
// handler is unchanged and never leaves a hold stranded (plan-final F2). A decline on create
// is Declined (terminal); a transport failure is Unknown.
func (s *Stripe) Authorize(ctx context.Context, req AuthorizeRequest) (Result, error) {
	pi, se, status, err := s.createIntent(ctx, req)
	if err != nil {
		return unknown(err)
	}
	if se != nil {
		return classifyError(status, se)
	}
	switch pi.Status {
	case "requires_capture":
		// Authorized — now capture it (immediate-capture charge flow). Reuse a derived
		// idempotency key so a retry of the whole Authorize is safe.
		return s.Capture(ctx, pi.ID, req.Amount, req.Currency)
	case "succeeded":
		// Already captured (some flows capture on confirm) — done.
		return Result{Outcome: Captured, Captured: true, Authorized: true, ProviderRef: pi.ID, ProviderChargeRef: pi.LatestCharge}, nil
	default:
		// requires_action / processing / etc. on the charge path: not settled, not terminal.
		return mapPIStatus(pi)
	}
}

// Capture captures a prior authorization -> Captured. Any capture-stage failure — even a
// card_error — is Unknown with the ProviderRef PRESERVED, never terminal Declined: the
// PaymentIntent already exists holding the customer's funds, so "no side effect" would be
// a lie and dropping the pi_ identity would strand the hold with no way to void it
// (ai-review A1). The unresolved operation stays recoverable via Status.
func (s *Stripe) Capture(ctx context.Context, providerRef string, amount int64, currency string) (Result, error) {
	form := url.Values{}
	if amount > 0 {
		form.Set("amount_to_capture", strconv.FormatInt(amount, 10))
	}
	status, raw, se, err := s.do(ctx, http.MethodPost, "/v1/payment_intents/"+providerRef+"/capture", "capture:"+providerRef, form)
	if err != nil {
		return Result{Outcome: Unknown, ProviderRef: providerRef}, err
	}
	if se != nil {
		msg := "stripe capture failed"
		if se.Err.Message != "" {
			msg = se.Err.Message
		}
		return Result{Outcome: Unknown, ProviderRef: providerRef}, fmt.Errorf("stripe capture status %d: %s", status, msg)
	}
	var pi stripePI
	if err := json.Unmarshal(raw, &pi); err != nil {
		return Result{Outcome: Unknown, ProviderRef: providerRef}, err
	}
	if pi.Status == "succeeded" {
		return Result{Outcome: Captured, Captured: true, Authorized: true, ProviderRef: pi.ID, ProviderChargeRef: pi.LatestCharge}, nil
	}
	return mapPIStatus(pi)
}

// Void cancels an uncaptured authorization -> Voided (terminal-no-side-effect).
func (s *Stripe) Void(ctx context.Context, providerRef, idempotencyKey string) (Result, error) {
	status, raw, se, err := s.do(ctx, http.MethodPost, "/v1/payment_intents/"+providerRef+"/cancel", idempotencyKey, url.Values{})
	if err != nil {
		return unknown(err)
	}
	if se != nil {
		return classifyError(status, se)
	}
	var pi stripePI
	if err := json.Unmarshal(raw, &pi); err != nil {
		return unknown(err)
	}
	if pi.Status == "canceled" {
		return Result{Outcome: Voided, TerminalNoSideEffect: true, ProviderRef: pi.ID}, nil
	}
	return Result{Outcome: Unknown, ProviderRef: pi.ID}, fmt.Errorf("stripe void did not cancel: status %q", pi.Status)
}

// Refund refunds a captured charge -> Refunded (only on Stripe status "succeeded"; a
// "pending" or "failed" refund is a non-terminal error so the caller keeps the compensation
// bound and does not append payment.refunded to the append-only journal — plan-final A7).
func (s *Stripe) Refund(ctx context.Context, providerRef, idempotencyKey string, amount int64, currency string) (Result, error) {
	form := url.Values{}
	form.Set("payment_intent", providerRef)
	if amount > 0 {
		form.Set("amount", strconv.FormatInt(amount, 10))
	}
	status, raw, se, err := s.do(ctx, http.MethodPost, "/v1/refunds", idempotencyKey, form)
	if err != nil {
		return unknown(err)
	}
	if se != nil {
		return classifyError(status, se)
	}
	var rf stripeRefund
	if err := json.Unmarshal(raw, &rf); err != nil {
		return unknown(err)
	}
	return mapRefundStatus(rf)
}

// mapRefundStatus maps a refund object to a Result. pending is ErrRefundPending so the
// caller persists the re_ ref and later RESOLVES it via Status — never re-submits.
func mapRefundStatus(rf stripeRefund) (Result, error) {
	switch rf.Status {
	case "succeeded":
		return Result{Outcome: Refunded, ProviderRef: rf.ID}, nil
	case "pending":
		return Result{Outcome: Unknown, ProviderRef: rf.ID}, ErrRefundPending
	default: // "failed" or unexpected
		return Result{Outcome: Unknown, ProviderRef: rf.ID}, fmt.Errorf("stripe refund not settled: status %q", rf.Status)
	}
}

// ErrRefundPending reports a refund the provider accepted but has not settled. The caller
// keeps the compensation bound, persists the refund reference, and resolves it later by
// retrieving that reference (Status with a re_ ProviderRef) — re-submitting the refund
// under the same idempotency key would only replay the provider's original "pending"
// snapshot, and after key expiry could mint a second refund (ai-review B3).
var ErrRefundPending = errors.New("psp: refund pending")

// Status resolves an operation via ProviderRef (GET retrieve) or, when no ProviderRef was
// persisted, by REPLAYING the original create under the SAME idempotency key (never a fresh
// key — Stripe idempotency returns the original PaymentIntent, so no duplicate charge).
func (s *Stripe) Status(ctx context.Context, req StatusRequest) (Result, error) {
	// A refund reference resolves against the refund object (ai-review B3): retrieving is
	// the only way to observe a pending refund settle.
	if strings.HasPrefix(req.ProviderRef, "re_") {
		status, raw, se, err := s.do(ctx, http.MethodGet, "/v1/refunds/"+req.ProviderRef, "", nil)
		if err != nil {
			return unknown(err)
		}
		if se != nil {
			return classifyError(status, se)
		}
		var rf stripeRefund
		if err := json.Unmarshal(raw, &rf); err != nil {
			return unknown(err)
		}
		return mapRefundStatus(rf)
	}
	if req.ProviderRef != "" {
		status, raw, se, err := s.do(ctx, http.MethodGet, "/v1/payment_intents/"+req.ProviderRef, "", nil)
		if err != nil {
			return unknown(err)
		}
		if se != nil {
			return classifyError(status, se)
		}
		var pi stripePI
		if err := json.Unmarshal(raw, &pi); err != nil {
			return unknown(err)
		}
		return mapPIStatus(pi)
	}
	// No provider ref: replay the create under the ORIGINAL key. This only learns the
	// outcome; it does not capture. BOUNDED: Stripe retains idempotency keys ~24h — after
	// that this same-key replay would CREATE a new PaymentIntent instead of returning the
	// original (ADR-032 §Status/replay amendment). Resolution must happen within the
	// window; an older ref-less operation is a manual-reconciliation case.
	pi, se, status, err := s.createIntent(ctx, AuthorizeRequest{
		Amount: req.Amount, Currency: req.Currency, PaymentToken: req.PaymentToken, IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		return unknown(err)
	}
	if se != nil {
		return classifyError(status, se)
	}
	return mapPIStatus(pi)
}
