#!/usr/bin/env bash
# Sourced, not executed: the isolated-stack preamble shared by scripts/smoke.sh
# (the gate) and scripts/browser.sh (the browser-submit gate). Usage:
#
#   ROOT="$(cd "$(dirname "$0")/.." && pwd)"
#   . "$ROOT/scripts/stack-env.sh" <stack-name>
#
# Sets PROJECT, every *_PORT, and the four service credentials. This lives in one
# file because the two stacks must agree: browser.sh started life as a per-ticket
# copy of these lines with the ports hardcoded, and 18099 — the gateway port that
# copy picked — is smoke's own gateway port at slot 19. Two tickets' worth of
# copies is what promoted this into a shared file (TKT-228).

STACK="${1:?stack-env.sh needs a stack name}"
: "${ROOT:?stack-env.sh needs ROOT}"

# Isolate the stack per checkout AND per stack kind — project name AND ports,
# derived from the checkout path plus the stack name.
#
# This repo is routinely worked on in sibling worktrees, and both halves of the isolation are
# load-bearing. A shared project name is worse than a port clash: smoke.sh's `cleanup` runs
# `compose down -v` on EXIT, so a second run tears the first one's stack down *mid-run* — and the
# wreckage reads like a broken change rather than a collision ("network ... not found",
# "nats-1 exited (0)", "dead or marked for removal"). That cost real debugging time on TKT-61.
# Fixing only the name would trade silent mutual destruction for a port clash, which is at least
# honest; moving the ports too lets the runs actually coexist.
#
# The slot is a stable hash of the checkout path and the stack name: same worktree and stack →
# same ports every run (greppable, debuggable); different worktrees, or the browser stack
# alongside the smoke stack in one worktree, → different ports. Distinct hosts/ranges keep the
# six services from colliding with each other. A hash collision just fails loudly on "port
# already allocated" — the honest failure this replaces the silent one with.
SLOT=$(( $(printf '%s/%s' "$ROOT" "$STACK" | cksum | cut -d' ' -f1) % 40 ))
PROJECT="${SMOKE_COMPOSE_PROJECT:-ticketing-${STACK}-${SLOT}}"
export GATEWAY_PORT=$((18080 + SLOT)) POSTGRES_PORT=$((15432 + SLOT)) NATS_PORT=$((14222 + SLOT)) \
       GRAFANA_PORT=$((13000 + SLOT)) PROM_PORT=$((19090 + SLOT)) OTLP_PORT=$((14318 + SLOT)) \
       CATALOG_PORT=$((15080 + SLOT)) INVENTORY_PORT=$((16080 + SLOT)) COMMERCE_PORT=$((17080 + SLOT)) PAYMENTS_PORT=$((17580 + SLOT)) \
       ACCESS_PORT=$((18580 + SLOT)) \
       ACCESS_EVENT_RETRY_BACKOFF=100ms,200ms,400ms,800ms,1s,1s \
       ACCESS_LIFECYCLE_CHECKPOINT_INTERVAL=1s
echo "$STACK: project=$PROJECT gateway=$GATEWAY_PORT postgres=$POSTGRES_PORT nats=$NATS_PORT (slot $SLOT from $ROOT)"

