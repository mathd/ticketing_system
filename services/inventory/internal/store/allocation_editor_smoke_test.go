//go:build smoke

package store

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TKT-244. The allocation editor reads a slot's allocations, lets an operator change a
// cap, and PUTs the whole set back — because ReplaceChannelAllocations is a full-set
// atomic replace under the pool lock (ADR-024), which DELETEs every row and re-INSERTs
// from what was submitted.
//
// That makes every field the editor does not carry a field the editor DESTROYS. The
// dangerous one is sold_by: TKT-246 made it load-bearing (inventory refuses a claim whose
// seller does not match, judged under the pool row lock), so silently dropping it turns a
// reseller's bound stock back into public stock. That is an AUTHORIZATION regression, not
// a cosmetic data loss, and it is invisible in a screenshot.
//
// The editor cannot preserve what it cannot read, and before this ticket the staff
// availability read returned neither requires_code nor sold_by. This test is the store
// half: the read must expose every field the write consumes, or round-tripping is
// impossible by construction.
func TestTheStaffReadExposesEveryFieldTheWriteConsumes(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)

	reseller := uuid.New()
	opens := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	closes := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	release := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)

	// One allocation carrying EVERY optional field at a non-default value. A fixture
	// that left any of them at its zero value could not tell preservation from a
	// coincidence — the defaults are what a dropping implementation produces.
	original := ChannelAllocation{
		Channel:      "reseller-acme",
		Cap:          40,
		ReleaseAt:    &release,
		OpensAt:      &opens,
		ClosesAt:     &closes,
		RequiresCode: true,
		SoldBy:       &reseller,
	}
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{original})

	a, err := st.StaffAvailability(ctx, org, slot)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Channels) != 1 {
		t.Fatalf("channels=%d want 1: %+v", len(a.Channels), a.Channels)
	}
	got := a.Channels[0]

	// The two fields this ticket adds. Asserted individually so a failure names which.
	if !got.RequiresCode {
		t.Error("the staff read does not report requires_code: an editor cannot preserve " +
			"a presale gate it cannot see, and re-saving would silently ungate the channel")
	}
	if got.SoldBy == nil {
		t.Fatal("the staff read does not report sold_by: an editor cannot preserve a " +
			"seller binding it cannot see, and re-saving would return a reseller's stock " +
			"to the public pool (TKT-246 authorization regression)")
	}
	if *got.SoldBy != reseller {
		t.Errorf("sold_by=%v want %v", *got.SoldBy, reseller)
	}

	// The fields that already round-tripped, re-asserted here so this test fails if a
	// future change drops one while adding the two above.
	if got.ReleaseAt == nil || !got.ReleaseAt.Equal(release) {
		t.Errorf("release_at=%v want %v", got.ReleaseAt, release)
	}
	if got.OpensAt == nil || !got.OpensAt.Equal(opens) {
		t.Errorf("opens_at=%v want %v", got.OpensAt, opens)
	}
	if got.ClosesAt == nil || !got.ClosesAt.Equal(closes) {
		t.Errorf("closes_at=%v want %v", got.ClosesAt, closes)
	}
}

