#!/usr/bin/env bash
# Smoke stage lifecycle: isolated compose project (own name + shifted ports),
# trap-based teardown, logs dumped on startup failure.
# Default: host-built artifacts via compose.smoke.yaml (fast path; binaries
# from `make build-gate-linux`, scanner dist from `make build-ts`).
# SMOKE_HERMETIC=1: original in-Docker builds only (compose.yaml), used by
# the hermetic-smoke workflow and available locally.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# Project name, ports and the four service credentials, isolated per checkout.
# Shared with scripts/browser.sh so the two stacks cannot pick the same ports —
# the browser gate's first, per-ticket copy of these lines hardcoded 18099,
# which is this stack's own gateway port at slot 19 (TKT-228). The reasoning
# behind each half of the isolation lives in that file.
. "$ROOT/scripts/stack-env.sh" smoke

# The load-proof override (pg_stat_statements preload, TKT-82) applies to every
# smoke stack — hermetic or fast path — so the gate's on-sale profile can always
# report the claim_history INSERT overhead.
# compose.direct-ports.yaml republishes what compose.yaml stopped publishing
# (ai-review S11): this suite drives the staff and internal surfaces directly,
# and connects to PostgreSQL, NATS and Prometheus from the host.
COMPOSE_FILES=(-f "$ROOT/compose.yaml" -f "$ROOT/compose.direct-ports.yaml" -f "$ROOT/compose.onsale-load.yaml")
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
# A SECOND database, for ./internal/mailer (TKT-226). The reason payments needed one for
# ./internal/api applies here and is now load-bearing rather than theoretical:
# `go test ./internal/...` runs packages CONCURRENTLY, and ./internal/mailer is the second
# package that calls store.Migrate. Two goose runs against one database race, and the
# loser dies on "column ... already exists" mid-migration. Sharing would also let the
# mailer's drainer — which claims the whole claimable set, not rows it seeded — retire
# mail_outbox rows a store test just enqueued.
docker exec "$(compose ps -q postgres)" psql -U postgres -v ON_ERROR_STOP=1 \
  -c "DROP DATABASE IF EXISTS commerce_mailer_smoke" \
  -c "CREATE DATABASE commerce_mailer_smoke OWNER commerce" >/dev/null
# A THIRD database, for ./internal/bulkrefund (TKT-198). The block above fixed the mailer
# and did NOT generalize: ./internal/bulkrefund (TKT-159) was already the third package
# calling store.Migrate and kept sharing commerce_store_smoke, so the two raced on every
# gate run — a different ~30-test subset failing each time on "relation ... already
# exists", which reads as flakiness rather than as one cause. The package-local sync.Once
# in each helper serializes nothing: these are separate, concurrent test binaries.
# TestCommerceSmokeDatabasesAreIsolated now fails if a fourth package starts sharing.
docker exec "$(compose ps -q postgres)" psql -U postgres -v ON_ERROR_STOP=1 \
  -c "DROP DATABASE IF EXISTS commerce_bulkrefund_smoke" \
  -c "CREATE DATABASE commerce_bulkrefund_smoke OWNER commerce" >/dev/null
# A FOURTH database, for ./internal/api (TKT-167). Commerce's first DB-backed test under
# internal/api — the exchange-resume suite, which needs a handler AND a database at once.
# The rule the three blocks above learned the hard way applies unchanged: `go test
# ./internal/...` runs packages concurrently, so a fourth store.Migrate caller sharing
# commerce_store_smoke races the others' migrations. It also shares no TABLE state: the
# resume tests settle exchanges, and ./internal/store has a test that counts
# `completion_outbox` rows for subject `order.exchanged` across the whole table — sharing
# a database made that count 6 instead of 1, which reads as a product defect in the
# outbox's deterministic id rather than as two suites in one schema.
docker exec "$(compose ps -q postgres)" psql -U postgres -v ON_ERROR_STOP=1 \
  -c "DROP DATABASE IF EXISTS commerce_api_smoke" \
  -c "CREATE DATABASE commerce_api_smoke OWNER commerce" >/dev/null
# No -run filter: every smoke test in this package is part of the gate. An allowlist
# means a newly added test silently never runs and the gate still passes green — which
# is exactly what happened to this file's first six tests.
# ./internal/... rather than ./internal/store, for the same reason inventory uses it
# (TKT-80): a DB-backed test added under ./internal/api would otherwise silently never
# run while the gate stayed green — the allowlist defect this file already records three
# times. Nothing lives there for commerce yet; the widening closes the hole before it is
# dug (TKT-156).
COMMERCE_TEST_DATABASE_URL="postgres://commerce:${COMMERCE_DB_PASSWORD}@localhost:${POSTGRES_PORT}/commerce_store_smoke" \
COMMERCE_BULKREFUND_TEST_DATABASE_URL="postgres://commerce:${COMMERCE_DB_PASSWORD}@localhost:${POSTGRES_PORT}/commerce_bulkrefund_smoke" \
COMMERCE_MAILER_TEST_DATABASE_URL="postgres://commerce:${COMMERCE_DB_PASSWORD}@localhost:${POSTGRES_PORT}/commerce_mailer_smoke" \
COMMERCE_MIGRATION_TEST_DATABASE_URL="postgres://postgres:${POSTGRES_PASSWORD}@localhost:${POSTGRES_PORT}/postgres" \
COMMERCE_API_TEST_DATABASE_URL="postgres://commerce:${COMMERCE_DB_PASSWORD}@localhost:${POSTGRES_PORT}/commerce_api_smoke" \
go test -tags smoke -count=1 ./internal/...

