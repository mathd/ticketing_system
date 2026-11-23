# ADR-087: Operator outcomes for failed post-purchase refunds

Date: 2026-11-20

## Status

Accepted (TKT-525, owner decisions of 2026-10-12, the TKT-525 plan decisions).

Amends [ADR-032](ADR-032-stripe-behind-the-psp-port.md) §Amendment — a provider-failed refund is
never re-submitted (TKT-347): the post-purchase refund path now has an operator route. The rule that
no code path and no operator command re-submits a failed refund is unchanged.

Makes one narrow exception to [ADR-037](ADR-037-post-purchase-refund-money-protocol.md) §4 (bound
attempts count against both ceilings): a refund with a recorded `abandoned` outcome no longer counts
(§12). ADR-037 §4 stays as written, including its rule against locks across provider calls. Qualifies
[ADR-040](ADR-040-event-cancellation-bulk-refund-runs.md) §3 the same way.

Delivery: TKT-525 is one PR in two slices. Slice S1 is `refunded_outside` (§1–§9). Slice S2 is
`abandoned`: the Commerce abandonment reservation, the Payments abandonment endpoint and marker
write, the two ceiling exclusions, the dispatch refusal, the Commerce outcome, the bulk rules and
the Back Office replacement (§10–§16).

**Amended by TKT-595 (2026-11-22)** — *the Decision introduction and §14 only.* Both command forms
check `PAYMENTS_URL`, the dedicated Payments credential and `DATABASE_URL` before they open the
database. The fallback to `INTERNAL_SERVICE_TOKEN` is removed.

**Amended by TKT-602 (2026-11-22)** — *§8 and §14 only.* §8 states when a later cancellation run
replays an outside completion and when it refunds a remaining quantity. §14 states that a Payments
marker can exist after a redirect on the first attempt. The Back Office redirect rule is
[ADR-088](ADR-088-backoffice-upstream-redirects.md).

**Amended by TKT-607 (2026-11-23)** — *the lost-reply consequence only.*

## Context

A staff refund or an event-cancellation (bulk) refund that the provider marks `failed` or
`canceled` stays `pending` in Commerce. Its Payments leg stays `bound`. ADR-037 §4 counts bound
attempts against both ceilings, so the failed attempt keeps its quantity and its amount reserved.
ADR-032 (TKT-347) forbids a re-submission, and no surface can record what an operator did outside
the platform. So the refund has no possible end, and the buyer can hold valid tickets for money that
came back by another route.

The owner decided on 2026-10-12:

- An operator command writes a new append-only outcome record in Commerce (ADR-003). The failed leg
  is not modified. The record is "not a payments fact" (ADR-032). It holds the refund id, the kind
  `refunded_outside` or `abandoned`, the amount in minor units and the currency, an external
  reference, the operator, the reason and the time.
- `refunded_outside` does not release the refund allowance. The money left, so the quantity and the
  amount stay counted against both ceilings, and the reversal runs.
- `abandoned` releases the allowance in **both** ceilings: the Commerce quantity sum and the Payments
  captured-money sum exclude the abandoned attempt.
- After a recorded `abandoned` outcome, staff can submit a new refund under a new key through the
  Back Office. The platform never re-submits automatically, and no operator command re-submits.
- A bulk cancellation run that finds an abandoned refund skips it with no Payments call.
- The scope is staff refunds and bulk cancellation. Recovery and exchange are TKT-532 and TKT-533.

## Decision

Both forms of `record-refund-outcome`, `refunded_outside` and `abandoned`, check their arguments and
then their configuration before they open the Commerce database (TKT-595). `PAYMENTS_URL` must be an
absolute `http` or `https` base URL with a host name, a port in 1–65535 if it names one, and no query
or fragment. `PAYMENTS_INTERNAL_TOKEN` is read with `runtimecfg.PaymentsTokenFromEnv`, the validator
Payments uses: at least 32 bytes, different from `INTERNAL_SERVICE_TOKEN`, and no fallback to the
shared token. `DATABASE_URL` must not be empty, because pgx reads an empty value as the shell's
PostgreSQL defaults. A local configuration refusal therefore creates no abandonment reservation.

### 1. One append-only outcome table with a generic subject

