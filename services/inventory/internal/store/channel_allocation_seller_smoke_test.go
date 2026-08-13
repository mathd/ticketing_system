//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// bindSeller binds an allocation to a reseller, in DB terms.
//
// Written directly rather than through ReplaceChannelAllocations for the same reason
// setWindow is: the binding is the thing under test, and routing the fixture through the
// writer under test would let one bug hide the other.
func bindSeller(t *testing.T, ctx context.Context, db *sql.DB, slot uuid.UUID, channel string, seller uuid.UUID) {
	t.Helper()
	res, err := db.ExecContext(ctx,
		`UPDATE channel_allocations SET sold_by=$3 WHERE pool_id=$1 AND channel_code=$2`,
		slot, channel, seller)
	if err != nil {
		t.Fatal(err)
	}
	// A fixture that silently updates nothing is how a green test comes to prove
	// nothing (AGENTS.md: a green test that cannot reach the failing state).
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("bindSeller updated %d rows, want 1 — the allocation fixture is not what the test thinks it is", n)
	}
}

// A bound allocation admits its own seller and refuses everyone else.
//
// The core of TKT-246. Three callers, one allocation:
//   - the bound reseller       -> sells
//   - a DIFFERENT reseller     -> refused (the bug a bare boolean would have shipped:
//     "someone owns this" is not the same as "you own this")
//   - no reseller identity     -> refused (the unauthenticated caller TKT-240's probe
//     used; this is the case the revert exists for)
func TestABoundAllocationAdmitsOnlyItsSeller(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)
	acme, globex := uuid.New(), uuid.New()

	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "reseller-acme", Cap: 10}})
	bindSeller(t, ctx, db, slot, "reseller-acme", acme)

	// The bound seller sells.
	claim, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "reseller-acme", "bound-own",
		WithReseller(acme))
	if err != nil {
		t.Fatalf("the bound reseller was refused its own allocation: %v", err)
	}
	if claim.Channel != "reseller-acme" {
		t.Fatalf("claim.Channel = %q, want the channel it consumed", claim.Channel)
	}

	// A different reseller does not.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "reseller-acme", "bound-other",
		WithReseller(globex)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a DIFFERENT reseller consumed acme's allocation: got %v want ErrUnavailable — "+
			"this is the bug a boolean 'is bound' flag would have shipped", err)
	}

	// And an unauthenticated caller — no reseller identity at all — does not.
	// This is TKT-240's probe, at the tier that decides it.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "reseller-acme", "bound-anon"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a caller presenting NO reseller identity consumed a bound allocation: got %v "+
			"want ErrUnavailable — that is exactly the bypass TKT-240 was reverted for", err)
	}
}

// An UNBOUND allocation behaves exactly as it did before this migration.
//
// The compatibility half, and the one that decides whether this ships safely: every
// allocation in every database today has sold_by NULL. If a NULL binding refused
// anyone, this change would stop the platform selling rather than close a seam.
func TestAnUnboundAllocationStaysPublic(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)

	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 10}})

	// No reseller identity: the pre-migration caller.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "presale", "unbound-anon"); err != nil {
		t.Fatalf("an unbound allocation refused an anonymous caller: %v — every allocation that "+
			"exists today is unbound, so this refusal would be a platform-wide outage", err)
	}
	// And a reseller identity is not a reason to refuse either: unbound means public,
	// not "public only".
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "presale", "unbound-reseller",
		WithReseller(uuid.New())); err != nil {
		t.Fatalf("an unbound allocation refused an identified reseller: %v — NULL means anyone", err)
	}
}

