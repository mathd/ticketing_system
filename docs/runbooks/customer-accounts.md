# Customer accounts and recovery

Local Compose procedures. Run commands from the repository root. Configuration comes from
[configuration](../configuration.md); safety and approval rules remain in [AGENTS.md](../../AGENTS.md).
Load only the section needed for the operation. Historical examples retain their ticket IDs.

## Customer accounts (TKT-220)

Buyers may create an **optional** account on the storefront. Guest checkout is unchanged and stays
the default: no storefront route redirects to sign-in, and buying never requires an account. See
ADR-049.

**Where things live.** Accounts are `customer_accounts` in **commerce** (migration `0015`), reached
through two public contract operations — `POST /api/commerce/customers` (register) and
`POST /api/commerce/customers/authenticate`. Neither takes a credential, which is why the storefront
container still holds exactly one environment variable (`GATEWAY_URL`) and **no service token**. Do
not "fix" that by giving it one.

**Sign in / register / sign out** are at `/{locale}/account/sign-in`, `/{locale}/account/register`
and (POST only) `/{locale}/account/sign-out`. There is no provisioning CLI and no seeded account —
registration is public, unlike staff.

**Password recovery exists (TKT-226).** `/{locale}/account/forgot-password` mails a link;
`/{locale}/account/reset-password?token=…` redeems it. See § *Password recovery* below. There is
still **no email verification and no magic link** — an account can register an address it does not
own.

**Registering an address that already exists answers 409 and says so.** That is a deliberate,
recorded disclosure — an unauthenticated membership oracle over the customer base (ADR-049 §2).
Rate limiting it is **TKT-224**. Now that mail exists the standard mitigation (answer 201, mail the
owner) has become available and **§2 is revisitable** — deliberately not done in TKT-226, because it
forces registration to stop returning a principal and breaks the register→signed-in flow. See
ADR-049 § *TKT-226 amendment*.

**A wrong password and an unknown address are the same answer and the same cost.** If you are
debugging a sign-in and want to know which it was, the answer is deliberately unavailable from
outside; look at `customer_accounts` directly.

**Sessions are in-process and are not persisted**, exactly like the back office: a storefront
restart signs every customer out, a second replica would not share them, the eight-hour lifetime is
absolute and does not slide, and one customer holds at most **five** concurrent sessions. The cap is
what bounds the session map, not the expiry sweep.

**The customer session cookie is scoped to `/`, not to an account subtree** — a deliberate departure
from the back office's `/admin`-scoped cookie, argued in ADR-049 §5. It is therefore attached to
same-origin requests to `/api/*`, `/admin/` and `/scanner/`.

> **Standing constraint:** request logging must never log the `Cookie` header. Today nothing does —
> `shared/go/obs/requestlog.go` records method, path, status and duration only — and ADR-049 §5
> accepts the cookie's scope *on that basis*. Adding header logging anywhere in the gateway or the
> services invalidates that argument and needs the cookie re-scoped, not a note.

**Verifying a change here needs a real browser**, for the same reason as the back office and with
one extra: `security.checkOrigin` defaults to **true** in Astro 7 and the storefront had never set
it, because it had no form until this ticket. `make check` renders storefront pages and never
submits one, so the whole "SSR rejects the write before the handler runs" class is invisible to it.
Drive `make up` and submit register, sign-in and sign-out.

### Attributing a purchase to an account (TKT-221)

A checkout made while signed in is attached to the account; a guest checkout is not, and guest stays
the default. See ADR-049 § *TKT-221 amendment*.

**The checkout no longer goes straight to commerce.** It posts to the storefront's own bridge at
**`/checkout`**, which attaches a commerce-signed assertion server-side when a session is live and
forwards everything else untouched. The session cookie is `httpOnly` and the checkout runs in a
browser island, so nothing else could have done it. If checkout starts failing, `/checkout` is the
first place to look — and note it passes the storefront's origin check, **including for guests**.

**There is now a fourth credential**, `COMMERCE_CUSTOMER_ASSERTION_KEY`, held only by commerce and
generated independently by `make up` alongside the other three. **Commerce refuses to start if any
two of its three credentials are equal** — the error names the pair and never echoes a value.

**Rotating that key signs everyone out of attribution.** There is no multi-key verification: every
assertion minted under the old key stops verifying immediately, so signed-in buyers get guest orders
until they sign in again. Rotate deliberately.

