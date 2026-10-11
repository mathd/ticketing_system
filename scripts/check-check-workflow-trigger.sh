#!/usr/bin/env bash
# The main ruleset (.github/rulesets/main.json) requires a status check context.
# This guard refuses a workflow or ruleset shape that would leave that context
# pending, skipped, orphaned, ambiguous, or green without `make check` (TKT-324).
# It reads files only; it does not read the live ruleset on GitHub.
#
# Threat model: this is a tripwire against accidental or careless edits to
# check.yaml and the ruleset. It does not, and cannot, stop a writer who changes
# the Makefile `check` target, scripts/gate.sh, a step that writes $GITHUB_ENV, or
# this guard. It does not read action inputs (`with:`), so an input that changes
# the checked-out ref (`ref: main` on actions/checkout) or the environment passes.
# Those are reviewed in the PR like any other code.
#
# The YAML reader is line-based, and a line reader cannot interpret all of YAML.
# So the guard FAILS CLOSED: check.yaml must have the allowlisted block-style
# shape below, and anything the reader cannot read plainly (flow style, quoted
# keys, `on:` as a list, a duplicate key, a second document, a merge key, a
# multi-line or block scalar in a step) is refused as unsupported.
#   Events:  `on:` holds exactly `push` (only the text `branches: [main]`) and an
#            empty `pull_request`. No workflow-level `defaults:` or `env:`.
#   Context: every required context is the reported name (the `name:` if set,
#            else the job id) of a check.yaml job, and the ruleset has one.
#   Job:     that job holds only `name`, `runs-on`, `steps`, `permissions` and
#            `timeout-minutes`. Its steps hold only `name`, `uses`, `with`, `run`
#            and `id`. At least one step is exactly `run: make check`.
#   Unique:  no other job in any workflow reports the same name. In the other
#            workflows only job ids and names are inspected: a job must be block
#            style, and its `name:` must be a plain one-line scalar or a simply
#            quoted string. An expression name is refused only if its literal
#            prefix could still make it a required context.
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
WORKFLOW_KEY_REASONS = {
    "defaults": "`defaults.run.shell` can change how `make check` runs",
    "env": "an environment variable such as MAKEFLAGS can change what `make check` does",
}
JOB_KEYS = {"name", "runs-on", "steps", "permissions", "timeout-minutes"}
JOB_KEY_REASONS = {
    "if": "a job-level `if:` can skip it",
    "needs": "a skipped or failed dependency skips it",
    "continue-on-error": "it can fail without failing the check",
    **WORKFLOW_KEY_REASONS,
}
STEP_KEYS = {"name", "uses", "with", "run", "id"}
STEP_KEY_REASONS = {
    "if": "a step-level `if:` can skip it",
    "continue-on-error": "it can fail without failing the check",
    "shell": "a shell such as `bash -n {0}` can skip the command",
    "env": "an environment variable such as MAKEFLAGS can change what `make check` does",
}
PLAIN_NAME = re.compile(r"""[^\s|>&*!%@`{}\[\]"'#,?:-][^#]*|-[^\s#][^#]*""")


class Unsupported(Exception):
    pass


def fail(message: str) -> None:
    print(f"check workflow trigger check failed: {message}", file=sys.stderr)
    sys.exit(1)


def quoted_string(text: str) -> str | None:
    """The content of a simply quoted string, or None. The one rule for every
    quoted key and name: a double-quoted string with no backslash (an escape such
    as \\x6d decodes to another character), or a single-quoted string with no ''
    (the KEY pattern cannot match one, so such a key is a non-key line)."""
    if re.fullmatch(r'"[^"\\]*"', text) or re.fullmatch(r"'[^']*'", text):
        return text[1:-1]
    return None


class Line:
    def __init__(self, path: Path, number: int, raw: str):
        self.path, self.number = path, number
        self.where = f"{path}:{number}"
        self.indent = len(raw) - len(raw.lstrip(" "))
        self.text = raw.strip()
        match = KEY.match(raw)
        self.key = match["key"] if match else None
        self.quoted = bool(self.key) and self.key[0] in "\"'"
        # The value exactly as written, comment included. Nothing is stripped.
        self.value = (match["value"] or "").strip() if match else ""
        self.continued = False


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


def mapping(lines: list[Line], parent: int, what: str, quoted_ok: bool = False) -> dict[str, int]:
    """The block mapping below lines[parent], read strictly: every line at the
    child indent must be a `key:`, with no duplicate key. A quoted key is
    refused, or read unquoted when quoted_ok."""
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
        key = quoted_string(line.key) if line.quoted else line.key
        if key is None:
            raise Unsupported(
                f"{line.where}: quoted key {line.key} in {what} has an escape, so YAML may "
                "decode it to another key; write it unquoted"
            )
        if line.quoted and not quoted_ok:
            raise Unsupported(f"{line.where}: quoted key {line.key} in {what}; write it unquoted")
        if key in keys:
            raise Unsupported(f"{line.where}: duplicate key `{key}` in {what}")
        keys[key] = index
    return keys


def block_value(lines: list[Line], index: int, what: str) -> None:
    """Refuse a value written on the key's own line (flow style, an alias, a scalar)."""
    if lines[index].value and not lines[index].value.startswith("#"):
        raise Unsupported(
            f"{lines[index].where}: {what} is written inline (`{lines[index].text}`); "
            "write it as a block"
        )


def deeper(lines: list[Line], index: int) -> bool:
    return index + 1 < len(lines) and lines[index + 1].indent > lines[index].indent


