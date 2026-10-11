#!/usr/bin/env bash
# The main ruleset (.github/rulesets/main.json) requires a status check context.
# This guard refuses a workflow or ruleset shape that would leave that context
# pending, skipped, orphaned or ambiguous (TKT-324). It reads files only; it does
# not read the live ruleset on GitHub. It refuses:
#   1. a `paths` or `paths-ignore` filter on check.yaml's `push` or
#      `pull_request` event, or no `pull_request` event;
#   2. a job-level `if:` on the check.yaml job that reports a required context;
#   3. a required context that is not the check-run name of a check.yaml job
#      (the job id, or its `name:` when one is set), or a ruleset with none;
#   4. more than one job, in any workflow, whose id or `name:` is that context.
# The YAML reader is line-based and reads block-style mappings, like the other
# workflow trigger checks. It refuses shapes that it cannot read.
set -euo pipefail

workflow=${1:-.github/workflows/check.yaml}
ruleset=${2:-.github/rulesets/main.json}
workflows_dir=${3:-.github/workflows}

python3 - "$workflow" "$ruleset" "$workflows_dir" <<'PY'
import json
import re
import sys
from pathlib import Path

workflow, ruleset, workflows_dir = (Path(arg) for arg in sys.argv[1:4])
failures: list[str] = []

KEY = re.compile(
    r"""^(?P<indent> *)(?P<key>"[^"]*"|'[^']*'|[A-Za-z0-9_.-]+)[ \t]*:(?:[ \t]+(?P<value>.*))?$"""
)
FILTER = re.compile(r"(?<![\w-])(paths|paths-ignore)\s*:")


def fail(message: str) -> None:
    print(f"check workflow trigger check failed: {message}", file=sys.stderr)
    sys.exit(1)


def unquote(text: str) -> str:
    if len(text) >= 2 and text[0] == text[-1] and text[0] in "\"'":
        return text[1:-1]
    return text


def scalar(value: str | None) -> str:
    """The value of `key: value`, without quotes or a trailing comment."""
    if not value:
        return ""
    value = value.strip()
    if value[:1] in "\"'":
        end = value.find(value[0], 1)
        return value[1:end] if end > 0 else value
    return re.sub(r"\s+#.*$", "", value).strip()


def load(path: Path) -> list[tuple[int, int, str | None, str]]:
    """(line number, indent, mapping key or None, value) per content line."""
    lines = []
    for number, raw in enumerate(path.read_text().splitlines(), 1):
        stripped = raw.strip()
        if not stripped or stripped.startswith("#") or stripped in ("---", "..."):
            continue
        match = KEY.match(raw)
        indent = len(raw) - len(raw.lstrip(" "))
        if match:
            lines.append((number, indent, unquote(match["key"]), scalar(match["value"])))
        else:
            lines.append((number, indent, None, ""))
    return lines


def children(lines, parent: int) -> list[int]:
    """Indexes of the mapping keys directly below lines[parent] (-1 = root)."""
    parent_indent = lines[parent][1] if parent >= 0 else -1
    child_indent = None
    found = []
    for index in range(parent + 1, len(lines)):
        indent = lines[index][1]
        if indent <= parent_indent:
            break
        if child_indent is None:
            child_indent = indent
        if indent == child_indent and lines[index][2] is not None:
            found.append(index)
    return found


def child(lines, parent: int, *names: str) -> int | None:
    for index in children(lines, parent):
        if lines[index][2] in names:
            return index
    return None


def jobs(path: Path) -> list[dict]:
    lines = load(path)
    root = child(lines, -1, "jobs")
    if root is None:
        return []
    found = []
    for index in children(lines, root):
        props = {lines[k][2]: lines[k] for k in children(lines, index)}
        name = props["name"][3] if "name" in props else ""
        found.append(
            {
                "path": path,
                "id": lines[index][2],
                "line": lines[index][0],
                "name": name,
                "check_run": name or lines[index][2],
                "props": props,
            }
        )
    return found


if not workflow.is_file():
    fail(f"workflow not found: {workflow}")

# 1. Every PR and every push to main must schedule the workflow.
lines = load(workflow)
on = child(lines, -1, "on", "true")
if on is None:
    fail(f"{workflow}: no `on:` block")
inline = lines[on][3]
if inline:
    if inline.startswith("{"):
        fail(f"{workflow}: `on:` is a flow mapping; write it as a block mapping")
    events = {unquote(e.strip()): (0, set()) for e in inline.strip("[]").split(",") if e.strip()}
else:
    events = {}
    for index in children(lines, on):
        filters = set(FILTER.findall(lines[index][3]))
        for sub in children(lines, index):
            if lines[sub][2] in ("paths", "paths-ignore"):
                filters.add(lines[sub][2])
        events[lines[index][2]] = (lines[index][0], filters)
if "pull_request" not in events:
    failures.append(f"{workflow}: no `pull_request` event, so no PR reports the required check")
for event in ("push", "pull_request"):
    number, filters = events.get(event, (0, set()))
    for key in sorted(filters):
        failures.append(
            f"{workflow}:{number}: `{event}` has a `{key}` filter; "
            "a skipped workflow leaves a required check pending"
        )

# 3. Every required context must be the check-run name of a check.yaml job.
try:
    data = json.loads(ruleset.read_text())
except (OSError, ValueError) as error:
    fail(f"ruleset not readable as JSON: {ruleset}: {error}")
contexts = []
for rule in data.get("rules", []) if isinstance(data, dict) else []:
    if isinstance(rule, dict) and rule.get("type") == "required_status_checks":
        for check in (rule.get("parameters") or {}).get("required_status_checks") or []:
            if isinstance(check, dict) and check.get("context"):
                contexts.append(check["context"])
if not contexts:
    fail(f"{ruleset}: names no required status check context")

own_jobs = jobs(workflow)
producers = {}
for context in contexts:
    producers[context] = [job for job in own_jobs if job["check_run"] == context]
    if not producers[context]:
        failures.append(
            f"{ruleset}: required context `{context}` is not the check-run name of any job "
            f"in {workflow} (the job id, or its `name:` when one is set)"
        )

# 2. The job that reports a required context must not be conditional.
for context, found in producers.items():
    for job in found:
        if "if" in job["props"]:
            failures.append(
                f"{workflow}:{job['props']['if'][0]}: job `{job['id']}` reports required "
                f"context `{context}` and has a job-level `if:`"
            )

# 4. Exactly one job in all workflows may carry a required context's name.
all_jobs = []
for path in sorted([*workflows_dir.glob("*.yaml"), *workflows_dir.glob("*.yml")]):
    all_jobs.extend(jobs(path))
for context in contexts:
    holders = [job for job in all_jobs if context in (job["id"], job["name"])]
    if len(holders) > 1:
        where = ", ".join(f"{job['path']}:{job['line']} `{job['id']}`" for job in holders)
        failures.append(f"required context `{context}` is the id or `name:` of {len(holders)} jobs: {where}")

if failures:
    for message in failures:
        print(f"check workflow trigger check failed: {message}", file=sys.stderr)
    sys.exit(1)
# No indexing here: a crash would make a disabled refusal above look like a pass.
for context in contexts:
    ids = ", ".join(f"`{job['id']}`" for job in producers[context])
    print(f"check workflow trigger: every PR runs {ids}, "
          f"the only job that reports required context `{context}`")
PY
