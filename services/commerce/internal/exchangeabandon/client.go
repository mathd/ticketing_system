package exchangeabandon

// The HTTP side of the abandonment command: Access's admission read, payments' evidence reads,
// its partial refund and its journal append, and inventory's capacity return.
//
// The client decodes; the service judges. Every answer is decoded strictly: exactly one JSON
// object of the declared shape, no unknown field, no trailing content. An answer that does not
// decode is ErrUnavailable, never a verdict. The HTTP mechanics follow exchangeunwind and
// refundoutcome, whose rules came from review there.

import (
	"bytes"
	"context"
	"encoding/json"
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
// the internal token.
type HTTP struct {
	Client                               *http.Client
	AccessURL, PaymentsURL, InventoryURL string
	InternalToken, PaymentsToken         string
}

// NewHTTP builds the client with one timeout per call.
func NewHTTP(accessURL, paymentsURL, inventoryURL, internalToken, paymentsToken string, timeout time.Duration) HTTP {
	return HTTP{
		Client:        &http.Client{Timeout: timeout},
		AccessURL:     strings.TrimSuffix(accessURL, "/"),
		PaymentsURL:   strings.TrimSuffix(paymentsURL, "/"),
		InventoryURL:  strings.TrimSuffix(inventoryURL, "/"),
		InternalToken: internalToken,
		PaymentsToken: paymentsToken,
	}
}

// call performs one request. It has the accessadmission.Caller shape so that the admission
// read is the same code the pre-money check runs.
func (h HTTP) call(ctx context.Context, method, u, key string, in any, internal bool) (int, []byte, error) {
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
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if internal {
		token := h.InternalToken
		if h.PaymentsURL != "" && strings.HasPrefix(u, h.PaymentsURL) {
			token = h.PaymentsToken
		}
		req.Header.Set("X-Internal-Token", token)
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := httpx.ReadResponseBody(resp.Body, maxAnswerBytes)
	return resp.StatusCode, raw, err
}

// decodeStrict accepts exactly one object of out's shape with no unknown field and no
// trailing content.
func decodeStrict(raw []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%w: decode: %v", ErrUnavailable, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing content after the answer", ErrUnavailable)
	}
	return nil
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

// Admission reads Access's admission summary of one order through the shared strict reader.
func (h HTTP) Admission(ctx context.Context, org, order uuid.UUID) (accessadmission.Admission, error) {
	return accessadmission.Read(ctx, h.call, h.AccessURL, order, org)
}

// ChargeOperation reads `GET /internal/operations` for one charge key. 404 is the only absence.
func (h HTTP) ChargeOperation(ctx context.Context, org uuid.UUID, key string) (ChargeEvidence, error) {
	q := url.Values{}
	q.Set("organizer_id", org.String())
	q.Set("idempotency_key", key)
	code, raw, err := h.call(ctx, http.MethodGet, h.PaymentsURL+"/internal/operations?"+q.Encode(), "", nil, true)
	if err != nil {
		return ChargeEvidence{}, fmt.Errorf("%w: payments operation read: %v", ErrUnavailable, err)
	}
	switch code {
	case http.StatusNotFound:
		return ChargeEvidence{}, nil
	case http.StatusOK:
	default:
		return ChargeEvidence{}, fmt.Errorf("%w: payments operation read answered %d", ErrUnavailable, code)
	}
	var body struct {
		Resolved                *bool           `json:"resolved"`
		OccurredAt              json.RawMessage `json:"occurred_at"`
		Status                  *string         `json:"status"`
		FactID                  *uuid.UUID      `json:"fact_id"`
		CapturedAmount          *int64          `json:"captured_amount"`
		Currency                *string         `json:"currency"`
		ConfirmedCapturedAmount *int64          `json:"confirmed_captured_amount"`
		ConfirmedCurrency       *string         `json:"confirmed_currency"`
		StatusReplayDeadlineAt  json.RawMessage `json:"status_replay_deadline_at"`
	}
	if err := decodeStrict(raw, &body); err != nil {
		return ChargeEvidence{}, err
	}
	if body.Resolved == nil {
		return ChargeEvidence{}, fmt.Errorf("%w: payments operation read has no resolved field", ErrUnavailable)
	}
	return ChargeEvidence{
		Found: true, Resolved: *body.Resolved,
		CapturedAmount: body.CapturedAmount, Currency: body.Currency,
		ConfirmedAmount: body.ConfirmedCapturedAmount, ConfirmedCurrency: body.ConfirmedCurrency,
	}, nil
}

// RefundLeg reads `GET /internal/refund-legs` for one (source key, refund key) leg. 404 is the
// only absence.
func (h HTTP) RefundLeg(ctx context.Context, org uuid.UUID, sourceKey, refundKey string) (LegEvidence, error) {
	q := url.Values{}
	q.Set("organizer_id", org.String())
	q.Set("source_idempotency_key", sourceKey)
	q.Set("refund_idempotency_key", refundKey)
	code, raw, err := h.call(ctx, http.MethodGet, h.PaymentsURL+"/internal/refund-legs?"+q.Encode(), "", nil, true)
	if err != nil {
		return LegEvidence{}, fmt.Errorf("%w: payments refund-leg read: %v", ErrUnavailable, err)
	}
	switch code {
	case http.StatusNotFound:
		return LegEvidence{}, nil
	case http.StatusOK:
	default:
		return LegEvidence{}, fmt.Errorf("%w: payments refund-leg read answered %d", ErrUnavailable, code)
	}
	var body struct {
		Completed         *bool   `json:"completed"`
		Amount            *int64  `json:"amount"`
		Currency          *string `json:"currency"`
		ConfirmedAmount   *int64  `json:"confirmed_amount"`
		ConfirmedCurrency *string `json:"confirmed_currency"`
	}
	if err := decodeStrict(raw, &body); err != nil {
		return LegEvidence{}, err
	}
	if body.Completed == nil || body.Amount == nil || body.Currency == nil {
		return LegEvidence{}, fmt.Errorf("%w: payments refund-leg read is missing a declared field", ErrUnavailable)
	}
	return LegEvidence{
		Found: true, Completed: *body.Completed, Amount: *body.Amount, Currency: *body.Currency,
		ConfirmedAmount: body.ConfirmedAmount, ConfirmedCurrency: body.ConfirmedCurrency,
	}, nil
}

// PartialRefund posts the delta refund. 200 with a well-formed `refunded` answer is the only
// success. A typed 422 `provider_refund_failed` with a provider `re_…` reference is the
// provider's terminal failure. 400, 404 and 409 are payments' conclusive refusals of this
// request. Everything else, and every answer that does not decode, is ErrUnavailable: payments
// may have moved the money, and the same key resolves it on the next run.
func (h HTTP) PartialRefund(ctx context.Context, r RefundRequest) (RefundAnswer, error) {
	code, raw, err := h.call(ctx, http.MethodPost, h.PaymentsURL+"/internal/psp/partial-refund", "", map[string]any{
		"organizer_id": r.OrganizerID, "idempotency_key": r.ChargeKey, "refund_key": r.RefundKey,
		"amount": r.Amount, "currency": r.Currency,
	}, true)
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
		if decodeStrict(raw, &failed) != nil || failed.Code != "provider_refund_failed" ||
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
	if err := decodeStrict(raw, &body); err != nil {
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
	code, raw, err := h.call(ctx, http.MethodPost, h.PaymentsURL+"/internal/facts", "", map[string]any{
		"fact_id": f.ID, "organizer_id": f.OrganizerID, "fact_type": store.ExchangeAbandonedFactType,
		"buyer_id": f.BuyerID, "amount": f.Amount, "currency": f.Currency,
		"occurred_at": f.OccurredAt.UTC().Format(time.RFC3339Nano),
		"payload":     f.Payload(),
	}, true)
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
	if err := decodeStrict(raw, &ack); err != nil {
		return err
	}
	if ack.Sequence == nil || *ack.Sequence <= 0 || ack.Hash == nil || *ack.Hash == "" || ack.Replay == nil {
		return fmt.Errorf("%w: payments journal acknowledgment is incomplete", ErrUnavailable)
	}
	return nil
}

// ReturnCapacity posts `/internal/holds/{hold}/refund-capacity` with the abandonment's receipt
// id. 4xx is inventory's conclusive refusal; everything else that is not a decoded 200 is
// ErrUnavailable, and the same receipt id replays.
func (h HTTP) ReturnCapacity(ctx context.Context, org, hold, receipt uuid.UUID, quantity int32) (CapacityReturn, error) {
	code, raw, err := h.call(ctx, http.MethodPost, fmt.Sprintf("%s/internal/holds/%s/refund-capacity", h.InventoryURL, hold), "",
		map[string]any{"organizer_id": org, "refund_id": receipt, "quantity": quantity}, true)
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
	if err := decodeStrict(raw, &body); err != nil {
		return CapacityReturn{}, err
	}
	if body.HoldID == nil || body.Quantity == nil || body.UnreturnedQuantity == nil || body.Replay == nil {
		return CapacityReturn{}, fmt.Errorf("%w: inventory's answer is missing a declared field", ErrUnavailable)
	}
	return CapacityReturn{HoldID: *body.HoldID, Quantity: *body.Quantity, Unreturned: *body.UnreturnedQuantity}, nil
}
