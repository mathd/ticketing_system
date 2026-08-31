package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// Absent versus broken, on the one path with a real consumer (TKT-305).
//
// A reseller polls this endpoint to decide whether to keep selling. `available: 0`
// is a FACT about the slot — sold out, stop offering it — and a reseller acts on it
// by backing off. An inventory outage is not that fact, and answering it with the
// same body tells a partner to stop selling a show that has seats.
//
// Every sibling in this service already draws the line: server.go answers 502
// "invalid inventory response" for a hold body it cannot read. This endpoint was the
// straggler.

// availabilityAsPartner drives one partner availability read against an inventory
// stub that answers `status` with `body`, and returns commerce's status and decoded
// response.
//
// The scope is injected rather than authenticated for the same reason
// partner_reserve_test.go injects it: authentication needs a database, and what is
// under test is what the handler does with an upstream answer once authenticated.
func availabilityAsPartner(t *testing.T, status int, body string) (int, map[string]any) {
	t.Helper()
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer inventory.Close()

	srv := New(nil, http.DefaultClient, "", inventory.URL, "", "secret")
	req := httptest.NewRequest(http.MethodGet,
		"/partners/availability?slot_id="+uuid.New().String(), nil)
	scope := &partnerScope{
		CredentialID: uuid.New(), ResellerID: uuid.New(),
		OrganizerID: uuid.New(), ChannelCode: "reseller-acme",
	}
	req = req.WithContext(context.WithValue(req.Context(), partnerScopeKey{}, scope))
	rec := httptest.NewRecorder()
	srv.partnerAvailability(rec, req)

	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// An inventory 200 whose body cannot be decoded is a 502, never `available: 0`.
//
// The defect this pins: `_ = json.Unmarshal(...)` discarded the error and fell
// through two nil pointers to `available = 0`, so a 200 carrying an HTML error page
// or a truncated body was indistinguishable from a genuine sellout.
//
// Asserting the STATUS alone would be too weak — a handler that 500'd would also
// satisfy "not 200". 502 specifically is the claim: the upstream answered and its
// answer was unusable, which is a retryable condition a partner should treat as
// "ask again", not as "stop selling".
func TestPartnerAvailabilityRefusesAnUndecodableInventoryBody(t *testing.T) {
	for name, body := range map[string]string{
		"an HTML error page": `<html><body>502 Bad Gateway</body></html>`,
		"a truncated body":   `{"slot_id":"x","avail`,
		"a JSON array":       `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			code, out := availabilityAsPartner(t, http.StatusOK, body)
			if code != http.StatusBadGateway {
				t.Fatalf("an undecodable inventory 200 answered %d %v, want 502 — a body commerce "+
					"could not read is a broken upstream, and reporting it as available:0 tells a "+
					"reseller to stop selling a show that may have seats", code, out)
			}
			if _, reported := out["available"]; reported {
				t.Errorf("the refusal still carried an availability figure %v: a 502 must not "+
					"assert anything about the slot", out)
			}
		})
	}
}

// A DECODABLE body that omits `available` is also a 502, and this is the case that
// makes the handler's SINGLE guard the right shape.
//
// `available` is required by inventory's Availability schema, so its absence is a
// contract violation, not a slot with no seats. The old code could not tell the
// difference: both `{}` and `{"available":0}` produced `available: 0`, because the
// field was decoded into a *int whose nil case defaulted to zero.
//
// This case is ALSO why the fix does not check the decode error separately. A
// mutation established it: restoring `_ = json.Unmarshal(...)` changed no result
// here, because encoding/json never leaves a pointer field populated on a body it
// rejected — so every undecodable body arrives nil and the test above is really
// exercising this same guard. Two checks would have read as two defences while one
// was unreachable. The nil test alone covers both, and deleting it turns all four
// assertions in this file red.
func TestPartnerAvailabilityRefusesAnInventoryBodyMissingAvailable(t *testing.T) {
	code, out := availabilityAsPartner(t, http.StatusOK, `{"slot_id":"00000000-0000-0000-0000-000000000009","capacity":100}`)
	if code != http.StatusBadGateway {
		t.Fatalf("an inventory 200 with no `available` answered %d %v, want 502 — the field is "+
			"required by inventory's contract, so its absence is a broken answer and not a sellout",
			code, out)
	}
}

// The honest sellout still reads as a sellout. Without this the fix could be
// "answer 502 always", which would pass every assertion above and break the
// endpoint's actual job.
func TestPartnerAvailabilityStillReportsAGenuineSellout(t *testing.T) {
	code, out := availabilityAsPartner(t, http.StatusOK,
		`{"slot_id":"00000000-0000-0000-0000-000000000009","capacity":100,"held":0,"confirmed":100,"available":0,"offering_status":"open"}`)
	if code != http.StatusOK {
		t.Fatalf("a well-formed sold-out answer became %d %v, want 200: an actual sellout is a "+
			"fact a reseller must still be able to read", code, out)
	}
	if out["available"] != float64(0) {
		t.Fatalf("available = %v, want 0", out["available"])
	}
}
