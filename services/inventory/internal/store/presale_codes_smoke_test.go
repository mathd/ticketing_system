//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// slotFor provisions ANOTHER slot under an EXISTING organizer.
//
// provisioned() mints a fresh org per slot, which is fine for pool-scoped tests
// and useless here: a presale code is scoped to (organizer, channel) and spans
// slots, so every cross-slot property in this file needs several pools under ONE
// org. That difference is the entire subject of
// TestPresaleCodeCapHoldsAcrossSlots.
func slotFor(t *testing.T, ctx context.Context, st *Postgres, org uuid.UUID, capacity int32) uuid.UUID {
	t.Helper()
	slot := uuid.New()
	if err := st.Provision(ctx, uuid.New(), slot, org, capacity); err != nil {
		t.Fatal(err)
	}
	return slot
}

func mustIssue(t *testing.T, ctx context.Context, st *Postgres, c PresaleCode) {
	t.Helper()
	if _, err := st.IssuePresaleCode(ctx, c); err != nil {
		t.Fatal(err)
	}
}

func capOf(n int32) *int32 { return &n }

// An UNGATED allocation ignores codes entirely — the compatibility guarantee.
//
// requires_code defaults false, so every allocation that existed before this
// migration must behave exactly as it did. This is the test that fails if the
// default flips or if code validation is ever made unconditional.
func TestUngatedAllocationIgnoresPresaleCodes(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 6}})

	// No code: sells, as it always has.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "ungated-none"); err != nil {
		t.Fatalf("ungated allocation refused a code-less hold: %v", err)
	}
	// A code nobody issued: also sells. An ungated channel does not consult the
	// code table at all, so an unknown string cannot refuse it.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "ungated-bogus",
		WithPresaleCode("NOT-A-REAL-CODE")); err != nil {
		t.Fatalf("ungated allocation refused an irrelevant code: %v", err)
	}

	// And the ignored code is NOT recorded. Persisting it would let any caller
	// write arbitrary strings into an attribution column reporting reads, on a
	// path where nothing validated them.
	var cited *string
	if err := db.QueryRowContext(ctx,
		`SELECT presale_code FROM claims WHERE idempotency_key='ungated-bogus'`).Scan(&cited); err != nil {
		t.Fatal(err)
	}
	if cited != nil {
		t.Fatalf("an ungated allocation recorded the ignored code %q", *cited)
	}
}

// A gated allocation refuses a hold with no code, and admits one with the code.
func TestGatedAllocationRequiresItsCode(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 6, RequiresCode: true}})
	mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "presale", Code: "VIP-AbC"})

	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "gated-none"); !errors.Is(err, ErrPresaleCodeInvalid) {
		t.Fatalf("gated allocation, no code: got %v want ErrPresaleCodeInvalid", err)
	}
	claim, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "gated-ok",
		WithPresaleCode("VIP-AbC"))
	if err != nil {
		t.Fatalf("gated allocation with a valid code: %v", err)
	}
	// The code travels with the hold and is recorded for attribution (COS-5).
	var cited string
	if err := db.QueryRowContext(ctx,
		`SELECT presale_code FROM claims WHERE id=$1`, claim.ID).Scan(&cited); err != nil {
		t.Fatal(err)
	}
	if cited != "VIP-AbC" {
		t.Fatalf("claim cites %q, want the exact redeemed code", cited)
	}
}

// Codes are EXACT opaque strings — no case folding, no trimming (ADR-024).
//
// A registry that normalized would disagree with the exact-match lookup, and a
// code that can be issued but never redeemed is worse than one rejected.
func TestPresaleCodesAreExactAndUnnormalized(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 20)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 20, RequiresCode: true}})
	mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "presale", Code: "VIP-AbC"})

	for _, variant := range []string{"vip-abc", "VIP-ABC", " VIP-AbC", "VIP-AbC ", "VIP-AbC\t"} {
		t.Run("refuses "+strings.ReplaceAll(variant, "\t", "\\t"), func(t *testing.T) {
			_, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "exact-"+variant,
				WithPresaleCode(variant))
			if !errors.Is(err, ErrPresaleCodeInvalid) {
				t.Fatalf("variant %q: got %v want ErrPresaleCodeInvalid — the code was normalized", variant, err)
			}
		})
	}
}

// A code issued on ANOTHER channel does not unlock this one.
//
// The channel is part of the primary key, so this needs no branch of its own —
// which is the point: a wrong-channel code cannot accidentally take a different
// path and report a different refusal.
func TestPresaleCodeDoesNotCrossChannels(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 20)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{
		{Channel: "presale", Cap: 8, RequiresCode: true},
		{Channel: "reseller", Cap: 8, RequiresCode: true},
	})
	mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "reseller", Code: "PARTNER-1"})

	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "cross-channel",
		WithPresaleCode("PARTNER-1")); !errors.Is(err, ErrPresaleCodeInvalid) {
		t.Fatalf("a reseller code unlocked presale: got %v want ErrPresaleCodeInvalid", err)
	}
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "reseller", "own-channel",
		WithPresaleCode("PARTNER-1")); err != nil {
		t.Fatalf("the code failed on its OWN channel: %v", err)
	}
}

// ⭐ THE LOAD-BEARING TEST: a code capped at N grants exactly N ACROSS SLOTS.
//
// This is the one that fails if redemption is serialized only by the pool row.
// The pool lock is `WHERE slot_id=$1` — ONE row — and every other derived count
// in this package is pool-scoped, which is precisely why that lock suffices for
// a channel cap. A presale code spans slots by design, so concurrent holds on
// DIFFERENT slots take DIFFERENT pool locks, never block each other, both read
// usage = N-1, and both succeed.
//
// A single-slot fixture — the shape TestChannelAllocationContention uses and the
// shape the plan draft proposed mirroring — takes one lock, serializes correctly,
// and PASSES while the oversell is live. It cannot construct the failing state.
// That is the epic's recurring defect, and this fixture is built specifically to
// escape it.
//
// THREE slots, not two: a bug that happens to serialize pairwise would still pass
// with two.
func TestPresaleCodeCapHoldsAcrossSlots(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, first := provisioned(t, ctx, st, 100)
	slots := []uuid.UUID{first, slotFor(t, ctx, st, org, 100), slotFor(t, ctx, st, org, 100)}
	for _, s := range slots {
		// Channel cap is generous on EVERY slot: nothing here may refuse for
		// capacity, or the test would pass for the wrong reason.
		mustReplace(t, ctx, st, org, s, []ChannelAllocation{{Channel: "presale", Cap: 100, RequiresCode: true}})
	}
	const capped = 5
	mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "presale", Code: "LIMIT-5",
		MaxRedemptions: capOf(capped)})

	var granted atomic.Int32
	var wg sync.WaitGroup
	const attempts = 30
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Round-robin across the three pools: adjacent goroutines contend on
			// DIFFERENT pool rows, which is exactly the state a single-slot
			// fixture cannot reach.
			slot := slots[i%len(slots)]
			_, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale",
				"across-"+uuid.NewString(), WithPresaleCode("LIMIT-5"))
			switch {
			case err == nil:
				granted.Add(1)
			case errors.Is(err, ErrPresaleCodeInvalid):
			default:
				t.Errorf("attempt %d: unexpected %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if got := granted.Load(); got != capped {
		t.Fatalf("a code capped at %d granted %d units across %d slots — the redemption count is "+
			"not serialized. The pool row lock does NOT cover a presale code: it is "+
			"(organizer, channel, code) and spans pools, so holds on different slots take "+
			"different locks and each reads a stale usage total.", capped, got, len(slots))
	}
}

// A code with no cap is unlimited, and says so by not counting.
func TestUncappedPresaleCodeNeverExhausts(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 50)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 50, RequiresCode: true}})
	mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "presale", Code: "OPEN"})

	for i := 0; i < 12; i++ {
		if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale",
			"uncapped-"+uuid.NewString(), WithPresaleCode("OPEN")); err != nil {
			t.Fatalf("hold %d on an uncapped code: %v", i, err)
		}
	}
}

// Redemption is DERIVED, so an expired hold gives its redemption back — with no
// sweeper (ADR-010) — once a terminal write reaches it.
//
// A counter would need decrementing on expiry, and expiry here is LAZY: nothing
// runs at the moment a hold dies. This is the test that fails if anyone
// "optimizes" the sum into a stored count.
//
// Since TKT-543 the code count keeps an expired held claim until a terminal write
// commits (presaleUsageClaims). Both holds here are on the SAME pool, and CreateHold
// sweeps its own pool inside its transaction before it counts the code: that sweep is
// the terminal write. TestPresaleAdmissionReclaimsExpiredCodeUsageAcrossPools covers an
// expired hold on another pool.
func TestExpiredHoldReturnsItsRedemption(t *testing.T) {
	// Negative TTL: every buyer hold is born expired.
	ctx, st, _ := storeForTest(t, -time.Second)
	org, slot := provisioned(t, ctx, st, 10)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 10, RequiresCode: true}})
	mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "presale", Code: "ONE-SHOT",
		MaxRedemptions: capOf(1)})

	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "expiring",
		WithPresaleCode("ONE-SHOT")); err != nil {
		t.Fatal(err)
	}
	// The first hold is already expired. The second hold's own sweep of this pool
	// writes that expiry before the code is counted, so the cap of 1 is available
	// again. Passing expires_at alone does not return it. No sweeper ran.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "after-expiry",
		WithPresaleCode("ONE-SHOT")); err != nil {
		t.Fatalf("an expired hold did not return its redemption: %v — the count is not derived", err)
	}
}

