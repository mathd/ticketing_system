# Check *why* a test is red, not just that it is

**TKT-116, PR #129.** Red-first discipline was followed and still produced two tests that could
never have failed for the reason they claimed. The red came from missing test scaffolding, not from
missing behaviour.

## What happened

Two new tests were written against a `httptest` stub that maps `"METHOD /path"` to a canned
response and answers **500** for any route it does not know:

```go
w.WriteHeader(500)
_, _ = w.Write([]byte(`{"error":{"message":"stub: no route for ` + key + `",...}}`))
```

`TestStripeRefundResolutionFailsClosedOnAFailedRefund` registered only `GET /v1/refunds` and
asserted `err != nil`. Before the feature existed, `Refund` went straight to `POST /v1/refunds`,
hit the missing-route 500, and returned an error — **the test passed**. After the feature, it would
list first and also return an error. Green both ways, for two unrelated reasons.

`TestStripeRefundResolutionIgnoresForeignRefunds` was worse: it registered both routes and asserted
the returned `ProviderRef`, which the direct POST already produced. It passed on the old code
outright.

Neither test would have failed if the resolution logic had been deleted. They were run in a batch
where five sibling tests went red, and the two green ones were nearly waved through as "already
covered".

## The rule

**A test is only red-first if it is red for the reason it exists.** After observing red, read the
failure message and confirm it names the missing behaviour. `FAIL` is not evidence; the *sentence*
is.

The specific fix here: when the behaviour under test is **an interaction** — "resolve before
submitting", "do not call X", "call A then B" — assert the **request sequence**, not the returned
value. Outcomes collide; sequences do not.

```go
if len(stub.requests) != 1 || stub.requests[0].method != http.MethodGet {
	t.Fatalf("want exactly one list call and no submit, got %+v", stub.requests)
}
```

With that, both tests went red for the right reason.

## The corollary: a permissive stub is a hazard

A stub that returns an error for unknown routes is convenient and quietly makes negative assertions
untrustworthy, because *"the code did the wrong thing"* and *"the code did the right thing against
an unconfigured stub"* produce the same observable. Prefer `t.Fatalf` on an unexpected route, or
assert the recorded interactions rather than the outcome.

## And when the fix and its test land together

Later in the same ticket a fix was written *with* its test in one step, so the test was never
observed red at all — the review-fix path skips the red phase by construction, which is exactly
where it is most tempting to say "obviously it would fail".

The cheap honest answer is to run the mutation: neuter the fix, re-run, confirm the test dies and
the *right* one dies.

```
psp_recovery_test.go:456: replay against a PARKED release_pending = 202 {...}
    , want 409 — 202 would promise progress no worker can make
--- FAIL: TestCheckoutReplayAtParkedReleasePendingIsNotPending
```

One test, the intended symptom. That costs one gate cycle and converts "obviously" into evidence.
See also [force the interleaving](./2026-07-29-force-the-interleaving-repetition-cannot-falsify-a-race-fix.md)
and [coarse observables can pass the broken build](../LEARNINGS.md) — the same family: the assertion
must be something only the correct path can satisfy.
