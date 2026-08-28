package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Staff-triggered redelivery of a completed order's tickets (TKT-203, ADR-068).
//
// A resend is a NEW write, not a replay of the automatic delivery on issuance, and
// three mechanisms in the original path are built to prevent exactly what this one
// must do:
//
//   - PendingDeliveries filters on NOT EXISTS (... event_type='delivered'), so the
//     tickets a resend exists to serve are precisely the ones it excludes. This file
//     selects the order's tickets directly and never calls it.
//   - DeliveryID derives one message id per ticket for all time and inserts it ON
//     CONFLICT DO NOTHING, so reusing it would hand the transport a message id it has
//     already seen — and a transport that deduplicates on message id would drop the
//     resend as a replay, which is the failure being fixed.
//   - MarkDelivered returns early without appending when a `delivered` event exists.
//
// So: its own event type, its own attempt table, its own message ids.
//
// WHY THERE IS NO COMPLETION CHECK AGAINST COMMERCE. Access's tickets exist only as a
// consequence of consuming order.completed — store.Issue has one non-test caller
// (consumer.issue), which has one call site (processCompleted). The exchange path
// moves tickets rather than issuing them. So "this order has ticket rows here" IS
// access's own evidence that the order completed, held locally and needing no
// credential. Commerce's staff order read would answer the same question behind
// COMMERCE_STAFF_WRITE_TOKEN, which access does not hold and should not be given for
// this (ADR-068 § the grant that was not made).
//
// THAT IS AN INVARIANT THIS FEATURE DEPENDS ON, not an incidental fact: a future path
// that issues tickets for a non-completed order silently makes this wrong.

// RedeliveryBound is the number of DISTINCT redelivery requests one order may make in
// RedeliveryWindow. A replay of a key already used is exempt — retrying an ambiguous
// request must stay possible, or an operator who lost the response is stuck.
//
// Five, not one: the support call this feature serves is "I did not get it", and the
// realistic interaction is resend, "still nothing", check the address, resend once
// more — inside one phone call. A bound of one refuses the second attempt and offers
// the operator nothing but "wait a day".
//
// NAME WHAT THIS DOES NOT BOUND (shared/go/ratelimit's rule, applied to a durable
// counter). It bounds distinct requests per ORDER. It does NOT bound: redeliveries
// across different orders; a caller who waits out the window; a holder of a stolen
// staff credential doing either; or anyone with write access to this database. It is
// a blast-radius bound on capability re-emission per order, not an anti-abuse control.
const (
	RedeliveryBound  = 5
	RedeliveryWindow = 24 * time.Hour
)

// ErrRedeliveryKeyConflict reports the same idempotency key presented against a
// different order. The key binds to exactly one order, checked before any send.
var ErrRedeliveryKeyConflict = errors.New("idempotency key reused with a different order")

// ErrRedeliveryBoundExceeded reports that this order has already made
// RedeliveryBound distinct requests inside RedeliveryWindow.
var ErrRedeliveryBoundExceeded = errors.New("order has reached its redelivery bound")

// RedeliveryTicket is one ticket to resend, with the message id minted for THIS
// request. BuyerID and GuestOrderRef are here because the caller must resolve the
// address and build the capability link; neither is ever persisted by this package
// beyond the rows issuance already wrote.
type RedeliveryTicket struct {
	TicketID      uuid.UUID
	BuyerID       uuid.UUID
	GuestOrderRef uuid.UUID
	MessageID     uuid.UUID
	// Accepted is true once the transport took this ticket's message AND the trail
	// recorded it — the two happen in MarkRedelivered under one transaction, so a
	// row is accepted or it is outstanding, never half.
	//
	// It exists because a claim COMMITS before the first send: a request that fails
	// partway leaves attempt rows with nothing sent against them, and a replay that
	// did not distinguish those would report the whole order delivered when some of
	// it never left (ai-review F2).
	Accepted bool
}

// RedeliveryClaim is one claimed redelivery request.
type RedeliveryClaim struct {
	OrderID uuid.UUID
	Tickets []RedeliveryTicket
	// Replay is true when this key had already claimed this order. The caller must
	// still send every ticket whose Accepted is false: a replay is a RESUME, not a
	// no-op, because the claim commits before any send and a request that died
	// partway left outstanding rows behind (ai-review F2).
	//
	// Replay therefore says "this key is not new", not "there is nothing to do".
	Replay bool
}

