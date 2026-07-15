package consumer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// PerformanceResolver supplies the capacity omitted from the schema-1
// publication event. Schema-2 events remain self-contained.
type PerformanceResolver interface {
	PublishedPerformance(ctx context.Context, id uuid.UUID) (PublishedPerformance, error)
}

type PublishedPerformance struct {
	OrganizerID     uuid.UUID
	Capacity        int32
	CapacityGroupID *uuid.UUID
	SharedCapacity  *int32
}

type CatalogResolver struct {
	baseURL    string
	credential string
	client     *http.Client
}

func NewCatalogResolver(baseURL, credential string, client *http.Client) *CatalogResolver {
	return &CatalogResolver{baseURL: strings.TrimRight(baseURL, "/"), credential: credential, client: client}
}

func (r *CatalogResolver) PublishedPerformance(ctx context.Context, id uuid.UUID) (PublishedPerformance, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+"/internal/performances/"+id.String(), nil)
	if err != nil {
		return PublishedPerformance{}, err
	}
	req.Header.Set("X-Internal-Token", r.credential)
	resp, err := r.client.Do(req)
	if err != nil {
		return PublishedPerformance{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return PublishedPerformance{}, fmt.Errorf("catalog performance lookup: status %d", resp.StatusCode)
	}
	var body struct {
		OrganizerID     uuid.UUID  `json:"organizer_id"`
		Capacity        int32      `json:"capacity"`
		CapacityGroupID *uuid.UUID `json:"capacity_group_id,omitempty"`
		SharedCapacity  *int32     `json:"shared_capacity,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return PublishedPerformance{}, fmt.Errorf("decode catalog performance lookup: %w", err)
	}
	if body.OrganizerID == uuid.Nil || body.Capacity <= 0 {
		return PublishedPerformance{}, fmt.Errorf("invalid catalog performance lookup")
	}
	if (body.CapacityGroupID == nil) != (body.SharedCapacity == nil) || body.SharedCapacity != nil && *body.SharedCapacity <= 0 {
		return PublishedPerformance{}, fmt.Errorf("invalid catalog festival capacity lookup")
	}
	return PublishedPerformance{OrganizerID: body.OrganizerID, Capacity: body.Capacity, CapacityGroupID: body.CapacityGroupID, SharedCapacity: body.SharedCapacity}, nil
}
