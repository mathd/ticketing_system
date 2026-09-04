#!/usr/bin/env bash
# TKT-265 / ADR-035 — no manifest may declare LESS than the workspace selects.
#
# The sibling checker (check-go-dependency-drift.sh) closes the HORIZONTAL case:
# two modules declaring different versions of the same dependency. This closes the
# VERTICAL one: a module declaring a version BELOW the one `go.work`'s MVS build
# list actually selects. MVS can raise a selected version through a transitive
# requirement without any `go.mod` line changing, and when it does every manifest
# still reads as internally consistent while describing a build that is not what
# happens. `go mod tidy` does not correct it and the horizontal checker cannot see
# it — the versions agree with each other, they just all agree on the wrong number.
#
# WHY THIS IS A SEPARATE SCRIPT. The sibling's documented contract is manifest-only
# and OFFLINE (`go mod edit -json` parses go.mod without resolving the workspace or
# touching the network). This check must resolve the build list, so it needs
# `go list -m`, which walks the module graph and CANNOT run under GOPROXY=off on a
# cold cache. Folding the two together would falsify the sibling's contract in its
# header, in the Makefile comment and in ADR-035. They stay separate on purpose.
#
# The gate as a whole already requires the network on a cold machine — `make check`
# runs `deps: pnpm install --frozen-lockfile` before any of this — so the property
# being spent here belongs to one script, not to the gate. ADR-035's amendment
# records that trade explicitly.
#
# Absent is not lagging. A module that does not declare a dependency at all is
# correct (`go mod tidy` removes what a module does not import); only a DECLARED
# version below the selected one is a defect.
set -euo pipefail

cd "$(dirname "$0")/.."

diagnostic_workspace_copy=""
if [ "${1:-}" = "--diagnostic-workspace-copy" ]; then
  if [ "$#" -lt 3 ] || [ -z "$2" ]; then
    echo "usage: $(basename "$0") [--diagnostic-workspace-copy <path>] <module-dir>..." >&2
    exit 2
  fi
  diagnostic_workspace_copy="$2"
  shift 2
fi

if [ "$#" -eq 0 ]; then
  echo "usage: $(basename "$0") [--diagnostic-workspace-copy <path>] <module-dir>..." >&2
  exit 2
fi

# The build list comes from the whole workspace, so comparing declarations from
# only a caller-selected subset would produce a true answer about the wrong set.
# Refuse missing, extra, or duplicate canonical module paths before resolving it.
python3 scripts/check-go-workspace-module-set.py check-go-build-list-lag "$PWD" "$@"

work=$(mktemp -d)
# Cleanup on EXIT only. Bash *resumes* after an INT/TERM handler that does not
# itself exit, so a cleanup-only handler on those signals would delete the work
# directory and then let the run continue to print a verdict and exit 0.
trap 'rm -rf "$work"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# EVERY `go list -m` below runs against a COPY of go.work, never the real one.
#
# Resolving the module graph makes Go write any checksums it learns into
# `go.work.sum`. On a cold or incomplete cache that MUTATES a tracked file, leaving
# a gate-only diff and falsifying the "never mutates the tree" property this check
# is supposed to have. `-mod=readonly` does NOT prevent it — the write is to the
# workspace sum file, which that flag does not govern. Both facts verified by
# execution; the first version of this script mutated `go.work.sum` on a seeded
# tree, and so did its own replace-detection probe.
#
# Pointing GOWORK at a temporary copy sends any such write to the copy, discarded
# with $work. The copy's `use` paths are relative to the original location, so they
# are absolutized.
# The copy is built from Go's OWN parse of go.work (`go work edit -json`), not by
# rewriting its text. A `use` path may be written `./x`, bare `x`, `.`, or quoted
# with spaces, and a text substitution that handles only the form this repo happens
# to use today would leave the others relative to $work. That fails closed rather
# than silently inspecting the wrong module set — $work contains no modules, so
# resolution errors — but "fails closed on a valid workspace" is still a broken
# check, and the ADR claims a general guarantee. Parsing removes the class.
#
# Workspace-level `replace` directives with relative targets need the same
# treatment, for the same reason.
if ! GOWORK="$PWD/go.work" go work edit -json > "$work/workspace.json" 2>"$work/workspace.err"; then
  echo "check-go-build-list-lag: cannot parse go.work — refusing to report a verdict" >&2
  sed 's/^/  /' "$work/workspace.err" >&2
  exit 2
