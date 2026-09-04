package store

// Seat-map authoring, versioning and pinning. An edit produces a new published
// version; a pinned seat identity is preserved across versions and an orphaning
// edit is hard-rejected, under a family-scoped advisory lock (ADR-029).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// --- Seat-map authoring (US-019 / TKT-102), draft-only. ---
//
// Each write is one INSERT ... SELECT that resolves the parent chain and
// requires the owning map to be draft, so cross-map/cross-organizer parentage
// and writes to a non-draft map are unrepresentable through the store: a
// no-match yields ErrNotFound; any UNIQUE collision yields ErrSeatMapConflict.

func (p *Postgres) CreateSeatMap(ctx context.Context, in SeatMapInput) (SeatMap, error) {
	m := SeatMap{OrganizerID: in.OrganizerID, VenueID: in.VenueID, Name: in.Name}
	err := p.db.QueryRowContext(ctx,
		`INSERT INTO seat_maps (organizer_id, venue_id, name, orphan_prevention_enabled)
		 SELECT $1, v.id, $3, $4 FROM venues v WHERE v.id = $2 AND v.organizer_id = $1
		 RETURNING id, version, status, created_at, orphan_prevention_enabled`,
		in.OrganizerID, in.VenueID, in.Name, in.OrphanPreventionEnabled).
		Scan(&m.ID, &m.Version, &m.Status, &m.CreatedAt, &m.OrphanPreventionEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return SeatMap{}, fmt.Errorf("venue: %w", ErrNotFound)
	}
	if err != nil {
		return SeatMap{}, fmt.Errorf("create seat map: %w", err)
	}
	return m, nil
}

// PublishSeatMap flips a seat map draft->published (TKT-103, COS-1). The
// transition is MONOTONIC and therefore lock-free (ADR-018 rule 1): a single
// atomic conditional UPDATE flips draft->published exactly once, so no row lock
// is needed — a concurrent transition cannot invalidate the publication's
// identity, exactly as PublishPerformance. needsEmit is true while the
// seat_map.published domain event is still owed (event_emitted_at is null);
// the caller emits, then marks. Publishing an already-published map is a
// resource no-op that still reports whether the event is owed.
// PublishSeatMap is deliberately NOT the ADR-029 family-lock shape (TKT-251).
// That lock exists for EditSeatMap/PinSeat, which resolve "the family's current
// published version" — an edit INSERTs a new version row, which no row lock on
// the old one would conflict with. Publish flips ONE specific draft row by id
// and resolves no current version, so it takes the organizer predicate on the
// conditional UPDATE and on the canonical re-read below, and nothing else.
// Adding the family lock here would change which row gets published.
func (p *Postgres) PublishSeatMap(ctx context.Context, organizerID, id uuid.UUID) (SeatMap, bool, error) {
	if _, err := p.db.ExecContext(ctx,
		`UPDATE seat_maps SET status = 'published', published_at = now()
		 WHERE id = $1 AND organizer_id = $2 AND status = 'draft'`, id, organizerID); err != nil {
		return SeatMap{}, false, fmt.Errorf("publish seat map: %w", err)
	}
	var m SeatMap
	var publishedAt sql.NullTime
	var emittedAt sql.NullTime
	err := p.db.QueryRowContext(ctx,
		`SELECT id, organizer_id, venue_id, name, version, status, published_at, event_emitted_at, created_at, orphan_prevention_enabled
		 FROM seat_maps WHERE id = $1 AND organizer_id = $2`, id, organizerID).
		Scan(&m.ID, &m.OrganizerID, &m.VenueID, &m.Name, &m.Version, &m.Status, &publishedAt, &emittedAt, &m.CreatedAt, &m.OrphanPreventionEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return SeatMap{}, false, fmt.Errorf("seat map: %w", ErrNotFound)
	}
	if err != nil {
		return SeatMap{}, false, fmt.Errorf("read seat map: %w", err)
	}
	if publishedAt.Valid {
		m.PublishedAt = &publishedAt.Time
	}
	if m.Status != "published" {
		// A map that could not flip and is not already published cannot be a
		// draft that just published, so it is an illegal transition target
		// (e.g. archived). Draft that already flipped falls through as published.
		return SeatMap{}, false, ErrIllegalTransition
	}
	return m, !emittedAt.Valid, nil
}

