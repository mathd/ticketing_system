package store

// Abandoning a SETTLED exchange whose ticket switch Access refused because the source tickets
// were already admitted (TKT-498, owner decision of 2026-10-12, ADR-039 §2).
//
// The exchange settled, so the delta moved and the replacement order exists, but Access will
// never switch the tickets: the buyer used the source tickets. The owner's resolution is to
// abandon the exchange: refund an upgrade delta, write off a downgrade delta, return the
// target capacity, and never issue the replacement.
//
// This file holds the Commerce half that the operator command reads and writes. It never calls
// another service. Access, Payments and Inventory are the command's business, and each write
// here records a fact the command established there.
//
// THE ELIGIBILITY MODEL. A candidate is scoped by (organizer_id, id). It is settled and not
// switched. Its source order's reservation and its replacement order's reservation both belong
// to the exchange's organizer, both carry the exchange quantity, the replacement order is the
// one on `replacement_reservation_id`, it and its reservation completed, the replacement
// reservation holds the target hold, and the source reservation holds a different hold. The
// listing, the load and the locked recheck read the same columns through settledExchangeSelect
// and judge them with the same two functions, stateError and consistencyError.
//
// `capacity_returned_at IS NULL` is not a separate predicate. 0011's
// order_exchanges_capacity_after_switch makes a returned row a switched one, so the switch
// predicate already excludes it, and a separate test of it could not fail.
//
// NAME THE ADVERSARY (ADR-021). The row scoping and the locks order honest Commerce writers.
// They do not constrain a writer with Commerce database access.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

var (
	// ErrExchangeAlreadySwitched reports an exchange whose tickets Access switched. Its
	// replacement is live, so it is not a candidate: the reversal sweep owns what remains.
	ErrExchangeAlreadySwitched = errors.New("the exchange's tickets were switched")
	// ErrExchangeAbandonmentInconsistent reports a settled exchange whose rows disagree with the
	// candidate definition: a reservation outside the organizer, a replacement on another
	// reservation or hold, a quantity that differs, or an incomplete replacement. The schema can
	// represent these; no writer produces them. The command stops rather than act on them.
	ErrExchangeAbandonmentInconsistent = errors.New("the exchange's source and replacement rows are inconsistent")
)

// SettledExchange is one exchange as the abandonment command and its listing read it.
type SettledExchange struct {
	OrganizerID, ID          uuid.UUID
	SourceOrderID            uuid.UUID
	SourceReservationID      uuid.UUID
	SourceHoldID             uuid.UUID
	ReplacementOrderID       uuid.UUID
	ReplacementReservationID uuid.UUID
	TargetHoldID             uuid.UUID
	BuyerID                  uuid.UUID
	Quantity                 int32
	// SourceTotal and TargetTotal are face values; their difference is DeltaAmount, signed.
	// SourceGrossTotal is what the buyer paid for the source (face plus retained fees).
	SourceTotal, SourceGrossTotal int64
	TargetTotal, DeltaAmount      int64
	Currency                      string
	Settled                       bool
	SettledAt                     time.Time
	TicketsExchanged              bool
	// PaymentSourceKey is the source order's checkout key, the key payments bound its charge
	// under. ReplacementPaymentKey is the replacement order's (`exchange:<id>`).
	PaymentSourceKey      string
	ReplacementPaymentKey string

	// The abandonment's durable progress (0028). Started is the reservation: the operator
	// name and the reason are the trimmed attestation, the session user the database login of
	// the connection that reserved it.
	AbandonmentStarted          bool
	AbandonmentStartedAt        time.Time
	AbandonmentOperator         string
	AbandonmentSessionUser      string
	AbandonmentReason           string
	AbandonmentRefundFactID     uuid.UUID
	AbandonmentRefundFailedRef  string
	AbandonmentTargetReturned   bool
	AbandonmentTargetReturnedAt time.Time
	AbandonmentWrittenOff       int64
	Abandoned                   bool
	AbandonedAt                 time.Time

	// What the joins found. Unexported: only consistencyError reads them.
	sourceInOrg, replacementFound, replacementInOrg bool
	sourceQuantity, replacementQuantity             int32
	replacementOrderStatus, replacementStatus       string
	replacementOrderReservation                     uuid.UUID
	replacementHold                                 uuid.UUID
}

