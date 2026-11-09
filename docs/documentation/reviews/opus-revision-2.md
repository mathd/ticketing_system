# Opus 5.5 revision review 2

Reviewer: Opus 5.5 (`claude-opus-5-5`), review sub-agent, high effort. Findings only. I edited no
tracked file. I made no commit and started no gate or subagent. I wrote nothing to the network.
The only files I wrote are this report and a scratch copy of the baseline ADR-085 outside the
repository. I applied the unslop skill to this report.

Date: 2026-11-09. Candidate: `f4eedf94df44fd37028714cdbdf903f31421eed6`. Revision 1: `771a5c1a`.
Baseline: `57dc4f4e`. Grid: `docs/documentation/evaluation.md`, sha256
`3ee6fb7a083781d31009693c44c79bce1cc91b6dadb159bdcf3b65e693b83c00`. I computed this hash after the
gate finished, and it matches. I read the grid in full. The worktree was clean at the candidate.

## Gate

Another process ran `make check` on the clean candidate. It started at 06:33:28Z. I did not start
it. `.gate.verdict` after it finished:

```text
PASS exit=0 head=f4eedf94df44fd37028714cdbdf903f31421eed6 tree=b916d86c480a6fe652abc34a0eccefe8bba9b7c3
```

All stages ran, through smoke. They include the three `doc-namespace.test.ts` checks,
`check-markdown-links` and `check-adr-numbers`.

While the gate ran, `.claude/hooks/guard-bash.py:28-38,90-100` blocked all my shell commands,
including reads under `/tmp`. For that period I worked only with the Read tool. After the lock
cleared I ran the git checks below.

## Scores

| ID | Metric | Opus rev 1 | Opus rev 2 | Main reason |
|---|---|---:|---:|---|
| M1 | Accuracy | 7 | 8 | Go gate rule, credential scopes, worker overlay, workflow-YAML wording and the recovery pointer now match source and the earlier source checks. Small leftovers only (N2, N6). |
| M2 | Authority and currency | 8 | **7** | The new ADR-085 citation rule assigns 10 decision references to the wrong source ticket (N1). This is a material defect in decision authority. Capped at 7. |
| M3 | Findability | 8 | 9 | Redirect headings for durables and password recovery exist. All five anchors I checked resolve. `solution-design.md` routes recovery to the runbook index. |
| M4 | Task execution | 7 | 8 | The setup page states the Go minor-version gate. The worker overlay and the staff credential set are explicit. The scanner synopsis meets the test contract, and the gate passed. |
| M5 | Coverage | 9 | 9 | Unchanged. The Access → Commerce row now names all three call purposes. |
| M6 | Contract and safety precision | 8 | 8 | Scanner time bounds, page-loss warning, credential boundaries and the Compose-only premise are intact. N1 changes decision citations, not rules or bounds, so I did not cap M6. |
| M7 | Token efficiency | 8 | 8 | Entry sizes stay low. Skill and scanner costs are disclosed (`results.md:112-118`). The ADR-085 entry grew from 13,036 to 14,077 bytes through qualification. |
| M8 | Clarity and structure | 8 | 8 | LEARNINGS titles and legacy notes are clean. One em-dash leftover and one garbled sentence remain (N4, N5). Notation such as "ADR-085 D33-ADR-085 D37" is heavy but is defined. |
| M9 | Reference integrity | 7 | 8 | The executable consumer contract passes in the gate. The link check passes. Old heading aliases keep the changed ADR-085 anchors. Non-Markdown referrers have redirect headings. The wrong decision referents are counted under M2. |
| M10 | Maintainability and provenance | 8 | 8 | The maintenance guide covers non-Markdown consumers and names the test. The evidence correction note is in place. The proof cannot detect N1, and `results.md` claims more than the proof shows (see design judgment). |

Result: the owner bar is **not met**. M2 is 7 because of N1. The other nine metrics are 8 or 9.
No critical or high finding. N1 is medium. The fix is documentation-only and needs no owner
decision.

## Revision-1 findings: status at the candidate

