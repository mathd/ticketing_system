#!/usr/bin/env bash
# Prove each guard blocks what it claims to and passes everything else. A guard
# that fails open is silent, so it gets the same treatment as the gate: seed the
# violation, observe the refusal. The "allowed" cases carry equal weight — an
# over-blocking guard gets switched off, not fixed — and where several rules can
# refuse the same command, assert on the REASON: a guard that blocked everything
# for one reason would satisfy every exit-code case here.
#
# The hook cases run against a throwaway git repo, never this one, so the result
# does not depend on whether a gate is running here — which is also what lets
# `make check-all` call this while holding .gate.lock.
set -euo pipefail

HOOKS="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(git -C "$HOOKS" rev-parse --show-toplevel)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM
git -C "$TMP" init -q
printf 'package main\n' > "$TMP/main.go"
# The merge guard shells out to the wrapper for a tree digest; give the throwaway
# repo a real copy, so the digest cases cannot pass for the wrong reason. That
# copy and the verdict must be IGNORED there: left untracked they are a dirty
# tree, and the dirty rule would refuse every later case before it reached the
# rule under test — which is what the reason assertions are for.
mkdir -p "$TMP/scripts"
cp "$ROOT/scripts/gate.sh" "$TMP/scripts/gate.sh"
printf 'scripts/\n.gate.*\n' > "$TMP/.gitignore"
git -C "$TMP" add main.go .gitignore
LOCK="$TMP/.gate.lock"

fail=0
run() { # run <expected-exit> <script> <payload> <label>
  set +e; echo "$3" | "$HOOKS/$2" >/dev/null 2>&1; got=$?; set -e
  if [ "$got" != "$1" ]; then echo "FAIL: $4 (expected exit $1, got $got)"; fail=1
  else echo "ok: $4"; fi
}
blocks_with() { # blocks_with <script> <payload> <substring> <label>
  set +e; msg="$(echo "$2" | "$HOOKS/$1" 2>&1 >/dev/null)"; got=$?; set -e
  if [ "$got" != 2 ]; then echo "FAIL: $4 (expected exit 2, got $got)"; fail=1
  elif ! grep -qF "$3" <<<"$msg"; then echo "FAIL: $4 (refused, but for: $msg)"; fail=1
  else echo "ok: $4"; fi
}
expect() { # expect <expected-exit> <label> <cmd...>
  set +e; "${@:3}" >/dev/null 2>&1; got=$?; set -e
  if [ "$got" != "$1" ]; then echo "FAIL: $2 (expected exit $1, got $got)"; fail=1
  else echo "ok: $2"; fi
}
b() { printf '{"tool_input":{"command":%s},"cwd":"%s"}' "$(jq -Rn --arg c "$1" '$c')" "$TMP"; }
e() { printf '{"tool_input":{"file_path":%s}}' "$(jq -Rn --arg p "$1" '$p')"; }

echo "-- the gate cannot run without its lock (the Makefile boundary)"
# The mechanism: a target with no prerequisites, so testing it cannot cascade
# into a real gate run. The wiring: that `check-all` actually depends on it —
# a mechanism proven to work while nothing calls it is the TKT-202 shape.
grep -qE '^check-all: gate-lock-held ' "$ROOT/Makefile" \
  && echo "ok: check-all is wired to the lock assertion" \
  || { echo "FAIL: check-all no longer requires gate-lock-held"; fail=1; }
grep -qE 'make -j1 check-all' "$ROOT/scripts/gate.sh" \
  && echo "ok: the gate forces serial stages" \
  || { echo "FAIL: gate.sh no longer passes -j1 — generate can race lint/test"; fail=1; }
# GATE_LOCK points at the throwaway repo, so a real gate running here cannot have
# its lock overwritten or removed by this test.
printf 'tok-good stamp\n' > "$LOCK"
expect 2 "stages refused with no token" env -u GATE_TOKEN make -C "$ROOT" GATE_LOCK="$LOCK" gate-lock-held
expect 2 "stages refused on a foreign lock" env GATE_TOKEN=tok-other make -C "$ROOT" GATE_LOCK="$LOCK" gate-lock-held
expect 0 "stages allowed for this run token" env GATE_TOKEN=tok-good make -C "$ROOT" GATE_LOCK="$LOCK" gate-lock-held
rm -f "$LOCK"

echo "-- nothing runs while the gate holds the lock, beyond waiting and reading"
date -u +%FT%TZ > "$LOCK"
run 0 guard-bash.py "$(b 'cat .gate.log')"                 "cat the log allowed"
run 0 guard-bash.py "$(b 'tail -20 .gate.verdict')"        "tail the verdict allowed"
run 0 guard-bash.py "$(b "grep -c '^ok:' .gate.log")"      "grep the log allowed"
run 0 guard-bash.py "$(b 'git status --porcelain')"        "git status allowed"
run 0 guard-bash.py "$(b 'while [ -e .gate.lock ]; do sleep 5; done')" "documented wait loop allowed"
run 0 guard-bash.py "$(b 'until [ ! -e .gate.lock ]; do sleep 5; done')" "until-form wait loop allowed"
run 2 guard-bash.py "$(b 'cat main.go')"                   "reading anything else blocked"
run 2 guard-bash.py "$(b 'find . -type f -delete')"        "find -delete blocked"
run 2 guard-bash.py "$(b 'find . -type f -exec touch {} +')" "find -exec blocked"
run 2 guard-bash.py "$(b 'sort -o main.go main.go')"       "sort -o blocked"
run 2 guard-bash.py "$(b 'uniq input.txt main.go')"        "uniq second-arg write blocked"
run 2 guard-bash.py "$(b 'git diff --output=main.go')"     "git diff --output blocked"
run 2 guard-bash.py "$(b 'git stash push -- list')"        "git stash push blocked"
run 2 guard-bash.py "$(b "sed -i '' s/a/b/ main.go")"      "sed -i blocked"
run 2 guard-bash.py "$(b 'echo x > main.go')"              "redirection blocked"
run 2 guard-bash.py "$(b 'pnpm install')"                  "unknown command blocked"
rm -f "$LOCK"
run 0 guard-bash.py "$(b 'pnpm install')"                  "same command allowed with no lock"

