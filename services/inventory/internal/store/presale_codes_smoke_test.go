//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
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
// the terminal write. TestPresaleCodeStaysBlockedByAnExpiredHoldUntilItsPoolIsSwept
// pins what happens when the expired hold is on ANOTHER pool.
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
		// The shipped const reads clock_timestamp(); pin it to a literal so a bound
		// can sit exactly ON the evaluation instant. atInstant is the derivation the
		// claim path itself uses (presaleDecision), so this judges that form.
		q := `SELECT ` + atInstant(codeWindowOpen, "$1::timestamptz") + ` FROM (SELECT ` + opens +
			`::timestamptz AS opens_at, ` + closes + `::timestamptz AS closes_at) w`
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
// in-flight retry would stop matching its stored fingerprint. A mismatch is a
// permanent ErrIdempotency for that key (ADR-010), which commerce shows as 409
// "inventory unavailable": every buyer with a retry in flight fails checkout,
// system-wide, from what looks like adding a field.
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
	// every in-flight retry is refused as a key reuse.
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
			"retry is now refused as a key reuse, so the buyer's checkout fails.", got, goldenBare)
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
// quantity, or releases it whole (a released source keeps its quantity for the
// record but no longer counts), so a committed draw-down preserves the code's
// counted usage. Citing both is conservative.
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

