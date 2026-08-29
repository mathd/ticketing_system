# Review fix batches

**Date**: 2026-08-28
**Source**: `2026-08-28-code-review.md`, `2026-08-28-architecture.md`, `2026-08-28-documentation.md`
**Purpose**: group the 51 findings into units that should be fixed together, so each batch is one ticket with one mechanism, one test story, and one review.

## Where these live

All 17 batches are on the board as **TKT-296 through TKT-312**, status Backlog, each labelled
**`review:2026-08-28`**. Filter on that label to see exactly this set and nothing else.

| Ticket | Batch | Risk | Subject |
|---|---|---|---|
| TKT-296 | 1 | high | Inventory's two idempotency idioms (retry 409 + staff key namespace) |
| TKT-297 | 2 | high | Refund-leg ceiling int64 overflow |
| TKT-298 | 3 | medium | The offline PSP contract after TKT-257's guard |
| TKT-299 | 4 | high | Access's admission union read four ways |
| TKT-300 | 5 | medium | Extract the leased-runner skeleton, then sweep |
| TKT-301 | 6 | high | Commerce is welded to the fake PSP's tokens |
| TKT-302 | 7 | medium | Backport the four web fixes to the sibling app |
| TKT-303 | 8 | medium | Make the gate prove what it claims |
| TKT-304 | 9 | low | Dead mechanisms and outlived comments |
| TKT-305 | 10 | medium | Absent versus broken, answered consistently |
| TKT-306 | 11 | low | Catalog's resolver and aggregate drift |
| TKT-307 | 12 | low | Inventory's own doctrine, unapplied |
| TKT-308 | 13 | medium | Cross-service calls on the untuned transport |
| TKT-309 | 14 | medium | The presale-code operator surface |
| TKT-310 | 15 | low | Four docs still name the superseded board |
| TKT-311 | 16 | low | The July-snapshot docs refresh |
| TKT-312 | 17 | low | Repo hygiene and untracked strays |

Two findings were **not** filed, because the board already tracks them — see the table below.

## Grouping rule

Findings are batched when they share **the mechanism, not the file**. Two defects in one file with
unrelated causes are two tickets. One rule broken in five files is one ticket, because the fix is a
sweep and the review question is "did you get all the call sites?" — which is exactly the failure
mode this review found over and over (a rule established in one file, its siblings left behind).

Each batch names the sweep's **enumeration**: the list the implementer must prove complete. That is
the acceptance criterion, not the individual edits.

## Already tracked — do not re-file

| Finding | Existing ticket | Note |
|---|---|---|
| Code R8 (parked order re-enters money path) | **TKT-292** (Ready) | Same defect, same call sites, already written up from TKT-280 pass 2. Drop R8 from the review backlog. |
| Code R6 (zero-amount charge admitted at the boundary) | **TKT-285** (Backlog) | TKT-285 owns the product question ("a comped order cannot be bought"); R6 adds the narrowing this review verified — `server.go:271` checks `< 0` while `store.go:246` requires strictly positive, and the operation ends bound-unresolved. **Append R6's evidence to TKT-285 rather than filing a new ticket.** |
| Code R5 (fake-ok status can never resolve) | related to **TKT-144** | TKT-144 owns the failed-compensation-refund path; R5 is a different terminal state (a captured charge the fake cannot confirm) reached through the same recovery runner. File separately (Batch 3) but link it. |

---

## Batch 1 — Inventory idempotency namespace and fingerprint discipline

**Findings**: R1 (hold retry 409), R2 (staff key prefixes in the public namespace), R23 (`%v` fingerprint join)
**Severity**: 2 critical, 1 suggestion · **Risk**: high (buyer path + tenant-visible 500s) · **Suggested type**: Story

Three faces of one thing: inventory has **two idempotency idioms** and the newer one was not swept
across the older call sites. R1 is a fingerprint computed at two different points in the same
function; R2 is a namespace separated by string prefix where migration 0016 already established a
structural column; R23 is the unframed join that makes a fingerprint non-injective.

Fix them together because they touch the same rows, the same unique indexes, and the same
"what identifies a repeat request" question — and because splitting them means two migrations over
`claims` instead of one.

