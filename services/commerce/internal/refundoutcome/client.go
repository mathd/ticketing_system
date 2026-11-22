package refundoutcome

// The read-only payments failure-proof client.
//
// Its own type, with one method that issues one GET, for the reason exchangeunwind gives for
// its own evidence client: the operator command must hold nothing that can move money. The
// HTTP mechanics are exactcall's (TKT-594): no redirect is followed, the body is capped, and the
// answer is decoded exactly.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/exactcall"
)

// HTTPPayments reads payments' failure-proof endpoint.
type HTTPPayments struct {
	Client        *http.Client
	PaymentsURL   string
	PaymentsToken string
}

func NewHTTPPayments(paymentsURL, paymentsToken string, timeout time.Duration) HTTPPayments {
	return HTTPPayments{
		Client:        &http.Client{Timeout: timeout},
		PaymentsURL:   strings.TrimSuffix(paymentsURL, "/"),
		PaymentsToken: paymentsToken,
	}
}

// maxProofBody bounds the response read. A proof is three short fields. A longer body is
// refused before it is decoded: a decoder that reads a truncated body sees its end at the cap
// and could accept a valid prefix of a malformed response.
const maxProofBody = 64 << 10

// maxProviderRef is the longest provider reference payments returns, in bytes: its own guard
// (partial_refund_failure_proof.go proveTerminalFailure) and its contract (openapi.yaml
// RefundLegFailureProof).
const maxProviderRef = 255

// errInvalidEvidence is a 200 that is not exactly the evidence payments writes. The operator
// sees this fixed text, never the body or a decoded value.
var errInvalidEvidence = errors.New("payments returned invalid refund evidence")

// ProveRefundFailure asks payments for the terminal-failure proof of one leg.
//
// 200 with a well-formed body is the only proof. 404 and 409 are payments' conclusive
// refusals (no such leg; or completed, marked, or contradicted). Everything else — a transport
// error, a redirect, 400, 401, 5xx, or a body that is not exactly the declared shape — is
// unavailable: payments could not be asked, which is not an answer.
func (p HTTPPayments) ProveRefundFailure(ctx context.Context, org uuid.UUID, chargeKey, refundKey string) (FailureProof, error) {
	q := url.Values{}
	q.Set("organizer_id", org.String())
	q.Set("idempotency_key", chargeKey)
	q.Set("refund_key", refundKey)
	code, raw, err := exactcall.Send(ctx, p.Client, exactcall.Request{
		Peer: "payments", Method: http.MethodGet,
		URL:        p.PaymentsURL + "/internal/psp/partial-refund/failure-proof?" + q.Encode(),
		Credential: p.PaymentsToken, MaxBody: maxProofBody,
	})
	if err != nil {
		return FailureProof{}, fmt.Errorf("%w: %v", ErrProofUnavailable, err)
	}
	switch code {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusConflict:
		return FailureProof{}, fmt.Errorf("%w: payments answered %d: %s", ErrProofRefused, code, errorText(raw))
	default:
		return FailureProof{}, fmt.Errorf("%w: payments answered %d: %s", ErrProofUnavailable, code, errorText(raw))
	}

	proof, err := decodeEvidence(raw)
	if err != nil {
		return FailureProof{}, fmt.Errorf("%w: %v", ErrProofUnavailable, err)
	}
	return proof, nil
}

// decodeEvidence reads payments' evidence body, which the failure proof and the abandon answer
// share (openapi.yaml RefundLegFailureProof): exactly one object with the three members, each
// present and not null, and each value one that payments can write. A provider reference is
// `re_` and a suffix, at most 255 bytes; the amount is positive minor units; the currency is
// three uppercase ASCII letters.
func decodeEvidence(raw []byte) (FailureProof, error) {
	var body struct {
		ProviderRef *string `json:"provider_ref"`
		Amount      *int64  `json:"amount"`
		Currency    *string `json:"currency"`
	}
	if exactcall.Decode(raw, &body) != nil ||
		body.ProviderRef == nil || body.Amount == nil || body.Currency == nil {
		return FailureProof{}, errInvalidEvidence
	}
	ref := *body.ProviderRef
	if !strings.HasPrefix(ref, "re_") || len(ref) <= len("re_") || len(ref) > maxProviderRef ||
		*body.Amount <= 0 || !isISOCurrency(*body.Currency) {
		return FailureProof{}, errInvalidEvidence
	}
	return FailureProof{ProviderRef: ref, Amount: *body.Amount, Currency: *body.Currency}, nil
}

// HTTPAbandoner calls payments' abandon endpoint (TKT-525 S2). It is its own type, not a
// method on HTTPPayments: the outside-completion path holds HTTPPayments and must stay unable
// to express a write.
type HTTPAbandoner struct {
	Client        *http.Client
	PaymentsURL   string
	PaymentsToken string
}

func NewHTTPAbandoner(paymentsURL, paymentsToken string, timeout time.Duration) HTTPAbandoner {
	return HTTPAbandoner{
		Client:        &http.Client{Timeout: timeout},
		PaymentsURL:   strings.TrimSuffix(paymentsURL, "/"),
		PaymentsToken: paymentsToken,
	}
}

// AbandonRefundLeg asks payments to mark one leg abandoned after its own terminal-failure proof.
//
// 200 with a well-formed body is the only success. 404 and 409 are payments' conclusive
// refusals of this request: it wrote no marker. Everything else — a transport error, a timeout,
// a redirect, 400, 401, 5xx, or a body that is not exactly the declared shape — is UNCERTAIN:
// payments may have committed the marker before the answer was lost. In every case Commerce
// keeps its reservation pending and the quantity reserved, and the same command run again
// finishes from the stored marker.
func (p HTTPAbandoner) AbandonRefundLeg(ctx context.Context, org uuid.UUID, chargeKey, refundKey string, amount int64, currency string) (FailureProof, error) {
	payload, err := json.Marshal(map[string]any{
		"organizer_id": org, "idempotency_key": chargeKey, "refund_key": refundKey,
		"amount": amount, "currency": currency,
	})
	if err != nil {
		return FailureProof{}, fmt.Errorf("%w: %v", ErrAbandonUncertain, err)
	}
	code, raw, err := exactcall.Send(ctx, p.Client, exactcall.Request{
		Peer: "payments", Method: http.MethodPost,
		URL:        p.PaymentsURL + "/internal/psp/partial-refund/abandon",
		Credential: p.PaymentsToken, JSONBody: payload, MaxBody: maxProofBody,
	})
	if err != nil {
		return FailureProof{}, fmt.Errorf("%w: %v", ErrAbandonUncertain, err)
	}
	switch code {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusConflict:
		return FailureProof{}, fmt.Errorf("%w: payments answered %d: %s", ErrAbandonRefused, code, errorText(raw))
	default:
		return FailureProof{}, fmt.Errorf("%w: payments answered %d: %s", ErrAbandonUncertain, code, errorText(raw))
	}
	evidence, err := decodeEvidence(raw)
	if err != nil {
		return FailureProof{}, fmt.Errorf("%w: %v", ErrAbandonUncertain, err)
	}
	return evidence, nil
}

// errorText returns payments' static error message, or nothing. Payments bounds these bodies;
// an undecodable one is not repeated to the operator.
func errorText(raw []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) != nil || len(e.Error) > 200 {
		return "(no message)"
	}
	return e.Error
}

func isISOCurrency(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}
