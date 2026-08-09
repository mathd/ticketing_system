# ADR-052: Undoing a guest-order claim

Date: 2026-08-09

## Status

Accepted (TKT-225). Amends [ADR-049](./ADR-049-customer-identity-and-storefront-sessions.md)
§ TKT-223, which named this gap and left it open.

## Context

[ADR-049](./ADR-049-customer-identity-and-storefront-sessions.md) § TKT-223 describes the claim it
shipped in its own words:

> A claim is destructive, exclusive, permanent and unrecoverable. The first holder to claim takes
> the order away from its buyer, and there is no un-claim path — no endpoint, no CLI, no support
> tool. The buyer's recourse is nothing.

The proof of ownership is the order reference **alone**, and that was deliberate: requiring the
checkout email to match would refuse a buyer who later signed up with a different address, which is
common and unfixable by them. The cost of that trade is that anyone holding a reference can take an
order permanently — and references **do** leak. The gateway logs them via the URL path
([TKT-202](../../.sdlc/), still open), which ADR-049 records as the cause rather than a coincidence.

Until this decision the only remedy was a manual `UPDATE` against the commerce database: no audit
trail, no refusal for a malformed request, and the same hand that fixes a mistake can make one.

## Decision

### 1. A commerce-internal operation, `POST /internal/orders/{id}/unclaim`

It restores `orders.customer_id` to `NULL` for a **completed, currently attributed** order, and
records who did it and why, in one transaction.

**It never repoints.** The statement sets a literal `NULL`; no caller-supplied customer can reach
that column through it. Detaching and re-attributing in one step would be a **transfer**, which has
a different adversary and belongs to TKT-9/TKT-160.

### 2. The guard is the shared internal token, compared inline

Per [ADR-043](./ADR-043-where-a-service-auth-guard-lives.md): `security:` schemes guard public
contract operations, and inline token checks guard `/internal/`. The gateway edge-denies
`/api/commerce/internal/` by construction, and a smoke test asserts that for this route specifically.

**Not `staffOrInternal`**, even though the back office holds a commerce credential. That credential
currently opens exactly one internal operation — the refund (TKT-194) — and
`staff_credential_test.go` enumerates every internal route to keep that list deliberate. This slice
ships no back-office surface, so accepting the staff credential would widen what the credential
reaches for a form that does not exist. Whoever adds that form moves the un-claim to the accepting
side of the enumeration and argues for it then. Widening a credential later is easy; narrowing one
after something depends on it is not.

The consequence, stated: **the operator is someone with the service credential**, not a support
agent with a login. That is a real limitation of this slice, not an oversight.

### 3. Every detach is recorded — and what that record is worth

`order_attribution_detachments` stores the order, the customer it was taken from, a free-text
`reason`, an `actor`, and a timestamp. The audit insert and the attribution update **commit
together**: a detach with no record is the untraceable purchase-mover this operation must not be,
and a record with no detach is a false accusation.

`reason` and `actor` are `NOT NULL` **and** `CHECK (btrim(...) <> '')`. An empty answer to "who did
this and why" is a failed record, not a permitted one, and enforcing it in the schema means a `psql`
session cannot write a blank one either.

**Name the adversary ([ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md)).** These rows live
in the same database as the attribution they describe. Anyone who can write that database can write,
alter or delete them. This is **accountability against a careless operator and against an
application path that forgets — not tamper evidence against a hostile database writer.** The
access service's lifecycle hash chain is the shape that resists the second adversary; it protects
access's own domain and was rejected here as cross-domain. **Do not describe this table as
tamper-evident.**

`actor` is an **operator-supplied label, not an authenticated identity.** The internal token
authenticates the caller as "something holding the service credential" and carries no individual.
When the back-office surface arrives and can derive the actor from a staff session, the column
starts meaning what its name suggests.

### 4. A detached order is immediately re-claimable

TKT-225's acceptance criteria asked whether an un-claimed order should be protected from immediate
re-claim by the same actor. **It is not, deliberately.**

The reasoning, and it turns on who the operation is actually for:

- **The most likely detach is support fixing a mistake** — the wrong account claimed an order, or a
  buyer claimed with the wrong one of their own accounts. Blocking re-claim would block **the
  rightful buyer**, recreating the "no recourse" problem this decision exists to solve, one level up.
