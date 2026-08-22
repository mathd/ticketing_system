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
trap cleanup EXIT INT TERM

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

# 8b. Build-list LAG (TKT-265) — the vertical direction check-dep-drift is blind to.
#     The seed MUST be a dependency declared by exactly ONE module, or it diverges
#     horizontally and check-dep-drift — which runs earlier in `check` — fires first;
#     the seeded case would then go red for the wrong reason and this stage would
#     never execute. `golang.org/x/net` is declared only by shared/go, so lowering it
#     produces lag with no horizontal divergence: a mutation only this checker catches.
#     That isolation is asserted, not assumed — check-dep-drift must stay GREEN on the
#     same seeded tree, which is the pairing that proves the two stages are distinct.
expect_pass "go build-list lag (clean baseline)" check-build-list-lag
#     The isolation assertion below would pass vacuously if the seed silently failed
#     to apply — "check-dep-drift is green" is equally true of an unseeded tree. So
#     confirm the seed is actually present before asserting anything about it.
(cd shared/go && go mod edit -require=golang.org/x/net@v0.50.0)
grep -q 'golang.org/x/net v0.50.0' shared/go/go.mod || {
  echo "FAIL: build-list-lag seed did not apply — the isolation assertion would be vacuous"
  fail_count=$((fail_count + 1))
}
expect_pass "go build-list lag seed does not disturb check-dep-drift" check-dep-drift
(cd shared/go && go mod edit -require=golang.org/x/net@v0.50.0)
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
#     The seeded requirement forces a checksum the cache does not have, so this
#     asserts the property in the state where it actually fails.
(cd shared/go && go mod edit -require=golang.org/x/net@v0.50.0)
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
#     copy. The copy exists to keep checksum writes off the real tree, but it must
#     still describe the same build: `go work edit -json` exposes only Go, Use and
#     Replace, so an earlier version that REGENERATED go.work from that JSON
#     silently dropped `godebug` — and the fidelity guard, which compares module
#     directories, passed over it. The copy is therefore the original file with
#     only its paths redirected. Asserted through the stage's own verdict: a
#     godebug-bearing workspace must still check clean.
go work edit -godebug=default=go1.25
expect_pass "go build-list lag (godebug survives the workspace copy)" check-build-list-lag

# 9. A credential compose.yaml refuses to start without, that env-bootstrap.sh
#    never generates (TKT-227). TKT-244 shipped exactly this: `make up` died on
#    interpolation while `make check` stayed green, because the smoke path takes
#    its environment from scripts/stack-env.sh instead. The positive control is
#    not optional here — this seed mutates an existing file, so a checker that
#    always failed would satisfy its own expect_fail.
expect_pass "required stack env (clean baseline)" check-required-env
sed -i 's/ INVENTORY_STAFF_WRITE_TOKEN//' scripts/env-bootstrap.sh
expect_fail "required stack env (compose requires a variable nothing generates)" check-required-env

# 9b. Present-but-EMPTY is not generated (ai-review [high]). `${VAR:?}` rejects an
#     empty value exactly as it rejects an unset one, so a checker comparing only
#     NAMES passes here while `make up` still dies. Deleting a name (9 above)
#     cannot expose that, which is why this is its own seed.
printf '\nenv_set INVENTORY_STAFF_WRITE_TOKEN ""\n' >> scripts/env-bootstrap.sh
expect_fail "required stack env (a required credential generated empty)" check-required-env

# 9c. The OTHER required form (ai-review [medium]). Compose supports `${VAR?msg}`
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
#     missing credential and fail the gate on a config that starts perfectly well
#     (ai-review pass 2). This is an expect_PASS: the seed must change nothing.
sed -i '1i # a commented placeholder: ${SELFTEST_COMMENTED:?never a requirement}' compose.yaml
sed -i 's|^\( *\)INVENTORY_STAFF_WRITE_TOKEN: \${INVENTORY_STAFF_WRITE_TOKEN:?|\1SELFTEST_LITERAL: "$${SELFTEST_ESCAPED:?never a requirement}"\n\1INVENTORY_STAFF_WRITE_TOKEN: ${INVENTORY_STAFF_WRITE_TOKEN:?|' compose.yaml
expect_pass "required stack env (comments and \$\$-escapes are not requirements)" check-required-env

# 10. A repeated ADR number. Two Accepted ADRs both numbered 055 shipped and made
#     every bare `ADR-055` citation in code, migrations, OpenAPI and AGENTS.md
#     ambiguous; nothing in the gate noticed for nine days. The seed reproduces
#     that exact state by copying an existing ADR onto a number already taken.
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

if [ "$fail_count" -gt 0 ]; then
  echo "gate-selftest: $fail_count seeded error(s) were NOT caught"
  exit 1
fi
echo "gate-selftest: all seeded errors caught"
