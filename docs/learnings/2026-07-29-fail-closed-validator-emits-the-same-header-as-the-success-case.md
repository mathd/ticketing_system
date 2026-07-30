# The fail-closed validator emits `Cache-Control: no-store` too — so a header-only assertion is not falsifying

Date: 2026-07-29 · From TKT-107 (PR #126) · Status: technical learning; joins the "coarse observable
passes the broken build" family (TKT-99) with a concrete, currently-live instance

## What happened

TKT-107 made three catalog seat-map reads emit `no-store` instead of the ADR-004 hours tier when the
payload contains a draft. The natural test is one line:

```go
if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
    t.Fatalf("draft geometry must be no-store, got %q", cc)
}
```

The ai-review (gpt-5.6-sol) argued this test cannot fail for the right reason, because ADR-028's
response validator emits **exactly `no-store`** on its own fail-closed path
(`shared/go/contract/http.go`):

```go
w.Header().Set("Content-Type", "application/json")
w.Header().Set("Cache-Control", "no-store")
w.WriteHeader(http.StatusInternalServerError)
_ = json.NewEncoder(w).Encode(map[string]string{"error": "response violates OpenAPI contract"})
```

So if the handler drifts from the contract and the 200 is *withheld and replaced by a 500*, the
header the test reads is still `no-store`. The assertion passes on the failure it exists to catch.

**In catalog this was refuted — and the refutation is the interesting half.** Rather than argue it,
the enum on the response header was temporarily narrowed so that *every* draft-bearing response
drifts, and the tests were re-run. All five cases failed loudly:

```
--- FAIL: TestSeatMapReadCacheTierByStatus/geometry/draft
    production validator masked the response for GET /public/seat-maps/1f8e6698-… (status 500)
    — handler drifted from the spec
```

The guard is `services/catalog/internal/api/server_test.go` (`(*env).validateResponse`), which
sniffs the mask **by body** before validating anything else:

```go
if strings.Contains(rec.Body.String(), "response violates OpenAPI contract") {
    e.t.Fatalf("production validator masked the response for %s %s (status %d) — handler drifted from the spec", ...)
}
```

## The trap, and where it is still live

The reviewer's hypothesis was **correct in general and wrong about catalog** — catalog is the one
service whose unit-test harness closes it. The others do not:

| Service | Unit tests run through the response validator | Harness sniffs the mask |
|---|---|---|
| catalog | yes (`NewRouter(..., true)`) | **yes** — `server_test.go` (`validateResponse`), `coverage_test.go` |
| inventory | yes (`s.Router(nil, true)` → `contract.RequestValidator(..., true)`) | **no** |
| commerce, payments, access | yes (validator wired in `internal/api/server.go`) | **no** |

Inventory is the one that matters today: its public availability read emits `public, max-age=5,
s-maxage=5` (ADR-004's seconds tier). A header-only assertion on any *`no-store`* response in
`services/inventory/internal/api` would be satisfied by a masked 500 — silently, with no diagnostic.

## The rule

**When a fail-closed wrap emits the same observable as the success case, that observable cannot
falsify on its own.** Assert the status code (and, for a header driven by payload content, one field
of the payload) alongside it — or add the mask sniff to that package's harness. Two lines either way.

This is TKT-99's "a coarse observable can be produced by the broken version too" with a specific
mechanism: it is not that the observable is coarse, it is that the *error path deliberately sets it*.
Any `no-store` default on an error response creates this collision for every `no-store` expectation
in the same service — and `writeJSON` in catalog defaults an unset `Cache-Control` to `no-store`, so
the collision is the norm, not an edge case.

And, as in TKT-99: this was settled by **running** the mutation, not by reading the code. Narrowing
the contract enum took one minute and turned a plausible finding into a decided one — refuted for
catalog, confirmed for four other services, which is more than either reading would have produced.

## References

- `services/catalog/internal/api/server_test.go` — `(*env).validateResponse`, the mask sniff
- `shared/go/contract/http.go` — `responseValidated`, the fail-closed path that sets `no-store`
- `services/catalog/internal/api/server.go` — `writeJSON`, the `no-store` default on error responses
- `services/inventory/internal/api/server.go` — `Router`, validator wired with no harness guard
- [ADR-028](../adr/ADR-028-response-drift-fail-closed.md) · [ADR-004](../adr/ADR-004-cache-first-read-path.md)
- [TKT-99's coarse-observable learning](./2026-07-25-coarse-observables-pass-the-broken-build.md)
