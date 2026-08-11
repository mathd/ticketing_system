#!/usr/bin/env bash
# One database + one role per service (ADR-007). CONNECT is revoked from PUBLIC
# on every service database so a service's credentials physically cannot reach
# another service's data — asserted by the smoke suite.
#
# A shell script rather than the plain .sql this was until ai-review S11, for one
# reason: the roles used to be created with a password equal to the ROLE NAME,
# committed to the repository. Anyone who could reach the published PostgreSQL
# port — which is every process on the developer's machine — had every service's
# database by typing the obvious thing. Passwords now arrive through the
# environment, generated per clone by scripts/env-bootstrap.sh and per run by
# scripts/stack-env.sh, and the postgres entrypoint runs *.sh files with the
# environment available while it runs *.sql through psql without it.
#
# The superuser password is the entrypoint's own POSTGRES_PASSWORD and is
# generated the same way; it is no longer the literal "postgres".
set -euo pipefail

for role in CATALOG INVENTORY COMMERCE PAYMENTS ACCESS; do
	var="${role}_DB_PASSWORD"
	if [ -z "${!var:-}" ]; then
		echo "postgres-init: $var is required — no default ships (run 'make up' once)" >&2
		exit 1
	fi
done

# Passwords are passed as psql VARIABLES and quoted with :'name', so the value is
# escaped by psql rather than pasted into the statement. The generators produce
# hex, so nothing here needs escaping today — this is what keeps that true if a
# deployment ever injects a password with a quote in it.
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres \
	-v catalog_password="$CATALOG_DB_PASSWORD" \
	-v inventory_password="$INVENTORY_DB_PASSWORD" \
	-v commerce_password="$COMMERCE_DB_PASSWORD" \
	-v payments_password="$PAYMENTS_DB_PASSWORD" \
	-v access_password="$ACCESS_DB_PASSWORD" <<-'EOSQL'
	CREATE ROLE catalog   LOGIN PASSWORD :'catalog_password';
	CREATE ROLE inventory LOGIN PASSWORD :'inventory_password';
	CREATE ROLE commerce  LOGIN PASSWORD :'commerce_password';
	CREATE ROLE payments  LOGIN PASSWORD :'payments_password';
	CREATE ROLE access    LOGIN PASSWORD :'access_password';

	CREATE DATABASE catalog   OWNER catalog;
	CREATE DATABASE inventory OWNER inventory;
	CREATE DATABASE commerce  OWNER commerce;
	CREATE DATABASE payments  OWNER payments;
	CREATE DATABASE access    OWNER access;

	REVOKE CONNECT ON DATABASE catalog   FROM PUBLIC;
	REVOKE CONNECT ON DATABASE inventory FROM PUBLIC;
	REVOKE CONNECT ON DATABASE commerce  FROM PUBLIC;
	REVOKE CONNECT ON DATABASE payments  FROM PUBLIC;
	REVOKE CONNECT ON DATABASE access    FROM PUBLIC;

	GRANT CONNECT ON DATABASE catalog   TO catalog;
	GRANT CONNECT ON DATABASE inventory TO inventory;
	GRANT CONNECT ON DATABASE commerce  TO commerce;
	GRANT CONNECT ON DATABASE payments  TO payments;
	GRANT CONNECT ON DATABASE access    TO access;
EOSQL
