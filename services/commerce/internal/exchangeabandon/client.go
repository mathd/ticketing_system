package exchangeabandon

// The HTTP side of the abandonment command: Access's admission read, payments' evidence reads,
// its partial refund and its journal append, and inventory's capacity return.
//
// The client decodes; the service judges. Every answer is decoded exactly: one JSON object of
// the declared shape, member names matched exactly, no unknown or duplicate member, no trailing
// content, and no null member. The client also refuses an answer that its producer cannot write
// (the endpoint contract in services/payments/api/openapi.yaml and its handlers). An answer that
// fails either rule is ErrUnavailable, never a verdict. A 404 is absence only when it is the
// endpoint's own "not found" object, and a redirect is never followed. The HTTP mechanics follow
// exchangeunwind and refundoutcome, whose rules came from review there.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/accessadmission"
	"ticketing/services/commerce/internal/store"
	"ticketing/shared/httpx"
)

// maxAnswerBytes bounds each response. Every answer here is a handful of short fields.
const maxAnswerBytes = 64 << 10

// HTTP reaches the three services. Payments takes its own credential; Access and inventory take
// the internal token. Each call site names its credential: a configured URL never chooses it.
//
// Client's timeout bounds each read and the two short writes. RefundTimeout bounds the delta
// refund POST alone, which can wait on several provider calls inside payments (R3-3).
type HTTP struct {
	Client                               *http.Client
	RefundTimeout                        time.Duration
	AccessURL, PaymentsURL, InventoryURL string
	InternalToken, PaymentsToken         string
}

// NewHTTP builds the client: timeout for each call, and refundTimeout for the delta refund.
func NewHTTP(accessURL, paymentsURL, inventoryURL, internalToken, paymentsToken string, timeout, refundTimeout time.Duration) HTTP {
	return HTTP{
		Client:        &http.Client{Timeout: timeout},
		RefundTimeout: refundTimeout,
		AccessURL:     strings.TrimSuffix(accessURL, "/"),
		PaymentsURL:   strings.TrimSuffix(paymentsURL, "/"),
		InventoryURL:  strings.TrimSuffix(inventoryURL, "/"),
		InternalToken: internalToken,
		PaymentsToken: paymentsToken,
	}
}

// WithTrace returns a copy of h whose every request carries one W3C traceparent with traceID
// and spanID (32 and 16 lowercase hex digits). The services extract it, so each request-log line
// of the command carries the command's trace_id: an operator, and the running-stack test, can
// tell this command's calls from every other caller's (RA2-5). The Access admission caller
// copies the client, so it sends the same header.
func (h HTTP) WithTrace(traceID, spanID string) HTTP {
	client := *h.Client
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	client.Transport = traceTransport{traceparent: "00-" + traceID + "-" + spanID + "-01", base: base}
	h.Client = &client
	return h
}

type traceTransport struct {
	traceparent string
	base        http.RoundTripper
}

func (t traceTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("traceparent", t.traceparent)
	return t.base.RoundTrip(r)
}

// send performs one request with the credential its caller names.
//
// It never follows a redirect: the 3xx answer is returned as it is, so every method reads it as
// an unresolved answer, and the redirect target receives no request and no credential. The rule
// is applied to a copy of the client on each call, so an HTTP built without NewHTTP keeps it.
func (h HTTP) send(ctx context.Context, method, u, token string, in any) (int, []byte, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return 0, nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Internal-Token", token)
	client := *h.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := httpx.ReadResponseBody(resp.Body, maxAnswerBytes)
	return resp.StatusCode, raw, err
}

// payments calls payments with the payments credential.
func (h HTTP) payments(ctx context.Context, method, u string, in any) (int, []byte, error) {
	return h.send(ctx, method, u, h.PaymentsToken, in)
}

// internal calls Access or inventory with the internal token.
func (h HTTP) internal(ctx context.Context, method, u string, in any) (int, []byte, error) {
	return h.send(ctx, method, u, h.InternalToken, in)
}

// absent reports payments' own 404 for a missing record: exactly the JSON object
// {"error": message} that the endpoint writes. A 404 from a missing route, a proxy, or an
// answer of any other shape is not evidence that the record is absent.
func absent(raw []byte, message string) bool {
	var body struct {
		Error *string `json:"error"`
	}
	return decodeExact(raw, &body) == nil && body.Error != nil && *body.Error == message
}

// decodeExact accepts exactly one object of out's shape: member names matched exactly, no
// unknown or duplicate member (escaped spellings included) and no trailing content. It decodes
// every evidence object here (R2-3, RA2-4, R3-1). The v1 decoder matched names without regard to
// case, so `resolved` and `Resolved` were two values for one field and the last one won.
//
// A required field is a pointer, so an absent or null member is nil and the caller refuses it.
// A member the producer may omit is an optional, which refuses an explicit null.
func decodeExact(raw []byte, out any) error {
	if err := jsonv2.Unmarshal(raw, out, jsonv2.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("%w: decode: %v", ErrUnavailable, err)
	}
	return nil
}

