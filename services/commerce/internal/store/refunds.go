package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// Post-purchase refunds, commerce half (TKT-156, ADR-037).
//
// Commerce owns the QUANTITY ceiling and the persisted unit price; payments owns the
// captured-money ceiling. Neither can enforce the other's honestly, so both do their own
// under their own row lock — see BindRefundLeg on the payments side.

var (
	// ErrOrderNotRefundable reports an order whose checkout did not end in `completed`.
	// Recovery's `refunded` is in that set: it means a FAILED checkout whose money was
	// returned, which is not a purchase anyone can refund.
	ErrOrderNotRefundable = errors.New("only a completed order can be refunded")
	// ErrRefundExceedsOrder reports that this refund would take the cumulative refunded
	// quantity past what was sold. PENDING refunds count — an unresolved refund may yet
	// settle, and releasing its allowance is how an order gets over-refunded.
	ErrRefundExceedsOrder = errors.New("refund would exceed the order quantity")
	// ErrRefundConflict reports the same idempotency key used with a different request.
	ErrRefundConflict = errors.New("refund idempotency key reused with a different request")
	// ErrRefundNoMoney reports an order with nothing to refund. No provider issues a
	// zero-amount refund, and pretending one happened would fabricate a money fact.
	ErrRefundNoMoney = errors.New("order has no captured money to refund")
)

// RefundRequest is one staff refund instruction.
type RefundRequest struct {
	OrderID, OrganizerID uuid.UUID
	Quantity             int32
	IdempotencyKey       string
	Actor, Reason        string
}

// Refund is a durable refund attempt.
type Refund struct {
	ID               uuid.UUID
	OrderID          uuid.UUID
	OrganizerID      uuid.UUID
	Quantity         int32
	UnitAmount       int64
	Amount           int64
	Currency         string
	Status           string
	Completed        bool
	// TicketsVoided reports whether the ticket-voiding half of the reversal has been
	// discharged (TKT-157). Money and reversal are tracked separately on purpose: a
	// refund whose money moved but whose tickets are still valid is a real state, and
	// it has to be visible rather than rounded up to "done".
	TicketsVoided bool
	BuyerID          uuid.UUID
	PaymentFactID    uuid.UUID
	RefundedQty      int32
	RefundedAmount   int64
	OrderRefundState string
	// PaymentSourceKey is the order's checkout idempotency key, which IS the key payments
	// bound the charge operation under: `checkout` passes the same header string to
	// `INSERT INTO orders(idempotency_key…)` and to `POST /internal/charges`. Nothing in
	// either service's types says so, so it is read here once and pinned by a test.
	PaymentSourceKey string
	// CreatedAt is the row's stable creation time. The compensating fact's occurred_at
	// comes from here, never the clock: the fact id is deterministic and the journal
	// compares the whole canonical fact on replay.
	CreatedAt time.Time
}

// RefundID derives a refund's identity from its organizer and idempotency key, so a
// retry that lost its response re-derives the same id — and therefore the same
// deterministic fact ids downstream — rather than minting a second refund.
func RefundID(org uuid.UUID, idempotencyKey string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("refund:"+org.String()+":"+idempotencyKey))
}

// RefundFactID derives the commerce journal fact id for one refund. Namespaced on the
// refund identity rather than on (order, type): the checkout helper's `order+type`
// derivation cannot distinguish two partial refunds of the same order, so a second one
// would silently collide.
func RefundFactID(refundID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(refundID.String()+":order.refunded"))
}

