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
	code, out, _ := availabilityAsPartnerForSlot(t, status, func(string) string { return body })
	return code, out
}

// availabilityAsPartnerForSlot is the same, for tests that need the slot id the
// handler asked about — the identity check compares the answer to the question, so a
// well-formed body has to echo it.
func availabilityAsPartnerForSlot(t *testing.T, status int, body func(slot string) string) (int, map[string]any, string) {
	t.Helper()
	slot := uuid.New().String()
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body(slot)))
	}))
	defer inventory.Close()

	srv := newTestServer(nil, http.DefaultClient, "", inventory.URL, "", "secret")
	req := httptest.NewRequest(http.MethodGet,
		"/partners/availability?slot_id="+slot, nil)
	scope := &partnerScope{
		CredentialID: uuid.New(), ResellerID: uuid.New(),
		OrganizerID: uuid.New(), ChannelCode: "reseller-acme",
	}
	req = req.WithContext(context.WithValue(req.Context(), partnerScopeKey{}, scope))
	rec := httptest.NewRecorder()
	srv.partnerAvailability(rec, req)

	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, slot
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
		// The three below are TYPE errors, and they are the cases that matter most:
		// encoding/json populates the field BEFORE it reports the error, so the nil
		// guard alone lets them through. `a string` yields &0 — a fabricated sellout —
		// and the duplicate-key cases yield &7, a number the upstream never asserted.
		// The first version of this fix dropped the decode check believing these could
		// not happen (ai-review [high]).
		"a string where a number belongs": `{"available":"bad"}`,
		"a duplicate key, valid then bad": `{"available":7,"available":"bad"}`,
		"a duplicate key, bad then valid": `{"available":"bad","available":7}`,
		"a float where an int belongs":    `{"available":1.5}`,
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
// This case needs the NIL guard, and the type-error cases above need the DECODE
// guard. Neither subsumes the other, which is the whole reason the handler carries
// both — see its comment for the false argument that briefly removed one of them.
// Each guard is mutation-checked separately below.
func TestPartnerAvailabilityRefusesAnInventoryBodyMissingAvailable(t *testing.T) {
	for name, body := range map[string]string{
		"a body with other fields only": `{"slot_id":"00000000-0000-0000-0000-000000000009","capacity":100}`,
		// Every one of these decodes CLEANLY, so the decode guard lets them past and
		// this is the only thing standing between them and a fabricated sellout.
		// Enumerated rather than assumed (ai-review pass 2 asked which shapes were
		// still missing): an error envelope is what inventory actually sends when
		// something is wrong, and an explicit null is what a serialiser emits for an
		// absent optional.
		"inventory's error envelope": `{"error":"slot not found"}`,
		"an explicit null":           `{"available":null}`,
		"an empty object":            `{}`,
		"the JSON null literal":      `null`,
		"the field nested a level":   `{"data":{"available":5}}`,
	} {
		t.Run(name, func(t *testing.T) {
			code, out := availabilityAsPartner(t, http.StatusOK, body)
			if code != http.StatusBadGateway {
				t.Fatalf("an inventory 200 with no readable `available` answered %d %v, want 502 — "+
					"the field is required by inventory's contract, so its absence is a broken "+
					"answer and not a sellout", code, out)
			}
		})
	}
}

// The honest sellout still reads as a sellout. Without this the fix could be
// "answer 502 always", which would pass every assertion above and break the
// endpoint's actual job.
func TestPartnerAvailabilityStillReportsAGenuineSellout(t *testing.T) {
	code, out, _ := availabilityAsPartnerForSlot(t, http.StatusOK, func(slot string) string {
		return `{"slot_id":"` + slot + `","capacity":100,"held":0,"confirmed":100,"available":0,"offering_status":"open"}`
	})
	if code != http.StatusOK {
		t.Fatalf("a well-formed sold-out answer became %d %v, want 200: an actual sellout is a "+
			"fact a reseller must still be able to read", code, out)
	}
	if out["available"] != float64(0) {
		t.Fatalf("available = %v, want 0", out["available"])
	}
}

// An answer ABOUT ANOTHER SLOT is refused, not republished under the requested id.
//
// The body is a perfectly valid Availability — it decodes, it carries a non-nil
// non-negative `available` — so every other guard in the handler passes it. Only the
// identity check can see it. Inventory reads the slot from a path parameter through a
// cache (availability.Read), so a cache keyed or invalidated wrongly is the realistic
// way this arrives; the effect is a reseller acting on a number inventory never
// asserted about their slot (ai-review pass 2 [medium]).
func TestPartnerAvailabilityRefusesAnAnswerAboutAnotherSlot(t *testing.T) {
	other := uuid.New().String()
	code, out, asked := availabilityAsPartnerForSlot(t, http.StatusOK, func(string) string {
		return `{"slot_id":"` + other + `","capacity":100,"held":0,"confirmed":40,"available":60,"offering_status":"open"}`
	})
	if code != http.StatusBadGateway {
		t.Fatalf("an Availability for slot %s answered %d %v when %s was asked about, want 502 — "+
			"republishing it under the requested id reports a number inventory never asserted "+
			"about this slot", other, code, out, asked)
	}
}

// A NEGATIVE availability is refused, not clamped to zero.
//
// It used to be clamped, on the argument that "less than nothing available" and
// "nothing available" are the same fact to a seller. They are not the same fact about
// INVENTORY: a negative means the upstream's arithmetic is wrong, and rounding it into
// a sellout performs exactly the substitution this ticket exists to stop — the reseller
// stops selling and the one signal that something is broken is gone (ai-review pass 2).
func TestPartnerAvailabilityRefusesANegativeCount(t *testing.T) {
	code, out, _ := availabilityAsPartnerForSlot(t, http.StatusOK, func(slot string) string {
		return `{"slot_id":"` + slot + `","capacity":100,"held":0,"confirmed":140,"available":-40,"offering_status":"open"}`
	})
	if code != http.StatusBadGateway {
		t.Fatalf("a negative availability answered %d %v, want 502 — a negative count is a broken "+
			"upstream, and clamping it to 0 tells a reseller the show is sold out", code, out)
	}
}
