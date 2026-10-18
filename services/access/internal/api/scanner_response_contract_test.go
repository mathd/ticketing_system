package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"

	apispec "ticketing/services/access/api"
	"ticketing/services/access/internal/store"
	"ticketing/shared/contract"
)

// TKT-557 (ADR-085 §8 ticket 0): the exact Access response contract on the
// three scanner routes, outer recovery, and explicit response validation for
// the answers that bypass the ordinary response validator.
//
// These stubs prove ROUTING and MAPPING: which status and body a lookup
// outcome produces, and that nothing past a failed lookup runs. They do not
// prove SQL revocation or driver classification; the store smoke tests and
// the Router smoke tests prove those against real PostgreSQL.

// lookupStub is the device port with per-token outcomes and call counts.
type lookupStub struct {
	mu        sync.Mutex
	outcomes  map[string]error
	device    store.ScannerDevice
	lookups   int
	touches   int
	onLookup  func(token string)
	panicWith any
}

func (l *lookupStub) AuthenticateScannerDevice(_ context.Context, token string) (store.ScannerDevice, error) {
	l.mu.Lock()
	l.lookups++
	outcome, known := l.outcomes[token]
	hook := l.onLookup
	l.mu.Unlock()
	if hook != nil {
		hook(token)
	}
	if l.panicWith != nil {
		panic(l.panicWith)
	}
	if !known {
		return store.ScannerDevice{}, store.ErrScannerDeviceUnknown
	}
	if outcome != nil {
		return store.ScannerDevice{}, outcome
	}
	return l.device, nil
}

func (l *lookupStub) TouchScannerDevice(context.Context, uuid.UUID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.touches++
}

func (l *lookupStub) counts() (lookups, touches int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lookups, l.touches
}

const (
	tokenOK          = "enrolled-and-live"
	tokenUnavailable = "lookup-unavailable"
	tokenFailed      = "lookup-failed"
	tokenRevoked     = "revoked-device"
)

// availabilityError is the store's named type, as the real store makes it.
func availabilityError() error {
	return &store.PreDecisionUnavailable{Site: store.SiteDeviceLookup, Class: store.ClassConnectRefused, Cause: errors.New("dial tcp: connect: connection refused")}
}

type scannerFixture struct {
	organizer  uuid.UUID
	payload    string
	devices    *lookupStub
	admissions *countingAdmissions
	server     *Server
}

func newScannerFixture(t *testing.T) *scannerFixture {
	t.Helper()
	organizer := uuid.New()
	verifier, payload := signedFor(t, organizer)
	devices := &lookupStub{
		outcomes: map[string]error{
			tokenOK:          nil,
			tokenUnavailable: availabilityError(),
			tokenFailed:      errors.New("authenticate scanner device: unexpected driver failure"),
			// A revoked device is the same refusal as an unknown one; the SQL
			// that makes it so is asserted against PostgreSQL in the store tests.
			tokenRevoked: store.ErrScannerDeviceUnknown,
		},
		device: store.ScannerDevice{ID: uuid.New(), OrganizerID: organizer, Label: "gate"},
	}
	admissions := &countingAdmissions{}
	server := newTestServer(nil, verifier).WithScannerDevices(devices).WithAdmissions(admissions).WithFeedCursorKey("test-feed-cursor-key")
	return &scannerFixture{organizer: organizer, payload: payload, devices: devices, admissions: admissions, server: server}
}

// scannerRoute is one of the three protected routes, with a VALID request
// shape: a successful authentication could reach its decision.
type scannerRoute struct {
	name, method, target, operationID string
	body                              func(payload string) string
}

var scannerRoutes = []scannerRoute{
	{"S", http.MethodPost, "/scans", "scanTicket", func(p string) string { return `{"qr_payload":` + quote(p) + `}` }},
	{"R", http.MethodPost, "/scans/reconciliations", "reconcileScans", func(p string) string {
		return `{"occurrences":[{"qr_payload":` + quote(p) + `,"occurrence_id":"6f3f5d18-0b41-4a2a-9c07-2f5f0f5f1a11","occurred_at":"2026-08-10T10:00:00Z"}]}`
	}},
	// A cursor the handler itself refuses: a success control that reaches the
	// feed handler answers its own 400, with no store needed.
	{"F", http.MethodGet, "/scans/voided-tickets?cursor=not-a-real-cursor", "listVoidedTickets", nil},
}

func quote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func (r scannerRoute) request(payload, token string, setToken bool) *http.Request {
	var body io.Reader
	if r.body != nil {
		body = strings.NewReader(r.body(payload))
	}
	req := httptest.NewRequest(r.method, r.target, body)
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if setToken {
		req.Header.Set(scannerDeviceHeader, token)
	}
	return req
}

