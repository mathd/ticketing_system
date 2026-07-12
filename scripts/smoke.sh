#!/usr/bin/env bash
# Smoke stage lifecycle: isolated compose project (own name + shifted ports),
# trap-based teardown, logs dumped on startup failure.
set -euo pipefail

PROJECT="${SMOKE_COMPOSE_PROJECT:-ticketing-smoke}"
export GATEWAY_PORT=18080 POSTGRES_PORT=15432 NATS_PORT=14222 \
       GRAFANA_PORT=13000 PROM_PORT=19090 OTLP_PORT=14318

compose() { docker compose -p "$PROJECT" "$@"; }
cleanup() { compose down -v --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT INT TERM

if ! compose up -d --build --wait; then
  echo "--- compose up failed; recent logs: ---"
  compose logs --tail 50
  exit 1
fi

cd smoke
SMOKE_GATEWAY_URL=http://localhost:18080 \
SMOKE_NATS_URL=nats://localhost:14222 \
SMOKE_PG=localhost:15432 \
SMOKE_PROM_URL=http://localhost:19090 \
SMOKE_COMPOSE_PROJECT="$PROJECT" \
go test -tags smoke -count=1 -v ./...
