#!/usr/bin/env bash
# TKT-380 probe runner. Disposable PostgreSQL, temporary module copy.
# Usage: bash docs/evidence/TKT-380/run.sh OUTPUT_DIR
# It changes no file in the repository except the files it writes in OUTPUT_DIR.
# It exits 0 only when every probe run has its expected exit code AND every
# schedule has its expected verdict in every repeat (see expect below).
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
command -v python3 >/dev/null || { echo "python3 is required" >&2; exit 2; }

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
presale_src="$mod/internal/store/presale_codes.go"
cp "$presale_src" "$tmp_root/presale_codes.go.orig"

build() { # $1 = binary name
  (cd "$mod" && GOWORK=off go build -o "$tmp_root/$1" ./cmd/tkt380probe)
}

# Mutation records are zero-context diffs (-U0): context lines would start
# with a space before a tab, which `git diff --check` reports.
mutate() { # $1 = name, $2 = sed program
  sed "$2" "$script_dir/prototype.go" >"$proto"
  diff -U0 --label prototype.go --label "prototype.go.mutation-$1" "$script_dir/prototype.go" "$proto" >"$out/mutation-$1.diff" || true
  build "probe-mut-$1"
}

cp "$script_dir/prototype.go" "$proto"
build probe-pristine
mutate time '/MUTATION-POINT:time/s/clock_timestamp()/now()/'
mutate lock '/MUTATION-POINT:lock/d'
mutate uncapped '/MUTATION-POINT:uncapped/s/skipUncapped := false/skipUncapped := true/'
cp "$script_dir/prototype.go" "$proto"

# Window-rule prototype: the code window read in a separate statement after the
# code lock. Applied to the temporary copy only.
python3 -I "$script_dir/window-fix.py" "$presale_src" >/dev/null
diff -U0 --label presale_codes.go --label presale_codes.go.window-fix "$tmp_root/presale_codes.go.orig" "$presale_src" >"$out/window-fix.diff" || true
build probe-window-fix
cp "$tmp_root/presale_codes.go.orig" "$presale_src"

# Reading R2 prototype (owner decision, 2026-10-13): the code's usage count
# keeps every held claim until a terminal write. Applied to the temporary copy only.
alloc_src="$mod/internal/store/channel_allocations.go"
cp "$alloc_src" "$tmp_root/channel_allocations.go.orig"
python3 -I "$script_dir/r2-predicate.py" "$alloc_src" >/dev/null
diff -U0 --label channel_allocations.go --label channel_allocations.go.r2 "$tmp_root/channel_allocations.go.orig" "$alloc_src" >"$out/r2-predicate.diff" || true
build probe-r2
cp "$tmp_root/channel_allocations.go.orig" "$alloc_src"

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

failures=0
fail() {
  echo "EXPECTATION FAILED: $*" | tee -a "$out/expectations.txt"
  failures=$((failures + 1))
}

run() { # $1 = label, $2 = expected exit, $3 = binary, rest = args
  local label=$1 want=$2 bin=$3
  shift 3
  set +e
  "$tmp_root/$bin" "$@" >"$out/$label.txt" 2>&1
  local rc=$?
  set -e
  echo "$label: $bin $* -> exit $rc (expected $want)" | tee -a "$out/exit-codes.txt"
  [[ "$rc" -eq "$want" ]] || fail "$label exited $rc, expected $want"
}

expect() { # $1 = label, $2 = schedule, $3 = verdict, $4 = runs
  local file="$out/$1.txt" total match
  total=$(grep -c "^SUMMARY $2 " "$file" || true)
  match=$(grep -c "^SUMMARY $2 .*verdict=$3\$" "$file" || true)
  if [[ "$total" -eq "$4" && "$match" -eq "$4" ]]; then
    echo "ok   $1: $2 verdict=$3 in $match of $4 runs" >>"$out/expectations.txt"
  else
    fail "$1: $2 expected verdict=$3 in $4 of $4 runs, saw $match of $total"
  fi
}

# Probe 1: today's code. Exit 1 = a defect was observed, as documented.
run probe1-presale 1 probe-pristine presale 6
expect probe1-presale presale-control HELD 6
expect probe1-presale presale-S1 BROKEN 6
expect probe1-presale window-control HELD 6
expect probe1-presale W BROKEN 6

# Probe 1b: the window rule prototype.
run probe1-window-fix 0 probe-window-fix window 6
expect probe1-window-fix window-control HELD 6
expect probe1-window-fix W HELD 6

# Probe 2: the D13 prototype. Baseline, each mutation, then 7 repeats.
run probe2-baseline 0 probe-pristine prototype 1
for s in prototype-S1 prototype-S2 prototype-S3; do expect probe2-baseline "$s" HELD 1; done

run probe2-mutation-time 1 probe-mut-time prototype 1
expect probe2-mutation-time prototype-S1 BROKEN 1
expect probe2-mutation-time prototype-S2 HELD 1
expect probe2-mutation-time prototype-S3 HELD 1

run probe2-mutation-lock 1 probe-mut-lock prototype 1
expect probe2-mutation-lock prototype-S1 HELD 1
expect probe2-mutation-lock prototype-S2 BROKEN 1
expect probe2-mutation-lock prototype-S3 BROKEN 1

run probe2-mutation-uncapped 1 probe-mut-uncapped prototype 1
expect probe2-mutation-uncapped prototype-S1 HELD 1
expect probe2-mutation-uncapped prototype-S2 HELD 1
expect probe2-mutation-uncapped prototype-S3 BROKEN 1

run probe2-repeats 0 probe-pristine prototype 7
for s in prototype-S1 prototype-S2 prototype-S3; do expect probe2-repeats "$s" HELD 7; done

# Probe 3: reading R2. The R2 build, then the same schedules on today's code
# (the mutation: the R2 predicate removed), then 7 repeats.
run probe3-r2 0 probe-r2 r2 6
expect probe3-r2 presale-control HELD 6
expect probe3-r2 r2-S1 HELD 6
expect probe3-r2 r2-liveness HELD 6

run probe3-r2-mutation 1 probe-pristine r2 1
expect probe3-r2-mutation presale-control HELD 1
expect probe3-r2-mutation r2-S1 BROKEN 1
expect probe3-r2-mutation r2-liveness BROKEN 1

run probe3-r2-repeats 0 probe-r2 r2 7
for s in presale-control r2-S1 r2-liveness; do expect probe3-r2-repeats "$s" HELD 7; done

# Distinct outcome lines per file. One line per schedule means every repeat
# produced the identical outcome.
for f in probe1-presale probe1-window-fix probe2-baseline probe2-mutation-time probe2-mutation-lock probe2-mutation-uncapped probe2-repeats probe3-r2 probe3-r2-mutation probe3-r2-repeats; do
  echo "== $f"
  grep '^SUMMARY' "$out/$f.txt" | sort | uniq -c
done >"$out/summary-distinct.txt"
cat "$out/summary-distinct.txt"
cat "$out/expectations.txt"

if [[ "$failures" -ne 0 ]]; then
  echo "run.sh: $failures expectation(s) failed" >&2
  exit 1
fi
echo "run.sh: every expectation met"
