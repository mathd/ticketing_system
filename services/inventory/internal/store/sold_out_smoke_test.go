//go:build smoke

package store

// TKT-341 / ADR-082: which GA hold refusals are a sellout.
//
// ErrSoldOut wraps ErrUnavailable, so an assertion on ErrUnavailable alone passes for
// both outcomes and cannot see the classification. Every case here asserts
// errors.Is(err, ErrSoldOut) positively or negatively.
//
// Each fixture passes the guards that come before the one it names, so the refusal is
// the one the name says. The comment on each case names the mutation it catches.

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type soldOutCase struct {
	name string
	// ttl is the buyer hold TTL. Negative means every buyer hold is born expired.
	ttl     time.Duration
	setup   func(t *testing.T, ctx context.Context, st *Postgres, db *sql.DB, org, slot uuid.UUID)
	attempt func(ctx context.Context, st *Postgres, org, slot uuid.UUID) error
	// want is "sold_out", "generic" (ErrUnavailable that is not ErrSoldOut), "ok", or
	// the name of another sentinel.
	want string
	// smaller, when set, is a public quantity that must succeed after the refusal: a
	// partial shortage is not a sellout because the buyer can buy fewer now.
	smaller int32
}

func soldOutPublicHold(qty int32) func(ctx context.Context, st *Postgres, org, slot uuid.UUID) error {
	return func(ctx context.Context, st *Postgres, org, slot uuid.UUID) error {
		_, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, qty, 0, "", "", "attempt-"+uuid.NewString())
		return err
	}
}

func soldOutChannelHold(qty int32, channel string, opts ...HoldOption) func(ctx context.Context, st *Postgres, org, slot uuid.UUID) error {
	return func(ctx context.Context, st *Postgres, org, slot uuid.UUID) error {
		_, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, qty, 0, "", channel, "attempt-"+uuid.NewString(), opts...)
		return err
	}
}

func soldOutSeedHold(t *testing.T, ctx context.Context, st *Postgres, org, slot uuid.UUID, qty int32, channel string, opts ...HoldOption) Claim {
	t.Helper()
	c, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, qty, 0, "", channel, "seed-"+uuid.NewString(), opts...)
	if err != nil {
		t.Fatalf("seed hold of %d on channel %q: %v", qty, channel, err)
	}
	return c
}

func soldOutSeedOperational(t *testing.T, ctx context.Context, st *Postgres, org, slot uuid.UUID, qty int32) {
	t.Helper()
	if _, _, err := st.PlaceOperationalHold(ctx, org, slot, qty, "house", "front-of-house", "staff:amy", "fixture", "op-"+uuid.NewString()); err != nil {
		t.Fatalf("seed operational hold of %d: %v", qty, err)
	}
}

func soldOutClassify(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrSoldOut):
		return "sold_out"
	case errors.Is(err, ErrChannelWindowClosed):
		return "channel_window_closed"
	case errors.Is(err, ErrPresaleCodeInvalid):
		return "presale_code_invalid"
	case errors.Is(err, ErrSlotClosed):
		return "slot_closed"
	case errors.Is(err, ErrSlotArchived):
		return "slot_archived"
	case errors.Is(err, ErrUnavailable):
		return "generic"
	}
	return "unexpected: " + err.Error()
}

