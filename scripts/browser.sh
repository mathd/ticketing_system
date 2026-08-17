#!/usr/bin/env bash
# The browser-submit gate (AGENTS.md): drive real Chrome against the real stack
# and SUBMIT the storefront's forms.
#
# `make check`'s smoke suite exercises the catalog API directly and only *renders*
# back-office and storefront pages — it never submits an Astro form. So the whole
# class of "the SSR layer rejects or mangles the write before the handler runs"
# (checkOrigin, base-path rewrites, redirects, cookie paths, cache headers,
# referrer policy) is invisible to it. Two tickets found real defects here that
# nothing else could see: TKT-105's proxy-aware origin check, and TKT-226's
# `Referrer-Policy: no-referrer`, which made Chrome send `Origin: null` and 403 every
# password reset.
#
# NOT part of `make check`: it needs a real Chrome on the host, so CI cannot run it
# and a developer without one must still be able to pass the gate. It is the
# separate, deliberate step AGENTS.md requires for any ticket that adds or changes
# a write form.
#
#   make browser              # up, run every spec, tear down
#   ./scripts/browser.sh up   # leave the stack up for iterating
#   ./scripts/browser.sh run  # run the specs against a stack left up
#   ./scripts/browser.sh down
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# Project name, ports and credentials — the same isolation smoke.sh gets, under
# its own slot so the two stacks can be up at once in one worktree (TKT-228).
. "$ROOT/scripts/stack-env.sh" browser

# Fast path only (host-built artifacts, compose.smoke.yaml), deliberately not
# `make up`. Same images, same wiring, same gateway — only the build source
# differs, and none of what this gate checks lives in the build, so paying for
# an in-Docker rebuild per run would buy nothing this gate reads.
#
# This comment used to say the in-Docker scanner build was BROKEN (TKT-227) and
# that the detour was forced. That was never true here and is not true now:
# TKT-227 did not reproduce on this platform, and `make up` builds the scanner
# image and comes up healthy. The detour is a speed choice — keep it, but do not
# re-inherit it as a workaround for a defect that does not exist.
# compose.direct-ports.yaml: see the note in scripts/smoke.sh (ai-review S11).
COMPOSE_FILES=(-f "$ROOT/compose.yaml" -f "$ROOT/compose.direct-ports.yaml" -f "$ROOT/compose.onsale-load.yaml" -f "$ROOT/compose.smoke.yaml")
compose() { docker compose -p "$PROJECT" "${COMPOSE_FILES[@]}" "$@"; }

require_artifacts() {
  for b in catalog inventory commerce payments access gateway; do
    [ -x "$ROOT/bin/gate/$b" ] || { echo "browser: missing bin/gate/$b — run 'make build-gate-linux'" >&2; exit 1; }
  done
  [ -f "$ROOT/web/scanner/dist/index.html" ] || { echo "browser: missing web/scanner/dist — run 'make build-ts'" >&2; exit 1; }
  [ -f "$ROOT/web/storefront/dist/server/entry.mjs" ] || { echo "browser: missing web/storefront/dist — run 'make build-ts'" >&2; exit 1; }
  # The back office is built from web/backoffice/dist by Dockerfile.smoke, exactly
  # as the storefront is. It was missing from this preflight (TKT-236): without it
  # a stale or absent dist fails later, at `docker build`, with a message about a
  # COPY path rather than the one the developer can act on.
  [ -f "$ROOT/web/backoffice/dist/server/entry.mjs" ] || { echo "browser: missing web/backoffice/dist — run 'make build-ts'" >&2; exit 1; }
  # REAL Chrome, checked here rather than discovered at browser launch (TKT-236).
  #
  # The specs ask for `channel: 'chrome'` deliberately — AGENTS.md wants the
  # host's browser, which is why CI cannot run this gate. Playwright's bundled
  # Chromium is not a substitute and using it would make the gate pass while
  # testing the thing the gate exists to reject.
  #
  # This check exists because the gate had NEVER RUN on one developer machine:
  # all three specs died identically at launch, after the whole stack had come
  # up, with an error that reads like a Playwright problem rather than a missing
  # dependency. A mandatory gate that cannot run is worse than one known to be
  # missing — it is green by omission. Fail here, before spending three minutes
  # on `docker compose up`, and say exactly what to do.
  #
  # The probe has to name every path Playwright's `channel: 'chrome'` would itself
  # accept, or it reintroduces the failure it was written to prevent: it was Linux-only,
  # so on macOS — where Chrome lives in the app bundle below — the gate refused to start
  # on a machine that had Chrome installed all along. A preflight stricter than the
  # launcher is the same "cannot run" outcome, just louder.
  CHROME_MACOS="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
  [ -x /opt/google/chrome/chrome ] || [ -x "$CHROME_MACOS" ] || command -v google-chrome >/dev/null 2>&1 || {
    echo "browser: real Google Chrome not found (looked for /opt/google/chrome/chrome, the macOS app bundle, and google-chrome on PATH)." >&2
    echo "  The specs drive the HOST's Chrome on purpose (AGENTS.md); Playwright's bundled Chromium is not a substitute." >&2
    echo "  Install it with 'npx playwright install chrome' (needs sudo), your distro's google-chrome-stable package, or the macOS download." >&2
    exit 1
  }
}

