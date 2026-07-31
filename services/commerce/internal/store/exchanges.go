package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// isUniqueViolation reports PostgreSQL unique_violation (23505) without the caller having
// to know the driver's error type.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// Exchanges (TKT-158, ADR-039). An exchange is a reversal AND a sale.
//
// It is deliberately NOT a refund plus a checkout. Composing those merged primitives —
// which are right there, which is what makes it tempting — would refund the old gross
// amount and capture the new gross amount: two provider movements, and the wrong
// cash-flow story. An exchange makes exactly ONE net movement for the difference. What
// the provider does and what the trail records are different questions, and both gross
// legs are still journalled.
//
// This slice stops at `switch_pending`: the delta is settled and the replacement is
// confirmed, while the buyer still holds VALID OLD TICKETS. Switching the entitlement is
// TKT-166. That state under-sells, cannot oversell, and never leaves the buyer with
// nothing — which is why it is a safe place to stop.

var (
	// ErrOrderNotExchangeable reports an order that cannot be exchanged: its checkout did
	// not end in `completed`, or it has already been reversed — by a refund or by another
	// exchange. An order reversed twice is the failure this guards.
	ErrOrderNotExchangeable = errors.New("only a completed, unreversed order can be exchanged")
	// ErrExchangeConflict reports the same idempotency key used with a different request.
	ErrExchangeConflict = errors.New("exchange idempotency key reused with a different request")
	// ErrExchangeCurrencyMismatch reports a target priced in another currency. There is no
	// FX inside an order (PRD; TKT-10 owns multi-currency), so this is a refusal rather
	// than a conversion.
	ErrExchangeCurrencyMismatch = errors.New("exchange target currency differs from the source order")
)

// ExchangeRequest is one staff exchange instruction.
type ExchangeRequest struct {
	SourceOrderID, OrganizerID uuid.UUID
	TargetTicketTypeID         uuid.UUID
	IdempotencyKey             string
	Actor, Reason              string
}

// Exchange is a durable exchange, settled or not.
type Exchange struct {
	ID, OrganizerID    uuid.UUID
	SourceOrderID      uuid.UUID
	ReplacementOrderID uuid.UUID
	TargetTicketTypeID uuid.UUID
	SourceReservation  uuid.UUID
	HoldID, BuyerID    uuid.UUID
	SlotID             uuid.UUID
	Quantity           int32
	SourceTotal        int64
	TargetTotal        int64
	DeltaAmount        int64
	Currency           string
	// Settled is the money half: the delta moved (or was zero) and the replacement order
	// exists. TicketsExchanged is TKT-166's half. Settled && !TicketsExchanged is
	// `switch_pending` — the state this slice deliberately ends in.
	Settled, TicketsExchanged bool
	// PaymentSourceKey is the source order's checkout idempotency key, which IS the key
	// payments bound its charge operation under. A downgrade refunds against it.
	PaymentSourceKey string
	// CreatedAt is the row's stable creation time. Every compensating fact's occurred_at
	// comes from here, never the clock — the journal compares the whole canonical fact on
	// replay, so a retry must rebuild byte-identical content.
	CreatedAt time.Time
}

// ExchangeID derives an exchange's identity from its organizer and idempotency key. Every
// downstream key — the charge, the refund leg, both facts, the lifecycle events, the
// inventory receipt — derives from it, so a retry that lost its response re-derives all of
// them rather than settling the difference a second time.
func ExchangeID(org uuid.UUID, idempotencyKey string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("exchange:"+org.String()+":"+idempotencyKey))
}

// ExchangeReversedFactID and ExchangeSoldFactID name the two GROSS journal facts. Both are
// written whichever way the money moved: the trail records that a line worth X was
// reversed and a line worth Y was sold, even when the provider only moved Y−X.
func ExchangeReversedFactID(id uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(id.String()+":order.exchange.reversed"))
}
func ExchangeSoldFactID(id uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(id.String()+":order.exchange.sold"))
}

// ExchangeDelta is the signed difference the provider settles: positive is an upgrade the
// buyer pays, negative a downgrade refunded to them, zero settles nothing. Both inputs are
// persisted integer minor units — the source total comes from the reservation, never from
// a re-read of a mutable catalog row.
func ExchangeDelta(sourceTotal, targetTotal int64) int64 { return targetTotal - sourceTotal }

// ValidateExchangeTarget refuses a target the exchange cannot settle against. Currency is
// the one that matters: there is no FX inside an order.
func ValidateExchangeTarget(ex Exchange, targetTotal int64, currency string) error {
	if currency != ex.Currency {
		return ErrExchangeCurrencyMismatch
	}
	if targetTotal < 0 {
		return errors.New("exchange target total must not be negative")
	}
	return nil
}

