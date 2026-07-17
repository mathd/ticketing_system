# ADR-022: Service migrations run out-of-band, as one-shot jobs

Date: 2026-07-15

## Status

Accepted (approved at the TKT-66 plan gate, 2026-07-15)

Supersedes [ADR-008](./ADR-008-embedded-migrations.md) **on placement only** — see § Decision for
what ADR-008 decided that still stands.

## Context

[ADR-008](./ADR-008-embedded-migrations.md) bundled four choices: goose v3 as a library, SQL embedded
via `embed.FS`, per-service schema ownership, and **`goose.Up` at service startup, before listening**,
single-instance and fail-fast under a 30-second deadline. Only the fourth is at issue here.

That placement was correct at Compose scale and is unchanged since. ADR-008 recorded its own cost —
*"startup-time migration couples deploy and migrate (fine for Compose-scale; revisit if rollout
orchestration ever appears)"* — and its own trigger: *"Startup ordering with future multi-replica
services needs goose's advisory-lock support — to be revisited when a second replica exists."*
This ADR is that revisit, pulled forward by TKT-62.

What is true today, before this decision, uniformly across all five services:

- **Deploy = downtime.** A service does not listen until every migration finishes.
- **A slow migration is a hard failure.** The 30s deadline cancels it; fail-fast means the process
  never starts.
- **No advisory lock.** A second replica would race the migration. No service runs a second replica.
- **No deployment exists.** `deploy/` contains one database-bootstrap SQL file; there is no
  Kubernetes, Helm, IaC, CD pipeline, or published image. `docs/conventions/continuous-delivery.md`
  is explicit: *"The repository currently has continuous integration, not an application release or
  deployment pipeline… Docker Compose is the only runtime topology."*

### What this decision does *not* buy — recorded because TKT-66 claimed otherwise

TKT-66 was filed on the premise that decoupling unblocks TKT-62's `CREATE INDEX CONCURRENTLY`
question, because [ADR-020](./ADR-020-catalog-index-build-concurrency.md) names decoupling as its
precondition (1). **That premise is false, and the plan gate rejected it.** ADR-020's preconditions
are **conjunctive** — *"when all hold, revisit this ADR"*, each joined by an explicit **and**:

1. migrations decoupled from startup — satisfied by this ADR; **and**
2. more than one replica or an external writer exists — **still false**; **and**
3. a target table is populated enough that the build duration matters — **still false**.

Decoupling is **necessary but not sufficient**. CIC remains deferred after this ADR, and (2) and (3)
are product realities, not decisions available at a gate.

Nor does this decision buy availability. **There is no deploy to decouple from**, so "deploy =
downtime" has no present cost. Every benefit below is an option on a future that may not arrive.

### Why decide it now anyway

`AGENTS.md` frames this repository as a **testbed** for evaluating AI-assisted development on a
non-trivial application, explicitly optimizing for learning over fastest-MVP delivery. Migration
placement is a real architectural boundary that every production ticketing system has to get right,
and the repo already contains both mechanisms this needs, making the experiment unusually cheap:

- **The service binaries already dispatch subcommands.** Distroless images have no shell, so
  `healthcheck` is already a subcommand of each binary; payments already dispatches three. A
  `migrate` subcommand adds no image plumbing.
- **A one-shot job is already idiomatic here.** `nats-init` runs `restart: "no"` and every Go service
  already waits on it via `service_completed_successfully`.

This is the honest justification, and the only one. Had this been a client engagement with a
delivery date, keeping startup migration would have been the right answer.

## Possible Solutions