Commerce migration 0027 adds `refund_outcomes`, keyed by `(organizer_id, subject_kind, subject_id)`.
TKT-525 writes only `subject_kind='order_refund'`. The table has no foreign key to `order_refunds`,
so TKT-532 and TKT-533 can add their own subject kinds. Row and statement triggers refuse `UPDATE`,
`DELETE` and `TRUNCATE`. Amount and currency are copied from the refund row; the operator never types
them, so an outcome covers the whole stored refund or nothing. The external reference is at most 200
bytes for `refunded_outside`, which the operator types, and at most 255 bytes for `abandoned`, which
is the provider reference and the limit Payments accepts. A shorter Commerce limit would let Payments
release its allowance for a reference that Commerce cannot store.

### 2. An outside completion is linked to its evidence

0027 replaces the 0007 completion CHECK. It finds that CHECK by relation and by its exact set of
referenced columns, and stops unless exactly one matches. `order_refunds` gets:

- `outcome_subject_kind`, a plain `NOT NULL` column with the constant default `order_refund` and a
  CHECK for that value. A constant default adds the column without a table rewrite; a stored
  generated column would rewrite the table under `ACCESS EXCLUSIVE`.
- `outside_completion_kind`, nullable.
- A composite foreign key `(organizer_id, outcome_subject_kind, id, outside_completion_kind)` to the
  outcome's `(organizer_id, subject_kind, subject_id, kind)`. A `NULL` kind skips it, so ordinary
  rows are unaffected. The `kind` column in the key is also what keeps a pending row from needing
  an outcome.
- `order_refunds_completion_evidence`, which admits three states, each with explicit `NULL` tests: a
  pending row with no evidence and no completion time; a completed row with a Payments fact and no
  outside kind; and a completed row with no Payments fact and the kind `refunded_outside`. An
  `abandoned` outcome cannot complete a refund.

The Down locks `order_refunds`, `refund_outcomes`, then `order_refund_abandonment_dispositions`
(§10), and only then refuses while any outcome or any reservation exists. A pending reservation can
be the only local record that Payments released an allowance. An empty Down restores the 0007 rule
under the name `order_refunds_completion_check`.

### 3. `refunded_outside` requires a read-only Payments proof first

Commerce cannot tell from its own pending row whether money moved: Payments can complete a leg
before Commerce records it. So the command asks Payments to prove the failure before it writes:

```
GET /internal/psp/partial-refund/failure-proof?organizer_id=&idempotency_key=&refund_key=
```

- The charge key is the order's stored checkout key. The refund key is the Commerce refund id, which
  Payments requires as a canonical UUID; that excludes exchange downgrade keys.
- Payments reads the leg, its charge and its abandonment marker in one statement. It refuses a
  completed leg or a marked leg (409) before any provider call.
- The provider lookup only lists and reads. `psp.RefundFailureProver` is an interface separate from
  `psp.PSP`; the Stripe adapter implements it with the same listing loop, match rule (`classify`)
  and status mapping (`mapRefundStatus`) that `resolveRefund` uses. The proof scans the whole
  listing. Precedence, in order:
  1. Any stamped refund that does not corroborate (no id, another PaymentIntent, amount or
     currency) makes the proof inconclusive (502), also beside a corroborated match. The adapter
     counts such refunds; it does not rely on their id.
  2. More than one corroborated stamped refund, or a corroborated `succeeded` refund, contradicts
     the failure (409).
  3. One corroborated stamped refund in `failed` or `canceled` is a proof and returns its `re_`
     reference.
  A pending refund, no stamped refund, an incomplete or malformed listing, an unavailable provider,
  and a provider with no prover are inconclusive (502). The adapter reads and validates the whole
  listing before it uses any entry. Every listed entry must be classifiable: a null entry, an entry
  whose `object` is not `refund`, an entry with no id or no `metadata` object, or a
  `compensation_key` that is present but null makes the whole listing inconclusive (Stripe sends
  `metadata: {}` on an unstamped refund, which stays a valid foreign refund). A refund id that
  occurs twice anywhere in the listing, the final page included, makes it inconclusive too: the
  provider did not advance past that id. A page with no entries is inconclusive unless it is the
  first page and also the last one: a later page exists only because the page before it said more
  entries follow, so an empty later page contradicts that page, whatever its own `has_more` says.
  The rule is in the shared listing loop, so `resolveRefund` does not submit on such a listing
  either.