// The code's validity window is half-open [opens_at, closes_at), evaluated
// against LITERAL bounds.
//
// Not through the claim path, and not with clock_timestamp()-relative fixtures,
// for the reason TKT-238 established the hard way: clock_timestamp() ADVANCES
// WITHIN A STATEMENT, so a bound written as "exactly now" is already past when
// the predicate reads it and > and >= agree. Three mutants survived a test that
// tried. Literal bounds make the boundary expressible.
func TestPresaleCodeWindowIsHalfOpen(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	_ = st

	eval := func(t *testing.T, opens, closes string) bool {
		t.Helper()
		q := `SELECT ` + codeWindowOpen + ` FROM (SELECT ` + opens + `::timestamptz AS opens_at, ` +
			closes + `::timestamptz AS closes_at) w`
		// The shipped const reads clock_timestamp(); pin it to a literal so a bound
		// can sit exactly ON the evaluation instant.
		q = strings.ReplaceAll(q, "clock_timestamp()", "$1::timestamptz")
		var open bool
		if err := db.QueryRowContext(ctx, q, "2026-08-10T12:00:00Z").Scan(&open); err != nil {
			t.Fatal(err)
		}
		return open
	}

	const at = `'2026-08-10T12:00:00Z'`
	const before = `'2026-08-10T11:00:00Z'`
	const after = `'2026-08-10T13:00:00Z'`

	for _, tc := range []struct {
		name          string
		opens, closes string
		want          bool
	}{
		// The two a clock-relative fixture cannot express.
		{"opens_at EXACTLY at the instant is OPEN", at, "NULL", true},
		{"closes_at EXACTLY at the instant is CLOSED", "NULL", at, false},

		{"unbounded both sides", "NULL", "NULL", true},
		{"opened in the past", before, "NULL", true},
		{"opens in the future", after, "NULL", false},
		{"closed in the past", "NULL", before, false},
		{"inside a bounded window", before, after, true},
		{"after a bounded window", before, at, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := eval(t, tc.opens, tc.closes); got != tc.want {
				t.Fatalf("codeWindowOpen(opens=%s closes=%s) = %v, want %v", tc.opens, tc.closes, got, tc.want)
			}
		})
	}
}

// A code outside its window refuses, and it refuses UNIFORMLY.
func TestPresaleCodeOutsideItsWindowRefuses(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 20)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 20, RequiresCode: true}})

	future := time.Now().Add(24 * time.Hour)
	past := time.Now().Add(-24 * time.Hour)
	mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "presale", Code: "NOT-YET", OpensAt: &future})
	mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "presale", Code: "OVER", ClosesAt: &past})

	for _, code := range []string{"NOT-YET", "OVER"} {
		if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "win-"+code,
			WithPresaleCode(code)); !errors.Is(err, ErrPresaleCodeInvalid) {
			t.Fatalf("code %q outside its window: got %v want ErrPresaleCodeInvalid", code, err)
		}
	}
}

// ⭐ THE UNIFORM REFUSAL: all five causes are indistinguishable to the caller.
//
// A refusal that distinguished them is an enumeration oracle — submitting
// candidates and learning "exists but spent" versus "no such code" is how
// presales get scraped. This asserts the SENTINEL and the MESSAGE are identical,
// because a caller sees the message.
func TestEveryInvalidPresaleCodeRefusesIdentically(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	// Capacity is deliberately generous and the caps deliberately sum BELOW it:
	// every refusal in this test must be about the CODE. A fixture whose caps
	// overshoot the pool refuses for capacity and would assert the uniform message
	// against the wrong error entirely.
	org, slot := provisioned(t, ctx, st, 80)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{
		{Channel: "presale", Cap: 50, RequiresCode: true},
		{Channel: "reseller", Cap: 20, RequiresCode: true},
	})
	past := time.Now().Add(-time.Hour)
	mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "presale", Code: "SPENT", MaxRedemptions: capOf(1)})
	mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "presale", Code: "EXPIRED", ClosesAt: &past})
	mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "reseller", Code: "OTHER-CHANNEL"})
	// Spend SPENT so the exhausted case is real rather than asserted.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "spend-it",
		WithPresaleCode("SPENT")); err != nil {
		t.Fatal(err)
	}

	causes := map[string]string{
		"no code at all":          "",
		"unknown code":            "NEVER-ISSUED",
		"code of another channel": "OTHER-CHANNEL",
		"exhausted code":          "SPENT",
		"out-of-window code":      "EXPIRED",
	}
	for name, code := range causes {
		t.Run(name, func(t *testing.T) {
			_, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale",
				"uniform-"+uuid.NewString(), WithPresaleCode(code))
			if !errors.Is(err, ErrPresaleCodeInvalid) {
				t.Fatalf("got %v want ErrPresaleCodeInvalid", err)
			}
			// The MESSAGE too: errors.Is passing while the text differs would still
			// hand an attacker the distinction over the wire.
			if err.Error() != ErrPresaleCodeInvalid.Error() {
				t.Fatalf("message %q differs from the uniform %q — that is the oracle",
					err.Error(), ErrPresaleCodeInvalid.Error())
			}
		})
	}
}

// Fingerprint back-compatibility: a hold with NO presale code hashes exactly as
// it did before TKT-239.
//
// An unconditional append would rehash EVERY claim in the database, so every
// in-flight retry would stop replaying and re-execute — a system-wide double-sell
// on retry, from what looks like adding a field.
func TestFingerprintStaysByteIdenticalWithoutAPresaleCode(t *testing.T) {
	// GOLDEN LITERALS, computed OUTSIDE this package from the pre-TKT-239
	// algorithm. That is the whole point: a first version of this test compared
	// fingerprint() against fingerprint() and a mutation check proved it could not
	// fail — an unconditional `s += ":" + presaleCode` SURVIVED it, because both
	// sides of the comparison moved together. A fixture built from the function
	// under test cannot detect a change to that function (ADR-017 section 5b'
	// makes the same point about event fixtures).
	//
	// If these literals ever need updating, that is a WIRE-COMPATIBILITY BREAK, not
	// a test update: every idempotency record in every database stops replaying and
	// every in-flight retry re-executes.
	const (
		goldenBare        = "bbb9368991be6cd401b4876d78a07bbb1ebcb18be9c90abe45a98102c11a4510"
		goldenWithChannel = "27ba9d12ad92b46f7f200784cd338b843fdbc10d8ffd779578a74de579a6af23"
	)
	org := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	slot := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	tt := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	if got := fingerprint(org, slot, tt, 2, 1500, "EUR", "", "", uuid.Nil); got != goldenBare {
		t.Fatalf("the no-channel no-code fingerprint CHANGED:\n got %s\nwant %s\n"+
			"Every pre-TKT-239 idempotency record just stopped replaying — an in-flight "+
			"retry now re-executes instead, which is a double-sell.", got, goldenBare)
	}
	if got := fingerprint(org, slot, tt, 2, 1500, "EUR", "presale", "", uuid.Nil); got != goldenWithChannel {
		t.Fatalf("the channel-only fingerprint CHANGED:\n got %s\nwant %s\n"+
			"The presale code is being appended even when absent.", got, goldenWithChannel)
	}

	// And a code MUST change it: two holds sharing an idempotency key but
	// presenting different codes are different requests, and replaying one as the
	// other would grant the second the first's redemption.
	withCode := fingerprint(org, slot, tt, 2, 1500, "EUR", "presale", "VIP", uuid.Nil)
	if withCode == goldenWithChannel {
		t.Fatal("the presale code does not enter the fingerprint at all")
	}
	if other := fingerprint(org, slot, tt, 2, 1500, "EUR", "presale", "OTHER", uuid.Nil); other == withCode {
		t.Fatal("two different codes hash identically")
	}
}

// A CONFIRMED claim keeps consuming its redemption — permanently.
//
// The code count includes confirmed claims (consumingClaims did, and
// presaleUsageClaims still does since TKT-543), and that is what makes a redemption
// stick after the buyer pays. Counting only live claims would return
// every sold redemption the moment the hold converted, so a code capped at N
// would sell unbounded tickets as fast as buyers completed checkout — the failure
// would appear only AFTER payment, which is the worst place to find it.
//
// A mutation check caught this gap: swapping consumingClaims for liveClaims in
// codeRedeemedQuantity survived the whole suite, because every other test leaves
// its claims in 'held'.
func TestConfirmedClaimKeepsItsRedemptionForever(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 20)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 20, RequiresCode: true}})
	mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "presale", Code: "ONCE", MaxRedemptions: capOf(1)})

	claim, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "to-confirm",
		WithPresaleCode("ONCE"))
	if err != nil {
		t.Fatal(err)
	}
	// Confirm it: the hold becomes a sale.
	if _, err := db.ExecContext(ctx, `UPDATE claims SET status='confirmed' WHERE id=$1`, claim.ID); err != nil {
		t.Fatal(err)
	}
	if used := redeemedUnits(t, ctx, db, org, "presale", "ONCE"); used != 1 {
		t.Fatalf("a CONFIRMED claim contributes %d to the redemption count, want 1 — "+
			"the count is live-only, so every sale hands its redemption back", used)
	}
	// The cap of 1 is genuinely spent.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", "after-sale",
		WithPresaleCode("ONCE")); !errors.Is(err, ErrPresaleCodeInvalid) {
		t.Fatalf("a code capped at 1 sold twice: got %v want ErrPresaleCodeInvalid", err)
	}
}

// A draw-down MOVES a redemption; it never creates or destroys one.
//
// The first version of this ticket had the child NOT inherit the citation, on the
// reasoning that source and children would double-count. That reasoning was wrong
// and ai-review caught it: a draw-down DECREMENTS the source by exactly the drawn
// quantity (or releases it whole), so source + children always sums to the
// original. Citing both is conservative.
//
// Measured on the un-fixed build: drawing a 10-unit reservation fully down took
// usage from 10 to ZERO and a code "capped at 10" then granted 10 MORE — 20 units
// from a cap of 10. The old test asserted usage == 6 after a partial draw and
// called that correct, which is why it was green while the defect shipped.
func TestDrawDownMovesTheRedemptionAndNeverFreesIt(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "agency", Cap: 100, RequiresCode: true}})
	mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "agency", Code: "AGENCY-10",
		MaxRedemptions: capOf(10)})

	res, _, err := st.PlaceGroupReservation(ctx, org, slot, 10, "agency-x",
		time.Now().Add(time.Hour), "agency", "staff", "presale", "grp-key",
		WithPresaleCode("AGENCY-10"))
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if got := redeemedUnits(t, ctx, db, org, "agency", "AGENCY-10"); got != 10 {
		t.Fatalf("after placing 10, usage is %d, want 10", got)
	}

	// PARTIAL draw: 4 units move to a child, the source keeps 6. Total unchanged.
	if _, _, err := st.DrawDownGroupReservation(ctx, org, res.ID, uuid.Nil, slot, 4, 0, "", "staff", "d", "draw-1"); err != nil {
		t.Fatalf("partial draw-down: %v", err)
	}
	if got := redeemedUnits(t, ctx, db, org, "agency", "AGENCY-10"); got != 10 {
		t.Fatalf("usage is %d after a PARTIAL draw-down, want 10 — the drawn units are "+
			"still consumed, so the redemption must not be freed", got)
	}

	// FULL draw of the remaining 6: the source is released, the child holds it all.
	if _, _, err := st.DrawDownGroupReservation(ctx, org, res.ID, uuid.Nil, slot, 6, 0, "", "staff", "d", "draw-2"); err != nil {
		t.Fatalf("full draw-down: %v", err)
	}
	if got := redeemedUnits(t, ctx, db, org, "agency", "AGENCY-10"); got != 10 {
		t.Fatalf("usage is %d after a FULL draw-down, want 10 — a released source with "+
			"live children must not zero the redemption count", got)
	}

	// And the cap genuinely still binds.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "agency", "over",
		WithPresaleCode("AGENCY-10")); !errors.Is(err, ErrPresaleCodeInvalid) {
		t.Fatalf("a code capped at 10 granted an 11th unit after draw-down: got %v", err)
	}
}