// requireExactJSON asserts status, the JSON and no-store headers, and the exact
// decoded body (key set and values).
func requireExactJSON(t *testing.T, rec *httptest.ResponseRecorder, status int, body map[string]any) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, status, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("body is not one JSON object: %v; body=%s", err, rec.Body.String())
	}
	if !reflect.DeepEqual(decoded, body) {
		t.Errorf("body = %v (keys %v), want exactly %v", decoded, sortedBodyKeys(decoded), body)
	}
}

func sortedBodyKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// validateCaptured runs the explicit response validation that the request
// hook and outer recovery bypass: the captured status, headers and body
// against the resolved operation, with IncludeResponseStatus, so an
// undeclared status is a failure.
func validateCaptured(t *testing.T, spec []byte, method, target string, rec *httptest.ResponseRecorder) error {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatal(err)
	}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, target, nil)
	route, params, err := router.FindRoute(req)
	if err != nil {
		t.Fatalf("resolve %s %s: %v", method, target, err)
	}
	return openapi3filter.ValidateResponse(context.Background(), &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
		Status:                 rec.Code,
		Header:                 rec.Header(),
		Body:                   io.NopCloser(bytes.NewReader(rec.Body.Bytes())),
		Options:                &openapi3filter.Options{IncludeResponseStatus: true},
	})
}

// --- COS-1: exact three-route authentication ---------------------------------

func TestScannerRoutesAnswerTheExactLookupContract(t *testing.T) {
	notEnrolled := map[string]any{"error": "scanner device is not enrolled"}
	cases := []struct {
		name     string
		token    string
		setToken bool
		build    func(f *scannerFixture) *Server
		status   int
		body     map[string]any
		lookups  int
	}{
		{name: "absent", setToken: false, status: 401, body: notEnrolled, lookups: 1},
		{name: "whitespace", token: " ", setToken: true, status: 401, body: notEnrolled, lookups: 1},
		{name: "unknown", token: "never-enrolled", setToken: true, status: 401, body: notEnrolled, lookups: 1},
		{name: "revoked", token: tokenRevoked, setToken: true, status: 401, body: notEnrolled, lookups: 1},
		{name: "nil device port", token: tokenOK, setToken: true, status: 401, body: notEnrolled, lookups: 0,
			build: func(f *scannerFixture) *Server { f.server.devices = nil; return f.server }},
		{name: "unknown scheme", token: tokenOK, setToken: true, status: 401, body: notEnrolled, lookups: 0,
			build: func(f *scannerFixture) *Server { return f.server.withSpec(renamedSchemeSpec(t)) }},
		{name: "typed availability", token: tokenUnavailable, setToken: true, status: 503, body: map[string]any{"error": "scanner device lookup unavailable"}, lookups: 1},
		{name: "ordinary error", token: tokenFailed, setToken: true, status: 500, body: map[string]any{"error": "scanner device lookup failed"}, lookups: 1},
	}
	for _, route := range scannerRoutes {
		for _, tc := range cases {
			t.Run(route.name+" "+tc.name, func(t *testing.T) {
				f := newScannerFixture(t)
				server := f.server
				if tc.build != nil {
					server = tc.build(f)
				}
				rec := httptest.NewRecorder()
				server.Router(nil, true).ServeHTTP(rec, route.request(f.payload, tc.token, tc.setToken))
				requireExactJSON(t, rec, tc.status, tc.body)
				lookups, touches := f.devices.counts()
				if lookups != tc.lookups {
					t.Errorf("lookups = %d, want %d", lookups, tc.lookups)
				}
				if touches != 0 {
					t.Errorf("a failed lookup touched the device %d times", touches)
				}
				if scans, reconciles, refusals := f.admissions.counts(); scans+reconciles+refusals != 0 {
					t.Errorf("a failed lookup reached the admission port: %d/%d/%d", scans, reconciles, refusals)
				}
				if tc.status == 503 || tc.status == 500 {
					if err := validateCaptured(t, apispec.Spec, route.method, route.target, rec); err != nil {
						t.Errorf("%s capture violates the declared %s responses: %v", route.name, route.operationID, err)
					}
				}
			})
		}
		// The success control: one lookup, one Touch, and the intended
		// operation runs.
		t.Run(route.name+" success", func(t *testing.T) {
			f := newScannerFixture(t)
			rec := httptest.NewRecorder()
			f.server.Router(nil, true).ServeHTTP(rec, route.request(f.payload, tokenOK, true))
			lookups, touches := f.devices.counts()
			if lookups != 1 || touches != 1 {
				t.Fatalf("lookups=%d touches=%d, want one of each", lookups, touches)
			}
			scans, reconciles, _ := f.admissions.counts()
			switch route.name {
			case "S":
				requireExactJSON(t, rec, 200, map[string]any{"decision": "accepted", "scanned_at": "2026-08-10T10:00:00Z"})
				if scans != 1 {
					t.Fatalf("scans = %d, want 1", scans)
				}
			case "R":
				if rec.Code != 200 || reconciles != 1 {
					t.Fatalf("reconcile = %d with %d calls; body=%s", rec.Code, reconciles, rec.Body.String())
				}
			case "F":
				requireExactJSON(t, rec, 400, map[string]any{"error": "invalid cursor"})
			}
		})
	}
}

