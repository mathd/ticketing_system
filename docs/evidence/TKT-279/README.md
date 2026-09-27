# TKT-279 evidence

This file separates checks that ran from checks left to the orchestrator. The red-first output is
in [red-first.txt](red-first.txt).

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

## Independent verification

The orchestrator committed the implementation as `fd9b31ec`, then ran 13 separate mutation probes.
Each failed at a test assertion, without a build failure. Removing the cardinality call failed
independently in shared, inventory and catalog tests. Other probes changed type selection, the
allowed count, equal-value counting, array handling, response matching, case matching, logging and
body forwarding. [mutation-results.txt](mutation-results.txt) records commands and observed failures.
Every probe restored the source bytes. `git diff` was empty after restoration.

The orchestrator then ran all new shared, inventory and catalog tests with `go test -count=8`.
All three packages passed. Comparing parsed OpenAPI documents with descriptions removed confirmed
that both contract edits changed descriptions only.

The full `make check` gate is still pending. No gate result is claimed here.
