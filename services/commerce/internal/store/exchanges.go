package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"ticketing/services/commerce/internal/events"
)

// isUniqueViolation reports PostgreSQL unique_violation (23505) without the caller having
// to know the driver's error type.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// violatedConstraint names the constraint a unique violation broke, or "" for any other
// error. WHICH index fired is load-bearing here — two of them mean opposite things about
// who owns the target hold (see BindOrderExchange's INSERT).
func violatedConstraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName
	}
	return ""
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
	// SourceGrossTotal is what the buyer was actually CHARGED for the source
	// order: face plus passed-on fees. SourceTotal is the face value alone.
	//
	// Both are needed and they are not interchangeable: the delta is a
	// price-to-price comparison and must use face (0010's CHECK ties
	// delta_amount to source_total), while the `order.exchange.reversed` money
	// fact must reverse the gross or the payments journal stops agreeing with
	// the original charge.
	SourceGrossTotal int64
	TargetTotal        int64
	DeltaAmount        int64
	Currency           string
	// Settled is the money half: the delta moved (or was zero) and the replacement order
	// exists. TicketsExchanged is TKT-166's half. Settled && !TicketsExchanged is
	// `switch_pending` — the state TKT-158 deliberately ended in.
	Settled, TicketsExchanged bool
	// CapacityReturned is the THIRD fact, and it is projected because it is reportable
	// (ai-review pass 3). Migration 0011 added the column precisely to make "switched, old
	// capacity still outstanding" visible; leaving it out of the projection and calling the
	// exchange `completed` from TicketsExchanged alone hid the exact substate the column
	// was introduced to expose, and left an under-selling exchange findable only by hand.
	CapacityReturned bool
	// BasisRecorded is the pre-money commitment (ai-review F3): target hold, replacement
	// reservation, target total and signed delta, all persisted BEFORE the provider is
	// called. A retry settles against these, never against a re-resolved price or a
	// re-taken hold — re-deriving on replay can produce a different basis, or fail on an
	// expired claim, leaving a charged buyer with an unsettled exchange.
	BasisRecorded            bool
	TargetHoldID             uuid.UUID
	ReplacementReservationID uuid.UUID
	TargetUnitAmount         int64
	TargetSlotID             uuid.UUID
	TargetPriceSnapshot      []byte
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
// ExchangeDelta compares FACE VALUES, and both sides must be face for the
// subtraction to mean anything (TKT-215).
//
// targetTotal is resolution.total(quantity) -- a rule-resolved unit price times
// quantity, with no fee in it. So sourceTotal is read from
// reservations.face_value_amount rather than total_amount: once TKT-215 made
// total_amount the GROSS charge (face + passed-on fees), comparing it against a
// price-only target made an EVEN exchange produce a negative delta and refund
// the buyer their service fee. Face-to-face keeps the subtraction honest.
//
// What an exchange does about the fees themselves -- re-resolve them for the new
// ticket, carry them over, or refund them -- is deliberately NOT decided here.
// It is a product question carved out of TKT-6 with its own ticket.
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

// ExchangeSource is the source line, read WITHOUT binding anything.
type ExchangeSource struct {
	ReservationID, HoldID, BuyerID, SlotID uuid.UUID
	Quantity                               int32
	// Total is the FACE value; GrossTotal is what the buyer was charged. See
	// Exchange.SourceGrossTotal for why both exist.
	Total                      int64
	GrossTotal                 int64
	Currency, PaymentSourceKey string
	// ChannelCode is the channel the SOURCE sale named; nil is the
	// default/public context. Carried so an exchange reprices the target on the
	// same channel it was bought on (TKT-237): repricing on the public channel
	// would silently change what the buyer owes on the difference, and the
	// change would look like a pricing bug rather than a lost field.
	ChannelCode *string
	// ResellerID is WHO sold the source, nil for a public sale (TKT-246).
	//
	// Read for two distinct purposes, and both were defects before this ticket:
	// the target hold must present it so a channelled exchange consumes the SAME
	// allocation the source came from rather than public stock, and the replacement
	// must inherit it or exchanging a reseller-attributed order silently produces an
	// unattributed one — irreversibly, since nothing else records who sold it.
	ResellerID *uuid.UUID
}

