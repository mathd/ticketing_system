//go:build smoke

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TKT-250. The allocation set carries a REVISION, and a replace that presents a stale
// one is refused instead of silently winning.
//
// ADR-024 accepted the gap in as many words — "Full-set PUT has no stale-write
// protection (If-Match); acceptable while allocation editing is single-operator" — and
// TKT-244 falsified the premise by shipping the first UI for the endpoint.
//
// WHAT THE POOL LOCK CANNOT DO, which is the whole reason this exists: it serializes the
// two transactions perfectly and that is not the problem. The staleness is in the READ
// that filled the form, which happened before the other writer committed. Ordering the
// writes correctly still applies the second one wholesale.
//
// Every assertion below reads the DATABASE back rather than trusting a return value:
// the regression this ticket names is what SURVIVES the write.

// allocationRows reads the pool's allocation set straight from the table, so an
// assertion cannot be satisfied by whatever the call under test chose to return.
func allocationRows(t *testing.T, ctx context.Context, st *Postgres, slot uuid.UUID) map[string]int32 {
	t.Helper()
	rows, err := st.db.QueryContext(ctx, `SELECT channel_code, cap FROM channel_allocations WHERE pool_id=$1`, slot)
	if err != nil {
		t.Fatalf("read allocation rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int32{}
	for rows.Next() {
		var code string
		var cap int32
		if err := rows.Scan(&code, &cap); err != nil {
			t.Fatalf("scan allocation row: %v", err)
		}
		out[code] = cap
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate allocation rows: %v", err)
	}
	return out
}

func currentRevision(t *testing.T, ctx context.Context, st *Postgres, org, slot uuid.UUID) int64 {
	t.Helper()
	a, err := st.StaffAvailability(ctx, org, slot)
	if err != nil {
		t.Fatalf("staff availability: %v", err)
	}
	return a.AllocationRevision
}

// The headline case: two operators, one slot. A reads, B reads, B commits, A submits.
//
// The fixture is built so a green result CANNOT be a coincidence — both directions of
// the full-set replace are represented, which is the trap the plan review called out:
//
//   - "b-only" exists only in B's committed set. If A's stale save applied, the replace
//     would have DELETED it, so its survival proves the write did not happen.
//   - "a-only" exists only in A's stale set. If A's save applied, it would have been
//     INSERTED, so its absence proves the same thing from the other side.
//
// One direction alone proves much less. A test asserting only that "a-only" is absent
// passes just as happily against an implementation that refuses every write, and a test
// asserting only that "b-only" survives passes against one that ignores the submitted
// set entirely.
func TestAStaleAllocationSaveIsRefusedAndChangesNothing(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)

	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "shared", Cap: 10}})

	// Both operators load the editor. They hold the SAME revision, which is the
	// situation this ticket is about: nothing is wrong yet.
	readByA := currentRevision(t, ctx, st, org, slot)
	readByB := currentRevision(t, ctx, st, org, slot)
	if readByA != readByB {
		t.Fatalf("two reads of an unchanged set disagreed: A=%d B=%d", readByA, readByB)
	}

	// B saves first and wins, legitimately: it presents the revision it read.
	if _, err := st.ReplaceChannelAllocations(ctx, org, slot,
		[]ChannelAllocation{{Channel: "shared", Cap: 25}, {Channel: "b-only", Cap: 5}}, &readByB); err != nil {
		t.Fatalf("B's save presented the current revision and must be accepted: %v", err)
	}

	// A now submits a set built before B committed. Its cap for "shared" differs from
	// B's, so applying it would be a visible lost update rather than a no-op.
	_, err := st.ReplaceChannelAllocations(ctx, org, slot,
		[]ChannelAllocation{{Channel: "shared", Cap: 99}, {Channel: "a-only", Cap: 5}}, &readByA)
	if !errors.Is(err, ErrAllocationRevisionMismatch) {
		t.Fatalf("A's stale save: got %v, want ErrAllocationRevisionMismatch", err)
	}
	// Still the sentinel every pre-existing caller matches on: the code is additive
	// information, not a re-classification.
	if !errors.Is(err, ErrConflict) {
		t.Error("the revision refusal stopped unwrapping to ErrConflict")
	}

	// The database is the assertion, in both directions.
	rows := allocationRows(t, ctx, st, slot)
	if _, present := rows["a-only"]; present {
		t.Error("a-only is present: the stale save was applied")
	}
	if _, present := rows["b-only"]; !present {
		t.Error("b-only is gone: the stale save's full-set replace deleted a row it never knew about")
	}
	if got := rows["shared"]; got != 25 {
		t.Errorf("shared cap=%d want 25 — B's committed value was overwritten by A's stale one", got)
	}
}

