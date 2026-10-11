#!/usr/bin/env bash
set -euo pipefail

workflow=${1:-.github/workflows/security.yaml}
source_path=${2:-services/catalog/internal/api/server.go}
makefile=${3:-Makefile}

fail() {
  echo "security workflow trigger check failed: $*" >&2
  exit 1
}

[[ -f "$workflow" ]] || fail "workflow not found: $workflow"

# An unfiltered pull_request event schedules the workflow for every changed path.
awk '
  $0 == "  pull_request:" {
    found = 1
    in_pull_request = 1
    next
  }
  in_pull_request && /^  [[:alnum:]_-]+:/ {
    in_pull_request = 0
  }
  in_pull_request && /^[[:space:]]+(paths|paths-ignore):/ {
    filtered = 1
  }
  END {
    exit !(found && !filtered)
  }
' "$workflow" || fail "pull_request is missing or has a path filter"

# The repository scan must be unconditional and must keep scanning for both
# secrets and configuration defects. Otherwise an all-PR workflow can still
# skip the only job that inspects source files.
awk '
  $0 == "  repository-scan:" {
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
  in_job && /aquasecurity\/trivy-action@/ {
    trivy = 1
  }
  in_job && /scanners:/ && /secret/ {
    secret = 1
  }
  in_job && /scanners:/ && /misconfig/ {
    misconfig = 1
  }
  END {
    exit !(found && !conditional && trivy && secret && misconfig)
  }
' "$workflow" || fail "repository-scan is missing, conditional, or lacks Trivy secret/misconfiguration scanning"

# The scan allowlist (ADR-081) is wired in three places, and each one can be
# lost without any scan going red. The workflow half is checked by parsing the
# YAML, not by matching lines, so text in a `name:` block or a quoted `"if":` key
# reads as what it is: pnpm-audit must end with exactly
# `node scripts/scan-allowlist.mjs audit`, with nothing that can skip it or hide
# a failure, and with a bounded `timeout-minutes`; the changes filter must match
# .trivyignore.yaml; the Trivy step must keep its inputs (exit-code "1",
# HIGH,CRITICAL, trivyignores) and nothing may skip it. Needs node_modules (the
# `yaml` package), so it runs after deps.
[[ -f "$makefile" ]] || fail "Makefile not found: $makefile"
node scripts/scan-allowlist.mjs wiring --workflow "$workflow" >/dev/null ||
  fail "the scan allowlist wiring in $workflow is wrong (see the lines above)"

# make check runs the check and its tests. The recipe lines must be exact: a "-"
# prefix or "|| true" would ignore a failure. The target must be defined once (a
# second definition replaces the recipe), and the Makefile must not have a
# special target or a MAKEFLAGS assignment that ignores recipe errors: `.IGNORE`
# (with or without prerequisites), `.ONESHELL` (only the last line of a recipe
# then sets its status), or `-i` / `--ignore-errors` in MAKEFLAGS.
awk '
  /^check-all:/ {
    for (i = 2; i <= NF; i++) if ($i == "check-scan-allowlist") listed = 1
  }
  /^check-scan-allowlist[[:space:]]*::?/ {
    definitions++
  }
  /^\.(IGNORE|ONESHELL)([[:space:]]|:|$)/ {
    special = 1
  }
  /^[[:space:]]*(override[[:space:]]+)?(export[[:space:]]+)?(GNU)?MAKEFLAGS[[:space:]]*([:+?!]?=|$)/ {
    makeflags = 1
  }
  $0 == "check-scan-allowlist:" {
    in_target = 1
    next
  }
  in_target && !/^\t/ {
    in_target = 0
  }
  in_target && $0 == "\t@node --test scripts/scan-allowlist.test.mjs" {
    tests = 1
  }
  in_target && $0 == "\t@node scripts/scan-allowlist.mjs check" {
    check = 1
  }
  END {
    exit !(listed && tests && check && definitions == 1 && !special && !makeflags)
  }
' "$makefile" || fail "check-all must list check-scan-allowlist, defined once, which must run 'node --test scripts/scan-allowlist.test.mjs' and 'node scripts/scan-allowlist.mjs check'; .IGNORE, .ONESHELL and MAKEFLAGS assignments are not allowed"

echo "security workflow trigger: $source_path schedules repository-scan"
echo "security workflow trigger: the scan allowlist is wired into pnpm-audit, repository-scan and make check"
