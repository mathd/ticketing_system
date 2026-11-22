package refundoutcome

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TKT-594 COS 1 and 2. A redirect is not an answer from payments. Neither client follows one:
// the redirect target gets no request and no credential, and the 3xx itself decides the class.
// The proof is unavailable, so nothing is recorded. The abandonment is uncertain, so Commerce
// keeps its reservation pending.
//
// The target is a second loopback server reached under ANOTHER host name, and it answers the
// evidence that the command would otherwise accept. Go forwards a custom header such as
// X-Internal-Token to another host, so a client that follows the redirect sends the payments
// credential to the target and takes its answer as payments' evidence.

const redirectTestToken = "payments-token-594"

// redirectTarget is the server a redirect points to. It records each request and its credential
// and answers 200 with a valid evidence body.
type redirectTarget struct {
	srv  *httptest.Server
	url  string
	mu   sync.Mutex
	seen []string
}

func newRedirectTarget(t *testing.T, evidence string) *redirectTarget {
	t.Helper()
	rt := &redirectTarget{}
	rt.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt.mu.Lock()
		rt.seen = append(rt.seen, fmt.Sprintf("%s %s token=%q", r.Method, r.URL.Path, r.Header.Get("X-Internal-Token")))
		rt.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(evidence))
	}))
	t.Cleanup(rt.srv.Close)
	// The origin is 127.0.0.1; the target is the same loopback listener under the name
	// localhost, so the redirect crosses to another host.
	u, err := url.Parse(rt.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = "localhost:" + u.Port()
	rt.url = u.String()
	return rt
}

func (rt *redirectTarget) requests() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]string(nil), rt.seen...)
}

// redirectingOrigin is the configured payments URL. It records what it receives and answers
// every request with `status`. A non-empty location is sent as the Location header.
type redirectingOrigin struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []*http.Request
}

func newRedirectingOrigin(t *testing.T, status int, location string) *redirectingOrigin {
	t.Helper()
	o := &redirectingOrigin{}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		o.seen = append(o.seen, r.Clone(context.Background()))
		o.mu.Unlock()
		if location != "" {
			w.Header().Set("Location", location+r.URL.RequestURI())
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(o.srv.Close)
	return o
}

func (o *redirectingOrigin) requests() []*http.Request {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]*http.Request(nil), o.seen...)
}

const validEvidence = `{"provider_ref":"re_failed","amount":2000,"currency":"EUR"}`

// proofClients are the two ways to build the proof client: the constructor, and a caller's
// struct literal with a plain http.Client (no redirect rule of its own).
func proofClients(paymentsURL string) map[string]HTTPPayments {
	return map[string]HTTPPayments{
		"constructor":     NewHTTPPayments(paymentsURL, redirectTestToken, 5*time.Second),
		"injected client": {Client: &http.Client{}, PaymentsURL: paymentsURL, PaymentsToken: redirectTestToken},
	}
}

func abandonClients(paymentsURL string) map[string]HTTPAbandoner {
	return map[string]HTTPAbandoner{
		"constructor":     NewHTTPAbandoner(paymentsURL, redirectTestToken, 5*time.Second),
		"injected client": {Client: &http.Client{}, PaymentsURL: paymentsURL, PaymentsToken: redirectTestToken},
	}
}

func TestProofClientRefusesARedirect(t *testing.T) {
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, name := range []string{"constructor", "injected client"} {
			t.Run(fmt.Sprintf("%d %s", code, name), func(t *testing.T) {
				target := newRedirectTarget(t, validEvidence)
				origin := newRedirectingOrigin(t, code, target.url)
				client := proofClients(origin.srv.URL)[name]
				org, refund := uuid.New(), uuid.NewString()
				got, err := client.ProveRefundFailure(context.Background(), org, "charge-key", refund)
				if seen := target.requests(); len(seen) != 0 {
					t.Errorf("the redirect target received %d request(s): %v; want none", len(seen), seen)
				}
				if !errors.Is(err, ErrProofUnavailable) || got != (FailureProof{}) {
					t.Errorf("proof = %+v err = %v, want ErrProofUnavailable and no proof", got, err)
				}
				want := fmt.Sprintf("refund failure proof unavailable: payments redirect refused (HTTP %d)", code)
				if err == nil || err.Error() != want {
					t.Errorf("err = %v, want %q", err, want)
				}
				reqs := origin.requests()
				if len(reqs) != 1 {
					t.Fatalf("the origin received %d requests, want 1", len(reqs))
				}
				r := reqs[0]
				q := r.URL.Query()
				if r.Method != http.MethodGet || r.URL.Path != "/internal/psp/partial-refund/failure-proof" ||
					r.Header.Get("X-Internal-Token") != redirectTestToken ||
					q.Get("organizer_id") != org.String() || q.Get("idempotency_key") != "charge-key" || q.Get("refund_key") != refund {
					t.Errorf("origin request = %s %s %v token=%q", r.Method, r.URL.Path, q, r.Header.Get("X-Internal-Token"))
				}
			})
		}
	}
}

