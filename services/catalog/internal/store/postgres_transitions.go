package store

// Grouped publish/archive transitions for series and festivals. Members refuse
// their own publish/archive; the group is the only transition point (ADR-018).

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
)

func (p *Postgres) PublishSeries(ctx context.Context, organizerID, id uuid.UUID) ([]SeriesTransition, error) {
	return p.transitionSeries(ctx, organizerID, id, "published")
}

func (p *Postgres) ArchiveSeries(ctx context.Context, organizerID, id uuid.UUID) ([]SeriesTransition, error) {
	return p.transitionSeries(ctx, organizerID, id, "archived")
}

func (p *Postgres) transitionSeries(ctx context.Context, organizerID, id uuid.UUID, target string) ([]SeriesTransition, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	// The organizer is a PREDICATE on the locked lookup, not a comparison after
	// it (TKT-251): a series belonging to another organizer is ErrNotFound here,
	// under the same lock that gates the transition, so there is no window in
	// which the row is read unscoped and no answer that distinguishes "not
	// yours" from "no such series" (ADR-018 — the decision happens under the
	// row lock).
	var lockedID uuid.UUID
	if err = tx.QueryRowContext(ctx, `SELECT id FROM series WHERE id=$1 AND organizer_id=$2 FOR UPDATE`, id, organizerID).Scan(&lockedID); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	type member struct {
		id               uuid.UUID
		status           string
		version, emitted int32
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.id,p.status,p.closure_version,p.closure_emitted_version
		FROM series_performances sp JOIN performances p ON p.id=sp.performance_id
		WHERE sp.series_id=$1 ORDER BY p.id FOR UPDATE OF p`, id)
	if err != nil {
		return nil, err
	}
	var members []member
	for rows.Next() {
		var m member
		if err = rows.Scan(&m.id, &m.status, &m.version, &m.emitted); err != nil {
			_ = rows.Close()
			return nil, err
		}
		members = append(members, m)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	// Close does NOT report an iteration failure — sql.Rows.Close returns the driver's
	// close error and leaves rs.lasterr to Err(). Without this a connection that died
	// mid-read looks like a short series: the transitions below run over the members
	// that happened to arrive, and COMMIT. transitionFestival has always checked; this
	// twin did not (TKT-184).
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, ErrEmptySeries
	}
	for _, m := range members {
		switch target {
		case "published":
			if m.status == "archived" {
				return nil, &SeriesTransitionConflict{PerformanceID: m.id, Reason: "archived member cannot be published", Cause: ErrIllegalTransition}
			}
			if m.status == "draft" {
				var sellable bool
				if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_types WHERE performance_id=$1)`, m.id).Scan(&sellable); err != nil {
					return nil, err
				}
				if !sellable {
					return nil, &SeriesTransitionConflict{PerformanceID: m.id, Reason: "member has no ticket type", Cause: ErrNotSellable}
				}
			}
		case "archived":
			if m.status == "draft" {
				return nil, &SeriesTransitionConflict{PerformanceID: m.id, Reason: "draft member cannot be archived", Cause: ErrIllegalTransition}
			}
			if m.status == "published" && m.emitted < m.version {
				return nil, &SeriesTransitionConflict{PerformanceID: m.id, Reason: "member has an owed closure event", Cause: ErrClosurePending}
			}
		}
	}
	for _, m := range members {
		if target == "published" && m.status == "draft" {
			if _, err = tx.ExecContext(ctx, `UPDATE performances SET status='published',published_at=now() WHERE id=$1`, m.id); err != nil {
				return nil, err
			}
		}
		if target == "archived" && m.status == "published" {
			if _, err = tx.ExecContext(ctx, `UPDATE performances SET status='archived',archived_at=now() WHERE id=$1`, m.id); err != nil {
				return nil, err
			}
		}
	}
	out := make([]SeriesTransition, 0, len(members))
	for _, m := range members {
		perf, pubMark, archiveMark, e := p.getPerformanceTx(ctx, tx, m.id)
		if e != nil {
			return nil, e
		}
		out = append(out, SeriesTransition{Performance: perf, PublishNeedsEmit: pubMark == nil, ArchiveNeedsEmit: target == "archived" && archiveMark == nil})
	}
	if err = p.commitPublicRead(tx, PublicReadAll); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *Postgres) PublishFestival(ctx context.Context, organizerID, id uuid.UUID) ([]SeriesTransition, error) {
	return p.transitionFestival(ctx, organizerID, id, "published")
}