// LoadExchangeSource reads the source order's line for eligibility checks.
//
// It exists because binding first was wrong (ai-review F2): a durable row inserted before
// the seated, currency and availability checks means any refusal — a typo, a sold-out
// target — leaves a row that the one-per-order index and the refund exclusion then treat
// as a live exchange, making a completed order permanently unreversible with no money
// having moved. Nothing durable is written until the request is known to be servable.
func LoadExchangeSource(ctx context.Context, db *sql.DB, org, order uuid.UUID) (ExchangeSource, error) {
	var out ExchangeSource
	var status string
	err := db.QueryRowContext(ctx, `
		SELECT o.status, o.idempotency_key, r.id, r.hold_id, r.buyer_id, r.slot_id, r.quantity, r.face_value_amount, r.total_amount, r.currency, r.channel_code, r.reseller_id
		FROM orders o JOIN reservations r ON r.id = o.reservation_id
		WHERE o.id=$1 AND r.organizer_id=$2`, order, org).
		Scan(&status, &out.PaymentSourceKey, &out.ReservationID, &out.HoldID, &out.BuyerID, &out.SlotID,
			&out.Quantity, &out.Total, &out.GrossTotal, &out.Currency, &out.ChannelCode, &out.ResellerID)
	if err != nil {
		return ExchangeSource{}, err
	}
	if status != "completed" {
		return ExchangeSource{}, ErrOrderNotExchangeable
	}
	return out, nil
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
	var total, gross int64
	err = tx.QueryRowContext(ctx, `
		SELECT o.status, o.idempotency_key, r.id, r.buyer_id, r.hold_id, r.slot_id, r.quantity, r.face_value_amount, r.total_amount, r.currency
		FROM orders o JOIN reservations r ON r.id = o.reservation_id
		WHERE o.id=$1 AND r.organizer_id=$2 FOR UPDATE OF o`, in.SourceOrderID, in.OrganizerID).
		Scan(&status, &chargeKey, &reservation, &buyer, &hold, &slot, &quantity, &total, &gross, &currency)
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
		INSERT INTO order_exchanges(organizer_id,id,source_order_id,target_ticket_type_id,idempotency_key,request_fingerprint,quantity,source_total,source_gross_total,currency,actor,reason)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		in.OrganizerID, id, in.SourceOrderID, in.TargetTicketTypeID, in.IdempotencyKey, fingerprint,
		quantity, total, gross, currency, in.Actor, in.Reason); err != nil {
		return Exchange{}, mapBindInsertError(err)
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
	var settled, switched, returned, basis sql.NullTime
	var targetHold, replacementReservation, targetSlot uuid.NullUUID
	var targetUnit sql.NullInt64
	var createdAt time.Time
	err := q.QueryRowContext(ctx, `
		SELECT id,source_order_id,replacement_order_id,target_ticket_type_id,request_fingerprint,quantity,
		       source_total,source_gross_total,target_total,delta_amount,currency,created_at,settled_at,tickets_exchanged_at,capacity_returned_at,
		       target_hold_id,replacement_reservation_id,basis_at,target_unit_amount,target_slot_id,target_price_snapshot
		FROM order_exchanges WHERE organizer_id=$1 AND id=$2`, org, id).
		Scan(&s.exchange.ID, &s.exchange.SourceOrderID, &replacement, &s.exchange.TargetTicketTypeID,
			&s.fingerprint, &s.exchange.Quantity, &s.exchange.SourceTotal, &s.exchange.SourceGrossTotal, &target, &delta,
			&s.exchange.Currency, &createdAt, &settled, &switched, &returned, &targetHold, &replacementReservation, &basis,
			&targetUnit, &targetSlot, &s.exchange.TargetPriceSnapshot)
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
	s.exchange.CapacityReturned = returned.Valid
	s.exchange.BasisRecorded = basis.Valid
	s.exchange.TargetHoldID, s.exchange.ReplacementReservationID = targetHold.UUID, replacementReservation.UUID
	s.exchange.TargetUnitAmount, s.exchange.TargetSlotID = targetUnit.Int64, targetSlot.UUID
	s.exchange.CreatedAt = createdAt.UTC().Truncate(time.Microsecond)
	return s, true, nil
}

