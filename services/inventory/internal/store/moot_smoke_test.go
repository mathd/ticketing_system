//go:build smoke

package store

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"strings"
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
	if err = st.RecordMootSlot(ctx, uuid.New(), org, slot, MootSourcePublication); err != nil {
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
	if err := st.RecordMootSlot(ctx, first, org, slot, MootSourcePublication); err != nil {
		t.Fatal(err)
	}
	// A replayed event and a second moot event for the same slot both succeed. The tombstone
	// keeps the first source event.
	if err := st.RecordMootSlot(ctx, first, org, slot, MootSourcePublication); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if err := st.RecordMootSlot(ctx, second, org, slot, MootSourcePublication); err != nil {
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
	if err = st.RecordMootSlot(ctx, uuid.New(), org, slot, MootSourcePublication); err != nil {
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
	if err := st.RecordMootSlot(ctx, uuid.New(), orgA, slot, MootSourcePublication); err != nil {
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
	if err := st.RecordMootSlot(ctx, uuid.New(), org, slotA, MootSourcePublication); err != nil {
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
	if err := st.RecordMootSlot(ctx, uuid.New(), org, slot, MootSourcePublication); err != nil {
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
	if err := st.RecordMootSlot(ctx, uuid.New(), org, slot, MootSourcePublication); err != nil {
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
	if err := st.RecordMootSlot(ctx, uuid.New(), org, slot, MootSourcePublication); err != nil {
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

// TKT-317 R2: the Down must not discard a table while a writer holds an uncommitted row in it. The
// emptiness check cannot see that row, so the Down has to wait for the writer before it checks. The
// schedule is forced: the writer commits only after the Down is blocked on a lock. A Down that
// finishes before it blocks fails the test, so the outcome never depends on timing.
func TestTKT317DownWaitsForAnUncommittedMootWriter(t *testing.T) {
	ctx, _, db := storeForTest(t, time.Minute)
	dsn := os.Getenv("INVENTORY_MIGRATION_TEST_DATABASE_URL")
	var schema string
	if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	// The Down runs on its own pool. Its application name finds its backend in pg_stat_activity.
	appName := "tkt317-down-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	downDB, err := sql.Open("pgx", dsn+"?search_path="+schema+"&application_name="+appName)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = downDB.Close() })
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, downDB, sub)
	if err != nil {
		t.Fatal(err)
	}

	org, slot := uuid.New(), uuid.New()
	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback() }()
	if _, err = writer.ExecContext(ctx, `INSERT INTO moot_slots(organizer_id, slot_id, source, source_event_id) VALUES($1,$2,$3,$4)`,
		org, slot, string(MootSourcePublication), uuid.New()); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := provider.DownTo(ctx, 19)
		done <- err
	}()
	// Poll until the Down is blocked on a lock. The poll ends on the Down's own result, and a Down
	// that never blocks fails here rather than hanging.
	deadline := time.Now().Add(30 * time.Second)
	for blocked := false; !blocked; {
		select {
		case err := <-done:
			t.Fatalf("down finished before it waited on the uncommitted writer (err=%v)", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("down never waited on a lock")
		}
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
			WHERE application_name=$1 AND wait_event_type='Lock')`, appName).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	var downErr error
	select {
	case downErr = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("down did not finish after the writer committed")
	}
	// The row is committed now. The Down must refuse, because discarding the table loses it.
	if downErr == nil {
		t.Error("down succeeded over a tombstone that a writer committed while the down waited; the tombstone was dropped")
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM moot_slots WHERE organizer_id=$1 AND slot_id=$2`, org, slot).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("the committed tombstone is not there after the down: rows=%d err=%v", rows, err)
	}
}

// TKT-317 R5: the replay query has its own predicates, and a fresh event id never reaches them. Each
// case below consumes its event first, and then asks again with ONE predicate failing. The
// positive control in each case is the same replay with every predicate holding.
func TestTKT317ReplayIsRefusedForAnotherOrganizer(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	orgA, orgB, slot, archive := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if err := st.RecordMootSlot(ctx, uuid.New(), orgA, slot, MootSourcePublication); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ConsumeMootOffering(ctx, archive, orgA, slot, slot); err != nil || !ok {
		t.Fatalf("first delivery: consumed=%v err=%v", ok, err)
	}
	// The replay names orgB. The pool is absent and the slot matches, so only the organizer
	// predicate in the replay query can refuse it.
	ok, err := st.ConsumeMootOffering(ctx, archive, orgB, slot, slot)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a replay with another organizer reported consumed")
	}
	ok, err = st.ConsumeMootOffering(ctx, archive, orgA, slot, slot)
	if err != nil || !ok {
		t.Fatalf("replay with the tombstone's organizer: consumed=%v err=%v", ok, err)
	}
	if n := tkt317Consumed(t, ctx, db, archive); n != 1 {
		t.Fatalf("consumed_events rows = %d, want 1", n)
	}
}

func TestTKT317ReplayIsRefusedForAnotherSlot(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slotA, slotB, archive := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if err := st.RecordMootSlot(ctx, uuid.New(), org, slotA, MootSourcePublication); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ConsumeMootOffering(ctx, archive, org, slotA, slotA); err != nil || !ok {
		t.Fatalf("first delivery: consumed=%v err=%v", ok, err)
	}
	// The replay names slotB, whose pool is absent. The organizer matches, so only the slot
	// predicate in the replay query can refuse it.
	ok, err := st.ConsumeMootOffering(ctx, archive, org, slotB, slotB)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a replay for another slot reported consumed")
	}
	ok, err = st.ConsumeMootOffering(ctx, archive, org, slotA, slotA)
	if err != nil || !ok {
		t.Fatalf("replay for the tombstoned slot: consumed=%v err=%v", ok, err)
	}
	if n := tkt317Consumed(t, ctx, db, archive); n != 1 {
		t.Fatalf("consumed_events rows = %d, want 1", n)
	}
}

func TestTKT317ReplayIsRefusedOnceThePoolExists(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot, archive := uuid.New(), uuid.New(), uuid.New()
	if err := st.RecordMootSlot(ctx, uuid.New(), org, slot, MootSourcePublication); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ConsumeMootOffering(ctx, archive, org, slot, slot); err != nil || !ok {
		t.Fatalf("first delivery: consumed=%v err=%v", ok, err)
	}
	// A publication provisions the pool after the first delivery. The organizer and the slot still
	// match, so only the pool predicate in the replay query can refuse this call.
	if err := st.Provision(ctx, uuid.New(), slot, org, 10); err != nil {
		t.Fatal(err)
	}
	ok, err := st.ConsumeMootOffering(ctx, archive, org, slot, slot)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a replay over a pool that now exists reported consumed; the archive would be lost")
	}
	if n := tkt317Consumed(t, ctx, db, archive); n != 1 {
		t.Fatalf("consumed_events rows = %d, want 1", n)
	}
}

// TKT-317 D7: a closure record is kept, and it authorises nothing. The archive it would unlock waits,
// and it is consumed only once a publication record exists for the slot.
func TestTKT317ClosureRecordAuthorisesNoArchive(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot, closeID, archive := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if err := st.RecordMootSlot(ctx, closeID, org, slot, MootSourceClosure); err != nil {
		t.Fatal(err)
	}
	if n := tkt317Consumed(t, ctx, db, closeID); n != 1 {
		t.Fatalf("moot closure has %d consumed_events rows, want 1", n)
	}
	ok, err := st.ConsumeMootOffering(ctx, archive, org, slot, slot)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a closure record authorised an archive")
	}
	if n := tkt317Consumed(t, ctx, db, archive); n != 0 {
		t.Fatalf("refused archive left %d consumed_events rows", n)
	}
	// Positive control: a publication record for the same slot authorises the same archive.
	if err := st.RecordMootSlot(ctx, uuid.New(), org, slot, MootSourcePublication); err != nil {
		t.Fatal(err)
	}
	ok, err = st.ConsumeMootOffering(ctx, archive, org, slot, slot)
	if err != nil || !ok {
		t.Fatalf("with a publication record: consumed=%v err=%v", ok, err)
	}
}

