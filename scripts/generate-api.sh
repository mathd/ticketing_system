#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# kind|source|output|config
entries=(
  "go-config|services/catalog/api/openapi.yaml|services/catalog/internal/api/openapi_gen.go|services/catalog/api/codegen.yaml"
  "go-model|services/inventory/api/openapi.yaml|services/inventory/internal/api/openapi_gen.go|"
  "go-model|services/commerce/api/openapi.yaml|services/commerce/internal/api/openapi_gen.go|"
  "go-model|services/payments/api/openapi.yaml|services/payments/internal/api/openapi_gen.go|"
  "go-model|services/access/api/openapi.yaml|services/access/internal/api/openapi_gen.go|"
  "typescript|services/catalog/api/openapi.yaml|web/storefront/src/lib/api-types.gen.ts|"
  "typescript|services/catalog/api/openapi.yaml|web/backoffice/src/lib/api-types.gen.ts|"
  "typescript|services/commerce/api/openapi.yaml|web/storefront/src/lib/commerce-api-types.gen.ts|"
  "typescript|services/inventory/api/openapi.yaml|web/backoffice/src/lib/inventory-api-types.gen.ts|"
  "typescript|services/access/api/openapi.yaml|web/storefront/src/lib/access-api-types.gen.ts|"
  "typescript|services/access/api/openapi.yaml|web/backoffice/src/lib/access-api-types.gen.ts|"
  "typescript|services/access/api/openapi.yaml|web/scanner/src/access-api-types.gen.ts|"
)

validate_entries() {
  local entry kind source output config seen_outputs='|'
  for entry in "${entries[@]}"; do
    IFS='|' read -r kind source output config <<<"$entry"
    [[ -n "$source" && -n "$output" ]] || {
      printf 'invalid API generator entry: %s\n' "$entry" >&2
      return 2
    }
    case "$kind" in
      go-config)
        [[ -n "$config" ]] || {
          printf 'go-config entry has no config: %s\n' "$entry" >&2
          return 2
        }
        ;;
      go-model|typescript)
        [[ -z "$config" ]] || {
          printf '%s entry has an unexpected config: %s\n' "$kind" "$entry" >&2
          return 2
        }
        ;;
      *)
        printf 'unknown API generator kind: %s\n' "$kind" >&2
        return 2
        ;;
    esac
    [[ "$seen_outputs" != *"|$output|"* ]] || {
      printf 'duplicate generated API output: %s\n' "$output" >&2
      return 2
    }
    seen_outputs+="$output|"
  done
}

outputs() {
  local entry kind source output config
  for entry in "${entries[@]}"; do
    IFS='|' read -r kind source output config <<<"$entry"
    printf '%s\n' "$output"
  done
}

generate() {
  local entry kind source output config
  cd "$repo_root"
  for entry in "${entries[@]}"; do
    IFS='|' read -r kind source output config <<<"$entry"
    case "$kind" in
      go-config)
        (
          cd "$(dirname "$source")"
          go tool oapi-codegen -config "$(basename "$config")" \
            -o "$repo_root/$output" "$(basename "$source")"
        )
        ;;
      go-model)
        (
          cd services/catalog
          go tool oapi-codegen -package api -generate models \
            -o "$repo_root/$output" "$repo_root/$source"
        )
        ;;
      typescript)
        pnpm exec openapi-typescript "$source" -o "$output"
        ;;
      *)
        printf 'unknown API generator kind: %s\n' "$kind" >&2
        return 2
        ;;
    esac
  done
}

verify_tracked() {
  local entry kind source output config tracked failed=0 declared_outputs='|'
  cd "$repo_root"
  for entry in "${entries[@]}"; do
    IFS='|' read -r kind source output config <<<"$entry"
    declared_outputs+="$output|"
    if ! git ls-files --error-unmatch -- "$output" >/dev/null 2>&1; then
      printf 'generated API output is not tracked: %s\n' "$output" >&2
      failed=1
    fi
  done

  while IFS= read -r -d '' tracked; do
    case "$tracked" in
      services/*/internal/api/openapi_gen.go|web/**/*api-types.gen.ts)
        if [[ "$declared_outputs" != *"|$tracked|"* ]]; then
          printf 'tracked generated API output is missing from the registry: %s\n' "$tracked" >&2
          failed=1
        fi
        ;;
    esac
  done < <(git ls-files -z)
  return "$failed"
}

case "${1:-generate}" in
  generate)
    validate_entries
    generate
    ;;
  outputs)
    validate_entries
    outputs
    ;;
  verify-tracked)
    validate_entries
    verify_tracked
    ;;
  *)
    printf 'usage: %s [generate|outputs|verify-tracked]\n' "${0##*/}" >&2
    exit 2
    ;;
esac
