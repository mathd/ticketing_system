//go:build smoke

package smoke_test

// Event-cancellation bulk refunds through the running stack (TKT-159, ADR-040).
//
// AC 2 and AC 5 are the reason this test exists at the whole-stack seam rather than in a
// unit suite: "no order is refunded twice" is a claim about commerce, payments, access and
// inventory agreeing, and a fake cannot be wrong about that in the same way the real
// services can. It also drives both new commerce operations directly, which is what
// registers them with the ADR-030 coverage gate — commerce's own service-to-service calls
// never reach the chokepoints.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

type cancellationRun struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
	Replay bool   `json:"replay"`
}

type cancellationReport struct {
	Run    cancellationRun `json:"run"`
	Counts struct {
		Total           int `json:"total"`
		Refunded        int `json:"refunded"`
		AlreadyRefunded int `json:"already_refunded"`
		Failed          int `json:"failed"`
		Pending         int `json:"pending"`
	} `json:"counts"`
	IncompleteAtEnumeration int `json:"incomplete_at_enumeration"`
	Orders             []struct {
		OrderID          string `json:"order_id"`
		Outcome          string `json:"outcome"`
		RefundID         string `json:"refund_id"`
		RefundedQuantity int    `json:"refunded_quantity"`
		RefundedAmount   int64  `json:"refunded_amount"`
		MoneyRefunded    bool   `json:"money_refunded"`
		TicketsVoided    bool   `json:"tickets_voided"`
		CapacityReturned bool   `json:"capacity_returned"`
		Failure          struct {
			Code   string `json:"code"`
			Reason string `json:"reason"`
		} `json:"failure"`
	} `json:"orders"`
}

// buyOne completes one purchase on the slot and returns its order id.
func buyOne(t *testing.T, slot, tt, key string, quantity int) string {
	t.Helper()
	code, body := postWithKey(t, gatewayURL+"/api/commerce/reservations", "res-"+key,
		map[string]any{"organizer_id": organizerID, "ticket_type_id": tt, "quantity": quantity})
	if code != http.StatusCreated {
		t.Fatalf("reserve %s: %d %s", key, code, body)
	}
	var reservation map[string]any
	if err := json.Unmarshal(body, &reservation); err != nil {
		t.Fatal(err)
	}
	code, body = postWithKey(t, gatewayURL+"/api/commerce/orders", "ord-"+key,
		map[string]any{"reservation_id": reservation["reservation_id"], "name": "Cancelled Buyer",
			"email": "cancel-" + key + "@example.test", "payment_token": "fake-ok"})
	if code != http.StatusOK {
		t.Fatalf("checkout %s: %d %s", key, code, body)
	}
	var order struct {
		OrderID       string `json:"order_id"`
		GuestOrderRef string `json:"guest_order_ref"`
	}
	if err := json.Unmarshal(body, &order); err != nil {
		t.Fatal(err)
	}
	// Wait for issuance before the cancellation: voiding drives access, and access answers
	// 503 until the tickets exist (its outbox/JetStream path is asynchronous). Cancelling
	// earlier would make tickets_voided a race, and the run would honestly report
	// `failed/reversal_outstanding` for a reason that has nothing to do with the feature.
	retry(t, 30*time.Second, func() error {
		code, body, _ := getWithHeaders(t, gatewayURL+"/api/access/orders/"+order.GuestOrderRef+"/tickets")
		if code != http.StatusOK {
			return fmt.Errorf("ticket bundle %d %s", code, body)
		}
		var bundle struct {
			Tickets []struct {
				QRPayload string `json:"qr_payload"`
			} `json:"tickets"`
		}
		if err := json.Unmarshal(body, &bundle); err != nil {
			return err
		}
		if len(bundle.Tickets) != quantity {
			return fmt.Errorf("issued %d tickets, want %d", len(bundle.Tickets), quantity)
		}
		return nil
	})
	return order.OrderID
}

// startRun creates a cancellation refund run and returns it.
func startRun(t *testing.T, slot, key string) cancellationRun {
	t.Helper()
	code, body := internalJSON(t, http.MethodPost,
		fmt.Sprintf("%s/internal/slots/%s/cancellation-refunds", commerceURL, slot), key,
		map[string]any{"organizer_id": organizerID, "actor": "ops@example.test", "reason": "event cancelled"})
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("create cancellation run: %d %s", code, body)
	}
	var run cancellationRun
	if err := json.Unmarshal(body, &run); err != nil {
		t.Fatal(err)
	}
	return run
}

