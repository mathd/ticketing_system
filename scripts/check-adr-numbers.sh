#!/usr/bin/env bash
# Every ADR file must claim a number no other ADR file claims.
#
# Two accepted ADRs both numbered 055 (on-sale write rate limiting, presale unlock
# codes) made every bare `ADR-055` in code, migrations, OpenAPI and AGENTS.md
# ambiguous, and ADR-056 recorded the collision rather than resolving it. A number
# is a reference target, so a repeat is a defect in the registry, not a cosmetic
# one. Resolved by the 2026-08-19 review; this check keeps it resolved.
set -euo pipefail

dir=${1:-docs/adr}

fail() {
  echo "ADR number check failed: $*" >&2
  exit 1
}

[[ -d "$dir" ]] || fail "ADR directory not found: $dir"

shopt -s nullglob
files=("$dir"/ADR-*.md)
shopt -u nullglob

(( ${#files[@]} > 0 )) || fail "no ADR files found in $dir"

# A name that looks like an ADR but does not carry a three-digit number is
# unreferenceable, so it fails here rather than being skipped silently.
for path in "${files[@]}"; do
  name=$(basename "$path")
  [[ "$name" =~ ^ADR-[0-9]{3}-.+\.md$ ]] ||
    fail "not named ADR-NNN-<slug>.md: $name"
done

duplicates=$(
  for path in "${files[@]}"; do
    basename "$path" | cut -c5-7
  done | sort | uniq -d
)

if [[ -n "$duplicates" ]]; then
  while read -r number; do
    [[ -n "$number" ]] || continue
    echo "ADR number check failed: ADR-$number is claimed by more than one file:" >&2
    for path in "$dir"/ADR-"$number"-*.md; do
      echo "  $path" >&2
    done
  done <<<"$duplicates"
  exit 1
fi

echo "ADR numbers unique: ${#files[@]} ADRs in $dir"