// The compatibility boundary is PRE-TKT-239 -> shipped, and nothing else.
//
// A second-pass review argued the framed encoding broke compatibility with
// fingerprints written by this branch's own first commit. It does not, and the
// reason is worth writing down because the argument is otherwise sound: that
// commit exists only on an unmerged feature branch. It never ran against a
// database, so no stored fingerprint was ever computed with the intermediate
// encoding, and a CODE-BEARING request could not have existed before the feature
// that introduced codes.
//
// What must hold is that a code-LESS request still hashes exactly as it did
// before this ticket — which is what the golden literals above assert. Every
// claim already in a database is code-less by construction.
func TestOnlyCodeLessFingerprintsNeedBackwardCompatibility(t *testing.T) {
	org := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	slot := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	tt := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	// Same inputs, no code: byte-identical to the pre-TKT-239 algorithm. This is
	// the only class of fingerprint that can already exist in a database.
	const goldenWithChannel = "27ba9d12ad92b46f7f200784cd338b843fdbc10d8ffd779578a74de579a6af23"
	if got := fingerprint(org, slot, tt, 2, 1500, "EUR", "presale", "", uuid.Nil); got != goldenWithChannel {
		t.Fatalf("code-less fingerprint drifted: got %s want %s", got, goldenWithChannel)
	}
}

// Two holds sharing an idempotency key but presenting DIFFERENT codes are
// different requests — including when the difference hides in the delimiter.
//
// channel and presale_code are both arbitrary opaque strings that may contain
// ':'. A bare join made (channel="a", code="b:c") and (channel="a:b", code="c")
// hash identically — measured — so the second request replayed the first BEFORE
// its allocation or code was checked.
func TestFingerprintCannotBeConfusedByADelimiterInTheCode(t *testing.T) {
	org, slot, tt := uuid.New(), uuid.New(), uuid.New()
	if x, y := fingerprint(org, slot, tt, 1, 100, "EUR", "a", "b:c", uuid.Nil),
		fingerprint(org, slot, tt, 1, 100, "EUR", "a:b", "c", uuid.Nil); x == y {
		t.Fatalf("(channel=a, code=b:c) and (channel=a:b, code=c) hash identically (%s) — "+
			"one replays as the other", x)
	}
	// The plain different-code case too.
	if x, y := fingerprint(org, slot, tt, 1, 100, "EUR", "presale", "AAA", uuid.Nil),
		fingerprint(org, slot, tt, 1, 100, "EUR", "presale", "BBB", uuid.Nil); x == y {
		t.Fatal("two different codes hash identically")
	}
}

// A group placement replayed with a DIFFERENT code is a key reuse, not a replay.
func TestGroupPlacementFingerprintIncludesThePresaleCode(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "agency", Cap: 100, RequiresCode: true}})
	mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "agency", Code: "FIRST"})
	mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "agency", Code: "SECOND"})

	expires := time.Now().Add(time.Hour)
	if _, _, err := st.PlaceGroupReservation(ctx, org, slot, 5, "cp", expires, "agency",
		"staff", "r", "same-key", WithPresaleCode("FIRST")); err != nil {
		t.Fatal(err)
	}
	// Same key, same everything, DIFFERENT code: must not replay as a success.
	_, replay, err := st.PlaceGroupReservation(ctx, org, slot, 5, "cp", expires, "agency",
		"staff", "r", "same-key", WithPresaleCode("SECOND"))
	if err == nil && replay {
		t.Fatal("a placement with a DIFFERENT presale code replayed the original as a " +
			"success — the code is not in the fingerprint, so the second request never " +
			"had its code checked at all")
	}
	if !errors.Is(err, ErrIdempotency) {
		t.Fatalf("got %v, want ErrIdempotency", err)
	}
	// The identical request DOES still replay.
	if _, replay, err := st.PlaceGroupReservation(ctx, org, slot, 5, "cp", expires, "agency",
		"staff", "r", "same-key", WithPresaleCode("FIRST")); err != nil || !replay {
		t.Fatalf("the identical request did not replay: replay=%v err=%v", replay, err)
	}
}

// redeemedUnits reads the shipped derived-usage expression, not a copy.
func redeemedUnits(t *testing.T, ctx context.Context, db *sql.DB, org uuid.UUID, channel, code string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRowContext(ctx, codeRedeemedQuantity, org, channel, code).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The operator read DOES distinguish what the public refusal hides.
func TestOperatorReadDistinguishesWhatThePublicRefusalHides(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 30)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 30, RequiresCode: true}})
	past := time.Now().Add(-time.Hour)
	mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "presale", Code: "AAA-SPENT", MaxRedemptions: capOf(2)})
	mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "presale", Code: "BBB-CLOSED", ClosesAt: &past})
	mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "presale", Code: "CCC-OPEN"})
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "presale", "spend-2",
		WithPresaleCode("AAA-SPENT")); err != nil {
		t.Fatal(err)
	}

	got, err := st.PresaleCodeStatuses(ctx, org, "presale")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d codes, want 3", len(got))
	}
	byCode := map[string]PresaleCodeStatus{}
	for _, s := range got {
		byCode[s.Code] = s
	}
	if s := byCode["AAA-SPENT"]; !s.Exhausted || s.Redeemed != 2 {
		t.Fatalf("AAA-SPENT: redeemed=%d exhausted=%v, want 2/true", s.Redeemed, s.Exhausted)
	}
	if s := byCode["BBB-CLOSED"]; s.WindowOpen {
		t.Fatal("BBB-CLOSED reports its window open")
	}
	if s := byCode["CCC-OPEN"]; !s.WindowOpen || s.Exhausted {
		t.Fatalf("CCC-OPEN: windowOpen=%v exhausted=%v, want true/false", s.WindowOpen, s.Exhausted)
	}
}

