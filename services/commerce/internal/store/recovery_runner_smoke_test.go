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
	t           *testing.T
	organizerID uuid.UUID
	key         string
	refundCalls int
}

func (p *connectedPayments) LookupOperation(_ context.Context, organizerID uuid.UUID, key string) (recovery.Operation, bool, error) {
	if organizerID != p.organizerID || key != p.key {
		p.t.Errorf("unexpected LookupOperation organizer=%s key=%q", organizerID, key)
		return recovery.Operation{}, false, recovery.ErrProviderUnresolved
	}
	return recovery.Operation{Resolved: true, Status: "captured"}, true, nil
}

func (p *connectedPayments) Status(_ context.Context, organizerID uuid.UUID, key string) (recovery.PSPStatus, error) {
	if organizerID != p.organizerID || key != p.key {
		p.t.Errorf("unexpected Status organizer=%s key=%q", organizerID, key)
		return recovery.PSPStatus{}, recovery.ErrProviderUnresolved
	}
	return recovery.PSPStatus{
		Outcome: "captured", Captured: true, Authorized: true,
		AuthorizedAmount: 1250, CapturedAmount: 1250, Currency: "EUR",
	}, nil
}

func (p *connectedPayments) Void(_ context.Context, organizerID uuid.UUID, key string) (recovery.CompensationResult, error) {
	p.t.Errorf("unexpected Void organizer=%s key=%q", organizerID, key)
	return recovery.CompensationResult{}, errors.New("unexpected void for seeded order")
}

func (p *connectedPayments) Refund(_ context.Context, organizerID uuid.UUID, key string) (recovery.CompensationResult, error) {
	if organizerID != p.organizerID || key != p.key {
		p.t.Errorf("unexpected Refund organizer=%s key=%q", organizerID, key)
		return recovery.CompensationResult{}, recovery.ErrProviderUnresolved
	}
	p.refundCalls++
	return recovery.CompensationResult{}, &recovery.ProviderRefundFailedError{ProviderRef: "re_connected"}
}

type connectedInventory struct {
	t *testing.T
}

func (i *connectedInventory) Confirm(_ context.Context, reservationID, holdID uuid.UUID) error {
	i.t.Errorf("unexpected inventory Confirm reservation=%s hold=%s", reservationID, holdID)
	return errors.New("unexpected inventory confirm")
}

func (i *connectedInventory) Release(_ context.Context, reservationID, holdID uuid.UUID) error {
	i.t.Errorf("unexpected inventory Release reservation=%s hold=%s", reservationID, holdID)
	return errors.New("unexpected inventory release")
}

type connectedJournal struct {
	t *testing.T
}

func (j *connectedJournal) OrderFailed(_ context.Context, s store.StuckOrder) error {
	j.t.Errorf("unexpected journal OrderFailed for order=%s", s.OrderID)
	return errors.New("unexpected journal call")
}

type connectedCompleter struct {
	t *testing.T
}

func (c *connectedCompleter) Complete(_ context.Context, s store.StuckOrder) error {
	c.t.Errorf("unexpected completer call for order=%s", s.OrderID)
	return errors.New("unexpected completer call")
}

type isolatedStore struct {
	recovery.DBStore
	t       *testing.T
	orderID uuid.UUID
}

func (s isolatedStore) ClaimStuckOrders(ctx context.Context, _ int, lease time.Duration) ([]store.StuckOrder, error) {
	rows, err := s.DBStore.ClaimStuckOrders(ctx, 10000, lease)
	if err != nil {
		return nil, err
	}
	var isolated []store.StuckOrder
	for _, row := range rows {
		if row.OrderID == s.orderID {
			isolated = append(isolated, row)
			continue
		}
		if err := s.DBStore.AbandonRecoveryClaim(ctx, row.OrderID, row.ClaimID); err != nil {
			s.t.Errorf("abandon unrelated recovery claim for order %s: %v", row.OrderID, err)
			return nil, err
		}
	}
	return isolated, nil
}

func TestRunnerReparksAnUnparkedTerminalRefundInOnePass(t *testing.T) {
	db, ctx := store.OutboxDBForTest(t)
	seeded := store.SeedStuckForTest(t, "reconciliation_required")
	t.Cleanup(func() {
		if _, err := db.ExecContext(ctx, `DELETE FROM order_recovery_unparks WHERE order_id=$1`, seeded.OrderID); err != nil {
			t.Errorf("delete unpark records: %v", err)
		}
	})
	if err := db.QueryRowContext(ctx, `SELECT idempotency_key FROM orders WHERE id=$1`, seeded.OrderID).Scan(&seeded.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	// The smoke database is shared, so isolate the claim batch before the runner can drive other orders.
	// Abandoning other claims releases their leases without charging attempts; all later store calls use the real DBStore.
	dbStore := recovery.DBStore{DB: db}
	payments := &connectedPayments{t: t, organizerID: seeded.OrganizerID, key: seeded.IdempotencyKey}
	inventory := &connectedInventory{t: t}
	journal := &connectedJournal{t: t}
	completer := &connectedCompleter{t: t}
	runner, err := recovery.New(isolatedStore{DBStore: dbStore, t: t, orderID: seeded.OrderID}, payments, inventory, journal, completer,
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
	// The earlier park refreshed updated_at. Backdate it past ClaimStuckOrders' two-minute
	// grace period so the next pass tests the re-drive, not the grace window.
	if _, err := db.ExecContext(ctx, `UPDATE orders SET updated_at=now()-interval '10 minutes' WHERE id=$1`, seeded.OrderID); err != nil {
		t.Fatal(err)
	}

	runner.RunOnce(ctx)
	assertParked("second pass")
	if payments.refundCalls != 2 {
		t.Fatalf("refund calls after second pass = %d, want 2", payments.refundCalls)
	}

	// Re-parking also refreshes updated_at, so age the row before checking parked-row exclusion.
	if _, err := db.ExecContext(ctx, `UPDATE orders SET updated_at=now()-interval '10 minutes' WHERE id=$1`, seeded.OrderID); err != nil {
		t.Fatal(err)
	}
	runner.RunOnce(ctx)
	if payments.refundCalls != 2 {
		t.Fatalf("refund calls after third pass = %d, want 2 for a parked order", payments.refundCalls)
	}
	assertParked("third pass")
}
