package consumer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
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
	// SeatMapAdjacency projects the exact published version's geometry into per-seat
	// neighbours, for a rule-enabled schema-5 publication (ADR-041). Called at
	// provisioning time only — never from the claim path.
	SeatMapAdjacency(ctx context.Context, seatMapID uuid.UUID) ([]SeatAdjacency, error)
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

// ErrGeometryInvalid marks geometry that is DETERMINISTICALLY unusable — a draft map,
// the wrong version, duplicate identities, bad positions, trailing bytes. Retrying it
// changes nothing, so the caller must terminate rather than park it for ever.
//
// Its opposite, errResolveUnavailable, means catalog could not be reached. The first fix
// for this pair terminated both (a blip deleted the publication); the second wrapped
// both as transient (corrupt geometry retried for ever). Only the distinction is right.
var ErrGeometryInvalid = errors.New("seat-map geometry is invalid")

// maxGeometryBytes bounds the buffered geometry read. A seat map is at most a few
// thousand seats; anything larger is not a map this consumer should be projecting, and
// an unbounded read from a dependency is an availability risk of its own.
const maxGeometryBytes = 8 << 20

// geometrySeat is the boundary decode of one seat. Named rather than inlined so the
// validation below can copy and sort a row without restating the shape.
type geometrySeat struct {
	SeatIdentity string `json:"seat_identity"`
	Position     int32  `json:"position"`
}

// geometryRow is the boundary decode of one row. Named so the ranking pass below can sort
// rows by their declared position without restating the shape.
type geometryRow struct {
	ID       uuid.UUID      `json:"id"`
	Position int32          `json:"position"`
	Seats    []geometrySeat `json:"seats"`
}

