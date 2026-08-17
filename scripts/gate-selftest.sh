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

if [ "$fail_count" -gt 0 ]; then
  echo "gate-selftest: $fail_count seeded error(s) were NOT caught"
  exit 1
fi
echo "gate-selftest: all seeded errors caught"
