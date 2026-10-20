#!/usr/bin/env bash
# Guarded make browser run for TKT-563 (plan section 3.1).
#
# make browser runs the TKT-559 timing spec. That spec writes
# docs/sprints/S-2/evidence/TKT-559-strict-commits.json inside the checkout.
# This wrapper keeps a verified copy of the historical bytes outside the checkout.
# On every trappable exit it archives the timing bytes that the run left, even
# when they equal history. Then it restores the historical bytes and checks the
# digest. It also checks that no other tracked file changed. Any failed step
# fails the wrapper. The probe lock is held for the whole run, so a probe
# cannot share the browser ports.
#
# A run that is killed without a trap leaves a pending marker. The marker names
# the checkout that wrote it. The next --recover-only run from that checkout
# archives the timing bytes that the killed run left, then restores and verifies
# the historical bytes. It refuses to run from any other checkout. run-probe.sh
# refuses to start while the marker exists.
#
#   run-make-browser.sh --historical-backup <file> --receipts <new directory outside the checkout>
#   run-make-browser.sh --recover-only
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../../../.." && pwd)"
TIMING_REL="docs/sprints/S-2/evidence/TKT-559-strict-commits.json"
TIMING="$ROOT/$TIMING_REL"
EXPECTED_SHA="677c6e65fea1b00e548aaef10242c5187fd0fcef9ff1a5e428c5381863dfa642"
REFERENCE_COMMIT="3752a96a16a2ed6e7fe267206c3dbd05bf5271bf"
GUARD_DIR="${TMPDIR:-/tmp}/tkt563-timing-guard"
PENDING="$GUARD_DIR/pending.env"
GUARD_COPY="$GUARD_DIR/historical.json"
ROOT_DIGEST="$(printf '%s' "$ROOT" | shasum -a 256 | cut -c1-16)"
PROBE_LOCK="${TMPDIR:-/tmp}/tkt563-probe-$ROOT_DIGEST.lock"

BACKUP=""
RECEIPTS=""
RECOVER_ONLY=0
LOCK_HELD=0
RECEIPTS_READY=0
RUN_RC=""
CLEANUP_FAILURE=""

die() {
  echo "run-make-browser: $*" >&2
  exit 3
}

usage() {
  echo "usage: run-make-browser.sh --historical-backup <file> --receipts <new directory outside the checkout> | --recover-only" >&2
  exit 3
}

sha_of() { shasum -a 256 "$1" | cut -d' ' -f1; }

while [ "$#" -gt 0 ]; do
  case "$1" in
    --recover-only) RECOVER_ONLY=1; shift ;;
    --historical-backup) [ "$#" -ge 2 ] || usage; BACKUP="$2"; shift 2 ;;
    --receipts) [ "$#" -ge 2 ] || usage; RECEIPTS="$2"; shift 2 ;;
    *) usage ;;
  esac
done

mkdir -p "$GUARD_DIR"

