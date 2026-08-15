//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

func inventoryStaffToken() string { return os.Getenv("SMOKE_INVENTORY_STAFF_WRITE_TOKEN") }

// credentialledRequest drives one operation presenting ONE named credential and nothing
// else — deliberately NOT internalRequest, which always sets X-Internal-Token and so
// could never show what a lone staff credential opens.
//
// It runs the same direct-service contract check internalRequest does, so a response that
// violates inventory's document fails here rather than passing quietly.
func credentialledRequest(t *testing.T, method, url, header, value string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("bad request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(header, value)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if service := directService(url); service != "" {
		if err := checkDirectServiceResponse(service, resp.Request, resp.StatusCode, resp.Header, out); err != nil {
			t.Fatalf("%v", err)
		}
	}
	return resp.StatusCode, out
}

// staffCredentialRequest drives one inventory operation presenting ONLY the back office's
// inventory credential — never the shared internal token.
func staffCredentialRequest(t *testing.T, method, url string, body any) (int, []byte) {
	t.Helper()
	return credentialledRequest(t, method, url, "X-Inventory-Staff-Write-Token", inventoryStaffToken(), body)
}

// TKT-244 / ADR-057. The back office's inventory credential, against the real service on
// the real network — which is where the wiring this ticket adds either works or does not.
//
// The unit suite proves the guard's DECISION (which routes accept which credential) and
// the router walk proves it stays narrow. Neither can prove that compose actually supplies
// the value, that inventory read it at startup, or that the credential a developer's stack
// generates is the one the service compares against. That is this test's job.
func TestBackOfficeInventoryCredentialOpensTheAllocationEditorsTwoOperations(t *testing.T) {
	if inventoryStaffToken() == "" {
		t.Fatal("SMOKE_INVENTORY_STAFF_WRITE_TOKEN is not set: the credential could not be exercised")
	}
	slot, _ := publishedSlot(t, "Inventory Staff Credential Hall", 50)

	availabilityURL := fmt.Sprintf("%s/internal/slots/%s/availability?organizer_id=%s", inventoryURL, slot, organizerID)
	allocURL := fmt.Sprintf("%s/internal/slots/%s/channel-allocations", inventoryURL, slot)

	// The read. The editor needs it for current consumption, which is a condition of
	// success and appears in no other read.
	code, body := staffCredentialRequest(t, http.MethodGet, availabilityURL, nil)
	if code != http.StatusOK {
		t.Fatalf("staff availability with the inventory credential: %d %s", code, body)
	}

	// The write, carrying every field the editor round-trips. sold_by is the one that
	// matters: a save that dropped it would return a reseller's stock to the public pool.
	reseller := "44444444-4444-4444-4444-444444444444"
	code, body = staffCredentialRequest(t, http.MethodPut, allocURL, map[string]any{
		"organizer_id": organizerID,
		"allocations": []map[string]any{
			{"channel": "reseller-staffcred", "cap": 10, "requires_code": true, "sold_by": reseller},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("allocation replace with the inventory credential: %d %s", code, body)
	}

	// The read reports the two fields TKT-244 added, which is what makes a safe editor
	// round trip possible at all: the write is a full-set replace, so a field the editor
	// cannot READ is a field it destroys on the next save.
	code, body = staffCredentialRequest(t, http.MethodGet, availabilityURL, nil)
	if code != http.StatusOK {
		t.Fatalf("staff availability re-read: %d %s", code, body)
	}
	var read struct {
		Channels []struct {
			Channel      string  `json:"channel"`
			Cap          int     `json:"cap"`
			RequiresCode bool    `json:"requires_code"`
			SoldBy       *string `json:"sold_by"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(body, &read); err != nil {
		t.Fatalf("decode staff availability: %v", err)
	}
	var found bool
	for _, c := range read.Channels {
		if c.Channel != "reseller-staffcred" {
			continue
		}
		found = true
		if !c.RequiresCode {
			t.Error("the staff read does not report requires_code, so an editor cannot preserve it")
		}
		if c.SoldBy == nil || *c.SoldBy != reseller {
			t.Errorf("the staff read reports sold_by=%v, want %q — an editor cannot preserve a "+
				"binding it cannot see, and re-saving would unbind a reseller's stock", c.SoldBy, reseller)
		}
	}
	if !found {
		t.Fatalf("the allocation just written is absent from the staff read: %s", body)
	}
}

// The narrowness of the grant, against the real service. ADR-057 opens two operations;
// the unit suite walks the router to prove the rest refuse, and this confirms the
// deployed binary agrees on a representative one.
func TestBackOfficeInventoryCredentialIsRefusedElsewhereOnTheInternalSurface(t *testing.T) {
	if inventoryStaffToken() == "" {
		t.Fatal("SMOKE_INVENTORY_STAFF_WRITE_TOKEN is not set: the credential could not be exercised")
	}
	slot, _ := publishedSlot(t, "Inventory Staff Credential Narrowness Hall", 20)

	// Capacity adjustment sits one path segment away from the allocation route and is a
	// far more powerful operation. It must refuse this credential.
	code, body := credentialledRequest(t, http.MethodPost,
		fmt.Sprintf("%s/internal/slots/%s/capacity-adjustments", inventoryURL, slot),
		"X-Inventory-Staff-Write-Token", inventoryStaffToken(),
		map[string]any{"organizer_id": organizerID, "capacity": 10, "actor": "staff:amy", "reason": "probe"})
	// 401, not 400: a 400 would mean the request validator refused the body and the
	// credential check never ran, so the probe would prove nothing.
	if code != http.StatusUnauthorized {
		t.Fatalf("capacity adjustment with the inventory staff credential: %d %s, want 401", code, body)
	}

	// The availability CACHE kill-switch, likewise.
	code, body = credentialledRequest(t, http.MethodPut, inventoryURL+"/internal/cache-control",
		"X-Inventory-Staff-Write-Token", inventoryStaffToken(), map[string]any{"enabled": true})
	if code != http.StatusUnauthorized {
		t.Fatalf("cache control with the inventory staff credential: %d %s, want 401", code, body)
	}
}

// The shared internal token must keep working on the two routes the new credential also
// opens: this is an ADDITIONAL accepted credential, not a replacement, and five other
// smoke drivers depend on that.
func TestTheSharedInternalTokenStillDrivesTheAllocationRoutes(t *testing.T) {
	slot, _ := publishedSlot(t, "Inventory Shared Token Hall", 30)
	code, body := internalJSON(t, http.MethodPut,
		fmt.Sprintf("%s/internal/slots/%s/channel-allocations", inventoryURL, slot), "",
		map[string]any{
			"organizer_id": organizerID,
			"allocations":  []map[string]any{{"channel": "pos-shared", "cap": 5}},
		})
	if code != http.StatusOK {
		t.Fatalf("allocation replace with the shared internal token: %d %s", code, body)
	}
	code, body = internalJSON(t, http.MethodGet,
		fmt.Sprintf("%s/internal/slots/%s/availability?organizer_id=%s", inventoryURL, slot, organizerID), "", nil)
	if code != http.StatusOK {
		t.Fatalf("staff availability with the shared internal token: %d %s", code, body)
	}
}

// The two allocation refusals carry a machine-readable code, and the cap-below-consumption
// one names its channel — which is what lets the editor put each message beside the field
// an operator must fix. Asserted on the WIRE, because the contract's Error schema is
// closed (additionalProperties: false, a closed `code` enum) and response validation runs:
// a code the document does not declare would fail here even if the handler emitted it.
func TestAllocationRefusalsAreCodedOnTheWire(t *testing.T) {
	slot, _ := publishedSlot(t, "Allocation Refusal Hall", 40)
	allocURL := fmt.Sprintf("%s/internal/slots/%s/channel-allocations", inventoryURL, slot)

	// Caps summing above the pool's capacity: names no channel, because the sum is a
	// property of the whole submitted set.
	code, body := internalJSON(t, http.MethodPut, allocURL, "", map[string]any{
		"organizer_id": organizerID,
		"allocations": []map[string]any{
			{"channel": "a-over", "cap": 30}, {"channel": "b-over", "cap": 30},
		},
	})
	if code != http.StatusConflict {
		t.Fatalf("over-capacity: %d %s, want 409", code, body)
	}
	var refusal struct {
		Code    string `json:"code"`
		Channel string `json:"channel"`
	}
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatalf("decode refusal %s: %v", body, err)
	}
	if refusal.Code != "allocation_caps_exceed_capacity" {
		t.Errorf("code=%q want allocation_caps_exceed_capacity (body=%s)", refusal.Code, body)
	}
	if refusal.Channel != "" {
		t.Errorf("channel=%q on a whole-set refusal: attributing it to one row would point "+
			"the operator at an arbitrary field", refusal.Channel)
	}

	// A cap below that channel's live consumption: names the offending channel.
	if code, body = internalJSON(t, http.MethodPut, allocURL, "", map[string]any{
		"organizer_id": organizerID,
		"allocations":  []map[string]any{{"channel": "consumed", "cap": 20}},
	}); code != http.StatusOK {
		t.Fatalf("seed allocation: %d %s", code, body)
	}
	if code, body = postJSON(t, gatewayURL+"/api/inventory/holds", map[string]any{
		"organizer_id": organizerID, "slot_id": slot, "quantity": 6,
		"unit_amount": 1000, "currency": "EUR", "channel": "consumed",
	}); code != http.StatusCreated {
		t.Fatalf("seed hold: %d %s", code, body)
	}
	code, body = internalJSON(t, http.MethodPut, allocURL, "", map[string]any{
		"organizer_id": organizerID,
		"allocations":  []map[string]any{{"channel": "consumed", "cap": 2}},
	})
	if code != http.StatusConflict {
		t.Fatalf("below-consumption: %d %s, want 409", code, body)
	}
	refusal.Code, refusal.Channel = "", ""
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatalf("decode refusal %s: %v", body, err)
	}
	if refusal.Code != "allocation_cap_below_consumption" {
		t.Errorf("code=%q want allocation_cap_below_consumption (body=%s)", refusal.Code, body)
	}
	if refusal.Channel != "consumed" {
		t.Errorf("channel=%q want %q — without it the editor cannot find the row to flag",
			refusal.Channel, "consumed")
	}
}