// BindOrderExchange inserts-or-loads the one exchange for (organizer, idempotency key)
// under the source order's row lock.
//
// The lock is the same one BindOrderRefund takes, and that is deliberate: an order must
// not be reversed twice, and a refund racing an exchange is exactly how it would be. The
// two paths contend on the order row rather than on a read either could lose.
func BindOrderExchange(ctx context.Context, db *sql.DB, in ExchangeRequest) (Exchange, error) {
	if in.TargetTicketTypeID == uuid.Nil {
		return Exchange{}, errors.New("exchange needs a target ticket type")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Exchange{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var status, chargeKey, currency string
	var reservation, buyer, hold, slot uuid.UUID
	var quantity int32
	var total int64
	err = tx.QueryRowContext(ctx, `
		SELECT o.status, o.idempotency_key, r.id, r.buyer_id, r.hold_id, r.slot_id, r.quantity, r.total_amount, r.currency
		FROM orders o JOIN reservations r ON r.id = o.reservation_id
		WHERE o.id=$1 AND r.organizer_id=$2 FOR UPDATE OF o`, in.SourceOrderID, in.OrganizerID).
		Scan(&status, &chargeKey, &reservation, &buyer, &hold, &slot, &quantity, &total, &currency)
	if err != nil {
		return Exchange{}, err
	}

	id := ExchangeID(in.OrganizerID, in.IdempotencyKey)
	fingerprint := exchangeFingerprint(in)
	// Replay before eligibility, as every other path in this epic does: an exchange that
	// already progressed moved the state it would now be judged against.
	existing, found, err := lookupExchange(ctx, tx, in.OrganizerID, id)
	if err != nil {
		return Exchange{}, err
	}
	if found {
		if existing.fingerprint != fingerprint {
			return Exchange{}, ErrExchangeConflict
		}
		out := existing.exchange
		out.SourceReservation, out.BuyerID, out.HoldID, out.SlotID, out.PaymentSourceKey = reservation, buyer, hold, slot, chargeKey
		return out, tx.Commit()
	}

	if status != "completed" {
		return Exchange{}, ErrOrderNotExchangeable
	}
	// Already reversed by a refund? Then it cannot also be exchanged.
	var refunds int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM order_refunds WHERE order_id=$1`, in.SourceOrderID).Scan(&refunds); err != nil {
		return Exchange{}, err
	}
	if refunds > 0 {
		return Exchange{}, ErrOrderNotExchangeable
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO order_exchanges(organizer_id,id,source_order_id,target_ticket_type_id,idempotency_key,request_fingerprint,quantity,source_total,currency,actor,reason)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		in.OrganizerID, id, in.SourceOrderID, in.TargetTicketTypeID, in.IdempotencyKey, fingerprint,
		quantity, total, currency, in.Actor, in.Reason); err != nil {
		if isUniqueViolation(err) {
			// The one-per-source index: another exchange already owns this order.
			return Exchange{}, ErrOrderNotExchangeable
		}
		return Exchange{}, err
	}
	bound, found, err := lookupExchange(ctx, tx, in.OrganizerID, id)
	if err != nil {
		return Exchange{}, err
	}
	if !found {
		return Exchange{}, errors.New("exchange row missing after bind")
	}
	out := bound.exchange
	out.SourceReservation, out.BuyerID, out.HoldID, out.SlotID, out.PaymentSourceKey = reservation, buyer, hold, slot, chargeKey
	return out, tx.Commit()
}

func exchangeFingerprint(in ExchangeRequest) string {
	sum := sha256.Sum256([]byte(in.SourceOrderID.String() + "\x00" + in.TargetTicketTypeID.String() + "\x00" + in.Actor + "\x00" + in.Reason))
	return hex.EncodeToString(sum[:])
}

type storedExchange struct {
	exchange    Exchange
	fingerprint string
}

func lookupExchange(ctx context.Context, q rowQuerier, org, id uuid.UUID) (storedExchange, bool, error) {
	var s storedExchange
	var replacement uuid.NullUUID
	var target, delta sql.NullInt64
	var settled, switched sql.NullTime
	var createdAt time.Time
	err := q.QueryRowContext(ctx, `
		SELECT id,source_order_id,replacement_order_id,target_ticket_type_id,request_fingerprint,quantity,
		       source_total,target_total,delta_amount,currency,created_at,settled_at,tickets_exchanged_at
		FROM order_exchanges WHERE organizer_id=$1 AND id=$2`, org, id).
		Scan(&s.exchange.ID, &s.exchange.SourceOrderID, &replacement, &s.exchange.TargetTicketTypeID,
			&s.fingerprint, &s.exchange.Quantity, &s.exchange.SourceTotal, &target, &delta,
			&s.exchange.Currency, &createdAt, &settled, &switched)
	if errors.Is(err, sql.ErrNoRows) {
		return storedExchange{}, false, nil
	}
	if err != nil {
		return storedExchange{}, false, err
	}
	s.exchange.OrganizerID = org
	s.exchange.ReplacementOrderID = replacement.UUID
	s.exchange.TargetTotal, s.exchange.DeltaAmount = target.Int64, delta.Int64
	s.exchange.Settled, s.exchange.TicketsExchanged = settled.Valid, switched.Valid
	s.exchange.CreatedAt = createdAt.UTC().Truncate(time.Microsecond)
	return s, true, nil
}

// CompleteExchangeSettlement records the money half: the replacement order, both totals,
// the signed delta, and the instant it settled. Guarded on `settled_at IS NULL`, so a
// replay keeps the original result — the timestamp is evidence of when the difference
// moved, and a retry must not rewrite it.
func CompleteExchangeSettlement(ctx context.Context, db *sql.DB, org, exchangeID, replacementOrder uuid.UUID, targetTotal, delta int64) error {
	_, err := db.ExecContext(ctx, `
		UPDATE order_exchanges
		SET replacement_order_id=$3, target_total=$4, delta_amount=$5, settled_at=now()
		WHERE organizer_id=$1 AND id=$2 AND settled_at IS NULL`,
		org, exchangeID, replacementOrder, targetTotal, delta)
	return err
}

// MarkExchangeTicketsSwitched is TKT-166's half, declared here so the shape of the whole
// operation is visible in one place: the reversal is complete when both timestamps are
// set, and the constraint refuses a switch that precedes settlement.
func MarkExchangeTicketsSwitched(ctx context.Context, db *sql.DB, org, exchangeID uuid.UUID) error {
	_, err := db.ExecContext(ctx, `
		UPDATE order_exchanges SET tickets_exchanged_at=now()
		WHERE organizer_id=$1 AND id=$2 AND tickets_exchanged_at IS NULL AND settled_at IS NOT NULL`,
		org, exchangeID)
	return err
}
