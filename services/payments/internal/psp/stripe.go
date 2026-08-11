package psp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
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

// stripeCallTimeout bounds a provider call when the caller supplies no client. It was
// http.DefaultClient — no timeout at all — on the outermost leg of the money path, so a
// hung Stripe call could outlive commerce's 2-minute recovery grace period and let a live
// checkout and the recovery runner act on the same order (TKT-116). It nests inside the
// 30s obs.Client() bound its callers sit behind, and outside recovery's stricter 10s.
const stripeCallTimeout = 15 * time.Second

// NewStripe builds the adapter. secretKey is the Basic-auth username (password empty).
//
// The scheme is checked here rather than trusted from the caller: the secret key
// travels on EVERY request as Basic auth, so an http:// base URL puts a live
// credential on the wire in cleartext. Production hard-codes https://api.stripe.com
// today — this makes a later config change fail at boot instead of silently
// downgrading. Loopback is exempt because that is where httptest serves from, and
// nowhere else. A panic rather than an error: it is a boot-time misconfiguration
// with no recovery, and the alternative is a running payments service.
func NewStripe(secretKey, baseURL string, client *http.Client) *Stripe {
	if !secureBaseURL(baseURL) {
		panic("psp: Stripe base URL must be https:// (loopback http is allowed for tests); got " + baseURL)
	}
	if client == nil {
		client = &http.Client{Timeout: stripeCallTimeout}
	}
	return &Stripe{secretKey: secretKey, baseURL: strings.TrimRight(baseURL, "/"), client: client}
}

// secureBaseURL reports whether baseURL may carry the secret key. Parsed rather
// than prefix-matched: "https://evil@..." and a bare host both have to resolve to a
// host before the loopback exemption can be judged, and net.ParseIP is what decides
// loopback — not a string test for "localhost" that 127.0.0.1 fails.
func secureBaseURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || net.ParseIP(host).IsLoopback()
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
	// The fields below identify a refund found by listing (TKT-116). Metadata carries the
	// compensation key we stamp at creation — it is what distinguishes a refund THIS system
	// issued from one someone made in the Stripe dashboard.
	Amount        int64             `json:"amount"`
	Currency      string            `json:"currency"`
	PaymentIntent string            `json:"payment_intent"`
	Metadata      map[string]string `json:"metadata"`
}

// stripeRefundList is a Stripe list response. has_more drives cursor pagination.
//
// Data and HasMore are POINTERS on purpose (ai-review F1). Go zero values make a missing
// `has_more` read as false and a missing `data` read as empty — which is precisely the
// shape of "this PaymentIntent has no refunds", the one answer that licenses submitting a
// new one. A page that merely decodes without error is not evidence of absence; presence
// has to be checked, not inferred.
type stripeRefundList struct {
	Object  string          `json:"object"`
	Data    *[]stripeRefund `json:"data"`
	HasMore *bool           `json:"has_more"`
}

// complete reports whether the page carries the fields a conclusive answer needs.
func (l stripeRefundList) complete() bool {
	return l.Object == "list" && l.Data != nil && l.HasMore != nil
}

// refundMatch is the verdict on one listed refund. Three outcomes, not two: see classify.
type refundMatch int

const (
	// refundMatchNo — somebody else's refund. Says nothing about ours.
	refundMatchNo refundMatch = iota
	// refundMatchYes — ours, corroborated.
	refundMatchYes
	// refundMatchInconclusive — carries our stamp but the evidence disagrees or is absent.
	refundMatchInconclusive
)

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
	// The capture key is operation-derived and stable; like the Status replay it is only
	// idempotent within Stripe's ~24h key retention (ADR-032 §Status/replay amendment).
	// Past the window an already-captured PI answers with an error we map to Unknown —
	// still recoverable, because Status's GET retrieve does not depend on the key.
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
	// RESOLVE before submitting. A refund Stripe settled whose response we lost leaves no
	// re_ reference to retrieve, so the recorded-ref dispatch cannot help. Within Stripe's
	// ~24h idempotency retention a same-key POST replays the original; past it the SAME key
	// submits a SECOND refund, which fails as already-fully-refunded — payments 502s
	// forever and commerce parks an order whose buyer already has their money (TKT-116).
	// ADR-032 §Status/replay: the retention window is a hard deadline, not a retry budget.
	if res, done, err := s.resolveRefund(ctx, providerRef, idempotencyKey, amount, currency); done {
		return res, err
	}
	form := url.Values{}
	form.Set("payment_intent", providerRef)
	if amount > 0 {
		form.Set("amount", strconv.FormatInt(amount, 10))
	}
	// The stamp that makes this refund recognizable as ours on a later resolution pass.
	// Without it the convergence above cannot tell our refund from a dashboard one.
	form.Set("metadata["+compensationKeyMetadata+"]", idempotencyKey)
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