// SeatAdjacency is one seat and its immediate neighbours in its row, derived from the
// published geometry's `position` order. A nil neighbour is a row end — a real answer,
// not missing data.
//
// RowKey and Position are the same geometry expressed the other way, kept for
// best-available selection (TKT-81). Until this ticket they were computed here and
// discarded three lines later: the derivation sorts a row by position and emits only the
// resulting edges. Neighbour edges answer "would this selection strand anything"; they
// cannot answer "find four seats together", because a linked list has no head to index
// and no order to sort by.
//
// RowKey is the row's catalog UUID, not its label. Labels are not unique across sections
// ("row A" exists in every one of them), so a label-keyed projection would merge rows that
// never touch and offer runs spanning a gangway.
type SeatAdjacency struct {
	SeatIdentity string
	Left         *string
	Right        *string
	RowKey       string
	Position     int32
	// RowRank orders the ROWS. Separate from RowKey because identity and order are two
	// different facts: RowKey must be the row's uuid (labels repeat across sections), and a
	// uuid sorts arbitrarily. Assigned by walking sections and rows in the order catalog
	// returns them, which the public geometry read guarantees is position order.
	RowRank int32
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
// SeatMapAdjacency fetches the EXACT published seat-map version and flattens it into
// per-seat left/right neighbours (ADR-041).
//
// It reads the version by id, never the family's current version: a pool is seated
// against one specific version and a published version is immutable (ADR-029), which is
// the whole reason a one-time projection is safe. Resolving the family would describe a
// map the pool is not seated against.
//
// Neighbours come from each row's `position` order, not from label arithmetic: labels
// are free text (`A`, `AA`, `12b`) and positions may have gaps, so arithmetic would
// invent adjacencies that do not exist. Sections and rows never connect.
//
// It fails closed on anything it cannot fully understand. A partial projection is worse
// than none: the rule would silently permit orphans exactly where the data was missing.
func (r *CatalogResolver) SeatMapAdjacency(ctx context.Context, seatMapID uuid.UUID) ([]SeatAdjacency, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		r.baseURL+"/public/seat-maps/"+seatMapID.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Only statuses that are catalog's SETTLED answer about this map are
		// deterministic. A blanket 4xx sweep is wrong: 408, 425 and 429 are explicitly
		// retryable, and a 401 can be transient during a credential or proxy change —
		// terminating on any of them would leave the performance with no inventory at
		// all (ai-review). Everything not on this list, including unknown statuses,
		// stays retryable: a needless retry costs a delay, a wrong terminate costs the
		// publication.
		switch resp.StatusCode {
		case http.StatusNotFound, http.StatusGone:
			return nil, fmt.Errorf("%w: seat map %s: status %d", ErrGeometryInvalid, seatMapID, resp.StatusCode)
		}
		return nil, fmt.Errorf("catalog seat-map geometry %s: status %d", seatMapID, resp.StatusCode)
	}
	var body struct {
		Map struct {
			ID     uuid.UUID `json:"id"`
			Status string    `json:"status"`
		} `json:"map"`
		Sections []struct {
			Position int32         `json:"position"`
			Rows     []geometryRow `json:"rows"`
		} `json:"sections"`
	}
	// The body is buffered BEFORE decoding so a transport failure mid-stream is
	// distinguishable from malformed content. Decoding straight from resp.Body makes an
	// interrupted read look like a syntax error, and a syntax error terminates the
	// publication permanently (ai-review). A connection that drops halfway is exactly
	// the case retrying exists for.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxGeometryBytes))
	if err != nil {
		return nil, fmt.Errorf("read catalog seat-map geometry %s: %w", seatMapID, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&body); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrGeometryInvalid, err)
	}
	// Exactly one value. A valid geometry object followed by garbage would otherwise
	// decode its prefix and commit that prefix as the authoritative projection —
	// permanently, since the same transaction consumes the event (ai-review).
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing bytes after the geometry document", ErrGeometryInvalid)
	}
	// The response must describe the version we asked for, and it must be published —
	// a draft's geometry is still mutable, so projecting it would bake in something
	// that can change underneath the pool.
	if body.Map.ID != seatMapID {
		return nil, fmt.Errorf("%w: catalog returned seat map %s, asked for %s", ErrGeometryInvalid, body.Map.ID, seatMapID)
	}
	if body.Map.Status != "published" {
		return nil, fmt.Errorf("%w: seat map %s is %q, not published", ErrGeometryInvalid, seatMapID, body.Map.Status)
	}

	var out []SeatAdjacency
	// Identities are compared AFTER trimming, and stored trimmed: "A" and " A" are the
	// same seat to any human and to catalog's own identity composition, so letting both
	// survive would put two adjacency rows on one seat and make the claim-path lookup
	// depend on which one it happened to match (ai-review).
	seen := map[string]struct{}{}
	// Rank rows by their DECLARED (section position, row position), not by the order they
	// happen to arrive in.
	//
	// The response is documented as position-ordered and today it is, but the ordering is the
	// producer's SQL rather than a contract this consumer can check, and it carries the
	// positions explicitly. Reading them is both cheaper to trust and self-describing: a
	// projection built from arrival order is silently wrong the day the producer adds a JOIN,
	// and nothing downstream could tell -- inventory holds no geometry to compare against.
	//
	// Sorting is stable on the pair, so two sections at the same position (which catalog's
	// own uniqueness rules forbid, but which this consumer must not assume) fall back to
	// arrival order rather than becoming non-deterministic.
	type rowRef struct {
		sectionPos, rowPos int32
		seq                int
		row                geometryRow
	}
	refs := make([]rowRef, 0, 16)
	seq := 0
	for _, section := range body.Sections {
		for _, row := range section.Rows {
			refs = append(refs, rowRef{sectionPos: section.Position, rowPos: row.Position, seq: seq, row: row})
			seq++
		}
	}
	sort.SliceStable(refs, func(i, j int) bool {
		if refs[i].sectionPos != refs[j].sectionPos {
			return refs[i].sectionPos < refs[j].sectionPos
		}
		if refs[i].rowPos != refs[j].rowPos {
			return refs[i].rowPos < refs[j].rowPos
		}
		return refs[i].seq < refs[j].seq
	})
	rank := int32(0)
	{
		for _, ref := range refs {
			row := ref.row
			rank++
			seats := append([]geometrySeat(nil), row.Seats...)
			// Positions must be present, positive and unique within the row. A missing
			// position decodes as zero and a duplicate makes sort order arbitrary — either
			// one invents neighbours that do not exist, and an invented neighbour is worse
			// than no rule at all because it refuses legal selections.
			positions := map[int32]struct{}{}
			for _, seat := range seats {
				if seat.Position <= 0 {
					return nil, fmt.Errorf("%w: seat map %s has a seat with position %d", ErrGeometryInvalid, seatMapID, seat.Position)
				}
				if _, dup := positions[seat.Position]; dup {
					return nil, fmt.Errorf("%w: seat map %s repeats position %d within a row", ErrGeometryInvalid, seatMapID, seat.Position)
				}
				positions[seat.Position] = struct{}{}
			}
			sort.Slice(seats, func(i, j int) bool { return seats[i].Position < seats[j].Position })
			identities := make([]string, 0, len(seats))
			for _, seat := range seats {
				id := strings.TrimSpace(seat.SeatIdentity)
				if id == "" {
					return nil, fmt.Errorf("%w: seat map %s has a seat with no identity", ErrGeometryInvalid, seatMapID)
				}
				if _, dup := seen[id]; dup {
					return nil, fmt.Errorf("%w: seat map %s repeats identity %q", ErrGeometryInvalid, seatMapID, id)
				}
				seen[id] = struct{}{}
				identities = append(identities, id)
			}
			// A row with no id cannot be keyed, and keying it on anything else (its
			// label, its index) would merge rows across sections or renumber them on the
			// next publication. Deterministically unusable, so terminate rather than
			// project a geometry whose rows cannot be told apart (TKT-81).
			if row.ID == uuid.Nil {
				return nil, fmt.Errorf("%w: seat map %s has a row with no id", ErrGeometryInvalid, seatMapID)
			}
			rowKey := row.ID.String()
			for i, id := range identities {
				// Position is the seat's rank WITHIN THIS ROW after the sort above, not
				// catalog's raw position value. Catalog positions are unique and ascending
				// but need not be contiguous — a row authored 10, 20, 30 is legal — and the
				// selection query groups runs by consecutive positions. Re-basing to 1..n
				// here is what makes "adjacent in the row" and "consecutive positions" the
				// same statement; carrying the raw value through would make three
				// neighbouring seats read as three separate one-seat runs.
				adj := SeatAdjacency{SeatIdentity: id, RowKey: rowKey, RowRank: rank, Position: int32(i + 1)}
				if i > 0 {
					left := identities[i-1]
					adj.Left = &left
				}
				if i < len(identities)-1 {
					right := identities[i+1]
					adj.Right = &right
				}
				out = append(out, adj)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: seat map %s has no seats", ErrGeometryInvalid, seatMapID)
	}
	return out, nil
}

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
