package consumer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// ErrSeatPinRejected is catalog's deterministic refusal of a seat pin: an identity is
// absent from the current published version (a map edit dropped it, or the client named
// a seat that never existed). It is NOT transient — the inventory hold must be released,
// not retried (TKT-80 hold-then-pin compensation).
var ErrSeatPinRejected = errors.New("catalog rejected seat pin")

// ErrPerformanceNotFound is the resolver's 404: the slot is not published right now.
// Callers must not treat it as transient — for closure events it means the slot has
// been archived since the event was emitted (TKT-75), and retrying never resolves it.
var ErrPerformanceNotFound = errors.New("performance not published")

// ErrPoolStateNotFound is the offer-state 404: catalog knows nothing under this
// id — neither a performance in any lifecycle nor a festival. Reconciliation
// must treat it as non-positive and touch nothing (TKT-90).
var ErrPoolStateNotFound = errors.New("pool unknown to catalog")

// PerformanceResolver supplies the capacity omitted from the schema-1
// publication event (schema-2 events remain self-contained) and the
// authoritative per-pool offer state the reconciliation pass reads.
type PerformanceResolver interface {
	PublishedPerformance(ctx context.Context, id uuid.UUID) (PublishedPerformance, error)
	PoolOfferState(ctx context.Context, id uuid.UUID) (PoolOfferState, error)
}

// PoolOfferState is catalog's answer for a pool id, whatever the id turns out
// to be. Lifecycle/closure fields are meaningful only for kind "performance".
type PoolOfferState struct {
	Kind           string // "performance" | "festival"
	Lifecycle      string // draft | published | archived
	ClosureStatus  string // open | closed
	ClosureVersion int32
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
	if resp.StatusCode == http.StatusNotFound {
		return PublishedPerformance{}, fmt.Errorf("catalog performance lookup %s: %w", id, ErrPerformanceNotFound)
	}
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

// PinSeats pins a seat-hold's whole set against catalog (TKT-80, hold-then-pin). The
// batch is all-or-nothing on catalog's side; a 4xx means a deterministic rejection
// (ErrSeatPinRejected) the caller compensates by releasing the hold, a 5xx/transport
// error is transient. Idempotent, so a replay-re-pin is safe.
func (r *CatalogResolver) PinSeats(ctx context.Context, org, seatMapID uuid.UUID, seats []string, pinnedBy string) error {
	return r.batchPin(ctx, "pins", org, seatMapID, seats, pinnedBy)
}

// UnpinSeats clears a seat-hold's pins (release/expiry/compensation). Idempotent.
func (r *CatalogResolver) UnpinSeats(ctx context.Context, org, seatMapID uuid.UUID, seats []string, pinnedBy string) error {
	return r.batchPin(ctx, "unpins", org, seatMapID, seats, pinnedBy)
}

func (r *CatalogResolver) batchPin(ctx context.Context, action string, org, seatMapID uuid.UUID, seats []string, pinnedBy string) error {
	if len(seats) == 0 {
		return nil
	}
	payload, err := json.Marshal(map[string]any{"organizer_id": org, "seat_identities": seats, "pinned_by": pinnedBy})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/internal/seat-maps/"+seatMapID.String()+"/"+action, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("X-Internal-Token", r.credential)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	// Only 409 is a DETERMINISTIC seat rejection (the identity is absent from the current
	// published version — catalog's ErrSeatIdentityNotFound). Everything else — 401 (token
	// rotation), 404 (map lookup), 429 (throttle), 5xx, transport — is transient: the caller
	// must NOT release a valid hold over it; retry or let the TTL reclaim it.
	if resp.StatusCode == http.StatusConflict {
		return fmt.Errorf("catalog seat %s %s: %w", action, seatMapID, ErrSeatPinRejected)
	}
	return fmt.Errorf("catalog seat %s: status %d", action, resp.StatusCode)
}

// PoolOfferState fetches the reconciliation read (TKT-90). The response is a
// trust boundary: shape is validated here so reconcile only ever sees a
// well-formed positive assertion or an error.
func (r *CatalogResolver) PoolOfferState(ctx context.Context, id uuid.UUID) (PoolOfferState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+"/internal/pools/"+id.String()+"/offer-state", nil)
	if err != nil {
		return PoolOfferState{}, err
	}
	req.Header.Set("X-Internal-Token", r.credential)
	resp, err := r.client.Do(req)
	if err != nil {
		return PoolOfferState{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return PoolOfferState{}, fmt.Errorf("pool offer-state lookup %s: %w", id, ErrPoolStateNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return PoolOfferState{}, fmt.Errorf("pool offer-state lookup: status %d", resp.StatusCode)
	}
	var body struct {
		Kind           string `json:"kind"`
		Lifecycle      string `json:"lifecycle"`
		ClosureStatus  string `json:"closure_status"`
		ClosureVersion int32  `json:"closure_version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return PoolOfferState{}, fmt.Errorf("decode pool offer-state lookup: %w", err)
	}
	switch body.Kind {
	case "festival":
		return PoolOfferState{Kind: "festival"}, nil
	case "performance":
		if body.Lifecycle != "draft" && body.Lifecycle != "published" && body.Lifecycle != "archived" {
			return PoolOfferState{}, fmt.Errorf("invalid pool offer-state lifecycle %q", body.Lifecycle)
		}
		if body.ClosureStatus != "open" && body.ClosureStatus != "closed" {
			return PoolOfferState{}, fmt.Errorf("invalid pool offer-state closure status %q", body.ClosureStatus)
		}
		if body.ClosureVersion < 0 {
			return PoolOfferState{}, fmt.Errorf("invalid pool offer-state closure version %d", body.ClosureVersion)
		}
		return PoolOfferState{Kind: "performance", Lifecycle: body.Lifecycle, ClosureStatus: body.ClosureStatus, ClosureVersion: body.ClosureVersion}, nil
	default:
		return PoolOfferState{}, fmt.Errorf("invalid pool offer-state kind %q", body.Kind)
	}
}
