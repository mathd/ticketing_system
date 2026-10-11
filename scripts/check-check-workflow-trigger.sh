#!/usr/bin/env bash
# The main ruleset (.github/rulesets/main.json) requires a status check context.
# This guard refuses a workflow or ruleset shape that would leave that context
# pending, skipped, orphaned, ambiguous, or green without `make check` (TKT-324).
# It reads files only; it does not read the live ruleset on GitHub.
#
# The YAML reader is line-based, and a line reader cannot interpret all of YAML.
# So the guard FAILS CLOSED: check.yaml must have the allowlisted block-style
# shape below, and any other spelling (flow style, quoted keys, `on:` as a list,
# a duplicate key, a second document, a merge key) is refused as unsupported.
#   Events:  `on:` holds exactly `push` (only `branches: [main]`) and an empty
#            `pull_request`.
#   Context: every required context is the reported name (the `name:` if set,
#            else the job id) of a check.yaml job, and the ruleset has one.
#   Job:     that job holds only `name`, `runs-on` and `steps`; it has exactly
#            one `run: make check` step; no step has `if:` or `continue-on-error`.
#   Unique:  no other job in any workflow reports the same name. Jobs in other
#            workflows must be block style with a literal `name:` (or none).
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
JOB_KEYS = {"name", "runs-on", "steps"}
JOB_KEY_REASONS = {
    "if": "a job-level `if:` can skip it",
    "needs": "a skipped or failed dependency skips it",
    "continue-on-error": "it can fail without failing the check",
}
STEP_KEY_REASONS = {
    "if": "a step-level `if:` can skip it",
    "continue-on-error": "it can fail without failing the check",
}


class Unsupported(Exception):
    pass


def fail(message: str) -> None:
    print(f"check workflow trigger check failed: {message}", file=sys.stderr)
    sys.exit(1)


def scalar(value: str | None) -> str:
    """The value of `key: value`, without quotes or a trailing comment."""
    if not value:
        return ""
    value = value.strip()
    if value[:1] in "\"'":
        end = value.find(value[0], 1)
        return value[1:end] if end > 0 else value
    return re.sub(r"(^|\s+)#.*$", "", value).strip()


class Line:
    def __init__(self, path: Path, number: int, raw: str):
        self.path, self.number = path, number
        self.where = f"{path}:{number}"
        self.indent = len(raw) - len(raw.lstrip(" "))
        self.text = raw.strip()
        match = KEY.match(raw)
        self.key = match["key"] if match else None
        self.quoted = bool(self.key) and self.key[0] in "\"'"
        self.value = scalar(match["value"]) if match else ""


def load(path: Path) -> list[Line]:
    lines = []
    for number, raw in enumerate(path.read_text().splitlines(), 1):
        if raw.strip() and not raw.lstrip().startswith("#"):
            lines.append(Line(path, number, raw))
    # A document marker (`---`, `...`) is never a `key:` line, so the strict
    # top-level mapping below refuses a second document.
    return lines


def block(lines: list[Line], parent: int) -> list[int]:
    """Indexes of every content line below lines[parent] (-1 = the root)."""
    parent_indent = lines[parent].indent if parent >= 0 else -1
    found = []
    for index in range(parent + 1, len(lines)):
        if lines[index].indent <= parent_indent:
            break
        found.append(index)
    return found


def mapping(lines: list[Line], parent: int, what: str) -> dict[str, int]:
    """The block mapping below lines[parent], read strictly: every line at the
    child indent must be an unquoted `key:`, with no duplicate key."""
    body = block(lines, parent)
    if not body:
        return {}
    child_indent = lines[body[0]].indent
    keys: dict[str, int] = {}
    for index in body:
        line = lines[index]
        if line.indent > child_indent:
            continue
        if line.indent < child_indent or line.key is None:
            raise Unsupported(f"{line.where}: `{line.text}` in {what} is not a block-style `key:`")
        if line.quoted:
            raise Unsupported(f"{line.where}: quoted key {line.key} in {what}; write it unquoted")
        if line.key in keys:
            raise Unsupported(f"{line.where}: duplicate key `{line.key}` in {what}")
        keys[line.key] = index
    return keys


def block_value(lines: list[Line], index: int, what: str) -> None:
    """Refuse a value written on the key's own line (flow style, an alias, a scalar)."""
    if lines[index].value:
        raise Unsupported(
            f"{lines[index].where}: {what} is written inline (`{lines[index].text}`); "
            "write it as a block"
        )


def steps(lines: list[Line], parent: int) -> list[tuple[Line, dict[str, Line]]]:
    """The block sequence of block-mapping steps below lines[parent]."""
    body = block(lines, parent)
    if not body:
        raise Unsupported(f"{lines[parent].where}: `steps:` has no block sequence below it")
    item_indent = lines[body[0]].indent
    found: list[tuple[Line, dict[str, Line]]] = []
    key_column = None
    for index in body:
        line = lines[index]
        if line.indent == item_indent:
            rest = line.text[2:].lstrip() if line.text.startswith("- ") else ""
            match = KEY.match(rest)
            if not match:
                raise Unsupported(f"{line.where}: step `{line.text}` is not a block-style `- key: value`")
            key_column = line.indent + len(line.text) - len(rest)
            found.append((line, {}))
            line = Line(line.path, line.number, " " * key_column + rest)
        elif key_column is not None and line.indent > key_column:
            continue
        elif line.indent != key_column or line.key is None:
            raise Unsupported(f"{line.where}: `{line.text}` in a step is not a block-style `key:`")
        if line.quoted:
            raise Unsupported(f"{line.where}: quoted key {line.key} in a step; write it unquoted")
        if line.key in found[-1][1]:
            raise Unsupported(f"{line.where}: duplicate key `{line.key}` in a step")
        found[-1][1][line.key] = line
    return found


