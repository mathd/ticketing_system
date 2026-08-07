# ADR-050: Transactional mail behind a port; password recovery through a queued message

Date: 2026-08-06

## Status

Accepted

Closes the gap ADR-049 recorded and amends ADR-049 §2 and its Consequences (see the
*TKT-226 amendment* section there). It does not supersede ADR-049 — customer identity,
the session model and the storefront's credential-free posture are unchanged.

## Context

ADR-049 states it plainly:

> This repo has no mail path. No SMTP, no queue, no provider. Every design that depends on
> sending mail — email verification, password reset, "we sent the owner a note" on a duplicate
> registration — is unavailable, not merely unbuilt.

Three of TKT-21's decisions were shaped by that absence, and one of them is that **a customer
who forgets their password has no way back in**. TKT-224 exists to rate-limit a disclosure
(ADR-049 §2's 409 membership oracle) that mail would remove outright.

**The sentence above is exactly true about senders and misleading about ports**, and this ADR
records the correction because TKT-226's own plan draft was briefed on it and got decision (a)
wrong as a result:

```go
// services/access/internal/consumer/consumer.go
type Mailer interface { Send(context.Context, uuid.UUID, string, string) error }
type LogMailer struct{ log *slog.Logger }
```

`LogMailer` is the only implementation, it is what access wires in `main.go`, and it **sends
nothing** — it logs a SHA-256 of the recipient and returns nil. ADR-012 records it deliberately.
So the accurate statement is: **this repo has had one mail port since ticket issuance shipped,
and has never had a sender.** Ticket delivery is not a capability access has and recovery lacks;
it is the same missing capability, already stubbed once.

Four constraints bound the design.

- **ADR-002 cuts at five services.** A sender lands inside one of them or the deviation is its
  own ADR. ADR-049 already priced and rejected a sixth service and a gateway-owned store.
- **ADR-032 fixed the shape for exactly this problem** — a provider behind a port with a
  deterministic offline fake, so `make check` runs with no network and no provider account.
- **ADR-043 places the operations.** Declared operations in a service's public contract are its
  public surface; an inline `X-Internal-Token` check guards `/internal/`.
- **The reset request must not reintroduce an enumeration oracle.** A request for an unknown
  address must answer as a request for a known one does. This is the constraint that decides
  the queue question, and it is not obvious until it is written down.

## Decision

### 1. The port and its fake live in `shared/go/mail`; commerce owns the reset flow

`Message{To, Subject, Body}` and `Sender.Send`. One operation, no HTML alternative, no
attachments, no template id — the fields a marketing sender needs, which TKT-226's non-goals
name explicitly.

`shared/go` rather than `services/commerce/internal/mail`, for the reason `shared/go/fakepsp`
is there: a port's offline fake that more than one module needs belongs in the module every
service already has in `go.work`. Two callers of the concept exist today. It costs zero extra
code and it is where a real provider must land.

**`access` is deliberately NOT migrated onto it.** Its `Send(ctx, deliveryID, email, link)` has
no subject and no body, so adapting it means composing ticket-delivery copy — a behaviour change
to a path TKT-226 does not otherwise touch, in the service carrying the lifecycle-trail
invariants (ADR-021). **Migrating access is what a real provider forces**, not what this ticket
does. Until then the repo has two mail ports and one sender, and this paragraph is why.

**Commerce owns the reset flow itself** — tokens, the outbox, the drainer — because it owns the
accounts and the credentials (ADR-049). No data crosses a service boundary and no new deploy
unit exists. The *port* is shared; the *durable delivery* is commerce's.

**The fake is the default and it is production wiring, not test scaffolding.** Commerce selects
it whenever no provider is configured, which is every environment this repo has. ADR-032's rule.

### 2. Both operations are PUBLIC contract operations

`POST /customers/password-reset` and `POST /customers/password-reset/complete`, carrying no
credential — ADR-043's rule, and ADR-049 §1's reasoning verbatim: the caller is by definition
someone who cannot sign in, so no credential exists to present, and the storefront that renders
the form still holds exactly one environment variable and no service token.

### 3. Sending is queued, and the reason is enumeration parity — not durability for its own sake

A transactional outbox (`mail_outbox`), drained by a background worker. The table's shape and
the drainer's protocol are `completion_outbox`'s (ADR-016 §Decision 6) applied to a different
table: claim under a lease with a claim id, send, retire only after the sender returned, release
with exponential backoff, dead-letter after ten attempts.

**Why a queue at all, when the only sender in this repo is a fake that cannot fail.** This is
the paragraph that stops the queue being deleted later:

> **Inline sending cannot satisfy the enumeration-parity criterion.** An unknown address does no
> send and therefore *cannot fail*; a known address can. So *inline and honest* makes a send
> failure an account-existence oracle — the thing this operation is shaped to avoid — and
> *inline and silent* is the "the reset never arrived and nobody was told" outcome, which is
> worse than refusing. Enqueueing is the only shape in which both answers are written **before
> any delivery is attempted**, so the provider's behaviour cannot be read from the response.

The durability is real and is a bonus. The enumeration argument is the requirement.

**What a send failure does:** the row stays claimable with the cause in `last_error` and an
exponential backoff; after ten attempts it is dead-lettered — permanently unclaimable, still
visible to an operator, and logged at ERROR as the last notice anyone gets. **Nothing from the
message reaches any log line**: not the recipient, not the subject, not the body. For a reset the
body *is* a live credential and the recipient *is* the fact the endpoint refuses to disclose, so
logging either would undo the whole design in a WARN line.

**Two drainers, not one abstraction.** `internal/mailer` is a near-copy of `internal/outbox`.
The store functions name their table in their SQL and return differently-shaped rows, so sharing
them means a row-shape abstraction over two tables that buys nothing today. Two instances do not
earn one; a third would. Recorded so a reviewer files it as a decision rather than duplication.

### 4. The token is 32 random bytes; the database stores its SHA-256

Not bcrypt, and the analogy with `customer_accounts.password_hash` six inches away is the wrong
one:

- **A bcrypt hash carries its own salt, so it can only be verified, never looked up.** Finding
  the row would mean comparing the presented token against every row at cost 10 — a full table
  scan of KDF operations, on an unauthenticated public endpoint.
- **A work factor buys nothing here.** A password is low-entropy and guessable, which is what a
  slow KDF defends. This token is 256 bits from `crypto/rand`: there is no dictionary, so there
  is nothing to slow down.

Single-use and expiry are enforced by **one conditional `UPDATE … RETURNING`** with
`used_at IS NULL AND expires_at > now()`, never a SELECT then an UPDATE — a read-then-write is a
race two redemptions both win, and the loser's password would silently replace the winner's. The
expiry is compared against the **database** clock. TTL is one hour: it bounds the gap between
asking for mail and reading it, not a working visit, so ADR-049 §4's eight hours does not
transplant.

Issuing a token **invalidates every outstanding token for that customer**, and so does redeeming
one. Without it, a buyer who re-requests because the first "didn't arrive" has widened the window
rather than narrowed it. The single-use predicate bounds each token; only this bounds their
number.

An unusable *new* password is refused **before** the token is spent, so a buyer who pastes
something over 72 bytes does not burn their one-shot link on a request that could never succeed.

### 5. The link carries the token in a query parameter, and the page posts it in a body

Not a path segment: `shared/go/obs/requestlog.go` logs `r.URL.Path` on every service and the
gateway, so a token in the path would be written to a log exactly the way TKT-202 records
`guest_order_ref` being written today. The query string is not logged, which ADR-049 §
*TKT-222 amendment* already relies on to put twenty uuids in a query.

**A URL fragment was considered and rejected.** A fragment never reaches any server including
ours, so the page would have to read `location.hash` in the browser — making password recovery
**the one storefront path that requires JavaScript**, for the buyer who is already locked out.
Register, sign-in and claim are all plain server-rendered forms; the only client-side code in the
storefront is the seat/hold picker on the *purchase* path.

The residual the fragment would have closed is **`Referer` leakage**, and that is closed
directly: the reset page sets `Referrer-Policy: no-referrer` and `Cache-Control: no-store`. No
such header existed anywhere in this repo before, so it is deliberate rather than a pattern being
followed.

### 6. Both recovery pages are anonymous, and that is load-bearing

`gate.ts` lists them beside register and sign-in. Gating them would redirect a locked-out buyer
to the form they are locked out of — this feature's own trap, rebuilt one layer up.

### 7. A reset destroys that customer's storefront sessions — in this process

Changing the credential does not touch the session map; it never re-checks a password. So
without this, an attacker holding a stolen live session keeps it through the owner's reset, and
"reset your password" would not mean what everyone assumes it means.

Commerce cannot do it — sessions are in-process in the storefront (ADR-049 §4) — so the
storefront route calls `destroyAllSessionsForCustomer(customerId)` with the id commerce returns.
Scoped to one customer, never global: a reset must not sign out strangers.

### The adversary, named (ADR-021)

**Refused:**

- Someone who has not read the buyer's mailbox. The token is 256 bits and is disclosed nowhere
  else.
- Replay of a redeemed or expired token, by the conditional `UPDATE`.
- A caller who submits a victim's address with their own `Host`. The link's origin is
  server-configured (`PUBLIC_BASE_URL`) and **nothing in the compose path reads the request** —
  building it from a header would make this endpoint a phishing generator that mails victims a
  genuine link pointing at the attacker's site.
- An attacker holding a stolen storefront session, once the owner resets **through the
  storefront**.
- Header injection into a recipient or subject: CR/LF are refused at the port, by every
  implementation, and refused rather than sanitized.

**Not refused, and not claimed to be:**

- **Anyone who can read the mailbox.** The whole scheme reduces to mailbox custody, as every
  reset flow does.
- **Anyone who can read commerce's database.** `mail_outbox` holds the composed body — including
  a live link — in plaintext until it is sent. That is inherent: something must hold the message.
  The `token_hash` column stops a *reader* of `password_reset_tokens` from minting a reset; it
  stops nothing against a **writer**, who can insert a row whose hash they chose — and who
  already owns `customer_accounts` anyway.
- **Timing.** The response is identical in **status and bytes**; it is **not** identical in
  **cost**. A known address commits two rows, an unknown address commits none. ADR-049 §3's mask
  is the KDF, and it does not transplant: a reset *request* submits no password, so there is no
  bcrypt to hide underneath. Closing this would mean writing rows for addresses that do not
  exist. Accepted because the residual is **smaller than what already ships** — registration's
  409 (ADR-049 §2) is an explicit, unmasked oracle over the same table — and because volume is
  what makes timing measurable, which is **TKT-224**.
- **A partially-broken database.** The parity holds while commerce is healthy and while it is
  wholly down (the customer lookup is the first statement, so both cases fail alike). It does
  **not** hold in between: a failure *after* the lookup succeeds — the token insert, the outbox
  insert, the commit — turns a known address into a 500 while an unknown address is still a 202.
  A caller who can observe commerce in a reads-work-writes-fail state can enumerate.

  Named rather than closed, because both ways of closing it are worse. Answering 202 on a write
  failure makes an outage silent, which is the "the reset never arrived and nobody was told"
  outcome the queue exists to prevent. Writing rows for addresses with no account is the option
  §3's cost paragraph already rejects. This is the narrowest residual of the three and it is the
  one **most likely to be forgotten**, so it is written down rather than left to a future reader
  to rediscover (ai-review [high], upheld as a gap and accepted as a trade).
- **A reset completed by calling commerce directly.** The operation is public contract, so a
  caller can bypass the storefront and change a password with the session map untouched. §7's
  guarantee is the storefront route's, not commerce's.
- **Cross-replica and cross-restart session invalidation**, by ADR-049 §4's design.

## Consequences

- **A customer who forgets their password can get back in.** ADR-049's Consequences said they
  could not; that sentence is now amended there rather than left to rot.
- **`mail_outbox` holds PII and live credentials in plaintext.** Retention and deletion are
  **TKT-33** and are not solved here. Nothing prunes the table.
- **Commerce gains a third background worker** and a new environment variable
  (`PUBLIC_BASE_URL`, the same variable and meaning access already uses). Unset degrades to a
  startup WARN and undeliverable reset mail; every other operation still serves. Refusing to
  start would be the wrong blast radius for one optional feature.
- **Nothing has ever actually sent an email.** The fake accepts and records; the gate, `make up`
  and the smoke stack all run against it. The first real provider is the first time any of this
  meets a network, and that is when access's `LogMailer` is migrated onto this port.
- **TKT-224 shrinks but does not disappear.** ADR-049 §2's 409 becomes removable now that "answer
  201 and mail the owner" is available — recorded as revisitable, deliberately **not** done here
  (it forces registration to stop returning a principal, which breaks the register→signed-in flow
  TKT-221 attaches to). Rate limiting is still needed for credential grinding and for reset-mail
  volume, which this ticket *adds* as an abuse surface: an unauthenticated caller can now make
  commerce enqueue a message per request.
- **Two mail ports, one sender, zero senders that send.** Stated plainly so the next reader is
  not surprised by it.