| Finding | Status | Evidence |
|---|---|---|
| Opus D1 / Sol R2, test contract | Fixed | `development.md:151-160` holds the tested H3 and four safety sentences. The gate PASS includes the three DOC-01 tests. |
| Opus D2, Go | Fixed | `development.md:8-11`; `go.work:1` is `go 1.27.3`. |
| Opus D3, non-Markdown referrers | Fixed | `development.md:131-141` adds the durable and password headings, and both anchors exist. `maintenance.md:36-47` adds the `git grep` step and names the DOC-01 test. |
| Opus D4 / Sol R4, punctuation | Mostly fixed | One leftover remains: N4. |
| Opus D5, recovery pointer | Fixed | `solution-design.md:42-43`. |
| Opus D6, worker overlay | Fixed | `configuration.md:75-76`, `configuration-reference.md:72-77`. |
| Opus D7, ADR amendments | Fixed | `adr/README.md:32,51,56,73`. |
| Opus D8, historical markers | Fixed | `LEARNINGS.md:101-108`. |
| Opus D9, AGENTS losses | Fixed | Owner line and "React" in AGENTS; `engineering-rules.md:46-47,98`. |
| Opus D10, Access edge | Fixed | `architecture.md:113`. |
| Opus D11, correction note | Fixed | `verification/on-sale-load/README.md:7-9`. |
| Opus D12, skill cost | Fixed | `results.md:112-118`. |
| Opus D13, CI term | Fixed | `continuous-delivery.md:1`. |
| Sol R1, credential scope | Fixed | `staff-accounts.md:62-70` agrees with `configuration.md:115-122` and `architecture.md:106-114`. I did not re-read source in this pass. Sol checked the Payments and Catalog code in revision 1. |
| Sol R3, workflow YAML | Fixed | `scheduled-workflows.md:87-90`. |

## Defects

### N1. The new ADR-085 citation rule assigns decisions to the wrong source ticket (medium, M2 cap)

Metrics: M2 (cap 7), M10.

The baseline rule (`57dc4f4e`, ADR-085 line 86-89) was: "A TKT-399 decision is always written
'TKT-399 Dn'. A bare 'Dn' … is a TKT-556 decision." Revision 2 replaced it
(`scanner-outcomes/baseline-evidence.md:16-21`): an `ADR-085 Dn` has the TKT-556 ticket as its
source "unless an explicit `of TKT-399` or other ticket qualifier follows it". TKT-399, TKT-556
and TKT-557 each have their own D-series, and the numbers collide.

The baseline had nine places where one ticket qualifier came before the number, either as a
slash list or as a noun phrase. Revision 2 put `ADR-085 ` between the ticket and the number,
and it added no trailing `of TKT-n`. Under the new rule each of these now reads as a TKT-556
decision. I found all nine with a regex over the baseline file and matched each to the
candidate:

| Baseline wording (line) | Candidate | Reads under new rule as | Meant |
|---|---|---|---|
| "owner history TKT-399 D29/D30" (370) | `outcome-tables.md:29` "…ADR-085 D29 of TKT-399/ADR-085 D30" | TKT-556 D30, replaced by D32 | TKT-399 D30, the owner time rule |
| "the owner's TKT-399 D29/D30 limits" (858) | `decision-log.md:69` | same | same |
| "TKT-399 history D33" (382) | `outcome-tables.md:41` "TKT-399 history ADR-085 D33" | TKT-556 D33, producer matrix | TKT-399 D33 |
| "TKT-399 history D7/D11" (585) | `outcome-tables.md:244` "…ADR-085 D7/ADR-085 D11" | TKT-556 D7 and D11 | TKT-399 D7 and D11 |
| "TKT-399 history D18" (624) | `finding-map.md:27` | TKT-556 D18, PENDING-first refusal | TKT-399 D18 |
| "TKT-399 history D18" (719) | `storage-upgrade.md:17` | same | TKT-399 D18, the version barrier |
| "TKT-399 history D39" (770) | `delivery-plan.md:13` | TKT-556 D39, lookup bound (`decision-log.md:109`) | TKT-399 D39, stop and design first (`decision-log.md:22`) |
| "TKT-557 deviation, D21" (779) | `delivery-plan.md:22` | TKT-556 D21, visible recovery (`decision-log.md:86`) | TKT-557 D21, sslmode deviation |
| "TKT-557 D29/D33" (788) | `delivery-plan.md:31` "ADR-085 D29 of TKT-557/ADR-085 D33" | TKT-556 D33 | TKT-557 D33 |

The table has nine places and ten references, because the D7/D11 row holds two.
`delivery-plan.md:31`'s "(ADR-085 D20 of TKT-557)" is correct. It had a single qualifier.

The current text also shows several of these conflicts with no baseline needed. For example,
`baseline-evidence.md:20-21` says that TKT-556 D30 "was replaced by ADR-085 D32" and that the
owner time rule is "ADR-085 D30 of TKT-399". Yet `outcome-tables.md:29` credits the 3 s/30 s
values to a bare "ADR-085 D30".