// RecordExchangeBasis commits what the settlement will be, BEFORE the provider is called,
// and returns the AUTHORITATIVE persisted basis whether or not this caller wrote it.
//
// Record-or-load (TKT-167). TKT-158 returned only `recorded bool`, which told a losing
// writer that its basis was not the money's basis and left it nothing to do but refuse —
// the handler answered 409. That is safe and it is not recovery: the persisted basis is
// exactly what a retry needs, and a function that will not hand it back forces every
// caller to re-derive the thing the row already knows.
//
// The returned basis is the row's, always. `written` is true only for the caller whose
// UPDATE set basis_at, because the handler has to be able to tell its own commitment from
// somebody else's — the two are indistinguishable in the values alone.
//
// The fallback SELECT is correct under concurrency and not merely a retry: a losing UPDATE
// BLOCKS on the winner's row lock, so by the time it reports zero rows the winner has
// committed and the subsequent read sees it. There is no window in which the update
// matches nothing because the winner is still in flight.
func RecordExchangeBasis(ctx context.Context, db *sql.DB, org, exchangeID uuid.UUID, basis ExchangeBasis) (ExchangeBasis, bool, error) {
	var out ExchangeBasis
	err := db.QueryRowContext(ctx, `
		UPDATE order_exchanges
		SET target_hold_id=$3, replacement_reservation_id=$4, target_total=$5, delta_amount=$6,
		    target_unit_amount=$7, target_slot_id=$8, target_price_snapshot=$9, basis_at=now()
		WHERE organizer_id=$1 AND id=$2 AND basis_at IS NULL
		RETURNING target_hold_id,replacement_reservation_id,target_total,delta_amount,
		          target_unit_amount,target_slot_id,target_price_snapshot`,
		org, exchangeID, basis.TargetHoldID, basis.ReplacementReservationID, basis.TargetTotal,
		basis.DeltaAmount, basis.TargetUnitAmount, basis.TargetSlotID, basis.PriceSnapshot).
		Scan(&out.TargetHoldID, &out.ReplacementReservationID, &out.TargetTotal, &out.DeltaAmount,
			&out.TargetUnitAmount, &out.TargetSlotID, &out.PriceSnapshot)
	if err == nil {
		return out, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ExchangeBasis{}, false, err
	}
	// Zero rows means either a basis is already there or the exchange does not exist, and
	// those must not answer alike: a missing row returning a zero-valued basis would hand
	// the caller total 0, delta 0 and a nil hold — a settlement of nothing that reads as an
	// ordinary even exchange. sql.ErrNoRows propagates instead.
	loaded, found, err := loadExchangeBasis(ctx, db, org, exchangeID)
	if err != nil {
		return ExchangeBasis{}, false, err
	}
	if !found {
		return ExchangeBasis{}, false, sql.ErrNoRows
	}
	return loaded, false, nil
}

// loadExchangeBasis reads a persisted basis. `found` is false when the exchange has no row
// or the row has no basis yet — the caller decides which of those it can tolerate.
func loadExchangeBasis(ctx context.Context, q rowQuerier, org, exchangeID uuid.UUID) (ExchangeBasis, bool, error) {
	var out ExchangeBasis
	var hold, reservation, slot uuid.NullUUID
	var total, delta, unit sql.NullInt64
	err := q.QueryRowContext(ctx, `
		SELECT target_hold_id,replacement_reservation_id,target_total,delta_amount,
		       target_unit_amount,target_slot_id,target_price_snapshot
		FROM order_exchanges WHERE organizer_id=$1 AND id=$2 AND basis_at IS NOT NULL`,
		org, exchangeID).
		Scan(&hold, &reservation, &total, &delta, &unit, &slot, &out.PriceSnapshot)
	if errors.Is(err, sql.ErrNoRows) {
		return ExchangeBasis{}, false, nil
	}
	if err != nil {
		return ExchangeBasis{}, false, err
	}
	// Every column is NOT NULL together with basis_at — migration 0010's
	// order_exchanges_basis_shape CHECK is what makes that true, so this reads them
	// through Null* types only because the columns are nullable in the schema, not
	// because a half-populated basis is representable.
	out.TargetHoldID, out.ReplacementReservationID, out.TargetSlotID = hold.UUID, reservation.UUID, slot.UUID
	out.TargetTotal, out.DeltaAmount, out.TargetUnitAmount = total.Int64, delta.Int64, unit.Int64
	return out, true, nil
}