// settledExchangeSelect reads one exchange with its source and replacement lines. The caller
// adds the WHERE clause, which always carries `x.organizer_id`.
//
// Both reservations are LEFT JOINed ON their organizer being the exchange's: a reservation in
// another organizer reads as absent, and consistencyError refuses it. Compared with
// x.organizer_id, not the caller's argument, as LoadExchangeSwitch does: the invariant holds
// between persisted rows, and the WHERE makes x.organizer_id the caller's organizer.
const settledExchangeSelect = `
	SELECT x.organizer_id, x.id, x.source_order_id, x.replacement_order_id, x.replacement_reservation_id,
	       x.target_hold_id, x.quantity, x.source_total, x.source_gross_total, x.target_total,
	       x.delta_amount, x.currency, x.settled_at, x.tickets_exchanged_at,
	       x.abandonment_started_at, x.abandonment_operator_name, x.abandonment_session_user,
	       x.abandonment_reason, x.abandonment_refund_fact_id, x.abandonment_refund_failed_ref,
	       x.abandonment_target_returned_at, x.abandonment_written_off_amount, x.abandoned_at,
	       so.idempotency_key,
	       sr.id, sr.hold_id, sr.buyer_id, sr.quantity,
	       ro.id, ro.idempotency_key, ro.status, ro.reservation_id,
	       rr.id, rr.hold_id, rr.quantity, rr.status
	FROM order_exchanges x
	JOIN orders so ON so.id = x.source_order_id
	LEFT JOIN reservations sr ON sr.id = so.reservation_id AND sr.organizer_id = x.organizer_id
	LEFT JOIN orders ro ON ro.id = x.replacement_order_id
	LEFT JOIN reservations rr ON rr.id = ro.reservation_id AND rr.organizer_id = x.organizer_id`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSettledExchange(s rowScanner) (SettledExchange, error) {
	var e SettledExchange
	var replacementOrder, replacementReservation, targetHold uuid.NullUUID
	var targetTotal, delta sql.NullInt64
	var settledAt, switchedAt sql.NullTime
	var srID, srHold, srBuyer, roID, roReservation, rrID, rrHold uuid.NullUUID
	var srQty, rrQty sql.NullInt32
	var roKey, roStatus, rrStatus sql.NullString
	var startedAt, returnedAt, abandonedAt sql.NullTime
	var operator, sessionUser, reason, failedRef sql.NullString
	var refundFact uuid.NullUUID
	var writtenOff sql.NullInt64
	if err := s.Scan(&e.OrganizerID, &e.ID, &e.SourceOrderID, &replacementOrder, &replacementReservation,
		&targetHold, &e.Quantity, &e.SourceTotal, &e.SourceGrossTotal, &targetTotal,
		&delta, &e.Currency, &settledAt, &switchedAt,
		&startedAt, &operator, &sessionUser,
		&reason, &refundFact, &failedRef,
		&returnedAt, &writtenOff, &abandonedAt,
		&e.PaymentSourceKey,
		&srID, &srHold, &srBuyer, &srQty,
		&roID, &roKey, &roStatus, &roReservation,
		&rrID, &rrHold, &rrQty, &rrStatus); err != nil {
		return SettledExchange{}, err
	}
	e.ReplacementOrderID, e.ReplacementReservationID, e.TargetHoldID = replacementOrder.UUID, replacementReservation.UUID, targetHold.UUID
	e.TargetTotal, e.DeltaAmount = targetTotal.Int64, delta.Int64
	e.Settled, e.SettledAt = settledAt.Valid, settledAt.Time.UTC()
	e.TicketsExchanged = switchedAt.Valid
	e.sourceInOrg = srID.Valid
	e.SourceReservationID, e.SourceHoldID, e.BuyerID, e.sourceQuantity = srID.UUID, srHold.UUID, srBuyer.UUID, srQty.Int32
	e.replacementFound = roID.Valid
	e.ReplacementPaymentKey, e.replacementOrderStatus, e.replacementOrderReservation = roKey.String, roStatus.String, roReservation.UUID
	e.replacementInOrg = rrID.Valid
	e.replacementHold, e.replacementQuantity, e.replacementStatus = rrHold.UUID, rrQty.Int32, rrStatus.String
	e.AbandonmentStarted, e.AbandonmentStartedAt = startedAt.Valid, startedAt.Time.UTC()
	e.AbandonmentOperator, e.AbandonmentSessionUser, e.AbandonmentReason = operator.String, sessionUser.String, reason.String
	e.AbandonmentRefundFactID, e.AbandonmentRefundFailedRef = refundFact.UUID, failedRef.String
	e.AbandonmentTargetReturned, e.AbandonmentTargetReturnedAt = returnedAt.Valid, returnedAt.Time.UTC()
	e.AbandonmentWrittenOff = writtenOff.Int64
	e.Abandoned, e.AbandonedAt = abandonedAt.Valid, abandonedAt.Time.UTC()
	return e, nil
}

// stateError judges the exchange's own progress: settled and not switched.
func (e SettledExchange) stateError() error {
	if !e.Settled {
		return fmt.Errorf("%w: %s", ErrExchangeNotSettled, e.ID)
	}
	if e.TicketsExchanged {
		return fmt.Errorf("%w: %s", ErrExchangeAlreadySwitched, e.ID)
	}
	return nil
}

// consistencyError judges the source and replacement rows against the candidate definition.
//
// Each check after a presence check reads its row only when the row is present, so that every
// input class is refused by exactly one predicate and its message names the actual defect: an
// absent reservation reads as quantity 0, which would otherwise also fail the quantity check.
// An unsettled row has no replacement order (0010's settlement shape), so the listing's
// exclusion of unsettled rows is the `replacement order is missing` check.
func (e SettledExchange) consistencyError() error {
	reason := ""
	switch {
	case !e.sourceInOrg:
		reason = "the source reservation is not in the exchange's organizer"
	case e.sourceInOrg && e.sourceQuantity != e.Quantity:
		reason = "the source reservation's quantity differs from the exchange's"
	case !e.replacementFound:
		reason = "the replacement order is missing"
	case e.replacementFound && e.replacementOrderReservation != e.ReplacementReservationID:
		reason = "the replacement order is on another reservation than the exchange records"
	case e.replacementFound && e.replacementOrderStatus != "completed":
		reason = "the replacement order did not complete"
	case e.replacementFound && !e.replacementInOrg:
		reason = "the replacement reservation is not in the exchange's organizer"
	case e.replacementInOrg && e.replacementStatus != "completed":
		reason = "the replacement reservation did not complete"
	case e.replacementInOrg && e.replacementQuantity != e.Quantity:
		reason = "the replacement reservation's quantity differs from the exchange's"
	case e.replacementInOrg && e.replacementHold != e.TargetHoldID:
		reason = "the replacement reservation does not hold the target hold"
	case e.SourceHoldID == e.TargetHoldID:
		reason = "the source and target holds are the same hold"
	default:
		return nil
	}
	return fmt.Errorf("%w: %s: %s", ErrExchangeAbandonmentInconsistent, e.ID, reason)
}