- **A block would not stop the attacker it targets.** Registration is public and unauthenticated
  (ADR-049), so anyone blocked at the account level registers another account and claims again. The
  block reliably stops only the honest re-claimer.
- **What does bound an attacker** is [ADR-051](./ADR-051-public-customer-surface-rate-limiting.md),
  which rate-limits `/orders/claim` per subject and per source, and fixing the leak itself (TKT-202).
  Neither is this ticket, and neither is replaced by a block.

A smoke test and a store test both pin the re-claim, so a later "hardening" has to argue with a test
rather than quietly reverse this.

**The consequence, which the first implementation missed: a detach is therefore NOT naturally
idempotent.** Detach A, lose the HTTP response, B claims the now-free order, retry the identical
request — and the retry detaches **B**, a customer the operator never reviewed, recorded under the
reason written about A. Retry timing would decide whose purchase is taken.

So `Idempotency-Key` is **required**, stored on the detachment row, and unique per order. A replay
returns the customer the *first* call detached and touches nothing. A **different** key on the same
order is a new decision and detaches the current owner — support fixing a second mis-claim must
still work. Both are pinned by tests, including one that drives the exact lost-response-then-claim
sequence.

### 5. Exactly two statements may write `orders.customer_id`

`TestNoProductionCodeUpdatesOrderAttribution` allowed exactly one — the claim (TKT-221/TKT-223). It
now allows exactly **two**, and each is bound to **its own** predicates:

| Statement | Assignment | Required predicates |
|---|---|---|
| Claim (TKT-223) | `customer_id = $2` | `guest_order_ref = $1`, `status = 'completed'`, `customer_id IS NULL OR customer_id = $2` |
| Detach (TKT-225) | `customer_id = NULL` | `id = $1`, `status = 'completed'`, `customer_id IS NOT NULL` |

**Not one relaxed rule covering both.** Checking "is this a known assignment" and "are these known
predicates" *independently* would admit a detach carrying the claim's `WHERE` clause, and vice
versa — both halves individually sanctioned, just not together. That is how a two-entry allowlist
becomes a blanket one, and `TestTheAllowlistCannotBeWidened` now asserts both cross-borrowings are
refused, alongside a transfer-shaped third statement.

**And matched whole, not by required substrings.** A per-predicate `strings.Contains` check
authorizes anything that *also* contains them, which SQL boolean precedence makes catastrophic:

```sql
WHERE id = $1 AND status = 'completed' AND customer_id IS NOT NULL OR $1 = $1
```

contains every required predicate, keeps the count at two — and clears attribution from **every row
in the table**. This bypass **pre-dated TKT-225**: the one-statement version admitted the same
append. It is fixed here rather than filed, because this ticket doubles the number of statements to
append to. The guard now compares the whole normalized assignment and where-clause (including the
`RETURNING` clause) for equality, and the widening test covers `OR $1 = $1`, `OR TRUE`, and
predicates parked in a dead branch.

A claim is the only `NULL → customer` transition and a detach the only `customer → NULL` one.
Together they are the whole of attribution's mutable life; a third statement is by construction
something else.

## Consequences

- A buyer whose order was wrongly claimed has a recourse, and support has a supported path that is
  not a hand-written `UPDATE`.
- `customer_id IS NOT NULL` in the detach is **load-bearing**: without it, detaching an
  already-unattributed order reports success and writes an audit row for a detachment that never
  happened — a false row in the one table whose purpose is being true.
- `updated_at` is not touched, matching the claim. It means "when did this order's checkout last
  move", and recovery reads it to decide what is stale; a support action months later is not
  checkout activity.
- No order status is added ([ADR-016](./ADR-016-checkout-recovery-state-machine.md)). Attribution is
  its own dimension.
- **`RETURNING OLD.customer_id` is PostgreSQL 18.** In an `UPDATE`'s `RETURNING` clause, both a bare
  and a *table-qualified* column name yield the **new** value, so the obvious spellings return `NULL`
  and record a detachment from nobody. `compose.yaml` pins `postgres:18.4`; on an older server the
  statement is a syntax error at first execution — loud, not silently wrong.
- **Still open:** no back-office surface (an operator needs the service credential), `actor` is
  unauthenticated text, and the audit rows are ordinary commerce state. TKT-202 remains the cause of
  the leak that makes wrongful claims possible in the first place.