A related citation existed before this revision: `configuration.md:171` "(TKT-557, ADR-085
decision D21)" has the same text at `57dc4f4e`, `771a5c1a` and `f4eedf94`. Fix it in the same
pass.

Why the proof did not catch it: `verify-namespaces.py:9-13` takes the ticket from `TKT-n `
directly before a `Dn`, or from ` of TKT-n` directly after it. Otherwise it assigns `556`. In
"TKT-399 D29/D30" it assigns D30 to 556, and in "ADR-085 D30" it also assigns 556. The two
versions match, so the check passes. I reran the script after the gate, and it prints "Decision
identity and source tickets preserved in 10 files" for a tree that has these ten errors. The
script shows that every token keeps its order and every adjacent qualifier is unchanged. It
does not show that each reference keeps its source ticket. These claims therefore overstate the
evidence: `results.md:57-58`, `results.md:105-106`, `results.md:122-126`, and the TSV column
`same_identity_and_source_ticket`.

Impact: no rule, bound or outcome changed. A reader who applies the documented rule to the
accepted scanner ADR finds the wrong decision for the time limit, the design-first stop, the
storage barrier, the producer origin and the sslmode deviation. Several of the wrong targets
are replaced or unrelated decisions. M2 requires that readers can tell which decision binds,
so M2 is capped.

Required fix (documentation only; it passes `doc-namespace.test.ts:27` because the prefix
stays):

- Add `of TKT-399` or `of TKT-557` to every reference in the table, for example
  "ADR-085 D29 of TKT-399/ADR-085 D30 of TKT-399" and "TKT-557 deviation, ADR-085 D21 of
  TKT-557". Also fix `configuration.md:171`.
- Rebuild the proof. Take the source ticket from the baseline with the slash-list and noun-phrase
  forms above (for example `TKT-n (history )?Dn(/Dn)+` and `TKT-n deviation, Dn`). Then compare
  the candidate with that list, not with the same default.
- Narrow the `results.md` claims until the rebuilt proof passes.

### N2. A historical ADR sentence still points operators to `development.md` (low)

Metrics: M3, M1. `scanner-outcomes/baseline-evidence.md:60-61`: "`docs/development.md` gives
operators the same list." The development page now holds only the synopsis. Add "at that
baseline; see the scanner runbook". The ADR References at `ADR-085…md:283` point to the
development redirect, not to `runbooks/scanner.md`. That adds one hop.

### N3. Browser specs quote AGENTS wording that no longer exists (low)

Metrics: M9, M10. `test/browser/{channels,organizer-assertion,reset-password,slot-allocations}.mjs:3`
quote AGENTS.md: "A web-UI ticket isn't verified until a browser has *submitted* its forms."
Revision 1 shortened AGENTS. The rule still exists in AGENTS, but in other words. `git grep -F
'AGENTS.md:' -- ':!*.md'` gives 16 hits. The maintenance step searches for moved paths, not for
quoted text. Add "search for quoted instruction text" to `maintenance.md:36-47`. The comments
are source files, so changing them is outside a documentation-only scope.

### N4. One em-dash leftover in the staff runbook (low)

Metric: M8. `docs/runbooks/staff-accounts.md:95`: "inventory will keep refusing to start ,
delete the line". Revision 1 introduced it, and revision 2 did not fix it.

### N5. A garbled sentence about database passwords (low)

Metric: M8. `docs/configuration.md:205-206`: "They are applied on **first boot only** — the init
scripts run against an empty data directory — `make down` deletes the volumes and their data; it
is a disposable-stack reset…". Revision 1 replaced the baseline ending ("— so rotate with `make
down` … or an `ALTER ROLE`") and left the dash construction broken. Split it into sentences.

### N6. README says plain `docker compose up` "works as usual" (low)

Metrics: M4, M1. `README.md:15` says this, and `README.md:23` then links Grafana on port 3000.
Base `compose.yaml` publishes only the gateway (`configuration.md:212-216`), so without the
direct-ports overlay the Grafana link fails. `development.md:44-52` has the right command.

## Design judgment

Revision 2 fixed the revision-1 design problem. Tests, runtime messages and Compose comments are
now incoming references in the maintenance procedure. The development page keeps a stable
synopsis for the executable consumer, and the gate passes. N2 to N6 are local.

N1 is a design problem, not a local slip. Two choices together caused it:

1. A default-owner rule replaced context that the original prose carried in the sentence. "TKT-399
   D29/D30" and "TKT-557 deviation, D21" were clear in context. Inserting `ADR-085 ` between the
   ticket and the number removed that context, and the default rule then took over.
2. The proof checks the text with the same default that it should test. A check that shares the
   change's assumption cannot find an error in that assumption. DOC-01 has a similar limit: it
   accepts any `ADR-085 ` prefix and does not check the referent.

A better invariant: every `Dn` whose number exists in more than one ticket series names its
ticket. Check that against a table of colliding numbers. A test change for this would be the
owner's decision. The documentation fix in N1 does not need one.

## Evidence and commands

After the lock cleared:

- `cat .gate.verdict`, `git rev-parse HEAD`, `git status --porcelain` (clean), `sha256sum` on the grid.
- `git show 57dc4f4e:docs/adr/ADR-085-scanner-local-outcome-model.md` to scratch. A regex search
  for slash-list and noun-phrase ticket forms gave the nine places in the N1 table.
- `git show 771a5c1a:…/baseline-evidence.md` lines 14-20: revision 1 kept the baseline rule.
- `git grep` for unqualified `ADR-085 D(18|20|21|29|30|33|39)` across the ten ADR-085 files,
  `configuration.md` and the runbooks. I checked each hit against the baseline table. The other
  hits are correct TKT-556 references.
- `git show` of `configuration.md` and `staff-accounts.md` at the baseline and revision 1, for N1,
  N4 and N5.
- `python3 -I /tmp/ticketing-documentation-review/verify-namespaces.py`: passes (see N1).
- `git grep -F 'AGENTS.md:' -- ':!*.md'`: 16 hits.

## Inspection scope and limits

Read in full at the candidate: `docs/documentation/evaluation.md`, `results.md`, `maintenance.md`,
`namespace-proof.tsv`, both revision-1 reports, `docs/README.md`, `docs/development.md`,
`web/scanner/test/doc-namespace.test.ts`, the ADR-085 stub, `scanner-outcomes/baseline-evidence.md`,
`decision-log.md`, `delivery-plan.md`, `invariants.md`, `docs/runbooks/staff-accounts.md`,
`docs/configuration.md`, `docs/conventions/engineering-rules.md`, `docs/architecture.md`,
`docs/adr/README.md`, `docs/LEARNINGS.md`, `docs/learnings/legacy-index-notes.md`,
`docs/known-limits.md`, `docs/runbooks/README.md`, `.claude/skills/README.md`, `docs/records.md`,
root `README.md`, `go.work`, the proof script and `.claude/hooks/guard-bash.py`. AGENTS.md and
CLAUDE.md were in context. 30 files.

Read in part: `solution-design.md` (1-56), `on-sale-load/README.md` (1-30),
`continuous-delivery.md` (1-12), `scheduled-workflows.md` (80-91), `scanner.md` (1-110),
`customer-accounts.md` (160-179), `nats.md` (1-15), `configuration-reference.md` (60-89),
`testing.md` (1-45), `state-model.md` (1-40), `storage-upgrade.md` (1-30), `finding-map.md` (1-25),
`outcome-tables.md` (1-30), `tests-and-mutations.md` (1-20), `test/browser/reset-password.mjs` (1-45).
The rest of the ADR-085 parts were covered only by the git greps in N1.

Not done: I did not read the full `git diff 771a5c1a f4eedf94` line by line. I checked revision 2
through the files above, the git greps and the revision-1 finding list. I did not run my own
anchor scan; I rely on the gate's link check and five anchors that I checked by hand. I did not
re-check source code, read the other 83 ADRs, the learnings, the skills, or the evidence and
sprint records beyond earlier reviews, or open images, external URLs or board state.

Reused, with limits: the structural coverage and 21 source checks from my revision-1 review
(`reviews/opus-revision-1.md:258-320`), and Sol's revision-1 source checks for R1. This is not a
full semantic read of the corpus. Pending final bookkeeping in `results.md` is not a defect, and
I did not reduce a score for it.

## Conclusion

The candidate does not reach 8 on every metric. M2 is 7 because of N1. The other nine metrics
are 8 or 9, and the gate passes on `f4eedf94`. To reach the bar:

1. Fix N1: qualify the ten references and `configuration.md:171`, rebuild the proof, and narrow
   the claims in `results.md`.
2. Fix N2 to N6 in the same pass.
3. Get a fresh review and a gate PASS on the new commit.