// ADR-019, BOTH halves: the redemption count is scoped AND its scan is.
//
// Asserting only the number would pass against a sequential scan over every
// organizer's claims — the no-op ADR-019 exists to stop. This count runs on the
// claim hot path under TWO locks (the pool row and the code row), so an unindexed
// scan is a contention problem, not merely a slow query: it lengthens the window
// during which every other redemption of that code is blocked.
//
// The query is the shipped const, not a hand-copied reduction free to drift.
//
// FIXTURE SHAPE IS THE WHOLE DIFFICULTY, and TKT-235 paid for this lesson twice.
// Under force_generic_plan the planner cannot see the literal, so it estimates
// from `n_distinct` on organizer_id: with a handful of organizers the average one
// owns a large fraction of the table and a seq scan is genuinely correct — 5020
// rows across 2 organizers still chose one. TABLE SIZE ALONE IS NOT ENOUGH. So
// this models real multi-tenancy: many organizers each holding a small slice,
// which is also the only condition under which the missing index would hurt in
// production.
func TestRedemptionCountIsIndexBacked(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 100, RequiresCode: true}})
	mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "presale", Code: "PLANNER"})

	// 200 organizers, each with a small slice of coded claims. claims.pool_id
	// references inventory_pools, so each bulk organizer gets its own pool.
	if _, err := db.ExecContext(ctx, `
		WITH pools AS (
			INSERT INTO inventory_pools(slot_id, organizer_id, source_event_id, capacity, confirmed_quantity)
			SELECT gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 1000, 0 FROM generate_series(1, 200)
			RETURNING slot_id, organizer_id
		)
		INSERT INTO claims(id, organizer_id, pool_id, quantity, status, expires_at,
		                   idempotency_key, request_fingerprint, claim_kind, channel_code, presale_code)
		SELECT gen_random_uuid(), p.organizer_id, p.slot_id, 1, 'held', now() + interval '1 hour',
		       'bulk-' || p.slot_id || '-' || g, 'fp', 'buyer', 'presale', 'BULK-' || g
		FROM pools p, generate_series(1, 25) g`); err != nil {
		t.Fatal(err)
	}
	// The named organizer gets a slice the same size as everyone else's, so it is
	// not special to the planner.
	for i := 0; i < 25; i++ {
		if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale",
			"planner-"+uuid.NewString(), WithPresaleCode("PLANNER")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `ANALYZE claims`); err != nil {
		t.Fatal(err)
	}

	// Half one: the result is scoped — this organizer's 25, not the 5000 others.
	if got := redeemedUnits(t, ctx, db, org, "presale", "PLANNER"); got != 25 {
		t.Fatalf("redemption count = %d, want 25 — the count is not scoped", got)
	}

	// Half two: the SCAN is scoped.
	plan := explainGenericPlan(t, db, codeRedeemedQuantity, org, "presale", "PLANNER")
	// Assert the plan uses THIS index, by name — not merely "not a seq scan".
	//
	// "No Seq Scan" is too weak and a mutation check proved it: with
	// claims_presale_usage DROPPED, the planner still avoided a seq scan by using
	// the organizer+key unique index (claims_public_idempotency since TKT-246's
	// migration 0016 split it by reseller_scope), then filtered channel_code and
	// presale_code in the heap. That is O(all this organizer's claims) per
	// redemption — under two locks — and the weaker assertion called it a pass.
	//
	// A test that cannot fail when the mechanism is deleted is not evidence
	// (docs/learnings/2026-08-10-a-green-test-that-cannot-reach-the-failing-state.md).
	if !strings.Contains(plan, "claims_presale_usage") {
		t.Fatalf("the redemption count does not use claims_presale_usage:\n%s\n"+
			"It runs under the pool lock AND the code row lock, so scanning every claim "+
			"of this organizer blocks every concurrent redemption of the same code for "+
			"the duration.", plan)
	}
	if strings.Contains(plan, "Seq Scan on claims") {
		t.Fatalf("the redemption count sequentially scans claims:\n%s", plan)
	}

	// TKT-543: the cleanup's DISCOVERY query, both halves again (plan amendment A5).
	//
	// Its scope is the cleanup's scope: sweepExpired expires every due claim of any pool
	// discovery returns. So each pool below holds claims that differ from a reclaimable
	// one in exactly ONE predicate, and every predicate has a row that only it excludes.
	// Removing the organizer filter is caught HERE and nowhere else: end to end, the
	// cleanup's organizer-scoped pool lock skips a foreign organizer's pool (D9).
	otherOrg, otherOrgPool := provisioned(t, ctx, st, 100)
	reclaimA, reclaimB := slotFor(t, ctx, st, org, 100), slotFor(t, ctx, st, org, 100)
	channelPool, codePool := slotFor(t, ctx, st, org, 100), slotFor(t, ctx, st, org, 100)
	livePool, finalizingPool := slotFor(t, ctx, st, org, 100), slotFor(t, ctx, st, org, 100)
	for _, s := range []struct {
		org, pool            uuid.UUID
		channel, code        string
		status, expiresAfter string
		qty                  int32
	}{
		// Reclaimable. Two claims in reclaimB, so the per-pool quantity is a sum.
		{org, reclaimA, "presale", "PLANNER", "held", "-1 minute", 2},
		{org, reclaimB, "presale", "PLANNER", "held", "-1 minute", 3},
		{org, reclaimB, "presale", "PLANNER", "held", "-2 minutes", 4},
		// One exclusion each.
		{org, slot, "presale", "PLANNER", "held", "-1 minute", 1},                 // pool_id <> $4
		{otherOrg, otherOrgPool, "presale", "PLANNER", "held", "-1 minute", 1},    // organizer_id
		{org, channelPool, "elsewhere", "PLANNER", "held", "-1 minute", 1},        // channel_code
		{org, codePool, "presale", "NOT-PLANNER", "held", "-1 minute", 1},         // presale_code
		{org, livePool, "presale", "PLANNER", "held", "1 hour", 1},                // expires_at <= now()
		{org, finalizingPool, "presale", "PLANNER", "finalizing", "-1 minute", 1}, // status='held'
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO claims(id, organizer_id, pool_id, quantity, status, expires_at,
				idempotency_key, request_fingerprint, claim_kind, channel_code, presale_code)
			VALUES(gen_random_uuid(), $1, $2, $3, $4, now() + $5::interval, 'disc-' || gen_random_uuid(), 'fp', 'buyer', $6, $7)`,
			s.org, s.pool, s.qty, s.status, s.expiresAfter, s.channel, s.code); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `ANALYZE claims`); err != nil {
		t.Fatal(err)
	}

	rows, err := db.QueryContext(ctx, expiredPresalePoolsSQL, org, "presale", "PLANNER", slot)
	if err != nil {
		t.Fatal(err)
	}
	found := map[uuid.UUID]int64{}
	for rows.Next() {
		var pool uuid.UUID
		var qty int64
		if err = rows.Scan(&pool, &qty); err != nil {
			t.Fatal(err)
		}
		found[pool] = qty
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err = rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 || found[reclaimA] != 2 || found[reclaimB] != 7 {
		names := map[uuid.UUID]string{reclaimA: "reclaimA", reclaimB: "reclaimB", slot: "the admission's pool",
			otherOrgPool: "another organizer", channelPool: "another channel", codePool: "another code",
			livePool: "a live hold", finalizingPool: "a finalizing claim"}
		got := map[string]int64{}
		for p, q := range found {
			got[names[p]] = q
		}
		t.Fatalf("discovery found %v, want exactly map[reclaimA:2 reclaimB:7]: the cleanup would sweep "+
			"pools the request does not cite", got)
	}
	plan = explainGenericPlan(t, db, expiredPresalePoolsSQL, org, "presale", "PLANNER", slot)
	if !strings.Contains(plan, "claims_presale_usage") {
		t.Fatalf("discovery does not use claims_presale_usage:\n%s", plan)
	}
	if strings.Contains(plan, "Seq Scan on claims") {
		t.Fatalf("discovery sequentially scans claims:\n%s", plan)
	}
}

// Retrying a hold that presented a code to an UNGATED channel must REPLAY, not
// refuse (TKT-296 D1).
//
// CreateHold computed the fingerprint twice, at two points in the same function:
// the replay comparison used the raw presaleCode, while the stored value was
// computed AFTER `presaleCode = ""` cleared it for an ungated allocation. So the
// first request stored a code-less fingerprint, and an identical retry compared a
// code-BEARING one, mismatched, and got ErrIdempotency — a leaked hold plus a
// failed checkout retry, on the buyer path. PlaceGroupReservation never had this:
// it computes fp once, pre-clear.
//
// WHAT A FAILURE LOOKS LIKE IN THE RESULT SET, which is what the assertions are
// built from: a wrong comparison does not return a foreign row and does not
// duplicate one. It returns NO row and a 409, so `replay` false or a non-nil
// error is the shape to assert — not "the write was refused".
//
// MUTATION that proves this test can fail: move the fingerprint computation at
// store.go:473 back after the clear at :589-594 (or recompute the insert value
// post-clear). The retry then returns ErrIdempotency and this test goes red. It
// was observed red against unmodified store.go before the fix landed.
func TestUngatedCodeBearingHoldReplaysOnRetry(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 6}})

	const key = "ungated-retry"
	const code = "NOT-A-REAL-CODE"

	first, replayed, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", key,
		WithPresaleCode(code))
	if err != nil {
		t.Fatalf("the first code-bearing hold against an ungated channel was refused: %v", err)
	}
	if replayed {
		t.Fatal("the FIRST request reported itself a replay")
	}

	// Byte-identical retry: same key, same terms, same code.
	second, replayed, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "presale", key,
		WithPresaleCode(code))
	if err != nil {
		t.Fatalf("the retry was refused instead of replaying: %v\n"+
			"This is TKT-296 D1: the replay comparison and the stored fingerprint "+
			"disagree about whether an ignored presale code is part of the request "+
			"identity. A buyer retrying checkout gets a 409 and the first hold leaks.", err)
	}
	if !replayed {
		t.Fatal("the retry created a second claim instead of replaying the first")
	}
	if second.ID != first.ID {
		t.Fatalf("the retry returned a DIFFERENT claim: first %s, retry %s", first.ID, second.ID)
	}

	// The retry must not have created a row. Counting is the assertion that
	// distinguishes "replayed" from "quietly sold twice".
	var claims int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM claims WHERE organizer_id=$1 AND idempotency_key=$2`,
		org, key).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if claims != 1 {
		t.Fatalf("expected exactly 1 claim for the retried key, found %d", claims)
	}

	// The ignored code is still not recorded — the fix changes the fingerprint's
	// INPUT, never what is persisted.
	var cited *string
	if err := db.QueryRowContext(ctx,
		`SELECT presale_code FROM claims WHERE organizer_id=$1 AND idempotency_key=$2`,
		org, key).Scan(&cited); err != nil {
		t.Fatal(err)
	}
	if cited != nil {
		t.Fatalf("an ungated allocation recorded the ignored code %q", *cited)
	}
}

// ---------------------------------------------------------------------------
// TKT-543: the presale cap across pools.
//
// A presale code spans pools, and only the code row lock serializes its admissions
// (ADR-064 §2). The terminal write that ends a held claim's usage is pool-local and takes
// no code lock. The tests below force the schedules where those two facts meet.
//
// Every test here stays SERIAL. The handshakes find a waiter by its statement text and by
// the pid that blocks it; a parallel test could supply a second waiter with the same text.
// ---------------------------------------------------------------------------

const (
	raceChannel = "presale"
	raceCode    = "CAP"
	// transitionPoolLock is Transition's pool lock. Matched as a whole statement: the
	// cleanup's lock differs from it only by a marker and an organizer filter (D9), so a
	// match on the table name would find either one
	// (docs/learnings/2026-07-16-lock-handshakes-pin-the-exact-statement.md).
	transitionPoolLock = "SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE"
	// cleanupPoolLock is the marker on the statement that the cross-pool cleanup uses to
	// lock a foreign pool (D9). Nothing else in inventory carries it.
	cleanupPoolLock = "/* presale-cleanup pool lock */"
	// drawDownChildInsert is the start of DrawDownGroupReservation's child INSERT. The
	// INSERT in CreateHold ends its column list with reseller_scope, not staff_scope.
	drawDownChildInsert = "INSERT INTO claims(id,organizer_id,pool_id,ticket_type_id,quantity,unit_amount,currency,status,expires_at,idempotency_key,request_fingerprint,claim_kind,channel_code,presale_code,staff_scope)"
	// codeRowLock identifies redeemPresaleCode's lock on the presale_codes row.
	codeRowLock = "SELECT max_redemptions,"
)

// presaleRace is one organizer with three pools. Each pool has a gated allocation on
// raceChannel, and one code, raceCode, is capped at the given number. Pool capacity and
// channel caps are generous, so no refusal in these tests can be for capacity.
type presaleRace struct {
	ctx        context.Context
	st         *Postgres
	db         *sql.DB
	org        uuid.UUID
	p1, p2, p3 uuid.UUID
}

func newPresaleRace(t *testing.T, maxRedemptions int32) presaleRace {
	t.Helper()
	// A one-minute buyer TTL: no claim a test creates through the store expires by itself.
	// Every expiry in these tests is placed deliberately.
	ctx, st, db := storeForTest(t, time.Minute)
	org, p1 := provisioned(t, ctx, st, 50)
	f := presaleRace{ctx: ctx, st: st, db: db, org: org, p1: p1,
		p2: slotFor(t, ctx, st, org, 50), p3: slotFor(t, ctx, st, org, 50)}
	for _, p := range []uuid.UUID{f.p1, f.p2, f.p3} {
		mustReplace(t, ctx, st, org, p, []ChannelAllocation{{Channel: raceChannel, Cap: 50, RequiresCode: true}})
	}
	mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: raceChannel, Code: raceCode,
		MaxRedemptions: capOf(maxRedemptions)})
	return f
}

type holdResult struct {
	claim  Claim
	replay bool
	err    error
}

type transitionResult struct {
	claim Claim
	err   error
}

type drawResult struct {
	res ConvertResult
	err error
}

// admit is a buyer admission that presents raceCode.
func (f presaleRace) admit(ctx context.Context, pool uuid.UUID, qty int32, key string) holdResult {
	c, replay, err := f.st.CreateHold(ctx, f.org, pool, uuid.Nil, qty, 0, "", raceChannel, key,
		WithPresaleCode(raceCode))
	return holdResult{c, replay, err}
}

// hold is an admission that the test needs to succeed as fixture.
func (f presaleRace) hold(t *testing.T, pool uuid.UUID, qty int32, key string) Claim {
	t.Helper()
	r := f.admit(f.ctx, pool, qty, key)
	if r.err != nil {
		t.Fatalf("fixture hold %q on %s: %v", key, pool, r.err)
	}
	return r.claim
}