// Authorization is judged BEFORE capacity, and the refusals stay distinguishable.
//
// TKT-238's finding, applied to the new guard: a channel-property refusal must not be
// masked by a full pool, or a gated channel reads as a sellout exactly when the on-sale
// is busiest. The window already obeys this; the seller check has to as well, and it sits
// between the window and the code.
func TestSellerIsJudgedBeforeCapacityAndAfterTheWindow(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	acme := uuid.New()

	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "reseller-acme", Cap: 6}})
	bindSeller(t, ctx, db, slot, "reseller-acme", acme)

	// Exhaust the pool WITHOUT touching the allocation's own cap.
	if _, _, err := st.PlaceOperationalHold(ctx, org, slot, 10, "house", "foh", "staff:amy", "ops", "seller-eat"); err != nil {
		t.Fatal(err)
	}

	// The discriminating assertion (ai-review [medium] F3).
	//
	// "Unauthorized on an exhausted pool answers ErrUnavailable" is NOT evidence of
	// precedence: a capacity refusal produces the identical error and the identical
	// state, so that assertion stays green with the seller guard deleted outright. The
	// first version of this test made exactly that mistake, and its own comment
	// claimed it "pins that the guard RAN".
	//
	// What discriminates is the AUTHORIZED caller on the same exhausted pool. If the
	// seller check runs before capacity, the authorized and unauthorized callers get
	// the same answer here (both ErrUnavailable, for different reasons) — but the
	// UNAUTHORIZED one must be refused even when the pool has room, which the earlier
	// half of this test established, AND the authorized one must be refused only for
	// capacity. Pair that with the code-precedence assertion below, which is
	// observable: a gated allocation must NOT redeem a code for a caller who fails the
	// seller check, because redeemPresaleCode mutates and the redemption is countable.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "reseller-acme", "seller-exhausted",
		WithReseller(uuid.New())); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v want ErrUnavailable", err)
	}
	// The bound seller on the SAME exhausted pool is also refused — for capacity. If
	// this ever succeeds, the pool exhaustion in this fixture is not real and every
	// assertion above it is measuring nothing.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "reseller-acme", "seller-exhausted-ok",
		WithReseller(acme)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("the BOUND seller on an exhausted pool: got %v want ErrUnavailable — if this "+
			"succeeded the pool is not actually exhausted and the unauthorized case above proved "+
			"nothing", err)
	}

	// The window still wins over the seller: a closed window refuses as a window even
	// for a caller who would also have failed the seller check. Precedence is
	// window -> seller -> code -> capacity.
	setWindow(t, ctx, db, slot, "reseller-acme", "clock_timestamp() + interval '1 hour'", "NULL")
	err := func() error {
		_, _, e := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "reseller-acme", "seller-window",
			WithReseller(uuid.New()))
		return e
	}()
	if !errors.Is(err, ErrChannelWindowClosed) {
		t.Fatalf("a closed window on a bound allocation: got %v want ErrChannelWindowClosed — "+
			"the window is judged first, and an unauthorized caller must not learn that the "+
			"channel is bound rather than closed", err)
	}
}

