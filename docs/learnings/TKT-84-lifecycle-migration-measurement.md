# TKT-84 — measured migration 0004 (repeatable admission events)

ADR-025 §D7 obliges the implementation ticket to measure the **complete** migration —
widened CHECK validation scan, both index builds (partial singleton unique + plain
`(ticket_id)` replacement), and the two constraint drops — at representative volume
against ADR-008/ADR-022's 30-second migrate bound.

## Result

| | |
|---|---|
| Elapsed (complete 0004, fresh 30s context) | **4.80 s** |
| Lifecycle rows | 10,000,002 (3,333,334 tickets × 3 singleton events) |
| `pg_total_relation_size('lifecycle_events')` before migration | 1743 MB |
| PostgreSQL | 18.4 (Debian, aarch64), pinned compose image |
| Host | Apple Silicon dev machine, Docker Desktop, default container resources |
| Verdict | **PASS** — under the 15 s engineering target (50% headroom rule), far under the 30 s hard bound |

Every seeded row matches the partial-index predicate (worst case for that build).
Seeding time excluded; timing starts immediately before `provider.UpTo(ctx, 4)` under a
fresh 30-second context and stops when goose records version 4.

## Reproduce

```sh
cd services/access
ACCESS_MIGRATION_TEST_DATABASE_URL=postgres://… \
ACCESS_MIGRATION_MEASUREMENT_TICKETS=3333334 \
go test -tags smoke -count=1 -timeout 60m -run TestRepeatableAdmissionMigrationRepresentativeVolume ./internal/store
```

The test is opt-in (skipped without the env var): seeding ~10M rows takes ~2 minutes and
does not belong in every `make check`. Re-run at the larger count if a real deployment
approaches or exceeds 10M lifecycle rows, and re-measure on deployment-representative
storage before trusting the margin there — this number is a dev-machine measurement of
CPU/IO-bound scans, not a claim about production disks.
