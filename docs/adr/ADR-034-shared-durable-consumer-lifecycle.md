# ADR-034: A shared durable-consumer lifecycle primitive — and where it stops

Date: 2026-07-27

## Status

Accepted

## Context

`docs/architecture.md` describes `shared/go` as a **shared kernel** whose additions **require an
ADR**. ADR-033 set the bar and, in passing, wrote this ADR's assignment. Its rejected
**Option 4 — "a shared consumer framework: envelope, dispatch, disposition, readiness"** — said:

> Wrong scope and wrong shape *today*. Dispositions genuinely differ: inventory quarantines and
> acks (TKT-68); access has no quarantine store and parks outstanding (ADR-017 §5b′, TKT-74). A
> framework would have to encode both, or flatten a difference the services depend on. **The
> run-loop half is TKT-127's subject, and it is shared *behaviour*, not shared *contract* — a
> different kind of kernel entry, deserving its own argument.**

This is that argument.

The concrete finding (architecture review R13) was that inventory and access implement the same
durable-consumer protocol twice. **Read against the code at the time, most of that was not true**,
and the difference matters for where the line lands:

| Claimed duplicated | Actually, at the time of this ADR |
|---|---|
| Termination handling | `waitConsume` existed in **access only** (`run.go`, two call sites). Inventory's `Run` blocked on `<-ctx.Done()` and **never observed `cc.Closed()`**. |
| Readiness latch | Three `ready atomic.Bool` fields — three instances of one stdlib type, not duplicated code. Only inventory had serialization (`readinessMu`, TKT-90). |
| Quarantine on unknown schema | Inventory quarantines to a bounded table and **acks**; access **parks** with a delayed NAK and has no quarantine store at all. |
| Backoff / `MaxDeliver` policy | One implementation (access's order consumer). Inventory has none; access's policy projector uses hardcoded delays. |
| Envelope dedup | Store-owned SQL in both, not consumer-loop logic. |

So there was **one** duplication to remove, and it was not a duplication of code between two
services — it was a **guarantee present in one service and absent from the other**. Access had
had, since TKT-97, the rule ADR-017 §236-241 states: async termination goes **loudly unready and
never self-heals**, latching `/readyz` false *and* returning an error that exits the process.
Inventory ran the same kind of loop with no such protection: delete its durable underneath it and
the process stayed up, reporting **ready**, consuming nothing.

The constraint on any fix was unusually sharp. TKT-127's COS required that TKT-90's, TKT-97's and
TKT-99's tests pass **unmodified** — a test needing an edit was to be treated as a moved guarantee
and investigated, not edited. Three of those pin things a move can silently break:

- TKT-97's four tests call the **unexported** `waitConsume` symbol directly, in `package consumer`.
- TKT-99's broker-level smoke test asserts the diagnostic **verbatim** —
  `access: access-slot-policy: consume context closed (durable deleted or subscription terminated)`
  — and its own comment calls that literal "the whole discriminator", assembled from three separate
  pieces of production code.
- TKT-90's tests reach directly into the struct field (`c.ready.Store(false)`).

## Possible Solutions

- **Option 1: Do nothing — leave `waitConsume` in access.**
    - Pros:
        - Zero risk. No kernel growth. No behaviour change anywhere.
        - Honest that only one service had written the helper.
    - Cons:
        - Leaves inventory **without the guarantee entirely** — the real defect. A deleted durable
          stays invisible: process up, `/readyz` green, nothing consuming.
        - The next consumer starts by copying or, worse, by not copying.

- **Option 2: Copy `waitConsume` into inventory.**
    - Pros:
        - Smallest diff. No kernel entry, no ADR.
        - Fixes inventory's missing guarantee immediately.
    - Cons:
        - Makes the reviewed finding literally true for the first time: two copies of one
          protocol, free to drift. TKT-123 (the diagnostic cannot distinguish causes) would then
          need two coordinated edits instead of one.
        - The verbatim TKT-99 contract string would exist in two places with nothing tying them.

- **Option 3: A shared `Wait`, plus a shared serialized `Readiness` type.**
    - Pros:
        - Would also absorb inventory's `readinessMu` and satisfy the most literal reading of
          "the readiness latch exists once".
    - Cons:
        - **Downgrades the guarantee it claims to centralize.** `readinessMu` is an unexported
          field in the same package as its only two writers: unbypassable *by construction*. An
          exported kernel type with a public `Store` that skips the mutex replaces that with a
          documented convention — and TKT-90 is one of the guarantees TKT-127 was told to preserve.
        - Serves two call sites in one service. Access has no check-then-latch race, because it has
          no quarantine state to check. Speculative generality at the kernel's highest bar.
        - The latch needs no lifting: it is `sync/atomic.Bool`, which already exists exactly once.

- **Option 4: The full framework** — durable config, `Consume`, dispatch, disposition, backoff,
  quarantine, dedup.
    - Pros:
        - Largest apparent code reduction; a starting point for future consumers.
    - Cons:
        - Already rejected by ADR-033, and the table above is why: it would encode a union of
          unrelated policies or flatten differences the services depend on.
        - Would move TKT-97's guarantee off its package-local production seam.
        - Would put NATS and service policy into the kernel.

## Decision

We adopt **Option 3 minus its readiness half** — that is, the narrowest form that removes the
defect: `ticketing/shared/durableconsumer` exports a narrow wait operation. The
cause-aware form is the production entry point; `Wait` preserves the simpler API
for callers without a broker cause handler.

```go
func Wait(ctx context.Context, closed <-chan struct{}, ready *atomic.Bool, name string) error
func WaitWithCause(ctx context.Context, closed <-chan struct{}, ready *atomic.Bool, name string, cause *TerminationCause) error
```

It distinguishes parent cancellation from asynchronous consume-context termination, returns `nil`
and touches nothing on the former, and on the latter latches `ready` false **and** returns the
diagnostic. It never stores `true`: termination does not self-heal. The package imports `context`,
`fmt` and `sync/atomic` — no JetStream, no `domainevent`, no database.

**Each service calls `WaitWithCause` directly from `Run`.** Shared-package tests own the wait
semantics. Service tests enter each production `Run` path and prove that a closed consume context
reaches the shared operation. This keeps production wiring observable without retaining a facade
whose direct tests could pass after production bypassed it.

**Inventory adopts it** — a deliberate behaviour addition, not a refactor, with its own test. Clean
shutdown is unchanged (`Wait` returns `nil`, and `Run`'s existing tail returns the same
context-cancellation error).

**The error string is a contract.** `TestWaitTerminationDiagnosticIsExact` pins it as a
**hand-written literal** — not built from the format string, because a fixture derived from the
code under test encodes the property it claims to prove and cannot fail (ADR-017's trap). This
moves TKT-99's verbatim contract from a docker-only smoke assertion into a unit test that fails in
milliseconds.

### The line this package does not cross

Everything below stays with the owning service:

- **Durable configuration, stream lookup, handler registration and dispatch.** The two
  `consumerConfig`s differ in filter subjects, `MaxAckPending` and `BackOff`, and inventory deletes
  an orphaned durable on the way in.
- **Disposition** — term, park, quarantine, NAK-with-delay, and whether readiness latches. Service
  policy, and it differs (ADR-033).
- **Envelope decoding, per-subject known schema ranges, retry schedules, quarantine persistence,
  dedup.**
- **Readiness serialization.** Inventory's `readinessMu` stays inventory's, for the Option 3 reason
  above. This is the boundary a future ticket is most likely to be tempted to cross, so it is
  written down rather than left to be re-derived: *the reason to lift a mutex is that two services
  need it, and only one does.*
- **Startup readiness gates.** Inventory's `startupConverge` and access's backlog-drain loop decide
  when a consumer *becomes* ready. This package only ever makes one unready.

The dividing line: **this package owns when a consumer stops and what it says about it. What a
consumer does stays with the service.**

## Consequences

- **Positive:**
    - Inventory gains the ADR-017 §236-241 guarantee it never had: a deleted durable now latches
      unready and exits instead of stalling silently while reporting healthy. **Bounded, not
      absolute** — see the matching Negative below.
    - The termination diagnostic has **one** producer. TKT-123's future fix has a single owner and
      a single place to renegotiate the TKT-99 smoke contract.
    - TKT-99's verbatim string is now pinned by a unit test as well as a broker test.
    - No `go.mod` changes anywhere — `go.work` already wires all eight modules.
    - The original TKT-127 move preserved the existing assertions. R15 later replaced facade-level
      tests with shared-helper tests for wait semantics and service `Run` tests for production
      wiring.

- **Negative:**
    - `shared/go` gains a second domain-adjacent package, and the kernel now holds *behaviour* as
      well as *contract*. The boundary section above is the whole defence, and it is prose, not a
      compiler check.
    - Inventory can now exit on durable deletion where before it stayed up. That is the point, but
      it is a new production exit path, and it interacts with TKT-121 (inventory's `main` does not
      filter cancellation-caused consumer errors) which remains open.
    - ~~**Inventory's adoption has a startup-shaped hole, and this ADR should not be read as
      claiming otherwise.**~~ ***Closed by TKT-122.*** `Run` now starts one `WaitWithCause` observer
      immediately after `Consume` and passes `startupConverge` a context that observer cancels, so a
      durable deleted during the pass unwinds it promptly instead of at the next retry boundary. On
      termination `Run` returns `WaitWithCause`'s durable-named diagnostic rather than
      `startupConverge`'s `context.Canceled`, which is what keeps `main`'s
      `isShutdownConsumerError` from filtering a real failure away as a clean stop.
      `refreshStartupReadiness` additionally refuses to latch `true` once termination has been
      observed. That check hangs off a one-way `terminated` flag which `Ready()` also consults, not
      off the context alone: `durableconsumer.Wait` stores `false` through a plain atomic **without**
      `readinessMu`, so it is a third readiness writer TKT-90's mutex never contemplated, and a
      context-only check is a TOCTOU that lets a startup pass overwrite the false Wait just latched.
      Serializing the store would merely shrink that window to a mutex handoff; a terminal flag
      closes it, because termination never self-heals (§240-241) and so a latch outliving the flag
      can never be right. The
      original text is kept below because the reasoning for deferring it is still the record of why
      it waited.
      **Inventory's adoption had a startup-shaped hole, and this ADR should not be read as
      claiming otherwise.** `Run` begins observing `cc.Closed()` only after `startupConverge`
      returns, and that pass retries up to three times with a 5s backoff, making a serial catalog
      call per pool. A durable deleted during it is not noticed until the pass ends. Readiness is
      `false` throughout — `refreshStartupReadiness` stores `true` as its last act — so the service
      is honestly unready rather than falsely ready, and the cost is a late exit, not a wrong
      answer. Closing the window means racing termination against the pass, which makes the pass's
      error indistinguishable from a shutdown cancellation and therefore collides with TKT-121.
      That is a design decision, so it is **TKT-135**, not a line in this refactor. The adversarial
      review of TKT-127 raised this and it was triaged incidental on the reasoning above; recorded
      here rather than only on the board, because the gap is invisible from the code.
    - Each service needs a production-path test for every `Run` call site. Shared helper tests
      prove the mechanism but cannot prove that a service wired it into the running consumer.
    - The reviewed finding's other four claims are **not** addressed, because they were not true.
      Anyone re-reading R13 will find it broader than what shipped; this ADR's Context table is the
      record of why.

## References

- TKT-127 (this decision), TKT-126 / ADR-033 (the envelope, and the option that handed this here)
- TKT-97 (the reaction to `ConsumeContext.Closed()`), TKT-99 (the broker-level proof), TKT-90 (the
  readiness/skew serialization left in place)
- Resolved since: TKT-121 (inventory `main`'s cancellation race, `2fb5709`); TKT-135 and TKT-122's
  startup half (the startup-window gap, closed as described above).
- Still open, deliberately: TKT-123 (the diagnostic cannot distinguish causes), TKT-133 (inventory
  does not validate `type` against the subject).

### After cancellation, a closed subscription is ambiguous — shutdown wins (TKT-122)

`Wait` used to pick at random when the parent context and `Closed()` were both ready, and this ADR
called both answers defensible. TKT-122's review agreed the randomness was wrong, then established
which way it has to be deterministic — and it is not the intuitive one.

**`Closed()` does not encode its cause, and after SIGTERM we close it ourselves.** Both mains
`defer nc.Close()` without joining their consumer goroutines, so an ordinary stop routinely closes
the subscription underneath a goroutine that has not yet arbitrated its already-cancelled context.
Preferring termination there would emit a durable-deletion diagnostic on **clean shutdowns** — a
false alarm on every stop that lost the race, corrupting the very operator evidence the
producer-side log exists to preserve. That is strictly worse than missing a rare true one.

So **shutdown wins** when both are ready, deterministically. A durable that genuinely dies inside
that window is not distinguished.

**This residual is quieter than the drain-snapshot one below, and the two must not be conflated.**
They share a cause — once SIGTERM has landed this process stops classifying late consumer events,
because doing so correctly requires a bounded join of every consumer before `nc.Close()`, precisely
the lifecycle coupling this ticket declined to buy — but they differ in what survives:

| | what `Wait` returns | logged? | exit code |
|---|---|---|---|
| **Arbitration** (this section) | `nil` — shutdown wins | **no, nothing at all** | 0 |
| **Drain snapshot** (below) | the durable diagnostic | **yes**, by the producer goroutine | 0 |

So the drain gives up only the exit code, while the arbitration gives up the **whole signal**. That
is the more expensive of the two accepted residuals and it is accepted for a narrower reason: any
attempt to report it produces false alarms on ordinary stops, which corrupts the evidence rather
than adding to it.

**Before** cancellation — a durable deleted under a live consumer, the case that actually matters —
nothing is ambiguous and termination is reported exactly as before.

*An earlier revision of this section claimed an ordinary stop "can never be misreported" because the
deferred `cc.Stop()` runs after `Run` returns. That was false: the outer `nc.Close()` closes
subscriptions independently of `cc.Stop()`, and the claim survived one review pass before a second
caught it.*

### The shutdown drain stays a snapshot — decided, not overlooked (TKT-122)

`awaitShutdown` in both services drains `consumerErr` for errors that have **already arrived**, then
shuts down. A consumer failure that materialises *during* teardown is published to a channel nobody
reads, and the process exits 0. TKT-122 asked whether to close that window by collecting one
terminal result per consumer within a bounded grace period. **It stays open, deliberately.**

The exit status of a SIGTERM'd process feeds nothing here:

- Every Go service inherits **`restart: unless-stopped`** (`compose.yaml`, the `x-go-service`
  anchor) — *not* `on-failure`. On an operator-requested stop Docker suppresses restart **regardless
  of exit code**, and the drain is only reached after `ctx.Done()`, i.e. on exactly that path.
- Compose does not act on an unhealthy `/readyz` either (§236-238 above), so readiness is already
  false and already inert.
- `scripts/smoke.sh` tears the stack down with `compose down` and never inspects service status;
  `smoke/access_consumer_test.go` (TKT-99) asserts a **restart plus the durable-named diagnostic**,
  not a numeric status.

So a bounded join would spend part of the shutdown budget waiting on the component most likely to be
wedged, to produce a value nothing reads. Access pays double — two terminal results, either of which
can hold the process open.

**What TKT-122 did change:** the error is now **logged at the producing goroutine**, before the send.
Previously a late failure left *no* exit code **and** no line anywhere — `main` prints to stderr only
on the error it returns, which by then it never sees. That was a separate loss from the exit code,
and it costs nothing to fix: no latency, no lifecycle coupling, same shutdown-cancellation predicate
so a clean unwind stays quiet.

Scope that claim precisely: it covers **errors `Wait` actually returns**. For those, the accepted
residual is *"we do not act on this signal"* rather than *"we lose it"*. It does **not** cover the
arbitration case above, where `Wait` returns `nil` and there is no error to log — that signal is
still lost completely.

**Reopen if:** an orchestrator or alerting path starts distinguishing zero from non-zero SIGTERM
exits; an incident shows a teardown-only error carries evidence the log line does not; or consumer
termination gains a documented upper bound and the deployment gains a shutdown budget that can pay
for a join.

### Amendment: NATS authentication and durable consumer permissions (ADR-072 / TKT-170)

[ADR-072](ADR-072-nats-publisher-acls.md) configures authentication and per-principal ACLs on the NATS
broker. Consumers now require explicit `$JS.API.CONSUMER.*` publish permissions and `_INBOX.>` subscribe
permissions to manage JetStream durables. The durable consumer lifecycle primitive in `shared/durableconsumer`
operates without modification because credentials are provided in the broker connection URL (`NATS_URL`).

## References

- [ADR-033: One domain-event envelope in the shared kernel](ADR-033-shared-domain-event-envelope.md)
- [ADR-017: Domain event schema evolution](ADR-017-domain-event-schema-evolution.md) §5b, §5b′, §236-241
- [ADR-009: Contract-first APIs and the platform envelope](ADR-009-contract-first-apis.md) §5