// ListSettledUnswitchedExchanges reports one organizer's settled exchanges whose tickets
// Access has not switched, oldest settlement first.
//
// A started abandonment stays listed with its progress and any recorded provider failure; a
// terminal one does not.
//
// These are CANDIDATES. Commerce cannot see Access's ticket state: a row here may be a switch
// whose callback is late, or an exchange Access refused because the source was admitted. Only
// the abandonment command's ordered Access reads can tell. The listing reads Commerce alone, so
// it works while every other service is down.
//
// Organizer-scoped, unlike list-wedged-exchanges: abandonment is per organizer and the
// command takes the organizer, so the listing answers for the same scope. The primary key
// (organizer_id, id) backs the filter.
//
// There is no `settled_at IS NOT NULL` predicate. 0010's order_exchanges_settlement_shape
// gives an unsettled row no replacement order, and consistencyError drops a row without one,
// so the predicate could not change the result.
func ListSettledUnswitchedExchanges(ctx context.Context, db *sql.DB, org uuid.UUID) ([]SettledExchange, error) {
	rows, err := db.QueryContext(ctx, settledExchangeSelect+`
		WHERE x.organizer_id = $1
		  AND x.tickets_exchanged_at IS NULL
		  AND x.abandoned_at IS NULL
		ORDER BY x.settled_at, x.id`, org)
	if err != nil {
		return nil, fmt.Errorf("list settled unswitched exchanges: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SettledExchange
	for rows.Next() {
		e, err := scanSettledExchange(rows)
		if err != nil {
			return nil, fmt.Errorf("scan settled exchange: %w", err)
		}
		if e.consistencyError() != nil {
			continue
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------------------------
// The abandonment command's store layer.

var (
	// ErrAbandonmentAttributionInvalid reports a blank or too long operator name or reason.
	ErrAbandonmentAttributionInvalid = errors.New("invalid abandonment attribution")
	// ErrAbandonmentAttributionConflict reports an abandonment reserved under another operator
	// and reason pair. Only the first pair can resume it, before and after completion.
	ErrAbandonmentAttributionConflict = errors.New("another operator and reason pair owns this exchange's abandonment")
	// ErrAbandonmentEvidenceStale reports exchange identities that changed after the caller
	// loaded them. The caller's Access proofs and refund inspection were made for the loaded
	// identities, so they do not cover the current ones.
	ErrAbandonmentEvidenceStale = errors.New("the exchange changed after it was inspected")
	// ErrReplacementRefundSetChanged reports replacement refunds that differ, in membership or
	// state, from the set the caller inspected against payments. The caller inspects again.
	ErrReplacementRefundSetChanged = errors.New("the replacement order's refunds changed after they were inspected")
	// ErrReplacementRefundCompleted reports a completed refund of the replacement order. Money
	// moved on the replacement, which needs reconciliation before the exchange can be abandoned.
	ErrReplacementRefundCompleted = errors.New("a refund of the replacement order completed")
	// ErrAbandonmentNotStarted reports progress recorded before the reservation.
	ErrAbandonmentNotStarted = errors.New("the exchange's abandonment has not started")
	// ErrAbandonmentProgressConflict reports progress that differs from what is recorded.
	ErrAbandonmentProgressConflict = errors.New("the abandonment already records different progress")
	// ErrAbandonmentNoRefund reports refund evidence for an exchange that is not an upgrade.
	ErrAbandonmentNoRefund = errors.New("only an upgrade's abandonment refunds a delta")
	// ErrAbandonmentRefundFailed reports a recorded terminal provider failure of the delta
	// refund. The abandonment stops there; TKT-588 owns the next disposition.
	ErrAbandonmentRefundFailed = errors.New("the provider refund of the exchange delta failed")
	// ErrAbandonmentRefundPending reports an upgrade whose delta refund is not recorded yet.
	ErrAbandonmentRefundPending = errors.New("the upgrade's delta refund is not recorded")
	// ErrAbandonmentTargetNotReturned reports a terminal step before the target return.
	ErrAbandonmentTargetNotReturned = errors.New("the target capacity return is not recorded")
	// ErrInvalidProviderRefundRef reports a failure reference that is not a provider `re_…`.
	ErrInvalidProviderRefundRef = errors.New("a provider refund reference is re_ followed by at most 252 bytes")
	// ErrAbandonmentFactMismatch reports a compensating fact that the locked row does not
	// describe.
	ErrAbandonmentFactMismatch = errors.New("the compensating fact does not describe the exchange")
	// ErrAbandonmentFactConflict reports another fact already stored under the compensating
	// fact's id.
	ErrAbandonmentFactConflict = errors.New("a different fact already uses the compensating fact's id")
	// ErrExchangeGrossFactsInconsistent reports original gross facts that disagree with the
	// exchange, or that are missing.
	ErrExchangeGrossFactsInconsistent = errors.New("the exchange's gross journal facts do not match the exchange")
	// ErrExchangeReplacementAbandoned refuses a refund bind or replay on the replacement order of
	// an exchange whose abandonment has started. It wraps ErrOrderNotRefundable, so the staff
	// endpoint answers its existing 409 and the bulk runner its existing `not_refundable`.
	ErrExchangeReplacementAbandoned = fmt.Errorf("%w: its exchange is being abandoned", ErrOrderNotRefundable)
)

// ExchangeAbandonmentRefundID is the one stable id of an abandonment. It keys the payments
// refund of an upgrade delta and the inventory receipt of the target return, so a retry
// re-derives both instead of moving money or capacity twice.
func ExchangeAbandonmentRefundID(org, exchangeID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("exchange-abandonment:"+org.String()+":"+exchangeID.String()))
}

// ExchangeAbandonedFactID is the id of the `order.exchange.abandoned` compensating fact.
func ExchangeAbandonedFactID(org, exchangeID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("order.exchange.abandoned:"+org.String()+":"+exchangeID.String()))
}

// AbandonmentAttribution is the typed operator name and reason, trimmed. The name is the
// operator's personal attestation (ADR-087 §7); the database login is recorded beside it.
type AbandonmentAttribution struct {
	Operator, Reason string
}

// NewAbandonmentAttribution trims and bounds the pair: 1-200 and 1-500 characters, the limits
// 0028's CHECKs apply.
func NewAbandonmentAttribution(operator, reason string) (AbandonmentAttribution, error) {
	a := AbandonmentAttribution{Operator: strings.TrimSpace(operator), Reason: strings.TrimSpace(reason)}
	return a, a.validate()
}

func (a AbandonmentAttribution) validate() error {
	if n := utf8.RuneCountInString(a.Operator); n < 1 || n > 200 || a.Operator != strings.TrimSpace(a.Operator) {
		return fmt.Errorf("%w: the operator name must be 1-200 characters after trimming", ErrAbandonmentAttributionInvalid)
	}
	if n := utf8.RuneCountInString(a.Reason); n < 1 || n > 500 || a.Reason != strings.TrimSpace(a.Reason) {
		return fmt.Errorf("%w: the reason must be 1-500 characters after trimming", ErrAbandonmentAttributionInvalid)
	}
	return nil
}

// LoadSettledExchange reads one exchange, scoped by (organizer, exchange), whatever its state.
// The caller judges it: a started abandonment by its attribution, a fresh one by
// FreshEligibility.
func LoadSettledExchange(ctx context.Context, db *sql.DB, org, exchangeID uuid.UUID) (SettledExchange, error) {
	return loadSettledExchange(ctx, db, org, exchangeID)
}

func loadSettledExchange(ctx context.Context, q rowQuerier, org, exchangeID uuid.UUID) (SettledExchange, error) {
	e, err := scanSettledExchange(q.QueryRowContext(ctx, settledExchangeSelect+`
		WHERE x.organizer_id = $1 AND x.id = $2`, org, exchangeID))
	if errors.Is(err, sql.ErrNoRows) {
		return SettledExchange{}, fmt.Errorf("%w: %s", ErrExchangeNotFound, exchangeID)
	}
	if err != nil {
		return SettledExchange{}, fmt.Errorf("load exchange %s: %w", exchangeID, err)
	}
	return e, nil
}

// FreshEligibility reports whether an exchange with no abandonment yet is a candidate: settled,
// unswitched, and consistent. The reservation repeats the judgment under its locks.
func (e SettledExchange) FreshEligibility() error {
	if err := e.stateError(); err != nil {
		return err
	}
	return e.consistencyError()
}

// ReplacementRefund is one refund of a replacement order, as the caller inspected it.
type ReplacementRefund struct {
	ID     uuid.UUID
	Status string
}

type rowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// ListReplacementRefunds reads the organizer's refunds of a replacement order, ordered by id.
// The caller asks payments about each pending one before it reserves; the reservation reads
// the same set again under the replacement order's lock.
func ListReplacementRefunds(ctx context.Context, q rowsQuerier, org, replacementOrder uuid.UUID) ([]ReplacementRefund, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, status FROM order_refunds
		WHERE organizer_id = $1 AND order_id = $2
		ORDER BY id`, org, replacementOrder)
	if err != nil {
		return nil, fmt.Errorf("list replacement refunds: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ReplacementRefund
	for rows.Next() {
		var r ReplacementRefund
		if err := rows.Scan(&r.ID, &r.Status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AbandonmentReservation is what the caller established before it asks to reserve.
type AbandonmentReservation struct {
	// Expected is the load the caller's Access proofs and refund inspection were made for.
	Expected    SettledExchange
	Attribution AbandonmentAttribution
	// InspectedRefunds is the replacement refund set the caller inspected: each pending, each
	// with a conclusive payments 404 for its leg.
	InspectedRefunds []ReplacementRefund
}

// ReserveExchangeAbandonment reserves the abandonment of one exchange, or resumes it for the
// same attribution. `reserved` is true only for the call that wrote the reservation.
//
// THE LOCK ORDER is the source order, the replacement order, then the exchange. The order locks
// are FOR NO KEY UPDATE, scoped through each order's reservation organizer: a settlement replay
// holds the exchange row and then inserts a completion_outbox row, whose foreign key takes a
// key-share lock on the replacement order, and FOR UPDATE would deadlock with it. BindOrderRefund
// locks one order and then reads order_exchanges without a lock, so it cannot form a cycle
// with this order either.
//
// Under the locks the row is read again. A started row is resumed by the same trimmed pair and
// refused for any other. A fresh row must still be eligible, still describe the identities the
// caller loaded, and still have exactly the replacement refunds the caller inspected, none
// completed. Then the four reservation fields are written in one statement, the session user
// from PostgreSQL on this connection.
func ReserveExchangeAbandonment(ctx context.Context, db *sql.DB, r AbandonmentReservation) (SettledExchange, bool, error) {
	if err := r.Attribution.validate(); err != nil {
		return SettledExchange{}, false, err
	}
	org, id := r.Expected.OrganizerID, r.Expected.ID
	if !r.Expected.Settled {
		return SettledExchange{}, false, fmt.Errorf("%w: %s", ErrExchangeNotSettled, id)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return SettledExchange{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	for _, order := range []uuid.UUID{r.Expected.SourceOrderID, r.Expected.ReplacementOrderID} {
		var locked uuid.UUID
		err := tx.QueryRowContext(ctx, `
			SELECT o.id FROM orders o
			JOIN reservations res ON res.id = o.reservation_id
			WHERE o.id = $1 AND res.organizer_id = $2
			FOR NO KEY UPDATE OF o`, order, org).Scan(&locked)
		if errors.Is(err, sql.ErrNoRows) {
			return SettledExchange{}, false, fmt.Errorf("%w: %s: order %s is not in the organizer", ErrExchangeAbandonmentInconsistent, id, order)
		}
		if err != nil {
			return SettledExchange{}, false, fmt.Errorf("lock order %s: %w", order, err)
		}
	}
	var one int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM order_exchanges WHERE organizer_id = $1 AND id = $2 FOR UPDATE`, org, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return SettledExchange{}, false, fmt.Errorf("%w: %s", ErrExchangeNotFound, id)
	}
	if err != nil {
		return SettledExchange{}, false, fmt.Errorf("lock exchange %s: %w", id, err)
	}
	cur, err := loadSettledExchange(ctx, tx, org, id)
	if err != nil {
		return SettledExchange{}, false, err
	}

	if cur.AbandonmentStarted {
		if cur.AbandonmentOperator != r.Attribution.Operator || cur.AbandonmentReason != r.Attribution.Reason {
			return SettledExchange{}, false, fmt.Errorf("%w: exchange %s", ErrAbandonmentAttributionConflict, id)
		}
		return cur, false, tx.Commit()
	}
	if err := cur.FreshEligibility(); err != nil {
		return SettledExchange{}, false, err
	}
	if !sameIdentities(cur, r.Expected) {
		return SettledExchange{}, false, fmt.Errorf("%w: %s", ErrAbandonmentEvidenceStale, id)
	}
	refunds, err := ListReplacementRefunds(ctx, tx, org, cur.ReplacementOrderID)
	if err != nil {
		return SettledExchange{}, false, err
	}
	if !sameRefundSet(refunds, r.InspectedRefunds) {
		return SettledExchange{}, false, fmt.Errorf("%w: exchange %s", ErrReplacementRefundSetChanged, id)
	}
	for _, rf := range refunds {
		if rf.Status != "pending" {
			return SettledExchange{}, false, fmt.Errorf("%w: refund %s of exchange %s", ErrReplacementRefundCompleted, rf.ID, id)
		}
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE order_exchanges
		SET abandonment_started_at = now(), abandonment_operator_name = $3,
		    abandonment_session_user = session_user, abandonment_reason = $4
		WHERE organizer_id = $1 AND id = $2 AND abandonment_started_at IS NULL`,
		org, id, r.Attribution.Operator, r.Attribution.Reason)
	if err != nil {
		return SettledExchange{}, false, fmt.Errorf("reserve abandonment of %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return SettledExchange{}, false, fmt.Errorf("reserve abandonment of %s: the exchange changed under its lock", id)
	}
	cur, err = loadSettledExchange(ctx, tx, org, id)
	if err != nil {
		return SettledExchange{}, false, err
	}
	return cur, true, tx.Commit()
}

// sameIdentities compares what the caller's evidence was gathered for with the locked row.
func sameIdentities(a, b SettledExchange) bool {
	return a.SourceOrderID == b.SourceOrderID && a.SourceReservationID == b.SourceReservationID &&
		a.SourceHoldID == b.SourceHoldID && a.ReplacementOrderID == b.ReplacementOrderID &&
		a.ReplacementReservationID == b.ReplacementReservationID && a.TargetHoldID == b.TargetHoldID &&
		a.BuyerID == b.BuyerID && a.Quantity == b.Quantity && a.DeltaAmount == b.DeltaAmount &&
		a.Currency == b.Currency
}

func sameRefundSet(current, inspected []ReplacementRefund) bool {
	if len(current) != len(inspected) {
		return false
	}
	want := make(map[uuid.UUID]string, len(inspected))
	for _, r := range inspected {
		want[r.ID] = r.Status
	}
	for _, r := range current {
		if status, ok := want[r.ID]; !ok || status != r.Status {
			return false
		}
	}
	return true
}

// RecordExchangeAbandonmentRefund records the payments fact of an upgrade's delta refund. A
// repeat with the same fact reads back the committed value; any other value conflicts.
func RecordExchangeAbandonmentRefund(ctx context.Context, db *sql.DB, org, exchangeID, factID uuid.UUID) error {
	if factID == uuid.Nil {
		return errors.New("a refund fact id is required")
	}
	res, err := db.ExecContext(ctx, `
		UPDATE order_exchanges SET abandonment_refund_fact_id = $3
		WHERE organizer_id = $1 AND id = $2 AND abandonment_started_at IS NOT NULL AND delta_amount > 0
		  AND abandonment_refund_fact_id IS NULL AND abandonment_refund_failed_ref IS NULL`, org, exchangeID, factID)
	if err != nil {
		return fmt.Errorf("record abandonment refund of %s: %w", exchangeID, err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	e, err := loadSettledExchange(ctx, db, org, exchangeID)
	if err != nil {
		return err
	}
	switch {
	case !e.AbandonmentStarted:
		return fmt.Errorf("%w: %s", ErrAbandonmentNotStarted, exchangeID)
	case e.DeltaAmount <= 0:
		return fmt.Errorf("%w: %s", ErrAbandonmentNoRefund, exchangeID)
	case e.AbandonmentRefundFailedRef != "":
		return fmt.Errorf("%w: %s (%s)", ErrAbandonmentRefundFailed, exchangeID, e.AbandonmentRefundFailedRef)
	case e.AbandonmentRefundFactID == factID:
		return nil
	default:
		return fmt.Errorf("%w: %s records refund fact %s", ErrAbandonmentProgressConflict, exchangeID, e.AbandonmentRefundFactID)
	}
}

// RecordExchangeAbandonmentRefundFailure records the provider reference of a terminal failure
// of an upgrade's delta refund. The abandonment stops there; nothing re-submits the refund.
func RecordExchangeAbandonmentRefundFailure(ctx context.Context, db *sql.DB, org, exchangeID uuid.UUID, ref string) error {
	if !strings.HasPrefix(ref, "re_") || len(ref) <= len("re_") || len(ref) > 255 {
		return fmt.Errorf("%w: %q", ErrInvalidProviderRefundRef, ref)
	}
	res, err := db.ExecContext(ctx, `
		UPDATE order_exchanges SET abandonment_refund_failed_ref = $3
		WHERE organizer_id = $1 AND id = $2 AND abandonment_started_at IS NOT NULL AND delta_amount > 0
		  AND abandonment_refund_fact_id IS NULL AND abandonment_refund_failed_ref IS NULL`, org, exchangeID, ref)
	if err != nil {
		return fmt.Errorf("record abandonment refund failure of %s: %w", exchangeID, err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	e, err := loadSettledExchange(ctx, db, org, exchangeID)
	if err != nil {
		return err
	}
	switch {
	case !e.AbandonmentStarted:
		return fmt.Errorf("%w: %s", ErrAbandonmentNotStarted, exchangeID)
	case e.DeltaAmount <= 0:
		return fmt.Errorf("%w: %s", ErrAbandonmentNoRefund, exchangeID)
	case e.AbandonmentRefundFailedRef == ref:
		return nil
	default:
		return fmt.Errorf("%w: %s records refund fact %s, failure %q", ErrAbandonmentProgressConflict,
			exchangeID, e.AbandonmentRefundFactID, e.AbandonmentRefundFailedRef)
	}
}

// RecordExchangeAbandonmentTargetReturned records, once, that inventory returned the target
// capacity, and returns the recorded instant. The compensating fact's occurred_at is that
// instant, so a retry rebuilds the same fact.
//
// A recorded provider failure needs no predicate of its own: 0028 allows it only on an upgrade
// and never beside a refund fact, so the refund requirement already refuses it.
func RecordExchangeAbandonmentTargetReturned(ctx context.Context, db *sql.DB, org, exchangeID uuid.UUID) (time.Time, error) {
	var at time.Time
	err := db.QueryRowContext(ctx, `
		UPDATE order_exchanges SET abandonment_target_returned_at = now()
		WHERE organizer_id = $1 AND id = $2 AND abandonment_started_at IS NOT NULL
		  AND abandonment_target_returned_at IS NULL
		  AND (delta_amount <= 0 OR abandonment_refund_fact_id IS NOT NULL)
		RETURNING abandonment_target_returned_at`, org, exchangeID).Scan(&at)
	if err == nil {
		return at.UTC(), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, fmt.Errorf("record target return of %s: %w", exchangeID, err)
	}
	e, err := loadSettledExchange(ctx, db, org, exchangeID)
	if err != nil {
		return time.Time{}, err
	}
	switch {
	case !e.AbandonmentStarted:
		return time.Time{}, fmt.Errorf("%w: %s", ErrAbandonmentNotStarted, exchangeID)
	case e.AbandonmentRefundFailedRef != "":
		return time.Time{}, fmt.Errorf("%w: %s (%s)", ErrAbandonmentRefundFailed, exchangeID, e.AbandonmentRefundFailedRef)
	case e.AbandonmentTargetReturned:
		return e.AbandonmentTargetReturnedAt, nil
	default:
		return time.Time{}, fmt.Errorf("%w: %s", ErrAbandonmentRefundPending, exchangeID)
	}
}

// ExchangeAbandonedFact is the `order.exchange.abandoned` compensating fact. The envelope uses
// the existing order_facts columns and the payments fact contract; the payload is rebuilt from
// write-once exchange fields, so it needs no storage of its own.
//
// Amount is the target gross the abandonment removes: the target face plus the fee retained
// from the source, the amount the `order.exchange.sold` fact recorded. SourceGross is the
// source gross it restores, the amount `order.exchange.reversed` recorded. Amount - SourceGross
// is DeltaAmount. An upgrade carries the payments fact of its delta refund, a downgrade the
// written-off delta, an even exchange neither.
type ExchangeAbandonedFact struct {
	ID, OrganizerID, OrderID, BuyerID, ExchangeID uuid.UUID
	Amount                                        int64
	Currency                                      string
	OccurredAt                                    time.Time
	SourceGross, DeltaAmount                      int64
	RefundFactID                                  uuid.UUID
	WrittenOffAmount                              int64
}

// ExchangeAbandonedFactType is the journal fact type of the compensating fact.
const ExchangeAbandonedFactType = "order.exchange.abandoned"

// Payload is the fact's payload as payments validates it: base-10 integers in minor units and
// canonical UUIDs.
func (f ExchangeAbandonedFact) Payload() map[string]string {
	p := map[string]string{
		"order_id":     f.OrderID.String(),
		"exchange_id":  f.ExchangeID.String(),
		"source_gross": strconv.FormatInt(f.SourceGross, 10),
		"delta_amount": strconv.FormatInt(f.DeltaAmount, 10),
	}
	switch {
	case f.DeltaAmount > 0:
		p["refund_fact_id"] = f.RefundFactID.String()
	case f.DeltaAmount < 0:
		p["written_off_amount"] = strconv.FormatInt(f.WrittenOffAmount, 10)
	}
	return p
}

func (f ExchangeAbandonedFact) equal(g ExchangeAbandonedFact) bool {
	return f.ID == g.ID && f.OrganizerID == g.OrganizerID && f.OrderID == g.OrderID && f.BuyerID == g.BuyerID &&
		f.ExchangeID == g.ExchangeID && f.Amount == g.Amount && f.Currency == g.Currency &&
		f.OccurredAt.Equal(g.OccurredAt) && f.SourceGross == g.SourceGross && f.DeltaAmount == g.DeltaAmount &&
		f.RefundFactID == g.RefundFactID && f.WrittenOffAmount == g.WrittenOffAmount
}

// targetGross is the target face plus the fee retained from the source, as the exchange
// handler's replacementGross computes it.
func (e SettledExchange) targetGross() (int64, error) {
	retained := e.SourceGrossTotal - e.SourceTotal
	if retained < 0 || e.TargetTotal > math.MaxInt64-retained {
		return 0, fmt.Errorf("exchange %s: target gross out of range", e.ID)
	}
	return e.TargetTotal + retained, nil
}

// BuildExchangeAbandonedFact builds the compensating fact of an abandonment whose target
// capacity came back. Its occurred_at is the recorded target-return instant.
func BuildExchangeAbandonedFact(e SettledExchange) (ExchangeAbandonedFact, error) {
	switch {
	case !e.AbandonmentStarted:
		return ExchangeAbandonedFact{}, fmt.Errorf("%w: %s", ErrAbandonmentNotStarted, e.ID)
	case e.AbandonmentRefundFailedRef != "":
		return ExchangeAbandonedFact{}, fmt.Errorf("%w: %s", ErrAbandonmentRefundFailed, e.ID)
	case !e.AbandonmentTargetReturned:
		return ExchangeAbandonedFact{}, fmt.Errorf("%w: %s", ErrAbandonmentTargetNotReturned, e.ID)
	case e.DeltaAmount > 0 && e.AbandonmentRefundFactID == uuid.Nil:
		return ExchangeAbandonedFact{}, fmt.Errorf("%w: %s", ErrAbandonmentRefundPending, e.ID)
	}
	gross, err := e.targetGross()
	if err != nil {
		return ExchangeAbandonedFact{}, err
	}
	f := ExchangeAbandonedFact{
		ID: ExchangeAbandonedFactID(e.OrganizerID, e.ID), OrganizerID: e.OrganizerID,
		OrderID: e.SourceOrderID, BuyerID: e.BuyerID, ExchangeID: e.ID,
		Amount: gross, Currency: e.Currency, OccurredAt: e.AbandonmentTargetReturnedAt.UTC(),
		SourceGross: e.SourceGrossTotal, DeltaAmount: e.DeltaAmount,
	}
	switch {
	case e.DeltaAmount > 0:
		f.RefundFactID = e.AbandonmentRefundFactID
	case e.DeltaAmount < 0:
		f.WrittenOffAmount = -e.DeltaAmount
	}
	return f, nil
}

// FinishExchangeAbandonment is the terminal transaction. It runs after payments acknowledged
// the identical fact, so the journal holds the correction before Commerce reports it done.
//
// Under the exchange row lock it rebuilds the fact from the row and refuses any other, appends
// it to order_facts with the existing columns, and sets abandoned_at and the downgrade write-off
// together. A fact row already under the id with other content is a conflicting identity and an
// error. An exchange already terminal returns itself with replay=true and writes nothing.
func FinishExchangeAbandonment(ctx context.Context, db *sql.DB, f ExchangeAbandonedFact) (SettledExchange, bool, error) {
	org, id := f.OrganizerID, f.ExchangeID
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return SettledExchange{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var one int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM order_exchanges WHERE organizer_id = $1 AND id = $2 FOR UPDATE`, org, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return SettledExchange{}, false, fmt.Errorf("%w: %s", ErrExchangeNotFound, id)
	}
	if err != nil {
		return SettledExchange{}, false, fmt.Errorf("lock exchange %s: %w", id, err)
	}
	cur, err := loadSettledExchange(ctx, tx, org, id)
	if err != nil {
		return SettledExchange{}, false, err
	}
	want, err := BuildExchangeAbandonedFact(cur)
	if err != nil {
		return SettledExchange{}, false, err
	}
	if !f.equal(want) {
		return SettledExchange{}, false, fmt.Errorf("%w: %s", ErrAbandonmentFactMismatch, id)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO order_facts(fact_id, order_id, organizer_id, buyer_id, fact_type, amount, currency, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (fact_id) DO NOTHING`,
		f.ID, f.OrderID, f.OrganizerID, f.BuyerID, ExchangeAbandonedFactType, f.Amount, f.Currency, f.OccurredAt); err != nil {
		return SettledExchange{}, false, fmt.Errorf("append abandonment fact of %s: %w", id, err)
	}
	var stored ExchangeAbandonedFact
	var storedType string
	if err := tx.QueryRowContext(ctx, `
		SELECT order_id, organizer_id, buyer_id, fact_type, amount, currency, occurred_at
		FROM order_facts WHERE fact_id = $1`, f.ID).
		Scan(&stored.OrderID, &stored.OrganizerID, &stored.BuyerID, &storedType, &stored.Amount, &stored.Currency, &stored.OccurredAt); err != nil {
		return SettledExchange{}, false, fmt.Errorf("read abandonment fact of %s: %w", id, err)
	}
	if stored.OrderID != f.OrderID || stored.OrganizerID != f.OrganizerID || stored.BuyerID != f.BuyerID ||
		storedType != ExchangeAbandonedFactType || stored.Amount != f.Amount || stored.Currency != f.Currency ||
		!stored.OccurredAt.Equal(f.OccurredAt) {
		return SettledExchange{}, false, fmt.Errorf("%w: fact %s", ErrAbandonmentFactConflict, f.ID)
	}
	if cur.Abandoned {
		return cur, true, tx.Commit()
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE order_exchanges
		SET abandoned_at = now(),
		    abandonment_written_off_amount = CASE WHEN delta_amount < 0 THEN -delta_amount END
		WHERE organizer_id = $1 AND id = $2 AND abandoned_at IS NULL`, org, id)
	if err != nil {
		return SettledExchange{}, false, fmt.Errorf("finish abandonment of %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return SettledExchange{}, false, fmt.Errorf("finish abandonment of %s: the exchange changed under its lock", id)
	}
	cur, err = loadSettledExchange(ctx, tx, org, id)
	if err != nil {
		return SettledExchange{}, false, err
	}
	return cur, false, tx.Commit()
}

// VerifyExchangeGrossFacts checks the two gross facts the settlement journalled against the
// exchange: same organizer, source order, buyer and currency, the expected types, the source
// gross on the reversal and the target gross on the sale. The two amounts then differ by the
// delta, because 0010 makes the delta the face difference and both grosses carry the same
// retained fee. The command runs this before a fresh reservation, so a missing or inconsistent
// original fact stops before any compensation.
func VerifyExchangeGrossFacts(ctx context.Context, q rowQuerier, e SettledExchange) error {
	gross, err := e.targetGross()
	if err != nil {
		return err
	}
	for _, leg := range []struct {
		id     uuid.UUID
		typ    string
		amount int64
	}{
		{ExchangeReversedFactID(e.ID), "order.exchange.reversed", e.SourceGrossTotal},
		{ExchangeSoldFactID(e.ID), "order.exchange.sold", gross},
	} {
		var order, buyer uuid.UUID
		var typ, currency string
		var amount int64
		err := q.QueryRowContext(ctx, `
			SELECT order_id, buyer_id, fact_type, amount, currency FROM order_facts
			WHERE fact_id = $1 AND organizer_id = $2`, leg.id, e.OrganizerID).Scan(&order, &buyer, &typ, &amount, &currency)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s: %s fact %s is missing", ErrExchangeGrossFactsInconsistent, e.ID, leg.typ, leg.id)
		}
		if err != nil {
			return fmt.Errorf("read %s fact of %s: %w", leg.typ, e.ID, err)
		}
		if order != e.SourceOrderID || buyer != e.BuyerID || typ != leg.typ || currency != e.Currency || amount != leg.amount {
			return fmt.Errorf("%w: %s: %s fact %s disagrees with the exchange", ErrExchangeGrossFactsInconsistent, e.ID, leg.typ, leg.id)
		}
	}
	return nil
}
