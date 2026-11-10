# API and event contracts

The source of each declared HTTP contract is `services/<service>/api/openapi.yaml`.
[ADR-009](adr/ADR-009-contract-first-apis.md) governs contract-first changes and conformance.
The executable generation registry is [generate-api.sh](../scripts/generate-api.sh).

## Generated outputs

Paths below are relative to the repo root. There are five Go outputs and seven TypeScript outputs.

| Source service | Go output | TypeScript outputs and consumers |
|---|---|---|
| Catalog | `services/catalog/internal/api/openapi_gen.go` | `web/storefront/src/lib/api-types.gen.ts`, `web/backoffice/src/lib/api-types.gen.ts` |
| Inventory | `services/inventory/internal/api/openapi_gen.go` | `web/backoffice/src/lib/inventory-api-types.gen.ts` |
| Commerce | `services/commerce/internal/api/openapi_gen.go` | `web/storefront/src/lib/commerce-api-types.gen.ts` |
| Payments | `services/payments/internal/api/openapi_gen.go` | None |
| Access | `services/access/internal/api/openapi_gen.go` | `web/storefront/src/lib/access-api-types.gen.ts`, `web/backoffice/src/lib/access-api-types.gen.ts`, `web/scanner/src/access-api-types.gen.ts` |

Catalog uses `oapi-codegen` with `services/catalog/api/codegen.yaml`. The other Go outputs are
models generated with that pinned tool. TypeScript uses the root `openapi-typescript` tool.
Do not edit generated files directly.

```bash
make generate
./scripts/generate-api.sh outputs
```

Inspect the spec and generated diff. Commit the generated files by explicit path when authorized,
then run `make check`. `check-generate` regenerates, verifies registry/tracked-output agreement,
and compares all outputs with HEAD. Uncommitted generation fails as drift.

The generated models do not enumerate every hand-mounted internal route. Read the service router
and [ADR-043](adr/ADR-043-where-a-service-auth-guard-lives.md) before changing security. Request
validation, runtime response validation, and conformance tests have different responsibilities;
[ADR-028](adr/ADR-028-response-drift-fail-closed.md) defines response-drift behavior.

## Domain events

The shared envelope is in `shared/go/domainevent/`. [ADR-017](adr/ADR-017-domain-event-schema-evolution.md)
defines schema evolution; [ADR-033](adr/ADR-033-shared-domain-event-envelope.md) defines the shared
type, and [ADR-034](adr/ADR-034-shared-durable-consumer-lifecycle.md) defines consumer lifecycle.
Read the schema before decoding `data`. Bump for changed consumer semantics, even if the old JSON
parser would accept it. Broker subject permissions are in [ADR-072](adr/ADR-072-nats-publisher-acls.md).
Use the [domain-event runbook](runbooks/domain-events.md) for failure and quarantine recovery.