cd "$ROOT/services/access"
# No -run filter, for the same reason as commerce and catalog above: an allowlist
# means a newly added test silently never runs while the gate still passes green.
# This block carried one until TKT-67, and it was the last one left — lines 48
# and 59 above had already recorded the defect twice.
#
# ./internal/... rather than ./internal/store (TKT-162). The narrower path was the
# SAME defect wearing a different shape: an allowlist by package instead of by
# test name. services/access/internal/api has carried smoke tests since TKT-105
# and not one of them has ever run here — three were failing on main when this was
# found, invisibly, because the gate never executed the package. TKT-162 added
# more api-tier tests, which would have been dark on arrival.
ACCESS_MIGRATION_TEST_DATABASE_URL="postgres://postgres:${POSTGRES_PASSWORD}@localhost:${POSTGRES_PORT}/postgres" \
go test -tags smoke -count=1 ./internal/...

cd "$ROOT/services/catalog"
# No -run filter, for the same reason as commerce above: an allowlist means a newly
# added test silently never runs while the gate still passes green. This block used to
# carry one, and it had already stranded two tests that never ran once merged —
# TestDirectArchiveRacingFestivalPublishCannotDesync and
# TestGetPublishedFestivalOrdersDaysAcrossEventsChronologically (TKT-53's scoped-read test).
#
# ./internal/... rather than ./internal/store, and this one closes the hole BEFORE it
# is dug rather than after. Catalog's smoke tests all live under ./internal/store
# today, so this widening changes nothing right now — which is the point. Access
# carried the narrow path while its ./internal/api tests accumulated, and three of
# them were failing on main for weeks because the gate never executed the package
# (TKT-162). A package allowlist is the same defect as a test-name allowlist, and it
# is invisible in exactly the same way: the gate stays green because it never looked.
CATALOG_MIGRATION_TEST_DATABASE_URL="postgres://postgres:${POSTGRES_PASSWORD}@localhost:${POSTGRES_PORT}/postgres" \
go test -tags smoke -count=1 ./internal/...

cd "$ROOT/services/inventory"
# No -run filter, for the same reason as commerce, catalog and access above: an allowlist
# means a newly added test silently never runs while the gate still passes green. This
# block was the last one carrying one (TKT-77 removed it).
# ./internal/... (not just ./internal/store): the seat-hold handler's DB-backed smoke
# tests live in ./internal/api (TKT-80), and scoping to ./internal/store would silently
# skip them — the exact allowlist defect the notes above warn about.
INVENTORY_MIGRATION_TEST_DATABASE_URL="postgres://postgres:${POSTGRES_PASSWORD}@localhost:${POSTGRES_PORT}/postgres" \
go test -tags smoke -count=1 ./internal/...

cd "$ROOT/services/payments"
# The journal fault-injection matrix (TKT-56 Slice 4). Its own database, for the same
# reason commerce has one and then some: the packaged-binary checks further down
# deliberately CORRUPT the live payments journal, so sharing it would make these tests
# race a fixture that is being broken on purpose.
# ./internal/... rather than ./internal/store: package-scoping strands a newly added
# test exactly the way a -run filter does, which is the defect the four blocks above
# each record.
docker exec "$(compose ps -q postgres)" psql -U postgres -v ON_ERROR_STOP=1 \
  -c "DROP DATABASE IF EXISTS payments_store_smoke" \
  -c "CREATE DATABASE payments_store_smoke OWNER payments" >/dev/null
# ./internal/api gets its OWN database (TKT-156). Two reasons, either sufficient:
# Journal.Verify scans the WHOLE journal_entries table, so the store suite can only
# verify a database in which every entry is signed by a key its ring knows — an
# api-package append under any other key fails seven unrelated store tests. And
# `go test ./internal/...` runs packages CONCURRENTLY, so an api-package append can
# land mid-scan of a store test's whole-table verification.
docker exec "$(compose ps -q postgres)" psql -U postgres -v ON_ERROR_STOP=1 \
  -c "DROP DATABASE IF EXISTS payments_api_smoke" \
  -c "CREATE DATABASE payments_api_smoke OWNER payments" >/dev/null
# A THIRD database, migrated only part-way on purpose (TKT-217): the legacy-capture
# backfill test stops at 0003, writes a capture under the pre-ledger schema, then
# applies 0004. It cannot share a database with suites that expect a fully migrated one.
docker exec "$(compose ps -q postgres)" psql -U postgres -v ON_ERROR_STOP=1 \
  -c "DROP DATABASE IF EXISTS payments_legacy_smoke" \
  -c "CREATE DATABASE payments_legacy_smoke OWNER payments" \
  -c "DROP DATABASE IF EXISTS payments_legacy_malformed_smoke" \
  -c "CREATE DATABASE payments_legacy_malformed_smoke OWNER payments" >/dev/null
