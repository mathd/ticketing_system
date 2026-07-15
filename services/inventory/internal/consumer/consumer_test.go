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
	e := publication{ID: uuid.New(), Schema: 2}
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
		organizerID: organizerID, capacity: 250, capacityGroupID: &groupID, sharedCapacity: &sharedCapacity,
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
	e := publication{ID: uuid.New(), Schema: 2}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = uuid.New()
	e.Data.Capacity = 250
	e.Data.CapacityGroupID = &groupID
	if _, err := (&Consumer{}).provisionInput(context.Background(), e); err == nil {
		t.Fatal("expected partial festival capacity to be rejected")
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

func TestProvisionInputRejectsUnsupportedSchema(t *testing.T) {
	e := publication{ID: uuid.New(), Schema: 3}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = uuid.New()
	c := &Consumer{resolver: fakeResolver{err: errors.New("must not resolve")}}
	if _, err := c.provisionInput(context.Background(), e); err == nil {
		t.Fatal("expected unsupported schema error")
	}
}
