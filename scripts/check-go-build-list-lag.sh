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

if [ "$#" -eq 0 ]; then
  echo "usage: $(basename "$0") <module-dir>..." >&2
  exit 2
fi

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
if ! go work edit -json > "$work/workspace.json" 2>"$work/workspace.err"; then
  echo "check-go-build-list-lag: cannot parse go.work — refusing to report a verdict" >&2
  sed 's/^/  /' "$work/workspace.err" >&2
  exit 2
fi
if ! python3 - "$work/workspace.json" "$PWD" "$work/go.work" <<'PY'
import json, os, sys
doc = json.load(open(sys.argv[1]))
root, out = sys.argv[2], sys.argv[3]
lines = []
if doc.get("Go"):
    lines.append("go %s" % doc["Go"])
if doc.get("Toolchain"):
    lines.append("toolchain %s" % doc["Toolchain"])
def absolutize(p):
    return p if os.path.isabs(p) else os.path.normpath(os.path.join(root, p))
for u in (doc.get("Use") or []):
    lines.append('use "%s"' % absolutize(u["DiskPath"]))
for r in (doc.get("Replace") or []):
    old, new = r["Old"], r["New"]
    o = old["Path"] + ("@" + old["Version"] if old.get("Version") else "")
    # A replacement target with no version is a filesystem path; anything else is
    # a module path and must NOT be touched.
    n = new["Path"] + ("@" + new["Version"] if new.get("Version") else "")
    if not new.get("Version") and (new["Path"].startswith(".") or new["Path"].startswith("/")):
        n = '"%s"' % absolutize(new["Path"])
    lines.append("replace %s => %s" % (o, n))
open(out, "w").write("\n".join(lines) + "\n")
PY
then
  echo "check-go-build-list-lag: cannot rewrite the workspace copy — refusing to report a verdict" >&2
  exit 2
fi
[ -f "$PWD/go.work.sum" ] && cp "$PWD/go.work.sum" "$work/go.work.sum"
export GOWORK="$work/go.work"

# The copy must place its `use` modules at the SAME directories as the real
# workspace. If the rewrite were unfaithful, every verdict below would be about a
# different module set — so prove it rather than assume it. Compared as sorted
# absolute paths: the copy is generated from Go's own parse, so this asserts the
# generation, not the parser.
go work edit -json | python3 -c "
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

# A `replace` directive makes the comparison below meaningless: the selected
# VERSION can match the declared one while the build links entirely different
# source (another version, or a local directory). Comparing versions would then
# report "none" and imply a guarantee that is false — the manifest would not
# describe what is built, which is the very property this check exists to defend.
# Refuse rather than report. ADR-035's register scopes the guarantee to
# unreplaced modules for the same reason.
# Detected from the DECLARATIONS (go.work and every go.mod), not from
# `go list -m all`. That command reports a replacement only for a module already in
# the build list, so a replace naming a module nothing currently requires is
# invisible to it — a false negative that would let the next requirement of that
# module land silently replaced. Read the manifests instead.
: > "$work/replaces"
go work edit -json 2>/dev/null | python3 -c "
import json,sys
d=json.load(sys.stdin)
for r in (d.get('Replace') or []):
    print('  go.work: %s => %s' % (r['Old']['Path'], r['New']['Path']))
" >> "$work/replaces" 2>/dev/null || true
for m in "$@"; do
  go mod edit -json "$m/go.mod" 2>/dev/null | python3 -c "
import json,sys
d=json.load(sys.stdin)
for r in (d.get('Replace') or []):
    print('  $m: %s => %s' % (r['Old']['Path'], r['New']['Path']))
" >> "$work/replaces" 2>/dev/null || true
done
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
