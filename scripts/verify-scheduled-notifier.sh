#!/usr/bin/env bash
# Exercise the scheduled-failure notifier against the REAL GitHub API (TKT-213).
#
# WHY THIS EXISTS. Nothing in `make check` parses, lints or executes .github/workflows/ —
# there is no test tier for workflow YAML in this repo. So the gate being green says
# nothing about whether the notifier works. This script is the substitute: it extracts the
# `run:` blocks VERBATIM out of hermetic.yaml and drives them against a throwaway label, so
# what is verified is the shipped text rather than a copy of it that can drift.
#
# It does NOT verify GitHub's own evaluation of `if:`, `needs:`, `permissions:` or matrix
# aggregation — only a real triggered run does that. Say so rather than implying coverage.
#
# Usage:  bash scripts/verify-scheduled-notifier.sh
# Needs:  gh, authenticated, with issue write access to the repo.
# Leaves: nothing — the trap closes every issue it opened and deletes the label.
#
# Verified against mutation: deleting the dedupe branch, the body marker, the `gh issue
# close`, or the label auto-create each makes this script fail, at the step that names the
# broken behaviour.
set -Eeuo pipefail
cd /home/mathieu/sources/ticketing_system
export GH_REPO=mathd/ticketing_system GH_TOKEN="$(gh auth token)"
export LABEL="tkt213-selftest-$$" 
export MARKER="<!-- scheduled-workflow-failure: SELFTEST-$$ -->"
export RUN_URL="https://example.test/run/1" GITHUB_SHA=deadbeef
# A private scratch dir, removed on exit — not $CLAUDE_JOB_DIR or /tmp, so two runs
# cannot clobber each other and nothing survives the script.
T="$(mktemp -d)"

extract () { python3 -c "
import yaml,sys
d=yaml.safe_load(open('.github/workflows/hermetic.yaml'))
sys.stdout.write(d['jobs'][sys.argv[1]]['steps'][0]['run'])" "$1"; }

extract scheduled-failure-notice  > "$T/fail.sh"
extract scheduled-recovery-notice > "$T/recover.sh"

cleanup () {
  # Close BEFORE deleting the label: deleting it first leaves the issues unfindable by
  # label and therefore orphaned open. (Learned the hard way on the first run.)
  for n in $(gh issue list --state all --label "$LABEL" --limit 50 --json number --jq '.[].number' 2>/dev/null || true); do
    gh issue close "$n" --reason "not planned" >/dev/null 2>&1 || true
  done
  gh label delete "$LABEL" --yes >/dev/null 2>&1 || true
  rm -rf "$T"
}
trap cleanup EXIT

# Two different questions, two different helpers — conflating them is what made the first
# version of this harness report a mutant as killed at the WRONG step.
#
# GitHub's issue list goes through an eventually-consistent search index, so a create can
# return 201 and not be listable for a second or two. That means:
#   - "did N issues appear?"  -> poll, because the answer can arrive late.
#   - "did NO extra issue appear?" -> polling for it is meaningless: waiting to see a value
#     you hope is absent just returns the moment it matches. Settle the index first, THEN
#     read once.
settle () { sleep 6; }

await_open () {   # await_open <expected>  — waits for the count to REACH expected
  local want="$1" got=""
  for _ in $(seq 1 20); do
    got=$(gh issue list --state open --label "$LABEL" --limit 50 --json number --jq 'length')
    [ "$got" = "$want" ] && { echo "$got"; return 0; }
    sleep 1
  done
  echo "$got"
}

read_open () {    # read_open — one settled read, for asserting nothing EXTRA appeared
  settle
  gh issue list --state open --label "$LABEL" --limit 50 --json number --jq 'length'
}

echo "### 1. label does not exist yet"
gh label list --limit 200 --json name --jq "any(.[]; .name==\"$LABEL\")" | grep -qx false && echo "   ok: absent"

echo "### 2. first failure -> creates label + exactly one issue"
bash "$T/fail.sh" >/dev/null
[ "$(gh label list --limit 200 --json name --jq "any(.[]; .name==\"$LABEL\")")" = true ] && echo "   ok: label auto-created"
[ "$(await_open 1)" = 1 ] && echo "   ok: 1 issue" || { echo "   FAIL: the first failure did not open an issue"; exit 1; }
NUM=$(gh issue list --state open --label "$LABEL" --limit 5 --json number --jq '.[0].number')
gh issue view "$NUM" --json body --jq '.body' | grep -q "$MARKER" && echo "   ok: marker present"
gh issue view "$NUM" --json body --jq '.body' | grep -q "$RUN_URL" && echo "   ok: run url present"

echo "### 3. second failure -> comments, does NOT open a second issue"
bash "$T/fail.sh" >/dev/null
n=$(read_open); [ "$n" = 1 ] && echo "   ok: still 1 issue (dedupe works)" || { echo "   FAIL: expected 1 issue after 2 failures, found $n"; exit 1; }
[ "$(gh issue view "$NUM" --json comments --jq '.comments|length')" -ge 1 ] && echo "   ok: commented"

echo "### 4. third failure -> still one issue"
bash "$T/fail.sh" >/dev/null
n=$(read_open); [ "$n" = 1 ] && echo "   ok: still 1 issue after 3 failures" || { echo "   FAIL: expected 1 issue after 3 failures, found $n"; exit 1; }

echo "### 5. recovery -> comments and closes"
GITHUB_RUN_ID=$(gh run list --workflow=hermetic.yaml --limit 1 --json databaseId --jq '.[0].databaseId') \
  bash "$T/recover.sh" >/dev/null
[ "$(await_open 0)" = 0 ] && echo "   ok: issue closed" || { echo "   FAIL: the recovery did not close the issue"; exit 1; }
gh issue view "$NUM" --json comments --jq '.comments[-1].body' | grep -q Recovered && echo "   ok: recovery comment"

echo "### 6. recovery with nothing open -> silent no-op, exit 0"
GITHUB_RUN_ID=$(gh run list --workflow=hermetic.yaml --limit 1 --json databaseId --jq '.[0].databaseId') \
  bash "$T/recover.sh" | grep -q "nothing to close" && echo "   ok: no-op path"

echo
echo "ALL SELFTESTS PASSED"
