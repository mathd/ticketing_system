package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"ticketing/services/access/internal/ticket"
)

func TestScanRejectsTrailingJSONBeforeRedeeming(t *testing.T) {
	verifier, err := ticket.NewVerifier("access-qr/test-v1=O2onvM62pC1io6jQKm8Nc2UyFXcd4kOmOsBIoYtZ2ik", "access-qr/test-v1")
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/scans", bytes.NewBufferString(`{"qr_payload":"not-a-ticket"}{}`))
	New(nil, verifier).Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnprocessableEntity)
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
