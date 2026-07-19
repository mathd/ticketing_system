package events

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"ticketing/services/catalog/internal/store"
)

func TestPerformancePublishedEnvelopeSchemas(t *testing.T) {
	groupID := uuid.New()
	shared := int32(1000)
	publishedAt := time.Now().UTC()
	tests := []struct {
		name       string
		perf       store.Performance
		wantSchema int
		wantGroup  bool
	}{
		{
			name: "plain publication stays schema 2",
			perf: store.Performance{
				ID: uuid.New(), EventID: uuid.New(), OrganizerID: uuid.New(), Kind: store.KindPerformance,
				Capacity: 250, PublishedAt: &publishedAt,
			},
			wantSchema: 2,
		},
		{
			name: "grouped festival publication uses schema 3",
			perf: store.Performance{
				ID: uuid.New(), EventID: uuid.New(), OrganizerID: uuid.New(), Kind: store.KindFestivalDay,
				Capacity: 250, CapacityGroupID: &groupID, SharedCapacity: &shared, PublishedAt: &publishedAt,
			},
			wantSchema: 3,
			wantGroup:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := performancePublishedEnvelope(tt.perf, publishedAt)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				ID     string `json:"id"`
				Schema int    `json:"schema"`
				Data   struct {
					CapacityGroupID *uuid.UUID `json:"capacity_group_id"`
					SharedCapacity  *int32     `json:"shared_capacity"`
				} `json:"data"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if got.ID != EventID(tt.perf) || got.Schema != tt.wantSchema {
				t.Fatalf("envelope id/schema = %s/%d, want %s/%d", got.ID, got.Schema, EventID(tt.perf), tt.wantSchema)
			}
			if (got.Data.CapacityGroupID != nil) != tt.wantGroup || (got.Data.SharedCapacity != nil) != tt.wantGroup {
				t.Fatalf("festival fields = (%v, %v), want present=%v", got.Data.CapacityGroupID, got.Data.SharedCapacity, tt.wantGroup)
			}
		})
	}
}

// TestPerformancePublishedEnvelopeCarriesReEntryPolicy pins the TKT-87
// additive ride-along (ADR-017 §2: no bump — no deployed consumer forks on the
// field): re_entry is present on every publication, at the UNCHANGED schemas 2
// and 3, so access's policy projection can read it while inventory keeps
// ignoring it. The decode struct here is hand-written on purpose — a fixture
// built from PerformancePublishedData could not fail (ADR-017 §5b′ trap).
func TestPerformancePublishedEnvelopeCarriesReEntryPolicy(t *testing.T) {
	publishedAt := time.Now().UTC()
	groupID := uuid.New()
	shared := int32(1000)
	maxEntries := int32(3)
	tests := []struct {
		name        string
		perf        store.Performance
		wantSchema  int
		wantMode    string
		wantMax     *int32
		wantExit    bool
	}{
		{
			name: "single performance carries explicit single policy at schema 2",
			perf: store.Performance{
				ID: uuid.New(), EventID: uuid.New(), OrganizerID: uuid.New(), Kind: store.KindPerformance,
				Capacity: 250, PublishedAt: &publishedAt,
				ReEntry: store.ReEntryPolicy{Mode: "single"},
			},
			wantSchema: 2, wantMode: "single",
		},
		{
			name: "multi operating day rides at schema 2",
			perf: store.Performance{
				ID: uuid.New(), EventID: uuid.New(), OrganizerID: uuid.New(), Kind: store.KindOperatingDay,
				Capacity: 5000, PublishedAt: &publishedAt,
				ReEntry: store.ReEntryPolicy{Mode: "multi", RequiresExit: true},
			},
			wantSchema: 2, wantMode: "multi", wantExit: true,
		},
		{
			name: "count-limited grouped festival day rides at schema 3",
			perf: store.Performance{
				ID: uuid.New(), EventID: uuid.New(), OrganizerID: uuid.New(), Kind: store.KindFestivalDay,
				Capacity: 250, CapacityGroupID: &groupID, SharedCapacity: &shared, PublishedAt: &publishedAt,
				ReEntry: store.ReEntryPolicy{Mode: "count_limited", MaxEntries: &maxEntries},
			},
			wantSchema: 3, wantMode: "count_limited", wantMax: &maxEntries,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := performancePublishedEnvelope(tt.perf, publishedAt)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Schema int `json:"schema"`
				Data   struct {
					ReEntry *struct {
						Mode         string `json:"mode"`
						MaxEntries   *int32 `json:"max_entries"`
						RequiresExit bool   `json:"requires_exit"`
					} `json:"re_entry"`
				} `json:"data"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if got.Schema != tt.wantSchema {
				t.Fatalf("schema = %d, want %d (the ride-along must not bump)", got.Schema, tt.wantSchema)
			}
			if got.Data.ReEntry == nil {
				t.Fatal("data.re_entry absent, want explicit policy on every publication")
			}
			if got.Data.ReEntry.Mode != tt.wantMode || got.Data.ReEntry.RequiresExit != tt.wantExit {
				t.Fatalf("re_entry = %+v, want mode=%s requires_exit=%v", got.Data.ReEntry, tt.wantMode, tt.wantExit)
			}
			if (got.Data.ReEntry.MaxEntries == nil) != (tt.wantMax == nil) {
				t.Fatalf("max_entries presence = %v, want %v", got.Data.ReEntry.MaxEntries, tt.wantMax)
			}
			if tt.wantMax != nil && *got.Data.ReEntry.MaxEntries != *tt.wantMax {
				t.Fatalf("max_entries = %d, want %d", *got.Data.ReEntry.MaxEntries, *tt.wantMax)
			}
		})
	}
}

