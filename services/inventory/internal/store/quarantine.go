package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrCatalogQuarantineFull rejects a write once MaxCatalogQuarantinePending unresolved
	// rows exist. The consumer answers it with a delayed NAK: at this bound the stall is
	// deliberate, inventory-owned backpressure — never a silent drop (TKT-61's whole point).
	ErrCatalogQuarantineFull = errors.New("catalog event quarantine is full")
	// ErrCatalogQuarantineCollision reports a (subject, event_id) already quarantined with
	// different schema or bytes — a producer invariant break (ADR-009 §5), poison to the caller.
	ErrCatalogQuarantineCollision = errors.New("catalog event id already quarantined with different content")
)

// MaxCatalogQuarantinePending bounds unresolved quarantine rows. Deliberately a code constant,
// not config surface: tune it here when operational evidence demands it (TKT-68 plan-final).
const MaxCatalogQuarantinePending = 10_000

// CatalogQuarantineRetention keeps reinjected rows for post-incident inspection before pruning.
const CatalogQuarantineRetention = 7 * 24 * time.Hour

type QuarantinedCatalogEvent struct {
	Subject     string
	EventID     uuid.UUID
	Schema      int
	Envelope    []byte
	FirstSeenAt time.Time
}

// QuarantineCatalogEvent persists a future-schema event's raw envelope so the original message
// can be acked (TKT-68). Idempotent for byte-identical redeliveries; a same-key write with
// different content returns ErrCatalogQuarantineCollision without overwriting the first copy.
// The table lock serializes count-and-insert so concurrent handlers cannot race past the cap.
func (p *Postgres) QuarantineCatalogEvent(ctx context.Context, subject string, eventID uuid.UUID, schema int, envelope []byte) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `LOCK TABLE catalog_event_quarantine IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return err
	}
	// Retention pruning lives here and only here (plan-final A4): reinjected rows past their
	// retention age go as a side effect of the next write.
	if _, err = tx.ExecContext(ctx, `DELETE FROM catalog_event_quarantine
		WHERE reinjected_at IS NOT NULL AND reinjected_at < now() - $1::interval`,
		CatalogQuarantineRetention.String()); err != nil {
		return err
	}
	var existingSchema int
	var existingEnvelope []byte
	err = tx.QueryRowContext(ctx, `SELECT schema, envelope FROM catalog_event_quarantine
		WHERE subject=$1 AND event_id=$2`, subject, eventID).Scan(&existingSchema, &existingEnvelope)
	switch {
	case err == nil:
		if existingSchema != schema || !bytes.Equal(existingEnvelope, envelope) {
			return ErrCatalogQuarantineCollision
		}
		if _, err = tx.ExecContext(ctx, `UPDATE catalog_event_quarantine SET last_seen_at=now()
			WHERE subject=$1 AND event_id=$2`, subject, eventID); err != nil {
			return err
		}
		return tx.Commit()
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	var pending int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM catalog_event_quarantine
		WHERE reinjected_at IS NULL`).Scan(&pending); err != nil {
		return err
	}
	if pending >= p.quarantineLimit() {
		return ErrCatalogQuarantineFull
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO catalog_event_quarantine(subject, event_id, schema, envelope)
		VALUES($1,$2,$3,$4)`, subject, eventID, schema, envelope); err != nil {
		return err
	}
	return tx.Commit()
}

func (p *Postgres) quarantineLimit() int {
	if p.quarantineCap > 0 {
		return p.quarantineCap
	}
	return MaxCatalogQuarantinePending
}

// HasPendingCatalogQuarantine reports whether unresolved rows exist — startup readiness asks
// this because acked originals can no longer be rediscovered from JetStream.
func (p *Postgres) HasPendingCatalogQuarantine(ctx context.Context) (bool, error) {
	var pending bool
	err := p.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM catalog_event_quarantine WHERE reinjected_at IS NULL)`).Scan(&pending)
	return pending, err
}

// ListCatalogQuarantine returns unresolved rows oldest-first, keyset-paginated after the given
// row (nil = from the start). The cursor moves past unsupported rows too — rows the current
// binary cannot re-inject must not starve supported rows behind them.
func (p *Postgres) ListCatalogQuarantine(ctx context.Context, after *QuarantinedCatalogEvent, limit int) ([]QuarantinedCatalogEvent, error) {
	var rows *sql.Rows
	var err error
	if after != nil {
		rows, err = p.db.QueryContext(ctx, `SELECT subject, event_id, schema, envelope, first_seen_at
			FROM catalog_event_quarantine WHERE reinjected_at IS NULL
			AND (first_seen_at, subject, event_id) > ($1, $2, $3)
			ORDER BY first_seen_at, subject, event_id LIMIT $4`,
			after.FirstSeenAt, after.Subject, after.EventID, limit)
	} else {
		rows, err = p.db.QueryContext(ctx, `SELECT subject, event_id, schema, envelope, first_seen_at
			FROM catalog_event_quarantine WHERE reinjected_at IS NULL
			ORDER BY first_seen_at, subject, event_id LIMIT $1`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []QuarantinedCatalogEvent
	for rows.Next() {
		var e QuarantinedCatalogEvent
		if err := rows.Scan(&e.Subject, &e.EventID, &e.Schema, &e.Envelope, &e.FirstSeenAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkCatalogEventReinjected records that the broker accepted the republication. It means
// exactly that — not that inventory business processing completed. Idempotent: marking an
// already-resolved row is a no-op, so a crashed reprocess run can simply be re-run.
func (p *Postgres) MarkCatalogEventReinjected(ctx context.Context, subject string, eventID uuid.UUID) error {
	_, err := p.db.ExecContext(ctx, `UPDATE catalog_event_quarantine SET reinjected_at=now()
		WHERE subject=$1 AND event_id=$2 AND reinjected_at IS NULL`, subject, eventID)
	return err
}
