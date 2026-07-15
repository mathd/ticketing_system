//go:build smoke

package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestCompleteOrderReturnsOneCanonicalReferenceConcurrently(t *testing.T) {
	dsn := os.Getenv("COMMERCE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("COMMERCE_TEST_DATABASE_URL is not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	reservationID, orderID := uuid.New(), uuid.New()
	organizerID, holdID, slotID := uuid.New(), uuid.New(), uuid.New()
	ticketTypeID, buyerID := uuid.New(), uuid.New()
	_, err = db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,2,1250,2500,'EUR','finalizing')`,
		reservationID, organizerID, holdID, slotID, ticketTypeID, buyerID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint) VALUES($1,$2,'created','completion-race','fingerprint')`, orderID, reservationID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM completion_outbox WHERE order_id=$1`, orderID)
		_, _ = db.Exec(`DELETE FROM orders WHERE id=$1`, orderID)
		_, _ = db.Exec(`DELETE FROM reservations WHERE id=$1`, reservationID)
	})

	const callers = 8
	start := make(chan struct{})
	refs := make(chan uuid.UUID, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			candidate := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("candidate-%d", i)))
			ref, err := CompleteOrder(ctx, db, Completion{
				ReservationID: reservationID, OrderID: orderID, OrganizerID: organizerID,
				BuyerID: buyerID, SlotID: slotID, TicketTypeID: ticketTypeID, Quantity: 2,
			}, candidate)
			refs <- ref
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(refs)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("complete order: %v", err)
		}
	}
	var canonical uuid.UUID
	for ref := range refs {
		if canonical == uuid.Nil {
			canonical = ref
		}
		if ref != canonical {
			t.Fatalf("completion references differ: %s != %s", ref, canonical)
		}
	}
	var orderStatus, reservationStatus string
	var persisted uuid.UUID
	if err := db.QueryRowContext(ctx, `SELECT o.status,o.guest_order_ref,r.status FROM orders o JOIN reservations r ON r.id=o.reservation_id WHERE o.id=$1`, orderID).Scan(&orderStatus, &persisted, &reservationStatus); err != nil {
		t.Fatal(err)
	}
	if orderStatus != "completed" || reservationStatus != "completed" || persisted != canonical {
		t.Fatalf("persisted completion = order %q reservation %q ref %s; want completed/completed/%s", orderStatus, reservationStatus, persisted, canonical)
	}

	// The winner owes the event exactly once, and the losers — who took the
	// already-completed short-circuit — owe nothing further.
	var owed int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM completion_outbox WHERE order_id=$1`, orderID).Scan(&owed); err != nil {
		t.Fatal(err)
	}
	if owed != 1 {
		t.Fatalf("owed completion events after %d concurrent completions = %d; want 1", callers, owed)
	}
}
