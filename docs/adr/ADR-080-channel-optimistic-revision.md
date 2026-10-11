# ADR-080: Optimistic revision on the catalog channel PUT

Date: 2026-10-11

## Status

Accepted (TKT-325). Decisions D2, D3, D5 and D6 of TKT-325. D6 supersedes D4. The catalog half
(D2, D3) ships first; the back-office half (D5, D6) ships in the same ticket.

## Context

`PUT /channels/{channelId}` replaces all mutable fields of a channel: `display_name`, `kind` and
`enabled`. `code` is required too, and must equal the stored code. The back office has two forms on
`channels.astro` that send this PUT. The rename form and the toggle form both put `code`,
`display_name`, `kind` and `enabled` in hidden inputs, with the values from the rendered row.

Before this ADR, the PUT had no optimistic token. That caused a lost update in two directions:

- Operator A opens the page. Operator B disables the channel. A renames it. A's stale hidden
  `enabled=true` re-enables the channel. Both operators see success.
- Operator A opens the page. Operator B renames the channel. A toggles it. A's stale hidden
  `display_name` reverts B's rename (D1).

This is the lost-update class that TKT-250 closed for channel allocations with
`allocation_revision` (ADR-024 §Decision (8)). It was never applied to channels. Migration
`0018_channels.sql` has no revision column, so the fix needs a migration and a contract change.

## Possible Solutions

- **Do nothing.** Last write wins. Rejected: the defect is silent, and both forms cause it.
- **Compare `updated_at`.** No new column. Rejected: a timestamp has a resolution and depends on a
  clock, and the token then is a formatted time that every client must round-trip exactly.
- **Conditional UPDATE (`WHERE revision = $n`) with a follow-up read to explain zero rows.**
  Rejected: the explanation read is a second statement, outside the write, that must be scoped the
  same way (TKT-251), and it reads a row that can change between the two statements.
- **Organizer-scoped `SELECT ... FOR UPDATE`, compare in Go, then UPDATE (chosen, D2).** One
  transaction. The lock makes the comparison and the write one decision. Ownership is decided by the
  locking read, before any other check, so a refusal never describes another organizer's row.
- **Make the hidden fields unsubmittable: the page reads the row and merges the one edited field
  (D4, superseded by D6).** See §Decision (D6) for why this was not done.

## Decision

### D2: the PUT requires `expected_revision`, compared under the row lock

- `ChannelUpdate.expected_revision` is **required**: an integer, `format: int64`, `minimum: 0`,
  `maximum: 9007199254740991`. It has no default, and there is no unconditional arm. Zero is a real
  revision (every channel starts there), not "skip the check". The maximum is the largest integer a
  JavaScript client holds exactly. Inventory's `allocation_revision` uses the same maximum.
- The allocation contract makes its token optional for one credential class (ADR-024 §Decision
  (8)). That exception does not apply here. The channel PUT already requires the staff credential
  and the organizer assertion together (ADR-058), and the back office is its only caller.
- `Channel.revision` is required in every full-row response: create, the internal GET, the operator
  list and the PUT. The public projection does not change.
- `store.UpdateChannel` does these steps, in this order:
  1. The pure write gate (`validateChannelWrite`).
  2. In one transaction, `SELECT ... FROM channels WHERE id = $1 AND organizer_id = $2 FOR UPDATE`.
     No row gives `ErrNotFound`. A missing channel and another organizer's channel give the same
     answer.
  3. Compare the locked revision with `expected_revision`. A mismatch gives
     `ErrChannelRevisionMismatch`.
  4. Compare the immutable code. A mismatch gives `ErrChannelCodeImmutable`.
  5. `UPDATE ... SET ..., revision = revision + 1, updated_at = now()`, scoped the same way,
     `RETURNING` the full row. Then commit. Success is returned only after the commit.
- The UPDATE has **no** revision predicate. The row lock is the only thing that serializes the
  comparison. The contention test depends on that (see §Test seams).
- Each successful PUT increments the revision by one, also when the values are unchanged. A refusal
  writes nothing: no field, no timestamp, no revision.
