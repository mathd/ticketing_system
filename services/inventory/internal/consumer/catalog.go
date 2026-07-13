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
	PublishedPerformance(ctx context.Context, id uuid.UUID) (uuid.UUID, int32, error)
}

type CatalogResolver struct {
	baseURL    string
	credential string
	client     *http.Client
}

func NewCatalogResolver(baseURL, credential string, client *http.Client) *CatalogResolver {
	return &CatalogResolver{baseURL: strings.TrimRight(baseURL, "/"), credential: credential, client: client}
}

func (r *CatalogResolver) PublishedPerformance(ctx context.Context, id uuid.UUID) (uuid.UUID, int32, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+"/internal/performances/"+id.String(), nil)
	if err != nil {
		return uuid.Nil, 0, err
	}
	req.Header.Set("X-Internal-Token", r.credential)
	resp, err := r.client.Do(req)
	if err != nil {
		return uuid.Nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return uuid.Nil, 0, fmt.Errorf("catalog performance lookup: status %d", resp.StatusCode)
	}
	var body struct {
		OrganizerID uuid.UUID `json:"organizer_id"`
		Capacity    int32     `json:"capacity"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return uuid.Nil, 0, fmt.Errorf("decode catalog performance lookup: %w", err)
	}
	if body.OrganizerID == uuid.Nil || body.Capacity <= 0 {
		return uuid.Nil, 0, fmt.Errorf("invalid catalog performance lookup")
	}
	return body.OrganizerID, body.Capacity, nil
}
