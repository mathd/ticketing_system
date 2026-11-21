// Package accessadmission reads Access's admission summary of one order:
// `GET /internal/orders/{id}/admission?organizer_id=…`.
//
// It is the one strict reader of that answer. Three callers share it and keep their own rule:
//
//   - the exchange's pre-money check refuses an admitted or incompletely issued source;
//   - the settled-exchange abandonment (TKT-498) requires an admitted, fully issued source and
//     then a replacement with no issued ticket;
//   - the replacement refund dispatch guard requires issued replacement tickets before money.
//
// The reader decides only whether Access gave a well-formed answer. It never turns a failure
// to ask into a verdict: every answer that is not a well-formed 200 is ErrUnavailable.
//
// Every caller reads through NewCaller (R2-2, RA2-1): it sends the shared internal token that
// its constructor names and never follows a redirect. A transport that picks the credential
// from the URL, or follows a 3xx, can send the internal credential to the redirect target and
// take that target's answer as evidence.
package accessadmission

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"

	"ticketing/shared/httpx"
)

// Caller performs one service-to-service HTTP call. It is the shape of the Commerce server's
// `call` method and of refunds.Caller, so each caller passes its own transport and token.
type Caller func(ctx context.Context, method, url, key string, in any, internal bool) (int, []byte, error)

// maxAnswerBytes bounds one admission answer, which is two short fields.
const maxAnswerBytes = 64 << 10

// NewCaller returns the Access caller for admission reads. It always sends internalToken as
// X-Internal-Token, whatever the URL, and it returns a 3xx answer as it is, so Read reports it
// as ErrUnavailable and the redirect target receives no request. The rule applies to a copy of
// client on each call, so the client's other users keep their own redirect policy. A nil client
// makes every read unavailable.
func NewCaller(client *http.Client, internalToken string) Caller {
	return func(ctx context.Context, method, url, _ string, in any, _ bool) (int, []byte, error) {
		if client == nil {
			return 0, nil, errors.New("no HTTP client is configured")
		}
		if in != nil {
			return 0, nil, errors.New("an admission read sends no body")
		}
		noRedirect := *client
		noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		req, err := http.NewRequestWithContext(ctx, method, url, nil)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("X-Internal-Token", internalToken)
		resp, err := noRedirect.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		raw, err := httpx.ReadResponseBody(resp.Body, maxAnswerBytes)
		return resp.StatusCode, raw, err
	}
}

// Admission is Access's summary: whether any of the order's tickets was admitted, and how many
// tickets Access issued for the order in the organizer.
type Admission struct {
	Admitted    bool
	IssuedCount int32
}

// ErrUnavailable reports that Access could not be asked or did not answer with a well-formed
// summary. It is not evidence about the order.
var ErrUnavailable = errors.New("access admission unavailable")

// maxIssuedCount is the order quantity bound. A larger count is not an answer about one order.
const maxIssuedCount = 50

// Read asks Access for one order's admission summary, scoped by the organizer.
func Read(ctx context.Context, call Caller, accessURL string, order, organizer uuid.UUID) (Admission, error) {
	if accessURL == "" {
		return Admission{}, fmt.Errorf("%w: Access is not configured", ErrUnavailable)
	}
	url := fmt.Sprintf("%s/internal/orders/%s/admission?organizer_id=%s", accessURL, order, organizer)
	status, body, err := call(ctx, http.MethodGet, url, "", nil, true)
	if err != nil {
		return Admission{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if status != http.StatusOK {
		return Admission{}, fmt.Errorf("%w: access answered %d", ErrUnavailable, status)
	}
	return decode(body)
}

// decode accepts exactly one object with both declared fields, each of its declared type, no
// unknown field, no duplicate member name, no trailing content, an issued count in 0-50, and no
// admission without an issued ticket.
func decode(body []byte) (Admission, error) {
	// encoding/json keeps the last of duplicate names, so a body that names a field twice could
	// decode to either value. jsontext rejects duplicate names, escaped spellings included (R2-3).
	if !jsontext.Value(body).IsValid() {
		return Admission{}, fmt.Errorf("%w: not one JSON value with unique member names", ErrUnavailable)
	}
	var answer struct {
		Admitted    *bool  `json:"admitted"`
		IssuedCount *int32 `json:"issued_count"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&answer); err != nil {
		return Admission{}, fmt.Errorf("%w: decode: %v", ErrUnavailable, err)
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return Admission{}, fmt.Errorf("%w: trailing content after the summary", ErrUnavailable)
	}
	if answer.Admitted == nil || answer.IssuedCount == nil {
		return Admission{}, fmt.Errorf("%w: a declared field is missing or null", ErrUnavailable)
	}
	if *answer.IssuedCount < 0 || *answer.IssuedCount > maxIssuedCount {
		return Admission{}, fmt.Errorf("%w: issued_count %d is out of range", ErrUnavailable, *answer.IssuedCount)
	}
	// Access reports an admission only for one of the order's issued tickets
	// (services/access/internal/store/admission.go). An admission with no issued ticket
	// contradicts itself, and must not read as "zero tickets" (R2-3).
	if *answer.Admitted && *answer.IssuedCount == 0 {
		return Admission{}, fmt.Errorf("%w: admitted with no issued ticket", ErrUnavailable)
	}
	return Admission{Admitted: *answer.Admitted, IssuedCount: *answer.IssuedCount}, nil
}
