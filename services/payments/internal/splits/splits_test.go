package splits

import (
	"errors"
	"math"
	"math/big"
	"sort"
	"testing"

	"github.com/google/uuid"
)

// The allocator's one obligation: the parts sum to the input EXACTLY.
//
// The oracle below is deliberately INDEPENDENT of the implementation — it uses
// math/big to compute each exact quotient and remainder rather than the
// quotient/remainder decomposition Allocate uses. An oracle that reused the
// implementation's arithmetic would prove only that the code agrees with
// itself, which is the mistake TKT-215 shipped and a review pass caught.

func payee(n int) uuid.UUID {
	return uuid.MustParse("00000000-0000-0000-0000-0000000000" + string("0123456789abcdef"[n%16]) + "0")
}

// oracle computes the expected allocation from the DEFINITION — exact rational
// arithmetic, then largest remainder — with no shared code with Allocate.
func oracle(t *testing.T, amount int64, shares []Share) []Part {
	t.Helper()
	type row struct {
		id   uuid.UUID
		base *big.Int
		frac *big.Int
	}
	rows := make([]row, len(shares))
	for i, s := range shares {
		product := new(big.Int).Mul(big.NewInt(amount), big.NewInt(int64(s.ShareBps)))
		base, frac := new(big.Int).QuoRem(product, big.NewInt(TotalBps), new(big.Int))
		rows[i] = row{id: s.PayeeID, base: base, frac: frac}
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].id.String() < rows[b].id.String() })
	distributed := big.NewInt(0)
	for _, r := range rows {
		distributed.Add(distributed, r.base)
	}
	residue := new(big.Int).Sub(big.NewInt(amount), distributed).Int64()
	order := make([]int, len(rows))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return rows[order[a]].frac.Cmp(rows[order[b]].frac) > 0 })
	out := make([]Part, len(rows))
	for i, r := range rows {
		out[i] = Part{PayeeID: r.id, Amount: r.base.Int64()}
	}
	for i := int64(0); i < residue; i++ {
		out[order[i]].Amount++
	}
	return out
}

func assertMatchesOracle(t *testing.T, amount int64, shares []Share) {
	t.Helper()
	got, err := Allocate(amount, shares)
	if err != nil {
		t.Fatalf("Allocate(%d, %v): %v", amount, shares, err)
	}
	want := oracle(t, amount, shares)
	if len(got) != len(want) {
		t.Fatalf("got %d parts, want %d", len(got), len(want))
	}
	var sum int64
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("part %d = %+v, want %+v (amount %d)", i, got[i], want[i], amount)
		}
		if got[i].Amount < 0 {
			t.Errorf("part %d is negative: %d", i, got[i].Amount)
		}
		sum += got[i].Amount
	}
	// The headline property, asserted separately from the oracle so it survives
	// even if the oracle itself were wrong.
	if sum != amount {
		t.Errorf("parts sum to %d, want EXACTLY %d — a lost or invented cent", sum, amount)
	}
}

func shares(bps ...int32) []Share {
	out := make([]Share, len(bps))
	for i, b := range bps {
		out[i] = Share{PayeeID: payee(i), ShareBps: b}
	}
	return out
}

// The pathological cases, named rather than left to a generator — a generated
// suite that happened not to draw them would look just as green.
func TestAllocateNamedPathologicalCases(t *testing.T) {
	for name, tc := range map[string]struct {
		amount int64
		shares []Share
	}{
		// The canonical lost cent: every floor is zero.
		"one cent across three payees":   {amount: 1, shares: shares(3333, 3333, 3334)},
		"zero amount":                    {amount: 0, shares: shares(3333, 3333, 3334)},
		"an odd amount split in half":    {amount: 101, shares: shares(5000, 5000)},
		"the contract's maximum amount":  {amount: 9007199254740991, shares: shares(3333, 3333, 3334)},
		"the maximum at an odd 50/50":    {amount: 9007199254740991, shares: shares(5000, 5000)},
		"int64's ceiling":                {amount: math.MaxInt64, shares: shares(3333, 3333, 3334)},
		"a single payee takes it all":    {amount: 7, shares: shares(10000)},
		"a payee with a zero share":      {amount: 100, shares: shares(0, 10000)},
		"many payees, one cent":          {amount: 1, shares: shares(2000, 2000, 2000, 2000, 2000)},
		"a one-basis-point payee":        {amount: 9999, shares: shares(1, 9999)},
	} {
		t.Run(name, func(t *testing.T) { assertMatchesOracle(t, tc.amount, tc.shares) })
	}
}

// A zero-amount fee is still a fee (ADR-046 §2), so every payee must appear —
// with zero — rather than the set collapsing to nothing.
func TestAllocateZeroAmountStillNamesEveryPayee(t *testing.T) {
	got, err := Allocate(0, shares(3333, 3333, 3334))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d parts, want 3 — a zero fee is still owed to its payees", len(got))
	}
	for _, p := range got {
		if p.Amount != 0 {
			t.Errorf("payee %s got %d, want 0", p.PayeeID, p.Amount)
		}
	}
}

