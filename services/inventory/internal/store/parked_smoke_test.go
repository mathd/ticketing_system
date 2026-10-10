//go:build smoke

package store

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TKT-317: a parked catalog event keeps the exact bytes of its first copy. A duplicate with the
// same bytes is a no-op. Different bytes are refused and never overwrite the first copy.

func TestTKT317ParkedEnvelopeKeepsItsFirstCopy(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	first := ParkedCatalogEvent{
		Subject:     "platform.catalog.performance.closed",
		EventID:     uuid.New(),
		OrganizerID: uuid.New(),
		SlotID:      uuid.New(),
		Schema:      1,
		Envelope:    []byte(`{"id":"first","schema":1,"data":{"closure_version":2}}`),
		Deliveries:  5,
		Reason:      "catalog answer unusable",
	}
	inserted, err := st.ParkCatalogEvent(ctx, first)
	if err != nil || !inserted {
		t.Fatalf("first park: inserted=%v err=%v", inserted, err)
	}
	// A byte-identical redelivery, for example after a crash between park and Term, is a no-op
	// and keeps the first delivery count.
	again := first
	again.Deliveries = 6
	inserted, err = st.ParkCatalogEvent(ctx, again)
	if err != nil || inserted {
		t.Fatalf("identical duplicate: inserted=%v err=%v, want a silent no-op", inserted, err)
	}
	// Different bytes under the same key are refused, and the first copy is left as it was.
	other := first
	other.Envelope = []byte(`{"id":"second","schema":1,"data":{"closure_version":9}}`)
	inserted, err = st.ParkCatalogEvent(ctx, other)
	if !errors.Is(err, ErrCatalogParkedCollision) || inserted {
		t.Fatalf("conflicting duplicate: inserted=%v err=%v, want ErrCatalogParkedCollision", inserted, err)
	}
	var stored []byte
	var deliveries int64
	var reason string
	if err = db.QueryRowContext(ctx, `SELECT envelope, delivery_count, reason FROM catalog_event_parked
		WHERE subject=$1 AND event_id=$2`, first.Subject, first.EventID).Scan(&stored, &deliveries, &reason); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, first.Envelope) {
		t.Fatalf("stored envelope = %q, want the first copy %q", stored, first.Envelope)
	}
	if deliveries != 5 || reason != first.Reason {
		t.Fatalf("stored deliveries=%d reason=%q, want the first park's 5 and %q", deliveries, reason, first.Reason)
	}
}

func TestTKT317ParkedRowsDoNotLatchStartupReadiness(t *testing.T) {
	ctx, st, db := storeForTest(t, time.Minute)
	parkedID := uuid.New()
	inserted, err := st.ParkCatalogEvent(ctx, ParkedCatalogEvent{
		Subject:     "platform.catalog.performance.reopened",
		EventID:     parkedID,
		OrganizerID: uuid.New(),
		SlotID:      uuid.New(),
		Schema:      1,
		Envelope:    []byte("{}"),
		Deliveries:  5,
		Reason:      "catalog answer unusable",
	})
	if err != nil || !inserted {
		t.Fatalf("park: inserted=%v err=%v", inserted, err)
	}
	// The readiness check below only proves something if the parked row was really written.
	var rows int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM catalog_event_parked WHERE event_id=$1`, parkedID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("parked rows = %d err=%v, want 1 before the readiness check", rows, err)
	}
	// Parked rows are not version skew, so the startup check must not see them.
	pending, err := st.HasPendingCatalogQuarantine(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("a parked catalog event latched startup readiness")
	}
	// Positive control: a real quarantine row does latch it, so the check is not always false.
	if err = st.QuarantineCatalogEvent(ctx, "platform.catalog.performance.published", uuid.New(), 9, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	pending, err = st.HasPendingCatalogQuarantine(ctx)
	if err != nil || !pending {
		t.Fatalf("quarantined skew event: pending=%v err=%v, want true", pending, err)
	}
}
