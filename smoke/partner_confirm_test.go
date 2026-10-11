//go:build smoke

package smoke_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
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

	// Joined through the capture fact, not the order id. `settlement_must_balance()`
	// already refuses a set whose order_id disagrees with the fact's payload
	// (0004_settlement_ledger.sql:134), so the two are equivalent by invariant --
	// but reading them through the fact is what this assertion is about, and it
	// keeps the fact id load-bearing rather than scanned and dropped.
	rows, err := payDB.Query(ctx, `SELECT entry_kind, fee_code, payee_id::text, coalesce(payee_external_ref, ''), amount, currency, coalesce(incidence, '')
		FROM settlement_entries WHERE capture_fact_id = $1`, factID)
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

// ---- TKT-330: the split remainder oracle and end-to-end partner idempotency ----
//
// Every test below makes its own reseller, channel, credential and slot, so a
// repeat run shares nothing with an earlier one and nothing with the shared
// SMOKE_PARTNER_* credential the tests above use.

// TestPartnerConfirmSettlesSplitRemainderByPayeeID (TKT-330 COS1) proves the
// remainder rule end to end: a commission whose shares do not divide it, settled
// through a real partner confirm, pays the leftover cent to the payee the WRITTEN
// rule names. The 70/30 test above cannot see a remainder bug, because 2000 divides
// cleanly.
//
// The expected amounts are literals derived by hand from the rule in
// services/payments/internal/splits/splits.go (Allocate) and ADR-047, never from a
// run. The rule: floor every part, then give the leftover cents to the parts with
// the largest fractional remainders, ties broken by payee id ascending.
//
//	payees  P1 = <prefix>…01, P2 = <prefix>…02 (the reseller), P3 = <prefix>…03
//	shares  P1 4000 bps, P2 3000 bps, P3 3000 bps (sum 10000)
//	amount  1008 cents (one ticket at 1008 per ticket)
//
//	q, r = 1008 / 10000, 1008 % 10000 = 0, 1008
//	P1: 1008 × 4000 = 4,032,000 → floor 403, fractional numerator 2000
//	P2: 1008 × 3000 = 3,024,000 → floor 302, fractional numerator 4000
//	P3: 1008 × 3000 = 3,024,000 → floor 302, fractional numerator 4000
//	floor sum 403 + 302 + 302 = 1007, so one cent is left over
//	largest remainder: P2 and P3 tie at 4000; P2 has the lower id and wins
//	expected P1 403, P2 303, P3 302 (sum 1008)
//
// Every plausible wrong rule gives a different answer on this fixture (TKT-330 D3):
// the cent to the last part or a reversed tie gives 403/302/303; the cent to the
// first payee or to the largest share gives 404/302/302. Parts are loaded ORDER BY
// payee_id, so the order of the INSERTs below decides nothing and is not a check.
func TestPartnerConfirmSettlesSplitRemainderByPayeeID(t *testing.T) {
	ctx := context.Background()
	run := uuid.NewString()[:8]
	reseller := uuid.NewString()
	channel := "tkt330-split-" + run
	cred := tkt330Enrol(t, ctx, reseller, channel, "tkt330 split")
	slot, ticketType := tkt330Slot(t, "TKT-330 Split Remainder Hall", tkt330Alloc{channel: channel, reseller: reseller})
	eventID := eventOf(t, ctx, ticketType)

	cat, err := pgx.Connect(ctx, dsn("catalog", "catalog"))
	if err != nil {
		t.Fatalf("connect catalog: %v", err)
	}
	defer func() { _ = cat.Close(ctx) }()

	const feeAmountPerTicket = int64(1008)
	if _, err := cat.Exec(ctx, `INSERT INTO fee_rules
		(organizer_id, scope_level, scope_id, fee_code, basis, amount, currency, incidence, channel_code)
		VALUES($1, 'event', $2, 'reseller_commission', 'per_ticket_fixed', $3, 'EUR', 'passed_on', $4)`,
		organizerID, eventID, feeAmountPerTicket, channel); err != nil {
		t.Fatalf("seed fee rule: %v", err)
	}

	// Fixed payee ids on a fresh prefix: the last byte fixes their order, the prefix
	// keeps a repeat run off the primary key of an earlier one.
	prefix := uuid.New()
	p1, p2, p3 := prefix, prefix, prefix
	p1[15], p2[15], p3[15] = 0x01, 0x02, 0x03
	resellerRef := "reseller:" + reseller

	tx, err := cat.Begin(ctx)
	if err != nil {
		t.Fatalf("begin split seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO payees (id, organizer_id, kind, display_name, external_reference) VALUES
		($1, $4, 'system', 'TKT-330 P1 4000', NULL),
		($2, $4, 'reseller', 'TKT-330 P2 3000 reseller', $5),
		($3, $4, 'system', 'TKT-330 P3 3000', NULL)`,
		p1, p2, p3, organizerID, resellerRef); err != nil {
		t.Fatalf("seed payees: %v", err)
	}
	var scheduleID string
	if err := tx.QueryRow(ctx, `INSERT INTO split_schedules
		(organizer_id, scope_level, scope_id, fee_code, channel_code)
		VALUES($1, 'event', $2, 'reseller_commission', $3) RETURNING id`,
		organizerID, eventID, channel).Scan(&scheduleID); err != nil {
		t.Fatalf("seed split schedule: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO split_schedule_parts
		(schedule_id, payee_id, organizer_id, share_bps) VALUES
		($1, $2, $5, 4000),
		($1, $3, $5, 3000),
		($1, $4, $5, 3000)`,
		scheduleID, p1, p2, p3, organizerID); err != nil {
		t.Fatalf("seed split parts: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit split seed: %v", err)
	}

	reservation := tkt330Reserve(t, cred.token, ticketType, "tkt330-split-reserve-"+run)
	order := tkt330ConfirmOK(t, cred.token, "tkt330-split-confirm-"+run,
		tkt330ConfirmBody(reservation, "TKT330 Buyer"), "partner confirm")
	tkt330AssertSlotOrders(t, ctx, slot, "after the confirm",
		tkt330OrderRow{id: order.OrderID, status: "completed", reseller: reseller, reservation: reservation})
	tkt330AssertCaptures(t, ctx, "after the confirm", order.OrderID)

	payDB, err := pgx.Connect(ctx, dsn("payments", "payments"))
	if err != nil {
		t.Fatalf("connect payments: %v", err)
	}
	defer func() { _ = payDB.Close(ctx) }()

	var factID string
	var capturedAmount int64
	if err := payDB.QueryRow(ctx, `SELECT fact_id::text, amount FROM journal_entries
		WHERE organizer_id = $1 AND fact_type = 'payment.captured' AND payload->>'order_id' = $2`,
		organizerID, order.OrderID).Scan(&factID, &capturedAmount); err != nil {
		t.Fatalf("read payment.captured fact: %v", err)
	}

	rows, err := payDB.Query(ctx, `SELECT entry_kind, coalesce(fee_code, ''), coalesce(payee_id::text, ''),
		coalesce(payee_external_ref, ''), amount, currency, coalesce(incidence, '')
		FROM settlement_entries WHERE capture_fact_id = $1`, factID)
	if err != nil {
		t.Fatalf("read settlement entries: %v", err)
	}
	defer rows.Close()

	type feeLine struct {
		externalRef, currency, incidence string
		amount                           int64
	}
	commission := map[string]feeLine{}
	var settledSum, feeRows int64
	for rows.Next() {
		var kind, feeCode, payee, ref, currency, incidence string
		var amount int64
		if err := rows.Scan(&kind, &feeCode, &payee, &ref, &amount, &currency, &incidence); err != nil {
			t.Fatalf("scan settlement entry: %v", err)
		}
		settledSum += amount
		if kind != "fee" {
			continue
		}
		feeRows++
		if feeCode != "reseller_commission" {
			t.Fatalf("unexpected fee line %q (%d) on a fixture that seeds only reseller_commission", feeCode, amount)
		}
		if payee == "" {
			t.Fatalf("a reseller_commission line of %d has no payee: the split schedule was not applied", amount)
		}
		if _, dup := commission[payee]; dup {
			t.Fatalf("payee %s has two reseller_commission lines", payee)
		}
		commission[payee] = feeLine{externalRef: ref, currency: currency, incidence: incidence, amount: amount}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate settlement entries: %v", err)
	}

	if settledSum != capturedAmount {
		t.Fatalf("settlement entries sum to %d, the capture is %d", settledSum, capturedAmount)
	}
	if feeRows != 3 || len(commission) != 3 {
		t.Fatalf("got %d fee lines over %d payees, want exactly 3 reseller_commission lines: %v", feeRows, len(commission), commission)
	}
	want := map[string]feeLine{
		p1.String(): {externalRef: "", currency: "EUR", incidence: "passed_on", amount: 403},
		p2.String(): {externalRef: resellerRef, currency: "EUR", incidence: "passed_on", amount: 303},
		p3.String(): {externalRef: "", currency: "EUR", incidence: "passed_on", amount: 302},
	}
	var commissionSum int64
	for payee, w := range want {
		got, ok := commission[payee]
		if !ok {
			t.Fatalf("no reseller_commission line for payee %s: %v", payee, commission)
		}
		if got != w {
			t.Errorf("payee %s settled %+v, want %+v (largest remainder, ties by payee id)", payee, got, w)
		}
		commissionSum += got.amount
	}
	if commissionSum != feeAmountPerTicket {
		t.Errorf("reseller_commission lines sum to %d, want the whole fee %d", commissionSum, feeAmountPerTicket)
	}
}

// TestPartnerConfirmSameRawKeyFromTwoResellersMakesTwoOrders (TKT-330 COS2): two
// resellers of ONE organizer send the same raw Idempotency-Key and each gets its own
// order. The derivation namespaces the key by reseller; this proves the namespace
// reaches the order the second request returns, not only the string it derives.
func TestPartnerConfirmSameRawKeyFromTwoResellersMakesTwoOrders(t *testing.T) {
	ctx := context.Background()
	run := uuid.NewString()[:8]
	resellerA, resellerB := uuid.NewString(), uuid.NewString()
	channelA, channelB := "tkt330-a-"+run, "tkt330-b-"+run
	credA := tkt330Enrol(t, ctx, resellerA, channelA, "tkt330 reseller A")
	credB := tkt330Enrol(t, ctx, resellerB, channelB, "tkt330 reseller B")
	// ONE allocation PUT for both channels: it replaces the slot's whole set, so a
	// second PUT would remove the first channel's allocation.
	slot, ticketType := tkt330Slot(t, "TKT-330 Two Resellers Hall",
		tkt330Alloc{channel: channelA, reseller: resellerA},
		tkt330Alloc{channel: channelB, reseller: resellerB})
	tkt330AssertSlotOrders(t, ctx, slot, "before any confirm")

	reservationA := tkt330Reserve(t, credA.token, ticketType, "tkt330-two-reserve-a-"+run)
	reservationB := tkt330Reserve(t, credB.token, ticketType, "tkt330-two-reserve-b-"+run)

	rawKey := "tkt330-two-confirm-" + run
	orderA := tkt330ConfirmOK(t, credA.token, rawKey, tkt330ConfirmBody(reservationA, "TKT330 Buyer"), "confirm by reseller A")
	orderB := tkt330ConfirmOK(t, credB.token, rawKey, tkt330ConfirmBody(reservationB, "TKT330 Buyer"),
		"confirm by reseller B under the same raw key")
	if orderA.OrderID == orderB.OrderID {
		t.Fatalf("two resellers' confirms under one raw key returned the same order %s", orderA.OrderID)
	}

	tkt330AssertSlotOrders(t, ctx, slot, "after both confirms",
		tkt330OrderRow{id: orderA.OrderID, status: "completed", reseller: resellerA, reservation: reservationA},
		tkt330OrderRow{id: orderB.OrderID, status: "completed", reseller: resellerB, reservation: reservationB})
	tkt330AssertCaptures(t, ctx, "after both confirms", orderA.OrderID, orderB.OrderID)
}

// TestPartnerConfirmReplaysOriginalOrderAfterCredentialRotation (TKT-330 COS3): a
// partner that rotates its credential and replays a confirm with the new one gets
// its ORIGINAL order back, and no second sale is recorded. The derivation excludes
// the credential id for exactly this; the pure derivation test proves the string is
// equal, and only this proves the second request returns the first order.
//
// Counts alone cannot see a regression here: commerce finds the order by
// reservation, so a key that changed with the credential is refused with 409 and
// still leaves one order. The original 200 response is what must hold.
func TestPartnerConfirmReplaysOriginalOrderAfterCredentialRotation(t *testing.T) {
	ctx := context.Background()
	run := uuid.NewString()[:8]
	reseller := uuid.NewString()
	channel := "tkt330-rotate-" + run
	c1 := tkt330Enrol(t, ctx, reseller, channel, "tkt330 rotation c1")
	slot, ticketType := tkt330Slot(t, "TKT-330 Rotation Hall", tkt330Alloc{channel: channel, reseller: reseller})
	tkt330AssertSlotOrders(t, ctx, slot, "before any confirm")

	// Reserved with C1 only. A reservation made with C2 would be a SECOND reservation,
	// and its confirm under the same key would collide on the order id even in
	// correct code.
	reservation := tkt330Reserve(t, c1.token, ticketType, "tkt330-rotate-reserve-"+run)
	key := "tkt330-rotate-confirm-" + run
	body := tkt330ConfirmBody(reservation, "TKT330 Buyer")
	original := tkt330ConfirmOK(t, c1.token, key, body, "original confirm with C1")
	only := tkt330OrderRow{id: original.OrderID, status: "completed", reseller: reseller, reservation: reservation}
	tkt330AssertSlotOrders(t, ctx, slot, "after the original confirm", only)
	tkt330AssertCaptures(t, ctx, "after the original confirm", original.OrderID)

	// Rotation: the replacement is issued while the predecessor is still live.
	c2 := tkt330Enrol(t, ctx, reseller, channel, "tkt330 rotation c2")
	if c2.id == c1.id || c2.token == c1.token {
		t.Fatalf("the replacement credential is not a new credential")
	}
	replay := tkt330ConfirmOK(t, c2.token, key, body, "replay with C2 while C1 is live")
	tkt330AssertSameOrder(t, "replay with C2 while C1 is live", original, replay)
	tkt330AssertSlotOrders(t, ctx, slot, "after the C2 replay", only)
	tkt330AssertCaptures(t, ctx, "after the C2 replay", original.OrderID)

	// Scenario check, with no mutation behind it: the replay still holds once the
	// predecessor is gone.
	tkt330Revoke(t, c1.id)
	if !tkt330Revoked(t, ctx, c1.id) {
		t.Fatalf("C1 %s is not revoked after revoke-reseller", c1.id)
	}
	if tkt330Revoked(t, ctx, c2.id) {
		t.Fatalf("revoking C1 also revoked C2 %s", c2.id)
	}
	if code, out := partnerDoAs(t, c1.token, http.MethodPost, "/api/commerce/partners/orders", key, body); code != http.StatusUnauthorized {
		t.Fatalf("the revoked C1 was answered %d, want 401: %s", code, out)
	}
	afterRevoke := tkt330ConfirmOK(t, c2.token, key, body, "replay with C2 after C1 is revoked")
	tkt330AssertSameOrder(t, "replay with C2 after C1 is revoked", original, afterRevoke)
	tkt330AssertSlotOrders(t, ctx, slot, "after the post-revoke replay", only)
	tkt330AssertCaptures(t, ctx, "after the post-revoke replay", original.OrderID)
}

// TestPartnerConfirmRefusesChangedBodyUnderSameKey (TKT-330 COS4): the same key with
// a CHANGED body is a conflict, not a replay. Answering it with the first order would
// tell the partner its second request succeeded when it was never processed.
func TestPartnerConfirmRefusesChangedBodyUnderSameKey(t *testing.T) {
	ctx := context.Background()
	run := uuid.NewString()[:8]
	reseller := uuid.NewString()
	channel := "tkt330-conflict-" + run
	cred := tkt330Enrol(t, ctx, reseller, channel, "tkt330 conflict")
	slot, ticketType := tkt330Slot(t, "TKT-330 Conflict Hall", tkt330Alloc{channel: channel, reseller: reseller})
	tkt330AssertSlotOrders(t, ctx, slot, "before any confirm")

	reservation := tkt330Reserve(t, cred.token, ticketType, "tkt330-conflict-reserve-"+run)
	key := "tkt330-conflict-confirm-" + run
	original := tkt330ConfirmOK(t, cred.token, key, tkt330ConfirmBody(reservation, "TKT330 Buyer"), "original confirm")
	only := tkt330OrderRow{id: original.OrderID, status: "completed", reseller: reseller, reservation: reservation}
	tkt330AssertSlotOrders(t, ctx, slot, "after the original confirm", only)
	tkt330AssertCaptures(t, ctx, "after the original confirm", original.OrderID)

	// Only the name changes, to a value no normalisation makes equal to the first.
	code, out := partnerDoAs(t, cred.token, http.MethodPost, "/api/commerce/partners/orders", key,
		tkt330ConfirmBody(reservation, "TKT330 Changed Buyer"))
	if code != http.StatusConflict {
		t.Fatalf("a changed body under the same key was answered %d, want 409: %s", code, out)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode the 409 body %q: %v", out, err)
	}
	if wantBody := map[string]any{"error": "checkout conflicts with an existing request"}; !reflect.DeepEqual(got, wantBody) {
		t.Fatalf("409 body = %v, want exactly %v", got, wantBody)
	}
	tkt330AssertSlotOrders(t, ctx, slot, "after the changed-body request", only)
	tkt330AssertCaptures(t, ctx, "after the changed-body request", original.OrderID)

	replay := tkt330ConfirmOK(t, cred.token, key, tkt330ConfirmBody(reservation, "TKT330 Buyer"),
		"replay of the original body after the conflict")
	tkt330AssertSameOrder(t, "replay of the original body after the conflict", original, replay)
	tkt330AssertSlotOrders(t, ctx, slot, "after the original-body replay", only)
	tkt330AssertCaptures(t, ctx, "after the original-body replay", original.OrderID)
}

// tkt330Credential is a reseller credential enrolled through the real CLI. The token
// is never printed by these helpers.
type tkt330Credential struct{ id, token string }

// tkt330Enrol issues a credential the way an operator does (TKT-240), on the seeded
// organizer, and resolves its id by the fresh (organizer, reseller, channel, label).
func tkt330Enrol(t *testing.T, ctx context.Context, reseller, channel, label string) tkt330Credential {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("docker", "exec", project+"-commerce-1", "/app", "enrol-reseller",
		organizerID, reseller, channel, label)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("enrol-reseller %s on %s: %v; stderr %q", reseller, channel, err, stderr.String())
	}
	token := strings.TrimSpace(stdout.String())
	if token == "" || strings.ContainsAny(token, " \t\n") {
		t.Fatalf("enrol-reseller %s on %s did not print exactly one token on stdout; stderr %q", reseller, channel, stderr.String())
	}

	db, err := pgx.Connect(ctx, dsn("commerce", "commerce"))
	if err != nil {
		t.Fatalf("connect commerce: %v", err)
	}
	defer func() { _ = db.Close(ctx) }()
	rows, err := db.Query(ctx, `SELECT id::text FROM reseller_credentials
		WHERE organizer_id = $1 AND reseller_id = $2 AND channel_code = $3 AND label = $4`,
		organizerID, reseller, channel, label)
	if err != nil {
		t.Fatalf("resolve credential id: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan credential id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate credential ids: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("found %d credentials for reseller %s on %s labelled %q, want exactly 1", len(ids), reseller, channel, label)
	}
	return tkt330Credential{id: ids[0], token: token}
}

// tkt330Revoke retires one credential by id through the real CLI.
func tkt330Revoke(t *testing.T, credentialID string) {
	t.Helper()
	var stderr bytes.Buffer
	cmd := exec.Command("docker", "exec", project+"-commerce-1", "/app", "revoke-reseller", credentialID)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("revoke-reseller %s: %v; stderr %q", credentialID, err, stderr.String())
	}
}

