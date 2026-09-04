package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ErrResponseTooLarge reports that an upstream response exceeded the caller's
// ceiling and was not read. A body that cannot be classified is ambiguous,
// never terminal.
var ErrResponseTooLarge = errors.New("upstream response body too large")

// ReadResponseBody reads an upstream response body with a byte ceiling. The
// extra byte distinguishes an exact-boundary body from a truncated one.
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

// WriteJSONNoStore writes a JSON response that must not be cached, replacing
// any cache policy already present on the response.
func WriteJSONNoStore(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// WriteJSONDefaultNoStore preserves an explicit cache policy and otherwise
// defaults the response to no-store.
func WriteJSONDefaultNoStore(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
