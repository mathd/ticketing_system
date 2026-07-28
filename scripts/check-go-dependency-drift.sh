#!/usr/bin/env bash
# TKT-129 / ADR-035 — one version per shared Go dependency declaration.
#
# The repo has eight modules under one `go.work` for one deploy unit, so every
# supported build already resolves a single MVS list: a divergent `require` is
# *declaration* drift, not runtime drift. It still matters — the manifests stop
# describing what is actually built, review loses the blast radius of a bump, and
# the divergence becomes real for anything built outside the workspace. Nothing
# self-heals: `go mod tidy` across all eight modules is a no-op on a drifted tree.
#
# Reads manifests only. `go mod edit -json` parses go.mod without loading packages,
# downloading modules or consulting the workspace, so this stays offline and fast
# (~0.2s for eight modules) — the gate budget from TKT-42 is not spent here.
#
# A dependency declared by a single module is not drift: forcing every module to
# pin dependencies it does not import would fight `go mod tidy` and misdescribe
# which packages actually use what.
set -euo pipefail

cd "$(dirname "$0")/.."

if [ "$#" -eq 0 ]; then
  echo "usage: $(basename "$0") <module-dir>..." >&2
  exit 2
fi

# "<dependency> <version> <module>", one line per require tuple.
#
# Each module is read into a file and its status checked explicitly. Do NOT fold
# this loop into a pipeline (`for ...; done | sort`): the loop then runs as a
# pipeline component, `errexit` does not stop it, and the pipeline reports the
# *last* command's status — so a module whose `go mod edit` failed is silently
# skipped and the run still exits 0. A checker that reports "none" for a module
# it never read passes forever, which is worse than having no checker at all.
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM
: > "$work/records"

for m in "$@"; do
  if ! go mod edit -json "$m/go.mod" > "$work/module.json"; then
    echo "check-go-dependency-drift: cannot read $m/go.mod — refusing to report a verdict" >&2
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
  echo "check-go-dependency-drift: no require declarations found in: $*" >&2
  exit 2
fi

records=$(LC_ALL=C sort -u "$work/records")

# A dependency path that survives `sort -u` on (path, version) more than once is
# declared at more than one version.
drifted=$(printf '%s\n' "$records" | cut -d' ' -f1,2 | LC_ALL=C sort -u | cut -d' ' -f1 | LC_ALL=C uniq -d)

if [ -n "$drifted" ]; then
  echo "go dependency drift: the same dependency is declared at different versions" >&2
  for dep in $drifted; do
    echo "  $dep" >&2
    printf '%s\n' "$records" | awk -v d="$dep" '$1 == d { printf "    %-26s %s\n", $3, $2 }' >&2
  done
  cat >&2 <<'EOF'

Modules sharing a deploy unit declare one version each (ADR-035). Realign with:

    go work sync

then commit the resulting go.mod/go.sum changes.
EOF
  exit 1
fi

echo "go dependency drift: none across $# module(s)"