func (f presaleRace) confirm(t *testing.T, id uuid.UUID) {
	t.Helper()
	if _, err := f.st.Transition(f.ctx, f.org, id, "confirmed"); err != nil {
		t.Fatalf("confirm %s: %v", id, err)
	}
}

// spawn runs fn in a goroutine. t.Cleanup cancels its context and waits for it, so a
// failed assertion never leaves a goroutine parked on a lock while the schema is dropped.
func spawn[T any](t *testing.T, ctx context.Context, fn func(context.Context) T) <-chan T {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	out := make(chan T, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		out <- fn(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return out
}

func await[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return within 10s", what)
	}
	panic("unreachable")
}

// lockedBy opens a transaction, runs lockSQL in it and returns it with its backend pid.
// t.Cleanup rolls it back on every exit path.
func lockedBy(t *testing.T, ctx context.Context, db *sql.DB, lockSQL string, args ...any) (*sql.Tx, int) {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	var pid int
	if err = tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, lockSQL, args...); err != nil {
		t.Fatalf("blocker: %v", err)
	}
	return tx, pid
}

type lockWait struct {
	pid        int
	xactStart  time.Time
	queryStart time.Time
}

// waitingOn reports the backend that runs a statement containing stmt and waits for a
// lock that the backend holder blocks. It asserts nothing about an absence.
//
// The holder is part of the match because a QUEUED waiter is blocked by the waiter ahead
// of it, not by the transaction that holds the row: a cleanup queued behind a finalize
// reports the finalize's pid, and the blocker's pid never appears for it (TKT-543 plan
// amendment A1, observed on PostgreSQL 18.4).
func waitingOn(t *testing.T, ctx context.Context, db *sql.DB, holder int, stmt string) (lockWait, bool) {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT pid, xact_start, query_start FROM pg_stat_activity
		WHERE $1 = ANY(pg_blocking_pids(pid)) AND wait_event_type = 'Lock'
		  AND strpos(query, $2) > 0 AND pid <> pg_backend_pid()`, holder, stmt)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var found []lockWait
	for rows.Next() {
		var w lockWait
		if err = rows.Scan(&w.pid, &w.xactStart, &w.queryStart); err != nil {
			t.Fatal(err)
		}
		found = append(found, w)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(found) > 1 {
		t.Fatalf("%d backends run %q behind pid %d; the handshake cannot tell them apart", len(found), stmt, holder)
	}
	if len(found) == 0 {
		return lockWait{}, false
	}
	return found[0], true
}

func awaitWaitingOn(t *testing.T, ctx context.Context, db *sql.DB, holder int, stmt string) lockWait {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if w, ok := waitingOn(t, ctx, db, holder, stmt); ok {
			return w
		}
		if time.Now().After(deadline) {
			t.Fatalf("reach: no backend running %q was blocked by pid %d within 10s", stmt, holder)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// awaitResultOrWait returns the result as soon as it arrives (true), or returns false as
// soon as a backend running stmt is observed waiting behind holder.
func awaitResultOrWait[T any](t *testing.T, ctx context.Context, db *sql.DB, ch <-chan T, holder int, stmt string) (T, bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case v := <-ch:
			return v, true
		default:
		}
		if _, ok := waitingOn(t, ctx, db, holder, stmt); ok {
			var zero T
			return zero, false
		}
		if time.Now().After(deadline) {
			t.Fatalf("within 10s the call neither returned nor was observed waiting on %q behind pid %d", stmt, holder)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// expireNow moves a held claim's expires_at to the database's current instant. Called
// from inside a forced wait, it places the expiry after the waiting transaction began by
// construction, so no TTL races the handshake.
func expireNow(t *testing.T, ctx context.Context, db *sql.DB, id uuid.UUID) time.Time {
	t.Helper()
	var at time.Time
	if err := db.QueryRowContext(ctx,
		`UPDATE claims SET expires_at=clock_timestamp() WHERE id=$1 AND status='held' RETURNING expires_at`,
		id).Scan(&at); err != nil {
		t.Fatalf("expire %s: %v", id, err)
	}
	return at
}

// awaitDBPast waits until the DATABASE clock is past at. A boundary is crossed in the time
// base the code judges it in, never in host time (TKT-78).
func awaitDBPast(t *testing.T, ctx context.Context, db *sql.DB, at time.Time) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var past bool
		if err := db.QueryRowContext(ctx, `SELECT clock_timestamp() > $1`, at).Scan(&past); err != nil {
			t.Fatal(err)
		}
		if past {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the database clock did not pass %v within 10s", at)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func claimStatus(t *testing.T, ctx context.Context, db *sql.DB, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := db.QueryRowContext(ctx, `SELECT status FROM claims WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func expiryRecords(t *testing.T, ctx context.Context, db *sql.DB, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM claim_history WHERE claim_id=$1 AND action='expire'`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func historyRecords(t *testing.T, ctx context.Context, db *sql.DB, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM claim_history WHERE claim_id=$1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func poolClaims(t *testing.T, ctx context.Context, db *sql.DB, pool uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM claims WHERE pool_id=$1`, pool).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// confirmedCodeUnits is what the code has actually SOLD: confirmed units citing it, net of
// returns. A cap is a promise about this number.
func confirmedCodeUnits(t *testing.T, ctx context.Context, db *sql.DB, org uuid.UUID, channel, code string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(sum(quantity-returned_quantity),0)::bigint FROM claims
		WHERE organizer_id=$1 AND channel_code=$2 AND presale_code=$3 AND status='confirmed'`,
		org, channel, code).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func operatorStatus(t *testing.T, ctx context.Context, st *Postgres, org uuid.UUID, channel, code string) PresaleCodeStatus {
	t.Helper()
	all, err := st.PresaleCodeStatuses(ctx, org, channel)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range all {
		if s.Code == code {
			return s
		}
	}
	t.Fatalf("the operator read has no row for %q", code)
	return PresaleCodeStatus{}
}

// assertUniformRefusal: the refusal is the public sentinel with the public message, and
// nothing that tells the five causes apart (ADR-064 §4).
func assertUniformRefusal(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, ErrPresaleCodeInvalid) || err.Error() != ErrPresaleCodeInvalid.Error() {
		t.Fatalf("%s: got %v, want exactly the uniform %q", what, err, ErrPresaleCodeInvalid)
	}
}

// slotInvalidations records which slots the availability cache was told about.
type slotInvalidations struct {
	mu    sync.Mutex
	slots map[uuid.UUID]int
}

func (s *slotInvalidations) record(slot uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.slots == nil {
		s.slots = map[uuid.UUID]int{}
	}
	s.slots[slot]++
}

func (s *slotInvalidations) count(slot uuid.UUID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.slots[slot]
}

// ⭐ TKT-543 COS-1/2: a finalize that queued on its pool before its hold expired keeps the
// unit, so an admission on ANOTHER pool must have counted that hold.
//
// The schedule is TKT-380 probe 1, S1. A blocker holds P2's pool row. H's finalize starts
// and queues on it. H expires while the finalize waits. An admission on P1 then presents
// the same code. When the blocker goes, the finalize judges expiry on its transaction-start
// clock (Transition keeps now() on purpose, TKT-148), which predates the expiry, and
// finalizes H. With consumingClaims the admission had already left the expired H out of the
// count and admitted a second unit: TKT-380 measured 2 confirmed units for a cap of 1, in
// 6 of 6 runs.
//
// Reading R2 (ADR-084 §3) counts every held claim until a terminal write commits, so the
// admission counts H and is refused. Its cleanup then queues on P2 behind the finalize and
// finds H finalizing, so there is nothing to reclaim and the refusal stands.
//
// The schedule completes before any assertion, and every claim that survives is confirmed:
// a broken run then shows the units it would actually sell, not an early stop.
func TestPresaleCapCountsExpiredHoldWhileFinalizeWaits(t *testing.T) {
	f := newPresaleRace(t, 1)
	h := f.hold(t, f.p2, 1, "h")

	blocker, blockerPID := lockedBy(t, f.ctx, f.db, transitionPoolLock, f.p2)
	finalized := spawn(t, f.ctx, func(ctx context.Context) transitionResult {
		c, err := f.st.Transition(ctx, f.org, h.ID, "finalizing")
		return transitionResult{c, err}
	})
	fin := awaitWaitingOn(t, f.ctx, f.db, blockerPID, transitionPoolLock)
	expiry := expireNow(t, f.ctx, f.db, h.ID)
	if !fin.xactStart.Before(expiry) {
		t.Fatalf("reach: the finalize's transaction began at %v, not before H's expiry %v", fin.xactStart, expiry)
	}
	awaitDBPast(t, f.ctx, f.db, expiry)

	admitted := spawn(t, f.ctx, func(ctx context.Context) holdResult { return f.admit(ctx, f.p1, 1, "a") })
	// Release only once the admission has returned, or once its cleanup is queued behind
	// the finalize. Waiting for the admission alone would deadlock the test when the
	// cleanup waits for a pool that only the release frees.
	adm, returned := awaitResultOrWait(t, f.ctx, f.db, admitted, fin.pid, cleanupPoolLock)
	t.Logf("admission returned before the release: %v (false: its cleanup was observed queued behind the finalize)", returned)
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	fr := await(t, finalized, "the finalize")
	if !returned {
		adm = await(t, admitted, "the admission")
	}
	if fr.err == nil {
		f.confirm(t, h.ID)
	}
	if adm.err == nil {
		f.confirm(t, adm.claim.ID)
	}

	if got := confirmedCodeUnits(t, f.ctx, f.db, f.org, raceChannel, raceCode); got != 1 {
		t.Fatalf("a code capped at 1 ended with %d confirmed units (admission err=%v, finalize err=%v): "+
			"the admission on P1 left out a hold whose queued finalize then kept it", got, adm.err, fr.err)
	}
	if fr.err != nil || fr.claim.Status != "finalizing" {
		t.Fatalf("the finalize that queued before expiry: status=%q err=%v, want finalizing", fr.claim.Status, fr.err)
	}
	assertUniformRefusal(t, "the admission on P1", adm.err)
	if n := poolClaims(t, f.ctx, f.db, f.p1); n != 0 {
		t.Fatalf("the refused admission left %d claims on P1, want 0", n)
	}
	if got := redeemedUnits(t, f.ctx, f.db, f.org, raceChannel, raceCode); got != 1 {
		t.Fatalf("admission usage = %d after confirm, want 1", got)
	}
	if got := operatorStatus(t, f.ctx, f.st, f.org, raceChannel, raceCode).Redeemed; got != 1 {
		t.Fatalf("operator usage = %d after confirm, want 1", got)
	}
}

// ⭐ TKT-543 COS-2: the draw-down child path of the same race.
//
// DrawDownGroupReservation judges the source's expiry at decision time (clock_timestamp)
// and then inserts the child. If the source expires between the decision and the commit,
// and an admission on another pool runs in that gap, consumingClaims saw neither the
// expired source nor the uncommitted child, and admitted. The child then commits with the
// citation: two units for a cap of 1.
//
// The seam that holds the draw-down between its decision and its commit: an uncommitted
// staff claim with the draw-down's idempotency key. claims_staff_idempotency (migration
// 0019) is unique on (organizer_id, idempotency_key) for staff claims, so the child INSERT
// waits for that transaction. The blocker is on P3, so a foreign-key lock on P2 cannot be
// what blocks; it cites no code and writes no history, so it changes no count and is no
// replay (registryLookup reads claim_history).
//
// The source's expiry is absolute and short. It cannot be moved from inside the wait the
// way the hold above is, because the draw-down holds the source's row lock while it waits.
func TestPresaleCapCountsReservationWhileDrawDownInsertWaits(t *testing.T) {
	f := newPresaleRace(t, 1)
	src, _, err := f.st.PlaceGroupReservation(f.ctx, f.org, f.p2, 1, "agency", dbClockOffset(t, f.db, "2 seconds"),
		raceChannel, "staff", "seed", "src", WithPresaleCode(raceCode))
	if err != nil {
		t.Fatalf("place the source: %v", err)
	}

	const drawKey = "draw"
	blocker, blockerPID := lockedBy(t, f.ctx, f.db,
		`INSERT INTO claims(id,organizer_id,pool_id,quantity,status,expires_at,idempotency_key,request_fingerprint,claim_kind,reservation_counterparty,staff_scope)
		 VALUES(gen_random_uuid(),$1,$2,1,'held',now()+interval '1 hour',$3,'seam-blocker','reservation','seam-blocker',true)`,
		f.org, f.p3, drawKey)
	drawn := spawn(t, f.ctx, func(ctx context.Context) drawResult {
		r, _, err := f.st.DrawDownGroupReservation(ctx, f.org, src.ID, uuid.Nil, f.p2, 1, 0, "", "staff", "draw", drawKey)
		return drawResult{r, err}
	})
	ins := awaitWaitingOn(t, f.ctx, f.db, blockerPID, drawDownChildInsert)
	// The INSERT starts after the decision, so an INSERT that started before the expiry
	// proves the decision did too.
	if !ins.queryStart.Before(*src.ExpiresAt) {
		t.Fatalf("reach: the child INSERT started at %v, not before the source's expiry %v", ins.queryStart, *src.ExpiresAt)
	}
	awaitDBPast(t, f.ctx, f.db, *src.ExpiresAt)

	admitted := spawn(t, f.ctx, func(ctx context.Context) holdResult { return f.admit(ctx, f.p1, 1, "a") })
	adm, returned := awaitResultOrWait(t, f.ctx, f.db, admitted, ins.pid, cleanupPoolLock)
	t.Logf("admission returned before the release: %v (false: its cleanup was observed queued behind the draw-down)", returned)
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	dr := await(t, drawn, "the draw-down")
	if !returned {
		adm = await(t, admitted, "the admission")
	}
	if dr.err == nil {
		f.confirm(t, dr.res.Child.ID)
	}
	if adm.err == nil {
		f.confirm(t, adm.claim.ID)
	}

	if got := confirmedCodeUnits(t, f.ctx, f.db, f.org, raceChannel, raceCode); got != 1 {
		t.Fatalf("a code capped at 1 ended with %d confirmed units (admission err=%v, draw-down err=%v): "+
			"the admission on P1 counted neither the expired source nor the uncommitted child", got, adm.err, dr.err)
	}
	if dr.err != nil || dr.res.SourceStatus != "released" {
		t.Fatalf("the draw-down that decided before expiry: source status=%q err=%v, want released", dr.res.SourceStatus, dr.err)
	}
	if s := claimStatus(t, f.ctx, f.db, src.ID); s != "released" {
		t.Fatalf("the source is %q in the database, want released", s)
	}
	var cited sql.NullString
	if err := f.db.QueryRowContext(f.ctx, `SELECT presale_code FROM claims WHERE id=$1`, dr.res.Child.ID).Scan(&cited); err != nil {
		t.Fatal(err)
	}
	if !cited.Valid || cited.String != raceCode {
		t.Fatalf("the child cites %v, want exactly %q (ADR-064 §6)", cited, raceCode)
	}
	assertUniformRefusal(t, "the admission on P1", adm.err)
	if n := poolClaims(t, f.ctx, f.db, f.p1); n != 0 {
		t.Fatalf("the refused admission left %d claims on P1, want 0", n)
	}
	if got := redeemedUnits(t, f.ctx, f.db, f.org, raceChannel, raceCode); got != 1 {
		t.Fatalf("admission usage = %d after confirm, want 1", got)
	}
	if got := operatorStatus(t, f.ctx, f.st, f.org, raceChannel, raceCode).Redeemed; got != 1 {
		t.Fatalf("operator usage = %d after confirm, want 1", got)
	}
}

// ⭐ TKT-543 COS-3: an expired hold on a QUIET pool does not lock the code out of every
// other pool.
//
// R2 counts the expired H until a terminal write commits, and the only terminal write that
// does not need a request on P2 is the admission's own cleanup (D2). Nothing else touches
// P2 in this test. Without the cleanup a single-use code refuses its own buyer on P1 for
// as long as P2 stays quiet, which is a regression from consumingClaims.
//
// The committed expiry is the essential assertion. Under consumingClaims the admission
// succeeds while H stays held: an acceptance-only check would pass on the defective code.
//
// Archived and closed P2: nothing else sweeps those pools (store.go guards new holds on
// them), so a cleanup that refused them would pin the code for good (D9).
func TestPresaleAdmissionReclaimsExpiredCodeUsageAcrossPools(t *testing.T) {
	buyer := func(f presaleRace, key string) holdResult { return f.admit(f.ctx, f.p1, 1, key) }
	// One expiry for every call: it is part of the placement's fingerprint, so a retry
	// with a different one is a different request.
	groupExpiry := time.Now().Add(time.Hour)
	group := func(f presaleRace, key string) holdResult {
		r, replay, err := f.st.PlaceGroupReservation(f.ctx, f.org, f.p1, 1, "agency", groupExpiry,
			raceChannel, "staff", "group", key, WithPresaleCode(raceCode))
		return holdResult{Claim{ID: r.ID, Status: r.Status}, replay, err}
	}
	for _, tc := range []struct {
		name    string
		admit   func(presaleRace, string) holdResult
		p2State string
	}{
		{"buyer admission", buyer, ""},
		{"group placement", group, ""},
		{"buyer admission, P2 archived", buyer, `lifecycle_status='archived'`},
		{"buyer admission, P2 closed", buyer, `closure_status='closed'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPresaleRace(t, 1)
			h := f.hold(t, f.p2, 1, "h")
			mustAgeClaim(t, f.ctx, f.db, h.ID, "clock_timestamp() - interval '1 second'")
			if tc.p2State != "" {
				if _, err := f.db.ExecContext(f.ctx, `UPDATE inventory_pools SET `+tc.p2State+` WHERE slot_id=$1`, f.p2); err != nil {
					t.Fatal(err)
				}
			}
			var inv slotInvalidations
			f.st.RegisterAvailabilityInvalidator(inv.record)

			first := tc.admit(f, "reclaim")
			if first.err != nil {
				t.Fatalf("admission on P1 refused although the code's only usage is an expired hold on a quiet P2: %v", first.err)
			}
			if s := claimStatus(t, f.ctx, f.db, h.ID); s != "expired" {
				t.Fatalf("H is %q after the admission, want expired: no terminal write committed on P2", s)
			}
			if n := expiryRecords(t, f.ctx, f.db, h.ID); n != 1 {
				t.Fatalf("H has %d expiry records, want exactly 1", n)
			}
			if n := inv.count(f.p2); n == 0 {
				t.Fatal("the cleanup changed P2's availability and did not invalidate P2's cache entry")
			}
			if got := redeemedUnits(t, f.ctx, f.db, f.org, raceChannel, raceCode); got != 1 {
				t.Fatalf("usage = %d after the reclaiming admission, want 1 (the new claim only)", got)
			}

			again := tc.admit(f, "reclaim")
			if again.err != nil || !again.replay || again.claim.ID != first.claim.ID {
				t.Fatalf("retry of the admitted request: id=%s replay=%v err=%v, want a replay of %s",
					again.claim.ID, again.replay, again.err, first.claim.ID)
			}
			var n int
			if err := f.db.QueryRowContext(f.ctx, `SELECT count(*) FROM claims WHERE organizer_id=$1 AND idempotency_key='reclaim'`, f.org).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("%d claims carry the retried key, want 1", n)
			}
			if n := expiryRecords(t, f.ctx, f.db, h.ID); n != 1 {
				t.Fatalf("H has %d expiry records after the retry, want exactly 1", n)
			}
		})
	}
}

