#!/usr/bin/env bash
# TKT-380 probe runner. Disposable PostgreSQL, temporary module copy.
# Usage: bash docs/evidence/TKT-380/run.sh OUTPUT_DIR
# It changes no file in the repository except the files it writes in OUTPUT_DIR.
set -euo pipefail

if [[ $# -ne 1 || -z "$1" ]]; then
  echo "usage: $0 OUTPUT_DIR" >&2
  exit 2
fi
if [[ -e "$1" ]]; then
  echo "refusing to overwrite existing output: $1" >&2
  exit 2
fi
repo_root=$(git rev-parse --show-toplevel)
script_dir=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$1"
out=$(cd "$1" && pwd)
tmp_root=$(mktemp -d "${TMPDIR:-/tmp}/tkt-380.XXXXXX")
container_name="tkt380-pg-$$"
# The image compose.yaml pins for the stack's PostgreSQL.
image='postgres:18.4-bookworm@sha256:1961f96e6029a02c3812d7cb329a3b03a3ac2bb067058dec17b0f5596aca9296'
db_password='tkt380-disposable-only'

cleanup() {
  docker rm -f "$container_name" >/dev/null 2>&1 || true
  rm -rf "$tmp_root"
}
trap cleanup EXIT INT TERM

command -v docker >/dev/null || { echo "docker is required" >&2; exit 2; }
command -v go >/dev/null || { echo "go is required" >&2; exit 2; }

{
  echo "date_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "repo_head=$(git -C "$repo_root" rev-parse HEAD)"
  echo "go=$(go version)"
  echo "image=$image"
  echo "command=bash docs/evidence/TKT-380/run.sh $1"
} >"$out/environment.txt"

# Temporary module copy. Nothing under services/ in the repository is touched.
mkdir -p "$tmp_root/services" "$tmp_root/shared"
cp -a "$repo_root/services/inventory" "$tmp_root/services/"
cp -a "$repo_root/shared/go" "$tmp_root/shared/"
mod="$tmp_root/services/inventory"
mkdir -p "$mod/cmd/tkt380probe"
cp "$script_dir/probe.go" "$mod/cmd/tkt380probe/main.go"
proto="$mod/internal/store/tkt380_prototype.go"

build() { # $1 = binary name; prototype must already be in place
  (cd "$mod" && GOWORK=off go build -o "$tmp_root/$1" ./cmd/tkt380probe)
}

cp "$script_dir/prototype.go" "$proto"
build probe-pristine

# Mutation "time": judge expiry on transaction-start now() again.
sed '/MUTATION-POINT:time/s/clock_timestamp()/now()/' "$script_dir/prototype.go" >"$proto"
diff -u --label prototype.go --label prototype.go.mutation-time "$script_dir/prototype.go" "$proto" >"$out/mutation-time.diff" || true
build probe-mut-time

# Mutation "lock": remove the code row lock from the finalize path.
sed '/MUTATION-POINT:lock/d' "$script_dir/prototype.go" >"$proto"
diff -u --label prototype.go --label prototype.go.mutation-lock "$script_dir/prototype.go" "$proto" >"$out/mutation-lock.diff" || true
build probe-mut-lock

cp "$script_dir/prototype.go" "$proto"

docker run --detach --rm --name "$container_name" \
  --publish '127.0.0.1::5432' \
  --env POSTGRES_PASSWORD="$db_password" \
  --env POSTGRES_DB=inventory \
  "$image" >/dev/null
host_port=$(docker port "$container_name" 5432/tcp | head -1 | sed 's/.*://')
ready=0
for _ in $(seq 1 60); do
  if docker exec "$container_name" pg_isready -U postgres -d inventory >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done
[[ "$ready" -eq 1 ]] || { echo "disposable PostgreSQL did not become ready" >&2; exit 1; }
sleep 2 # the entrypoint restarts the server once after initdb
export DATABASE_URL="postgres://postgres:${db_password}@127.0.0.1:${host_port}/inventory?sslmode=disable"

run() { # $1 = label, $2 = binary, rest = args
  local label=$1 bin=$2
  shift 2
  set +e
  "$tmp_root/$bin" "$@" >"$out/$label.txt" 2>&1
  local rc=$?
  set -e
  echo "$label: $bin $* -> exit $rc" | tee -a "$out/exit-codes.txt"
}

# Probe 1: today's presale path. Control + forced race, 6 repeats.
run probe1-presale probe-pristine presale 6
# Probe 2: D13 prototype, baseline once, each mutation once, then 7 repeats.
run probe2-baseline probe-pristine prototype 1
run probe2-mutation-time probe-mut-time prototype 1
run probe2-mutation-lock probe-mut-lock prototype 1
run probe2-repeats probe-pristine prototype 7

# Distinct normalized outcomes per file. One line per schedule means every
# repeat produced the identical outcome.
for f in probe1-presale probe2-baseline probe2-mutation-time probe2-mutation-lock probe2-repeats; do
  echo "== $f"
  grep '^SUMMARY' "$out/$f.txt" | sort | uniq -c
done >"$out/summary-distinct.txt"
cat "$out/summary-distinct.txt"
