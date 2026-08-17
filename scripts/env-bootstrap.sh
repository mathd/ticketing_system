#!/usr/bin/env bash
# One-time local credential bootstrap for `make up` (TKT-83, extended by
# ai-review S5). No secret ships in this repo with a working default.
#
# It preserves unrelated .env entries, replaces only a missing or RETIRED value,
# and never prints one. Compose reads .env natively, so a bare
# `docker compose up` keeps working after the first `make up`.
#
# Two kinds of secret, generated separately, and the separation is the point:
# every value below is read from its own /dev/urandom draw or its own keygen
# call, so one leaking never implies another. The services enforce that from
# their side too — commerce refuses to start when any two of its three arrive
# equal.
#
#   1. Opaque tokens and HMAC keys — a plain random hex string.
#      INTERNAL_SERVICE_TOKEN opens every service's internal surface;
#      CATALOG_STAFF_WRITE_TOKEN opens catalog writes; COMMERCE_STAFF_WRITE_TOKEN
#      opens exactly one commerce operation (the staff refund);
#      INVENTORY_STAFF_WRITE_TOKEN opens exactly the two operations the back
#      office's channel-allocation editor needs (TKT-244, ADR-057) — inventory
#      REFUSES TO START when it equals INTERNAL_SERVICE_TOKEN, so its own draw
#      below is load-bearing, not tidiness;
#      COMMERCE_CUSTOMER_ASSERTION_KEY signs proofs that a checkout belongs to a
#      customer; PAYMENTS_INTERNAL_TOKEN opens payments' money surface, split off
#      the shared token by ai-review S8; JOURNAL_SIGNING_KEY signs the payments
#      journal; ACCESS_TICKET_LINK_KEY signs the short-lived QR image links
#      (ai-review S2).
#
#   2. Ed25519 signing PAIRS — minted by `access keygen`, because a shell cannot
#      derive a public key from a seed. The private seed and the public keyring
#      are written TOGETHER and never independently: a fresh seed left beside the
#      old public key breaks verification, and a fresh public key left beside the
#      old seed keeps the old seed a valid signer. That second direction is why
#      this regenerates the pair when EITHER half is missing or retired.
#
# The retired literals are refused forever by the binaries themselves
# (shared/go/runtimecfg). They were active Compose defaults once, which means
# they are published key material, not secrets that merely leaked.
set -euo pipefail
umask 077

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

# The retired checked-in defaults. Kept in step with shared/go/runtimecfg.
RETIRED_TOKEN='local-service-token'
RETIRED_JOURNAL_KEY='local-development-journal-key'
RETIRED_QR_SEED='AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'
RETIRED_LIFECYCLE_SEED='AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyA'

# Reads one .env value, unquoted. Last assignment wins, as it does for Compose.
# "not present" is an ordinary answer here, not a failure — without the `|| true`
# grep's exit 1 would take the whole script down under `set -e -o pipefail` on the
# very first variable that has yet to be generated.
env_value() {
	grep -E "^$1[[:space:]]*=" .env 2>/dev/null | tail -n1 \
		| sed -e 's/^[^=]*=[[:space:]]*//' | tr -d '\r' \
		| sed -e 's/^"//' -e 's/"$//' -e "s/^'//" -e "s/'$//" || true
}

# Replaces one .env value, preserving every other line.
env_set() {
	rm -f .env.tmp
	{ grep -vE "^$1[[:space:]]*=" .env 2>/dev/null || true; printf '%s=%s\n' "$1" "$2"; } > .env.tmp
	mv .env.tmp .env
	echo "generated $1 in .env"
}

# needs_generation <var> <retired-literal>
needs_generation() {
	local current
	current="$(env_value "$1")"
	[ -z "$current" ] || [ "$current" = "$2" ]
}

# Each name gets its own iteration and therefore its own /dev/urandom read.
# `scripts/check-required-env.sh` asserts this list keeps up with every variable
# compose.yaml marks mandatory: TKT-244 added INVENTORY_STAFF_WRITE_TOKEN to
# compose and not here, and `make up` failed on interpolation — telling the
# developer to run `make up` to generate it (TKT-227).
for var in INTERNAL_SERVICE_TOKEN CATALOG_STAFF_WRITE_TOKEN CATALOG_ORGANIZER_ASSERTION_KEY COMMERCE_STAFF_WRITE_TOKEN COMMERCE_CUSTOMER_ASSERTION_KEY INVENTORY_STAFF_WRITE_TOKEN PAYMENTS_INTERNAL_TOKEN ACCESS_TICKET_LINK_KEY; do
	if needs_generation "$var" "$RETIRED_TOKEN"; then
		env_set "$var" "$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')"
	fi
done

# Database passwords (ai-review S11). The superuser's was the committed literal
# "postgres" and each service role's was its own NAME, so anything that could
# reach the published port owned every database without needing to read a file.
#
# One per role, not one shared: ADR-007 gives each service its own database and
# revokes CONNECT from PUBLIC precisely so one service's credentials cannot reach
# another's data, and a shared password would hand that boundary back.
#
# These are only applied on FIRST boot — the init scripts run once, against an
# empty data directory. Changing one here after the volume exists changes what
# the services present, not what PostgreSQL expects, so rotate with
# `make down` (which takes the volume) or an ALTER ROLE.
for var in POSTGRES_PASSWORD CATALOG_DB_PASSWORD INVENTORY_DB_PASSWORD COMMERCE_DB_PASSWORD PAYMENTS_DB_PASSWORD ACCESS_DB_PASSWORD; do
	if needs_generation "$var" 'postgres'; then
		env_set "$var" "$(od -An -tx1 -N24 /dev/urandom | tr -d ' \n')"
	fi
done

if needs_generation JOURNAL_SIGNING_KEY "$RETIRED_JOURNAL_KEY"; then
	env_set JOURNAL_SIGNING_KEY "$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')"
fi

# keypair <private-var> <public-keyring-var> <kid-var> <default-kid> <retired-seed>
keypair() {
	local priv_var="$1" pub_var="$2" kid_var="$3" default_kid="$4" retired="$5"
	local kid seed pub
	if ! needs_generation "$priv_var" "$retired" && [ -n "$(env_value "$pub_var")" ]; then
		return 0
	fi
	kid="$(env_value "$kid_var")"
	[ -n "$kid" ] || kid="$default_kid"
	# `access keygen` prints "<seed> <public key>", both raw-standard base64 —
	# the encoding the loaders and keyrings already read.
	read -r seed pub < <(cd services/access && go run ./cmd/access keygen)
	env_set "$priv_var" "$seed"
	env_set "$pub_var" "$kid=$pub"
}

keypair ACCESS_QR_PRIVATE_KEY ACCESS_QR_PUBLIC_KEYS ACCESS_QR_KID access-qr/local-v1 "$RETIRED_QR_SEED"
keypair ACCESS_LIFECYCLE_PRIVATE_KEY ACCESS_LIFECYCLE_PUBLIC_KEYS ACCESS_LIFECYCLE_KID access-lifecycle/local-v1 "$RETIRED_LIFECYCLE_SEED"

[ ! -f .env ] || chmod 600 .env