echo "-- the merge must contain the work"
git -C "$TMP" -c user.email=t@t -c user.name=t commit -qm init
printf 'package main // edited\n' > "$TMP/main.go"
MERGE='gh pr merge --squash'
blocks_with guard-bash.py "$(b "$MERGE")" "commit or stash them first" "merge with uncommitted tracked change blocked"
git -C "$TMP" checkout -q -- main.go
printf 'print("hi")\n' > "$TMP/tool.py"
blocks_with guard-bash.py "$(b "$MERGE")" "commit or stash them first" "merge with untracked source blocked"
rm -f "$TMP/tool.py"
blocks_with guard-bash.py "$(b "$MERGE")" "no passing gate verdict" "merge with no gate verdict blocked"

HEAD_SHA="$(git -C "$TMP" rev-parse HEAD)"
echo "PASS exit=0 head=$HEAD_SHA tree=x log=x" > "$TMP/.gate.verdict"
git -C "$TMP" -c user.email=t@t -c user.name=t commit -q --allow-empty -m later
blocks_with guard-bash.py "$(b "$MERGE")" "not on HEAD" "merge on a revision the gate never saw blocked"

# The verdict must describe this TREE, not just this commit: a gate that passed
# over uncommitted edits says nothing about the clean commit left behind.
HEAD_SHA="$(git -C "$TMP" rev-parse HEAD)"
mkdir "$TMP/HEAD"
printf 'package main // dirty\n' > "$TMP/main.go"
digest_status=0
DIRTY="$(cd "$TMP" && ./scripts/gate.sh --digest 2>scripts/digest.err)" || digest_status=$?
if [ "$digest_status" -ne 0 ]; then
  echo "FAIL: dirty-tree digest exited $digest_status"
  fail=1
elif [ -s "$TMP/scripts/digest.err" ]; then
  echo "FAIL: dirty-tree digest wrote a diagnostic: $(cat "$TMP/scripts/digest.err")"
  fail=1
fi
git -C "$TMP" checkout -q -- main.go
digest_status=0
CLEAN="$(cd "$TMP" && ./scripts/gate.sh --digest 2>scripts/digest.err)" || digest_status=$?
if [ "$digest_status" -ne 0 ]; then
  echo "FAIL: clean-tree digest exited $digest_status"
  fail=1
elif [ -s "$TMP/scripts/digest.err" ]; then
  echo "FAIL: clean-tree digest wrote a diagnostic: $(cat "$TMP/scripts/digest.err")"
  fail=1
fi
[ "$DIRTY" != "$CLEAN" ] && echo "ok: the digest distinguishes those trees" \
  || { echo "FAIL: dirty and clean trees digest the same — the check can see nothing"; fail=1; }
echo "PASS exit=0 head=$HEAD_SHA tree=$DIRTY log=x" > "$TMP/.gate.verdict"
blocks_with guard-bash.py "$(b "$MERGE")" "different content" "merge on a tree the gate never saw blocked"
echo "PASS exit=0 head=$HEAD_SHA tree=$CLEAN log=x" > "$TMP/.gate.verdict"
blocks_with guard-bash.py "$(b "$MERGE")" "cannot read the PR head" "a matching commit and tree reaches the forge check"

echo "-- the merge guard sees the ordinary spellings"
blocks_with guard-bash.py "$(b "env $MERGE")" "cannot read the PR head" "env-prefixed merge seen"
blocks_with guard-bash.py "$(b "command $MERGE")" "cannot read the PR head" "command-prefixed merge seen"
blocks_with guard-bash.py "$(b "($MERGE)")" "cannot read the PR head" "subshelled merge seen"
blocks_with guard-bash.py "$(b "echo checking
$MERGE")" "cannot read the PR head" "merge on a second line seen"
run 0 guard-bash.py "$(b "echo '$MERGE' >> notes.md")" "merge quoted as an argument allowed"
rm -f "$TMP/.gate.verdict"

echo "-- no AI attribution"
run 2 guard-bash.py "$(b 'git commit -m "x

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"')"  "attributed commit blocked"
run 0 guard-bash.py "$(b 'git commit -m "TKT-1: fix"')"    "clean commit allowed"

echo "-- the Edit/Write guard"
run 0 guard-edit.sh "$(e "$TMP/main.go")"                  "edit allowed, no gate running"
date -u +%FT%TZ > "$LOCK"
run 2 guard-edit.sh "$(e "$TMP/main.go")"                  "edit blocked while gate runs"
run 0 guard-edit.sh "$(e /tmp/scratch.txt)"                "edit outside a repo allowed"
rm -f "$LOCK"

exit $fail