- A mismatch is HTTP 409 with the generated `ChannelUpdateConflict` body:
  `{"error": "channel changed since it was read; nothing was saved", "code": "channel_revision_mismatch"}`.
  `code` is optional in the schema, with `enum: [channel_revision_mismatch]`, so the
  code-immutability 409 (which has no `code`) still validates under ADR-028. Two protections
  cover two different edits:
    - The **build** protects the type. `writeStoreError` builds the generated
      `ChannelUpdateConflict`, so if that component is deleted, catalog does not compile.
    - The build does **not** protect the declaration. If only the operation's `'409'` response is
      deleted, the type is still generated (`codegen.yaml` sets `skip-prune: true`), catalog
      compiles, and ADR-028's response validator turns every stale refusal into a 500. The router
      test `TestStaleChannelPutIsTheCodedConflictAndWritesNothing` (`channels_test.go`) protects the
      declaration: it asserts the exact stale 409 through `env.validateResponse`, which fails on a
      masked response. Verified by deleting only that `'409'`: catalog built, and the test failed
      with "production validator masked the response ... (status 500)".
- The message does not blame another operator. After a lost response, the change can be the
  caller's own earlier write.

### D3: migration 0025

```sql
ALTER TABLE channels ADD COLUMN revision bigint NOT NULL DEFAULT 0 CHECK (revision >= 0);
```

- Existing rows and new creates start at 0. No backfill and no index: the column is read only
  through the primary-key lookup, and ADR-020 stands.
- The constant default is metadata, so there is no table rewrite. The CHECK is validated by one
  scan of the table under the ALTER's lock. TKT-325's shaping called the change "metadata-only";
  that is correct for the default and not for the CHECK.
- The Down takes `ACCESS EXCLUSIVE` and refuses while any revision is above 0, because an editor
  relies on the protection then. Like inventory 0017, the guard is keyed on real state, not on row
  count: every pre-existing channel is at 0.
- The migration runs out-of-band, through the catalog binary's `migrate` subcommand (ADR-022).

### D5: a refusal keeps the submitted token; recovery is an explicit review

- After a refusal, the failed row keeps the token the browser **submitted**. A second click sends
  the same token and is refused again. If the page rendered the fresh token, the second click would
  apply the change the refusal just stopped (TKT-250).
- The page says that nothing was saved, and offers a "Review current channel" action. That action
  is a POST that reads the channel and renders fresh values and a fresh token. It sends no PUT.
  The operator then makes the change again, on purpose. Recovery needs no browser reload.

### D6: the forms keep their hidden fields and add the token (supersedes D4)

- Both forms keep their hidden replacement fields and add a hidden `expected_revision` from the
  rendered row. The page adds no server read before the PUT.
- After a refusal, the failed row's hidden fields come from the fresh read and its token from the
  submission. The rename input is pre-filled from the submitted name only when the failed action
  was a rename.
- Why not D4's merge: `UpdateChannel` is the only writer and bumps on every success, so a matching
  token means the stored row equals the rendered row. The merge would change no honest result.
  TKT-244's "make it unsubmittable" rule is for values the client must not choose; here every
  hidden value is the operator's own allowed write to their own tenant, and catalog already checks
  `code`. The merge would also keep the other operator's field when the comparison is deleted, so
  the browser spec's survival assertions could not detect a missing token. A merge can be added
  later; removing one is harder.

## The adversary (ADR-021)

This is lost-update protection between **honest writers**. It is not authorization and not
tamper-evidence.

- A caller can send any revision. Ownership is decided by the organizer predicate in the locking
  read, never by this number.
- A writer with catalog database access can set any revision. Nothing here detects that.
- The protection stops two operators who race through the back office from silently overwriting
  each other. Nothing more.

## Test seams

Each assertion is at the tier where its mechanism is (AGENTS.md):

- **Store smoke tests** (`channels_smoke_test.go`), against real PostgreSQL. Create starts at 0, and
  GET and the operator list return the stored value after a bump (at 0, a read that forgot the
  column would also pass). A stale update is refused in both directions, and the other writer's
  full row, timestamp and revision survive. An unchanged replacement bumps exactly once. Another
  organizer gets `ErrNotFound` with the current revision and code, and with a stale revision and a
  wrong code; the owner can still update. A wrong code with a current token is
  `ErrChannelCodeImmutable`; with a stale token it is `ErrChannelRevisionMismatch`.