// After a refusal the set has NOT moved, so the revision must not have moved either.
//
// This is the property that makes the refusal usable rather than merely correct: an
// operator who reloads gets a revision that will be accepted. A revision bumped on a
// refused write would invalidate the reload too, and the editor could never converge.
func TestARefusedReplaceLeavesTheRevisionWhereItWas(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "shared", Cap: 10}})

	before := currentRevision(t, ctx, st, org, slot)
	stale := before - 1

	if _, err := st.ReplaceChannelAllocations(ctx, org, slot,
		[]ChannelAllocation{{Channel: "shared", Cap: 20}}, &stale); !errors.Is(err, ErrAllocationRevisionMismatch) {
		t.Fatalf("stale save: got %v want ErrAllocationRevisionMismatch", err)
	}
	if after := currentRevision(t, ctx, st, org, slot); after != before {
		t.Errorf("revision moved on a REFUSED write: %d -> %d; a reload would be stale on arrival", before, after)
	}

	// And the reload converges: presenting the current revision is accepted.
	if _, err := st.ReplaceChannelAllocations(ctx, org, slot,
		[]ChannelAllocation{{Channel: "shared", Cap: 20}}, &before); err != nil {
		t.Fatalf("the reloaded revision must be accepted: %v", err)
	}
}

// A successful replace moves the revision by exactly one.
//
// The expected values are derived from the RULE — "a successful replace advances the
// revision once" — not from watching what the code produced. So the arithmetic is
// written out rather than read back: after n successful replaces from a base of b, the
// revision is b+n, whatever the implementation happens to do.
func TestEachSuccessfulReplaceAdvancesTheRevisionExactlyOnce(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)

	base := currentRevision(t, ctx, st, org, slot)
	for n := 1; n <= 3; n++ {
		rev := currentRevision(t, ctx, st, org, slot)
		if _, err := st.ReplaceChannelAllocations(ctx, org, slot,
			[]ChannelAllocation{{Channel: "shared", Cap: int32(10 * n)}}, &rev); err != nil {
			t.Fatalf("replace %d: %v", n, err)
		}
		if got, want := currentRevision(t, ctx, st, org, slot), base+int64(n); got != want {
			t.Fatalf("after %d successful replaces: revision=%d want=%d", n, got, want)
		}
	}
}

// A state-identical replace still advances the revision.
//
// The counter is monotonic, not a change detector, and that is deliberate: a reader
// holding the old value has read a set that a writer has since re-committed. Treating
// "same content" as "no change" would need the canonical encoding of the set that the
// hash option was rejected for needing.
func TestAStateIdenticalReplaceStillAdvancesTheRevision(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)
	set := []ChannelAllocation{{Channel: "shared", Cap: 10}}
	mustReplace(t, ctx, st, org, slot, set)

	before := currentRevision(t, ctx, st, org, slot)
	rev := before
	if _, err := st.ReplaceChannelAllocations(ctx, org, slot, set, &rev); err != nil {
		t.Fatalf("identical replace: %v", err)
	}
	if after := currentRevision(t, ctx, st, org, slot); after != before+1 {
		t.Errorf("revision after an identical replace: %d want %d", after, before+1)
	}
}

