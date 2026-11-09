# Opus 5.5 revision review 3

Reviewer: Opus 5.5 (`claude-opus-5-5`), review sub-agent. Findings only. I edited no tracked
file. I made no commit, started no gate and no subagent, and wrote nothing to the network. I
wrote this report, plus one scratch copy of the proof script and its output in my session
scratchpad. I did not modify the grid, the tests or any evidence under `/tmp/ticketing-documentation-review/`.
I applied the unslop skill to this report.

Date: 2026-11-09. Candidate: `1c49ddd3`. Revision 2: `f4eedf94`. Revision 1: `771a5c1a`.
Baseline: `57dc4f4e`. Grid: `docs/documentation/evaluation.md`, sha256
`3ee6fb7a083781d31009693c44c79bce1cc91b6dadb159bdcf3b65e693b83c00`, unchanged since the
baseline. `.gate.lock` was absent for the whole review. The worktree was clean before and after.

## Scores

| ID | Metric | Opus rev 2 | Opus rev 3 | Main reason |
|---|---|---:|---:|---|
| M1 | Accuracy | 8 | 8 | README startup (N6) and the password paragraph (N5) now match `Makefile:260-264` and `compose.direct-ports.yaml:36`. One historical learning keeps an unqualified `ADR-085 D21` (L1). No source re-check beyond the delta. |
| M2 | Authority and currency | 7 | 8 | N1 is fixed. All 319 decision tokens in the ten ADR-085 files name their ticket. The ten contextual TKT-399/TKT-557 references match the board bodies. No default remains. L1 is a residual outside the ADR. |
| M3 | Findability | 9 | 9 | The ADR References now go straight to `runbooks/scanner.md` (`ADR-085…md:305`). The historical gap list points to the runbook (N2). |
| M4 | Task execution | 8 | 8 | README now tells the reader to use `make up` for every start and says why. The final-head gate is deferred, so I keep 8. |
| M5 | Coverage | 9 | 9 | Unchanged. |
| M6 | Contract and safety precision | 8 | 8 | I re-ran the bounded proof. Normalized ADR text is equal to revision 1, so no rule, bound or outcome changed. The configuration TLS citation is now exact. |
| M7 | Token efficiency | 8 | 8 | The ADR-085 stub grew from 14,077 to 15,271 bytes and the nine parts by about 5 KB, all from `of TKT-n` suffixes. AGENTS grew by 74 bytes. Entry reductions are still large and measured. |
| M8 | Clarity and structure | 8 | 8 | N4 and N5 are fixed, and the delta adds no em dash. The explicit notation is heavy. Eleven ranges read like compound identifiers (L4). |
| M9 | Reference integrity | 8 | 8 | Links pass for 293 files. Old and revision-2 anchor aliases are both kept. The exact browser-submit quote is back in AGENTS (N3). About 79 code comments still cite AGENTS.md for rules that now live in engineering rules (L5). |
| M10 | Maintainability and provenance | 8 | 8 | Maintenance states the explicit-source rule and the quoted-text search. `results.md` keeps both failed score sets and says the old proof was invalid. Receipts are committed. The proof generator is not, and no executable guard enforces the new rule (L2, L3). |

Result: every metric is 8 or 9 from me. I found no material defect, so no metric is capped. I
found no critical or high finding. My part of the owner bar is met. Completion still needs
Sol's independent scores at 8 or more on every metric and a gate PASS on the final committed head.
This is a judgment on the documentation, not on application or venue readiness.

## Revision-2 findings: status at the candidate

| Finding | Status | Evidence |
|---|---|---|
| N1, decision sources | Fixed | See the next section. |
| N2, historical pointer | Fixed | `baseline-evidence.md:60-62` labels the list as historical and links the scanner runbook. `ADR-085…md:305` links the runbook directly. |
| N3, browser quote | Fixed | `AGENTS.md:57` holds the exact sentence. All four `test/browser/*.mjs:3` quotes match it. `maintenance.md:49-52` adds the quoted-text search. |
| N4, staff comma | Fixed | `staff-accounts.md:95-96` is two complete sentences. |
| N5, password paragraph | Fixed | `configuration.md:205-209` is four plain sentences. The rotation boundary is kept. |
| N6, Compose overlay | Fixed | `README.md:15-17`. `make up` uses both Compose files, and Grafana is on the overlay at `compose.direct-ports.yaml:36`. |
| `configuration.md:171` | Fixed | "(TKT-557 decision D21, recorded in ADR-085)". |

