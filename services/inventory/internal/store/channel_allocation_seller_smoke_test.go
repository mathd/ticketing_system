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

// The seller is judged BEFORE the presale code, and the WHICH-ERROR is the evidence.
//
// This test's first version tried to prove the ordering by counting redemptions, and
// could not fail (ai-review pass 2 [medium]): redemption is DERIVED from committed
// claims (`PresaleCodeStatuses`, consumingClaims), so a refusal at either check rolls
// the transaction back, inserts no claim, and leaves the count identical. It measured
// nothing while naming the right case — the exact shape AGENTS.md warns about, written
// by me while fixing a finding about a test that could not fail.
//
// What discriminates is giving the two checks DIFFERENT answers and seeing which one
// speaks. A wrong reseller AND a bad code:
//
//	seller first -> ErrUnavailable        (uniform, tells an attacker nothing)
//	code first   -> ErrPresaleCodeInvalid (tells an unauthorized caller the code was
//	                                       wrong, which is an oracle they should never
//	                                       have reached)
//
// Two distinct sentinels, so the mutation is visible. Ordering matters beyond the
// oracle: redeemPresaleCode takes the presale_codes ROW LOCK, and a caller who may not
// consume the allocation must not be able to serialize every other holder of that code
// behind them.
func TestTheSellerIsJudgedBeforeThePresaleCode(t *testing.T) {
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

	// Wrong reseller, and a code that does not exist. Both checks would refuse; only
	// the FIRST one to run decides which error comes back.
	_, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "reseller-acme", "order-both-bad",
		WithPresaleCode("NOT-A-CODE"), WithReseller(uuid.New()))
	if errors.Is(err, ErrPresaleCodeInvalid) {
		t.Fatal("an unauthorized seller was told its CODE was invalid — the code is being judged " +
			"before the seller. That hands a caller who may not consume this allocation an " +
			"oracle on presale codes, and lets them take the presale_codes row lock, " +
			"serializing every legitimate holder of that code behind a request that was " +
			"never eligible")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable — the uniform seller refusal", err)
	}

	// The fixture can reach the OTHER answers too, or the assertion above would be
	// vacuous — a fixture that refuses everything for one reason proves nothing about
	// which reason won.
	//
	// The AUTHORIZED seller with a bad code must hear about the code:
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "reseller-acme", "order-bad-code",
		WithPresaleCode("NOT-A-CODE"), WithReseller(acme)); !errors.Is(err, ErrPresaleCodeInvalid) {
		t.Fatalf("the bound seller with an invalid code: got %v want ErrPresaleCodeInvalid — if "+
			"this is not reachable, the negative above cannot distinguish the two checks", err)
	}
	// And with a good code, sells:
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "reseller-acme", "order-ok",
		WithPresaleCode("VIP"), WithReseller(acme)); err != nil {
		t.Fatalf("the bound seller with a valid code was refused: %v — the fixture admits no "+
			"allowed input", err)
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

// Two resellers sharing an idempotency key do not replay each other's holds
// (ai-review [high] F1).
//
// The bypass this closes did not beat the seller guard, it SKIPPED it. claims are
// UNIQUE (organizer_id, idempotency_key) and CreateHold returns a fingerprint-matching
// row as a replay before it ever reads sold_by. Two reseller credentials may legally
// share an organizer, and keys are caller-chosen and frequently sequential — so
// reseller B sending A's key with identical terms was handed A's authorized hold on
// A's bound allocation, with the guard never running.
//
// Three assertions, because each covers a different way the fix can rot:
//   - B does not receive A's claim (the bypass itself)
//   - B is refused by the SELLER guard rather than by an idempotency conflict, which
//     is what tells us B reached the guard at all
//   - A's own retry still replays, so the namespacing did not break idempotency for
//     the reseller it belongs to
func TestTwoResellersSharingAKeyDoNotReplayEachOthersHolds(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)
	acme, globex := uuid.New(), uuid.New()
	const shared = "1" // the kind of key a partner actually sends

	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "reseller-acme", Cap: 10}})
	bindSeller(t, ctx, db, slot, "reseller-acme", acme)

	// A sells, legitimately.
	first, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "reseller-acme", shared,
		WithReseller(acme))
	if err != nil {
		t.Fatalf("the bound reseller was refused its own allocation: %v", err)
	}

	// B sends the SAME key with IDENTICAL terms against A's bound allocation.
	got, replayed, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "reseller-acme", shared,
		WithReseller(globex))
	if err == nil {
		t.Fatalf("reseller B was granted a hold (%s, replay=%v) on an allocation bound to A by "+
			"reusing A's idempotency key — the seller guard was never reached", got.ID, replayed)
	}
	if got.ID == first.ID {
		t.Fatalf("reseller B received A's claim %s — another reseller's hold, handed over by an "+
			"idempotency collision", first.ID)
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("reseller B was refused with %v, want ErrUnavailable from the SELLER guard. An "+
			"ErrIdempotency here would mean B collided with A's row instead of being judged on "+
			"its own — the bypass would be closed by accident, and would reopen the moment the "+
			"terms differed", err)
	}

	// A's own retry still replays. The namespacing must not break idempotency for the
	// reseller the key belongs to.
	again, wasReplay, err := st.CreateHold(ctx, org, slot, uuid.Nil, 2, 0, "", "reseller-acme", shared,
		WithReseller(acme))
	if err != nil {
		t.Fatalf("A's own retry was refused: %v", err)
	}
	if !wasReplay || again.ID != first.ID {
		t.Fatalf("A's retry produced claim %s (replay=%v), want a replay of %s — a partner "+
			"retrying a timeout would place a SECOND hold", again.ID, wasReplay, first.ID)
	}
}

