#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
identity="$ROOT/scripts/stack-env.sh"

read -r first_slot first_project < <(bash "$identity" --identity /tmp/ticketing-stack-selftest-repeat smoke)
read -r repeat_slot repeat_project < <(bash "$identity" --identity /tmp/ticketing-stack-selftest-repeat smoke)
[ "$first_slot" = "$repeat_slot" ] && [ "$first_project" = "$repeat_project" ] || {
  echo "stack-env self-test: identical inputs produced different identities" >&2
  exit 1
}

seen_roots=()
seen_projects=()
collision_found=false
for ((i = 0; i <= 40; i++)); do
  checkout="/tmp/ticketing-stack-selftest-$i"
  read -r slot project < <(bash "$identity" --identity "$checkout" smoke)
  [[ $slot =~ ^[0-9]+$ ]] && [ "$slot" -ge 0 ] && [ "$slot" -lt 40 ] || {
    echo "stack-env self-test: invalid port slot: $slot" >&2
    exit 1
  }
  [[ $project =~ ^ticketing-smoke-[0-9a-f]{16}$ ]] || {
    echo "stack-env self-test: invalid Compose project name: $project" >&2
    exit 1
  }

  if [ -n "${seen_roots[$slot]:-}" ]; then
    [ "${seen_projects[$slot]}" != "$project" ] || {
      echo "stack-env self-test: slot collision reused project $project" >&2
      exit 1
    }
    collision_found=true
    break
  fi
  seen_roots[$slot]="$checkout"
  seen_projects[$slot]="$project"
done

$collision_found || {
  echo "stack-env self-test: 41 identities did not produce a slot collision" >&2
  exit 1
}

echo "stack-env self-test: stable identity and collision isolation verified"
