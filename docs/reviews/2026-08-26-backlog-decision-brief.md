# Backlog decision brief — 2026-08-26

**STATUS: all twelve decisions taken by the owner on 2026-08-26.** This document is now the record of
what was decided and why, not a list of open questions. Each decision is also recorded as a
`kind=decision` comment on its ticket, which is what a shaping or planning run should read.

Compiled from a full verification pass over all 36 non-epic backlog tickets at HEAD `1aaee7cd`; every
fact below was read from the code, not inherited from ticket text.

## The decisions, in one table

| # | Ticket | Decision |
|---|---|---|
| 1 | TKT-155 | Price-resolution moves behind the **internal credential**, matching `resolveTicketTypeFees` |
| 2 | TKT-176 | **Refuse** channel allocations on seated pools; seat-set allocations are a future ticket |
| 3 | TKT-169 | Refuse exchange of an admitted **single-admission** ticket, pre-money; **passes carved out** |
| 4 | TKT-201 | A **separate** staff-scoped order read: money **yes**, buyer contact **no** |
| 5 | TKT-203 | Resend to the address **on file only**; operator-typed address out of scope |
| 6 | TKT-273 | **Keep transaction-start time.** Deliverable is an ADR-024 amendment, zero code |
| 7 | TKT-253 | **Loose sanity bound** on `STRIPE_SECRET_KEY`, plus fix the `runtimecfg` gap |
| 8 | TKT-271 | Staleness ceiling is **24 hours** |
| 9 | TKT-141 | **Demote the two list reads**; by-id geometry stays hours-tier. **After TKT-209** |
| 10 | TKT-171 | Comped reversal is a **distinct void action**, not a zero-amount refund |
| 11 | TKT-276 | **Ship the listing command now**; bound the overlap in a follow-up |
| 12 | TKT-234 | **Split into three** — and a `[high]` blocking finding may **no longer be agent-overridden** |

### Three follow-up tickets to file at shaping

1. **Multi-entry pass exchange semantics** (from TKT-169) — may a partially-used pass be exchanged, and does the entry history follow it?
2. **Bound the reseller credential overlap** (from TKT-276) — cap, supersede, or accept-and-document. Auth surface, so it needs an owner answer.
3. **The override rule change** (from TKT-234) — a `[high]` blocking finding escalates to the owner on an autonomous run. Process change to the SDLC skill and `AGENTS.md`.

### One sequencing constraint

**TKT-141 is blocked by TKT-209** (in Ready). TKT-209 replaces catalog's free-form `CacheControl`
with a closed enum; TKT-141 then amends that enum. In the other order, TKT-209 absorbs a tier change
mid-flight.

---

## The reasoning behind each

What follows is the analysis the decisions were taken from — the options, the in-repo precedent, and
the recommendation. Kept because the *why* is what a future reader needs when the decision looks
wrong to them.

---

## 1. TKT-155 — May the public read expose forward pricing intent?

`GET /ticket-types/{ticketTypeId}/price-resolution` is declared, non-`/internal/`, and explicitly
`security: []` (`services/catalog/api/openapi.yaml:932-936`), so the gateway proxies it to anyone.
Its `candidates` array reports every considered rule — `rule_id`, `scope_level`, `scope_id`,
`amount`, `priority`, `forced` — including rules that lost with `outside_window_future`. An
organizer's unannounced future prices and the whole rule ladder are readable before they go on sale.

**Precedent, which the ticket predates.** The fee sibling already took this decision the other way:
`resolveTicketTypeFees` is `/internal/` for exactly this reason
(`services/catalog/api/openapi.yaml:1076-1093`). TKT-237 also partially mitigated the price endpoint
on the same reasoning — different-channel rules are now omitted *"because this operation is public"*
(`:981-987`) — without closing the ticket. A live assertion pins the concern at
`services/catalog/internal/store/pricing_test.go:795,831`.

**Recommendation: require the internal credential, matching `resolveTicketTypeFees`.** This is not
a v1-auth complaint — the whole catalog API is open in v1 — it is that this endpoint publishes a new
*category* of data. Commerce already holds the credential.

**The question:** confirm we do here what the fee endpoint already does, or state that forward
pricing intent is acceptably public.

---

## 2. TKT-176 — Should reserved seating support channel allocations at all?