**Enumeration to prove complete**: every `fingerprint`/`opFingerprint` computation site, and every
key written into `claims.idempotency_key` — currently `store.go:473,656`, `operational.go:186,342`,
`reservations.go:51,211,325`.

**Test story**: a retry test per entry point (none exists today for the ungated-code path), plus a
public-caller-poisons-staff-key test that must go red before the fix. The staff-prefix fix needs a
migration; the enumeration test should assert no code path writes a prefixed key into the public
namespace.

**Do not** fix R1 alone. It looks like a two-line move of one `fingerprint()` call, and shipping it
without R2 leaves the namespace defect live under a test suite that now looks like it covers
idempotency.

---

## Batch 2 — Refund-leg ceiling overflow

**Findings**: R3
**Severity**: critical · **Risk**: high (money) · **Suggested type**: Task

Stands alone deliberately. `bound.Int64+amount > captured.Int64` is unchecked Go addition
(`refund_legs.go:127`) and the contract sets no maximum (`openapi.yaml:373`). The fix is a checked
add plus a contract bound, and the review question is narrow: does any other money comparison in
payments do unchecked arithmetic on a caller-supplied value?

**Enumeration to prove complete**: every arithmetic comparison in payments against a request-supplied
amount. Commerce already has `checkedAdd`/`checkedMul`/`checkedSub` (`catalog_fees.go:194-259`) —
decide whether that helper moves to `shared/go` or gets a payments-local twin, and say why in the PR.

**Test story**: a near-`MaxInt64` leg must be refused. Derive the expected refusal from the rule
("no leg may take bound past captured"), not from what the code does — this is exactly the
blessed-value trap AGENTS.md names.

---

## Batch 3 — The offline PSP contract after TKT-257's fail-closed guard

**Findings**: R5 (fake-ok unresolvable), R29 (`CompleteOperation` ignores RowsAffected), R30 (superseded status drops confirmation)
**Severity**: 1 important, 2 suggestions · **Risk**: medium (parks orders permanently in the offline stack) · **Suggested type**: Story

All three are the same root cause: TKT-257 added a fail-closed confirmation guard and the other
parties to the old contract were not revisited. The fake still answers the pre-257 shape, the
completion sink still uses the pre-257 silent-no-op style its two siblings were migrated away from,
and one status branch still drops the evidence 257 introduced.

