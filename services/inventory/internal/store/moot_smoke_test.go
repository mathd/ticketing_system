//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

// TKT-317: a moot outcome is recorded durably before its event is acked. A later archive or
// closure for that slot may then be consumed without a pool, and only when a tombstone matches
// the organizer and the slot and the pool is absent. Every predicate has its own case, and each
// case carries a positive control, so a deleted predicate turns a refusal into a consumption.

func tkt317Consumed(t *testing.T, ctx context.Context, db *sql.DB, eventID uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM consumed_events WHERE event_id=$1`, eventID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTKT317MootMigrationDownRefusesWhileRowsRemain(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, sub)
	if err != nil {
		t.Fatal(err)
	}
	// Empty tables reverse cleanly, and the migration applies again afterwards.
	if _, err = provider.DownTo(ctx, 19); err != nil {
		t.Fatalf("down over empty tables: %v", err)
	}
	if _, err = provider.UpTo(ctx, 20); err != nil {
		t.Fatalf("up again: %v", err)
	}
	org, slot := uuid.New(), uuid.New()
	if err = st.RecordMootSlot(ctx, uuid.New(), org, slot); err != nil {
		t.Fatal(err)
	}
	var tombstones int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM moot_slots WHERE organizer_id=$1 AND slot_id=$2`, org, slot).Scan(&tombstones); err != nil || tombstones != 1 {
		t.Fatalf("tombstone rows = %d err=%v, want 1 before the down", tombstones, err)
	}
	if _, err = provider.DownTo(ctx, 19); err == nil {
		t.Fatal("down discarded a moot tombstone")
	}
	if _, err = db.ExecContext(ctx, `DELETE FROM moot_slots`); err != nil {
		t.Fatal(err)
	}
	// The parked table refuses on its own, so only it is seeded here.
	if _, err = db.ExecContext(ctx, `INSERT INTO catalog_event_parked(subject,event_id,schema,organizer_id,slot_id,envelope,delivery_count,reason)
		VALUES('platform.catalog.performance.closed',$1,1,$2,$3,$4,5,'test')`, uuid.New(), org, slot, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if _, err = provider.DownTo(ctx, 19); err == nil {
		t.Fatal("down discarded a parked catalog event")
	}
	if _, err = db.ExecContext(ctx, `DELETE FROM catalog_event_parked`); err != nil {
		t.Fatal(err)
	}
	if _, err = provider.DownTo(ctx, 19); err != nil {
		t.Fatalf("down after both tables are empty: %v", err)
	}
}

