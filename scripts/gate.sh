#!/usr/bin/env bash
# Run the local gate under a lock and report a verdict the caller cannot misread.
# `make check` calls this; the stages live in `make check-all`, which refuses to
# start unless this run's token is in the lock. So the gate has exactly one entry.
#
# Six tickets were spent re-learning one lesson. A chained gate reports the
# chain's exit code (TKT-71, TKT-87); a piped one reports the pipe's (TKT-94); a
# backgrounded one is reported as exit 0 by the harness while the log says
# `Error 1` (TKT-101, TKT-235); and the `pgrep -f` poll used to wait on it
# self-matches, so it reports "still running" forever (TKT-106). A mis-cwd'd run
# exits 2 on "No rule to make target" and reads as a real failure (TKT-71).
#
# A verdict also has to say WHAT it is about, so it records the commit and a
# digest of the tree it tested, and re-reads that digest at the end.
#
# Be precise about what that catches. It is a NET-change backstop: it sees an
# edit that is still there when the run ends, which is the ordinary shape of the
# accident (TKT-240 left its edit in place). It cannot see a file changed while a
# stage reads it and restored before the end — that run still reports PASS over a
# tree no single stage saw whole. Catching every concurrent mutation needs the
# gate to run against an immutable snapshot, which this does not do, because the
# local gate's whole job is to test the working tree as it stands.
#
# It is still the right layer to put it in: the agent hooks that refuse a mid-run
# edit only bind agents that go through them, and another provider, an IDE or a
# human shell does not. So the digest is the backstop and the hooks are an early
# refusal — neither is a proof that nothing touched the tree.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

LOCK="$ROOT/.gate.lock"
LOG="$ROOT/.gate.log"
VERDICT="$ROOT/.gate.verdict"

# Tracked HEAD, the working diff, and the content of every untracked file the
# gate can see. Ignored paths are excluded, so the gate's own growing log is not
# part of its input.
tree_digest() {
  {
    git rev-parse HEAD
    git diff HEAD
    # Not `xargs`: with no untracked files it can still run the command once,
    # and `shasum` with no arguments reads stdin and blocks the gate forever.
    git ls-files --others --exclude-standard -z |
      while IFS= read -r -d '' f; do shasum "$f"; done
  } | shasum | cut -d' ' -f1
}

# The merge guard asks for this to check a verdict still describes the tree in
# front of it, so the definition lives in one place. Runs before any locking.
if [ "${1:-}" = "--digest" ]; then tree_digest; exit 0; fi

TOKEN="$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
owns_lock() { [ -e "$LOCK" ] && [ "$(cut -d' ' -f1 < "$LOCK" 2>/dev/null)" = "$TOKEN" ]; }

# Create-if-absent in one step: a test followed by a write is two steps, and two
# wrappers racing between them would both run the gate over one tree. The token
# is what makes the lock evidence of THIS run rather than of a file existing —
# a hand-made or stale lock lets the stages start otherwise.
if ! (set -o noclobber; printf '%s %s\n' "$TOKEN" "$(date -u +%FT%TZ)" > "$LOCK") 2>/dev/null; then
  echo "gate already running (started $(cut -d' ' -f2 < "$LOCK" 2>/dev/null)). If it is not, rm $LOCK" >&2
  exit 2
fi
trap 'owns_lock && rm -f "$LOCK"' EXIT INT TERM

HEAD_SHA="$(git rev-parse HEAD)"
BEFORE="$(tree_digest)"

rm -f "$VERDICT"
# `tee` so a human and CI see the run as it goes; `pipefail` so the status is
# make's and not tee's — an unguarded pipe here is exactly TKT-94. `-j1` because
# the stages are ordered: generation writes the API files that lint and test
# read, so a parallel run reads them mid-write.
set +e
set -o pipefail
GATE_TOKEN="$TOKEN" make -j1 check-all 2>&1 | tee "$LOG"
code=$?
set +o pipefail
set -e

AFTER="$(tree_digest)"

verdict() { echo "$1 exit=$code head=$HEAD_SHA tree=$BEFORE log=$LOG" > "$VERDICT"; cat "$VERDICT"; }

# The exit code alone is not trusted, and neither is the log alone.
if grep -qE 'Error [0-9]+|^FAIL|--- FAIL|drifted' "$LOG"; then body=FAIL; else body=PASS; fi
if [ "$code" -eq 0 ]; then by_code=PASS; else by_code=FAIL; fi

if [ "$body" != "$by_code" ]; then
  verdict "FAIL"
  echo "Exit code says $by_code, log body says $body — believe neither alone. Read $LOG." >&2
  exit 1
fi

if [ "$body" = FAIL ]; then
  verdict "FAIL"
  # A stage that rewrites generated files before failing moves the digest. Say so,
  # but never in place of the failure the run actually found.
  [ "$BEFORE" = "$AFTER" ] || echo "(the tree also changed during the run — expected when a stage rewrites generated files)" >&2
  exit 1
fi

if [ "$BEFORE" != "$AFTER" ]; then
  echo "FAIL exit=$code head=$HEAD_SHA tree=changed-during-run log=$LOG" > "$VERDICT"
  cat "$VERDICT"
  echo "The tree changed while the gate read it — this PASS describes no revision. Re-run." >&2
  exit 1
fi

verdict PASS
