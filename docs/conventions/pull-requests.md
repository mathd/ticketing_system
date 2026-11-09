# Pull requests and review

Scope: changes to this repository. A merge does not deploy an application.
Use [commit and branch conventions](commits-and-branching.md).

## Workflow authority

Ticket work follows [.claude/sdlc.config.json](../../.claude/sdlc.config.json) and the
[ticket skill](../../.claude/skills/sdlc-ticket/SKILL.md). In human mode, the owner takes the
workflow gates. In autonomous mode, the agent takes gates 2–4 after the required independent
reviews and evidence; priority remains owner-controlled. Never override a High or Critical
blocking review finding on an autonomous run. Fix it or escalate it.

Internal documentation, ADR, process, and tooling maintenance can use the skill's direct
maintenance path when authorized. A session request for a PR takes precedence over that default.
Do not invent a product ticket for unticketed maintenance. Session authorization still controls
commits, pushes, merges, and deployment. A requested PR authorizes its required branch commit
and push, not a merge.

## Before opening a PR

1. Keep one outcome per PR. Aim for about 250 changed code lines; owner-approved larger work
   needs a review guide and risk-ordered commits. Open a large PR as a draft early when useful.
2. Check the active GitHub account against [AGENTS.md](../../AGENTS.md) and the remote before
   using `gh`. Preserve unrelated work and stage explicit edited paths.
3. Sync with `main` and resolve conflicts before final verification. Never rewrite `main`.
4. Review every changed line, including generated code, docs, and test fixtures. Apply the
   [engineering rules](engineering-rules.md) and confirm requirements at their enforcing tier.
5. Run `make check` on the final tree. For changed write forms, also add a submitting browser
   spec and run `make browser`. Describe any unmet verification requirement explicitly.

## PR description

State the concrete problem and resulting behavior. Include the ticket when there is one,
scope, material risks, validation results, and documentation or ADR impact. For maintenance,
state that scope directly. There is no PR template.

Keep the description consistent with the final diff. Explain compatibility changes to HTTP,
events, migrations, or existing clients. Do not add AI attribution or co-author trailers.

## Review and merge

Review correctness, design boundaries, failure paths, authorization, money, compatibility, and
whether tests can detect removal of the real mechanism. Review fixes together, then re-read
claims added while fixing them. State whether findings reveal a design problem.

Validate suggestions against the actual code and accepted ADRs. Resolve factual disagreements
with evidence. Personal style preferences do not block a PR unless project policy requires them.
The owner remains accountable for the result; autonomous mode changes who takes a gate, not the
required evidence.

The `main` ruleset requires a PR and a successful GitHub Actions `check`. The `mathd` admin can
bypass those host controls; agents must follow the workflow anyway. Never use `--admin` or the
bypass checkbox. See [CI policy and remaining guard limits](continuous-delivery.md#main-ruleset).
Do not merge unless the current session authorizes it and the configured review gates pass.
