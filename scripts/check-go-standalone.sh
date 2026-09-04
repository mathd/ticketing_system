#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

if [ "$#" -eq 0 ]; then
  echo "usage: $(basename "$0") <module-dir>..." >&2
  exit 2
fi

# The caller does not define the scope of this verdict. Every module named by
# go.work must be checked exactly once, even if GO_MODULES is not updated when a
# ninth module is added. Canonical paths catch aliases such as x and ./x too.
python3 scripts/check-go-workspace-module-set.py check-go-standalone "$PWD" "$@"

for module in "$@"; do
  echo "standalone Go module: $module"
  (
    cd "$module"
    patterns=(./...)
    if grep -Eq '^tool[[:space:]]' go.mod; then
      patterns+=(tool)
    fi
    GOWORK=off GOPROXY=off go list -deps -test -tags smoke -mod=readonly "${patterns[@]}" >/dev/null
  )
done
