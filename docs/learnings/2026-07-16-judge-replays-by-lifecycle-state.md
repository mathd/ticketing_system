# Judge idempotent replays by lifecycle state, never by timestamp

**Ticket:** TKT-77 (PR #53) · **Caught by:** adversarial review passes 2 and 3 — not by tests, not by the author.

## The lesson

When an idempotent replay returns a previously-created resource, deciding what to do with it
by any *derived* signal (a timestamp, a count, a price) instead of its **lifecycle state**
ships a guard that is wrong in both directions. And when the state field arrives over a
service boundary, an **unknown value is an invalid response, not a terminal state** —
ADR-017's dispatch rule applies to synchronous HTTP replies exactly as it does to consumed
events.

## What happened

TKT-77's staff conversion carves quantity out of an operational hold into a TTL buyer hold;
commerce persists a reservation after inventory commits, so a crash between the two writes is
repaired by replaying the request. The guard protecting that replay was rewritten three times:

1. **Draft:** no guard — a replay could persist a reservation whose hold had already expired
   (capacity already returned to the public pool), minting a reservation no checkout could
   complete.
2. **First fix — by timestamp** (`expires_at` elapsed → 409): wrong in both directions. A
   *confirmed* claim keeps its elapsed `expires_at` forever, so a legitimate replay after a
   successful checkout got a 409 **whose error text instructed staff to convert again** — a
   double-carve instruction for already-sold seats. Meanwhile a *released* child with a
   future timestamp sailed through.
3. **Second fix — by status, with a catch-all:** `default:` treated every unrecognized status
   as terminal and again advised re-conversion. A version-skew status from a newer inventory
   would trigger unsafe guidance for a possibly-live child.

Final shape: `confirmed`/`finalizing` accepted; `expired`/`released` enumerated as the only
re-convert 409s; `held` rejected only past its deadline; anything else (empty, unknown) is a
502 invalid-response.

## Why every layer missed it

Every fact in the timestamp guard was correct — expired holds *do* have elapsed timestamps.
The synthesis was wrong: elapsed-timestamp is a property of expired holds, not a definition
of them. The unit tests couldn't object because they were built from the same mental model
(only an expired-held fixture existed — the fixture encoded the assumption it should have
challenged, the TKT-61 fixture trap in miniature). It took a reviewer primed to refute, with
the diff and no justification, to ask "what else has an elapsed timestamp?"

## The rule

- A replay guard's switch enumerates **known** states on both sides; the default is an
  error about *the response*, never a decision about *the resource*.
- Error text is part of the contract: "place a new conversion" is an instruction someone
  will follow — never attach it to a state you haven't positively identified as terminal.
- For any state machine crossing a service boundary, write the fixture for the state your
  model says "can't happen here" (the confirmed-with-elapsed-timestamp case). If you can't
  construct it, that's the finding.