// A Touch failure is advisory: the stub's Touch cannot fail, so this asserts
// the narrower thing a stub CAN show — Touch runs after a successful lookup,
// once, and a lookup class is set only by the lookup.
func TestTouchRunsOnlyAfterASuccessfulLookup(t *testing.T) {
	f := newScannerFixture(t)
	var touchesAtLookup int
	f.devices.onLookup = func(string) { _, touchesAtLookup = f.devices.counts() }
	rec := httptest.NewRecorder()
	f.server.Router(nil, true).ServeHTTP(rec, scannerRoutes[0].request(f.payload, tokenUnavailable, true))
	if _, touches := f.devices.counts(); touches != 0 || touchesAtLookup != 0 {
		t.Fatalf("Touch ran around an unavailable lookup: %d", touches)
	}
}

// One slot per request: a lookup class from one request never leaks into the
// next. Sequential, so the order is forced: an unavailable lookup, then an
// unknown device on the same router, must still answer 401.
func TestLookupClassNeverLeaksIntoTheNextRequest(t *testing.T) {
	f := newScannerFixture(t)
	router := f.server.Router(nil, true)
	first := httptest.NewRecorder()
	router.ServeHTTP(first, scannerRoutes[0].request(f.payload, tokenUnavailable, true))
	requireExactJSON(t, first, 503, map[string]any{"error": "scanner device lookup unavailable"})
	second := httptest.NewRecorder()
	router.ServeHTTP(second, scannerRoutes[0].request(f.payload, "never-enrolled", true))
	requireExactJSON(t, second, 401, map[string]any{"error": "scanner device is not enrolled"})
}

// Two concurrent requests with distinct lookup results, both inside the
// lookup at once (a barrier), each get their own answer.
func TestConcurrentRequestsKeepTheirOwnLookupClass(t *testing.T) {
	f := newScannerFixture(t)
	var arrived sync.WaitGroup
	arrived.Add(2)
	f.devices.onLookup = func(string) {
		arrived.Done()
		arrived.Wait()
	}
	router := f.server.Router(nil, true)
	var wg sync.WaitGroup
	recs := map[string]*httptest.ResponseRecorder{tokenUnavailable: httptest.NewRecorder(), tokenOK: httptest.NewRecorder()}
	for token, rec := range recs {
		wg.Add(1)
		go func(token string, rec *httptest.ResponseRecorder) {
			defer wg.Done()
			router.ServeHTTP(rec, scannerRoutes[0].request(f.payload, token, true))
		}(token, rec)
	}
	wg.Wait()
	requireExactJSON(t, recs[tokenUnavailable], 503, map[string]any{"error": "scanner device lookup unavailable"})
	requireExactJSON(t, recs[tokenOK], 200, map[string]any{"decision": "accepted", "scanned_at": "2026-08-10T10:00:00Z"})
}

// renamedSchemeSpec is an isolated clone of the embedded contract with the
// security scheme and all three references renamed (D9). The global spec is
// not touched.
func renamedSchemeSpec(t *testing.T) []byte {
	t.Helper()
	before := append([]byte(nil), apispec.Spec...)
	clone := bytes.ReplaceAll(append([]byte(nil), apispec.Spec...), []byte("ScannerDeviceToken"), []byte("RenamedDeviceToken"))
	if !bytes.Equal(apispec.Spec, before) {
		t.Fatal("cloning mutated the embedded spec")
	}
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(clone)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("the cloned spec does not validate: %v", err)
	}
	if _, ok := doc.Components.SecuritySchemes["RenamedDeviceToken"]; !ok {
		t.Fatal("the clone has no renamed scheme")
	}
	operations := 0
	for _, item := range doc.Paths.Map() {
		for _, op := range item.Operations() {
			if op.Security == nil {
				continue
			}
			for _, req := range *op.Security {
				if _, ok := req["RenamedDeviceToken"]; ok {
					operations++
				}
				if _, ok := req["ScannerDeviceToken"]; ok {
					t.Fatalf("operation %s still requires the original scheme", op.OperationID)
				}
			}
		}
	}
	if operations != 3 {
		t.Fatalf("%d operations carry the renamed requirement, want the three scanner routes", operations)
	}
	return clone
}

