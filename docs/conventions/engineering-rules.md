# Engineering rules

Binding review and test rules. Read the sections that apply to the change. Project context,
gate operation, workflow authority, and required ADR triggers are in [AGENTS.md](../../AGENTS.md).
Incident details stay in [learnings](../LEARNINGS.md). Code comments that cite AGENTS.md for
a test or review rule refer to the rules in this file.

## Test placement and assertions

- Test each mechanism at the tier that enforces it. A fake that scopes rows in Go cannot prove a
  SQL tenant predicate. When enforcement moves tiers, move the fake's contract too.
- Assert client status, response body, and displayed behavior where classification occurs, even
  when the failing mechanism lives deeper. A store test cannot prove an HTTP error mapper.
- Derive expected values from requirements, never observed output. Prefer an invariant such as
  "a draw-down preserves redemption units" to a number copied from a run. Mutation tests cannot
  detect a test that blesses the defect. Cross-model review remains required by the ticket flow.
- Give each guard predicate its own case, with earlier predicates satisfied. Mutate each predicate
  separately. A refusal for the wrong reason does not prove the intended guard.
- For database refusals, satisfy other constraints and assert the exact constraint name. Scope
  catalog lookups by relation. A SQLSTATE alone does not identify the constraint.
- Before debugging a red test, check that its fixture can express an allowed input and the state
  the test needs. Before trusting green, remove the mechanism and confirm the relevant test fails.
- Ask of every fixture seed what would fail if that seed were removed. One assertion over a fee
  cannot prove a separately seeded split schedule.
- Prove a new coverage test catches a gap that existing checks miss. Run a case where the new
  test fails and pre-existing guards pass. A lower-tier failure before the new test runs proves
  only the lower tier.
- Assert the values real code observed. A callback or injected write can pin instrumentation
  while production reads the wrong snapshot. Make probes report by defer so failure runs report.
- Test removal of the mechanism from its production call site as well as damage to the component.
  A test that builds its own provider cannot prove `Setup` installs the processor.
- Record and assert fake inputs when the real answer depends on them. A fixed-answer stub cannot
  expose a wrong lookup key.

## Mutation, determinism, and reachability

- After every fixture change, run the relevant compiling mutation and confirm red, then run
  6–8 unmutated repetitions. Check both properties every time. Force scheduling with explicit
  coordination where the fixture depends on concurrency. No observed flake is not a proof.
- Confirm the mutant builds and the intended test executes. Bypass cached test results when
  external state changes. Recreate the database for changed unreleased migrations; goose does
  not rerun an edited migration already applied to the fixture.
- A green mutation can mean a blind fixture, an inert mechanism, or a mutation that missed the
  distinguishing input class. State an input where the mechanism changes the result.
- Distinguish structural unreachability from temporary policy. Delete a structurally inert guard.
  Keep a guard shadowed by a policy planned for removal, and reach it by varying the unpoliced
  input. A test that pins the currently observed refusal can bless a temporary limitation and fail
  when the guard starts working. Derive its expected behavior from the requirement. Before deleting
  an exported symbol, check the sentinels and classifications it produces.
- Compare accepted input classes before calling guards redundant. For malformed input, enumerate
  syntax, type, absence, identity, and range. A type error can populate a partially decoded value.
- For redaction, fill every generated position with the forbidden value. Sanitize secret shapes,
  including rejected route spellings. A route-based check inherits normalization gaps.
- A minimum, branch, or winning-arm assertion proves only the branch it reaches. Choose values
  that distinguish each branch and each swapped position.

## Authorization and write boundaries

- Scope a conditional write's failure-path reads as well as the write. Otherwise a refused write
  can still disclose another organizer's state.
- When a field must not be client-chosen, remove it from the submitted type. A hidden field is
  client-controlled. Derive protected values from authenticated server state.
- Hidden inputs always submit. Present-and-empty is not absent. Apply absent-means-false only
  to controls that can actually be absent, such as unchecked checkboxes.
- For full-set replacement, test both unauthorized addition and unauthorized omission. Omission
  deletes a row. Verify what the downstream service actually validates.
- When a request value starts deciding capacity, permission, price, or routing, enumerate every
  producer and its authentication. Enforce the rule where the decision occurs, under the
  authoritative lock when needed. Caller-side validation binds only that caller.
- Optimistic tokens must come from the page state the client acted on and return unchanged.
  Comparing two reads within one request is inert. After a refusal, re-render the submitted token,
  or a second click can apply the refused change. A fixture must use the real version-changing write.
- SQL guard predicates must fail closed for NULL. Use `IS DISTINCT FROM` where unknown must count
  as different. Scope drain checks to client backends that can write, excluding maintenance workers.
- Execute security hypotheses before claiming a guarantee. If an out-of-scope gap remains, pin
  its presence with an honest regression test and link the governing ADR.

## Classification, fixes, and claims

- Before adding an error, inspect `errors.As` shape matches as well as named sentinels. An
  anonymous structural interface matches every later error with that shape. Put a new case
  before any broad case it satisfies when that broad classification would be wrong.
- Before narrowing a condition, evaluate what only the removed arm covers. An apparently broad
  schema check may be the only retry path for a real outage. Fix classification before narrowing.
- Review the interaction of a batch of fixes. A bounded or optimized replacement is a new
  implementation; identify what the replaced version would have caught. Test reachable fixture
  states before blaming the new code.
- Re-read prose and comments added during fixes. Verify every named function and proposed design
  alternative against actual code. A design space has no compiler; label unchecked alternatives.
- Preserve pure-input guard semantics even if the database currently prevents that input. Check
  the guard at the seam it protects before reordering or deleting it.
- State the adversary for integrity claims. Database state cannot constrain a database writer.
  Lifecycle rollback and current-key compromise remain outside the closed guarantees in ADR-021.
  Keep the test that demonstrates the rollback gap until the guarantee and ADR change together.
- Append every ticket lifecycle event through `store.appendLifecycle`. Direct inserts break
  one-to-one verifier coverage. Golden canonical bytes require a canonical-version migration,
  not an expected-value update. Read the [lifecycle runbook](../runbooks/ticket-lifecycle.md).

New tests receive the same implementation and review scrutiny as production changes.

## Evidence references

The source incidents and their ticket IDs are indexed in [LEARNINGS.md](../LEARNINGS.md).
Key proof boundaries are [ADR-017](../adr/ADR-017-domain-event-schema-evolution.md),
[ADR-021](../adr/ADR-021-ticket-lifecycle-trail-integrity.md), and
[ADR-029](../adr/ADR-029-seat-identity-pinning-contract.md).