// MarkSeatMapEventEmitted records that the seat_map.published event for this map
// has been acknowledged by the stream (TKT-103). Mirrors
// MarkPerformanceEventEmitted: at-least-once, so a retry before this lands
// re-emits under the same deterministic id.
func (p *Postgres) MarkSeatMapEventEmitted(ctx context.Context, id uuid.UUID) error {
	if _, err := p.db.ExecContext(ctx,
		`UPDATE seat_maps SET event_emitted_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("mark seat map emitted: %w", err)
	}
	return nil
}

// lockCurrentPublishedVersion serializes all edit/pin/unpin work on a seat-map
// FAMILY, then resolves the family's current published version. EditSeatMap and
// PinSeat both call it, so a concurrent edit and pin serialize (TKT-104): the
// loser sees the winner's committed result. ErrNotFound if no version of the
// family is published (or the id/organizer does not match).
//
// It must NOT serialize by locking the current-version *row* with FOR UPDATE.
// EditSeatMap makes a new version by INSERTing a new row, which never conflicts
// with a FOR UPDATE lock on the old row — so a PinSeat blocked on the old row
// would, once the edit commits, still hold the STALE old row (PostgreSQL rechecks
// the locked row, it does not re-run the ORDER BY … LIMIT) and validate against
// the old geometry, landing an orphaned pin (ai-review F1/F2; reproduced). The
// same stale row lets two concurrent edits both derive version+1 and collide
// (F3). We therefore serialize on the FAMILY IDENTITY via a transaction-scoped
// advisory lock keyed on the family UUID — a lock that is immune to which row is
// current — and only then read the current published version, freshly, seeing
// every committed row. A UNIQUE(map_family_id, version) constraint (migration
// 0011) is the belt-and-suspenders backstop against a version collision.
func lockCurrentPublishedVersion(ctx context.Context, tx *sql.Tx, seatMapID, organizerID uuid.UUID) (id uuid.UUID, version int32, family uuid.UUID, err error) {
	// Resolve the family from any version id (organizer-scoped), then take the
	// family advisory lock BEFORE reading the current version. hashtextextended
	// maps the family UUID to the bigint pg_advisory_xact_lock expects; the lock
	// releases automatically at commit/rollback.
	err = tx.QueryRowContext(ctx,
		`SELECT map_family_id FROM seat_maps WHERE id = $1 AND organizer_id = $2`,
		seatMapID, organizerID).Scan(&family)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, 0, uuid.Nil, fmt.Errorf("seat map: %w", ErrNotFound)
	}
	if err != nil {
		return uuid.Nil, 0, uuid.Nil, fmt.Errorf("resolve seat-map family: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, family); err != nil {
		return uuid.Nil, 0, uuid.Nil, fmt.Errorf("lock seat-map family: %w", err)
	}
	// Now that the family is exclusively held, the current published version is
	// stable for the rest of this transaction. organizer_id is carried through
	// explicitly (defense in depth, ADR-002): the family was already resolved
	// organizer-scoped and map_family_id is immutable, so this is transitively
	// redundant — but it means the read never crosses a tenant boundary even if
	// the resolution above were ever refactored away.
	err = tx.QueryRowContext(ctx,
		`SELECT id, version FROM seat_maps
		 WHERE map_family_id = $1 AND organizer_id = $2 AND status = 'published'
		 ORDER BY version DESC
		 LIMIT 1`, family, organizerID).Scan(&id, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, 0, uuid.Nil, fmt.Errorf("seat map: %w", ErrNotFound)
	}
	if err != nil {
		return uuid.Nil, 0, uuid.Nil, fmt.Errorf("read current published version: %w", err)
	}
	return id, version, family, nil
}

// EditSeatMap edits a published seat map into a new published version (TKT-104,
// COS-1/2/3/4). The transition is state-deriving — whether the edit is legal
// depends on which seats are currently pinned — so it decides under the family
// advisory lock (lockCurrentPublishedVersion, NOT a row FOR UPDATE — see ADR-029
// and that function's doc) and emits after commit.
// A pinned seat identity that the submitted geometry does not reproduce exactly
// once is orphaned; the edit is hard-rejected (ErrSeatMapEditOrphansPinned) and
// the predecessor is left untouched. Duplicate identities in the submission are
// caught by the new version's UNIQUE(seat_map_id, seat_identity) as
// ErrSeatMapConflict. Organizer scoping is enforced in the lock resolution and
// on every insert (ADR-002).
func (p *Postgres) EditSeatMap(ctx context.Context, in EditSeatMapInput) (SeatMap, bool, error) {
	// Check every identity before opening the transaction. A rejected edit must
	// not insert a new map version or any partial geometry.
	submitted := map[string]struct{}{}
	for _, s := range in.Sections {
		for _, r := range s.Rows {
			for _, seat := range r.Seats {
				identity, err := ComposeSeatIdentity(s.Name, r.Label, seat.Label)
				if err != nil {
					return SeatMap{}, false, err
				}
				submitted[identity] = struct{}{}
			}
		}
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return SeatMap{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	curID, curVersion, family, err := lockCurrentPublishedVersion(ctx, tx, in.SeatMapID, in.OrganizerID)
	if err != nil {
		return SeatMap{}, false, err
	}

	// Pins are family-scoped and version-independent: read them under the lock so
	// a concurrent PinSeat (which holds the same lock) cannot slip a pin in
	// between this check and the commit.
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT seat_identity FROM seat_map_pins WHERE map_family_id = $1`, family)
	if err != nil {
		return SeatMap{}, false, fmt.Errorf("read pins: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var identity string
		if err := rows.Scan(&identity); err != nil {
			return SeatMap{}, false, fmt.Errorf("scan pin: %w", err)
		}
		if _, ok := submitted[identity]; !ok {
			return SeatMap{}, false, ErrSeatMapEditOrphansPinned
		}
	}
	if err := rows.Err(); err != nil {
		return SeatMap{}, false, fmt.Errorf("iterate pins: %w", err)
	}
	_ = rows.Close()

	// Create the new published version in the same family. name/organizer/venue
	// are copied from the current version so the edit is a pure geometry change.
	var newMap SeatMap
	var publishedAt sql.NullTime
	err = tx.QueryRowContext(ctx,
		`INSERT INTO seat_maps (organizer_id, venue_id, name, version, status, published_at, map_family_id, orphan_prevention_enabled)
		 SELECT organizer_id, venue_id, name, $2, 'published', now(), map_family_id,
		        COALESCE($3, orphan_prevention_enabled)
		 FROM seat_maps WHERE id = $1
		 RETURNING id, organizer_id, venue_id, name, version, status, published_at, created_at, orphan_prevention_enabled`,
		curID, curVersion+1, in.OrphanPreventionEnabled).
		Scan(&newMap.ID, &newMap.OrganizerID, &newMap.VenueID, &newMap.Name,
			&newMap.Version, &newMap.Status, &publishedAt, &newMap.CreatedAt,
			&newMap.OrphanPreventionEnabled)
	if err != nil {
		return SeatMap{}, false, fmt.Errorf("create new version: %w", err)
	}
	if publishedAt.Valid {
		newMap.PublishedAt = &publishedAt.Time
	}

	// Clone the submitted geometry into the new version. seat_identity is composed
	// server-side, exactly as AddSeatMapSeat, so a pinned identity that the caller
	// reproduced by section/row/seat labels resolves byte-identically.
	for _, s := range in.Sections {
		var sectionID uuid.UUID
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO seat_map_sections (organizer_id, seat_map_id, name, position)
			 VALUES ($1, $2, $3, $4) RETURNING id`,
			in.OrganizerID, newMap.ID, s.Name, s.Position).Scan(&sectionID); err != nil {
			if isUniqueViolation(err) {
				return SeatMap{}, false, fmt.Errorf("section: %w", ErrSeatMapConflict)
			}
			return SeatMap{}, false, fmt.Errorf("clone section: %w", err)
		}
		for _, r := range s.Rows {
			var rowID uuid.UUID
			if err := tx.QueryRowContext(ctx,
				`INSERT INTO seat_map_rows (organizer_id, seat_map_id, section_id, label, position)
				 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
				in.OrganizerID, newMap.ID, sectionID, r.Label, r.Position).Scan(&rowID); err != nil {
				if isUniqueViolation(err) {
					return SeatMap{}, false, fmt.Errorf("row: %w", ErrSeatMapConflict)
				}
				return SeatMap{}, false, fmt.Errorf("clone row: %w", err)
			}
			for _, seat := range r.Seats {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO seat_map_seats (organizer_id, seat_map_id, row_id, seat_identity, label, position)
					 VALUES ($1, $2, $3, $4 || '/' || $5 || '/' || $6, $6, $7)`,
					in.OrganizerID, newMap.ID, rowID, s.Name, r.Label, seat.Label, seat.Position); err != nil {
					if isUniqueViolation(err) {
						return SeatMap{}, false, fmt.Errorf("seat: %w", ErrSeatMapConflict)
					}
					return SeatMap{}, false, fmt.Errorf("clone seat: %w", err)
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return SeatMap{}, false, fmt.Errorf("commit edit: %w", err)
	}
	// A freshly created version always owes its event.
	return newMap, true, nil
}

// PinSeat records a sale/hold reference to a seat identity (TKT-104, COS-5 — the
// write path TKT-80 consumes). Under the SAME family advisory lock EditSeatMap
// takes, it re-resolves the current published version and validates the identity
// exists in that version before inserting, so an edit that already dropped the
// seat is visible here as ErrSeatIdentityNotFound. Idempotent on
// (map_family_id, seat_identity, pinned_by).
func (p *Postgres) PinSeat(ctx context.Context, in PinSeatInput) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	curID, _, family, err := lockCurrentPublishedVersion(ctx, tx, in.SeatMapID, in.OrganizerID)
	if err != nil {
		return err
	}

	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM seat_map_seats WHERE seat_map_id = $1 AND seat_identity = $2)`,
		curID, in.SeatIdentity).Scan(&exists); err != nil {
		return fmt.Errorf("check seat identity: %w", err)
	}
	if !exists {
		return ErrSeatIdentityNotFound
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO seat_map_pins (organizer_id, map_family_id, seat_identity, pinned_by)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (map_family_id, seat_identity, pinned_by) DO NOTHING`,
		in.OrganizerID, family, in.SeatIdentity, in.PinnedBy); err != nil {
		return fmt.Errorf("insert pin: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit pin: %w", err)
	}
	return nil
}

// UnpinSeat clears a pin (sale cancelled / hold released). Idempotent: removing
// an absent pin is a no-op. It runs under the SAME family advisory lock as
// PinSeat/EditSeatMap (ai-review F4): a standalone DELETE could not see an
// uncommitted concurrent PinSeat and would report a release that then never
// happened once the pin commits — serializing on the family closes that window.
func (p *Postgres) UnpinSeat(ctx context.Context, in PinSeatInput) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Resolve the family (organizer-scoped) and take the family advisory lock, so
	// the delete serializes with a concurrent pin/edit on the same family.
	var family uuid.UUID
	err = tx.QueryRowContext(ctx,
		`SELECT map_family_id FROM seat_maps WHERE id = $1 AND organizer_id = $2`,
		in.SeatMapID, in.OrganizerID).Scan(&family)
	if errors.Is(err, sql.ErrNoRows) {
		// Nothing to unpin — but SAY WHICH nothing (TKT-306). Still not an error to the
		// caller (see the handler), and still idempotent; the sentinel exists so a
		// caller that passed the wrong organizer for a real map can tell that apart
		// from pins that were already released.
		return ErrSeatMapFamilyNotFound
	}
	if err != nil {
		return fmt.Errorf("resolve seat-map family: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, family); err != nil {
		return fmt.Errorf("lock seat-map family: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM seat_map_pins
		 WHERE organizer_id = $1 AND map_family_id = $2 AND seat_identity = $3 AND pinned_by = $4`,
		in.OrganizerID, family, in.SeatIdentity, in.PinnedBy); err != nil {
		return fmt.Errorf("unpin seat: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit unpin: %w", err)
	}
	return nil
}

// PinSeats pins a seat set atomically (TKT-80): one family advisory lock, every identity
// validated against the current published version, all inserted or — if any is absent —
// none, returning ErrSeatIdentityNotFound. This mirrors the single-seat PinSeat's
// lock-then-validate-then-insert but over a set, so an inventory seat-hold's pins land
// all-or-nothing under the same lock EditSeatMap takes.
func (p *Postgres) PinSeats(ctx context.Context, in BatchPinInput) error {
	if len(in.SeatIdentities) == 0 {
		return nil
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	curID, _, family, err := lockCurrentPublishedVersion(ctx, tx, in.SeatMapID, in.OrganizerID)
	if err != nil {
		return err
	}
	for _, identity := range in.SeatIdentities {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM seat_map_seats WHERE seat_map_id = $1 AND seat_identity = $2)`,
			curID, identity).Scan(&exists); err != nil {
			return fmt.Errorf("check seat identity: %w", err)
		}
		if !exists {
			return ErrSeatIdentityNotFound
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO seat_map_pins (organizer_id, map_family_id, seat_identity, pinned_by)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (map_family_id, seat_identity, pinned_by) DO NOTHING`,
			in.OrganizerID, family, identity, in.PinnedBy); err != nil {
			return fmt.Errorf("insert pin: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit pins: %w", err)
	}
	return nil
}

// UnpinSeats clears a seat set atomically under the same family advisory lock. Idempotent:
// removing an absent pin is a no-op, so a retried release cannot fail.
func (p *Postgres) UnpinSeats(ctx context.Context, in BatchPinInput) error {
	if len(in.SeatIdentities) == 0 {
		return nil
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var family uuid.UUID
	err = tx.QueryRowContext(ctx,
		`SELECT map_family_id FROM seat_maps WHERE id = $1 AND organizer_id = $2`,
		in.SeatMapID, in.OrganizerID).Scan(&family)
	if errors.Is(err, sql.ErrNoRows) {
		// See UnpinSeat: distinguishable, not fatal (TKT-306).
		return ErrSeatMapFamilyNotFound
	}
	if err != nil {
		return fmt.Errorf("resolve seat-map family: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, family); err != nil {
		return fmt.Errorf("lock seat-map family: %w", err)
	}
	for _, identity := range in.SeatIdentities {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM seat_map_pins
			 WHERE organizer_id = $1 AND map_family_id = $2 AND pinned_by = $3 AND seat_identity = $4`,
			in.OrganizerID, family, in.PinnedBy, identity); err != nil {
			return fmt.Errorf("unpin seat: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit unpins: %w", err)
	}
	return nil
}

// ListSeatMapPins drains seat_map_pins one bounded page at a time, keyset-ordered by the
// primary key (TKT-112). The scan is bounded by the page, which the PRIMARY KEY's unique
// index backs — no additional index, and no ADR-019 subset claim to prove, because this is
// a deliberate FULL drain for an operator command rather than a scoped read.
//
// Every pin is returned regardless of `pinned_by` namespace: the reconciler owns the
// hold/sale classification, and a catalog-side filter would silently define which pins are
// reclaimable in the service that has no way to know.
//
// The seat-map id is resolved per FAMILY (newest version), not per creation version — pins
// are version-independent (ADR-029) and `UnpinSeats` resolves the family from whatever
// version id it is handed, so the newest member reaches every pin in the lineage. The join
// is an inner one: a pin whose family has no seat_maps row left cannot be named by any
// version id, so it is unreachable through the family-locked path and is not listed.
func (p *Postgres) ListSeatMapPins(ctx context.Context, after uuid.UUID, limit int) ([]SeatMapPin, error) {
	if limit <= 0 || limit > MaxSeatMapPinPage {
		return nil, fmt.Errorf("seat-map pin page limit must be 1..%d, got %d", MaxSeatMapPinPage, limit)
	}
	rows, err := p.db.QueryContext(ctx,
		`SELECT p.id, p.organizer_id, m.id, p.seat_identity, p.pinned_by
		   FROM seat_map_pins p
		   JOIN LATERAL (
		        SELECT s.id FROM seat_maps s
		         WHERE s.map_family_id = p.map_family_id AND s.organizer_id = p.organizer_id
		         ORDER BY s.version DESC LIMIT 1
		   ) m ON true
		  WHERE p.id > $1
		  ORDER BY p.id
		  LIMIT $2`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list seat-map pins: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []SeatMapPin{}
	for rows.Next() {
		var pin SeatMapPin
		if err := rows.Scan(&pin.ID, &pin.OrganizerID, &pin.SeatMapID, &pin.SeatIdentity, &pin.PinnedBy); err != nil {
			return nil, fmt.Errorf("scan seat-map pin: %w", err)
		}
		out = append(out, pin)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate seat-map pins: %w", err)
	}
	return out, nil
}

func (p *Postgres) AddSeatMapSection(ctx context.Context, in SeatMapSectionInput) (SeatMapSection, error) {
	s := SeatMapSection{Name: in.Name, Position: in.Position}
	err := p.db.QueryRowContext(ctx,
		`INSERT INTO seat_map_sections (organizer_id, seat_map_id, name, position)
		 SELECT $1, m.id, $3, $4 FROM seat_maps m
		 WHERE m.id = $2 AND m.organizer_id = $1 AND m.status = 'draft'
		 RETURNING id`,
		in.OrganizerID, in.SeatMapID, in.Name, in.Position).Scan(&s.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return SeatMapSection{}, fmt.Errorf("seat map: %w", ErrNotFound)
	}
	if isUniqueViolation(err) {
		return SeatMapSection{}, fmt.Errorf("section: %w", ErrSeatMapConflict)
	}
	if err != nil {
		return SeatMapSection{}, fmt.Errorf("add section: %w", err)
	}
	return s, nil
}

func (p *Postgres) AddSeatMapRow(ctx context.Context, in SeatMapRowInput) (SeatMapRow, error) {
	r := SeatMapRow{Label: in.Label, Position: in.Position}
	err := p.db.QueryRowContext(ctx,
		`INSERT INTO seat_map_rows (organizer_id, seat_map_id, section_id, label, position)
		 SELECT $1, s.seat_map_id, s.id, $4, $5 FROM seat_map_sections s
		 JOIN seat_maps m ON m.id = s.seat_map_id
		 WHERE s.id = $3 AND s.seat_map_id = $2 AND s.organizer_id = $1 AND m.status = 'draft'
		 RETURNING id`,
		in.OrganizerID, in.SeatMapID, in.SectionID, in.Label, in.Position).Scan(&r.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return SeatMapRow{}, fmt.Errorf("section: %w", ErrNotFound)
	}
	if isUniqueViolation(err) {
		return SeatMapRow{}, fmt.Errorf("row: %w", ErrSeatMapConflict)
	}
	if err != nil {
		return SeatMapRow{}, fmt.Errorf("add row: %w", err)
	}
	return r, nil
}

func (p *Postgres) AddSeatMapSeat(ctx context.Context, in SeatMapSeatInput) (SeatMapSeat, error) {
	// Resolve the immutable parent labels first so an overlong identity is refused
	// before the INSERT. The INSERT repeats the ownership and draft predicates in
	// case the map is published between these two statements.
	var sectionName, rowLabel string
	err := p.db.QueryRowContext(ctx,
		`SELECT sec.name, r.label
		 FROM seat_map_rows r
		 JOIN seat_map_sections sec ON sec.id = r.section_id
		 JOIN seat_maps m ON m.id = r.seat_map_id
		 WHERE r.id = $3 AND r.seat_map_id = $2 AND r.organizer_id = $1 AND m.status = 'draft'`,
		in.OrganizerID, in.SeatMapID, in.RowID).Scan(&sectionName, &rowLabel)
	if errors.Is(err, sql.ErrNoRows) {
		return SeatMapSeat{}, fmt.Errorf("row: %w", ErrNotFound)
	}
	if err != nil {
		return SeatMapSeat{}, fmt.Errorf("resolve seat identity: %w", err)
	}
	identity, err := ComposeSeatIdentity(sectionName, rowLabel, in.Label)
	if err != nil {
		return SeatMapSeat{}, err
	}

	seat := SeatMapSeat{Label: in.Label, Position: in.Position, SeatIdentity: identity}
	err = p.db.QueryRowContext(ctx,
		`INSERT INTO seat_map_seats (organizer_id, seat_map_id, row_id, seat_identity, label, position)
		 SELECT $1, r.seat_map_id, r.id, $4, $5, $6
		 FROM seat_map_rows r
		 JOIN seat_maps m ON m.id = r.seat_map_id
		 WHERE r.id = $3 AND r.seat_map_id = $2 AND r.organizer_id = $1 AND m.status = 'draft'
		 RETURNING id`,
		in.OrganizerID, in.SeatMapID, in.RowID, identity, in.Label, in.Position).
		Scan(&seat.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return SeatMapSeat{}, fmt.Errorf("row: %w", ErrNotFound)
	}
	if isUniqueViolation(err) {
		return SeatMapSeat{}, fmt.Errorf("seat: %w", ErrSeatMapConflict)
	}
	if err != nil {
		return SeatMapSeat{}, fmt.Errorf("add seat: %w", err)
	}
	return seat, nil
}

func (p *Postgres) ListVenueSeatMaps(ctx context.Context, venueID uuid.UUID) ([]SeatMap, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, organizer_id, venue_id, name, version, status, created_at, orphan_prevention_enabled
		   FROM seat_maps WHERE venue_id = $1 ORDER BY version, name, id`, venueID)
	if err != nil {
		return nil, fmt.Errorf("list seat maps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]SeatMap, 0)
	for rows.Next() {
		var m SeatMap
		if err := rows.Scan(&m.ID, &m.OrganizerID, &m.VenueID, &m.Name, &m.Version, &m.Status, &m.CreatedAt, &m.OrphanPreventionEnabled); err != nil {
			return nil, fmt.Errorf("scan seat map: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListSeatMapVersions returns every version of the family that seatMapID belongs
// to (TKT-105 COS-3), newest first, each carrying published_at. The family is
// resolved from any version id via a subselect on map_family_id (immutable,
// UNIQUE(map_family_id, version) — migration 0011); an unknown id yields no rows
// and ErrNotFound. Unscoped-by-organizer like its sibling public reads
// (ListVenueSeatMaps, GetSeatMapGeometry) — the map id is unguessable and the
// read is non-sensitive geometry metadata.
func (p *Postgres) ListSeatMapVersions(ctx context.Context, seatMapID uuid.UUID) ([]SeatMap, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, organizer_id, venue_id, name, version, status, published_at, created_at, orphan_prevention_enabled
		   FROM seat_maps
		  WHERE map_family_id = (SELECT map_family_id FROM seat_maps WHERE id = $1)
		  ORDER BY version DESC`, seatMapID)
	if err != nil {
		return nil, fmt.Errorf("list seat-map versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]SeatMap, 0)
	for rows.Next() {
		var m SeatMap
		if err := rows.Scan(&m.ID, &m.OrganizerID, &m.VenueID, &m.Name, &m.Version, &m.Status, &m.PublishedAt, &m.CreatedAt, &m.OrphanPreventionEnabled); err != nil {
			return nil, fmt.Errorf("scan seat-map version: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("seat map: %w", ErrNotFound)
	}
	return out, nil
}

// UpdateVenueGACapacity sets a venue's GA capacity (TKT-105 COS-5). The
// organizer_id predicate scopes the write to the tenant (ADR-002); no matching
// row yields ErrNotFound (unknown venue or cross-tenant).
func (p *Postgres) UpdateVenueGACapacity(ctx context.Context, in VenueGACapacityInput) (Venue, error) {
	v := Venue{ID: in.VenueID, OrganizerID: in.OrganizerID, GACapacity: in.GACapacity}
	err := p.db.QueryRowContext(ctx,
		`UPDATE venues SET ga_capacity = $3
		  WHERE id = $1 AND organizer_id = $2
		  RETURNING name, created_at`,
		in.VenueID, in.OrganizerID, in.GACapacity).Scan(&v.Name, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Venue{}, fmt.Errorf("venue: %w", ErrNotFound)
	}
	if err != nil {
		return Venue{}, fmt.Errorf("update venue ga_capacity: %w", err)
	}
	return v, nil
}

// seatMapSeatsScopedQuery is the seat-level geometry read, referenced as a const
// so the ADR-019 scan-scope proof EXPLAINs the exact shipped statement. It
// filters seats by seat_map_id, backed by seat_map_seats_by_map.
const seatMapSeatsScopedQuery = `SELECT id, row_id, seat_identity, label, position
	FROM seat_map_seats WHERE seat_map_id = $1 ORDER BY position`

func (p *Postgres) GetSeatMapGeometry(ctx context.Context, seatMapID uuid.UUID) (SeatMapGeometry, error) {
	var g SeatMapGeometry
	err := p.db.QueryRowContext(ctx,
		`SELECT id, organizer_id, venue_id, name, version, status, created_at, orphan_prevention_enabled
		   FROM seat_maps WHERE id = $1`, seatMapID).
		Scan(&g.Map.ID, &g.Map.OrganizerID, &g.Map.VenueID, &g.Map.Name,
			&g.Map.Version, &g.Map.Status, &g.Map.CreatedAt, &g.Map.OrphanPreventionEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return SeatMapGeometry{}, ErrNotFound
	}
	if err != nil {
		return SeatMapGeometry{}, fmt.Errorf("get seat map: %w", err)
	}

	// Rows keyed by their parent so the three flat reads assemble into a tree.
	sections := map[uuid.UUID]*SeatMapSection{}
	rowsByID := map[uuid.UUID]*SeatMapRow{}

	secRows, err := p.db.QueryContext(ctx,
		`SELECT id, name, position FROM seat_map_sections WHERE seat_map_id = $1 ORDER BY position`, seatMapID)
	if err != nil {
		return SeatMapGeometry{}, fmt.Errorf("read sections: %w", err)
	}
	defer func() { _ = secRows.Close() }()
	for secRows.Next() {
		var s SeatMapSection
		if err := secRows.Scan(&s.ID, &s.Name, &s.Position); err != nil {
			return SeatMapGeometry{}, fmt.Errorf("scan section: %w", err)
		}
		s.Rows = []SeatMapRow{}
		g.Sections = append(g.Sections, s)
	}
	if err := secRows.Err(); err != nil {
		return SeatMapGeometry{}, fmt.Errorf("sections rows: %w", err)
	}
	for i := range g.Sections {
		sections[g.Sections[i].ID] = &g.Sections[i]
	}

	rowRows, err := p.db.QueryContext(ctx,
		`SELECT id, section_id, label, position FROM seat_map_rows WHERE seat_map_id = $1 ORDER BY position`, seatMapID)
	if err != nil {
		return SeatMapGeometry{}, fmt.Errorf("read rows: %w", err)
	}
	defer func() { _ = rowRows.Close() }()
	for rowRows.Next() {
		var r SeatMapRow
		var sectionID uuid.UUID
		if err := rowRows.Scan(&r.ID, &sectionID, &r.Label, &r.Position); err != nil {
			return SeatMapGeometry{}, fmt.Errorf("scan row: %w", err)
		}
		r.Seats = []SeatMapSeat{}
		if sec, ok := sections[sectionID]; ok {
			sec.Rows = append(sec.Rows, r)
		}
	}
	if err := rowRows.Err(); err != nil {
		return SeatMapGeometry{}, fmt.Errorf("rows rows: %w", err)
	}
	for si := range g.Sections {
		for ri := range g.Sections[si].Rows {
			rowsByID[g.Sections[si].Rows[ri].ID] = &g.Sections[si].Rows[ri]
		}
	}

	seatRows, err := p.db.QueryContext(ctx, seatMapSeatsScopedQuery, seatMapID)
	if err != nil {
		return SeatMapGeometry{}, fmt.Errorf("read seats: %w", err)
	}
	defer func() { _ = seatRows.Close() }()
	for seatRows.Next() {
		var s SeatMapSeat
		var rowID uuid.UUID
		if err := seatRows.Scan(&s.ID, &rowID, &s.SeatIdentity, &s.Label, &s.Position); err != nil {
			return SeatMapGeometry{}, fmt.Errorf("scan seat: %w", err)
		}
		if r, ok := rowsByID[rowID]; ok {
			r.Seats = append(r.Seats, s)
		}
	}
	if err := seatRows.Err(); err != nil {
		return SeatMapGeometry{}, fmt.Errorf("seats rows: %w", err)
	}
	return g, nil
}
