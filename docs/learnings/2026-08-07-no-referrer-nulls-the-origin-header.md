# `Referrer-Policy: no-referrer` nulls the `Origin` header, and 403s every form POST

**2026-08-07 · TKT-226**

## What happened

The password-reset page carried a live token in its GET URL, so it was given

```js
Astro.response.headers.set('Referrer-Policy', 'no-referrer');
```

to stop that URL travelling in a `Referer` to anywhere the buyer went next. It is the strictest
value, it was added on an adversarial reviewer's advice, and it **silently disabled the feature it
was protecting**.

Chrome sends `Origin: null` on a form POST from a page whose referrer policy is `no-referrer`. The
storefront's proxy-aware origin check (`web/storefront/src/lib/gate.ts`, ADR-049 §6) compares
`Origin` against the gateway's public origin and refuses anything else, so **every password reset
was answered 403 before the handler ran**.

Measured side by side in one browser, against the real stack:

```
POST /en/account/forgot-password   origin="http://localhost:18099"  → 200
POST /en/account/reset-password    origin="null"                     → 403
```

The fix is `Referrer-Policy: origin`. It keeps `Origin` intact and **still** strips the path and
query from the `Referer`, so the token never appears in one — same-origin or cross-origin. That is
more than `same-origin` would have given, and it is exactly the residual the header was added for.

## Why nothing caught it

**Four green `make check` runs. Two adversarial review passes. 166 unit tests. All over a feature
that did not work at all.**

- `make check`'s smoke suite **renders** storefront pages and never **submits** one, so the whole
  "the SSR layer rejects the write before the handler runs" family is invisible to it. This is
  already in `AGENTS.md`.
- The reviewer that recommended the header was reading the diff, where the header and the origin
  check are in different files and neither mentions the other.
- The relationship is not in either file's vocabulary. Nothing named `Referrer-Policy` appears
  anywhere near `originIsTrusted`; the coupling exists only in the browser.

## The rule

**A response header that changes what the browser *sends* on the next request cannot be reasoned
about from either file alone.** `Referrer-Policy` looks like it governs `Referer`. It also governs
`Origin`, and `Origin` is what CSRF checks are built on.

Before adding or tightening a `Referrer-Policy` on any page with a form, ask what the browser will
put in `Origin` — and then **submit the form in a real browser**, because that is the only place the
answer exists.

## Why this one is worth a file

This is the **third** time this repo has paid for the render-is-not-submit gap:

- **TKT-105** — Astro's `security.checkOrigin` 403'd every back-office POST behind the gateway.
- **TKT-220** — the same trap, latent in a second app, plus the relative-vs-absolute form action.
- **TKT-226** — this.

Each time the defect was a **total failure of the write path**, and each time the gate was green.
The pattern is not "we keep making origin mistakes"; it is that **the class of defect that lives
between the browser and the handler has no automated coverage in this repo at all**.

**TKT-196** is the ticket for a browser-submit harness. Its value is no longer a matter of opinion:
the tooling that found this was untracked scratch, written twice now, and thrown away twice.

## Also from this ticket

Two smaller things, recorded because they are the same discipline failing under fix-momentum:

- **Two tests written to close review findings could not fail.** One stole a database row's claim
  *after* the function under test had already retired it; the other released goroutines from a
  channel and called that a race, when releasing them only makes them *eligible* to run. Both were
  caught — one by the gate, one by the next review pass. See
  [three ways one test could not fail](2026-08-06-three-ways-one-test-could-not-fail.md).
- **The fix for a race created a deadlock.** A `SELECT … FOR UPDATE` added to serialize token
  issuance inverted lock order against the redemption path. See
  [two correct fixes can compose into a new defect](2026-08-03-two-correct-fixes-can-compose-into-a-new-defect.md);
  reviewing the **fix diff** rather than the feature is what found it.
