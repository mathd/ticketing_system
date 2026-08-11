package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/access/internal/store"
	"ticketing/services/access/internal/ticket"
)

// Every scan and reconciliation now needs an enrolled device (ai-review S1). The
// contract tests below are about the SCAN semantics, so they pair a device once,
// here, and send its token on every request — otherwise each of them would be
// asserting the enrolment guard a tenth time and its own subject not at all.
//
// The fake enforces revocation in Go. The shipped predicate is a SQL WHERE
// clause, so what a fake can prove is that the handler consults the port and
// refuses when it says no — the enrolment and revocation guarantees themselves
// are asserted against real PostgreSQL in scanner_devices_smoke_test.go.
const testDeviceToken = "enrolled-device-token"

type fakeDevices struct {
	token string
	// organizer is what the device is ENROLLED for. A field rather than a fresh
	// uuid.New() per call because the scope check compares it to the ticket's
	// organizer: a random value makes every scan a mismatch, so a fixture that
	// generated one could never construct the allowed case.
	organizer uuid.UUID
	touched   int
}

func (f *fakeDevices) AuthenticateScannerDevice(_ context.Context, token string) (store.ScannerDevice, error) {
	if token == "" || token != f.token {
		return store.ScannerDevice{}, store.ErrScannerDeviceUnknown
	}
	return store.ScannerDevice{ID: uuid.New(), OrganizerID: f.organizer, Label: "gate"}, nil
}

func (f *fakeDevices) TouchScannerDevice(context.Context, uuid.UUID) { f.touched++ }

// enrolled builds a server whose scan routes accept testDeviceToken, for the
// organizer the device is paired to.
func enrolled(s *Server, organizer ...uuid.UUID) *Server {
	device := &fakeDevices{token: testDeviceToken, organizer: uuid.New()}
	if len(organizer) > 0 {
		device.organizer = organizer[0]
	}
	return s.WithScannerDevices(device)
}

// scanRequest is httptest.NewRequest plus the device credential, so a test that
// means to exercise a scan does not silently exercise the enrolment guard.
func scanRequest(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.Header.Set(scannerDeviceHeader, testDeviceToken)
	return r
}