// TKT-543 COS-3/4: the cleanup touches only pools with an expired claim citing the
// requested organizer, channel AND code.
//
// sweepExpired expires every due claim in a pool it is given, so the scope of the cleanup
// is decided entirely by which pools discovery selects. Each poison claim below sits in its
// own pool and differs from the target in exactly one identity field.
//
// The organizer poison stays untouched even with discovery's organizer filter removed,
// because the cleanup's pool lock is scoped to the organizer and skips a pool it finds no
// row for (D9). That filter is proven by the discovery scope assertion in
// TestRedemptionCountIsIndexBacked instead (plan amendment A2).
func TestPresaleCleanupTouchesOnlyPoolsCitingTheRequestedCode(t *testing.T) {
	f := newPresaleRace(t, 1)
	h := f.hold(t, f.p2, 1, "h")
	mustAgeClaim(t, f.ctx, f.db, h.ID, "clock_timestamp() - interval '1 second'")

	type poison struct {
		name    string
		id      uuid.UUID
		history int
	}
	var poisons []poison
	seed := func(name string, org uuid.UUID, channel, code string) {
		t.Helper()
		pool := slotFor(t, f.ctx, f.st, org, 50)
		mustReplace(t, f.ctx, f.st, org, pool, []ChannelAllocation{{Channel: channel, Cap: 50, RequiresCode: true}})
		mustIssue(t, f.ctx, f.st, PresaleCode{OrganizerID: org, Channel: channel, Code: code})
		c, _, err := f.st.CreateHold(f.ctx, org, pool, uuid.Nil, 1, 0, "", channel, "poison-"+name, WithPresaleCode(code))
		if err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		mustAgeClaim(t, f.ctx, f.db, c.ID, "clock_timestamp() - interval '1 second'")
		poisons = append(poisons, poison{name, c.ID, historyRecords(t, f.ctx, f.db, c.ID)})
	}
	seed("another organizer", uuid.New(), raceChannel, raceCode)
	seed("another channel", f.org, "other-channel", raceCode)
	seed("another code", f.org, raceChannel, "OTHER-CODE")

	if r := f.admit(f.ctx, f.p1, 1, "a"); r.err != nil {
		t.Fatalf("admission on P1: %v", r.err)
	}
	if s := claimStatus(t, f.ctx, f.db, h.ID); s != "expired" {
		t.Fatalf("the target H is %q, want expired", s)
	}
	for _, p := range poisons {
		if s := claimStatus(t, f.ctx, f.db, p.id); s != "held" {
			t.Errorf("poison (%s) is %q, want held: the cleanup swept a pool the request does not cite", p.name, s)
		}
		if n := historyRecords(t, f.ctx, f.db, p.id); n != p.history {
			t.Errorf("poison (%s) has %d history records, want %d", p.name, n, p.history)
		}
	}
}