- **Contention test** (`TestConcurrentChannelUpdatesWithOneRevisionApplyExactlyOnce`). A lock
  holder on a dedicated `db.Conn` takes the row. Two writers with the same revision start. The test
  waits until PostgreSQL reports both writers blocked behind the holder, then releases it. Exactly
  one writer succeeds, and the revision moves once. With `FOR UPDATE` removed and the Go comparison
  kept, both writers read 0, both pass, both apply, and the test fails.
  The wait counts the holder's **transitive** waiters. The direct form,
  `$holder = ANY(pg_blocking_pids(pid))`, reaches only 1: the second row-lock waiter queues on the
  tuple lock that the first waiter holds, so `pg_blocking_pids` names the first waiter, not the
  holder (checked on PostgreSQL 18).
- **Migration smoke tests** (`migration_smoke_test.go`, `versionBeforeChannelRevision = 24`). A
  channel seeded at version 24 keeps its fields and reads revision 0 after 0025. The CHECK refuses
  -1 with SQLSTATE 23514 (any error would also match "column does not exist"). The Down succeeds
  with every revision at 0 and asserts the column was present first. The Down is refused after a
  bump made through `UpdateChannel`.
- **API router tests** (`channels_test.go`), through the real router and both validators. A fresh
  PUT answers the next revision; the second PUT sends 1, so a handler that dropped the token would
  be refused. A stale PUT is the exact coded 409 and leaves the row unchanged. A missing, null,
  string, fractional, negative, above-maximum or overflowing token is 400. These use a revision-0
  row, so only the validator, not a decoded zero, can refuse. Another organizer is 404 with the
  current and a stale token. The code-immutability 409 has no `code`.
- **The API fake** compares in Go, in the store's order, and bumps only on success. It proves the
  handler forwards the token and maps the refusal. It cannot prove the lock or the SQL predicate.
- **Back-office unit and page tests** (`web/backoffice/test/`). `channel-form.test.ts`: the token
  parser accepts digits only and a safe integer (`''`, `' '`, `-1`, `1.5`, `1e3`, `0x10` and
  `9007199254740993` are refused; `0` is accepted), and a coded 409 is classified as a stale
  refusal before the "every 409 is about `code`" branch. `api.test.ts` and `api-decoding.test.ts`:
  the client sends `expected_revision` unchanged, keeps the error `code`, and makes the read fail on
  a missing or malformed revision, never 0. `channels-page.test.ts` renders the real page against a
  stubbed catalog. It checks which token each form carries after a refusal, that the toggle
  direction comes from the fresh state, that the rename input is pre-filled only after a refused
  rename, and that Review and a malformed token send no PUT. The page makes the PUT itself, so
  counting it at this tier is valid.
- **Browser spec** (`test/browser/channel-revision.mjs`, back-office half). Two actors, both
  directions, all writes through the forms, so the counter moves. A test that writes the table
  directly does not move the revision, and the "stale" token then still matches.

## Consequences

- **Positive:**
    - A stale channel replacement is refused, not applied, in both directions.
    - The refusal needs no follow-up read, so the failure path is scoped by the same locking read
      as the write (TKT-251).
    - The 409 body is the generated type, so the build catches a deleted schema component, and
      `TestStaleChannelPutIsTheCodedConflictAndWritesNothing` catches a deleted `'409'`
      declaration. Neither check alone covers both edits.
- **Negative:**
    - Every PUT client must read a revision and send it back. Today the back office is the only
      client. Between the catalog half and the back-office half, its PUT is refused with 400.
    - One more column, and one CHECK scan when the migration runs.
    - A save now takes a row lock for one short transaction.

## References

- TKT-325; origin TKT-302 (finding R11 of the 2026-08-28 code review; that file was removed from
  `docs/reviews/` in e3744ccd).
- [ADR-024](ADR-024-channel-allocations.md) §Decision (8): the allocation revision (TKT-250).
- [ADR-021](ADR-021-ticket-lifecycle-trail-integrity.md): name the adversary.
- [ADR-058](ADR-058-catalog-organizer-assertion.md): the organizer comes from the verified assertion.
- [ADR-028](ADR-028-response-drift-fail-closed.md): declared responses, fail-closed validation.
- [ADR-022](ADR-022-out-of-band-service-migrations.md), [ADR-020](ADR-020-catalog-index-build-concurrency.md),
  [ADR-009](ADR-009-contract-first-apis.md).
- `services/catalog/internal/store/migrations/0025_channel_revision.sql`
- [A precondition that cannot fail](../learnings/2026-08-15-a-precondition-that-cannot-fail.md),
  [Make it unsubmittable](../learnings/2026-08-15-make-it-unsubmittable-not-validated.md).
