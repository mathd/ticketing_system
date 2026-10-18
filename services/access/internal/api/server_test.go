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
	"sync"
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
	// id is the device's own identity, and it is a field for the SAME reason
	// organizer is one — an argument that was available one line up and simply
	// not applied to it (TKT-272). Returning uuid.New() per call meant no test
	// could assert that the device a request authenticated AS is the device the
	// telemetry names: every comparison was against a value the fixture had
	// already thrown away. Zero means "let enrolled() pick one".
	id      uuid.UUID
	touched int
}

func (f *fakeDevices) AuthenticateScannerDevice(_ context.Context, token string) (store.ScannerDevice, error) {
	if token == "" || token != f.token {
		return store.ScannerDevice{}, store.ErrScannerDeviceUnknown
	}
	return store.ScannerDevice{ID: f.id, OrganizerID: f.organizer, Label: "gate"}, nil
}

func (f *fakeDevices) TouchScannerDevice(context.Context, uuid.UUID) { f.touched++ }

// countingAdmissions is the admission port with call counts (TKT-557, D7).
// Outer recovery now turns a store panic into a 500, so a recovered 500 can no
// longer prove that a request reached the store: these counters do. Each call
// answers a valid result, so a positive control proves the intended call ran
// and was mapped, not only that something was reached.
type countingAdmissions struct {
	mu                         sync.Mutex
	scans, reconciles, refusal int
}

func (c *countingAdmissions) Scan(_ context.Context, _ store.ScanInput) (store.RedeemResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scans++
	return store.RedeemResult{Accepted: true, Decision: store.DecisionAccepted, OccurredAt: time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)}, nil
}

func (c *countingAdmissions) ReconcileAdmission(_ context.Context, in store.ReconcileOccurrence) (store.ReconcileResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reconciles++
	return store.ReconcileResult{OccurrenceID: in.OccurrenceID, Outcome: store.ReconcileRecorded, OccurredAt: in.OccurredAt}, nil
}

func (c *countingAdmissions) RecordRevocationRefusal(_ context.Context, _, _, _, _, occurrenceID uuid.UUID, occurredAt time.Time, _ string) (store.ReconcileResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refusal++
	return store.ReconcileResult{OccurrenceID: occurrenceID, Outcome: store.ReconcileRecorded, OccurredAt: occurredAt}, nil
}

func (c *countingAdmissions) counts() (scans, reconciles, refusals int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scans, c.reconciles, c.refusal
}

// enrolled builds a server whose scan routes accept testDeviceToken, for the
// organizer the device is paired to.
func enrolled(s *Server, organizer ...uuid.UUID) *Server {
	device := &fakeDevices{token: testDeviceToken, organizer: uuid.New(), id: uuid.New()}
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
			enrolled(newTestServer(nil, verifier)).Router(nil, true).ServeHTTP(recorder, request)
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
	router := enrolled(newTestServer(nil, verifier)).Router(nil, true)

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
	enrolled(newTestServer(nil, verifier)).Router(nil, true).ServeHTTP(recorder, request)
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
	enrolled(newTestServer(nil, verifier)).Router(nil, true).ServeHTTP(recorder, request)
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
	enrolled(newTestServer(nil, nil)).Router(nil, true).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
}

