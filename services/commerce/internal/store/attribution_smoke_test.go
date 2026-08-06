//go:build smoke

package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Order attribution against real Postgres (TKT-221 / US-A2, migration 0016).
//
// The claims worth proving here are the ones only the database can settle: that
// attribution written at claim time SURVIVES completion and recovery untouched,
// that a guest order really stores NULL, and that the FK refuses an account that
// does not exist.

func seedOrder(t *testing.T, db *sql.DB, ctx context.Context, customer uuid.NullUUID) (Completion, uuid.UUID) {
	t.Helper()
	c := Completion{
		ReservationID: uuid.New(), OrderID: uuid.New(), OrganizerID: uuid.New(),
		BuyerID: uuid.New(), SlotID: uuid.New(), TicketTypeID: uuid.New(), Quantity: 2,
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,2,1500,3000,3000,'EUR','held')`,
		c.ReservationID, c.OrganizerID, uuid.New(), c.SlotID, c.TicketTypeID, c.BuyerID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,customer_id)
		VALUES($1,$2,'created',$3,'fingerprint',$4)`,
		c.OrderID, c.ReservationID, "attribution-"+uuid.NewString(), customer); err != nil {
		t.Fatal(err)
	}
	return c, uuid.New()
}

func registerForAttribution(t *testing.T, db *sql.DB, ctx context.Context) uuid.UUID {
	t.Helper()
	account, err := RegisterCustomer(ctx, db, uniqueEmail("attribution"), "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	return account.ID
}

func attributionOf(t *testing.T, db *sql.DB, ctx context.Context, order uuid.UUID) uuid.NullUUID {
	t.Helper()
	var got uuid.NullUUID
	if err := db.QueryRowContext(ctx, `SELECT customer_id FROM orders WHERE id=$1`, order).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

// Completion must not touch the attribution — it is written once, at claim time,
// and read by nobody in between. This is the assertion that would fail if someone
// "helpfully" rewrote customer_id during completion from a request that, by then,
// may not exist: an order can be completed minutes later by the recovery runner,
// after the assertion expired and the storefront session is gone.
func TestCompletionLeavesAttributionUntouched(t *testing.T) {
	db, ctx := outboxDB(t)
	customer := registerForAttribution(t, db, ctx)

	c, candidate := seedOrder(t, db, ctx, uuid.NullUUID{UUID: customer, Valid: true})
	if _, err := CompleteOrder(ctx, db, c, candidate); err != nil {
		t.Fatalf("complete: %v", err)
	}

	got := attributionOf(t, db, ctx, c.OrderID)
	if !got.Valid || got.UUID != customer {
		t.Fatalf("attribution after completion = %v, want %s", got, customer)
	}
}

// Guest is the default and stays NULL through the whole lifecycle. A completion
// that invented an attribution — or a column defaulting to anything but NULL —
// would make every guest purchase look like it belonged to someone.
func TestGuestOrderStaysUnattributedThroughCompletion(t *testing.T) {
	db, ctx := outboxDB(t)

	c, candidate := seedOrder(t, db, ctx, uuid.NullUUID{})
	if got := attributionOf(t, db, ctx, c.OrderID); got.Valid {
		t.Fatalf("a guest order was attributed at claim time: %v", got)
	}
	if _, err := CompleteOrder(ctx, db, c, candidate); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got := attributionOf(t, db, ctx, c.OrderID); got.Valid {
		t.Fatalf("a guest order became attributed at completion: %v", got)
	}
}

// The FK is not decorative. An attribution naming an account that does not exist
// is unusable by the wallet TKT-222 builds, and it would fail silently there
// rather than loudly here.
func TestAttributionRefusesAnAccountThatDoesNotExist(t *testing.T) {
	db, ctx := outboxDB(t)

	reservation := uuid.New()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reservations(id,organizer_id,hold_id,slot_id,ticket_type_id,buyer_id,quantity,unit_amount,total_amount,face_value_amount,currency,status)
		VALUES($1,$2,$3,$4,$5,$6,1,1000,1000,1000,'EUR','held')`,
		reservation, uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO orders(id,reservation_id,status,idempotency_key,request_fingerprint,customer_id)
		VALUES($1,$2,'created',$3,'fingerprint',$4)`,
		uuid.New(), reservation, "fk-"+uuid.NewString(), uuid.New())
	if err == nil {
		t.Fatal("an order was attributed to an account that does not exist")
	}
}

// Rolling 0016 back with any attribution present must refuse: dropping the column
// silently turns every account's purchase history into guest orders.
func TestAttributionDownRefusesToDiscardAccountHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, provider := schemaDB(t, ctx)

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	customer := registerForAttribution(t, db, ctx)
	seedOrder(t, db, ctx, uuid.NullUUID{UUID: customer, Valid: true})

	// Pin the aim at 0016 — provider.Down() rolls back exactly one migration, so
	// without this the assertion drifts onto whatever lands next (TKT-173, and
	// again on TKT-220).
	if _, err := provider.DownTo(ctx, 16); err != nil {
		t.Fatalf("unwind to 0016: %v", err)
	}
	if _, err := provider.Down(ctx); err == nil {
		t.Fatal("0016 rolled back over an attributed order — account history would be silently discarded")
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM orders WHERE customer_id IS NOT NULL`).Scan(&count); err != nil {
		t.Fatalf("the column did not survive the refused rollback: %v", err)
	}
	if count != 1 {
		t.Fatalf("attributed orders after the refused rollback = %d, want 1", count)
	}
}
