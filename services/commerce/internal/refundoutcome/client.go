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

// readEvidenceBody reads at most maxProofBody bytes, plus one to detect a longer body. A body
// longer than the cap is refused before it is decoded: a decoder that reads a truncated body
// sees its end at the cap and could accept a valid prefix of a malformed response.
func readEvidenceBody(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxProofBody+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %v", err)
	}
	if len(raw) > maxProofBody {
		return nil, fmt.Errorf("response is longer than %d bytes", maxProofBody)
	}
	return raw, nil
}

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
	raw, err := readEvidenceBody(resp.Body)
	if err != nil {
		return FailureProof{}, fmt.Errorf("%w: %v", ErrProofUnavailable, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusConflict:
		return FailureProof{}, fmt.Errorf("%w: payments answered %d: %s", ErrProofRefused, resp.StatusCode, errorText(raw))
	default:
		return FailureProof{}, fmt.Errorf("%w: payments answered %d: %s", ErrProofUnavailable, resp.StatusCode, errorText(raw))
	}

	proof, err := decodeEvidence(raw)
	if err != nil {
		return FailureProof{}, fmt.Errorf("%w: %v", ErrProofUnavailable, err)
	}
	return proof, nil
}

// decodeEvidence reads payments' evidence body: exactly one object of the declared shape, no
// unknown field and no trailing content. The failure proof and the abandon answer share it.
func decodeEvidence(raw []byte) (FailureProof, error) {
	var body struct {
		ProviderRef string `json:"provider_ref"`
		Amount      int64  `json:"amount"`
		Currency    string `json:"currency"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return FailureProof{}, fmt.Errorf("decode evidence: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return FailureProof{}, errors.New("trailing content after the evidence")
	}
	if !strings.HasPrefix(body.ProviderRef, "re_") || len(body.ProviderRef) <= len("re_") ||
		body.Amount <= 0 || !isISOCurrency(body.Currency) {
		return FailureProof{}, errors.New("evidence is not the declared shape")
	}
	return FailureProof{ProviderRef: body.ProviderRef, Amount: body.Amount, Currency: body.Currency}, nil
}

// HTTPAbandoner calls payments' abandon endpoint (TKT-525 S2). It is its own type, not a
// method on HTTPPayments: the outside-completion path holds HTTPPayments and must stay unable
// to express a write.
type HTTPAbandoner struct {
	Client      *http.Client
	PaymentsURL string
	Token       string
}

func NewHTTPAbandoner(paymentsURL, token string, timeout time.Duration) HTTPAbandoner {
	return HTTPAbandoner{
		Client:      &http.Client{Timeout: timeout},
		PaymentsURL: strings.TrimSuffix(paymentsURL, "/"),
		Token:       token,
	}
}

// AbandonRefundLeg asks payments to mark one leg abandoned after its own terminal-failure proof.
//
// 200 with a well-formed body is the only success. 404 and 409 are payments' conclusive
// refusals of this request: it wrote no marker. Everything else — a transport error, a timeout,
// 400, 401, 5xx, or a body that is not exactly the declared shape — is UNCERTAIN: payments may
// have committed the marker before the answer was lost. In every case Commerce keeps its
// reservation pending and the quantity reserved, and the same command run again finishes from
// the stored marker.
func (p HTTPAbandoner) AbandonRefundLeg(ctx context.Context, org uuid.UUID, chargeKey, refundKey string, amount int64, currency string) (FailureProof, error) {
	payload, err := json.Marshal(map[string]any{
		"organizer_id": org, "idempotency_key": chargeKey, "refund_key": refundKey,
		"amount": amount, "currency": currency,
	})
	if err != nil {
		return FailureProof{}, fmt.Errorf("%w: %v", ErrAbandonUncertain, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.PaymentsURL+"/internal/psp/partial-refund/abandon", bytes.NewReader(payload))
	if err != nil {
		return FailureProof{}, fmt.Errorf("%w: %v", ErrAbandonUncertain, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", p.Token)
	resp, err := p.Client.Do(req)
	if err != nil {
		return FailureProof{}, fmt.Errorf("%w: %v", ErrAbandonUncertain, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := readEvidenceBody(resp.Body)
	if err != nil {
		return FailureProof{}, fmt.Errorf("%w: %v", ErrAbandonUncertain, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusConflict:
		return FailureProof{}, fmt.Errorf("%w: payments answered %d: %s", ErrAbandonRefused, resp.StatusCode, errorText(raw))
	default:
		return FailureProof{}, fmt.Errorf("%w: payments answered %d: %s", ErrAbandonUncertain, resp.StatusCode, errorText(raw))
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