# Two MORE, for the same reason and by the same rule (TKT-257): the migration-0006 tests
# stand the schema up at 0005, seed rows in the pre-0006 shape, then apply 0006 and check
# what it did to them — and the down test rolls the schema BACK. Neither can share a
# database with suites that expect a fully migrated one. They are provisioned here, as
# superuser, rather than created by the tests: the `payments` role has no CREATEDB, which
# is how the first attempt failed in CI while passing every local check.
docker exec "$(compose ps -q postgres)" psql -U postgres -v ON_ERROR_STOP=1 \
  -c "DROP DATABASE IF EXISTS payments_migration_smoke" \
  -c "CREATE DATABASE payments_migration_smoke OWNER payments" \
  -c "DROP DATABASE IF EXISTS payments_migration_down_smoke" \
  -c "CREATE DATABASE payments_migration_down_smoke OWNER payments" \
  -c "DROP DATABASE IF EXISTS payments_compensation_smoke" \
  -c "CREATE DATABASE payments_compensation_smoke OWNER payments" >/dev/null
PAYMENTS_MIGRATION_TEST_DATABASE_URL="postgres://payments:${PAYMENTS_DB_PASSWORD}@localhost:${POSTGRES_PORT}/payments_migration_smoke" \
PAYMENTS_MIGRATION_DOWN_TEST_DATABASE_URL="postgres://payments:${PAYMENTS_DB_PASSWORD}@localhost:${POSTGRES_PORT}/payments_migration_down_smoke" \
PAYMENTS_COMPENSATION_TEST_DATABASE_URL="postgres://payments:${PAYMENTS_DB_PASSWORD}@localhost:${POSTGRES_PORT}/payments_compensation_smoke" \
PAYMENTS_LEGACY_TEST_DATABASE_URL="postgres://payments:${PAYMENTS_DB_PASSWORD}@localhost:${POSTGRES_PORT}/payments_legacy_smoke" \
PAYMENTS_LEGACY_MALFORMED_TEST_DATABASE_URL="postgres://payments:${PAYMENTS_DB_PASSWORD}@localhost:${POSTGRES_PORT}/payments_legacy_malformed_smoke" \
PAYMENTS_TEST_DATABASE_URL="postgres://payments:${PAYMENTS_DB_PASSWORD}@localhost:${POSTGRES_PORT}/payments_store_smoke" \
PAYMENTS_API_TEST_DATABASE_URL="postgres://payments:${PAYMENTS_DB_PASSWORD}@localhost:${POSTGRES_PORT}/payments_api_smoke" \
go test -tags smoke -count=1 ./internal/...

cd "$ROOT"
# ADR-016 §Decision 8 / COS C3: the PACKAGED binary verifies a journal whose entries
# span TWO signing keys. The rotation test above leaves exactly such a chain behind in
# payments_store_smoke; this runs the real CLI over it with v2 active and v1 retired.
# Without this, "verify-journal verifies a multi-key journal" would be an argument about
# a library call — the gate's own journal is signed under one key forever, and
# verify-concurrent-append cannot append under a second (its fact id is a literal, so a
# re-run replays 8/8 and fails its own new==1 guard).
# Guard the fixture both checks below depend on. The mixed-key chain is left behind by
# TestJournalRotationKeepsHistoryVerifiable; if that test is ever renamed, skipped or given
# a cleanup, the positive check still passes — a single-key journal verifies fine under a
# superset ring — and the negative one then fails with a retired-key message that
# misdiagnoses the real cause. Assert the fixture directly instead.
mixed_kids=$(docker exec "$(compose ps -q postgres)" psql -U postgres -d payments_store_smoke -tAc \
  "SELECT count(DISTINCT key_id) FROM journal_entries")
# Validate before comparing. Inside an `if` condition set -e is suspended, and
# `[ "$x" -lt 2 ]` on empty or non-numeric input prints an error and evaluates FALSE — so
# a broken psql invocation would sail past a guard whose whole purpose is to fail closed.
case "$mixed_kids" in
  ''|*[!0-9]*) echo "smoke: could not count key ids in payments_store_smoke (got: '$mixed_kids')" >&2; exit 1 ;;
esac
if [ "$mixed_kids" -lt 2 ]; then
  echo "smoke: payments_store_smoke holds $mixed_kids key id(s); the rotation test's mixed-key fixture is missing" >&2
  exit 1
