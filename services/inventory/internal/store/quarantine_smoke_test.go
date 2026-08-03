//go:build smoke

package store

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

const quarantineSubject = "platform.catalog.performance.published"

// The envelope column must hold the exact received bytes — whitespace, unknown fields,
// incompatible future data and all — because reprocess-quarantine republishes them verbatim.
func TestQuarantinePreservesExactEnvelopeBytes(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	id := uuid.New()
	raw := []byte("{\n  \"id\":\"" + id.String() + "\", \"schema\":4,\n  \"data\":[1, \"not an object\", null]\n}")

	if err := st.QuarantineCatalogEvent(ctx, quarantineSubject, id, 4, raw); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListCatalogQuarantine(ctx, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || string(rows[0].Envelope) != string(raw) {
		t.Fatalf("stored envelope %q, want the exact bytes %q", rows[0].Envelope, raw)
	}
	if rows[0].Subject != quarantineSubject || rows[0].EventID != id || rows[0].Schema != 4 {
		t.Fatalf("stored (%s, %s, %d), want (%s, %s, 4)", rows[0].Subject, rows[0].EventID, rows[0].Schema, quarantineSubject, id)
	}
}

// JetStream redelivers: an identical write is one row with a bumped last_seen_at; the same key
// with different content is a collision that must not overwrite the first copy.
func TestQuarantineDuplicateAndCollision(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	id := uuid.New()
	raw := []byte(`{"id":"` + id.String() + `","schema":4,"data":{"x":1}}`)

	for range 2 {
		if err := st.QuarantineCatalogEvent(ctx, quarantineSubject, id, 4, raw); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM catalog_event_quarantine`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d err=%v, want exactly 1 for an identical redelivery", n, err)
	}
	var firstSeen, lastSeen time.Time
	if err := db.QueryRowContext(ctx, `SELECT first_seen_at, last_seen_at FROM catalog_event_quarantine`).Scan(&firstSeen, &lastSeen); err != nil {
		t.Fatal(err)
	}
	if !lastSeen.After(firstSeen) {
		t.Fatalf("last_seen_at %v not after first_seen_at %v — the redelivery must be recorded", lastSeen, firstSeen)
	}

	err := st.QuarantineCatalogEvent(ctx, quarantineSubject, id, 5, []byte(`{"different":true}`))
	if !errors.Is(err, ErrCatalogQuarantineCollision) {
		t.Fatalf("err = %v, want ErrCatalogQuarantineCollision", err)
	}
	var kept []byte
	if err := db.QueryRowContext(ctx, `SELECT envelope FROM catalog_event_quarantine WHERE event_id=$1`, id).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if string(kept) != string(raw) {
		t.Fatalf("envelope = %q — a collision must not overwrite the first copy", kept)
	}
}

// The pending cap is a hard ceiling on unresolved rows: the third distinct event at a cap of two
// is rejected, an identical redelivery still succeeds, and resolving a row frees a slot.
func TestQuarantineCapacityBoundsUnresolvedRows(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	st.quarantineCap = 2
	first := uuid.New()
	envelope := func(id uuid.UUID) []byte { return []byte(`{"id":"` + id.String() + `","schema":4}`) }

	if err := st.QuarantineCatalogEvent(ctx, quarantineSubject, first, 4, envelope(first)); err != nil {
		t.Fatal(err)
	}
	second := uuid.New()
	if err := st.QuarantineCatalogEvent(ctx, quarantineSubject, second, 4, envelope(second)); err != nil {
		t.Fatal(err)
	}
	if err := st.QuarantineCatalogEvent(ctx, quarantineSubject, uuid.New(), 4, envelope(first)); !errors.Is(err, ErrCatalogQuarantineFull) {
		t.Fatalf("err = %v, want ErrCatalogQuarantineFull at the cap", err)
	}
	// An identical redelivery of a held event is not new demand — it must still succeed.
	if err := st.QuarantineCatalogEvent(ctx, quarantineSubject, first, 4, envelope(first)); err != nil {
		t.Fatalf("identical redelivery at capacity: %v, want success", err)
	}
	// Resolving a row frees its slot.
	if err := st.MarkCatalogEventReinjected(ctx, quarantineSubject, first); err != nil {
		t.Fatal(err)
	}
	third := uuid.New()
	if err := st.QuarantineCatalogEvent(ctx, quarantineSubject, third, 4, envelope(third)); err != nil {
		t.Fatalf("write after resolving a row: %v, want success", err)
	}
}

// The schema column is bigint on purpose: the consumer forwards ANY schema above its known max,
// and a value past int32 must quarantine like any other future variant — an INSERT range error
// would loop one malformed event as a permanent delayed NAK (ai-review finding 1).
func TestQuarantineAcceptsSchemaBeyondInt32(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	id := uuid.New()
	if err := st.QuarantineCatalogEvent(ctx, quarantineSubject, id, 4_000_000_000, []byte(`{}`)); err != nil {
		t.Fatalf("schema beyond int32: %v, want quarantined like any future variant", err)
	}
	rows, err := st.ListCatalogQuarantine(ctx, nil, 1)
	if err != nil || len(rows) != 1 || rows[0].Schema != 4_000_000_000 {
		t.Fatalf("rows=%v err=%v, want the huge schema stored intact", rows, err)
	}
}

func TestQuarantinePendingStateAndReinjection(t *testing.T) {
	ctx, st, _ := storeForTest(t, 10*time.Minute)
	if pending, err := st.HasPendingCatalogQuarantine(ctx); err != nil || pending {
		t.Fatalf("pending = %v err=%v on an empty table, want false", pending, err)
	}
	id := uuid.New()
	if err := st.QuarantineCatalogEvent(ctx, quarantineSubject, id, 4, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if pending, err := st.HasPendingCatalogQuarantine(ctx); err != nil || !pending {
		t.Fatalf("pending = %v err=%v with an unresolved row, want true", pending, err)
	}
	if err := st.MarkCatalogEventReinjected(ctx, quarantineSubject, id); err != nil {
		t.Fatal(err)
	}
	if pending, err := st.HasPendingCatalogQuarantine(ctx); err != nil || pending {
		t.Fatalf("pending = %v err=%v after reinjection, want false", pending, err)
	}
	if rows, err := st.ListCatalogQuarantine(ctx, nil, 10); err != nil || len(rows) != 0 {
		t.Fatalf("rows = %d err=%v — reinjected rows must leave the pending list", len(rows), err)
	}
	// Marking an already-resolved row is a no-op, so a crashed reprocess run can be re-run.
	if err := st.MarkCatalogEventReinjected(ctx, quarantineSubject, id); err != nil {
		t.Fatalf("re-mark: %v, want idempotent success", err)
	}
}

// Keyset pagination oldest-first: an unsupported row at the front must not starve rows behind
// it — the cursor is the last row seen, supported or not.
func TestQuarantineListPaginatesOldestFirstPastUnsupportedRows(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	ids := make([]uuid.UUID, 3)
	for i := range ids {
		ids[i] = uuid.New()
		if err := st.QuarantineCatalogEvent(ctx, quarantineSubject, ids[i], 4+i, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		// Distinct first_seen_at so the keyset order is deterministic under test speed.
		if _, err := db.ExecContext(ctx, `UPDATE catalog_event_quarantine SET first_seen_at = now() - $1::interval WHERE event_id=$2`,
			(time.Duration(len(ids)-i) * time.Minute).String(), ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	page1, err := st.ListCatalogQuarantine(ctx, nil, 2)
	if err != nil || len(page1) != 2 {
		t.Fatalf("page1 = %d rows err=%v, want 2", len(page1), err)
	}
	if page1[0].EventID != ids[0] || page1[1].EventID != ids[1] {
		t.Fatalf("page1 = [%s %s], want oldest first [%s %s]", page1[0].EventID, page1[1].EventID, ids[0], ids[1])
	}
	page2, err := st.ListCatalogQuarantine(ctx, &page1[1], 2)
	if err != nil || len(page2) != 1 || page2[0].EventID != ids[2] {
		t.Fatalf("page2 = %v err=%v, want exactly the newest row %s", page2, err, ids[2])
	}
}

// Reinjected rows past retention are pruned on the next write; unresolved rows never age out —
// that would turn immediate silent loss into delayed silent loss.
func TestQuarantineRetentionPrunesOnlyReinjectedRows(t *testing.T) {
	ctx, st, db := storeForTest(t, 10*time.Minute)
	resolved, unresolved := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{resolved, unresolved} {
		if err := st.QuarantineCatalogEvent(ctx, quarantineSubject, id, 4, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	// Age both far past retention; only the resolved one may go.
	if _, err := db.ExecContext(ctx, `UPDATE catalog_event_quarantine SET first_seen_at = now() - interval '30 days'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE catalog_event_quarantine SET reinjected_at = now() - interval '30 days' WHERE event_id=$1`, resolved); err != nil {
		t.Fatal(err)
	}
	if err := st.QuarantineCatalogEvent(ctx, quarantineSubject, uuid.New(), 4, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM catalog_event_quarantine WHERE event_id=$1`, resolved).Scan(&n); err != nil || n != 0 {
		t.Fatalf("resolved row count = %d err=%v, want pruned", n, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM catalog_event_quarantine WHERE event_id=$1`, unresolved).Scan(&n); err != nil || n != 1 {
		t.Fatalf("unresolved row count = %d err=%v — unresolved rows must never age out", n, err)
	}
}
