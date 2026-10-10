//go:build smoke

package consumer

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"ticketing/services/inventory/internal/store"
)

// TKT-317 at the consumer tier: the handler runs against a real PostgreSQL store, and each
// delivery is a fake. The state a disposition leaves is read back from the database, not from a
// recorder. No broker is used (TKT-317 D3). The broker's redelivery clock is not tested here.

// The envelopes are handwritten literals, never built from the types under test (ADR-017).
func tkt317Msg(subject, body string) *fakeMsg {
	return &fakeMsg{subject: subject, data: []byte(withSubjectType(subject, body))}
}

func tkt317SchemaOnePublication(id, slot, org uuid.UUID) string {
	return fmt.Sprintf(`{"id":"%s","schema":1,"data":{"performance_id":"%s","organizer_id":"%s"}}`, id, slot, org)
}

func tkt317SchemaTwoPublication(id, slot, org uuid.UUID, capacity int) string {
	return fmt.Sprintf(`{"id":"%s","schema":2,"data":{"performance_id":"%s","organizer_id":"%s","capacity":%d}}`, id, slot, org, capacity)
}

func tkt317Archive(id, slot, org uuid.UUID) string {
	return fmt.Sprintf(`{"id":"%s","schema":2,"data":{"performance_id":"%s","organizer_id":"%s"}}`, id, slot, org)
}

func tkt317Closure(id, slot, org uuid.UUID, version int) string {
	return fmt.Sprintf(`{"id":"%s","schema":1,"data":{"performance_id":"%s","organizer_id":"%s","closure_version":%d}}`, id, slot, org, version)
}

// tkt317Store opens an isolated schema, as the store's own smoke helper does, and migrates it.
func tkt317Store(t *testing.T) (context.Context, *store.Postgres, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("INVENTORY_MIGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("INVENTORY_MIGRATION_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	schema := "consumer_tkt317_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") })
	db, err := sql.Open("pgx", dsn+"?search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return ctx, store.New(db, time.Minute), db
}