// awaitReport polls the report until the run completes. 202 is the run still working; the
// counts are served either way, which is what an operator watches.
func awaitReport(t *testing.T, runID string) cancellationReport {
	t.Helper()
	var report cancellationReport
	retry(t, 90*time.Second, func() error {
		code, body := internalJSON(t, http.MethodGet,
			fmt.Sprintf("%s/internal/cancellation-refunds/%s?organizer_id=%s", commerceURL, runID, organizerID), "", nil)
		if code != http.StatusOK && code != http.StatusAccepted {
			return fmt.Errorf("report %d %s", code, body)
		}
		if err := json.Unmarshal(body, &report); err != nil {
			return err
		}
		if code == http.StatusAccepted {
			return fmt.Errorf("run still %s (%d pending)", report.Run.Status, report.Counts.Pending)
		}
		return nil
	})
	return report
}

// AC 1, 2, 3, 5, 7: cancelling a slot refunds its whole book, the report is readable and
// organizer-scoped, and a SECOND run over the same slot refunds nobody twice — it reports
// every order as already-refunded.
func TestEventCancellationRefundsTheBookAndIsIdempotent(t *testing.T) {
	slot, tt := publishedSlot(t, "Cancelled Hall", 20)
	first := buyOne(t, slot, tt, "cancel-a-"+slot, 2)
	second := buyOne(t, slot, tt, "cancel-b-"+slot, 1)

	run := startRun(t, slot, "cancel-run-1-"+slot)
	// Creating the same run again is a replay of the SAME run, not a second one.
	if replay := startRun(t, slot, "cancel-run-1-"+slot); replay.RunID != run.RunID || !replay.Replay {
		t.Fatalf("replayed create = %+v, want the same run reported as a replay", replay)
	}

	report := awaitReport(t, run.RunID)
	if report.Counts.Total != 2 || report.Counts.Refunded != 2 {
		t.Fatalf("counts = %+v, want both orders refunded", report.Counts)
	}
	if report.Counts.Failed != 0 || report.Counts.Pending != 0 {
		t.Fatalf("counts = %+v, want no failures and nothing pending", report.Counts)
	}
	if report.IncompleteAtEnumeration != 0 {
		t.Fatalf("incomplete_at_enumeration = %d, want 0 — every order on the slot was completed", report.IncompleteAtEnumeration)
	}
	if len(report.Orders) != 2 {
		t.Fatalf("report rows = %d, want 2", len(report.Orders))
	}
	seen := map[string]bool{}
	for _, o := range report.Orders {
		seen[o.OrderID] = true
		// A success means EVERY obligation is discharged — money back, tickets void, seat
		// returned. Reporting money-only as `refunded` is what ADR-039 forbids.
		if o.Outcome != "refunded" || !o.MoneyRefunded || !o.TicketsVoided || !o.CapacityReturned {
			t.Fatalf("order %s = %+v, want refunded with every obligation discharged", o.OrderID, o)
		}
		if o.RefundID == "" || o.RefundedAmount <= 0 {
			t.Fatalf("order %s reports no refund identity or no money: %+v", o.OrderID, o)
		}
	}
	if !seen[first] || !seen[second] {
		t.Fatalf("report is missing an order of the book: %v", seen)
	}

	// AC 2, the load-bearing one: a SECOND run over the same slot, under a different run
	// key. Nothing may be refunded twice, and every order must come back as
	// already-refunded rather than as a failure or a fresh refund.
	repeat := startRun(t, slot, "cancel-run-2-"+slot)
	if repeat.RunID == run.RunID {
		t.Fatal("a distinct idempotency key must start a distinct run")
	}
	repeatReport := awaitReport(t, repeat.RunID)
	if repeatReport.Counts.Total != 2 || repeatReport.Counts.AlreadyRefunded != 2 {
		t.Fatalf("second run counts = %+v, want both orders already_refunded", repeatReport.Counts)
	}
	if repeatReport.Counts.Refunded != 0 {
		t.Fatalf("the second run refunded %d orders again — a double refund", repeatReport.Counts.Refunded)
	}
	for _, o := range repeatReport.Orders {
		if o.Outcome != "already_refunded" {
			t.Fatalf("order %s on the second run = %q, want already_refunded", o.OrderID, o.Outcome)
		}
	}

	// AC 5: the same run under another organizer is not readable, and is not an empty
	// report either — an empty report reads as "this cancellation refunded nobody".
	code, body := internalJSON(t, http.MethodGet,
		fmt.Sprintf("%s/internal/cancellation-refunds/%s?organizer_id=%s", commerceURL, run.RunID, uuid.NewString()), "", nil)
	if code != http.StatusNotFound {
		t.Fatalf("cross-organizer report = %d %s, want 404", code, body)
	}
}