// optional is a member that the producer omits when it has no value. Set reports that the answer
// carries it. Payments omits these keys and never writes null, so a null is refused: it must not
// read as the legacy absence (RA3-1).
type optional[T any] struct {
	Set   bool
	Value T
}

func (o *optional[T]) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	if dec.PeekKind() == 'n' {
		return errors.New("an optional member is null; the producer omits it instead")
	}
	o.Set = true
	return jsonv2.UnmarshalDecode(dec, &o.Value)
}

// pointer returns the value of a present member, or nil for an omitted one.
func (o optional[T]) pointer() *T {
	if !o.Set {
		return nil
	}
	v := o.Value
	return &v
}

// errorText returns a dependency's short error message, or nothing. An undecodable or long
// body is not repeated to the operator.
func errorText(raw []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) != nil || e.Error == "" || len(e.Error) > 200 {
		return "(no message)"
	}
	return e.Error
}

// Admission reads Access's admission summary of one order through the shared strict reader and
// the same credentialed, no-redirect caller that the Commerce server's two admission reads use.
func (h HTTP) Admission(ctx context.Context, org, order uuid.UUID) (accessadmission.Admission, error) {
	return accessadmission.Read(ctx, accessadmission.NewCaller(h.Client, h.InternalToken), h.AccessURL, order, org)
}

// ChargeOperation reads `GET /internal/operations` for one charge key. Payments' own
// `operation not found` 404 is the only absence.
func (h HTTP) ChargeOperation(ctx context.Context, org uuid.UUID, key string) (ChargeEvidence, error) {
	q := url.Values{}
	q.Set("organizer_id", org.String())
	q.Set("idempotency_key", key)
	code, raw, err := h.payments(ctx, http.MethodGet, h.PaymentsURL+"/internal/operations?"+q.Encode(), nil)
	if err != nil {
		return ChargeEvidence{}, fmt.Errorf("%w: payments operation read: %v", ErrUnavailable, err)
	}
	switch {
	case code == http.StatusNotFound && absent(raw, "operation not found"):
		return ChargeEvidence{}, nil
	case code == http.StatusOK:
	default:
		return ChargeEvidence{}, fmt.Errorf("%w: payments operation read answered %d", ErrUnavailable, code)
	}
	// Payments writes resolved and occurred_at for every found operation; status and fact_id for a
	// resolved one; the amounts for a captured one; the confirmation pair when the provider
	// confirmed; and the status-replay deadline only for an unresolved one (server.go operation,
	// psp.go statusReplayDeadline).
	var body struct {
		Resolved                *bool               `json:"resolved"`
		OccurredAt              *time.Time          `json:"occurred_at"`
		Status                  optional[string]    `json:"status"`
		FactID                  optional[uuid.UUID] `json:"fact_id"`
		CapturedAmount          optional[int64]     `json:"captured_amount"`
		Currency                optional[string]    `json:"currency"`
		ConfirmedCapturedAmount optional[int64]     `json:"confirmed_captured_amount"`
		ConfirmedCurrency       optional[string]    `json:"confirmed_currency"`
		StatusReplayDeadlineAt  optional[time.Time] `json:"status_replay_deadline_at"`
	}
	if err := decodeExact(raw, &body); err != nil {
		return ChargeEvidence{}, err
	}
	switch {
	case body.Resolved == nil:
		return ChargeEvidence{}, fmt.Errorf("%w: payments operation read has no resolved field", ErrUnavailable)
	case body.OccurredAt == nil || body.OccurredAt.IsZero():
		return ChargeEvidence{}, fmt.Errorf("%w: payments operation read has no bind time", ErrUnavailable)
	case *body.Resolved && (!body.FactID.Set || body.FactID.Value == uuid.Nil):
		return ChargeEvidence{}, fmt.Errorf("%w: payments resolved operation has no fact id", ErrUnavailable)
	case *body.Resolved && body.StatusReplayDeadlineAt.Set:
		return ChargeEvidence{}, fmt.Errorf("%w: payments resolved operation carries a status-replay deadline", ErrUnavailable)
	}
	return ChargeEvidence{
		Found: true, Resolved: *body.Resolved, Status: body.Status.Value,
		CapturedAmount: body.CapturedAmount.pointer(), Currency: body.Currency.pointer(),
		ConfirmedAmount: body.ConfirmedCapturedAmount.pointer(), ConfirmedCurrency: body.ConfirmedCurrency.pointer(),
	}, nil
}

