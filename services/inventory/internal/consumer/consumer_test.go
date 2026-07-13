package consumer

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

type fakeResolver struct {
	organizerID uuid.UUID
	capacity    int32
	err         error
}

func (r fakeResolver) PublishedPerformance(context.Context, uuid.UUID) (uuid.UUID, int32, error) {
	return r.organizerID, r.capacity, r.err
}

func TestProvisionInputResolvesSchema1Capacity(t *testing.T) {
	organizerID := uuid.New()
	e := publication{ID: uuid.New(), Schema: 1}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = organizerID
	c := &Consumer{resolver: fakeResolver{organizerID: organizerID, capacity: 500}}
	gotOrganizer, gotCapacity, err := c.provisionInput(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if gotOrganizer != organizerID || gotCapacity != 500 {
		t.Fatalf("resolved (%s, %d), want (%s, 500)", gotOrganizer, gotCapacity, organizerID)
	}
}

func TestProvisionInputRejectsSchema1CatalogMismatch(t *testing.T) {
	e := publication{ID: uuid.New(), Schema: 1}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = uuid.New()
	c := &Consumer{resolver: fakeResolver{organizerID: uuid.New(), capacity: 500}}
	if _, _, err := c.provisionInput(context.Background(), e); err == nil {
		t.Fatal("expected catalog mismatch")
	}
}

func TestProvisionInputRejectsUnsupportedSchema(t *testing.T) {
	e := publication{ID: uuid.New(), Schema: 3}
	e.Data.PerformanceID = uuid.New()
	e.Data.OrganizerID = uuid.New()
	c := &Consumer{resolver: fakeResolver{err: errors.New("must not resolve")}}
	if _, _, err := c.provisionInput(context.Background(), e); err == nil {
		t.Fatal("expected unsupported schema error")
	}
}
