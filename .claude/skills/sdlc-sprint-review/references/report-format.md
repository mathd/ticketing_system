# Release report format

The release report is one JSON source file and one HTML file. The [renderer](../scripts/render_report.py)
validates the source and fills [report.html](../assets/report.html). The output is one offline file with
inline CSS, system fonts, no JavaScript and no external assets.

This page defines the source data and its rules. Other files hold the rest of the contract:

- [report.labels.json](../assets/report.labels.json) holds all interface copy and all enum names in
  English and French. It is the only maintained copy source.
- The renderer owns the derived values, the template placeholders and the fragment markup.
- The examples show valid data:
  - [report.example.en.json](../assets/report.example.en.json): a ready candidate, approval pending.
  - [report.example.fr.json](../assets/report.example.fr.json): a blocked candidate, approval not
    requested.

Both examples are fictional. They use `DEMO` keys, `example.com` hosts and `example-*` model names.
They record no human decision and no published tag. A real report sets `report.fictional` to `false`
or leaves it out.

## Render and verify

Run the renderer with Python 3.10 or newer. It uses only the standard library. Use a scratch
directory outside the repository for the HTML output (`publication.md`):

```sh
python3 -B .claude/skills/sdlc-sprint-review/scripts/render_report.py --input docs/sprints/S-3/report.en.json --output /tmp/sprint-S-3/report.en.html
python3 -B -m unittest discover -s .claude/skills/sdlc-sprint-review/tests
```

The output directory must exist. The renderer does not overwrite a file, so use a new path for each
version.

- Exit 0 prints a JSON receipt with `sourceDigest`, `htmlDigest` and `output`. `sourceDigest` is the
  SHA-256 of the exact bytes of the source file. `htmlDigest` is the SHA-256 of the exact bytes
  that were written to `output`.
- Exit 1 prints `FAIL:` and the reason. The data is invalid, or a file operation failed.
- Exit 2 is a usage error.

Keep both digests in the publication record. After the commit, read the committed files back and
compare their digests with the receipt. The renderer never fetches a link, never authenticates an
approval, never translates the narrative and never commits a file.

A direct `render(data)` call without the source bytes hashes canonical JSON: sorted keys, compact
separators, UTF-8 and no trailing newline. Use the receipt of the command for a saved source file.

## Language and copy

`report.language` is `fr` or `en`. It comes from `sprints.reportLanguage` in the project config. The
default is `en`.

The renderer reads each label from `<language>.labels` and each enum name from `<language>.enums` in
`report.labels.json`. A label key `k` fills the template placeholder `$l_k`. Both languages have the
same keys. To change a copy text, edit that file only. Do not copy label text into this page or into
the renderer.

Some labels are Python `str.format` patterns:

| Label | Fields |
|---|---|
| `checks_summary` | `{passed}`, `{excluded}`, `{missing}`, `{optional}` |
| `date_range` | `{start}`, `{end}` |
| `money` | `{amount}`, `{currency}` |

Each pattern puts a count after a fixed word. So one form is right for 0, 1 and more.

The author writes every narrative field in the report language before the render. The renderer never
translates the narrative, an ID, a command, a model name or a severity value.

## Source data

General rules:

- All keys use camelCase. The renderer rejects an unknown key.
- Every documented key is required, also a key that can be `null`, unless this page marks it
  optional. `null` means "not recorded".
- A timestamp is ISO 8601 with a UTC offset.
- Every `url` is an absolute `https://` or `http://` URL without credentials.
- A **ticket** is a plain key such as `DEMO-4`. The board has no ticket page to link to.
- Every `evidence` field is a list of items `{"label": text, "url": URL or null}`, with an optional
  `ref`. An item holds a URL, or a board reference, or neither. It never holds both.
  - A **board reference** is `{"key": text, "digest": 64 hex characters}`. `key` names the note that
    holds the comment (the sprint or a ticket). `digest` is the SHA-256 of the comment body, as the
    board returns it. A board comment has no ID. A reader finds the comment by hashing the comments
    of that note.
  - A **linked item** has a URL or a board reference. When a rule below requires evidence, it
    requires at least one linked item. An item with a label only can add context. It never satisfies
    an evidence rule alone.
  - A link into the repository holds the full commit ID, never a branch name. A branch moves.
- Narrative fields are plain text. A blank line separates paragraphs. The renderer does not interpret
  Markdown or HTML.

### Top level