// When TKT-239 merged, the compatibility boundary was PRE-TKT-239 -> shipped,
// and nothing else.
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
// before this ticket — which is what the golden literals above assert.
//
// Code-bearing claims HAVE been stored since TKT-239 shipped, so the framed
// format is now a stored format too. TestFingerprintGoldenPreHashForFramedClasses
// (fingerprint_test.go) pins its bytes. This test keeps only the code-less class.
func TestOnlyCodeLessFingerprintsNeedBackwardCompatibility(t *testing.T) {
	org := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	slot := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	tt := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	// Same inputs, no code: byte-identical to the pre-TKT-239 algorithm. This
	// was the only stored class before TKT-239.
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
// TKT-543: the presale cap across pools (reading R2).
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
	// transitionPoolLock is Transition's pool lock, matched as a whole statement: a match on
	// the table name would find any pool lock
	// (docs/learnings/2026-07-16-lock-handshakes-pin-the-exact-statement.md).
	transitionPoolLock = "SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE"
	// drawDownChildInsert is the start of DrawDownGroupReservation's child INSERT. The
	// INSERT in CreateHold ends its column list with reseller_scope, not staff_scope.
	drawDownChildInsert = "INSERT INTO claims(id,organizer_id,pool_id,ticket_type_id,quantity,unit_amount,currency,status,expires_at,idempotency_key,request_fingerprint,claim_kind,channel_code,presale_code,staff_scope)"
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
// admission counts H and is refused at once, while the finalize still waits; the finalize
// then keeps H. The expected values follow from the cap: one unit is ever sold.
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

	// The admission takes no lock on P2, so it returns while the finalize still waits.
	adm := await(t, spawn(t, f.ctx, func(ctx context.Context) holdResult { return f.admit(ctx, f.p1, 1, "a") }), "the admission")
	if w, ok := waitingOn(t, f.ctx, f.db, blockerPID, transitionPoolLock); !ok || w.pid != fin.pid {
		t.Fatal("reach: the finalize no longer waits on P2 after the admission returned")
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	fr := await(t, finalized, "the finalize")
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
// citation: two units for a cap of 1. Under R2 the admission counts the expired source,
// which stays held until the draw-down commits, and is refused at once.
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
// So the schedule is a race the fixture can lose: on a loaded host, placement, the blocker
// and the draw-down can take longer than the margin, and the draw-down then decides after
// the expiry and never reaches its INSERT. That is a missed REACH condition, not a result,
// so drawDownWaitingAtItsInsert rebuilds the fixture and tries again, a bounded number of
// times. Once the schedule is reached, every cap assertion below is mandatory.
func TestPresaleCapCountsReservationWhileDrawDownInsertWaits(t *testing.T) {
	const attempts = 3
	var r drawDownSchedule
	for attempt := 1; ; attempt++ {
		var reached bool
		if r, reached = drawDownWaitingAtItsInsert(t); reached {
			break
		}
		if attempt == attempts {
			t.Fatalf("reach: in %d attempts the draw-down never waited at its child INSERT before the source's expiry", attempts)
		}
	}
	f, src, blocker, blockerPID, drawn, ins := r.f, r.src, r.blocker, r.blockerPID, r.drawn, r.ins
	awaitDBPast(t, f.ctx, f.db, *src.ExpiresAt)

	// The admission takes no lock on P2 or P3, so it returns while the draw-down still
	// waits at its child INSERT.
	adm := await(t, spawn(t, f.ctx, func(ctx context.Context) holdResult { return f.admit(ctx, f.p1, 1, "a") }), "the admission")
	if w, ok := waitingOn(t, f.ctx, f.db, blockerPID, drawDownChildInsert); !ok || w.pid != ins.pid {
		t.Fatal("reach: the draw-down no longer waits at its child INSERT after the admission returned")
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	dr := await(t, drawn, "the draw-down")
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

// drawDownMargin is how much of the source's life must be left, on the database clock, when
// the draw-down starts (see drawDownWaitingAtItsInsert).
const drawDownMargin = "1 second"

// drawDownSchedule is one fixture of the draw-down race, with the draw-down held at its
// child INSERT by the blocker.
type drawDownSchedule struct {
	f          presaleRace
	src        GroupReservation
	blocker    *sql.Tx
	blockerPID int
	drawn      <-chan drawResult
	ins        lockWait
}

// drawDownWaitingAtItsInsert builds a fresh fixture and tries to reach the schedule: the
// draw-down waits at its child INSERT, and that INSERT started before the source's expiry.
// The INSERT starts after the decision, so an INSERT that started before the expiry proves
// the decision did too. It reports false when this fixture missed; a missed fixture is
// left to its own schema and cleanups.
func drawDownWaitingAtItsInsert(t *testing.T) (drawDownSchedule, bool) {
	t.Helper()
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
	// MARGIN. The draw-down starts only while at least drawDownMargin of the source's life
	// is left on the database clock. A decision after the expiry then needs a delay longer
	// than the margin between this check and the decision, which keeps the unprovable band
	// below out of reach in practice. A short margin is a missed reach, not a result.
	var enough bool
	if err := f.db.QueryRowContext(f.ctx, `SELECT $1::timestamptz - clock_timestamp() >= $2::interval`,
		*src.ExpiresAt, drawDownMargin).Scan(&enough); err != nil {
		t.Fatal(err)
	}
	if !enough {
		t.Logf("missed: less than %s of the source's life was left when the draw-down was about to start; rebuilding", drawDownMargin)
		return drawDownSchedule{}, false
	}
	drawn := spawn(t, f.ctx, func(ctx context.Context) drawResult {
		r, _, err := f.st.DrawDownGroupReservation(ctx, f.org, src.ID, uuid.Nil, f.p2, 1, 0, "", "staff", "draw", drawKey)
		return drawResult{r, err}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if ins, ok := waitingOn(t, f.ctx, f.db, blockerPID, drawDownChildInsert); ok {
			if ins.queryStart.Before(*src.ExpiresAt) {
				return drawDownSchedule{f, src, blocker, blockerPID, drawn, ins}, true
			}
			t.Logf("missed: the child INSERT started at %v, not before the source's expiry %v; rebuilding", ins.queryStart, *src.ExpiresAt)
			return drawDownSchedule{}, false
		}
		select {
		case r := <-drawn:
			// Retry ONLY on the one early result a lost race produces: the draw-down found
			// the source DUE, committed that expiry, and refused. Anything else is a defect
			// or a broken fixture, and retrying it would hide it.
			var status string
			var updatedAt time.Time
			var expires int
			var recordedAt sql.NullTime
			if err := f.db.QueryRowContext(f.ctx, `SELECT c.status, c.updated_at,
					(SELECT count(*) FROM claim_history h WHERE h.claim_id=c.id AND h.action='expire'),
					(SELECT max(h.occurred_at) FROM claim_history h WHERE h.claim_id=c.id AND h.action='expire')
				FROM claims c WHERE c.id=$1`, src.ID).Scan(&status, &updatedAt, &expires, &recordedAt); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(r.err, ErrConflict) || status != "expired" || expires != 1 {
				t.Fatalf("the draw-down returned before its child INSERT with err=%v, source %q and %d expiry records; "+
					"only ErrConflict with a committed expiry is a missed schedule", r.err, status, expires)
			}
			// WAS the source due when the draw-down decided? The decision reads
			// clock_timestamp() in its source SELECT, which no column keeps. Two columns
			// bound it: the claim's updated_at is that transaction's now() (its start, so at
			// or before the decision), and the expiry record's occurred_at defaults to
			// clock_timestamp() at its INSERT (after the decision). Only updated_at at or
			// after expires_at PROVES the source was due; an occurred_at before expires_at
			// proves it was not.
			//
			// ASSUMPTION: the database clock does not step backwards between the draw-down's
			// transaction start and its history insert, so updated_at <= occurred_at. A
			// sample that contradicts it (updated_at at or after expires_at while occurred_at
			// is before it) proves nothing either way, and fails rather than rebuilds.
			//
			// The band between the bounds (updated_at before expires_at, occurred_at at or
			// after it) is UNPROVABLE, not a defect: the decision time is not observable, so
			// a correct late decision and a premature expiry look the same. It fails closed.
			// With the margin above, reaching it takes more than drawDownMargin to elapse
			// between the margin check and the decision, with the transaction starting before
			// the expiry: the delay can fall before BeginTx (goroutine start, the pool lookup)
			// as well as inside the transaction. RESIDUAL: such a delay fails the test although
			// production may be correct.
			exp := *src.ExpiresAt
			switch {
			case !updatedAt.Before(exp) && recordedAt.Valid && recordedAt.Time.Before(exp):
				t.Fatalf("contradictory clock sample: the draw-down began at %v, at or after the source's expiry %v, "+
					"but recorded the expiry at %v, before it; the clock stepped backwards", updatedAt, exp, recordedAt.Time)
			case !updatedAt.Before(exp):
				t.Logf("missed: the draw-down began at %v, after the source's expiry %v, committed the expiry and refused; rebuilding", updatedAt, exp)
				return drawDownSchedule{}, false
			case recordedAt.Valid && recordedAt.Time.Before(exp):
				t.Fatalf("premature expiry: the draw-down recorded the source's expiry at %v, before its expires_at %v", recordedAt.Time, exp)
			default:
				t.Fatalf("unprovable sample, not evidence of a defect: the draw-down's transaction began at %v, before the "+
					"source's expires_at %v, and recorded the expiry at %v, after it. Its decision time is not observable, so "+
					"a late decision (more than %s between the margin check and the decision) cannot be told from a premature "+
					"expiry; the test fails closed rather than retrying", updatedAt, exp, recordedAt.Time, drawDownMargin)
			}
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("reach: within 10s the draw-down neither waited at its child INSERT nor returned")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ⭐ TKT-543: a claim's code usage lasts until a terminal write commits, and the two reads
// count it by their own rules.
//
// The admission count (codeRedeemedQuantity) is reading R2 (ADR-084 §3): every confirmed,
// finalizing or held claim citing the code, with no expiry test, confirmed net of refund
// returns (consumedQuantity, unchanged). The operator read (PresaleCodeStatuses) keeps
// consumingClaims (D19): it reports current usage under that predicate and predicts
// nothing. An expired held claim that no terminal write has reached is NOT in it, so the
// admission can refuse a code that the operator read shows with headroom for as long as
// that claim stays unwritten (ADR-064 §3).
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

// TKT-543's ACCEPTED COST, pinned: an expired hold on a quiet pool keeps its code blocked on
// every other pool until a write on that pool sweeps it (ADR-064 §3, decision D18).
//
// Reading R2 counts an expired held claim until a terminal write commits, and every
// terminal write in the request path is pool-local: an admission sweeps only its own pool.
// A request-triggered cleanup of the other pools was built and rejected, because every
// effect it left was a code-enumeration oracle (ADR-064 §3). So H on a quiet P2 keeps a
// single-use code exhausted for P1, and an admission on P1 is refused. Any committed write
// on P2 that sweeps it (here a code-less hold on P2's ungated channel) writes H's expiry,
// and the admission on P1 then succeeds.
//
// TKT-547 (a background sweeper) will change the FIRST half of this test: once the sweeper
// has run, H is expired without any request on P2, and the first admission on P1 succeeds.
// Change this test with TKT-547; the first half is a documented cost, not a property to
// keep.
func TestPresaleCodeStaysBlockedByAnExpiredHoldUntilItsPoolIsSwept(t *testing.T) {
	f := newPresaleRace(t, 1)
	// P2 also sells an ungated channel, so a hold on P2 that presents no code can commit a
	// sweep of P2. 40 + 10 = 50, P2's capacity.
	mustReplace(t, f.ctx, f.st, f.org, f.p2, []ChannelAllocation{
		{Channel: raceChannel, Cap: 40, RequiresCode: true},
		{Channel: "open", Cap: 10},
	})
	h := f.hold(t, f.p2, 1, "h")
	mustAgeClaim(t, f.ctx, f.db, h.ID, "clock_timestamp() - interval '1 second'")

	// The cost: H is past its expiry, nothing has written that, and it still counts (R2).
	assertUniformRefusal(t, "an admission on P1 while H on P2 is expired but unswept", f.admit(f.ctx, f.p1, 1, "blocked").err)
	if s := claimStatus(t, f.ctx, f.db, h.ID); s != "held" {
		t.Fatalf("H is %q after the refused admission on P1, want held: nothing on P1 writes P2's claims", s)
	}
	if n := poolClaims(t, f.ctx, f.db, f.p1); n != 0 {
		t.Fatalf("the refused admission left %d claims on P1, want 0", n)
	}

	// A committed write on P2 that sweeps it.
	if _, _, err := f.st.CreateHold(f.ctx, f.org, f.p2, uuid.Nil, 1, 0, "", "open", "sweeps-p2"); err != nil {
		t.Fatalf("a code-less hold on P2: %v", err)
	}
	if s := claimStatus(t, f.ctx, f.db, h.ID); s != "expired" {
		t.Fatalf("H is %q after a committed hold on P2, want expired", s)
	}
	if n := expiryRecords(t, f.ctx, f.db, h.ID); n != 1 {
		t.Fatalf("H has %d expiry records, want exactly 1", n)
	}
	if r := f.admit(f.ctx, f.p1, 1, "after-sweep"); r.err != nil {
		t.Fatalf("an admission on P1 after P2 was swept: %v, want admitted", r.err)
	}
}

// ---------------------------------------------------------------------------
// TKT-544: the channel's window and the code's window are judged AFTER the
// code-lock wait.
//
// The code row lock can WAIT. A redemption of the same code on another pool holds
// it without updating the row, and so can an issuance. So an admission can reach
// the code-lock statement before a window boundary and get the lock after it.
// Whatever it judged before that wait is stale.
//
// Before the fix, redeemPresaleCode computed the code's window in the target list
// of the FOR UPDATE statement itself. PostgreSQL computed that value before the
// wait and did not compute it again, because the holder did not update the row
// (TKT-380 probe 1 schedule W: 6 of 6 admitted after closes_at). The channel's
// window had the same exposure one statement earlier: the caller read it under
// the pool lock, before the code-lock wait.
//
// Since D9, one statement after the lock judges the channel's window and the
// code's window and cap at ONE sampled instant, and a channel that closed during
// the wait answers the uniform ErrPresaleCodeInvalid. A distinct answer there was
// an oracle: only a code with a row can wait on it.
//
// THE SEAM IS A REAL POSTGRESQL LOCK, held by a blocker transaction that never
// writes. No production hook. A blocker that UPDATED the code row would let
// PostgreSQL check the row again and could hide the defect, so it only locks.
//
// THE HANDSHAKE PINS THE STATEMENT
// (docs/learnings/2026-07-16-lock-handshakes-pin-the-exact-statement.md): exactly
// one backend, in a Lock wait, whose blocking pids are exactly the blocker's, and
// whose statement matches codeLockStatement. It must be seen while the database
// clock is still before the boundary. If it is not, the test fails as a setup
// failure. It never skips and never passes on a refusal it did not force.
// ---------------------------------------------------------------------------

// codeLockStatement is a LIKE pattern for the code-lock statement in
// redeemPresaleCode. It names the table, the key predicate and FOR UPDATE, never
// the projection, so the old combined statement
// `SELECT max_redemptions, (window) FROM presale_codes WHERE … FOR UPDATE` matches
// it too. A mutation that restores that statement still reaches the forced
// schedule, and goes red on the outcome, not on the setup.
const codeLockStatement = `SELECT % FROM presale_codes%WHERE organizer_id=$1 AND channel_code=$2 AND code=$3 FOR UPDATE%`

// windowMargin is how far past the database clock a fixture puts the boundary.
// The handshake usually lands within tens of milliseconds; the margin is for a
// loaded machine. If it is not enough, the handshake fails loudly.
const windowMargin = "3 seconds"

// lockScheduleQty is the quantity each admission asks for: not 1, so that the
// claim-row assertion can tell the requested quantity from a default.
const lockScheduleQty = 2

// presaleAdmission is one path that REDEEMS a code: both create new consumption.
type presaleAdmission struct {
	name  string
	admit func(ctx context.Context, st *Postgres, org, slot uuid.UUID, code, key string) (uuid.UUID, bool, error)
}

var presaleAdmissions = []presaleAdmission{
	{"CreateHold", func(ctx context.Context, st *Postgres, org, slot uuid.UUID, code, key string) (uuid.UUID, bool, error) {
		c, replay, err := st.CreateHold(ctx, org, slot, uuid.Nil, lockScheduleQty, 0, "", "presale", key,
			WithPresaleCode(code))
		return c.ID, replay, err
	}},
	{"PlaceGroupReservation", func(ctx context.Context, st *Postgres, org, slot uuid.UUID, code, key string) (uuid.UUID, bool, error) {
		// A distant expiry: nothing but the window under test may refuse.
		r, replay, err := st.PlaceGroupReservation(ctx, org, slot, lockScheduleQty, "agency-544",
			time.Now().Add(24*time.Hour), "presale", "staff", "tkt-544", key, WithPresaleCode(code))
		return r.ID, replay, err
	}},
}

// holdCodeRow locks a code's row the way a concurrent redemption on another pool
// does: FOR UPDATE, never written. It is spelled unlike codeLockStatement (key
// order, projection), so the blocker cannot match the waiter's pattern.
func holdCodeRow(org uuid.UUID, channel, code string) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		var got string
		return tx.QueryRowContext(ctx,
			`SELECT code FROM presale_codes WHERE code=$1 AND channel_code=$2 AND organizer_id=$3 FOR UPDATE`,
			code, channel, org).Scan(&got)
	}
}

// holdCodeTable takes EXCLUSIVE on presale_codes. That mode conflicts with the
// ROW SHARE lock that every SELECT … FOR UPDATE on the table takes, and not with
// the ACCESS SHARE of a plain read. It is the only lock that can delay the
// code-lock statement for a code that has NO row, because a row lock needs a row.
// So it is the one schedule a valid code and an unknown code can share.
func holdCodeTable(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `LOCK TABLE presale_codes IN EXCLUSIVE MODE`)
	return err
}

type admissionOutcome struct {
	id     uuid.UUID
	replay bool
	err    error
}

// admitAcrossBoundary forces this schedule:
//
//  1. a blocker transaction takes its lock (hold) and writes nothing;
//  2. the admission starts and is observed waiting in its code-lock statement,
//     blocked by exactly the blocker, while the database clock is before boundary;
//  3. during, when it is not nil, runs while the admission waits (it must not
//     wait on the blocker itself);
//  4. the database clock passes boundary (plus 200 ms), and the admission is
//     confirmed still waiting;
//  5. the blocker rolls back, and the admission finishes.
//
// Every wait is on an observed database state, bounded by ctx. On a failure, the
// cleanup releases the blocker and waits for the admission before storeForTest
// drops the schema.
func admitAcrossBoundary(t *testing.T, ctx context.Context, db *sql.DB, boundary time.Time,
	hold func(context.Context, *sql.Tx) error, admit func() (uuid.UUID, bool, error), during func()) admissionOutcome {
	t.Helper()
	if now := dbNow(t, ctx, db); !now.Before(boundary) {
		t.Fatalf("setup: the database clock (%s) is already at the boundary (%s); the schedule cannot be forced",
			now.UTC().Format(time.RFC3339Nano), boundary.UTC().Format(time.RFC3339Nano))
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	btx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	var out admissionOutcome
	finished := make(chan struct{})
	started := false
	t.Cleanup(func() {
		// Before storeForTest's cleanups (LIFO): DROP SCHEMA would queue behind
		// the blocker, and the admission behind both.
		_ = btx.Rollback()
		_ = conn.Close()
		if started {
			<-finished // bounded by ctx's deadline
		}
	})
	var bpid int
	if err = btx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&bpid); err != nil {
		t.Fatal(err)
	}
	if err = hold(ctx, btx); err != nil {
		t.Fatalf("setup: the blocker could not take its lock: %v", err)
	}

	started = true
	go func() {
		defer close(finished)
		out.id, out.replay, out.err = admit()
	}()

	// (2) The handshake.
	var waiterPID int
	for {
		select {
		case <-finished:
			t.Fatalf("setup: the admission returned (err=%v) without waiting in the code-lock statement; "+
				"the schedule was not forced", out.err)
		default:
		}
		rows, err := db.QueryContext(ctx, `SELECT pid, query_start, clock_timestamp(), COALESCE(wait_event,''), query
			FROM pg_stat_activity
			WHERE wait_event_type='Lock' AND pg_blocking_pids(pid) = ARRAY[$1::integer] AND query LIKE $2`,
			bpid, codeLockStatement)
		if err != nil {
			t.Fatal(err)
		}
		type waiter struct {
			pid              int
			queryStart, seen time.Time
			waitEvent, query string
		}
		var ws []waiter
		for rows.Next() {
			var w waiter
			if err = rows.Scan(&w.pid, &w.queryStart, &w.seen, &w.waitEvent, &w.query); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			ws = append(ws, w)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
		if len(ws) > 1 {
			t.Fatalf("setup: %d backends wait on the blocker in a code-lock statement, want exactly 1", len(ws))
		}
		if len(ws) == 1 {
			w := ws[0]
			if !w.seen.Before(boundary) || !w.queryStart.Before(boundary) {
				t.Fatalf("setup: the admission was seen waiting only at %s (query_start %s), not before the boundary %s",
					w.seen.UTC().Format(time.RFC3339Nano), w.queryStart.UTC().Format(time.RFC3339Nano),
					boundary.UTC().Format(time.RFC3339Nano))
			}
			t.Logf("admission pid %d waits on blocker pid %d (wait_event=%s) since %s, seen at %s, boundary %s; query=%q",
				w.pid, bpid, w.waitEvent, w.queryStart.UTC().Format(time.RFC3339Nano),
				w.seen.UTC().Format(time.RFC3339Nano), boundary.UTC().Format(time.RFC3339Nano), w.query)
			waiterPID = w.pid
			break
		}
		if now := dbNow(t, ctx, db); !now.Before(boundary) {
			t.Fatalf("setup: no admission waited on blocker pid %d in a statement matching %q before the boundary %s",
				bpid, codeLockStatement, boundary.UTC().Format(time.RFC3339Nano))
		}
		time.Sleep(10 * time.Millisecond)
	}

	// (3) Other work while the admission waits.
	if during != nil {
		during()
	}

	// (4) Past the boundary by the database clock, still waiting.
	for {
		var past bool
		if err = db.QueryRowContext(ctx,
			`SELECT clock_timestamp() > $1::timestamptz + interval '200 milliseconds'`, boundary).Scan(&past); err != nil {
			t.Fatal(err)
		}
		if past {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var stillWaiting bool
	if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
		WHERE pid=$1 AND wait_event_type='Lock' AND pg_blocking_pids(pid) = ARRAY[$2::integer] AND query LIKE $3)`,
		waiterPID, bpid, codeLockStatement).Scan(&stillWaiting); err != nil {
		t.Fatal(err)
	}
	if !stillWaiting {
		t.Fatalf("setup: admission pid %d stopped waiting before the blocker released its lock", waiterPID)
	}
	t.Logf("database clock is past the boundary; the blocker rolls back without a write")

	// (5) Release and collect.
	if err = btx.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatalf("the admission did not finish after the blocker released: %v", ctx.Err())
	}
	return out
}

// keyedClaim is a claim row as persisted, read independently of every derived
// count (codeRedeemedQuantity among them, which TKT-543 changes).
type keyedClaim struct {
	id, pool      uuid.UUID
	status        string
	qty           int32
	channel, code string
}

func (c keyedClaim) String() string {
	return "{id=" + c.id.String() + " pool=" + c.pool.String() + " status=" + c.status +
		" qty=" + strconv.Itoa(int(c.qty)) + " channel=" + c.channel + " presale_code=" + c.code + "}"
}

func claimsUnderKey(t *testing.T, ctx context.Context, db *sql.DB, org uuid.UUID, key string) []keyedClaim {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT id, pool_id, status, quantity, COALESCE(channel_code,''), COALESCE(presale_code,'')
		FROM claims WHERE organizer_id=$1 AND idempotency_key=$2 ORDER BY id`, org, key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []keyedClaim
	for rows.Next() {
		var c keyedClaim
		if err = rows.Scan(&c.id, &c.pool, &c.status, &c.qty, &c.channel, &c.code); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// The CODE's window, on both bounds and both admission paths, judged after the
// code-lock wait.
//
// The code is UNCAPPED in every case, so the usage count never runs: these
// schedules are independent of codeRedeemedQuantity, which TKT-543 changes. The
// channel's window is unbounded and capacity is generous, so nothing but the
// code's window can decide.
//
// Exact equality at a bound stays with TestPresaleCodeWindowIsHalfOpen, which
// substitutes a literal clock into the shipped predicate. A wall-clock "exactly
// now" fixture cannot express it.
func TestPresaleCodeWindowAfterCodeLock(t *testing.T) {
	for _, path := range presaleAdmissions {
		t.Run(path.name, func(t *testing.T) {
			// closes_at falls inside the wait: the admission started while the code
			// was valid and gets the lock after it closed. It must be refused.
			// Failing state: one extra held claim under the attempt's key, citing
			// the code.
			t.Run("a code that closes during the wait refuses", func(t *testing.T) {
				ctx, st, db := storeForTest(t, time.Hour)
				org, slot := provisioned(t, ctx, st, 100)
				mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 100, RequiresCode: true}})
				const code, key = "CLOSING-544", "across-close"
				// Last setup step, from the database clock.
				closes := dbClockOffset(t, db, windowMargin)
				mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "presale", Code: code, ClosesAt: &closes})

				out := admitAcrossBoundary(t, ctx, db, closes, holdCodeRow(org, "presale", code),
					func() (uuid.UUID, bool, error) { return path.admit(ctx, st, org, slot, code, key) }, nil)

				rows := claimsUnderKey(t, ctx, db, org, key)
				if !errors.Is(out.err, ErrPresaleCodeInvalid) || out.replay || len(rows) != 0 {
					t.Fatalf("admission across the code's closes_at: err=%v replay=%v, claims under its key: %v; "+
						"want ErrPresaleCodeInvalid and no claim. The code window was judged before the "+
						"code-lock wait, so a code that closed during the wait still sold.",
						out.err, out.replay, rows)
				}
				if out.err.Error() != ErrPresaleCodeInvalid.Error() {
					t.Fatalf("message %q differs from the uniform %q", out.err.Error(), ErrPresaleCodeInvalid.Error())
				}
			})

			// opens_at falls inside the wait: the admission started before the code
			// was valid and gets the lock after it opened. Judged after the wait, it
			// is admitted, and exactly one claim cites the code.
			t.Run("a code that opens during the wait admits", func(t *testing.T) {
				ctx, st, db := storeForTest(t, time.Hour)
				org, slot := provisioned(t, ctx, st, 100)
				mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 100, RequiresCode: true}})
				const code, key = "OPENING-544", "across-open"
				opens := dbClockOffset(t, db, windowMargin)
				mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "presale", Code: code, OpensAt: &opens})

				out := admitAcrossBoundary(t, ctx, db, opens, holdCodeRow(org, "presale", code),
					func() (uuid.UUID, bool, error) { return path.admit(ctx, st, org, slot, code, key) }, nil)

				rows := claimsUnderKey(t, ctx, db, org, key)
				if out.err != nil || out.replay {
					t.Fatalf("admission across the code's opens_at: err=%v replay=%v, claims under its key: %v; "+
						"want it admitted. The code window was judged before the code-lock wait, when "+
						"the code had not opened yet.", out.err, out.replay, rows)
				}
				want := keyedClaim{id: out.id, pool: slot, status: "held", qty: lockScheduleQty, channel: "presale", code: code}
				if len(rows) != 1 || rows[0] != want {
					t.Fatalf("claims under the admitted key: %v, want exactly %v", rows, want)
				}
			})
		})
	}
}

