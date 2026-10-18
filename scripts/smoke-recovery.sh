#!/usr/bin/env bash
# Sourced by scripts/smoke.sh, never executed. The recovery-held PostgreSQL
# that the Access smoke suite needs (TKT-557, ADR-085 §8 ticket 0, D28/D31).
#
# The Access availability ledger admits connect-time SQLSTATE 57P03 ("the
# database system is starting up") as a pre-decision 503. A restart is the
# most common venue outage, and ADR-085 requires a REAL database for the class
# ledger, so the gate needs a real server that rejects every startup with
# 57P03. A wire simulation cannot detect a pgx or PostgreSQL upgrade that
# changes the real form. This file starts one such server: a standalone
# container of the pinned smoke image, held in recovery by standby.signal,
# hot_standby=off and a primary that cannot be reached.
#
# Ownership rules. The container has a unique name and this run's exact label
# (ticketing.smoke.recovery.project=$PROJECT), and its id is captured when it
# is created. Cleanup removes the captured id, or ids with that exact label,
# with `docker rm -fv`, which also removes the anonymous data volume. Nothing
# here filters containers by a name prefix: another checkout's stack must
# never match.
#
# Needs: PROJECT and a `compose` function (both from scripts/smoke.sh).
# Sets: RECOVERY_ID while a container exists, RECOVERY_DSN after a good start.

RECOVERY_LABEL="ticketing.smoke.recovery.project=${PROJECT:?smoke-recovery.sh needs PROJECT}"
RECOVERY_ID=""
RECOVERY_DSN=""

# recovery_remove_owned removes the captured container. Pass the exit status:
# on a failure the container's last log lines are kept in the output first.
recovery_remove_owned() {
  local status=${1:-0}
  if [ -n "$RECOVERY_ID" ]; then
    if [ "$status" -ne 0 ]; then
      echo "--- recovery PostgreSQL logs (exit $status): ---"
      docker logs --tail 40 "$RECOVERY_ID" 2>&1 || true
    fi
    docker rm -fv "$RECOVERY_ID" >/dev/null 2>&1 || true
    RECOVERY_ID=""
    RECOVERY_DSN=""
  fi
}

# recovery_preclean removes containers that a hard-killed earlier run of THIS
# checkout left behind, selected by the exact label only.
recovery_preclean() {
  local ids
  ids=$(docker ps -aq --filter "label=$RECOVERY_LABEL")
  if [ -n "$ids" ]; then
    # shellcheck disable=SC2086 # one id per word
    docker rm -fv $ids >/dev/null
  fi
}

recovery_fail() {
  echo "smoke: recovery-held PostgreSQL: $*" >&2
  return 1
}

# recovery_start creates, configures and restarts the container, then waits
# a bounded time for startup REJECTION (not for healthy readiness). It sets
# RECOVERY_DSN with explicit sslmode=disable. A failed start returns non-zero:
# the caller fails the run. It never skips and never falls back to a
# simulation or to the ordinary admin database.
recovery_start() {
  local image name password i rc port
  image=$(compose config --images postgres) || recovery_fail "cannot resolve the pinned postgres image" || return 1
  [ -n "$image" ] || recovery_fail "compose resolved no postgres image" || return 1
  name="${PROJECT}-recovery-$(od -An -tx1 -N6 /dev/urandom | tr -d ' \n')"
  password=$(od -An -tx1 -N24 /dev/urandom | tr -d ' \n')
  # create, not run: the id is captured before anything can fail after it.
  RECOVERY_ID=$(docker create --name "$name" --label "$RECOVERY_LABEL" \
    -e POSTGRES_PASSWORD="$password" -p 127.0.0.1::5432 -v /var/lib/postgresql "$image") \
    || recovery_fail "docker create failed" || return 1
  docker start "$RECOVERY_ID" >/dev/null || recovery_fail "docker start failed" || return 1

  # First boot: wait for the final server on TCP. The image's init server
  # listens on the socket only, so a TCP answer means initialisation is done.
  for i in $(seq 1 60); do
    if docker exec "$RECOVERY_ID" pg_isready -h 127.0.0.1 -U postgres -q; then break; fi
    [ "$i" -lt 60 ] || recovery_fail "first boot did not become ready in 60 s" || return 1
    sleep 1
  done
  docker exec "$RECOVERY_ID" psql -h 127.0.0.1 -U postgres -v ON_ERROR_STOP=1 -q \
    -c "CREATE ROLE access LOGIN PASSWORD '$password'" \
    -c "CREATE DATABASE access OWNER access" >/dev/null \
    || recovery_fail "cannot create the access role and database" || return 1
  # Hold the server in recovery: a standby with hot_standby off accepts no
  # connection, and its primary is unreachable, so recovery never ends.
  # shellcheck disable=SC2016 # $PGDATA expands inside the container
  docker exec "$RECOVERY_ID" sh -c 'touch "$PGDATA/standby.signal" && printf "%s\n" "hot_standby = off" "primary_conninfo = '"'"'host=127.0.0.1 port=1 user=none connect_timeout=1'"'"'" >> "$PGDATA/postgresql.auto.conf"' \
    || recovery_fail "cannot write the recovery settings" || return 1
  docker restart -t 10 "$RECOVERY_ID" >/dev/null || recovery_fail "docker restart failed" || return 1

  # pg_isready: 0 accepting, 1 rejecting, 2 no response. Wait for 1. A 0 means
  # the recovery marker did not survive the restart.
  for i in $(seq 1 60); do
    rc=0
    docker exec "$RECOVERY_ID" pg_isready -h 127.0.0.1 -U postgres -q || rc=$?
    [ "$rc" -ne 0 ] || recovery_fail "the server accepts connections after the restart: it is not in recovery" || return 1
    [ "$rc" -ne 1 ] || break
    [ "$i" -lt 60 ] || recovery_fail "no startup rejection within 60 s (last pg_isready status $rc)" || return 1
    sleep 1
  done
  # The published port can change on restart, so read it only now.
  port=$(docker port "$RECOVERY_ID" 5432/tcp | head -n1)
  port=${port##*:}
  [ -n "$port" ] || recovery_fail "no published loopback port" || return 1
  RECOVERY_DSN="postgres://access:${password}@127.0.0.1:${port}/access?sslmode=disable"
  echo "smoke: recovery-held PostgreSQL ready (container ${RECOVERY_ID:0:12}, 127.0.0.1:${port}, rejecting startups)"
}
