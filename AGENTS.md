# Ticketing system

This is the owner's AI-assisted development testbed. The owner has built three ticketing
platforms and sold tens of millions of tickets. Treat them as the domain expert. Do not explain
ticketing basics; surface non-obvious engineering trade-offs. Follow the
selected delivery process so the experiment remains valid.

## Read for this task

Start at [docs/README.md](docs/README.md). Load only relevant task references.

| Task or change | Required reading |
|---|---|
| Any implementation or review | [Engineering rules](docs/conventions/engineering-rules.md), including test and review rules |
| System boundaries or ownership | [Architecture](docs/architecture.md), [decision index](docs/adr/README.md) |
| Setup, configuration, recovery | [Development](docs/development.md), [runbooks](docs/runbooks/README.md), [configuration](docs/configuration.md) |
| Tests or fixtures | [Testing](docs/testing.md), engineering rules above; place each assertion at the tier that enforces it |
| Ticket pipeline | [.claude/skills/sdlc-ticket/SKILL.md](.claude/skills/sdlc-ticket/SKILL.md) and only the stage references it names |
| Domain-event payload or consumer | [ADR-017](docs/adr/ADR-017-domain-event-schema-evolution.md), dispatch on schema before decoding data |
| Catalog slot transition | [ADR-018](docs/adr/ADR-018-catalog-slot-transition-concurrency.md) |
| Scoped catalog read | [ADR-019](docs/adr/ADR-019-catalog-read-path-scoping.md) |
| Catalog migration index | [ADR-020](docs/adr/ADR-020-catalog-index-build-concurrency.md); ADR-022 alone does not permit concurrent index builds |
| Audit or integrity claim | [ADR-021](docs/adr/ADR-021-ticket-lifecycle-trail-integrity.md), name the adversary and the rollback limit |
| Ticket lifecycle event or canonical form | ADR-021 and the [lifecycle runbook](docs/runbooks/ticket-lifecycle.md); use `store.appendLifecycle`, preserve golden bytes and the rollback-gap test |
| Migration placement or invocation | [ADR-022](docs/adr/ADR-022-out-of-band-service-migrations.md), one-shot service jobs; ADR-008 still governs all other migration rules |
| Published seat-map edit or seat pin | [ADR-029](docs/adr/ADR-029-seat-identity-pinning-contract.md), immutable versions and family-scoped advisory locks; re-resolve under that lock |
| Scanner outcomes or offline behavior | ADR-025, ADR-066 and ADR-085 through the decision index; check staged delivery before claiming support |

## Always apply

- Follow the current request's scope. Investigation and review do not authorize source fixes.
- Write or refine the spec before implementation. Record design decisions in `docs/adr/`.
- Keep five Go services behind the Go gateway, TypeScript frontends, per-service PostgreSQL
  databases, React, NATS JetStream, and Docker Compose. A stack deviation needs an ADR.
- Money uses integer minor units and an ISO currency code. Never use floats on money paths.
- Enforce organizer scope on writes, reads, and failure paths. Derive protected identity from
  authenticated server state. Review every producer when a request value starts deciding access,
  routing, price, or capacity.
- On autonomous runs, fix or escalate every High or Critical blocking review finding. Never
  override one yourself. Owner authorization is required to proceed with an unresolved blocker.
- A security claim needs executed evidence. State what was checked and what remains unverified.
- Preserve unrelated changes. Stage explicit edited paths, never a whole directory with `git add -A`.

## Verification

Claude-specific [tool hooks](docs/technical-delivery-standards.md#tool-hooks-and-limits) add limited guards; do not assume they protect this runtime.

Run `make deps` on a clean clone, then `make check`. The gate holds `.gate.lock`, streams to
`.gate.log`, and writes `.gate.verdict` from both the exit status and log. `make check-all` is
internal and refuses to run without the wrapper's lock. Run the gate as a standalone command.
Never edit tracked files while it runs. For a background run, wait for `.gate.lock` to disappear,
then read `.gate.verdict`. Never use `pgrep -f` to wait for it.

Commit regenerated `openapi_gen.go` and `api-types.gen.ts` files before the gate, when commits
are authorized. `check-generate` compares generated outputs with HEAD.

For every changed back-office, storefront, or scanner write form, add a submitting spec under
`test/browser/` and run `make browser` with real host Chrome. Smoke submits selected forms but
cannot prove browser action, Origin, Referer, SameSite, navigation, or JavaScript behavior.
Assert visible submits, redirects, rendered results, or exact persisted values. Browser request
listeners cannot see upstream calls made by SSR handlers.

When a fixture changes, run the relevant mutation first and confirm red, then 6–8 unmutated
runs. Test each guard predicate independently after passing earlier predicates. A green mutation
can mean a blind test, an inert mechanism, or a blind mutation. Check the input classes and
whether unreachability is structural or only current policy before deleting a guard.

The full test, security, error-classification, and review requirements remain binding in
[engineering rules](docs/conventions/engineering-rules.md). Read its applicable sections before implementation
or review. Incident details are linked through the learnings index.

## Workflow and records

All documentation lives in this repository. Product briefs and PRDs are in `docs/product/`,
ADRs in `docs/adr/`, and learnings in `docs/LEARNINGS.md` and `docs/learnings/`.
See [documentation map](docs/README.md) for current guidance and historical evidence.

Ticket state is outside this repo, one note per ticket in a Fast Note Sync vault served by
`~/sources/sdlc-board`, `python3 server.py 8787`. Read and write it through the board HTTP API
specified in [.claude/skills/sdlc-ticket/references/local-tracker.md](.claude/skills/sdlc-ticket/references/local-tracker.md).
Never write the `sdlc-state` archive branch. The old `.sdlc/` stub is gone.
The `sdlc-ticket` skill and `.claude/sdlc.config.json` bind ticket workflow and configured roles.
Documentation maintenance follows that skill's maintenance exception; an explicit owner request
for a PR still requires a PR.

`main` requires a PR and a successful GitHub Actions `check`; see
[continuous delivery](docs/conventions/continuous-delivery.md). The repository admin `mathd`
can bypass the host rules, so host acceptance does not prove workflow compliance. Ticket work
must merge only after `check` passes, without `--admin` or a bypass checkbox. Direct pushes are
limited to authorized internal maintenance. Do not commit, push, merge, or deploy without session
authorization. Creating a requested PR includes its required commit and branch push; it does not
authorize a merge or deployment.