**An expired session at checkout is a guest checkout, not an error.** That is on purpose: breaking a
working checkout because the buyer had once been signed in is worse than an unattributed order they
can still reach by order reference.

**`GET /orders/{id}` reports `customer_id` and it is informational only** — that read is public and
answers for any order id. It is not an ownership check.

### The wallet (TKT-222)

`/{locale}/account` lists the signed-in customer's completed purchases, newest first, 20 to a page,
each linking to the existing ticket page. See ADR-049 § *TKT-222 amendment*.

**If the wallet shows purchases with no event name**, catalog's display-name resolver is failing —
commerce logs a `WARN` and renders the rows anyway, deliberately: a row the buyer can still open
beats a wallet that will not load. Look for `wallet display names unavailable`.

**If the wallet says "could not be loaded"**, the commerce read itself failed. That is a different
state from an empty wallet, and the page distinguishes them on purpose.

**A customer asking for another customer's wallet gets 404, not 403** — the same answer as an id that
does not exist. That is deliberate non-disclosure, not a bug: 403 would confirm the account exists.

### Claiming a past guest order (TKT-223)

A signed-in buyer can attach a completed guest order to their account from the ticket page. See
ADR-049 § *TKT-223 amendment*.

> **A claim can be undone since TKT-225** — see *Undoing a claim* below. It is an operator action,
> not something the buyer can do, and it is recorded.

**The proof is the order reference alone**, deliberately — matching the email would refuse a buyer
who signed up with a different address. That means anyone holding a leaked reference can claim the
order, which made **TKT-202** (every service logged the reference via the URL path) a safety issue
for this feature and not just logging hygiene. TKT-202 is closed: the request log, the contract-drift
log and the OTel span attribute all sanitise declared capability segments. References already written
to retained logs before that change remain valid, so ADR-052's detach path is still the recovery.

**Every refusal is the same 404.** "No such order", "not completed" and "already claimed by somebody
else" are indistinguishable on purpose. If a buyer reports that claiming does not work, the answer is
in `orders` — check `status` and `customer_id` for that `guest_order_ref`.

**Paging is keyset on `created_at`, never `updated_at`.** `updated_at` is rewritten by checkout
retries, recovery and the refund runner, and a cursor on a mutable key makes rows jump pages. If
someone "optimizes" the sort key, that is the bug to look for.

### Undoing a claim (TKT-225)

The recourse for "someone else claimed my order". See ADR-052.

```bash
curl -sS -X POST "$COMMERCE_URL/internal/orders/$ORDER_ID/unclaim" \
  -H "X-Internal-Token: $COMMERCE_INTERNAL_TOKEN" \
  -H "Idempotency-Key: support-4471" \
  -H 'Content-Type: application/json' \
  -d '{"actor":"staff:amy","reason":"claimed by the wrong account, buyer confirmed by phone"}'
```

It answers `200` with the order id and **the customer it was detached from** — record that value.
It is gone from `orders` the moment the call succeeds, and it is the only way to know who to hand
the order back to if you detached the wrong one.

**Direct to commerce, not through the gateway.** `/api/commerce/internal/` is edge-denied by
construction, and a smoke test pins that for this route.

**`Idempotency-Key` is required, and it is not ceremony.** Because a detached order is immediately
claimable again, a retry without a key could detach whoever claimed it in between — someone you
never reviewed, recorded under your reason. Reuse the same key when retrying; use a NEW key when you
genuinely mean to detach again (a second mis-claim on the same order).

**`actor` and `reason` are required and are recorded** in `order_attribution_detachments`. Blank or
whitespace-only values are refused by the handler *and* by a database `CHECK` — an un-claim that
says nothing about itself is a failed record, not a permitted one.

> **What the record is worth.** These rows live in the commerce database, so anyone who can write it
> can alter or delete them: **accountability against a careless operator, not tamper evidence
> against a hostile one** (ADR-021). And `actor` is a label the caller supplies — the internal token
> carries no individual identity. Do not describe this trail as tamper-evident.

**Every refusal is `404 "order is not detachable"`** — no such order, not completed, or already
unattributed. As with the claim, the answer is in `orders`: check `status` and `customer_id`. A
malformed request (blank actor/reason, bad uuid) is `400`, which is a different problem.

