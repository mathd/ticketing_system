package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"

	"github.com/google/uuid"
)

// Comped-order voids (TKT-171). A void is the reversal of an order that has no
// money leg: it voids the tickets and returns the capacity, and it never moves
// money.
//
// The invariant, in one sentence: A VOID MOVES TICKETS AND CAPACITY AND NEVER
// MONEY. Everything below exists to make that structurally true rather than
// conventionally true — there is no unit_amount, no currency and no payment fact
// anywhere in this file, and migration 0025 omits the columns that could record
// one.

// ErrOrderNotVoidable reports an order this path must not reverse.
//
// Deliberately one error for several predicates, matching ErrOrderNotRefundable:
// an unauthorized caller probing which orders exist and in what state learns
// nothing from the distinction, and the tests assert each predicate separately
// rather than relying on the error to tell them apart.
var ErrOrderNotVoidable = errors.New("order cannot be voided")

// ErrVoidHasMoney reports an order with a money leg, which must go through the
// refund path instead.
//
// Separate from ErrOrderNotVoidable because it is the one refusal that is a
// routing answer rather than a rejection: it tells the caller the order is
// reversible, just not here. It is the exact mirror of ErrRefundNoMoney, which
// tells a comped order the same thing in the other direction.
var ErrVoidHasMoney = errors.New("order has captured money and must be refunded, not voided")

// OrderVoid is one comped-order reversal and its two independent obligations.
//
// Note what is NOT here: no Amount, no UnitAmount, no Currency, no
// PaymentFactID. A caller cannot accidentally report money from a void because
// there is no field to read it from.
type OrderVoid struct {
	ID          uuid.UUID
	OrderID     uuid.UUID
	OrganizerID uuid.UUID
	Quantity    int32
	Actor       string
	Reason      string
	// TicketsVoided and CapacityReturned are independent in STORAGE and ordered in
	// EXECUTION (ADR-038 §1) — the same split order_refunds carries. A void whose
	// tickets are void but whose seat has not come back is a real, visible state.
	TicketsVoided    bool
	CapacityReturned bool
	// HoldID is the inventory claim capacity is returned against.
	HoldID uuid.UUID
}

// VoidRequest is one instruction to reverse a comped order.
//
// There is deliberately no Quantity: a comped reversal is whole-order, and the
// quantity is read from the reservation under the order lock. A client that
// cannot state a quantity cannot forge one (AGENTS.md — make it unsubmittable,
// not validated).
type VoidRequest struct {
	OrderID, OrganizerID uuid.UUID
	IdempotencyKey       string
	Actor, Reason        string
}

// VoidID derives a void's identity from the ORDER, not from the request key.
//
// This is the load-bearing decision of TKT-171. Both downstream legs are keyed on
// the id this returns (they call the field `refund_id`, which is their
// idempotency/correlation field and not an assertion that money moved), so the id
// has to be the same one on every attempt at reversing the same order. A staff
// retry, the cancellation runner, and a process restart each arrive with a
// DIFFERENT request key; deriving from the key would give each of them its own
// downstream operation and reverse the order more than once.
//
// The namespace string is distinct from RefundID's "refund:" and from access's
// own "refunded:" ticket-event derivation, so a void id can never collide with a
// refund id for the same order and be replayed as one.
func VoidID(org, orderID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("order-void:"+org.String()+":"+orderID.String()))
}

func voidFingerprint(in VoidRequest) string {
	sum := sha256.Sum256([]byte(in.OrderID.String() + "\x00" + in.Actor + "\x00" + in.Reason))
	return hex.EncodeToString(sum[:])
}

