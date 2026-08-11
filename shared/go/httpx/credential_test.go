package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"ticketing/shared/httpx"
)

// The fail-closed arm is the one worth pinning: a service started without a
// credential must refuse everyone, and a plain constant-time compare of ""
// against "" returns TRUE — which would open every internal surface on a
// service whose env var went missing.
func TestCredentialMatches(t *testing.T) {
	cases := []struct {
		presented, configured string
		want                  bool
	}{
		{"secret", "secret", true},
		{"secret", "secrft", false},
		{"secret", "secre", false},
		{"", "", false},
		{"", "secret", false},
		{"secret", "", false},
	}
	for _, c := range cases {
		if got := httpx.CredentialMatches(c.presented, c.configured); got != c.want {
			t.Errorf("CredentialMatches(%q, %q) = %v, want %v", c.presented, c.configured, got, c.want)
		}
	}
}

func TestHeaderCredentialMatches(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(httpx.InternalToken, "secret")
	if !httpx.HeaderCredentialMatches(r, httpx.InternalToken, "secret") {
		t.Error("matching header rejected")
	}
	if httpx.HeaderCredentialMatches(r, httpx.InternalToken, "other") {
		t.Error("mismatched header accepted")
	}
	if httpx.HeaderCredentialMatches(httptest.NewRequest(http.MethodGet, "/", nil), httpx.InternalToken, "secret") {
		t.Error("absent header accepted")
	}
}
