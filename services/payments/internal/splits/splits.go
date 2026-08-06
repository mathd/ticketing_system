// Package splits turns one fee amount into whole-cent payee parts (TKT-216 /
// ADR-047).
//
// Shares are basis points and money is whole cents, and the two do not divide.
// A fee of 1¢ split three ways at 3333/3333/3334 floors to 0/0/0 and loses the
// cent — every real ticketing settlement has that bug once. So this package has
// exactly one obligation, and it is checkable: **the parts sum to the input
// exactly, for every input.**
package splits

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/google/uuid"
)

// Share is one payee's claim on a fee, in basis points.
type Share struct {
	PayeeID  uuid.UUID
	ShareBps int32
}

// Part is what that payee is actually owed, in minor units.
type Part struct {
	PayeeID uuid.UUID
	Amount  int64
}

var (
	// ErrUnbalanced reports shares that do not sum to exactly 10000. The
	// database refuses to store such a set, but this function is handed
	// PERSISTED snapshots by TKT-217 and must not trust that the row it is
	// reading was written by the path that validates.
	ErrUnbalanced = errors.New("split shares do not sum to 10000")
	// ErrMalformedSplit reports a set this function cannot allocate over at
	// all: no parts, a duplicate payee, a share outside 0..10000, or a
	// negative amount.
	ErrMalformedSplit = errors.New("split is malformed")
)

// TotalBps is the whole of a fee, in basis points.
const TotalBps = 10000

// Allocate distributes `amount` across `shares` so that the parts sum to
// `amount` EXACTLY.
//
// Largest remainder: floor every part, then hand the leftover cents to the
// parts with the largest fractional remainders. At most len(shares)-1 cents are
// ever left over, because each floor discards less than one cent.
//
// DETERMINISM. The result must not depend on the order the caller passes shares
// in, or on which row Postgres returned first — a settlement that pays a
// different payee depending on query plan is not one anybody can reconcile. So
// ties are broken by payee id and the output is sorted by payee id. The id is
// the only stable identity available: display names and external references are
// editable, and row order is not a fact about anything.
//
// OVERFLOW. `amount × share_bps` is NOT formed. At a large amount that product
// overflows int64 even though the quotient is perfectly representable — the
// same trap TKT-215 hit on percentage fees, where the first implementation
// multiplied first and refused legitimate inputs. The decomposition below is
// exact and its intermediates cannot overflow:
//
//	q, r  = amount / 10000, amount % 10000
//	base  = q×bps + (r×bps)/10000        // q×bps <= amount
//	frac  = (r×bps) % 10000              // r×bps <= 99,990,000
func Allocate(amount int64, shares []Share) ([]Part, error) {
	if amount < 0 {
		return nil, fmt.Errorf("%w: negative amount %d", ErrMalformedSplit, amount)
	}
	if len(shares) == 0 {
		return nil, fmt.Errorf("%w: no parts", ErrMalformedSplit)
	}

	seen := make(map[uuid.UUID]struct{}, len(shares))
	var total int64
	for _, s := range shares {
		if s.ShareBps < 0 || s.ShareBps > TotalBps {
			return nil, fmt.Errorf("%w: share_bps %d outside 0..%d",
				ErrMalformedSplit, s.ShareBps, TotalBps)
		}
		if _, dup := seen[s.PayeeID]; dup {
			// Two rows for one payee would make the answer depend on which
			// came first, and the id tie-break cannot separate them.
			return nil, fmt.Errorf("%w: payee %s appears twice", ErrMalformedSplit, s.PayeeID)
		}
		seen[s.PayeeID] = struct{}{}
		total += int64(s.ShareBps)
	}
	if total != TotalBps {
		return nil, fmt.Errorf("%w: got %d", ErrUnbalanced, total)
	}

	// Work on a copy: mutating the caller's slice would make this function's
	// determinism depend on nobody reusing the input.
	work := make([]Share, len(shares))
	copy(work, shares)
	sort.Slice(work, func(i, j int) bool {
		return work[i].PayeeID.String() < work[j].PayeeID.String()
	})

	q, r := amount/TotalBps, amount%TotalBps
	parts := make([]Part, len(work))
	fracs := make([]int64, len(work))
	var distributed int64
	for i, s := range work {
		bps := int64(s.ShareBps)
		base := q*bps + (r*bps)/TotalBps
		fracs[i] = (r * bps) % TotalBps
		parts[i] = Part{PayeeID: s.PayeeID, Amount: base}
		distributed += base
	}

	// residue < len(parts) by construction, so this loop cannot run off the
	// end — but the bound is asserted rather than assumed, because "by
	// construction" is exactly the kind of claim that stops being true when
	// somebody changes the flooring.
	residue := amount - distributed
	if residue < 0 || residue >= int64(len(parts)) {
		return nil, fmt.Errorf("%w: residue %d is outside [0,%d)", ErrMalformedSplit, residue, len(parts))
	}

	// Rank by remainder descending, then by payee id ascending. `work` is
	// already id-sorted, so a stable sort leaves equal remainders in id order.
	order := make([]int, len(parts))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return fracs[order[a]] > fracs[order[b]] })
	for i := int64(0); i < residue; i++ {
		parts[order[i]].Amount++
	}
	return parts, nil
}

// Sum adds the parts, refusing an overflow rather than wrapping. Callers use it
// to check Allocate's contract at the boundary where the money is written.
func Sum(parts []Part) (int64, error) {
	var total int64
	for _, p := range parts {
		if p.Amount < 0 {
			return 0, fmt.Errorf("%w: negative part %d", ErrMalformedSplit, p.Amount)
		}
		if total > math.MaxInt64-p.Amount {
			return 0, fmt.Errorf("%w: parts overflow int64", ErrMalformedSplit)
		}
		total += p.Amount
	}
	return total, nil
}