func TestAbandonClientRefusesARedirect(t *testing.T) {
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, name := range []string{"constructor", "injected client"} {
			t.Run(fmt.Sprintf("%d %s", code, name), func(t *testing.T) {
				target := newRedirectTarget(t, validEvidence)
				origin := newRedirectingOrigin(t, code, target.url)
				client := abandonClients(origin.srv.URL)[name]
				org, refund := uuid.New(), uuid.NewString()
				got, err := client.AbandonRefundLeg(context.Background(), org, "charge-key", refund, 2000, "EUR")
				if seen := target.requests(); len(seen) != 0 {
					t.Errorf("the redirect target received %d request(s): %v; want none", len(seen), seen)
				}
				if !errors.Is(err, ErrAbandonUncertain) || got != (FailureProof{}) {
					t.Errorf("abandon = %+v err = %v, want ErrAbandonUncertain and no evidence", got, err)
				}
				want := fmt.Sprintf("payments abandonment answer unknown: payments redirect refused (HTTP %d)", code)
				if err == nil || err.Error() != want {
					t.Errorf("err = %v, want %q", err, want)
				}
				reqs := origin.requests()
				if len(reqs) != 1 {
					t.Fatalf("the origin received %d requests, want 1", len(reqs))
				}
				r := reqs[0]
				if r.Method != http.MethodPost || r.URL.Path != "/internal/psp/partial-refund/abandon" ||
					r.Header.Get("X-Internal-Token") != redirectTestToken {
					t.Errorf("origin request = %s %s token=%q", r.Method, r.URL.Path, r.Header.Get("X-Internal-Token"))
				}
			})
		}
	}
}

// A 3xx with no Location is not followed by any client. It is still not an answer: the same
// refusal names its status.
func TestEvidenceClientsRefuseA3xxWithoutLocation(t *testing.T) {
	for _, code := range []int{http.StatusMultipleChoices, http.StatusFound, http.StatusNotModified, http.StatusTemporaryRedirect} {
		origin := newRedirectingOrigin(t, code, "")
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			_, err := NewHTTPPayments(origin.srv.URL, redirectTestToken, 5*time.Second).ProveRefundFailure(context.Background(), uuid.New(), "k", uuid.NewString())
			if want := fmt.Sprintf("refund failure proof unavailable: payments redirect refused (HTTP %d)", code); err == nil || err.Error() != want || !errors.Is(err, ErrProofUnavailable) {
				t.Errorf("proof: err = %v, want %q", err, want)
			}
			_, err = NewHTTPAbandoner(origin.srv.URL, redirectTestToken, 5*time.Second).AbandonRefundLeg(context.Background(), uuid.New(), "k", uuid.NewString(), 2000, "EUR")
			if want := fmt.Sprintf("payments abandonment answer unknown: payments redirect refused (HTTP %d)", code); err == nil || err.Error() != want || !errors.Is(err, ErrAbandonUncertain) {
				t.Errorf("abandon: err = %v, want %q", err, want)
			}
		})
	}
}

// A Location that Go cannot parse fails the request inside the HTTP client, before any redirect
// rule runs. It takes the same transport path: unavailable and uncertain.
func TestEvidenceClientsClassifyAnUnparsableLocation(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://[::1")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(origin.Close)
	if _, err := NewHTTPPayments(origin.URL, "t", 5*time.Second).ProveRefundFailure(context.Background(), uuid.New(), "k", uuid.NewString()); !errors.Is(err, ErrProofUnavailable) ||
		!strings.Contains(err.Error(), "Location") {
		t.Errorf("proof: err = %v, want ErrProofUnavailable from the unparsable Location", err)
	}
	if _, err := NewHTTPAbandoner(origin.URL, "t", 5*time.Second).AbandonRefundLeg(context.Background(), uuid.New(), "k", uuid.NewString(), 2000, "EUR"); !errors.Is(err, ErrAbandonUncertain) ||
		!strings.Contains(err.Error(), "Location") {
		t.Errorf("abandon: err = %v, want ErrAbandonUncertain from the unparsable Location", err)
	}
}