// compensationKeyMetadata is the Stripe refund metadata key carrying the compensation's
// idempotency key. Stripe does not echo the Idempotency-Key header on the refund object,
// so this stamp is the only way a later listing can identify a refund as ours.
const compensationKeyMetadata = "compensation_key"

// refundListPages bounds cursor pagination. A PaymentIntent has a handful of refunds at
// most; the bound exists so a provider that never clears has_more cannot spin forever.
const refundListPages = 10

// resolveRefund searches the PaymentIntent's refunds for one THIS system already created
// under idempotencyKey.
//
// done=true means Refund must return (res, err) WITHOUT submitting — either a match was
// found, or the listing did not conclusively prove that no refund exists. The second case
// is the important one: a transport failure, a provider error or a malformed page proves
// nothing, and submitting on it is how you refund twice. Fail closed; the compensation
// stays bound and recoverable, which is what a 502 upstream already means.
//
// done=false — the only path that licenses a POST — requires a complete, successful
// listing with no match.
func (s *Stripe) resolveRefund(ctx context.Context, providerRef, idempotencyKey string, amount int64, currency string) (Result, bool, error) {
	q := url.Values{}
	q.Set("payment_intent", providerRef)
	q.Set("limit", "100")
	for page := 0; page < refundListPages; page++ {
		status, raw, se, err := s.do(ctx, http.MethodGet, "/v1/refunds?"+q.Encode(), "", nil)
		if err != nil {
			res, e := unknown(err)
			return res, true, e
		}
		if se != nil {
			res, e := classifyError(status, se)
			return res, true, e
		}
		var list stripeRefundList
		if err := json.Unmarshal(raw, &list); err != nil {
			res, e := unknown(err)
			return res, true, e
		}
		if !list.complete() {
			res, e := unknown(errors.New("stripe refund list page is missing object/data/has_more"))
			return res, true, e
		}
		data := *list.Data
		for _, rf := range data {
			switch rf.classify(providerRef, idempotencyKey, amount, currency) {
			case refundMatchYes:
				// Including a FAILED refund: the money did not come back, so this is not
				// resolved — but re-submitting would be a fresh money movement chosen by a
				// heuristic. mapRefundStatus keeps it a non-terminal error carrying the
				// re_, which is the evidence a human reconciles from.
				res, e := mapRefundStatus(rf)
				return res, true, e
			case refundMatchInconclusive:
				res, e := unknown(fmt.Errorf("stripe refund %q carries this compensation key with evidence that does not corroborate it", rf.ID))
				return res, true, e
			}
		}
		if !*list.HasMore {
			return Result{}, false, nil // conclusively absent: submitting is safe
		}
		if len(data) == 0 {
			// More pages exist but this one carries no id to advance the cursor past, so
			// no further page can be reached: absence is unprovable.
			res, e := unknown(errors.New("stripe refund list reports more pages but returned none"))
			return res, true, e
		}
		q.Set("starting_after", data[len(data)-1].ID)
	}
	res, e := unknown(errors.New("stripe refund list did not terminate"))
	return res, true, e
}

// classify decides whether a listed refund is the one this compensation created.
//
// Three verdicts, not two (ai-review F1). The metadata stamp is the identity: a refund
// without it is somebody else's action — a dashboard refund — and adopting it would
// fabricate a payments-owned fact for money we did not move, the same rule ADR-032 applies
// to an externally released hold. That is a clean NO.
//
// The third verdict exists for the dangerous middle: a refund carrying OUR key whose
// corroborating evidence is absent or disagrees. Calling that NO would license a second
// refund on top of one that probably is ours; calling it YES would append a money fact on
// evidence that does not confirm it. Neither is defensible, so it fails closed and a human
// looks. An earlier revision folded this case into NO — the reviewer was right that the
// leniency quietly broke the fail-closed rule the rest of this function exists to keep.
func (rf stripeRefund) classify(providerRef, idempotencyKey string, amount int64, currency string) refundMatch {
	if idempotencyKey == "" || rf.Metadata == nil || rf.Metadata[compensationKeyMetadata] != idempotencyKey {
		return refundMatchNo
	}
	if rf.ID == "" || rf.PaymentIntent != providerRef || rf.Currency != lc(currency) {
		return refundMatchInconclusive
	}
	// amount is the compensation's durable basis and is always the stored captured amount,
	// which compensationAllowed guarantees is positive. A non-positive one cannot
	// corroborate anything, so it does not get to license adoption either.
	if amount <= 0 || rf.Amount != amount {
		return refundMatchInconclusive
	}
	return refundMatchYes
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
