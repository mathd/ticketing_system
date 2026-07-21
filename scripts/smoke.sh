#!/usr/bin/env bash
# Smoke stage lifecycle: isolated compose project (own name + shifted ports),
# trap-based teardown, logs dumped on startup failure.
# Default: host-built artifacts via compose.smoke.yaml (fast path; binaries
# from `make build-gate-linux`, scanner dist from `make build-ts`).
# SMOKE_HERMETIC=1: original in-Docker builds only (compose.yaml), used by
# the hermetic-smoke workflow and available locally.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# Isolate the stack per checkout — project name AND ports, derived from the checkout path.
#
# This repo is routinely worked on in sibling worktrees, and both halves of the isolation are
# load-bearing. A shared project name is worse than a port clash: `cleanup` below runs
# `compose down -v` on EXIT, so a second run tears the first one's stack down *mid-run* — and the
# wreckage reads like a broken change rather than a collision ("network ... not found",
# "nats-1 exited (0)", "dead or marked for removal"). That cost real debugging time on TKT-61.
# Fixing only the name would trade silent mutual destruction for a port clash, which is at least
# honest; moving the ports too lets the runs actually coexist.
#
# The slot is a stable hash of the checkout path: same worktree → same ports every run (greppable,
# debuggable), different worktrees → different ports. Distinct hosts/ranges keep the six services
# from colliding with each other. A hash collision between two worktrees just fails loudly on
# "port already allocated" — the honest failure this replaces the silent one with.
SLOT=$(( $(printf '%s' "$ROOT" | cksum | cut -d' ' -f1) % 40 ))
PROJECT="${SMOKE_COMPOSE_PROJECT:-ticketing-smoke-${SLOT}}"
export GATEWAY_PORT=$((18080 + SLOT)) POSTGRES_PORT=$((15432 + SLOT)) NATS_PORT=$((14222 + SLOT)) \
       GRAFANA_PORT=$((13000 + SLOT)) PROM_PORT=$((19090 + SLOT)) OTLP_PORT=$((14318 + SLOT)) \
       INVENTORY_PORT=$((16080 + SLOT)) COMMERCE_PORT=$((17080 + SLOT)) PAYMENTS_PORT=$((17580 + SLOT)) \
       ACCESS_EVENT_RETRY_BACKOFF=100ms,200ms,400ms,800ms,1s,1s \
       ACCESS_LIFECYCLE_CHECKPOINT_INTERVAL=1s
echo "smoke: project=$PROJECT gateway=$GATEWAY_PORT postgres=$POSTGRES_PORT nats=$NATS_PORT (slot $SLOT from $ROOT)"

# The load-proof override (pg_stat_statements preload, TKT-82) applies to every
# smoke stack — hermetic or fast path — so the gate's on-sale profile can always
# report the claim_history INSERT overhead.
COMPOSE_FILES=(-f "$ROOT/compose.yaml" -f "$ROOT/compose.onsale-load.yaml")
if [ "${SMOKE_HERMETIC:-0}" != "1" ]; then
  for b in catalog inventory commerce payments access gateway; do
    [ -x "$ROOT/bin/gate/$b" ] || { echo "smoke: missing bin/gate/$b — run 'make smoke' (or 'make build-gate-linux')" >&2; exit 1; }
  done
  [ -f "$ROOT/web/scanner/dist/index.html" ] || { echo "smoke: missing web/scanner/dist — run 'make build-ts'" >&2; exit 1; }
  [ -f "$ROOT/web/storefront/dist/server/entry.mjs" ] || { echo "smoke: missing web/storefront/dist — run 'make build-ts'" >&2; exit 1; }
  COMPOSE_FILES+=(-f "$ROOT/compose.smoke.yaml")
fi

# One credential per smoke invocation (TKT-83): generated here so the isolated
# stack never depends on a developer's .env or shell (the export takes
# precedence over compose's .env lookup) and CI needs no secret.
SMOKE_INTERNAL_TOKEN=$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')
export SMOKE_INTERNAL_TOKEN
export INTERNAL_SERVICE_TOKEN="$SMOKE_INTERNAL_TOKEN"

