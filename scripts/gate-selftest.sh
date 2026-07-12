#!/usr/bin/env bash
# Gate self-test (US-001 AC): prove `make check` FAILS on seeded errors.
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
ln -s "$ROOT/node_modules" node_modules 2>/dev/null || true
ln -s "$ROOT/web/scanner/node_modules" web/scanner/node_modules 2>/dev/null || true

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

# 3. TS lint violation (oxlint no-debugger)
cat > web/scanner/src/seeded.ts <<'EOF'
export function seeded(): void {
  debugger
}
EOF
expect_fail "ts lint" lint-ts

# 4. Failing vitest test
cat > web/scanner/src/seeded.test.ts <<'EOF'
import { expect, it } from 'vitest'
it('seeded failure', () => { expect(1).toBe(2) })
EOF
expect_fail "ts test" test-ts

# 5. TS type error (tsc, build stage)
cat > web/scanner/src/seeded.ts <<'EOF'
export const seeded: number = 'not a number'
EOF
expect_fail "ts build" build-ts

if [ "$fail_count" -gt 0 ]; then
  echo "gate-selftest: $fail_count seeded error(s) were NOT caught"
  exit 1
fi
echo "gate-selftest: all seeded errors caught"
