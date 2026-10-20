#!/usr/bin/env bash
# Runs one command from the TKT-552 worktree root and keeps a raw log:
# label, cwd, command line, UTC start, unabridged output, UTC end, exit code.
# Usage: run-logged.sh <label> <command...>
set -u
ROOT=/home/mathieu/sources/ticketing_system.TKT-552-entry-copy
LOGDIR=/tmp/sdlc-S-2/TKT-552-implementation/logs
label="$1"; shift
cd "$ROOT" || exit 2
mkdir -p "$LOGDIR"
out="$LOGDIR/$label.log"
{
  echo "label: $label"
  echo "cwd: $(pwd)"
  echo "command: $*"
  echo "start_utc: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "----- output begins -----"
} > "$out"
"$@" >> "$out" 2>&1
rc=$?
{
  echo "----- output ends -----"
  echo "end_utc: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "exit_code: $rc"
} >> "$out"
echo "label=$label exit_code=$rc log=$out"
exit "$rc"