fi
# These four literals are OWNED by services/payments/internal/store/journal_smoke_test.go
# (smokeKIDv1/smokeKIDv2/smokeKeyv1/smokeKeyv2) — the rotation test there writes the
# mixed-kid journal this verifies. TestSmokeJournalKeyLiteralsMatchScript reads this file
# and fails if the two sets drift, so edit both or neither (TKT-117).
compose exec -T \
  -e DATABASE_URL=postgres://payments:${PAYMENTS_DB_PASSWORD}@postgres:5432/payments_store_smoke \
  -e JOURNAL_KEY_ID=smoke-v2 \
  -e JOURNAL_SIGNING_KEY=smoke-journal-key-v2-0123456789 \
  -e JOURNAL_HISTORICAL_KEYS=smoke-v1=c21va2Utam91cm5hbC1rZXktdjEtMDEyMzQ1Njc4OQ \
  payments /app verify-journal
# ...and the retirement consequence is real: the same journal with v1 dropped from the
# ring must FAIL, or "an unknown key id is a verification failure" is untested at the
# CLI where operators actually meet it.
#
# Assert the DIAGNOSTIC, not merely a non-zero exit. "It failed" is satisfied by a typo'd
# flag, an unreachable database, a missing binary, or an unrelated pre-existing
# corruption — every one of which would let this check pass while proving nothing about
# retirement. Only the unknown-key message proves the retired key is why.
retired_out=$(compose exec -T \
  -e DATABASE_URL=postgres://payments:${PAYMENTS_DB_PASSWORD}@postgres:5432/payments_store_smoke \
  -e JOURNAL_KEY_ID=smoke-v2 \
  -e JOURNAL_SIGNING_KEY=smoke-journal-key-v2-0123456789 \
  payments /app verify-journal 2>&1) && {
  echo "smoke: verify-journal accepted a journal signed under a retired key" >&2
  exit 1
}
case "$retired_out" in
  *'unknown key id "smoke-v1"'*) ;;
  *) echo "smoke: verify-journal failed, but not because of the retired key: $retired_out" >&2
     exit 1 ;;
esac

# TKT-190: provision the back-office staff account the sign-in smoke tests use,
# through the REAL CLI against the REAL migrated schema — the same path an
# operator follows. The password is generated per run and never written to the
# log or to a file; it reaches the CLI on stdin (never argv, which would put it
# in the container's process table) and the test process through the environment.
# TKT-197: one account per role, so the smoke suite can prove the matrix REFUSES
# rather than only that it admits. A single admin account can only ever show the
# allow path, which is the half that fails safe.
SMOKE_STAFF_IDENTIFIER="smoke-staff@example.test"
SMOKE_STAFF_PASSWORD=$(od -An -tx1 -N24 /dev/urandom | tr -d ' \n')
SMOKE_BOXOFFICE_IDENTIFIER="smoke-boxoffice@example.test"
SMOKE_BOXOFFICE_PASSWORD=$(od -An -tx1 -N24 /dev/urandom | tr -d ' \n')
SMOKE_FINANCE_IDENTIFIER="smoke-finance@example.test"
SMOKE_FINANCE_PASSWORD=$(od -An -tx1 -N24 /dev/urandom | tr -d ' \n')
export SMOKE_STAFF_IDENTIFIER SMOKE_STAFF_PASSWORD \
       SMOKE_BOXOFFICE_IDENTIFIER SMOKE_BOXOFFICE_PASSWORD \
       SMOKE_FINANCE_IDENTIFIER SMOKE_FINANCE_PASSWORD
provision_staff() { # identifier password role
  printf '%s' "$2" | compose exec -T catalog /app provision-staff \
    --organizer-id 00000000-0000-0000-0000-000000000001 \
    --identifier "$1" --role "$3" >/dev/null
}
provision_staff "$SMOKE_STAFF_IDENTIFIER"     "$SMOKE_STAFF_PASSWORD"     admin
provision_staff "$SMOKE_BOXOFFICE_IDENTIFIER" "$SMOKE_BOXOFFICE_PASSWORD" box_office
provision_staff "$SMOKE_FINANCE_IDENTIFIER"   "$SMOKE_FINANCE_PASSWORD"   finance

# ai-review S1: the scan routes admit only ENROLLED devices, so the suite pairs
# one gate per run exactly as an operator would — through the CLI, reading the
# token off its output. It is printed once and never recoverable, which is the
# property being exercised as much as it is a constraint on this script.
SMOKE_SCANNER_TOKEN=$(compose exec -T access /app enrol-scanner \
  00000000-0000-0000-0000-000000000001 "smoke gate" \
  | sed -n 's/^  X-Scanner-Token: //p' | tr -d '\r')
if [ -z "$SMOKE_SCANNER_TOKEN" ]; then
  echo "smoke: could not enrol a scanner device — every scan would 401" >&2
  exit 1
fi
export SMOKE_SCANNER_TOKEN

# TKT-240: enrol one reseller through the REAL CLI, the same path an operator
# follows. The token is printed once on stdout (everything else goes to stderr)
# and is not recoverable, which is the property being exercised as much as it is
# a constraint on this script. The channel matches the allocation the partner
# smoke tests create.
SMOKE_PARTNER_RESELLER_ID="00000000-0000-0000-0000-000000000240"
SMOKE_PARTNER_CHANNEL="reseller-smoke"
SMOKE_PARTNER_TOKEN=$(compose exec -T commerce /app enrol-reseller \
  00000000-0000-0000-0000-000000000001 "$SMOKE_PARTNER_RESELLER_ID" \
  "$SMOKE_PARTNER_CHANNEL" "smoke reseller" 2>/dev/null | tr -d '\r')
