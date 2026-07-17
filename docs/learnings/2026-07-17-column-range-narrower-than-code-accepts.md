# A column narrower than the value the code accepts turns storage into a validator — silently, and in the retry arm

**Ticket:** TKT-68, PR #64 (ai-review finding 1) · **Date:** 2026-07-17

## What happened

TKT-68's quarantine table stored the event envelope's `schema` as Postgres `integer` (int32).
The consumer's dispatch treats *any* schema above its known max as a legitimate future variant
and forwards it to `QuarantineCatalogEvent` — and the Go field is `int` (int64 on our targets),
so a value like `4_000_000_000` sails through every explicit check and dies inside the INSERT
with a 22003 range error.

The kicker is *where* that error lands: not in a poison branch, but in the generic
`err != nil` retry arm — permanent 5-second NAK loop, readiness latched, one malformed event =
the exact outage class the poison rules exist to prevent. Every explicit validation was correct;
the column type was doing extra, unwritten validation with the wrong disposition attached.

## The rule

When a code path accepts an externally-supplied integer over a range check like `> max`, the
storage column bounds the *upper* end implicitly. Either the column matches the full range the
code type accepts (`bigint` for Go `int`), or the out-of-range case must be classified
explicitly (poison → terminate) before the write. A DB range error in a retry arm is a
permanent loop wearing an innocent face.

Checklist form: for every column fed from a decoded wire value, ask "what happens when the wire
value exceeds the column, and which disposition arm catches that error?"

## Why the gate missed it

Unit tests faked the store (no SQL types in play); smoke tests used realistic small schemas.
Only an adversarial reviewer asking "what value passes the checks but breaks the write?" found
it. The pinned regression is `TestQuarantineAcceptsSchemaBeyondInt32` — through the production
write path against real Postgres.
