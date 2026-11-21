package refundoutcome

// The read-only payments failure-proof client.
//
// Its own type, with one method that issues one GET, for the reason exchangeunwind gives for
// its own evidence client: the operator command must hold nothing that can move money. The
// HTTP mechanics follow exchangeunwind/client.go — one decode, no trailing content — because
// those were learned from review there.

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
)

// HTTPPayments reads payments' failure-proof endpoint.
type HTTPPayments struct {
	Client      *http.Client
	PaymentsURL string
	Token       string
}

func NewHTTPPayments(paymentsURL, token string, timeout time.Duration) HTTPPayments {
	return HTTPPayments{
		Client:      &http.Client{Timeout: timeout},
		PaymentsURL: strings.TrimSuffix(paymentsURL, "/"),
		Token:       token,
	}
}

// maxProofBody bounds the response read. A proof is three short fields.
const maxProofBody = 64 << 10

// ProveRefundFailure asks payments for the terminal-failure proof of one leg.
//
// 200 with a well-formed body is the only proof. 404 and 409 are payments' conclusive
// refusals (no such leg; or completed, marked, or contradicted). Everything else — a transport
// error, 400, 401, 5xx, or a body that is not exactly the declared shape — is unavailable:
// payments could not be asked, which is not an answer.
func (p HTTPPayments) ProveRefundFailure(ctx context.Context, org uuid.UUID, chargeKey, refundKey string) (FailureProof, error) {
	q := url.Values{}
	q.Set("organizer_id", org.String())
	q.Set("idempotency_key", chargeKey)
	q.Set("refund_key", refundKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.PaymentsURL+"/internal/psp/partial-refund/failure-proof?"+q.Encode(), nil)
	if err != nil {
		return FailureProof{}, fmt.Errorf("%w: %v", ErrProofUnavailable, err)
	}
	req.Header.Set("X-Internal-Token", p.Token)
	resp, err := p.Client.Do(req)
	if err != nil {
		return FailureProof{}, fmt.Errorf("%w: %v", ErrProofUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxProofBody))
	if err != nil {
		return FailureProof{}, fmt.Errorf("%w: read response: %v", ErrProofUnavailable, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusConflict:
		return FailureProof{}, fmt.Errorf("%w: payments answered %d: %s", ErrProofRefused, resp.StatusCode, errorText(raw))
	default:
		return FailureProof{}, fmt.Errorf("%w: payments answered %d: %s", ErrProofUnavailable, resp.StatusCode, errorText(raw))
	}

	var body struct {
		ProviderRef string `json:"provider_ref"`
		Amount      int64  `json:"amount"`
		Currency    string `json:"currency"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return FailureProof{}, fmt.Errorf("%w: decode proof: %v", ErrProofUnavailable, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return FailureProof{}, fmt.Errorf("%w: trailing content after the proof", ErrProofUnavailable)
	}
	if !strings.HasPrefix(body.ProviderRef, "re_") || len(body.ProviderRef) <= len("re_") ||
		body.Amount <= 0 || !isISOCurrency(body.Currency) {
		return FailureProof{}, fmt.Errorf("%w: proof is not the declared shape", ErrProofUnavailable)
	}
	return FailureProof{ProviderRef: body.ProviderRef, Amount: body.Amount, Currency: body.Currency}, nil
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
