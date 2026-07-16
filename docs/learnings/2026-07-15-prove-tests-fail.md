# A passing test is not evidence it tests anything

Date: 2026-07-15 · From TKT-53, TKT-60, PR #43; seen again in TKT-67/PR #51 · Status: practice, not enforced by the gate

## What happened

Green is the default state of a test that is broken, misaimed, or not running at all. Three
instances in two tickets, none of which the gate could see:

- **A test that never ran.** TKT-53's scoped-read test sat behind a `-run` allowlist in
  `scripts/smoke.sh` and executed zero times before merge — see
  [`2026-07-15-run-allowlists-strand-tests.md`](./2026-07-15-run-allowlists-strand-tests.md).
- **A test that asserted the wrong claim.** TKT-53's festival read test asserted the *output* of a
  scoping fix. The broken code returned the correct festival — only the scan was wrong — so the
  test would have passed against unfixed code. It proved nothing it was written to prove.
  (ADR-019 rule 2 is the catalog-read form of this.)
- **A test that drifted onto the wrong target.** `TestArchivedLifecycleMigrationRollbackGuard`
  called a bare `provider.Down(ctx)`, which pops whichever migration is newest. The moment TKT-60
  added migration 0007, the subtest named "0003 rolls back cleanly" was silently testing an index
  drop instead. It kept passing throughout. Fixed in PR #43 with `DownTo(versionBeforeArchived)`
  plus an assertion that 0003's columns are actually gone.

**Nothing here was found by sabotage, and none of it by the gate — every one was green.** The first
was found by reading `smoke.sh`; the second by reading the test; the third by an adversarial review
of the diff. Sabotage is what *settled* them afterwards: PR #43 confirmed the migration fix by
leaving `archived_at` behind and watching the new assertion go red while the old version stayed
green through the same sabotage.

Keep that distinction — it is the whole point. Sabotage is a cheap **verifier**, not a detector. It
answers "is this test evidence?" once you already suspect a test; it will not tell you which of your
green tests to suspect. Reading the gate script, reading the test against the claim it names, and
adversarial review are what surface the candidates.

## The practice

**Break it on purpose and confirm the test goes red.** Before trusting a test as evidence, make the
thing it guards actually wrong — revert the fix, leave the column behind, remove the index — and
watch it fail. If it stays green, it is not evidence, whatever its name says.

It is cheap: one edit, one run, one revert. Applied to any of the three above it would have exposed
them, and it is worth doing exactly when the stakes tempt you to skip it — a test written to prove a
fix, a test whose name makes a strong claim, a test you are about to cite in a PR description.

Corollary for a fix that makes **two** claims: it owes two tests, and you must be able to say which
sabotage reddens which. A scoping fix claims both that the result is scoped and that the scan is;
one of those can pass while the other is broken. ADR-019 records that specific case.

## Evidence

- `services/catalog/internal/store/season_smoke_test.go` — the two-claims/two-tests shape (TKT-60)
- `services/catalog/internal/store/migration_smoke_test.go` — the `DownTo` fix and its comment (PR #43)
- [ADR-019](../adr/ADR-019-catalog-read-path-scoping.md) — rule 2, the catalog-read instance