if [ -z "$SMOKE_PARTNER_TOKEN" ]; then
  echo "smoke: could not enrol a reseller credential — every partner call would 401" >&2
  exit 1
fi
export SMOKE_PARTNER_TOKEN SMOKE_PARTNER_CHANNEL SMOKE_PARTNER_RESELLER_ID

cd "$ROOT/smoke"
SMOKE_STAFF_IDENTIFIER="$SMOKE_STAFF_IDENTIFIER" \
SMOKE_STAFF_PASSWORD="$SMOKE_STAFF_PASSWORD" \
SMOKE_BOXOFFICE_IDENTIFIER="$SMOKE_BOXOFFICE_IDENTIFIER" \
SMOKE_BOXOFFICE_PASSWORD="$SMOKE_BOXOFFICE_PASSWORD" \
SMOKE_FINANCE_IDENTIFIER="$SMOKE_FINANCE_IDENTIFIER" \
SMOKE_FINANCE_PASSWORD="$SMOKE_FINANCE_PASSWORD" \
SMOKE_CATALOG_STAFF_WRITE_TOKEN="$SMOKE_CATALOG_STAFF_WRITE_TOKEN" \
SMOKE_CATALOG_ORGANIZER_ASSERTION_KEY="$SMOKE_CATALOG_ORGANIZER_ASSERTION_KEY" \
SMOKE_COMMERCE_STAFF_WRITE_TOKEN="$SMOKE_COMMERCE_STAFF_WRITE_TOKEN" \
SMOKE_INVENTORY_STAFF_WRITE_TOKEN="$SMOKE_INVENTORY_STAFF_WRITE_TOKEN" \
SMOKE_COMMERCE_CUSTOMER_ASSERTION_KEY="$SMOKE_COMMERCE_CUSTOMER_ASSERTION_KEY" \
SMOKE_ACCESS_QR_SEED="$SMOKE_ACCESS_QR_SEED" \
SMOKE_SCANNER_TOKEN="$SMOKE_SCANNER_TOKEN" \
SMOKE_PARTNER_TOKEN="$SMOKE_PARTNER_TOKEN" \
SMOKE_PARTNER_CHANNEL="$SMOKE_PARTNER_CHANNEL" \
SMOKE_PARTNER_RESELLER_ID="$SMOKE_PARTNER_RESELLER_ID" \
SMOKE_PAYMENTS_INTERNAL_TOKEN="$SMOKE_PAYMENTS_INTERNAL_TOKEN" \
SMOKE_GATEWAY_URL=http://localhost:${GATEWAY_PORT} \
SMOKE_NATS_URL=nats://localhost:${NATS_PORT} \
SMOKE_PG=localhost:${POSTGRES_PORT} \
SMOKE_PROM_URL=http://localhost:${PROM_PORT} \
SMOKE_CATALOG_URL=http://localhost:${CATALOG_PORT} \
 SMOKE_INVENTORY_URL=http://localhost:${INVENTORY_PORT} \
SMOKE_COMMERCE_URL=http://localhost:${COMMERCE_PORT} \
SMOKE_PAYMENTS_URL=http://localhost:${PAYMENTS_PORT} \
SMOKE_ACCESS_URL=http://localhost:${ACCESS_PORT} \
SMOKE_COMPOSE_PROJECT="$PROJECT" \
go test -tags smoke -count=1 -v -timeout "${SMOKE_TEST_TIMEOUT:-10m}" ./...

# QUIESCE THE ONLY LIVE WRITER BEFORE ANY JOURNAL VERIFICATION (TKT-254).
#
# Everything from here to the final verify-journal below reads, corrupts and RESTORES
# the payments journal. All of it assumed nothing else was writing. Something was.
#
# commerce runs background tickers -- recovery (internal/recovery/runner.go), bulk
# refunds, reversals, the exchange sweep -- and the recovery runner's OrderFailed POSTs
# payments /internal/facts, which appends to this journal for the seeded organizer. It
# ticks every RECOVERY_INTERVAL, which compose.smoke.yaml pins at 2s for this stack, and
# it always has work: smoke/psp_recovery_test.go deliberately backdates orders past the
# claim grace period so the runner picks them up.
#
# The failure that follows is not a race the verifier loses and re-wins on a retry -- it
# is DURABLE CORRUPTION. The restores below snapshot journal_entries, then reinstate it
# with DELETE + INSERT ... SELECT several statements later. An append that commits inside
# that window is not in the snapshot, so the DELETE removes it and the INSERT does not
# put it back -- while its journal_heads update survives. Every later append then chains
# onto a row that no longer exists, and verify-journal reports a sequence gap about a
# journal the writer never got wrong. That is TKT-254: `broken chain organizer=...0001
# sequence=167` on a catalog-only change, green on the next run.
#
# `stop`, not `pause` or `scale 0`: only stop drains accepted HTTP requests before
# cancelling the workers (services/commerce/cmd/commerce/main.go -- srv.Shutdown, then
# stopWorkers). -t 45 covers the 10s HTTP drain plus each worker's 5s grace. The backup
# below is taken only AFTER this returns, because a worker inside its grace window can
# still append.
#
# It is never restarted. Nothing after this line uses commerce, and a restart would be
# actively harmful: Runner.Run calls RunOnce BEFORE its first tick, so restarting near
# the restore block appends immediately. If you add a commerce-dependent check below,
# it goes ABOVE this line -- not after a restart.
#
# What this claims, precisely: the producer is stopped AND payments is observed to have no
# busy client backend on the journal's database before the window opens. Two conditions,
# neither sufficient alone -- and NOT a proof that no append can occur. The drain barrier
# below states exactly what it does and does not cover; read it before trusting either.
compose stop -t 45 commerce