- **Option 1 — A `migrate` subcommand on each service binary, run as a one-shot Compose job per service (chosen):**
    - Pros: reuses the existing binary, image and subcommand dispatch — none of ADR-008 Option 2's
      "extra CLI + image plumbing" cost; reuses the `nats-init` job pattern and its
      `service_completed_successfully` edge, so no process can start against an unmigrated schema;
      keeps every ADR-008 property except placement (goose-as-library, embedded SQL, per-service
      ownership, fail-fast, 30s bound); five independent jobs, each holding only its own role, so no
      cross-service coordinator appears (ADR-002/007); the deploy primitive a future rollout would
      invoke exists and is exercised by the gate rather than described in prose.
    - Cons: **buys no availability today** — nothing is deployed, and locally the stack still waits
      for migrations before it is healthy; five more Compose entries and five more dependency edges;
      correct ordering moves from an application invariant to an orchestration invariant, so a
      missing `depends_on` edge becomes a way to start against an old schema; without an advisory
      lock, two concurrently launched jobs for the same service would race (no such invoker exists).
- **Option 2 — A one-shot job using an external migration CLI (`golang-migrate`), per ADR-008 Option 2:**
    - Pros: decouples migration from start.
    - Cons: exactly what ADR-008 rejected — a second migration tool, five more images, and SQL that
      no longer travels inside the binary that needs it. Option 1 obtains the same decoupling with
      the binary already built.
- **Option 3 — goose advisory locking, retaining startup migration:**
    - Pros: makes concurrent replicas safe, which is ADR-008's literal trigger sentence.
    - Cons: addresses concurrency but **preserves the placement problem** — every new process still
      couples readiness to migration and still faces the 30s ceiling. It answers a question nothing
      is asking (no second replica) while leaving the one that was asked.
- **Option 4 — Keep startup migration, accept the ceiling, record the trigger:**
    - Pros: zero cost; adequate and simple for the only topology that exists; nothing speculative.
    - Cons: leaves ADR-020 precondition (1) false; declines to exercise a boundary the testbed exists
      to exercise. **This is the option a delivery-pressured project should choose**, and it loses
      here only on the testbed argument.

## Decision

We **move migrations out of the service startup path**. Each service binary gains a `migrate`
subcommand that applies its own embedded migrations and exits; each service runs it as a one-shot
Compose job (`<service>-migrate`), and the service declares
`depends_on: { <service>-migrate: { condition: service_completed_successfully } }`. The server path
never migrates.

**What ADR-008 decided that still stands** (superseded on placement only — do not read this ADR as
reversing them): goose v3 **as a library**, migrations **embedded** via `embed.FS` in the service
binary, **per-service schema ownership** with no coordinator crossing service boundaries, product-
mandated seed data as ordinary migrations, and **fail-fast** on migration failure under a **30-second
deadline**. Schema and code still ship in the same binary and the same PR.

**Advisory locking is deferred, deliberately.** Compose runs exactly one job per service and no
rollout system exists, so a lock would add mechanism with no race to exercise. The invariant is
therefore: **per-service migrations are serialized by having exactly one invoker.** Any future
deployment work that can launch two migration jobs for the same service concurrently **must** adopt
goose's advisory lock before doing so, or preserve single-invoker serialization. This ADR is the
place that obligation is recorded.

**The 30s deadline stays.** ADR-020 requires it be removed only in the same change that adopts CIC,
never before; this is not that change.

## Consequences

- **Positive:**
    - No application process executes schema changes; a service starting is no longer a migration
      event.
    - The `migrate` subcommand is the primitive a future rollout invokes — the reason
      "deploy = migrate" is no longer structural.
    - ADR-020 precondition (1) is satisfied. **CIC remains deferred**: (2) and (3) are still false.
    - Each schema stays owned by its service and role (ADR-002/007); the five jobs are independent
      and could run in parallel, since they touch five separate databases.
    - The boundary is enforced by the gate, not by prose, and it takes **three** assertions, because
      each is vacuous for what the next one proves:
        1. `TestMigrationsAppliedOutOfBand` — each database is at its latest checked-in version.
           Proves *migratedness*. It was equally true under ADR-008, so it passes unchanged on the
           code this ADR replaces and proves nothing about placement on its own.
        2. `TestMigrationsRanBeforeServicesStarted` — each job exited 0 before its service started.
           Catches an absent, failing, or ungated job. Still not placement: a job that exits 0 first
           and a server that *also* migrates satisfies it.
        3. `TestServerModeDoesNotMigrate` — catalog in server mode against an empty database never
           creates `goose_db_version`. **This is the one that fails if `store.Migrate` returns to
           `run()`**, which the other two would let through as a silent no-op. A passing healthcheck
           is its positive control, so a crash-on-boot cannot pass it vacuously.