| Key | Content |
|---|---|
| `schemaVersion` | `1` |
| `report` | Report identity |
| `project` | `key`, `name` |
| `sprint` | The sprint on the board |
| `release` | Repository, base, candidate, proposed tag |
| `outcome` | Readiness and narrative summary |
| `scope` | Planned, added, delivered and deferred items |
| `areas` | Exactly four review areas |
| `checks` | Executed and not executed checks, with costs |
| `measurements` | Coverage, complexity and other observations |
| `risks` | Findings and release risks that the human must see |
| `followups` | Linked work that this release created |
| `retrospective` | Summary and improvement candidates |
| `decision` | The actual human decision and the tag publication |

### `report`

| Field | Meaning |
|---|---|
| `id` | Stable report ID across versions, for example `S-3-release` |
| `version` | Positive integer, starts at 1. A new candidate, new evidence, a factual correction or a recorded decision needs a new version |
| `language` | `fr` or `en` |
| `generatedAt` | The time the author assembled the source data |
| `preparedBy` | The actual model or person that assembled the source |
| `packetDigest` | SHA-256 of the frozen review packet. Required for a new full-review closure. Optional only to render legacy reports, which cannot supply new closure evidence without it |
| `fictional` | Optional boolean, default `false`. `true` shows the banner for a fictional example |

The source never holds its own SHA-256 or its own HTML digest. The HTML digest goes in the
`report-artifact` record (`publication.md`).

### `sprint`

| Field | Meaning |
|---|---|
| `vault` | The name of the board vault |
| `key` | The sprint key. It matches `S-` and a number, for example `S-3`. The vault and the key together identify the sprint |
| `name`, `goal` | The sprint's name and goal, as the board returns them |
| `state` | The board's state: `planned`, `active` or `closed` |
| `startDate`, `endDate` | The planned dates, with an offset. `endDate` is after `startDate` |
| `scopeDigest` | The board's scope digest when you assembled the source: 64 lowercase hex characters |
| `closureTask` | The key of the closure task. It is a member of the sprint |

### `release`

| Field | Meaning |
|---|---|
| `repository` | Repository URL |
| `base` | `{"ref", "sha"}`. The previous release baseline. `ref` can be `null` |
| `candidate` | `{"sha", "url"}`. The full commit ID (40 or 64 lowercase hex characters) of the exact candidate, and a link to it |
| `publishTag` | Optional boolean. Defaults to `true` for existing reports. Set `false` when the approved brief has `release: false` |
| `tag` | Proposed tag name when publishing, otherwise `null` |

### `outcome`