def jobs(path: Path) -> list[dict]:
    """Every job of a workflow with its reported name, read strictly."""
    lines = load(path)
    root = mapping(lines, -1, f"{path}'s top level")
    if "jobs" not in root:
        return []
    block_value(lines, root["jobs"], "`jobs:`")
    found = []
    for job_id, index in mapping(lines, root["jobs"], f"{path}'s jobs").items():
        block_value(lines, index, f"job `{job_id}`")
        props = mapping(lines, index, f"job `{job_id}`")
        name = lines[props["name"]].value if "name" in props else ""
        if "${{" in name:
            raise Unsupported(f"{lines[props['name']].where}: job `{job_id}` has an expression `name:`; its reported name is unknown")
        found.append({
            "id": job_id,
            "where": lines[index].where,
            "reported": name or job_id,
            "lines": lines,
            "index": index,
            "props": props,
        })
    return found


def check_events(lines: list[Line]) -> None:
    root = mapping(lines, -1, f"{workflow}'s top level")
    if "on" not in root:
        failures.append(f"{workflow}: no unquoted `on:` key")
        return
    block_value(lines, root["on"], "`on:`")
    events = mapping(lines, root["on"], "`on:`")
    for event in ("push", "pull_request"):
        if event not in events:
            failures.append(f"{workflow}: no `{event}` event; `on:` must hold exactly push and pull_request")
    for event, index in events.items():
        if event not in ("push", "pull_request"):
            failures.append(f"{lines[index].where}: event `{event}` is not allowed; `on:` must hold exactly push and pull_request")
    if "push" in events:
        block_value(lines, events["push"], "`push:`")
        for key, index in mapping(lines, events["push"], "`push:`").items():
            line = lines[index]
            if key != "branches":
                failures.append(f"{line.where}: `push` has `{key}`; it may hold only `branches: [main]`")
            elif re.sub(r"[\s\"']", "", line.value) != "[main]":
                failures.append(f"{line.where}: `push` has `branches: {line.value}`; it must be `branches: [main]`")
    if "pull_request" in events:
        block_value(lines, events["pull_request"], "`pull_request:`")
        for key, index in mapping(lines, events["pull_request"], "`pull_request:`").items():
            failures.append(
                f"{lines[index].where}: `pull_request` has `{key}`; it must be empty, "
                "because a filtered or skipped workflow leaves a required check pending"
            )


def check_job(job: dict, context: str) -> None:
    lines, props = job["lines"], job["props"]
    for key, index in props.items():
        if key not in JOB_KEYS:
            reason = JOB_KEY_REASONS.get(key, "only `name`, `runs-on` and `steps` are allowed")
            failures.append(f"{lines[index].where}: job `{job['id']}` reports required context `{context}` and has `{key}`; {reason}")
    make_check = 0
    if "steps" in props:
        block_value(lines, props["steps"], f"job `{job['id']}`'s `steps:`")
        for item, keys in steps(lines, props["steps"]):
            for key, reason in STEP_KEY_REASONS.items():
                if key in keys:
                    failures.append(f"{keys[key].where}: a step of job `{job['id']}` has `{key}`; {reason}")
            if "run" in keys and keys["run"].value == "make check":
                make_check += 1
    if make_check != 1:
        failures.append(f"{job['where']}: job `{job['id']}` has {make_check} `run: make check` steps; it must have exactly one")


def main() -> None:
    if not workflow.is_file():
        fail(f"workflow not found: {workflow}")
    check_events(load(workflow))

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
        producers[context] = [job for job in own_jobs if job["reported"] == context]
        if not producers[context]:
            failures.append(
                f"{ruleset}: required context `{context}` is not the reported name of any job "
                f"in {workflow} (the `name:` when set, else the job id)"
            )
        for job in producers[context]:
            check_job(job, context)

    all_jobs = []
    for path in sorted([*workflows_dir.glob("*.yaml"), *workflows_dir.glob("*.yml")]):
        all_jobs.extend(jobs(path))
    for context in contexts:
        holders = [job for job in all_jobs if job["reported"] == context]
        if len(holders) > 1:
            where = ", ".join(f"{job['where']} `{job['id']}`" for job in holders)
            failures.append(f"required context `{context}` is the reported name of {len(holders)} jobs: {where}")

    if failures:
        for message in failures:
            print(f"check workflow trigger check failed: {message}", file=sys.stderr)
        sys.exit(1)
    # No indexing here: a crash would make a disabled refusal above look like a pass.
    for context in contexts:
        ids = ", ".join(f"`{job['id']}`" for job in producers[context])
        print(f"check workflow trigger: every PR runs {ids}, "
              f"the only job that reports required context `{context}`")


try:
    main()
except Unsupported as error:
    fail(f"unsupported shape: {error}")
PY
