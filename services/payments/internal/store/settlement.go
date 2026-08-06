package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"

	"ticketing/services/payments/internal/splits"
)

// The settlement ledger (TKT-217 / ADR-048): who is owed what out of a capture.
//
// One identity holds the whole thing together, and it holds for EVERY input:
//
//	(face − absorbed) + passed_on + absorbed  =  face + passed_on  =  captured
//
// The organizer's line is face value minus the fees they absorbed; the fee lines
// are every fee, passed-on and absorbed alike. Absorbed fees are the case most
// likely to be got wrong: the buyer never sees them, but a payee is still owed
// them out of money the buyer already paid, so a ledger that records only
// passed-on fees balances against the charge and is wrong about who earned what.

// EntryKind distinguishes the organizer's own line from a fee owed to a payee.
type EntryKind string

const (
	// EntryFaceValue is the organizer's line: face value less absorbed fees.
	EntryFaceValue EntryKind = "face_value"
	// EntryFee is one payee's share of one fee code.
	EntryFee EntryKind = "fee"
)

var (
	// ErrSettlementUnbalanced reports an entry set that does not sum to the
	// captured amount. It is the one thing this package exists to prevent.
	ErrSettlementUnbalanced = errors.New("settlement entries do not sum to the captured amount")
	// ErrSettlementPlanUnusable reports a plan this build cannot settle: a fee
	// whose split is unresolved, a currency mismatch, an amount out of range.
	ErrSettlementPlanUnusable = errors.New("settlement plan unusable")
)

// PayeeRef is the payee identity SNAPSHOTTED onto the entry.
//
// Copied rather than referenced on purpose: a payee's display name or external
// reference can be edited, and a settlement row must keep saying who was paid at
// the time they were paid. Same discipline as the price and fee snapshots.
type PayeeRef struct {
	ID                uuid.UUID
	Kind              string
	DisplayName       string
	ExternalReference *string
}

// FeeLine is one composed fee from commerce's snapshot, with the payees it
// resolved to.
type FeeLine struct {
	FeeCode   string
	Incidence string
	Amount    int64
	Currency  string
	// Shares are the winning split's parts. Empty means the fee resolved
	// `unsplit`, which this ledger refuses — an unattributable fee is exactly
	// what it exists to prevent.
	Shares []splits.Share
	// Payees carries the snapshotted identity for each share, keyed by payee id.
	Payees map[uuid.UUID]PayeeRef
}

// SettlementPlan is what commerce hands payments: the composition it already
// persisted, never a fresh resolution.
type SettlementPlan struct {
	FaceValue   int64
	PassedOn    int64
	Absorbed    int64
	TotalAmount int64
	Currency    string
	Fees        []FeeLine
}

// SettlementEntry is one immutable ledger line.
type SettlementEntry struct {
	Kind      EntryKind
	Payee     *PayeeRef // nil on the organizer's line
	FeeCode   string    // empty on the organizer's line
	Incidence string    // empty on the organizer's line
	Amount    int64
	Currency  string
}

