#!/usr/bin/env bash
# Exercise the scheduled-failure notifier against the REAL GitHub API (TKT-213).
#
# WHY THIS EXISTS. Nothing in `make check` parses, lints or executes .github/workflows/ —
# there is no test tier for workflow YAML in this repo. So the gate being green says
# nothing about whether the notifier works. This script is the substitute: it extracts the
# `run:` blocks VERBATIM out of the named workflow and drives them against a throwaway
# label, so
# what is verified is the shipped text rather than a copy of it that can drift.
#
# It does NOT verify GitHub's own evaluation of `if:`, `needs:`, `permissions:` or matrix
# aggregation — only a real triggered run does that. Say so rather than implying coverage.
#
# Usage:  bash scripts/verify-scheduled-notifier.sh all      # both workflows
#         bash scripts/verify-scheduled-notifier.sh hermetic  # just one
# Needs:  gh, authenticated, with issue write access to the repo.
# Leaves: nothing — the trap closes every issue it opened and deletes the label.
#
# Verified against mutation: deleting the dedupe branch, the body marker, the `gh issue
# close`, or the label auto-create each makes this script fail, at the step that names the
# broken behaviour.
set -Eeuo pipefail
cd /home/mathieu/sources/ticketing_system
export GH_REPO=mathd/ticketing_system GH_TOKEN="$(gh auth token)"
# Per-workflow AND per-process, so the two halves of `all` cannot see each other's issues.
export LABEL="tkt213-selftest-$$-${1:-x}"
export RUN_URL="https://example.test/run/1" GITHUB_SHA=deadbeef
# A private scratch dir, removed on exit — not $CLAUDE_JOB_DIR or /tmp, so two runs
# cannot clobber each other and nothing survives the script.
T="$(mktemp -d)"

# WHICH workflow to exercise. Both files carry their own inlined copy of the notifier, and
# an earlier version of this script only ever read hermetic.yaml — so a typo or a semantic
# drift in security.yaml passed every assertion here (ai-review pass 2). The whole point of
# accepting the duplication was that the two must stay equivalent; a verifier that reads one
# of them cannot say anything about that.
WORKFLOW="${1:-}"
case "$WORKFLOW" in
  hermetic|security) ;;
  "") echo "usage: $0 <hermetic|security>   (or: $0 all)" >&2; exit 2 ;;
  all)
    # Run the whole suite once per workflow, in separate processes, so a failure names
    # which copy broke.
    rc=0
    for w in hermetic security; do
      echo "=================== $w.yaml ==================="
      bash "$0" "$w" || rc=1
    done
    exit "$rc"
    ;;
  *) echo "unknown workflow: $WORKFLOW" >&2; exit 2 ;;
esac

