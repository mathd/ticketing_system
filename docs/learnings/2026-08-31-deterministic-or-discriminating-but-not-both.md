# Deterministic or discriminating, but not both

**2026-08-31 — TKT-308**

`AGENTS.md` already carries two rules about tests: mutate the mechanism and watch the test go red, and
ask whether the fixture can reach the failing state. Both are about a test's *power*.

This note is about their interaction, which cost three rewrites of one test:

**Determinism and discrimination are separate properties, and fixing one routinely destroys the
other — most easily right after you have fixed it, when attention is on the thing just repaired.**

## The sequence

The test had to prove that a client with its own timeout shares the tuned connection pool rather than
falling back to `http.DefaultTransport`. Warm a pool with one client, then use another, then assert
no new connections were opened.

| version | deterministic? | catches the mutation? |
|---|---|---|
| sequential warm | **yes** | no |
| concurrent warm | no | **yes** |
| barrier on warm, sequential second burst | **yes** | no |
| barrier on both bursts | **yes** | **yes** |

**v1 — sequential warm.** Opens exactly one connection. `DefaultTransport` holds two idle per host, so
it can hold that one too; the second client reused it and the test passed whether or not the pool was
shared. Stable across every run, and worthless.

**v2 — concurrent warm.** Now more connections exist than an untuned pool can hold, so the mutation
fails. But the server answers immediately and nothing *forces* the requests to overlap: a warm burst
that happened to serialise leaves a smaller pool than the second burst needs, and the test reports
fragmentation that did not happen. Caught by review, not by a run — it had not flaked yet.

**v3 — barrier on the warm burst, second burst left sequential.** Every warm request is held in flight
until all of them have arrived, so the pool size is exact. Deterministic. And the mutation passed
again, because a sequential second loop needs one connection and `DefaultTransport` still has two.

**v4 — barrier on both.** The first burst leaves 8 idle; the second needs 0 on a shared pool and at
least 6 on its own. Stable across 8 runs; the mutation opens 6 extra connections.

## Why each intermediate version looked finished

Each was a *correct fix for the defect it was addressing*. v2 genuinely fixed v1's blindness. v3
genuinely fixed v2's raciness. What made them wrong was the property nobody re-checked — the one that
had been fine a moment earlier.

That is the trap: after fixing discrimination you run the mutation and see red, and stop. After fixing
determinism you run the test ten times and see green, and stop. **Each check confirms the property
you were just working on.**

## The habit

After any change to a **fixture** — not the code, the fixture — run both, in this order:

1. **the mutation**: break the mechanism, confirm red;
2. **the repeat loop**: 6–8 runs unmutated, confirm green every time.

Both, every time, even when the change was "obviously" only about one of them. In this ticket every
single version needed both and got one.

A corollary worth naming: **a test that has not flaked yet is not evidence it is deterministic.** v2's
raciness was found by reading, and it would have passed CI that day. Where a fixture depends on
scheduling, force the schedule — a barrier holding N requests in flight is a few lines and converts
"usually" into "exactly".

## The other half of the same ticket

Two more of the six findings were the same failure in different clothes, both in tests written
*because* a risk had been identified:

- A test meant to prove the shared transport still records spans asserted that a **traceparent header**
  reached the server. A transport bound to a no-op tracer still injects that header from the parent
  context while recording nothing — so it would have stayed green through the exact failure it was
  written to catch. Identifying a hazard and then writing an assertion that cannot see it is worse
  than writing none, because it reads as coverage.
- Its replacement mutated **global** OTel state and claimed to restore it. It cannot: the initial
  global provider is a delegating proxy whose delegate is set once and permanently, so "restoring"
  leaves later callers routed at a shut-down provider.

Six findings across two review passes, **zero of them on the four-line production change**. The change
was verified carefully — measured before and after, checked against the in-repo precedent — and the
tests were written the way tests usually are: plausibly. **A new test is a change and earns the same
treatment.**

## And a separate lesson from the same ticket

**A correct measurement can sit beside a regression it does not measure.**

Tuning the shared client's transport produced an honest 56 → 16 improvement in connections opened. It
would *also* have fragmented an existing pool: `obs.Client()` and the clients built as
`&http.Client{Timeout: …}` with a nil Transport had been sharing `http.DefaultTransport` **by
accident**, and cloning for one of them split that apart. The before/after number would have been just
as true with the regression shipped.

When a change makes something **private** that was previously **shared by default**, the measurement of
the private thing improving says nothing about what stopped sharing. Enumerate what else was riding
the default — here, one `grep '&http.Client{'` across every service, which surfaced two call sites to
move and three to deliberately exclude.