- The fake PSP (offline and smoke stacks) proves only a refund failure that it recorded itself:
  a charge captured with the `fake-refund-fail-once` token fails its first refund terminally and
  keeps that refund in memory. The proof looks that refund up by charge reference and key; the fake
  records at most one refund per key, so the ambiguity cases exist only for Stripe. Every other fake
  charge keeps its stateless refund and stays inconclusive (502). The guarantee holds only for a
  capture whose provider reference Payments persisted: a capture that status recovery confirmed
  without a stored reference takes the stateless refund path. A Payments restart loses the fake's
  memory, so the proof then answers inconclusive. The fake invents no evidence: it is the provider,
  and it proves only its own recorded failure.
- After the provider answers, Payments reads the leg, its charge and its marker again. Any change
  refuses (409).
- Error bodies are static. The provider idempotency key never crosses the boundary.

The CLI requires the proof's amount and currency to equal the refund's. It makes no writing HTTP
call.

### 4. The completion writes no money fact

The owner calls the outcome "not a payments fact", citing ADR-032. The ordinary Commerce completion
appends `order.refunded` to `order_facts` and forwards it to the Payments journal. Under this
reading, an outside completion does neither. **Consumers of the Payments journal or of Commerce
`order_facts` must read `refund_outcomes` for outside refunds.**

### 5. One short transaction in the ordinary lock order

The command inspects the refund in the organizer with no lock, asks Payments with no lock (ADR-037
§4), then writes in one transaction. It locks the refund row and then the order row, the order
`CompleteOrderRefund` uses. Under those locks it rechecks the source reservation's organizer, the
order's charge key, and the refund's order, amount and currency against what it inspected. It
decides replay or conflict against any outcome committed in the meantime, refuses a refund with an
abandonment reservation in either state (§10), refuses a refund that Payments completed, appends the
outcome, sets the outside completion evidence, and advances the order projection with the same
arithmetic as `CompleteOrderRefund` (one shared helper). That helper scopes its `orders` UPDATE to
the organizer through the order's source reservation and requires exactly one updated row, so a
scope miss rolls back the whole completion, for both callers. `CompleteOrderRefund`, the ordinary
`order.refunded` append and fact forwarding are unchanged.

Lock invariant (stated in 0027). Every transaction that touches `refund_outcomes` or the reservation
table takes its relation lock on `order_refunds` first. The command writes lock the scoped refund
row, then its order row, then read or write the two new tables. Ordinary reads and the cancellation
bind read `order_refunds` before the outcome evidence, with no refund row lock. 0027 Up locks only
`order_refunds`; Down locks the three tables in that order. Locking `orders` first could deadlock
with a live completion that holds a refund row and waits for its order. So the rollout needs no
Commerce writer drain. While 0027 Up holds `ACCESS EXCLUSIVE`, it validates the new CHECK and
foreign key over every refund row, so writers queue for that time; at the current table size that
is acceptable.

### 6. Replay, conflict and scope

A missing refund and a refund outside the organizer, on either the refund row or its source
reservation, give the same refusal. An exact repeat (same kind, operator name, external reference
and reason) returns the original record with its original time and attribution, and calls nothing.
Any other repeat is a conflict. A refund that Payments completed is ineligible. All four are
answered from Commerce alone, with no Payments call. An outside completion of a refund with an
abandonment reservation is refused before the proof read, also with no Payments call, and again
under the locks for a reservation that committed while the proof was in flight.

### 7. Attribution is honest-operator evidence (ADR-003, ADR-021)

The command records the typed operator name and PostgreSQL `session_user`, which the database reads
on the writing connection. On the documented access path (`docker compose exec commerce …`),
`session_user` is the shared `commerce` service login. **The typed operator name is the only
personal attribution.** Name the adversary: a writer with Commerce database access can forge or
remove an outcome, disable the triggers, or complete a refund directly, and a coordinated rollback
can erase the record. The record is evidence of what an honest operator attested to. The Payments
proof endpoint is behind the Payments internal credential only and relies on its authorized Commerce
caller for the operator workflow.

### 8. Reversal and bulk use the existing paths (ADR-038, ADR-040, ADR-062)

