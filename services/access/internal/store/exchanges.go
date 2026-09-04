package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// The entitlement switch (TKT-166, ADR-039 §3).
//
// TKT-158 leaves an exchange in `switch_pending`: the money settled, the replacement order
// exists, and the buyer still holds VALID OLD TICKETS. That state under-sells and cannot
// oversell, which is why it was a safe place to stop. This closes it.
//
// The whole value of this function is that it is ONE TRANSACTION. Two would each be
// individually correct and jointly wrong:
//
//   - void, then issue → a window where NEITHER admits. The buyer is at the gate holding
//     tickets that just stopped working and replacements that do not exist yet.
//   - issue, then void → a window where BOTH admit. Two people walk in on one paid seat,
//     and inventory has no idea.
//
// Neither window is recoverable by a retry, because the harm lands during the window, not
// after it. So the void and the issue commit together or not at all.

// ErrExchangeTicketsNotIssued reports that the source order does not have exactly the
// expected number of live tickets to switch.
//
// The ordinary cause is benign and RETRYABLE: issuance is asynchronous (commerce's outbox
// → JetStream → this service), so an exchange settled seconds after checkout can genuinely
// arrive before the tickets it replaces exist. Answering "switched zero tickets" would
// report success for an obligation nobody discharged, and the replacement would be issued
// alongside a live original — the both-admit outcome by another route.
var ErrExchangeTicketsNotIssued = errors.New("source order does not have its full ticket set to exchange")

// ErrSourceTicketsAlreadyVoided reports source tickets already voided by something else —
// a refund, or another exchange. Commerce refuses to exchange a reversed order, so this is
// a race or a repair, not a normal path; switching anyway would append a second void to a
// ticket that already has one and collide on the singleton index halfway through.
var ErrSourceTicketsAlreadyVoided = errors.New("source order tickets are already voided")

// ErrSourceTicketsAlreadyAdmitted reports a source ticket that has already been used.
//
// This refusal exists because switching anyway is a DOUBLE ADMISSION (ai-review F1). The
// holder went through the door on the old ticket; voiding it and issuing a fresh
// unredeemed replacement lets them go through again, on one paid entitlement. The window
// is small — a scan during `switch_pending` — and it is real, and it grows with any delay
// in the switch.
//
// The cost is a settled exchange that never switches: the buyer paid the difference and
// keeps their used old ticket. That is NOT a new failure state. It is exactly what TKT-158
// shipped for every exchange, and this refusal simply keeps that behaviour for the one
// case where switching is unsafe. Under-selling one exchange beats admitting twice, and
// the obligation is visible (`tickets_exchanged_at IS NULL`) rather than silent.
//
// It is not the whole answer. Whether a used ticket should be exchangeable AT ALL —
// refused before the money moves — and whether the entry should instead carry forward to
// the replacement, which is not even binary for a multi-entry pass (ADR-005), is a product
// decision. TKT-169 owns it. This is the safe default until it is taken.
var ErrSourceTicketsAlreadyAdmitted = errors.New("source order tickets have already been admitted")

// SwitchExchangeInput is one exchange's switch: which source order loses its tickets, and
// which replacement tickets take their place.
type SwitchExchangeInput struct {
	// EventID is the domain event's id, which is also the dedupe key. Unlike a refund,
	// nothing here is caller-supplied beside it: the source order travels INSIDE the
	// event, so `consumed_events` alone binds the whole request.
	EventID       uuid.UUID
	ExchangeID    uuid.UUID
	SourceOrderID uuid.UUID
	OrganizerID   uuid.UUID
	// Tickets are the replacement tickets, already signed by the caller.
	Tickets []Ticket
}

// exchangedEventID derives the lifecycle event id for one (exchange, source ticket) pair,
// mirroring refundEventID: an event under this id can only have been written by this
// exchange against this ticket.
func exchangedEventID(exchangeID, ticketID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("exchanged:"+exchangeID.String()+":"+ticketID.String()))
}

