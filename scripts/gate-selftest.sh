#!/usr/bin/env bash
# Gate self-test: prove `make check` fails on seeded errors.
# Each seed runs in a disposable git worktree of HEAD (the developer's tree
# is never touched); cleanup is trap-based so interruption can't leave state.
# Each seed targets its own gate stage; `make check` aggregates all stages,
# so any stage failing fails the gate.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
WORK="$(mktemp -d)"
cleanup() {
  git -C "$ROOT" worktree remove --force "$WORK/tree" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

git -C "$ROOT" worktree add --detach "$WORK/tree" HEAD >/dev/null
cd "$WORK/tree"
# Real install (NOT a symlink to the main tree — pnpm running in the worktree
# would rewire the main tree's links); pnpm's shared store keeps this fast.
pnpm install --prefer-offline >/dev/null

fail_count=0
expect_fail() {
  local name="$1" target="$2"
  echo "=== selftest: $name (expect: make $target fails) ==="
  if make "$target" >/dev/null 2>&1; then
    echo "FAIL: make $target PASSED despite seeded error ($name)"
    fail_count=$((fail_count + 1))
  else
    echo "ok: make $target failed as expected"
  fi
  git checkout -- . && git clean -fdq --exclude=node_modules --exclude=bin
}

# Positive control for a stage whose seed mutates an existing file rather than
# adding one: a checker that always fails would satisfy its own expect_fail.
expect_pass() {
  local name="$1" target="$2"
  echo "=== selftest: $name (expect: make $target passes) ==="
  if make "$target" >/dev/null 2>&1; then
    echo "ok: make $target passed on the clean baseline"
  else
    echo "FAIL: make $target FAILED on the clean baseline ($name)"
    fail_count=$((fail_count + 1))
  fi
  git checkout -- . && git clean -fdq --exclude=node_modules --exclude=bin
}

expect_refusal() {
  local name="$1" expected="$2" output status=0
  shift 2
  echo "=== selftest: $name (expect: checker refuses with status 2) ==="
  output=$("$@" 2>&1) || status=$?
  if [ "$status" -ne 2 ]; then
    echo "FAIL: checker returned status $status, want 2 ($name)"
    printf '%s\n' "$output" | sed 's/^/  /'
    fail_count=$((fail_count + 1))
  elif ! printf '%s\n' "$output" | grep -Fq "$expected"; then
    echo "FAIL: checker did not report $expected ($name)"
    printf '%s\n' "$output" | sed 's/^/  /'
    fail_count=$((fail_count + 1))
  else
    echo "ok: checker refused $name"
  fi
}

# The port range deliberately has 40 stable slots. The project identity must
# remain distinct when two checkout paths land in the same slot, or one run's
# cleanup can remove the other run's containers and volumes.
./scripts/stack-env-selftest.sh

# 1. Go lint violation (govet printf)
cat > shared/go/httpx/seeded.go <<'EOF'
package httpx

import "fmt"

func seeded() string { return fmt.Sprintf("%d", "not-a-number") }
EOF
expect_fail "go lint" lint-go

# 2. Failing Go test
cat > shared/go/httpx/seeded_test.go <<'EOF'
package httpx

import "testing"

func TestSeededFailure(t *testing.T) { t.Fatal("seeded failure") }
EOF
expect_fail "go test" test-go

# 3. Go compile error (build stage — proves build-go fails before smoke
#    can ever package a broken binary.)
cat > shared/go/httpx/seeded.go <<'EOF'
package httpx

var seededBroken int = "not-a-number"
EOF
expect_fail "go build" build-go

# 4. TS lint violation (oxlint no-debugger)
cat > web/scanner/src/seeded.ts <<'EOF'
export function seeded(): void {
  debugger
}
EOF
expect_fail "ts lint" lint-ts

# 5. Failing vitest test
cat > web/scanner/src/seeded.test.ts <<'EOF'
import { expect, it } from 'vitest'
it('seeded failure', () => { expect(1).toBe(2) })
EOF
expect_fail "ts test" test-ts

# 6. TS type error (tsc, build stage)
cat > web/scanner/src/seeded.ts <<'EOF'
export const seeded: number = 'not a number'
EOF
expect_fail "ts build" build-ts

# 7. Go dependency-declaration drift (TKT-129). access and shared/go both declare
#    otel directly; dropping access to v1.43.0 diverges them. `go mod edit` rewrites
#    the manifest without touching the network or the workspace build list.
expect_pass "go dependency drift (clean baseline)" check-dep-drift
(cd services/access && go mod edit -require=go.opentelemetry.io/otel@v1.43.0)
expect_fail "go dependency drift" check-dep-drift

# 8. An unreadable module must not be silently skipped. shared/go is FIRST in
#    GO_MODULES, and the checker's original piped-loop form reported "none" and
#    exited 0 whenever a non-final module failed to parse — a checker that never
#    read a module still passing is worse than no checker.
printf 'this is not a go.mod\n' > shared/go/go.mod
expect_fail "go dependency drift (unreadable non-final module)" check-dep-drift

# 8b. Build-list lag (TKT-265), the vertical direction check-dep-drift cannot see.
#     The seed must use a dependency declared by exactly one module. Otherwise it
#     creates horizontal drift, and check-dep-drift runs first for the wrong reason.
#     Only Catalog declares golang.org/x/mod. Lowering it to v0.38.0 leaves x/tools
#     selecting v0.39.0 for the workspace, so only the build-list checker catches it.
#     The paired check-dep-drift pass proves that isolation.
expect_pass "go build-list lag (clean baseline)" check-build-list-lag
#     The isolation assertion below would pass vacuously if the seed silently failed
#     to apply. "check-dep-drift is green" is equally true of an unseeded tree. So
#     confirm the seed is actually present before asserting anything about it.
(cd services/catalog && go mod edit -require=golang.org/x/mod@v0.38.0)
grep -q 'golang.org/x/mod v0.38.0' services/catalog/go.mod || {
  echo "FAIL: build-list-lag seed did not apply; the isolation assertion would be vacuous"
  fail_count=$((fail_count + 1))
}
expect_pass "go build-list lag seed does not disturb check-dep-drift" check-dep-drift
(cd services/catalog && go mod edit -require=golang.org/x/mod@v0.38.0)
expect_fail "go build-list lag" check-build-list-lag

# 8c. An unreadable module must fail closed here too, for the same reason as case 8:
#     a checker that cannot read a manifest must never report "no lag" and exit 0.
printf 'this is not a go.mod\n' > shared/go/go.mod
expect_fail "go build-list lag (unreadable module)" check-build-list-lag

# 8d. The stage must not MUTATE the tree. Resolving the module graph makes Go write
#     newly-learned checksums into go.work.sum, and `-mod=readonly` does not prevent
#     it — the write targets the workspace sum file, which that flag does not govern.
#     The first implementation of this checker did exactly that, which is the whole
#     reason `go work sync && git diff` was rejected as the gate in the first place.
#     The seeded lag sends the checker through graph resolution while the checksum
#     comparison proves that any workspace writes went to its temporary copy.
(cd services/catalog && go mod edit -require=golang.org/x/mod@v0.38.0)
git checkout -- go.work.sum 2>/dev/null || true
echo "=== selftest: go build-list lag leaves go.work.sum unchanged ==="
#     Both hashes would be EMPTY if go.work.sum were missing, and empty equals
#     empty — the comparison below would then pass while observing nothing. Assert
#     the file is there first, or this case joins the ones it exists to prevent.
if [ ! -s go.work.sum ]; then
  echo "FAIL: go.work.sum is missing or empty — the non-mutation check cannot observe anything"
  fail_count=$((fail_count + 1))
fi
#     The run's OUTCOME is asserted too. Discarding the exit status would make any
#     unrelated early failure — a broken workspace copy, a resolution error — look
#     identical to a clean non-mutating run, so the case would go green over exactly
#     the regression it exists to catch. The seeded tree must produce the LAG
#     verdict (exit 1), which is only reachable if the graph actually resolved.
before_sum=$(md5sum go.work.sum | cut -d' ' -f1)
lag_status=0
./scripts/check-go-build-list-lag.sh shared/go services/catalog services/inventory \
  services/commerce services/payments services/access gateway smoke >/dev/null 2>&1 || lag_status=$?
after_sum=$(md5sum go.work.sum 2>/dev/null | cut -d' ' -f1)
#     Exit 1 EXACTLY: 0 is "no lag found" and 2 is "refused to report a verdict",
#     and both are reachable without the graph ever resolving. Accepting any
#     non-zero status would let a checker that refuses on every run satisfy this
#     case while never observing the property. `make` maps the script's 1 to 2, so
#     the script is invoked directly here.
if [ "$lag_status" -ne 1 ]; then
  echo "FAIL: the seeded lag was not reported (status $lag_status) — the non-mutation check never reached graph resolution"
  fail_count=$((fail_count + 1))
elif [ -n "$before_sum" ] && [ "$before_sum" = "$after_sum" ]; then
  echo "ok: check-build-list-lag reported the seeded lag and did not modify go.work.sum"
else
  echo "FAIL: check-build-list-lag MODIFIED go.work.sum — the stage mutates the tree"
  fail_count=$((fail_count + 1))
fi
git checkout -- . && git clean -fdq --exclude=node_modules --exclude=bin

# 8e. A `replace` directive puts the comparison outside what it can speak to: the
#     selected version can match the declared one while the build links different
#     source. The stage must REFUSE rather than report a false "none".
mkdir -p "$WORK/replacement" && printf 'module golang.org/x/net\n\ngo 1.26\n' > "$WORK/replacement/go.mod"
(cd shared/go && go mod edit -replace=golang.org/x/net="$WORK/replacement")
expect_fail "go build-list lag (replace directive in effect)" check-build-list-lag

# 8f. The WORKSPACE form of the same thing, and a relative one. This is not a
#     duplicate of 8e: `go list -m all` reports a replacement only for a module
#     already in the build list, so a go.work replace naming a module nothing
#     requires was invisible to the first implementation and the stage reported
#     "none". Detection reads the declarations for that reason.
mkdir -p localdep && printf 'module example.com/dep\n\ngo 1.26\n' > localdep/go.mod
go work edit -replace=example.com/dep=./localdep
expect_fail "go build-list lag (relative replace in go.work)" check-build-list-lag

# 8g. A directive that is not `use` or `replace` must SURVIVE into the workspace
#     copy. `go work edit -json` exposes only Go, Use and Replace, so an earlier
#     version that REGENERATED go.work from that JSON silently dropped `godebug`,
#     and the fidelity guard — which compares module directories — passed over it.
#
#     The verdict alone cannot observe this. Dropping godebug still lets the graph
#     resolve, so inspect the production checker's diagnostic copy as well.
go work edit -godebug=default=go1.25
echo "=== selftest: go build-list lag (godebug survives the workspace copy) ==="
gw_probe=$(mktemp -d)
gw_diagnostic="$gw_probe/go.work"
gw_output=""
gw_status=0
gw_output=$(./scripts/check-go-build-list-lag.sh \
  --diagnostic-workspace-copy "$gw_diagnostic" \
  shared/go services/catalog services/inventory services/commerce \
  services/payments services/access gateway smoke 2>&1) || gw_status=$?
gw_expected="check-go-build-list-lag: wrote diagnostic workspace copy to $gw_diagnostic"
if [ "$gw_status" -ne 0 ]; then
  echo "FAIL: production checker returned status $gw_status while building the diagnostic copy"
  printf '%s\n' "$gw_output" | sed 's/^/  /'
  fail_count=$((fail_count + 1))
elif ! printf '%s\n' "$gw_output" | grep -Fqx "$gw_expected"; then
  echo "FAIL: production checker did not report the diagnostic workspace copy"
  printf '%s\n' "$gw_output" | sed 's/^/  /'
  fail_count=$((fail_count + 1))
elif [ ! -s "$gw_diagnostic" ]; then
  echo "FAIL: production checker did not write a diagnostic workspace copy"
  fail_count=$((fail_count + 1))
elif grep -q '^godebug default=go1.25$' "$gw_diagnostic"; then
  echo "ok: godebug survived the path-only rewrite of the workspace copy"
else
  echo "FAIL: the workspace copy DROPPED godebug — it no longer describes the same build"
  fail_count=$((fail_count + 1))
fi
rm -rf "$gw_probe"
git checkout -- . && git clean -fdq --exclude=node_modules --exclude=bin

# 8h. The module list is hand-written in Make. Adding a module to go.work without
#     updating GO_MODULES must fail both checks rather than silently narrowing
#     their verdict to the old eight-module set. The ninth module needs no defect
#     of its own: omission is the defect this fixture isolates.
mkdir omitted-module
printf 'module ticketing/omitted\n\ngo 1.27.0\n' > omitted-module/go.mod
go work use ./omitted-module
expect_fail "standalone Go modules (workspace module omitted from Make)" check-go-standalone

mkdir omitted-module
printf 'module ticketing/omitted\n\ngo 1.27.0\n' > omitted-module/go.mod
go work use ./omitted-module
expect_fail "go build-list lag (workspace module omitted from Make)" check-build-list-lag

# The set comparison has three independent argument failures. The new-module
# cases above cover a missing argument. These cases cover an extra argument and
# two spellings of the same canonical path. Run the production scripts, not a
# copy of their Python helper, so removing the check from either caller fails.
go_modules=(shared/go services/catalog services/inventory services/commerce services/payments services/access gateway smoke)
for checker in ./scripts/check-go-standalone.sh ./scripts/check-go-build-list-lag.sh; do
  expect_refusal "${checker##*/} extra module argument" "extra argument" \
    "$checker" "${go_modules[@]}" not-a-workspace-module
  expect_refusal "${checker##*/} duplicate canonical module argument" "duplicate argument path" \
    "$checker" "${go_modules[@]}" ./shared/go
done

# A hand-edited workspace can repeat a use path even though `go work use` does
# not create that spelling. The checker promises an exact set on both sides, so
# duplicate workspace entries must also produce a specific refusal.
python3 - <<'SEED'
p = 'go.work'
s = open(p).read()
s = s.replace('\t./shared/go\n', '\t./shared/go\n\t./shared/go\n', 1)
open(p, 'w').write(s)
SEED
expect_refusal "standalone Go modules duplicate workspace entry" "duplicate workspace path" \
  ./scripts/check-go-standalone.sh "${go_modules[@]}"
git checkout -- go.work
python3 - <<'SEED'
p = 'go.work'
s = open(p).read()
s = s.replace('\t./shared/go\n', '\t./shared/go\n\t./shared/go\n', 1)
open(p, 'w').write(s)
SEED
expect_refusal "go build-list lag duplicate workspace entry" "duplicate workspace path" \
  ./scripts/check-go-build-list-lag.sh "${go_modules[@]}"
git checkout -- go.work

# 9. A credential compose.yaml refuses to start without, that env-bootstrap.sh
#    never generates (TKT-227). TKT-244 shipped exactly this: `make up` died on
#    interpolation while `make check` stayed green, because the smoke path takes
#    its environment from scripts/stack-env.sh instead. The positive control is
#    not optional here — this seed mutates an existing file, so a checker that
#    always failed would satisfy its own expect_fail.
expect_pass "required stack env (clean baseline)" check-required-env
sed -i 's/ INVENTORY_STAFF_WRITE_TOKEN//' scripts/env-bootstrap.sh
expect_fail "required stack env (compose requires a variable nothing generates)" check-required-env

# 9b. Present-but-EMPTY is not generated. `${VAR:?}` rejects an
#     empty value exactly as it rejects an unset one, so a checker comparing only
#     NAMES passes here while `make up` still dies. Deleting a name (9 above)
#     cannot expose that, which is why this is its own seed.
printf '\nenv_set INVENTORY_STAFF_WRITE_TOKEN ""\n' >> scripts/env-bootstrap.sh
expect_fail "required stack env (a required credential generated empty)" check-required-env

# 9c. The OTHER required form. Compose supports `${VAR?msg}`
#     as well as `${VAR:?msg}`; a requirement written that way leaves the existing
#     matches intact, so MIN_REQUIRED stays satisfied and an ungenerated credential
#     escapes. Seeded as a NEW requirement rather than by rewriting an existing
#     one, so the floor cannot be what catches it.
sed -i 's|^\( *\)INVENTORY_STAFF_WRITE_TOKEN: \${INVENTORY_STAFF_WRITE_TOKEN:?|\1SELFTEST_UNGENERATED: ${SELFTEST_UNGENERATED?seeded by gate-selftest}\n\1INVENTORY_STAFF_WRITE_TOKEN: ${INVENTORY_STAFF_WRITE_TOKEN:?|' compose.yaml
expect_fail "required stack env (alternate \${VAR?} requirement form)" check-required-env

# 9d. The OPPOSITE failure, and the one a gate is least forgiven for: refusing a
#     VALID stack. Compose ignores a placeholder inside a YAML comment and treats
#     `$${VAR:?}` as a literal — both confirmed against `docker compose config`
#     with the variables unset. A checker that counted either would invent a
#     missing credential and fail the gate on a config that starts perfectly well.
#     This is an expect_PASS: the seed must change nothing.
sed -i '1i # a commented placeholder: ${SELFTEST_COMMENTED:?never a requirement}' compose.yaml
sed -i 's|^\( *\)INVENTORY_STAFF_WRITE_TOKEN: \${INVENTORY_STAFF_WRITE_TOKEN:?|\1SELFTEST_LITERAL: "$${SELFTEST_ESCAPED:?never a requirement}"\n\1INVENTORY_STAFF_WRITE_TOKEN: ${INVENTORY_STAFF_WRITE_TOKEN:?|' compose.yaml
expect_pass "required stack env (comments and \$\$-escapes are not requirements)" check-required-env

# 10. A repeated ADR number makes every bare reference to that number ambiguous.
#     Copy an existing ADR onto a number already taken to create the collision.
#     Positive control first: this stage reads a directory rather than a seeded
#     file, so a checker that always failed would satisfy its own expect_fail.
expect_pass "ADR numbers (clean baseline)" check-adr-numbers
cp docs/adr/ADR-064-presale-unlock-codes.md docs/adr/ADR-055-presale-unlock-codes.md
expect_fail "ADR numbers (two ADRs claim 055)" check-adr-numbers

# 10b. An ADR whose name carries no three-digit number is unreferenceable, and it
#      cannot collide, so the duplicate seed above cannot expose it. Its own seed.
cp docs/adr/ADR-064-presale-unlock-codes.md docs/adr/ADR-64-presale-unlock-codes.md
expect_fail "ADR numbers (an ADR with no three-digit number)" check-adr-numbers

# 11. A broken documentation cross-reference. ADR-062 linked to a filename ADR-010
#     never had, so the inherited locking decision was unreachable; no gate stage
#     resolved a link target, so it survived indefinitely. The seed reproduces that
#     exact shape — a real ADR citing a nonexistent sibling.
expect_pass "markdown links (clean baseline)" check-markdown-links
sed -i 's|ADR-010-postgres-claim-transaction.md)|ADR-010-nonexistent-file.md)|' \
  docs/adr/ADR-062-refund-reversal-reconciliation.md
expect_fail "markdown links (an ADR cites a file that does not exist)" check-markdown-links

# 11b. The angle-bracket spelling, which the review document uses for paths that
#      contain brackets. It is matched by a separate branch of the extractor, so
#      the plain-link seed above cannot show it works.
printf '\nA broken angle link [x](<docs/adr/ADR-000-nonexistent [id].md>)\n' >> docs/README.md
expect_fail "markdown links (broken angle-bracket target)" check-markdown-links

# 11c. The OPPOSITE failure, and the one a gate is least forgiven for: refusing
#      valid documentation. An external URL must never be fetched or resolved as a
#      path, and a pure anchor has no path at all. A checker that treated either as
#      a file would fail the gate on correct prose. expect_PASS: nothing changes.
printf '\nAn external [x](https://example.invalid/nope) and an anchor [y](#context).\n' >> docs/README.md
expect_pass "markdown links (external URLs and anchors are not paths)" check-markdown-links

# 12. The security workflow skipping code-only pull requests. A top-level `paths:`
#     filter meant the Trivy secret scanner never inspected the changes most likely
#     to introduce a credential — a source-only PR scheduled none of the workflow.
#     The seed restores that filter verbatim.
expect_pass "security workflow trigger (clean baseline)" check-security-workflow-trigger
python3 - <<'SEED'
p = '.github/workflows/security.yaml'
s = open(p).read()
s = s.replace('  pull_request:\n', '  pull_request:\n    paths:\n      - "**/go.mod"\n', 1)
open(p, 'w').write(s)
SEED
expect_fail "security workflow trigger (a path filter skips code-only PRs)" check-security-workflow-trigger

# 12b. The OTHER half, and one the filter seed cannot reach: an all-PR workflow
#      whose repository-scan is itself conditional skips the only job that reads
#      source files, while the event above it still looks unfiltered.
python3 - <<'SEED'
p = '.github/workflows/security.yaml'
s = open(p).read()
s = s.replace('  repository-scan:\n    runs-on: ubuntu-latest\n',
              '  repository-scan:\n    runs-on: ubuntu-latest\n    if: false\n', 1)
open(p, 'w').write(s)
SEED
expect_fail "security workflow trigger (repository-scan made conditional)" check-security-workflow-trigger

# 12c. And the third way to lose the same guarantee without touching either: keep
#      the job unconditional and drop the scanner that reads source files.
python3 - <<'SEED'
p = '.github/workflows/security.yaml'
s = open(p).read()
s = s.replace('scanners: vuln,misconfig,secret', 'scanners: vuln', 1)
open(p, 'w').write(s)
SEED
expect_fail "security workflow trigger (secret scanning dropped)" check-security-workflow-trigger

# 12d. The scan allowlist wiring (ADR-081). pnpm applies auditConfig.ignoreGhsas
#      with no expiry, so an expired entry keeps hiding its advisory unless the
#      expiry check runs before the audit. Each seed loses one wiring line, and no
#      scan goes red for any of them.
python3 - <<'SEED'
p = '.github/workflows/security.yaml'
s = open(p).read()
before = s
s = s.replace('      - run: node scripts/scan-allowlist.mjs check --now\n', '', 1)
assert s != before, 'the seed did not apply; update it to the current file'
open(p, 'w').write(s)
SEED
expect_fail "security workflow trigger (allowlist expiry check dropped)" check-security-workflow-trigger

# 12e. The check is present but runs after the audit, so the audit reads an
#      expired ignore list first.
python3 - <<'SEED'
p = '.github/workflows/security.yaml'
s = open(p).read()
before = s
check = '      - run: node scripts/scan-allowlist.mjs check --now\n'
audit = '      - run: pnpm audit --audit-level high\n'
s = s.replace(check, '', 1).replace(audit, audit + check, 1)
assert s != before, 'the seed did not apply; update it to the current file'
open(p, 'w').write(s)
SEED
expect_fail "security workflow trigger (allowlist check after the audit)" check-security-workflow-trigger

# 12f. `pnpm audit --ignore` writes the ignore list and exits 0 without
#      reporting the remaining findings, so the audit is green whatever it finds.
python3 - <<'SEED'
p = '.github/workflows/security.yaml'
s = open(p).read()
before = s
s = s.replace('      - run: pnpm audit --audit-level high\n',
              '      - run: pnpm audit --audit-level high --ignore GHSA-7w5x-hrqm-74c2\n', 1)
assert s != before, 'the seed did not apply; update it to the current file'
open(p, 'w').write(s)
SEED
expect_fail "security workflow trigger (pnpm audit --ignore)" check-security-workflow-trigger

# 12g. The check still runs, but its failure no longer fails the job.
python3 - <<'SEED'
p = '.github/workflows/security.yaml'
s = open(p).read()
before = s
s = s.replace('      - run: node scripts/scan-allowlist.mjs check --now\n',
              '      - run: node scripts/scan-allowlist.mjs check --now\n        continue-on-error: true\n', 1)
assert s != before, 'the seed did not apply; update it to the current file'
open(p, 'w').write(s)
SEED
expect_fail "security workflow trigger (allowlist check made tolerant)" check-security-workflow-trigger

# 12h. Trivy no longer reads the allowlist.
python3 - <<'SEED'
p = '.github/workflows/security.yaml'
s = open(p).read()
before = s
s = s.replace('          trivyignores: .trivyignore.yaml\n', '', 1)
assert s != before, 'the seed did not apply; update it to the current file'
open(p, 'w').write(s)
SEED
expect_fail "security workflow trigger (trivyignores input dropped)" check-security-workflow-trigger

# 12i. make check no longer runs the allowlist checker. The target still exists
#      and still passes when called by name, so only the check-all list shows it.
python3 - <<'SEED'
p = 'Makefile'
s = open(p).read()
before = s
s = s.replace(' check-security-workflow-trigger check-scan-allowlist check-hermetic-workflow-trigger check-check-workflow-trigger check-adr-numbers check-markdown-links check-hooks check-go-toolchain lint ',
              ' check-security-workflow-trigger check-hermetic-workflow-trigger check-check-workflow-trigger check-adr-numbers check-markdown-links check-hooks check-go-toolchain lint ', 1)
assert s != before, 'the seed did not apply; update it to the current file'
open(p, 'w').write(s)
SEED
expect_fail "security workflow trigger (check-scan-allowlist dropped from check-all)" check-security-workflow-trigger

# 12j. The allowlist checker itself, on the real files: a pnpm mirror that is not
#      the allowlist's. The seed adds a GHSA that no reviewed entry names, so it
#      makes drift whatever the mirror holds today, empty or not.
expect_pass "scan allowlist (clean baseline)" check-scan-allowlist
python3 - <<'SEED'
p = 'pnpm-workspace.yaml'
s = open(p).read()
before = s
extra = '    - GHSA-2222-2222-2222\n'
if '  ignoreGhsas: []\n' in s:
    s = s.replace('  ignoreGhsas: []\n', '  ignoreGhsas:\n' + extra, 1)
else:
    assert '  ignoreGhsas:\n' in s, 'the mirror is not in the block form that sync writes'
    s = s.replace('  ignoreGhsas:\n', '  ignoreGhsas:\n' + extra, 1)
assert s != before, 'the seed did not apply; update it to the current file'
open(p, 'w').write(s)
SEED
expect_fail "scan allowlist (pnpm mirror drift)" check-scan-allowlist

# 12k. The effective-configuration step dropped: pnpm then audits with whatever
#      it expanded or a pnpmfile hook wrote, unchecked.
python3 - <<'SEED'
p = '.github/workflows/security.yaml'
s = open(p).read()
before = s
s = s.replace('      - run: node scripts/scan-allowlist.mjs check --effective\n', '', 1)
assert s != before, 'the seed did not apply; update it to the current file'
open(p, 'w').write(s)
SEED
expect_fail "security workflow trigger (effective-configuration step dropped)" check-security-workflow-trigger

# 12l. A quoted "if" key skips the expiry step; a line matcher did not see it.
python3 - <<'SEED'
p = '.github/workflows/security.yaml'
s = open(p).read()
before = s
s = s.replace('      - run: node scripts/scan-allowlist.mjs check --now\n',
              '      - run: node scripts/scan-allowlist.mjs check --now\n        "if": false\n', 1)
assert s != before, 'the seed did not apply; update it to the current file'
open(p, 'w').write(s)
SEED
expect_fail "security workflow trigger (quoted if on the expiry step)" check-security-workflow-trigger

# 12m. Trivy still runs, but its findings no longer fail the job.
python3 - <<'SEED'
p = '.github/workflows/security.yaml'
s = open(p).read()
before = s
s = s.replace('          exit-code: "1"\n', '          exit-code: "0"\n', 1)
assert s != before, 'the seed did not apply; update it to the current file'
open(p, 'w').write(s)
SEED
expect_fail "security workflow trigger (Trivy exit-code 0)" check-security-workflow-trigger

# 13. The hermetic workflow missing a production web image input. The enumerated
#     path list covered the scanner and the storefront but not web/backoffice, so a
#     broken back-office production image could merge with neither build path run —
#     `make check` builds the SMOKE Dockerfiles, not these. The seed restores the
#     enumeration that omitted it.
expect_pass "hermetic workflow trigger (clean baseline)" check-hermetic-workflow-trigger
python3 - <<'SEED'
p = '.github/workflows/hermetic.yaml'
s = open(p).read()
s = s.replace('      - "web/*/package.json"\n      - "web/*/Dockerfile"\n',
              '      - web/scanner/package.json\n      - web/scanner/Dockerfile\n'
              '      - web/storefront/Dockerfile\n', 1)
open(p, 'w').write(s)
SEED
expect_fail "hermetic workflow trigger (a production web image input is uncovered)" check-hermetic-workflow-trigger

# Bash's `*` crosses `/`, while GitHub's path-filter `*` does not. A checker
# using Bash pattern matching therefore mistakes `web/*` for coverage of files
# below each application directory. The production checker must reject it.
python3 - <<'SEED'
p = '.github/workflows/hermetic.yaml'
s = open(p).read()
needle = '      - "web/*/package.json"\n      - "web/*/Dockerfile"\n'
if s.count(needle) != 1:
    raise SystemExit('expected canonical web workflow patterns')
open(p, 'w').write(s.replace(needle, '      - "web/*"\n', 1))
SEED
expect_fail "hermetic workflow trigger (web wildcard stops above image inputs)" check-hermetic-workflow-trigger

# A later negative pattern wins over the earlier web wildcard.
python3 - <<'SEED'
p = '.github/workflows/hermetic.yaml'
s = open(p).read()
needle = '      - "web/*/Dockerfile"\n'
if s.count(needle) != 1:
    raise SystemExit('expected canonical Dockerfile workflow pattern')
open(p, 'w').write(s.replace(
    needle,
    needle + '      - "!web/backoffice/Dockerfile"\n',
    1,
))
SEED
expect_fail "hermetic workflow trigger (later pattern excludes an image input)" check-hermetic-workflow-trigger

# A positive pattern after the exclusion includes the path again. This is the
# positive control for ordering, not a set-membership check.
python3 - <<'SEED'
p = '.github/workflows/hermetic.yaml'
s = open(p).read()
needle = '      - "web/*/Dockerfile"\n'
if s.count(needle) != 1:
    raise SystemExit('expected canonical Dockerfile workflow pattern')
open(p, 'w').write(s.replace(
    needle,
    needle
    + '      - "!web/backoffice/Dockerfile"\n'
    + '      - web/backoffice/Dockerfile\n',
    1,
))
SEED
expect_pass "hermetic workflow trigger (later pattern re-includes an image input)" check-hermetic-workflow-trigger

# Reversing those last two entries must exclude the path. A checker that groups
# positive and negative entries instead of applying them in order passes this.
python3 - <<'SEED'
p = '.github/workflows/hermetic.yaml'
s = open(p).read()
needle = '      - "web/*/Dockerfile"\n'
if s.count(needle) != 1:
    raise SystemExit('expected canonical Dockerfile workflow pattern')
open(p, 'w').write(s.replace(
    needle,
    needle
    + '      - web/backoffice/Dockerfile\n'
    + '      - "!web/backoffice/Dockerfile"\n',
    1,
))
SEED
expect_fail "hermetic workflow trigger (re-inclusion precedes exclusion)" check-hermetic-workflow-trigger

# Each runtime overlay is independently required. Removing one must fail even
# while the other three remain covered.
for overlay in compose.yaml compose.direct-ports.yaml compose.onsale-load.yaml compose.smoke-cadence.yaml; do
  python3 - "$overlay" <<'SEED'
import sys

p = '.github/workflows/hermetic.yaml'
s = open(p).read()
needle = f'      - {sys.argv[1]}\n'
if s.count(needle) != 1:
    raise SystemExit(f'expected one workflow path entry: {sys.argv[1]}')
open(p, 'w').write(s.replace(needle, '', 1))
SEED
  expect_fail "hermetic workflow trigger ($overlay is uncovered)" check-hermetic-workflow-trigger
done

# TKT-327: nats-init provisions two service durables from deploy/nats/consumers/, under the ACL in
# nats-server.conf. Each entry is independently required.
for nats_input in deploy/nats/nats-server.conf '"deploy/nats/consumers/**"'; do
  python3 - "$nats_input" <<'SEED'
import sys

p = '.github/workflows/hermetic.yaml'
s = open(p).read()
needle = f'      - {sys.argv[1]}\n'
if s.count(needle) != 1:
    raise SystemExit(f'expected one workflow path entry: {sys.argv[1]}')
open(p, 'w').write(s.replace(needle, '', 1))
SEED
  expect_fail "hermetic workflow trigger ($nats_input is uncovered)" check-hermetic-workflow-trigger
done

sed -i 's/- run: make smoke-hermetic/- run: make smoke/' .github/workflows/hermetic.yaml
expect_fail "hermetic workflow trigger (job runs the fast smoke target)" check-hermetic-workflow-trigger

# A step-level condition can disable the command while leaving both the job and
# its run line unchanged. Inspect the command's step mapping, not just the job.
python3 - <<'SEED'
p = '.github/workflows/hermetic.yaml'
s = open(p).read()
needle = '      - run: make smoke-hermetic\n'
if s.count(needle) != 1:
    raise SystemExit('expected one hermetic smoke step')
open(p, 'w').write(s.replace(needle, needle + '        if: false\n', 1))
SEED
expect_fail "hermetic workflow trigger (smoke step made conditional)" check-hermetic-workflow-trigger

# A smoke command whose failure is ignored does not gate the workflow.
python3 - <<'SEED'
p = '.github/workflows/hermetic.yaml'
s = open(p).read()
needle = '      - run: make smoke-hermetic\n'
if s.count(needle) != 1:
    raise SystemExit('expected one hermetic smoke step')
open(p, 'w').write(s.replace(needle, needle + '        continue-on-error: true\n', 1))
SEED
expect_fail "hermetic workflow trigger (smoke failure ignored)" check-hermetic-workflow-trigger

# 13b. The main ruleset requires the `check` context (TKT-324). That context must
#      report on every PR, come from check.yaml's `check` job, run `make check`, and
#      have exactly one producer. The guard reads YAML line by line, so it accepts
#      only an allowlisted block-style shape and refuses every other shape (fail
#      closed). Each seed breaks one refusal. A YAML spelling that the guard
#      silently misread would pass, so the spelling seeds (flow style, quoted keys,
#      a list, a second document) matter as much as the policy seeds.
# check_seed FILE OLD NEW: replace the one OLD in FILE with NEW (\n is a newline).
check_seed() {
  python3 - "$@" <<'SEED'
import sys

p, old, new = sys.argv[1], sys.argv[2].replace('\\n', '\n'), sys.argv[3].replace('\\n', '\n')
s = open(p).read()
if s.count(old) != 1:
    raise SystemExit(f'expected one {old!r} in {p}')
open(p, 'w').write(s.replace(old, new, 1))
SEED
}
CW=.github/workflows/check.yaml
HW=.github/workflows/hermetic.yaml
CJOB='  check:\n    runs-on: ubuntu-latest\n'
MAKE_STEP='      - run: make check\n'
LAST_STEP='      - run: ./scripts/gate-selftest.sh\n'
HJOB='  hermetic-smoke:\n'
expect_pass "check workflow trigger (clean baseline)" check-check-workflow-trigger

# Events: only push (branches: [main]) and an empty pull_request are allowed. A
# path filter, a branch or type filter, or a missing event can skip the workflow,
# and a skipped workflow leaves a required check pending.
check_seed $CW '    branches: [main]\n' '    branches: [main]\n    paths-ignore: ["docs/**"]\n'
expect_fail "check workflow trigger (paths-ignore filter on push)" check-check-workflow-trigger
check_seed $CW '    branches: [main]\n' '    branches: [main]\n    paths: ["docs/**"]\n'
expect_fail "check workflow trigger (paths filter on push)" check-check-workflow-trigger
check_seed $CW '    branches: [main]\n' '    branches: [develop]\n'
expect_fail "check workflow trigger (push no longer targets main)" check-check-workflow-trigger
# Quote stripping would read this as main. Only the exact text is accepted.
check_seed $CW '    branches: [main]\n' "    branches: [\"m'a'in\"]\n"
expect_fail "check workflow trigger (push branches spelled with quotes)" check-check-workflow-trigger
check_seed $CW '  pull_request:\n' '  pull_request:\n    paths-ignore: ["docs/**"]\n'
expect_fail "check workflow trigger (paths-ignore filter on pull_request)" check-check-workflow-trigger
check_seed $CW '  pull_request:\n' '  pull_request:\n    paths: ["docs/**"]\n'
expect_fail "check workflow trigger (paths filter on pull_request)" check-check-workflow-trigger
check_seed $CW '  pull_request:\n' '  pull_request:\n    branches-ignore: [main]\n'
expect_fail "check workflow trigger (branches-ignore on pull_request)" check-check-workflow-trigger
check_seed $CW '  pull_request:\n' '  pull_request:\n    types: [labeled]\n'
expect_fail "check workflow trigger (types on pull_request)" check-check-workflow-trigger
check_seed $CW '  pull_request:\n' ''
expect_fail "check workflow trigger (pull_request event removed)" check-check-workflow-trigger
check_seed $CW '  push:\n    branches: [main]\n' ''
expect_fail "check workflow trigger (push event removed)" check-check-workflow-trigger
check_seed $CW '  push:\n    branches: [main]\n' '  push:\n'
expect_fail "check workflow trigger (push branches removed)" check-check-workflow-trigger
check_seed $CW '  pull_request:\n' '  pull_request:\n  workflow_dispatch:\n'
expect_fail "check workflow trigger (extra event)" check-check-workflow-trigger

# Spellings the line reader cannot interpret. Each one hides a filter, a
# condition or an event from a reader that expects block style.
check_seed $CW '  pull_request:\n' '  pull_request: { paths-ignore: ["docs/**"] }\n'
expect_fail "check workflow trigger (flow-style event)" check-check-workflow-trigger
check_seed $CW '  pull_request:\n' '  pull_request: {"paths-ignore": ["docs/**"]}\n'
expect_fail "check workflow trigger (quoted key in a flow-style event)" check-check-workflow-trigger
check_seed $CW '  pull_request:\n' "  'pull_request':\n"
expect_fail "check workflow trigger (quoted event key)" check-check-workflow-trigger
check_seed $CW 'on:\n  push:\n    branches: [main]\n  pull_request:\n' 'on: [push, pull_request]\n'
expect_fail "check workflow trigger (on: as a list)" check-check-workflow-trigger
check_seed $CW 'on:\n  push:\n' '"on":\n  push:\n'
expect_fail "check workflow trigger (quoted on: key)" check-check-workflow-trigger
# The bad copy comes FIRST: a reader where the last duplicate wins sees only the
# clean one.
check_seed $CW 'on:\n  push:\n' 'on:\n  pull_request:\n    paths-ignore: ["docs/**"]\non:\n  push:\n'
expect_fail "check workflow trigger (second on: block)" check-check-workflow-trigger
# A second document holding only keys the first lacks: no duplicate key shows it.
check_seed $CW "$LAST_STEP" "${LAST_STEP}"'---\nenv:\n  SECOND_DOCUMENT: "1"\n'
expect_fail "check workflow trigger (second YAML document)" check-check-workflow-trigger
check_seed $CW 'on:\n  push:\n' 'true:\n  push:\n'
expect_fail "check workflow trigger (no on: key)" check-check-workflow-trigger
check_seed $CW '  push:\n    branches: [main]\n' '  push: {branches: [main], paths: ["docs/**"]}\n'
expect_fail "check workflow trigger (flow-style push)" check-check-workflow-trigger

# The job that reports the context: block style, allowlisted job and step keys,
# and no step that can be skipped or ignored. Any of these lets the required
# check read success without `make check` deciding it.
check_seed $CW "$CJOB" "${CJOB}    if: github.event_name == 'push'\n"
expect_fail "check workflow trigger (check job made conditional)" check-check-workflow-trigger
check_seed $CW "$CJOB" "${CJOB}    needs: gate-selftest\n"
expect_fail "check workflow trigger (check job needs another job)" check-check-workflow-trigger
check_seed $CW "$CJOB" "${CJOB}    continue-on-error: true\n"
expect_fail "check workflow trigger (check job ignores failure)" check-check-workflow-trigger
check_seed $CW "$CJOB" "${CJOB}    <<: *defaults\n"
expect_fail "check workflow trigger (merge key in check job)" check-check-workflow-trigger
check_seed $CW "$CJOB" '  check-old:\n    runs-on: ubuntu-latest\n'
check_seed $CW '  check-old:\n' '  check: {runs-on: ubuntu-latest, if: false, steps: [{run: make check}]}\n  check-old:\n'
expect_fail "check workflow trigger (flow-style check job)" check-check-workflow-trigger
check_seed $CW "$MAKE_STEP" "${MAKE_STEP}        if: false\n"
expect_fail "check workflow trigger (make check step made conditional)" check-check-workflow-trigger
check_seed $CW "$MAKE_STEP" "${MAKE_STEP}        continue-on-error: true\n"
expect_fail "check workflow trigger (make check step ignores failure)" check-check-workflow-trigger
# The step seeds keep the `make check` step, so the step count cannot be what
# refuses them.
check_seed $CW "$MAKE_STEP" "${MAKE_STEP}"'      - { run: echo extra, if: false }\n'
expect_fail "check workflow trigger (flow-style step)" check-check-workflow-trigger
check_seed $CW "$MAKE_STEP" "${MAKE_STEP}"'        "if": false\n'
expect_fail "check workflow trigger (quoted key in a step)" check-check-workflow-trigger
check_seed $CW "$MAKE_STEP" '      - run: make lint\n        run: make check\n'
expect_fail "check workflow trigger (duplicate key in a step)" check-check-workflow-trigger
check_seed $CW "$MAKE_STEP" "${MAKE_STEP}"'        <<: *step\n'
expect_fail "check workflow trigger (merge key in a step)" check-check-workflow-trigger
check_seed $CW "$MAKE_STEP" '      - run: make lint\n'
expect_fail "check workflow trigger (make check step removed)" check-check-workflow-trigger
# A more-indented line continues a plain scalar: GitHub runs `make check
# --just-print`, while a line reader sees `make check`. Each step's values must
# be one line, and a block scalar (| or >) is refused the same way.
check_seed $CW "$MAKE_STEP" "${MAKE_STEP}"'          --just-print\n'
expect_fail "check workflow trigger (make check run continues with --just-print)" check-check-workflow-trigger
check_seed $CW "$MAKE_STEP" "${MAKE_STEP}"'          || true\n'
expect_fail "check workflow trigger (make check run continues with || true)" check-check-workflow-trigger
check_seed $CW "$MAKE_STEP" "${MAKE_STEP}"'      - run: |\n          make check\n'
expect_fail "check workflow trigger (block scalar run)" check-check-workflow-trigger
# An empty block scalar has no lines below it, so only the indicator shows it.
check_seed $CW "$MAKE_STEP" "${MAKE_STEP}"'      - run: >-\n'
expect_fail "check workflow trigger (empty block scalar run)" check-check-workflow-trigger
# A step may hold only name, uses, with, run and id. shell and env can change what
# `make check` does without changing its text.
check_seed $CW "$MAKE_STEP" "${MAKE_STEP}"'        shell: bash -n {0}\n'
expect_fail "check workflow trigger (shell on the make check step)" check-check-workflow-trigger
check_seed $CW "$MAKE_STEP" "${MAKE_STEP}"'        env:\n          MAKEFLAGS: --just-print\n'
expect_fail "check workflow trigger (env on the make check step)" check-check-workflow-trigger
check_seed $CW "$CJOB" "${CJOB}"'    env:\n      MAKEFLAGS: --just-print\n'
expect_fail "check workflow trigger (env on the check job)" check-check-workflow-trigger
check_seed $CW "$CJOB" "${CJOB}"'    defaults:\n      run:\n        shell: bash -n {0}\n'
expect_fail "check workflow trigger (defaults on the check job)" check-check-workflow-trigger
check_seed $CW 'jobs:\n' 'defaults:\n  run:\n    shell: bash -n {0}\n\njobs:\n'
expect_fail "check workflow trigger (workflow-level defaults)" check-check-workflow-trigger
check_seed $CW 'jobs:\n' 'env:\n  MAKEFLAGS: --just-print\n\njobs:\n'
expect_fail "check workflow trigger (workflow-level env)" check-check-workflow-trigger
# The OPPOSITE failure: keys that cannot make the check pass without the gate
# must not be refused.
check_seed $CW "$CJOB" "${CJOB}"'    permissions:\n      contents: read\n'
expect_pass "check workflow trigger (permissions on the check job)" check-check-workflow-trigger
check_seed $CW "$CJOB" "${CJOB}"'    timeout-minutes: 30\n'
expect_pass "check workflow trigger (timeout-minutes on the check job)" check-check-workflow-trigger
check_seed $CW "$MAKE_STEP" "${MAKE_STEP}${MAKE_STEP}"
expect_pass "check workflow trigger (two make check steps)" check-check-workflow-trigger
# A comment after `with:` leaves its value empty; the mapping below it is not a
# continued scalar.
check_seed $CW '        with:\n          path: ~/.cache/golangci-lint\n' '        with: # tool cache\n          path: ~/.cache/golangci-lint\n'
expect_pass "check workflow trigger (comment after with:)" check-check-workflow-trigger

# Ruleset: the context is the job's reported name, which is the job id, or its
# `name:` when one is set. A rename and an added name both orphan it.
check_seed $CW "$CJOB" '  gate:\n    runs-on: ubuntu-latest\n'
expect_fail "check workflow trigger (check job renamed)" check-check-workflow-trigger
check_seed $CW "$CJOB" '  check:\n    name: gate\n    runs-on: ubuntu-latest\n'
expect_fail "check workflow trigger (check job given another name)" check-check-workflow-trigger
# A ruleset that names no required check makes that comparison vacuous.
python3 - <<'SEED'
import json

p = '.github/rulesets/main.json'
data = json.load(open(p))
before = len(data['rules'])
data['rules'] = [r for r in data['rules'] if r['type'] != 'required_status_checks']
if len(data['rules']) != before - 1:
    raise SystemExit('expected one required_status_checks rule')
open(p, 'w').write(json.dumps(data, indent=2) + '\n')
SEED
expect_fail "check workflow trigger (ruleset names no required check)" check-check-workflow-trigger
rm .github/rulesets/main.json
expect_fail "check workflow trigger (ruleset file missing)" check-check-workflow-trigger

# One producer: no other job, in any workflow, may report the same name. It
# could satisfy the requirement without `make check`.
# The renamed job also runs `make check`, so it passes every job rule and only
# the duplicate refusal can fail this seed.
check_seed $CW '  gate-selftest:\n' '  gate-selftest:\n    name: check\n'
check_seed $CW '      - run: make deps\n' "$MAKE_STEP"
expect_fail "check workflow trigger (second job named check)" check-check-workflow-trigger
check_seed $HW "$HJOB" '  check:\n    runs-on: ubuntu-latest\n    steps:\n      - run: "true"\n\n'"$HJOB"
expect_fail "check workflow trigger (job id check in another workflow)" check-check-workflow-trigger
check_seed $HW "$HJOB" '  sneaky: {name: check, runs-on: ubuntu-latest, steps: [{run: "true"}]}\n\n'"$HJOB"
expect_fail "check workflow trigger (flow-style job in another workflow)" check-check-workflow-trigger
printf 'on: push\njobs: {check: {runs-on: ubuntu-latest, steps: [{run: "true"}]}}\n' > .github/workflows/sneaky.yaml
expect_fail "check workflow trigger (flow-style jobs: in another workflow)" check-check-workflow-trigger
check_seed $HW "$HJOB" '  sneaky:\n    name: ${{ '"'check'"' }}\n    runs-on: ubuntu-latest\n    steps:\n      - run: "true"\n\n'"$HJOB"
expect_fail "check workflow trigger (expression job name in another workflow)" check-check-workflow-trigger
check_seed $HW "$HJOB" '  sneaky:\n    "name": check\n    runs-on: ubuntu-latest\n    steps:\n      - run: "true"\n\n'"$HJOB"
expect_fail "check workflow trigger (quoted name: key in another workflow)" check-check-workflow-trigger
# A job name must be a plain one-line scalar or a simply quoted string. A quoted
# `check` is still `check`; an escape or a folded scalar cannot be read plainly.
check_seed $HW "$HJOB" '  sneaky:\n    name: "check"\n    runs-on: ubuntu-latest\n    steps:\n      - run: "true"\n\n'"$HJOB"
expect_fail "check workflow trigger (quoted job name check in another workflow)" check-check-workflow-trigger
check_seed $HW "$HJOB" '  sneaky:\n    name: "che\x63k"\n    runs-on: ubuntu-latest\n    steps:\n      - run: "true"\n\n'"$HJOB"
expect_fail "check workflow trigger (escaped job name in another workflow)" check-check-workflow-trigger
check_seed $HW "$HJOB" '  sneaky:\n    name: >-\n      check\n    runs-on: ubuntu-latest\n    steps:\n      - run: "true"\n\n'"$HJOB"
expect_fail "check workflow trigger (folded job name in another workflow)" check-check-workflow-trigger
check_seed $HW "$HJOB" '  sneaky:\n    name: ch\n      eck\n    runs-on: ubuntu-latest\n    steps:\n      - run: "true"\n\n'"$HJOB"
expect_fail "check workflow trigger (multi-line plain job name in another workflow)" check-check-workflow-trigger
check_seed $HW "$HJOB" '  sneaky:\n    name: c${{ matrix.x }}\n    runs-on: ubuntu-latest\n    steps:\n      - run: "true"\n\n'"$HJOB"
expect_fail "check workflow trigger (expression name with a prefix of check)" check-check-workflow-trigger
# The OPPOSITE failure: an expression name whose literal prefix rules out
# `check`, and a quoted key that is not a job id or name, protect nothing.
check_seed $HW "$HJOB" '  sneaky:\n    name: govulncheck-${{ matrix.module }}\n    runs-on: ubuntu-latest\n    steps:\n      - run: "true"\n\n'"$HJOB"
expect_pass "check workflow trigger (expression name that cannot be check)" check-check-workflow-trigger
check_seed $HW "${HJOB}"'    runs-on: ubuntu-latest\n' "${HJOB}"'    runs-on: ubuntu-latest\n    "timeout-minutes": 30\n'
expect_pass "check workflow trigger (quoted key elsewhere in another workflow)" check-check-workflow-trigger
check_seed $HW 'name: hermetic-smoke\n' '"name": hermetic-smoke\n'
expect_pass "check workflow trigger (quoted top-level key in another workflow)" check-check-workflow-trigger
# But a quoted key with an escape decodes to another key: "na\x6de" is `name`,
# "jo\x62s" is `jobs`, "che\x63k" is the job id `check`. Stripping the quotes
# without decoding misreads all three, so any quoted key with a backslash is
# refused, in every workflow.
check_seed $HW "$HJOB" '  sneaky:\n    "na\x6de": check\n    runs-on: ubuntu-latest\n    steps:\n      - run: "true"\n\n'"$HJOB"
expect_fail "check workflow trigger (escaped name key in another workflow)" check-check-workflow-trigger
cat > .github/workflows/sneaky.yaml <<'WORKFLOW'
on: push
"jo\x62s":
  check:
    runs-on: ubuntu-latest
    steps:
      - run: "true"
WORKFLOW
expect_fail "check workflow trigger (escaped jobs key in another workflow)" check-check-workflow-trigger
check_seed $HW "$HJOB" '  "che\x63k":\n    runs-on: ubuntu-latest\n    steps:\n      - run: "true"\n\n'"$HJOB"
expect_fail "check workflow trigger (escaped job id in another workflow)" check-check-workflow-trigger
# The OPPOSITE failure: a job whose id is `check` but whose reported name is
# something else does not report the context, so it must not be refused.
check_seed $HW "$HJOB" '  check:\n    name: unrelated\n    runs-on: ubuntu-latest\n    steps:\n      - run: "true"\n\n'"$HJOB"
expect_pass "check workflow trigger (job id check reports another name)" check-check-workflow-trigger

# 14. Every tracked generated API output must have a registry entry. Otherwise
#     deleting an entry stops regeneration and leaves the stale tracked file
#     outside the drift comparison.
expect_pass "generated API registry (clean baseline)" check-generate
python3 - <<'SEED'
p = 'scripts/generate-api.sh'
s = open(p).read()
needle = '  "typescript|services/access/api/openapi.yaml|web/scanner/src/access-api-types.gen.ts|"\n'
if s.count(needle) != 1:
    raise SystemExit('expected one scanner generator registry entry')
open(p, 'w').write(s.replace(needle, '', 1))
SEED
expect_fail "generated API registry (tracked output entry deleted)" check-generate

# 14b. Generated drift must be measured against HEAD, not the index. A contributor
#     can stage a contract and its regenerated clients before running the gate;
#     comparing only worktree to index calls that clean even though the generated
#     files are still absent from HEAD.
expect_pass "generated API drift (clean baseline)" check-generate
python3 - <<'SEED'
p = 'services/catalog/api/openapi.yaml'
s = open(p).read()
s = s.replace('    PublicChannelList:\n',
              '    SelftestGeneratedShape:\n'
              '      type: object\n'
              '      properties:\n'
              '        marker: { type: string }\n'
              '    PublicChannelList:\n', 1)
open(p, 'w').write(s)
SEED
make generate >/dev/null
git add services/catalog/api/openapi.yaml $(./scripts/generate-api.sh outputs)
expect_fail "generated API drift (staged contract and outputs)" check-generate

if [ "$fail_count" -gt 0 ]; then
  echo "gate-selftest: $fail_count seeded error(s) were NOT caught"
  exit 1
fi
echo "gate-selftest: all seeded errors caught"
