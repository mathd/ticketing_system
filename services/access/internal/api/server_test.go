package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
			New(nil, verifier).Router().ServeHTTP(recorder, request)
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

func TestScanRejectsWhenVerifierIsUnavailable(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/scans", bytes.NewBufferString(`{"qr_payload":"not-a-ticket"}`))
	request.Header.Set("Content-Type", "application/json")
	New(nil, nil).Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
}