**A detached order is immediately claimable again, by anyone holding the reference.** That is
deliberate (ADR-052 § 4): blocking re-claim would block the rightful buyer, and would not stop an
attacker who can register another account. Tell the buyer to claim it promptly. What bounds abuse is
ADR-051's rate limiting on the claim path — and the underlying exposure was fixed by TKT-202, which
stops this platform emitting the reference into its own logs and traces (it cannot un-leak references
already retained elsewhere).

**Detaching is not transferring.** It restores `NULL` and stops. There is no operation that moves an
order from one account to another in one step (TKT-9/TKT-160). If you need to move an order, detach
it and have the correct buyer claim it.

### Password recovery and the mail path (TKT-226)

`/{locale}/account/forgot-password` asks for a link; the mailed link opens
`/{locale}/account/reset-password?token=…`. See ADR-050.

> **Nothing in this system has ever sent an email.** The only `mail.Sender` is
> `shared/go/mail.Fake`, which validates a message, keeps it in memory and returns success.
> `make up`, the smoke stack and `make check` all run against it, deliberately — the gate must not
> need a network or a provider account. **A buyer on a local stack will never receive anything.**
> To see what *would* have been sent, read the row (below).

**Where a message actually is.** `mail_outbox` in the commerce database. One row per message,
`sent_at IS NULL` until the drainer's sender accepted it:

```sql
SELECT id, recipient, subject, sent_at, attempts, last_error, dead_lettered_at
  FROM mail_outbox ORDER BY created_at DESC LIMIT 20;
-- the reset link itself, when you need to follow it locally:
SELECT body FROM mail_outbox WHERE recipient = 'buyer@example.test' ORDER BY created_at DESC LIMIT 1;
```

> **That table holds PII and live credentials in plaintext.** The body of a reset message *is* a
> working reset link until it is redeemed or expires. Nothing prunes the table — retention is
> **TKT-33**. Treat a dump of it as you would a password file.

**"The reset link never arrived."** In order: is there a row? Is `sent_at` set? Is
`dead_lettered_at` set — that row is quarantined and will never be retried, and `last_error` says
why. A row that is present and unsent with rising `attempts` means the sender is refusing; a row
that never appears means the address has no account, and **that is indistinguishable from the
outside on purpose**.

> **`last_error` starting with `DELIVERED-BUT-UNCONFIRMED` means the opposite of what the row
> looks like.** That row has `sent_at IS NULL` and is dead-lettered, but the sender *accepted* the
> message — possibly on every attempt — and only the database write recording that failed. **Do not
> tell a customer nothing was sent, and do not resend by hand without asking them first.** Every
> other dead-lettered row means what it says: nothing left.

**The drainer logs nothing from the message** — not the recipient, not the subject, not the body,
on any path including failure. The `message_id` is the operator's handle. Do not "improve" those log
lines: for a reset the body is a credential and the recipient is the fact the endpoint refuses to
disclose.

**A reset request answers 202 for every address**, known or not. There is no 404 and adding one
would be a security regression, not a usability fix. The answer is identical in status and bytes; it
is **not** identical in cost (a known address commits two rows), and ADR-050 records that residual
rather than claiming otherwise.

**`PUBLIC_BASE_URL` is what reset links are built from** — the same variable and meaning access
already uses for ticket delivery. Commerce **never** derives it from the request: a link base taken
from the `Host` header lets a caller mail a victim a genuine reset link pointing at the attacker's
site. Unset degrades to a startup WARN and undeliverable mail; every other operation still serves.

**Tokens are single-use, one hour, and SHA-256 in the database.** Not bcrypt — a salted hash cannot
be looked up, only verified, so finding the row would be a full-table scan of KDF operations.
Requesting a new link invalidates the previous one, and redeeming one invalidates the customer's
others.

**A completed reset signs that customer out everywhere — in the storefront process.** That is the
half that makes a reset meaningful: changing the credential does not touch the session map. It works
because the storefront route calls `destroyAllSessionsForCustomer`. **A reset completed by calling
commerce directly signs nobody out**, and a storefront restart or a second replica is outside it
either way (ADR-049 §4).

**Verifying a change here needs a real browser**, for the reason the rest of this section's features
do: `make check` renders storefront pages and never submits one. Submit both reset forms.