An outside completion is a `completed` refund, so the reversal reconciler claims it and drives the
tickets and then the capacity, with its ordering guard and retries unchanged. A pending refund is
still never claimed. If the outside transfer completed an earlier cancellation refund, a later run
for the same slot uses the same cancellation key (`cancel:<slot>:<order>`) and replays that refund
without calling Payments. It reports `already_refunded` after the order is fully refunded and all
reversals are complete. After a partial staff refund, a cancellation run can refund the remaining
quantity under the cancellation key and report `refunded`.

### 9. Payments schema for S2

Payments migration 0007 adds the append-only `payment_refund_leg_abandonments` marker, keyed by and
linked to the exact composite leg key, with the provider reference and a database timestamp. The
proof refuses a marked leg, so an outside completion cannot follow an abandonment that Payments has
committed. The Down locks `payment_operations`, then `payment_refund_legs`, then the marker table, and
refuses while a marker exists.

### 10. `abandoned`: reserve in Commerce, then Payments, then finish in Commerce

```
commerce record-refund-outcome <organizer-id> <refund-id> abandoned <operator> <reason>
```

The operator confirms that no outside transfer occurred and none is in flight. The command takes no
reference: Payments supplies the provider's `re_` for the failed refund.

Commerce migration 0027 adds `order_refund_abandonment_dispositions`, one row per
`(organizer_id, refund_id)` with a composite foreign key to `order_refunds`. A row holds its state
(`pending` or `finalized`), the refund's charge key, amount and currency, the operator name and
reason, and the reservation time. A trigger admits only the change from `pending` to `finalized`
with every other column kept, and refuses `DELETE` and `TRUNCATE`. The reservation is in its own
table so that the append-only outcome stays evidence of a completed disposition.

1. **Inspect.** Commerce reads the refund in the organizer with no lock. It answers a missing or
   foreign refund, an exact replay of a finalized abandonment, a conflicting outcome and a
   completed refund from Commerce alone, with no Payments call.
2. **Reserve.** In one short transaction, under the §5 locks and rechecks, Commerce refuses an
   outside outcome (conflict) and a refund that is no longer pending (ineligible). It returns an
   exact finalized replay. It inserts a `pending` reservation, or resumes a `pending` one when the
   operator name and reason match exactly; a mismatch is a conflict. The reservation commits
   **before** any Payments call. If the commit returns an error, the command stops before HTTP, and
   an exact retry finds out whether the row committed.
3. **Call.** With no Commerce lock held, Commerce calls `POST /internal/psp/partial-refund/abandon`
   with the organizer, the reserved charge key, the refund id as the refund key, and the reserved
   amount and currency.
4. **Finish.** After a valid 200 for exactly the reserved money, one short transaction under the
   same locks rechecks the target, the reservation and the refund state, appends the `abandoned`
   outcome with the reserved operator name and reason and the returned `re_`, and sets the
   reservation to `finalized`. Only this commit releases the quantity. If a concurrent run of the
   same command already finalized the reservation, the finish returns that original outcome. The
   refund stays `pending`. The order projection, the tickets, the capacity and the reversal state do
   not change.

From step 2, an outside completion of the refund is refused (§5, §6), so the two dispositions cannot
both commit, also when another order shares the Payments charge. An exact replay (same kind,
operator name and reason) returns the finalized record; the reference is not part of the command.

### 11. The Payments abandonment endpoint and its marker transaction

The endpoint takes the organizer, the charge `idempotency_key`, a canonical UUID `refund_key`, and
the expected `amount` and `currency`. It is behind the Payments internal credential only. It relies
on the authorized Commerce caller for the operator workflow.

1. It reads the leg, its charge and its marker. A missing leg is 404. A completed leg, or a leg whose
   money differs from the request, is 409. A committed marker is answered at once as a 200 replay
   with the stored reference, **without a provider call**, so a caller that lost the first answer can
   always finish, also when the provider is down.
2. For a new marker it asks the provider for the same proof as §3, with no lock held and no
   transaction open. A contradiction is 409; anything inconclusive is 502 and writes nothing.
3. One short transaction locks the `payment_operations` row (the lock `BindRefundLeg` decides the
   ceiling under), then the exact leg. It rechecks that the leg is `bound` and that its money, its
   provider key and the charge's PaymentIntent are the ones the proof used. A change is 409 and
   writes nothing. A marker committed meanwhile with the same reference is a replay; with another
   reference it is 409. Otherwise it inserts the marker.