// ⭐ TKT-543 COS-3: two mirror admissions, each blocked by the other's expired claim, both
// finish and the cap holds.
//
// Cap 2. P1 and P2 each carry one expired, unswept unit of the code. A1 asks for 2 on P1,
// A2 asks for 2 on P2. Each admission sweeps its OWN pool inside its transaction, so each
// counts only the other pool's expired unit: 1 + 2 > 2 for both. Each must clean up the
// pool the other holds.
//
// The forced schedule puts both on the code row together: a blocker holds it, A1 queues on
// it, A2 queues behind A1, then the blocker goes. If an admission cleaned up while it still
// held its pool and the code, A1 would wait for P2 while A2 waits for the code that A1
// holds. Rolling back first is what makes that impossible.
//
// PostgreSQL cannot see that cycle: A1's admission transaction would be idle, waiting in
// Go for its own cleanup, not waiting on a lock. Only the cleanup's lock_timeout would end
// it, and it would end in exactly the results asserted below. So the timeout is raised past
// this test's 10s bound: a cycle then fails the test instead of passing slowly.
func TestPresaleMirrorAdmissionsReleaseLocksBeforeCleanup(t *testing.T) {
	f := newPresaleRace(t, 2)
	f.st.cleanupLockTimeout = 20 * time.Second
	e1 := f.hold(t, f.p1, 1, "e1")
	e2 := f.hold(t, f.p2, 1, "e2")
	mustAgeClaim(t, f.ctx, f.db, e1.ID, "clock_timestamp() - interval '1 second'")
	mustAgeClaim(t, f.ctx, f.db, e2.ID, "clock_timestamp() - interval '1 second'")

	blocker, blockerPID := lockedBy(t, f.ctx, f.db,
		`SELECT 1 FROM presale_codes WHERE organizer_id=$1 AND channel_code=$2 AND code=$3 FOR UPDATE`,
		f.org, raceChannel, raceCode)
	first := spawn(t, f.ctx, func(ctx context.Context) holdResult { return f.admit(ctx, f.p1, 2, "a1") })
	a1 := awaitWaitingOn(t, f.ctx, f.db, blockerPID, codeRowLock)
	second := spawn(t, f.ctx, func(ctx context.Context) holdResult { return f.admit(ctx, f.p2, 2, "a2") })
	awaitWaitingOn(t, f.ctx, f.db, a1.pid, codeRowLock)
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	r1 := await(t, first, "A1 on P1")
	r2 := await(t, second, "A2 on P2")

	for name, id := range map[string]uuid.UUID{"P1's expired unit": e1.ID, "P2's expired unit": e2.ID} {
		if s := claimStatus(t, f.ctx, f.db, id); s != "expired" {
			t.Errorf("%s is %q, want expired", name, s)
		}
	}
	var admitted, refused int
	for _, r := range []holdResult{r1, r2} {
		switch {
		case r.err == nil:
			admitted++
		case errors.Is(r.err, ErrPresaleCodeInvalid) && r.err.Error() == ErrPresaleCodeInvalid.Error():
			refused++
		default:
			t.Errorf("a mirror admission returned %v, want success or the uniform refusal", r.err)
		}
	}
	if admitted != 1 || refused != 1 {
		t.Fatalf("admitted=%d refused=%d (A1 err=%v, A2 err=%v), want exactly one of each", admitted, refused, r1.err, r2.err)
	}
	if got := redeemedUnits(t, f.ctx, f.db, f.org, raceChannel, raceCode); got != 2 {
		t.Fatalf("usage = %d, want 2: the one two-unit admission", got)
	}
}

// ⭐ TKT-543 COS-4: a claim's code usage lasts until a terminal write commits, and the two
// reads count it by their own rules.
//
// The admission count (codeRedeemedQuantity) is reading R2 (ADR-084 §3): every confirmed,
// finalizing or held claim citing the code, with no expiry test, confirmed net of refund
// returns (consumedQuantity, unchanged). The operator read (PresaleCodeStatuses) keeps
// consumingClaims (D6): it reports what remains after reclamation, so an expired held claim
// that nothing has swept yet is NOT in it.
//
// Every expected value below is derived from those two rules, not from a run. Each case
// has its own organizer, pool and code, so no case can feed another.
func TestPresaleUsageCountsUntilTerminalWrite(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	expire := func(t *testing.T, id uuid.UUID) {
		mustAgeClaim(t, ctx, db, id, "clock_timestamp() - interval '1 second'")
	}
	transition := func(t *testing.T, org, id uuid.UUID, target string) {
		t.Helper()
		if _, err := st.Transition(ctx, org, id, target); err != nil {
			t.Fatalf("%s: %v", target, err)
		}
	}
	type usage struct{ admission, operator int64 }
	for _, tc := range []struct {
		name string
		cap  int32
		// setup leaves one code-citing claim on pool and returns its id.
		setup func(t *testing.T, org, pool uuid.UUID) uuid.UUID
		// terminal is the claim's terminal write, or, for a claim that already had one
		// or can have none, a write that must change nothing.
		terminal      func(t *testing.T, org, pool, id uuid.UUID)
		before, after usage
	}{
		{
			// R2: held counts whatever its expiry -> 2. consumingClaims: an expired held
			// claim is not live -> 0. Transition's release of an expired claim commits its
			// expiry (store.go, "vacuously satisfied") -> 0 and 0.
			name: "expired unswept buyer hold", cap: 2,
			setup: func(t *testing.T, org, pool uuid.UUID) uuid.UUID {
				c, _, err := st.CreateHold(ctx, org, pool, uuid.Nil, 2, 0, "", raceChannel, "c", WithPresaleCode(raceCode))
				if err != nil {
					t.Fatal(err)
				}
				expire(t, c.ID)
				return c.ID
			},
			terminal: func(t *testing.T, org, _, id uuid.UUID) { transition(t, org, id, "released") },
			before:   usage{2, 0}, after: usage{0, 0},
		},
		{
			// The same rule for a group reservation. Its terminal write here is the
			// refused draw-down, which commits the source's expiry before it refuses.
			name: "expired unswept group reservation", cap: 3,
			setup: func(t *testing.T, org, pool uuid.UUID) uuid.UUID {
				r, _, err := st.PlaceGroupReservation(ctx, org, pool, 3, "agency", time.Now().Add(time.Hour),
					raceChannel, "staff", "seed", "r", WithPresaleCode(raceCode))
				if err != nil {
					t.Fatal(err)
				}
				expire(t, r.ID)
				return r.ID
			},
			terminal: func(t *testing.T, org, pool, id uuid.UUID) {
				t.Helper()
				if _, _, err := st.DrawDownGroupReservation(ctx, org, id, uuid.Nil, pool, 1, 0, "", "staff", "d", "d"); !errors.Is(err, ErrConflict) {
					t.Fatalf("draw-down of an expired source: got %v want ErrConflict", err)
				}
			},
			before: usage{3, 0}, after: usage{0, 0},
		},
		{
			// Finalizing counts in both reads (liveClaims counts it always). Release ends it.
			name: "finalizing", cap: 2,
			setup: func(t *testing.T, org, pool uuid.UUID) uuid.UUID {
				c, _, err := st.CreateHold(ctx, org, pool, uuid.Nil, 2, 0, "", raceChannel, "c", WithPresaleCode(raceCode))
				if err != nil {
					t.Fatal(err)
				}
				transition(t, org, c.ID, "finalizing")
				return c.ID
			},
			terminal: func(t *testing.T, org, _, id uuid.UUID) { transition(t, org, id, "released") },
			before:   usage{2, 2}, after: usage{0, 0},
		},
		{
			// Confirmed counts net of returns in both reads: 3 - 1 = 2. A confirmed claim
			// has no terminal write; a release is refused and changes nothing.
			name: "confirmed, one unit returned", cap: 3,
			setup: func(t *testing.T, org, pool uuid.UUID) uuid.UUID {
				c, _, err := st.CreateHold(ctx, org, pool, uuid.Nil, 3, 0, "", raceChannel, "c", WithPresaleCode(raceCode))
				if err != nil {
					t.Fatal(err)
				}
				transition(t, org, c.ID, "finalizing")
				transition(t, org, c.ID, "confirmed")
				if _, err := st.ReturnRefundedCapacity(ctx, org, c.ID, uuid.New(), 1); err != nil {
					t.Fatal(err)
				}
				return c.ID
			},
			terminal: func(t *testing.T, org, _, id uuid.UUID) {
				t.Helper()
				if _, err := st.Transition(ctx, org, id, "released"); !errors.Is(err, ErrConflict) {
					t.Fatalf("release of a confirmed claim: got %v want ErrConflict", err)
				}
			},
			before: usage{2, 2}, after: usage{2, 2},
		},
		{
			// Already expired by a committed write: counts in neither read, and a second
			// terminal write is satisfied without counting it back.
			name: "expired", cap: 2,
			setup: func(t *testing.T, org, pool uuid.UUID) uuid.UUID {
				c, _, err := st.CreateHold(ctx, org, pool, uuid.Nil, 2, 0, "", raceChannel, "c", WithPresaleCode(raceCode))
				if err != nil {
					t.Fatal(err)
				}
				expire(t, c.ID)
				transition(t, org, c.ID, "released")
				return c.ID
			},
			terminal: func(t *testing.T, org, _, id uuid.UUID) { transition(t, org, id, "released") },
			before:   usage{0, 0}, after: usage{0, 0},
		},
		{
			name: "released", cap: 2,
			setup: func(t *testing.T, org, pool uuid.UUID) uuid.UUID {
				c, _, err := st.CreateHold(ctx, org, pool, uuid.Nil, 2, 0, "", raceChannel, "c", WithPresaleCode(raceCode))
				if err != nil {
					t.Fatal(err)
				}
				transition(t, org, c.ID, "released")
				return c.ID
			},
			terminal: func(t *testing.T, org, _, id uuid.UUID) { transition(t, org, id, "released") },
			before:   usage{0, 0}, after: usage{0, 0},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			org, pool := provisioned(t, ctx, st, 50)
			mustReplace(t, ctx, st, org, pool, []ChannelAllocation{{Channel: raceChannel, Cap: 50, RequiresCode: true}})
			mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: raceChannel, Code: raceCode, MaxRedemptions: capOf(tc.cap)})
			id := tc.setup(t, org, pool)

			check := func(when string, want usage) {
				t.Helper()
				if got := redeemedUnits(t, ctx, db, org, raceChannel, raceCode); got != want.admission {
					t.Errorf("%s: admission usage = %d, want %d", when, got, want.admission)
				}
				s := operatorStatus(t, ctx, st, org, raceChannel, raceCode)
				remaining := max(int64(tc.cap)-want.operator, 0)
				if s.Redeemed != want.operator || s.Remaining == nil || *s.Remaining != remaining || s.Exhausted != (remaining == 0) {
					got := "nil"
					if s.Remaining != nil {
						got = fmt.Sprint(*s.Remaining)
					}
					t.Errorf("%s: operator read redeemed=%d remaining=%s exhausted=%v, want %d/%d/%v",
						when, s.Redeemed, got, s.Exhausted, want.operator, remaining, remaining == 0)
				}
			}
			check("before the terminal write", tc.before)
			tc.terminal(t, org, pool, id)
			check("after the terminal write", tc.after)
		})
	}
}

