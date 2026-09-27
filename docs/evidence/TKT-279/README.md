# TKT-279 evidence

This file separates checks that ran from checks left to the orchestrator. The red-first output is
at `/tmp/tkt279-wtq_tp1j/red-evidence.txt` on the implementation host.

## Executed

Tests ran with Go's build cache under `/tmp/tkt279-wtq_tp1j/gocache`. The default cache path was
read-only in the sandbox.

Before the production change, these tests ran against unchanged production code:

```sh
(cd shared/go && GOCACHE=/tmp/tkt279-wtq_tp1j/gocache go test -count=1 ./contract -run '^(TestResponseHeaderCardinality|TestRequestHeaderCardinalityUnchanged)$' -v)
(cd services/inventory && GOCACHE=/tmp/tkt279-wtq_tp1j/gocache go test -count=1 ./internal/api -run '^TestInventoryResponseHeaderCardinality$' -v)
```

The primitive duplicate cases returned 200 where the new tests expected 500. The single-value,
array, untyped-with-no-value, noncanonical-key, and request-compatibility controls passed. The final
exact/range/default precedence fixtures were completed after this red run; they passed after the
change. The inventory controls with one `Cache-Control` and one `Age` passed; each availability and
seat-occupancy duplicate case returned 200 instead of 500. Earlier attempts with an invalid
untyped-header fixture and an unwritable default Go cache are also in `red-evidence.txt`; they are
not counted as red-first proof.

After the production change, the old catalog gap pin ran before it was deleted:

```sh
(cd services/catalog && GOCACHE=/tmp/tkt279-wtq_tp1j/gocache go test -count=1 ./internal/api -run '^TestPublicReadCacheTierDuplicateHeaderIsNotCaught$' -v)
```

It failed as expected. The response was 500, while the obsolete test expected 200. The captured
output is in `red-evidence.txt`.

These eight-repeat checks passed after the final test fixtures were in place:

```sh
(cd shared/go && GOCACHE=/tmp/tkt279-wtq_tp1j/gocache go test -count=8 ./contract -run '^(TestResponseHeaderCardinality|TestRequestHeaderCardinalityUnchanged)$')
(cd services/inventory && GOCACHE=/tmp/tkt279-wtq_tp1j/gocache go test -count=8 ./internal/api -run '^TestInventoryResponseHeaderCardinality$')
(cd services/catalog && GOCACHE=/tmp/tkt279-wtq_tp1j/gocache go test -count=8 ./internal/api -run '^TestPublicReadCacheTiersAreContractEnforced$|^TestCatalogCacheTierDuplicateValuesAreRefused$')
```

The full focused suites passed:

```sh
(cd shared/go && GOCACHE=/tmp/tkt279-wtq_tp1j/gocache go test -count=1 ./contract ./cachetier)
(cd services/catalog && GOCACHE=/tmp/tkt279-wtq_tp1j/gocache go test -count=1 ./internal/api)
(cd services/inventory && GOCACHE=/tmp/tkt279-wtq_tp1j/gocache go test -count=1 ./internal/api)
```

The catalog API run included `write_credential_test.go` and its combined-credential assertion.
`make generate` passed. It changed catalog's generated TypeScript API declarations for the
storefront and backoffice clients, and inventory's generated declarations for the backoffice.
It did not change generated Go API files. `git diff --check` passed.

## Not run here

- Mutation probes and mutant restoration checks. The orchestrator will run these independently.
- `make check`. This task explicitly leaves the gate to the orchestrator after it handles generated
  files.

No result is claimed for either item above.