No database lock spans a provider call (ADR-037 §4 is unchanged).

### 12. The two ceiling exclusions, and what still counts

- **Payments.** `BindRefundLeg`'s captured-money sum excludes a leg with a marker for the same
  organizer, charge key and refund key. Only that sum excludes it. The whole-refund exclusion
  (`BindCompensation`) still counts every leg, marked or not, so a charge with a marked leg cannot
  get a whole refund. The marked leg stays `bound` with its provider key.
- **Commerce.** `BindOrderRefund`'s quantity sum excludes a refund with a matching `abandoned`
  outcome: same organizer, subject kind `order_refund` and subject id. An outside completion and an
  ordinary pending refund still count. A pending reservation alone releases nothing. The release
  happens when the finish (§10) commits the outcome.

This is the only exception to ADR-037 §4. Payments releases the amount when its abandonment marker
commits. Commerce releases the quantity when its `abandoned` outcome commits. Between those commits,
Commerce still counts the quantity. Both ceilings continue to count a `refunded_outside` refund.

### 13. Dispatch refuses an abandoned leg and never records a Refunded answer for one

In `/internal/psp/partial-refund`, after the existing bind and completed-replay check, Payments reads
the marker in its own short statement immediately before `psp.Refund`. A marked leg answers the
existing 409 (`refund leg abandoned`) with no provider call. A read error fails closed (500).

The guarantee has two halves:

- **A dispatch that reads the marker after it commits makes zero provider calls.**
- **A dispatch admitted before the commit can make one POST** under the leg's stored provider key.
  The Stripe adapter lists before it submits, and nothing spans that listing and the POST, so a
  marker can commit between them. The POST is the same request as the original: the stored key as
  both the `Idempotency-Key` header and the `compensation_key` stamp, with the same amount and
  PaymentIntent. The provider deduplicates it within its key retention (ADR-032 §Refund), and no
  other key can create this leg's refund.

**A Refunded answer on an abandoned leg is never recorded.** After the provider answers Refunded,
`CompleteRefundLeg` runs ONE short transaction. It takes the locks the abandonment's marker
transaction takes (§11), in the same order: the `payment_operations` row, then the leg row, both
scoped by organizer. Under those locks it reads the marker in a separate statement. On a marker it
appends no fact, completes nothing, and dispatch answers the same 409 and logs both refund
references, the marker's and the answer's. A Refunded answer whose confirmed money differs from the
leg, carries none, or fails the port's validation (for example an amount with no currency) reads the
marker under the same locks before it is refused: on a marker the answer is the same 409 with both
references in the log; without one it is the existing refusal, the money-mismatch 502 or the
invalid-result 500. Without a marker, the `payment.refunded` journal fact and
the leg completion commit in that same transaction. The marker's foreign key to the leg row cannot be
satisfied while that row is locked, so no marker commits between the read and the commit. No lock is
held across the provider call. A completed replay keeps its existing answer. A succeeded refund on a
leg proven failed is a reconciliation alarm; a different reference is the stronger one.

A marker read in its own statement, followed by a separate append and completion, was rejected: a
marker that commits between those statements still lets an append-only fact through.

**Money under the abandoned key is not promised never to move.** The marker records that the platform
will not refund that attempt again and that its answer is not recorded. It does not stop a POST that
was already admitted. A lease that fenced admission was rejected: it needs a new table and
operator-facing refusals, and it still relies on provider idempotency, because a frozen process
resumes and POSTs under whatever lease it holds. **Accepted residual risk:** a process that stalls
between the listing and the POST for longer than the provider's 24 h key retention could create a new
refund under the stored key. Nothing records it. Reconciliation finds it from the stamp.

