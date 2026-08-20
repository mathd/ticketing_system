package store

// Grouping aggregates: series, seasons and festivals — creation, reads and
// membership attachment. Their state-deriving transitions live in
// postgres_transitions.go (ADR-018), and the published read shapes in
// postgres_public_read.go.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

func (p *Postgres) CreateSeries(ctx context.Context, in SeriesInput) (Series, error) {
	raw, err := json.Marshal(in.Name)
	if err != nil {
		return Series{}, err
	}
	var s Series
	err = p.db.QueryRowContext(ctx, `INSERT INTO series(organizer_id,event_id,name)
		SELECT $1,e.id,$3 FROM events e WHERE e.id=$2 AND e.organizer_id=$1
		RETURNING id,organizer_id,event_id,name,created_at`, in.OrganizerID, in.EventID, raw).
		Scan(&s.ID, &s.OrganizerID, &s.EventID, &raw, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Series{}, ErrNotFound
	}
	if err != nil {
		return Series{}, fmt.Errorf("create series: %w", err)
	}
	if err := json.Unmarshal(raw, &s.Name); err != nil {
		return Series{}, err
	}
	s.Members = []SeriesMember{}
	return s, nil
}

func getSeriesFrom(ctx context.Context, q rowQueryer, id uuid.UUID) (Series, error) {
	var s Series
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT id,organizer_id,event_id,name,created_at FROM series WHERE id=$1`, id).
		Scan(&s.ID, &s.OrganizerID, &s.EventID, &raw, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Series{}, ErrNotFound
	}
	if err != nil {
		return Series{}, err
	}
	if err := json.Unmarshal(raw, &s.Name); err != nil {
		return Series{}, err
	}
	rows, err := q.QueryContext(ctx, `SELECT performance_id,position FROM series_performances WHERE series_id=$1 ORDER BY position`, id)
	if err != nil {
		return Series{}, err
	}
	defer func() { _ = rows.Close() }()
	s.Members = []SeriesMember{}
	for rows.Next() {
		var m SeriesMember
		if err := rows.Scan(&m.PerformanceID, &m.Position); err != nil {
			return Series{}, err
		}
		s.Members = append(s.Members, m)
	}
	return s, rows.Err()
}

// AttachPerformanceToSeries wires a slot into a series.
//
// TKT-251: BOTH locked lookups carry the caller's verified organizer. Before
// this, the method read organizer_id from both rows and compared them to EACH
// OTHER — which two resources of the same victim tenant satisfy, so a caller
// holding only the shared staff credential could re-wire another organizer's
// series. That same-organizer comparison remains below, because it still does a
// real job for the legitimate case (your own series and your own slot belonging
// to different events); it is simply no longer load-bearing for authorization.
func (p *Postgres) AttachPerformanceToSeries(ctx context.Context, organizerID, seriesID, performanceID uuid.UUID, position int32) (Series, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return Series{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var orgID, eventID uuid.UUID
	if err = tx.QueryRowContext(ctx, `SELECT organizer_id,event_id FROM series WHERE id=$1 AND organizer_id=$2 FOR UPDATE`, seriesID, organizerID).Scan(&orgID, &eventID); errors.Is(err, sql.ErrNoRows) {
		return Series{}, ErrNotFound
	} else if err != nil {
		return Series{}, err
	}
	var targetOrg, targetEvent uuid.UUID
	var status string
	if err = tx.QueryRowContext(ctx, `SELECT organizer_id,event_id,status FROM performances WHERE id=$1 AND organizer_id=$2 FOR UPDATE`, performanceID, organizerID).Scan(&targetOrg, &targetEvent, &status); errors.Is(err, sql.ErrNoRows) {
		return Series{}, ErrNotFound
	} else if err != nil {
		return Series{}, err
	}
	if targetOrg != orgID || targetEvent != eventID {
		return Series{}, ErrOrganizerMismatch
	}
	if status != "draft" {
		return Series{}, ErrMembershipFrozen
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.status FROM series_performances sp JOIN performances p ON p.id=sp.performance_id WHERE sp.series_id=$1 ORDER BY p.id FOR UPDATE OF p`, seriesID)
	if err != nil {
		return Series{}, err
	}
	launched := false
	for rows.Next() {
		var memberStatus string
		if err = rows.Scan(&memberStatus); err != nil {
			_ = rows.Close()
			return Series{}, err
		}
		launched = launched || memberStatus != "draft"
	}
	if err = rows.Close(); err != nil {
		return Series{}, err
	}
	if err = rows.Err(); err != nil {
		return Series{}, err
	}
	if launched {
		return Series{}, ErrMembershipFrozen
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO series_performances(series_id,performance_id,position) VALUES($1,$2,$3)`, seriesID, performanceID, position); err != nil {
		if isUniqueViolation(err) {
			return Series{}, ErrMembershipConflict
		}
		return Series{}, err
	}
	s, err := getSeriesFrom(ctx, tx, seriesID)
	if err != nil {
		return Series{}, err
	}
	if err = tx.Commit(); err != nil {
		return Series{}, err
	}
	return s, nil
}

func (p *Postgres) CreateSeason(ctx context.Context, in SeasonInput) (Season, error) {
	raw, err := json.Marshal(in.Name)
	if err != nil {
		return Season{}, err
	}
	var s Season
	err = p.db.QueryRowContext(ctx, `INSERT INTO seasons(organizer_id,name) SELECT o.id,$2 FROM organizers o WHERE o.id=$1 RETURNING id,organizer_id,name,created_at`, in.OrganizerID, raw).Scan(&s.ID, &s.OrganizerID, &raw, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Season{}, ErrNotFound
	}
	if err != nil {
		return Season{}, err
	}
	if err = json.Unmarshal(raw, &s.Name); err != nil {
		return Season{}, err
	}
	s.SeriesIDs = []uuid.UUID{}
	s.EventIDs = []uuid.UUID{}
	return s, nil
}

func getSeasonFrom(ctx context.Context, q rowQueryer, id uuid.UUID) (Season, error) {
	var s Season
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT id,organizer_id,name,created_at FROM seasons WHERE id=$1`, id).
		Scan(&s.ID, &s.OrganizerID, &raw, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Season{}, ErrNotFound
	}
	if err != nil {
		return Season{}, err
	}
	if err = json.Unmarshal(raw, &s.Name); err != nil {
		return Season{}, err
	}
	s.SeriesIDs = []uuid.UUID{}
	s.EventIDs = []uuid.UUID{}
	for query, dest := range map[string]*[]uuid.UUID{
		`SELECT series_id FROM season_series WHERE season_id=$1 ORDER BY series_id`: &s.SeriesIDs,
		`SELECT event_id FROM season_events WHERE season_id=$1 ORDER BY event_id`:   &s.EventIDs,
	} {
		rows, queryErr := q.QueryContext(ctx, query, id)
		if queryErr != nil {
			return Season{}, queryErr
		}
		for rows.Next() {
			var memberID uuid.UUID
			if scanErr := rows.Scan(&memberID); scanErr != nil {
				_ = rows.Close()
				return Season{}, scanErr
			}
			*dest = append(*dest, memberID)
		}
		if closeErr := rows.Close(); closeErr != nil {
			return Season{}, closeErr
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			return Season{}, rowsErr
		}
	}
	return s, nil
}