// BindOrderVoid records the intent to reverse a comped order, idempotently.
//
// Takes the order row lock and re-derives eligibility under it, exactly as
// BindOrderRefund does — the lock is what makes the exchange check meaningful
// rather than advisory.
//
// A replay resolves BEFORE eligibility is re-derived: a void that already
// discharged one of its obligations must answer as itself rather than be
// re-judged, since its own progress is not a reason to refuse it.
func BindOrderVoid(ctx context.Context, db *sql.DB, in VoidRequest) (OrderVoid, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return OrderVoid{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		status  string
		hold    uuid.UUID
		soldQty int32
		unit    int64
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT o.status, r.hold_id, r.quantity, r.unit_amount
		FROM orders o JOIN reservations r ON r.id = o.reservation_id
		WHERE o.id=$1 AND r.organizer_id=$2 FOR UPDATE OF o`, in.OrderID, in.OrganizerID).
		Scan(&status, &hold, &soldQty, &unit); err != nil {
		return OrderVoid{}, err
	}

	id := VoidID(in.OrganizerID, in.OrderID)
	fingerprint := voidFingerprint(in)
	existing, found, err := lookupVoid(ctx, tx, in.OrganizerID, id)
	if err != nil {
		return OrderVoid{}, err
	}
	if found {
		// A reused identity with a different actor/reason is a conflict, not a
		// silent replay of somebody else's void — same rule as refundFingerprint.
		if existing.fingerprint != fingerprint {
			return OrderVoid{}, ErrRefundConflict
		}
		out := existing.void
		out.OrganizerID, out.HoldID = in.OrganizerID, hold
		return out, tx.Commit()
	}

	if status != "completed" {
		return OrderVoid{}, ErrOrderNotVoidable
	}
	// An order an exchange already owns cannot also be voided — it would be
	// reversed twice. Mirrors BindOrderRefund's identical refusal (TKT-158); both
	// paths take the order row lock above, so this cannot race an exchange being
	// bound.
	var exchanges int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM order_exchanges WHERE source_order_id=$1`, in.OrderID).Scan(&exchanges); err != nil {
		return OrderVoid{}, err
	}
	if exchanges > 0 {
		return OrderVoid{}, ErrOrderNotVoidable
	}
	// The predicate that makes this path a void rather than a refund. Strictly
	// zero, not `<= 0`: a NEGATIVE unit amount is not a comped order, it is
	// corrupt data, and silently reversing it would hide the corruption behind a
	// successful-looking void.
	if unit != 0 {
		return OrderVoid{}, ErrVoidHasMoney
	}
	// Whole-order, from the reservation. Never from the request.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO order_voids(organizer_id,id,order_id,request_fingerprint,quantity,actor,reason)
		VALUES($1,$2,$3,$4,$5,$6,$7)`,
		in.OrganizerID, id, in.OrderID, fingerprint, soldQty, in.Actor, in.Reason); err != nil {
		return OrderVoid{}, err
	}
	bound, found, err := lookupVoid(ctx, tx, in.OrganizerID, id)
	if err != nil {
		return OrderVoid{}, err
	}
	if !found {
		return OrderVoid{}, errors.New("void row missing after bind")
	}
	out := bound.void
	out.OrganizerID, out.HoldID = in.OrganizerID, hold
	return out, tx.Commit()
}

type storedVoid struct {
	void        OrderVoid
	fingerprint string
}

func lookupVoid(ctx context.Context, q rowQuerier, org, id uuid.UUID) (storedVoid, bool, error) {
	var (
		s        storedVoid
		voided   sql.NullTime
		returned sql.NullTime
	)
	err := q.QueryRowContext(ctx, `
		SELECT id, order_id, quantity, actor, reason, request_fingerprint, tickets_voided_at, capacity_returned_at
		FROM order_voids WHERE organizer_id=$1 AND id=$2`, org, id).
		Scan(&s.void.ID, &s.void.OrderID, &s.void.Quantity, &s.void.Actor, &s.void.Reason,
			&s.fingerprint, &voided, &returned)
	if errors.Is(err, sql.ErrNoRows) {
		return storedVoid{}, false, nil
	}
	if err != nil {
		return storedVoid{}, false, err
	}
	s.void.OrganizerID = org
	s.void.TicketsVoided = voided.Valid
	s.void.CapacityReturned = returned.Valid
	return s, true, nil
}

// LookupOrderVoid reads a void by order, for the cancellation runner's resume path.
func LookupOrderVoid(ctx context.Context, db *sql.DB, org, orderID uuid.UUID) (OrderVoid, bool, error) {
	s, found, err := lookupVoid(ctx, db, org, VoidID(org, orderID))
	return s.void, found, err
}

// MarkVoidTicketsVoided records that the first obligation is discharged.
//
// Guarded on the column still being NULL: the timestamp is evidence of WHEN the
// obligation was discharged, and a retry must not rewrite it.
func MarkVoidTicketsVoided(ctx context.Context, db *sql.DB, org, voidID uuid.UUID) error {
	_, err := db.ExecContext(ctx, `
		UPDATE order_voids SET tickets_voided_at=now()
		WHERE organizer_id=$1 AND id=$2 AND tickets_voided_at IS NULL`, org, voidID)
	return err
}

// MarkVoidCapacityReturned records the second obligation.
//
// Guarded on `tickets_voided_at IS NOT NULL` as well as on its own column, so the
// ADR-038 §1 ordering is enforced by the WRITE and not only by the driver's
// control flow. Migration 0025 carries the same rule as a CHECK constraint; both
// are deliberate, because the sequence they protect against is the one that
// oversells and a guard that lives in exactly one place is a guard one refactor
// away from not existing.
func MarkVoidCapacityReturned(ctx context.Context, db *sql.DB, org, voidID uuid.UUID) error {
	_, err := db.ExecContext(ctx, `
		UPDATE order_voids SET capacity_returned_at=now()
		WHERE organizer_id=$1 AND id=$2 AND capacity_returned_at IS NULL AND tickets_voided_at IS NOT NULL`, org, voidID)
	return err
}
