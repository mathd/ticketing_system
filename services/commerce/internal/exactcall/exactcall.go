// Package exactcall sends one credentialed request to another service and decodes its JSON
// answer exactly (TKT-594).
//
// It holds only the mechanics. Each client keeps its own field rules and its own error classes:
// an answer that this package refuses is never a verdict, and the caller decides what "not an
// answer" means for its operation.
//
// The rules come from the exchange abandonment client (TKT-498 D25, D35, R2-3, RA2-4, R3-1):
//
//   - A redirect is never followed. The 3xx answer is the answer, so the redirect target gets
//     no request and no credential. The rule is applied to a copy of the caller's http.Client on
//     each request, so a client that a caller builds without a constructor keeps it.
//   - The body is read up to a ceiling. A longer body is refused before it is decoded.
//   - A body is exactly one JSON value of the declared shape: member names match exactly, and an
//     unknown member, a duplicate member (escaped spellings included) or trailing content is
//     refused.
package exactcall

import (
	"bytes"
	"context"
	jsonv2 "encoding/json/v2"
	"fmt"
	"io"
	"net/http"

	"ticketing/shared/httpx"
)

// RedirectError is a 3xx answer. The request was not repeated anywhere else.
type RedirectError struct {
	// Peer names the service in the refusal, for example "payments".
	Peer   string
	Status int
}

func (e *RedirectError) Error() string {
	return fmt.Sprintf("%s redirect refused (HTTP %d)", e.Peer, e.Status)
}

// Request is one call. Credential is sent as the internal-token header; the caller names which
// credential it is at its own call site, so a configured URL never chooses it.
type Request struct {
	Peer        string
	Method, URL string
	Credential  string
	// JSONBody is sent with Content-Type application/json. Nil sends no body.
	JSONBody []byte
	// MaxBody bounds the response body in bytes.
	MaxBody int64
}

// Send performs r with a copy of client that never follows a redirect. It returns the status and
// the body of every answer that is not a 3xx. A 3xx is a *RedirectError, and its body is not read.
// A body longer than r.MaxBody is httpx.ErrResponseTooLarge.
func Send(ctx context.Context, client *http.Client, r Request) (int, []byte, error) {
	var body io.Reader
	if r.JSONBody != nil {
		body = bytes.NewReader(r.JSONBody)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, r.URL, body)
	if err != nil {
		return 0, nil, err
	}
	if r.JSONBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(httpx.InternalToken, r.Credential)
	once := *client
	once.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := once.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return resp.StatusCode, nil, &RedirectError{Peer: r.Peer, Status: resp.StatusCode}
	}
	raw, err := httpx.ReadResponseBody(resp.Body, r.MaxBody)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}

// Decode accepts exactly one JSON value of out's shape: member names matched exactly, no unknown
// or duplicate member (escaped spellings included) and no trailing content. The v1 decoder
// matched names without regard to case and kept the last duplicate, so a body could carry two
// values for one field.
//
// Decode does not refuse a missing member or an explicit null. Make a required field a pointer
// and refuse nil: both an absent member and a null leave it nil.
func Decode(raw []byte, out any) error {
	return jsonv2.Unmarshal(raw, out, jsonv2.RejectUnknownMembers(true))
}
