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

if [ "$fail_count" -gt 0 ]; then
  echo "gate-selftest: $fail_count seeded error(s) were NOT caught"
  exit 1
fi
echo "gate-selftest: all seeded errors caught"