func (p *Postgres) ArchiveFestival(ctx context.Context, organizerID, id uuid.UUID) ([]SeriesTransition, error) {
	return p.transitionFestival(ctx, organizerID, id, "archived")
}

// transitionFestival keeps the group status, every member transition and the
// owed-event snapshots in one row-locked decision. Emission happens only after
// this transaction commits in the API layer.
func (p *Postgres) transitionFestival(ctx context.Context, organizerID, id uuid.UUID, target string) ([]SeriesTransition, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var festivalStatus string
	var sharedCapacity int32
	// organizer_id was not even in this projection before TKT-251 — the column is
	// added as a PREDICATE, not read and compared afterwards.
	if err = tx.QueryRowContext(ctx, `SELECT status,shared_capacity FROM festivals WHERE id=$1 AND organizer_id=$2 FOR UPDATE`, id, organizerID).
		Scan(&festivalStatus, &sharedCapacity); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	type member struct {
		id               uuid.UUID
		status           string
		version, emitted int32
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.id,p.status,p.closure_version,p.closure_emitted_version
		FROM performances p WHERE p.capacity_group_id=$1 ORDER BY p.id FOR UPDATE OF p`, id)
	if err != nil {
		return nil, err
	}
	var members []member
	for rows.Next() {
		var m member
		if err = rows.Scan(&m.id, &m.status, &m.version, &m.emitted); err != nil {
			_ = rows.Close()
			return nil, err
		}
		members = append(members, m)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, ErrEmptyFestival
	}
	for _, m := range members {
		switch target {
		case "published":
			if m.status == "archived" {
				return nil, &FestivalTransitionConflict{PerformanceID: m.id, Reason: "archived member cannot be published", Cause: ErrIllegalTransition}
			}
			if m.status == "draft" {
				var sellable bool
				if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_types WHERE performance_id=$1)`, m.id).Scan(&sellable); err != nil {
					return nil, err
				}
				if !sellable {
					return nil, &FestivalTransitionConflict{PerformanceID: m.id, Reason: "member has no ticket type", Cause: ErrNotSellable}
				}
			}
		case "archived":
			if m.status == "draft" {
				return nil, &FestivalTransitionConflict{PerformanceID: m.id, Reason: "draft member cannot be archived", Cause: ErrIllegalTransition}
			}
			if m.status == "published" && m.emitted < m.version {
				return nil, &FestivalTransitionConflict{PerformanceID: m.id, Reason: "member has an owed closure event", Cause: ErrClosurePending}
			}
		}
	}
	for _, m := range members {
		if target == "published" && m.status == "draft" {
			if _, err = tx.ExecContext(ctx, `UPDATE performances SET status='published',published_at=now() WHERE id=$1`, m.id); err != nil {
				return nil, err
			}
		}
		if target == "archived" && m.status == "published" {
			if _, err = tx.ExecContext(ctx, `UPDATE performances SET status='archived',archived_at=now() WHERE id=$1`, m.id); err != nil {
				return nil, err
			}
		}
	}
	if festivalStatus != target {
		if _, err = tx.ExecContext(ctx, `UPDATE festivals SET status=$2 WHERE id=$1`, id, target); err != nil {
			return nil, err
		}
	}
	out := make([]SeriesTransition, 0, len(members))
	for _, m := range members {
		perf, pubMark, archiveMark, getErr := p.getPerformanceTx(ctx, tx, m.id)
		if getErr != nil {
			return nil, getErr
		}
		perf.SharedCapacity = &sharedCapacity
		out = append(out, SeriesTransition{Performance: perf, PublishNeedsEmit: pubMark == nil, ArchiveNeedsEmit: target == "archived" && archiveMark == nil})
	}
	if err = p.commitPublicRead(tx, PublicReadAll); err != nil {
		return nil, err
	}
	return out, nil
}