A seated pool can carry channel allocations today — `ReplaceChannelAllocations` never checks
`inventory_kind` (`services/inventory/internal/store/channel_allocations.go:173,199`) — and the
public availability read subtracts them (`store.go:825-828`), but `CreateSeatHold` admits against
capacity alone and `SeatHoldCreate` has no `channel` field at all
(`seat_claims.go:685-691`; `services/inventory/api/openapi.yaml:583-597`). The read and the claim
disagree.

**The two branches differ by an order of magnitude:**

| | Work | Shape |
|---|---|---|
| **No** | A refusal plus a test | `ReplaceChannelAllocations` rejects a seated pool |
| **Yes** | An ADR plus two contract changes | On a seated map an allocation wants to name *which seats*, not a quantity — a quantity cap is probably the wrong shape |

**Urgency is genuinely low and the trigger is precise:** the state is unreachable through the API
(only an operator writing allocations directly creates it), and the trigger is *before any seated
event is sold through a reseller channel*.

**Recommendation: answer "no" for now** — refuse the combination explicitly, and let the seat-set
allocation shape be its own ticket when a reseller actually needs seated inventory. That converts a
silent divergence into a legible refusal at a cost of roughly one test.

**Note:** `TestASeatedSaleCannotCarryAChannelAtAll`
(`services/commerce/internal/api/channel_seam_test.go:279`) is a live tripwire stating the current
contract. Whichever way this goes, it must be consciously updated, never deleted to make a change pass.

---

## 3. TKT-169 — May a used ticket be exchanged?

There is a reachable double-admission: exchange, scan, then switch. The refusal shipped
(`ErrSourceTicketsAlreadyAdmitted`, `services/access/internal/store/exchanges.go:63`) but ADR-039 §2
carries an explicit qualification saying TKT-169 owns the real decision (`:76-81`).

**"Used" is not binary for a pass, and that is the hard part.** One `entry` on a 5-day festival pass
should probably not block exchanging the remaining days, but one entry on a single-admission ticket
plainly should.

**Options:** refuse pre-money · carry the entry forward to the target ticket · accept and reconcile.

**Recommendation: refuse pre-money for single-admission, and decide multi-entry separately** rather
than letting the pass case hold up the simple one. Engineering is small once the rule is chosen.

**Check at shaping:** ADR-067 (wedged-exchange operator unwind) postdates the ticket and may already
cover its "stranded settled-but-unswitched exchange" AC.

---

## 4. TKT-201 + TKT-203 — Two back-office reads/writes that expose buyer PII

These are separate tickets but one decision area, and the back office holds no internal credential
by design (ADR-042).

**TKT-201 — order detail read.** `OrderState` is `{order_id, status}` with
`additionalProperties: false` (`services/commerce/api/openapi.yaml:1013-1019`); ADR-028 fails closed
so the handler cannot return more. There is no read anywhere for line items or money totals, and
buyer contact exists only at `/internal/buyers/{id}/delivery-email`, which the gateway denies.
Two live modules are waiting on it: `web/backoffice/src/lib/unresolved-refunds.ts:20-25` carries a
self-declared expiry naming TKT-201.

  - **Q1:** one read or two — widen `OrderState`, or add a separate staff detail read?
  - **Q2:** may staff see buyer contact, and which caller specifically?
  - **Recommendation:** a separate staff-scoped read (widening `OrderState` changes a contract the
    storefront also consumes), money included, buyer contact **excluded** until there is a named use
    case. ADR-001 binds the money half either way: integer minor units, no float.

**TKT-203 — re-deliver an order's tickets.** No resend exists anywhere; delivery is a side effect of
consuming the issuance event (`services/access/internal/consumer/consumer.go:403-425`), and the
query filters on `NOT EXISTS (… event_type='delivered')` (`store/postgres.go:141`), so replaying
delivers nothing.

  - **Q3, and it is the whole ticket:** on-file address only, or an address the staff member types?
    On-file is safe and often useless — the customer is calling *because* they cannot receive it.
    Operator-typed turns the back office into a mail relay emitting ticket capabilities to arbitrary
    recipients.
  - **Recommendation: on-file only in v1.** The fraud surface of arbitrary-recipient send is not
    worth it before there is an audited operator identity on the action. A resend re-emits a guest
    retrieval capability (ADR-012) that nothing revokes, so each send widens the holder set
    permanently.
  - **Q4:** back office, or storefront self-service?

---

## 5. TKT-273 — Should a hold whose TTL lapsed during the lock wait still be judged live?