// The editor's actual round trip, end to end through the store: read the set, change ONE
// cap the way an operator would, write the whole set back, and re-read. Everything the
// operator did not touch must be exactly as it was.
//
// The invariant, stated without naming the implementation: EDITING ONE FIELD OF ONE
// ALLOCATION CHANGES THAT FIELD AND NOTHING ELSE.
func TestAnEditorRoundTripChangesOnlyWhatTheOperatorChanged(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)

	reseller := uuid.New()
	opens := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	closes := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	before := []ChannelAllocation{
		{Channel: "reseller-acme", Cap: 40, OpensAt: &opens, ClosesAt: &closes, RequiresCode: true, SoldBy: &reseller},
		{Channel: "presale", Cap: 20, RequiresCode: true},
	}
	mustReplace(t, ctx, st, org, slot, before)

	// What the editor does: read the current set, and rebuild the write from it.
	read, err := st.StaffAvailability(ctx, org, slot)
	if err != nil {
		t.Fatal(err)
	}
	submitted := make([]ChannelAllocation, 0, len(read.Channels))
	for _, c := range read.Channels {
		alloc := ChannelAllocation{
			Channel:      c.Channel,
			Cap:          c.Cap,
			ReleaseAt:    c.ReleaseAt,
			OpensAt:      c.OpensAt,
			ClosesAt:     c.ClosesAt,
			RequiresCode: c.RequiresCode,
			SoldBy:       c.SoldBy,
		}
		if c.Channel == "reseller-acme" {
			alloc.Cap = 50 // the operator's single edit
		}
		submitted = append(submitted, alloc)
	}
	mustReplace(t, ctx, st, org, slot, submitted)

	after, err := st.StaffAvailability(ctx, org, slot)
	if err != nil {
		t.Fatal(err)
	}
	byChannel := map[string]ChannelAvailability{}
	for _, c := range after.Channels {
		byChannel[c.Channel] = c
	}

	acme, ok := byChannel["reseller-acme"]
	if !ok {
		t.Fatal("reseller-acme vanished across the round trip")
	}
	if acme.Cap != 50 {
		t.Errorf("cap=%d want 50: the operator's edit did not take", acme.Cap)
	}
	if acme.SoldBy == nil || *acme.SoldBy != reseller {
		t.Errorf("sold_by=%v want %v — a cap edit unbound the allocation, "+
			"returning a reseller's stock to the public pool", acme.SoldBy, reseller)
	}
	if !acme.RequiresCode {
		t.Error("requires_code was cleared by an unrelated cap edit: the channel is now ungated")
	}
	if acme.OpensAt == nil || !acme.OpensAt.Equal(opens) {
		t.Errorf("opens_at=%v want %v", acme.OpensAt, opens)
	}
	if acme.ClosesAt == nil || !acme.ClosesAt.Equal(closes) {
		t.Errorf("closes_at=%v want %v", acme.ClosesAt, closes)
	}

	presale, ok := byChannel["presale"]
	if !ok {
		t.Fatal("presale vanished across the round trip")
	}
	if presale.Cap != 20 || !presale.RequiresCode {
		t.Errorf("the untouched row changed: cap=%d requires_code=%v want 20/true",
			presale.Cap, presale.RequiresCode)
	}
}

// The two refusals the editor must attribute to a field carry a machine-readable identity
// from the store (TKT-244). The API tier turns these into coded 409s; this asserts the
// mechanism at the tier that OWNS it — the store is where the decision is made, under the
// pool lock, and an assertion one tier up would prove only that the handler and the store
// agree about a value the handler never computes.
func TestTheTwoAllocationRefusalsAreDistinguishableAndNameTheirChannel(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)

	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 40}})

	// Caps summing above pool capacity.
	_, err := st.ReplaceChannelAllocations(ctx, org, slot, []ChannelAllocation{
		{Channel: "presale", Cap: 60}, {Channel: "reseller-acme", Cap: 60},
	})
	if !errors.Is(err, ErrAllocationCapsExceedCapacity) {
		t.Fatalf("over-capacity: got %v want ErrAllocationCapsExceedCapacity", err)
	}
	// Still the sentinel every pre-existing caller matches on.
	if !errors.Is(err, ErrUnavailable) {
		t.Error("the typed over-capacity refusal stopped unwrapping to ErrUnavailable")
	}
	// It must NOT masquerade as the channel-specific refusal, or the editor would
	// attribute a whole-set problem to one arbitrary row.
	var named interface{ Channel() string }
	if errors.As(err, &named) {
		t.Errorf("the over-capacity refusal names channel %q; the sum is a property of "+
			"the whole set and belongs on the total", named.Channel())
	}

	// A cap below that channel's live consumption.
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 10, 0, "", "presale", "editor-consume"); err != nil {
		t.Fatal(err)
	}
	_, err = st.ReplaceChannelAllocations(ctx, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 5}})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("below-consumption: got %v want (wrapping) ErrConflict", err)
	}
	if !errors.As(err, &named) {
		t.Fatal("the below-consumption refusal does not name its channel, so the editor " +
			"cannot put the message beside the row the operator must fix")
	}
	if named.Channel() != "presale" {
		t.Errorf("channel=%q want %q", named.Channel(), "presale")
	}

	// The channel named is the OFFENDING one, not merely the first submitted. With two
	// rows where the SECOND is the violator, a implementation returning the first would
	// point the operator at a row that is fine.
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "presale", Cap: 40}, {Channel: "reseller-acme", Cap: 40}})
	if _, _, err := st.CreateHold(ctx, org, slot, uuid.Nil, 10, 0, "", "reseller-acme", "editor-consume-2"); err != nil {
		t.Fatal(err)
	}
	_, err = st.ReplaceChannelAllocations(ctx, org, slot, []ChannelAllocation{
		{Channel: "presale", Cap: 40}, {Channel: "reseller-acme", Cap: 5},
	})
	if !errors.As(err, &named) {
		t.Fatalf("below-consumption on the second row: got %v, which names no channel", err)
	}
	if named.Channel() != "reseller-acme" {
		t.Errorf("channel=%q want %q — the refusal named a row that is not the violator",
			named.Channel(), "reseller-acme")
	}
}