def steps(lines: list[Line], parent: int) -> list[dict[str, Line]]:
    """The block sequence of block-mapping steps below lines[parent]. A key whose
    value has more-indented lines below it is marked `continued`."""
    body = block(lines, parent)
    if not body:
        raise Unsupported(f"{lines[parent].where}: `steps:` has no block sequence below it")
    item_indent = lines[body[0]].indent
    found: list[dict[str, Line]] = []
    key_column = None
    last = None
    for index in body:
        line = lines[index]
        if line.indent == item_indent:
            rest = line.text[2:].lstrip() if line.text.startswith("- ") else ""
            if not KEY.match(rest):
                raise Unsupported(f"{line.where}: step `{line.text}` is not a block-style `- key: value`")
            key_column = line.indent + len(line.text) - len(rest)
            found.append({})
            line = Line(line.path, line.number, " " * key_column + rest)
        elif key_column is not None and line.indent > key_column:
            if last is not None:
                last.continued = True
            continue
        elif line.indent != key_column or line.key is None:
            raise Unsupported(f"{line.where}: `{line.text}` in a step is not a block-style `key:`")
        if line.quoted:
            raise Unsupported(f"{line.where}: quoted key {line.key} in a step; write it unquoted")
        if line.key in found[-1]:
            raise Unsupported(f"{line.where}: duplicate key `{line.key}` in a step")
        found[-1][line.key] = last = line
    return found


def job_name(lines: list[Line], index: int, job_id: str, contexts: list[str]) -> str:
    """A job's `name:`, read only if it is plain enough to read."""
    line = lines[index]
    value = line.value
    if deeper(lines, index) or not value:
        raise Unsupported(f"{line.where}: job `{job_id}`'s `name:` is not on one line")
    if value[:1] in "\"'":
        name = quoted_string(value)
        if name is None:
            raise Unsupported(
                f"{line.where}: job `{job_id}`'s `name: {value}` is not a plain scalar or a simply "
                "quoted string"
            )
    elif PLAIN_NAME.fullmatch(value) and " #" not in value:
        name = value
    else:
        raise Unsupported(
            f"{line.where}: job `{job_id}`'s `name: {value}` is not a plain scalar or a simply "
            "quoted string"
        )
    if "${{" in name:
        prefix = name.split("${{", 1)[0]
        if any(context.startswith(prefix) for context in contexts):
            raise Unsupported(
                f"{line.where}: job `{job_id}`'s `name: {value}` is an expression that could "
                "evaluate to a required context"
            )
    return name


def jobs(path: Path, contexts: list[str], strict: bool) -> list[dict]:
    """Every job of a workflow with its reported name. In a non-strict read only
    job ids and names matter, so quoted keys are read rather than refused."""
    lines = load(path)
    root = mapping(lines, -1, f"{path}'s top level", quoted_ok=not strict)
    if "jobs" not in root:
        return []
    block_value(lines, root["jobs"], "`jobs:`")
    found = []
    for job_id, index in mapping(lines, root["jobs"], f"{path}'s jobs", quoted_ok=not strict).items():
        block_value(lines, index, f"job `{job_id}`")
        props = mapping(lines, index, f"job `{job_id}`", quoted_ok=not strict)
        name = job_name(lines, props["name"], job_id, contexts) if "name" in props else ""
        found.append({
            "id": job_id,
            "where": lines[index].where,
            "reported": name or job_id,
            "lines": lines,
            "props": props,
        })
    return found


def check_workflow(lines: list[Line]) -> None:
    root = mapping(lines, -1, f"{workflow}'s top level")
    for key, reason in WORKFLOW_KEY_REASONS.items():
        if key in root:
            failures.append(f"{lines[root[key]].where}: workflow-level `{key}:` is not allowed; {reason}")
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
        push = mapping(lines, events["push"], "`push:`")
        if "branches" not in push:
            failures.append(f"{lines[events['push']].where}: `push` has no `branches:`; it must hold exactly `branches: [main]`")
        for key, index in push.items():
            line = lines[index]
            if key != "branches":
                failures.append(f"{line.where}: `push` has `{key}`; it may hold only `branches: [main]`")
            elif line.value != "[main]" or deeper(lines, index):
                failures.append(f"{line.where}: `push` has `{line.text}`; it must be exactly `branches: [main]`")
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
            reason = JOB_KEY_REASONS.get(key, "only `name`, `runs-on`, `steps`, `permissions` and `timeout-minutes` are allowed")
            failures.append(f"{lines[index].where}: job `{job['id']}` reports required context `{context}` and has `{key}`; {reason}")
    make_check = 0
    if "steps" in props:
        block_value(lines, props["steps"], f"job `{job['id']}`'s `steps:`")
        for keys in steps(lines, props["steps"]):
            for key, line in keys.items():
                if key not in STEP_KEYS:
                    reason = STEP_KEY_REASONS.get(key, "only `name`, `uses`, `with`, `run` and `id` are allowed")
                    failures.append(f"{line.where}: a step of job `{job['id']}` has `{key}`; {reason}")
                # A comment-only value (`with: # note`) is empty, as in block_value().
                if line.value and not line.value.startswith("#") and (line.continued or line.value[0] in "|>"):
                    raise Unsupported(
                        f"{line.where}: `{key}: {line.value}` in a step of job `{job['id']}` "
                        "continues on more lines; write it as one plain line"
                    )
            if "run" in keys and keys["run"].value == "make check":
                make_check += 1
    if make_check == 0:
        failures.append(f"{job['where']}: job `{job['id']}` has no step that is exactly `run: make check`")


def main() -> None:
    if not workflow.is_file():
        fail(f"workflow not found: {workflow}")
    check_workflow(load(workflow))

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

    own_jobs = jobs(workflow, contexts, strict=True)
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
        all_jobs.extend(own_jobs if path.resolve() == workflow.resolve() else jobs(path, contexts, strict=False))
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
