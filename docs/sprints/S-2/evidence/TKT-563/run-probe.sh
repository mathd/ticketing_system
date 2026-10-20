#!/usr/bin/env bash
# TKT-563 combined verification probe runner. Report-only evidence.
#
#   run-probe.sh --variant candidate --repeat 8 --output <new-directory>
#   run-probe.sh --variant <COPY-EXIT|COPY-LIMIT|COUNT-REJECTED|AX01|AX02> --repeat 1 --output <new-directory>
#
# Exit codes: 0 PASS or DETECTED, 1 FAIL or NOT_DETECTED, 2 setup error inside
# the run, 3 refused before the stack started. The probe's exit code is returned
# unchanged. A diagnostic that was not detected never reads as a pass.
#
# The runner builds nothing and installs nothing. Run make deps, make
# build-gate-linux and make build-ts first, from the committed candidate.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# TKT-563, evidence, S-2, sprints and docs are five levels below the checkout root.
ROOT="$(cd "$HERE/../../../../.." && pwd)"
BASELINE="dbee66e17145d316e06a4d4e092d65483dc33db8"
EVIDENCE_PREFIX="docs/sprints/S-2/evidence/TKT-563/"
TIMING_PENDING="${TMPDIR:-/tmp}/tkt563-timing-guard/pending.env"

OUTPUT=""
OUTPUT_CREATED=0
PROBE_LOCK=""
LOCK_HELD=0
PROJECT=""
STACK_STARTED=0
DISPOSABLE=""
DIAG_DIST=""

die() {
  echo "run-probe: $*" >&2
  if [ "$OUTPUT_CREATED" = 1 ]; then echo "refused: $*" >> "$OUTPUT/refused.txt"; fi
  exit 3
}

usage() {
  echo "usage: run-probe.sh --variant <candidate|COPY-EXIT|COPY-LIMIT|COUNT-REJECTED|AX01|AX02> --repeat <8 for candidate, 1 otherwise> --output <new directory outside the checkout>" >&2
  exit 3
}

# Installed before any lock is taken, so every refusal path releases what it holds.
cleanup() {
  local rc=$?
  set +e
  if [ "$STACK_STARTED" = 1 ] && [ -n "$PROJECT" ]; then
    compose down -v --remove-orphans 2>&1 | node "$HERE/lib/redact.mjs" > "$OUTPUT/compose-down.log" || true
  fi
  if [ -n "$DISPOSABLE" ]; then rm -rf "$DISPOSABLE"; fi
  if [ "$LOCK_HELD" = 1 ]; then rm -rf "$PROBE_LOCK"; fi
  if [ "$OUTPUT_CREATED" = 1 ]; then
    printf 'runner_exit=%s\nfinished_utc=%s\n' "$rc" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$OUTPUT/runner-receipt.txt"
  fi
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

VARIANT=""
REPEAT=""
OUTPUT_ARG=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --variant) [ "$#" -ge 2 ] || usage; VARIANT="$2"; shift 2 ;;
    --repeat) [ "$#" -ge 2 ] || usage; REPEAT="$2"; shift 2 ;;
    --output) [ "$#" -ge 2 ] || usage; OUTPUT_ARG="$2"; shift 2 ;;
    *) usage ;;
  esac
done
case "$VARIANT" in
  candidate) [ "$REPEAT" = "8" ] || die "candidate takes --repeat 8" ;;
  COPY-EXIT|COPY-LIMIT|COUNT-REJECTED|AX01|AX02) [ "$REPEAT" = "1" ] || die "$VARIANT takes --repeat 1" ;;
  *) usage ;;
esac
[ -n "$OUTPUT_ARG" ] || usage

# A repeat must never inherit a mutation from the caller's environment.
if [ "${SCANNER_MUTATION+set}" = "set" ]; then
  die "SCANNER_MUTATION is set in the environment; refusing to run"
fi