// The result must not depend on the order the caller passes shares in, or on
// which row the database returned first.
func TestAllocateIsIndependentOfInputOrder(t *testing.T) {
	base := shares(1000, 2000, 3000, 4000)
	want, err := Allocate(12345, base)
	if err != nil {
		t.Fatal(err)
	}
	for i := range base {
		rotated := append(append([]Share{}, base[i:]...), base[:i]...)
		got, err := Allocate(12345, rotated)
		if err != nil {
			t.Fatal(err)
		}
		for j := range got {
			if got[j] != want[j] {
				t.Fatalf("rotation %d changed the allocation: %+v vs %+v", i, got, want)
			}
		}
		// And the caller's slice must be untouched.
		if rotated[0] != append(append([]Share{}, base[i:]...), base[:i]...)[0] {
			t.Errorf("rotation %d: Allocate mutated the caller's slice", i)
		}
	}
}

// Equal remainders break on the payee id, so two payees who tie get a stable
// answer rather than one decided by row order.
func TestAllocateBreaksTiesOnPayeeID(t *testing.T) {
	got, err := Allocate(1, shares(5000, 5000))
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Amount != 1 || got[1].Amount != 0 {
		t.Errorf("allocation = %+v, want the cent on the LOWER payee id", got)
	}
	if got[0].PayeeID.String() > got[1].PayeeID.String() {
		t.Error("parts must be returned in payee-id order")
	}
}

// A deterministic sweep, checked against the independent oracle. Every sample
// gets a real expected value — TKT-215 shipped a sweep where 3997 of 4000
// samples asserted only "no error", which looked thorough and checked nothing.
func TestAllocateExactSumProperty(t *testing.T) {
	next := func(x *int64) int64 {
		*x = (*x*6364136223846793005 + 1442695040888963407) & math.MaxInt64
		return *x
	}
	seed := int64(1)
	for i := 0; i < 3000; i++ {
		n := int(next(&seed)%5) + 1
		// Build n shares summing to exactly 10000.
		cuts := make([]int32, 0, n-1)
		for j := 0; j < n-1; j++ {
			cuts = append(cuts, int32(next(&seed)%(TotalBps-1))+1)
		}
		sort.Slice(cuts, func(a, b int) bool { return cuts[a] < cuts[b] })
		bps := make([]int32, 0, n)
		prev := int32(0)
		for _, c := range cuts {
			bps = append(bps, c-prev)
			prev = c
		}
		bps = append(bps, TotalBps-prev)

		amount := next(&seed)
		if i%3 == 0 {
			amount %= 1000 // small amounts are where the residue actually bites
		}
		assertMatchesOracle(t, amount, shares(bps...))
	}
}

// A set the function cannot allocate over is refused, not guessed at. The
// unbalanced case matters most: the database refuses to STORE one, but TKT-217
// hands this function persisted snapshots and must not assume the row it read
// was written by the path that validates.
func TestAllocateRefusesMalformedSets(t *testing.T) {
	for name, tc := range map[string]struct {
		amount int64
		shares []Share
		want   error
	}{
		"shares below 10000":  {amount: 100, shares: shares(3333, 3333, 3333), want: ErrUnbalanced},
		"shares above 10000":  {amount: 100, shares: shares(5000, 6000), want: ErrUnbalanced},
		"no parts at all":     {amount: 100, shares: nil, want: ErrMalformedSplit},
		"a negative amount":   {amount: -1, shares: shares(10000), want: ErrMalformedSplit},
		"a negative share":    {amount: 100, shares: []Share{{PayeeID: payee(0), ShareBps: -1}, {PayeeID: payee(1), ShareBps: 10001}}, want: ErrMalformedSplit},
		"a share above 10000": {amount: 100, shares: []Share{{PayeeID: payee(0), ShareBps: 10001}, {PayeeID: payee(1), ShareBps: -1}}, want: ErrMalformedSplit},
		"a duplicate payee": {amount: 100, shares: []Share{
			{PayeeID: payee(0), ShareBps: 5000}, {PayeeID: payee(0), ShareBps: 5000}}, want: ErrMalformedSplit},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Allocate(tc.amount, tc.shares); !errors.Is(err, tc.want) {
				t.Errorf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestSumRefusesOverflow(t *testing.T) {
	if _, err := Sum([]Part{{Amount: math.MaxInt64}, {Amount: 1}}); !errors.Is(err, ErrMalformedSplit) {
		t.Error("Sum must refuse to wrap")
	}
	got, err := Sum([]Part{{Amount: 3}, {Amount: 4}})
	if err != nil || got != 7 {
		t.Errorf("Sum = %d, %v", got, err)
	}
}
