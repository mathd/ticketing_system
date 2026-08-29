package store

import (
	"errors"
	"math"
	"math/big"
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
// The oracle is big.Int arithmetic, which is genuinely independent of the helper: it forms
// the true mathematical sum and compares it with MaxInt64, rather than asking whether it
// would fit. The distinction is the whole value of this test and the first version got it
// wrong — the oracle read `a > math.MaxInt64-b`, character for character the production
// predicate, so a shared mistake in that expression would have been blessed by both sides
// agreeing (caught by the TKT-297 ai-review). The pairs sweep both sides of the boundary at
// several magnitudes rather than only at MaxInt64, so a guard that fires only for the
// largest amounts is caught.
func TestCheckedAddMoneyAgreesWithTheRuleAtEveryMagnitude(t *testing.T) {
	maxInt64 := new(big.Int).SetInt64(math.MaxInt64)
	interesting := []int64{0, 1, 2, 1250, 2500, 1 << 31, 1 << 53, math.MaxInt64/2 - 1,
		math.MaxInt64 / 2, math.MaxInt64/2 + 1, math.MaxInt64 - 2, math.MaxInt64 - 1, math.MaxInt64}
	for _, a := range interesting {
		for _, b := range interesting {
			// The true sum, formed in arbitrary precision so it cannot itself overflow,
			// then compared with the bound. No int64 arithmetic is involved in deciding
			// what the answer should be.
			sum := new(big.Int).Add(new(big.Int).SetInt64(a), new(big.Int).SetInt64(b))
			wantFail := sum.Cmp(maxInt64) > 0
			got, err := checkedAddMoney(a, b)
			if wantFail {
				if !errors.Is(err, errAmountOverflow) {
					t.Fatalf("checkedAddMoney(%d, %d) = (%d, %v), want errAmountOverflow — "+
						"the true sum exceeds int64", a, b, got, err)
				}
				continue
			}
			if err != nil {
				t.Fatalf("checkedAddMoney(%d, %d) = %v, want %s — the true sum fits", a, b, err, sum)
			}
			if !sum.IsInt64() || got != sum.Int64() {
				t.Fatalf("checkedAddMoney(%d, %d) = %d, want %s", a, b, got, sum)
			}
		}
	}
}
