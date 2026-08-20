#!/usr/bin/env bash
set -euo pipefail

workflow=${1:-.github/workflows/security.yaml}
source_path=${2:-services/catalog/internal/api/server.go}

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

echo "security workflow trigger: $source_path schedules repository-scan"
