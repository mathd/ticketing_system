package consumer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
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

// SeatPin is one catalog pin row as the reconciliation read returns it (TKT-112).
// SeatMapID is a representative version of the pin's family — enough to reach the pin
// through catalog's family-locked unpin path (ADR-029).
type SeatPin struct {
	ID           uuid.UUID
	OrganizerID  uuid.UUID
	SeatMapID    uuid.UUID
	SeatIdentity string
	PinnedBy     string
}

// maxSeatPinPageBytes bounds a pin page's response body so a runaway body cannot be read into
// memory. It is NOT derived from a producer contract, because there isn't one: `seat_identity`
// and `pinned_by` are unbounded `text` (migration 0011), and catalog bounds only the whole pin
// REQUEST at 1 MiB — so a single identity can approach that on its own. 4 MiB therefore leaves
// room for one worst-case row plus JSON overhead, and a page that still overflows is reported as
// ErrSeatPinPageTooLarge so the caller can ask for fewer rows instead of giving up.
const maxSeatPinPageBytes = 4 << 20

// ErrSeatPinPageTooLarge reports that the page did not fit the byte cap. It is a RETRYABLE
// signal, not a dead end: the caller re-asks for fewer rows. Aborting instead would wedge the
// drain permanently, since the keyset cursor only advances past rows that were read — one
// oversized page would make every later page unreachable (ai-review pass 3).
//
// KNOWN LIMITATION, accepted at the review churn cap (ai-review pass 4), tracked by TKT-143.
// A SINGLE row can still exceed the cap, and then the drain does stop at that cursor. The 4 MiB
// figure above is not a proof: catalog serializes with json.NewEncoder, which HTML-escapes
// '<', '>' and '&' into six-byte sequences, so a ~1 MiB field of those lists as ~6 MiB (control
// characters expand the same way, which is why disabling HTML escaping would narrow this rather
// than close it). Closing it needs a producer-side length bound and a cap derived from it — a
// public-contract change with a migration story for existing data, which is TKT-143's decision,
// not this command's.
//
// Why it is a limitation and not a defect worth blocking on: nothing this system writes can
// trigger it. `pinned_by` is `"hold:" + uuid` (fixed length) and seat identities are composed
// server-side from labels. It takes a caller with the internal token deliberately writing a
// megabyte-scale identity. If that ever happens the failure is loud and names the cursor, no pin
// is wrongly deleted, and the pre-existing manual batch-unpin remedy still works — the tool
// stops early rather than doing damage.
var ErrSeatPinPageTooLarge = errors.New("catalog seat pin page exceeds the response cap")

// ListSeatPins reads one bounded, keyset-ordered page of catalog's pin table (TKT-112).
//
// It fails closed on anything it cannot fully trust — a short field, a nil id, a duplicate, a
// row at or before the cursor, more rows than were asked for. This is stricter than a decoder
// needs to be because the caller uses the result to decide what to DELETE: a page accepted on
// partial data is a reclaim decision made on partial data. Rejecting a row at or before the
// cursor also makes a server that ignores `after` a loud failure rather than an infinite drain.
func (r *CatalogResolver) ListSeatPins(ctx context.Context, after uuid.UUID, limit int) ([]SeatPin, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("seat pin page limit must be positive, got %d", limit)
	}
	url := r.baseURL + "/internal/seat-map-pins?"
	if after != uuid.Nil {
		url += "after=" + after.String() + "&"
	}
	url += "limit=" + strconv.Itoa(limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Internal-Token", r.credential)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog seat pin list: status %d", resp.StatusCode)
	}
	var body struct {
		Pins *[]struct {
			ID           uuid.UUID `json:"id"`
			OrganizerID  uuid.UUID `json:"organizer_id"`
			SeatMapID    uuid.UUID `json:"seat_map_id"`
			SeatIdentity string    `json:"seat_identity"`
			PinnedBy     string    `json:"pinned_by"`
		} `json:"pins"`
	}
	// Read the whole page into a bounded buffer FIRST, then decode from the buffer, so the
	// decoder's EOF is the real end of the response.
	//
	// Two requirements that fight each other if you stream (ai-review pass 1 F2, then pass 2):
	// the body must be size-bounded, and it must contain EXACTLY one JSON value — json.Decode
	// alone accepts a valid first value followed by trailing garbage or a truncated second one,
	// which at this boundary means a spliced or half-written 200 can drive a DELETE. Enforcing
	// the bound with io.LimitReader defeats the second check: the reader manufactures an EOF at
	// the limit, so a page padded through it and then followed by garbage reports "exactly one
	// value" and is accepted. Reading max+1 bytes and rejecting the overflow keeps both.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSeatPinPageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read catalog seat pin list: %w", err)
	}
	if len(raw) > maxSeatPinPageBytes {
		return nil, fmt.Errorf("catalog seat pin list of %d rows: %w", limit, ErrSeatPinPageTooLarge)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&body); err != nil {
		return nil, fmt.Errorf("decode catalog seat pin list: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("catalog seat pin list: response is not exactly one JSON value (%v)", err)
	}
	// A pointer, so an absent "pins" key is distinguishable from an empty page — the empty
	// page is the drain's termination signal and must never be inferred from a typo.
	if body.Pins == nil {
		return nil, errors.New("catalog seat pin list: response has no pins")
	}
	if len(*body.Pins) > limit {
		return nil, fmt.Errorf("catalog seat pin list: %d rows for a limit of %d", len(*body.Pins), limit)
	}
	out := make([]SeatPin, 0, len(*body.Pins))
	prev := after
	for i, p := range *body.Pins {
		if p.ID == uuid.Nil || p.OrganizerID == uuid.Nil || p.SeatMapID == uuid.Nil ||
			strings.TrimSpace(p.SeatIdentity) == "" || strings.TrimSpace(p.PinnedBy) == "" {
			return nil, fmt.Errorf("catalog seat pin list: row %d is incomplete", i)
		}
		// Strictly increasing by primary key, and strictly past the cursor. Compares the
		// raw 16 bytes, which is exactly how Postgres orders uuid.
		if prev != uuid.Nil && bytes.Compare(p.ID[:], prev[:]) <= 0 {
			return nil, fmt.Errorf("catalog seat pin list: row %d (%s) is not past %s", i, p.ID, prev)
		}
		prev = p.ID
		out = append(out, SeatPin{ID: p.ID, OrganizerID: p.OrganizerID, SeatMapID: p.SeatMapID,
			SeatIdentity: p.SeatIdentity, PinnedBy: p.PinnedBy})
	}
	return out, nil
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