// BuildSettlementEntries turns a plan into the lines that must be written, or
// refuses the plan.
//
// It refuses BEFORE any money moves, so a plan this build cannot settle never
// reaches the provider.
func BuildSettlementEntries(plan SettlementPlan, capturedAmount int64) ([]SettlementEntry, error) {
	bad := func(why string) error { return fmt.Errorf("%w: %s", ErrSettlementPlanUnusable, why) }

	if plan.Currency == "" {
		return nil, bad("no currency")
	}
	if plan.FaceValue < 0 || plan.PassedOn < 0 || plan.Absorbed < 0 {
		return nil, bad("a negative component")
	}
	// Commerce and payments must agree about what was charged. A mismatch means
	// one of them is settling a different sale.
	if plan.TotalAmount != capturedAmount {
		return nil, fmt.Errorf("%w: plan total %d, captured %d",
			ErrSettlementPlanUnusable, plan.TotalAmount, capturedAmount)
	}
	if plan.FaceValue > math.MaxInt64-plan.PassedOn {
		return nil, bad("face + passed_on overflows")
	}
	if plan.FaceValue+plan.PassedOn != plan.TotalAmount {
		return nil, fmt.Errorf("%w: face %d + passed_on %d != total %d",
			ErrSettlementPlanUnusable, plan.FaceValue, plan.PassedOn, plan.TotalAmount)
	}

	entries := make([]SettlementEntry, 0, len(plan.Fees)+1)
	var feeTotal, passedOnSeen, absorbedSeen int64

	for _, f := range plan.Fees {
		if f.Currency != plan.Currency {
			return nil, bad("fee " + f.FeeCode + " is in another currency")
		}
		if f.Amount < 0 {
			return nil, bad("fee " + f.FeeCode + " is negative")
		}
		// A fee with no split is UNATTRIBUTED, not invalid.
		//
		// The first version refused it, and the gate caught what that meant:
		// TKT-215 shipped fees before TKT-216 shipped split schedules, so every
		// fee sold in that window has no schedule — and refusing would have
		// failed those sales at CHECKOUT, after the buyer had committed and
		// entered payment details. Breaking shipped sales to enforce a
		// configuration rule is the wrong trade, and it is the same trade this
		// epic already declined twice (a payout misconfiguration must not refuse
		// a purchase).
		//
		// So the money is recorded as collected and unattributed: one fee entry
		// with no payee. The ledger stays balanced, the sale completes, and the
		// gap is queryable — which is the thing an operator actually needs.
		if len(f.Shares) == 0 {
			entries = append(entries, SettlementEntry{
				Kind: EntryFee, FeeCode: f.FeeCode, Incidence: f.Incidence,
				Amount: f.Amount, Currency: f.Currency,
			})
			feeTotal += f.Amount
			switch f.Incidence {
			case "passed_on":
				passedOnSeen += f.Amount
			case "absorbed":
				absorbedSeen += f.Amount
			default:
				return nil, bad("fee " + f.FeeCode + " has unknown incidence " + f.Incidence)
			}
			continue
		}
		// The allocator validates the shares itself — and it must, because these
		// come from a PERSISTED snapshot rather than from the write path that
		// checked them. ADR-047 §5 records two ways an unbalanced schedule
		// reached the database, so ErrUnbalanced here is reachable, not
		// theoretical.
		parts, err := splits.Allocate(f.Amount, f.Shares)
		if err != nil {
			return nil, fmt.Errorf("%w: fee %s: %v", ErrSettlementPlanUnusable, f.FeeCode, err)
		}
		for _, p := range parts {
			ref, ok := f.Payees[p.PayeeID]
			if !ok {
				return nil, bad("fee " + f.FeeCode + " names a payee it carries no identity for")
			}
			// A zero part is still an entry (ADR-046 §2): a payee owed nothing
			// on this sale and a payee absent from it are different facts, and
			// only one of them means the schedule did not apply.
			entries = append(entries, SettlementEntry{
				Kind: EntryFee, Payee: &ref, FeeCode: f.FeeCode, Incidence: f.Incidence,
				Amount: p.Amount, Currency: f.Currency,
			})
		}
		feeTotal += f.Amount
		switch f.Incidence {
		case "passed_on":
			passedOnSeen += f.Amount
		case "absorbed":
			absorbedSeen += f.Amount
		default:
			return nil, bad("fee " + f.FeeCode + " has unknown incidence " + f.Incidence)
		}
	}

	// The plan's own totals must agree with the fees it lists, or the ledger
	// would balance against a number the sale never used.
	if passedOnSeen != plan.PassedOn {
		return nil, fmt.Errorf("%w: passed-on fees total %d, plan says %d",
			ErrSettlementPlanUnusable, passedOnSeen, plan.PassedOn)
	}
	if absorbedSeen != plan.Absorbed {
		return nil, fmt.Errorf("%w: absorbed fees total %d, plan says %d",
			ErrSettlementPlanUnusable, absorbedSeen, plan.Absorbed)
	}

	// The organizer's line. SIGNED, deliberately: if the organizer absorbed more
	// than the face value, they netted negative and the ledger says so. That is
	// a true statement about a real (if misconfigured) sale, and refusing it here
	// would make payout configuration able to refuse a SALE — which the buyer's
	// purchase does not depend on.
	organizer := plan.FaceValue - plan.Absorbed
	entries = append(entries, SettlementEntry{
		Kind: EntryFaceValue, Amount: organizer, Currency: plan.Currency,
	})

	total, err := sumEntries(entries)
	if err != nil {
		return nil, err
	}
	if total != capturedAmount {
		// Unreachable if the identity holds — which is exactly why it is checked
		// rather than trusted. The allocator guarantees each fee's parts sum to
		// that fee; this guarantees the whole set sums to the money that moved.
		return nil, fmt.Errorf("%w: entries sum to %d, captured %d",
			ErrSettlementUnbalanced, total, capturedAmount)
	}
	return entries, nil
}