**The same window applies to `refunded_outside`.** The outside path writes no Payments marker: its
proof (§3) only reads. A dispatch admitted before the proof can still send one POST under the stored
key after Commerce records the outside outcome. Within the provider's key retention, provider
deduplication stops that POST from creating a second refund: Stripe answers it with the original
response body. That body is not current evidence. It can say `succeeded` for the refund that the
proof found failed. **The local journal records only a success that the current provider state
confirms.** The Stripe adapter re-reads a POST that answered `succeeded`, by its refund id, before it
returns Refunded. The retrieved refund decides the result and carries the confirmed money. A refund
that is now `failed` or `canceled` is the terminal 422, a `pending` one is unresolved (502), and a
failed re-read or a different refund is unresolved too. In each case Payments records nothing and the
leg stays bound. The re-read is one more provider GET on every successful refund POST, the ordinary
path included; it adds no new state and holds no database lock. **Remaining in-flight risk:** the
re-read and the journal transaction are separate steps, and no lock spans a provider call. A refund
that is `succeeded` at the re-read and fails before the journal commits is recorded as refunded; the
detection below finds it. That the refund retrieve and the refund listing show the same state at the
same time is not verified live. A stall past
the 24 h retention can create a new refund. Because the leg has no marker, Payments can then journal
`payment.refunded` and complete the leg, and an in-flight Commerce request can append and forward
`order.refunded` before `CompleteOrderRefund` finds the refund already completed. This is the
same accepted residual risk as for `abandoned`, for the same reason: no database mechanism stops a
frozen process from sending its POST, a lease only moves the window, and the stored key is the real
fence. A Payments marker and lease for the outside path were rejected. **Detection:** the
order-recovery runbook lists the leg's provider refunds by its compensation stamp. A `succeeded`
refund with that stamp on a refund closed with either outcome, or a `payment.refunded` fact on its
leg, is an invariant violation that an operator reconciles by hand.

`TestPartialRefundDispatchRefusesAnAbandonedLeg` pins the 409 with no provider call.
`TestPartialRefundReplaySendsTheStoredKeyAsHeaderAndStamp` pins the identical replay request.
`TestPartialRefundDispatchRecordsNothingWhenTheMarkerCommitsDuringTheProviderCall` commits the marker
inside the provider call and pins no fact, no completion and the 409.
`TestPartialRefundDispatchRecordsNothingWhenTheMarkerCommitsAfterTheProviderAnswer` opens the marker
transaction during the provider call and commits it after the Refunded answer, while dispatch waits
for the operation lock: 409, the leg still bound, zero refund facts.
`TestAbandonIsRefusedWhenDispatchCommitsFirst` pins the reverse order: dispatch commits one fact and
the completion, and the abandonment then refuses a completed leg with no marker.
`TestPartialRefundLateMarkerWinsOverAConfirmedMoneyMismatch` pins the 409, both references in the
log, the bound leg and zero refund facts for a different or absent confirmed amount and for an amount
with no currency, with the marker committed during the provider call and with the marker transaction
still holding the locks. `TestPartialRefundRefusesAnInvalidConfirmationWithoutAMarker` pins the
unchanged 500 without a marker.
`TestPartialRefundDispatchRecordsNothingWhenTheCachedSuccessIsStale` lets the failure proof find the
leg's refund failed while the POST is in flight, then answers the POST with the cached `succeeded`
body: the 422 for the current failed refund, no fact, the leg still bound.

### 14. Recovery, and the cost of starting an abandonment

This section applies after the command has reserved the abandonment. A configuration refusal comes
before the database is opened (Decision introduction), so it creates no reservation and needs no
recovery. A credential that passes the local checks and that Payments still refuses is a non-200
answer below.

Any non-200 answer, a transport failure, unusable evidence or a failed Commerce finish leaves the
reservation `pending`, unless another run already finalized it. A 404 or 409 describes one abandon
request: an overlapping request for the same leg can still commit the marker. The Payments marker
can exist even on the first attempt. A redirect leaves the outcome of the initial request unknown.
The next run uses the same command. Payments answers from a committed marker without asking the provider (§11), and
Commerce finishes once. While the reservation is pending and Payments has no marker, both allowances
stay reserved. After a marker and before the finish, Payments has released its allowance and
Commerce still counts the quantity: the stricter ceiling.

An operator who starts an abandonment commits to that disposition for the refund. An outside
completion is refused from then on. If the refusal clears, the operator finishes the abandonment
with the original operator name and reason, and staff submit a fresh refund where appropriate. A
resume attributes the outcome to the operator who started it. A pending reservation on a refund
that an ordinary completion has since completed is inert. A pending reservation whose proof never
becomes conclusive, such as an unstamped leg, stays pending. An outside outcome together with a
marker on the same leg is an invariant violation: stop further money actions and investigate.

### 15. Bulk runs and abandoned refunds (owner decision of 2026-10-12)