## N1 verification and the proof's assumptions

I did not take the new proof on trust. These are the checks I ran.

1. **All references are explicit.** I scanned every `D` number in the ten ADR-085 files at the
   candidate. Each of the 319 tokens either has a following `of TKT-n` or follows `TKT-558 `.
   The split is TKT-556 283, TKT-399 29, TKT-557 4, TKT-558 3. Zero are unqualified. The
   TSV totals 316 because the proof leaves out the citation guide, which holds three tokens.
2. **The proof reproduces.** I ran a copy of `verify-third-pass.py` with its output sent to my
   scratchpad. It prints PASS, and its TSV is byte-identical to the committed
   `namespace-proof.tsv`.
3. **The proof now finds the N1 class.** I changed `ADR-085 D30 of TKT-399` to `of TKT-556` in
   memory, and in a second run did the same for the D33 history site. The proof's `refs()`
   comparison found both changes. The revision-2 proof could not have found them.
4. **Assumption: revision 1 kept the baseline context.** The proof compares with `771a5c1a`, not
   with the baseline. I applied the proof's own extractor to the baseline single file and to the
   ten revision-1 files. Every baseline (decision, ticket) pair is present in revision 1. Revision
   1 has 27 extra pairs, all TKT-556. I checked them sentence by sentence. They come from new part
   headings and link labels ("Tests and mutations, by invariant, D11", "D19, D24, and D25") and
   from stub sentences copied from the baseline. The comparison base is sound.
5. **Assumption: the context regex finds every contextual form.** The proof knows only
   `TKT-n `, `TKT-n history ` and `TKT-n deviation, ` with slash groups. I scanned the baseline
   for any non-556 ticket in the same sentence before a `D` number, with 70- and 300-character
   windows. Outside the nine fixed sites, the only hits were `ticket 0 (TKT-557) … D39`, `Shipped
   in TKT-557.** D24 as amended by D34/D35/D37/D39` and three `TKT-552 under D14-4` phrases. I read the
   TKT-557 board decisions D22-D39. TKT-557 D34, D35, D37 and D39 are about Touch budgets and pgx
   controls. The ADR's D34/D35/D37/D39 amendments are TKT-556 decisions
   (`decision-log.md:105-112`). D14-4 is the TKT-556 reason-copy decision. All of these are
   correctly TKT-556. No contextual form was missed.
6. **Assumption: the old default is the documented baseline rule.** Baseline ADR-085 lines 86-89
   say a bare `Dn` is a TKT-556 decision. The proof applies that default only to the old text.
7. **Receipts.** For each row of `decision-sources.tsv` I hashed every comment body in the
   TKT-399 and TKT-557 snapshots. All ten hashes match a `kind=decision` comment with the stated
   number, and each opening sentence matches the summary. The snapshots support decision bodies
   only. They do not show current ticket status.
8. **DOC-01 and links.** `vitest run test/doc-namespace.test.ts`: 3 of 3 pass.
   `scripts/check-markdown-links.sh`: 293 files resolve. `scripts/check-adr-numbers.sh`: 84
   unique.

