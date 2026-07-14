package httpx

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeJSON(t *testing.T) {
	tests := []struct {
		name string
		body string
		max  int64
		want string
	}{
		{name: "valid", body: `{"name":"ticket"}`, max: 1 << 10},
		{name: "trailing whitespace", body: "{\"name\":\"ticket\"}\n \t", max: 1 << 10},
		{name: "unknown field", body: `{"name":"ticket","price":10}`, max: 1 << 10, want: "unknown field"},
		{name: "second value", body: `{"name":"ticket"}{}`, max: 1 << 10, want: "multiple values"},
		{name: "malformed", body: `{"name":`, max: 1 << 10, want: "unexpected EOF"},
		{name: "oversized", body: `{"name":"ticket"}`, max: 5, want: "request body too large"},
		{name: "invalid limit", body: `{}`, max: 0, want: "limit must be positive"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(test.body))
			var destination struct {
				Name string `json:"name"`
			}
			err := DecodeJSON(httptest.NewRecorder(), request, &destination, test.max)
			if test.want == "" {
				if err != nil {
					t.Fatalf("DecodeJSON() error = %v", err)
				}
				if destination.Name != "ticket" {
					t.Fatalf("name = %q", destination.Name)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecodeJSON() error = %v, want substring %q", err, test.want)
			}
		})
	}
}