// BindOrderRefund inserts-or-loads the one refund for (organizer, idempotency key) under
// the order row lock, enforcing the quantity ceiling.
//
// FOR UPDATE on the order, not on the refund: the ceiling is a property of the order, and
// two refunds that individually fit can only be judged together by whoever holds it. No
// provider call happens inside this transaction.
func BindOrderRefund(ctx context.Context, db *sql.DB, in RefundRequest) (Refund, error) {
	if in.Quantity < 1 {
		return Refund{}, errors.New("refund quantity must be positive")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Refund{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var status, chargeKey, currency string
	var buyer uuid.UUID
	var soldQty int32
	var unit int64
	err = tx.QueryRowContext(ctx, `
		SELECT o.status, o.idempotency_key, r.buyer_id, r.quantity, r.unit_amount, r.currency
		FROM orders o JOIN reservations r ON r.id = o.reservation_id
		WHERE o.id=$1 AND r.organizer_id=$2 FOR UPDATE OF o`, in.OrderID, in.OrganizerID).
		Scan(&status, &chargeKey, &buyer, &soldQty, &unit, &currency)
	if err != nil {
		return Refund{}, err
	}

	id := RefundID(in.OrganizerID, in.IdempotencyKey)
	fingerprint := refundFingerprint(in)
	// The replay resolves BEFORE eligibility is re-derived, mirroring the payments
	// compensation path: a refund that already settled must answer as itself, not be
	// re-judged against a ceiling its own progress has moved.
	existing, found, err := lookupRefund(ctx, tx, in.OrganizerID, id)
	if err != nil {
		return Refund{}, err
	}
	if found {
		if existing.fingerprint != fingerprint {
			return Refund{}, ErrRefundConflict
		}
		out := existing.refund
		out.OrganizerID, out.BuyerID, out.PaymentSourceKey = in.OrganizerID, buyer, chargeKey
		out.RefundedQty, out.RefundedAmount, out.OrderRefundState, err = orderProjection(ctx, tx, in.OrderID)
		if err != nil {
			return Refund{}, err
		}
		return out, tx.Commit()
	}

	if status != "completed" {
		return Refund{}, ErrOrderNotRefundable
	}
	if unit <= 0 {
		return Refund{}, ErrRefundNoMoney
	}
	var refundedQty sql.NullInt32
	if err := tx.QueryRowContext(ctx, `SELECT sum(quantity) FROM order_refunds WHERE order_id=$1`, in.OrderID).Scan(&refundedQty); err != nil {
		return Refund{}, err
	}
	if refundedQty.Int32+in.Quantity > soldQty {
		return Refund{}, ErrRefundExceedsOrder
	}

	// q × unit_amount, never total ÷ q: the total is a product, and dividing it back
	// introduces a rounding step on a money path that has none.
	amount := int64(in.Quantity) * unit
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO order_refunds(organizer_id,id,order_id,idempotency_key,request_fingerprint,quantity,unit_amount,amount,currency,actor,reason)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		in.OrganizerID, id, in.OrderID, in.IdempotencyKey, fingerprint, in.Quantity, unit, amount, currency, in.Actor, in.Reason); err != nil {
		return Refund{}, err
	}
	bound, found, err := lookupRefund(ctx, tx, in.OrganizerID, id)
	if err != nil {
		return Refund{}, err
	}
	if !found {
		return Refund{}, errors.New("refund row missing after bind")
	}
	out := bound.refund
	out.OrganizerID, out.BuyerID, out.PaymentSourceKey = in.OrganizerID, buyer, chargeKey
	out.RefundedQty, out.RefundedAmount, out.OrderRefundState, err = orderProjection(ctx, tx, in.OrderID)
	if err != nil {
		return Refund{}, err
	}
	return out, tx.Commit()
}

// refundFingerprint is what makes a reused idempotency key with a different body a
// conflict rather than a silent replay of somebody else's refund, exactly as claimOrder
// treats a reused checkout key.
func refundFingerprint(in RefundRequest) string {
	sum := sha256.Sum256([]byte(in.OrderID.String() + "\x00" + strconv.Itoa(int(in.Quantity)) + "\x00" + in.Actor + "\x00" + in.Reason))
	return hex.EncodeToString(sum[:])
}

type storedRefund struct {
	refund      Refund
	fingerprint string
}

func lookupRefund(ctx context.Context, q rowQuerier, org, id uuid.UUID) (storedRefund, bool, error) {
	var s storedRefund
	var factID uuid.NullUUID
	var createdAt time.Time
	var voidedAt sql.NullTime
	err := q.QueryRowContext(ctx, `
		SELECT id,order_id,request_fingerprint,quantity,unit_amount,amount,currency,status,payment_fact_id,created_at,tickets_voided_at
		FROM order_refunds WHERE organizer_id=$1 AND id=$2`, org, id).
		Scan(&s.refund.ID, &s.refund.OrderID, &s.fingerprint, &s.refund.Quantity, &s.refund.UnitAmount,
			&s.refund.Amount, &s.refund.Currency, &s.refund.Status, &factID, &createdAt, &voidedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return storedRefund{}, false, nil
	}
	if err != nil {
		return storedRefund{}, false, err
	}
	s.refund.PaymentFactID = factID.UUID
	s.refund.Completed = s.refund.Status == "completed"
	s.refund.TicketsVoided = voidedAt.Valid
	s.refund.CreatedAt = createdAt.UTC().Truncate(time.Microsecond)
	return s, true, nil
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// OrderRefundProjection reads an order's aggregate refund state. Exported for the
// handler, which needs the projection AFTER completing a refund and must not re-bind the
// refund (and re-take the order lock) just to read three columns.
func OrderRefundProjection(ctx context.Context, db *sql.DB, order uuid.UUID) (int32, int64, string, error) {
	return orderProjection(ctx, db, order)
}

func orderProjection(ctx context.Context, q rowQuerier, order uuid.UUID) (int32, int64, string, error) {
	var qty int32
	var amount int64
	var state string
	err := q.QueryRowContext(ctx, `SELECT refunded_quantity,refunded_amount,refund_status FROM orders WHERE id=$1`, order).Scan(&qty, &amount, &state)
	return qty, amount, state, err
}

// CompleteOrderRefund records the payments fact and advances the order's refund
// projection, in ONE transaction under the order row lock. Only the first completion
// writes (the status guard), so a replayed completion cannot double the projection.
func CompleteOrderRefund(ctx context.Context, db *sql.DB, org, refundID, paymentFactID uuid.UUID) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var order uuid.UUID
	var quantity int32
	var amount int64
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT order_id,quantity,amount,status FROM order_refunds WHERE organizer_id=$1 AND id=$2 FOR UPDATE`, org, refundID).
		Scan(&order, &quantity, &amount, &status); err != nil {
		return err
	}
	if status == "completed" {
		return tx.Commit()
	}
	// Lock the order before touching the projection: CompleteOrderRefund and
	// BindOrderRefund both read the refund set, and taking the order lock second here
	// would let two completions interleave their read-modify-write of the aggregate.
	var sold int32
	if err := tx.QueryRowContext(ctx, `
		SELECT r.quantity FROM orders o JOIN reservations r ON r.id=o.reservation_id
		WHERE o.id=$1 FOR UPDATE OF o`, order).Scan(&sold); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE order_refunds SET status='completed',payment_fact_id=$3,completed_at=now()
		WHERE organizer_id=$1 AND id=$2 AND status='pending'`, org, refundID, paymentFactID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE orders
		SET refunded_quantity = refunded_quantity + $2,
		    refunded_amount   = refunded_amount + $3,
		    refund_status     = CASE WHEN refunded_quantity + $2 >= $4 THEN 'full' ELSE 'partial' END,
		    updated_at        = now()
		WHERE id=$1`, order, quantity, amount, sold); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkRefundTicketsVoided records that a refund's tickets have been voided. Guarded on
// the column still being NULL so a replay keeps the original instant — the timestamp is
// evidence of when the obligation was discharged, and a retry must not rewrite it.
func MarkRefundTicketsVoided(ctx context.Context, db *sql.DB, org, refundID uuid.UUID) error {
	_, err := db.ExecContext(ctx, `
		UPDATE order_refunds SET tickets_voided_at=now()
		WHERE organizer_id=$1 AND id=$2 AND tickets_voided_at IS NULL`, org, refundID)
	return err
}