// tkt330Revoked reads whether one credential's revoked_at is set.
func tkt330Revoked(t *testing.T, ctx context.Context, credentialID string) bool {
	t.Helper()
	db, err := pgx.Connect(ctx, dsn("commerce", "commerce"))
	if err != nil {
		t.Fatalf("connect commerce: %v", err)
	}
	defer func() { _ = db.Close(ctx) }()
	var revoked bool
	if err := db.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM reseller_credentials WHERE id = $1`,
		credentialID).Scan(&revoked); err != nil {
		t.Fatalf("read credential %s: %v", credentialID, err)
	}
	return revoked
}

// tkt330Alloc is one channel allocation bound to the reseller that may sell it.
type tkt330Alloc struct{ channel, reseller string }

// tkt330Slot publishes a fresh GA slot (capacity 20, 2500 EUR) and sets its channel
// allocations in ONE PUT, which replaces the slot's whole set.
func tkt330Slot(t *testing.T, name string, allocs ...tkt330Alloc) (string, string) {
	t.Helper()
	slot, ticketType := publishedSlot(t, name, 20)
	list := make([]map[string]any, 0, len(allocs))
	for _, a := range allocs {
		list = append(list, map[string]any{"channel": a.channel, "cap": 3, "sold_by": a.reseller})
	}
	allocURL := fmt.Sprintf("%s/internal/slots/%s/channel-allocations", inventoryURL, slot)
	if code, body := internalJSON(t, http.MethodPut, allocURL, "", map[string]any{
		"organizer_id": organizerID,
		"allocations":  list,
	}); code != http.StatusOK {
		t.Fatalf("allocate %v: %d %s", allocs, code, body)
	}
	return slot, ticketType
}

// tkt330Reserve holds one ticket with the given credential and returns the
// reservation id.
func tkt330Reserve(t *testing.T, token, ticketType, key string) string {
	t.Helper()
	code, body := partnerDoAs(t, token, http.MethodPost, "/api/commerce/partners/reservations", key,
		map[string]any{"organizer_id": organizerID, "ticket_type_id": ticketType, "quantity": 1})
	if code != http.StatusCreated {
		t.Fatalf("partner reserve %s: %d %s", key, code, body)
	}
	var reservation struct {
		ID string `json:"reservation_id"`
	}
	if err := json.Unmarshal(body, &reservation); err != nil {
		t.Fatalf("decode reservation %s: %v", body, err)
	}
	if _, err := uuid.Parse(reservation.ID); err != nil {
		t.Fatalf("reservation id %q: %v", reservation.ID, err)
	}
	return reservation.ID
}

func tkt330ConfirmBody(reservation, name string) map[string]any {
	return map[string]any{
		"reservation_id": reservation,
		"name":           name,
		"email":          "tkt330@example.test",
		"payment_token":  "fake-ok",
	}
}

// tkt330Order is a completed confirm's answer. A completed replay carries the same
// three fields and no replay flag.
type tkt330Order struct {
	OrderID       string `json:"order_id"`
	GuestOrderRef string `json:"guest_order_ref"`
	Status        string `json:"status"`
}

// tkt330ConfirmOK confirms with the given credential and requires a 200 completed
// order.
func tkt330ConfirmOK(t *testing.T, token, key string, body map[string]any, step string) tkt330Order {
	t.Helper()
	code, out := partnerDoAs(t, token, http.MethodPost, "/api/commerce/partners/orders", key, body)
	if code != http.StatusOK {
		t.Fatalf("%s: status %d, want 200: %s", step, code, out)
	}
	var order tkt330Order
	if err := json.Unmarshal(out, &order); err != nil {
		t.Fatalf("%s: decode %s: %v", step, out, err)
	}
	if order.Status != "completed" {
		t.Fatalf("%s: status %q, want completed: %s", step, order.Status, out)
	}
	if _, err := uuid.Parse(order.OrderID); err != nil {
		t.Fatalf("%s: order id %q: %v", step, order.OrderID, err)
	}
	if order.GuestOrderRef == "" {
		t.Fatalf("%s: no guest_order_ref: %s", step, out)
	}
	return order
}

func tkt330AssertSameOrder(t *testing.T, step string, original, replay tkt330Order) {
	t.Helper()
	if replay != original {
		t.Fatalf("%s: answered %+v, want the original %+v", step, replay, original)
	}
}

// tkt330OrderRow is one order as commerce stores it.
type tkt330OrderRow struct{ id, status, reseller, reservation string }

// tkt330AssertSlotOrders requires the slot's orders to be EXACTLY want. It reads
// every order of every status in the slot, not only the order a response named, so
// a second order, finished or not, cannot hide from the count.
func tkt330AssertSlotOrders(t *testing.T, ctx context.Context, slot, step string, want ...tkt330OrderRow) {
	t.Helper()
	db, err := pgx.Connect(ctx, dsn("commerce", "commerce"))
	if err != nil {
		t.Fatalf("connect commerce: %v", err)
	}
	defer func() { _ = db.Close(ctx) }()
	rows, err := db.Query(ctx, `SELECT o.id::text, o.status, coalesce(o.reseller_id::text, ''), o.reservation_id::text
		FROM orders o JOIN reservations r ON r.id = o.reservation_id
		WHERE r.organizer_id = $1 AND r.slot_id = $2`, organizerID, slot)
	if err != nil {
		t.Fatalf("%s: read slot orders: %v", step, err)
	}
	defer rows.Close()
	got := map[string]tkt330OrderRow{}
	for rows.Next() {
		var o tkt330OrderRow
		if err := rows.Scan(&o.id, &o.status, &o.reseller, &o.reservation); err != nil {
			t.Fatalf("%s: scan order: %v", step, err)
		}
		if _, dup := got[o.id]; dup {
			t.Fatalf("%s: order %s read twice", step, o.id)
		}
		got[o.id] = o
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: iterate orders: %v", step, err)
	}
	wantSet := map[string]tkt330OrderRow{}
	for _, o := range want {
		wantSet[o.id] = o
	}
	if !reflect.DeepEqual(got, wantSet) {
		t.Fatalf("%s: slot orders = %+v, want exactly %+v", step, got, wantSet)
	}
}

// tkt330AssertCaptures requires exactly one payment.captured fact per order: no
// order missing its capture and none captured twice. It counts recorded captures,
// not provider calls.
func tkt330AssertCaptures(t *testing.T, ctx context.Context, step string, orderIDs ...string) {
	t.Helper()
	db, err := pgx.Connect(ctx, dsn("payments", "payments"))
	if err != nil {
		t.Fatalf("connect payments: %v", err)
	}
	defer func() { _ = db.Close(ctx) }()
	rows, err := db.Query(ctx, `SELECT payload->>'order_id', count(*) FROM journal_entries
		WHERE organizer_id = $1 AND fact_type = 'payment.captured' AND payload->>'order_id' = ANY($2::text[])
		GROUP BY payload->>'order_id'`, organizerID, orderIDs)
	if err != nil {
		t.Fatalf("%s: read captures: %v", step, err)
	}
	defer rows.Close()
	got := map[string]int64{}
	for rows.Next() {
		var order string
		var n int64
		if err := rows.Scan(&order, &n); err != nil {
			t.Fatalf("%s: scan capture count: %v", step, err)
		}
		got[order] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: iterate captures: %v", step, err)
	}
	want := map[string]int64{}
	for _, id := range orderIDs {
		want[id] = 1
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: payment.captured per order = %v, want exactly %v", step, got, want)
	}
}
