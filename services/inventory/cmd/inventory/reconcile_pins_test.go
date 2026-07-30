package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/google/uuid"

	"ticketing/services/inventory/internal/consumer"
	"ticketing/services/inventory/internal/store"
)

// fakeCatalogPins models catalog's pin table behind the two calls the reconciler makes:
// a keyset-paged list and a batch unpin. It APPLIES the unpins to its own state, so a test
// can assert the exact surviving set rather than the weaker "these deletes were requested".
type fakeCatalogPins struct {
	pins      []consumer.SeatPin
	pageSizes []int
	unpinCall int
	unpinErr  func(pinnedBy string) error
}

func (f *fakeCatalogPins) list(_ context.Context, after uuid.UUID, limit int) ([]consumer.SeatPin, error) {
	f.pageSizes = append(f.pageSizes, limit)
	sort.Slice(f.pins, func(i, j int) bool { return f.pins[i].ID.String() < f.pins[j].ID.String() })
	out := []consumer.SeatPin{}
	for _, p := range f.pins {
		if after != uuid.Nil && p.ID.String() <= after.String() {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeCatalogPins) unpinSeats(_ context.Context, org, seatMapID uuid.UUID, seats []string, pinnedBy string) error {
	f.unpinCall++
	if f.unpinErr != nil {
		if err := f.unpinErr(pinnedBy); err != nil {
			return err
		}
	}
	drop := map[string]bool{}
	for _, s := range seats {
		drop[s] = true
	}
	kept := f.pins[:0]
	for _, p := range f.pins {
		if p.OrganizerID == org && p.SeatMapID == seatMapID && p.PinnedBy == pinnedBy && drop[p.SeatIdentity] {
			continue
		}
		kept = append(kept, p)
	}
	f.pins = kept
	return nil
}

// survivors is the set-equality observation the COS is stated in: which pins are still
// there afterwards, keyed pinned_by/seat.
func (f *fakeCatalogPins) survivors() []string {
	out := []string{}
	for _, p := range f.pins {
		out = append(out, p.PinnedBy+"|"+p.SeatIdentity)
	}
	sort.Strings(out)
	return out
}

type livenessRecorder struct {
	states map[uuid.UUID]store.SeatClaimState
	asked  []uuid.UUID
	err    error
	omit   uuid.UUID // when set, this id is left out of the answer
}

func (l *livenessRecorder) verdicts(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]store.SeatClaimState, error) {
	l.asked = append(l.asked, ids...)
	if l.err != nil {
		return nil, l.err
	}
	out := map[uuid.UUID]store.SeatClaimState{}
	for _, id := range ids {
		if id == l.omit {
			continue
		}
		if s, ok := l.states[id]; ok {
			out[id] = s
		} else {
			out[id] = store.SeatClaimUnknown
		}
	}
	return out, nil
}

// seatMapA is the representative family version every fixture pin hangs off.
var (
	orgA     = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	seatMapA = uuid.MustParse("22222222-2222-2222-2222-222222222222")
)

func pin(seq int, seat, pinnedBy string) consumer.SeatPin {
	return consumer.SeatPin{
		ID:           uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-%012d", seq)),
		OrganizerID:  orgA,
		SeatMapID:    seatMapA,
		SeatIdentity: seat,
		PinnedBy:     pinnedBy,
	}
}

// TestReconcilePinsRemovesExactlyDeadHoldPins is the COS, stated as set equality: the dead
// hold pins go and NOTHING else does. It asserts what SURVIVES — a test that only checked
// which deletes were requested would pass while over-deleting.
func TestReconcilePinsRemovesExactlyDeadHoldPins(t *testing.T) {
	expired := uuid.New()
	released := uuid.New()
	held := uuid.New()
	finalizing := uuid.New()
	confirmed := uuid.New()
	unknown := uuid.New()

	cat := &fakeCatalogPins{pins: []consumer.SeatPin{
		pin(1, "Orchestra/A/1", "hold:"+expired.String()),
		pin(2, "Orchestra/A/2", "hold:"+released.String()),
		pin(3, "Orchestra/A/3", "hold:"+held.String()),
		pin(4, "Orchestra/A/4", "hold:"+finalizing.String()),
		pin(5, "Orchestra/A/5", "hold:"+confirmed.String()),
		pin(6, "Orchestra/A/6", "hold:"+unknown.String()),
		pin(7, "Orchestra/A/7", "hold:not-a-uuid"),
		pin(8, "Orchestra/A/8", "sale:"+uuid.New().String()),
		pin(9, "Orchestra/A/9", "house:kill"),
	}}
	live := &livenessRecorder{states: map[uuid.UUID]store.SeatClaimState{
		expired:    store.SeatClaimDead,
		released:   store.SeatClaimDead,
		held:       store.SeatClaimLive,
		finalizing: store.SeatClaimLive,
		confirmed:  store.SeatClaimLive,
		unknown:    store.SeatClaimUnknown,
	}}

	stats, err := pinReconciler{listPins: cat.list, liveness: live.verdicts, unpin: cat.unpinSeats}.run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	want := []string{
		"hold:" + confirmed.String() + "|Orchestra/A/5",
		"hold:" + finalizing.String() + "|Orchestra/A/4",
		"hold:" + held.String() + "|Orchestra/A/3",
		"hold:" + unknown.String() + "|Orchestra/A/6",
		"hold:not-a-uuid|Orchestra/A/7",
		"house:kill|Orchestra/A/9",
		"sale:" + cat.saleRef() + "|Orchestra/A/8",
	}
	sort.Strings(want)
	got := cat.survivors()
	if len(got) != len(want) {
		t.Fatalf("survivors = %v\nwant %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("survivors = %v\nwant %v", got, want)
		}
	}
	if stats.Reclaimed != 2 || stats.Live != 3 || stats.Unknown != 1 || stats.Malformed != 1 || stats.Other != 2 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.Scanned != 9 {
		t.Fatalf("scanned = %d want 9", stats.Scanned)
	}
	// A malformed or non-hold reference must never even be put to the liveness authority.
	for _, id := range live.asked {
		if id == uuid.Nil {
			t.Fatal("liveness asked about a nil uuid")
		}
	}
	if len(live.asked) != 6 {
		t.Fatalf("liveness asked about %d claims, want the 6 well-formed hold refs", len(live.asked))
	}
}

// saleRef digs the sale pin's reference out of the fixture so the expectation above does not
// have to hardcode a random uuid.
func (f *fakeCatalogPins) saleRef() string {
	for _, p := range f.pins {
		if len(p.PinnedBy) > 5 && p.PinnedBy[:5] == "sale:" {
			return p.PinnedBy[5:]
		}
	}
	return ""
}

// TestReconcilePinsDrainsPagesAndRerunIsNoop covers the drain loop and the idempotency COS.
func TestReconcilePinsDrainsPagesAndRerunIsNoop(t *testing.T) {
	dead := uuid.New()
	pins := []consumer.SeatPin{}
	for i := 1; i <= reconcilePinPageSize+7; i++ {
		pins = append(pins, pin(i, fmt.Sprintf("Orchestra/A/%d", i), "hold:"+dead.String()))
	}
	keep := uuid.New()
	pins = append(pins, pin(reconcilePinPageSize+8, "Balcony/B/1", "hold:"+keep.String()))
	cat := &fakeCatalogPins{pins: pins}
	live := &livenessRecorder{states: map[uuid.UUID]store.SeatClaimState{dead: store.SeatClaimDead, keep: store.SeatClaimLive}}

	r := pinReconciler{listPins: cat.list, liveness: live.verdicts, unpin: cat.unpinSeats}
	stats, err := r.run(context.Background())
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if stats.Reclaimed != reconcilePinPageSize+7 {
		t.Fatalf("reclaimed = %d want %d", stats.Reclaimed, reconcilePinPageSize+7)
	}
	for _, size := range cat.pageSizes {
		if size != reconcilePinPageSize {
			t.Fatalf("page sizes = %v, every request must use the bound", cat.pageSizes)
		}
	}
	if len(cat.pageSizes) < 2 {
		t.Fatalf("page sizes = %v, want the drain to need more than one page", cat.pageSizes)
	}

	before := cat.survivors()
	callsBefore := cat.unpinCall
	stats2, err := r.run(context.Background())
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if stats2.Reclaimed != 0 {
		t.Fatalf("rerun reclaimed %d, want 0 (idempotent)", stats2.Reclaimed)
	}
	if cat.unpinCall != callsBefore {
		t.Fatalf("rerun issued %d unpins, want none", cat.unpinCall-callsBefore)
	}
	after := cat.survivors()
	if len(before) != len(after) || (len(before) == 1 && before[0] != after[0]) {
		t.Fatalf("rerun changed the survivor set: %v -> %v", before, after)
	}
}

// TestReconcilePinsFailsClosedWithoutCompleteVerdicts: an incomplete or failed liveness
// answer must reclaim nothing from that page. A missing verdict is not "dead".
func TestReconcilePinsFailsClosedWithoutCompleteVerdicts(t *testing.T) {
	dead := uuid.New()
	silent := uuid.New()
	base := []consumer.SeatPin{
		pin(1, "Orchestra/A/1", "hold:"+dead.String()),
		pin(2, "Orchestra/A/2", "hold:"+silent.String()),
	}

	t.Run("omitted verdict", func(t *testing.T) {
		cat := &fakeCatalogPins{pins: append([]consumer.SeatPin{}, base...)}
		live := &livenessRecorder{states: map[uuid.UUID]store.SeatClaimState{dead: store.SeatClaimDead}, omit: silent}
		if _, err := (pinReconciler{listPins: cat.list, liveness: live.verdicts, unpin: cat.unpinSeats}).run(context.Background()); err == nil {
			t.Fatal("an incomplete liveness answer must fail the run")
		}
		if cat.unpinCall != 0 {
			t.Fatalf("unpinned %d groups from a page with an unclassified pin", cat.unpinCall)
		}
	})

	t.Run("liveness error", func(t *testing.T) {
		cat := &fakeCatalogPins{pins: append([]consumer.SeatPin{}, base...)}
		live := &livenessRecorder{err: errors.New("db down")}
		if _, err := (pinReconciler{listPins: cat.list, liveness: live.verdicts, unpin: cat.unpinSeats}).run(context.Background()); err == nil {
			t.Fatal("a liveness failure must fail the run")
		}
		if cat.unpinCall != 0 {
			t.Fatalf("unpinned %d groups despite no verdict", cat.unpinCall)
		}
	})
}

// TestReconcilePinsSurfacesUnpinFailure: a failed unpin aborts non-zero without claiming
// the work was done, and the already-applied deletes stay applied (they are idempotent, so
// a rerun completes).
func TestReconcilePinsSurfacesUnpinFailure(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	cat := &fakeCatalogPins{
		pins: []consumer.SeatPin{
			pin(1, "Orchestra/A/1", "hold:"+first.String()),
			pin(2, "Orchestra/A/2", "hold:"+second.String()),
		},
		unpinErr: func(pinnedBy string) error {
			if pinnedBy == "hold:"+second.String() {
				return errors.New("catalog 503")
			}
			return nil
		},
	}
	live := &livenessRecorder{states: map[uuid.UUID]store.SeatClaimState{first: store.SeatClaimDead, second: store.SeatClaimDead}}
	r := pinReconciler{listPins: cat.list, liveness: live.verdicts, unpin: cat.unpinSeats}
	if _, err := r.run(context.Background()); err == nil {
		t.Fatal("an unpin failure must fail the run rather than report success")
	}

	// The failure was transient; clearing it and rerunning must complete the work.
	cat.unpinErr = nil
	stats, err := r.run(context.Background())
	if err != nil {
		t.Fatalf("rerun after a transient failure: %v", err)
	}
	if len(cat.survivors()) != 0 {
		t.Fatalf("rerun left %v", cat.survivors())
	}
	if stats.Reclaimed == 0 {
		t.Fatal("rerun reclaimed nothing, want the pin the failed run could not")
	}
}

// TestReconcilePinsGroupsUnpinsPerClaim: a claim holding several seats is unpinned in ONE
// batch call keyed to its own pinned_by, and a DIFFERENT claim's pin on the same seat
// identity survives. That second half is the invariant the whole design rests on —
// pinned_by is per-claim, so a reclaim can never delete a newer hold's pin.
func TestReconcilePinsGroupsUnpinsPerClaim(t *testing.T) {
	dead, fresh := uuid.New(), uuid.New()
	cat := &fakeCatalogPins{pins: []consumer.SeatPin{
		pin(1, "Orchestra/A/1", "hold:"+dead.String()),
		pin(2, "Orchestra/A/2", "hold:"+dead.String()),
		pin(3, "Orchestra/A/3", "hold:"+dead.String()),
		// Same seat identity as the dead claim's first seat, different claim: a fresh
		// hold that re-took the seat after the dead one expired.
		pin(4, "Orchestra/A/1", "hold:"+fresh.String()),
	}}
	live := &livenessRecorder{states: map[uuid.UUID]store.SeatClaimState{dead: store.SeatClaimDead, fresh: store.SeatClaimLive}}

	stats, err := pinReconciler{listPins: cat.list, liveness: live.verdicts, unpin: cat.unpinSeats}.run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if cat.unpinCall != 1 {
		t.Fatalf("unpin calls = %d, want one batch for the one dead claim", cat.unpinCall)
	}
	if stats.Reclaimed != 3 {
		t.Fatalf("reclaimed = %d want 3", stats.Reclaimed)
	}
	got := cat.survivors()
	if len(got) != 1 || got[0] != "hold:"+fresh.String()+"|Orchestra/A/1" {
		t.Fatalf("survivors = %v, the fresh hold's pin on the same seat must survive", got)
	}
}

// TestReconcilePinsShrinksThePageRatherThanStalling closes ai-review pass 3's finding. The
// consumer caps a page's bytes, but catalog bounds neither `seat_identity` nor `pinned_by` (both
// unbounded text; the pin request is capped at 1 MiB, so ONE identity can be nearly that big).
// A page that overflows the byte cap used to abort the run at that cursor — and since the cursor
// only advances past rows that were successfully read, the drain could never get past it. One
// oversized page permanently disabled the tool built to reclaim pins.
func TestReconcilePinsShrinksThePageRatherThanStalling(t *testing.T) {
	dead := uuid.New()
	pins := []consumer.SeatPin{}
	for i := 1; i <= 30; i++ {
		pins = append(pins, pin(i, fmt.Sprintf("Orchestra/A/%d", i), "hold:"+dead.String()))
	}
	cat := &fakeCatalogPins{pins: pins}
	live := &livenessRecorder{states: map[uuid.UUID]store.SeatClaimState{dead: store.SeatClaimDead}}

	// Any page above 25 rows overflows the byte cap; smaller ones fit.
	oversized := func(ctx context.Context, after uuid.UUID, limit int) ([]consumer.SeatPin, error) {
		if limit > 25 {
			return nil, fmt.Errorf("page of %d: %w", limit, consumer.ErrSeatPinPageTooLarge)
		}
		return cat.list(ctx, after, limit)
	}

	stats, err := pinReconciler{listPins: oversized, liveness: live.verdicts, unpin: cat.unpinSeats}.run(context.Background())
	if err != nil {
		t.Fatalf("an oversized page must be retried smaller, not abort the drain: %v", err)
	}
	if stats.Reclaimed != 30 {
		t.Fatalf("reclaimed = %d want 30 — the drain must still reach every pin", stats.Reclaimed)
	}
	if len(cat.survivors()) != 0 {
		t.Fatalf("survivors = %v want none", cat.survivors())
	}

	// A single row that still overflows is a genuine dead end: fail loudly, naming the cursor,
	// rather than looping or silently skipping the row.
	alwaysTooBig := func(_ context.Context, _ uuid.UUID, limit int) ([]consumer.SeatPin, error) {
		return nil, fmt.Errorf("page of %d: %w", limit, consumer.ErrSeatPinPageTooLarge)
	}
	cat2 := &fakeCatalogPins{pins: append([]consumer.SeatPin{}, pins...)}
	if _, err = (pinReconciler{listPins: alwaysTooBig, liveness: live.verdicts, unpin: cat2.unpinSeats}).run(context.Background()); err == nil {
		t.Fatal("a single row over the cap must fail the run, not spin or skip")
	}
	if cat2.unpinCall != 0 {
		t.Fatalf("unpinned %d groups without ever reading a page", cat2.unpinCall)
	}
}
