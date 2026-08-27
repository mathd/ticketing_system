//go:build smoke

package smoke_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The staff order read (TKT-201), executed against the RUNNING commerce service.
//
// COS 2 and COS 4 both require execution rather than argument: AGENTS.md is explicit that a
// security claim is a hypothesis until it is executed, and the unit tier cannot settle
// either one. The unit tier builds its own router and proves the handler refuses; only a
// real request through the real service proves the deployed thing refuses.

// getStaffOrderDetail performs the read with whatever credential the caller supplies.
//
// Deliberately NOT internalJSON: that helper always sets X-Internal-Token, so it cannot
// express the case COS 2 is about — a request carrying no credential at all.
func getStaffOrderDetail(t *testing.T, order, org, header, token string) (int, []byte) {
	t.Helper()
	url := fmt.Sprintf("%s/internal/orders/%s?organizer_id=%s", commerceURL, order, org)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("bad request: %v", err)
	}
	if header != "" {
		req.Header.Set(header, token)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	// Through the suite's chokepoint, like every other direct-service call. Two things
	// depend on it and neither is optional: the response is CONTRACT-VALIDATED against
	// commerce's OpenAPI (so a body that drifts from the schema fails here rather than in
	// a client months later), and the 200 is recorded for the coverage gate, which fails
	// the suite when a documented 2xx operation has no happy path driving it. A
	// hand-rolled client that skips this passes its own assertions and leaves the
	// operation looking untested — which is exactly what it would be.
	if service := directService(url); service != "" {
		if err := checkDirectServiceResponse(service, resp.Request, resp.StatusCode, resp.Header, out); err != nil {
			t.Fatalf("%v", err)
		}
	}
	return resp.StatusCode, out
}

// TestStaffOrderDetailIsRefusedWithoutACredential is COS 2, executed.
//
// EVERY request here is otherwise WELL-FORMED — a real order id and a real organizer id.
// That is load-bearing rather than tidy: organizer_id is a required query parameter, so the
// contract validator answers 400 for a request that omits it before the credential is ever
// compared. A refusal test that left it out would be green because the request was
// malformed, and would say nothing at all about the credential. Each case below is a
// request commerce would happily serve if only it were credentialed, and is refused anyway.
//
// 404 rather than 401 is the contract (ADR-043): commerce's refusal must be
// indistinguishable from the gateway's own edge deny on the same path, so a prober cannot
// learn that the route exists.
func TestStaffOrderDetailIsRefusedWithoutACredential(t *testing.T) {
	orderID, _, _, _ := consoleFixture(t, "detail-refuse")

	for _, tc := range []struct {
		name   string
		header string
		token  string
	}{
		{"no credential at all", "", ""},
		{"a wrong staff credential", "X-Commerce-Staff-Write-Token", uuid.NewString()},
		{"a wrong internal token", "X-Internal-Token", uuid.NewString()},
		{"the staff credential in the internal header", "X-Internal-Token", os.Getenv("SMOKE_COMMERCE_STAFF_WRITE_TOKEN")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := getStaffOrderDetail(t, orderID, organizerID, tc.header, tc.token)
			if code != http.StatusNotFound {
				t.Fatalf("status=%d want 404 — this order EXISTS and the request is well-formed, "+
					"so anything but a refusal here means the credential check did not run; body=%.300s",
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

	code, body := getStaffOrderDetail(t, orderID, organizerID,
		"X-Commerce-Staff-Write-Token", os.Getenv("SMOKE_COMMERCE_STAFF_WRITE_TOKEN"))
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
	if code, body := getStaffOrderDetail(t, orderID, organizerID,
		"X-Commerce-Staff-Write-Token", os.Getenv("SMOKE_COMMERCE_STAFF_WRITE_TOKEN")); code != http.StatusOK {
		t.Fatalf("owner read: %d %s", code, body)
	}

	code, body := getStaffOrderDetail(t, orderID, uuid.NewString(),
		"X-Commerce-Staff-Write-Token", os.Getenv("SMOKE_COMMERCE_STAFF_WRITE_TOKEN"))
	if code != http.StatusNotFound {
		t.Fatalf("cross-organizer read = %d want 404: a valid credential scoped to another "+
			"organizer must not read this order, and must not be told it exists; body=%.300s", code, body)
	}
}
