package store

// Unwinding a WEDGED exchange (TKT-255, ADR-067).
//
// The durable half of the operator path out of the wedge TKT-167's review found: an
// exchange whose inventory target claim went terminal answers 409 at `finalize` forever,
// while its `order_exchanges` row keeps the source order both un-exchangeable (the unique
// index) and un-refundable (`BindOrderRefund`'s bare count). Nothing in the service can
// resolve that, because 0010 records deliberately that an exchange has no cancelled state.
//
// WHAT THIS FILE DOES NOT DO, and the boundary is the design rather than a scope note: it
// never moves money and it never asks whether money moved. Deciding that is
// `internal/exchangeunwind`'s job, against PAYMENTS' own records, before this is called —
// COS 2 says "proven against payments-side evidence, not commerce's flags", and a flag on
// the row here cannot answer it. `UnwindWedgedExchange` takes `moneyMoved` as a fact
// established elsewhere and refuses on it; it cannot derive it, and giving it the ability
// to would put the decision back inside the database that the wedge already proved is not
// the authority on it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrExchangeNotFound reports an organizer/exchange pair with no row.
	ErrExchangeNotFound = errors.New("exchange not found")
	// ErrExchangeSettled reports an exchange that has settled. A settled exchange is not
	// wedged: its money moved, its replacement order exists, and the switch and capacity
	// obligations that remain are the ADR-063 sweep's to drive. Unwinding one would delete
	// the record of a completed sale.
	ErrExchangeSettled = errors.New("a settled exchange cannot be unwound")
	// ErrExchangeMoneyMoved reports an unwind refused because payments' own records say a
	// provider movement exists — or, deliberately, because payments could not be asked and
	// answered anything other than a clean absence. The refusal is the point of the ticket:
	// deleting the binding of a charged buyer strands them worse than the wedge does.
	ErrExchangeMoneyMoved = errors.New("an exchange whose money moved cannot be unwound")
	// ErrExchangeUnwindConflict reports a row that changed under a lock that should have
	// prevented it.
	ErrExchangeUnwindConflict = errors.New("exchange changed during unwind")
	// ErrExchangeSettling reports an unwind refused because a settlement is IN FLIGHT for
	// this exchange — it has passed inventory's finalize and may be at the provider right
	// now (ai-review pass 1 [critical]).
	//
	// The window is real and narrow. A resume's source-order lock is released when
	// `BindOrderExchange` commits, and everything that moves money happens after that, so
	// the unwind's lock alone cannot see an in-flight resume. The mitigating fact is
	// ordering: `completeExchangeFromBasis` calls finalize BEFORE the provider, and a
	// genuinely wedged exchange — the only kind an operator is meant to unwind — cannot
	// pass finalize, so it can never reach the charge. The exposure is therefore an
	// operator unwinding a HEALTHY exchange that is mid-flight, which the CLI cannot rule
	// out because commerce holds no copy of inventory's claim state.
	//
	// So the marker exists rather than the argument: a settlement that has passed finalize
	// says so durably, and the unwind refuses while it is outstanding.
	ErrExchangeSettling = errors.New("a settlement is in flight for this exchange")
	// ErrExchangeUnwound reports a settlement whose exchange was unwound underneath it.
	// Returned by CompleteExchangeSettlement instead of the silent no-op a missing row used
	// to mean, because since this ticket a missing row is reachable and means something
	// specific — see that function.
	ErrExchangeUnwound = errors.New("exchange was unwound while its settlement was in flight")
)