The proof has three limits. First, its `expected` block checks substrings that the fix script
wrote. It agrees with the fix by construction, so the evidence comes from the `refs()`
comparison, not from that block. Second, `omit_guide` takes the citation guide out of the
comparison. I read the guide by hand (`baseline-evidence.md:16-22`), and it is correct. Third,
the 283 TKT-556 references depend on the baseline's own default. There is no TKT-556 snapshot, so
nobody checked them against the TKT-556 board in this pass. If the baseline had a wrong citation,
it is still there. `results.md:155-156` states this limit correctly ("not a proof that every
decision was correctly summarized").

## Findings

All are low. None caps a metric.

### L1. A learning keeps an unqualified ADR-085 decision reference (low)

Metrics: M2, M1. `docs/learnings/2026-10-18-probe-the-production-connection-string.md:22`:
"refuses to start without it (ADR-085 D21)". It means TKT-557 D21, the sslmode deviation. Under
the baseline rule it read as TKT-556 D21, visible recovery. The new guide removes the default,
so this reference now has no defined source. This is the same pattern that revision 3 fixed at
`configuration.md:171`. The sentence context (sslmode) lets a reader find the correct decision,
so the defect is not material. A citation qualifier does not rewrite the historical lesson.
Change it to "(TKT-557 decision D21, recorded in ADR-085)". This is the only Markdown hit outside
the ADR for an `ADR-085` reference followed by an unqualified `D` number. I excluded `docs/sprints`
and the review copies, which are evidence.

The same form is in code comments, outside a documentation-only scope: `compose.yaml:427`
"(TKT-557, ADR-085 D21)", `services/access/cmd/access/database_config.go:13`,
`web/scanner/src/occurrence-lifecycle.test.ts:11` "ADR-085 D27", and
`web/scanner/src/reconcile-batch.ts:1` "owner D31". Record them as known limits, or fix them in a
later code change.

### L2. No executable guard for the explicit-source rule (low)

Metrics: M10, M9. `maintenance.md:46-48` makes the rule binding but gives only a manual check.
DOC-01 (`doc-namespace.test.ts:20-30`) accepts a bare `ADR-085 Dn` and does not need `of TKT-n`.
A later edit can bring back the N1 class, and the gate will still pass. A change to the test is
the owner's decision. A documentation-only mitigation is to add this command to maintenance.
I checked it at the candidate (0 hits) and against a sample line, where it reports `ADR-085 D30`
and `ADR-085 D13-1` and passes `TKT-558 D8` and `D14-3 of TKT-556`:

```bash
git grep -nP '(?<!TKT-558 )(?<![\w-])D[1-9][0-9]*+(?:-[0-9]+)?+[a-z]?+(?!\w)(?! of TKT-)' \
  -- docs/adr/ADR-085-scanner-local-outcome-model.md docs/adr/scanner-outcomes
```

### L3. The committed proof cannot be reproduced from the repository (low)

Metric: M10. `namespace-proof.tsv` has a `non_citation_text_sha256` column. The normalization
that produces it is only in `/tmp/ticketing-documentation-review/verify-third-pass.py`.
`results.md:149-156` describes the method. It does not say that the script is outside the
repository, and it does not give the normalization rules exactly. `results.md:161` already
discloses this limit for the board records. Add the same sentence for the script, or commit the
script beside the TSV as evidence.

### L4. Range and history notation is hard to read (low)

Metric: M8. Eleven ranges read like one compound identifier, for example
`ADR-085…md:69` "ADR-085 D33 of TKT-556-ADR-085 D37 of TKT-556" and `:125` "ADR-085 D15 of
TKT-556-ADR-085 D17 of TKT-556". Five history sites repeat the ticket, for example
`outcome-tables.md:43` "TKT-399 history ADR-085 D33 of TKT-399". Use "to" for ranges ("ADR-085
D33 of TKT-556 to ADR-085 D37 of TKT-556"). That form still passes DOC-01, because each number
keeps its `ADR-085 ` prefix. Cosmetic only.

### L5. Code comments cite AGENTS.md for rules that moved to engineering rules (low)

Metrics: M9, M10. About 79 comments in non-Markdown files cite AGENTS.md, by my grep over seven
citation patterns. Most name a test or review rule that revision 1 moved into
`docs/conventions/engineering-rules.md`. Examples:
`services/inventory/internal/store/history_order_smoke_test.go:119` ("the `fixture too small`
trap in AGENTS.md"), `services/commerce/internal/store/reseller_credentials_smoke_test.go:533`
("AGENTS.md TKT-308"), `web/backoffice/src/components/SeatMapEditor.tsx:66` ("AGENTS.md's 'a
hidden input is not a checkbox' trap"). I checked two of these rules. They still exist at
`engineering-rules.md:36-38` and `:62`. AGENTS sends the reader to engineering rules, so this
costs one hop and loses no requirement. No document tells the reader that these citations now
resolve there. The new quoted-text search in maintenance finds these comments but does not
explain them. Add one sentence near `engineering-rules.md:3`, for example: "Code comments that
cite AGENTS.md for a test or review rule refer to the rules in this file."

## Design judgment

The revision-2 design problem is fixed. In revision 2 the proof shared the rewrite's default, so
it could not find an error in that default. Now the default is gone from the current text. The
proof uses the old default only where the baseline documented it. Mutations of the N1 class
fail the proof. I checked the comparison base and the context patterns myself, and both are
sound. The board receipts are an independent source for the ten references that were
ambiguous.

L1 to L5 are local fixes. One structural weakness remains. The new invariant ("every ADR-085
decision names its ticket") is enforced by a one-off script in `/tmp` and by manual review, not
by anything in the gate. The repository has already lost this invariant once. L2 gives a
documentation-only command for it. Making it a test is the owner's decision.

## Evidence and commands

- `git diff --stat f4eedf94 1c49ddd3`: 20 files. No source, test, script, Makefile or Compose
  file changed. Grid and `assets.tsv` show no diff.
- Full diff read for `AGENTS.md`, `README.md`, `configuration.md`, `staff-accounts.md`,
  `maintenance.md`, `decision-sources.tsv`, `namespace-proof.tsv`, and the first 60 lines of the
  ADR-085 stub diff. Current `results.md` and `maintenance.md` read in full.
- `diff` of `/tmp/…/opus-revision-2.md` and `sol-revision-2.md` against
  `docs/documentation/reviews/`: identical.
- Python token scan of the ten ADR files, baseline context scans (70 and 300 characters),
  revision-1 versus baseline multiset, proof re-run and two mutations (all run with `python3 -I`).
- Receipt hashing against `TKT-399.json` and `TKT-557.json`. I also read the TKT-557 D22-D39 bodies.
- `web/scanner/node_modules/.bin/vitest run test/doc-namespace.test.ts`,
  `scripts/check-markdown-links.sh`, `scripts/check-adr-numbers.sh`.
- `git grep` for unqualified ADR-085 references in Markdown and non-Markdown files, for
  AGENTS.md citations in non-Markdown files, and for range and history notation.
- Inventory: 274 baseline rows. Tracked `*.md` and `*.txt` now total 318 files and 3,198,028
  bytes. That count happens to equal the 318 supporting assets, which are a separate set.

## Inspection scope and limits

Read in full: the grid, `results.md`, `maintenance.md`, both TSVs, `verify-third-pass.py`,
`prepare-third-pass.py`, `doc-namespace.test.ts`, my revision-2 report, and the delta of the
small changed files listed above. AGENTS.md and CLAUDE.md were in context.

Read in part: `sol-revision-2.md` (scores, R5, previous findings, checks, design judgment),
`record-third-pass.py` (first 80 lines), `baseline-evidence.md` (8-26, 55-66), `decision-log.md`
(105-112 and grep hits), `engineering-rules.md` (1-12 and grep hits), `configuration.md`
(196-210), `README.md` (1-30), `Makefile` (`up`/`down`), `compose.direct-ports.yaml` (grep), the
baseline ADR-085 (80-92, 290-293, 773) and baseline AGENTS (105-118).

Not done:

- I did not read the 102 KB diff of the nine ADR parts line by line. The token scan, the proof
  re-run, the mutations and the baseline context scans cover the citation changes. The
  normalized-text equality covers everything else.
- I did not run my own 1,308-reference anchor scan. I relied on the link check, which drops
  fragments, and on the visible double anchor aliases in the stub.
- I did not run the gate, `make browser`, or any board request. Board evidence is the two
  snapshots only. I did not check the 283 TKT-556 references against the TKT-556 board.
- I did not re-check source code beyond the Makefile and Compose lines for N6. I did not re-read
  the other ADRs, learnings, skills, runbooks or evidence beyond earlier reviews.

Reused, with limits: the structural coverage and 21 source checks from my revision-1 review, and
the revision-2 scope above for the files that revision 3 did not change. Scores for M3, M5 and
M7 depend mostly on that earlier coverage. This is not a full semantic read of the corpus.
Pending bookkeeping in `results.md` ("Revision 3 is pending review") is not a defect, and I did
not reduce a score for it.

## Conclusion

All ten metrics are 8 or more from me: M3 and M5 are 9, the others are 8. No finding is material,
critical or high. N1 to N6 are fixed, and I checked the new proof's assumptions independently.
To complete the owner bar:

1. Sol's independent revision-3 scores must also be 8 or more on every metric.
2. `make check` must pass on the final committed head.
3. Optional, documentation only: fix L1 and L3 to L5, and add the L2 command to maintenance. Any
   later commit needs the gate again.