// The CHANNEL's window, judged again after the code-lock wait (TKT-544 D3, D9).
//
// The caller reads the channel's window under the pool lock, BEFORE
// redeemPresaleCode waits on the code. A channel that closes during that wait
// must still refuse: ADR-054's "nobody can buy outside the window".
//
// It refuses with the UNIFORM ErrPresaleCodeInvalid, not ErrChannelWindowClosed
// (D9). Only a code that has a row can wait on it, so a distinct answer after the
// wait would tell a caller that the code exists (TestPresaleRefusalIsUniform-
// AcrossTheCodeLockWait). A channel that is closed at the caller's check, before
// the code step, still answers channel_window_closed.
//
// A valid code and an unknown code get the same answer under the same schedule.
// A row lock cannot delay an unknown code (it has no row), so the shared schedule
// locks the TABLE (holdCodeTable). The row-lock case stays as well, because it is
// the schedule production can produce: another redemption of the same code.
func TestChannelWindowAfterCodeLock(t *testing.T) {
	const validCode = "VALID-544"
	for _, path := range presaleAdmissions {
		t.Run(path.name, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				code string
				hold func(org uuid.UUID) func(context.Context, *sql.Tx) error
			}{
				{"a redemption on another pool holds the code row, valid code", validCode,
					func(org uuid.UUID) func(context.Context, *sql.Tx) error {
						return holdCodeRow(org, "presale", validCode)
					}},
				{"the code table is locked, valid code", validCode,
					func(uuid.UUID) func(context.Context, *sql.Tx) error { return holdCodeTable }},
				{"the code table is locked, unknown code", "NEVER-ISSUED-544",
					func(uuid.UUID) func(context.Context, *sql.Tx) error { return holdCodeTable }},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ctx, st, db := storeForTest(t, time.Hour)
					org, slot := provisioned(t, ctx, st, 100)
					// Uncapped, unbounded code: it cannot refuse on its own terms.
					mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "presale", Code: validCode})
					const key = "channel-across-close"
					// Last setup step, from the database clock.
					closes := dbClockOffset(t, db, windowMargin)
					mustReplace(t, ctx, st, org, slot, []ChannelAllocation{
						{Channel: "presale", Cap: 100, RequiresCode: true, ClosesAt: &closes}})

					out := admitAcrossBoundary(t, ctx, db, closes, tc.hold(org),
						func() (uuid.UUID, bool, error) { return path.admit(ctx, st, org, slot, tc.code, key) }, nil)

					rows := claimsUnderKey(t, ctx, db, org, key)
					if !errors.Is(out.err, ErrPresaleCodeInvalid) || out.replay || len(rows) != 0 {
						t.Fatalf("admission with code %q across the CHANNEL's closes_at: err=%v replay=%v, "+
							"claims under its key: %v; want the uniform ErrPresaleCodeInvalid and no claim. "+
							"A claim means the channel window was judged only before the code-lock wait; "+
							"ErrChannelWindowClosed means the answer after the wait is distinct (D9).",
							tc.code, out.err, out.replay, rows)
					}
					if out.err.Error() != ErrPresaleCodeInvalid.Error() {
						t.Fatalf("message %q differs from the uniform %q", out.err.Error(), ErrPresaleCodeInvalid.Error())
					}
				})
			}
		})
	}
}

