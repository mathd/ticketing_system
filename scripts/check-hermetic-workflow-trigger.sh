#!/usr/bin/env bash
set -euo pipefail

workflow=${1:-.github/workflows/hermetic.yaml}

fail() {
  echo "hermetic workflow trigger check failed: $*" >&2
  exit 1
}

[[ -f "$workflow" ]] || fail "workflow not found: $workflow"

paths=$(awk '
  $0 == "  pull_request:" {
    found = 1
    in_pull_request = 1
    next
  }
  in_pull_request && /^  [[:alnum:]_-]+:/ {
    in_pull_request = 0
    in_paths = 0
  }
  in_pull_request && $0 == "    paths:" {
    in_paths = 1
    next
  }
  in_paths && /^      -[[:space:]]+/ {
    entry = $0
    sub(/^      -[[:space:]]+/, "", entry)
    gsub(/^['\''"]|['\''"]$/, "", entry)
    print entry
  }
  END {
    if (!found) {
      exit 1
    }
  }
' "$workflow") || fail "pull_request paths could not be read"

awk '
  $0 == "  hermetic-smoke:" {
    found = 1
    in_job = 1
    next
  }
  in_job && /^  [[:alnum:]_-]+:/ {
    in_job = 0
  }
  in_job && /^    if:/ {
    conditional = 1
  }
  END {
    exit !(found && !conditional)
  }
' "$workflow" || fail "hermetic-smoke is missing or conditional"

for app in backoffice storefront scanner; do
  for input in Dockerfile package.json; do
    changed_path="web/$app/$input"
    matched=false
    while IFS= read -r pattern; do
      if [[ $changed_path == $pattern ]]; then
        matched=true
        break
      fi
    done <<<"$paths"
    $matched || fail "$changed_path does not schedule hermetic-smoke"
    echo "hermetic workflow trigger: $changed_path schedules hermetic-smoke"
  done
done