func TestPerformancePublishedFailsClosedForCorruptGroup(t *testing.T) {
	groupID := uuid.New()
	shared := int32(1000)
	tests := []store.Performance{
		{Kind: store.KindPerformance, CapacityGroupID: &groupID, SharedCapacity: &shared},
		{Kind: store.KindFestivalDay, CapacityGroupID: &groupID},
		{Kind: store.KindFestivalDay, CapacityGroupID: &groupID, SharedCapacity: new(int32)},
		{Kind: store.KindFestivalDay, CapacityGroupID: new(uuid.UUID), SharedCapacity: &shared},
	}
	for _, perf := range tests {
		if err := (&JetStream{}).PerformancePublished(context.Background(), perf); err == nil {
			t.Fatalf("PerformancePublished(%+v) succeeded, want corruption error", perf)
		}
	}
}

func TestPerformanceArchivedGroupedEnvelopeTargetsSharedPool(t *testing.T) {
	groupID := uuid.New()
	shared := int32(1000)
	archivedAt := time.Now().UTC()
	perf := store.Performance{
		ID: uuid.New(), EventID: uuid.New(), OrganizerID: uuid.New(), Kind: store.KindFestivalDay,
		CapacityGroupID: &groupID, SharedCapacity: &shared, ArchivedAt: &archivedAt,
	}
	body, err := performanceArchivedEnvelope(perf, archivedAt)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ID     string `json:"id"`
		Schema int    `json:"schema"`
		Data   struct {
			CapacityGroupID *uuid.UUID `json:"capacity_group_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != ArchivedEventID(perf) || got.Schema != 3 || got.Data.CapacityGroupID == nil || *got.Data.CapacityGroupID != groupID {
		t.Fatalf("archive envelope = %+v, want schema 3 shared pool %s", got, groupID)
	}

	plain := store.Performance{ID: uuid.New(), EventID: uuid.New(), OrganizerID: uuid.New(), Kind: store.KindPerformance, ArchivedAt: &archivedAt}
	body, err = performanceArchivedEnvelope(plain, archivedAt)
	if err != nil {
		t.Fatal(err)
	}
	got.Data.CapacityGroupID = nil
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != ArchivedEventID(plain) || got.Schema != 2 || got.Data.CapacityGroupID != nil {
		t.Fatalf("plain archive envelope = %+v, want schema 2 without shared pool", got)
	}
}
