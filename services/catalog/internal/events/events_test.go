package events

import (
	"testing"

	"github.com/google/uuid"

	"ticketing/services/catalog/internal/store"
)

func TestFestivalCapacityInvariant(t *testing.T) {
	groupID := uuid.New()
	shared := int32(1000)
	tests := []struct {
		name string
		perf store.Performance
		want bool
	}{
		{name: "grouped festival day", perf: store.Performance{Kind: store.KindFestivalDay, CapacityGroupID: &groupID, SharedCapacity: &shared}, want: true},
		{name: "plain performance", perf: store.Performance{Kind: store.KindPerformance, CapacityGroupID: &groupID, SharedCapacity: &shared}},
		{name: "missing shared capacity", perf: store.Performance{Kind: store.KindFestivalDay, CapacityGroupID: &groupID}},
		{name: "missing group", perf: store.Performance{Kind: store.KindFestivalDay, SharedCapacity: &shared}},
		{name: "zero shared capacity", perf: store.Performance{Kind: store.KindFestivalDay, CapacityGroupID: &groupID, SharedCapacity: new(int32)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			group, capacity := festivalCapacity(tt.perf)
			if (group != nil && capacity != nil) != tt.want {
				t.Fatalf("festivalCapacity() = (%v, %v), want both set=%v", group, capacity, tt.want)
			}
			if (group == nil) != (capacity == nil) {
				t.Fatalf("festival fields must be both set or both absent: (%v, %v)", group, capacity)
			}
		})
	}
}
