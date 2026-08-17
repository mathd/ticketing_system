# A harness that cannot catch what it hunts, and a guard that tests the mechanism instead of the wiring

**TKT-202**, 2026-08-17. Nine findings across four adversarial review passes on one ~1600-line diff.
Three of the nine were the same defect class, and two were defects in the *previous pass's fixes*.
Both shapes below already have cousins in `AGENTS.md`; what this ticket adds is that each one bites
one level up from where the existing rule looks.

## 1. The fixture-that-cannot-fail rule applies to your attack harness too

`AGENTS.md` already says: *before trusting a green test, ask whether its fixture can reach the state
that would fail.* TKT-202 obeyed that for its tests and then broke it for its **brute-force sweep**.

The sanitiser had to keep a redeemable order reference out of logs. A review pass found that
`/api/access/orders/<ref-1>/../<ref-2>/tickets` redacted `<ref-2>` — the segment the route resolves
on — and left `<ref-1>` in the line verbatim. Both are live, unauthenticated, redeemable credentials.

The defect had been live through a sweep written specifically to attack that code: **576 arrangements
of `.`, `..`, empty and junk segments around a capability route, all green.** The sweep put a harmless
`"x"` in the junk position and asserted only that *the surviving reference* was gone. It could not
observe a leak in the popped position because it never put anything worth leaking there.

Re-run with live references in every junk slot, the same sweep found the defect immediately — and
then found **twelve more spellings** that the first fix still missed, because that fix only redacted
segments popped from a *capability* position and these were popped from *literal* ones
(`/api/<ref>/../access/orders/<ref-2>/tickets`).

**The rule.** A harness that generates adversarial inputs needs the same question asked of it as a
test: *if the defect I am hunting were present, would this input actually exhibit it?* For a
"value must not appear in output" property, that means **every position the generator can fill must
be filled with the value that must not appear** — a placeholder in any slot is a slot the sweep is
blind to. And state the invariant without naming the implementation: *no value ever offered at a
capability position may appear in the output* is checkable; *the capability segment is replaced* is a
description of the mechanism and is satisfied by the broken code.

Corrected sweep: 2050 arrangements, three route families, zero leaks.

## 2. A guard can test the mechanism and never test the wiring

The same ticket had to stop an OpenTelemetry span attribute carrying the credential to a collector.
The fix was a span processor; the test built a tracer provider, installed the processor, drove a
request, and asserted the exported span was clean. It passed, and it was worthless in one specific
way: **deleting the processor from `setup.go` — the production install — left the whole suite green.**
The test proved the processor worked. Nothing proved production used it.

The first repair extracted `newTracerProvider`, called from both `Setup` and the test. That is the
obvious move and it is **still bypassable**: replacing the call *inside `Setup`* with a plain SDK
provider compiles, leaks in production, and leaves the test green, because the test exercises the
helper rather than `Setup`. It closed "the processor is broken" while leaving "Setup doesn't call it"
— which was the original finding.

What finally held: drive the **real `Setup`** against a local HTTP server standing in for the
collector, and grep the OTLP payload for the reference. There is no construction path left to route
around, because the assertion is on bytes that actually left the process.

**The rule.** When a mechanism must be *installed* somewhere to matter, ask which edit the test
catches: breaking the mechanism, or **removing it from the place that uses it**. Only the second is
the guarantee. Extracting a shared helper is not sufficient — it creates one more layer that can be
bypassed at the call site. Test at the boundary the value crosses on its way out (the wire, the file,
the database row), not at the component that is supposed to be in the path.

## 3. Corollary: sanitising a credential follows the shape of the secret, not route identity

A related trap the same ticket hit. A redaction keyed on *"is this a real route?"* inherits the
router's case-sensitivity and normalisation, and therefore **leaks on every spelling the router
rejects**. The request logger runs *before* the router, so a `404` or a `307` is logged with the
credential intact — and a refused request does not make the credential any less redeemable.

Sanitise on a canonical, case-folded, dot-reduced form even where that is broader than routing. The
two errors are not symmetric: over-redacting a route-shaped 404 costs one segment of a request that
did nothing; under-redacting writes a live credential to a log. See ADR-012 § *TKT-202 amendment*.

## 4. A mutation that breaks the mechanism *syntactically* proves nothing

**TKT-199**, same day. The claim under test was "the cross-tenant guard on catalog's publish is live
and its test is not vacuous". The guard is a SQL predicate, so the mutation was to delete it:

```sql
-- before
WHERE id = $1 AND organizer_id = $2 AND status = 'draft'
-- mutation attempt 1
WHERE id = $1 AND $2 IS NOT NULL AND status = 'draft'
```

The gate went red, the cross-tenant test was among the failures, and it looked like strong evidence.
It was not. `$2` now had no inferable type, so Postgres refused the statement (`SQLSTATE 42P18`) and
**eight** tests failed — every test that touches the publish path, for a reason that has nothing to do
with tenancy. A mutation that makes the query *invalid* tells you the query is reachable, not that the
predicate is load-bearing.

The valid mutation keeps the statement well-formed and the parameter bound, and changes only the
predicate's truth:

```sql
WHERE id = $1 AND (organizer_id = $2 OR $2::uuid IS NOT NULL) AND status = 'draft'
```

Now **one** test fails, and it fails with the sentence that names the actual defect:
`cross-tenant publish = <nil>, want ErrNotFound` — the attacker's write succeeded.

**The rule.** After a mutation goes red, read *which* tests failed and *what they said*. A mutation is
evidence only if the failure is **semantic** (the mechanism stopped doing its job) rather than
**structural** (the code stopped compiling, the query stopped parsing, the fixture stopped loading).
The tell is breadth: a whole cluster of unrelated tests failing together usually means you broke the
scaffolding, not the guard. This is the same mistake as a fixture that cannot reach the failing state,
inverted — there the test could not observe the defect; here the test observed something else entirely
and got credit for it.

## See also

- [a green test that cannot reach the failing state](2026-08-10-a-green-test-that-cannot-reach-the-failing-state.md) — the rule this extends
- [a green test can bless the defect](2026-08-10-a-green-test-can-bless-the-defect.md) — why the invariant must not name the mechanism
- [a fixture that seeds two mechanisms](2026-08-16-a-fixture-that-seeds-two-mechanisms.md) — ask of each seed what would notice its absence
- [ADR-012](../adr/ADR-012-ticket-issuance-and-qr-credentials.md) § *TKT-202 amendment* — the sinks audited and the adversary bound