up() {
  require_artifacts
  # Pre-clean for the same reason smoke.sh does: a hard-killed previous run leaves
  # migrated pgdata and Exited-0 one-shot migrate jobs behind, and a plain `up`
  # would reuse both.
  compose down -v --remove-orphans >/dev/null 2>&1 || true
  if ! compose up -d --build --wait; then
    echo "--- compose up failed; recent logs: ---" >&2
    compose logs --tail 50 >&2
    exit 1
  fi
  echo "browser: gateway=http://localhost:${GATEWAY_PORT}"
}

# The specs query the database the way an operator does — through psql in the
# running container — rather than pulling in a node postgres driver for two
# SELECTs. They get the container id, not a connection string.
run_specs() {
  local pg
  pg="$(compose ps -q postgres)"
  [ -n "$pg" ] || { echo "browser: no stack is up — run './scripts/browser.sh up' first" >&2; exit 1; }

  # No nullglob: an unmatched glob stays literal, so `-f` on the first element is
  # false and this fails loudly. Loud, not skipped — a run that silently executes
  # nothing is a green gate that proved nothing, the defect scripts/smoke.sh
  # records four times over -run filters. (It also sidesteps `${#arr[@]}` on an
  # empty array, which is an unbound-variable error under `set -u` on bash 3.2.)
  local specs=("$ROOT"/test/browser/*.mjs)
  [ -f "${specs[0]}" ] || { echo "browser: no specs in test/browser" >&2; exit 1; }

  # The catalog container, for the same reason as postgres: a back-office spec has
  # to SIGN IN, and this script provisions no staff account (smoke.sh does, for its
  # own suite). Handing over the container rather than a credential keeps the
  # provisioning where the password can go in on stdin and never onto a command
  # line (TKT-236).
  local catalog
  catalog="$(compose ps -q catalog)"
  [ -n "$catalog" ] || { echo "browser: catalog container not found" >&2; exit 1; }

  local failed=0
  for spec in "${specs[@]}"; do
    echo "=== $(basename "$spec")"
    BASE="http://localhost:${GATEWAY_PORT}" POSTGRES_CONTAINER="$pg" CATALOG_CONTAINER="$catalog" \
      node "$spec" || failed=1
  done
  return $failed
}

case "${1:-all}" in
all)
  # Trap BEFORE `up`, as smoke.sh does: a stack that half-starts must not be left
  # behind for the next run's pre-clean to find.
  trap 'compose down -v --remove-orphans >/dev/null 2>&1 || true' EXIT INT TERM
  up
  run_specs
  ;;
up)   up ;;
run)  run_specs ;;
down) compose down -v --remove-orphans ;;
logs) compose logs --tail 60 "${2:-storefront}" ;;
*)    echo "usage: browser.sh [all|up|run|down|logs [service]]" >&2; exit 2 ;;
esac
