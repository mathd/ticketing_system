//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
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
// sweeper (ADR-010).
//
// A counter would need decrementing on expiry, and expiry here is LAZY: nothing
// runs at the moment a hold dies. This is the test that fails if anyone
// "optimizes" the sum into a stored count.
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
	// The first hold is already expired, so its redemption is no longer live and
	// the cap of 1 is available again. No sweeper ran.
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
		"no code at all":         "",
		"unknown code":           "NEVER-ISSUED",
		"code of another channel": "OTHER-CHANNEL",
		"exhausted code":         "SPENT",
		"out-of-window code":     "EXPIRED",
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
		goldenBare       = "bbb9368991be6cd401b4876d78a07bbb1ebcb18be9c90abe45a98102c11a4510"
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
// consumingClaims counts confirmed-or-live, and the confirmed half is what makes
// a redemption stick after the buyer pays. Counting only live claims would return
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
	// claims_organizer_id_idempotency_key_key, then filtered channel_code and
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
