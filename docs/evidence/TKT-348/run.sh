#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 || -z "$1" ]]; then
  echo "usage: $0 OUTPUT.json" >&2
  exit 2
fi

repo_root=$(git rev-parse --show-toplevel)
script_dir=$(cd "$(dirname "$0")" && pwd)
output_path=$(mkdir -p "$(dirname "$1")" && cd "$(dirname "$1")" && printf '%s/%s' "$PWD" "$(basename "$1")")
if [[ -e "$output_path" ]]; then
  echo "refusing to overwrite existing output: $output_path" >&2
  exit 2
fi
tmp_root=$(mktemp -d "${TMPDIR:-/tmp}/tkt-348.XXXXXX")
container_name="tkt-348-$(cat /proc/sys/kernel/random/uuid | tr -d '-')"
image='postgres:18.4-bookworm@sha256:1961f96e6029a02c3812d7cb329a3b03a3ac2bb067058dec17b0f5596aca9296'
db_password='tkt348-disposable-only'

cleanup() {
  docker rm -f "$container_name" >/dev/null 2>&1 || true
  rm -rf "$tmp_root"
}
trap cleanup EXIT INT TERM

command -v docker >/dev/null || { echo "docker is required" >&2; exit 2; }
command -v go >/dev/null || { echo "go is required" >&2; exit 2; }

mkdir -p "$tmp_root/services" "$tmp_root/shared"
cp -a "$repo_root/services/inventory" "$tmp_root/services/"
cp -a "$repo_root/shared/go" "$tmp_root/shared/"
mkdir -p "$tmp_root/services/inventory/cmd/tkt348probe"
cp "$script_dir/probe.go" "$tmp_root/services/inventory/cmd/tkt348probe/main.go"

docker run --detach --rm --name "$container_name" \
  --publish '127.0.0.1::5432' \
  --env POSTGRES_PASSWORD="$db_password" \
  --env POSTGRES_DB=inventory \
  "$image" >/dev/null

host_port=$(docker port "$container_name" 5432/tcp | sed 's/.*://')
ready=0
for attempt in $(seq 1 60); do
  if docker exec "$container_name" pg_isready -U postgres -d inventory >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done
if [[ "$ready" -ne 1 ]]; then
  echo "disposable PostgreSQL did not become ready" >&2
  exit 1
fi

cd "$tmp_root/services/inventory"
runner_command="bash docs/evidence/TKT-348/run.sh $(printf '%q' "$output_path")"
TKT348_RUNNER_COMMAND="$runner_command" \
DATABASE_URL="postgres://postgres:${db_password}@127.0.0.1:${host_port}/inventory?sslmode=disable" \
  go run ./cmd/tkt348probe "$output_path"
printf 'probe output: %s\n' "$output_path"
