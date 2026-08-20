package main

// Operator commands for parked recovery orders (TKT-146).
//
// A parked order is one the recovery runner gave up on: ReleaseStuckOrder sets
// recovery_parked_at once the attempts budget is exhausted, and ClaimStuckOrders excludes
// parked rows, so nothing in the service revisits it. ADR-016 chose that on purpose — an
// order that failed ten re-drives should not keep failing them on a timer — but it left the
// human it defers to with nothing to look at and nothing to press.
//
// CLI subcommands rather than an HTTP surface, following commerce's own enrol-reseller
// shape: unparking is a deliberate operator act taken during an incident, and a command that
// must work during an incident should not depend on the service being up.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"

	store "ticketing/services/commerce/internal/store"
)

// operatorDBTimeout bounds both commands. Generous for a listing of a population bounded by
// attempt exhaustion, and for a single-row update under a lock.
const operatorDBTimeout = 30 * time.Second

// listParked reports every order the recovery runner has given up on.
//
// Usage: commerce list-parked
//
// Exit code ZERO when there is nothing parked — "nothing to do" is not a failure, the same
// contract inventory's reconcile-pins states. Non-zero is reserved for a store failure, so a
// wrapper can tell an empty queue from a broken one.
func listParked(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: commerce list-parked (no arguments)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), operatorDBTimeout)
	defer cancel()
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	parked, err := store.ListParkedOrders(ctx, db)
	if err != nil {
		return err
	}
	if len(parked) == 0 {
		fmt.Println("no parked orders")
		return nil
	}
	for _, p := range parked {
		fmt.Printf("order=%s status=%s attempts=%d parked_at=%s terminal_outcome=%s last_error=%s\n",
			p.OrderID, p.Status, p.Attempts, p.ParkedAt.UTC().Format(time.RFC3339),
			nullableField(p.TerminalOutcome), nullableField(p.LastError))
	}
	fmt.Fprintf(os.Stderr, "\n%d parked order(s). Each one is excluded from recovery until an operator "+
		"unparks it. Read last_error first, and establish what the order actually needs: a "+
		"`reconciliation_required` order may hold CAPTURED money, and unparking it asks the runner to "+
		"re-decide on PSP evidence alone. A capture with positive durable evidence is submitted for "+
		"REFUND, and only then does the release discover whether the claim was already confirmed; a "+
		"capture with no such evidence re-parks without refunding. See docs/development.md.\n",
		len(parked))
	return nil
}

// nullableField renders an absent value distinguishably from an empty one. `last_error=` and
// `last_error=<none>` mean different things to an operator deciding whether a park had a
// stated cause, and collapsing them loses the distinction the column exists to carry.
func nullableField(v sql.NullString) string {
	if !v.Valid || v.String == "" {
		return "<none>"
	}
	return v.String
}

// unparkOrder returns one parked order to the recovery runner's claimable set.
//
// Usage: commerce unpark-order <order-id> <reason>
//
// The reason is required and is recorded durably. It is the only part of the record a later
// reader cannot reconstruct from the row: everything else — what the order was, how many
// attempts it burned, what the last error said — is captured automatically.
//
// This does NOT resolve the order. It makes the order eligible to be driven again by the
// existing runner under the existing rules; whether that succeeds depends on whatever was
// failing. For a `reconciliation_required` order holding captured money the operator's real
// work — establishing whether the claim is confirmed, and whether a refund is the right
// answer — happens BEFORE this command, not after it: unparking hands that decision back to
// the runner, which decides on PSP evidence alone. See docs/development.md.
func unparkOrder(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: commerce unpark-order <order-id> <reason>")
	}
	id, err := uuid.Parse(args[0])
	if err != nil {
		return fmt.Errorf("order id: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), operatorDBTimeout)
	defer cancel()
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := store.UnparkOrder(ctx, db, id, args[1]); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "order %s unparked with a fresh retry budget; the recovery runner will claim it "+
		"on its next pass. This did not resolve the order — it made it drivable again.\n", id)
	return nil
}
