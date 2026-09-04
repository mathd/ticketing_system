#!/usr/bin/env bash
# Every repository-relative Markdown link must resolve to a file that exists.
#
# ADR-062 linked to `ADR-010-inventory-claim-transaction.md` while the file has
# always been `ADR-010-postgres-claim-transaction.md`. Nothing in the gate
# resolves a link target, so the inherited locking decision was simply
# unreachable and stayed that way. Documentation here is load-bearing —
# AGENTS.md routes work through it — so a dead cross-reference is a defect.
#
# Deliberately LOCAL and network-free: external URLs are not fetched, because a
# gate that depends on someone else's uptime fails for reasons that are not
# about this repository.
#
# One awk pass over all tracked Markdown, not a subshell per line: this runs on
# every `make check`, and a gate stage nobody wants to wait for is a gate stage
# that gets removed.
set -euo pipefail

# Files come from git, so untracked scratch files and node_modules are out of
# scope by construction rather than by an exclude list that drifts.
# A tracked file that has been deleted in the working tree is still listed by
# `git ls-files`, and awk exits 2 on the first unreadable input. A checker that
# DIES rather than reporting is worse than one that misses: it fails the gate
# with a message about awk instead of about the documentation. Read only what
# is actually there, and say so.
mapfile -t files < <(git ls-files '*.md' | while IFS= read -r f; do [[ -f "$f" ]] && printf '%s\n' "$f"; done)

if (( ${#files[@]} == 0 )); then
  echo "markdown link check failed: no readable tracked Markdown files" >&2
  exit 1
fi

broken=$(
  awk '
    FNR == 1 {
      dir = FILENAME
      # dirname
      if (sub(/\/[^\/]*$/, "", dir) == 0) dir = "."
    }

    # The ADR template ships `](link)` placeholders. They are not links.
    FILENAME == "docs/adr/[template].md" { next }

    {
      rest = $0
      # Both inline spellings: `](target)` and the angle-bracket `](<target>)`
      # form, which this repo uses for paths containing brackets, e.g.
      # venues/[id].astro.
      while (match(rest, /\]\(<[^>]*>\)|\]\([^)( ]*\)/)) {
        raw = substr(rest, RSTART, RLENGTH)
        rest = substr(rest, RSTART + RLENGTH)

        target = raw
        sub(/^\]\(</, "", target)
        sub(/^\]\(/, "", target)
        sub(/>\)$/, "", target)
        sub(/\)$/, "", target)

        if (target == "") continue
        # External and non-file schemes are out of scope (network-free gate).
        if (target ~ /^(https?|mailto|tel|ftp):/) continue
        # A pure anchor points inside the same file; no path to resolve.
        if (target ~ /^#/) continue

        # Drop the fragment: the file must exist; anchors are not resolved.
        # Resolving headings would mean reproducing GitHub'"'"'s slug rules, a
        # different and much less certain check than "does the target exist".
        path = target
        sub(/#.*$/, "", path)
        if (path == "") continue

        # A repository-absolute link is rooted at the repo, not the filesystem.
        resolved = (path ~ /^\//) ? "." path : dir "/" path

        if (!(resolved in seen)) {
          # `test -e` covers files and directories both.
          seen[resolved] = (system("test -e \"" resolved "\"") == 0)
        }
        if (!seen[resolved]) {
          printf "%s:%d: link target does not exist: %s\n", FILENAME, FNR, target
        }
      }
    }
  ' "${files[@]}"
)

if [[ -n "$broken" ]]; then
  echo "$broken" >&2
  echo "markdown link check failed: $(printf '%s\n' "$broken" | wc -l) broken link(s)" >&2
  exit 1
fi

echo "markdown links resolve: ${#files[@]} tracked files checked"