// The default Router enforces the committed embedded spec: the same token
// that the renamed-scheme server refuses is accepted here.
func TestDefaultRouterUsesTheEmbeddedSpec(t *testing.T) {
	f := newScannerFixture(t)
	if f.server.spec != nil {
		t.Fatal("a test fixture server carries a contract override by default")
	}
	rec := httptest.NewRecorder()
	f.server.Router(nil, true).ServeHTTP(rec, scannerRoutes[0].request(f.payload, tokenOK, true))
	if rec.Code != http.StatusOK {
		t.Fatalf("default Router = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// --- D16: a missing admission port is a configuration 500, never 503 --------

func TestMissingAdmissionPortIsAnExact500WhenNeeded(t *testing.T) {
	for name, build := range map[string]func(t *testing.T, f *scannerFixture) *Server{
		"nil": func(t *testing.T, f *scannerFixture) *Server {
			return f.server.WithAdmissions(nil)
		},
		"typed nil store": func(t *testing.T, f *scannerFixture) *Server {
			s := New(ServerConfig{Store: (*store.Postgres)(nil), Verifier: f.server.verifier})
			return s.WithScannerDevices(f.devices)
		},
	} {
		for _, tc := range []struct {
			route  string
			target string
			body   func(p string) string
			want   map[string]any
		}{
			{"S", "/scans", scannerRoutes[0].body, map[string]any{"error": "redeem ticket"}},
			{"R normal", "/scans/reconciliations", scannerRoutes[1].body, map[string]any{"error": "reconcile admissions"}},
			{"R refusal", "/scans/reconciliations", func(p string) string {
				return `{"occurrences":[{"qr_payload":` + quote(p) + `,"occurrence_id":"6f3f5d18-0b41-4a2a-9c07-2f5f0f5f1a11","occurred_at":"2026-08-10T10:00:00Z","local_decision":"revocation_refused"}]}`
			}, map[string]any{"error": "record scanner refusal"}},
		} {
			t.Run(name+" "+tc.route, func(t *testing.T) {
				f := newScannerFixture(t)
				server := build(t, f)
				req := httptest.NewRequest(http.MethodPost, tc.target, strings.NewReader(tc.body(f.payload)))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set(scannerDeviceHeader, tokenOK)
				rec := httptest.NewRecorder()
				server.Router(nil, true).ServeHTTP(rec, req)
				requireExactJSON(t, rec, 500, tc.want)
			})
		}
	}
	// Verifier first: without a verifier the existing 503 still answers before
	// the port is needed.
	f := newScannerFixture(t)
	f.server.verifier = nil
	rec := httptest.NewRecorder()
	f.server.WithAdmissions(nil).Router(nil, true).ServeHTTP(rec, scannerRoutes[0].request(f.payload, tokenOK, true))
	requireExactJSON(t, rec, 503, map[string]any{"error": "scanner unavailable"})
}

// --- COS-5: the feed's 503 declaration ---------------------------------------

// The hook's 503 on the feed never meets the ordinary response validator, so
// only an explicit validation against the resolved operation can show that
// the status is declared. Remove the feed's responses.503 and this fails.
func TestFeedLookupUnavailableIsADeclaredResponse(t *testing.T) {
	f := newScannerFixture(t)
	rec := httptest.NewRecorder()
	f.server.Router(nil, true).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/scans/voided-tickets", nil).WithContext(context.Background()))
	// No token: refusal first, to prove the route is the feed.
	requireExactJSON(t, rec, 401, map[string]any{"error": "scanner device is not enrolled"})

	req := httptest.NewRequest(http.MethodGet, "/scans/voided-tickets", nil)
	req.Header.Set(scannerDeviceHeader, tokenUnavailable)
	rec = httptest.NewRecorder()
	f.server.Router(nil, true).ServeHTTP(rec, req)
	requireExactJSON(t, rec, 503, map[string]any{"error": "scanner device lookup unavailable"})
	if err := validateCaptured(t, apispec.Spec, http.MethodGet, "/scans/voided-tickets", rec); err != nil {
		t.Fatalf("the feed's 503 is not a declared listVoidedTickets response: %v", err)
	}
}

// --- COS-3: recovery ---------------------------------------------------------

// panickingAdmissions panics inside the admission port with a token-bearing
// value, which must never reach the log.
type panickingAdmissions struct{ value any }

func (p panickingAdmissions) Scan(context.Context, store.ScanInput) (store.RedeemResult, error) {
	panic(p.value)
}

func (p panickingAdmissions) ReconcileAdmission(context.Context, store.ReconcileOccurrence) (store.ReconcileResult, error) {
	panic(p.value)
}

func (p panickingAdmissions) RecordRevocationRefusal(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, time.Time, string) (store.ReconcileResult, error) {
	panic(p.value)
}

type panickingRedeliveries struct{ value any }

func (p panickingRedeliveries) ClaimRedelivery(context.Context, uuid.UUID, uuid.UUID, string) (store.RedeliveryClaim, error) {
	panic(p.value)
}

func (p panickingRedeliveries) MarkRedelivered(context.Context, uuid.UUID, string, uuid.UUID, uuid.UUID) error {
	panic(p.value)
}

// logSink captures the structured log as JSON records.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *logSink) records(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(s.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		out = append(out, rec)
	}
	return out
}

// recoveryCase is one operation driven to a panic with valid, authorized
// input that reaches the intended handler.
type recoveryCase struct {
	name, method, target string
	request              func(t *testing.T) *http.Request
	server               func(t *testing.T, secret string) *Server
}

const recoveryInternalToken = "internal-token-for-recovery-tests-32b"

func recoveryCases(t *testing.T) []recoveryCase {
	t.Helper()
	ref, ticketID, order := uuid.New(), uuid.New(), uuid.New()
	organizer := uuid.New()
	verifier, payload := signedFor(t, organizer)
	enrolledDevice := func(secret string) *lookupStub {
		return &lookupStub{outcomes: map[string]error{tokenOK: nil}, device: store.ScannerDevice{ID: uuid.New(), OrganizerID: organizer}}
	}
	scannerServer := func(t *testing.T, secret string) *Server {
		return newTestServer(nil, verifier).WithScannerDevices(enrolledDevice(secret)).
			WithAdmissions(panickingAdmissions{value: "panic carrying " + secret})
	}
	jsonReq := func(method, target, body string, headers map[string]string) func(t *testing.T) *http.Request {
		return func(t *testing.T) *http.Request {
			var r io.Reader
			if body != "" {
				r = strings.NewReader(body)
			}
			req := httptest.NewRequest(method, target, r)
			if body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			return req
		}
	}
	qrServer := newTestServer(nil, nil).WithQRLinkKey("qr-link-key-for-recovery-tests-32by")
	qrTarget := "/orders/" + ref.String() + "/tickets/" + ticketID.String() + "/qr.png" + qrServer.qrLinks.mint(ref, ticketID, time.Now())
	return []recoveryCase{
		{name: "scanTicket", method: http.MethodPost, target: "/scans", server: scannerServer,
			request: jsonReq(http.MethodPost, "/scans", scannerRoutes[0].body(payload), map[string]string{scannerDeviceHeader: tokenOK})},
		{name: "reconcileScans", method: http.MethodPost, target: "/scans/reconciliations", server: scannerServer,
			request: jsonReq(http.MethodPost, "/scans/reconciliations", scannerRoutes[1].body(payload), map[string]string{scannerDeviceHeader: tokenOK})},
		{name: "listVoidedTickets hook", method: http.MethodGet, target: "/scans/voided-tickets", server: func(t *testing.T, secret string) *Server {
			return newTestServer(nil, verifier).WithScannerDevices(&lookupStub{panicWith: "panic carrying " + secret})
		}, request: jsonReq(http.MethodGet, "/scans/voided-tickets", "", map[string]string{scannerDeviceHeader: tokenOK})},
		{name: "getTickets", method: http.MethodGet, target: "/orders/" + ref.String() + "/tickets", server: func(t *testing.T, secret string) *Server {
			return newTestServer(nil, nil)
		}, request: jsonReq(http.MethodGet, "/orders/"+ref.String()+"/tickets", "", nil)},
		{name: "getTicketQR", method: http.MethodGet, target: qrTarget, server: func(t *testing.T, secret string) *Server {
			return newTestServer(nil, nil).WithQRLinkKey("qr-link-key-for-recovery-tests-32by")
		}, request: jsonReq(http.MethodGet, qrTarget, "", nil)},
		{name: "refundTickets", method: http.MethodPost, target: "/internal/orders/" + order.String() + "/refunds", server: func(t *testing.T, secret string) *Server {
			return newTestServer(nil, nil, recoveryInternalToken)
		}, request: jsonReq(http.MethodPost, "/internal/orders/"+order.String()+"/refunds",
			`{"organizer_id":"`+organizer.String()+`","refund_id":"`+uuid.NewString()+`","quantity":1}`, map[string]string{"X-Internal-Token": recoveryInternalToken})},
		{name: "getOrderAdmission", method: http.MethodGet, target: "/internal/orders/" + order.String() + "/admission?organizer_id=" + organizer.String(), server: func(t *testing.T, secret string) *Server {
			return newTestServer(nil, nil, recoveryInternalToken)
		}, request: jsonReq(http.MethodGet, "/internal/orders/"+order.String()+"/admission?organizer_id="+organizer.String(), "", map[string]string{"X-Internal-Token": recoveryInternalToken})},
		{name: "redeliverOrderTickets", method: http.MethodPost, target: "/internal/orders/" + order.String() + "/redeliveries", server: func(t *testing.T, secret string) *Server {
			return newTestServer(nil, nil, recoveryInternalToken).WithRedeliveryStore(panickingRedeliveries{value: "panic carrying " + secret}).
				WithRedelivery(constructorAddressBook{}, constructorMailer{}, "https://tickets.example.test")
		}, request: jsonReq(http.MethodPost, "/internal/orders/"+order.String()+"/redeliveries",
			`{"organizer_id":"`+organizer.String()+`"}`, map[string]string{"X-Internal-Token": recoveryInternalToken, "Idempotency-Key": "recovery-probe"})},
	}
}

// Every operation, before headers, with validation on and off: the exact
// route-neutral 500, the JSON and no-store headers, one fixed-message log
// record with a stack, no secret, and a capture that passes explicit
// response validation against the operation.
func TestEveryOperationRecoversAPanicBeforeHeaders(t *testing.T) {
	for _, validate := range []bool{true, false} {
		for _, tc := range recoveryCases(t) {
			t.Run(fmt.Sprintf("%s validate=%v", tc.name, validate), func(t *testing.T) {
				secret := "scanner-token-" + uuid.NewString()
				sink := &logSink{}
				log := slog.New(slog.NewJSONHandler(sink, nil))
				req := tc.request(t)
				rec := httptest.NewRecorder()
				tc.server(t, secret).Router(log, validate).ServeHTTP(rec, req)
				requireExactJSON(t, rec, 500, map[string]any{"error": "request failed"})
				if err := validateCaptured(t, apispec.Spec, tc.method, tc.target, rec); err != nil {
					t.Errorf("the recovered 500 violates the declared %s responses: %v", tc.name, err)
				}
				records := sink.records(t)
				panics := 0
				for _, r := range records {
					if r["msg"] == "access request panic" {
						panics++
						if stack, _ := r["stack"].(string); !strings.Contains(stack, "goroutine") || !strings.Contains(stack, "api.") {
							t.Errorf("the panic record carries no usable stack: %v", r["stack"])
						}
					}
				}
				if panics != 1 {
					t.Errorf("%d panic records, want exactly one", panics)
				}
				if strings.Contains(sink.String(), secret) {
					t.Error("the raw panic value (a token) reached the log")
				}
				if strings.Contains(sink.String(), "panic carrying") {
					t.Error("the raw panic value reached the log")
				}
			})
		}
	}
}

// The guest bundle's order reference is a capability: the panic log must not
// carry it either.
func TestRecoveryLogOmitsTheCapabilityPath(t *testing.T) {
	ref := uuid.New()
	sink := &logSink{}
	rec := httptest.NewRecorder()
	newTestServer(nil, nil).Router(slog.New(slog.NewJSONHandler(sink, nil)), true).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/orders/"+ref.String()+"/tickets", nil))
	requireExactJSON(t, rec, 500, map[string]any{"error": "request failed"})
	if strings.Contains(sink.String(), ref.String()) {
		t.Fatal("the capability order reference reached the panic log")
	}
}

// A handler that set entity headers and then panicked: recovery removes the
// stale ones before the fixed JSON.
func TestRecoveryClearsStaleEntityHeaders(t *testing.T) {
	handler := recoverRequests(nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "4096")
		w.Header().Set("ETag", `"abc"`)
		w.Header().Set("Content-Disposition", "inline")
		panic("before headers")
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	requireExactJSON(t, rec, 500, map[string]any{"error": "request failed"})
	for _, name := range []string{"Content-Length", "ETag", "Content-Disposition"} {
		if rec.Header().Get(name) != "" {
			t.Errorf("stale %s survived recovery: %q", name, rec.Header().Get(name))
		}
	}
}

// http.ErrAbortHandler is re-thrown, never turned into a 500.
func TestRecoveryRethrowsAbortHandler(t *testing.T) {
	handler := recoverRequests(nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	rec := httptest.NewRecorder()
	if got := servePanics(handler, rec); !errors.Is(asError(got), http.ErrAbortHandler) {
		t.Fatalf("recovered value = %v, want http.ErrAbortHandler re-thrown", got)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("an aborted handler got a body: %s", rec.Body.String())
	}
}

func asError(v any) error {
	if err, ok := v.(error); ok {
		return err
	}
	return nil
}

// servePanics serves one request and returns what escaped, if anything.
func servePanics(handler http.Handler, rec *httptest.ResponseRecorder) (escaped any) {
	defer func() { escaped = recover() }()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	return nil
}

// fullRecorder adds the optional writer paths a real connection has, so each
// one reaches its own httpsnoop hook: ReadFrom and FlushError.
type fullRecorder struct{ *httptest.ResponseRecorder }

func (f fullRecorder) ReadFrom(src io.Reader) (int64, error) {
	return io.Copy(struct{ io.Writer }{f.ResponseRecorder}, src)
}

func (f fullRecorder) FlushError() error {
	f.Flush()
	return nil
}

// hijackRecorder supports Hijack. After a hijack the response is the
// handler's connection, so recovery must not write to it.
type hijackRecorder struct {
	*httptest.ResponseRecorder
	hijacked, wroteAfterHijack bool
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	client, server := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, server) }()
	return client, bufio.NewReadWriter(bufio.NewReader(client), bufio.NewWriter(client)), nil
}