// Outstanding returns the tickets this claim still owes the transport.
//
// A claim with none outstanding is genuinely complete; one with some is a partial
// send being resumed. The caller re-sends under the SAME derived message ids, so a
// transport that deduplicates on message id will not deliver twice.
func (c RedeliveryClaim) Outstanding() []RedeliveryTicket {
	var out []RedeliveryTicket
	for _, t := range c.Tickets {
		if !t.Accepted {
			out = append(out, t)
		}
	}
	return out
}

// redeliveryEventID derives the lifecycle event id for one (request, ticket) pair.
// It is what makes a resend's own events identifiable among the many a ticket may
// accumulate, and what makes a retry of the same request idempotent without an index
// — `redelivered` is repeatable by design (ADR-025 §D3).
func redeliveryEventID(organizer uuid.UUID, key string, ticketID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("redelivered:"+organizer.String()+":"+key+":"+ticketID.String()))
}

// redeliveryMessageID derives the transport message id for one (request, ticket).
// Derived rather than random so a caller that crashes between claiming and sending
// resumes with the SAME id and the transport can deduplicate; distinct from
// delivery_attempts' ticket+":delivery" so it can never collide with the original.
func redeliveryMessageID(organizer uuid.UUID, key string, ticketID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("redelivery:"+organizer.String()+":"+key+":"+ticketID.String()))
}