fi
# The copy is the ORIGINAL FILE, with only its filesystem paths redirected by
# `go work edit` — Go's own editor, operating on Go's own syntax. Regenerating the
# file from the JSON was the previous approach and was wrong in a way the fidelity
# guard could not see: `go work edit -json` exposes only Go, Use and Replace, so a
# `godebug` line (and anything a future release adds) was silently dropped from the
# copy while the module directories still matched. Directives that change build
# semantics must survive, and the only way not to enumerate them is not to rewrite
# them.
cp "$PWD/go.work" "$work/go.work"
# The edit arguments go through a NUL-delimited FILE, not a shell variable: bash
# command substitution silently strips null bytes, and a path containing whitespace
# would then be re-split into two arguments.
if ! python3 - "$work/workspace.json" "$PWD" "$work/editargs" <<'PY'
import json, os, sys
doc = json.load(open(sys.argv[1]))
root = sys.argv[2]
args = []
def absolutize(p):
    return p if os.path.isabs(p) else os.path.normpath(os.path.join(root, p))
# Drops name the path EXACTLY as `go work edit -json` reported it, which is exactly
# as the file spells it; adds name the absolutised form. Every entry is dropped
# before any is added, so the two sets cannot interleave.
#
# Do not "normalise" the drop spelling. `-dropuse` matches the literal written
# path: `-dropuse=./gateway` does NOT remove a bare `use gateway`, and
# `-dropuse=<abs>` removes neither. Both were tried; each left the entry in place
# and the copy then held it twice, once absolute and once still relative. The
# fidelity guard below is what caught it, which is the reason it compares
# directories rather than trusting the rewrite.
#
# KNOWN LIMIT, deliberately not closed: a hand-written bare `use gateway` (no
# "./") cannot be dropped by any spelling `go work edit` accepts, so this stage
# REFUSES on such a workspace instead of checking it. That is fail-closed and
# loud, not a wrong verdict. It is left open because Go's own tooling never
# writes that form — `go work use m` emits `use ./m` — so reaching it requires
# hand-editing go.work into a spelling the toolchain does not produce.
for u in (doc.get("Use") or []):
    args.append("-dropuse=" + u["DiskPath"])
for u in (doc.get("Use") or []):
    args.append("-use=" + absolutize(u["DiskPath"]))
for r in (doc.get("Replace") or []):
    old, new = r["Old"], r["New"]
    o = old["Path"] + ("@" + old["Version"] if old.get("Version") else "")
    # Only a target with NO version is a filesystem path; anything else names a
    # module and must not be touched.
    if not new.get("Version") and not os.path.isabs(new["Path"]) \
            and (new["Path"].startswith("./") or new["Path"].startswith("../")):
        args += ["-dropreplace=" + o, "-replace=" + o + "=" + absolutize(new["Path"])]
with open(sys.argv[3], "wb") as fh:
    fh.write(b"\0".join(a.encode() for a in args))
PY
then
  echo "check-go-build-list-lag: cannot plan the workspace copy rewrite — refusing to report a verdict" >&2
  exit 2
fi
if [ -s "$work/editargs" ]; then
  if ! GOWORK="$work/go.work" xargs -0 go work edit < "$work/editargs"; then
    echo "check-go-build-list-lag: cannot rewrite the workspace copy — refusing to report a verdict" >&2
    exit 2
  fi
fi
[ -f "$PWD/go.work.sum" ] && cp "$PWD/go.work.sum" "$work/go.work.sum"
export GOWORK="$work/go.work"

# The copy must place its `use` modules at the SAME directories as the real
# workspace. If the rewrite were unfaithful, every verdict below would be about a
# different module set — so prove it rather than assume it. Compared as sorted
# absolute paths: the copy is generated from Go's own parse, so this asserts the
# generation, not the parser.
GOWORK="$PWD/go.work" go work edit -json | python3 -c "
import json,sys,os
root=os.getcwd()
d=json.load(sys.stdin)
for u in (d.get('Use') or []):
    p=u['DiskPath']
    print(p if os.path.isabs(p) else os.path.normpath(os.path.join(root,p)))
" 2>/dev/null | LC_ALL=C sort > "$work/want-dirs"
GOWORK="$work/go.work" go work edit -json | python3 -c "
import json,sys,os
d=json.load(sys.stdin)
for u in (d.get('Use') or []):
    print(os.path.normpath(u['DiskPath']))