func TestScanRejectsWhenVerifierIsUnavailable(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := scanRequest(http.MethodPost, "/scans", bytes.NewBufferString(`{"qr_payload":"not-a-ticket"}`))
	request.Header.Set("Content-Type", "application/json")
	enrolled(newTestServer(nil, nil)).Router(nil, true).ServeHTTP(recorder, request)
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
	router := enrolled(newTestServer(nil, verifier)).Router(nil, true)

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
	router := enrolled(newTestServer(nil, verifier)).Router(nil, true)

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

// An unknown event_type on a VALID, in-scope credential is refused for that item
// only: "only its item" means HTTP 200 with a per-item rejected result, not a 422
// for the whole batch. The request holds one row, so it does not prove that
// another row succeeds. The test above cannot see the event_type check, because
// its credential is invalid and the credential check refuses that row too. The
// store is nil on purpose: without the check the row would be recorded as an
// entry admission, and here it panics in (*Postgres).ReconcileAdmission instead.
//
// The plan adds a positive control (TKT-553 D8): a second, separate single-row
// request with event_type "exit" must reach admission recording. It proves that
// occurrence parsing, the credential and the scope check let this fixture
// through, so the refusal of the "sideways" row comes from the event_type check.
// It is a separate request and not a second row, because an exit row in the same
// batch would reach the store and panic before the sideways row is handled.
// D8 also adds eight unmutated runs to the mutation check of this test, so the
// control must reach recording on every run, not only once.
func TestReconcileUnknownEventTypeRejectsOnlyItsItem(t *testing.T) {
	organizer := uuid.New()
	verifier, payload := signedFor(t, organizer)
	// A setup check only: a broken credential fails here, and does not read as a
	// refusal by the event_type check.
	claims, err := verifier.Verify(payload)
	if err != nil {
		t.Fatalf("fixture credential does not verify: %v", err)
	}
	if claims.OrganizerID != organizer {
		t.Fatalf("fixture credential organizer = %s, want %s", claims.OrganizerID, organizer)
	}
	admissions := &countingAdmissions{}
	router := enrolled(newTestServer(nil, verifier), organizer).WithAdmissions(admissions).Router(nil, true)
	reconcile := func(occurrenceID, eventType string) *http.Request {
		body, err := json.Marshal(map[string]any{"occurrences": []map[string]string{{
			"qr_payload": payload, "occurrence_id": occurrenceID, "occurred_at": "2026-07-17T09:00:00Z",
			"local_decision": "", "event_type": eventType,
		}}})
		if err != nil {
			t.Fatal(err)
		}
		request := scanRequest(http.MethodPost, "/scans/reconciliations", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		return request
	}

	// The positive control reaches admission recording ONCE and is mapped to
	// its valid per-item result (TKT-557 D7: a port call count, not a panic).
	controlID := uuid.NewString()
	controlRecorder := httptest.NewRecorder()
	router.ServeHTTP(controlRecorder, reconcile(controlID, "exit"))
	if _, reconciles, _ := admissions.counts(); reconciles != 1 || controlRecorder.Code != http.StatusOK ||
		!strings.Contains(controlRecorder.Body.String(), `"result":"recorded"`) || !strings.Contains(controlRecorder.Body.String(), controlID) {
		t.Fatalf("valid exit control: reconciles=%d status=%d body=%s, want one recorded call", reconciles, controlRecorder.Code, controlRecorder.Body.String())
	}

	occurrenceID := uuid.NewString()
	sideways := reconcile(occurrenceID, "sideways")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, sideways)
	if scans, reconciles, refusals := admissions.counts(); scans != 0 || reconciles != 1 || refusals != 0 {
		t.Fatalf("unknown event_type reached the admission port: scans=%d reconciles=%d refusals=%d", scans, reconciles, refusals)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("reconcile with unknown event_type = %d, want 200 with a per-item result; body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, recorder.Body.String())
	}
	if len(response.Results) != 1 {
		t.Fatalf("results = %+v, want exactly one", response.Results)
	}
	if response.Results[0]["occurrence_id"] != occurrenceID {
		t.Errorf("occurrence_id = %v, want %s echoed unchanged", response.Results[0]["occurrence_id"], occurrenceID)
	}
	if response.Results[0]["result"] != "rejected" {
		t.Errorf("result = %v, want rejected", response.Results[0]["result"])
	}
}

// An unknown local_decision on a VALID, in-scope credential is refused for that
// item only. The credential must verify, or the credential check refuses the item
// first and the test cannot see the decision check at all. The store is nil on
// purpose: an item that reached it would be recorded as an admission, and here it
// would panic instead. A local_decision beside an event_type is refused too,
// because the item would then name two different facts.
func TestReconcileUnknownLocalDecisionRejectsOnlyItsItem(t *testing.T) {
	organizer := uuid.New()
	verifier, payload := signedFor(t, organizer)
	router := enrolled(newTestServer(nil, verifier), organizer).Router(nil, true)
	unknown, mixed := uuid.NewString(), uuid.NewString()
	request := scanRequest(http.MethodPost, "/scans/reconciliations", bytes.NewBufferString(
		`{"occurrences":[`+
			`{"qr_payload":"`+payload+`","occurrence_id":"`+unknown+`","occurred_at":"2026-07-17T09:00:00Z","local_decision":"future_decision"},`+
			`{"qr_payload":"`+payload+`","occurrence_id":"`+mixed+`","occurred_at":"2026-07-17T09:00:00Z","local_decision":"revocation_refused","event_type":"exit"}]}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("reconcile with unknown local decision = %d, want per-item results", recorder.Code)
	}
	var response struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 2 ||
		response.Results[0]["occurrence_id"] != unknown || response.Results[0]["result"] != "rejected" ||
		response.Results[1]["occurrence_id"] != mixed || response.Results[1]["result"] != "rejected" {
		t.Fatalf("results = %+v, want both items rejected by id", response.Results)
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
//
// TKT-557 (D13/D25): exactly 400. The store is nil, and outer recovery now
// turns a store panic into a declared 500, so a set of declared statuses could
// no longer fail. Exact 400 can.
func TestInternalRoutesRejectMalformedRequestsWithADeclaredStatus(t *testing.T) {
	router := enrolled(newTestServer(nil, nil, "internal-token")).Router(nil, true)

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
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want exactly 400 from refundTickets; body=%s", recorder.Code, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), "invalid_credential") {
				t.Fatalf("internal route answered with the gate's scan-shaped rejection: %s", recorder.Body.String())
			}
		})
	}

	request := httptest.NewRequest(http.MethodGet,
		"/internal/orders/00000000-0000-0000-0000-000000000003/admission?organizer_id=not-a-uuid", nil)
	request.Header.Set("X-Internal-Token", "internal-token")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("orderAdmission returned %d for malformed organizer_id, want exactly 400: %s",
			recorder.Code, recorder.Body.String())
	}
}

func TestOrderAdmissionRejectsMissingTokenAndInvalidOrder(t *testing.T) {
	router := newTestServer(nil, nil, "internal-token").Router(nil, true)
	for _, tc := range []struct {
		name, path, token string
		want              int
	}{
		{name: "missing token", path: "/internal/orders/00000000-0000-0000-0000-000000000003/admission?organizer_id=00000000-0000-0000-0000-000000000002", want: http.StatusNotFound},
		{name: "invalid order", path: "/internal/orders/nope/admission?organizer_id=00000000-0000-0000-0000-000000000002", token: "internal-token", want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.token != "" {
				request.Header.Set("X-Internal-Token", tc.token)
			}
			router.ServeHTTP(recorder, request)
			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, tc.want, recorder.Body.String())
			}
		})
	}
}

// The gate keeps its own representation — the fix is route-scoped, not a replacement.
func TestScanKeepsTheScanShapedRejection(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := scanRequest(http.MethodPost, "/scans", strings.NewReader(`{"qr_payload":123}`))
	request.Header.Set("Content-Type", "application/json")
	enrolled(newTestServer(nil, nil)).Router(nil, true).ServeHTTP(recorder, request)
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
	router := enrolled(newTestServer(nil, verifier)).Router(nil, true)

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
	newTestServer(nil, verifier).Router(nil, true).ServeHTTP(recorder, request)
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
	foreign := &countingAdmissions{}
	router := enrolled(newTestServer(nil, verifier), uuid.New()).WithAdmissions(foreign).Router(nil, true)

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
	if scans, reconciles, refusals := foreign.counts(); scans+reconciles+refusals != 0 {
		t.Fatalf("a foreign organizer's ticket reached the admission port: scans=%d reconciles=%d refusals=%d", scans, reconciles, refusals)
	}

	// ...and the device enrolled for THIS organizer reaches the admission port
	// on both routes and gets the valid results back. Delete the scope check
	// and the cases above join these; invert it and these join the cases above.
	own := &countingAdmissions{}
	ownRouter := enrolled(newTestServer(nil, verifier), organizer).WithAdmissions(own).Router(nil, true)
	scanned := httptest.NewRecorder()
	scan := scanRequest(http.MethodPost, "/scans", bytes.NewReader(body))
	scan.Header.Set("Content-Type", "application/json")
	ownRouter.ServeHTTP(scanned, scan)
	if scans, _, _ := own.counts(); scans != 1 || scanned.Code != http.StatusOK || !strings.Contains(scanned.Body.String(), `"decision":"accepted"`) {
		t.Fatalf("own organizer /scans: scans=%d status=%d body=%s, want one accepted call", scans, scanned.Code, scanned.Body.String())
	}
	synced := httptest.NewRecorder()
	syncRequest := scanRequest(http.MethodPost, "/scans/reconciliations", bytes.NewReader(batch))
	syncRequest.Header.Set("Content-Type", "application/json")
	ownRouter.ServeHTTP(synced, syncRequest)
	if _, reconciles, _ := own.counts(); reconciles != 1 || synced.Code != http.StatusOK || !strings.Contains(synced.Body.String(), `"result":"recorded"`) {
		t.Fatalf("own organizer reconcile: reconciles=%d status=%d body=%s, want one recorded call", reconciles, synced.Code, synced.Body.String())
	}
}

// The voided feed's authorization surface (TKT-162).
//
// These live at the API tier because that is where the mechanism lives: the
// organizer comes from the device token via the validator's AuthenticationFunc,
// and the handler reads it out of the context slot. The database-tier question —
// does the SQL actually scope? — is asserted in the store's smoke tests against
// real PostgreSQL, because a fake that scopes in Go would prove only that the
// fake and the handler agree.

func TestVoidedFeedRefusesAnUnenrolledDevice(t *testing.T) {
	verifier, err := ticket.NewVerifier("access-qr/test-v1=O2onvM62pC1io6jQKm8Nc2UyFXcd4kOmOsBIoYtZ2ik", "access-qr/test-v1")
	if err != nil {
		t.Fatal(err)
	}
	router := enrolled(newTestServer(nil, verifier)).Router(nil, true)

	for name, token := range map[string]string{
		"absent":     "",
		"wrong":      "not-the-enrolled-token",
		"near-miss":  testDeviceToken + "x",
		"whitespace": " ",
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/scans/voided-tickets", nil)
			if token != "" {
				request.Header.Set(scannerDeviceHeader, token)
			}
			router.ServeHTTP(recorder, request)
			// 401, not an empty page. A feed that answers "nothing is revoked" to
			// an unpaired device is the most dangerous possible response: the
			// scanner would cache it as a complete view and admit voided holders.
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("unenrolled %s = %d, want 401 (never an empty feed, which reads as 'nothing is revoked')", name, recorder.Code)
			}
		})
	}
}

// The cursor is bound to the organizer it was issued for. An unbound cursor
// cannot read another organizer's rows — the query filters regardless — but it
// CAN suppress: copied or forged, it makes the holder's own next page skip rows
// and come back short, silently. For a revocation feed that is the direction
// that matters, so it must be a refusal rather than a quiet gap.
func TestVoidedFeedRefusesAForeignCursor(t *testing.T) {
	mine, theirs := uuid.New(), uuid.New()

	signer := newTestServer(nil, nil).WithFeedCursorKey("test-feed-cursor-key")
	foreign, err := signer.encodeCursor(store.VoidedCursor{
		OccurredAt: time.Now().UTC(), EventID: uuid.New(), OrganizerID: theirs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signer.decodeCursor(foreign, mine); err == nil {
		t.Fatal("a cursor issued for another organizer was accepted — it would silently shorten this organizer's page")
	}

	// The same cursor, presented by the organizer it belongs to, must work: a
	// test that only proves refusal is satisfied by a decoder that refuses
	// everything.
	own, err := signer.encodeCursor(store.VoidedCursor{
		OccurredAt: time.Now().UTC(), EventID: uuid.New(), OrganizerID: mine,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signer.decodeCursor(own, mine); err != nil {
		t.Fatalf("a cursor presented by its own organizer was refused: %v", err)
	}
}

func TestVoidedFeedCursorRoundTripsAndRefusesGarbage(t *testing.T) {
	org := uuid.New()
	at := time.Date(2026, 8, 22, 14, 30, 0, 123456789, time.UTC)
	id := uuid.New()

	signer := newTestServer(nil, nil).WithFeedCursorKey("test-feed-cursor-key")
	encoded, err := signer.encodeCursor(store.VoidedCursor{OccurredAt: at, EventID: id, OrganizerID: org})
	if err != nil {
		t.Fatal(err)
	}
	back, err := signer.decodeCursor(encoded, org)
	if err != nil {
		t.Fatal(err)
	}
	// Nanoseconds survive: the keyset compares on this value, so a cursor that
	// loses precision resumes at the wrong place and skips or repeats a row.
	if !back.OccurredAt.Equal(at) || back.EventID != id || back.OrganizerID != org {
		t.Fatalf("round trip = %+v, want %v/%s/%s", back, at, id, org)
	}

	// Each of these must be refused for the reason its NAME says, which means each
	// must carry a VALID signature — otherwise every case fails at the MAC and the
	// whole table degenerates into one signature test wearing six labels. `sign`
	// is what keeps them about their subjects.
	sign := func(payload string) string {
		body := base64.RawURLEncoding.EncodeToString([]byte(payload))
		return body + "." + base64.RawURLEncoding.EncodeToString(signer.cursors.sign([]byte(body)))
	}
	for name, bad := range map[string]string{
		"not base64":    "!!!not-base64!!!",
		"not json":      sign("nonsense"),
		"wrong version": sign(`{"v":99,"occurred_at":"2026-08-22T14:30:00Z","event_id":"` + id.String() + `","organizer_id":"` + org.String() + `"}`),
		"zero event":    sign(`{"v":1,"occurred_at":"2026-08-22T14:30:00Z","event_id":"00000000-0000-0000-0000-000000000000","organizer_id":"` + org.String() + `"}`),
		"zero time":     sign(`{"v":1,"occurred_at":"0001-01-01T00:00:00Z","event_id":"` + id.String() + `","organizer_id":"` + org.String() + `"}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := signer.decodeCursor(bad, org); err == nil {
				t.Fatalf("%s was accepted as a cursor", name)
			}
		})
	}
}

// A cursor a device made up is refused, even for its own organizer.
//
// Base64 is an encoding, not a protection (ai-review [high]). Without a MAC an
// enrolled device can hand-craft a position — an old one, or one past the end —
// and receive an empty page with next_cursor: null. It cannot reach another
// organizer's rows, so the forger and the victim are the same party; that still
// matters here, because for a revocation feed the damaging state is a device
// that believes its view is complete when it is not, and this is a way to enter
// that state and never learn.
func TestVoidedFeedRefusesAnUnsignedOrTamperedCursor(t *testing.T) {
	signer := newTestServer(nil, nil).WithFeedCursorKey("test-feed-cursor-key")
	org := uuid.New()

	valid, err := signer.encodeCursor(store.VoidedCursor{
		OccurredAt: time.Now().UTC(), EventID: uuid.New(), OrganizerID: org,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _, _ := strings.Cut(valid, ".")

	// Hand-rolled: exactly what a device can construct on its own.
	forged := base64.RawURLEncoding.EncodeToString([]byte(
		`{"v":1,"occurred_at":"2020-01-01T00:00:00Z","event_id":"` + uuid.New().String() +
			`","organizer_id":"` + org.String() + `"}`))

	for name, cursor := range map[string]string{
		"unsigned":           body,
		"no signature":       forged,
		"empty signature":    forged + ".",
		"wrong signature":    forged + "." + base64.RawURLEncoding.EncodeToString([]byte("not-the-mac")),
		"signature reused":   forged + "." + strings.SplitN(valid, ".", 2)[1],
		"body swapped under": forged + "." + strings.SplitN(valid, ".", 2)[1],
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := signer.decodeCursor(cursor, org); err == nil {
				t.Fatalf("a %s cursor was accepted — a device can forge its own position and skip its own revocations", name)
			}
		})
	}

	// And the honest one still works: a test that only proves refusal is
	// satisfied by a decoder that refuses everything.
	if _, err := signer.decodeCursor(valid, org); err != nil {
		t.Fatalf("a legitimately issued cursor was refused: %v", err)
	}
}
