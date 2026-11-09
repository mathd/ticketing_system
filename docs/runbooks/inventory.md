# Inventory repair and seat pins

Local Compose procedures. Run commands from the repository root. Configuration comes from
[configuration](../configuration.md); safety and approval rules remain in [AGENTS.md](../../AGENTS.md).
Load only the section needed for the operation. Historical examples retain their ticket IDs.

## Restoring `claim_history` (TKT-295)

Since TKT-295, `append_order` leads the order of inventory's `claim_history` (ADR-021 §Amendment
(TKT-295)). A restore must keep the numbers and the sequence that issues them consistent:

1. Stop every inventory writer and any maintenance session before the restore.
2. Restore the rows. Keep `append_order` (legacy NULLs included) and `occurred_at` as they are.
   An ordinary `INSERT` or `COPY` fires `claim_history_set_append_order` and **renumbers** the
   rows in input order — that keeps relative order only if the input is already in append order,
   and no tool here checks that. To keep the original numbers, disable that trigger for the load.
   A session with `session_replication_role = replica` also skips it.
3. Re-enable the trigger, and return the restore session to its normal role.
4. With writers still stopped, run the guard:

   ```bash
   docker compose run --rm --no-deps inventory check-claim-history-sequence
   ```

   Exit 0 (`status=ok`): continue. A refusal (`status=needs-repair`) means the next append would be
   numbered at or below restored history. Run it with `--repair` (it only advances the sequence to
   the restored maximum; it never changes a row), then run the check again and require exit 0.
   These refusals are never repaired, and need an operator: the numbering trigger is missing,
   disabled or replica-only (step 3 did not happen); the sequence's settings differ from
   migration 0012 (increment 1, cache 1, no cycle, a positive range); or fewer than two numbers remain (an
   ordinary append uses two).
5. Reopen writes.

The guard holds `claim_history` locked for the whole check (`SHARE ROW EXCLUSIVE`), so it waits
for any append still in flight and blocks new ones until it is done — run it while writes are
stopped. That lock does not cover a `nextval` called outside an insert, which is one reason step 1
stops maintenance sessions too. `--timeout` (default `30s`) bounds the whole check, lock wait
included; `max(append_order)` scans the table, so give a large restore a longer one. It does not
detect missing, duplicated or dishonest rows.

## Seat-pin reconciliation (TKT-112)

A seated hold pins its seats in catalog (`seat_map_pins`, `pinned_by = 'hold:<claim_id>'`) and
unpins them when the hold is released, or best-effort when the next mutation on that pool sweeps
the expiry (ADR-031 §4 — no worker, deliberately). A pool that is never touched again after an
on-sale therefore keeps the pins of holds that simply timed out. Those pins fail **safe**: they
block a now-unnecessary seat-map edit with a 409, they never orphan or oversell a seat. Left
alone they accumulate and slowly degrade an organizer's ability to edit a published map.

`inventory reconcile-pins` is the one-shot cleanup:

```bash
docker compose exec inventory /app reconcile-pins
# inventory reconcile-pins: scanned=812 reclaimed=37 live=770 unknown=2 malformed=0 other=3
```

It drains catalog's pin table in keyset pages over the internal read
(`GET /internal/seat-map-pins`), asks inventory for one verdict per `hold:` reference, and unpins
the dead ones through the same family-locked batch route a release uses. Requires the service's
usual `DATABASE_URL`, `CATALOG_URL` and `INTERNAL_SERVICE_TOKEN`; it never reads catalog's
database (ADR-010).

**Three dispositions leave the pin in place on purpose**, and the counters name each:

- `live` — the claim still consumes its seats: held and unexpired, finalizing, or **confirmed**.
  A confirmed seated claim keeps its pin because the seat is sold.
- `unknown` — the pin names a claim this inventory database does not have. Reported, never
  reclaimed: that is the shape an inventory database restored *behind* catalog presents, and
  since `hold:` pins cover sold seats too, unpinning one could let an edit orphan a sold seat.
- `malformed` — `pinned_by` is `hold:<not-a-uuid>`. The column is free-form, so this is
  reachable; it cannot be correlated to a claim, so it is a human's problem.

The run **exits zero** when `unknown` or `malformed` pins remain — that is the expected fail-safe
state, the same contract `reprocess-quarantine` has for rows awaiting a newer binary. A non-zero
exit means a store, transport, or unpin failure; every unpin it did apply is idempotent, so the
fix is to rerun. Deciding a claim is dead **settles that pool's lazy expiry under the pool lock**
(the ordinary `sweepExpired`), so a run also expires other due holds in the pools it touches and
settles any pending capacity cut there — the same thing the next hold on that pool would have
done.

Scope, stated the way ADR-021 requires: this is **honest-writer reconciliation**. It guards
against our own bugs and against concurrent honest writers. A writer with catalog database access
can insert or delete pins at will and nothing here detects it; a writer with inventory database
access can forge the claims the verdict is derived from. It is not tamper-evidence.

**Enforced limits and response cap (TKT-143).** The megabyte-scale row limitation is closed.
Catalog enforces length bounds in its database schema and OpenAPI contract: `seat_identity` is
constrained to 1..200 characters, and `pinned_by` is constrained to 1..45 characters. The inventory
consumer bounds response bodies with `maxSeatPinPageBytes = 828511`, derived from these limits:

