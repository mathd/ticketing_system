//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The TKT-510 pair. Request A presents CODE on channel "agency". Request B sends no
// code, on a channel whose name spells A's code frame. The two have the same
// fingerprint (TestFingerprintCollisionsAcrossDifferentChannelsArePinned).
const (
	collideChannelA = "agency"
	collideCodeA    = "CODE"
	collideChannelB = "agency:c6:agency:p4:CODE"
)

// claimSnapshot is every stored field that a wrong replay or a wrong expiry write
// would change.
type claimSnapshot struct {
	ID          uuid.UUID
	Channel     sql.NullString
	Quantity    int32
	Status      string
	Fingerprint string
	PresaleCode sql.NullString
	Currency    sql.NullString
	ExpiresAt   string
	UpdatedAt   string
	History     int
}

func snapshotClaim(t *testing.T, ctx context.Context, db *sql.DB, id uuid.UUID) claimSnapshot {
	t.Helper()
	var s claimSnapshot
	var expires sql.NullTime
	var updated time.Time
	if err := db.QueryRowContext(ctx, `SELECT id,channel_code,quantity,status,request_fingerprint,presale_code,currency,expires_at,updated_at,
		(SELECT count(*) FROM claim_history h WHERE h.claim_id=c.id) FROM claims c WHERE id=$1`, id).
		Scan(&s.ID, &s.Channel, &s.Quantity, &s.Status, &s.Fingerprint, &s.PresaleCode, &s.Currency, &expires, &updated, &s.History); err != nil {
		t.Fatal(err)
	}
	if expires.Valid {
		s.ExpiresAt = expires.Time.UTC().Format(time.RFC3339Nano)
	}
	s.UpdatedAt = updated.UTC().Format(time.RFC3339Nano)
	return s
}