`Claim.expired()` compares against **transaction-start** time (`snapshotTime`), and every one of
these transactions takes the pool row lock before reading (ADR-010), so the lock wait sits between
the timestamp and the decision. A hold that died 1s ago is returned alive on a finalize that waited
2s behind an on-sale (`services/inventory/internal/store/store.go:167-176`, call sites at `:476`,
`:694`, `seat_claims.go:719`, `:1594`).

**The trade is squarely yours.** Moving to `clock_timestamp()` **kills more in-flight holds** — a
buyer completing a purchase whose finalize queued behind contention loses it. That is the opposite
direction from TKT-148, which deliberately lengthened effective holds.

**Recommendation: keep transaction-start time, and record why.** A buyer who queued behind an
on-sale and loses their hold at the moment of payment is a worse outcome than a hold living a few
hundred milliseconds past its TTL, and the oversell risk is bounded by the pool lock regardless.

**The deliverable may be zero code** — an ADR-024 amendment recording the position is a complete
ticket. `TestTransitionKeepsTransactionStartClockDeliberately`
(`buyer_ttl_clock_smoke_test.go:541`) already pins this and carries a do-not-delete instruction.

---

## 6. TKT-253 — How much shape should we assert on a Stripe secret key?

`pspForKey` branches on `strings.HasPrefix(key, "sk_test_")` with nothing after it
(`services/payments/cmd/payments/main.go:147`), so `sk_test_x` selects the real Stripe adapter and
the service starts cleanly; the error surfaces at the first charge. `main.go:330` still reads the
variable with a bare `os.Getenv`, so TKT-252's `runtimecfg` floor does not reach it.

**The trap the ticket states correctly:** a bound we invent about a credential Stripe issues and we
do not control can refuse a working deployment, which is worse than today's failure.

**Recommendation (carried from the 2026-08-25 pass, still right): the loose sanity bound.** A
non-empty, whitespace-free body after the prefix catches `sk_test_x`, a truncated paste and a
shell-mangled value — the failures actually observed — while asserting nothing about Stripe's
alphabet, length or future format.

**The question:** loose sanity bound (recommended) · strict format assertion · record as accepted
and amend ADR-032.

---

## 7. TKT-271 — What is the revocation-feed staleness ceiling?

"Generous" was decided on 2026-08-22; the number was not, and the AC cannot be written without it.
Hours rather than minutes is implied but not stated.

**Three sub-questions, all yours** (this decides who gets through a door):
  - **The ceiling number** — hours or days.
  - **Whether a refused-on-revocation scan is recorded differently** from an ordinary refusal, and where.
  - **The override's shape at the device** — it was decided that one exists and is recorded, not what it looks like.

**Recommendation: pick a number first and let the other two follow.** The ceiling is what makes the
ticket plannable; the recording and override questions can be shaped once there is an AC that can
assert something. TKT-162's feed exists and is mounted
(`services/access/internal/api/server.go:127`), so nothing else blocks this.

---

## 8. TKT-261 — How is the access lifecycle smoke writer quiesced?

The smoke block backs up, corrupts and restores three tables while a live in-container writer
appends to one of them — the same race TKT-254 fixed for payments. But the payments fix does not
transfer: there is **no `compose stop access`** in the block (payments stops commerce at
`scripts/smoke.sh:393` and waits on a drain barrier), `Checkpointer.Run` calls `c.Once(ctx)` before
its first tick (`lifecyclejob/runner.go:87-88`) so the window does not require waiting out the 60s
interval, and both the checkpointer and `AlarmDrainer` are mounted unconditionally
(`services/access/cmd/access/main.go:287-289`) with no existing disable path.

**Held because it is CI/deploy config** — one of the four surfaces where `deferred` is not available.
An env-gated pause is not a config tweak; it means adding a disable path to a binary.

**Recommendation: split it.** Ship *(i) make the corruption observable* — assert the restore actually
restored, so a recurrence reports itself — independently of *(ii) the quiescing mechanism*. (i) needs
no decision and closes the silent-failure half today.

**Reminder for whichever mechanism wins:** TKT-254's barrier predicate must be NULL-safe
(`state IS DISTINCT FROM 'idle'`) and scoped to `backend_type='client backend'`, or the guard fails
open on an unknown backend and fires on autovacuum.

---

## 9. TKT-141 — Which cache tier for seat-map *list* reads, and who owns invalidation?

An all-published seat-map list is hours-tier (`cacheControlForSeatMaps`,
`services/catalog/internal/api/server.go:79-88`), so a seat map authored a minute later stays
invisible for up to an hour — the same "an authoring write looks lost" failure TKT-107 was filed for.
TKT-107 fixed content staleness and deliberately left *membership* staleness, documenting it in
ADR-004 (`:214-219`).

