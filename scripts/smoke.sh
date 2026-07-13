#!/usr/bin/env bash
# Smoke stage lifecycle: isolated compose project (own name + shifted ports),
# trap-based teardown, logs dumped on startup failure.
# Default: host-built artifacts via compose.smoke.yaml (fast path; binaries
# from `make build-gate-linux`, scanner dist from `make build-ts`).
# SMOKE_HERMETIC=1: original in-Docker builds only (compose.yaml), used by
# the hermetic-smoke workflow and available locally.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROJECT="${SMOKE_COMPOSE_PROJECT:-ticketing-smoke}"
export GATEWAY_PORT=18080 POSTGRES_PORT=15432 NATS_PORT=14222 \
       GRAFANA_PORT=13000 PROM_PORT=19090 OTLP_PORT=14318

COMPOSE_FILES=(-f "$ROOT/compose.yaml")
if [ "${SMOKE_HERMETIC:-0}" != "1" ]; then
  for b in catalog inventory commerce payments access gateway; do
    [ -x "$ROOT/bin/gate/$b" ] || { echo "smoke: missing bin/gate/$b — run 'make smoke' (or 'make build-gate-linux')" >&2; exit 1; }
  done
  [ -f "$ROOT/web/scanner/dist/index.html" ] || { echo "smoke: missing web/scanner/dist — run 'make build-ts'" >&2; exit 1; }
  [ -f "$ROOT/web/storefront/dist/server/entry.mjs" ] || { echo "smoke: missing web/storefront/dist — run 'make build-ts'" >&2; exit 1; }
  COMPOSE_FILES+=(-f "$ROOT/compose.smoke.yaml")
fi

compose() { docker compose -p "$PROJECT" "${COMPOSE_FILES[@]}" "$@"; }
cleanup() { compose down -v --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT INT TERM

if ! compose up -d --build --wait; then
  echo "--- compose up failed; recent logs: ---"
  compose logs --tail 50
  exit 1
fi

cd "$ROOT/smoke"
SMOKE_GATEWAY_URL=http://localhost:18080 \
SMOKE_NATS_URL=nats://localhost:14222 \
SMOKE_PG=localhost:15432 \
SMOKE_PROM_URL=http://localhost:19090 \
SMOKE_COMPOSE_PROJECT="$PROJECT" \
go test -tags smoke -count=1 -v ./...

# ADR-003: verify the populated canonical journal before Compose teardown.
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
