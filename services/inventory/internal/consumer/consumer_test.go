package consumer

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

type fakeResolver struct {
	organizerID     uuid.UUID
	capacity        int32
	capacityGroupID *uuid.UUID
	sharedCapacity  *int32
	err             error
}

func (r fakeResolver) PublishedPerformance(context.Context, uuid.UUID) (PublishedPerformance, error) {
	return PublishedPerformance{
		OrganizerID:     r.organizerID,
		Capacity:        r.capacity,
		CapacityGroupID: r.capacityGroupID,
		SharedCapacity:  r.sharedCapacity,
	}, r.err
}

func TestProvisionInputResolvesSchema1Capacity(t *testing.T) {
	organizerID := uuid.New()
	e := publication{ID: uuid.New(), Schema: 1}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = organizerID
	c := &Consumer{resolver: fakeResolver{organizerID: organizerID, capacity: 500}}
	got, err := c.provisionInput(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if got.organizerID != organizerID || got.poolID != e.Data.PerformanceID || got.capacity != 500 {
		t.Fatalf("resolved (%s, %s, %d), want (%s, %s, 500)", got.organizerID, got.poolID, got.capacity, organizerID, e.Data.PerformanceID)
	}
}

func TestProvisionInputUsesOneSharedFestivalPool(t *testing.T) {
	organizerID, groupID := uuid.New(), uuid.New()
	sharedCapacity := int32(1000)
	e := publication{ID: uuid.New(), Schema: 3}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = organizerID
	e.Data.Capacity = 250
	e.Data.CapacityGroupID = &groupID
	e.Data.SharedCapacity = &sharedCapacity

	got, err := (&Consumer{}).provisionInput(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if got.poolID != groupID || got.capacity != sharedCapacity {
		t.Fatalf("pool (%s, %d), want shared festival pool (%s, %d)", got.poolID, got.capacity, groupID, sharedCapacity)
	}
}

func TestProvisionInputResolvesLegacyFestivalGroup(t *testing.T) {
	organizerID, groupID := uuid.New(), uuid.New()
	sharedCapacity := int32(1000)
	e := publication{ID: uuid.New(), Schema: 1}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = organizerID
	c := &Consumer{resolver: fakeResolver{
		organizerID: organizerID, capacityGroupID: &groupID, sharedCapacity: &sharedCapacity,
	}}
	got, err := c.provisionInput(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if got.poolID != groupID || got.capacity != sharedCapacity {
		t.Fatalf("legacy pool (%s, %d), want shared festival pool (%s, %d)", got.poolID, got.capacity, groupID, sharedCapacity)
	}
}

func TestProvisionInputRejectsPartialFestivalCapacity(t *testing.T) {
	groupID := uuid.New()
	sharedCapacity := int32(1000)
	zeroCapacity := int32(0)
	tests := []struct {
		name     string
		groupID  *uuid.UUID
		capacity *int32
	}{
		{name: "both missing"},
		{name: "shared capacity missing", groupID: &groupID},
		{name: "group missing", capacity: &sharedCapacity},
		{name: "zero group", groupID: new(uuid.UUID), capacity: &sharedCapacity},
		{name: "zero shared capacity", groupID: &groupID, capacity: &zeroCapacity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := publication{ID: uuid.New(), Schema: 3}
			e.Data.PerformanceID = uuid.New()
			e.Data.OrganizerID = uuid.New()
			e.Data.CapacityGroupID = tt.groupID
			e.Data.SharedCapacity = tt.capacity
			if _, err := (&Consumer{}).provisionInput(context.Background(), e); err == nil {
				t.Fatal("expected invalid schema-3 festival capacity to be rejected")
			}
		})
	}
}

func TestProvisionInputRejectsFestivalCapacityOnSchema2(t *testing.T) {
	groupID := uuid.New()
	sharedCapacity := int32(1000)
	e := publication{ID: uuid.New(), Schema: 2}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = uuid.New()
	e.Data.Capacity = 250
	e.Data.CapacityGroupID = &groupID
	e.Data.SharedCapacity = &sharedCapacity
	if _, err := (&Consumer{}).provisionInput(context.Background(), e); err == nil {
		t.Fatal("expected schema-2 festival fields to be rejected")
	}
}

func TestProvisionInputUsesPerSlotPoolWithoutFestivalGroup(t *testing.T) {
	e := publication{ID: uuid.New(), Schema: 2}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = uuid.New()
	e.Data.Capacity = 250

	got, err := (&Consumer{}).provisionInput(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if got.poolID != e.Data.PerformanceID || got.capacity != e.Data.Capacity {
		t.Fatalf("pool (%s, %d), want slot pool (%s, %d)", got.poolID, got.capacity, e.Data.PerformanceID, e.Data.Capacity)
	}
}

func TestProvisionInputRejectsSchema1CatalogMismatch(t *testing.T) {
	e := publication{ID: uuid.New(), Schema: 1}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = uuid.New()
	c := &Consumer{resolver: fakeResolver{organizerID: uuid.New(), capacity: 500}}
	if _, err := c.provisionInput(context.Background(), e); err == nil {
		t.Fatal("expected catalog mismatch")
	}
}

// This asserts classification only. It passed before TKT-61 and the event was still dropped:
// the error was never the bug, the disposition was. TestUnknownSchemaVersionSkewIsParked is the
// one that fails if Term() comes back — keep the two separate.
func TestProvisionInputRejectsUnsupportedSchema(t *testing.T) {
	e := publication{ID: uuid.New(), Schema: 4}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = uuid.New()
	c := &Consumer{resolver: fakeResolver{err: errors.New("must not resolve")}}
	_, err := c.provisionInput(context.Background(), e)
	if !errors.Is(err, errUnsupportedPublicationSchema) {
		t.Fatalf("err = %v, want it to wrap errUnsupportedPublicationSchema", err)
	}
}

// The version-skew regression (TKT-61, ADR-017 §5b): an inventory binary meets a retained variant
// it does not know — by rollback, by durable recreation, or merely by restarting across a window
// where catalog emitted it. The event is well-formed and a newer binary can provision it, so it
// must be parked for that binary. Term() would advance the durable consumer past it for good.
func TestUnknownSchemaVersionSkewIsParked(t *testing.T) {
	// 4: a variant a newer catalog emits. 0: an envelope with no `schema` at all (ADR-009 §5
	// requires one, so this is a producer bug) — parked too, deliberately. Parking a bad event
	// costs noise; terminating a real variant costs inventory nobody notices is missing.
	for _, schema := range []int{4, 0} {
		e := publication{ID: uuid.New(), Schema: schema}
		e.Data.PerformanceID = uuid.New()
		e.Data.OrganizerID = uuid.New()
		e.Data.Capacity = 250
		c := &Consumer{resolver: fakeResolver{err: errors.New("must not resolve")}}

		_, err := c.provisionInput(context.Background(), e)
		if !errors.Is(err, errUnsupportedPublicationSchema) {
			t.Fatalf("schema %d: err = %v, want it to wrap errUnsupportedPublicationSchema", schema, err)
		}
		if got := dispositionForProvisionError(schema, err); got != dispositionPark {
			t.Fatalf("schema %d: disposition = %q, want %q — Term() drops the event with no retry", schema, got, dispositionPark)
		}
	}
}

// The other side of the rule: a payload that is bad at a schema this binary *does* know is poison —
// no binary can provision it — so it still terminates. This is what stops parking from wedging the
// consumer, and it keeps ADR-017 §4's negative validation meaningful.
func TestInvalidKnownSchemaIsTerminated(t *testing.T) {
	groupID := uuid.New()
	sharedCapacity := int32(1000)
	e := publication{ID: uuid.New(), Schema: 2}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = uuid.New()
	e.Data.Capacity = 250
	e.Data.CapacityGroupID = &groupID
	e.Data.SharedCapacity = &sharedCapacity

	_, err := (&Consumer{}).provisionInput(context.Background(), e)
	if err == nil {
		t.Fatal("expected schema-2 festival fields to be rejected")
	}
	if got := dispositionForProvisionError(e.Schema, err); got != dispositionTerminate {
		t.Fatalf("disposition = %q, want %q — a corrupt known variant must not retry forever", got, dispositionTerminate)
	}
}

// Schema 1 resolves against catalog, so its failures are transient by nature. Unchanged by TKT-61;
// asserted because the call site that decided it was reshaped.
func TestSchema1ResolutionFailureIsRetried(t *testing.T) {
	e := publication{ID: uuid.New(), Schema: 1}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = uuid.New()
	c := &Consumer{resolver: fakeResolver{err: errors.New("catalog unreachable")}}

	_, err := c.provisionInput(context.Background(), e)
	if err == nil {
		t.Fatal("expected resolver error")
	}
	if got := dispositionForProvisionError(e.Schema, err); got != dispositionRetry {
		t.Fatalf("disposition = %q, want %q", got, dispositionRetry)
	}
}