// mapBindInsertError turns the exchange INSERT's failure into the error the handler acts on.
//
// WHICH unique index fired decides who owns the target hold, and collapsing them was a real
// defect (TKT-167 ai-review pass 3), while enumerating them the OTHER way round was an
// incomplete fix for it (pass 4). Both are recorded because the second mistake is the
// instructive one.
//
// The handler releases the target hold on every bind error except ErrExchangeConflict. The
// hold is keyed `exchange-target:<ExchangeID(organizer, idempotency_key)>`, so ANY collision
// on that identity means another request already holds the SAME claim — releasing it kills a
// claim that may be finalized and already charged against, leaving the buyer paid with no
// target. Only `order_exchanges_one_per_source` means something different: a DIFFERENT
// exchange owns this source order, nothing bound under this identity, so the caller's hold is
// its own and releasing it is the correct cleanup.
//
// **The default is therefore the SAFE answer, and only the one provably-safe-to-release index
// is enumerated.** The first version of this fix inverted that — it named
// `order_exchanges_pkey` and let everything else fall through to ErrOrderNotExchangeable —
// and missed `order_exchanges_organizer_id_idempotency_key_key`, migration 0010's
// `UNIQUE (organizer_id, idempotency_key)`. That index means the same thing as the primary
// key (same identity, shared hold) and the catch-all sent it to the releasing branch, so the
// defect survived the fix through a different constraint. Enumerating the dangerous cases
// requires knowing all of them; enumerating the safe one requires knowing one, and a future
// index added to this table fails safe by default.
//
// Not reachable via BindOrderExchange today — the id is derived from the same
// (organizer, key) that index covers, so the pkey collides first — but the mapping must not
// depend on which of two equivalent constraints PostgreSQL happens to report.
func mapBindInsertError(err error) error {
	switch violatedConstraint(err) {
	case "":
		// Not a unique violation at all.
		return err
	case "order_exchanges_one_per_source":
		return ErrOrderNotExchangeable
	default:
		return ErrExchangeConflict
	}
}

// ExchangeBasis is everything the settlement and the replacement are built from, committed
// in one write before the provider is called.
type ExchangeBasis struct {
	TargetHoldID, ReplacementReservationID, TargetSlotID uuid.UUID
	TargetTotal, DeltaAmount, TargetUnitAmount           int64
	PriceSnapshot                                        []byte
}

// LookupExchangeFor resolves an existing exchange for THIS request, without binding or
// locking anything. The handler calls it before any external work, so a settled replay
// answers without needing catalog to be reachable (ai-review pass 2).
//
// It takes the whole request, not just the id, because answering 200 with a prior exchange
// for a DIFFERENT order or target would tell the caller their request succeeded when a
// different one did (ai-review pass 3). A key names one request or it names nothing.
func LookupExchangeFor(ctx context.Context, db *sql.DB, in ExchangeRequest) (Exchange, bool, error) {
	stored, found, err := lookupExchange(ctx, db, in.OrganizerID, ExchangeID(in.OrganizerID, in.IdempotencyKey))
	if err != nil || !found {
		return Exchange{}, found, err
	}
	if stored.fingerprint != exchangeFingerprint(in) {
		return Exchange{}, false, ErrExchangeConflict
	}
	return stored.exchange, true, nil
}

