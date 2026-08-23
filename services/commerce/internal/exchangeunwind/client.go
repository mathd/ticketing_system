package exchangeunwind

// The read-only payments evidence client.
//
// Deliberately its own type rather than a method set bolted onto `recovery.HTTPClients`:
// that type carries Void, Refund and the inventory surface, and the whole point of this
// package's port is that the unwind holds nothing capable of moving money. The HTTP
// mechanics below are the same shape as `recovery/clients.go`'s — one decode, no trailing
// content, credential chosen by destination — because those were learned from three review
// passes there and re-deriving them would be how they get lost.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// HTTPPayments reads payments' two evidence endpoints.
type HTTPPayments struct {
	Client      *http.Client
	PaymentsURL string
	Token       string
}

func NewHTTPPayments(paymentsURL, token string, timeout time.Duration) HTTPPayments {
	return HTTPPayments{
		Client:      &http.Client{Timeout: timeout},
		PaymentsURL: paymentsURL,
		Token:       token,
	}
}

// get performs one evidence read and reports the status alongside the decoded body.
//
// A transport error returns status 0 and the error, which every caller maps to
// Indeterminate. It never returns "not found" for a failure to ask — the distinction
// between "payments says no operation exists" and "payments could not be reached" is the
// one this whole mechanism turns on, and collapsing it would let an outage read as
// permission to delete a charged buyer's binding.
func (p HTTPPayments) get(ctx context.Context, u string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Internal-Token", p.Token)
	resp, err := p.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil && resp.StatusCode == http.StatusOK {
		dec := json.NewDecoder(resp.Body)
		if err := dec.Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode response: %w", err)
		}
		// Exactly one JSON value, proven by a Token() that reports EOF. `recovery`'s client
		// carries the same check and the comment explaining why dec.More() is not enough:
		// it answers false on a `}` trailer, so two concatenated bodies were accepted as
		// proof. A body that cannot be read in full is not evidence.
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			return resp.StatusCode, errors.New("decode response: trailing content after JSON value")
		}
	}
	return resp.StatusCode, nil
}

// LookupChargeOperation reads `GET /internal/operations` for an upgrade's charge key.
//
// 404 is the ONLY absence. Payments documents it as exactly that: "no operation exists —
// evidence the charge was never submitted, which is what lets commerce release the claim
// rather than guess."
//
// A 200 is NOT automatically presence, and the difference decides whether an operator is
// told they have a charged buyer or an unanswerable question:
//
//   - resolved with a captured provider state → money moved. Present.
//   - resolved but DECLINED → no money moved, but this unit refuses to call that Absent.
//     Deciding that a declined operation is safe to unwind means reasoning about provider
//     semantics from a status string, and the cost of being wrong is a charged buyer's
//     binding deleted. It answers Indeterminate, and the operator sees why.
//   - bound but UNRESOLVED → payments does not know either. Indeterminate.
func (p HTTPPayments) LookupChargeOperation(ctx context.Context, org uuid.UUID, key string) (MoneyEvidence, error) {
	u := fmt.Sprintf("%s/internal/operations?organizer_id=%s&idempotency_key=%s",
		p.PaymentsURL, org, url.QueryEscape(key))
	var body struct {
		Resolved       bool   `json:"resolved"`
		Status         string `json:"status"`
		CapturedAmount *int64 `json:"captured_amount"`
	}
	code, err := p.get(ctx, u, &body)
	if err != nil {
		return Indeterminate, fmt.Errorf("lookup charge operation: %w", err)
	}
	switch code {
	case http.StatusNotFound:
		return Absent, nil
	case http.StatusOK:
		// `captured_amount` is published by payments ONLY when the provider state is
		// `captured`, which makes its presence the least interpretive signal available —
		// stronger than reading the status string, because payments already did that
		// reading when it decided whether to publish the field.
		if body.CapturedAmount != nil {
			return Present, nil
		}
		return Indeterminate, fmt.Errorf(
			"payments answered 200 for %s without capture evidence (resolved=%t status=%q): "+
				"this is not proof the charge is absent", key, body.Resolved, body.Status)
	default:
		return Indeterminate, fmt.Errorf("lookup charge operation %s: unexpected status %d", key, code)
	}
}

// LookupRefundLeg reads `GET /internal/refund-legs` for a downgrade's refund key.
//
// THE ENDPOINT AND THE KEYS ARE BOTH DIFFERENT from the upgrade path, which is the entire
// reason this method exists separately. A downgrade's money lives in payments' refund-leg
// table, not in `payment_operations`, and asking the operations endpoint about a refund key
// returns 404 — an answer that looks exactly like proof of safety and is not.
//
// `completed` distinguishes a leg that settled from one merely bound; payments' own comment
// is that "a bound leg is money the buyer has not received back". Either way money is in
// flight against this exchange, so both refuse — a bound-but-uncompleted leg is
// Indeterminate rather than Absent.
func (p HTTPPayments) LookupRefundLeg(ctx context.Context, org uuid.UUID, sourceKey, refundKey string) (MoneyEvidence, error) {
	u := fmt.Sprintf("%s/internal/refund-legs?organizer_id=%s&source_idempotency_key=%s&refund_idempotency_key=%s",
		p.PaymentsURL, org, url.QueryEscape(sourceKey), url.QueryEscape(refundKey))
	var body struct {
		Completed bool  `json:"completed"`
		Amount    int64 `json:"amount"`
	}
	code, err := p.get(ctx, u, &body)
	if err != nil {
		return Indeterminate, fmt.Errorf("lookup refund leg: %w", err)
	}
	switch code {
	case http.StatusNotFound:
		return Absent, nil
	case http.StatusOK:
		if body.Completed {
			return Present, nil
		}
		return Indeterminate, fmt.Errorf(
			"payments holds an uncompleted refund leg for %s (amount=%d): money is bound "+
				"against this exchange and has not settled", refundKey, body.Amount)
	default:
		return Indeterminate, fmt.Errorf("lookup refund leg %s: unexpected status %d", refundKey, code)
	}
}
