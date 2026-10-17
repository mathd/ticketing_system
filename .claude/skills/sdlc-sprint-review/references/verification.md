# Test relevance and real-system verification

Map each release criterion to the smallest useful test that observes the real behavior. Read the
tests and the production boundary that they claim to protect. Look for:

- assertions that repeat another test and cover no new risk,
- mocks that enforce a rule that production does not have,
- tests that only mirror the implementation or chase a coverage number.

A recommendation to remove a test must name the remaining test that protects the behavior.

Inspect the real integration and end-to-end evidence for the critical journeys. Include the failures
that someone found by hand. For a change that uses an LLM, keep two things apart: deterministic
contract checks, and variable evaluations of task quality. A mocked or recorded response cannot show
that the real model works today. Check the model, the configuration and the fixture versions, the
success criteria of the domain, the approved call budget, the trial policy, the failures that were
observed and the traces. Do not recommend a retry until the result is green. Do not recommend a
change to the acceptance criteria after you saw the result.

An exhausted budget or an unavailable environment is incomplete verification. It is not a pass. It
is not permission to spend more.

Review coverage, complexity and the changed hotspots against the baseline, with the real tools and
artifacts. Name each tool or datum that is missing. A metric is evidence to investigate. This
workflow sets no global coverage target, no complexity target and no ratio of unit tests to end-to-end
tests. Read the relevant tests before you ask for more tests because a metric is high.

## Who runs what

The primary agent runs the commands, inside the authorization the brief gave. The reviewer reads the
results and asks for the evidence that is missing. A required check that is missing or failed leaves
the release incomplete or blocked. "Not applicable" needs a real scope reason. Missing credentials,
budget or infrastructure never make a check not applicable.

## What you return

Return the shared result for the area `verification`. Set `stage` to `sprint-review` and `area` to
`verification` on the record and on both lens results. State the behavior that was tested and the
behavior that was not, the actual evidence, and bounded recommendations. The report must tell apart
the tests that ran, the checks that did not run, the technical failures and the quality shortfalls.