" 2>/dev/null | LC_ALL=C sort > "$work/got-dirs"
# Non-emptiness is checked FIRST. Both lists are produced with stderr discarded, so
# a failure on either side yields an empty file — and empty compares equal to empty,
# which would pass this guard while proving nothing. That is the same vacuity this
# check exists to prevent, one level up.
if [ ! -s "$work/want-dirs" ] || [ ! -s "$work/got-dirs" ]; then
  echo "check-go-build-list-lag: cannot enumerate the workspace's module directories — refusing to report a verdict" >&2
  exit 2
fi
if ! cmp -s "$work/want-dirs" "$work/got-dirs"; then
  echo "check-go-build-list-lag: the workspace copy does not name the repo's module directories — refusing to report a verdict" >&2
  diff "$work/want-dirs" "$work/got-dirs" | sed 's/^/  /' >&2 || true
  exit 2
fi

# The self-test can inspect the exact workspace copy used below. The checker
# continues after writing it, so a diagnostic cannot hide a refused or lagging
# verdict.
if [ -n "$diagnostic_workspace_copy" ]; then
  if ! cp "$work/go.work" "$diagnostic_workspace_copy"; then
    echo "check-go-build-list-lag: cannot write diagnostic workspace copy to $diagnostic_workspace_copy" >&2
    exit 2
  fi
  echo "check-go-build-list-lag: wrote diagnostic workspace copy to $diagnostic_workspace_copy"
fi

# External replacements make version comparison meaningless. A narrow exception
# is safe for an exact path to another module already named by this workspace:
# its module identity and source directory can both be verified. Those local
# requirements exist so each module also resolves with GOWORK=off.
: > "$work/replaces"
if ! python3 - "$work/workspace.json" "$PWD" "$work/workspace-module-paths" "$@" > "$work/replaces" <<'PY'
import json, os, subprocess, sys

workspace_file, root, paths_file, *modules = sys.argv[1:]
documents = [("go.work", root, json.load(open(workspace_file)))]
workspace_modules = {}
for module in modules:
    path = os.path.join(root, module, "go.mod")
    parsed = subprocess.run(
        ["go", "mod", "edit", "-json", path], capture_output=True, text=True
    )
    if parsed.returncode != 0:
        raise SystemExit(parsed.returncode)
    document = json.loads(parsed.stdout)
    module_path = document.get("Module", {}).get("Path")
    if not module_path:
        raise SystemExit(2)
    workspace_modules[module_path] = os.path.realpath(os.path.join(root, module))
    documents.append((module, os.path.join(root, module), document))

with open(paths_file, "w") as output:
    for module_path in sorted(workspace_modules):
        output.write(module_path + "\n")

for label, base, document in documents:
    for replacement in document.get("Replace") or []:
        old, new = replacement["Old"], replacement["New"]
        target = os.path.realpath(os.path.join(base, new["Path"]))
        safe = (
            not old.get("Version")
            and not new.get("Version")
            and workspace_modules.get(old["Path"]) == target
        )
        if not safe:
            print(f"  {label}: {old['Path']} => {new['Path']}")
PY
then
  echo "check-go-build-list-lag: cannot validate replace directives — refusing to report a verdict" >&2
  exit 2
fi
if [ -s "$work/replaces" ]; then
  echo "check-go-build-list-lag: a 'replace' directive is in effect — refusing to report a verdict" >&2
  echo "  A replaced module can match on version while linking different source, so a version" >&2
  echo "  comparison cannot speak to what is built. Remove the replace, or extend this check to" >&2
  echo "  compare effective module identity (ADR-035 §Amendment)." >&2
  cat "$work/replaces" >&2
  exit 2
fi

# The selected build list, one "<path> <version>" per line. Failing to resolve it
# is FAIL-CLOSED and distinct from finding lag: a proxy outage or a cold cache is
# not a source defect, and reporting "no lag" because the graph never loaded is the
# failure mode this whole ticket exists to close.
if ! go list -mod=readonly -m -f '{{.Path}} {{.Version}}' all > "$work/selected" 2>"$work/selected.err"; then
  echo "check-go-build-list-lag: cannot resolve the workspace build list — refusing to report a verdict" >&2
  echo "  (this stage resolves the module graph and needs the module cache or network; see ADR-035)" >&2
  sed 's/^/  /' "$work/selected.err" >&2
  exit 2
fi
if [ ! -s "$work/selected" ]; then
  echo "check-go-build-list-lag: empty build list — refusing to report a verdict" >&2
  exit 2
