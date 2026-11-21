package main

// Operator commands for SETTLED exchanges whose ticket switch never happened (TKT-498).
//
// Access refuses the switch when a source ticket was already admitted, so the exchange stays
// settled and unswitched for good: the buyer used the source tickets, the replacement order
// exists, and the target capacity stays claimed. The owner's resolution is to abandon the
// exchange (ADR-039 §2). The listing below is the read half; it reads Commerce only.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"

	store "ticketing/services/commerce/internal/store"
)

// listSettledUnswitchedExchanges reports one organizer's settled, unswitched exchanges.
//
// Usage: commerce list-settled-unswitched-exchanges <organizer-id>
//
// Exit code ZERO when there is nothing to list, as for list-parked and list-wedged-exchanges.
func listSettledUnswitchedExchanges(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: commerce list-settled-unswitched-exchanges <organizer-id>")
	}
	org, err := uuid.Parse(args[0])
	if err != nil {
		return fmt.Errorf("organizer id: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), operatorDBTimeout)
	defer cancel()
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	rows, err := store.ListSettledUnswitchedExchanges(ctx, db, org)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("no settled unswitched exchanges")
		return nil
	}
	printSettledUnswitched(os.Stdout, rows)
	// The caveat goes to stderr, next to the count, because it changes what the rows mean.
	fmt.Fprintf(os.Stderr, "\n%d settled exchange(s) without a ticket switch. These are CANDIDATES. "+
		"Commerce cannot see Access's tickets: a row can be a switch whose callback is late, or an "+
		"exchange Access refused because a source ticket was already admitted. This listing reads "+
		"Commerce only and cannot prove either. Do not act on a row from this listing alone. "+
		"`abandonment=refund-failed` means the provider refused the upgrade's delta refund: the "+
		"buyer is still charged and the target is still held, and running the abandonment again "+
		"reports the same failure without a new refund. See docs/runbooks/order-recovery.md.\n", len(rows))
	return nil
}

// printSettledUnswitched writes one line per exchange, with the abandonment's progress when
// one has started: `started`, `refund-failed` with the provider reference, or
// `target-returned` when only the terminal step is left.
func printSettledUnswitched(w io.Writer, rows []store.SettledExchange) {
	for _, r := range rows {
		state := "none"
		switch {
		case r.AbandonmentRefundFailedRef != "":
			state = "refund-failed"
		case r.AbandonmentTargetReturned:
			state = "target-returned"
		case r.AbandonmentStarted:
			state = "started"
		}
		fmt.Fprintf(w, "organizer=%s exchange=%s source_order=%s replacement_order=%s target_hold=%s "+
			"settled_at=%s quantity=%d delta=%s %s abandonment=%s",
			r.OrganizerID, r.ID, r.SourceOrderID, r.ReplacementOrderID, r.TargetHoldID,
			r.SettledAt.UTC().Format(time.RFC3339), r.Quantity, signedAmount(r.DeltaAmount), r.Currency, state)
		if r.AbandonmentStarted {
			fmt.Fprintf(w, " abandonment_started_at=%s operator=%q",
				r.AbandonmentStartedAt.UTC().Format(time.RFC3339), r.AbandonmentOperator)
		}
		if r.AbandonmentRefundFailedRef != "" {
			fmt.Fprintf(w, " refund_failed_ref=%s", r.AbandonmentRefundFailedRef)
		}
		fmt.Fprintln(w)
	}
}

// signedAmount renders a delta with its sign, so an upgrade reads `+1000` and not `1000`.
func signedAmount(v int64) string {
	if v > 0 {
		return fmt.Sprintf("+%d", v)
	}
	return fmt.Sprintf("%d", v)
}
