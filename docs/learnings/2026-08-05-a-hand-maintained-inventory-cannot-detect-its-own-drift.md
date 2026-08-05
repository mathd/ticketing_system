# A hand-maintained inventory cannot detect the drift it exists to catch

**TKT-194, ai-review pass 1.** Filed 2026-08-05.

## What happened

The back office gained a commerce credential so box office could refund. The whole risk of that
change is the credential opening more than the one operation it is for, so the test that mattered
enumerated commerce's other seven internal operations — exchanges, the exchange callback,
cancellation-refund create and report, operational-hold conversion, group draw-down, the buyer
delivery-email read — and required each to refuse it.

It looked thorough. It compared a hand-written table against a hand-written number:

```go
if len(ops) != 7 {
    t.Fatalf("did commerce gain an internal route?")
}
```

Add a ninth internal route — **including one mistakenly guarded by the staff credential** — and
`len(ops)` is still 7, all seven existing probes still return 404, and the security test is still
green while the credential opens something new. The guard against drift was itself the thing that
drifted.

## Why it is easy to write

The count *feels* like a tripwire. It is written at the same moment as the table, when the two
genuinely agree, and it encodes the author's belief about the system rather than the system. Nothing
ever re-derives it. The test's own comment said it existed "to make that failure loud rather than
silent", which is exactly what it did not do.

## The fix

Derive the inventory from the thing that knows: the router.

```go
chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
    if strings.HasPrefix(route, "/internal/") {
        found = append(found, method+" "+route)
    }
    return nil
})
```

and compare it against the fixture table **in both directions** — a route the table misses is
unproven, and a table entry naming a route that no longer exists is a probe hitting 404 for the wrong
reason. That required splitting `registerRoutes` out of `Router` so a test could walk it, which is a
small price.

Verified by adding a ninth route: the test names it.

## The rule

**Anywhere a test asserts "only X may do Y", the set of candidates must be read from the system, not
written down beside it.** A count, a hard-coded list, or a "remember to add it here" comment all fail
the same way, and they fail *silently* — the test stays green, which is worse than deleting it.

Ask of any allow-list test: *if someone adds a new member tomorrow and forgets this file, does this
test fail?* If the answer is no, the test does not cover the property it claims.

## The same shape elsewhere in this repo

- `web/backoffice/test/authorization.test.ts` already does this correctly — it walks `src/pages/**`
  from disk, so a page with no `ROUTE_MATRIX` rule fails the build. That test was written for
  TKT-197 and is the model this one should have followed.
- Fixture tables listing "every event type" or "every status" have the same exposure.

## See also

- [a fixture too small cannot show the negative](2026-08-03-a-fixture-too-small-cannot-show-the-negative.md)
  — the adjacent failure: the inventory is right and the *inputs* cannot express the negative.
- ADR-042 § *TKT-194 amendment* — why this test carries the credential's whole guarantee.