fi
# A path selected twice means the list is not what this check assumes it is, and
# the first-match lookup below would silently pick one. Refuse instead.
if dupes=$(cut -d' ' -f1 "$work/selected" | LC_ALL=C sort | LC_ALL=C uniq -d) && [ -n "$dupes" ]; then
  echo "check-go-build-list-lag: the build list selects a path more than once — refusing to report a verdict" >&2
  printf '%s\n' "$dupes" | sed 's/^/  /' >&2
  exit 2
fi

# Each module is read into a file and its status checked explicitly. Do NOT fold
# this loop into a pipeline (`for ...; done | sort`): the loop then runs as a
# pipeline component, `errexit` does not stop it, and the pipeline reports the
# *last* command's status — so a module whose `go mod edit` failed is silently
# skipped and the run still exits 0. A checker that reports "none" for a module it
# never read passes forever, which is worse than having no checker at all.
: > "$work/records"
for m in "$@"; do
  if ! go mod edit -json "$m/go.mod" > "$work/module.json"; then
    echo "check-go-build-list-lag: cannot read $m/go.mod — refusing to report a verdict" >&2
    exit 2
  fi
  # A zero exit does not prove the JSON arrived whole. Emptiness cannot be the
  # signal — `gateway` legitimately reports "Require": null — so check for the
  # closing brace instead. A partially read module must never reach a verdict.
  if [ "$(tail -n 1 "$work/module.json")" != "}" ]; then
    echo "check-go-build-list-lag: truncated go mod edit output for $m/go.mod — refusing to report a verdict" >&2
    exit 2
  fi
  awk -v mod="$m" '
    /"Require": \[/       { req = 1; next }
    req && /^\t\]/        { req = 0 }
    req && /"Path": "/    { p = $0; sub(/^.*"Path": "/, "", p);    sub(/".*$/, "", p); next }
    req && /"Version": "/ { v = $0; sub(/^.*"Version": "/, "", v); sub(/".*$/, "", v)
                            if (p != "") { print p, v, mod; p = "" } }
  ' "$work/module.json" >> "$work/records"
done

if [ ! -s "$work/records" ]; then
  echo "check-go-build-list-lag: no require declarations found in: $*" >&2
  exit 2
fi

# Compare each declaration against the selected version for the same path.
#
# MVS never selects BELOW a declared requirement — the selected version is by
# definition the maximum over everything that requires the path. So for any
# declaration that differs from the selected version, the declaration is the
# lower one, and a plain inequality is the whole test. No version ordering is
# performed here on purpose: a hand-rolled semver comparison that is subtly
# wrong in a guard fails OPEN, and this check does not need one.
: > "$work/lagging"
: > "$work/uncompared"
while read -r path version mod; do
  if grep -Fqx "$path" "$work/workspace-module-paths"; then
    continue
  fi
  selected=$(awk -v p="$path" '$1 == p { print $2; exit }' "$work/selected")
  if [ -z "$selected" ]; then
    # A declared requirement absent from the build list is NOT evidence of safety.
    # Today every declaration resolves, so this never fires; if it starts firing,
    # something about graph resolution changed and a lagging declaration could
    # vanish from comparison and produce a PASS. Silently continuing here is the
    # same fail-open shape this whole check exists to close, so collect and refuse.
    printf '%s %s %s\n' "$path" "$version" "$mod" >> "$work/uncompared"
    continue
  fi
  [ "$version" = "$selected" ] && continue
  printf '%s %s %s %s\n' "$path" "$version" "$selected" "$mod" >> "$work/lagging"
done < "$work/records"

if [ -s "$work/uncompared" ]; then
  echo "check-go-build-list-lag: a declared requirement is absent from the build list — refusing to report a verdict" >&2
  echo "  Every declaration must be comparable, or a lagging one could go unexamined:" >&2
  awk '{ printf "    %-45s %-26s declares %s\n", $1, $3, $2 }' "$work/uncompared" >&2
  exit 2
fi

if [ -s "$work/lagging" ]; then
  echo "go build-list lag: a module declares less than the workspace selects" >&2
  LC_ALL=C sort -u "$work/lagging" | awk '{ printf "  %-45s %-26s declares %-12s selected %s\n", $1, $4, $2, $3 }' >&2
  cat >&2 <<'EOF'

The manifest no longer describes what is built: MVS selects the higher version
for every build in this workspace, so a reviewer reading the go.mod sees a
version that is not what links (ADR-035). Realign with:

    go work sync

then commit the resulting go.mod/go.sum changes.
EOF
  exit 1
fi

echo "go build-list lag: none across $# module(s)"
