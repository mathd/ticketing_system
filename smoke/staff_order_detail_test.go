//go:build smoke

package smoke_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The staff order read (TKT-201), executed against the RUNNING commerce service.
//
// COS 2 and COS 4 both require execution rather than argument: AGENTS.md is explicit that a
// security claim is a hypothesis until it is executed, and the unit tier cannot settle
// either one. The unit tier builds its own router and proves the handler refuses; only a
// real request through the real service proves the deployed thing refuses.

// getStaffOrderDetail performs the read with exactly the headers given (TKT-287: the
// staff credential AND an organizer assertion; no organizer_id in the query). Through
// commerceStaffCall, so the response is contract-validated and its 200 counted for the
// coverage gate.
func getStaffOrderDetail(t *testing.T, order string, headers map[string]string) (int, []byte) {
	t.Helper()
	return commerceStaffCall(t, http.MethodGet, fmt.Sprintf("%s/internal/orders/%s", commerceURL, order), "", headers, nil)
}

// TestStaffOrderDetailIsRefusedWithoutACredential is COS 2 of TKT-201 and COS3 of TKT-287,
// executed: every predicate, each case otherwise well-formed for an order that EXISTS.
//
// 404 rather than 401 is the contract (ADR-043): commerce's refusal must be
// indistinguishable from the gateway's own edge deny on the same path, so a prober cannot
// learn that the route exists — and a valid assertion for ANOTHER organizer gets the same
// 404, so it cannot learn whose order this is either.
func TestStaffOrderDetailIsRefusedWithoutACredential(t *testing.T) {
	orderID, _, _, _ := consoleFixture(t, "detail-refuse")

	cases := staffRefusals(t)
	cases = append(cases,
		struct {
			name    string
			headers map[string]string
		}{"no credential at all", map[string]string{}},
		struct {
			name    string
			headers map[string]string
		}{"a wrong staff credential with the owner's assertion", map[string]string{
			"X-Commerce-Staff-Write-Token": uuid.NewString(),
			organizerAssertionHeader:       organizerAssertionFor(t, organizerID)}},
		struct {
			name    string
			headers map[string]string
		}{"the staff credential in the internal header", map[string]string{
			"X-Internal-Token":       os.Getenv("SMOKE_COMMERCE_STAFF_WRITE_TOKEN"),
			organizerAssertionHeader: organizerAssertionFor(t, organizerID)}},
	)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := getStaffOrderDetail(t, orderID, tc.headers)
			if code != http.StatusNotFound {
				t.Fatalf("status=%d want 404 — this order EXISTS and the request is well-formed, "+
					"so anything but a refusal here means a guard predicate did not run; body=%.300s",
					code, body)
			}
			// The refusal must not be distinguishable from a missing route by its body either.
			if strings.Contains(string(body), "organizer") || strings.Contains(string(body), "order_id") {
				t.Errorf("the refusal body describes the request; it must say only 'not found': %.300s", body)
			}
		})
	}
}