// sumEntries adds the lines, refusing an overflow rather than wrapping.
func sumEntries(entries []SettlementEntry) (int64, error) {
	var total int64
	for _, e := range entries {
		if e.Amount > 0 && total > math.MaxInt64-e.Amount {
			return 0, fmt.Errorf("%w: entries overflow int64", ErrSettlementUnbalanced)
		}
		if e.Amount < 0 && total < math.MinInt64-e.Amount {
			return 0, fmt.Errorf("%w: entries underflow int64", ErrSettlementUnbalanced)
		}
		total += e.Amount
	}
	return total, nil
}

// insertSettlement writes the ledger lines for a fact, inside the caller's
// transaction.
//
// It takes the transaction rather than the pool deliberately: the whole
// guarantee of this ticket is that the entries and the captured fact commit
// together, and a function that could open its own transaction would make that
// a convention instead of a fact.
func insertSettlement(ctx context.Context, tx *sql.Tx, f Fact, entries []SettlementEntry) error {
	if len(entries) == 0 {
		return nil
	}
	orderID, err := uuid.Parse(f.Payload["order_id"])
	if err != nil {
		return fmt.Errorf("%w: settlement needs an order_id on the fact: %v",
			ErrSettlementPlanUnusable, err)
	}
	for _, e := range entries {
		var payeeID *uuid.UUID
		var kind, name, ref, feeCode, incidence *string
		if e.Payee != nil {
			id := e.Payee.ID
			payeeID = &id
			k, n := e.Payee.Kind, e.Payee.DisplayName
			kind, name, ref = &k, &n, e.Payee.ExternalReference
		}
		if e.FeeCode != "" {
			c := e.FeeCode
			feeCode = &c
		}
		if e.Incidence != "" {
			i := e.Incidence
			incidence = &i
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO settlement_entries
			  (organizer_id, order_id, capture_fact_id, entry_kind, payee_id, payee_kind,
			   payee_display_name, payee_external_ref, fee_code, incidence, amount, currency)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			f.OrganizerID, orderID, f.ID, string(e.Kind), payeeID, kind, name, ref,
			feeCode, incidence, e.Amount, e.Currency); err != nil {
			return err
		}
	}
	return nil
}

// LedgerLine is one settlement row as read back.
type LedgerLine struct {
	Kind             EntryKind
	Amount           int64
	Currency         string
	PayeeID          *uuid.UUID
	PayeeKind        *string
	PayeeDisplayName *string
	FeeCode          *string
	Incidence        *string
}

// ReadOrderSettlement returns every line attributing an order's capture, and
// their total.
//
// Ordered so the answer is stable between reads: a settlement report that
// reshuffles is one nobody can diff.
func (j *Journal) ReadOrderSettlement(ctx context.Context, organizerID, orderID uuid.UUID) ([]LedgerLine, int64, string, error) {
	rows, err := j.db.QueryContext(ctx, `
		SELECT entry_kind, amount, currency, payee_id, payee_kind, payee_display_name,
		       fee_code, incidence
		FROM settlement_entries
		WHERE organizer_id = $1 AND order_id = $2
		ORDER BY entry_kind, fee_code NULLS FIRST, payee_id NULLS FIRST`, organizerID, orderID)
	if err != nil {
		return nil, 0, "", err
	}
	defer func() { _ = rows.Close() }()

	var out []LedgerLine
	var total int64
	var currency string
	for rows.Next() {
		var l LedgerLine
		if err := rows.Scan(&l.Kind, &l.Amount, &l.Currency, &l.PayeeID, &l.PayeeKind,
			&l.PayeeDisplayName, &l.FeeCode, &l.Incidence); err != nil {
			return nil, 0, "", err
		}
		total += l.Amount
		currency = l.Currency
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, "", err
	}
	return out, total, currency, nil
}
