//go:build smoke

package store

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// These tests assert the invariants ADR-016 §Decision 6 claims. Each one fails on the
// pre-outbox code: completion committed without an owed event, which is the
// paid-but-no-ticket window.

func outboxDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("COMMERCE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("COMMERCE_TEST_DATABASE_URL is not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return db, ctx
}

// seedCompletable creates a reservation + order ready for CompleteOrder, and returns
// the completion payload the envelope is built from.
func seedCompletable(t *testing.T, db *sql.DB, ctx context.Context, key string) (Completion, uuid.UUID) {
	t.Helper()
	c := Completion{
		ReservationID: uuid.New(),
		OrderID:       uuid.New(),
		OrganizerID:   uuid.New(),
		BuyerID:       uuid.New(),
		SlotID:        uuid.New(),
		TicketTypeID:  uuid.New(),
		Quantity:      2,
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,$7,1250,2500,'EUR','finalizing')`,
		c.ReservationID, c.OrganizerID, uuid.New(), c.SlotID, c.TicketTypeID, c.BuyerID, c.Quantity); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint) VALUES($1,$2,'created',$3,'fingerprint')`,
		c.OrderID, c.ReservationID, key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM completion_outbox WHERE order_id=$1`, c.OrderID)
		_, _ = db.Exec(`DELETE FROM orders WHERE id=$1`, c.OrderID)
		_, _ = db.Exec(`DELETE FROM reservations WHERE id=$1`, c.ReservationID)
	})
	return c, uuid.New()
}

// The core ADR-016 invariant: completion and the owed event commit together.
func TestCompleteOrderWritesOutboxRowInSameTransaction(t *testing.T) {
	db, ctx := outboxDB(t)
	c, candidate := seedCompletable(t, db, ctx, "outbox-same-tx")

	if _, err := CompleteOrder(ctx, db, c, candidate); err != nil {
		t.Fatalf("complete order: %v", err)
	}

	var status string
	var published sql.NullTime
	var subject string
	if err := db.QueryRowContext(ctx, `
		SELECT o.status, x.published_at, x.subject
		FROM orders o JOIN completion_outbox x ON x.order_id = o.id
		WHERE o.id=$1`, c.OrderID).Scan(&status, &published, &subject); err != nil {
		t.Fatalf("completed order must own an outbox row: %v", err)
	}
	if status != "completed" {
		t.Fatalf("order status = %q, want completed", status)
	}
	if published.Valid {
		t.Fatal("outbox row must be unpublished at completion time; publishing happens after commit")
	}
	if subject != "platform.commerce.order.completed" {
		t.Fatalf("subject = %q", subject)
	}
}

// A crash between commit and publish must leave a claimable row — this is the
// paid-but-no-ticket window the outbox exists to close.
func TestUnpublishedCompletionIsClaimableAfterRestart(t *testing.T) {
	db, ctx := outboxDB(t)
	c, candidate := seedCompletable(t, db, ctx, "outbox-restart")
	if _, err := CompleteOrder(ctx, db, c, candidate); err != nil {
		t.Fatal(err)
	}
	// No publish happened: this is exactly the post-commit crash state.

	claimed, err := ClaimOutbox(ctx, db, 10)
	if err != nil {
		t.Fatalf("claim outbox: %v", err)
	}
	var found bool
	for _, m := range claimed {
		if m.OrderID == c.OrderID {
			found = true
			if len(m.Envelope) == 0 {
				t.Fatal("claimed message carries no frozen envelope")
			}
		}
	}
	if !found {
		t.Fatal("a completed-but-unpublished order must be claimable after restart")
	}
}

// The envelope is frozen at completion: two claims of the same message must yield
// byte-identical payloads. Rebuilding per attempt (the pre-outbox publisher) fails this.
func TestClaimedEnvelopeIsFrozenAcrossAttempts(t *testing.T) {
	db, ctx := outboxDB(t)
	c, candidate := seedCompletable(t, db, ctx, "outbox-frozen")
	if _, err := CompleteOrder(ctx, db, c, candidate); err != nil {
		t.Fatal(err)
	}

	first := claimOne(t, db, ctx, c.OrderID)
	// Expire the lease so the same row is claimable again, as it would be after a
	// failed publish attempt.
	if _, err := db.ExecContext(ctx, `UPDATE completion_outbox SET lease_until=now()-interval '1 minute' WHERE order_id=$1`, c.OrderID); err != nil {
		t.Fatal(err)
	}
	second := claimOne(t, db, ctx, c.OrderID)

	if string(first.Envelope) != string(second.Envelope) {
		t.Fatalf("envelope changed between attempts:\n first=%s\nsecond=%s", first.Envelope, second.Envelope)
	}
	if first.EventID != second.EventID {
		t.Fatalf("event id changed between attempts: %s != %s", first.EventID, second.EventID)
	}
}

// Concurrent drainers (multi-replica) must not both claim the same row.
func TestConcurrentDrainersClaimDisjointRows(t *testing.T) {
	db, ctx := outboxDB(t)
	const orders = 6
	ids := map[uuid.UUID]bool{}
	for i := range orders {
		c, candidate := seedCompletable(t, db, ctx, "outbox-concurrent-"+uuid.New().String())
		if _, err := CompleteOrder(ctx, db, c, candidate); err != nil {
			t.Fatal(err)
		}
		ids[c.OrderID] = true
		_ = i
	}

	const drainers = 4
	var mu sync.Mutex
	seen := map[uuid.UUID]int{}
	var total atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range drainers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			msgs, err := ClaimOutbox(ctx, db, orders)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, m := range msgs {
				if ids[m.OrderID] {
					seen[m.OrderID]++
					total.Add(1)
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	for id, n := range seen {
		if n != 1 {
			t.Fatalf("order %s claimed by %d concurrent drainers; want 1", id, n)
		}
	}
}

// Draining is at-least-once with a deterministic id, so re-draining a published row
// must not happen: MarkPublished removes it from the claimable set permanently.
func TestMarkPublishedRemovesRowFromClaimableSet(t *testing.T) {
	db, ctx := outboxDB(t)
	c, candidate := seedCompletable(t, db, ctx, "outbox-published")
	if _, err := CompleteOrder(ctx, db, c, candidate); err != nil {
		t.Fatal(err)
	}
	m := claimOne(t, db, ctx, c.OrderID)
	if err := MarkPublished(ctx, db, m.EventID); err != nil {
		t.Fatalf("mark published: %v", err)
	}

	// Even with every lease expired, a published row is never claimable again.
	if _, err := db.ExecContext(ctx, `UPDATE completion_outbox SET lease_until=NULL`); err != nil {
		t.Fatal(err)
	}
	claimed, err := ClaimOutbox(ctx, db, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range claimed {
		if got.OrderID == c.OrderID {
			t.Fatal("published row must never be claimed again")
		}
	}
}

// A replayed completion must not owe a second event: the deterministic event id is
// the outbox PK, so the idempotent CompleteOrder short-circuit stays single-event.
func TestReplayedCompletionOwesExactlyOneEvent(t *testing.T) {
	db, ctx := outboxDB(t)
	c, candidate := seedCompletable(t, db, ctx, "outbox-replay")
	if _, err := CompleteOrder(ctx, db, c, candidate); err != nil {
		t.Fatal(err)
	}
	// Replay: same order, already completed. Returns the persisted ref, owes nothing new.
	if _, err := CompleteOrder(ctx, db, c, uuid.New()); err != nil {
		t.Fatalf("replayed completion: %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM completion_outbox WHERE order_id=$1`, c.OrderID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("outbox rows for replayed completion = %d; want 1", n)
	}
}

func claimOne(t *testing.T, db *sql.DB, ctx context.Context, order uuid.UUID) OutboxMessage {
	t.Helper()
	msgs, err := ClaimOutbox(ctx, db, 50)
	if err != nil {
		t.Fatalf("claim outbox: %v", err)
	}
	for _, m := range msgs {
		if m.OrderID == order {
			return m
		}
	}
	t.Fatalf("order %s not claimable", order)
	return OutboxMessage{}
}