| Field | Meaning |
|---|---|
| `readiness` | `ready`, `incomplete` or `blocked`. See [readiness rules](#readiness-rules) |
| `headline` | One or two sentences: what the release does, and the main reservation |
| `summary` | Narrative. What the human must know before a decision |

### `scope`

`summary` is narrative about the scope changes. `items` lists every story of the original or of the
current approved scope. It includes work that was deferred or removed later. It leaves out the
closure task. Keys are unique. Take the original scope from the board's `startScope` and from the
briefs. Take the current members from the board. Removing a member must not remove it from the
report.

| Field | Values |
|---|---|
| `key`, `title` | The ticket key and its title |
| `origin` | `planned` (in the first approved brief) or `added` (an approved amendment) |
| `outcome` | `delivered`, `deferred` or `removed` |
| `destination` | The key of the ticket that now holds the work, or `null`. Required for `deferred` |
| `note` | Short text, or `null`. Required for `deferred` and `removed`, to explain what happened to the code |

### `areas`

An object with exactly the keys `implementation`, `verification`, `documentation` and
`retrospective`. The renderer shows them in that order.

| Field | Meaning |
|---|---|
| `status` | `passed`, `incomplete` or `failed` |
| `decision` | The decision of `review_round.py` for the area: `HUMAN_REVIEW`, `FIX`, `REVIEW_AGAIN`, `STOP`, or `null` when the area has no decision |
| `round` | Positive integer, the sprint review round, or `null` when it was not captured |
| `reviewers` | The actual model identities of the lenses that ran. It can be empty only when `decision` is `null` |
| `findings` | `{"raised", "resolved", "deferred"}` counts, or `null` when not captured. Required when `decision` is set |
| `summary` | The area's conclusion, as narrative |
| `evidence` | Review records and other sources |

Rules:

- `status` follows `decision`. `HUMAN_REVIEW` gives `passed`. `STOP` gives `failed`. `FIX` and
  `REVIEW_AGAIN` give `incomplete`.
- `decision: null` gives `incomplete`. The area keeps the facts that were captured before the stop.
  `round`, `reviewers` and `findings` can hold partial values of the types above. Use `null` or `[]`
  for a fact that was not captured. A partial area never passes.
- A `FAIL:` from the review script is a tool error. It is not a verdict. Record `decision: null` and
  keep the facts that were captured.
- Every `round` that is not `null`, in the four areas, has the same value.
- No reviewer identity equals `report.preparedBy`. `review_round.py sprint-round` checks the full
  runner records and all the other authors.
- Finding counts are integers of 0 or more. `resolved + deferred` is not more than `raised`.
- `passed` needs a linked evidence item, and `raised` equal to `resolved + deferred`.

### `checks`

| Field | Meaning |
|---|---|
| `id` | A short unique ID, for example `C4` |
| `name` | What the check proves |
| `kind` | `gate`, `unit`, `integration`, `e2e`, `llm_eval`, `manual` or `other` |
| `required` | `true` when the release criteria or the verification review require it |
| `status` | `passed`, `failed`, `not_run` or `not_applicable` |
| `command` | The exact command, or `null` |
| `detail` | The observed result. For `failed`, `not_run` and `not_applicable`, the reason. Always text |
| `evidence` | Run receipts, logs, traces |
| `cost` | `null`, or `{"amount", "currency", "budget", "calls", "model"}` |

Rules:

- At least one check has `required: true`.
- A `passed` or `failed` check needs a linked evidence item.
- `not_applicable` needs a reason about the scope. Missing credentials, budget or infrastructure
  give `not_run`, not `not_applicable`.
- An `llm_eval` check with `passed` or `failed` needs `cost.amount` and `cost.model`.
- `cost.amount` is the real spend: a finite number of 0 or more. `currency` is a three-letter ISO 4217
  code in capitals. `budget` (finite, 0 or more, same currency), `calls` (integer, 0 or more) and
  `model` (text) are optional keys.
- A spend above `budget` stays visible. The renderer marks that row with the `budget_exceeded` label.
  The overrun does not change readiness. Explain it in `detail`, and take it to the human under the
  budget that the brief approved.

### `measurements`

| Field | Meaning |
|---|---|
| `metric` | The name of the measure |
| `scope` | The path, module or journey that was measured |
| `baseline`, `candidate` | The value, formatted as text, or `null` when it was not measured |
| `tool` | The tool and its version, or `null` |
| `observation` | What the value means here. When a value is `null`, the reason |

Values are evidence to investigate. The report sets no target, coverage included.

### `risks`

List every Critical or High finding, every finding that is not resolved and every release risk that
is not a finding. IDs are unique. Prefix a finding ID with its area (`IMPL-1`, `VER-2`, `DOC-1`,
`RETRO-1`) so that IDs from different areas do not collide.

| Field | Meaning |
|---|---|
| `id` | The original finding ID, or a new `R` ID |
| `severity` | The normalized severity: `Critical`, `High`, `Medium`, `Low` or `Nit` |
| `area` | One of the four area names |
| `title` | The risk in one line |
| `status` | `open`, `resolved` or `deferred` |
| `blocking` | `true` when the item fails a release criterion, correctness, security or acceptance |
| `resolution` | How it was fixed, or why it waits, or `null` |
| `followup` | The key of the follow-up ticket, or `null` |
| `evidence` | Finding records, fixes, tests |

Rules:

- `resolved` needs `resolution` text and a linked evidence item.
- `deferred` needs `blocking: false` and a `followup` key that is listed in `followups`.
- An unresolved Critical or High risk blocks readiness, also when it is `deferred`. `blocking` does
  not change that.

### `followups`

Each item is `{"key", "title", "reason"}`. Keys are unique. `reason` names the source: a scope item,
a risk ID or a check ID.

### `retrospective`

`summary` is narrative. "No repeated pattern" is a valid summary. Each candidate:

| Field | Meaning |
|---|---|
| `id` | A short unique ID, for example `P1` |
| `type` | `adr`, `process` or `skill` |
| `observation` | The pattern, with the stories that show it |
| `proposal` | The proposed change |
| `recommendation` | The agent's recommendation: `adopt`, `trial`, `reject` or `no_action` |
| `humanChoice` | The human's actual selection: `pending`, `adopt`, `trial`, `reject` or `no_action` |
| `destination` | The key of the follow-up ticket after a human selection, or `null` |
| `evidence` | Closeouts, findings, decisions |

`adopt` and `trial` need a `destination` and a linked evidence item for the human decision.

### `decision.approval`

| Field | Meaning |
|---|---|
| `status` | `not_requested`, `pending`, `approved` or `rejected` |
| `approver` | The name of the human who decided |
| `decidedAt` | The time of the decision |
| `ref` | A board reference to the `close-approval` record. Before a decision it can hold the request |
| `sprint` | The sprint key that the human named |
| `scopeDigest` | The scope digest that the human named |
| `candidate` | The full commit ID that the human named |
| `reportDigest` | The HTML SHA-256 of the report version that the human named |
| `reportUrl` | A link to that exact report file, with the full commit ID that holds it |
| `tag` | The tag name that the human named |
| `note` | Short text, or `null` |

| Status | Rules |
|---|---|
| `not_requested`, `pending` | `approver`, `decidedAt`, `sprint`, `scopeDigest`, `candidate`, `reportDigest`, `reportUrl` and `tag` are `null`. `ref` can hold the request, or `null` |
| `approved` | Every field except `note` is set. `sprint`, `scopeDigest`, `candidate` and `tag` equal `sprint.key`, `sprint.scopeDigest`, `release.candidate.sha` and `release.tag`. `reportDigest` is 64 lowercase hex characters. `outcome.readiness` is `ready` |
| `rejected` | The same fields and the same matches as `approved`. Readiness can have any value, `blocked` included |

`reportUrl` points to one file at one commit. It is never a branch link, and never a path that a later
commit replaces.

### Which report a decision names

The human decides on one rendered version, by its HTML digest. That digest exists only after the
render. So a report never records a decision about itself. The next version records the decision.
That version has another HTML digest, and nobody approved it.

When `status` is `approved` or `rejected`, the renderer shows the `approval_prior_note` label and
links `reportUrl` with the `approved_report_link` label. The reader then sees that the decision names
the earlier report, not the open file. The report never shows its own HTML digest as approved.

The version that records a decision differs from the decided version only in the `report` identity
fields, in `decision` and in `sprint.state`. Any other change needs a new request: set `pending` and
set the decision targets to `null`. A report update never inherits an old approval by name or by
link.

### `decision.tagPublication`

| Field | Meaning |
|---|---|
| `status` | `not_published`, `published` or `not_required` |
| `tag` | The published tag name |
| `candidate` | The commit ID that the tag points to |
| `object` | The annotated tag object ID, 40 or 64 lowercase hex characters |
| `verifiedAt` | The time of the local and remote read-back |
| `evidence` | Read-back and publication receipts |

- `not_published`: `tag`, `candidate`, `object` and `verifiedAt` are `null`, and `evidence` is `[]`.
- `published`: `approval.status` is `approved`. `tag` equals `release.tag` and `candidate` equals
  `release.candidate.sha`. `object` and `verifiedAt` are set. `verifiedAt` is not before
  `approval.decidedAt`. `evidence` has a linked item.
- `sprint.state: closed` needs `published` when a tag is planned, or exact `approved` close targets when no tag is planned.

## Readiness rules

Readiness is a technical claim by the agents. It is never a human approval. The renderer computes the
expected value and rejects a source where `outcome.readiness` differs.

A risk blocks when its status is not `resolved` and either `blocking` is true or its severity is
`Critical` or `High`. Deferring a serious defect does not remove the release gate.

1. `blocked` when any area is `failed`, any required check is `failed`, or any risk blocks.
2. Otherwise `incomplete` when any area is `incomplete` or any required check is `not_run`.
3. Otherwise `ready`.

A failed optional check does not change readiness. Record its cause as a risk, and mark it
`blocking` if it shows a release defect. A budget overrun is not a readiness rule. See
[checks](#checks).

## Rendering

### Sections

The order follows the reader's questions: the state of the release, the human decision, then the
evidence.

| HTML section | Data |
|---|---|
| Header | The project name, the report kind and version, the section links and the theme switch |
| Masthead | The sprint name and goal, and one row of identifiers: dates, sprint key, closure task, proposed tag, base, full candidate commit and report version |
| `#outcome` | The overview, the `outcome` narrative, the holds list and the human sign-off. See below |
| `#decision` | `decision` |
| `#scope` | `scope` |
| `#risks` | `risks` and `followups` |
| `#reviews` | `areas`, in the fixed order |
| `#checks` | `checks`, the check count summary and the cost totals |
| `#measurements` | `measurements` |
| `#retrospective` | `retrospective` |
| `#identity` | `sprint`, `release` and `report` identity, and the source digest |
| Footer | The source digest, `generatedAt`, `schemaVersion` |

The overview shows the agent evidence: readiness beside the scope, checks and blocker figures. Under
it is one row with the status of each review area. Below that, the `outcome` narrative is on the
left. The holds list and the human sign-off are on the right. The sign-off has its own blue panel,
so agent evidence and human authorization stay apart. It shows the approval and the tag publication.
Before a decision, it lists what a decision must name: the sprint, the scope digest, the tag, the
candidate and the HTML digest of one report version. After a decision, it shows the approver and the
`approval_prior_note` label with the `reportUrl` link.

The holds list repeats the readiness rules as items. "Blocks release" lists failed areas, failed
required checks and blocking risks. "Not finished" lists incomplete areas and required checks that
did not run. When both lists are empty, it shows the `holds_none` label. Each item links to its row
or area.

### Derived values

The renderer computes these values. This list gives their meaning. Read the renderer for the exact
output.

- Scope figure: the delivered items out of all scope items.
- Checks figure: the required checks that passed, out of the required checks that are not
  `not_applicable`.
- Blocker figure: the number of risks that block. A failed area or a failed required check also
  blocks, and the readiness label shows that.
- Cost figure and total: the sum of `cost.amount` for each currency. The renderer never converts or
  adds different currencies.
- A risk that blocks gets the `risk_blocker` label in its row.
- A `null` value shows the `unknown` label. An empty list shows the `none` label.

All amounts use one format: row amounts, budgets and totals. The `money` and `decimal_mark` labels
give the local form, for example `USD 1.86` and `1,86 USD`. An amount shows at least two decimals
and keeps the fractional digits that were given. A nonzero amount never rounds to zero, so `0.0042`
stays visible.

### Escaping and links

- The renderer escapes every plain value with `html.escape(value, quote=True)`. A placeholder that
  ends in `_html` takes markup that the renderer builds from escaped data.
- The renderer uses `string.Template.substitute`, which does not scan values again. A `$` inside a
  value is safe.
- Only a checked `http` or `https` URL becomes an `href`. A board reference and a label-only item
  render as escaped text.
- Narrative becomes `<p>` elements: split on blank lines, escape each paragraph, join the lines with
  `<br>`.
- An enum value goes into a class name only after validation.

### Markup

The renderer builds the rows, the review areas, the sign-off fragments and the chips. The exact bytes,
the white space and the separators inside these fragments are not a separate contract. Keep the class
names that `report.html` styles. This snippet is only an illustration:

```html
<span class="chip chip--good">Passed</span>
<ul class="evidence"><li><a href="https://git.example.com/demo/invitations/blob/f1e2d3c4b5a6978877665544332211ffeeddccbb/docs/sprints/S-3/evidence/gate-verdict.txt" rel="noreferrer">Gate verdict</a></li><li>Round record <code>S-3 sha256:9c89e413d310…</code></li></ul>
```

## Limits

- The template has no charts. Status always shows as text, a shape mark and a color. So it stays
  readable in grayscale print.
- The header has a System, Light and Dark theme switch. It uses radio inputs and CSS `:has()`, not a
  script. System follows the reader's operating system. The choice is not kept after a reload. Print
  always uses the light palette. A browser without `:has()` always shows the light palette, and the
  switch has no effect. The small marks beside some figures repeat counts that the text already gives.
- In print, the link URLs show after the links in the fact lists, the evidence lists, the follow-ups
  and the sign-off details only.
- Below 40 rem, the tables become labelled records.
- The renderer's tests prove the mapping and the escaping. They do not prove that the facts of one
  report are true. The documentation review checks that.

## No-tag sprint reports

Full review and HTML publication apply with either tag plan. With `release.publishTag: false`,
`release.tag` and the approval's tag are `null`, and `tagPublication.status` is `not_required`.
Its object, verification time, tag and candidate fields remain `null`, and its evidence is empty.
The page states that no tag is planned. A closed no-tag sprint still requires exact human approval
of a ready candidate, scope and report. Existing reports without `publishTag` retain tag semantics.