func TestScanRejectsNonStrictJSONBeforeRedeeming(t *testing.T) {
	verifier, err := ticket.NewVerifier("access-qr/test-v1=O2onvM62pC1io6jQKm8Nc2UyFXcd4kOmOsBIoYtZ2ik", "access-qr/test-v1")
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"unknown field":  `{"qr_payload":"not-a-ticket","unexpected":true}`,
		"trailing value": `{"qr_payload":"not-a-ticket"}{}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := scanRequest(http.MethodPost, "/scans", bytes.NewBufferString(body))
			request.Header.Set("Content-Type", "application/json")
			enrolled(New(nil, verifier)).Router(nil, true).ServeHTTP(recorder, request)
			if recorder.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnprocessableEntity)
			}
			var response map[string]string
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response["decision"] != "rejected" || response["reason"] != "invalid_credential" {
				t.Fatalf("response = %v, want committed ScanRejected representation", response)
			}
		})
	}
}

// The expand phase of ADR-025 §D10: the contract must accept the occurrence
// fields (today it rejects them), and the server must enforce the pairing rule
// the schema cannot express — occurred_at is required iff occurrence_id is
// present.
func TestScanContractAcceptsOccurrenceFields(t *testing.T) {
	verifier, err := ticket.NewVerifier("access-qr/test-v1=O2onvM62pC1io6jQKm8Nc2UyFXcd4kOmOsBIoYtZ2ik", "access-qr/test-v1")
	if err != nil {
		t.Fatal(err)
	}
	router := enrolled(New(nil, verifier)).Router(nil, true)

	// Full pair: passes the contract validator, fails on the (bad) credential —
	// proof the fields got past additionalProperties:false.
	recorder := httptest.NewRecorder()
	request := scanRequest(http.MethodPost, "/scans", bytes.NewBufferString(
		`{"qr_payload":"not-a-ticket","occurrence_id":"`+uuid.NewString()+`","occurred_at":"2026-07-17T09:00:00Z"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	var response map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusUnprocessableEntity || response["reason"] != "invalid_credential" {
		t.Fatalf("scan with occurrence fields = %d %v, want the credential rejection (not a contract one)", recorder.Code, response)
	}

	// Occurrence without its claimed time: the server-side pairing rule, with
	// its own reason so a scanner can tell it from a bad credential.
	recorder = httptest.NewRecorder()
	request = scanRequest(http.MethodPost, "/scans", bytes.NewBufferString(
		`{"qr_payload":"not-a-ticket","occurrence_id":"`+uuid.NewString()+`"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusUnprocessableEntity || response["reason"] != "invalid_occurrence" {
		t.Fatalf("occurrence without occurred_at = %d %v, want invalid_occurrence", recorder.Code, response)
	}
}

// One bad occurrence never fails the batch: each entry gets its own result,
// order-preserving (ADR-025 §D6 — reconciliation is recording; a gate syncing
// N occurrences must learn the fate of each).
func TestReconcileRejectsBadCredentialsPerOccurrence(t *testing.T) {
	verifier, err := ticket.NewVerifier("access-qr/test-v1=O2onvM62pC1io6jQKm8Nc2UyFXcd4kOmOsBIoYtZ2ik", "access-qr/test-v1")
	if err != nil {
		t.Fatal(err)
	}
	occA, occB := uuid.NewString(), uuid.NewString()
	// The third id is deliberately not a UUID: the response must echo it
	// VERBATIM, or the scanner can never correlate the rejection back to its
	// queue entry and re-sends it forever.
	malformed := "not-a-uuid"
	recorder := httptest.NewRecorder()
	request := scanRequest(http.MethodPost, "/scans/reconciliations", bytes.NewBufferString(
		`{"occurrences":[`+
			`{"qr_payload":"not-a-ticket","occurrence_id":"`+occA+`","occurred_at":"2026-07-17T09:00:00Z"},`+
			`{"qr_payload":"also-not-a-ticket","occurrence_id":"`+occB+`","occurred_at":"2026-07-17t09:01:00z"},`+
			`{"qr_payload":"not-a-ticket","occurrence_id":"`+malformed+`","occurred_at":"2026-07-17T09:02:00Z"},`+
			`{"qr_payload":"","occurrence_id":"`+uuid.NewString()+`","occurred_at":"2026-07-17T09:03:00Z"}]}`))
	request.Header.Set("Content-Type", "application/json")
	enrolled(New(nil, verifier)).Router(nil, true).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a bad credential is a per-occurrence result, not a batch failure)", recorder.Code)
	}
	var response struct {
		Results []struct {
			OccurrenceID string `json:"occurrence_id"`
			Result       string `json:"result"`
		} `json:"results"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	// Four entries: bad credential, lowercase timestamp + bad credential,
	// malformed id, empty qr_payload — each a per-occurrence rejection, none a
	// batch failure, every id echoed verbatim.
	if len(response.Results) != 4 || response.Results[0].OccurrenceID != occA || response.Results[1].OccurrenceID != occB || response.Results[2].OccurrenceID != malformed {
		t.Fatalf("results = %+v, want all four occurrence ids echoed verbatim in request order", response.Results)
	}
	for _, r := range response.Results {
		if r.Result != "rejected" {
			t.Fatalf("result = %+v, want rejected", r)
		}
	}
}

// RFC 3339 permits lowercase t/z; the contract says date-time, so the server
// must accept what it advertises rather than Go's stricter layout.
func TestScanAcceptsLowercaseRFC3339(t *testing.T) {
	verifier, err := ticket.NewVerifier("access-qr/test-v1=O2onvM62pC1io6jQKm8Nc2UyFXcd4kOmOsBIoYtZ2ik", "access-qr/test-v1")
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := scanRequest(http.MethodPost, "/scans", bytes.NewBufferString(
		`{"qr_payload":"not-a-ticket","occurrence_id":"`+uuid.NewString()+`","occurred_at":"2026-07-17t09:00:00z"}`))
	request.Header.Set("Content-Type", "application/json")
	enrolled(New(nil, verifier)).Router(nil, true).ServeHTTP(recorder, request)
	var response map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	// invalid_credential, not invalid_occurrence: the timestamp parsed and the
	// request fell through to the (bad) credential.
	if recorder.Code != http.StatusUnprocessableEntity || response["reason"] != "invalid_credential" {
		t.Fatalf("lowercase RFC3339 scan = %d %v, want the credential rejection", recorder.Code, response)
	}
}

func TestReconcileUnavailableWithoutVerifier(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := scanRequest(http.MethodPost, "/scans/reconciliations", bytes.NewBufferString(
		`{"occurrences":[{"qr_payload":"x","occurrence_id":"`+uuid.NewString()+`","occurred_at":"2026-07-17T09:00:00Z"}]}`))
	request.Header.Set("Content-Type", "application/json")
	enrolled(New(nil, nil)).Router(nil, true).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
}

func TestScanRejectsWhenVerifierIsUnavailable(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := scanRequest(http.MethodPost, "/scans", bytes.NewBufferString(`{"qr_payload":"not-a-ticket"}`))
	request.Header.Set("Content-Type", "application/json")
	enrolled(New(nil, nil)).Router(nil, true).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
}

// The TKT-87 expand phase: the contract must accept `direction` on scans and
// `event_type` on reconciliation occurrences (today it rejects both under
// additionalProperties:false), old bodies stay valid, and an invalid direction
// is a contract rejection.
func TestScanContractAcceptsDirectionField(t *testing.T) {
	verifier, err := ticket.NewVerifier("access-qr/test-v1=O2onvM62pC1io6jQKm8Nc2UyFXcd4kOmOsBIoYtZ2ik", "access-qr/test-v1")
	if err != nil {
		t.Fatal(err)
	}
	router := enrolled(New(nil, verifier)).Router(nil, true)

	// direction gets past the validator: the failure must be the (bad)
	// credential, not the contract.
	recorder := httptest.NewRecorder()
	request := scanRequest(http.MethodPost, "/scans", bytes.NewBufferString(
		`{"qr_payload":"not-a-ticket","direction":"exit","occurrence_id":"`+uuid.NewString()+`","occurred_at":"2026-07-17T09:00:00Z"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	var response map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusUnprocessableEntity || response["reason"] != "invalid_credential" {
		t.Fatalf("scan with direction = %d %v, want the credential rejection (not a contract one)", recorder.Code, response)
	}

	// An unknown direction is refused by the contract enum.
	recorder = httptest.NewRecorder()
	request = scanRequest(http.MethodPost, "/scans", bytes.NewBufferString(
		`{"qr_payload":"not-a-ticket","direction":"sideways"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown direction = %d, want 422", recorder.Code)
	}
}

func TestReconcileContractAcceptsEventTypePerOccurrence(t *testing.T) {
	verifier, err := ticket.NewVerifier("access-qr/test-v1=O2onvM62pC1io6jQKm8Nc2UyFXcd4kOmOsBIoYtZ2ik", "access-qr/test-v1")
	if err != nil {
		t.Fatal(err)
	}
	router := enrolled(New(nil, verifier)).Router(nil, true)

	// event_type passes the contract; the bad credential earns a per-item
	// rejected result, and a malformed event_type is ALSO a per-item rejected
	// result — batch entries never 422 the whole sync (the established
	// batch-item posture).
	recorder := httptest.NewRecorder()
	request := scanRequest(http.MethodPost, "/scans/reconciliations", bytes.NewBufferString(
		`{"occurrences":[`+
			`{"qr_payload":"not-a-ticket","occurrence_id":"`+uuid.NewString()+`","occurred_at":"2026-07-17T09:00:00Z","event_type":"exit"},`+
			`{"qr_payload":"not-a-ticket","occurrence_id":"`+uuid.NewString()+`","occurred_at":"2026-07-17T09:00:00Z","event_type":"sideways"}]}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("reconcile with event_type = %d, want 200 with per-item results", recorder.Code)
	}
	var out struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 2 || out.Results[0]["result"] != "rejected" || out.Results[1]["result"] != "rejected" {
		t.Fatalf("results = %+v, want two per-item rejections", out.Results)
	}
}

// ai-review F4. The scan-shaped 422 is the gate's established representation for a
// request the contract rejects, and it was applied to EVERY route — including the
// internal refund operation, which declares no 422. An undeclared status is exactly the
// drift ADR-028's response validator exists to convert into a 500, and this one slipped
// past it because the REQUEST validator answers first, before the response validator can
// see anything.
//
// The assertion is on the status *being declared*, not merely on it being 400: the point
// is that the route can no longer answer outside its own contract.
func TestInternalRoutesRejectMalformedRequestsWithADeclaredStatus(t *testing.T) {
	declared := map[int]bool{400: true, 404: true, 409: true, 500: true, 503: true}
	router := enrolled(New(nil, nil, "internal-token")).Router(nil, true)

	for name, body := range map[string]string{
		"unknown field":  `{"organizer_id":"00000000-0000-0000-0000-000000000001","refund_id":"00000000-0000-0000-0000-000000000002","quantity":1,"nope":true}`,
		"missing field":  `{"organizer_id":"00000000-0000-0000-0000-000000000001"}`,
		"wrong type":     `{"organizer_id":"00000000-0000-0000-0000-000000000001","refund_id":"00000000-0000-0000-0000-000000000002","quantity":"two"}`,
		"quantity zero":  `{"organizer_id":"00000000-0000-0000-0000-000000000001","refund_id":"00000000-0000-0000-0000-000000000002","quantity":0}`,
		"malformed json": `{`,
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost,
				"/internal/orders/00000000-0000-0000-0000-000000000003/refunds", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Internal-Token", "internal-token")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if !declared[recorder.Code] {
				t.Fatalf("status %d is not declared by refundTickets; body=%s", recorder.Code, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), "invalid_credential") {
				t.Fatalf("internal route answered with the gate's scan-shaped rejection: %s", recorder.Body.String())
			}
		})
	}
}

// The gate keeps its own representation — the fix is route-scoped, not a replacement.
func TestScanKeepsTheScanShapedRejection(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := scanRequest(http.MethodPost, "/scans", strings.NewReader(`{"qr_payload":123}`))
	request.Header.Set("Content-Type", "application/json")
	enrolled(New(nil, nil)).Router(nil, true).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "invalid_credential") {
		t.Fatalf("scan rejection changed: %d %s", recorder.Code, recorder.Body.String())
	}
}

// ai-review S1, the finding this whole change exists for. POST /scans and
// POST /scans/reconciliations accepted an admission decision from anyone who
// could reach the gateway; the second rewrites a night's history in bulk.
//
// The refusal has to be its OWN status. A gate app must tell "this phone is not
// paired" from "turn this person away", and answering the scan-shaped 422 to an
// unpaired phone sends an operator to the wrong problem at the worst moment.
//
// Delete WithScannerDevices from the router construction and this test goes back
// to 422/200 — which is what it looked like before the fix, and why the
// assertions below are on the status rather than on "not 200".
func TestScanRoutesRefuseAnUnenrolledDevice(t *testing.T) {
	verifier, err := ticket.NewVerifier("access-qr/test-v1=O2onvM62pC1io6jQKm8Nc2UyFXcd4kOmOsBIoYtZ2ik", "access-qr/test-v1")
	if err != nil {
		t.Fatal(err)
	}
	router := enrolled(New(nil, verifier)).Router(nil, true)

	bodies := map[string]string{
		"/scans":                 `{"qr_payload":"not-a-ticket"}`,
		"/scans/reconciliations": `{"occurrences":[{"qr_payload":"not-a-ticket","occurrence_id":"6f3f5d18-0b41-4a2a-9c07-2f5f0f5f1a11","occurred_at":"2026-08-10T10:00:00Z"}]}`,
	}
	for path, body := range bodies {
		for name, token := range map[string]string{
			"absent":     "",
			"wrong":      "not-the-enrolled-token",
			"near-miss":  testDeviceToken + "x",
			"whitespace": " ",
		} {
			t.Run(path+" "+name, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
				request.Header.Set("Content-Type", "application/json")
				if token != "" {
					request.Header.Set(scannerDeviceHeader, token)
				}
				router.ServeHTTP(recorder, request)
				if recorder.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401; body=%s", recorder.Code, recorder.Body.String())
				}
				// Never the gate's rejection shape: that would tell an unpaired
				// scanner it had a ticket problem.
				if strings.Contains(recorder.Body.String(), "invalid_credential") {
					t.Errorf("an unenrolled device got the ticket-rejection body: %s", recorder.Body.String())
				}
			})
		}
	}

	// The credential is checked BEFORE the body is decoded, so an unenrolled
	// caller cannot use the scan endpoint as a payload validator.
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/scans", bytes.NewBufferString(`{"nonsense":`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("malformed body from an unenrolled device = %d, want 401 (the credential is checked first)", recorder.Code)
	}
}

// A Server built without an enrolment port must refuse, not admit. The failure
// this guards against is the one that looks like nothing: a construction path
// that skips New leaves `devices` nil, and "nil means allow" would make every
// such server an open door.
func TestScanRefusesWhenEnrolmentCannotBeChecked(t *testing.T) {
	verifier, err := ticket.NewVerifier("access-qr/test-v1=O2onvM62pC1io6jQKm8Nc2UyFXcd4kOmOsBIoYtZ2ik", "access-qr/test-v1")
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := scanRequest(http.MethodPost, "/scans", bytes.NewBufferString(`{"qr_payload":"not-a-ticket"}`))
	request.Header.Set("Content-Type", "application/json")
	New(nil, verifier).Router(nil, true).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("a server that cannot check enrolment answered %d, want 401", recorder.Code)
	}
}

// signedFor mints a real QR payload for one organizer, and the verifier that
// accepts it. A generated keypair rather than the fixture keys, so the test
// carries no dependency on which literal seed the other tests happen to use.
func signedFor(t *testing.T, organizer uuid.UUID) (*ticket.Verifier, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const kid = "access-qr/scope-test"
	signer, err := ticket.New(base64.RawStdEncoding.EncodeToString(private.Seed()), kid)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := ticket.NewVerifier(kid+"="+base64.RawStdEncoding.EncodeToString(public), kid)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := signer.Payload(uuid.New(), uuid.New(), organizer, uuid.New(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return verifier, payload
}

// Enrolment carries an organizer_id and the CLI demands one, but until the scope
// check existed nothing compared it to the ticket being scanned: any enrolled
// device could redeem — and so BURN — any organizer's ticket at its own door,
// and reconcile could rewrite a night of someone else's admission history in
// bulk. The device is validly paired, so the refusal is the gate's 422 and NOT a
// 401: the scanner unpairs itself on 401.
//
// The allowed case is asserted too, and it is what stops this passing against a
// check that refuses everything. With a matching organizer the request reaches
// the store — which this fixture deliberately does not provide, so it panics
// there — and that is the observable difference from a scope refusal, which
// answers before the store is ever touched.
func TestScanRoutesRefuseAnotherOrganizersTicket(t *testing.T) {
	organizer := uuid.New()
	verifier, payload := signedFor(t, organizer)
	body, err := json.Marshal(map[string]string{"qr_payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := json.Marshal(map[string]any{"occurrences": []map[string]string{{
		"qr_payload": payload, "occurrence_id": "6f3f5d18-0b41-4a2a-9c07-2f5f0f5f1a11",
		"occurred_at": "2026-08-10T10:00:00Z",
	}}})
	if err != nil {
		t.Fatal(err)
	}

	// A device enrolled for somebody else gets nowhere on either route.
	router := enrolled(New(nil, verifier), uuid.New()).Router(nil, true)

	recorder := httptest.NewRecorder()
	request := scanRequest(http.MethodPost, "/scans", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("/scans status = %d, want 422; body=%s", recorder.Code, recorder.Body.String())
	}
	var rejection map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &rejection); err != nil {
		t.Fatal(err)
	}
	// The same reason an unverifiable payload gets: a distinct one would tell a
	// device pointed at someone else's event that the ticket it read is real.
	if rejection["decision"] != "rejected" || rejection["reason"] != "invalid_credential" {
		t.Errorf("/scans response = %v, want the committed ScanRejected representation", rejection)
	}

	recorder = httptest.NewRecorder()
	request = scanRequest(http.MethodPost, "/scans/reconciliations", bytes.NewReader(batch))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	// Per-occurrence, following the batch posture: 200 with a rejected entry.
	if recorder.Code != http.StatusOK {
		t.Fatalf("/scans/reconciliations status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	var batchResponse struct {
		Results []struct{ Result string } `json:"results"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &batchResponse); err != nil {
		t.Fatal(err)
	}
	if len(batchResponse.Results) != 1 || batchResponse.Results[0].Result != "rejected" {
		t.Errorf("reconcile results = %+v, want one rejected entry", batchResponse.Results)
	}

	// ...and the device enrolled for THIS organizer is let through to the store.
	// Delete the scope check and the case above joins this one; invert it and
	// this one joins the case above.
	reached := func() (reached bool) {
		defer func() { reached = recover() != nil }()
		scanned := httptest.NewRecorder()
		scan := scanRequest(http.MethodPost, "/scans", bytes.NewReader(body))
		scan.Header.Set("Content-Type", "application/json")
		enrolled(New(nil, verifier), organizer).Router(nil, true).ServeHTTP(scanned, scan)
		return false
	}()
	if !reached {
		t.Error("a device enrolled for the ticket's own organizer was refused before the store")
	}
}