# ...and then WAIT FOR PAYMENTS TO DRAIN, which stopping commerce does not prove
# (ai-review pass 1, [medium]). `compose stop` closes the door on NEW calls; it cannot
# recall one payments has already accepted. A worker that sent /internal/facts and was
# then cancelled leaves payments committing an append after `stop` has returned — and an
# append that commits after the backup below is exactly the row the restore deletes and
# never puts back. The barrier has to be on the PAYMENTS side, not the caller's.
#
# `state IS DISTINCT FROM 'idle'`, NOT `state <> 'idle'` (ai-review pass 2). A pooled
# connection sitting idle holds no statement and cannot append; anything else (active,
# idle in transaction) is work that can still reach journal_entries. But `state` is
# NULLABLE -- a backend whose state has not been published yet, or any backend at all if
# track_activities is ever turned off -- and `NULL <> 'idle'` evaluates to NULL, which
# WHERE discards. The plain inequality therefore counts an unknown backend as drained,
# which is backwards for a guard that exists to fail closed. Verified rather than
# reasoned: `SELECT (NULL <> 'idle') IS NULL` is true, `SELECT (NULL IS DISTINCT FROM
# 'idle')` is true.
#
# backend_type='client backend' is the other half, and it is about FALSE POSITIVES rather
# than false negatives (ai-review pass 3). Most server processes carry datname NULL and are
# already excluded -- verified: checkpointer, walwriter, background writer, io worker and
# both launchers all report NULL. An AUTOVACUUM WORKER does not: it runs with datname set to
# the database it is vacuuming and a non-idle state, so it would be counted as a journal
# writer. It cannot append a fact, it has no completion bound, and a wraparound vacuum
# resists interruption -- so counting it means unrelated maintenance can hold this loop past
# its timeout and fail the whole smoke stage. A barrier that fails the gate for a reason
# that has nothing to do with the journal is worse than the flake it replaces.
#
# pid <> pg_backend_pid() excludes this very query. datname='payments' scopes it to the
# live journal's database, not the isolated *_smoke ones the store suites use --
# pg_stat_activity is cluster-wide, so the filter is doing real work here.
#
# 30 × 1s, then fail loudly rather than proceeding: a barrier that gives up quietly and
# lets the corruption block open anyway is worse than no barrier, because the gate then
# reports the resulting sequence gap as an integrity failure. If this ever trips, the
# thing to investigate is what is still writing, not this timeout.
#
# THE RESIDUAL, AND WHAT IS *NOT* BOUNDED. Stated plainly because an earlier version of this
# comment claimed a bound that does not exist, and a false reassurance in a comment is worse
# than an admitted gap (ai-review pass 3, [high]).
#
# A handler payments accepted but that has not yet reached BeginTx is invisible to this poll:
# it holds no backend, so a zero sample does not prove it is gone.
#
# The retracted claim, so nobody re-derives it: commerce's shutdown does NOT bound this.
# `srv.Shutdown` drains commerce's INBOUND handlers, and with no inbound traffic it returns
# at once rather than after its 10s budget; the workers run on independent contexts and are
# cancelled only afterwards by stopWorkers (services/commerce/cmd/commerce/main.go). Worse,
# cancelling a worker aborts the CLIENT side of a request payments has already accepted --
# it does not stop the server handler. So the ordering bounds inbound drain, not the
# outbound call that appends to the journal.
#
# What IS true, and is the honest basis for shipping this:
#   - commerce is the ONLY caller of payments in this repository -- four call sites, all
#     under services/commerce (the one smoke test that POSTs /api/payments/internal/facts is
#     refused at the gateway edge and never reaches payments). Stopping it removes every
#     producer.
#   - stopWorkers cancels each worker and BLOCKS up to 5s for it to exit, so a worker cannot
#     still be starting new requests once `compose stop` has returned.
#   - the poll then requires payments to show no busy client backend.
# The uncovered case is narrow and specific: a request accepted by payments in the instant
# before its producer was cancelled, still ahead of BeginTx when the poll samples zero.
#
# Closing even that means quiescing payments itself, and payments cannot be stopped here --
# it is the container the four verify-journal calls below `compose exec` into. Converting
# those to one-off containers would change what they prove (a fresh container is not the
# running service, which is the point of the ADR-016 §D8 multi-key check). That is a
# redesign, not a fix, and it is TKT-261's shape of problem rather than this ticket's.
#
# If a journal flake recurs here despite the stop and the poll, THIS is the gap to suspect
# first.
payments_drained=0
for _ in $(seq 1 30); do
  busy=$(docker exec "$(compose ps -q postgres)" psql -U postgres -tAc \
    "SELECT count(*) FROM pg_stat_activity WHERE datname='payments' AND backend_type='client backend' AND state IS DISTINCT FROM 'idle' AND pid <> pg_backend_pid()")
  case "$busy" in
    ''|*[!0-9]*) echo "smoke: could not count active payments backends (got: '$busy')" >&2; exit 1 ;;
  esac
  if [ "$busy" -eq 0 ]; then payments_drained=1; break; fi
  sleep 1
