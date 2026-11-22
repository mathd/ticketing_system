package api

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TKT-498 (R2-2, RA2-1). Both Access admission reads — the exchange's pre-money check here and
// the replacement refund guard (admission_wiring_smoke_test.go) — use the Access caller that
// New builds: it sends the shared internal token, named at the call site, and never follows a
// redirect. These tests build the Server through New, as cmd/commerce does.

const wiringPaymentsTok = "payments-credential-for-wiring-0123456789"

// recordingServer counts the requests it receives and the X-Internal-Token each carried, and
// answers every request with `body`.
type recordingServer struct {
	*httptest.Server
	mu     sync.Mutex
	tokens []string
}

func newRecordingServer(t *testing.T, body string) *recordingServer {
	t.Helper()
	rs := &recordingServer{}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		rs.tokens = append(rs.tokens, r.Header.Get("X-Internal-Token"))
		rs.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(rs.Close)
	return rs
}

func (rs *recordingServer) received() []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]string(nil), rs.tokens...)
}

// redirectingAccess answers every request with `status` and a Location on `target`.
func redirectingAccess(t *testing.T, status int, target string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+r.URL.RequestURI(), status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wiredServer(accessURL, paymentsURL string) *Server {
	return New(ServerConfig{
		Client:        &http.Client{Timeout: 5 * time.Second},
		AccessURL:     accessURL,
		PaymentsURL:   paymentsURL,
		InventoryURL:  "http://inventory.invalid",
		InternalToken: internalTok,
		PaymentsToken: wiringPaymentsTok,
	})
}

func preMoneyCheck(s *Server, quantity int32) (bool, int) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/orders/x/exchanges", nil)
	ok := s.checkSourceAdmission(rec, req, uuid.New(), uuid.New(), quantity)
	return ok, rec.Code
}

// A redirect from Access is an unresolved answer: the redirect target receives no request (and
// so no credential), and the check answers the existing 502. Without the no-redirect rule the
// target's well-formed "not admitted, fully issued" answer would pass the check.
func TestPreMoneyAdmissionReadNeverFollowsARedirect(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			target := newRecordingServer(t, `{"admitted":false,"issued_count":2}`)
			access := redirectingAccess(t, status, target.URL)
			ok, code := preMoneyCheck(wiredServer(access.URL, "http://payments.invalid"), 2)
			if ok || code != http.StatusBadGateway {
				t.Fatalf("checkSourceAdmission after a %d = %v/%d, want refused with 502", status, ok, code)
			}
			if got := target.received(); len(got) != 0 {
				t.Fatalf("the redirect target received %d request(s) with tokens %q, want 0", len(got), got)
			}
		})
	}
}

// Access and payments configured under one origin, so the Access URL starts with the payments
// URL: Access still receives the internal token, never the payments credential.
func TestPreMoneyAdmissionReadSendsTheInternalTokenUnderASharedPrefix(t *testing.T) {
	shared := newRecordingServer(t, `{"admitted":false,"issued_count":2}`)
	ok, code := preMoneyCheck(wiredServer(shared.URL+"/access", shared.URL), 2)
	if !ok {
		t.Fatalf("checkSourceAdmission = refused with %d, want passed", code)
	}
	if got := shared.received(); len(got) != 1 || got[0] != internalTok {
		t.Fatalf("Access received tokens %q, want exactly [%q] (never the payments credential)", got, internalTok)
	}
}

// The check keeps its own status mapping on the new caller: 503 for incomplete issuance, 409
// for an admitted source, 502 for an answer that is not a well-formed summary.
func TestPreMoneyAdmissionStatusMappingIsPreserved(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"not yet issued", `{"admitted":false,"issued_count":1}`, http.StatusServiceUnavailable},
		{"admitted", `{"admitted":true,"issued_count":2}`, http.StatusConflict},
		{"malformed", `{"admitted":false}`, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			access := newRecordingServer(t, tc.body)
			if ok, code := preMoneyCheck(wiredServer(access.URL, "http://payments.invalid"), 2); ok || code != tc.want {
				t.Fatalf("checkSourceAdmission = %v/%d, want refused with %d", ok, code, tc.want)
			}
		})
	}
}