func TestTKT317RecordMootSlotWritesTombstoneAndConsumption(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := uuid.New(), uuid.New()
	first, second := uuid.New(), uuid.New()
	if err := st.RecordMootSlot(ctx, first, org, slot); err != nil {
		t.Fatal(err)
	}
	// A replayed event and a second moot event for the same slot both succeed. The tombstone
	// keeps the first source event.
	if err := st.RecordMootSlot(ctx, first, org, slot); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if err := st.RecordMootSlot(ctx, second, org, slot); err != nil {
		t.Fatalf("second moot event: %v", err)
	}
	var source uuid.UUID
	if err := db.QueryRowContext(ctx, `SELECT source_event_id FROM moot_slots WHERE organizer_id=$1 AND slot_id=$2`, org, slot).Scan(&source); err != nil {
		t.Fatalf("tombstone missing: %v", err)
	}
	if source != first {
		t.Fatalf("tombstone source = %s, want the first moot event %s", source, first)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM moot_slots WHERE organizer_id=$1 AND slot_id=$2`, org, slot).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("tombstone rows = %d, want 1", rows)
	}
	if tkt317Consumed(t, ctx, db, first) != 1 || tkt317Consumed(t, ctx, db, second) != 1 {
		t.Fatal("a moot event was acked without its consumed_events row")
	}
}

func TestTKT317ConsumeMootOfferingNeedsATombstone(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot, archive := uuid.New(), uuid.New(), uuid.New()
	// No tombstone: the early archive is refused and nothing is consumed.
	ok, err := st.ConsumeMootOffering(ctx, archive, org, slot, slot)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("an archive with no tombstone was consumed")
	}
	if n := tkt317Consumed(t, ctx, db, archive); n != 0 {
		t.Fatalf("refused archive left %d consumed_events rows", n)
	}
	// Positive control: with the tombstone, the same archive is consumed and recorded.
	if err = st.RecordMootSlot(ctx, uuid.New(), org, slot); err != nil {
		t.Fatal(err)
	}
	ok, err = st.ConsumeMootOffering(ctx, archive, org, slot, slot)
	if err != nil || !ok {
		t.Fatalf("with a tombstone: consumed=%v err=%v", ok, err)
	}
	if n := tkt317Consumed(t, ctx, db, archive); n != 1 {
		t.Fatalf("consumed archive has %d consumed_events rows, want 1", n)
	}
}

func TestTKT317ConsumeMootOfferingMatchesTheOrganizer(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	orgA, orgB, slot := uuid.New(), uuid.New(), uuid.New()
	if err := st.RecordMootSlot(ctx, uuid.New(), orgA, slot); err != nil {
		t.Fatal(err)
	}
	// Another organizer's event for the same slot id must not match. The pool is absent, so
	// only the organizer predicate can refuse this call.
	ok, err := st.ConsumeMootOffering(ctx, uuid.New(), orgB, slot, slot)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("another organizer's tombstone matched")
	}
	// Positive control: the tombstone's own organizer matches.
	ok, err = st.ConsumeMootOffering(ctx, uuid.New(), orgA, slot, slot)
	if err != nil || !ok {
		t.Fatalf("the tombstone's own organizer: consumed=%v err=%v", ok, err)
	}
}

func TestTKT317ConsumeMootOfferingMatchesThePerformance(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slotA, slotB := uuid.New(), uuid.New(), uuid.New()
	if err := st.RecordMootSlot(ctx, uuid.New(), org, slotA); err != nil {
		t.Fatal(err)
	}
	// Another slot of the same organizer must not match. Its pool is absent, so only the
	// performance predicate can refuse this call.
	ok, err := st.ConsumeMootOffering(ctx, uuid.New(), org, slotB, slotB)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("another slot's tombstone matched")
	}
	// Positive control: the tombstoned slot matches.
	ok, err = st.ConsumeMootOffering(ctx, uuid.New(), org, slotA, slotA)
	if err != nil || !ok {
		t.Fatalf("the tombstoned slot: consumed=%v err=%v", ok, err)
	}
}

func TestTKT317ConsumeMootOfferingRefusesWhenThePoolExists(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot := uuid.New(), uuid.New()
	if err := st.RecordMootSlot(ctx, uuid.New(), org, slot); err != nil {
		t.Fatal(err)
	}
	// A later publication provisioned the pool. The tombstone matches, so only the pool
	// predicate can refuse this call.
	if err := st.Provision(ctx, uuid.New(), slot, org, 10); err != nil {
		t.Fatal(err)
	}
	ok, err := st.ConsumeMootOffering(ctx, uuid.New(), org, slot, slot)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a moot fallback consumed an event whose pool exists")
	}
	// Positive control, grouped identity: the performance keys the tombstone and an absent
	// capacity group keys the pool. This is how a grouped archive reaches the fallback.
	group := uuid.New()
	ok, err = st.ConsumeMootOffering(ctx, uuid.New(), org, slot, group)
	if err != nil || !ok {
		t.Fatalf("grouped member with an absent capacity group: consumed=%v err=%v", ok, err)
	}
}

func TestTKT317FallbackSeesAProvisioningThatCommittedFirst(t *testing.T) {
	ctx, st, _ := storeForTest(t, time.Minute)
	org, slot, archive := uuid.New(), uuid.New(), uuid.New()
	if err := st.RecordMootSlot(ctx, uuid.New(), org, slot); err != nil {
		t.Fatal(err)
	}
	// The normal apply sees no pool.
	if err := st.ApplyArchive(ctx, archive, slot); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ApplyArchive before provisioning = %v, want ErrNotFound", err)
	}
	// Provisioning commits between that answer and the fallback.
	if err := st.Provision(ctx, uuid.New(), slot, org, 10); err != nil {
		t.Fatal(err)
	}
	ok, err := st.ConsumeMootOffering(ctx, archive, org, slot, slot)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("the fallback consumed an archive over a pool that now exists")
	}
	// The redelivery applies to the pool, so the archive is not lost.
	if err := st.ApplyArchive(ctx, archive, slot); err != nil {
		t.Fatalf("redelivered archive: %v", err)
	}
	a, err := st.Availability(ctx, org, slot, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.OfferingStatus != "archived" {
		t.Fatalf("offering status = %q after the redelivered archive, want archived", a.OfferingStatus)
	}
}

func TestTKT317ConsumeMootOfferingReplayIsIdempotent(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot, archive := uuid.New(), uuid.New(), uuid.New()
	if err := st.RecordMootSlot(ctx, uuid.New(), org, slot); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		ok, err := st.ConsumeMootOffering(ctx, archive, org, slot, slot)
		if err != nil || !ok {
			t.Fatalf("delivery %d: consumed=%v err=%v, want a replay to report consumed", i+1, ok, err)
		}
	}
	if n := tkt317Consumed(t, ctx, db, archive); n != 1 {
		t.Fatalf("consumed_events rows = %d after two deliveries, want 1", n)
	}
}