done
if [ "$payments_drained" -ne 1 ]; then
  echo "smoke: payments still had active backends 30s after commerce stopped; refusing to open the journal corruption window" >&2
  exit 1
fi

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
# TKT-217: settlement_entries carries a foreign key to journal_entries, so the
# restores below use DELETE rather than TRUNCATE -- TRUNCATE is refused outright
# on a referenced table, and CASCADE would take the settlement ledger with it
# (and be refused in turn by its own append-only trigger). DISABLE TRIGGER ALL,
# not USER: the append-only triggers are user triggers, but the referential
# integrity that makes the DELETE illegal is enforced by system triggers.
psql_payments "CREATE TABLE journal_entries_smoke_backup AS TABLE journal_entries; CREATE TABLE journal_heads_smoke_backup AS TABLE journal_heads; ALTER TABLE journal_entries DISABLE TRIGGER ALL;"

psql_payments "UPDATE journal_entries SET entry_hash=decode(repeat('00',32),'hex') WHERE fact_id=(SELECT fact_id FROM journal_entries ORDER BY organizer_id,sequence LIMIT 1);"
expect_verify_failure "hash"
psql_payments "DELETE FROM journal_entries; INSERT INTO journal_entries SELECT * FROM journal_entries_smoke_backup;"

psql_payments "DELETE FROM journal_entries WHERE fact_id=(SELECT fact_id FROM journal_entries ORDER BY organizer_id,sequence OFFSET 1 LIMIT 1);"
expect_verify_failure "sequence gap"
psql_payments "DELETE FROM journal_entries; INSERT INTO journal_entries SELECT * FROM journal_entries_smoke_backup;"

psql_payments "UPDATE journal_heads SET last_hash=decode(repeat('00',32),'hex');"
expect_verify_failure "chain head"
psql_payments "TRUNCATE journal_heads; INSERT INTO journal_heads SELECT * FROM journal_heads_smoke_backup; ALTER TABLE journal_entries ENABLE TRIGGER ALL; DROP TABLE journal_entries_smoke_backup; DROP TABLE journal_heads_smoke_backup;"
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

# ADR-031 / TKT-112: reclaim leaked seat pins end-to-end, through the real binary.
#
# What this block exists to prove is the WIRING, not the classification — the verdict logic is
# pinned by unit tests and by the two store suites. Only a real run can catch the class of
# failure those cannot see: the subcommand not being registered, the container missing
# CATALOG_URL or the internal token, the internal read not being mounted, or the unpin route
# refusing the call. That is the gap TKT-94 was filed for.
#
# The fixture is seeded by SQL (as the journal and lifecycle blocks above do) because the state
# being reconciled is by definition state nobody is touching any more: a hold that expired on a
# pool no request has come near since.
#
# -tAqc, not -tAc: without -q, psql prints the "INSERT 0 1" status tag after the RETURNING
# value, and capturing both yields a uuid with the tag glued onto it.
psql_inventory() { compose exec -T postgres psql -v ON_ERROR_STOP=1 -U postgres -d inventory -tAqc "$1"; }
psql_catalog() { compose exec -T postgres psql -v ON_ERROR_STOP=1 -U postgres -d catalog -tAqc "$1"; }