// CompleteExchangeSettlement records the money half: the replacement order, both totals,
// the signed delta, and the instant it settled. Guarded on `settled_at IS NULL`, so a
// replay keeps the original result — the timestamp is evidence of when the difference
// moved, and a retry must not rewrite it.
// It settles and OWES THE SWITCH EVENT in one transaction (TKT-166). A settled exchange
// that owes no switch work is a permanent `switch_pending`: the money moved, the
// replacement order exists, and nothing will ever issue its tickets — because that order
// deliberately owes no `order.completed` (ADR-039 §4), so this event is the only trigger
// there is. Committing the settlement without the outbox row leaves the buyer paid-up
// holding tickets to an event they exchanged away, and no retry can notice.
//
// This is ADR-016 §Decision 6's rule applied to a second subject, and the reason it is a
// transaction rather than two statements.
func CompleteExchangeSettlement(ctx context.Context, db *sql.DB, org, exchangeID, replacementOrder uuid.UUID) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Lock the exchange before reading its settlement state: two requests completing the
	// same exchange would otherwise both see `settled_at IS NULL`, both build an envelope
	// from their own clock, and race on the outbox primary key with different bytes under
	// one deterministic id.
	var settledAt sql.NullTime
	var alreadyReplacement uuid.NullUUID
	err = tx.QueryRowContext(ctx, `
		SELECT settled_at, replacement_order_id FROM order_exchanges
		WHERE organizer_id=$1 AND id=$2 AND basis_at IS NOT NULL FOR UPDATE`, org, exchangeID).
		Scan(&settledAt, &alreadyReplacement)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// No row, or no basis yet. A no-op, exactly as before this function became
			// transactional (TKT-158's asserted contract): settlement cannot precede the
			// basis, and the caller's flow always records one first.
			return nil
		}
		return err
	}
	if settledAt.Valid {
		// Already settled. The event is still owed if a crash landed between the two
		// writes before they shared a transaction, so this path is not a no-op — but it
		// must settle against the SAME instant, or the replayed bytes would differ from
		// the published ones under one id.
		// The caller's argument is IGNORED on this path, not validated against. Settlement
		// is once-only (TKT-158): the persisted replacement is the one the money settled
		// against, and it is the only one the frozen bytes may describe.
		replacementOrder = alreadyReplacement.UUID
	} else {
		if err := tx.QueryRowContext(ctx, `
			UPDATE order_exchanges SET replacement_order_id=$3, settled_at=now()
			WHERE organizer_id=$1 AND id=$2 AND settled_at IS NULL AND basis_at IS NOT NULL
			RETURNING settled_at`, org, exchangeID, replacementOrder).Scan(&settledAt); err != nil {
			return err
		}
	}

	// The payload comes from the PERSISTED rows, never from a caller's in-memory copy:
	// the frozen bytes must describe what actually committed, and on the replay path
	// there is no in-memory copy to trust anyway.
	data := events.OrderExchangedData{ExchangeID: exchangeID, ReplacementOrderID: replacementOrder, OrganizerID: org}
	err = tx.QueryRowContext(ctx, `
		SELECT x.source_order_id, o.guest_order_ref, r.buyer_id, x.target_slot_id, x.target_ticket_type_id, x.quantity
		FROM order_exchanges x
		JOIN orders o ON o.id = x.source_order_id
		JOIN orders ro ON ro.id = x.replacement_order_id
		JOIN reservations r ON r.id = ro.reservation_id
		WHERE x.organizer_id=$1 AND x.id=$2`, org, exchangeID).
		Scan(&data.SourceOrderID, &data.GuestOrderRef, &data.BuyerID, &data.SlotID, &data.TicketTypeID, &data.Quantity)
	if err != nil {
		return fmt.Errorf("read exchange event basis: %w", err)
	}

	eventID := events.ExchangedEventID(exchangeID)
	envelope, err := events.OrderExchangedEnvelope(eventID, data, settledAt.Time.UTC())
	if err != nil {
		return fmt.Errorf("freeze exchange envelope: %w", err)
	}
	// Same ownership check CompleteOrder makes: DO NOTHING on a row belonging to a
	// different order would let this exchange settle owing nothing. The outbox is keyed
	// by event id and its order_id is the REPLACEMENT order — the one whose tickets the
	// event issues, and the one no other outbox row can claim.
	var owner uuid.UUID
	err = tx.QueryRowContext(ctx, `
		INSERT INTO completion_outbox(event_id,order_id,subject,envelope) VALUES($1,$2,$3,$4)
		ON CONFLICT (event_id) DO UPDATE SET event_id=completion_outbox.event_id
		RETURNING order_id`, eventID, replacementOrder, events.SubjectOrderExchanged, envelope).Scan(&owner)
	if err != nil {
		return fmt.Errorf("owe exchange event: %w", err)
	}
	if owner != replacementOrder {
		return fmt.Errorf("exchange event %s is owned by order %s, not %s", eventID, owner, replacementOrder)
	}
	return tx.Commit()
}

