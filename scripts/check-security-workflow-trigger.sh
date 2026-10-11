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
# lost without any scan going red. pnpm applies auditConfig.ignoreGhsas with no
# expiry, so the pnpm-audit job must run the expiry check before the audit, with
# nothing that can skip the check or hide the audit's exit status. The audit
# must be the exact command: `pnpm audit --ignore` writes the ignore list and
# exits 0 without reporting the remaining findings.
[[ -f "$makefile" ]] || fail "Makefile not found: $makefile"

awk '
  $0 == "  pnpm-audit:" {
    found = 1
    in_job = 1
    next
  }
  in_job && /^  [[:alnum:]_-]+:/ {
    in_job = 0
  }
  in_job && /continue-on-error/ {
    tolerant = 1
  }
  in_job && /^        if:/ {
    step_if = 1
  }
  in_job && $0 == "      - run: node scripts/scan-allowlist.mjs check --now" {
    checks++
    check_line = NR
  }
  in_job && $0 == "      - run: pnpm audit --audit-level high" {
    audits++
    audit_line = NR
  }
  /pnpm audit/ && $0 != "      - run: pnpm audit --audit-level high" {
    other_audit = 1
  }
  END {
    exit !(found && !tolerant && !step_if && !other_audit && checks == 1 && audits == 1 && check_line < audit_line)
  }
' "$workflow" || fail "pnpm-audit must run 'node scripts/scan-allowlist.mjs check --now' and then exactly 'pnpm audit --audit-level high', with no step-level if, no continue-on-error and no other pnpm audit command"

# Trivy reads the allowlist only through this input. Without it every listed
# finding fails the scan again, and a plain .trivyignore would be read instead.
awk '
  $0 == "  repository-scan:" {
    in_job = 1
    next
  }
  in_job && /^  [[:alnum:]_-]+:/ {
    in_job = 0
  }
  in_job && $0 == "          trivyignores: .trivyignore.yaml" {
    wired = 1
  }
  END {
    exit !wired
  }
' "$workflow" || fail "repository-scan does not pass 'trivyignores: .trivyignore.yaml' to Trivy"

# make check runs the deterministic half of the checker and its tests. The
# recipe lines must be exact: a "-" prefix or "|| true" would ignore a failure.
awk '
  /^check-all:/ {
    for (i = 2; i <= NF; i++) if ($i == "check-scan-allowlist") listed = 1
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
    exit !(listed && tests && check)
  }
' "$makefile" || fail "check-all must list check-scan-allowlist, which must run 'node --test scripts/scan-allowlist.test.mjs' and 'node scripts/scan-allowlist.mjs check'"

echo "security workflow trigger: $source_path schedules repository-scan"
echo "security workflow trigger: the scan allowlist is wired into pnpm-audit, repository-scan and make check"
