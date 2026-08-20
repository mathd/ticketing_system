# A test that pins the harness, not the contract

**2026-08-19 — TKT-254**

`AGENTS.md` already says: *before trusting a green test, ask whether its fixture can reach the state
that would fail*, and *delete the mechanism and re-run — if it stays green, the test is about
something else*. This is the same rule meeting a case where **the mechanism is still there**, the
fixture **can** reach the failing state, and the test is still about something else — because the
assertion is a fact about the **test harness** rather than about the code's **contract**.

**Ask what your assertion is a fact about. If it is a fact about the test, it is not a fact about
the code.**

## The instance

`Journal.Verify` scanned `journal_entries` and then `journal_heads` as two statements on a pooled
`*sql.DB` — two snapshots. A legal append committing between them made the head scan see a sequence
the entry scan never had, so an intact journal was reported as `journal head mismatch`. The fix was
to put both scans in one `REPEATABLE READ` transaction.

Testing that needs a concurrent commit at a precise instant, which cannot be arranged from outside
the function. So the first attempt added a seam:

```go
func (j *Journal) verify(ctx context.Context, afterEntries func()) error
```

`afterEntries` ran between the two scans; the test passed a callback that appended, then asserted the
append had landed:

```go
if appended.Sequence != 2 {
    t.Fatalf("interleaved append landed at sequence %d, want 2; the fixture did not construct the race", appended.Sequence)
}
if err != nil {
    t.Fatalf("a legal append between the two reads was reported as corruption: %v", err)
}
```

That looks careful. It even has a guard against the fixture not constructing the race — the exact
precaution this rule usually asks for. It was observed **red** before the fix, for the right reason,
with the right message.

**It still could not fail.** An adversarial reviewer proposed a *coordinated* reversion: remove the
transaction **and** move the callback above the entry scan. Applied and run — the test passed with
the defect fully live. Both queries then saw sequence 2, both assertions held, and production was
back to two snapshots.

## Why the guard did not help

`appended.Sequence == 2` is a fact about **when the callback ran relative to the append**, and
`err == nil` is a fact about **the final verdict**. Neither is a fact about **where the two reads
happened**. The seam let the test *place* an event; the test then asserted the event had been
placed. That is a closed loop — the harness confirming itself — and the production behaviour it was
supposed to pin was never observed at all.

The tell, in hindsight: every assertion could be satisfied by editing the *seam* rather than the
*logic*. When a test's assertions are all satisfiable by moving the instrumentation, the
instrumentation is what is under test.

## The fix — assert what the code observed, not what the test did

The seam became a **probe that reports what each scan actually read**:

```go
type verifyProbe struct {
	afterEntries func(seqByOrg map[uuid.UUID]int64)
	afterHeads   func(headByOrg map[uuid.UUID]int64)
}
```

and the test asserts the two **agree**:

```go
if sawHeads != sawEntries {
    t.Errorf("the two scans disagree: entries saw sequence %d, heads saw %d — they did not read one snapshot",
        sawEntries, sawHeads)
}
```

A shared snapshot makes the head scan blind to an append that commits after the entry scan, so the
two match. Two snapshots make them differ by exactly that append. **No placement of the callback can
manufacture agreement out of two snapshots, because the divergence is created by the commit, not by
the call.** The coordinated reversion now fails, and both single mutations (head scan moved back to
the pool; `REPEATABLE READ` weakened to `READ COMMITTED`) fail on that assertion by name.

One detail that mattered: the head probe is reported by `defer`, so it fires **on the mismatch return
too**. Reported only after the loop, it never ran on precisely the runs where the contract was
broken, and the test could only say *"the seam did not run"* instead of naming the divergence.

## Why nothing local caught it

Not types, not the gate, not mutation testing of the production code — every mutation of `store.go`
*was* caught, because each broke the mechanism the test was watching. What the test could not
survive was a mutation of **itself plus** the code, and no local tool proposes that pair. It took a
reviewer that had not written the test, reading it as an adversary, asking *which edit does this
fail to catch?* — the same reason `AGENTS.md` calls cross-model review a prerequisite rather than an
option.

## The rule

- **Ask what each assertion is a fact about.** "The callback ran", "the append landed", "no error
  returned" are facts about the harness and the verdict. "The head scan read sequence 1" is a fact
  about the code.
- **A seam that lets a test place an event must also let it observe an outcome.** If the test can
  only place, it can only assert placement.
- **Try the coordinated reversion, not just the single mutation.** Revert the production change *and*
  adjust the seam the way a careless refactor would. A test that survives single mutations but not
  that pair is pinning its own instrumentation.
- **Prefer an invariant stated without naming the implementation.** *"The two scans agree about the
  journal"* survives a rewrite; *"the callback ran between the two queries"* describes today's code
  and nothing else.
