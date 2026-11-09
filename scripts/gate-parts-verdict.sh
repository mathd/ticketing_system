#!/usr/bin/env bash
# The verdict of the required `check` context when CI runs the gate as parallel
# parts (ADR-086). check.yaml's `check` job needs every part job, runs with
# `if: always()`, and calls this with `${{ toJSON(needs) }}`.
#
# Why a job of its own, and why `always()`: GitHub counts a SKIPPED required check
# as passed. Without `always()`, one failed part would skip `check`, and the PR
# could merge green. With it, `check` always runs, and this script is what fails it.
#
# It fails unless:
#   - the needed jobs are EXACTLY gate-static and one gate-smoke-<part> for each
#     part in scripts/smoke.sh SMOKE_ALL_PARTS (a missing part is a part that did
#     not run; an extra job is a job this verdict does not describe), and
#   - every one of them has the result `success` (not failure, cancelled or skipped).
# scripts/check-check-workflow-trigger.sh checks the same set statically, in the
# gate. This is the runtime half: it reads what GitHub actually ran.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SMOKE_SCRIPT=${GATE_PARTS_SMOKE_SCRIPT:-$ROOT/scripts/smoke.sh}

if [ "$#" -ne 1 ]; then
  echo "gate parts verdict: usage: $0 '<toJSON(needs)>'" >&2
  exit 2
fi

parts=$(sed -n 's/^SMOKE_ALL_PARTS="\([a-z0-9 -]*\)"$/\1/p' "$SMOKE_SCRIPT")
if [ -z "$parts" ] || [ "$(grep -c '^SMOKE_ALL_PARTS=' "$SMOKE_SCRIPT")" -ne 1 ]; then
  echo "gate parts verdict: cannot read exactly one SMOKE_ALL_PARTS=\"...\" line in $SMOKE_SCRIPT" >&2
  exit 1
fi

# shellcheck disable=SC2086 # the parts are one word each, by the sed pattern above
python3 - "$1" $parts <<'PY'
import json
import sys

try:
    needs = json.loads(sys.argv[1])
except ValueError as error:
    print(f"gate parts verdict: the needs context is not JSON: {error}", file=sys.stderr)
    sys.exit(1)
if not isinstance(needs, dict):
    print("gate parts verdict: the needs context is not a JSON object", file=sys.stderr)
    sys.exit(1)

expected = {"gate-static", *(f"gate-smoke-{part}" for part in sys.argv[2:])}
failures = []
for job in sorted(expected - needs.keys()):
    failures.append(f"part job `{job}` is not in needs, so nothing shows that it ran")
for job in sorted(needs.keys() - expected):
    failures.append(f"job `{job}` is in needs but is not a gate part")
for job in sorted(expected & needs.keys()):
    entry = needs[job]
    result = entry.get("result") if isinstance(entry, dict) else None
    print(f"gate part {job}: {result}")
    if result != "success":
        failures.append(f"part job `{job}` has result `{result}`, not `success`")

if failures:
    for message in failures:
        print(f"gate parts verdict: FAIL: {message}", file=sys.stderr)
    sys.exit(1)
print(f"gate parts verdict: PASS: all {len(expected)} gate parts succeeded")
PY
