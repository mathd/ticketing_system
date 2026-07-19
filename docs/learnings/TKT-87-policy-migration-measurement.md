# TKT-87 — 0006 pass-policy migration, measured

ADR-025 §D7's obligation pattern (via ADR-008/ADR-022's 30-second out-of-band
bound): measure the **complete migration**, not the index alone, at
representative volume.

## What 0006 does

- `CREATE TABLE slot_re_entry_policies` — new, empty; no lock risk.
- `ALTER TABLE lifecycle_integrity_quarantine ADD COLUMN event_type text NOT
  NULL DEFAULT 'redeemed' CHECK (…)` — constant default, so PostgreSQL (11+)
  adds it **without a table rewrite**; the CHECK still validates existing rows
  with its own full-table scan.
- `CREATE INDEX lifecycle_integrity_quarantine_ticket_idx` — plain build
  (ADR-020), one more scan.
- `CREATE TABLE pass_policy_conflicts` — new, empty.

The hot `lifecycle_events` table is untouched.

## Measurement

Opt-in reproduction:

```
ACCESS_MIGRATION_TEST_DATABASE_URL=… \
ACCESS_MIGRATION_MEASUREMENT_QUARANTINE_ROWS=1000000 \
go test -tags smoke -run TestPassPolicyMigrationRepresentativeVolume -v ./internal/store/
```

Result (2026-07-19, WSL2, PostgreSQL 18.3):

| quarantine rows | table size | complete 0006 | bound |
|---|---|---|---|
| 1,000,000 | 202 MB | **0.48 s** | 30 s (15 s engineering target) |

Quarantine is an **error table** — realistic volumes are orders of magnitude
below the seeded 1M. Margin is ~60× at the engineering target; no `NOT VALID`
staging needed.