- Fixed row bytes: 2 braces + 4 commas + 3 UUIDs of 36 characters + 47 field-name characters +
  10 quotes round the names + 5 colons + 10 quotes round the values = 186 bytes.
- Bounded strings worst-case: 6 × (200 + 45) = 1470 bytes. `json.NewEncoder` escapes `&`, `<`, `>`,
  U+2028, U+2029 and most control characters into 6-byte `\uXXXX` sequences. `\b`, `\f`, `\n`,
  `\r` and `\t` take 2 bytes, and a multi-byte rune encodes as raw UTF-8 in 4 bytes or fewer, so
  no input beats 6 bytes per character.
- Maximum row bytes: 186 + 1470 = 1656 bytes.
- Full page of 500 rows: 9 (prefix `{"pins":[`) + 500 × 1656 + 499 (commas) + 3 (suffix `]}\n`) = 828511 bytes.

A conforming single row cannot exceed 1656 bytes. Adaptive page halving in `reconcile-pins` provides
defence in depth against deployment skew or producer regressions that yield an oversized multi-row page.

## Orphan-prevention correction wave (TKT-183)

Catalog emits `performance.published` **schema 5** for a seated slot bound to a seat-map
version with `orphan_prevention_enabled` on. TKT-179 shipped that setting *before* this
transport existed, so slots published in between were emitted at **schema 4**, had
`event_emitted_at` set, and will never be emitted again — re-POSTing publish is idempotent.
Inventory holds an ordinary seated pool for them, with no flag and no adjacency, while the
back office insists the rule is on. The wave repairs exactly those.

```
docker compose exec catalog /app reemit-orphan-prevention
```

**Rollout order is the reason this is a separate revision — do not reorder it:**

1. TKT-181's inventory (schema-5 arm + projection) and TKT-182's rule are fully deployed.
2. Roll out this catalog binary.
3. Run the command.
4. Once every pre-TKT-183 catalog replica has drained, **run it again**.

Step 4 is a stopping condition, not a correctness precondition. The wave keeps no
correction state, so each run reconciles every published slot currently bound to an enabled
version — a slot published at schema 4 by an undrained old replica is picked up by the next
run. Re-running is safe and expected: the correction identity is deterministic per
publication, so repeats converge instead of multiplying events (inventory's
`consumed_events` and JetStream's dedup window absorb them).

It prints `corrected=<n>`, the number of publications re-emitted. Any publish failure aborts
the run and surfaces the error — the count never claims a repair it cannot prove. A second
run reporting the same count is normal and means nothing is wrong; it does **not** mean the
first run failed.

**What it does not tell you.** `corrected=` counts *emissions*, not repaired pools. Inventory
applies them asynchronously. To confirm the repair, check the pool: `orphan_prevention_enabled`
true and a populated `seat_claim_adjacency` for that pool. And per ADR-021: this is
honest-writer consistency — anyone who can write to catalog can change bindings or forge
candidates, and anyone who can write to inventory can undo the repair.

**Do not run it against an inventory that predates TKT-181.** Schema 5 reaching such a replica
is quarantined and acked, latches the consumer unready, and needs `reprocess-quarantine`
**plus a restart** by an operator (see *Inventory catalog-event quarantine operations*).

## Best-available ordering correction wave (TKT-258)

Catalog keeps emitting the same schema-4 publication for rule-off seated performances. The
payload already names the bound seat-map version. New inventory binaries fetch that map's
geometry and store ordering data while leaving `orphan_prevention_enabled` false. TKT-258
changes no event schema or payload, and makes no access-service change.

Deploy the new inventory version to every replica first. Let all old inventory consumers
drain before running the correction wave. The inventory version decides whether the event
repairs a pool: an old consumer can acknowledge the correction's fresh event id without
adding ordering rows, and `consumed_events` then prevents a later delivery with that same id
from repairing the pool.

After the inventory rollout and drain, deploy catalog with the command and run:

```bash
docker compose exec catalog /app reemit-best-available-ordering
```

The command takes no flags. It scans published seated performances bound to rule-off maps,
in pages ordered by performance id, and re-emits each schema-4 publication with a separate
deterministic event id. A publish failure stops the run. `corrected=<n>` counts emitted
publications, not inventory repairs.

If a correction run happened while any old inventory consumer was still consuming, bump the
wave's id namespace suffix in `services/catalog/internal/events/events.go` (for example,
`best-available-ordering-schema4-1` to `best-available-ordering-schema4-2`), deploy that
catalog build, and run the command again. The namespace is versioned because an old
inventory consumer records the id even though it cannot apply the projection. The same id
cannot repair that pool later.

The candidate query has no completion marker, so each run reconciles the current set. A
repeat run with the same namespace emits the same ids and is safe only after all consuming
inventory replicas can apply the correction.

To confirm a repair, check inventory. The pool must still have
`orphan_prevention_enabled=false`, and its `seat_claim_adjacency` rows must have non-null
`row_key`, `position`, and `row_rank`. A pool with no ordering data still returns
`best_available_unsupported` until a valid projection is applied. Rule-off maps that
inventory cannot project (a seatless map, labels that differ only by trailing whitespace, a
map over inventory's 8 MiB read limit) keep their seated inventory without ordering rows
and return `best_available_unsupported`. There is no supported repair for such a pool yet:
editing the map creates a new version that the pool is not bound to, and a rerun of the
wave reuses a consumed event id. TKT-499 owns the repair path.