func (h *hijackRecorder) WriteHeader(code int) {
	if h.hijacked {
		h.wroteAfterHijack = true
		return
	}
	h.ResponseRecorder.WriteHeader(code)
}

func (h *hijackRecorder) Write(b []byte) (int, error) {
	if h.hijacked {
		h.wroteAfterHijack = true
		return 0, http.ErrHijacked
	}
	return h.ResponseRecorder.Write(b)
}

// After final headers recovery aborts: the original status and partial bytes
// stay, and no error JSON is appended. Each write path through the wrapper is
// its ONLY path to the network in its case, so each case exercises one hook.
func TestRecoveryAbortsAfterFinalHeaders(t *testing.T) {
	for name, write := range map[string]func(w http.ResponseWriter){
		"implicit Write":       func(w http.ResponseWriter) { _, _ = w.Write([]byte("partial")) },
		"explicit WriteHeader": func(w http.ResponseWriter) { w.WriteHeader(http.StatusAccepted) },
		"Flush":                func(w http.ResponseWriter) { w.(http.Flusher).Flush() },
		"FlushError":           func(w http.ResponseWriter) { _ = http.NewResponseController(w).Flush() },
		"WriteString":          func(w http.ResponseWriter) { _, _ = io.WriteString(w, "partial") },
		"ReadFrom": func(w http.ResponseWriter) {
			_, _ = w.(io.ReaderFrom).ReadFrom(strings.NewReader("partial"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			handler := recoverRequests(nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				write(w)
				panic("after headers")
			}))
			rec := httptest.NewRecorder()
			escaped := func() (escaped any) {
				defer func() { escaped = recover() }()
				handler.ServeHTTP(fullRecorder{rec}, httptest.NewRequest(http.MethodGet, "/", nil))
				return nil
			}()
			if !errors.Is(asError(escaped), http.ErrAbortHandler) {
				t.Fatalf("escaped = %v, want http.ErrAbortHandler", escaped)
			}
			if strings.Contains(rec.Body.String(), "request failed") {
				t.Fatalf("error JSON appended after headers: %q", rec.Body.String())
			}
			if rec.Code == http.StatusInternalServerError {
				t.Fatal("recovery claimed a 500 after the status was already sent")
			}
		})
	}
	// A hijacked connection is no longer replaceable either.
	t.Run("Hijack", func(t *testing.T) {
		rec := &hijackRecorder{ResponseRecorder: httptest.NewRecorder()}
		handler := recoverRequests(nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
			}
			defer func() { _ = conn.Close() }()
			panic("after hijack")
		}))
		escaped := func() (escaped any) {
			defer func() { escaped = recover() }()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			return nil
		}()
		if !errors.Is(asError(escaped), http.ErrAbortHandler) {
			t.Fatalf("escaped = %v, want http.ErrAbortHandler", escaped)
		}
		if rec.wroteAfterHijack {
			t.Fatal("recovery wrote a response to a hijacked connection")
		}
	})
	// 1xx informational headers are not final: a panic after one still gets
	// the clean 500.
	handler := recoverRequests(nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusEarlyHints)
		panic("after an informational header")
	}))
	rec := httptest.NewRecorder()
	if escaped := servePanics(handler, rec); escaped != nil {
		t.Fatalf("escaped = %v after a 1xx header", escaped)
	}
	if !strings.Contains(rec.Body.String(), "request failed") {
		t.Fatalf("no clean 500 after a 1xx header: %d %q", rec.Code, rec.Body.String())
	}
}