// SwitchExchange voids every ticket of the source order and issues the replacement set, in
// one transaction.
//
// It is a sibling of Issue rather than a widening of it. Issue models creation; teaching it
// to lock a source order and invalidate its tickets would put exchange-only invariants on
// the path every ordinary checkout takes. They share the insert-and-append step instead.
func (p *Postgres) SwitchExchange(ctx context.Context, in SwitchExchangeInput) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Dedupe first and in the same transaction as the work, exactly as Issue does. A
	// redelivery after a successful commit finds the receipt and does nothing; a
	// redelivery after a ROLLBACK finds no receipt, because the receipt rolled back with
	// everything else. That is the property AC6 needs: a mid-switch failure leaves the
	// old tickets valid AND the event still owed.
	result, err := tx.ExecContext(ctx, `INSERT INTO consumed_events(event_id) VALUES($1) ON CONFLICT DO NOTHING`, in.EventID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return tx.Commit()
	}

	// Lock every source ticket in id order before deciding anything — the total order is
	// what makes concurrent operations on one order queue rather than deadlock, and the
	// lock is what stops a redeem committing between the check and the void.
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM tickets WHERE order_id=$1 AND organizer_id=$2 ORDER BY id FOR UPDATE`,
		in.SourceOrderID, in.OrganizerID)
	if err != nil {
		return err
	}
	var source []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		source = append(source, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	// An exchange replaces the WHOLE order — TKT-158 has no partial form — so the source
	// set must be complete before anything is voided. Fewer tickets than the replacement
	// quantity means issuance has not caught up yet.
	if len(source) == 0 || len(source) < len(in.Tickets) {
		return fmt.Errorf("%w: %d present, %d expected", ErrExchangeTicketsNotIssued, len(source), len(in.Tickets))
	}

	// Nothing here resolves a per-ticket replay the way RefundOrderTickets does. It does
	// not need to: the receipt above already answered that question for the entire
	// operation, and it did so in this transaction. A per-ticket check would be a second
	// idempotency mechanism disagreeing with the first.
	for _, id := range source {
		voided, err := ticketCommerciallyVoid(ctx, tx, id)
		if err != nil {
			return err
		}
		if voided {
			return fmt.Errorf("%w: ticket %s", ErrSourceTicketsAlreadyVoided, id)
		}
		// Checked under the same row lock as the void, so a scan cannot commit between
		// the two. Without the lock this would be a read that both paths could lose.
		admitted, err := ticketAdmittedUnion(ctx, tx, id)
		if err != nil {
			return err
		}
		if admitted {
			return fmt.Errorf("%w: ticket %s", ErrSourceTicketsAlreadyAdmitted, id)
		}
	}

	var identity TicketIdentity
	if err := tx.QueryRowContext(ctx, `SELECT order_id,organizer_id,slot_id FROM tickets WHERE id=$1`, source[0]).
		Scan(&identity.OrderID, &identity.OrganizerID, &identity.SlotID); err != nil {
		return err
	}
	for _, id := range source {
		if _, err := p.appendLifecycle(ctx, tx, appendInput{
			TicketID: id, OrderID: identity.OrderID, OrganizerID: identity.OrganizerID, SlotID: identity.SlotID,
			EventID: exchangedEventID(in.ExchangeID, id), Type: "exchanged",
		}); err != nil {
			return fmt.Errorf("void source ticket %s: %w", id, err)
		}
	}

	// The replacement, in the same transaction. Between the loop above and this one there
	// is no committed state at all — that is the point.
	for _, t := range in.Tickets {
		if err := p.insertIssuedTicket(ctx, tx, t); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ticketCommerciallyVoid reports whether a ticket has been voided by a refund or an
// exchange. Both are commercial facts that end a ticket's life; neither is a chain-health
// verdict.
func ticketCommerciallyVoid(ctx context.Context, tx *sql.Tx, ticketID uuid.UUID) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM lifecycle_events WHERE ticket_id=$1 AND event_type IN ('refunded','exchanged'))`,
		ticketID).Scan(&exists)
	return exists, err
}

// ticketExchanged reports whether a ticket was voided by an exchange. Read on the caller's
// transaction, under the ticket row lock the scan paths already hold — the same shape as
// ticketRefunded, and deliberately a separate verdict from it.
func ticketExchanged(ctx context.Context, tx *sql.Tx, ticketID uuid.UUID) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM lifecycle_events WHERE ticket_id=$1 AND event_type='exchanged')`, ticketID).Scan(&exists)
	return exists, err
}