- **Negative:**
    - **Buys no availability today**, and local startup still waits for migrations. This ADR spends
      real diff on a future option, and `AGENTS.md`'s testbed framing is the whole justification.
    - **The startup path is not actually clean — commerce is a named exception.**
      `BackfillCompletionOutbox` (`services/commerce/internal/store/store.go:193`) stays on the
      server path: it is data repair rather than schema, it is idempotent, and the migrate job has
      already applied the schema it reads. But it **sequentially scans** `orders` on every boot —
      nothing indexes `status` or `guest_order_ref`; the only index on the table is
      `orders_recovery_claimable_idx (recovery_next_attempt_at)` — buffers every match in memory,
      runs under `main.go`'s 30-second deadline, and fails the service if it trips. That is
      precisely the fail-fast startup coupling this ADR removes from migrations, still live for one
      service. Nothing in code bounds the table; it is small **today**, which is a fact about the
      data, not a guarantee.
      **Moving it into the migrate job would help, and the first draft of this ADR wrongly said it
      would not.** `service_completed_successfully` gates at stack *creation*; a restarted commerce
      container does **not** re-run its completed one-shot job, but *does* re-run `run()` and so
      re-scans. So the job placement would pay the scan once per deploy instead of once per boot.
      It stays on the server path by the TKT-66 gate's decision, with the ceiling filed as TKT-71 —
      recorded rather than hidden: this decision removed migrations from startup, **not** all
      startup-coupled data work.
      *Update (TKT-71, 2026-07-17):* the backfill now runs behind the outbox drainer — once per
      process, before the first drain pass, error logged rather than fatal — so commerce's startup
      path no longer does data work that can delay or fail readiness. The exception this paragraph
      records is closed; `smoke/smoke_test.go` `TestCommerceStartsWithoutRunningBackfill` pins it.
    - Correct ordering is now an orchestration invariant. A missing `service_completed_successfully`
      edge starts a service against an old schema — invisible to `/healthz`, which only pings the
      connection. The version assertion in `smoke/smoke_test.go` exists for exactly this.
    - Five more Compose services and five more entries in `compose.smoke.yaml`; a Go service present
      in `compose.yaml` but missing from the smoke override falls back to a hermetic build — slower,
      never wrong.
    - A future out-of-band migration can run while an old revision still serves, which makes
      backward-compatible **expand/contract** migrations a required discipline rather than a
      nicety — the cost of decoupling, owed by whoever builds the rollout.
    - No advisory lock: safe only while exactly one invoker exists per service. Recorded above as an
      obligation on future deployment work.
    - **Rollback is not automated and must not be.** `goose down` never runs automatically; roll the
      application back only if it is compatible with the forward schema, otherwise roll forward.

## References

- TKT-66 · TKT-62 (which filed this, on a premise the gate corrected) · TKT-2
- [ADR-008](./ADR-008-embedded-migrations.md) — superseded on placement; its other three choices stand
- [ADR-020](./ADR-020-catalog-index-build-concurrency.md) — precondition (1) satisfied; CIC still deferred
- [ADR-002](./ADR-002-services-from-day-one.md) · [ADR-007](./ADR-007-postgres-nats.md) — per-service database, role and schema ownership
- `docs/conventions/continuous-delivery.md` — no deployment pipeline exists today
- [goose](https://github.com/pressly/goose) · [Compose `depends_on`](https://docs.docker.com/reference/compose-file/services/#depends_on)