// Through the real Router with validation OFF, the handler writes straight
// through the wrapper. A panic during that write (here in the outer writer)
// is after headers: recovery aborts and keeps the original bytes.
func TestRouterWithoutValidationAbortsAPanicAfterHeaders(t *testing.T) {
	f := newScannerFixture(t)
	writer := &panicAfterWrite{ResponseRecorder: httptest.NewRecorder()}
	escaped := func() (escaped any) {
		defer func() { escaped = recover() }()
		f.server.Router(nil, false).ServeHTTP(writer, scannerRoutes[0].request(f.payload, tokenOK, true))
		return nil
	}()
	if !errors.Is(asError(escaped), http.ErrAbortHandler) {
		t.Fatalf("escaped = %v, want http.ErrAbortHandler", escaped)
	}
	if writer.Code != http.StatusOK || !strings.Contains(writer.Body.String(), `"decision":"accepted"`) || strings.Contains(writer.Body.String(), "request failed") {
		t.Fatalf("after-header state = %d %q, want the original 200 bytes only", writer.Code, writer.Body.String())
	}
}

// panicAfterWrite writes the bytes and then panics, as a broken transport
// layer would.
type panicAfterWrite struct{ *httptest.ResponseRecorder }

func (p *panicAfterWrite) Write(b []byte) (int, error) {
	n, err := p.ResponseRecorder.Write(b)
	if err == nil {
		panic("transport failed after the write")
	}
	return n, err
}

