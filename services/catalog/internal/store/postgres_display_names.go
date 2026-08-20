package store

// The internal display-name resolution read (ADR-019).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// performanceDisplayNamesQuery is a const so a plan assertion can EXPLAIN the
// statement production runs rather than a lookalike (ADR-019).
//
// `= ANY($1::uuid[])` over a uuid[] rather than an IN-list built by string
// concatenation: one prepared statement shape for every page size, and no
// injection surface.
//
// The `::uuid[]` cast is REQUIRED, not decoration. Without it the driver cannot
// infer the parameter's type from `= ANY($1)` alone and the query errors at
// runtime — which ADR-028 then launders into a generic 500, so the symptom
// arrives as "response violates OpenAPI contract" a long way from the cause. The
// scoped public-performance read carries the same cast for the same reason.
//
// No publication predicate — see the port's doc comment. No organizer scope
// either: this is an internal, unscoped read, which is exactly why it sits behind
// guardInternalSurface and the gateway's edge-deny rather than in the public
// contract.
const performanceDisplayNamesQuery = `
	SELECT p.id, e.name, p.starts_at
	  FROM performances p
	  JOIN events e ON e.id = p.event_id
	 WHERE p.id = ANY($1::uuid[])`

// PerformanceDisplayNames resolves a set of performances to their event names.
func (p *Postgres) PerformanceDisplayNames(ctx context.Context, ids []uuid.UUID) ([]PerformanceDisplayName, error) {
	rows, err := p.db.QueryContext(ctx, performanceDisplayNamesQuery, ids)
	if err != nil {
		return nil, fmt.Errorf("resolve performance display names: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]PerformanceDisplayName, 0, len(ids))
	for rows.Next() {
		var d PerformanceDisplayName
		// `events.name` is jsonb and LocalizedText is a plain map — it implements
		// no sql.Scanner, so it must be scanned as bytes and unmarshalled. Every
		// other read in this file does the same; scanning straight into the map
		// compiles and fails at runtime with "unsupported Scan, storing driver
		// .Value type []uint8", which no fake-store test can reach.
		var name []byte
		var startsAt sql.NullTime
		if err := rows.Scan(&d.PerformanceID, &name, &startsAt); err != nil {
			return nil, fmt.Errorf("scan performance display name: %w", err)
		}
		if err := json.Unmarshal(name, &d.EventName); err != nil {
			return nil, fmt.Errorf("performance display name jsonb: %w", err)
		}
		if startsAt.Valid {
			at := startsAt.Time
			d.StartsAt = &at
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate performance display names: %w", err)
	}
	return out, nil
}