// RefundLeg reads `GET /internal/refund-legs` for one (source key, refund key) leg. Payments'
// own `refund leg not found` 404 is the only absence.
func (h HTTP) RefundLeg(ctx context.Context, org uuid.UUID, sourceKey, refundKey string) (LegEvidence, error) {
	q := url.Values{}
	q.Set("organizer_id", org.String())
	q.Set("source_idempotency_key", sourceKey)
	q.Set("refund_idempotency_key", refundKey)
	code, raw, err := h.payments(ctx, http.MethodGet, h.PaymentsURL+"/internal/refund-legs?"+q.Encode(), nil)
	if err != nil {
		return LegEvidence{}, fmt.Errorf("%w: payments refund-leg read: %v", ErrUnavailable, err)
	}
	switch {
	case code == http.StatusNotFound && absent(raw, "refund leg not found"):
		return LegEvidence{}, nil
	case code == http.StatusOK:
	default:
		return LegEvidence{}, fmt.Errorf("%w: payments refund-leg read answered %d", ErrUnavailable, code)
	}
	// Payments sets the confirmation pair when the leg completes, and omits it on a bound leg and
	// on a leg completed before its migration 0006 (refund_leg.go).
	var body struct {
		Completed         *bool            `json:"completed"`
		Amount            *int64           `json:"amount"`
		Currency          *string          `json:"currency"`
		ConfirmedAmount   optional[int64]  `json:"confirmed_amount"`
		ConfirmedCurrency optional[string] `json:"confirmed_currency"`
	}
	if err := decodeExact(raw, &body); err != nil {
		return LegEvidence{}, err
	}
	if body.Completed == nil || body.Amount == nil || body.Currency == nil {
		return LegEvidence{}, fmt.Errorf("%w: payments refund-leg read is missing a declared field", ErrUnavailable)
	}
	if !*body.Completed && (body.ConfirmedAmount.Set || body.ConfirmedCurrency.Set) {
		return LegEvidence{}, fmt.Errorf("%w: payments refund leg carries a confirmation but is not completed", ErrUnavailable)
	}
	return LegEvidence{
		Found: true, Completed: *body.Completed, Amount: *body.Amount, Currency: *body.Currency,
		ConfirmedAmount: body.ConfirmedAmount.pointer(), ConfirmedCurrency: body.ConfirmedCurrency.pointer(),
	}, nil
}

// PartialRefund posts the delta refund. 200 with a well-formed `refunded` answer is the only
// success. A typed 422 `provider_refund_failed` with a provider `re_…` reference is the
// provider's terminal failure. 400, 404 and 409 are payments' conclusive refusals of this
// request. Everything else, and every answer that does not decode, is ErrUnavailable: payments
// may have moved the money, and the same key resolves it on the next run.
func (h HTTP) PartialRefund(ctx context.Context, r RefundRequest) (RefundAnswer, error) {
	client := *h.Client
	client.Timeout = h.RefundTimeout
	refund := h
	refund.Client = &client
	code, raw, err := refund.payments(ctx, http.MethodPost, h.PaymentsURL+"/internal/psp/partial-refund", map[string]any{
		"organizer_id": r.OrganizerID, "idempotency_key": r.ChargeKey, "refund_key": r.RefundKey,
		"amount": r.Amount, "currency": r.Currency,
	})
	if err != nil {
		return RefundAnswer{}, fmt.Errorf("%w: payments partial refund: %v", ErrUnavailable, err)
	}
	switch code {
	case http.StatusOK:
	case http.StatusUnprocessableEntity:
		var failed struct {
			Error       string `json:"error"`
			Code        string `json:"code"`
			ProviderRef string `json:"provider_ref"`
		}
		// The producer's typed failure: exactly these three members and values (openapi.yaml
		// ProviderRefundFailed, psp.go failedRefund422). Only this answer writes the stop marker.
		if decodeExact(raw, &failed) != nil || failed.Error != "provider refund failed" || failed.Code != "provider_refund_failed" ||
			!strings.HasPrefix(failed.ProviderRef, "re_") || len(failed.ProviderRef) <= len("re_") || len(failed.ProviderRef) > 255 {
			return RefundAnswer{}, fmt.Errorf("%w: payments answered 422 without a typed provider failure", ErrUnavailable)
		}
		return RefundAnswer{}, &ProviderRefundFailedError{ProviderRef: failed.ProviderRef}
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict:
		return RefundAnswer{}, fmt.Errorf("%w: payments answered %d to the partial refund: %s", ErrRefused, code, errorText(raw))
	default:
		return RefundAnswer{}, fmt.Errorf("%w: payments answered %d to the partial refund", ErrUnavailable, code)
	}
	var body struct {
		Status   *string    `json:"status"`
		FactID   *uuid.UUID `json:"fact_id"`
		Amount   *int64     `json:"amount"`
		Currency *string    `json:"currency"`
		Replay   *bool      `json:"replay"`
	}
	if err := decodeExact(raw, &body); err != nil {
		return RefundAnswer{}, err
	}
	if body.Status == nil || *body.Status != "refunded" || body.FactID == nil || *body.FactID == uuid.Nil ||
		body.Amount == nil || body.Currency == nil || body.Replay == nil {
		return RefundAnswer{}, fmt.Errorf("%w: payments' partial refund answer is not a complete `refunded` answer", ErrUnavailable)
	}
	return RefundAnswer{FactID: *body.FactID, Amount: *body.Amount, Currency: *body.Currency}, nil
}