// ONE clock sample decides the channel's window AND the code's window (TKT-544 D9).
//
// Two half-open windows that abut — the channel closes at T, the code opens at T —
// must never both admit. Judged at one instant they cannot: before T the code is
// not open, from T on the channel is closed. Judged at two instants (one for the
// channel, a later one for the code) they can, if T falls between the samples.
//
// At the admission tier that gap cannot be forced. The two samples are adjacent
// statements of one transaction, and no lock can sit between them: the admission
// already holds ROW SHARE on presale_codes and ACCESS SHARE on channel_allocations,
// so no blocker can take a lock that a plain read would wait on. So this test sits
// at the tier that DECIDES: it calls redeemPresaleCode in a transaction whose
// clock_timestamp() is a test-only function in this schema, ahead of pg_catalog on
// the search_path (executed on PostgreSQL 18.4: pg_catalog is searched first
// unless it is named later in the path). Each call returns the next instant of a
// fixed sequence, one hour apart, starting at 11:30 — so every sample is visible
// and the "gap" is an hour wide.
//
// No production hook: the production SQL is unchanged and unaware of the test.
// The function is VOLATILE, the same class as clock_timestamp(), so PostgreSQL
// evaluates it the way it evaluates the real one.
//
// Both orientations of the abutting pair are judged, because a two-sample
// statement admits only when the earlier sample goes to the window that closes.
// A control case, with both windows open at the first sample, proves the fixture
// can admit at all.
func TestPresaleDecisionSamplesTheClockOnce(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Hour)
	org, slot := provisioned(t, ctx, st, 100)
	var schema string
	if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE SEQUENCE clock_ticks`,
		`CREATE FUNCTION clock_timestamp() RETURNS timestamptz VOLATILE LANGUAGE sql AS $$
			SELECT '2026-08-10T11:30:00Z'::timestamptz + (nextval('` + schema + `.clock_ticks') - 1) * interval '1 hour' $$`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	instant := func(s string) *time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return &v
	}
	const code = "ADJACENT-544"
	for _, tc := range []struct {
		name                  string
		chOpens, chCloses     *time.Time
		codeOpens, codeCloses *time.Time
		wantAdmitted          bool
	}{
		// The reachable orientation: the channel closes at T while the code opens at T.
		{"the channel closes at T and the code opens at T", nil, instant("2026-08-10T12:00:00Z"),
			instant("2026-08-10T12:00:00Z"), nil, false},
		// The mirror: the code closes at T while the channel opens at T.
		{"the code closes at T and the channel opens at T", instant("2026-08-10T12:00:00Z"), nil,
			nil, instant("2026-08-10T12:00:00Z"), false},
		// Control: the first sample (11:30) is inside both windows.
		{"control: both windows contain the first sample", nil, instant("2026-08-10T13:00:00Z"),
			instant("2026-08-10T11:00:00Z"), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mustReplace(t, ctx, st, org, slot, []ChannelAllocation{
				{Channel: "presale", Cap: 100, RequiresCode: true, OpensAt: tc.chOpens, ClosesAt: tc.chCloses}})
			// Uncapped: the usage count never runs, so only the two windows decide.
			mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "presale", Code: code,
				OpensAt: tc.codeOpens, ClosesAt: tc.codeCloses})

			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if _, err = tx.ExecContext(ctx, `SET LOCAL search_path = `+schema+`, pg_catalog`); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.ExecContext(ctx, `SELECT setval('clock_ticks', 1, false)`); err != nil {
				t.Fatal(err)
			}
			redeemErr := redeemPresaleCode(ctx, tx, org, slot, "presale", code, lockScheduleQty)
			var last int64
			var called bool
			if err = tx.QueryRowContext(ctx, `SELECT last_value, is_called FROM clock_ticks`).Scan(&last, &called); err != nil {
				t.Fatal(err)
			}
			samples := int64(0)
			if called {
				samples = last
			}

			if samples != 1 {
				t.Errorf("redeemPresaleCode sampled the database clock %d times, want exactly 1: the "+
					"channel's window and the code's window must be judged at ONE instant (D9), or "+
					"two abutting windows can both admit (err=%v)", samples, redeemErr)
			}
			if tc.wantAdmitted {
				if redeemErr != nil {
					t.Fatalf("control: both windows contain the sampled instant, got %v; the fixture "+
						"cannot admit, so the refusals above would prove nothing", redeemErr)
				}
				return
			}
			if redeemErr == nil {
				t.Fatalf("ADMITTED across two abutting windows: the channel's window and the code's "+
					"window were judged at different instants (%d clock samples)", samples)
			}
			if !errors.Is(redeemErr, ErrPresaleCodeInvalid) || redeemErr.Error() != ErrPresaleCodeInvalid.Error() {
				t.Fatalf("got %v, want the uniform ErrPresaleCodeInvalid: the code step answers one "+
					"refusal whatever closed (D9)", redeemErr)
			}
		})
	}
}

// ⭐ THE ORACLE SCHEDULE (TKT-544 D9): the code step answers ONE refusal whatever
// the code is, including after a wait that only a code with a row can have.
//
// Production can produce this: a redemption of an EXHAUSTED code on another pool
// holds the code's row (here, a blocker that never writes), and an attacker's
// admission with that code waits on it across the channel's closes_at. Meanwhile
// admissions with an UNKNOWN code, NO code, and a code of ANOTHER channel, each on
// its own pool so they do not queue behind the first one's pool lock, have no row
// to wait on and finish before closes_at. All four must answer the identical
// error. With D3 the waiting one answered channel_window_closed and the other
// three presale_code_invalid, which tells the caller which code exists.
//
// The wire body is asserted where it is decided, in the api package
// (TestPresaleRefusalBodyIsUniformAcrossTheCodeLockWait).
func TestPresaleRefusalIsUniformAcrossTheCodeLockWait(t *testing.T) {
	const exhausted, other = "EXHAUSTED-544", "OTHER-CHANNEL-544"
	for _, path := range presaleAdmissions {
		t.Run(path.name, func(t *testing.T) {
			ctx, st, db := storeForTest(t, time.Hour)
			org, exhaustedPool := provisioned(t, ctx, st, 100)
			unknownPool := slotFor(t, ctx, st, org, 100)
			emptyPool := slotFor(t, ctx, st, org, 100)
			otherPool := slotFor(t, ctx, st, org, 100)
			pools := []uuid.UUID{exhaustedPool, unknownPool, emptyPool, otherPool}
			for _, p := range pools {
				mustReplace(t, ctx, st, org, p, []ChannelAllocation{{Channel: "presale", Cap: 100, RequiresCode: true}})
			}
			mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "presale", Code: exhausted,
				MaxRedemptions: capOf(lockScheduleQty)})
			mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: "reseller", Code: other})
			// Spend the cap, so the code is exhausted for real rather than by assertion.
			if _, _, err := path.admit(ctx, st, org, exhaustedPool, exhausted, "spend"); err != nil {
				t.Fatalf("spend the exhausted code: %v", err)
			}
			// Last setup step, from the database clock: every pool's channel closes at once.
			closes := dbClockOffset(t, db, windowMargin)
			for _, p := range pools {
				mustReplace(t, ctx, st, org, p, []ChannelAllocation{
					{Channel: "presale", Cap: 100, RequiresCode: true, ClosesAt: &closes}})
			}

			noRow := []struct {
				name, code, key string
				pool            uuid.UUID
				out             admissionOutcome
			}{
				{"unknown code", "NEVER-ISSUED-544", "unknown", unknownPool, admissionOutcome{}},
				{"no code", "", "empty", emptyPool, admissionOutcome{}},
				{"code of another channel", other, "other-channel", otherPool, admissionOutcome{}},
			}
			waited := admitAcrossBoundary(t, ctx, db, closes, holdCodeRow(org, "presale", exhausted),
				func() (uuid.UUID, bool, error) {
					return path.admit(ctx, st, org, exhaustedPool, exhausted, "exhausted")
				},
				func() {
					for i := range noRow {
						r := &noRow[i]
						r.out.id, r.out.replay, r.out.err = path.admit(ctx, st, org, r.pool, r.code, r.key)
					}
					// They answered while the channel was still open: the comparison is
					// between requests that all started before closes_at.
					if now := dbNow(t, ctx, db); !now.Before(closes) {
						t.Fatalf("setup: the three no-row admissions finished only at %s, not before the "+
							"channel's closes_at %s", now.UTC().Format(time.RFC3339Nano),
							closes.UTC().Format(time.RFC3339Nano))
					}
				})

			answers := []struct {
				name, key string
				out       admissionOutcome
			}{{"exhausted code that waited on its row across closes_at", "exhausted", waited}}
			for _, r := range noRow {
				answers = append(answers, struct {
					name, key string
					out       admissionOutcome
				}{r.name + " (no row, no wait)", r.key, r.out})
			}
			var report []string
			uniform := true
			for _, a := range answers {
				rows := claimsUnderKey(t, ctx, db, org, a.key)
				report = append(report, a.name+": err="+errString(a.out.err)+" claims="+strconv.Itoa(len(rows)))
				if !errors.Is(a.out.err, ErrPresaleCodeInvalid) || a.out.err.Error() != ErrPresaleCodeInvalid.Error() ||
					a.out.replay || len(rows) != 0 {
					uniform = false
				}
			}
			if !uniform {
				t.Fatalf("the four refusals differ; each must be the uniform ErrPresaleCodeInvalid with no "+
					"claim:\n  %s\nA distinct answer for the code that waited tells the caller the code "+
					"has a row (ADR-064 §4).", strings.Join(report, "\n  "))
			}
		})
	}
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return strconv.Quote(err.Error())
}