# The output directory must be new and must sit outside the checkout.
case "$OUTPUT_ARG" in
  /*) OUTPUT="$OUTPUT_ARG" ;;
  *) OUTPUT="$PWD/$OUTPUT_ARG" ;;
esac
[ ! -e "$OUTPUT" ] || die "output already exists: $OUTPUT"
OUT_PARENT="$(dirname "$OUTPUT")"
[ -d "$OUT_PARENT" ] || die "output parent does not exist: $OUT_PARENT"
OUT_PARENT_REAL="$(cd "$OUT_PARENT" && pwd -P)"
ROOT_REAL="$(cd "$ROOT" && pwd -P)"
case "$OUT_PARENT_REAL/" in
  "$ROOT_REAL"/*) die "output must be outside the checkout: $OUTPUT" ;;
esac
mkdir "$OUTPUT" || die "could not create $OUTPUT"
OUTPUT_CREATED=1

# A timing restore that a killed run left behind must be recovered first.
[ ! -e "$TIMING_PENDING" ] || die "a timing-evidence restore is pending ($TIMING_PENDING); run run-make-browser.sh --recover-only first"

# One probe run at a time per checkout. The lock is outside the checkout.
ROOT_DIGEST="$(printf '%s' "$ROOT" | shasum -a 256 | cut -c1-16)"
PROBE_LOCK="${TMPDIR:-/tmp}/tkt563-probe-$ROOT_DIGEST.lock"
if ! mkdir "$PROBE_LOCK" 2>/dev/null; then
  die "another TKT-563 probe run holds $PROBE_LOCK; remove it only after confirming that no run is active"
fi
LOCK_HELD=1
echo "$$" > "$PROBE_LOCK/pid"

[ ! -e "$ROOT/.gate.lock" ] || die "a gate run holds .gate.lock; wait for it to finish"

# Provenance: the candidate is the committed tree, and it differs from the starting product only under the evidence directory.
HEAD_SHA="$(git -C "$ROOT" rev-parse HEAD)"
TREE_SHA="$(git -C "$ROOT" rev-parse 'HEAD^{tree}')"
[ -z "$(git -C "$ROOT" status --porcelain --untracked-files=no)" ] || die "tracked files differ from HEAD; commit or discard them first"
[ -z "$(git -C "$ROOT" status --porcelain -- "$EVIDENCE_PREFIX")" ] || die "the evidence directory has uncommitted or untracked files"
if ! CHANGED="$(git -C "$ROOT" diff --name-only "$BASELINE" HEAD)"; then
  die "cannot diff HEAD against the starting product commit $BASELINE"
fi
while IFS= read -r changed; do
  [ -n "$changed" ] || continue
  case "$changed" in
    "$EVIDENCE_PREFIX"*) ;;
    *) die "HEAD changes a path outside the evidence directory: $changed" ;;
  esac
done <<EOF
$CHANGED
EOF

# Pinned inputs must equal the bytes in inputs.json.
if ! node "$HERE/lib/verify-inputs.mjs" "$ROOT" "$HERE/inputs.json" > "$OUTPUT/input-pins.txt" 2>&1; then
  die "pinned input mismatch; see input-pins.txt"
fi

# Built artifacts, dependencies and the host browser must exist. Nothing is built here.
for binary in catalog inventory commerce payments access gateway; do
  [ -x "$ROOT/bin/gate/$binary" ] || die "missing bin/gate/$binary; run make build-gate-linux"
done
for built in web/scanner/dist/index.html web/storefront/dist/server/entry.mjs web/backoffice/dist/server/entry.mjs; do
  [ -f "$ROOT/$built" ] || die "missing $built; run make build-ts"
done
[ -f "$ROOT/node_modules/playwright-core/package.json" ] || die "missing node_modules; run make deps"
CHROME_MACOS="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
if ! { [ -x /opt/google/chrome/chrome ] || [ -x "$CHROME_MACOS" ] || command -v google-chrome >/dev/null 2>&1; }; then
  die "real Google Chrome not found; the probe drives the host Chrome, not Playwright's bundled browser"
fi
command -v docker >/dev/null 2>&1 || die "docker is not on PATH"
docker info >/dev/null 2>&1 || die "the docker daemon is not reachable"
command -v go >/dev/null 2>&1 || die "go is not on PATH; stack-env.sh runs go run for key generation"
command -v node >/dev/null 2>&1 || die "node is not on PATH"

# The browser slot is the identity the stack-env script derives for this checkout.
read -r SLOT BROWSER_PROJECT < <(bash "$ROOT/scripts/stack-env.sh" --identity "$ROOT" browser)
[ -z "$(docker ps -q --filter "label=com.docker.compose.project=$BROWSER_PROJECT")" ] || die "the browser stack $BROWSER_PROJECT is running; wait for make browser or browser.sh to finish"

# The probe-only project uses the same digest as the browser slot, so it is traceable to this checkout.
PROBE_DIGEST="$(printf '%s/%s' "$ROOT" browser | shasum -a 256 | cut -c1-16)"
PROBE_PROJECT="ticketing-tkt563-probe-$PROBE_DIGEST"
[ -z "$(docker ps -aq --filter "label=com.docker.compose.project=$PROBE_PROJECT")" ] || die "containers for $PROBE_PROJECT already exist; they are not from this run"

# Source the stack environment once, with the probe-only project name set first.
export ROOT
export SMOKE_COMPOSE_PROJECT="$PROBE_PROJECT"
# shellcheck source=/dev/null
. "$ROOT/scripts/stack-env.sh" browser
[ "$PROJECT" = "$PROBE_PROJECT" ] || die "stack identity mismatch: $PROJECT"
export PUBLIC_BASE_URL="http://localhost:${GATEWAY_PORT}"

for port in "$GATEWAY_PORT" "$POSTGRES_PORT" "$ACCESS_PORT" "$CATALOG_PORT" "$NATS_PORT"; do
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    die "port $port is already in use on this host"
  fi
done

COMPOSE_FILES=(-f "$ROOT/compose.yaml" -f "$ROOT/compose.direct-ports.yaml" -f "$ROOT/compose.onsale-load.yaml" -f "$ROOT/compose.smoke-cadence.yaml" -f "$ROOT/compose.smoke.yaml")
compose() { docker compose -p "$PROJECT" "${COMPOSE_FILES[@]}" "$@"; }

# Presenter diagnostics run from a disposable copy under /tmp. The worktree is never edited.
build_disposable() {
  local variant="$1" file entry name
  [ -x "$ROOT/web/scanner/node_modules/.bin/vite" ] || die "missing web/scanner/node_modules; run make deps"
  DISPOSABLE="$(mktemp -d "${TMPDIR:-/tmp}/tkt563-disposable.XXXXXX")"
  mkdir -p "$DISPOSABLE/scanner/node_modules"
  tar -C "$ROOT/web" --exclude='scanner/node_modules' --exclude='scanner/dist' -cf - scanner | tar -C "$DISPOSABLE" -xf -
  # Link each dependency entry. The cache directories stay out of the links, so tsc and vite write into the copy, never into the repository.
  for entry in "$ROOT/web/scanner/node_modules"/* "$ROOT/web/scanner/node_modules"/.[!.]*; do
    [ -e "$entry" ] || continue
    name="$(basename "$entry")"
    case "$name" in
      .tmp|.vite|.cache) continue ;;
    esac
    ln -s "$entry" "$DISPOSABLE/scanner/node_modules/$name"
  done
  node "$HERE/lib/mutate-source.mjs" "$DISPOSABLE/scanner" "$variant" "$OUTPUT/mutation-$variant.json" || die "mutation $variant did not match exactly once"
  case "$variant" in
    COPY-EXIT|COPY-LIMIT) file="src/scan-result.ts" ;;
    COUNT-REJECTED) file="src/App.tsx" ;;
  esac
  diff -u --label "a/web/scanner/$file" --label "b/web/scanner/$file" "$DISPOSABLE/originals/$(basename "$file")" "$DISPOSABLE/scanner/$file" > "$OUTPUT/mutation-$variant.diff" || true
  (cd "$DISPOSABLE/scanner" && ./node_modules/.bin/tsc -b && ./node_modules/.bin/vite build --outDir dist --emptyOutDir) 2>&1 \
    | node "$HERE/lib/redact.mjs" > "$OUTPUT/disposable-build-$variant.log" \
    || die "disposable build for $variant failed; see disposable-build-$variant.log"
  [ -f "$DISPOSABLE/scanner/dist/index.html" ] || die "disposable build for $variant produced no index.html"
  DIAG_DIST="$DISPOSABLE/scanner/dist"
}

case "$VARIANT" in
  COPY-EXIT|COPY-LIMIT|COUNT-REJECTED) build_disposable "$VARIANT" ;;
esac

# Start the probe-only stack. Logs are redacted before they reach the output.
compose down -v --remove-orphans 2>&1 | node "$HERE/lib/redact.mjs" > "$OUTPUT/compose-prestart.log" || true
STACK_STARTED=1
if ! compose up -d --build --wait 2>&1 | node "$HERE/lib/redact.mjs" --generic > "$OUTPUT/compose-up.log"; then
  compose logs --no-color --tail 200 2>&1 | node "$HERE/lib/redact.mjs" --generic > "$OUTPUT/compose-failed-start.log" || true
  echo "run-probe: compose up failed; see compose-failed-start.log" >&2
  exit 2
fi
PG_ID="$(compose ps -q postgres)"
CAT_ID="$(compose ps -q catalog)"
ACC_ID="$(compose ps -q access)"
if [ -z "$PG_ID" ] || [ -z "$CAT_ID" ] || [ -z "$ACC_ID" ]; then
  echo "run-probe: postgres, catalog or access container is missing" >&2
  exit 2
fi

{
  echo "project=$PROJECT"
  echo "slot=$SLOT"
  echo "gateway_port=$GATEWAY_PORT"
  echo "postgres_port=$POSTGRES_PORT"
  echo "access_port=$ACCESS_PORT"
  echo "catalog_port=$CATALOG_PORT"
  echo "access_qr_kid=$ACCESS_QR_KID"
  echo "postgres_container=$PG_ID"
  echo "catalog_container=$CAT_ID"
  echo "access_container=$ACC_ID"
  echo "docker_server=$(docker version --format '{{.Server.Version}}' 2>/dev/null || echo unknown)"
  echo "go=$(go version 2>/dev/null || echo unknown)"
  echo "node=$(node --version)"
  echo "head=$HEAD_SHA"
  echo "tree=$TREE_SHA"
  echo "baseline=$BASELINE"
  echo "variant=$VARIANT"
  echo "repeat=$REPEAT"
  echo "scanner_mutation=absent"
  echo "disposable_dist=${DIAG_DIST:-none}"
  echo "started_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  for binary in catalog inventory commerce payments access gateway; do
    echo "binary_sha256 $binary $(shasum -a 256 "$ROOT/bin/gate/$binary" | cut -d' ' -f1)"
  done
  while IFS= read -r built_file; do
    [ -n "$built_file" ] || continue
    echo "stock_dist_sha256 ${built_file#"$ROOT/"} $(shasum -a 256 "$built_file" | cut -d' ' -f1)"
  done <<EOF
$(find "$ROOT/web/scanner/dist" -type f | LC_ALL=C sort)
EOF
  for probe_file in run-probe.sh probe.mjs inputs.json README.md run-make-browser.sh lib/checks.mjs lib/kit.mjs lib/fixtures.mjs lib/assets.mjs lib/denial.mjs lib/mixed.mjs lib/mutate-source.mjs lib/redact.mjs lib/verify-inputs.mjs; do
    echo "probe_file_sha256 $probe_file $(shasum -a 256 "$HERE/$probe_file" | cut -d' ' -f1)"
  done
} > "$OUTPUT/runner-receipt.txt"

# The probe receives the stack values through its own environment, and the
# credential values are never printed by the runner.
set +e
env -u SCANNER_MUTATION \
  PROBE_REPO="$ROOT" PROBE_VARIANT="$VARIANT" PROBE_REPEAT="$REPEAT" PROBE_OUTPUT="$OUTPUT" \
  PROBE_STOCK_DIST="$ROOT/web/scanner/dist" PROBE_DIAG_DIST="$DIAG_DIST" \
  PROBE_HEAD="$HEAD_SHA" PROBE_TREE="$TREE_SHA" PROBE_BASELINE="$BASELINE" \
  BASE="http://localhost:${GATEWAY_PORT}" POSTGRES_CONTAINER="$PG_ID" CATALOG_CONTAINER="$CAT_ID" ACCESS_CONTAINER="$ACC_ID" \
  ACCESS_PORT="$ACCESS_PORT" ACCESS_QR_PRIVATE_KEY="$ACCESS_QR_PRIVATE_KEY" ACCESS_QR_KID="$ACCESS_QR_KID" \
  INTERNAL_SERVICE_TOKEN="$INTERNAL_SERVICE_TOKEN" \
  node "$HERE/probe.mjs" 2>&1 | node "$HERE/lib/redact.mjs" > "$OUTPUT/probe.log"
PROBE_RC=${PIPESTATUS[0]}
set -e

compose logs --no-color --tail 300 2>&1 | node "$HERE/lib/redact.mjs" --generic > "$OUTPUT/compose-logs.redacted.log" || true
echo "probe_exit=$PROBE_RC" >> "$OUTPUT/runner-receipt.txt"
exit "$PROBE_RC"
