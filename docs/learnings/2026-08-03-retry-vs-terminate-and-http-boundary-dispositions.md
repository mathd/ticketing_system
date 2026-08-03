# Retry-vs-terminate is asymmetric, and a comment is not evidence

**TKT-181 (PR #151) — 2026-08-03**

## What happened

One boolean — retry or terminate a message — took three adversarial review passes to get right.

## The rules that came out of it

- **The safe default is asymmetric.** A needless retry costs a delay; a wrong terminate costs the
  event. So **permanence must be a narrow enumerated list**, never a range. "Any 4xx is
  deterministic" terminates on a 429 and a misconfigured 403.
- **Decode from a buffer, not from the response body.** Read the body fully (under a limit) and
  *then* decode. Decoding straight from the stream makes an interrupted transport indistinguishable
  from malformed content — and those two need **opposite** dispositions.
- **A comment asserting a disposition is not evidence of it.** The critical finding in this ticket
  sat two lines beneath a comment claiming the opposite behaviour. The fix is a **disposition test
  per branch**: assert Ack / Nak / Term, not just the returned error.
- **`ON CONFLICT DO NOTHING` cannot tell a replay from an upgrade.** Both look like "the row
  exists". If a correction wave may ever re-emit under a fresh id, the write path needs an explicit
  upgrade branch with identity verification — otherwise the upgrade is silently swallowed.
- **An HTTP boundary with no tests is invisible until something decodes badly.** Test doubles that
  return well-formed Go values prove nothing about parsing, status classification or truncation.
  Test the real client against a real `httptest` server.

## What to do

When adding a dependency call to an existing handler, **read that handler's failure disposition
first** and state which branch the new failure takes. The existing branch was written when only one
schema called out to another service, and inheriting it silently was the root of all three passes.
