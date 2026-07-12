# ADR-010: PostgreSQL claim transaction and hold lifecycle

## Status

Accepted (approved at the TKT-27 plan gate, 2026-07-12)

## Context

All dated-slot inventory models need one correctness boundary that prevents oversell under
contention. Holds also need retry-safe creation, deterministic expiry, and terminal transitions
without relying on an in-memory lock or cleanup worker.

## Decision

The inventory database is the sole write-side correctness boundary. Each mutation locks the
inventory pool row first, then its claim rows. Operations addressed by hold ID discover the
immutable pool ID with a non-locking read before taking locks, then re-read the claim after locking
the pool. This gives every path the same pool-to-claim lock order.

A claim is `held`, `confirmed`, `released`, or `expired`; the last three are terminal. PostgreSQL
time decides expiry. A transaction expires due holds before capacity accounting, so cleanup jobs
are optional optimizations rather than correctness machinery. Confirmed plus unexpired-held
quantity may never exceed pool capacity.

Hold creation requires an organizer-scoped idempotency key. Its canonical request fingerprint is
persisted: the same key and request replay the original result; the same key with different input
conflicts. Catalog publication provisions a generic dated-slot `capacity` snapshot over JetStream;
the durable inventory consumer records event IDs and never reads the catalog database.

## Consequences

- One hot pool serializes its writes, deliberately trading per-slot throughput for a simple proof.
- Expired capacity is reclaimed on the next mutation even if no sweeper runs.
- Capacity adjustments after publication require a new explicit event and policy; they cannot
  silently overwrite a pool that already has claims.
- Quantity pools are the first adapter. Reserved seats, date ranges, and entitlements reuse the
  lifecycle and lock ordering but add their own resource constraints.