compose() { docker compose -p "$PROJECT" "${COMPOSE_FILES[@]}" "$@"; }
cleanup() { compose down -v --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT INT TERM

# Pre-clean, between the trap install and `up`: a hard-killed previous run
# (SIGKILL, crashed daemon, killed CI runner) never fires the trap and leaves
# its volumes behind; `compose up` would then reuse the already-migrated
# same-revision pgdata and the gate would "prove" the clean-clone bootstrap
# against schema an earlier run applied — silently voiding
# TestMigrationsAppliedOutOfBand (ADR-022). The full `down` matters, not just
# volume removal: the kill also leaves the one-shot migrate jobs as Exited-0
# containers that a plain `up` would reuse, so a volume-only pre-clean would
# recreate pgdata that nothing migrates. Scoped to this worktree's project
# name, so a sibling's running stack is untouched (TKT-70).
cleanup

if ! compose up -d --build --wait; then
  echo "--- compose up failed; recent logs: ---"
  compose logs --tail 50
  exit 1
fi

cd "$ROOT/services/commerce"
# The commerce store tests claim and retire completion_outbox rows directly, and the
# live commerce service runs an outbox drainer that polls the same table. Pointed at
# the service's own database they would race it: the drainer can claim and retire a row
# a test just seeded, so the test either fails intermittently or "passes" having proved
# nothing. Give them their own database — they exercise store functions directly and
# need no running service. Migrations run inside the tests (see storeSmokeDB).
# Separate -c flags: psql wraps a multi-statement -c in a transaction, and DROP/CREATE
# DATABASE cannot run inside one.
docker exec "$(compose ps -q postgres)" psql -U postgres -v ON_ERROR_STOP=1 \
  -c "DROP DATABASE IF EXISTS commerce_store_smoke" \
  -c "CREATE DATABASE commerce_store_smoke OWNER commerce" >/dev/null
# No -run filter: every smoke test in this package is part of the gate. An allowlist
# means a newly added test silently never runs and the gate still passes green — which
# is exactly what happened to this file's first six tests.
COMMERCE_TEST_DATABASE_URL="postgres://commerce:commerce@localhost:${POSTGRES_PORT}/commerce_store_smoke" \
go test -tags smoke -count=1 ./internal/store

cd "$ROOT/services/access"
# No -run filter, for the same reason as commerce and catalog above: an allowlist
# means a newly added test silently never runs while the gate still passes green.
# This block carried one until TKT-67, and it was the last one left — lines 48
# and 59 above had already recorded the defect twice.
ACCESS_MIGRATION_TEST_DATABASE_URL="postgres://postgres:postgres@localhost:${POSTGRES_PORT}/postgres" \
go test -tags smoke -count=1 ./internal/store

cd "$ROOT/services/catalog"
# No -run filter, for the same reason as commerce above: an allowlist means a newly
# added test silently never runs while the gate still passes green. This block used to
# carry one, and it had already stranded two tests that never ran once merged —
# TestDirectArchiveRacingFestivalPublishCannotDesync and
# TestGetPublishedFestivalOrdersDaysAcrossEventsChronologically (TKT-53's scoped-read test).
CATALOG_MIGRATION_TEST_DATABASE_URL="postgres://postgres:postgres@localhost:${POSTGRES_PORT}/postgres" \
go test -tags smoke -count=1 ./internal/store

cd "$ROOT/services/inventory"
# No -run filter, for the same reason as commerce, catalog and access above: an allowlist
# means a newly added test silently never runs while the gate still passes green. This
# block was the last one carrying one (TKT-77 removed it).
# ./internal/... (not just ./internal/store): the seat-hold handler's DB-backed smoke
# tests live in ./internal/api (TKT-80), and scoping to ./internal/store would silently
# skip them — the exact allowlist defect the notes above warn about.
INVENTORY_MIGRATION_TEST_DATABASE_URL="postgres://postgres:postgres@localhost:${POSTGRES_PORT}/postgres" \
go test -tags smoke -count=1 ./internal/...

cd "$ROOT/smoke"
SMOKE_GATEWAY_URL=http://localhost:${GATEWAY_PORT} \
SMOKE_NATS_URL=nats://localhost:${NATS_PORT} \
SMOKE_PG=localhost:${POSTGRES_PORT} \
SMOKE_PROM_URL=http://localhost:${PROM_PORT} \
SMOKE_INVENTORY_URL=http://localhost:${INVENTORY_PORT} \
SMOKE_COMMERCE_URL=http://localhost:${COMMERCE_PORT} \
SMOKE_PAYMENTS_URL=http://localhost:${PAYMENTS_PORT} \
SMOKE_COMPOSE_PROJECT="$PROJECT" \
go test -tags smoke -count=1 -v -timeout "${SMOKE_TEST_TIMEOUT:-10m}" ./...

# ADR-003: verify the populated canonical journal before Compose teardown.
compose exec -T payments /app verify-concurrent-append
compose exec -T payments /app verify-journal

# Exercise the verifier against real PostgreSQL corruption. Production
# triggers are temporarily disabled only in this isolated smoke database.
psql_payments() { compose exec -T postgres psql -v ON_ERROR_STOP=1 -U postgres -d payments -c "$1" >/dev/null; }
expect_verify_failure() {
  if compose exec -T payments /app verify-journal >/dev/null 2>&1; then
    echo "smoke: verify-journal accepted $1 corruption" >&2
    exit 1
  fi
}
psql_payments "CREATE TABLE journal_entries_smoke_backup AS TABLE journal_entries; CREATE TABLE journal_heads_smoke_backup AS TABLE journal_heads; ALTER TABLE journal_entries DISABLE TRIGGER USER;"

psql_payments "UPDATE journal_entries SET entry_hash=decode(repeat('00',32),'hex') WHERE fact_id=(SELECT fact_id FROM journal_entries ORDER BY organizer_id,sequence LIMIT 1);"
expect_verify_failure "hash"
psql_payments "TRUNCATE journal_entries; INSERT INTO journal_entries SELECT * FROM journal_entries_smoke_backup;"

psql_payments "DELETE FROM journal_entries WHERE fact_id=(SELECT fact_id FROM journal_entries ORDER BY organizer_id,sequence OFFSET 1 LIMIT 1);"
expect_verify_failure "sequence gap"
psql_payments "TRUNCATE journal_entries; INSERT INTO journal_entries SELECT * FROM journal_entries_smoke_backup;"

psql_payments "UPDATE journal_heads SET last_hash=decode(repeat('00',32),'hex');"
expect_verify_failure "chain head"
psql_payments "TRUNCATE journal_heads; INSERT INTO journal_heads SELECT * FROM journal_heads_smoke_backup; ALTER TABLE journal_entries ENABLE TRIGGER USER; DROP TABLE journal_entries_smoke_backup; DROP TABLE journal_heads_smoke_backup;"
compose exec -T payments /app verify-journal

# ADR-021: the ticket lifecycle trail gets the same treatment as the money
# journal — verify the populated trail, then prove the verifier actually rejects
# real PostgreSQL corruption rather than merely running.
#
# The scope this proves, stated the way ADR-021 §The trust boundary requires:
# modification and insertion are evident against an adversary who cannot re-sign
# the chain. A coordinated rollback (a head reverted with the checkpoint suffix
# that committed it) verifies CLEAN and is not tested here as a failure — it is a
# known, accepted gap until TKT-11, pinned by
# TestVerifyLifecycleAcceptsACoordinatedRollback in the store suite.
compose exec -T access /app verify-lifecycle

psql_access() { compose exec -T postgres psql -v ON_ERROR_STOP=1 -U postgres -d access -c "$1" >/dev/null; }
expect_lifecycle_failure() {
  if compose exec -T access /app verify-lifecycle >/dev/null 2>&1; then
    echo "smoke: verify-lifecycle accepted $1 corruption" >&2
    exit 1
  fi
}
# Production triggers are DDL and removable — which is the entire premise of
# ADR-021 §Context — so disabling them here is not cheating, it is the adversary.
psql_access "CREATE TABLE lifecycle_event_integrity_smoke_backup AS TABLE lifecycle_event_integrity;
            CREATE TABLE lifecycle_heads_smoke_backup AS TABLE lifecycle_heads;
            CREATE TABLE lifecycle_checkpoints_smoke_backup AS TABLE lifecycle_checkpoints;
            ALTER TABLE lifecycle_event_integrity DISABLE TRIGGER USER;
            ALTER TABLE lifecycle_heads DISABLE TRIGGER USER;
            ALTER TABLE lifecycle_checkpoints DISABLE TRIGGER USER;"

psql_access "UPDATE lifecycle_event_integrity SET entry_hash=decode(repeat('00',32),'hex') WHERE sequence=1;"
expect_lifecycle_failure "entry hash"
psql_access "TRUNCATE lifecycle_event_integrity; INSERT INTO lifecycle_event_integrity SELECT * FROM lifecycle_event_integrity_smoke_backup;"

psql_access "DELETE FROM lifecycle_event_integrity WHERE sequence=1;"
expect_lifecycle_failure "missing integrity row"
psql_access "TRUNCATE lifecycle_event_integrity; INSERT INTO lifecycle_event_integrity SELECT * FROM lifecycle_event_integrity_smoke_backup;"

psql_access "UPDATE lifecycle_heads SET last_hash=decode(repeat('11',32),'hex');"
expect_lifecycle_failure "chain head"
psql_access "TRUNCATE lifecycle_heads; INSERT INTO lifecycle_heads SELECT * FROM lifecycle_heads_smoke_backup;"

psql_access "UPDATE lifecycle_heads SET key_id='access-lifecycle/attacker';"
expect_lifecycle_failure "unknown signing key"
psql_access "TRUNCATE lifecycle_heads; INSERT INTO lifecycle_heads SELECT * FROM lifecycle_heads_smoke_backup;"

# Restored by UPDATE, not TRUNCATE: lifecycle_head_changes carries a foreign key
# to this table, so TRUNCATE ... CASCADE would take the checkpoint's own leaves
# with it and "restore" into an empty trail that verifies for the wrong reason.
psql_access "UPDATE lifecycle_checkpoints SET root=decode(repeat('22',32),'hex');"
expect_lifecycle_failure "checkpoint root"
psql_access "UPDATE lifecycle_checkpoints c SET root=b.root FROM lifecycle_checkpoints_smoke_backup b WHERE c.checkpoint_id=b.checkpoint_id;"

psql_access "ALTER TABLE lifecycle_event_integrity ENABLE TRIGGER USER;
            ALTER TABLE lifecycle_heads ENABLE TRIGGER USER;
            ALTER TABLE lifecycle_checkpoints ENABLE TRIGGER USER;
            DROP TABLE lifecycle_event_integrity_smoke_backup;
            DROP TABLE lifecycle_heads_smoke_backup;
            DROP TABLE lifecycle_checkpoints_smoke_backup;"
compose exec -T access /app verify-lifecycle
