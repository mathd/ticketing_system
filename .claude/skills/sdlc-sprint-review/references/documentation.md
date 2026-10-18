# Documentation and report fidelity

Check the user documentation, the technical guidance, the release notes and the diagrams against the
whole outcome as built and against the real acceptance evidence. Trace each claim across the stories.
Do not accept each story's summary on its own. Find missing setup steps, changed contracts and
behavior that would mislead a reader of this release.

Check that the report source and the HTML match the release packet and the findings of the other
areas. Check:

- the sprint identity (vault and key) and the closure task,
- the exact candidate,
- the scope changes and the remaining work,
- the test results, the costs and the measurements that are not available,
- the difference between readiness, approval and publication.

A report that looks polished must not hide missing evidence. It must not suggest that an agent's
verdict is a human authorization.

Check the evidence in the report. A board reference must name a real comment: the key of the note and
the SHA-256 of the comment body. A link to the repository must hold the full commit ID, not a branch
name. A report that names evidence that nobody can retrieve has no evidence.

Use `sprints.reportLanguage` for the labels and the text. Keep the meaning of the source and the
technical identifiers. Review the rendered page in the chosen language, in a narrow viewport, in the
dark theme and in print. Check that it reads offline with scripts off. Check the evidence links, the
escaping and that all content is visible. The renderer's tests do not prove the facts of one report
or its layout.

## What each lens receives

- **Initial inspection:** the release claims and the report data that exists. No author rationale.
- **Decision audit:** the approved brief, the effective decisions and the assembled report, against
  the evidence of all areas.

Run this decision audit after the other three areas have final results and the report draft exists. Both lenses retain the same frozen product packet. Add the other results and the exact source and HTML draft as audit supplements, with their digests in the area record. If a later edit changes the report, run a bounded independent check of the changed source and rendered page. Post its receipt as a new `sprint-review` / `report-check` comment referencing the documentation record. Do not edit that earlier comment. It does not replace either lens. Do not call narrative that nobody reviewed "approved".

## What you return

Return the shared result for the area `documentation`. Set `stage` to `sprint-review` and `area` to
`documentation` on the record and on both lens results. A mistake in the documentation of the current
release must be corrected. An editorial improvement that is not needed can become follow-up work.
