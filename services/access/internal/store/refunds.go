package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Ticket voiding on refund (TKT-157, ADR-038).
//
// A refund of q of an order's Q tickets voids exactly q of them. Two things have to
// be true and neither is free: the SELECTION must be deterministic, and it must be
// STABLE across a replay.
//
// Selection is the lowest ticket ids ascending among the order's not-yet-refunded
// tickets — deterministic, and available without inventing an issuance ordinal. It is
// not stable on its own: recomputing it after a first pass would skip the tickets
// that pass voided and select the NEXT q, voiding the order twice over.
//
// Stability comes from the lifecycle event id, which is derived from (refund, ticket).
// A replay looks for events this refund itself wrote and answers with those tickets —
// the trail already records the choice, under a name only this refund can produce.
//
// That covers "which tickets", and it is not the whole binding. It says nothing about
// which ORDER a refund id belongs to: presented against a different order, the same
// refund id derives different event ids, finds nothing of its own, and voids a fresh
// batch. `ticket_refund_batches` exists for exactly that one fact — (refund → order,
// quantity) — and deliberately does not duplicate the ticket ids the trail already
// holds (ai-review F2).

// ErrTicketsNotIssued reports that the order does not have q unrefunded tickets to
// void — including the case where it has none at all.
//
// This is the answer for a refund that outruns issuance. Tickets are issued
// asynchronously (commerce's outbox → JetStream → this service), so a refund seconds
// after checkout can genuinely arrive first. Treating that as "voided zero tickets"
// would report success for an obligation nobody discharged.
var ErrTicketsNotIssued = errors.New("order does not have enough unrefunded tickets")

// ErrRefundBatchConflict reports the same refund id replayed against a different
// order or quantity.
var ErrRefundBatchConflict = errors.New("refund id reused with a different request")

// TicketRefundBatch is one refund's voided tickets, in selection order.
type TicketRefundBatch struct {
	RefundID  uuid.UUID
	OrderID   uuid.UUID
	TicketIDs []uuid.UUID
	// Replay is true when this refund had already voided its tickets and nothing
	// was appended.
	Replay bool
}

// refundEventID derives the lifecycle event id for one (refund, ticket) pair. It is
// what makes a replay identifiable: an event under this id can only have been written
// by this refund against this ticket.
func refundEventID(refundID, ticketID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("refunded:"+refundID.String()+":"+ticketID.String()))
}

// RefundOrderTickets voids q of an order's tickets and returns which ones.
func (p *Postgres) RefundOrderTickets(ctx context.Context, org, order, refundID uuid.UUID, quantity int32) (TicketRefundBatch, error) {
	if quantity < 1 {
		return TicketRefundBatch{}, errors.New("refund quantity must be positive")
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return TicketRefundBatch{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// Lock EVERY ticket of the order, in id order, before deciding anything. Two
	// refunds of one order would otherwise both read the same unrefunded set and
	// select the same tickets; locking in a total order also means they can only
	// queue, never deadlock.
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM tickets WHERE order_id=$1 AND organizer_id=$2 ORDER BY id FOR UPDATE`, order, org)
	if err != nil {
		return TicketRefundBatch{}, err
	}
	var all []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return TicketRefundBatch{}, err
		}
		all = append(all, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return TicketRefundBatch{}, err
	}
	_ = rows.Close()

	// The refund id binds to exactly one (order, quantity), and that binding is checked
	// BEFORE the per-ticket replay below. The event-id derivation cannot express it: the
	// same refund id presented against a different order produces different event ids,
	// so the per-ticket check would find nothing of its own and void a fresh batch
	// (ai-review F2). The binding is what makes ErrRefundBatchConflict true.
	var boundOrder uuid.UUID
	var boundQty int32
	err = tx.QueryRowContext(ctx, `SELECT order_id,quantity FROM ticket_refund_batches WHERE organizer_id=$1 AND refund_id=$2`, org, refundID).
		Scan(&boundOrder, &boundQty)
	switch {
	case err == nil:
		if boundOrder != order || boundQty != quantity {
			return TicketRefundBatch{}, ErrRefundBatchConflict
		}
	case !errors.Is(err, sql.ErrNoRows):
		return TicketRefundBatch{}, err
	}

	// The per-ticket replay resolves BEFORE any selection and appends nothing —
	// appendLifecycle's own contract is that idempotency is settled before it is
	// called, never inside it.
	var mine, free []uuid.UUID
	for _, id := range all {
		voidedBy, err := refundThatVoided(ctx, tx, id)
		if err != nil {
			return TicketRefundBatch{}, err
		}
		switch {
		case voidedBy == uuid.Nil:
			free = append(free, id)
		case voidedBy == refundEventID(refundID, id):
			mine = append(mine, id)
		}
	}
	if len(mine) > 0 {
		if int32(len(mine)) != quantity {
			return TicketRefundBatch{}, ErrRefundBatchConflict
		}
		return TicketRefundBatch{RefundID: refundID, OrderID: order, TicketIDs: mine, Replay: true}, tx.Commit()
	}

	if int32(len(free)) < quantity {
		return TicketRefundBatch{}, fmt.Errorf("%w: %d of %d available", ErrTicketsNotIssued, len(free), quantity)
	}
	selected := free[:quantity]

	// Claim the binding in the same transaction as the events. A concurrent request
	// reusing this refund id against another order loses on the primary key rather than
	// voiding a second batch.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ticket_refund_batches(organizer_id,refund_id,order_id,quantity) VALUES($1,$2,$3,$4)`,
		org, refundID, order, quantity); err != nil {
		return TicketRefundBatch{}, err
	}

	var identity TicketIdentity
	if err := tx.QueryRowContext(ctx, `SELECT order_id,organizer_id,slot_id FROM tickets WHERE id=$1`, selected[0]).
		Scan(&identity.OrderID, &identity.OrganizerID, &identity.SlotID); err != nil {
		return TicketRefundBatch{}, err
	}
	for _, id := range selected {
		if _, err := p.appendLifecycle(ctx, tx, appendInput{
			TicketID: id, OrderID: identity.OrderID, OrganizerID: identity.OrganizerID, SlotID: identity.SlotID,
			EventID: refundEventID(refundID, id), Type: "refunded",
		}); err != nil {
			return TicketRefundBatch{}, fmt.Errorf("void ticket %s: %w", id, err)
		}
	}
	return TicketRefundBatch{RefundID: refundID, OrderID: order, TicketIDs: selected}, tx.Commit()
}

// refundThatVoided returns the id of the `refunded` lifecycle event on a ticket, or
// uuid.Nil when the ticket has not been voided. The event id is what identifies the
// refund that wrote it.
func refundThatVoided(ctx context.Context, tx *sql.Tx, ticketID uuid.UUID) (uuid.UUID, error) {
	var eventID uuid.UUID
	err := tx.QueryRowContext(ctx, `SELECT id FROM lifecycle_events WHERE ticket_id=$1 AND event_type='refunded'`, ticketID).Scan(&eventID)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, err
	}
	return eventID, nil
}

// ticketRefunded reports whether a ticket has been voided by a refund. Read on the
// caller's transaction, under the ticket row lock the scan paths already hold.
func ticketRefunded(ctx context.Context, tx *sql.Tx, ticketID uuid.UUID) (bool, error) {
	id, err := refundThatVoided(ctx, tx, ticketID)
	return id != uuid.Nil, err
}
