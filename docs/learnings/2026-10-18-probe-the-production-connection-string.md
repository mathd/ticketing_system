# Probe the production connection string, not a convenient one

**2026-10-18 — TKT-557**

An error classifier is only as good as the error shapes it was built from. The shapes depend on the
whole connection path, and the connection string is part of that path. A probe that used a
"cleaner" DSN than production measured a different driver.

## How it was established

TKT-557 classifies PostgreSQL availability failures at four pre-decision sites. Its first two probes
ran with `sslmode=disable`. Production Access set no `sslmode` (`compose.yaml`, all three Access
URLs), so pgx used its default `prefer`: try TLS, then plaintext. The Compose PostgreSQL has no TLS,
so every connect attempt made two attempts. The plan-review audit (round 2, PA2-1) read this from the
pgx source. A re-run with the exact production DSN then executed it. All 40 restart (57P03) and
connection-limit (53300) calls returned a `ConnectError` join with an untyped "server refused TLS
connection" child beside the PgError. The plan's rule "every branch must qualify" therefore made a
routine database restart answer 500, and after TKT-558 every gate would stop. The `disable` control
removed the extra child.

The fix was to make production match the evidence, not to widen the rule: Access now requires an
explicit `sslmode=disable` and refuses to start without it (ADR-085 D21). The TLS-refusal error
has no exported type, so the only alternative was message matching, which is banned.

## The rule

- Probe with the production connection string byte for byte. That includes defaults the string
  leaves out (here `sslmode`, and the fallback behaviour it selects).
- When a probe and production differ, either change the probe or change production so that they
  match. Then make startup refuse the other form, so that they cannot drift apart again.
- A reviewer's library-source reading is a hypothesis. Confirm it with an executed probe before you
  change a rule (AGENTS.md: a security claim is a hypothesis until it is executed).