// WedgedExchange is one unsettled exchange as an operator needs to read it.
//
// "Unsettled" and not "wedged" is what the query can actually establish, and the difference
// is not pedantry — it decides what the CLI is allowed to print. Whether the target claim is
// terminal is INVENTORY's state, and commerce holds no copy of it. So this lists candidates,
// and the operator establishes terminality before acting. Saying otherwise would be the
// mistake ADR-063 §2 names: asserting a fact about another service's state.
type WedgedExchange struct {
	OrganizerID, ID uuid.UUID
	SourceOrderID   uuid.UUID
	IdempotencyKey  string
	Actor, Reason   string
	Currency        string
	Quantity        int32
	SourceTotal     int64
	CreatedAt       time.Time
	// BasisRecorded decides whether money COULD have moved at all, because the basis is
	// persisted before any provider call (ADR-039 §3c). It is therefore the first thing an
	// operator reads: a bound exchange with no basis never reached payments.
	BasisRecorded bool
	// TargetHoldID is the claim to go and inspect in inventory. Zero when no basis exists.
	TargetHoldID uuid.UUID
	// DeltaAmount is the signed difference, and its SIGN selects which payments record the
	// unwind must consult — an upgrade binds a charge operation, a downgrade a refund leg,
	// and an even exchange calls nobody. Valid only when BasisRecorded.
	DeltaAmount int64
	TargetTotal int64
	// PaymentSourceKey is the SOURCE order's checkout idempotency key. A downgrade's refund
	// leg is addressed by it together with the derived refund key, so the listing carries it
	// rather than making every caller re-join to `orders`.
	PaymentSourceKey string
	// Settling reports that this exchange passed inventory's finalize and may be moving
	// money right now. An operator must not unwind it, and the unwind refuses. A marker that
	// is minutes old means a settlement crashed after finalize — a retry can still complete
	// it, so it still must not be unwound silently, and it is a state a human should look at.
	Settling   bool
	SettlingAt time.Time
}

// ListWedgedExchanges reports every UNSETTLED exchange, oldest first.
//
// No organizer scope, and that is deliberate rather than an omission: this is an operator
// command run by a human holding the commerce database credentials during an incident, and
// the population it reads is bounded by how many exchanges are simultaneously stuck. An
// operator who cannot see a tenant's stuck order cannot resolve it. The listing is not a
// tenant-facing read path and must never become one.
//
// `settled_at IS NULL` is the whole predicate. It admits both wedge shapes — basis recorded
// (which may have charged) and bound-with-no-basis (which cannot have) — because both are
// stuck for the same reason and neither is visible to any other surface in the service: the
// ADR-063 sweep filters `settled_at IS NOT NULL` in every conjunct and every gauge.
//
// It also admits exchanges that are merely IN FLIGHT — a request being processed right now
// is unsettled for a few hundred milliseconds. That is a false positive by construction and
// cannot be predicated away, because "wedged" is not a state commerce records. `created_at`
// is returned so an operator can tell a second-old row from a week-old one, and the CLI says
// so.
func ListWedgedExchanges(ctx context.Context, db *sql.DB) ([]WedgedExchange, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT x.organizer_id, x.id, x.source_order_id, x.idempotency_key, x.actor, x.reason,
		       x.currency, x.quantity, x.source_total, x.created_at,
		       x.basis_at, x.target_hold_id, x.delta_amount, x.target_total, o.idempotency_key,
		       x.settling_at
		FROM order_exchanges x
		JOIN orders o ON o.id = x.source_order_id
		WHERE x.settled_at IS NULL
		ORDER BY x.created_at`)
	if err != nil {
		return nil, fmt.Errorf("list wedged exchanges: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []WedgedExchange
	for rows.Next() {
		var w WedgedExchange
		var basisAt, settlingAt sql.NullTime
		var hold uuid.NullUUID
		var delta, target sql.NullInt64
		if err := rows.Scan(&w.OrganizerID, &w.ID, &w.SourceOrderID, &w.IdempotencyKey, &w.Actor,
			&w.Reason, &w.Currency, &w.Quantity, &w.SourceTotal, &w.CreatedAt,
			&basisAt, &hold, &delta, &target, &w.PaymentSourceKey, &settlingAt); err != nil {
			return nil, fmt.Errorf("scan wedged exchange: %w", err)
		}
		w.BasisRecorded = basisAt.Valid
		w.Settling, w.SettlingAt = settlingAt.Valid, settlingAt.Time.UTC()
		w.TargetHoldID, w.DeltaAmount, w.TargetTotal = hold.UUID, delta.Int64, target.Int64
		w.CreatedAt = w.CreatedAt.UTC()
		out = append(out, w)
	}
	return out, rows.Err()
}

// LoadWedgedExchange reads one unsettled exchange, for the caller that must decide whether
// its money moved before asking for it to be unwound.
//
// It refuses a settled exchange here as well as inside the transaction, and the duplication
// is deliberate: this read is what the money check is performed against, so answering for a
// settled row would send the caller to payments about an exchange it must not touch anyway.
// The authoritative refusal is still the one under the lock — this one can go stale the
// instant it returns, which is exactly why it is not the only one.
func LoadWedgedExchange(ctx context.Context, db *sql.DB, org, exchangeID uuid.UUID) (WedgedExchange, error) {
	var w WedgedExchange
	var basisAt, settledAt, settlingAt sql.NullTime
	var hold uuid.NullUUID
	var delta, target sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT x.organizer_id, x.id, x.source_order_id, x.idempotency_key, x.actor, x.reason,
		       x.currency, x.quantity, x.source_total, x.created_at,
		       x.basis_at, x.target_hold_id, x.delta_amount, x.target_total, o.idempotency_key,
		       x.settled_at, x.settling_at
		FROM order_exchanges x
		JOIN orders o ON o.id = x.source_order_id
		WHERE x.organizer_id=$1 AND x.id=$2`, org, exchangeID).
		Scan(&w.OrganizerID, &w.ID, &w.SourceOrderID, &w.IdempotencyKey, &w.Actor, &w.Reason,
			&w.Currency, &w.Quantity, &w.SourceTotal, &w.CreatedAt,
			&basisAt, &hold, &delta, &target, &w.PaymentSourceKey, &settledAt, &settlingAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WedgedExchange{}, fmt.Errorf("%w: %s", ErrExchangeNotFound, exchangeID)
	}
	if err != nil {
		return WedgedExchange{}, fmt.Errorf("load wedged exchange %s: %w", exchangeID, err)
	}
	if settledAt.Valid {
		return WedgedExchange{}, fmt.Errorf("%w: %s", ErrExchangeSettled, exchangeID)
	}
	w.BasisRecorded = basisAt.Valid
	w.Settling, w.SettlingAt = settlingAt.Valid, settlingAt.Time.UTC()
	w.TargetHoldID, w.DeltaAmount, w.TargetTotal = hold.UUID, delta.Int64, target.Int64
	w.CreatedAt = w.CreatedAt.UTC()
	return w, nil
}

