package events

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/catalog/internal/store"
)

// Wire-compatibility goldens (TKT-126).
//
// These literals were captured from the PRE-refactor emitters — the local
// `Envelope` struct declared in this package — and committed BEFORE
// the shared envelope package existed. That ordering is the whole point: ADR-017 §5b′
// warns that a fixture built from the type under test cannot fail, because it
// encodes the compatibility it claims to prove. Regenerating these bytes from
// the shared envelope would prove nothing at all.
//
// The reviewer's check is one command — at the commit that introduced this
// file, the shared package does not exist yet:
//
//	git show <baseline-commit>:shared/go/domainevent/envelope.go   # must fail
//
// So: DO NOT regenerate these. If a legitimate wire change ever lands, updating
// a literal here is a deliberate contract change that belongs in its own ADR —
// the friction is the feature.
//
// Comparison is over the COMPLETE byte slice, including `occurred_at` and key
// order. Never decode-and-compare, never normalize, never drop a field: a
// re-marshalled comparison cannot see a key-order or timestamp-format change,
// which is most of what "byte-for-byte identical" is protecting.

// Fixed identifiers. Deliberately literal, not uuid.New(): a golden cannot be
// golden if its inputs move.
var (
	goldPerfID      = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	goldEventID     = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	goldOrganizerID = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	goldGroupID     = uuid.MustParse("44444444-4444-4444-8444-444444444444")
	goldSeatMapID   = uuid.MustParse("55555555-5555-4555-8555-555555555555")
	goldVenueID     = uuid.MustParse("66666666-6666-4666-8666-666666666666")

	// A non-zero nanosecond component pins RFC3339Nano formatting: a golden
	// whose timestamp lands on a whole second cannot detect a precision change.
	goldOccurred = time.Date(2026, 7, 20, 12, 34, 56, 123456789, time.UTC)
)

func goldPerformance() store.Performance {
	published := goldOccurred
	return store.Performance{
		ID: goldPerfID, EventID: goldEventID, OrganizerID: goldOrganizerID,
		Kind: store.KindPerformance, Capacity: 250, PublishedAt: &published,
	}
}

func TestWireGoldenPerformancePublishedSchema2(t *testing.T) {
	body, err := performancePublishedEnvelope(goldPerformance(), goldOccurred)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"76722551-ed68-5a5b-933b-60b4fa0abfba","type":"platform.catalog.performance.published","occurred_at":"2026-07-20T12:34:56.123456789Z","schema":2,"data":{"performance_id":"11111111-1111-4111-8111-111111111111","event_id":"22222222-2222-4222-8222-222222222222","organizer_id":"33333333-3333-4333-8333-333333333333","kind":"performance","capacity":250,"re_entry":{"mode":"single","requires_exit":false}}}`
	assertGolden(t, want, body)
}

func TestWireGoldenPerformancePublishedSchema3Festival(t *testing.T) {
	perf := goldPerformance()
	shared := int32(1000)
	perf.Kind = store.KindFestivalDay
	perf.CapacityGroupID = &goldGroupID
	perf.SharedCapacity = &shared
	body, err := performancePublishedEnvelope(perf, goldOccurred)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"76722551-ed68-5a5b-933b-60b4fa0abfba","type":"platform.catalog.performance.published","occurred_at":"2026-07-20T12:34:56.123456789Z","schema":3,"data":{"performance_id":"11111111-1111-4111-8111-111111111111","event_id":"22222222-2222-4222-8222-222222222222","organizer_id":"33333333-3333-4333-8333-333333333333","kind":"festival_day","capacity":250,"capacity_group_id":"44444444-4444-4444-8444-444444444444","shared_capacity":1000,"re_entry":{"mode":"single","requires_exit":false}}}`
	assertGolden(t, want, body)
}

func TestWireGoldenPerformancePublishedSchema4Seated(t *testing.T) {
	perf := goldPerformance()
	perf.SeatMapID = &goldSeatMapID
	maxEntries := int32(3)
	perf.ReEntry = store.ReEntryPolicy{Mode: "count_limited", MaxEntries: &maxEntries, RequiresExit: true}
	body, err := performancePublishedEnvelope(perf, goldOccurred)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"76722551-ed68-5a5b-933b-60b4fa0abfba","type":"platform.catalog.performance.published","occurred_at":"2026-07-20T12:34:56.123456789Z","schema":4,"data":{"performance_id":"11111111-1111-4111-8111-111111111111","event_id":"22222222-2222-4222-8222-222222222222","organizer_id":"33333333-3333-4333-8333-333333333333","kind":"performance","capacity":250,"seat_map_id":"55555555-5555-4555-8555-555555555555","re_entry":{"mode":"count_limited","max_entries":3,"requires_exit":true}}}`
	assertGolden(t, want, body)
}

// The backfill re-emission is byte-identical to the live publish EXCEPT for the
// envelope id — that difference is load-bearing (TKT-96: sharing the live id
// would let access's dedup swallow the re-emission), so it gets its own golden.
func TestWireGoldenPerformancePublishedBackfill(t *testing.T) {
	perf := goldPerformance()
	body, err := performancePublishedEnvelopeWithID(perf, goldOccurred, BackfillEventID(perf))
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"69cf4f07-9df9-5055-870b-4ad3d87aaf65","type":"platform.catalog.performance.published","occurred_at":"2026-07-20T12:34:56.123456789Z","schema":2,"data":{"performance_id":"11111111-1111-4111-8111-111111111111","event_id":"22222222-2222-4222-8222-222222222222","organizer_id":"33333333-3333-4333-8333-333333333333","kind":"performance","capacity":250,"re_entry":{"mode":"single","requires_exit":false}}}`
	assertGolden(t, want, body)
}