// MarkExchangeTicketsSwitched records that access committed the switch.
//
// Set BEFORE the capacity return, not after (TKT-166). That ordering is what makes
// ADR-038 §1 checkable: capacity may only come back once the old tickets have stopped
// admitting, and the database enforces exactly that with
// `order_exchanges_capacity_after_switch`. Marking it after the return would invert the
// evidence — capacity freed while the row still claims the switch never happened.
//
// The cost is a real substate: switched, capacity outstanding. That is what
// `capacity_returned_at` exists to name (migration 0011); it under-sells until the retry
// lands, which is the safe direction.
func MarkExchangeTicketsSwitched(ctx context.Context, db *sql.DB, org, exchangeID uuid.UUID) error {
	_, err := db.ExecContext(ctx, `
		UPDATE order_exchanges SET tickets_exchanged_at=now()
		WHERE organizer_id=$1 AND id=$2 AND tickets_exchanged_at IS NULL AND settled_at IS NOT NULL`,
		org, exchangeID)
	return err
}

// MarkExchangeCapacityReturned closes the reversal. Guarded on the switch having happened,
// so the row cannot claim a return that preceded it even if a caller asks out of order.
func MarkExchangeCapacityReturned(ctx context.Context, db *sql.DB, org, exchangeID uuid.UUID) error {
	_, err := db.ExecContext(ctx, `
		UPDATE order_exchanges SET capacity_returned_at=now()
		WHERE organizer_id=$1 AND id=$2 AND capacity_returned_at IS NULL AND tickets_exchanged_at IS NOT NULL`,
		org, exchangeID)
	return err
}

// ExchangeSwitch is what the tickets-switched callback needs: which hold gives capacity
// back, how much, and what has already been discharged.
type ExchangeSwitch struct {
	ID, OrganizerID  uuid.UUID
	SourceHoldID     uuid.UUID
	Quantity         int32
	TicketsExchanged bool
	CapacityReturned bool
}

// ErrExchangeNotSettled reports an exchange that is unknown, or known and not yet settled.
// The switch cannot precede the money: access is told to switch by an event that only a
// settled exchange produces, so this is a forged or badly-ordered call, not a race.
var ErrExchangeNotSettled = errors.New("exchange is not settled")

// LoadExchangeSwitch reads the reversal state, organizer-scoped. The hold is the SOURCE
// order's — the one holding the capacity the exchange is giving back.
func LoadExchangeSwitch(ctx context.Context, db *sql.DB, org, exchangeID uuid.UUID) (ExchangeSwitch, error) {
	out := ExchangeSwitch{ID: exchangeID, OrganizerID: org}
	var settled sql.NullTime
	var switched, returned sql.NullTime
	err := db.QueryRowContext(ctx, `
		SELECT x.settled_at, x.tickets_exchanged_at, x.capacity_returned_at, x.quantity, r.hold_id
		FROM order_exchanges x
		JOIN orders o ON o.id = x.source_order_id
		JOIN reservations r ON r.id = o.reservation_id
		WHERE x.organizer_id=$1 AND x.id=$2`, org, exchangeID).
		Scan(&settled, &switched, &returned, &out.Quantity, &out.SourceHoldID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExchangeSwitch{}, ErrExchangeNotSettled
		}
		return ExchangeSwitch{}, err
	}
	if !settled.Valid {
		return ExchangeSwitch{}, ErrExchangeNotSettled
	}
	out.TicketsExchanged, out.CapacityReturned = switched.Valid, returned.Valid
	return out, nil
}
