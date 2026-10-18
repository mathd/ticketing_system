# Sprint brief

Fill every line. Keep the original scope. Append each approved change with its reason and time. The
board's member list stays the authority. Show the stories and the closure task apart.

- Vault and sprint key:
- Name and goal:
- Planned start and end, with the offset the human agreed:
- Review: `review: "full"` by default; `review: "none"` only with an owner-approved `reviewReason`:
- Tag publication: `release: true` or `release: false`:
- Owner priority approval for the exact member scope, or manual Ready transitions:
- Members in dependency order, and the closure task (full review):
- Outcome that people can observe, acceptance criteria, exclusions and useful subsets:
- Baseline SHA for full review, and proposed tag only for `release: true`:
- Shared architecture choices and the governing ADRs:
- Dependency inspection: the selected maintenance work, or `no justified update`:
- Required story, integration, end-to-end and LLM checks, and the work that creates missing suites:
- Continuation: `autonomous` or `manual`, approved by the owner:
- Continuation owner, expected review calls, required gate runs and exception triggers:
- Authorized environments, fixtures, paid-call limits and repetition policy:
- Report language, `fr` or `en` (full review; from `sprints.reportLanguage`):
- Named human close approver:
- Scope digest that the human approves (from `sprint_state.py progress`):
- Digest of the brief that this one supersedes, or `none`:
- Human approval: name, time, and what the human said:

After the human approves, post the brief as a `sprint-brief` record. End the body with the JSON
block that `references/records.md` defines. The start request is a separate record
(`sprint-start`).