# Borrow a published seat map the Go suite already created, and one real identity from it. One
# seat is enough for every case: the pins differ by pinned_by, which is the whole point.
recon_map=$(psql_catalog "SELECT s.seat_map_id FROM seat_map_seats s JOIN seat_maps m ON m.id=s.seat_map_id
  WHERE m.status='published' ORDER BY s.seat_map_id, s.seat_identity LIMIT 1" | tr -d '[:space:]')
if [ -z "$recon_map" ]; then
  # Loud, not skipped: a silently skipped assertion is a green gate that proved nothing, which
  # is the exact defect the -run filters above were removed for.
  echo "smoke: no published seat map with seats to reconcile against" >&2
  exit 1
fi
recon_org=$(psql_catalog "SELECT organizer_id FROM seat_maps WHERE id='$recon_map'" | tr -d '[:space:]')
recon_family=$(psql_catalog "SELECT map_family_id FROM seat_maps WHERE id='$recon_map'" | tr -d '[:space:]')
recon_seat=$(psql_catalog "SELECT seat_identity FROM seat_map_seats WHERE seat_map_id='$recon_map' ORDER BY seat_identity LIMIT 1" | tr -d '[:space:]')

# Inventory side: one seated pool with two claims on the same identity — one terminal (its pin
# is garbage) and one confirmed (its pin guards a SOLD seat). Both can hold the same identity
# because claim_seats' uniqueness covers live rows only, and the terminal one is released.
recon_slot=$(psql_inventory "SELECT gen_random_uuid()" | tr -d '[:space:]')
psql_inventory "INSERT INTO inventory_pools(slot_id,organizer_id,capacity,source_event_id,inventory_kind,seat_map_id)
  VALUES('$recon_slot','$recon_org',100,gen_random_uuid(),'seated','$recon_map')" >/dev/null
# expires_at is NOT NULL for a buyer claim (claims_kind_shape) even when it is seeded already
# terminal, and idempotency_key/request_fingerprint are NOT NULL on every claim.
recon_dead_claim=$(psql_inventory "INSERT INTO claims(id,organizer_id,pool_id,quantity,status,expires_at,idempotency_key,request_fingerprint,claim_kind)
  VALUES(gen_random_uuid(),'$recon_org','$recon_slot',1,'expired',now()-interval '1 hour','recon-dead-$recon_slot','recon-dead-$recon_slot','buyer') RETURNING id" | tr -d '[:space:]')
psql_inventory "INSERT INTO claim_seats(claim_id,pool_id,seat_identity,released_at)
  VALUES('$recon_dead_claim','$recon_slot','$recon_seat',now())" >/dev/null
recon_live_claim=$(psql_inventory "INSERT INTO claims(id,organizer_id,pool_id,quantity,status,expires_at,idempotency_key,request_fingerprint,claim_kind)
  VALUES(gen_random_uuid(),'$recon_org','$recon_slot',1,'confirmed',now()+interval '1 hour','recon-live-$recon_slot','recon-live-$recon_slot','buyer') RETURNING id" | tr -d '[:space:]')
psql_inventory "INSERT INTO claim_seats(claim_id,pool_id,seat_identity)
  VALUES('$recon_live_claim','$recon_slot','$recon_seat')" >/dev/null

# Catalog side: the leaked pin, the sold seat's pin, a sale pin, and a pin naming a claim
# inventory has never seen. Only the first may be reclaimed.
recon_orphan_ref="hold:$(psql_inventory "SELECT gen_random_uuid()" | tr -d '[:space:]')"
psql_catalog "INSERT INTO seat_map_pins(organizer_id,map_family_id,seat_identity,pinned_by) VALUES
  ('$recon_org','$recon_family','$recon_seat','hold:$recon_dead_claim'),
  ('$recon_org','$recon_family','$recon_seat','hold:$recon_live_claim'),
  ('$recon_org','$recon_family','$recon_seat','sale:00000000-0000-0000-0000-0000000000ff'),
  ('$recon_org','$recon_family','$recon_seat','$recon_orphan_ref')" >/dev/null

compose exec -T inventory /app reconcile-pins

expect_pin() {
  local pinned_by="$1" want="$2" got
  got=$(psql_catalog "SELECT count(*) FROM seat_map_pins WHERE map_family_id='$recon_family' AND pinned_by='$pinned_by'" | tr -d '[:space:]')
  if [ "$got" != "$want" ]; then
    echo "smoke: reconcile-pins left $got pin(s) for $pinned_by, want $want" >&2
    exit 1
  fi
}
assert_recon_survivors() {
  expect_pin "hold:$recon_dead_claim" 0 # the leak this ticket exists to reclaim
  expect_pin "hold:$recon_live_claim" 1 # CONFIRMED means sold: removing this pin would let an
                                        # edit orphan a sold seat
  expect_pin "sale:00000000-0000-0000-0000-0000000000ff" 1 # not this command's namespace
  expect_pin "$recon_orphan_ref" 1      # unknown claim: reported, never reclaimed (fail safe)
}
assert_recon_survivors

# Idempotent: a second run reclaims nothing and still exits zero even though fail-safe residue
# (the unknown reference) is sitting right there.
compose exec -T inventory /app reconcile-pins
assert_recon_survivors

# Scoped to the four pins this block created. The family very likely also carries REAL pins
# from the Go suite's own seat holds, and a family-wide delete would take those with it.
psql_catalog "DELETE FROM seat_map_pins WHERE map_family_id='$recon_family' AND pinned_by IN
  ('hold:$recon_dead_claim','hold:$recon_live_claim','sale:00000000-0000-0000-0000-0000000000ff','$recon_orphan_ref')" >/dev/null
psql_inventory "DELETE FROM claim_seats WHERE pool_id='$recon_slot';
  DELETE FROM claim_history WHERE claim_id IN ('$recon_dead_claim','$recon_live_claim');
  DELETE FROM claims WHERE pool_id='$recon_slot';
  DELETE FROM inventory_pools WHERE slot_id='$recon_slot'" >/dev/null