`LookupRefundByID` (and `BindOrderRefund`'s replay) carry the abandoned flag, scoped to the refund's
own organizer. The bulk runner checks it in **both** quantity branches, before it uses a fixed
quantity and before it calls the refund unit. It makes no Payments call. It classifies from the order
projection and the reversal state, which the abandonment did not change:

- Remaining quantity: `failed` / `refund_refused`, terminal.
- Zero remaining quantity: the existing reversal precedence. `already_refunded` when every reversal
  obligation is discharged; `reversal_outstanding` when one remains. 0012's success CHECK forbids a
  success with an obligation outstanding, so the owner's "zero means already refunded" is read
  through that CHECK.

If the abandonment commits between the runner's lookup and the Commerce bind, the shared refund unit
refuses (`refunds.ErrRefundAbandoned`) before any Payments call, and the runner applies the same
classification. If it commits after the bind, Payments' marker has committed first and dispatch
answers its generic 409. The runner then reads its own refund again, scoped to the organizer, and
when the abandonment has committed it applies the same classification with no further Payments call.
A staff replacement can complete before the runner handles that 409, so the zero-quantity verdicts
apply there too. Any other Payments refusal keeps `refund_refused`.

**A failed re-read after a Payments refusal is not a verdict.** The runner cannot tell an abandonment
from any other refusal without that read, so it records a retryable `internal` failure: the pass is
charged one attempt, and the lease is the backoff. The next pass resolves the row again. If the
abandonment has committed, it skips with no Payments call. If not, it calls Payments again under the
same key, which the provider deduplicates, and a refusal with a successful read is then
`refund_refused`. So a failing read can use more of the row's attempt budget and cause one more
same-key Payments call per pass. `TestBulkRunRetriesWhenTheReadAfterAPaymentsRefusalFails` fails only
that read and pins the charge and both retry outcomes.

**A fresh cancellation bind on an order with an abandoned refund is refused.** A runner requests the
sold quantity minus the completed quantity, and a later staff refund does not say which abandoned
units it replaced. So `BindOrderRefund` refuses a new key in the reserved `cancel:` namespace when any
refund of the scoped order has an `abandoned` outcome, whoever bound it, also after staff completed a
replacement. The check runs under the order lock, before the quantity ceiling, and reads
`order_refunds` first. The finish (§10) holds the same order lock, so the bind sees an abandonment
entirely or not at all. A pending reservation alone does not trigger it. The runner classifies the
refusal like the own-key skip: `refund_refused` at positive remaining quantity, and at zero
`already_refunded` after discharge or `reversal_outstanding` while an obligation remains. It makes
zero Payments calls. Valid same-key replays keep their behavior. Staff own any further refund of the
order. The `(slot, order)` key, attempt charges and terminal report snapshots are unchanged.

### 16. Staff replacement through the Back Office (owner decision of 2026-10-12)

After a recorded `abandoned` outcome, staff can submit a new refund under a new key through the Back
Office. The platform never re-submits automatically, and no operator command re-submits.

A valid staff replay of the abandoned refund's key answers Commerce 409 with `code: refund_abandoned`
and sends nothing to Payments. "Abandoned" here means a finalized `abandoned` outcome: while a
reservation is only pending, a staff replay still reaches Payments, so an ordinary completion can
still settle the refund and leave the reservation inert. A mismatched fingerprint and an ordinary Payments 409 stay generic
409s with no code. The handwritten Back Office client (`web/backoffice/src/lib/commerce.ts`) reads the
code only on a 409. `unresolvedRefund` (`order-console.ts`) then settles the saved retry, and the page
clears only the saved retry with the same organizer, order and key, and renders a blank refund form
with a fresh key. A generic refusal and a different saved key keep the existing retry behaviour.

The saved retry belongs to the order, not to the staff member who submitted it, and it keeps the
original request: Commerce fingerprints the key over the actor too. So another staff member's retry of
that key is a generic 409, not `refund_abandoned`. For this case the staff order detail
(`GET /internal/orders/{id}`, scoped by the session's organizer assertion) reports `abandoned` per
refund: true only for a committed `abandoned` outcome, never for a pending reservation. Whenever the
page holds a saved retry, it reads that detail and retires the retry only when the detail names the
same organizer and order and an abandoned refund under the same key. The organizer and order compare
as UUIDs, case-insensitively: the form accepts an uppercase UUID and Commerce answers in lowercase.
The key compares exactly. Any other answer, including a failed read and an answer from an older
Commerce without the `abandoned` flag, keeps the retry. The next form has a fresh key, and the replacement carries the current
session's identity.

## Consequences

- An operator can close a failed staff or bulk refund that was refunded outside the platform. Both
  allowances stay consumed and the tickets are voided before the capacity returns.
- The proof uses only the platform's stamped refund as evidence. Every platform refund for a leg
  is stamped, and Payments sets the stamp before it sends the request, so a lost reply can leave
  a stamped refund. An unstamped refund on the charge came from outside the platform. It can be
  the operator's own outside transfer through the PSP, or a refund that already returned the
  money. Check for it before a same-key replay or an outside transfer
  ([runbook](../runbooks/order-recovery.md#resolve-a-lost-provider-reply)). Both allowances stay
  reserved while the leg is unresolved and unmarked.
- Stripe documents refund listing filtered by PaymentIntent and the `failed` and `canceled`
  statuses. That historical failed and canceled refunds stay visible in that listing under the
  deployed API version is not verified live; the recorded fixtures assume it.
- Rollout: run the Payments migration and replace every old Payments process, then run the Commerce
  migration and update Commerce. Enable the command only when both services are current. The
  `migrate` subcommand has a 30-second deadline (ADR-008). If a migration fails, stop the rollout and
  correct the cause. Rerun the failed one-shot migration job with the new release's image against the
  existing service database. In that database, run
  `SELECT max(version_id) FROM goose_db_version WHERE is_applied`. Continue only after the job
  succeeds and the result matches the latest migration version in the new image.
- Roll forward, not back, once either migration holds outcome or marker history. Both Downs refuse.
- The quantity and money reports that read only `order_facts` or the Payments journal do not see an
  outside refund.
- An operator can close a failed refund that nobody will refund. Each service releases only that
  refund's allowance, and staff can refund again under a new key. The abandoned row stays `pending`
  and its leg stays `bound`: both are history, not work.
- A lost answer after Payments committed its marker leaves Payments' allowance released and
  Commerce's quantity reserved until the same command is run again. Find such a case with the
  runbook's query: a pending reservation with a marker and no Commerce outcome.
- Like the outcome, the marker and the reservation are honest-operator evidence (ADR-003, ADR-021),
  not tamper-evidence. The shared `commerce` login is session provenance; the typed name is the
  personal attribution.
- Checkout accepts the synthetic `exchange:<id>` key spelling, so two Commerce orders with their own
  quantity ceilings can store the same Payments charge key. The reservation keeps an outside
  completion and an abandonment of one refund from both committing, so this alias cannot release a
  captured-money allowance that another order then spends. The alias itself is pre-existing and is
  deferred to TKT-586.

## Verification

The rules above are pinned by the service suites under `services/commerce/internal/` (`store`,
`refundoutcome`, `bulkrefund`, `api`) and `services/payments/internal/` (`psp`, `api`, `store`), the
CLI test in `services/commerce/cmd/commerce/`, the running-stack journeys in
`smoke/refund_outcome_test.go`, the Back Office tests in `web/backoffice/test/` and the browser spec
`test/browser/refund-abandoned.mjs`. The cross-service recovery and shared-charge cases run the
Commerce coordinator against the smoke stack's real Payments
(`services/commerce/internal/refundoutcome/realpayments_smoke_test.go`). The TKT-525 ticket records
the mutation evidence for each guard.

## References

- [ADR-003](ADR-003-append-only-audit-trail.md), [ADR-021](ADR-021-ticket-lifecycle-trail-integrity.md)
- [ADR-032](ADR-032-stripe-behind-the-psp-port.md), [ADR-037](ADR-037-post-purchase-refund-money-protocol.md)
- [ADR-038](ADR-038-refund-reversal-ticket-voiding.md), [ADR-040](ADR-040-event-cancellation-bulk-refund-runs.md),
  [ADR-062](ADR-062-refund-reversal-reconciliation.md), [ADR-071](ADR-071-internal-mutation-ordering-honest-caller.md)
- [Order recovery runbook](../runbooks/order-recovery.md) §Failed post-purchase refund: operator outcome