func claimsOf(t *testing.T, ctx context.Context, db *sql.DB, org uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM claims WHERE organizer_id=$1`, org).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// collisionFixture provisions a pool with an allocation for EACH channel of the pair.
//
// A's channel is gated and CODE is issued on it. B's channel is ungated, so B would
// sell on its own merits. A refusal of B can therefore come only from the key, and
// not from a missing allocation. A reseller binds both allocations to that reseller.
func collisionFixture(t *testing.T, ctx context.Context, st *Postgres, reseller uuid.UUID) (org, slot uuid.UUID) {
	t.Helper()
	org, slot = provisioned(t, ctx, st, 100)
	var soldBy *uuid.UUID
	if reseller != uuid.Nil {
		soldBy = &reseller
	}
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{
		{Channel: collideChannelA, Cap: 10, RequiresCode: true, SoldBy: soldBy},
		{Channel: collideChannelB, Cap: 10, SoldBy: soldBy},
	})
	mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: collideChannelA, Code: collideCodeA})
	return org, slot
}

// requireCollision fails the test if the pair no longer collides. A pair that does not
// collide is refused by the fingerprint alone, and the test then proves nothing about
// the channel compare.
func requireCollision(t *testing.T, org, slot, tt uuid.UUID, reseller uuid.UUID) {
	t.Helper()
	a := fingerprint(org, slot, tt, 2, 1500, "EUR", collideChannelA, collideCodeA, reseller)
	b := fingerprint(org, slot, tt, 2, 1500, "EUR", collideChannelB, "", reseller)
	if a != b {
		t.Fatal("precondition: the TKT-510 pair no longer collides, so this test cannot reach " +
			"the channel compare. See TestFingerprintCollisionsAcrossDifferentChannelsArePinned.")
	}
}

var replayScopes = []struct {
	name     string
	reseller uuid.UUID
}{
	{"public", uuid.Nil},
	{"reseller", uuid.MustParse("55555555-5555-5555-5555-555555555555")},
}

func resellerOpts(reseller uuid.UUID) []HoldOption {
	if reseller == uuid.Nil {
		return nil
	}
	return []HoldOption{WithReseller(reseller)}
}

// Under one key, a request on a DIFFERENT channel is a key reuse, also when its
// fingerprint equals the stored one (TKT-510, COS 1).
//
// ADR-024: "a replay that changes or drops the channel is a key reuse". ADR-010: the same
// key with different input conflicts. B is a different request, so B must get
// ErrIdempotency. A replay would hand B the hold A paid a code for, on a channel B
// never named.
func TestARequestOnAnotherChannelDoesNotReplayAStoredCodeHold(t *testing.T) {
	for _, scope := range replayScopes {
		t.Run(scope.name, func(t *testing.T) {
			ctx, st, db := storeForTest(t, time.Minute)
			org, slot := collisionFixture(t, ctx, st, scope.reseller)
			tt := uuid.New()
			requireCollision(t, org, slot, tt, scope.reseller)
			const key = "tkt-510-a-then-b"

			a, replay, err := st.CreateHold(ctx, org, slot, tt, 2, 1500, "EUR", collideChannelA, key,
				append(resellerOpts(scope.reseller), WithPresaleCode(collideCodeA))...)
			if err != nil || replay {
				t.Fatalf("A: err=%v replay=%v, want a new hold", err, replay)
			}
			before := snapshotClaim(t, ctx, db, a.ID)
			redeemed := redeemedUnits(t, ctx, db, org, collideChannelA, collideCodeA)

			got, replay, err := st.CreateHold(ctx, org, slot, tt, 2, 1500, "EUR", collideChannelB, key,
				resellerOpts(scope.reseller)...)
			if err == nil && got.ID == a.ID {
				t.Fatalf("B (channel %q, no code) was handed A's hold %s (replay=%v): the "+
					"fingerprints are equal and the channel was not compared", collideChannelB, a.ID, replay)
			}
			if !errors.Is(err, ErrIdempotency) {
				t.Fatalf("B under A's key: err=%v, want ErrIdempotency", err)
			}
			if replay || got.ID != uuid.Nil {
				t.Fatalf("B was refused but also returned replay=%v claim=%s", replay, got.ID)
			}
			if after := snapshotClaim(t, ctx, db, a.ID); after != before {
				t.Fatalf("A's claim changed:\nbefore %+v\nafter  %+v", before, after)
			}
			if n := claimsOf(t, ctx, db, org); n != 1 {
				t.Fatalf("%d claims after the refusal, want 1 (A's only)", n)
			}
			if got := redeemedUnits(t, ctx, db, org, collideChannelA, collideCodeA); got != redeemed {
				t.Fatalf("CODE redemptions moved from %d to %d", redeemed, got)
			}

			// The fixture check: under its own key, B sells. So the refusal above came
			// from the key, not from B's allocation.
			own, _, err := st.CreateHold(ctx, org, slot, tt, 2, 1500, "EUR", collideChannelB, "tkt-510-b-own",
				resellerOpts(scope.reseller)...)
			if err != nil || own.Channel != collideChannelB {
				t.Fatalf("fixture: B under its own key: err=%v channel=%q, want a hold on %q", err, own.Channel, collideChannelB)
			}
		})
	}
}

// The reverse order: B is stored first, then A arrives under B's key (TKT-510, COS 2).
//
// B's own row already carries the colliding fingerprint, because the two requests hash
// equally. So no stored value needs to be forged. A replay would give A a hold on a
// channel A never named, and A's code would never be judged.
func TestACodeRequestDoesNotReplayAStoredHoldOnAnotherChannel(t *testing.T) {
	for _, scope := range replayScopes {
		t.Run(scope.name, func(t *testing.T) {
			ctx, st, db := storeForTest(t, time.Minute)
			org, slot := collisionFixture(t, ctx, st, scope.reseller)
			tt := uuid.New()
			requireCollision(t, org, slot, tt, scope.reseller)
			const key = "tkt-510-b-then-a"

			b, replay, err := st.CreateHold(ctx, org, slot, tt, 2, 1500, "EUR", collideChannelB, key,
				resellerOpts(scope.reseller)...)
			if err != nil || replay {
				t.Fatalf("B: err=%v replay=%v, want a new hold", err, replay)
			}
			before := snapshotClaim(t, ctx, db, b.ID)

			got, replay, err := st.CreateHold(ctx, org, slot, tt, 2, 1500, "EUR", collideChannelA, key,
				append(resellerOpts(scope.reseller), WithPresaleCode(collideCodeA))...)
			if err == nil && got.ID == b.ID {
				t.Fatalf("A (channel %q, code %q) was handed B's hold %s (replay=%v): the "+
					"fingerprints are equal and the channel was not compared", collideChannelA, collideCodeA, b.ID, replay)
			}
			if !errors.Is(err, ErrIdempotency) {
				t.Fatalf("A under B's key: err=%v, want ErrIdempotency", err)
			}
			if replay || got.ID != uuid.Nil {
				t.Fatalf("A was refused but also returned replay=%v claim=%s", replay, got.ID)
			}
			if after := snapshotClaim(t, ctx, db, b.ID); after != before {
				t.Fatalf("B's claim changed:\nbefore %+v\nafter  %+v", before, after)
			}
			if n := claimsOf(t, ctx, db, org); n != 1 {
				t.Fatalf("%d claims after the refusal, want 1 (B's only)", n)
			}
			// The refusal happens before the code is judged, so no redemption is spent.
			if got := redeemedUnits(t, ctx, db, org, collideChannelA, collideCodeA); got != 0 {
				t.Fatalf("CODE has %d redemptions after a refused request, want 0", got)
			}

			// The fixture check: under its own key, A sells with its code.
			own, _, err := st.CreateHold(ctx, org, slot, tt, 2, 1500, "EUR", collideChannelA, "tkt-510-a-own",
				append(resellerOpts(scope.reseller), WithPresaleCode(collideCodeA))...)
			if err != nil || own.Channel != collideChannelA {
				t.Fatalf("fixture: A under its own key: err=%v channel=%q, want a hold on %q", err, own.Channel, collideChannelA)
			}
		})
	}
}

// An identical retry still replays, for every row class the compare can meet.
//
// This is the control for the two tests above. The new compare must not refuse a
// genuine retry: an empty channel (stored NULL, read back as ""), a colon-free channel,
// a channel with colons, with and without a code, with and without a reseller, and the
// legacy request (no ticket type, currency "").
//
// An empty channel has no allocation, so a code there is ignored and not stored. It
// still enters the fingerprint (store.go computes the fingerprint before it clears the
// code), so that class is in the table too.
func TestAnIdenticalRetryReplaysForEveryRowClass(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	type class struct {
		currency      string
		legacy        bool
		channel, code string
		reseller      uuid.UUID
	}
	var classes []class
	for _, ch := range []string{"", "presale", "a:b:c"} {
		for _, code := range []string{"", "K:1"} {
			for _, scope := range replayScopes {
				classes = append(classes, class{"EUR", false, ch, code, scope.reseller})
				classes = append(classes, class{"", true, ch, code, scope.reseller})
			}
		}
	}
	for _, c := range classes {
		name := fmt.Sprintf("channel=%q code=%q reseller=%v legacy=%v", c.channel, c.code, c.reseller != uuid.Nil, c.legacy)
		t.Run(name, func(t *testing.T) {
			org, slot := provisioned(t, ctx, st, 100)
			if c.channel != "" {
				var soldBy *uuid.UUID
				if c.reseller != uuid.Nil {
					r := c.reseller
					soldBy = &r
				}
				// Gated only when the request carries a code, so the code is judged
				// and stored.
				mustReplace(t, ctx, st, org, slot, []ChannelAllocation{
					{Channel: c.channel, Cap: 10, RequiresCode: c.code != "", SoldBy: soldBy}})
				if c.code != "" {
					mustIssue(t, ctx, st, PresaleCode{OrganizerID: org, Channel: c.channel, Code: c.code})
				}
			}
			tt, amount := uuid.New(), int64(1500)
			if c.legacy {
				tt, amount = uuid.Nil, 0
			}
			opts := resellerOpts(c.reseller)
			if c.code != "" {
				opts = append(opts, WithPresaleCode(c.code))
			}
			first, replay, err := st.CreateHold(ctx, org, slot, tt, 2, amount, c.currency, c.channel, "retry", opts...)
			if err != nil || replay {
				t.Fatalf("first: err=%v replay=%v", err, replay)
			}
			again, replay, err := st.CreateHold(ctx, org, slot, tt, 2, amount, c.currency, c.channel, "retry", opts...)
			if err != nil {
				t.Fatalf("an identical retry was refused: %v", err)
			}
			if !replay || again.ID != first.ID {
				t.Fatalf("an identical retry gave claim %s (replay=%v), want a replay of %s", again.ID, replay, first.ID)
			}
			if n := claimsOf(t, ctx, db, org); n != 1 {
				t.Fatalf("%d claims after a retry, want 1", n)
			}
		})
	}
}

// The currency is compared too, for the same reason as the channel (TKT-510, D10).
//
// The currency is the field before the channel in the fingerprint, and it is not
// framed. So currency "EUR:c0::p1:K" with no channel and no code hashes as currency
// "EUR" with no channel and code "K". The handler refuses any currency but EUR or ""
// (api/server.go), so this request reaches the store only through a direct caller.
// The test calls CreateHold directly for that reason: the store must not depend on
// one caller's validation for the meaning of a replay.
func TestARequestInAnotherCurrencyDoesNotReplayAStoredHold(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 10)
	tt := uuid.New()
	const spelled = "EUR:c0::p1:K"
	if fingerprint(org, slot, tt, 1, 1500, "EUR", "", "K", uuid.Nil) != fingerprint(org, slot, tt, 1, 1500, spelled, "", "", uuid.Nil) {
		t.Fatal("precondition: the currency pair no longer collides, so this test cannot reach the currency compare")
	}
	const key = "tkt-510-currency"

	// An empty channel cannot be gated, so the code is ignored and not stored. It
	// still enters the stored fingerprint.
	first, _, err := st.CreateHold(ctx, org, slot, tt, 1, 1500, "EUR", "", key, WithPresaleCode("K"))
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotClaim(t, ctx, db, first.ID)

	got, replay, err := st.CreateHold(ctx, org, slot, tt, 1, 1500, spelled, "", key)
	if err == nil && got.ID == first.ID {
		t.Fatalf("a request in currency %q was handed the EUR hold %s (replay=%v): the "+
			"fingerprints are equal and the currency was not compared", spelled, first.ID, replay)
	}
	if !errors.Is(err, ErrIdempotency) {
		t.Fatalf("err=%v, want ErrIdempotency", err)
	}
	if after := snapshotClaim(t, ctx, db, first.ID); after != before {
		t.Fatalf("the stored claim changed:\nbefore %+v\nafter  %+v", before, after)
	}
	if n := claimsOf(t, ctx, db, org); n != 1 {
		t.Fatalf("%d claims, want 1", n)
	}
}

// An identical retry replays when the column drops trailing spaces from the currency
// (TKT-510, D15).
//
// claims.currency is varchar(3). PostgreSQL stores a longer value only when every
// excess character is a space, and then it drops those spaces. So the row holds "EUR"
// while the request, and the fingerprint, hold "EUR ". The currency compare must see
// the stored form, or the request is refused by its own retry.
//
// varchar(n) counts characters, not bytes. "€UR " is 6 bytes and 4 characters, so the
// row holds "€UR".
//
// The handler refuses these currencies (api/server.go), so the test calls CreateHold
// directly, as TestARequestInAnotherCurrencyDoesNotReplayAStoredHold does.
func TestAnIdenticalRetryReplaysWhenTheColumnDropsTrailingSpaces(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	cases := []struct{ sent, stored string }{
		{"EUR ", "EUR"},
		{"EUR   ", "EUR"},
		{"\u20acUR ", "\u20acUR"},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%q", c.sent), func(t *testing.T) {
			org, slot := provisioned(t, ctx, st, 10)
			tt := uuid.New()
			const key = "tkt-510-padded-currency"
			first, replay, err := st.CreateHold(ctx, org, slot, tt, 1, 1500, c.sent, "", key)
			if err != nil || replay {
				t.Fatalf("first: err=%v replay=%v, want a new hold", err, replay)
			}
			// The fixture check: the column dropped the spaces. If it did not, the
			// raw compare would pass and this test would prove nothing.
			if got := snapshotClaim(t, ctx, db, first.ID).Currency; !got.Valid || got.String != c.stored {
				t.Fatalf("fixture: stored currency %+v, want %q", got, c.stored)
			}

			again, replay, err := st.CreateHold(ctx, org, slot, tt, 1, 1500, c.sent, "", key)
			if err != nil {
				t.Fatalf("an identical retry was refused: %v", err)
			}
			if !replay || again.ID != first.ID {
				t.Fatalf("an identical retry gave claim %s (replay=%v), want a replay of %s", again.ID, replay, first.ID)
			}
			if n := claimsOf(t, ctx, db, org); n != 1 {
				t.Fatalf("%d claims after a retry, want 1", n)
			}
		})
	}
}

// A different request under the key of an EXPIRED claim is a key reuse, not an expiry
// (TKT-510, COS 5).
//
// ADR-010: a key is single-use across the claim's full lifecycle. The key belongs to
// the stored claim whether that claim is live or dead. So the answer to a different
// request is ErrIdempotency, and that request must not write anything to the stored
// claim: no expiry, no history row. Only the claim's own retry may write its expiry.
//
// Two kinds of "different": a different fingerprint, and the same fingerprint on a
// different channel.
func TestADifferentRequestUnderAnExpiredClaimsKeyIsAKeyReuse(t *testing.T) {
	type req struct {
		qty           int32
		channel, code string
	}
	cases := []struct {
		name         string
		owner, other req
	}{
		{"different fingerprint", req{2, collideChannelB, ""}, req{3, collideChannelB, ""}},
		{"same fingerprint, different channel", req{2, collideChannelA, collideCodeA}, req{2, collideChannelB, ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, st, db := storeForTest(t, time.Minute)
			org, slot := collisionFixture(t, ctx, st, uuid.Nil)
			tt := uuid.New()
			const key = "tkt-510-expired"
			hold := func(r req) (Claim, bool, error) {
				var opts []HoldOption
				if r.code != "" {
					opts = append(opts, WithPresaleCode(r.code))
				}
				return st.CreateHold(ctx, org, slot, tt, r.qty, 1500, "EUR", r.channel, key, opts...)
			}

			owner, _, err := hold(c.owner)
			if err != nil {
				t.Fatal(err)
			}
			// Expire it by its clock, not by a status write: the claim stays 'held'
			// with an expiry in the past, which is the state a retry must judge.
			if _, err := db.ExecContext(ctx, `UPDATE claims SET expires_at=now()-interval '1 minute' WHERE id=$1`, owner.ID); err != nil {
				t.Fatal(err)
			}
			before := snapshotClaim(t, ctx, db, owner.ID)
			if before.Status != "held" {
				t.Fatalf("fixture: status %q, want held", before.Status)
			}

			_, replay, err := hold(c.other)
			if errors.Is(err, ErrConflict) {
				t.Fatal("a different request got ErrConflict: it was judged as the expired claim's " +
					"own retry, and it wrote that claim's expiry")
			}
			if !errors.Is(err, ErrIdempotency) || replay {
				t.Fatalf("err=%v replay=%v, want ErrIdempotency", err, replay)
			}
			if after := snapshotClaim(t, ctx, db, owner.ID); after != before {
				t.Fatalf("a different request wrote to the stored claim:\nbefore %+v\nafter  %+v", before, after)
			}

			// The owner's own retry is the one request that may expire the claim.
			if _, _, err := hold(c.owner); !errors.Is(err, ErrConflict) {
				t.Fatalf("the owner's retry: err=%v, want ErrConflict (the claim expired)", err)
			}
			expired := snapshotClaim(t, ctx, db, owner.ID)
			if expired.Status != "expired" || expired.History != before.History+1 {
				t.Fatalf("after the owner's retry: status=%q history=%d, want expired and %d",
					expired.Status, expired.History, before.History+1)
			}

			// And the key stays the claim's after it is terminal. A different request
			// is still a key reuse, and it does not create a new claim.
			if _, _, err := hold(c.other); !errors.Is(err, ErrIdempotency) {
				t.Fatalf("after expiry: err=%v, want ErrIdempotency (a key is single-use)", err)
			}
			if after := snapshotClaim(t, ctx, db, owner.ID); after != expired {
				t.Fatalf("the expired claim changed:\nbefore %+v\nafter  %+v", expired, after)
			}
			if n := claimsOf(t, ctx, db, org); n != 1 {
				t.Fatalf("%d claims, want 1", n)
			}
		})
	}
}
