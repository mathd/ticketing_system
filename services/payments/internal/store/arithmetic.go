package store

import (
	"errors"
	"math"
)

// checkedAddMoney adds two non-negative minor-unit amounts, refusing an int64 overflow
// rather than wrapping.
//
// Go wraps signed overflow modulo 2^64, so an unchecked `a + b` past MaxInt64 turns
// NEGATIVE — which means a ceiling written as `total + amount > limit` does not merely
// lose precision, it inverts: the larger the amount, the more certainly it passes. That
// is why this returns an error instead of saturating at MaxInt64. A saturating add would
// keep the comparison true and hide the caller's nonsense; an error makes the caller say
// what an unrepresentable total means for it.
//
// Deliberately payments-local rather than shared. Commerce has the same idiom
// (services/commerce/internal/api/catalog_fees.go) and it stays there: that copy carries
// commerce's own error sentinel and sits beside fee-rate machinery payments has no use
// for, so hoisting it into shared/go would create a cross-service API to serve one call
// site. Should a third service need it, that is the point to reconsider.
//
// Bounded at int64, NOT at the contract's Money cap — the same distinction commerce
// records. A capture may legitimately exceed 2^53-1, so a cap-bounded add here would
// refuse honest refunds of large orders. Overflow is the failure; large is not.
var errAmountOverflow = errors.New("amount arithmetic overflows int64")

func checkedAddMoney(a, b int64) (int64, error) {
	if a < 0 || b < 0 {
		return 0, errAmountOverflow
	}
	if a > math.MaxInt64-b {
		return 0, errAmountOverflow
	}
	return a + b, nil
}
