# TKT-85 — measured migration 0005 (per-occurrence quarantine)

ADR-025 §D7's measured-migration obligation, applied to 0005: the **complete**
migration — two column adds, `admitted_at` NOT NULL/DEFAULT drops, the
`time_check` CHECK validation scan, two partial unique index builds, and the
primary-key swap — against ADR-008/ADR-022's 30-second migrate bound.

The quarantine table is small **by design** (one live row per corrupt ticket;
reconciliation-learned rows only accumulate while chains are broken), so the
honest representative volume is far below lifecycle_events'. 1M rows is a
deployment where a canonicalization bug quarantined a million tickets — well
past the point where ADR-021 §D6's operator escalation has fired.

## Result

| | |
|---|---|
| Elapsed (complete 0005, fresh 30s context) | **1.57 s** |
| Quarantine rows | 1,000,000 (every row matching the one-admission partial index predicate — worst case) |
| PostgreSQL | 18.4 (Debian, aarch64), pinned compose image |
| Host | Apple Silicon dev machine, Docker Desktop, default container resources |
| Verdict | **PASS** — under the 15 s engineering target (50% headroom rule), far under the 30 s hard bound |

Seeding time excluded; timing starts immediately before `provider.UpTo(ctx, 5)`
under a fresh 30-second context and stops when goose records version 5.

Pre-state provenance: the measurement seeds at schema version 4 (migrations
0001–0004 applied), which **is** the production pre-state for 0005 — 0004
touches `lifecycle_events` only and never the quarantine table, and version
4's `admitted_at NOT NULL` means all-live-rows is the only seedable shape
(also the worst case for the one-admission partial index build).

## Reproduce

```sh
cd services/access
ACCESS_MIGRATION_TEST_DATABASE_URL=postgres://… \
ACCESS_QUARANTINE_MIGRATION_MEASUREMENT_ROWS=1000000 \
go test -tags smoke -count=1 -timeout 30m -run TestPerOccurrenceQuarantineMigrationRepresentativeVolume ./internal/store
```

Opt-in (skipped without the env var), same posture as TKT-84's measurement:
a dev-machine measurement of CPU/IO-bound scans, not a claim about production
disks — re-measure on deployment-representative storage before trusting the
margin there.
