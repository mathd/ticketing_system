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
package accessadmission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"
)

// Caller performs one service-to-service HTTP call. It is the shape of the Commerce server's
// `call` method and of refunds.Caller, so each caller passes its own transport and token.
type Caller func(ctx context.Context, method, url, key string, in any, internal bool) (int, []byte, error)

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
// unknown field, no trailing content, and an issued count in 0-50.
func decode(body []byte) (Admission, error) {
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
	return Admission{Admitted: *answer.Admitted, IssuedCount: *answer.IssuedCount}, nil
}
