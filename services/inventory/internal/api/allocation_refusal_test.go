package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"ticketing/services/inventory/internal/store"
)

// TKT-244. The allocation editor must put a refusal BESIDE THE FIELD the operator has to
// fix, and there are exactly two server-side refusals to attribute:
//
//   - the caps sum above the pool's capacity   → belongs on the total
//   - one cap is below that channel's current consumption → belongs on THAT channel's row
//
// Before this ticket both arrived as a bare 409 carrying only `{"error": …}`
// (ErrUnavailable and ErrConflict respectively, collapsed by problem()), so a client
// could not tell them apart by status, and the second never said WHICH channel.
//
// Why the client cannot re-derive this locally, which is how TKT-236 solved the
// equivalent problem for the channel form: those bounds are STATIC (a code's length), so
// re-checking the submitted values locally gives the same answer the server got.
// Consumption is LIVE — it changes between the GET that populated the form and the PUT
// that submits it — so a local re-derivation can name the wrong row with full confidence.
// The server is the only party that knows which channel actually failed.
func TestAllocationRefusalsCarryAMachineReadableCodeAndTheOffendingChannel(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    string
		wantChannel string
	}{
		{
			name:       "caps above pool capacity names no channel: every row shares the blame",
			err:        store.ErrAllocationCapsExceedCapacity,
			wantStatus: http.StatusConflict,
			wantCode:   "allocation_caps_exceed_capacity",
		},
		{
			name:        "a cap below consumption names the exact channel",
			err:         store.AllocationCapBelowConsumption("reseller-acme"),
			wantStatus:  http.StatusConflict,
			wantCode:    "allocation_cap_below_consumption",
			wantChannel: "reseller-acme",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := httptest.NewRecorder()
			problem(res, tc.err)

			if res.Code != tc.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", res.Code, tc.wantStatus, res.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body %q: %v", res.Body.String(), err)
			}
			if got, _ := body["code"].(string); got != tc.wantCode {
				t.Errorf("code=%q want=%q (body=%s)", got, tc.wantCode, res.Body.String())
			}
			got, present := body["channel"].(string)
			if tc.wantChannel == "" {
				if present {
					t.Errorf("channel=%q present on a refusal that names no single channel; "+
						"attributing it to a row would point the operator at the wrong field", got)
				}
			} else if got != tc.wantChannel {
				t.Errorf("channel=%q want=%q", got, tc.wantChannel)
			}
		})
	}
}

// The two typed errors must keep answering 409 through the sentinels they wrap, so every
// existing caller that matches on ErrUnavailable/ErrConflict still behaves identically.
// This is what makes the change additive rather than a re-classification.
func TestTypedAllocationErrorsStillUnwrapToTheirSentinels(t *testing.T) {
	if !errors.Is(store.ErrAllocationCapsExceedCapacity, store.ErrUnavailable) {
		t.Error("ErrAllocationCapsExceedCapacity must unwrap to ErrUnavailable: " +
			"a caller matching the sentinel would otherwise stop recognising this refusal")
	}
	if !errors.Is(store.AllocationCapBelowConsumption("x"), store.ErrConflict) {
		t.Error("AllocationCapBelowConsumption must unwrap to ErrConflict")
	}
}

// A channel code is operator-supplied and opaque (ADR-024: no normalization, no case
// folding). It is echoed back into a JSON body, so it must survive verbatim — including
// the characters an implementation might be tempted to strip.
func TestTheEchoedChannelSurvivesVerbatim(t *testing.T) {
	for _, channel := range []string{"reseller-acme", "POS/Booth #2", `quote"inside`, "présale"} {
		res := httptest.NewRecorder()
		problem(res, store.AllocationCapBelowConsumption(channel))
		var body map[string]any
		if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
			t.Fatalf("channel %q: decode %q: %v", channel, res.Body.String(), err)
		}
		if got, _ := body["channel"].(string); got != channel {
			t.Errorf("channel=%q want=%q", got, channel)
		}
	}
}
