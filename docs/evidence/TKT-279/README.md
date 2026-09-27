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
untyped-header fixture and an unwritable default Go cache are also in [red-first.txt](red-first.txt); they are
not counted as red-first proof.

After the production change, the old catalog gap pin ran before it was deleted:

```sh
(cd services/catalog && GOCACHE=/tmp/tkt279-wtq_tp1j/gocache go test -count=1 ./internal/api -run '^TestPublicReadCacheTierDuplicateHeaderIsNotCaught$' -v)
```

It failed as expected. The response was 500, while the obsolete test expected 200. The captured
output is in [red-first.txt](red-first.txt).

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

The first full gate failed on Staticcheck QF1001 at `6493d21b`. After the equivalent predicate rewrite,
`make check` passed at `6e42f3c7db5646a30e73968008bb9b7534a46d31`. The verdict recorded tree digest
`1e41526d128c4c1767fcbd259068c99c97f6c1c4`, which the orchestrator independently matched to the clean
worktree. This result includes lint, tests, builds, database and gateway smoke tests, and integrity
verification. Later commits require their own gate run.

PR #394's repository security scan failed on the five dependency advisories tracked by TKT-329.
That CI failure prevents merge despite the local gate pass.


## Decision-audit fixture corrections

The informed review found that an untyped control with no values could not prove the exclusion,
and that explicit primitive types with composition had no direct test. Commit `f3e1b8c9` added
both cases. The untyped case emits two values on a declared 304 response, where ordinary kin
validation is skipped. The composed string case emits two individually valid values and expects
the cardinality refusal.

The orchestrator reran all 13 probes and added two more: apply the check to untyped headers, and
exempt headers with `allOf`. All 15 failed at assertions, and the two new probes failed specifically
in the new cases. [mutation-details.txt](mutation-details.txt) records each exact patch, base commit,
command, output and exit status. Every mutation was restored. Eight unmutated repeat runs then
passed in all three packages; [repeat-results.txt](repeat-results.txt) contains the captured result.
A fresh full gate remains required before the next push.
