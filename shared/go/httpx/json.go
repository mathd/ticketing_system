package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// DecodeJSON decodes exactly one size-bounded JSON value. Public write
// endpoints use it to reject contract drift and ambiguous concatenated input.
func DecodeJSON(w http.ResponseWriter, r *http.Request, destination any, maxBytes int64) error {
	if maxBytes <= 0 {
		return errors.New("JSON body limit must be positive")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode JSON: multiple values")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}