// MarkExchangeSettling records that a settlement has passed inventory's finalize and may
// now move money (TKT-255, ai-review pass 1 [critical]).
//
// Called by the resume and the forward path at the ONE moment that matters: finalize has
// succeeded, so the target claim is secured and the provider call is next. Before that
// instant the exchange either is wedged (finalize refuses) or has not started; after it, an
// unwind must not delete the row.
//
// Idempotent and never cleared. A repeat sets the same marker again, which a resume does on
// every retry. Nothing reclaims it, deliberately: a settlement that crashed after finalize
// leaves an exchange a retry can still complete, and an operator who unwound it silently
// would be doing exactly what this marker exists to prevent. A stale marker is a state a
// human should look at, and the listing shows it.
//
// Best-effort by design at the call site — a failure to mark must not fail an exchange that
// is otherwise fine, because the marker protects against a concurrent operator command, not
// against losing money. The caller logs and continues.
func MarkExchangeSettling(ctx context.Context, db *sql.DB, org, exchangeID uuid.UUID) error {
	result, err := db.ExecContext(ctx, `
		UPDATE order_exchanges SET settling_at=now()
		WHERE organizer_id=$1 AND id=$2 AND settling_at IS NULL`, org, exchangeID)
	if err != nil {
		return fmt.Errorf("mark exchange %s settling: %w", exchangeID, err)
	}
	// ZERO ROWS HAS TWO MEANINGS and only one of them is benign (ai-review pass 2).
	// Already-marked is the ordinary case — a resume marks on every retry. But the row
	// being GONE means an unwind won the race and deleted it while this settlement was
	// between finalize and the provider, and reporting that as a successful mark tells the
	// caller it is protected when it is not. The caller cannot undo the finalize, but it can
	// be told, and its log line is the only notice anyone gets.
	if n, _ := result.RowsAffected(); n == 0 {
		var exists bool
		if err := db.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM order_exchanges WHERE organizer_id=$1 AND id=$2)`,
			org, exchangeID).Scan(&exists); err != nil {
			return fmt.Errorf("confirm exchange %s still exists: %w", exchangeID, err)
		}
		if !exists {
			return fmt.Errorf("%w: %s", ErrExchangeUnwound, exchangeID)
		}
	}
	return nil
}

// SettlingGraceWindow bounds how long a settlement-in-flight marker VETOES an unwind
// (ai-review pass 2 [high]).
//
// The marker must not be a permanent veto, and the first version of it was. `settling_at` is
// write-once and nothing clears it, so a settlement that failed definitively — payments
// refusing, or unreachable for long enough that the caller gave up — left the marker set
// forever and the source order permanently un-unwindable. That is the WEDGE THIS TICKET
// EXISTS TO FIX, reintroduced by the guard added to protect it, and it would have been worse
// than the original because it also blocks the operator command.
//
// So the marker bounds a WINDOW rather than granting a veto. Inside it, a settlement is
// plausibly still in flight at the provider and the unwind refuses without asking anything
// else — cheap, and correct for the case the marker was added for. Outside it, the marker
// stops deciding and the authoritative check takes over: payments' own records, which is
// what COS 2 says must decide this and what a commerce flag can never answer. A settlement
// that really did move money is refused by that check anyway; one that failed before moving
// any is unwound, which is the outcome an operator needs.
//
// Five minutes because it is comfortably longer than any settlement round trip this system
// makes and short enough that an operator working an incident is not blocked by it. It is not
// a lease: nothing reclaims the marker, and its AGE stays visible in the listing so a
// long-stale one is still a state a human should look at.
const SettlingGraceWindow = 5 * time.Minute

// UnwindWedgedExchange removes a wedged exchange's binding and records why, atomically.
//
// `moneyMoved` is a fact the CALLER established against payments, and this function's
// contract is to refuse on it, never to interpret it. See the file header for why it is not
// derived here.
//
// FOUR predicates guard the write, each reported distinguishably, and the ORDER matters the
// same way `UnparkOrder`'s does: a blank reason is refused before the transaction opens,
// because it is a caller mistake rather than a state problem; then existence, then settled,
// then money. Reporting "money moved" for an exchange that does not exist would send an
// operator hunting for a charge that was never made.
//
// THE LOCK IS ON THE SOURCE ORDER, not on the exchange row, and that is the load-bearing
// choice. `BindOrderExchange` and `BindOrderRefund` both take `FOR UPDATE` on `orders`, and
// `refunds.go` records that this lock is "what makes the check meaningful rather than
// advisory". A resume cannot reach `completeExchangeFromBasis` without passing
// `BindOrderExchange` first, so it queues behind this lock. Locking `order_exchanges`
// instead would lock the artefact rather than the identity that arbitrates access to it —
// the mistake ADR-029 documents for seat maps, where a blocked writer rechecked a stale row.
//
// NAME THE ADVERSARY (ADR-021). What this closes is the concurrency race between two
// commerce callers. It does NOT make the payments read atomic with the delete — payments is
// another service, and no lock here reaches it. The honest claim is that the evidence was
// true when it was read and the window is one transaction wide. It constrains nobody holding
// commerce's database credentials, who can delete the row directly and leave no evidence.
func UnwindWedgedExchange(ctx context.Context, db *sql.DB, org, exchangeID uuid.UUID,
	reason string, moneyMoved bool) error {
	// Refused before the transaction opens, as UnparkOrder refuses its own: the reason is
	// the only part of this record a later reader cannot reconstruct from what is captured
	// automatically, and a blank one leaves evidence that looks complete and says nothing.
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("a reason is required: it is the only part of the record a later reader cannot reconstruct")
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("unwind exchange: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The SOURCE ORDER's lock first, taken through the exchange row so that a missing
	// exchange is reported as such rather than as a missing order.
	var sourceOrder uuid.UUID
	err = tx.QueryRowContext(ctx, `
		SELECT o.id
		FROM order_exchanges x
		JOIN orders o ON o.id = x.source_order_id
		WHERE x.organizer_id=$1 AND x.id=$2
		FOR UPDATE OF o`, org, exchangeID).Scan(&sourceOrder)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrExchangeNotFound, exchangeID)
	}
	if err != nil {
		return fmt.Errorf("lock source order for %s: %w", exchangeID, err)
	}

	// Re-read the exchange UNDER the lock. The caller's earlier read decided the money
	// question against payments; this one decides the state question against the row as it
	// stands now, which is the only reading that can be trusted to still be true at COMMIT.
	var settledAt, basisAt, settlingAt sql.NullTime
	var hold uuid.NullUUID
	var delta, target sql.NullInt64
	var idempotencyKey, actor, currency string
	var sourceTotal int64
	if err := tx.QueryRowContext(ctx, `
		SELECT settled_at, basis_at, settling_at, target_hold_id, delta_amount, target_total,
		       idempotency_key, actor, currency, source_total
		FROM order_exchanges WHERE organizer_id=$1 AND id=$2`, org, exchangeID).
		Scan(&settledAt, &basisAt, &settlingAt, &hold, &delta, &target,
			&idempotencyKey, &actor, &currency, &sourceTotal); err != nil {
		return fmt.Errorf("read exchange %s: %w", exchangeID, err)
	}
	if settledAt.Valid {
		return fmt.Errorf("%w: %s", ErrExchangeSettled, exchangeID)
	}
	// SETTLING, and this predicate is what closes the resume race the source-order lock
	// cannot (ai-review pass 1 [critical]). It is read under the lock, and the resume writes
	// it after finalize succeeds and before it calls the provider — so an exchange that can
	// still move money says so, and refusing here is refusing to delete a binding out from
	// under a charge in flight.
	//
	// Ordered before the money check because it is the cheaper, more specific answer: an
	// operator told "a settlement is in flight" should wait and re-run, while "money moved"
	// means stop and compensate. Reporting the second for the first would send them to the
	// wrong place.
	if settlingAt.Valid && time.Since(settlingAt.Time) < SettlingGraceWindow {
		return fmt.Errorf("%w: %s (since %s)", ErrExchangeSettling, exchangeID,
			settlingAt.Time.UTC().Format(time.RFC3339))
	}
	// Money last, so the four cheaper refusals are never reported as this one.
	if moneyMoved {
		return fmt.Errorf("%w: %s", ErrExchangeMoneyMoved, exchangeID)
	}

	// The evidence row goes in FIRST. Ordering it before the delete is not cosmetic: the
	// pre-state columns are copied from the row read under the lock, and writing them while
	// the row still exists is what makes them a copy rather than a reconstruction. A
	// constraint violation here — a delta that does not equal the difference, a basis shape
	// that disagrees with itself — aborts the whole transaction with the exchange intact.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO order_exchange_unwinds
		  (id, organizer_id, exchange_id, source_order_id, reason, idempotency_key, actor,
		   pre_delta_amount, pre_target_total, pre_source_total, currency,
		   pre_basis_recorded, pre_target_hold_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		uuid.New(), org, exchangeID, sourceOrder, reason, idempotencyKey, actor,
		delta, target, sourceTotal, currency, basisAt.Valid, hold); err != nil {
		return fmt.Errorf("record unwind of %s: %w", exchangeID, err)
	}

	result, err := tx.ExecContext(ctx, `
		DELETE FROM order_exchanges
		WHERE organizer_id=$1 AND id=$2 AND settled_at IS NULL
		  AND (settling_at IS NULL OR settling_at <= now() - $3::interval)`,
		org, exchangeID, SettlingGraceWindow.String())
	if err != nil {
		return fmt.Errorf("unwind exchange %s: %w", exchangeID, err)
	}
	// Belt and braces behind the lock, exactly as UnparkOrder does: a zero-row delete after
	// a locked read that saw an unsettled row would mean the row changed under a lock that
	// should have prevented it, and committing the evidence anyway would record an
	// intervention that did not happen.
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrExchangeUnwindConflict
	}
	return tx.Commit()
}
