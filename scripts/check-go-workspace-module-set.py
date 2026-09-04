import json
import os
import subprocess
import sys


def refuse(checker: str, message: str) -> None:
    print(f"{checker}: {message} -- refusing to report a verdict", file=sys.stderr)
    raise SystemExit(2)


if len(sys.argv) < 4:
    raise SystemExit("usage: check-go-workspace-module-set.py <checker> <repo-root> <module-dir>...")

checker, root, *arguments = sys.argv[1:]
parsed = subprocess.run(
    ["go", "work", "edit", "-json"],
    cwd=root,
    capture_output=True,
    text=True,
    env={**os.environ, "GOWORK": os.path.join(root, "go.work")},
)
if parsed.returncode != 0:
    if parsed.stderr:
        print(parsed.stderr.rstrip(), file=sys.stderr)
    refuse(checker, "cannot parse go.work")

try:
    workspace_document = json.loads(parsed.stdout)
except json.JSONDecodeError:
    refuse(checker, "go.work produced invalid JSON")

workspace_entries = workspace_document.get("Use") or []
if not workspace_entries:
    refuse(checker, "go.work names no modules")


def canonical(path: str) -> str:
    return os.path.realpath(path if os.path.isabs(path) else os.path.join(root, path))


workspace_paths = [canonical(entry["DiskPath"]) for entry in workspace_entries]
argument_paths = [canonical(argument) for argument in arguments]

duplicate_workspace = sorted({path for path in workspace_paths if workspace_paths.count(path) > 1})
duplicate_arguments = sorted({path for path in argument_paths if argument_paths.count(path) > 1})
missing = sorted(set(workspace_paths) - set(argument_paths))
extra = sorted(set(argument_paths) - set(workspace_paths))

if not (duplicate_workspace or duplicate_arguments or missing or extra):
    raise SystemExit(0)

print(f"{checker}: module arguments do not exactly match go.work", file=sys.stderr)
for label, paths in (
    ("duplicate workspace path", duplicate_workspace),
    ("duplicate argument path", duplicate_arguments),
    ("missing argument", missing),
    ("extra argument", extra),
):
    for path in paths:
        print(f"  {label}: {os.path.relpath(path, root)}", file=sys.stderr)
print("  refusing to report a verdict", file=sys.stderr)
raise SystemExit(2)
