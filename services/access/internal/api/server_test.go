package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/access/internal/ticket"
)

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
			request := httptest.NewRequest(http.MethodPost, "/scans", bytes.NewBufferString(body))
			request.Header.Set("Content-Type", "application/json")
			New(nil, verifier).Router(nil, true).ServeHTTP(recorder, request)
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
	router := New(nil, verifier).Router(nil, true)

	// Full pair: passes the contract validator, fails on the (bad) credential —
	// proof the fields got past additionalProperties:false.
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/scans", bytes.NewBufferString(
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
	request = httptest.NewRequest(http.MethodPost, "/scans", bytes.NewBufferString(
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
	request := httptest.NewRequest(http.MethodPost, "/scans/reconciliations", bytes.NewBufferString(
		`{"occurrences":[`+
			`{"qr_payload":"not-a-ticket","occurrence_id":"`+occA+`","occurred_at":"2026-07-17T09:00:00Z"},`+
			`{"qr_payload":"also-not-a-ticket","occurrence_id":"`+occB+`","occurred_at":"2026-07-17t09:01:00z"},`+
			`{"qr_payload":"not-a-ticket","occurrence_id":"`+malformed+`","occurred_at":"2026-07-17T09:02:00Z"},`+
			`{"qr_payload":"","occurrence_id":"`+uuid.NewString()+`","occurred_at":"2026-07-17T09:03:00Z"}]}`))
	request.Header.Set("Content-Type", "application/json")
	New(nil, verifier).Router(nil, true).ServeHTTP(recorder, request)
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
	request := httptest.NewRequest(http.MethodPost, "/scans", bytes.NewBufferString(
		`{"qr_payload":"not-a-ticket","occurrence_id":"`+uuid.NewString()+`","occurred_at":"2026-07-17t09:00:00z"}`))
	request.Header.Set("Content-Type", "application/json")
	New(nil, verifier).Router(nil, true).ServeHTTP(recorder, request)
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
	request := httptest.NewRequest(http.MethodPost, "/scans/reconciliations", bytes.NewBufferString(
		`{"occurrences":[{"qr_payload":"x","occurrence_id":"`+uuid.NewString()+`","occurred_at":"2026-07-17T09:00:00Z"}]}`))
	request.Header.Set("Content-Type", "application/json")
	New(nil, nil).Router(nil, true).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
}

func TestScanRejectsWhenVerifierIsUnavailable(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/scans", bytes.NewBufferString(`{"qr_payload":"not-a-ticket"}`))
	request.Header.Set("Content-Type", "application/json")
	New(nil, nil).Router(nil, true).ServeHTTP(recorder, request)
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
	router := New(nil, verifier).Router(nil, true)

	// direction gets past the validator: the failure must be the (bad)
	// credential, not the contract.
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/scans", bytes.NewBufferString(
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
	request = httptest.NewRequest(http.MethodPost, "/scans", bytes.NewBufferString(
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
	router := New(nil, verifier).Router(nil, true)

	// event_type passes the contract; the bad credential earns a per-item
	// rejected result, and a malformed event_type is ALSO a per-item rejected
	// result — batch entries never 422 the whole sync (the established
	// batch-item posture).
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/scans/reconciliations", bytes.NewBufferString(
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
