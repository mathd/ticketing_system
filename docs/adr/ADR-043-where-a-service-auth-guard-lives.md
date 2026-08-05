# ADR-043: Where a service's auth guard lives — declared security vs inline check

Date: 2026-08-05

## Status

Accepted

## Context

TKT-194 gave the back office a commerce credential so box office staff can refund from the
order console. It guards the one operation that credential opens with an inline header
comparison (`staffOrInternal`, `services/commerce/internal/api/staff_credential.go`), extending
what commerce already did on its other internal routes.

The TKT-22 handoff called that the epic's one wrong decision, on this reading: catalog declares
`security:` in its OpenAPI contract and lets `openapi3filter` enforce it, commerce declares none
and compares headers in handlers, so *the system has two auth idioms across two services* and
commerce's allowlist lives in code rather than in the contract, against ADR-009's spirit.

**That reading does not survive checking, and the correction changes the question.** Counted from
the repo:

| | `security:` in the contract | its `/internal/` routes | inline `X-Internal-Token` checks | refusal on an internal route |
|---|---|---|---|---|
| catalog | 12 lines: 1 document-level default + 11 `security: []` opt-outs, over 37 operations | **not in the contract at all** | 5 | **401** |
| commerce | none | 8 paths, **documented** | every internal route | **404** |

So catalog runs *both* idioms already. The line it draws is not catalog-vs-commerce; it is
**contract operation vs internal route**, and commerce's inline check sits on the internal side of
exactly that line. What is genuinely inconsistent between the two services is narrower than the
handoff framed it: commerce documents its internal surface where catalog does not, and commerce
refuses with 404 where catalog refuses with 401.

That leaves one real question — should commerce declare `security:` on its 8 documented internal
operations and let the validator enforce them? — and one blocker the handoff correctly insisted be
answered rather than inherited: **is commerce's 404 load-bearing, or is it cargo?**

## Possible Solutions

- **Option 1 — Declare `security:` on commerce's internal operations.** A document-level
  requirement, `security: []` on its public ops, a per-op override for the refund; delete
  `staffOrInternal`; the enumeration test possibly becomes a spec assertion.
    - Pros:
        - The allowlist is in the contract, visible to anyone reading the document.
        - New operations are closed by default; opening one takes a visible `security: []`.
        - One mechanism inside commerce.
    - Cons:
        - **Changes the refusal on 8 routes from 404 to 401** — see the Decision; this is not a
          detail, it is the reason the option fails.
        - Two schemes must coexist (`X-Internal-Token` and `X-Commerce-Staff-Write-Token`) with a
          per-operation override, which is more contract machinery than the one exception needs.
        - The enumeration test that walks the real chi router would be replaced by an assertion
          over the document — and the document is not what serves requests. That is the defect the
          test already replaced once (ai-review pass 1: a hand-maintained count cannot detect the
          drift it exists to catch).
- **Option 2 — Keep the inline check and write down why.** No code change; the reasoning stops
  being implicit in a ticket comment.
    - Pros:
        - Refusal semantics stay uniform across commerce's internal surface, and stay identical to
          the gateway's own refusal for the same paths.
        - The router-walking enumeration test keeps proving the property against the router.
    - Cons:
        - `staffOrInternal` survives; the allowlist stays in code.
        - The 401/404 split between catalog and commerce internal routes remains, now deliberate
          rather than accidental.
- **Option 3 — Do nothing and leave it undocumented.** Rejected outright: the current state is
  explained only in a TKT-194 plan comment, which is the failure this ADR exists to fix.

## Decision

**We keep the inline check on internal routes, and we adopt the contract-vs-internal line as the
rule: declared `security:` guards operations in a service's public contract; an inline check
guards its internal surface.** Catalog already follows it. Commerce now follows it explicitly.

**Commerce's 404 is load-bearing, not cargo.** The gateway registers `/api/<svc>/internal/` to a
deny handler that answers **404** with a body saying only that the edge refused — it deliberately
does not disclose which route table entry matched or why, because "enumerating the internal
surface is the caller's problem to solve, not ours to help with"
(`gateway/cmd/gateway/main.go`). Commerce answering 404 on the same paths makes the two layers
indistinguishable from outside. A 401 would not: it would tell a caller that it got *past* the
edge and that the route exists behind a credential it does not hold — which is precisely the fact
the edge refuses to disclose.

Name the adversary (ADR-021's rule). This is not about the public internet, which cannot reach
these routes at all. It is about **a compromised back office** — the process TKT-194 handed a
commerce credential, which is internet-facing SSR and can already open a socket to commerce on the
container network. Probing commerce's other 7 internal operations with that credential, it gets 404
from each and learns nothing about which exist or which the credential is short of. Under Option 1
it would get 401 from each and learn both. The property this buys is modest and it is not the
control that stops the attack — `staffOrInternal` refusing is — but it is free, and Option 1
spends it.

Catalog's internal routes answering 401 is a pre-existing inconsistency this ADR does not resolve.
It is a smaller exposure: nothing hands out a narrow catalog credential that reaches catalog's
internal surface, so there is no holder positioned to enumerate it. Left as-is rather than churned.

## Consequences

- **Positive:**
    - The rule is now stated, so the next service's internal route has an answer to copy instead of
      a precedent to argue about.
    - `TestTheEnumerationCoversEveryInternalRouteCommerceServes` keeps its job: it walks the real
      chi router, which is the only thing that knows what commerce serves. It is not scaffolding
      propping up a weaker idiom — it is the enforcement the contract-side option would have had to
      reproduce anyway, and less well.
    - No behaviour changes, so nothing needs re-reviewing.
- **Negative:**
    - The commerce allowlist stays in code. Its correctness rests entirely on the enumeration test,
      which must not be weakened into a hand-written list (it already was once, and was fixed).
    - Two refusal codes for internal routes across the estate (catalog 401, commerce 404). Now
      documented, still not uniform.
    - Commerce's contract documents 8 internal paths it declares no security for. A reader of the
      document alone cannot tell they are guarded. The gateway's deny and this ADR are what say so.

## References

- TKT-194 (plan-final, amendment A2) · TKT-22 refactor handoff
- [ADR-002: services from day one](ADR-002-services-from-day-one.md) — the route table is the security boundary
- [ADR-009](ADR-009-contract-first-apis.md) — contract-first
- [ADR-021](ADR-021-ticket-lifecycle-trail-integrity.md) — name the adversary before claiming a property
- [ADR-042: staff identity and back-office sessions](ADR-042-staff-identity-and-backoffice-sessions.md)
- `services/commerce/internal/api/staff_credential.go`, `services/catalog/internal/api/write_credential.go`, `gateway/cmd/gateway/main.go`