// The security hook is inside recovery: a panic in the device port is a
// clean 500 and nothing past it runs.
func TestSecurityHookPanicIsRecovered(t *testing.T) {
	for _, route := range scannerRoutes {
		t.Run(route.name, func(t *testing.T) {
			f := newScannerFixture(t)
			f.devices.panicWith = "hook panic"
			rec := httptest.NewRecorder()
			f.server.Router(nil, true).ServeHTTP(rec, route.request(f.payload, tokenOK, true))
			requireExactJSON(t, rec, 500, map[string]any{"error": "request failed"})
			if err := validateCaptured(t, apispec.Spec, route.method, route.target, rec); err != nil {
				t.Errorf("hook panic capture violates %s: %v", route.operationID, err)
			}
			if _, touches := f.devices.counts(); touches != 0 {
				t.Error("Touch ran after a hook panic")
			}
			if scans, reconciles, refusals := f.admissions.counts(); scans+reconciles+refusals != 0 {
				t.Error("the admission port ran after a hook panic")
			}
		})
	}
}

// --- response drift -------------------------------------------------------------

// scannerResponseDriftHandler emits scan responses that violate the contract:
// a 409 without its reason and a 200 with an undeclared field. It proves that
// the response validator SUBSTITUTES the 500; it proves no store commit.
func scannerResponseDriftHandler(status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch status {
		case http.StatusConflict:
			write(w, status, map[string]any{"decision": "rejected"})
		default:
			write(w, status, map[string]any{"decision": "accepted", "scanned_at": "2026-08-10T10:00:00Z", "undeclared": true})
		}
	})
}

