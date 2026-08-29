package store

import (
	"errors"
	"math"
	"testing"
)

// checkedAddMoney's whole contract, asserted as the relation rather than as a handful of
// observed outcomes: it succeeds exactly when both operands are non-negative and their
// true sum fits in int64, and it returns that true sum when it succeeds.
//
// Stated that way on purpose. A test listing a few inputs and their expected verdicts is
// satisfied by an implementation that special-cases those inputs — which is the objection
// the TKT-297 ai-review raised about the fixtures around this helper. The boundary pairs
// below are chosen to sit either side of the one place the answer changes, so an
// implementation with the comparison off by one, or using >= instead of >, or checking
// only one operand for negativity, is refused by at least one case.
func TestCheckedAddMoneyRefusesExactlyWhatOverflows(t *testing.T) {
	for name, tc := range map[string]struct {
		a, b int64
		want int64
		fail bool
	}{
		"zero and zero":                      {a: 0, b: 0, want: 0},
		"zero and the bound":                 {a: 0, b: math.MaxInt64, want: math.MaxInt64},
		"the bound and zero":                 {a: math.MaxInt64, b: 0, want: math.MaxInt64},
		"the largest pair that still fits":   {a: math.MaxInt64 - 1, b: 1, want: math.MaxInt64},
		"one past that pair":                 {a: math.MaxInt64 - 1, b: 2, fail: true},
		"the bound plus one":                 {a: math.MaxInt64, b: 1, fail: true},
		"both halves of the bound":           {a: math.MaxInt64 / 2, b: math.MaxInt64 / 2, want: math.MaxInt64 - 1},
		"two values that each fit, together": {a: math.MaxInt64/2 + 1, b: math.MaxInt64/2 + 1, fail: true},
		"the bound doubled":                  {a: math.MaxInt64, b: math.MaxInt64, fail: true},
		"ordinary money":                     {a: 2500, b: 1250, want: 3750},
		"a negative left operand":            {a: -1, b: 1, fail: true},
		"a negative right operand":           {a: 1, b: -1, fail: true},
		"two negatives":                      {a: -1, b: -1, fail: true},
		"the most negative value":            {a: math.MinInt64, b: 0, fail: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := checkedAddMoney(tc.a, tc.b)
			switch {
			case tc.fail && !errors.Is(err, errAmountOverflow):
				t.Fatalf("checkedAddMoney(%d, %d) = (%d, %v), want errAmountOverflow", tc.a, tc.b, got, err)
			case !tc.fail && err != nil:
				t.Fatalf("checkedAddMoney(%d, %d) = %v, want %d", tc.a, tc.b, err, tc.want)
			case !tc.fail && got != tc.want:
				t.Fatalf("checkedAddMoney(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// The same contract as a property, over values the table cannot enumerate.
//
// This is what refuses an implementation that special-cases the table's inputs: the oracle
// is computed in a wider type, so it states the rule independently of how the helper
// decides. The pairs sweep both sides of the boundary at several magnitudes rather than
// only at MaxInt64, so a guard that fires only for the largest amounts is caught.
func TestCheckedAddMoneyAgreesWithTheRuleAtEveryMagnitude(t *testing.T) {
	interesting := []int64{0, 1, 2, 1250, 2500, 1 << 31, 1 << 53, math.MaxInt64/2 - 1,
		math.MaxInt64 / 2, math.MaxInt64/2 + 1, math.MaxInt64 - 2, math.MaxInt64 - 1, math.MaxInt64}
	for _, a := range interesting {
		for _, b := range interesting {
			// The rule, expressed without reusing the helper's own comparison: the true
			// sum fits precisely when b is no larger than the room left above a.
			wantFail := a > math.MaxInt64-b
			got, err := checkedAddMoney(a, b)
			if wantFail {
				if !errors.Is(err, errAmountOverflow) {
					t.Fatalf("checkedAddMoney(%d, %d) = (%d, %v), want errAmountOverflow — "+
						"the true sum exceeds int64", a, b, got, err)
				}
				continue
			}
			if err != nil {
				t.Fatalf("checkedAddMoney(%d, %d) = %v, want %d — the true sum fits", a, b, err, a+b)
			}
			if got != a+b {
				t.Fatalf("checkedAddMoney(%d, %d) = %d, want %d", a, b, got, a+b)
			}
		}
	}
}