// TestStaffOrderDetailAnswersMoneyToTheStaffCredential is COS 1 and COS 4, executed.
//
// COS 4 is asserted on the RAW BYTES rather than on a decoded struct, deliberately: a
// struct with no buyer field cannot observe one arriving, so decoding first would make the
// assertion unfalsifiable. The bytes are what leaves the service.
func TestStaffOrderDetailAnswersMoneyToTheStaffCredential(t *testing.T) {
	orderID, _, _, _ := consoleFixture(t, "detail-read")

	code, body := getStaffOrderDetail(t, orderID, staffHeaders(t, organizerID))
	if code != http.StatusOK {
		t.Fatalf("staff read: %d %s", code, body)
	}

	var detail struct {
		OrderID     string `json:"order_id"`
		OrganizerID string `json:"organizer_id"`
		Status      string `json:"status"`
		LineItems   []struct {
			TicketTypeID string `json:"ticket_type_id"`
			Quantity     int64  `json:"quantity"`
			UnitAmount   int64  `json:"unit_amount"`
			FaceValue    int64  `json:"face_value_amount"`
			TotalAmount  int64  `json:"total_amount"`
			Currency     string `json:"currency"`
		} `json:"line_items"`
		Totals struct {
			TotalAmount  int64  `json:"total_amount"`
			FaceValue    int64  `json:"face_value_amount"`
			PassedOnFees int64  `json:"passed_on_fees"`
			RefundStatus string `json:"refund_status"`
			Currency     string `json:"currency"`
		} `json:"totals"`
		Refunds []json.RawMessage `json:"refunds"`
	}
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatalf("decode %.400s: %v", body, err)
	}

	if detail.OrderID != orderID {
		t.Errorf("order_id = %q want %q", detail.OrderID, orderID)
	}
	if len(detail.LineItems) != 1 {
		t.Fatalf("line_items = %d want exactly 1 — an order references exactly one reservation, "+
			"and a reservation names one ticket type; body=%.400s", len(detail.LineItems), body)
	}
	line := detail.LineItems[0]
	if line.Quantity < 1 || line.UnitAmount < 1 || line.TotalAmount < 1 {
		t.Errorf("line = %+v; a completed order must report a real quantity and price", line)
	}
	if len(line.Currency) != 3 || line.Currency != strings.ToUpper(line.Currency) {
		t.Errorf("currency = %q want an uppercase ISO 4217 code (ADR-001)", line.Currency)
	}
	// The invariant, stated without naming the implementation: what the buyer paid is the
	// face value plus the fees passed on to them, exactly, in integers.
	if detail.Totals.FaceValue+detail.Totals.PassedOnFees != detail.Totals.TotalAmount {
		t.Errorf("face %d + passed-on %d != total %d: the three must reconcile exactly, "+
			"which is what makes them integers rather than a rounded share (ADR-001)",
			detail.Totals.FaceValue, detail.Totals.PassedOnFees, detail.Totals.TotalAmount)
	}
	if detail.Totals.PassedOnFees < 0 {
		t.Errorf("passed_on_fees = %d; a table CHECK keeps face <= total, so this cannot be negative",
			detail.Totals.PassedOnFees)
	}
	if detail.Refunds == nil {
		t.Error("refunds is null; an order with no refunds must report [] so a client can tell " +
			"'none' from 'absent'")
	}

	// COS 4, on the bytes. No buyer contact, no buyer identity, in any form.
	for _, forbidden := range []string{
		"buyer_id", "customer_id", "\"name\"", "email", "address", "delivery_email", "buyer",
	} {
		if strings.Contains(string(body), forbidden) {
			t.Errorf("the staff order read leaked %q: this response carries money and no contact, "+
				"and buyer contact lives only at /internal/buyers/{id}/delivery-email (ADR-003); body=%.500s",
				forbidden, body)
		}
	}
}

// A staff caller scoped to a DIFFERENT organizer is refused, through the real service.
//
// The store tier proves the SQL predicate; this proves the deployed path honours it, and
// that the refusal is a 404 rather than an empty detail — an empty answer would read as
// "this order contains nothing", which is a different and wrong claim.
func TestStaffOrderDetailRefusesAnotherOrganizersScope(t *testing.T) {
	orderID, _, _, _ := consoleFixture(t, "detail-scope")

	// The order IS readable by its owner. Without this the refusal below is satisfied by a
	// handler that refuses everything.
	if code, body := getStaffOrderDetail(t, orderID, staffHeaders(t, organizerID)); code != http.StatusOK {
		t.Fatalf("owner read: %d %s", code, body)
	}

	// Since TKT-287 "scoped to another organizer" means a VALID assertion for that
	// organizer: no request field names one any more.
	code, body := getStaffOrderDetail(t, orderID, staffHeaders(t, uuid.NewString()))
	if code != http.StatusNotFound {
		t.Fatalf("cross-organizer read = %d want 404: a valid credential scoped to another "+
			"organizer must not read this order, and must not be told it exists; body=%.300s", code, body)
	}
}