# Archives the timing bytes that a killed run left, restores the historical bytes
# and clears the marker. The marker must name this checkout. Nothing is restored
# until the archive is written and checked. The archive goes to the pending
# receipts directory. If that directory is missing, a new recovery directory
# outside the checkout takes it, and the path is printed and kept in GUARD_DIR.
recover_pending() {
  PENDING_ROOT=""
  PENDING_RECEIPTS=""
  PENDING_STARTED=""
  # The marker holds values written with printf %q, so sourcing it is safe.
  # shellcheck source=/dev/null
  . "$PENDING"
  [ -n "$PENDING_ROOT" ] || die "the pending marker $PENDING does not name a checkout. Restore $TIMING_REL from $GUARD_COPY by hand after you check its digest, then remove the marker."
  [ "$PENDING_ROOT" = "$ROOT" ] || die "the pending marker $PENDING belongs to checkout $PENDING_ROOT. Run --recover-only from that checkout."
  [ "$(sha_of "$GUARD_COPY")" = "$EXPECTED_SHA" ] || die "the guard copy is not the historical bytes. Restore $TIMING_REL from the backup by hand."

  local archive_dir stamp archived archived_sha
  if [ -n "$PENDING_RECEIPTS" ] && [ -d "$PENDING_RECEIPTS" ]; then
    archive_dir="$PENDING_RECEIPTS"
  else
    archive_dir="$(mktemp -d "${TMPDIR:-/tmp}/tkt563-timing-recovery.XXXXXX")" || die "cannot create a recovery evidence directory"
    case "$archive_dir/" in
      "$ROOT"/*) die "the recovery evidence directory $archive_dir is inside the checkout" ;;
    esac
    echo "run-make-browser: the pending receipts directory ${PENDING_RECEIPTS:-none} is missing. Recovery evidence goes to $archive_dir" >&2
  fi
  printf '%s\n' "$archive_dir" > "$GUARD_DIR/last-recovery-dir.txt"

  stamp="$(date -u +%Y%m%dT%H%M%SZ).$$"
  archived="none"
  archived_sha="missing"
  if [ -e "$TIMING" ]; then
    archived="$archive_dir/timing-at-recovery.$stamp.json"
    [ ! -e "$archived" ] || die "the archive name $archived is taken. Nothing was restored."
    cp "$TIMING" "$archived" || die "cannot archive the current timing bytes to $archived. Nothing was restored."
    archived_sha="$(sha_of "$archived")"
    [ "$archived_sha" = "$(sha_of "$TIMING")" ] || die "the archived copy does not match the current timing bytes. Nothing was restored."
    printf '%s\n' "$archived_sha" > "$archived.sha256"
  fi

  cp "$GUARD_COPY" "$TIMING" || die "restore failed. The archived timing bytes are at $archived."
  [ "$(sha_of "$TIMING")" = "$EXPECTED_SHA" ] || die "restore did not reproduce the historical digest. The archived timing bytes are at $archived."
  {
    printf 'recovered_utc=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'checkout=%s\n' "$ROOT"
    printf 'pending_started=%s\n' "${PENDING_STARTED:-unknown}"
    printf 'pending_receipts=%s\n' "${PENDING_RECEIPTS:-none}"
    printf 'archive_dir=%s\n' "$archive_dir"
    printf 'archived_timing=%s\n' "$archived"
    printf 'archived_timing_sha256=%s\n' "$archived_sha"
    printf 'restored_digest=%s\n' "$EXPECTED_SHA"
  } > "$archive_dir/recovery-$stamp.txt"
  rm -f "$PENDING"
  echo "run-make-browser: archived timing bytes: $archived. Restored and verified $TIMING_REL."
  echo "run-make-browser: recovery receipt: $archive_dir/recovery-$stamp.txt"
}

if [ -e "$PENDING" ]; then
  recover_pending
fi
if [ "$RECOVER_ONLY" = 1 ]; then
  [ -e "$PENDING" ] && die "recovery did not clear the marker"
  exit 0
fi

[ -n "$BACKUP" ] && [ -n "$RECEIPTS" ] || usage
[ -f "$BACKUP" ] || die "the historical backup does not exist: $BACKUP"
[ ! -e "$ROOT/.gate.lock" ] || die "a gate run holds .gate.lock; wait for it to finish"
if compgen -G "${TMPDIR:-/tmp}/tkt563-probe-*.lock" > /dev/null; then
  die "a TKT-563 probe run holds its lock; wait for it to finish"
fi

# The receipts directory is new and sits outside the checkout.
case "$RECEIPTS" in
  /*) ;;
  *) RECEIPTS="$PWD/$RECEIPTS" ;;
esac
[ ! -e "$RECEIPTS" ] || die "receipts directory already exists: $RECEIPTS"
RECEIPTS_PARENT="$(dirname "$RECEIPTS")"
[ -d "$RECEIPTS_PARENT" ] || die "receipts parent does not exist: $RECEIPTS_PARENT"
RECEIPTS_PARENT_REAL="$(cd "$RECEIPTS_PARENT" && pwd -P)"
ROOT_REAL="$(cd "$ROOT" && pwd -P)"
case "$RECEIPTS_PARENT_REAL/" in
  "$ROOT_REAL"/*) die "receipts must be outside the checkout" ;;
esac
mkdir "$RECEIPTS"
RECEIPTS_READY=1

# Every copy is checked before the run starts.
[ "$(sha_of "$TIMING")" = "$EXPECTED_SHA" ] || die "$TIMING_REL is not the historical digest before the run"
[ "$(sha_of "$BACKUP")" = "$EXPECTED_SHA" ] || die "the supplied historical backup has the wrong digest"
if ! REF_SHA="$(git -C "$ROOT" show "$REFERENCE_COMMIT:$TIMING_REL" | shasum -a 256 | cut -d' ' -f1)"; then
  die "cannot read $TIMING_REL at $REFERENCE_COMMIT"
fi
[ "$REF_SHA" = "$EXPECTED_SHA" ] || die "the reference commit has the wrong digest"
cp "$BACKUP" "$GUARD_COPY"
cp "$TIMING" "$RECEIPTS/timing-before.json"
git -C "$ROOT" status --porcelain --untracked-files=no > "$RECEIPTS/status-before.txt"

# A probe run must not start while this wrapper runs. The lock is the probe's own lock.
if ! mkdir "$PROBE_LOCK" 2>/dev/null; then
  die "a TKT-563 probe run holds $PROBE_LOCK"
fi
LOCK_HELD=1
printf 'pending_receipts=%s\n' "$RECEIPTS" > "$PROBE_LOCK/pid"

# The marker names this checkout, so a recovery from another checkout is refused.
printf 'PENDING_ROOT=%q\nPENDING_RECEIPTS=%q\nPENDING_STARTED=%q\n' "$ROOT" "$RECEIPTS" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$PENDING"

# Archives whatever timing bytes the run left, even when they equal history. It
# restores the historical bytes only after the archive is written and checked.
restore_timing() {
  local writer=not_observed archived_sha
  if [ -e "$TIMING" ] && [ "$TIMING" -nt "$RECEIPTS/timing-before.json" ]; then writer=ran; fi
  echo "timing_writer=$writer" >> "$RECEIPTS/timing-result.txt"
  if [ -e "$TIMING" ]; then
    if ! cp "$TIMING" "$RECEIPTS/timing-after-run.json"; then
      CLEANUP_FAILURE="cannot archive the timing bytes to $RECEIPTS/timing-after-run.json"
      return 1
    fi
    archived_sha="$(sha_of "$RECEIPTS/timing-after-run.json")"
    if [ "$archived_sha" != "$(sha_of "$TIMING")" ]; then
      CLEANUP_FAILURE="the archived timing bytes do not match the bytes that the run left"
      return 1
    fi
    echo "timing_after_run_sha256=$archived_sha" >> "$RECEIPTS/timing-result.txt"
    if [ "$archived_sha" = "$EXPECTED_SHA" ]; then
      echo "timing_after_run_matches_history=true" >> "$RECEIPTS/timing-result.txt"
    else
      echo "timing_after_run_matches_history=false" >> "$RECEIPTS/timing-result.txt"
    fi
  else
    echo "timing_after_run=missing" >> "$RECEIPTS/timing-result.txt"
  fi
  if ! cp "$GUARD_COPY" "$TIMING"; then
    CLEANUP_FAILURE="cannot restore $TIMING_REL from the guard copy"
    return 1
  fi
  if [ "$(sha_of "$TIMING")" != "$EXPECTED_SHA" ]; then
    CLEANUP_FAILURE="the restored $TIMING_REL does not have the historical digest"
    return 1
  fi
  echo "restored_digest=$EXPECTED_SHA" >> "$RECEIPTS/timing-result.txt"
  return 0
}

# Compares the tracked status with the status at the start of the run. It changes
# no other file. A difference fails the wrapper, and the operator decides what to do.
check_tracked_status() {
  if ! git -C "$ROOT" status --porcelain --untracked-files=no > "$RECEIPTS/status-after.txt"; then
    CLEANUP_FAILURE="git status failed after the run"
    return 1
  fi
  if ! cmp -s "$RECEIPTS/status-before.txt" "$RECEIPTS/status-after.txt"; then
    echo "unexpected_tracked_changes=true" >> "$RECEIPTS/timing-result.txt"
    CLEANUP_FAILURE="tracked changes differ from the start of the run. Compare $RECEIPTS/status-before.txt with $RECEIPTS/status-after.txt. The wrapper changed no other file."
    return 1
  fi
  return 0
}

finish() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  local cleanup_failed=0
  CLEANUP_FAILURE=""
  if [ "$RECEIPTS_READY" = 1 ]; then
    if restore_timing && check_tracked_status; then
      rm -f "$PENDING"
    else
      cleanup_failed=1
    fi
  fi
  if [ "$cleanup_failed" = 1 ]; then
    rc=4
    echo "run-make-browser: cleanup failed: $CLEANUP_FAILURE" >&2
    echo "run-make-browser: make browser exit ${RUN_RC:-not run}. The pending marker $PENDING is kept." >&2
    echo "run-make-browser: next step: fix the cause above, then run docs/sprints/S-2/evidence/TKT-563/run-make-browser.sh --recover-only from $ROOT. It archives the current timing bytes before it restores them." >&2
    echo "cleanup_failure=$CLEANUP_FAILURE" >> "$RECEIPTS/timing-result.txt"
  fi
  if [ "$RECEIPTS_READY" = 1 ]; then
    printf 'make_browser_exit=%s\nrun_exit=%s\nfinished_utc=%s\n' "${RUN_RC:-not_run}" "$rc" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$RECEIPTS/timing-result.txt"
  fi
  if [ "$LOCK_HELD" = 1 ]; then rm -rf "$PROBE_LOCK"; fi
  exit "$rc"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

set +e
(cd "$ROOT" && make browser) 2>&1 | node "$HERE/lib/redact.mjs" --generic > "$RECEIPTS/make-browser.log"
RUN_RC=${PIPESTATUS[0]}
set -e
echo "make browser exit $RUN_RC; receipts in $RECEIPTS"
exit "$RUN_RC"