func (p *Postgres) CreateFestival(ctx context.Context, in FestivalInput) (Festival, error) {
	raw, err := json.Marshal(in.Name)
	if err != nil {
		return Festival{}, err
	}
	var f Festival
	err = p.db.QueryRowContext(ctx, `INSERT INTO festivals(organizer_id,name,shared_capacity)
		SELECT o.id,$2,$3 FROM organizers o WHERE o.id=$1
		RETURNING id,organizer_id,name,shared_capacity,status,created_at`, in.OrganizerID, raw, in.SharedCapacity).
		Scan(&f.ID, &f.OrganizerID, &raw, &f.SharedCapacity, &f.Status, &f.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Festival{}, ErrNotFound
	}
	if err != nil {
		return Festival{}, err
	}
	if err = json.Unmarshal(raw, &f.Name); err != nil {
		return Festival{}, err
	}
	f.MemberIDs = []uuid.UUID{}
	return f, nil
}

func getFestivalFrom(ctx context.Context, q rowQueryer, id uuid.UUID) (Festival, error) {
	var f Festival
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT id,organizer_id,name,shared_capacity,status,created_at FROM festivals WHERE id=$1`, id).
		Scan(&f.ID, &f.OrganizerID, &raw, &f.SharedCapacity, &f.Status, &f.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Festival{}, ErrNotFound
	}
	if err != nil {
		return Festival{}, err
	}
	if err = json.Unmarshal(raw, &f.Name); err != nil {
		return Festival{}, err
	}
	f.MemberIDs = []uuid.UUID{}
	rows, err := q.QueryContext(ctx, `SELECT id FROM performances WHERE capacity_group_id=$1 ORDER BY id`, id)
	if err != nil {
		return Festival{}, err
	}
	for rows.Next() {
		var memberID uuid.UUID
		if err = rows.Scan(&memberID); err != nil {
			_ = rows.Close()
			return Festival{}, err
		}
		f.MemberIDs = append(f.MemberIDs, memberID)
	}
	if err = rows.Close(); err != nil {
		return Festival{}, err
	}
	if err = rows.Err(); err != nil {
		return Festival{}, err
	}
	return f, nil
}

// AttachDayToFestival carries the same TKT-251 change as
// AttachPerformanceToSeries: both locked lookups are scoped to the caller's
// verified organizer, and the festivalOrg/performanceOrg comparison below stops
// being authorization while still catching the legitimate same-tenant mismatch.
func (p *Postgres) AttachDayToFestival(ctx context.Context, organizerID, festivalID, performanceID uuid.UUID) (Festival, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return Festival{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var festivalOrg uuid.UUID
	var festivalStatus string
	var sharedCapacity int32
	if err = tx.QueryRowContext(ctx, `SELECT organizer_id,status,shared_capacity FROM festivals WHERE id=$1 AND organizer_id=$2 FOR UPDATE`, festivalID, organizerID).
		Scan(&festivalOrg, &festivalStatus, &sharedCapacity); errors.Is(err, sql.ErrNoRows) {
		return Festival{}, ErrNotFound
	} else if err != nil {
		return Festival{}, err
	}
	var performanceOrg uuid.UUID
	var kind, performanceStatus string
	var capacityGroup uuid.NullUUID
	if err = tx.QueryRowContext(ctx, `SELECT organizer_id,kind,status,capacity_group_id FROM performances WHERE id=$1 AND organizer_id=$2 FOR UPDATE`, performanceID, organizerID).
		Scan(&performanceOrg, &kind, &performanceStatus, &capacityGroup); errors.Is(err, sql.ErrNoRows) {
		return Festival{}, ErrNotFound
	} else if err != nil {
		return Festival{}, err
	}
	if festivalOrg != performanceOrg {
		return Festival{}, ErrOrganizerMismatch
	}
	if kind != KindFestivalDay {
		return Festival{}, ErrSlotKindMismatch
	}
	if festivalStatus != "draft" {
		return Festival{}, ErrFestivalNotDraft
	}
	if performanceStatus != "draft" {
		return Festival{}, ErrMembershipFrozen
	}
	if capacityGroup.Valid {
		return Festival{}, ErrAlreadyGrouped
	}
	if _, err = tx.ExecContext(ctx, `UPDATE performances SET capacity_group_id=$1 WHERE id=$2`, festivalID, performanceID); err != nil {
		return Festival{}, err
	}
	f, err := getFestivalFrom(ctx, tx, festivalID)
	if err != nil {
		return Festival{}, err
	}
	if err = tx.Commit(); err != nil {
		return Festival{}, err
	}
	return f, nil
}

func (p *Postgres) AttachSeriesToSeason(ctx context.Context, organizerID, seasonID, seriesID uuid.UUID) (Season, error) {
	return p.attachSeasonMember(ctx, organizerID, seasonID, seriesID, true)
}
func (p *Postgres) AttachEventToSeason(ctx context.Context, organizerID, seasonID, eventID uuid.UUID) (Season, error) {
	return p.attachSeasonMember(ctx, organizerID, seasonID, eventID, false)
}

// attachSeasonMember carries the same TKT-251 change as the other two attach
// paths: both the season and the member lookup are scoped to the caller's
// verified organizer, so the seasonOrg/memberOrg comparison is no longer what
// stands between a staff-credential holder and another tenant's season.
func (p *Postgres) attachSeasonMember(ctx context.Context, organizerID, seasonID, memberID uuid.UUID, isSeries bool) (Season, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return Season{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var seasonOrg, memberOrg uuid.UUID
	if err = tx.QueryRowContext(ctx, `SELECT organizer_id FROM seasons WHERE id=$1 AND organizer_id=$2 FOR UPDATE`, seasonID, organizerID).Scan(&seasonOrg); errors.Is(err, sql.ErrNoRows) {
		return Season{}, ErrNotFound
	} else if err != nil {
		return Season{}, err
	}
	table, col := "events", "event_id"
	if isSeries {
		table, col = "series", "series_id"
	}
	if err = tx.QueryRowContext(ctx, `SELECT organizer_id FROM `+table+` WHERE id=$1 AND organizer_id=$2`, memberID, organizerID).Scan(&memberOrg); errors.Is(err, sql.ErrNoRows) {
		return Season{}, ErrNotFound
	} else if err != nil {
		return Season{}, err
	}
	if memberOrg != seasonOrg {
		return Season{}, ErrOrganizerMismatch
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO season_`+table+`(season_id,`+col+`) VALUES($1,$2)`, seasonID, memberID); err != nil {
		if isUniqueViolation(err) {
			return Season{}, ErrMembershipConflict
		}
		return Season{}, err
	}
	s, err := getSeasonFrom(ctx, tx, seasonID)
	if err != nil {
		return Season{}, err
	}
	if err = p.commitPublicRead(tx, PublicReadDetail); err != nil {
		return Season{}, err
	}
	return s, nil
}
