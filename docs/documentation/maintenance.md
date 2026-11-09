# Documentation maintenance

Keep current instructions short and task-specific. The document family owns the rule; indexes
route readers to it. See [authority rules](../README.md#authority-and-freshness).

| Change | Update and verify |
|---|---|
| Service or frontend boundary | `architecture.md`; check ownership against modules, gateway routes, and Compose |
| Runtime, tool, gate, or dependency | `development.md`, `testing.md`, or dependency policy; read Makefile, manifests, and workflow pins |
| HTTP or domain-event behavior | OpenAPI or event contract plus relevant ADR; inspect handlers/consumers and their conformance tests |
| Recovery command or required credential | The matching `runbooks/` file and `configuration.md`; inspect CLI flags, command dispatch, and startup checks |
| Product intent or design decision | Product document or new/amended ADR; preserve prior decision, status, date, and ticket provenance |
| Operational or test lesson | One `learnings/YYYY-MM-DD-title.md` plus a short index entry; promote a stable rule to the applicable engineering section |
| New evidence or sprint record | A dated record with baseline, procedure, result, and limits; link it from `records.md` or the relevant family README |
| New/moved document | Update the task index and incoming links. Keep old section headings as redirects where existing references need them. |

Do not rewrite logs, receipts, signed evidence, or a historical result to match current behavior.
Add current interpretation at its entry point. A snapshot is not current ticket state, a deployed
control, or permission to repeat an external action. Check the board when current ticket status
is required. Document review does not authorize running historical mutation probes.

## Verification

```bash
make check-markdown-links
make check-adr-numbers
make check
```

Stage new documents by explicit path before the link check, which reads tracked Markdown only.
The existing link check confirms target existence. It skips external URLs and pure anchors, and
drops fragments from file links. Separately check moved headings and inline-code source paths.
Use the same heading text for redirects, or check each incoming fragment against the new file.
The full gate verifies the application tree, not the truth of every prose claim.

Search non-Markdown consumers before moving a path or heading:

```bash
git grep -n 'docs/development.md' -- ':!*.md'
```

Review tests, source comments, runtime messages, migrations, and Compose comments in those hits.
Use the actual old path for another move. `web/scanner/test/doc-namespace.test.ts` reads a fixed
scanner subsection and safety sentences from `development.md`; it also checks added ADR decision
references. Keep that synopsis and its safety limits, namespace references without changing their
source authority, and run the test after moves. Every ADR-085 numbered decision must name its
source ticket explicitly. Check grouped and historical references against the original sentence
and decision body; a default-ticket parser cannot prove ownership. Password-recovery browser comments and durable
configuration errors also cite development headings. A Markdown link scan cannot see these contracts. Search quoted instruction text too, for example
`git grep -n 'AGENTS.md' -- ':!*.md'`. Several browser specs quote the browser-submit rule.
Preserve that wording or review each dependent quote when changing it. A path-only search misses
quoted rules and embedded section text.

Check for missing decision sources after an ADR-085 edit. This command must produce no hits;
`git grep` exits 1 when there is no match. It does not verify that the named ticket is correct.
The gate's namespace test does not enforce this stronger rule.

```bash
git grep -nP '(?<!TKT-558 )(?<![\w-])D[1-9][0-9]*+(?:-[0-9]+)?+[a-z]?+(?!\w)(?! of TKT-)' \
  -- docs/adr/ADR-085-scanner-local-outcome-model.md docs/adr/scanner-outcomes
```

Read source before changing a technical claim. Record the inspected files and limits in review
evidence. Check malformed input classes and failure paths before writing guarantees. Refer to
manifest/configuration files for exact versions rather than repeating a version in many pages.

## Re-evaluation

Use the unchanged [scorecard](evaluation.md). Give both requested models the same candidate and
inspection scope; record each score and defect independently in [results](results.md). Require
at least 8/10 from both on every metric. A known material defect caps that metric at 7. Retain
baseline and intermediate scores. State scan versus full-read coverage and unexecuted checks.

Measure required entry-context bytes and a named token estimate before/after. Byte reduction is
exact; a byte-based token estimate is not a model tokenizer count. Moving detail lowers task load
without deleting history, so report both entry-point size and total prose size.
