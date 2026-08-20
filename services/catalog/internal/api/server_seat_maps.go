package api

// Seat-map authoring handlers: draft-only writes at the trusted root, geometry
// and summary reads under /public at the ADR-004 hours tier (ADR-029).

import (
	"encoding/json"
	"net/http"
	"ticketing/services/catalog/internal/store"

	"github.com/google/uuid"
)

// --- Seat-map authoring (US-019 / TKT-102). Draft-only writes at the trusted
// root; geometry + summary reads under /public at the ADR-004 hours tier. ---

func seatMapPayload(m store.SeatMap) SeatMap {
	return SeatMap{
		Id: m.ID, OrganizerId: m.OrganizerID, VenueId: m.VenueID, Name: m.Name,
		Version: m.Version, Status: SeatMapStatus(m.Status), PublishedAt: m.PublishedAt,
		OrphanPreventionEnabled: m.OrphanPreventionEnabled,
		CreatedAt:               m.CreatedAt,
	}
}

// PublishSeatMap is idempotent on the resource and at-least-once on the event
// (TKT-103), the same emit-after-commit owed-marker contract as
// PublishPerformance: the seat_map.published event is emitted only while
// unacknowledged (event_emitted_at null), so a failed emission is retried by
// re-POSTing publish; consumers de-duplicate on the deterministic id.
func (s *Server) PublishSeatMap(w http.ResponseWriter, r *http.Request, seatMapId SeatMapId) {
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	m, needsEmit, err := s.store.PublishSeatMap(r.Context(), organizerID, seatMapId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if needsEmit {
		if err := s.pub.SeatMapPublished(r.Context(), m); err != nil {
			s.log.ErrorContext(r.Context(), "seat-map domain event emission failed; re-POST publish to retry",
				"seat_map_id", m.ID, "err", err)
			writeJSON(w, http.StatusInternalServerError,
				Error{Error: "seat map is published but the domain event was not emitted; retry publish"})
			return
		}
		if err := s.store.MarkSeatMapEventEmitted(r.Context(), m.ID); err != nil {
			// Ack'd but unmarked: a publish retry may re-emit — the at-least-once
			// contract, consumers de-duplicate on the deterministic id.
			s.log.ErrorContext(r.Context(), "seat-map event emitted but not marked", "seat_map_id", m.ID, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, seatMapPayload(m))
}

// EditSeatMap surfaces the TKT-104 safe-edit contract (ADR-029) over HTTP
// (TKT-105). It is a thin wrapper: the store re-resolves the family's current
// published version under a family advisory lock, validates that every pinned
// seat identity survives, and INSERTs a new published version — the HTTP layer
// re-implements none of that. An orphaning edit surfaces as
// ErrSeatMapEditOrphansPinned -> 409 via writeStoreError. The new version owes
// its own seat_map.published event, so this mirrors PublishSeatMap's
// emit-after-commit owed-marker discipline (a failed emission -> 500; recovery
// is re-POSTing publish of the NEW version id, NOT retrying the edit, which
// would mint yet another version).
//
// The 500 is declared in the spec (TKT-108), so the recovery hint reaches the
// client through the ADR-028 response validator. The new version is intact and
// event-owed; operators recover via the owed-event retry the same way as for
// publish.
func (s *Server) EditSeatMap(w http.ResponseWriter, r *http.Request, seatMapId SeatMapId) {
	var in SeatMapEdit
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	m, needsEmit, err := s.store.EditSeatMap(r.Context(), editInput(organizerID, seatMapId, in))
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if needsEmit {
		if err := s.pub.SeatMapPublished(r.Context(), m); err != nil {
			s.log.ErrorContext(r.Context(), "edited seat-map event emission failed; re-POST publish of the new version to retry",
				"seat_map_id", m.ID, "version", m.Version, "err", err)
			writeJSON(w, http.StatusInternalServerError,
				Error{Error: "the new version is published but its domain event was not emitted; retry by publishing the new version"})
			return
		}
		if err := s.store.MarkSeatMapEventEmitted(r.Context(), m.ID); err != nil {
			s.log.ErrorContext(r.Context(), "edited seat-map event emitted but not marked", "seat_map_id", m.ID, "err", err)
		}
	}
	writeJSON(w, http.StatusCreated, seatMapPayload(m))
}

// editInput maps the wire SeatMapEdit (any version id + full geometry tree) to
// the store's EditSeatMapInput. Seat identity is composed server-side from the
// labels, so no id plumbing is needed.
//
// The organizer is a PARAMETER, taken from the verified assertion by the caller
// (TKT-245). It used to come off the wire type, which is what let the seat-map
// editor round-trip a tenant id through a hidden form field and hand it back.
func editInput(organizerID uuid.UUID, seatMapID SeatMapId, in SeatMapEdit) store.EditSeatMapInput {
	sections := make([]store.EditSectionInput, 0, len(in.Sections))
	for _, sec := range in.Sections {
		rows := make([]store.EditRowInput, 0, len(sec.Rows))
		for _, row := range sec.Rows {
			seats := make([]store.EditSeatInput, 0, len(row.Seats))
			for _, seat := range row.Seats {
				seats = append(seats, store.EditSeatInput{Label: seat.Label, Position: seat.Position})
			}
			rows = append(rows, store.EditRowInput{Label: row.Label, Position: row.Position, Seats: seats})
		}
		sections = append(sections, store.EditSectionInput{Name: sec.Name, Position: sec.Position, Rows: rows})
	}
	// nil INHERITS the edited version's setting; a value applies to the new version
	// only. The pointer survives the mapping deliberately — collapsing it to a bool
	// here would turn "the staffer said nothing" into "the staffer said off"
	// (ADR-041).
	return store.EditSeatMapInput{
		OrganizerID: organizerID, SeatMapID: seatMapID, Sections: sections,
		OrphanPreventionEnabled: in.OrphanPreventionEnabled,
	}
}

// ListSeatMapVersions is the TKT-105 version-history read (COS-3): the family's
// versions newest-first, current_version = highest published. Status-driven cache
// tier (cacheControlForSeatMaps, TKT-107), catalog-owned.
func (s *Server) ListSeatMapVersions(w http.ResponseWriter, r *http.Request, seatMapId SeatMapId) {
	versions, err := s.store.ListSeatMapVersions(r.Context(), seatMapId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	out := SeatMapVersionHistory{Versions: make([]SeatMap, 0, len(versions))}
	for _, v := range versions {
		out.Versions = append(out.Versions, seatMapPayload(v))
		// versions are newest-first, so the first published row is the current one.
		if out.CurrentVersion == nil && v.Status == "published" {
			cv := v.Version
			out.CurrentVersion = &cv
		}
	}
	w.Header().Set("Cache-Control", cacheControlForSeatMaps(versions...))
	writeJSON(w, http.StatusOK, out)
}

// UpdateVenueGaCapacity sets a venue's GA capacity (TKT-105 COS-5). Write ->
// no-store.
func (s *Server) UpdateVenueGaCapacity(w http.ResponseWriter, r *http.Request, venueId VenueId) {
	var in VenueGaCapacityUpdate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	v, err := s.store.UpdateVenueGACapacity(r.Context(), store.VenueGACapacityInput{
		OrganizerID: organizerID, VenueID: venueId, GACapacity: in.GaCapacity,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, Venue{
		Id: v.ID, OrganizerId: v.OrganizerID, Name: v.Name,
		GaCapacity: v.GACapacity, CreatedAt: v.CreatedAt,
	})
}

func (s *Server) CreateSeatMap(w http.ResponseWriter, r *http.Request, venueId VenueId) {
	var in SeatMapCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	m, err := s.store.CreateSeatMap(r.Context(), store.SeatMapInput{
		OrganizerID: organizerID, VenueID: venueId, Name: in.Name,
		// Absent means false: a caller that has never heard of the rule creates
		// exactly the map it created before (ADR-041).
		OrphanPreventionEnabled: in.OrphanPreventionEnabled != nil && *in.OrphanPreventionEnabled,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, seatMapPayload(m))
}

func (s *Server) AddSeatMapSection(w http.ResponseWriter, r *http.Request, seatMapId SeatMapId) {
	var in SeatMapSectionCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	sec, err := s.store.AddSeatMapSection(r.Context(), store.SeatMapSectionInput{
		OrganizerID: organizerID, SeatMapID: seatMapId, Name: in.Name, Position: in.Position,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, SeatSection{Id: sec.ID, Name: sec.Name, Position: sec.Position})
}

func (s *Server) AddSeatMapRow(w http.ResponseWriter, r *http.Request, seatMapId SeatMapId) {
	var in SeatMapRowCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	row, err := s.store.AddSeatMapRow(r.Context(), store.SeatMapRowInput{
		OrganizerID: organizerID, SeatMapID: seatMapId, SectionID: in.SectionId,
		Label: in.Label, Position: in.Position,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, SeatRow{Id: row.ID, Label: row.Label, Position: row.Position})
}

func (s *Server) AddSeatMapSeat(w http.ResponseWriter, r *http.Request, seatMapId SeatMapId) {
	var in SeatMapSeatCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, Error{Error: "invalid body"})
		return
	}
	organizerID, ok := s.organizerFor(w, r)
	if !ok {
		return
	}
	seat, err := s.store.AddSeatMapSeat(r.Context(), store.SeatMapSeatInput{
		OrganizerID: organizerID, SeatMapID: seatMapId, RowID: in.RowId,
		Label: in.Label, Position: in.Position,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, Seat{
		Id: seat.ID, SeatIdentity: seat.SeatIdentity, Label: seat.Label, Position: seat.Position,
	})
}

func (s *Server) GetPublicSeatMapGeometry(w http.ResponseWriter, r *http.Request, seatMapId SeatMapId) {
	g, err := s.store.GetSeatMapGeometry(r.Context(), seatMapId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	out := SeatMapGeometry{Map: seatMapPayload(g.Map), Sections: make([]SeatSection, 0, len(g.Sections))}
	for _, sec := range g.Sections {
		rows := make([]SeatRow, 0, len(sec.Rows))
		for _, row := range sec.Rows {
			seats := make([]Seat, 0, len(row.Seats))
			for _, st := range row.Seats {
				seats = append(seats, Seat{
					Id: st.ID, SeatIdentity: st.SeatIdentity, Label: st.Label, Position: st.Position,
				})
			}
			outRow := SeatRow{Id: row.ID, Label: row.Label, Position: row.Position, Seats: &seats}
			rows = append(rows, outRow)
		}
		out.Sections = append(out.Sections, SeatSection{
			Id: sec.ID, Name: sec.Name, Position: sec.Position, Rows: &rows,
		})
	}
	w.Header().Set("Cache-Control", cacheControlForSeatMaps(g.Map))
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) ListVenueSeatMaps(w http.ResponseWriter, r *http.Request, venueId VenueId) {
	maps, err := s.store.ListVenueSeatMaps(r.Context(), venueId)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	out := SeatMapList{SeatMaps: make([]SeatMap, 0, len(maps))}
	for _, m := range maps {
		out.SeatMaps = append(out.SeatMaps, seatMapPayload(m))
	}
	w.Header().Set("Cache-Control", cacheControlForSeatMaps(maps...))
	writeJSON(w, http.StatusOK, out)
}