// TKT-317 D7: the record key includes the source. A closure record written first must not stop a
// publication record from being kept, and a repeated closure record must not touch the publication.
func TestTKT317RecordsOfEachSourceAreNotShadowed(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot := uuid.New(), uuid.New()
	closeID, pubID, laterCloseID := uuid.New(), uuid.New(), uuid.New()
	if err := st.RecordMootSlot(ctx, closeID, org, slot, MootSourceClosure); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordMootSlot(ctx, pubID, org, slot, MootSourcePublication); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordMootSlot(ctx, laterCloseID, org, slot, MootSourceClosure); err != nil {
		t.Fatal(err)
	}
	for source, want := range map[MootSource]uuid.UUID{MootSourcePublication: pubID, MootSourceClosure: closeID} {
		var got uuid.UUID
		if err := db.QueryRowContext(ctx, `SELECT source_event_id FROM moot_slots WHERE organizer_id=$1 AND slot_id=$2 AND source=$3`,
			org, slot, string(source)).Scan(&got); err != nil {
			t.Fatalf("%s record missing: %v", source, err)
		}
		if got != want {
			t.Fatalf("%s record source event = %s, want the first %s event %s", source, got, source, want)
		}
	}
	ok, err := st.ConsumeMootOffering(ctx, uuid.New(), org, slot, slot)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the publication record did not authorise the archive once a closure record existed")
	}
}

// TKT-317 R5 (D7): a closure event that its own record consumed must not pass the fallback on a
// replay, because only a publication record authorises. A publication record for the slot is the
// positive control.
func TestTKT317ReplayIsRefusedForAClosureOnlyRecord(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	org, slot, closeID := uuid.New(), uuid.New(), uuid.New()
	if err := st.RecordMootSlot(ctx, closeID, org, slot, MootSourceClosure); err != nil {
		t.Fatal(err)
	}
	// The closure event is already consumed by its record. Only the source predicate in the replay
	// query can refuse this call, because the organizer, the slot and the pool all match.
	ok, err := st.ConsumeMootOffering(ctx, closeID, org, slot, slot)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a replay of a closure event was consumed on its closure record")
	}
	if err := st.RecordMootSlot(ctx, uuid.New(), org, slot, MootSourcePublication); err != nil {
		t.Fatal(err)
	}
	ok, err = st.ConsumeMootOffering(ctx, closeID, org, slot, slot)
	if err != nil || !ok {
		t.Fatalf("replay with a publication record: consumed=%v err=%v", ok, err)
	}
	if n := tkt317Consumed(t, ctx, db, closeID); n != 1 {
		t.Fatalf("consumed_events rows = %d, want 1", n)
	}
}