// A public caller's idempotency key is stored VERBATIM, on both paths.
//
// The compatibility half. Every claim in every database was written under the bare key,
// so transforming it strands in-flight retries — they would derive a new key, miss the
// persisted claim and hold twice. Asserted on the STORED value rather than on replay
// behaviour, because a replay can succeed for the wrong reason if both the write and
// the lookup are transformed identically.
//
// A partner's key is stored verbatim too: the namespace is the reseller_scope COLUMN,
// not a decoration on the key (see below).
func TestIdempotencyKeysAreStoredVerbatimOnBothPaths(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)
	acme := uuid.New()
	const key = "legacy-key-1"

	public, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "", key)
	if err != nil {
		t.Fatal(err)
	}
	partner, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "", key, WithReseller(acme))
	if err != nil {
		t.Fatalf("a partner reusing a public caller's key was refused: %v — the two namespaces "+
			"must not meet", err)
	}
	if partner.ID == public.ID {
		t.Fatal("a partner received the PUBLIC caller's claim for the same key")
	}

	for _, tc := range []struct {
		name  string
		id    uuid.UUID
		scope any
	}{
		{"public", public.ID, nil},
		{"partner", partner.ID, acme},
	} {
		var stored string
		var scope *uuid.UUID
		if err := db.QueryRowContext(ctx,
			`SELECT idempotency_key, reseller_scope FROM claims WHERE id=$1`, tc.id).
			Scan(&stored, &scope); err != nil {
			t.Fatal(err)
		}
		if stored != key {
			t.Fatalf("%s hold stored idempotency_key %q, want %q verbatim — the namespace is the "+
				"reseller_scope column, never a decoration on the caller's key", tc.name, stored, key)
		}
		switch want := tc.scope.(type) {
		case nil:
			if scope != nil {
				t.Fatalf("public hold stored reseller_scope %v, want NULL", *scope)
			}
		case uuid.UUID:
			if scope == nil || *scope != want {
				t.Fatalf("partner hold stored reseller_scope %v, want %s", scope, want)
			}
		}
	}
}

// A PUBLIC caller cannot forge its way into a reseller's namespace (ai-review pass 2
// [high] F4).
//
// The defect that made the first fix wrong. That version derived the partner's key as
// the string "r:<uuid>:<key>" while public keys stayed arbitrary raw strings IN THE SAME
// COLUMN — so a public caller could send that exact derived string, take the row first,
// and permanently deny the reseller that key. Predictable keys ("1") plus a known
// reseller id make it targeted rather than theoretical.
//
// The namespace is now a column the caller does not supply, so this test sends the most
// hostile string available — the old derived form — and asserts it cannot touch the
// partner's row. It would have failed against the string-prefix implementation, which is
// the point of writing it this way rather than asserting on the column directly.
func TestAPublicCallerCannotForgeAResellerNamespace(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)
	acme := uuid.New()
	const key = "1"
	forged := "r:" + acme.String() + ":" + key

	// The attacker gets there first, with the string the old scheme would have derived.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "", forged); err != nil {
		t.Fatal(err)
	}
	// And a second public claim on the bare key, because a public caller may use that too.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "", key); err != nil {
		t.Fatal(err)
	}

	// The reseller's own key is unaffected: it sells, and it is a NEW claim.
	claim, replayed, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "", key, WithReseller(acme))
	if err != nil {
		t.Fatalf("a public caller poisoned reseller %s's idempotency key %q: %v — this is a "+
			"targeted denial of service against one partner, and it is what a string prefix "+
			"inside a shared namespace buys you", acme, key, err)
	}
	if replayed {
		t.Fatal("the reseller REPLAYED a public caller's claim — the two namespaces are still one")
	}

	// And its own retry replays, so the scope is a real namespace rather than a way of
	// never matching anything.
	again, wasReplay, err := st.CreateHold(ctx, org, slot, uuid.Nil, 1, 0, "", "", key, WithReseller(acme))
	if err != nil {
		t.Fatalf("the reseller's own retry: %v", err)
	}
	if !wasReplay || again.ID != claim.ID {
		t.Fatalf("the reseller's retry produced %s (replay=%v), want a replay of %s — scoping "+
			"that never matches is not idempotency, it is a second hold on every retry",
			again.ID, wasReplay, claim.ID)
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