func TestScanResponseDriftFailsClosed(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			handler, err := contract.ResponseValidator(apispec.Spec, scannerResponseDriftHandler(status), nil, true)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/scans", strings.NewReader(`{"qr_payload":"x"}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			requireExactJSON(t, rec, 500, map[string]any{"error": "response violates OpenAPI contract"})
		})
	}
}

// scriptedAdmissions answers every Scan with one fixed error.
type scriptedAdmissions struct {
	countingAdmissions
	err error
}

func (s *scriptedAdmissions) Scan(ctx context.Context, in store.ScanInput) (store.RedeemResult, error) {
	_, _ = s.countingAdmissions.Scan(ctx, in)
	return store.RedeemResult{}, s.err
}

// The scan handler's store-error mapping: credential and collision refusals
// first, then ONLY the named pre-decision type is 503; every other error,
// including a raw driver error of the same shape, is 500.
func TestScanStoreErrorMapping(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		status int
		body   map[string]any
	}{
		"policy availability": {&store.PreDecisionUnavailable{Site: store.SitePolicyRead, Class: store.ClassConnectStartup}, 503, map[string]any{"error": "scanner store unavailable"}},
		"begin expiry":        {&store.PreDecisionUnavailable{Site: store.SiteSingleBegin, Class: store.ClassStepExpired}, 503, map[string]any{"error": "scanner store unavailable"}},
		"raw error":           {errors.New("tx: commit failed"), 500, map[string]any{"error": "redeem ticket"}},
		"raw 57P01-like text": {errors.New("FATAL: terminating connection due to administrator command (SQLSTATE 57P01)"), 500, map[string]any{"error": "redeem ticket"}},
		"credential":          {store.ErrTicketCredential, 422, map[string]any{"decision": "rejected", "reason": "invalid_credential"}},
		"collision":           {store.ErrOccurrenceCollision, 422, map[string]any{"decision": "rejected", "reason": "occurrence_collision"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newScannerFixture(t)
			rec := httptest.NewRecorder()
			f.server.WithAdmissions(&scriptedAdmissions{err: tc.err}).Router(nil, true).ServeHTTP(rec, scannerRoutes[0].request(f.payload, tokenOK, true))
			requireExactJSON(t, rec, tc.status, tc.body)
		})
	}
}