**Enumeration to prove complete**: every producer of a `psp.Result` (fake, Stripe, the status
resolver's own branches) and every sink that judges one. Also every `Complete*` function in
payments' store — two of three already re-read on zero rows.

**Test story**: the gap must be demonstrated. A crashed-then-resolved `fake-ok` operation currently
502s forever and no test covers `GET /internal/psp/status` for one; that test should go red first.

---

## Batch 4 — Access: the admission union is defined once and read three ways

**Findings**: R4 (exchange admitted-check misses quarantine), R31 (three drifted replay helpers)
**Severity**: 1 critical, 1 suggestion · **Risk**: high (double admission) · **Suggested type**: Story

R4 is the defect; R31 is why it happened. ADR-025 §D2's "admission history is trail ∪ quarantine"
lives in three hand-copies (`postgres.go:438-482`, `scan.go:229-276`, `reconcile.go:314-355`) plus a
fourth partial copy in `exchanges.go:219-225` that reads only half. Consolidating without fixing R4
leaves the bug; fixing R4 without consolidating leaves four copies.

**Enumeration to prove complete**: every place that answers "has this ticket been admitted?" — the
scan path, the replay path, the reconcile path, and the exchange guard.

**Test story**: the existing tests admit via the trail only. Add a quarantine-side admission fixture
and assert `SwitchExchange` refuses it. Per the repo's guard-with-N-predicates rule, the test must
satisfy every earlier predicate so it actually reaches the admitted check.

---

## Batch 5 — The leased-runner skeleton: extract it, then sweep its rules

**Findings**: Arch R2 (extract the skeleton), Code R7 (bulk-refund lease sized from the wrong timeout), R28 (two attempt-accounting conventions)
**Severity**: 1 important, 1 medium-impact, 1 suggestion · **Risk**: medium · **Suggested type**: Story

The copy-don't-share decision (ADR-062 §1) has now produced its predicted failure twice: the lease
sizing fix reached two of three runners, and ADR-062 F4's release-only attempt accounting reached
two of four. A third recurrence is a matter of time.

Do the extraction and the two sweeps in one ticket, in that order — extracting first means the
sweeps are one edit each instead of four, and the shared package is where the pinning tests belong.
The per-runner state machines stay put; only the claim/lease-expiry/release/gauge loop moves.

**Enumeration to prove complete**: `recovery`, `bulkrefund`, `reversal`, `exchangesweep` — for each,
the lease is sized from the timeout of the client that runner actually drives, and attempts are
charged on release, not claim.

**Test story**: `TestSizingTheLeaseFromTheWrongTimeoutUnderCoversTheBatch` exists in two packages;
after extraction it should exist once, in the shared package, and cover all four call sites.
Note the crash case F4 names — `Abandon` only mitigates orderly shutdown.

**ADR**: this amends ADR-062 §1. Record the reversal of the copy-don't-share decision, with the two
recurrences as the evidence.

---

## Batch 6 — Unhook commerce from the fake PSP's token vocabulary

**Findings**: Arch R1, Code R9 (exchange upgrades hard-code `fake-ok`)
**Severity**: 1 high-impact, 1 important · **Risk**: high (forecloses a real PSP) · **Suggested type**: Story

Commerce validates `fakepsp.ValidToken()` at public checkout (`server.go:1548`) and submits a
literal `"fake-ok"` for exchange upgrades (`exchanges.go:580`). Both are the same boundary error:
provider semantics leaked out of payments into commerce, via `shared/`. Payments' own port declares
the token opaque and already builds a Stripe adapter when configured.

One ticket because the fix is one decision (commerce treats the token as opaque; payments refuses
unknown tokens) applied at two call sites, and because the exchange-upgrade half raises a product
question the checkout half answers: **no buyer instrument is collected for an upgrade at all.**

**Enumeration to prove complete**: every reference to `shared/go/fakepsp` outside payments, and every
literal payment token in commerce.

**ADR**: required. Every other deferred provider decision in commerce is recorded (the mail fake is
loudly logged and ADR'd); this one is recorded nowhere. State what an upgrade charge does when a
real PSP is configured, even if the answer is "still deferred, and here is the refusal".

---

## Batch 7 — Backport the fixes the sibling app never got

**Findings**: R10 (server-timezone datetime), R11 (channel rename has no revision), R12 (back-office wall clock), R13 (scanner not strict)
**Severity**: 4 important · **Risk**: medium (silent data corruption, lost updates) · **Suggested type**: Story

Four defects, one cause: a fix landed in the app whose ticket triggered it and was never carried to
its sibling. Each app's own comments already describe the correct behaviour — the storefront
explains why the wall clock is wrong, `events/new.astro` explains why bare `datetime-local` is
wrong, `slots/[id].astro` implements the revision token the channel form lacks.

Batch them because they are one review question ("which app-level rules are not uniform?") and one
browser-spec story. Splitting them into four tickets means four browser runs and four reviews of the
same class of change.

**Enumeration to prove complete**: for each rule — zone-carrying datetime inputs, optimistic
revision on full-replace writes, monotonic session clock, strict tsconfig — list every app and page
that should follow it and show it does.

**Test story**: R11 needs a **two-actor** browser spec; the current one is single-actor and
structurally cannot see the lost update. R10 needs a spec that submits from a non-UTC client and
re-reads the row exactly (the repo's own truncation lesson: assert equality, not non-null).

**Note**: R11 overlaps TKT-286's territory (the same page, the allocation editor's blind spots).
Check whether it belongs there before filing.

---

## Batch 8 — Make the gate prove what it claims

**Findings**: R14 (back-office smoke skips), R37 partial (trace-provenance substring match), R36 partial (access has no generated TS types)
**Severity**: 1 important, 2 suggestions · **Risk**: medium (silent coverage loss) · **Suggested type**: Task

Three ways the gate can be green while proving less than it appears to. The skip helpers are the
serious one: 19 call sites funnel through `t.Skip`, so a refactor of credential provisioning drops
the entire back-office role matrix — including the refund refusal cases — without a red run. The
suite's own rule says `t.Fatal`, never `t.Skip`.

Access having no generated types belongs here rather than with the web batch: it is a
`check-generate` gap, and the fix is a Makefile/codegen change plus adopting the types in three
files.

**Enumeration to prove complete**: every `t.Skip` in `smoke/`, every service with an
`api/openapi.yaml` versus every entry in `check-generate`'s list.

**Test story**: for the skip fix, run the suite with the credential env unset and show it fails
rather than passes.

---

## Batch 9 — Retire the dead mechanisms and the comments that outlived their code

**Findings**: R17, R21 (partial), R22, R26, R30 (partial), R32, R37 (partial)
**Severity**: suggestions · **Risk**: low, but corrosive · **Suggested type**: Task

In a codebase where in-code rationale is load-bearing and (this review confirmed) accurate almost
everywhere, a stale comment is worse than in an ordinary repo: readers correctly trust these
comments. Same for a dead function kept green by its own test, which reads as a guarantee.

Contents: the TKT-155 exposure rationale that still argues from a public-internet threat model; the
`PinSeat` reachability note falsified by TKT-80; the high-water-ceiling comment describing a
mechanism TKT-162 deleted; the delivery package's false "one definition" claim; `ValidateExchangeTarget`
and the unused `Outstanding()`/`Attempts` members; `psp.ErrNotImplemented`; the truncated
`compose.yaml` comment and the miscounted urandom draws in `stack-env.sh`.

One ticket, because each edit is a line or two and the review question is uniform: does the comment
now match the code, and is the deleted code actually dead? Note `performancePublishedEnvelope` is the
opposite case — it must **not** be deleted; it needs a comment saying it exists for the wire-compat
golden test.

---

## Batch 10 — Absent versus broken, answered consistently

**Findings**: R24 partial (`holdSeating` 404s on DB failure), R27 (partner availability swallows a bad body), R34 (ticket page says "Not found" for a 503; scanner says "No connection" for a server error)
**Severity**: suggestions · **Risk**: medium (a reseller's retry loop cannot tell sold-out from broken) · **Suggested type**: Task

Four places where a failure is reported as an absence. Both codebases treat this distinction as
load-bearing elsewhere — `upstream.ts:32-55` in the back office, the 502-on-undecodable idiom in
commerce — so these are the stragglers, not a missing convention.

Batch them because the fix is one rule ("an error you could not read is 5xx, never 404/empty/absent")
and one reviewer question. R27 is the one with a real consumer impact: a reseller polling
availability during an inventory outage reads `available: 0` and backs off as if sold out.

---

## Batch 11 — Catalog's resolver and aggregate drift

**Findings**: R18 (three divergent duplicate-id guards), R19 (unpin succeeds against an unknown pair), R20 (zero-seat seat map publishable), R21 partial (festival aggregate leaves EventID zero)
**Severity**: suggestions · **Risk**: low · **Suggested type**: Task

Catalog's deliberate duplication (three resolvers, two aggregate builders) has started to diverge in
small ways that are all currently harmless and all currently invisible. ADR-046 §7 named the revisit
trigger; this is it. Keep as one ticket: same file cluster, same question (are the copies still
saying the same thing?), and no behaviour change expected — which makes it a good candidate for a
low-risk run.

R20 is the odd one out (an operator-error surface, not drift) but lives in the same file and the
same lifecycle; fold it in or split it off if the ticket gets too wide.

---

## Batch 12 — Inventory's error mapping and consumer disposition

**Findings**: R24 (reversed window 500s instead of 400), R25 (schema-1 poison NAKs forever)
**Severity**: suggestions · **Risk**: low-medium (each poison event holds a MaxAckPending slot) · **Suggested type**: Task

Two places where inventory's own doctrine is not applied: `validatePresaleCode` exists specifically
so a constraint violation does not surface as a 500, and the consumer file states a retry-vs-terminate
rule its schema-1 branch ignores. Small, adjacent, same service, same "apply the rule you already
wrote" review.

---

## Batch 13 — Shared HTTP transport tuning

**Findings**: R15
**Severity**: important · **Risk**: medium under on-sale load · **Suggested type**: Task

Stands alone: it is a performance change on the money path with a load-test story rather than a unit
test. The gateway's comment already makes the argument; `obs.Client()` needs the same treatment one
hop later, where checkout calls inventory and payments.

**Verification**: this is what `compose.onsale-load.yaml` and `TestOnsaleLoadProof` exist for.
Measure connection churn before and after rather than asserting a config value — a test that pins
`MaxIdleConnsPerHost` is a test about the setting, not about the behaviour.

**Watch**: TKT-149 tracks an existing flake in that load proof; don't let this ticket inherit it.

---

## Batch 14 — Ship the presale-code operator surface, or pin its absence

**Findings**: R16
**Severity**: important · **Risk**: medium (feature is unusable as shipped) · **Suggested type**: Story

`IssuePresaleCode` and `PresaleCodeStatuses` have no route, no OpenAPI path, and no CLI subcommand,
so ADR-064's gated channels can only be configured by hand-writing rows. This is a product decision
(which surface: API, CLI, or back office?) before it is a code change, so it wants shaping rather
than a direct fix.

If the answer is "later", the ticket should still land the pin: a test asserting the exports are
unreachable, so nobody mistakes them for a live surface — the ADR-021 pattern of pinning a known gap.

---

## Batch 15 — Documentation: the four docs the board correction missed

**Findings**: Doc R1
**Severity**: critical · **Risk**: an agent or a person following the wrong source of truth · **Suggested type**: Task

`docs/ROADMAP.md:3`, `docs/technical-delivery-standards.md:11`, `docs/product/prd-v1.md:6`,
`docs/conventions/commits-and-branching.md:26` still call the `.sdlc/` git-derived board the source
of truth for ticket status. Commit b0b7f7c1 fixed AGENTS.md only. Do this one first and alone —
it is four small edits, it is the highest-consequence doc error, and it should not wait behind a
larger docs refresh.

---

## Batch 16 — Documentation: the July snapshot refresh

**Findings**: Doc R2 (ROADMAP completed list), R3 (docker.md), R4 (docs/README.md index + empty `architecture/` dir), R5 (nonexistent gate-scan path), R6 (back office missing from design docs), R7 (make check stage list), R8 (smoke port/slot)
**Severity**: 4 important, 3 suggestions · **Suggested type**: Task

Every one of these is a doc written in mid-July and not revisited: the back office is missing from
three of them, image pins are two patch versions stale, the index covers half of `docs/`, and
ROADMAP shows zero progress across two months of merges. One ticket, one pass, one reviewer reading
the docs against the tree.

**Do it after Batch 15**, not with it — mixing an urgent four-line correction into a broad refresh
delays the correction.

---

## Batch 17 — Repo hygiene and untracked strays

**Findings**: Doc R9 (ADR-064 front matter), R10 (untracked files), Code R36 partial (browser specs predating `lib/support.mjs`)
**Severity**: suggestions · **Suggested type**: Task

Delete the `Zone.Identifier` NTFS artifact. Decide where `.sdlc/vault-migration-plan.md` belongs now
that `.sdlc/` is a superseded stub. Commit `docs/reviews/2026-08-26-backlog-decision-brief.md`, which
looks finished and simply unstaged. Normalize ADR-064's front matter to the template so status
tooling reads it. Migrate the three browser specs that hand-roll what `lib/support.mjs` provides.

Trivial individually; batched so they cost one review instead of five. **Stage paths by name** —
AGENTS.md's `git add -A docs/` lesson is directly relevant to a ticket whose whole content is
untracked-file cleanup.

---

## Suggested order

1. **TKT-310** (batch 15 — doc correction, four lines, highest consequence per byte)
2. **TKT-296, TKT-297, TKT-299** (batches 1, 2, 4 — the criticals: buyer-path 409, money overflow, double admission)
3. **TKT-301, then TKT-298** (batches 6, 3 — the fake-PSP boundary constrains the offline contract, so it goes first)
4. **TKT-300** (batch 5 — extraction plus sweeps; largest structural change, do it before more runners land)
5. **TKT-302, TKT-303** (batches 7, 8 — the sibling backports and the gate's honesty)
6. **TKT-308, TKT-309, TKT-311** (batches 13, 14, 16 — load-path tuning, the presale surface decision, the docs refresh)
7. **TKT-304, TKT-305, TKT-306, TKT-307, TKT-312** (batches 9-12, 17 — the low-risk cleanups; good candidates for cheaper models)

Batches 1, 4, 5, and 6 each also want an ADR touch — 1 amends nothing but should cite migration
0016's rule, 4 clarifies ADR-025 §D2's scope, 5 reverses ADR-062 §1, and 6 needs a new one.
