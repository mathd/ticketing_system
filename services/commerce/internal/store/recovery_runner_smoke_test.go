//go:build smoke

package store_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/commerce/internal/recovery"
	"ticketing/services/commerce/internal/store"
)

const terminalRefundReason = "provider_refund_failed: provider refused the refund (re_connected); manual reconciliation required"

type connectedPayments struct {
	key         string
	refundCalls int
}

func (p *connectedPayments) LookupOperation(_ context.Context, _ uuid.UUID, key string) (recovery.Operation, bool, error) {
	if key != p.key {
		return recovery.Operation{}, false, recovery.ErrProviderUnresolved
	}
	return recovery.Operation{Resolved: true, Status: "captured"}, true, nil
}

func (p *connectedPayments) Status(_ context.Context, _ uuid.UUID, key string) (recovery.PSPStatus, error) {
	if key != p.key {
		return recovery.PSPStatus{}, recovery.ErrProviderUnresolved
	}
	return recovery.PSPStatus{
		Outcome: "captured", Captured: true, Authorized: true,
		AuthorizedAmount: 1250, CapturedAmount: 1250, Currency: "EUR",
	}, nil
}

func (p *connectedPayments) Void(_ context.Context, _ uuid.UUID, key string) (recovery.CompensationResult, error) {
	if key != p.key {
		return recovery.CompensationResult{}, recovery.ErrProviderUnresolved
	}
	return recovery.CompensationResult{}, errors.New("unexpected void for seeded order")
}

func (p *connectedPayments) Refund(_ context.Context, _ uuid.UUID, key string) (recovery.CompensationResult, error) {
	if key != p.key {
		return recovery.CompensationResult{}, recovery.ErrProviderUnresolved
	}
	p.refundCalls++
	return recovery.CompensationResult{}, &recovery.ProviderRefundFailedError{ProviderRef: "re_connected"}
}

type connectedInventory struct {
	holdID       uuid.UUID
	confirmCalls int
	releaseCalls int
}

func (i *connectedInventory) Confirm(_ context.Context, _, holdID uuid.UUID) error {
	if holdID == i.holdID {
		i.confirmCalls++
	}
	return nil
}

func (i *connectedInventory) Release(_ context.Context, _, holdID uuid.UUID) error {
	if holdID == i.holdID {
		i.releaseCalls++
	}
	return nil
}

type connectedJournal struct {
	orderID uuid.UUID
	calls   int
}

func (j *connectedJournal) OrderFailed(_ context.Context, s store.StuckOrder) error {
	if s.OrderID == j.orderID {
		j.calls++
	}
	return nil
}

type connectedCompleter struct {
	orderID uuid.UUID
	calls   int
}

func (c *connectedCompleter) Complete(_ context.Context, s store.StuckOrder) error {
	if s.OrderID == c.orderID {
		c.calls++
	}
	return nil
}

func TestRunnerReparksAnUnparkedTerminalRefundInOnePass(t *testing.T) {
	db, ctx := store.OutboxDBForTest(t)
	seeded := store.SeedStuckForTest(t, "reconciliation_required")
	if err := db.QueryRowContext(ctx, `SELECT idempotency_key FROM orders WHERE id=$1`, seeded.OrderID).Scan(&seeded.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	payments := &connectedPayments{key: seeded.IdempotencyKey}
	inventory := &connectedInventory{holdID: seeded.HoldID}
	journal := &connectedJournal{orderID: seeded.OrderID}
	completer := &connectedCompleter{orderID: seeded.OrderID}
	runner, err := recovery.New(recovery.DBStore{DB: db}, payments, inventory, journal, completer,
		time.Minute, 16, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}

	assertParked := func(pass string) {
		t.Helper()
		var parked bool
		var lastError string
		var attempts int
		var claimID sql.NullString
		var status string
		if err := db.QueryRowContext(ctx, `
			SELECT recovery_parked_at IS NOT NULL, recovery_last_error,
			       recovery_attempts, recovery_claim_id::text, status
			FROM orders WHERE id=$1`, seeded.OrderID).
			Scan(&parked, &lastError, &attempts, &claimID, &status); err != nil {
			t.Fatalf("%s: read order: %v", pass, err)
		}
		if !parked || lastError != terminalRefundReason || attempts != 0 || claimID.Valid || status != "reconciliation_required" {
			t.Fatalf("%s: parked=%t last_error=%q attempts=%d claim_id=%v status=%q",
				pass, parked, lastError, attempts, claimID, status)
		}
	}

	runner.RunOnce(ctx)
	assertParked("first pass")
	if payments.refundCalls != 1 {
		t.Fatalf("refund calls after first pass = %d, want 1", payments.refundCalls)
	}

	if err := store.UnparkOrder(ctx, db, seeded.OrderID, "operator reviewed"); err != nil {
		t.Fatal(err)
	}
	// UnparkOrder updates updated_at. Backdate it past ClaimStuckOrders' two-minute
	// in-flight grace period so the next pass tests the re-drive, not the grace window.
	if _, err := db.ExecContext(ctx, `UPDATE orders SET updated_at=now()-interval '10 minutes' WHERE id=$1`, seeded.OrderID); err != nil {
		t.Fatal(err)
	}

	runner.RunOnce(ctx)
	assertParked("second pass")
	if payments.refundCalls != 2 {
		t.Fatalf("refund calls after second pass = %d, want 2", payments.refundCalls)
	}

	runner.RunOnce(ctx)
	if payments.refundCalls != 2 {
		t.Fatalf("refund calls after third pass = %d, want 2 for a parked order", payments.refundCalls)
	}
	if inventory.confirmCalls != 0 || inventory.releaseCalls != 0 || journal.calls != 0 || completer.calls != 0 {
		t.Fatalf("seeded order took an unrelated action: confirms=%d releases=%d journal=%d complete=%d",
			inventory.confirmCalls, inventory.releaseCalls, journal.calls, completer.calls)
	}
}