**Options:** (a) demote both list reads to a shorter tier · (b) keep the tier and make invalidation
TKT-31's problem — *only credible with an invalidation mechanism that does not exist*.

**Recommendation: (a), demote the two list reads.** A single published version genuinely is immutable
(ADR-029) and can stay hours-tier by id; list *membership* is not immutable and should not claim to
be. (b) defers a real bug behind an unwritten mechanism.

**Not urgent:** there is still no CDN or shared cache anywhere in the stack, so nothing observes this
today. **Sequencing note:** TKT-209 (now in Ready) pins catalog's existing tiers into a closed enum.
It does not re-decide any tier, so they do not conflict — but if (a) is chosen it lands as an
amendment on top of TKT-209's enum, so doing TKT-209 first is the cheaper order.

---

## 10. TKT-171 — May a comped order be "refunded" on its own?

A comped (zero-price) order survives event cancellation: its tickets still admit and its seat stays
sold, because the whole reversal hangs off a refund and `BindOrderRefund` refuses `unit <= 0`
(`services/commerce/internal/store/refunds.go:34,166`) while `unit_amount` permits `0`
(`migrations/0001_checkout.sql:5`). The cancellation run reports these as
`failed/no_captured_money`, so they are visible today — nothing repairs them.

**Everything else in this ticket is plannable engineering.** The single blocked item is AC5, scoped
to the staff path: may a comped order be reversed on its own, outside a cancellation?

**Recommendation: yes, as a distinct "void" action, not a refund.** ADR-003 binds it either way —
the journal records what happened, not what didn't — so a comped reversal must void tickets and
return capacity while making no zero-amount provider call and fabricating no money fact.

---

## 11. TKT-276 — Bound reseller credential overlap: cap, supersede, or accept?

Repeated enrolment mints unboundedly many live credentials for one partner, and
`ListResellerCredentials` exists in the store
(`services/commerce/internal/store/reseller_credentials.go:143`) with no caller — so an operator
cannot enumerate them. `revoke-reseller` takes a credential id an operator has no supported way to
obtain after the one-time token print.

**Recommendation: split, and ship the listing command first.** The CLI verb over an existing store
method is plumbing with no decision in it, and it is the half that unblocks the operator today. The
bound (cap · supersede-on-enrol · accept and document) is an auth-surface policy call and can follow
with your answer.

---

## 12. TKT-234 — A governance question, not a technical one

Carved out of TKT-230's third adversarial review pass. Two findings, both rated [high], both refused
as changes to that ticket — and, in the ticket's own words:

> **An agent overrode a reviewer's "do not ship" twice to get TKT-230 merged** — a human should
> decide whether that was right.

No plan can settle that. The two technical items underneath it are both still live and confirmed:
wall-clock time is the primary sort key of the append-only trails
(`capacity.go:132`, `operational.go:369`, `lifecycle_checkpoint.go:317`), and `claim_history` has no
guarded restore path (`migrations/0012_claim_history_append_order.sql:111-120`).

**Recommendation: split into three** — an ADR on what the trails guarantee (ADR-021's rule applies
directly: name the adversary before writing "tamper-evident"), a restore-path design, and the
process question, which is yours alone.

---

## Also worth your attention — not a decision, a finding

**TKT-195 is mis-tracked.** The mechanism it describes **shipped** on 2026-08-10 through the security
sweep (`services/catalog/internal/api/ratelimit.go`, dual subject + source limiters at `:68-75`,
named in `server.go:114`), not through the ticket — so the board never learned. What is genuinely
still owed is the second half of its own title: **abuse telemetry** (the file contains zero metric,
counter or log statements) and **ADR reconciliation**, since three ADR clauses still say the limiter
does not exist.

**TKT-177's delegation failed silently.** It was delegated to TKT-31; TKT-31 is Done; TKT-31 cached
*availability* and left *seat-occupancy* uncached (`services/inventory/internal/api/server.go:534-553`).
A status-only read of the board says this is handled. It is not.

**TKT-145 has grown a correctness defect** that its own constraint forbids, independent of its product
question: the replay branch answers **409** for a parked `release_pending` order
(`services/commerce/internal/api/server.go:1594-1603`) while `answerRecovered` still answers **202**
for the identical state (`:1417-1424`, which never reads `recovery_parked_at`). The desired answer is
already settled in the sibling path, so this half needs no decision and should be split out and shipped.
