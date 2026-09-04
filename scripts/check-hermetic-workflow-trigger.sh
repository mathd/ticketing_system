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

awk '
  function unquote_key(key, single_quote) {
    single_quote = sprintf("%c", 39)
    if ((substr(key, 1, 1) == "\"" && substr(key, length(key), 1) == "\"") ||
        (substr(key, 1, 1) == single_quote && substr(key, length(key), 1) == single_quote)) {
      return substr(key, 2, length(key) - 2)
    }
    return key
  }
  function inspect_step_field(field, key, value) {
    key = field
    sub(/[[:space:]]*:.*$/, "", key)
    gsub(/^[[:space:]]+|[[:space:]]+$/, "", key)
    key = unquote_key(key)
    if (key == "if") {
      step_is_conditional = 1
    }
    if (key == "continue-on-error") {
      step_ignores_failure = 1
    }
    if (key == "run") {
      value = field
      sub(/^[^:]*:[[:space:]]*/, "", value)
      if (value ~ /^make smoke-hermetic[[:space:]]*$/) {
        step_runs_smoke = 1
      }
    }
  }
  $0 == "  hermetic-smoke:" {
    in_job = 1
    next
  }
  in_job && /^  [[:alnum:]_-]+:/ {
    in_job = 0
  }
  in_job && /^      -[[:space:]]+/ {
    if (step_runs_smoke && (step_is_conditional || step_ignores_failure)) {
      unsafe_smoke_step = 1
    }
    if (step_runs_smoke) {
      found = 1
    }
    step_runs_smoke = 0
    step_is_conditional = 0
    step_ignores_failure = 0

    entry = $0
    sub(/^      -[[:space:]]+/, "", entry)
    inspect_step_field(entry)
    next
  }
  in_job && /^        [^[:space:]]/ {
    entry = $0
    sub(/^        /, "", entry)
    inspect_step_field(entry)
  }
  END {
    if (step_runs_smoke && (step_is_conditional || step_ignores_failure)) {
      unsafe_smoke_step = 1
    }
    if (step_runs_smoke) {
      found = 1
    }
    exit !(found && !unsafe_smoke_step)
  }
' "$workflow" || fail "hermetic-smoke does not run make smoke-hermetic as a required step"

path_is_covered() {
  HERMETIC_PATH_PATTERNS="$paths" python3 - "$1" <<'PY'
import os
import re
import sys


def github_pattern(pattern: str) -> re.Pattern[str]:
    """Translate the path-filter wildcards GitHub documents to an anchored regex."""
    translated: list[str] = []
    i = 0
    while i < len(pattern):
        char = pattern[i]
        if char == "\\" and i + 1 < len(pattern):
            translated.append(re.escape(pattern[i + 1]))
            i += 2
            continue
        if char == "*":
            if i + 1 < len(pattern) and pattern[i + 1] == "*":
                i += 2
                if i < len(pattern) and pattern[i] == "/":
                    translated.append("(?:.*/)?")
                    i += 1
                else:
                    translated.append(".*")
                continue
            translated.append("[^/]*")
            i += 1
            continue
        if char == "?":
            translated.append("[^/]")
            i += 1
            continue
        if char == "[":
            end = i + 1
            if end < len(pattern) and pattern[end] in "!^":
                end += 1
            if end < len(pattern) and pattern[end] == "]":
                end += 1
            while end < len(pattern) and pattern[end] != "]":
                end += 1
            if end == len(pattern):
                translated.append(r"\[")
                i += 1
                continue
            content = pattern[i + 1 : end]
            if content.startswith("!"):
                content = "^" + content[1:]
            elif content.startswith("^"):
                content = "\\" + content
            translated.append("[" + content.replace("\\", r"\\") + "]")
            i = end + 1
            continue
        translated.append(re.escape(char))
        i += 1
    return re.compile("".join(("^", *translated, "$")))


changed_path = sys.argv[1]
included = False
for entry in os.environ["HERMETIC_PATH_PATTERNS"].splitlines():
    excluded = entry.startswith("!")
    pattern = entry[1:] if excluded else entry
    if github_pattern(pattern).fullmatch(changed_path):
        included = not excluded
raise SystemExit(0 if included else 1)
PY
}

for input in compose.yaml compose.direct-ports.yaml compose.onsale-load.yaml compose.smoke-cadence.yaml; do
  path_is_covered "$input" || fail "$input does not schedule hermetic-smoke"
  echo "hermetic workflow trigger: $input schedules hermetic-smoke"
done

for app in backoffice storefront scanner; do
  for input in Dockerfile package.json; do
    changed_path="web/$app/$input"
    path_is_covered "$changed_path" || fail "$changed_path does not schedule hermetic-smoke"
    echo "hermetic workflow trigger: $changed_path schedules hermetic-smoke"
  done
done