extract () { python3 -c "
import yaml,sys
d=yaml.safe_load(open('.github/workflows/'+sys.argv[2]+'.yaml'))
sys.stdout.write(d['jobs'][sys.argv[1]]['steps'][0]['run'])" "$1" "$WORKFLOW"; }

extract scheduled-failure-notice  > "$T/fail.sh"
extract scheduled-recovery-notice > "$T/recover.sh"

# The marker is per-workflow, and the label is shared. Derive the marker from the file being
# exercised rather than hardcoding it, or this suite would dedupe against the wrong key.
export MARKER="<!-- scheduled-workflow-failure: .github/workflows/${WORKFLOW}.yaml -->"

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
# EVERY assertion goes through this. `set -e` does NOT abort on a failing left-hand side
# of an `&&` list, so `[ x = y ] && echo ok` is a check that cannot fail the script — it
# just prints nothing and carries on to the success banner. That is not hypothetical: an
# earlier version of this file printed ALL SELFTESTS PASSED while the issue body was
# missing its link to the failed run, because that assertion was written in exactly that
# shape (ai-review F3).
ok () { printf '   ok: %s\n' "$1"; }
die () { printf '   FAIL: %s\n' "$1" >&2; exit 1; }
assert () {  # assert <condition-result> <description>
  [ "$1" = 0 ] || die "$2"
}

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
[ "$(gh label list --limit 200 --json name --jq "any(.[]; .name==\"$LABEL\")")" = false ] && rc=0 || rc=$?
assert "$rc" "the throwaway label already exists; this run would not prove auto-creation"
ok "absent"

echo "### 2. first failure -> creates label + exactly one issue"
bash "$T/fail.sh" >/dev/null
[ "$(gh label list --limit 200 --json name --jq "any(.[]; .name==\"$LABEL\")")" = true ] && rc=0 || rc=$?
assert "$rc" "the notifier did not create its label"
ok "label auto-created"
[ "$(await_open 1)" = 1 ] && rc=0 || rc=$?; assert "$rc" "the first failure did not open an issue"
ok "1 issue"
NUM=$(gh issue list --state open --label "$LABEL" --limit 5 --json number --jq '.[0].number')
BODY=$(gh issue view "$NUM" --json body --jq '.body')
printf '%s' "$BODY" | grep -qF "$MARKER" && rc=0 || rc=$?
assert "$rc" "the issue body carries no ownership marker, so dedupe cannot match it"
ok "marker present"
printf '%s' "$BODY" | grep -qF "$RUN_URL" && rc=0 || rc=$?
assert "$rc" "the issue body does not link the failed run — the one thing a reader needs"
ok "run url present"

echo "### 3. second failure -> comments, does NOT open a second issue"
bash "$T/fail.sh" >/dev/null
n=$(read_open); [ "$n" = 1 ] && rc=0 || rc=$?; assert "$rc" "expected 1 issue after 2 failures, found $n"
ok "still 1 issue (dedupe works)"
[ "$(gh issue view "$NUM" --json comments --jq '.comments|length')" -ge 1 ] && rc=0 || rc=$?
assert "$rc" "the repeat failure left no comment, so a continuing outage is invisible"
ok "commented"

echo "### 4. third failure -> still one issue"
bash "$T/fail.sh" >/dev/null
n=$(read_open); [ "$n" = 1 ] && rc=0 || rc=$?; assert "$rc" "expected 1 issue after 3 failures, found $n"
ok "still 1 issue after 3 failures"

echo "### 5. recovery REFUSES to close while a non-notifier job failed"
# The guard's refusal path, exercised against a real run whose jobs did NOT all succeed.
# Without this the `clean` check could be deleted entirely and every other assertion here
# would still pass — the guard would be present but never observed doing its job.
# The fixture is any COMPLETED run, in this repository, that contains at least one job
# which failed, was cancelled or timed out — searched across every workflow rather than
# one, because pinning it to a specific broken workflow makes this test disappear the day
# that workflow is fixed.
#
# And when no such run exists, this FAILS rather than skipping (ai-review pass 2). A test
# that quietly skips itself is indistinguishable from a test that passed, and this is the
# only case that observes the guard doing its job at all — delete the guard and every other
# assertion here still passes. If this ever fires, the fix is to widen the search or build a
# fixture run, never to restore the skip.
FAILED_RUN=$(gh run list --limit 100 --json databaseId,conclusion \
  --jq 'map(select(.conclusion=="failure"))|.[0].databaseId')
[ -n "$FAILED_RUN" ] && rc=0 || rc=$?
assert "$rc" "no run with a failed job exists to exercise the guard's refusal path; without it the guard is untested and this suite would be blessing an unverified mechanism"
GITHUB_RUN_ID="$FAILED_RUN" bash "$T/recover.sh" >/dev/null
n=$(read_open); [ "$n" = 1 ] && rc=0 || rc=$?; assert "$rc" "recovery closed the issue despite a failed job in the run — the guard is inert"
ok "issue left open (guard refused)"
gh issue view "$NUM" --json comments --jq '.comments[-1].body' | grep -qF "not every job in it finished cleanly" && rc=0 || rc=$?
assert "$rc" "the refusal left no explanation on the issue"
ok "refusal explained on the issue"

echo "### 6. recovery closes when the guard passes"
# The guard's live invariant is "exactly one job still in flight — me", which a COMPLETED
# historical run can never satisfy: nothing is running in it. So this case cannot be staged
# from real run data, and pointing it at a finished run is what made an earlier version of
# this script pass while the shipped guard could not close anything at all.
#
# The close path is therefore driven with the guard's own query stubbed to the verdict it
# would return in the live state. What that verdict SHOULD be, for every job arrangement, is
# what step 7 proves against the shipped predicate. Split deliberately: this case tests
# "given a clean verdict, does it comment and close?", step 7 tests "which arrangements are
# clean?" — and neither can quietly stand in for the other.
sed 's|gh run view "\$GITHUB_RUN_ID" --json jobs --jq .*|echo true)|; s|^ *and .*||; s|^ *| |' "$T/recover.sh" > "$T/recover_clean.sh"
python3 - "$T/recover.sh" "$T/recover_clean.sh" <<'PY'
import re, sys
src, dst = sys.argv[1], sys.argv[2]
s = open(src).read()
# Replace the whole verdict=$( ... ) command substitution with a literal true.
i = s.index('verdict=$(gh run view')
depth, j = 0, i + len('verdict=$(') - 1
while True:
    if s[j] == '(': depth += 1
    elif s[j] == ')':
        depth -= 1
        if depth == 0: break
    j += 1
open(dst, 'w').write(s[:i] + 'verdict=true' + s[j+1:])
PY
bash -n "$T/recover_clean.sh"; assert $? "the stubbed recovery script does not parse"
GITHUB_RUN_ID=0 bash "$T/recover_clean.sh" >/dev/null
[ "$(await_open 0)" = 0 ] && rc=0 || rc=$?; assert "$rc" "a clean verdict did not close the issue"
ok "issue closed"
gh issue view "$NUM" --json comments --jq '.comments[-1].body' | grep -q Recovered && rc=0 || rc=$?
assert "$rc" "the close left no recovery comment"
ok "recovery comment"

echo "### 7. the guard's predicate, against synthetic job states"
# The real-API cases above cannot reach the state the guard actually runs in. A completed
# historical run has NO job in flight, while the live guard always has exactly one — itself.
# That gap hid a shipping defect for a whole review round: `gh run view --json jobs` renders
# a RUNNING job's conclusion as the empty STRING, not JSON null, so a guard written against
# null rejected its own run and would never have closed anything (ai-review pass 3).
#
# So the predicate is also exercised directly, against payloads shaped like the ones gh
# emits, including states that cannot be staged on demand: two unfinished jobs, an empty
# array, an unknown future conclusion.
# THE PREDICATE UNDER TEST IS EXTRACTED, NOT COPIED (ai-review pass 4).
#
# An earlier version kept its own copy of the jq program and compared it to the shipped one
# after deleting whitespace. That comparison was unsound in a way worth recording: `norm`
# stripped spaces inside string literals too, so a shipped predicate broken to `"com pleted"`
# compared EQUAL to the correct copy, and the synthetic cases then ran the correct copy while
# the workflow shipped the broken one. A drift check that can bless the drift is worse than
# none. Extracting and executing the real program removes the copy entirely, and lets the
# workflow be reformatted without failing this suite.
PRED=$(python3 - "$WORKFLOW" <<'PY'
import yaml, sys
r = yaml.safe_load(open('.github/workflows/'+sys.argv[1]+'.yaml'))['jobs']['scheduled-recovery-notice']['steps'][0]['run']
marker = '(.jobs | length) > 0'
if marker not in r:
    sys.exit(1)   # the shipped guard is not the shape this suite knows how to drive
i = r.index(marker)
sys.stdout.write(r[i:r.index(chr(39)+')', i)])
PY
) || PRED=""
[ -n "$PRED" ] && rc=0 || rc=$?
assert "$rc" "could not extract the recovery guard's predicate from $WORKFLOW.yaml — it has been rewritten, and these cases would prove nothing about what ships"
ok "predicate extracted from $WORKFLOW.yaml"

check_pred () {  # check_pred <expected> <description> <json>
  got=$(printf '%s' "$3" | jq "$PRED" 2>/dev/null || echo ERROR)
  [ "$got" = "$1" ] && rc=0 || rc=$?; assert "$rc" "$2 (expected $1, got $got)"
  ok "$2"
}
# The live shape: this job running, the sibling notifier skipped, workloads green.
check_pred true  "live run with only this job in flight is clean" \
  '{"jobs":[{"status":"completed","conclusion":"success"},{"status":"completed","conclusion":"skipped"},{"status":"in_progress","conclusion":""}]}'
# An omitted job that was SKIPPED — a false `if`, a missing input — must block. This is the
# case an unconditional skip allowance blessed: workloads green, sibling skipped, and a scan
# that never ran (ai-review pass 4).
check_pred false "a second skipped job blocks the close" \
  '{"jobs":[{"status":"completed","conclusion":"success"},{"status":"completed","conclusion":"skipped"},{"status":"completed","conclusion":"skipped"},{"status":"in_progress","conclusion":""}]}'
# The omitted-job race: a second job still going.
check_pred false "a second unfinished job blocks the close" \
  '{"jobs":[{"status":"completed","conclusion":"success"},{"status":"queued","conclusion":""},{"status":"in_progress","conclusion":""}]}'
# Each blocking conclusion, beside an otherwise live-shaped run.
for c in failure cancelled timed_out action_required stale; do
  check_pred false "a $c job blocks the close" \
    "{\"jobs\":[{\"status\":\"completed\",\"conclusion\":\"$c\"},{\"status\":\"completed\",\"conclusion\":\"skipped\"},{\"status\":\"in_progress\",\"conclusion\":\"\"}]}"
done
# An unknown future conclusion must fail closed, not sail through.
check_pred false "an unrecognised conclusion blocks the close" \
  '{"jobs":[{"status":"completed","conclusion":"something_github_added_later"},{"status":"completed","conclusion":"skipped"},{"status":"in_progress","conclusion":""}]}'
# Degenerate payloads.
check_pred false "an empty jobs array is not clean" '{"jobs":[]}'
check_pred false "a run with nothing in flight is not the live state" \
  '{"jobs":[{"status":"completed","conclusion":"success"}]}'

echo "### 8. recovery with nothing open -> silent no-op, exit 0"
GITHUB_RUN_ID=0 bash "$T/recover_clean.sh" | grep -q "nothing to close" && rc=0 || rc=$?
assert "$rc" "the no-op path did not report cleanly"
ok "no-op path"

echo
echo "ALL SELFTESTS PASSED"
