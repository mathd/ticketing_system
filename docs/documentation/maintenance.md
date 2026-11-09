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