// The seller is judged BEFORE the presale code, and that is OBSERVABLE.
//
// The one precedence claim in this ticket that leaves physical evidence, which is why
// it carries the weight (ai-review [medium] F3): a refusal ordering assertion between
// two paths that both answer ErrUnavailable cannot discriminate, but a REDEMPTION can —
// redeemPresaleCode mutates, and its usage count is readable afterwards.
//
// So: a gated AND bound allocation, approached by a caller with a valid code and the
// WRONG reseller. If the seller check runs first the code is untouched. If the code
// check runs first, an unauthorized caller has consumed one of a scarce code's
// redemptions — a denial-of-service on a presale by anyone who learns a code, and the
// refusal they get back tells them the code was real.
func TestAnUnauthorizedSellerDoesNotBurnAPresaleRedemption(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)
	acme := uuid.New()

	mustReplace(t, ctx, st, org, slot,
		[]ChannelAllocation{{Channel: "reseller-acme", Cap: 10, RequiresCode: true}})
	bindSeller(t, ctx, db, slot, "reseller-acme", acme)
	maxRedemptions := int32(5)
	if _, err := st.IssuePresaleCode(ctx, PresaleCode{
		OrganizerID: org, Channel: "reseller-acme", Code: "VIP", MaxRedemptions: &maxRedemptions,
	}); err != nil {
		t.Fatalf("seed presale code: %v", err)
	}

	used := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COALESCE(sum(quantity),0) FROM claims WHERE organizer_id=$1 AND channel_code=$2 AND presale_code=$3`,
			org, "reseller-acme", "VIP").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := used()

	// A VALID code, the WRONG reseller.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "reseller-acme", "code-precedence",
		WithPresaleCode("VIP"), WithReseller(uuid.New())); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("an unauthorized seller with a valid code: got %v want ErrUnavailable", err)
	}
	if after := used(); after != before {
		t.Fatalf("presale redemption moved %d -> %d for a caller who failed the SELLER check — "+
			"the code is being judged before the seller, so anyone holding a code can burn a "+
			"scarce presale's redemptions against an allocation they may not consume, and the "+
			"refusal confirms the code was valid", before, after)
	}

	// And the bound seller with the same code still sells, so the fixture is capable of
	// reaching the success state rather than refusing everything.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "reseller-acme", "code-precedence-ok",
		WithPresaleCode("VIP"), WithReseller(acme)); err != nil {
		t.Fatalf("the BOUND seller with a valid code was refused: %v — the fixture admits no "+
			"allowed input and the negative above proves nothing", err)
	}
	if after := used(); after == before {
		t.Fatal("an AUTHORIZED sale did not move the redemption count — this fixture cannot " +
			"observe redemptions at all, so the assertion above is vacuous")
	}
}

// A seller-bound allocation is refused to a group reservation with no reseller identity.
//
// PlaceGroupReservation is the OTHER channelled claim path, and it has no credential.
// TKT-240's post-mortem named "every layer reasoned about the path being changed" as the
// root cause of the paths it missed; this is that lesson applied to the sibling path.
// An unbound allocation must still admit it, or every existing agency group placement
// breaks.
func TestGroupPlacementObeysTheSellerBinding(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)
	acme := uuid.New()
	expiry := time.Now().Add(24 * time.Hour)

	mustReplace(t, ctx, st, org, slot,
		[]ChannelAllocation{{Channel: "reseller-acme", Cap: 10}, {Channel: "agency", Cap: 10}})
	bindSeller(t, ctx, db, slot, "reseller-acme", acme)

	// Bound, no identity: refused.
	if _, _, err := st.PlaceGroupReservation(ctx, org, slot, 2, "agency-a", expiry,
		"reseller-acme", "staff:amy", "group", "grp-bound"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a group placement consumed a seller-bound allocation with no credential: "+
			"got %v want ErrUnavailable", err)
	}
	// Unbound: sells, exactly as today.
	if _, _, err := st.PlaceGroupReservation(ctx, org, slot, 2, "agency-a", expiry,
		"agency", "staff:amy", "group", "grp-unbound"); err != nil {
		t.Fatalf("a group placement on an UNBOUND allocation was refused: %v — that is every "+
			"agency placement in existence today", err)
	}
}

// A replace that omits sold_by UNBINDS, and that is worth knowing rather than
// discovering.
//
// ReplaceChannelAllocations is a full-set replace under the pool lock (ADR-024): it
// DELETEs the set and re-INSERTs it, so a caller that reads the allocations, edits a
// cap and writes them back drops any field it did not carry. For sold_by that is an
// authorization change performed by omission -- a bound allocation silently becomes
// public, which is the TKT-236 shape (renaming a disabled channel re-enabled it).
//
// The behaviour is CORRECT for a full-set replace and this test pins it as deliberate
// rather than fixing it: an editor that round-trips the set must carry sold_by, and
// TKT-244 (the allocation editor UI) has to read this test before it ships.
func TestAReplaceThatOmitsSoldByUnbinds(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)
	acme := uuid.New()

	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "reseller-acme", Cap: 10}})
	bindSeller(t, ctx, db, slot, "reseller-acme", acme)

	// Bound: an anonymous caller is refused.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "reseller-acme", "unbind-before"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("precondition: a bound allocation should refuse an anonymous caller, got %v", err)
	}

	// A cap edit that does not carry sold_by.
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "reseller-acme", Cap: 20}})

	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "reseller-acme", "unbind-after"); err != nil {
		t.Fatalf("after a replace that omitted sold_by the allocation should be UNBOUND and sell "+
			"to anyone, got %v — if this now refuses, the replace semantics changed and TKT-244's "+
			"editor assumptions changed with them", err)
	}

	// And carrying it preserves the binding, so the editor has a correct path.
	bound := acme
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "reseller-acme", Cap: 20, SoldBy: &bound}})
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "reseller-acme", "rebind"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a replace CARRYING sold_by should preserve the binding, got %v", err)
	}
}

// The binding does not leak into the registry question.
//
// ADR-024's registry is a lookup, not a constraint (TKT-235). sold_by is a property of
// the ALLOCATION, so an unregistered channel code with an unbound allocation must keep
// selling — the guard must key on the row, never on whether anyone registered the code.
func TestSellerBindingDoesNotMakeTheRegistryLoadBearing(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)
	acme := uuid.New()

	const unregistered = "legacy-partner-2019"
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: unregistered, Cap: 8}})

	// Unregistered AND unbound: sells, as TestAnUnregisteredChannelCodeStillSells pins.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", unregistered, "unreg-unbound"); err != nil {
		t.Fatalf("an unregistered, unbound channel stopped selling: %v — the registry is a "+
			"lookup, not a constraint (ADR-024)", err)
	}
	// Unregistered AND bound: the binding still applies. Being absent from a registry
	// inventory cannot see is not a reason to skip authorization.
	bindSeller(t, ctx, db, slot, unregistered, acme)
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", unregistered, "unreg-bound"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a bound allocation on an unregistered code admitted an anonymous caller: %v", err)
	}
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", unregistered, "unreg-bound-ok",
		WithReseller(acme)); err != nil {
		t.Fatalf("the bound seller was refused on an unregistered code: %v", err)
	}
}
