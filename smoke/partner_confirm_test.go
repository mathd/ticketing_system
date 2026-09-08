//go:build smoke

package smoke_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestPartnerConfirmSucceedsAndSettlesCommissionLeg (COS 1) verifies that an authenticated
// partner confirm completes on merchant-of-record terms, creating a payment.captured fact
// and a balanced settlement entry set with the partner commission leg.
func TestPartnerConfirmSucceedsAndSettlesCommissionLeg(t *testing.T) {
	if partnerToken() == "" {
		t.Fatal("SMOKE_PARTNER_TOKEN is not set")
	}
	if partnerReseller() == "" {
		t.Fatal("SMOKE_PARTNER_RESELLER_ID is not set")
	}
	if partnerChannel() == "" {
		t.Fatal("SMOKE_PARTNER_CHANNEL is not set")
	}

	ctx := context.Background()
	slot, ticketType := publishedSlot(t, "TKT-277 Partner Confirm Hall", 20)
	eventID := eventOf(t, ctx, ticketType)

	// Allocate stock bound to the partner reseller on its channel
	allocURL := fmt.Sprintf("%s/internal/slots/%s/channel-allocations", inventoryURL, slot)
	if code, body := internalJSON(t, http.MethodPut, allocURL, "", map[string]any{
		"organizer_id": organizerID,
		"allocations": []map[string]any{
			{"channel": partnerChannel(), "cap": 6, "sold_by": partnerReseller()},
		},
	}); code != http.StatusOK {
		t.Fatalf("allocate bound: %d %s", code, body)
	}

	// Seed fee rule and split schedule in catalog
	cat, err := pgx.Connect(ctx, dsn("catalog", "catalog"))
	if err != nil {
		t.Fatalf("connect catalog: %v", err)
	}
	defer func() { _ = cat.Close(ctx) }()

	const feeAmountPerTicket = int64(500)
	if _, err := cat.Exec(ctx, `INSERT INTO fee_rules
		(organizer_id, scope_level, scope_id, fee_code, basis, amount, currency, incidence, channel_code)
		VALUES($1, 'event', $2, 'reseller_commission', 'per_ticket_fixed', $3, 'EUR', 'passed_on', $4)`,
		organizerID, eventID, feeAmountPerTicket, partnerChannel()); err != nil {
		t.Fatalf("seed fee rule: %v", err)
	}

	tx, err := cat.Begin(ctx)
	if err != nil {
		t.Fatalf("begin split seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	extRef := fmt.Sprintf("reseller:%s", partnerReseller())
	var payeeID string
	if err := tx.QueryRow(ctx, `INSERT INTO payees (organizer_id, kind, display_name, external_reference)
		VALUES($1, 'reseller', 'Reseller Partner Payee', $2) RETURNING id`,
		organizerID, extRef).Scan(&payeeID); err != nil {
		t.Fatalf("seed payee: %v", err)
	}

	var scheduleID string
	if err := tx.QueryRow(ctx, `INSERT INTO split_schedules
		(organizer_id, scope_level, scope_id, fee_code, channel_code)
		VALUES($1, 'event', $2, 'reseller_commission', $3) RETURNING id`,
		organizerID, eventID, partnerChannel()).Scan(&scheduleID); err != nil {
		t.Fatalf("seed split schedule: %v", err)
	}

	if _, err := tx.Exec(ctx, `INSERT INTO split_schedule_parts
		(schedule_id, payee_id, organizer_id, share_bps) VALUES($1, $2, $3, 10000)`,
		scheduleID, payeeID, organizerID); err != nil {
		t.Fatalf("seed split part: %v", err)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit split seed: %v", err)
	}

	// 1. Partner reserve for 2 tickets
	reserveKey := "tkt277-reserve-" + slot
	code, body := partnerDo(t, http.MethodPost, "/api/commerce/partners/reservations", reserveKey,
		map[string]any{"organizer_id": organizerID, "ticket_type_id": ticketType, "quantity": 2})
	if code != http.StatusCreated {
		t.Fatalf("partner reserve: %d %s", code, body)
	}

	var reservation struct {
		ID string `json:"reservation_id"`
	}
	if err := json.Unmarshal(body, &reservation); err != nil {
		t.Fatal(err)
	}

	// 2. Partner confirm
	confirmKey := "tkt277-confirm-" + slot
	code, body = partnerDo(t, http.MethodPost, "/api/commerce/partners/orders", confirmKey,
		map[string]any{
			"reservation_id": reservation.ID,
			"name":           "Partner Buyer",
			"email":          "pbuyer@example.test",
			"payment_token":  "fake-ok",
		})
	if code != http.StatusOK {
		t.Fatalf("partner confirm: %d %s", code, body)
	}

	var orderResp struct {
		OrderID string `json:"order_id"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(body, &orderResp); err != nil {
		t.Fatal(err)
	}
	if orderResp.Status != "completed" {
		t.Fatalf("expected order status 'completed', got %q", orderResp.Status)
	}

	// 3. Connect to payments DB and assert settlement
	payDB, err := pgx.Connect(ctx, dsn("payments", "payments"))
	if err != nil {
		t.Fatalf("connect payments: %v", err)
	}
	defer func() { _ = payDB.Close(ctx) }()

	var factID string
	var capturedAmount int64
	if err := payDB.QueryRow(ctx, `SELECT fact_id, amount FROM journal_entries
		WHERE fact_type = 'payment.captured' AND payload->>'order_id' = $1`,
		orderResp.OrderID).Scan(&factID, &capturedAmount); err != nil {
		t.Fatalf("read payment.captured fact: %v", err)
	}

	rows, err := payDB.Query(ctx, `SELECT entry_kind, fee_code, payee_id::text, coalesce(payee_external_ref, ''), amount, currency, coalesce(incidence, '')
		FROM settlement_entries WHERE order_id = $1`, orderResp.OrderID)
	if err != nil {
		t.Fatalf("read settlement entries: %v", err)
	}
	defer rows.Close()

	type entry struct {
		kind        string
		feeCode     *string
		payeeID     *string
		externalRef string
		amount      int64
		currency    string
		incidence   string
	}

	var entries []entry
	var sumAmount int64
	for rows.Next() {
		var e entry
		var fc, pid *string
		if err := rows.Scan(&e.kind, &fc, &pid, &e.externalRef, &e.amount, &e.currency, &e.incidence); err != nil {
			t.Fatalf("scan settlement entry: %v", err)
		}
		e.feeCode = fc
		e.payeeID = pid
		entries = append(entries, e)
		sumAmount += e.amount
	}

	// Balanced ledger check: entries sum must equal captured amount
	if sumAmount != capturedAmount {
		t.Fatalf("settlement entries sum %d != captured amount %d", sumAmount, capturedAmount)
	}

	// Must contain the reseller commission entry
	const expectedCommission = feeAmountPerTicket * 2 // 1000 cents
	foundCommission := false
	for _, e := range entries {
		if e.kind == "fee" && e.feeCode != nil && *e.feeCode == "reseller_commission" {
			foundCommission = true
			if e.payeeID == nil || *e.payeeID != payeeID {
				t.Fatalf("commission payee_id = %v, want %s", e.payeeID, payeeID)
			}
			if e.externalRef != extRef {
				t.Fatalf("commission external_ref = %q, want %q", e.externalRef, extRef)
			}
			if e.amount != expectedCommission {
				t.Fatalf("commission amount = %d, want %d", e.amount, expectedCommission)
			}
			if e.incidence != "passed_on" {
				t.Fatalf("commission incidence = %q, want 'passed_on'", e.incidence)
			}
			if e.currency != "EUR" {
				t.Fatalf("commission currency = %q, want 'EUR'", e.currency)
			}
		}
	}
	if !foundCommission {
		t.Fatal("reseller_commission fee entry not found in settlement entries")
	}
}

// TestPartnerConfirmSettlesMultipleBeneficiariesSplit (COS 2) verifies that fractional
// shares between multiple beneficiaries are computed with integer minor units according to share_bps.
func TestPartnerConfirmSettlesMultipleBeneficiariesSplit(t *testing.T) {
	if partnerToken() == "" {
		t.Fatal("SMOKE_PARTNER_TOKEN is not set")
	}
	if partnerReseller() == "" {
		t.Fatal("SMOKE_PARTNER_RESELLER_ID is not set")
	}
	if partnerChannel() == "" {
		t.Fatal("SMOKE_PARTNER_CHANNEL is not set")
	}

	ctx := context.Background()
	slot, ticketType := publishedSlot(t, "TKT-277 Multi Beneficiary Hall", 20)
	eventID := eventOf(t, ctx, ticketType)

	allocURL := fmt.Sprintf("%s/internal/slots/%s/channel-allocations", inventoryURL, slot)
	if code, body := internalJSON(t, http.MethodPut, allocURL, "", map[string]any{
		"organizer_id": organizerID,
		"allocations": []map[string]any{
			{"channel": partnerChannel(), "cap": 6, "sold_by": partnerReseller()},
		},
	}); code != http.StatusOK {
		t.Fatalf("allocate bound: %d %s", code, body)
	}

	cat, err := pgx.Connect(ctx, dsn("catalog", "catalog"))
	if err != nil {
		t.Fatalf("connect catalog: %v", err)
	}
	defer func() { _ = cat.Close(ctx) }()

	const feeAmountPerTicket = int64(1000)
	if _, err := cat.Exec(ctx, `INSERT INTO fee_rules
		(organizer_id, scope_level, scope_id, fee_code, basis, amount, currency, incidence, channel_code)
		VALUES($1, 'event', $2, 'reseller_commission', 'per_ticket_fixed', $3, 'EUR', 'passed_on', $4)`,
		organizerID, eventID, feeAmountPerTicket, partnerChannel()); err != nil {
		t.Fatalf("seed fee rule: %v", err)
	}

	tx, err := cat.Begin(ctx)
	if err != nil {
		t.Fatalf("begin split seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	extRef := fmt.Sprintf("reseller:%s", partnerReseller())
	var partnerPayeeID string
	if err := tx.QueryRow(ctx, `INSERT INTO payees (organizer_id, kind, display_name, external_reference)
		VALUES($1, 'reseller', 'Reseller Payee 70%', $2) RETURNING id`,
		organizerID, extRef).Scan(&partnerPayeeID); err != nil {
		t.Fatalf("seed partner payee: %v", err)
	}

	var platformPayeeID string
	if err := tx.QueryRow(ctx, `INSERT INTO payees (organizer_id, kind, display_name)
		VALUES($1, 'system', 'Platform Payee 30%') RETURNING id`,
		organizerID).Scan(&platformPayeeID); err != nil {
		t.Fatalf("seed platform payee: %v", err)
	}

	var scheduleID string
	if err := tx.QueryRow(ctx, `INSERT INTO split_schedules
		(organizer_id, scope_level, scope_id, fee_code, channel_code)
		VALUES($1, 'event', $2, 'reseller_commission', $3) RETURNING id`,
		organizerID, eventID, partnerChannel()).Scan(&scheduleID); err != nil {
		t.Fatalf("seed split schedule: %v", err)
	}

	// 7000 bps (70%) to partner, 3000 bps (30%) to platform
	if _, err := tx.Exec(ctx, `INSERT INTO split_schedule_parts
		(schedule_id, payee_id, organizer_id, share_bps) VALUES
		($1, $2, $4, 7000),
		($1, $3, $4, 3000)`,
		scheduleID, partnerPayeeID, platformPayeeID, organizerID); err != nil {
		t.Fatalf("seed split parts: %v", err)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit split seed: %v", err)
	}

	// Partner reserve 2 tickets (total fee = 2000 cents)
	reserveKey := "tkt277-multi-reserve-" + slot
	code, body := partnerDo(t, http.MethodPost, "/api/commerce/partners/reservations", reserveKey,
		map[string]any{"organizer_id": organizerID, "ticket_type_id": ticketType, "quantity": 2})
	if code != http.StatusCreated {
		t.Fatalf("partner reserve: %d %s", code, body)
	}

	var reservation struct {
		ID string `json:"reservation_id"`
	}
	if err := json.Unmarshal(body, &reservation); err != nil {
		t.Fatal(err)
	}

	// Partner confirm
	confirmKey := "tkt277-multi-confirm-" + slot
	code, body = partnerDo(t, http.MethodPost, "/api/commerce/partners/orders", confirmKey,
		map[string]any{
			"reservation_id": reservation.ID,
			"name":           "Partner Buyer Multi",
			"email":          "pmulti@example.test",
			"payment_token":  "fake-ok",
		})
	if code != http.StatusOK {
		t.Fatalf("partner confirm: %d %s", code, body)
	}

	var orderResp struct {
		OrderID string `json:"order_id"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(body, &orderResp); err != nil {
		t.Fatal(err)
	}
	if orderResp.Status != "completed" {
		t.Fatalf("expected order status 'completed', got %q", orderResp.Status)
	}

	payDB, err := pgx.Connect(ctx, dsn("payments", "payments"))
	if err != nil {
		t.Fatalf("connect payments: %v", err)
	}
	defer func() { _ = payDB.Close(ctx) }()

	rows, err := payDB.Query(ctx, `SELECT payee_id::text, amount FROM settlement_entries
		WHERE order_id = $1 AND fee_code = 'reseller_commission'`, orderResp.OrderID)
	if err != nil {
		t.Fatalf("read settlement entries: %v", err)
	}
	defer rows.Close()

	amountsByPayee := map[string]int64{}
	for rows.Next() {
		var pid string
		var amt int64
		if err := rows.Scan(&pid, &amt); err != nil {
			t.Fatalf("scan entry: %v", err)
		}
		amountsByPayee[pid] = amt
	}

	// 70% of 2000 = 1400; 30% of 2000 = 600
	const expectedPartnerAmount = int64(1400)
	const expectedPlatformAmount = int64(600)

	if amountsByPayee[partnerPayeeID] != expectedPartnerAmount {
		t.Fatalf("partner share = %d, want %d (7000 bps of 2000)", amountsByPayee[partnerPayeeID], expectedPartnerAmount)
	}
	if amountsByPayee[platformPayeeID] != expectedPlatformAmount {
		t.Fatalf("platform share = %d, want %d (3000 bps of 2000)", amountsByPayee[platformPayeeID], expectedPlatformAmount)
	}
}