// AppendFact posts the compensating fact to the payments journal. 200 with an acknowledgment is
// the only success. A 4xx is payments refusing the fact, which leaves the abandonment pending
// (D13: an older payments does not know the type, and that must fail closed). Everything else
// is ErrUnavailable: payments may have journalled the fact, and the same fact id replays.
func (h HTTP) AppendFact(ctx context.Context, f store.ExchangeAbandonedFact) error {
	code, raw, err := h.payments(ctx, http.MethodPost, h.PaymentsURL+"/internal/facts", map[string]any{
		"fact_id": f.ID, "organizer_id": f.OrganizerID, "fact_type": store.ExchangeAbandonedFactType,
		"buyer_id": f.BuyerID, "amount": f.Amount, "currency": f.Currency,
		"occurred_at": f.OccurredAt.UTC().Format(time.RFC3339Nano),
		"payload":     f.Payload(),
	})
	if err != nil {
		return fmt.Errorf("%w: payments journal: %v", ErrUnavailable, err)
	}
	switch {
	case code == http.StatusOK:
	case code >= 400 && code < 500:
		return fmt.Errorf("%w: payments journal answered %d: %s", ErrRefused, code, errorText(raw))
	default:
		return fmt.Errorf("%w: payments journal answered %d", ErrUnavailable, code)
	}
	var ack struct {
		Sequence *int64  `json:"sequence"`
		Hash     *string `json:"hash"`
		Replay   *bool   `json:"replay"`
	}
	if err := decodeExact(raw, &ack); err != nil {
		return err
	}
	if ack.Sequence == nil || *ack.Sequence <= 0 || ack.Hash == nil || !isEntryHash(*ack.Hash) || ack.Replay == nil {
		return fmt.Errorf("%w: payments journal acknowledgment is incomplete", ErrUnavailable)
	}
	return nil
}

// isEntryHash reports a journal entry hash as payments writes it: the lowercase hex of a SHA-256
// digest (store.Hex of the entry hash), 64 digits.
func isEntryHash(s string) bool {
	if len(s) != 2*32 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// ReturnCapacity posts `/internal/holds/{hold}/refund-capacity` with the abandonment's receipt
// id. 4xx is inventory's conclusive refusal; everything else that is not a decoded 200 is
// ErrUnavailable, and the same receipt id replays.
func (h HTTP) ReturnCapacity(ctx context.Context, org, hold, receipt uuid.UUID, quantity int32) (CapacityReturn, error) {
	code, raw, err := h.internal(ctx, http.MethodPost, fmt.Sprintf("%s/internal/holds/%s/refund-capacity", h.InventoryURL, hold),
		map[string]any{"organizer_id": org, "refund_id": receipt, "quantity": quantity})
	if err != nil {
		return CapacityReturn{}, fmt.Errorf("%w: inventory capacity return: %v", ErrUnavailable, err)
	}
	switch {
	case code == http.StatusOK:
	case code >= 400 && code < 500:
		return CapacityReturn{}, fmt.Errorf("%w: inventory answered %d: %s", ErrRefused, code, errorText(raw))
	default:
		return CapacityReturn{}, fmt.Errorf("%w: inventory answered %d", ErrUnavailable, code)
	}
	var body struct {
		HoldID             *uuid.UUID `json:"hold_id"`
		Quantity           *int32     `json:"quantity"`
		UnreturnedQuantity *int32     `json:"unreturned_quantity"`
		Replay             *bool      `json:"replay"`
	}
	if err := decodeExact(raw, &body); err != nil {
		return CapacityReturn{}, err
	}
	if body.HoldID == nil || body.Quantity == nil || body.UnreturnedQuantity == nil || body.Replay == nil {
		return CapacityReturn{}, fmt.Errorf("%w: inventory's answer is missing a declared field", ErrUnavailable)
	}
	return CapacityReturn{HoldID: *body.HoldID, Quantity: *body.Quantity, Unreturned: *body.UnreturnedQuantity}, nil
}
