package httpx

import (
	"errors"
	"fmt"
	"io"
)

// ErrResponseTooLarge reports that an upstream response exceeded the caller's
// ceiling and was NOT read. Callers that have to classify a side effect check
// for it with errors.Is: a body we refused to read is a body we could not
// classify, which is ambiguous, never terminal.
var ErrResponseTooLarge = errors.New("upstream response body too large")

// ReadResponseBody reads an upstream response body with a byte ceiling.
//
// A client timeout bounds how LONG a response may take, not how MANY bytes it
// may be. A malformed or hostile upstream can stream indefinitely inside its
// deadline, and io.ReadAll grows a buffer for all of it — on a checkout or
// recovery path, on a server holding other requests' claims.
//
// The limit is applied as maxBytes+1 so that a body of exactly maxBytes is
// accepted and the first byte past it is detected rather than silently
// truncated. Truncation is the failure mode to avoid above all others here: a
// clipped JSON body does not decode, but a clipped body that DOES decode would
// be classified on partial evidence.
func ReadResponseBody(body io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errors.New("response body limit must be positive")
	}
	raw, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("%w: over %d bytes", ErrResponseTooLarge, maxBytes)
	}
	return raw, nil
}