// TKT-543 D8: a cleanup that cannot get a foreign pool gives up after lock_timeout, and
// the caller sees the uniform refusal, nothing else.
//
// A returned lock-timeout error would be a 500 that only a VALID, exhausted code can
// produce: a distinguishable refusal, which ADR-064 §4 forbids. The bound keeps a refused
// request from queuing behind a hot pool for a whole on-sale. A cleanup that gives up
// costs liveness only: H keeps counting, and the next request can reclaim it.
func TestPresaleCleanupGivesUpOnALockedPoolWithTheUniformRefusal(t *testing.T) {
	f := newPresaleRace(t, 1)
	h := f.hold(t, f.p2, 1, "h")
	mustAgeClaim(t, f.ctx, f.db, h.ID, "clock_timestamp() - interval '1 second'")

	blocker, blockerPID := lockedBy(t, f.ctx, f.db, transitionPoolLock, f.p2)
	admitted := spawn(t, f.ctx, func(ctx context.Context) holdResult { return f.admit(ctx, f.p1, 1, "a") })
	adm, returned := awaitResultOrWait(t, f.ctx, f.db, admitted, blockerPID, cleanupPoolLock)
	if !returned {
		adm = await(t, admitted, "the admission whose cleanup waits on a locked P2")
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}

	assertUniformRefusal(t, "the admission whose cleanup could not lock P2", adm.err)
	if returned {
		t.Fatal("reach: the admission returned before its cleanup was observed waiting on P2")
	}
	if s := claimStatus(t, f.ctx, f.db, h.ID); s != "held" {
		t.Fatalf("H is %q, want held: the cleanup gave up, so no terminal write may have committed", s)
	}
	if n := poolClaims(t, f.ctx, f.db, f.p1); n != 0 {
		t.Fatalf("the refused admission left %d claims on P1, want 0", n)
	}
}

// TKT-543 D7: the cleanup runs only when the expired usage it could reclaim would make the
// request fit: redeemed - reclaimable + qty <= cap.
//
// Every cap refusal pays for one discovery query. Without this bound it would also pay for
// a cleanup and a second admission whenever ANY expired claim cited the code, so during an
// on-sale of a genuinely exhausted code every refused request would take the locks of the
// pools that hold those claims.
//
// P2 stays locked, and the cleanup's lock timeout is raised past this test's bound: a
// cleanup that starts is SEEN waiting on P2, rather than ending in a refusal that looks
// like the right one.
func TestPresaleCleanupRunsOnlyWhenExpiredUsageCanCoverTheShortfall(t *testing.T) {
	f := newPresaleRace(t, 3)
	f.st.cleanupLockTimeout = 20 * time.Second
	h := f.hold(t, f.p2, 1, "h")
	sold := f.hold(t, f.p3, 2, "sold")
	f.confirm(t, sold.ID)
	mustAgeClaim(t, f.ctx, f.db, h.ID, "clock_timestamp() - interval '1 second'")
	// Counted usage (R2): H's 1 on P2 + 2 confirmed on P3 = 3, the cap. Reclaimable: 1.

	blocker, blockerPID := lockedBy(t, f.ctx, f.db, transitionPoolLock, f.p2)

	// Two units: 3 - 1 + 2 = 4 > 3. A complete cleanup would still leave it refused.
	two := spawn(t, f.ctx, func(ctx context.Context) holdResult { return f.admit(ctx, f.p1, 2, "two") })
	r, returned := awaitResultOrWait(t, f.ctx, f.db, two, blockerPID, cleanupPoolLock)
	if !returned {
		t.Fatal("a request that reclaiming every expired unit could not satisfy started a cleanup, and it waits on P2")
	}
	assertUniformRefusal(t, "two units against one reclaimable", r.err)
	if s := claimStatus(t, f.ctx, f.db, h.ID); s != "held" {
		t.Fatalf("H is %q after a refusal that needed no cleanup, want held", s)
	}

	// One unit: 3 - 1 + 1 = 3 <= 3. The fixture reaches the other answer: a cleanup
	// starts and waits on P2 ...
	one := spawn(t, f.ctx, func(ctx context.Context) holdResult { return f.admit(ctx, f.p1, 1, "one") })
	if _, returned := awaitResultOrWait(t, f.ctx, f.db, one, blockerPID, cleanupPoolLock); returned {
		t.Fatal("reach: a request that reclaiming one unit would satisfy returned without starting a cleanup")
	}
	// ... and the request is admitted once P2 is free.
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	if r := await(t, one, "the one-unit admission"); r.err != nil {
		t.Fatalf("one unit against one reclaimable unit: %v, want admitted", r.err)
	}
	if s := claimStatus(t, f.ctx, f.db, h.ID); s != "expired" {
		t.Fatalf("H is %q after the reclaiming admission, want expired", s)
	}
}

// TKT-543: the cleanup runs only after the window and seller checks, on both admission
// paths.
//
// A cap refusal triggers it, and the code is judged after the window and the seller
// (window -> seller -> code -> capacity; ADR-064 §5, TKT-246). A cleanup ahead of either
// guard would let a caller who may not sell take foreign pool locks and write terminal
// states. It would also make a closed or bound channel answer slowly exactly when the code
// is exhausted: a latency oracle on a code the caller never got to present.
//
// Each request below presents an exhausted code whose expired usage sits on a LOCKED P2,
// with the cleanup's lock timeout raised past this test's bound. A cleanup that ran would
// be seen waiting there. The guard must answer first.
func TestPresaleCleanupStaysBehindTheWindowAndSellerChecks(t *testing.T) {
	f := newPresaleRace(t, 1)
	f.st.cleanupLockTimeout = 20 * time.Second
	h := f.hold(t, f.p2, 1, "h")
	mustAgeClaim(t, f.ctx, f.db, h.ID, "clock_timestamp() - interval '1 second'")
	blocker, blockerPID := lockedBy(t, f.ctx, f.db, transitionPoolLock, f.p2)

	expiry := time.Now().Add(time.Hour)
	buyer := func(ctx context.Context, key string) error { return f.admit(ctx, f.p1, 1, key).err }
	group := func(ctx context.Context, key string) error {
		_, _, err := f.st.PlaceGroupReservation(ctx, f.org, f.p1, 1, "agency", expiry, raceChannel,
			"staff", "group", key, WithPresaleCode(raceCode))
		return err
	}
	refuses := func(name string, admit func(context.Context, string) error, want error) {
		t.Helper()
		ch := spawn(t, f.ctx, func(ctx context.Context) error { return admit(ctx, name) })
		err, returned := awaitResultOrWait(t, f.ctx, f.db, ch, blockerPID, cleanupPoolLock)
		if !returned {
			t.Fatalf("%s: a cleanup started before the guard refused, and it waits on P2", name)
		}
		if !errors.Is(err, want) {
			t.Fatalf("%s: got %v, want %v", name, err, want)
		}
	}

	bindSeller(t, f.ctx, f.db, f.p1, raceChannel, uuid.New())
	refuses("buyer, not the seller", buyer, ErrUnavailable)
	refuses("group, not the seller", group, ErrUnavailable)
	if _, err := f.db.ExecContext(f.ctx, `UPDATE channel_allocations SET sold_by=NULL WHERE pool_id=$1 AND channel_code=$2`,
		f.p1, raceChannel); err != nil {
		t.Fatal(err)
	}
	setWindow(t, f.ctx, f.db, f.p1, raceChannel, "clock_timestamp() + interval '1 hour'", "NULL")
	refuses("buyer, window closed", buyer, ErrChannelWindowClosed)
	refuses("group, window closed", group, ErrChannelWindowClosed)
	if s := claimStatus(t, f.ctx, f.db, h.ID); s != "held" {
		t.Fatalf("H is %q after refusals by the window and the seller, want held", s)
	}

	// The fixture reaches the cleanup once both guards admit: an eligible request waits
	// on P2, and is admitted when P2 is free.
	setWindow(t, f.ctx, f.db, f.p1, raceChannel, "NULL", "NULL")
	eligible := spawn(t, f.ctx, func(ctx context.Context) error { return buyer(ctx, "eligible") })
	if _, returned := awaitResultOrWait(t, f.ctx, f.db, eligible, blockerPID, cleanupPoolLock); returned {
		t.Fatal("reach: an eligible request returned without starting a cleanup")
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := await(t, eligible, "the eligible admission"); err != nil {
		t.Fatalf("the eligible admission: %v, want admitted", err)
	}
}