func TestWireGoldenPerformanceArchivedSchema2(t *testing.T) {
	perf := goldPerformance()
	archived := goldOccurred
	perf.ArchivedAt = &archived
	body, err := performanceArchivedEnvelope(perf, goldOccurred)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"b3cc4e37-80ba-5f7a-a6b7-5005ba71e37d","type":"platform.catalog.performance.archived","occurred_at":"2026-07-20T12:34:56.123456789Z","schema":2,"data":{"performance_id":"11111111-1111-4111-8111-111111111111","event_id":"22222222-2222-4222-8222-222222222222","organizer_id":"33333333-3333-4333-8333-333333333333"}}`
	assertGolden(t, want, body)
}

func TestWireGoldenPerformanceArchivedSchema3Festival(t *testing.T) {
	perf := goldPerformance()
	shared := int32(1000)
	archived := goldOccurred
	perf.Kind = store.KindFestivalDay
	perf.CapacityGroupID = &goldGroupID
	perf.SharedCapacity = &shared
	perf.ArchivedAt = &archived
	body, err := performanceArchivedEnvelope(perf, goldOccurred)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"b3cc4e37-80ba-5f7a-a6b7-5005ba71e37d","type":"platform.catalog.performance.archived","occurred_at":"2026-07-20T12:34:56.123456789Z","schema":3,"data":{"performance_id":"11111111-1111-4111-8111-111111111111","event_id":"22222222-2222-4222-8222-222222222222","organizer_id":"33333333-3333-4333-8333-333333333333","capacity_group_id":"44444444-4444-4444-8444-444444444444"}}`
	assertGolden(t, want, body)
}

func TestWireGoldenSlotClosed(t *testing.T) {
	perf := goldPerformance()
	reason := "storm"
	perf.Closure = store.Closure{Version: 2, Reason: &reason}
	body, err := closureEnvelope(SubjectSlotClosed, perf, goldOccurred)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"35869945-90bc-5f53-a0fb-39f63f70798d","type":"platform.catalog.performance.closed","occurred_at":"2026-07-20T12:34:56.123456789Z","schema":1,"data":{"performance_id":"11111111-1111-4111-8111-111111111111","event_id":"22222222-2222-4222-8222-222222222222","organizer_id":"33333333-3333-4333-8333-333333333333","kind":"performance","closure_version":2,"reason":"storm"}}`
	assertGolden(t, want, body)
}

// Reopened omits `reason` entirely (omitempty on a nil pointer) — the golden
// pins the absence, which a decoded comparison would silently accept.
func TestWireGoldenSlotReopened(t *testing.T) {
	perf := goldPerformance()
	perf.Closure = store.Closure{Version: 3}
	body, err := closureEnvelope(SubjectSlotReopened, perf, goldOccurred)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"185ea466-0c7a-5037-b741-701fbec2b9ab","type":"platform.catalog.performance.reopened","occurred_at":"2026-07-20T12:34:56.123456789Z","schema":1,"data":{"performance_id":"11111111-1111-4111-8111-111111111111","event_id":"22222222-2222-4222-8222-222222222222","organizer_id":"33333333-3333-4333-8333-333333333333","kind":"performance","closure_version":3}}`
	assertGolden(t, want, body)
}

func TestWireGoldenSeatMapPublished(t *testing.T) {
	published := goldOccurred
	m := store.SeatMap{
		ID: goldSeatMapID, OrganizerID: goldOrganizerID, VenueID: goldVenueID,
		Name: "Main floor", Version: 2, Status: "published", PublishedAt: &published,
	}
	body, err := seatMapPublishedEnvelope(m)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"0c40c76a-74e9-575f-96da-49757a04d644","type":"platform.catalog.seat_map.published","occurred_at":"2026-07-20T12:34:56.123456789Z","schema":1,"data":{"seat_map_id":"55555555-5555-4555-8555-555555555555","organizer_id":"33333333-3333-4333-8333-333333333333","venue_id":"66666666-6666-4666-8666-666666666666","version":2}}`
	assertGolden(t, want, body)
}

func assertGolden(t *testing.T, want string, got []byte) {
	t.Helper()
	if string(got) != want {
		t.Fatalf("wire bytes changed (TKT-126 forbids it)\n got: %s\nwant: %s", got, want)
	}
}
