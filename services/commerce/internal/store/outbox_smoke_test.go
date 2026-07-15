//go:build smoke

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// These tests assert the invariants ADR-016 §Decision 6 claims. Each one fails on the
// pre-outbox code: completion committed without an owed event, which is the
// paid-but-no-ticket window.

// outboxDB opens the store-test database and ensures the schema exists.
//
// This database is deliberately NOT the live service's: the commerce container runs an
// outbox drainer polling the same table, and it would claim and retire rows these tests
// seed. smoke.sh creates a dedicated one; migrating here keeps the tests runnable
// against any empty database.
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
	migrateOnce.Do(func() { migrateErr = Migrate(ctx, db) })
	if migrateErr != nil {
		t.Fatalf("migrate store-test database: %v", migrateErr)
	}
	return db, ctx
}

var (
	migrateOnce sync.Once
	migrateErr  error
)

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

	claimed, err := ClaimOutbox(ctx, db, 10, time.Minute)
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

// Concurrent drainers (multi-replica) must not both claim the same row, and every row
// must be claimed by someone.
//
// The completeness assertions matter as much as the disjointness one: an earlier
// version of this test only iterated the rows it happened to see, so claiming NOTHING
// passed vacuously. A concurrency test that can pass while doing no work proves nothing.
func TestConcurrentDrainersClaimDisjointRows(t *testing.T) {
	db, ctx := outboxDB(t)
	const orders = 6
	ids := map[uuid.UUID]bool{}
	for range orders {
		c, candidate := seedCompletable(t, db, ctx, "outbox-concurrent-"+uuid.New().String())
		if _, err := CompleteOrder(ctx, db, c, candidate); err != nil {
			t.Fatal(err)
		}
		ids[c.OrderID] = true
	}

	const drainers = 4
	var mu sync.Mutex
	seen := map[uuid.UUID]int{}
	errs := make(chan error, drainers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range drainers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			msgs, err := ClaimOutbox(ctx, db, orders, time.Minute)
			if err != nil {
				errs <- err // never swallow: a failed claim must not look like a clean pass
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, m := range msgs {
				if ids[m.OrderID] {
					seen[m.OrderID]++
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent claim: %v", err)
	}

	if len(seen) != orders {
		t.Fatalf("claimed %d of %d owed events; every owed event must be claimed by someone", len(seen), orders)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("order %s claimed by %d concurrent drainers; want exactly 1", id, n)
		}
	}
}

// A claimant whose lease lapsed mid-publish must not be able to disturb the drainer
// that superseded it. Without a claim token, the stale release clears the new lease and
// a third drainer can publish concurrently with the second.
func TestStaleClaimantCannotReleaseOrRetireANewerClaim(t *testing.T) {
	db, ctx := outboxDB(t)
	c, candidate := seedCompletable(t, db, ctx, "outbox-stale-claim")
	if _, err := CompleteOrder(ctx, db, c, candidate); err != nil {
		t.Fatal(err)
	}

	// D1 claims, then stalls (simulated by expiring its lease).
	d1 := claimOne(t, db, ctx, c.OrderID)
	if _, err := db.ExecContext(ctx, `UPDATE completion_outbox SET lease_until=now()-interval '1 second' WHERE order_id=$1`, c.OrderID); err != nil {
		t.Fatal(err)
	}
	// D2 takes over.
	d2 := claimOne(t, db, ctx, c.OrderID)
	if d1.ClaimID == d2.ClaimID {
		t.Fatal("second claim must carry a distinct claim id")
	}

	// D1 finally fails and tries to release. It must not clear D2's lease.
	if err := ReleaseOutbox(ctx, db, d1.EventID, d1.ClaimID, errors.New("stale publish failed")); err != nil {
		t.Fatal(err)
	}
	var claim uuid.NullUUID
	if err := db.QueryRowContext(ctx, `SELECT claim_id FROM completion_outbox WHERE order_id=$1`, c.OrderID).Scan(&claim); err != nil {
		t.Fatal(err)
	}
	if !claim.Valid || claim.UUID != d2.ClaimID {
		t.Fatalf("stale claimant cleared the live claim: claim_id=%v, want %s", claim, d2.ClaimID)
	}

	// D1 must not be able to retire it either.
	retired, err := MarkPublished(ctx, db, d1.EventID, d1.ClaimID)
	if err != nil {
		t.Fatal(err)
	}
	if retired {
		t.Fatal("stale claimant retired a row it no longer holds; that would mask a publication that never happened")
	}
	// D2 still can.
	retired, err = MarkPublished(ctx, db, d2.EventID, d2.ClaimID)
	if err != nil {
		t.Fatal(err)
	}
	if !retired {
		t.Fatal("the live claimant must be able to retire its own row")
	}
}

// A permanently-failing row must not starve newer orders. Claiming is oldest-first, so
// without backoff + dead-lettering the poison row is re-selected on every pass forever.
func TestPoisonRowDoesNotStarveNewerEvents(t *testing.T) {
	db, ctx := outboxDB(t)
	poison, candidate := seedCompletable(t, db, ctx, "outbox-poison")
	if _, err := CompleteOrder(ctx, db, poison, candidate); err != nil {
		t.Fatal(err)
	}

	// Burn its attempts, as a drainer failing to publish repeatedly would.
	for range MaxOutboxAttempts + 1 {
		msgs, err := ClaimOutbox(ctx, db, 10, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			if m.OrderID != poison.OrderID {
				continue
			}
			if err := ReleaseOutbox(ctx, db, m.EventID, m.ClaimID, errors.New("poison")); err != nil {
				t.Fatal(err)
			}
		}
		// Defeat the backoff so the loop can exhaust attempts promptly.
		if _, err := db.ExecContext(ctx, `UPDATE completion_outbox SET next_attempt_at=now() WHERE order_id=$1 AND dead_lettered_at IS NULL`, poison.OrderID); err != nil {
			t.Fatal(err)
		}
	}

	var dead sql.NullTime
	if err := db.QueryRowContext(ctx, `SELECT dead_lettered_at FROM completion_outbox WHERE order_id=$1`, poison.OrderID).Scan(&dead); err != nil {
		t.Fatal(err)
	}
	if !dead.Valid {
		t.Fatalf("row failing %d times must be dead-lettered, not retried forever", MaxOutboxAttempts)
	}

	// A newer order must now drain despite the poison row being older.
	fresh, freshRef := seedCompletable(t, db, ctx, "outbox-after-poison")
	if _, err := CompleteOrder(ctx, db, fresh, freshRef); err != nil {
		t.Fatal(err)
	}
	msgs, err := ClaimOutbox(ctx, db, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var sawFresh, sawPoison bool
	for _, m := range msgs {
		if m.OrderID == fresh.OrderID {
			sawFresh = true
		}
		if m.OrderID == poison.OrderID {
			sawPoison = true
		}
	}
	if !sawFresh {
		t.Fatal("a newer owed event must be claimable while an older poison row exists")
	}
	if sawPoison {
		t.Fatal("a dead-lettered row must never be claimed again")
	}
}

// Backfill exists because CompleteOrder's already-completed short-circuit never inserts
// an outbox row: without it, every order completed before this migration stays
// unrecoverable — the exact paid-but-no-ticket bug the outbox closes.
func TestBackfillOwesEventsForPreExistingCompletedOrders(t *testing.T) {
	db, ctx := outboxDB(t)
	c, candidate := seedCompletable(t, db, ctx, "outbox-backfill")

	// Complete the order the way the pre-outbox code did: no outbox row.
	if _, err := db.ExecContext(ctx, `UPDATE reservations SET status='completed' WHERE id=$1`, c.ReservationID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE orders SET status='completed',guest_order_ref=$2,updated_at=now() WHERE id=$1`, c.OrderID, candidate); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM completion_outbox WHERE order_id=$1`, c.OrderID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("precondition: pre-outbox completion must own no row, got %d", n)
	}

	if _, err := BackfillCompletionOutbox(ctx, db); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	m := claimOne(t, db, ctx, c.OrderID)
	if len(m.Envelope) == 0 {
		t.Fatal("backfilled row carries no envelope")
	}
	// The envelope must describe the order, not the backfill run.
	var env struct {
		ID   uuid.UUID `json:"id"`
		Data struct {
			OrderID       uuid.UUID `json:"order_id"`
			GuestOrderRef uuid.UUID `json:"guest_order_ref"`
		} `json:"data"`
	}
	if err := json.Unmarshal(m.Envelope, &env); err != nil {
		t.Fatalf("backfilled envelope is not valid JSON: %v", err)
	}
	if env.Data.OrderID != c.OrderID || env.Data.GuestOrderRef != candidate {
		t.Fatalf("backfilled envelope = order %s ref %s; want %s / %s",
			env.Data.OrderID, env.Data.GuestOrderRef, c.OrderID, candidate)
	}
	if env.ID != m.EventID {
		t.Fatalf("backfilled envelope id %s != row event id %s", env.ID, m.EventID)
	}

	// Idempotent: a second run owes nothing new.
	before := countRows(t, db, ctx, c.OrderID)
	if _, err := BackfillCompletionOutbox(ctx, db); err != nil {
		t.Fatal(err)
	}
	if after := countRows(t, db, ctx, c.OrderID); after != before {
		t.Fatalf("backfill is not idempotent: %d -> %d rows", before, after)
	}
}

func countRows(t *testing.T, db *sql.DB, ctx context.Context, order uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM completion_outbox WHERE order_id=$1`, order).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
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
	retired, err := MarkPublished(ctx, db, m.EventID, m.ClaimID)
	if err != nil {
		t.Fatalf("mark published: %v", err)
	}
	if !retired {
		t.Fatal("the claim holder must be able to retire its row")
	}

	// Even with every lease expired, a published row is never claimable again.
	if _, err := db.ExecContext(ctx, `UPDATE completion_outbox SET lease_until=NULL`); err != nil {
		t.Fatal(err)
	}
	claimed, err := ClaimOutbox(ctx, db, 50, time.Minute)
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
	msgs, err := ClaimOutbox(ctx, db, 50, time.Minute)
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