// An OMITTED revision replaces unconditionally — the pre-TKT-250 behaviour the shared
// internal token keeps (ADR-057 distinguishes the two credentials; the API layer decides
// which callers may omit it).
//
// This is the standing proof that the eight X-Internal-Token smoke call sites did not
// need to change. If it ever fails, the compatibility half of this ticket is broken.
func TestAnOmittedRevisionReplacesUnconditionally(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "shared", Cap: 10}})

	// Move the revision so ANY value a caller might have held is now stale. An
	// unconditional replace must not care.
	before := currentRevision(t, ctx, st, org, slot)
	if _, err := st.ReplaceChannelAllocations(ctx, org, slot,
		[]ChannelAllocation{{Channel: "shared", Cap: 20}}, &before); err != nil {
		t.Fatal(err)
	}

	if _, err := st.ReplaceChannelAllocations(ctx, org, slot,
		[]ChannelAllocation{{Channel: "shared", Cap: 30}}, nil); err != nil {
		t.Fatalf("an omitted revision must replace unconditionally: %v", err)
	}
	if got := allocationRows(t, ctx, st, slot)["shared"]; got != 30 {
		t.Errorf("shared cap=%d want 30 — the unconditional replace did not apply", got)
	}
}

// A revision of ZERO is a real revision, not "absent".
//
// Guards the pointer in the request struct. A non-pointer int64 would read an omitted
// field as 0, which is the legitimate revision of a pool nobody has edited yet — so a
// stale-by-omission save would be admitted at exactly the moment the set is most likely
// to be edited for the first time. Here the pool HAS been edited, so 0 is stale and must
// be refused; the difference between 0 and absent is the whole assertion.
func TestRevisionZeroIsPresentedAsAValueNotAsAbsence(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)

	if fresh := currentRevision(t, ctx, st, org, slot); fresh != 0 {
		t.Fatalf("a freshly provisioned pool starts at revision %d, want 0", fresh)
	}
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "shared", Cap: 10}})

	zero := int64(0)
	if _, err := st.ReplaceChannelAllocations(ctx, org, slot,
		[]ChannelAllocation{{Channel: "shared", Cap: 99}}, &zero); !errors.Is(err, ErrAllocationRevisionMismatch) {
		t.Fatalf("revision 0 after an edit is STALE: got %v want ErrAllocationRevisionMismatch", err)
	}
}

// Ordinary trading does NOT move the allocation revision.
//
// The counter's value is that an operator's open form survives a busy on-sale. If a
// confirm, a refund or a capacity adjustment bumped it, every save during trading would
// be refused and the editor would be unusable — which is precisely why `updated_at` on
// inventory_pools was rejected as the revision: it moves on all of those.
func TestTradingDoesNotMoveTheAllocationRevision(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := provisioned(t, ctx, st, 100)
	mustReplace(t, ctx, st, org, slot, []ChannelAllocation{{Channel: "shared", Cap: 50}})

	before := currentRevision(t, ctx, st, org, slot)

	// A real sale on the channel, driven through the production transitions so the
	// pool's counters move exactly as they do in production.
	confirmedClaim(t, ctx, st, org, slot, 4, "shared", "revision-trade-1")
	if after := currentRevision(t, ctx, st, org, slot); after != before {
		t.Errorf("a confirmed sale moved the allocation revision: %d -> %d", before, after)
	}

	// A capacity adjustment: same pool row, different concern.
	if _, _, err := st.AdjustCapacity(ctx, org, slot, 120, "operator", "more room", "revision-trade-cap"); err != nil {
		t.Fatalf("adjust capacity: %v", err)
	}
	if after := currentRevision(t, ctx, st, org, slot); after != before {
		t.Errorf("a capacity adjustment moved the allocation revision: %d -> %d", before, after)
	}

	// And the revision read before all that trading is still accepted.
	if _, err := st.ReplaceChannelAllocations(ctx, org, slot,
		[]ChannelAllocation{{Channel: "shared", Cap: 50}}, &before); err != nil {
		t.Fatalf("a form opened before the trading must still save: %v", err)
	}
}
