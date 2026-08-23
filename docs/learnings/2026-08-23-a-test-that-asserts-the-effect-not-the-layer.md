# A test that asserts the EFFECT cannot tell you which layer produced it

**2026-08-23 — TKT-255.** Two tests in this ticket passed their first mutation. Both had accurate
names, both went red for the right reason when the *feature* was broken, and neither could see the
mechanism it claimed to be about. The shape is one level past the green-test rules already in
`AGENTS.md`: not a fixture that cannot reach the failing state, but a fixture that reaches it by more
than one road and never asks which one it took.

## Case 1 — two layers, one observable

`UnwindWedgedExchange` refuses a blank reason before it opens a transaction. The test asserted the
effect: an error came back, the exchange survived, no evidence row was written.

Delete the Go guard and it stays **green**. Migration 0024's `CHECK (btrim(reason) <> '')` rejects
the insert instead, the transaction rolls back, and the observable state is byte-for-byte identical.
The test was a true statement about the system and said nothing about either layer.

The fix was to make the two layers *distinguishable*, which meant finding something they do not
share. A guard that runs first never touches the database; one that runs later must. So the test now
calls the function with a **closed connection pool**: a guard running first still reports the reason
problem, and a guard running later reports the connection. Two tests now, one per layer, because they
defend against different writers — the Go guard is an argument contract for this service's callers,
the CHECK constrains anyone inserting directly.

## Case 2 — an ordering claim that no error can carry

`recovery_operations_test.go` states a house contract for operator subcommands: every argument case
is refused **before `sql.Open` is reached**. I copied the framing for two new commands and wrote a
test to pin it.

It was untestable, and it took two attempts to work out why. First attempt: assert the right error
comes back. Green with the checks moved below `sql.Open`. Second attempt, on a theory: set
`DATABASE_URL` to an unparseable DSN so `sql.Open` fails eagerly, and a later check would then never
run. Also green — because **`sql.Open` with pgx never errors**. It stores a DSN string and returns a
pool; nothing is parsed and nothing is dialled until first use. No value of `DATABASE_URL` can make
the two orderings produce different errors.

What *is* observable is a **TCP connection attempt**. `DATABASE_URL` now points at a listener the test
owns, and the assertion is a connection count: a check that runs first leaves it at zero, one that
runs after the store call has already dialled. That is a fact about what the code *did* rather than
about what it returned, and it catches the mutation — verified by deleting a validation branch and
watching the count go to one with the confusing DSN error in the message.

The pre-existing tests in `recovery_operations_test.go` carry the same untestable framing. Left
alone — not that ticket's file — but the comment there is a claim its tests cannot support.

## The question that finds both

Not *"does this test fail when the feature breaks?"* — both of these did. Ask instead:

> **If I delete this specific mechanism and something ELSE produces the same observable, does the
> test notice?**

If two layers can produce one outcome, the test must assert something only one of them can produce.
If nothing distinguishes them, the ordering or the layering is not a testable claim and should not be
written down as though it were — say what is actually pinned instead.

The corollary is worth stating because it is the cheaper half: a claim about *ordering* almost never
survives as an assertion about a **return value**. Ordering is visible in side effects — a connection
opened, a row written, a call made — and if none of them differ between the two orderings, the two
orderings are the same program.