func soldOutClaimCount(t *testing.T, ctx context.Context, db *sql.DB, slot uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM claims WHERE pool_id=$1`, slot).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestGAHoldRefusalsSeparateASelloutFromOtherCapacityRefusals(t *testing.T) {
	cases := []soldOutCase{
		// --- Zero public headroom: sellout. ---

		// Mutation: return the generic refusal when headroom is zero.
		{name: "pool exhausted by confirmed claims", ttl: time.Minute, want: "sold_out",
			setup: func(t *testing.T, ctx context.Context, st *Postgres, _ *sql.DB, org, slot uuid.UUID) {
				c := soldOutSeedHold(t, ctx, st, org, slot, 10, "")
				if _, err := st.Transition(ctx, org, c.ID, "finalizing"); err != nil {
					t.Fatal(err)
				}
				if _, err := st.Transition(ctx, org, c.ID, "confirmed"); err != nil {
					t.Fatal(err)
				}
			}, attempt: soldOutPublicHold(1)},
		// Mutation: leave live held demand out of the public headroom.
		{name: "pool exhausted by live buyer holds", ttl: time.Minute, want: "sold_out",
			setup: func(t *testing.T, ctx context.Context, st *Postgres, _ *sql.DB, org, slot uuid.UUID) {
				soldOutSeedHold(t, ctx, st, org, slot, 10, "")
			}, attempt: soldOutPublicHold(1)},
		// A finalizing claim is live demand (liveClaims).
		{name: "pool exhausted by a finalizing claim", ttl: time.Minute, want: "sold_out",
			setup: func(t *testing.T, ctx context.Context, st *Postgres, _ *sql.DB, org, slot uuid.UUID) {
				c := soldOutSeedHold(t, ctx, st, org, slot, 10, "")
				if _, err := st.Transition(ctx, org, c.ID, "finalizing"); err != nil {
					t.Fatal(err)
				}
			}, attempt: soldOutPublicHold(1)},
		// An operational hold never expires and is live demand.
		{name: "pool exhausted by an operational hold", ttl: time.Minute, want: "sold_out",
			setup: func(t *testing.T, ctx context.Context, st *Postgres, _ *sql.DB, org, slot uuid.UUID) {
				soldOutSeedOperational(t, ctx, st, org, slot, 10)
			}, attempt: soldOutPublicHold(1)},
		// Physical headroom 4, all of it reserved for a channel.
		// Mutation: leave the channel reservation out of the public headroom.
		{name: "physical headroom fully reserved for a channel", ttl: time.Minute, want: "sold_out",
			setup: func(t *testing.T, ctx context.Context, st *Postgres, _ *sql.DB, org, slot uuid.UUID) {
				mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 4}})
				soldOutSeedHold(t, ctx, st, org, slot, 6, "")
			}, attempt: soldOutPublicHold(1)},
		// Global headroom 2 is below the requested 3, and the public headroom is 0.
		// Mutation: let the global bound refuse before the classification (the HEAD
		// shape, store.go's former early return), which answers generic.
		{name: "global headroom below quantity while public headroom is zero", ttl: time.Minute, want: "sold_out",
			setup: func(t *testing.T, ctx context.Context, st *Postgres, _ *sql.DB, org, slot uuid.UUID) {
				mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 2}})
				soldOutSeedHold(t, ctx, st, org, slot, 8, "")
			}, attempt: soldOutPublicHold(3)},
		// A closed sales window does not release the reservation (ADR-054 §B).
		{name: "reservation of a channel whose window is not open yet", ttl: time.Minute, want: "sold_out",
			setup: func(t *testing.T, ctx context.Context, st *Postgres, db *sql.DB, org, slot uuid.UUID) {
				mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 4}})
				setWindow(t, ctx, db, slot, "presale", "now() + interval '1 day'", "now() + interval '2 days'")
				soldOutSeedHold(t, ctx, st, org, slot, 6, "")
			}, attempt: soldOutPublicHold(1)},
		// A draining cut to 4 under 6 live held: the headroom is -2, not 0.
		// Mutation: test "== 0" instead of "<= 0".
		{name: "draining capacity cut leaves negative headroom", ttl: time.Minute, want: "sold_out",
			setup: func(t *testing.T, ctx context.Context, st *Postgres, _ *sql.DB, org, slot uuid.UUID) {
				soldOutSeedHold(t, ctx, st, org, slot, 6, "")
				if _, _, err := st.AdjustCapacity(ctx, org, slot, 4, "staff:amy", "fixture", "cut-"+uuid.NewString()); err != nil {
					t.Fatal(err)
				}
			}, attempt: soldOutPublicHold(1)},

		// --- Partial shortage: not a sellout. ---

		// Mutation: classify every public shortage as a sellout.
		{name: "positive physical headroom below quantity", ttl: time.Minute, want: "generic", smaller: 2,
			setup: func(t *testing.T, ctx context.Context, st *Postgres, _ *sql.DB, org, slot uuid.UUID) {
				soldOutSeedHold(t, ctx, st, org, slot, 8, "")
			}, attempt: soldOutPublicHold(3)},
		// Public headroom 1 because a channel reserves 4.
		{name: "positive public headroom below quantity because of a reservation", ttl: time.Minute, want: "generic", smaller: 1,
			setup: func(t *testing.T, ctx context.Context, st *Postgres, _ *sql.DB, org, slot uuid.UUID) {
				mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 4}})
				soldOutSeedHold(t, ctx, st, org, slot, 5, "")
			}, attempt: soldOutPublicHold(2)},
		// Expired buyer holds are not demand: the pool is sellable again.
		{name: "expired buyer holds give their capacity back", ttl: -time.Second, want: "ok",
			setup: func(t *testing.T, ctx context.Context, st *Postgres, _ *sql.DB, org, slot uuid.UUID) {
				soldOutSeedHold(t, ctx, st, org, slot, 10, "")
			}, attempt: soldOutPublicHold(1)},

		// --- Channel refusals: uniform, never a sellout (TKT-246). ---

		// Mutation: classify a channel request on an exhausted pool as a sellout.
		{name: "authorized channel request on an exhausted pool", ttl: time.Minute, want: "generic",
			setup: func(t *testing.T, ctx context.Context, st *Postgres, _ *sql.DB, org, slot uuid.UUID) {
				mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 4}})
				soldOutSeedHold(t, ctx, st, org, slot, 6, "")
				soldOutSeedOperational(t, ctx, st, org, slot, 4)
			}, attempt: soldOutChannelHold(1, "presale")},
		{name: "channel cap exhausted with pool headroom", ttl: time.Minute, want: "generic",
			setup: func(t *testing.T, ctx context.Context, st *Postgres, _ *sql.DB, org, slot uuid.UUID) {
				mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 2}})
				soldOutSeedHold(t, ctx, st, org, slot, 2, "presale")
			}, attempt: soldOutChannelHold(1, "presale")},
		{name: "channel with no allocation", ttl: time.Minute, want: "generic",
			setup:   func(t *testing.T, ctx context.Context, st *Postgres, _ *sql.DB, org, slot uuid.UUID) {},
			attempt: soldOutChannelHold(1, "nosuch")},
		{name: "channel with no allocation on an exhausted pool", ttl: time.Minute, want: "generic",
			setup: func(t *testing.T, ctx context.Context, st *Postgres, _ *sql.DB, org, slot uuid.UUID) {
				soldOutSeedHold(t, ctx, st, org, slot, 10, "")
			}, attempt: soldOutChannelHold(1, "nosuch")},
		{name: "seller mismatch with pool and cap headroom", ttl: time.Minute, want: "generic",
			setup: func(t *testing.T, ctx context.Context, st *Postgres, db *sql.DB, org, slot uuid.UUID) {
				mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "reseller-acme", Cap: 5}})
				bindSeller(t, ctx, db, slot, "reseller-acme", uuid.New())
			}, attempt: soldOutChannelHold(1, "reseller-acme", WithReseller(uuid.New()))},
		{name: "seller mismatch on an exhausted pool", ttl: time.Minute, want: "generic",
			setup: func(t *testing.T, ctx context.Context, st *Postgres, db *sql.DB, org, slot uuid.UUID) {
				mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "reseller-acme", Cap: 5}})
				bindSeller(t, ctx, db, slot, "reseller-acme", uuid.New())
				soldOutSeedHold(t, ctx, st, org, slot, 5, "")
				soldOutSeedOperational(t, ctx, st, org, slot, 5)
			}, attempt: soldOutChannelHold(1, "reseller-acme", WithReseller(uuid.New()))},

		// --- Earlier refusals keep their precedence over a full pool. ---

		{name: "closed channel window on an exhausted pool", ttl: time.Minute, want: "channel_window_closed",
			setup: func(t *testing.T, ctx context.Context, st *Postgres, db *sql.DB, org, slot uuid.UUID) {
				mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 4}})
				setWindow(t, ctx, db, slot, "presale", "now() + interval '1 day'", "now() + interval '2 days'")
				soldOutSeedHold(t, ctx, st, org, slot, 6, "")
				soldOutSeedOperational(t, ctx, st, org, slot, 4)
			}, attempt: soldOutChannelHold(1, "presale")},
		{name: "invalid presale code on an exhausted pool", ttl: time.Minute, want: "presale_code_invalid",
			setup: func(t *testing.T, ctx context.Context, st *Postgres, _ *sql.DB, org, slot uuid.UUID) {
				mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 4, RequiresCode: true}})
				soldOutSeedHold(t, ctx, st, org, slot, 6, "")
				soldOutSeedOperational(t, ctx, st, org, slot, 4)
			}, attempt: soldOutChannelHold(1, "presale", WithPresaleCode("WRONG"))},
		{name: "closed slot on an exhausted pool", ttl: time.Minute, want: "slot_closed",
			setup: func(t *testing.T, ctx context.Context, st *Postgres, db *sql.DB, org, slot uuid.UUID) {
				soldOutSeedHold(t, ctx, st, org, slot, 10, "")
				if _, err := db.ExecContext(ctx, `UPDATE inventory_pools SET closure_status='closed' WHERE slot_id=$1`, slot); err != nil {
					t.Fatal(err)
				}
			}, attempt: soldOutPublicHold(1)},
		{name: "archived slot on an exhausted pool", ttl: time.Minute, want: "slot_archived",
			setup: func(t *testing.T, ctx context.Context, st *Postgres, db *sql.DB, org, slot uuid.UUID) {
				soldOutSeedHold(t, ctx, st, org, slot, 10, "")
				if _, err := db.ExecContext(ctx, `UPDATE inventory_pools SET lifecycle_status='archived' WHERE slot_id=$1`, slot); err != nil {
					t.Fatal(err)
				}
			}, attempt: soldOutPublicHold(1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, st, db := storeForTest(t, tc.ttl)
			org, slot := provisioned(t, ctx, st, 10)
			tc.setup(t, ctx, st, db, org, slot)
			before := soldOutClaimCount(t, ctx, db, slot)
			err := tc.attempt(ctx, st, org, slot)
			if got := soldOutClassify(err); got != tc.want {
				t.Fatalf("refusal = %s (%v), want %s", got, err, tc.want)
			}
			if err != nil {
				// A refused hold writes nothing.
				if after := soldOutClaimCount(t, ctx, db, slot); after != before {
					t.Fatalf("a refused hold changed the claim count from %d to %d", before, after)
				}
			}
			if tc.smaller > 0 {
				if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, tc.smaller, 0, "", "", "smaller-"+uuid.NewString()); err != nil {
					t.Fatalf("a public hold of %d must fit after a partial refusal: %v", tc.smaller, err)
				}
			}
		})
	}
}