func tkt317Count(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func tkt317String(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var s string
	if err := db.QueryRowContext(ctx, query, args...).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// COS1 (TKT-317): a schema-1 publication that catalog no longer publishes is acked as moot only
// after its tombstone and its consumed_events row are committed. The archive that follows is then
// acked, and no pool is ever created for the slot.
func TestTKT317MootPublicationIsDurableBeforeItsAck(t *testing.T) {
	ctx, st, db := tkt317Store(t)
	org, slot, pubID, archiveID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	c := offeringConsumer(st, fakeResolver{err: ErrPerformanceNotFound})

	pub := tkt317Msg(subjectPublished, tkt317SchemaOnePublication(pubID, slot, org))
	pub.onAck = func() {
		if n := tkt317Count(t, ctx, db, `SELECT count(*) FROM moot_slots WHERE organizer_id=$1 AND slot_id=$2`, org, slot); n != 1 {
			t.Errorf("publication acked with %d tombstones; it must commit the tombstone first", n)
		}
		if n := tkt317Count(t, ctx, db, `SELECT count(*) FROM consumed_events WHERE event_id=$1`, pubID); n != 1 {
			t.Errorf("publication acked with %d consumed_events rows", n)
		}
	}
	c.handle(ctx, pub)
	if !slices.Contains(pub.actions, "ack") {
		t.Fatalf("publication actions = %v, want ack", pub.actions)
	}

	arch := tkt317Msg(subjectArchived, tkt317Archive(archiveID, slot, org))
	c.handle(ctx, arch)
	if !slices.Contains(arch.actions, "ack") {
		t.Fatalf("archive actions = %v, want ack: the tombstone must let it through", arch.actions)
	}
	if n := tkt317Count(t, ctx, db, `SELECT count(*) FROM inventory_pools WHERE slot_id=$1`, slot); n != 0 {
		t.Fatalf("%d pools exist for a slot that was never published", n)
	}
	if n := tkt317Count(t, ctx, db, `SELECT count(*) FROM consumed_events WHERE event_id=$1`, archiveID); n != 1 {
		t.Fatalf("archive has %d consumed_events rows, want 1", n)
	}
	if !c.Ready() {
		t.Fatal("a moot publication latched readiness")
	}
}

// COS2 (TKT-317): a moot closure records its own tombstone before its ack, and the archive that
// follows completes. Both closure subjects are covered.
func TestTKT317MootClosureIsDurableBeforeItsAck(t *testing.T) {
	for _, subject := range []string{subjectClosed, subjectReopened} {
		t.Run(subject, func(t *testing.T) {
			ctx, st, db := tkt317Store(t)
			org, slot, closeID, archiveID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			c := offeringConsumer(st, fakeResolver{err: ErrPerformanceNotFound})

			closure := tkt317Msg(subject, tkt317Closure(closeID, slot, org, 2))
			closure.onAck = func() {
				if n := tkt317Count(t, ctx, db, `SELECT count(*) FROM moot_slots WHERE organizer_id=$1 AND slot_id=$2`, org, slot); n != 1 {
					t.Errorf("closure acked with %d tombstones; it must commit the tombstone first", n)
				}
				if n := tkt317Count(t, ctx, db, `SELECT count(*) FROM consumed_events WHERE event_id=$1`, closeID); n != 1 {
					t.Errorf("closure acked with %d consumed_events rows", n)
				}
			}
			c.handle(ctx, closure)
			if !slices.Contains(closure.actions, "ack") {
				t.Fatalf("closure actions = %v, want ack", closure.actions)
			}

			arch := tkt317Msg(subjectArchived, tkt317Archive(archiveID, slot, org))
			c.handle(ctx, arch)
			if !slices.Contains(arch.actions, "ack") {
				t.Fatalf("archive actions = %v, want ack", arch.actions)
			}
			if n := tkt317Count(t, ctx, db, `SELECT count(*) FROM inventory_pools WHERE slot_id=$1`, slot); n != 0 {
				t.Fatalf("%d pools exist for a slot that was never published", n)
			}
		})
	}
}

// COS2 (TKT-317), resolved branch: a closure whose catalog answer succeeds finds no pool, and the
// tombstone from an earlier moot publication lets it through. Catalog does not produce this state
// today (ADR-077). COS2 requires the branch, so the test pins the branch.
func TestTKT317ClosureOnAResolvedSlotWithoutPoolUsesTheFallback(t *testing.T) {
	ctx, st, db := tkt317Store(t)
	org, slot, pubID, closeID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	moot := offeringConsumer(st, fakeResolver{err: ErrPerformanceNotFound})
	pub := tkt317Msg(subjectPublished, tkt317SchemaOnePublication(pubID, slot, org))
	moot.handle(ctx, pub)
	if !slices.Contains(pub.actions, "ack") {
		t.Fatalf("publication actions = %v, want ack", pub.actions)
	}

	c := offeringConsumer(st, fakeResolver{organizerID: org, capacity: 10})
	closure := tkt317Msg(subjectClosed, tkt317Closure(closeID, slot, org, 1))
	c.handle(ctx, closure)
	if !slices.Contains(closure.actions, "ack") {
		t.Fatalf("closure actions = %v, want ack via the fallback", closure.actions)
	}
	if n := tkt317Count(t, ctx, db, `SELECT count(*) FROM consumed_events WHERE event_id=$1`, closeID); n != 1 {
		t.Fatalf("closure has %d consumed_events rows, want 1", n)
	}
	if n := tkt317Count(t, ctx, db, `SELECT count(*) FROM inventory_pools WHERE slot_id=$1`, slot); n != 0 {
		t.Fatalf("%d pools created by a closure", n)
	}
}

// COS3 (TKT-317): an archive with no pool and no tombstone waits, and consumes nothing. Once the
// publication provisions the pool, the same archive applies to that pool.
func TestTKT317ArchiveWaitsForItsPublicationThenApplies(t *testing.T) {
	ctx, st, db := tkt317Store(t)
	org, slot, pubID, archiveID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	c := offeringConsumer(st, nil)

	early := tkt317Msg(subjectArchived, tkt317Archive(archiveID, slot, org))
	c.handle(ctx, early)
	if !slices.Contains(early.actions, "nak-delay") {
		t.Fatalf("early archive actions = %v, want a delayed retry", early.actions)
	}
	if n := tkt317Count(t, ctx, db, `SELECT count(*) FROM consumed_events WHERE event_id=$1`, archiveID); n != 0 {
		t.Fatalf("a waiting archive left %d consumed_events rows", n)
	}

	pub := tkt317Msg(subjectPublished, tkt317SchemaTwoPublication(pubID, slot, org, 10))
	c.handle(ctx, pub)
	if !slices.Contains(pub.actions, "ack") {
		t.Fatalf("publication actions = %v, want ack", pub.actions)
	}

	redelivered := tkt317Msg(subjectArchived, tkt317Archive(archiveID, slot, org))
	c.handle(ctx, redelivered)
	if !slices.Contains(redelivered.actions, "ack") {
		t.Fatalf("redelivered archive actions = %v, want ack", redelivered.actions)
	}
	a, err := st.Availability(ctx, org, slot, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.OfferingStatus != "archived" {
		t.Fatalf("offering status = %q after the archive applied, want archived", a.OfferingStatus)
	}
}

// COS4 (TKT-317): a distinct publication for a slot that already has a tombstone provisions
// normally. The closure and the archive that follow apply to that pool, and are not skipped
// because of the tombstone.
func TestTKT317RepublishProvisionsDespiteATombstone(t *testing.T) {
	ctx, st, db := tkt317Store(t)
	org, slot := uuid.New(), uuid.New()

	// A moot closure leaves the tombstone. Its own event id is not the publication's.
	moot := offeringConsumer(st, fakeResolver{err: ErrPerformanceNotFound})
	mootClosure := tkt317Msg(subjectClosed, tkt317Closure(uuid.New(), slot, org, 1))
	moot.handle(ctx, mootClosure)
	if !slices.Contains(mootClosure.actions, "ack") {
		t.Fatalf("moot closure actions = %v, want ack", mootClosure.actions)
	}
	if n := tkt317Count(t, ctx, db, `SELECT count(*) FROM moot_slots WHERE organizer_id=$1 AND slot_id=$2`, org, slot); n != 1 {
		t.Fatalf("tombstone rows = %d, want 1 before the republish", n)
	}

	c := offeringConsumer(st, fakeResolver{organizerID: org, capacity: 10})
	pub := tkt317Msg(subjectPublished, tkt317SchemaTwoPublication(uuid.New(), slot, org, 10))
	c.handle(ctx, pub)
	if !slices.Contains(pub.actions, "ack") {
		t.Fatalf("republish actions = %v, want ack", pub.actions)
	}
	if n := tkt317Count(t, ctx, db, `SELECT count(*) FROM inventory_pools WHERE slot_id=$1`, slot); n != 1 {
		t.Fatalf("republish created %d pools, want 1", n)
	}

	closure := tkt317Msg(subjectClosed, tkt317Closure(uuid.New(), slot, org, 2))
	c.handle(ctx, closure)
	if !slices.Contains(closure.actions, "ack") {
		t.Fatalf("closure actions = %v, want ack", closure.actions)
	}
	if got := tkt317String(t, ctx, db, `SELECT closure_status FROM inventory_pools WHERE slot_id=$1`, slot); got != "closed" {
		t.Fatalf("pool closure_status = %q after the closure, want closed: the tombstone must not skip the apply", got)
	}

	arch := tkt317Msg(subjectArchived, tkt317Archive(uuid.New(), slot, org))
	c.handle(ctx, arch)
	if !slices.Contains(arch.actions, "ack") {
		t.Fatalf("archive actions = %v, want ack", arch.actions)
	}
	if got := tkt317String(t, ctx, db, `SELECT lifecycle_status FROM inventory_pools WHERE slot_id=$1`, slot); got != "archived" {
		t.Fatalf("pool lifecycle_status = %q after the archive, want archived: the tombstone must not skip the apply", got)
	}
}

// COS5 (TKT-317): an unusable catalog answer retries below the bound. At the bound, the exact
// envelope is committed to catalog_event_parked, the message is terminated, and nothing is consumed.
func TestTKT317UnusableAnswerParksItsExactBytesAtTheBound(t *testing.T) {
	for _, subject := range []string{subjectPublished, subjectClosed} {
		t.Run(subject, func(t *testing.T) {
			ctx, st, db := tkt317Store(t)
			org, slot, id := uuid.New(), uuid.New(), uuid.New()
			body := tkt317Closure(id, slot, org, 1)
			if subject == subjectPublished {
				body = tkt317SchemaOnePublication(id, slot, org)
			}
			c := offeringConsumer(st, fakeResolver{err: errCatalogUnusable})

			early := tkt317Msg(subject, body)
			early.delivered = parkAfterDeliveries - 1
			c.handle(ctx, early)
			if !slices.Contains(early.actions, "nak-delay") {
				t.Fatalf("delivery %d actions = %v, want a delayed retry", early.delivered, early.actions)
			}
			if n := tkt317Count(t, ctx, db, `SELECT count(*) FROM catalog_event_parked`); n != 0 {
				t.Fatalf("%d rows parked before the bound", n)
			}

			atBound := tkt317Msg(subject, body)
			atBound.delivered = parkAfterDeliveries
			c.handle(ctx, atBound)
			if !slices.Contains(atBound.actions, "term") {
				t.Fatalf("delivery %d actions = %v, want term", atBound.delivered, atBound.actions)
			}
			var stored []byte
			var deliveries int64
			var storedSubject string
			if err := db.QueryRowContext(ctx, `SELECT envelope, delivery_count, subject FROM catalog_event_parked WHERE event_id=$1`, id).
				Scan(&stored, &deliveries, &storedSubject); err != nil {
				t.Fatalf("parked row missing: %v", err)
			}
			if !bytes.Equal(stored, atBound.data) {
				t.Fatalf("parked bytes = %q, want the delivered envelope %q", stored, atBound.data)
			}
			if deliveries != parkAfterDeliveries || storedSubject != subject {
				t.Fatalf("parked delivery_count=%d subject=%q, want %d and %q", deliveries, storedSubject, parkAfterDeliveries, subject)
			}
			if n := tkt317Count(t, ctx, db, `SELECT count(*) FROM consumed_events WHERE event_id=$1`, id); n != 0 {
				t.Fatalf("a parked event left %d consumed_events rows; a re-driven copy must still apply", n)
			}
			if !c.Ready() {
				t.Fatal("a parked event latched readiness")
			}
		})
	}
}

// COS5 (TKT-317), real resolver: the classification in CatalogResolver feeds the disposition. A
// catalog that answers 200 with a body that cannot be used parks the closure at the bound. A
// catalog that cannot be reached is retried past the bound, and nothing is parked for it. The fake
// resolvers above inject the class directly, so this test is the one that exercises HTTP.
func TestTKT317CatalogAnswersThatCannotBeUsedParkThroughTheRealResolver(t *testing.T) {
	ctx, st, db := tkt317Store(t)
	org, slot, id := uuid.New(), uuid.New(), uuid.New()
	garbled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>maintenance</html>`))
	}))
	defer garbled.Close()
	c := offeringConsumer(st, NewCatalogResolver(garbled.URL, "token", garbled.Client()))
	body := tkt317Closure(id, slot, org, 1)

	early := tkt317Msg(subjectClosed, body)
	early.delivered = parkAfterDeliveries - 1
	c.handle(ctx, early)
	if !slices.Contains(early.actions, "nak-delay") {
		t.Fatalf("delivery %d actions = %v, want a delayed retry", early.delivered, early.actions)
	}
	atBound := tkt317Msg(subjectClosed, body)
	atBound.delivered = parkAfterDeliveries
	c.handle(ctx, atBound)
	if !slices.Contains(atBound.actions, "term") {
		t.Fatalf("delivery %d actions = %v, want term", atBound.delivered, atBound.actions)
	}
	if n := tkt317Count(t, ctx, db, `SELECT count(*) FROM catalog_event_parked WHERE event_id=$1`, id); n != 1 {
		t.Fatalf("parked rows = %d, want 1", n)
	}

	// Transport: nothing listens at the address, so the failure is never a bad answer.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	transportID := uuid.New()
	down := offeringConsumer(st, NewCatalogResolver(deadURL, "token", http.DefaultClient))
	outage := tkt317Msg(subjectClosed, tkt317Closure(transportID, slot, org, 2))
	outage.delivered = parkAfterDeliveries + 1
	down.handle(ctx, outage)
	if !slices.Contains(outage.actions, "nak-delay") {
		t.Fatalf("outage past the bound actions = %v, want a delayed retry", outage.actions)
	}
	if n := tkt317Count(t, ctx, db, `SELECT count(*) FROM catalog_event_parked WHERE event_id=$1`, transportID); n != 0 {
		t.Fatalf("a transport failure was parked (%d rows)", n)
	}

	// The same holds for a publication. Its outage past the bound is retried, and is never parked.
	pubID := uuid.New()
	pubOutage := tkt317Msg(subjectPublished, tkt317SchemaOnePublication(pubID, slot, org))
	pubOutage.delivered = parkAfterDeliveries + 1
	down.handle(ctx, pubOutage)
	if !slices.Contains(pubOutage.actions, "nak-delay") {
		t.Fatalf("publication outage past the bound actions = %v, want a delayed retry", pubOutage.actions)
	}
	if n := tkt317Count(t, ctx, db, `SELECT count(*) FROM catalog_event_parked WHERE event_id=$1`, pubID); n != 0 {
		t.Fatalf("a publication transport failure was parked (%d rows)", n)
	}
}

// COS6 (TKT-317): parked rows never latch startup readiness. A restart with parked rows present
// comes up ready. A quarantined version-skew event, the control, keeps the consumer unready.
func TestTKT317RestartWithParkedRowsStaysReady(t *testing.T) {
	ctx, st, _ := tkt317Store(t)
	org, slot, id := uuid.New(), uuid.New(), uuid.New()
	first := offeringConsumer(st, fakeResolver{err: errCatalogUnusable})
	msg := tkt317Msg(subjectClosed, tkt317Closure(id, slot, org, 1))
	msg.delivered = parkAfterDeliveries
	first.handle(ctx, msg)
	if !slices.Contains(msg.actions, "term") {
		t.Fatalf("parking actions = %v, want term", msg.actions)
	}

	// A new process starts unready, then reads the database at startup.
	restarted := offeringConsumer(st, nil)
	restarted.ready.Store(false)
	if err := restarted.refreshStartupReadiness(ctx); err != nil {
		t.Fatal(err)
	}
	if !restarted.Ready() {
		t.Fatal("a restart with parked rows stayed unready")
	}

	// Control: an unresolved quarantined version-skew event does keep the consumer unready.
	if err := st.QuarantineCatalogEvent(ctx, subjectPublished, uuid.New(), 9, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	skewed := offeringConsumer(st, nil)
	skewed.ready.Store(false)
	if err := skewed.refreshStartupReadiness(ctx); err != nil {
		t.Fatal(err)
	}
	if skewed.Ready() {
		t.Fatal("a pending version-skew quarantine did not keep the restarted consumer unready")
	}
}
