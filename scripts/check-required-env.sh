#!/usr/bin/env bash
# TKT-227. The invariant:
#
#   Every credential the development stack refuses to start without is produced
#   by the one command a developer is told to run.
#
# It was violated for two days. TKT-244 made INVENTORY_STAFF_WRITE_TOKEN
# mandatory in compose.yaml (`${VAR:?}`, which is a hard interpolation failure)
# and did not extend scripts/env-bootstrap.sh, so `make up` died before it
# reached any image build — telling the developer to "run 'make up' once to
# generate a local credential", which is the command that had just failed.
#
# `make check` stayed green throughout, because its smoke stage sources
# scripts/stack-env.sh, and THAT script does generate the token. The two env
# paths diverged and only one of them is in the gate. This checker exists to
# make that divergence impossible to ship again.
#
# The expected set is derived from the REQUIREMENT (compose.yaml's `:?` markers),
# never from what env-bootstrap.sh happens to emit. Deriving it from a run would
# pin the behaviour instead of the rule, and would have called the defect correct.
#
# Deliberately Docker-free: the CI job that runs this (gate-selftest, in
# .github/workflows/check.yaml) installs Go, pnpm and Node and no Docker, so
# asking Compose to resolve the file would not be a standing guard there. Parsing
# compose.yaml recovers the same set in milliseconds.
#
# It never prints a credential VALUE — only names.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

COMPOSE_FILE="compose.yaml"

# Every variable compose.yaml marks mandatory: `${NAME:?message}`. The `:?` form
# is a hard failure at interpolation time, which is exactly the class of value a
# developer cannot start the stack without.
required_vars() {
	grep -oE '\$\{[A-Z_][A-Z0-9_]*:\?' "$COMPOSE_FILE" \
		| sed -e 's/^\${//' -e 's/:?$//' \
		| sort -u
}

# What a developer actually ends up with: run env-bootstrap.sh for real, in a
# throwaway directory against an absent .env, and read back the names it wrote.
#
# Running it beats reading its source. The source form is an implementation
# detail (today: three loops, a keypair helper and some one-offs), and a checker
# that scraped variable names out of the text would go green on a script that
# lists a name and fails to write it. What matters to `make up` is the file that
# exists afterwards.
#
# The developer's own .env is never read, written or consulted: the run happens
# in a mktemp directory holding a COPY of the working tree's scripts/ plus
# symlinks to what `access keygen` needs to mint the two Ed25519 pairs.
#
# It must copy the WORKING TREE, not HEAD. A `git worktree add HEAD` would be the
# obvious idiom (scripts/gate-selftest.sh uses it) and would be wrong twice here:
# it would report on committed state while an uncommitted repair sat in the tree,
# and — worse — gate-selftest.sh seeds its mutations INSIDE its own worktree, so
# checking out HEAD again would silently undo the seeded deletion and the
# expect_fail case could never fail. A guard whose fixture cannot reach the
# failing state is the trap this ticket is about.
#
# env-bootstrap.sh runs `go run ./cmd/access keygen` from services/access, which
# needs the Go WORKSPACE, not just that one module — so every directory go.work
# names is linked in, not only services/. Linking a partial set makes keygen fail
# to build, for a reason that has nothing to do with this check.
generated_vars() {
	local tmp status dir
	tmp="$(mktemp -d)"

	mkdir -p "$tmp/root/scripts"
	cp "$ROOT/scripts/env-bootstrap.sh" "$tmp/root/scripts/env-bootstrap.sh"
	for dir in gateway services shared smoke go.work go.work.sum; do
		[ -e "$ROOT/$dir" ] && ln -s "$ROOT/$dir" "$tmp/root/$dir"
	done

	status=0
	# No .env in the sandbox: the state this must measure is a clean checkout.
	( cd "$tmp/root" && ./scripts/env-bootstrap.sh ) >"$tmp/log" 2>&1 || status=1

	if [ "$status" -ne 0 ]; then
		echo "check-required-env: scripts/env-bootstrap.sh failed on a clean checkout:" >&2
		sed -e 's/^/    /' "$tmp/log" >&2
	else
		# Names only. No value ever leaves this function.
		sed -nE 's/^([A-Z_][A-Z0-9_]*)[[:space:]]*=.*/\1/p' "$tmp/root/.env" | sort -u
	fi

	rm -rf "$tmp"
	return "$status"
}

# A parser that matches nothing would report "no missing variables" and pass —
# a precondition that cannot fail is worse than none (AGENTS.md). The stack has
# had at least this many mandatory variables since TKT-244; if the count drops,
# the parse broke or the compose file changed shape, and either way this checker
# has stopped meaning what it claims. Raise the floor deliberately, never lower
# it to make a run go green.
MIN_REQUIRED=19

main() {
	local required missing count
	required="$(required_vars)"
	count="$(printf '%s\n' "$required" | grep -c . || true)"

	if [ "$count" -lt "$MIN_REQUIRED" ]; then
		echo "check-required-env: parsed only $count required variables from $COMPOSE_FILE," >&2
		echo "  expected at least $MIN_REQUIRED. The parse is broken or compose.yaml changed" >&2
		echo "  shape — this checker is not measuring what it claims. Fix it, do not lower" >&2
		echo "  MIN_REQUIRED to make this pass." >&2
		return 1
	fi

	local generated
	generated="$(generated_vars)"

	missing="$(comm -23 <(printf '%s\n' "$required") <(printf '%s\n' "$generated"))"

	if [ -n "$missing" ]; then
		echo "check-required-env: $COMPOSE_FILE requires variables that scripts/env-bootstrap.sh" >&2
		echo "  never generates, so 'make up' fails before it builds anything:" >&2
		printf '    %s\n' $missing >&2
		echo "  Add each to scripts/env-bootstrap.sh with its OWN /dev/urandom draw — a shared" >&2
		echo "  value satisfies 'is present' and still refuses to boot (see ADR-057)." >&2
		return 1
	fi

	echo "check-required-env: all $count mandatory stack variables are generated by env-bootstrap.sh"
}

main "$@"
