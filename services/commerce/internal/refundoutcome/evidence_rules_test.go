package refundoutcome

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TKT-594 COS 3 and 4. The failure proof and the abandon answer are one evidence object
// (openapi.yaml RefundLegFailureProof). Each case changes ONE thing in a producer-valid body,
// and both clients must refuse it with the same fixed text: the proof as unavailable, the
// abandonment as uncertain. The accepted cases are the controls: each refused case differs from
// one of them in the rule under test only.

// answerWith is a payments that answers every request 200 with body.
func answerWith(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// askBoth sends one proof read and one abandon request to a payments at paymentsURL.
func askBoth(t *testing.T, paymentsURL string) (proof FailureProof, proofErr error, abandon FailureProof, abandonErr error) {
	t.Helper()
	ctx := context.Background()
	proof, proofErr = NewHTTPPayments(paymentsURL, "t", 5*time.Second).ProveRefundFailure(ctx, uuid.New(), "k", uuid.NewString())
	abandon, abandonErr = NewHTTPAbandoner(paymentsURL, "t", 5*time.Second).AbandonRefundLeg(ctx, uuid.New(), "k", uuid.NewString(), 2000, "EUR")
	return proof, proofErr, abandon, abandonErr
}

// assertRefused checks that both clients refused with their class and exactly `detail`.
func assertRefused(t *testing.T, paymentsURL, detail string) {
	t.Helper()
	proof, proofErr, abandon, abandonErr := askBoth(t, paymentsURL)
	if want := "refund failure proof unavailable: " + detail; !errors.Is(proofErr, ErrProofUnavailable) || proofErr.Error() != want || proof != (FailureProof{}) {
		t.Errorf("proof = %+v err = %v, want %q", proof, proofErr, want)
	}
	if want := "payments abandonment answer unknown: " + detail; !errors.Is(abandonErr, ErrAbandonUncertain) || abandonErr.Error() != want || abandon != (FailureProof{}) {
		t.Errorf("abandon = %+v err = %v, want %q", abandon, abandonErr, want)
	}
}

func assertAccepted(t *testing.T, paymentsURL string, want FailureProof) {
	t.Helper()
	proof, proofErr, abandon, abandonErr := askBoth(t, paymentsURL)
	if proofErr != nil || proof != want {
		t.Errorf("proof = %+v err = %v, want %+v", proof, proofErr, want)
	}
	if abandonErr != nil || abandon != want {
		t.Errorf("abandon = %+v err = %v, want %+v", abandon, abandonErr, want)
	}
}

const invalidEvidence = "payments returned invalid refund evidence"

var (
	// ref255 is the longest reference payments returns: 255 bytes.
	ref255 = "re_" + strings.Repeat("x", 255-3)
	// ref255multibyte is 255 bytes in 129 characters: the limit counts bytes.
	ref255multibyte = "re_" + strings.Repeat("é", 126)
)

func TestEvidenceAcceptsWhatPaymentsWrites(t *testing.T) {
	for _, c := range []struct {
		name string
		body string
		want FailureProof
	}{
		{"control", validEvidence, FailureProof{"re_failed", 2000, "EUR"}},
		{"members in another order, with white space", " {\"currency\" : \"EUR\",\n\"amount\":2000, \"provider_ref\":\"re_failed\"}\n", FailureProof{"re_failed", 2000, "EUR"}},
		{"a 255-byte reference", fmt.Sprintf(`{"provider_ref":%q,"amount":2000,"currency":"EUR"}`, ref255), FailureProof{ref255, 2000, "EUR"}},
		{"a 255-byte reference in 129 characters", fmt.Sprintf(`{"provider_ref":%q,"amount":2000,"currency":"EUR"}`, ref255multibyte), FailureProof{ref255multibyte, 2000, "EUR"}},
		{"a one-character suffix", `{"provider_ref":"re_1","amount":2000,"currency":"EUR"}`, FailureProof{"re_1", 2000, "EUR"}},
		{"the smallest amount", `{"provider_ref":"re_failed","amount":1,"currency":"JPY"}`, FailureProof{"re_failed", 1, "JPY"}},
		{"the largest amount", `{"provider_ref":"re_failed","amount":9223372036854775807,"currency":"EUR"}`, FailureProof{"re_failed", 9223372036854775807, "EUR"}},
	} {
		t.Run(c.name, func(t *testing.T) { assertAccepted(t, answerWith(t, c.body), c.want) })
	}
}

func TestEvidenceRefusesAnAnswerPaymentsDoesNotWrite(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		// The object's shape. Each duplicate puts the valid value last, so a decoder that keeps
		// the last value would accept it.
		{"duplicate member", `{"provider_ref":"re_failed","amount":1999,"amount":2000,"currency":"EUR"}`},
		{"escaped duplicate member", `{"provider_ref":"re_failed","amount":1999,"amount":2000,"currency":"EUR"}`},
		{"case variant of a member", `{"provider_ref":"re_failed","Amount":2000,"currency":"EUR"}`},
		{"canonical member and its case variant", `{"provider_ref":"re_failed","amount":2000,"AMOUNT":1999,"currency":"EUR"}`},
		{"unknown member", `{"provider_ref":"re_failed","amount":2000,"currency":"EUR","completed":true}`},
		{"trailing object", validEvidence + `{}`},
		{"trailing value", validEvidence + ` 1`},
		{"not an object", `["re_failed",2000,"EUR"]`},
		{"not JSON", `proof`},
		{"empty body", ``},
		// Each member is required and is never null.
		{"no provider_ref", `{"amount":2000,"currency":"EUR"}`},
		{"no amount", `{"provider_ref":"re_failed","currency":"EUR"}`},
		{"no currency", `{"provider_ref":"re_failed","amount":2000}`},
		{"null provider_ref", `{"provider_ref":null,"amount":2000,"currency":"EUR"}`},
		{"null amount", `{"provider_ref":"re_failed","amount":null,"currency":"EUR"}`},
		{"null currency", `{"provider_ref":"re_failed","amount":2000,"currency":null}`},
		// Each member has its JSON type.
		{"amount as a string", `{"provider_ref":"re_failed","amount":"2000","currency":"EUR"}`},
		{"amount with a fraction", `{"provider_ref":"re_failed","amount":2000.5,"currency":"EUR"}`},
		{"amount beyond int64", `{"provider_ref":"re_failed","amount":9223372036854775808,"currency":"EUR"}`},
		{"provider_ref as a number", `{"provider_ref":1,"amount":2000,"currency":"EUR"}`},
		{"currency as a number", `{"provider_ref":"re_failed","amount":2000,"currency":978}`},
		// Each value is one payments can write.
		{"reference without the re_ prefix", `{"provider_ref":"pi_failed","amount":2000,"currency":"EUR"}`},
		{"reference with an uppercase prefix", `{"provider_ref":"RE_failed","amount":2000,"currency":"EUR"}`},
		{"bare re_ prefix", `{"provider_ref":"re_","amount":2000,"currency":"EUR"}`},
		{"a 256-byte reference", fmt.Sprintf(`{"provider_ref":%q,"amount":2000,"currency":"EUR"}`, ref255+"x")},
		{"a 256-byte reference in 130 characters", fmt.Sprintf(`{"provider_ref":%q,"amount":2000,"currency":"EUR"}`, ref255multibyte+"x")},
		{"zero amount", `{"provider_ref":"re_failed","amount":0,"currency":"EUR"}`},
		{"negative amount", `{"provider_ref":"re_failed","amount":-2000,"currency":"EUR"}`},
		{"lowercase currency", `{"provider_ref":"re_failed","amount":2000,"currency":"eur"}`},
		{"mixed-case currency", `{"provider_ref":"re_failed","amount":2000,"currency":"Eur"}`},
		{"two-letter currency", `{"provider_ref":"re_failed","amount":2000,"currency":"EU"}`},
		{"four-letter currency", `{"provider_ref":"re_failed","amount":2000,"currency":"EURO"}`},
		{"three-byte non-ASCII currency", `{"provider_ref":"re_failed","amount":2000,"currency":"ÉU"}`},
		{"currency with a digit", `{"provider_ref":"re_failed","amount":2000,"currency":"EU1"}`},
	} {
		t.Run(c.name, func(t *testing.T) { assertRefused(t, answerWith(t, c.body), invalidEvidence) })
	}
}

// paddedEvidence is the valid control padded with white space to exactly n bytes.
func paddedEvidence(n int) string {
	return validEvidence + strings.Repeat(" ", n-len(validEvidence))
}

// The response body is read up to 64 KiB. A body of exactly 64 KiB is read and decoded; one
// byte more is refused before the decoder sees it.
func TestEvidenceBodyCapBoundary(t *testing.T) {
	t.Run("exactly the cap", func(t *testing.T) {
		assertAccepted(t, answerWith(t, paddedEvidence(maxProofBody)), FailureProof{"re_failed", 2000, "EUR"})
	})
	t.Run("one byte over the cap", func(t *testing.T) {
		assertRefused(t, answerWith(t, paddedEvidence(maxProofBody+1)), "upstream response body too large: over 65536 bytes")
	})
}
