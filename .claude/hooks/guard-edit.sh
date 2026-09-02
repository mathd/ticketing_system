#!/usr/bin/env bash
# PreToolUse guard for Edit/Write: no edits inside the repo while the gate runs.
# The gate reads the working tree as it goes, so an edit mid-run is read
# half-written and fails for a reason that does not exist — TKT-240 spent a full
# cycle diagnosing one. Files outside a git repo (scratchpad) are never blocked.
set -euo pipefail

path="$(jq -r '.tool_input.file_path // empty')"
[ -n "$path" ] || exit 0

root="$(git -C "$(dirname "$path")" rev-parse --show-toplevel 2>/dev/null || true)"
[ -n "$root" ] || exit 0
lock="$root/.gate.lock"
[ -e "$lock" ] || exit 0

echo "Blocked: the gate has been running since $(cat "$lock") — editing the tree under it produces a failure that isn't real (TKT-240). Wait for .gate.lock to clear. If no gate is running: rm $lock" >&2
exit 2