# One credential per invocation (TKT-83): generated here so the isolated
# stack never depends on a developer's .env or shell (the export takes
# precedence over compose's .env lookup) and CI needs no secret.
SMOKE_INTERNAL_TOKEN=$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')
export SMOKE_INTERNAL_TOKEN
export INTERNAL_SERVICE_TOKEN="$SMOKE_INTERNAL_TOKEN"
# TKT-191: catalog's staff-write credential. Generated INDEPENDENTLY of the
# internal token — they authorize different things, and a run where one value
# served both would pass while proving nothing about the separation.
SMOKE_CATALOG_STAFF_WRITE_TOKEN=$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')
export SMOKE_CATALOG_STAFF_WRITE_TOKEN
export CATALOG_STAFF_WRITE_TOKEN="$SMOKE_CATALOG_STAFF_WRITE_TOKEN"
# TKT-245: catalog's organizer-assertion signing key. A separate /dev/urandom
# read for a reason the ticket turns on -- catalog refuses to start when this
# equals CATALOG_STAFF_WRITE_TOKEN, because a signing key equal to the write
# credential lets anyone who can write mint their own tenancy. Deriving one from
# the other here would make the smoke suite the one place that guard is never
# exercised honestly.
SMOKE_CATALOG_ORGANIZER_ASSERTION_KEY=$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')
export SMOKE_CATALOG_ORGANIZER_ASSERTION_KEY
export CATALOG_ORGANIZER_ASSERTION_KEY="$SMOKE_CATALOG_ORGANIZER_ASSERTION_KEY"
# TKT-194. A separate /dev/urandom read, not a copy: commerce refuses to start
# when this equals INTERNAL_SERVICE_TOKEN, and the back office refuses to refund
# when it equals CATALOG_STAFF_WRITE_TOKEN. Deriving one from another here would
# make the smoke suite the one place those guards are never exercised honestly.
SMOKE_COMMERCE_STAFF_WRITE_TOKEN=$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')
export SMOKE_COMMERCE_STAFF_WRITE_TOKEN
export COMMERCE_STAFF_WRITE_TOKEN="$SMOKE_COMMERCE_STAFF_WRITE_TOKEN"
# TKT-221: the customer checkout assertion key. A FOURTH independent /dev/urandom
# read, not a copy — commerce refuses to start when it equals either other
# credential, and a run where one value served two would pass while proving
# nothing about the separation those refusals exist to enforce.
SMOKE_COMMERCE_CUSTOMER_ASSERTION_KEY=$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')
export SMOKE_COMMERCE_CUSTOMER_ASSERTION_KEY
export COMMERCE_CUSTOMER_ASSERTION_KEY="$SMOKE_COMMERCE_CUSTOMER_ASSERTION_KEY"
# TKT-244 / ADR-057: inventory's staff-write credential, for the back office's
# channel-allocation editor. A FIFTH independent /dev/urandom read, not a copy —
# inventory refuses to start when this equals INTERNAL_SERVICE_TOKEN, and the back
# office refuses to boot when it equals either credential it already holds. Deriving
# one from another here would make the smoke suite the one place those guards are
# never exercised honestly.
SMOKE_INVENTORY_STAFF_WRITE_TOKEN=$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')
export SMOKE_INVENTORY_STAFF_WRITE_TOKEN
export INVENTORY_STAFF_WRITE_TOKEN="$SMOKE_INVENTORY_STAFF_WRITE_TOKEN"
# TKT-203 / ADR-068: access's staff-write credential, for the back office's ticket
# resend. A SIXTH independent /dev/urandom read, for the same reason the fifth is
# one — access refuses to start when this equals INTERNAL_SERVICE_TOKEN, and the
# back office refuses to boot when it equals any credential it already holds.
# Deriving one from another here would make the smoke suite the one place those
# guards are never exercised honestly.
SMOKE_ACCESS_STAFF_WRITE_TOKEN=$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')
export SMOKE_ACCESS_STAFF_WRITE_TOKEN
export ACCESS_STAFF_WRITE_TOKEN="$SMOKE_ACCESS_STAFF_WRITE_TOKEN"
# ai-review S11: database passwords. The roles' passwords used to equal their
# names, committed. One draw per role, and exported TWICE: the compose stack reads
# <ROLE>_DB_PASSWORD, and the smoke test process reads SMOKE_DB_<ROLE>_PASSWORD to
# build its own connections (smoke/smoke_test.go: dsn).
export POSTGRES_PASSWORD="$(od -An -tx1 -N24 /dev/urandom | tr -d ' \n')"
# The superuser under the same SMOKE_DB_<ROLE>_PASSWORD convention: the load and
# read-proof suites connect as `postgres` for pg_stat_statements, through the same
# dsn() helper, so it needs the alias or it silently falls back to the old literal.
export SMOKE_DB_POSTGRES_PASSWORD="$POSTGRES_PASSWORD"
for role in CATALOG INVENTORY COMMERCE PAYMENTS ACCESS; do
	password=$(od -An -tx1 -N24 /dev/urandom | tr -d ' \n')
	export "${role}_DB_PASSWORD=$password"
	export "SMOKE_DB_${role}_PASSWORD=$password"
done
unset password

# ai-review S8: payments' own credential. A FIFTH independent /dev/urandom read —
# commerce refuses to start when it equals any of its other three, so deriving it
# from one of them here would make the smoke suite the one place that guard is
# never exercised honestly.
SMOKE_PAYMENTS_INTERNAL_TOKEN=$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')
export SMOKE_PAYMENTS_INTERNAL_TOKEN
export PAYMENTS_INTERNAL_TOKEN="$SMOKE_PAYMENTS_INTERNAL_TOKEN"
# ai-review S2: the QR image-link key. Its own draw — it proves a URL is fresh,
# which is a different claim from the QR credential's, and one key making both
# claims spends a cheap leak at an expensive price.
export ACCESS_TICKET_LINK_KEY="$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')"
export ACCESS_FEED_CURSOR_KEY="$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')"
# ai-review S5: the three signing keys. They used to arrive as compose defaults —
# a readable journal key and two Ed25519 seeds committed to this repository — and
# the stack now refuses to start without them, so the isolated stacks mint their
# own here for the same reason they mint the tokens: never depend on a
# developer's .env, and give CI no secret to hold.
#
# The QR seed is exported for the TEST PROCESS as well as the stack. smoke's
# forged-payload cases sign a QR the gate must reject for reasons other than the
# signature — a claim mismatch, a wrong organizer — and that needs the same key
# the stack issues under. It is a per-run throwaway; the point of removing the
# default was that the OLD one was permanent and public.
export JOURNAL_SIGNING_KEY="$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')"
read -r SMOKE_ACCESS_QR_SEED SMOKE_ACCESS_QR_PUB < <(cd "$ROOT/services/access" && go run ./cmd/access keygen)
export SMOKE_ACCESS_QR_SEED
export ACCESS_QR_KID="access-qr/local-v1"
export ACCESS_QR_PRIVATE_KEY="$SMOKE_ACCESS_QR_SEED"
export ACCESS_QR_PUBLIC_KEYS="$ACCESS_QR_KID=$SMOKE_ACCESS_QR_PUB"
# An INDEPENDENT pair, not a copy: ADR-021 §D4 separates credential signing from
# history signing, and a run where one key served both would pass while proving
# nothing about the separation the namespaces exist to enforce.
read -r SMOKE_ACCESS_LIFECYCLE_SEED SMOKE_ACCESS_LIFECYCLE_PUB < <(cd "$ROOT/services/access" && go run ./cmd/access keygen)
export ACCESS_LIFECYCLE_KID="access-lifecycle/local-v1"
export ACCESS_LIFECYCLE_PRIVATE_KEY="$SMOKE_ACCESS_LIFECYCLE_SEED"
export ACCESS_LIFECYCLE_PUBLIC_KEYS="$ACCESS_LIFECYCLE_KID=$SMOKE_ACCESS_LIFECYCLE_PUB"