// ClaimRedelivery binds an idempotency key to an order, enforces the per-order bound
// and returns the tickets to resend with a fresh message id each.
//
// It does NOT append lifecycle events: the send has not happened yet, and a trail
// that recorded a delivery before the transport accepted it would claim something
// untrue. MarkRedelivered closes that loop afterwards, per ticket.
func (p *Postgres) ClaimRedelivery(ctx context.Context, org, order uuid.UUID, key string) (RedeliveryClaim, error) {
	if key == "" {
		return RedeliveryClaim{}, errors.New("idempotency key is required")
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return RedeliveryClaim{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// Lock every ticket of the order in id order before deciding anything: two
	// concurrent requests for one order would otherwise both pass the bound check.
	// Locking in a total order means they queue rather than deadlock — the same
	// reasoning RefundOrderTickets records.
	rows, err := tx.QueryContext(ctx, `
		SELECT id,buyer_id,guest_order_ref FROM tickets
		WHERE order_id=$1 AND organizer_id=$2 ORDER BY id FOR UPDATE`, order, org)
	if err != nil {
		return RedeliveryClaim{}, err
	}
	var all []RedeliveryTicket
	for rows.Next() {
		var t RedeliveryTicket
		if err := rows.Scan(&t.TicketID, &t.BuyerID, &t.GuestOrderRef); err != nil {
			_ = rows.Close()
			return RedeliveryClaim{}, err
		}
		all = append(all, t)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return RedeliveryClaim{}, err
	}
	_ = rows.Close()

	// No tickets is "not yet", not "no such order". Issuance is asynchronous, so a
	// resend can outrun it exactly as a refund can — and access cannot tell a
	// never-completed order from one whose issuance event is still in flight. The
	// caller must retry rather than read this as "there is nothing to resend"
	// (the semantics ErrTicketsNotIssued already carries on the refund path).
	if len(all) == 0 {
		return RedeliveryClaim{}, fmt.Errorf("%w: order has no issued tickets", ErrTicketsNotIssued)
	}

	// The key binds to one order, checked BEFORE the bound: a replay must not be
	// refused by a quota it already passed once.
	var boundOrder uuid.UUID
	err = tx.QueryRowContext(ctx, `SELECT order_id FROM redelivery_requests WHERE organizer_id=$1 AND idempotency_key=$2`, org, key).
		Scan(&boundOrder)
	switch {
	case err == nil:
		if boundOrder != order {
			return RedeliveryClaim{}, ErrRedeliveryKeyConflict
		}
		claimed, err := loadRedeliveryAttempts(ctx, tx, org, key, all)
		if err != nil {
			return RedeliveryClaim{}, err
		}
		return RedeliveryClaim{OrderID: order, Tickets: claimed, Replay: true}, tx.Commit()
	case !errors.Is(err, sql.ErrNoRows):
		return RedeliveryClaim{}, err
	}

	var recent int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM redelivery_requests WHERE order_id=$1 AND requested_at > $2`,
		order, p.now().Add(-RedeliveryWindow)).Scan(&recent); err != nil {
		return RedeliveryClaim{}, err
	}
	if recent >= RedeliveryBound {
		return RedeliveryClaim{}, fmt.Errorf("%w: %d in the last %s", ErrRedeliveryBoundExceeded, recent, RedeliveryWindow)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO redelivery_requests(organizer_id,idempotency_key,order_id,ticket_count) VALUES($1,$2,$3,$4)`,
		org, key, order, len(all)); err != nil {
		// Two requests reusing one key against DIFFERENT orders lock disjoint ticket
		// rows, so nothing above serializes them: both see no binding and race here.
		// The loser surfaces 23505 once the winner commits. Returning it raw would
		// answer 500 for a condition the API declares a 409 for — the shape
		// RefundOrderTickets was corrected into on its second review pass.
		if isUniqueViolation(err) {
			return RedeliveryClaim{}, ErrRedeliveryKeyConflict
		}
		return RedeliveryClaim{}, err
	}

	for i := range all {
		all[i].MessageID = redeliveryMessageID(org, key, all[i].TicketID)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO redelivery_attempts(organizer_id,idempotency_key,ticket_id,message_id) VALUES($1,$2,$3,$4)`,
			org, key, all[i].TicketID, all[i].MessageID); err != nil {
			return RedeliveryClaim{}, fmt.Errorf("claim redelivery of ticket %s: %w", all[i].TicketID, err)
		}
	}
	return RedeliveryClaim{OrderID: order, Tickets: all}, tx.Commit()
}

// loadRedeliveryAttempts reads back the message ids a previous claim minted, in the
// same ticket order the caller was given the first time. A replay must hand back the
// ORIGINAL ids: re-deriving them would be right today and silently wrong the moment
// the derivation changes, and the stored row is the record of what was actually sent.
func loadRedeliveryAttempts(ctx context.Context, tx *sql.Tx, org uuid.UUID, key string, all []RedeliveryTicket) ([]RedeliveryTicket, error) {
	out := make([]RedeliveryTicket, 0, len(all))
	for _, t := range all {
		var msg uuid.UUID
		var accepted sql.NullTime
		err := tx.QueryRowContext(ctx, `SELECT message_id, accepted_at FROM redelivery_attempts WHERE organizer_id=$1 AND idempotency_key=$2 AND ticket_id=$3`,
			org, key, t.TicketID).Scan(&msg, &accepted)
		if errors.Is(err, sql.ErrNoRows) {
			// The request row exists without this ticket's attempt row. Both are
			// written in one transaction, so this means the order gained a ticket
			// after the claim — not a state this service can produce today, and not
			// one to paper over by sending to it.
			continue
		}
		if err != nil {
			return nil, err
		}
		t.MessageID = msg
		// accepted_at IS NULL means the transport never took this one, or took it and
		// the process died before the trail recorded it. Either way it is outstanding
		// and the caller must send it again under this same message id.
		t.Accepted = accepted.Valid
		out = append(out, t)
	}
	return out, nil
}

// MarkRedelivered records that the transport accepted one ticket's resend.
//
// Separate from the claim, and after the send, so the trail records deliveries that
// were actually handed over rather than ones that were merely intended. Appending
// before the send would make the trail claim something untrue; ADR-021's rule is that
// the claim must match what happened.
//
// Idempotency is settled HERE, before appendLifecycle, never inside it — the append
// path's own contract (lifecycle.go). A repeat commits without appending.
func (p *Postgres) MarkRedelivered(ctx context.Context, org uuid.UUID, key string, ticketID, messageID uuid.UUID) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// The same ticket row Redeem and MarkDelivered lock: the chain serializes per
	// ticket (ADR-021 §D1).
	var id TicketIdentity
	err = tx.QueryRowContext(ctx, `SELECT order_id,organizer_id,slot_id FROM tickets WHERE id=$1 FOR UPDATE`, ticketID).
		Scan(&id.OrderID, &id.OrganizerID, &id.SlotID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTicketCredential
		}
		return err
	}
	if id.OrganizerID != org {
		return ErrTicketCredential
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE redelivery_attempts SET accepted_at=now()
		WHERE organizer_id=$1 AND idempotency_key=$2 AND ticket_id=$3 AND message_id=$4 AND accepted_at IS NULL`,
		org, key, ticketID, messageID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		// Already accepted, or no such claim. Either way this send is not the one
		// that gets to append: a second append under the same derived event id would
		// collide on the integrity row's primary key and turn a harmless repeat into
		// a hard error.
		return tx.Commit()
	}

	eventID := redeliveryEventID(org, key, ticketID)
	if _, err = p.appendLifecycle(ctx, tx, appendInput{
		TicketID: ticketID, OrderID: id.OrderID, OrganizerID: id.OrganizerID, SlotID: id.SlotID,
		EventID: eventID, Type: "redelivered",
	}); err != nil {
		return err
	}
	return tx.Commit()
}
