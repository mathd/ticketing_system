# Sprint records and local checks

The board owns the sprint: its key, state, dates and members. The sprint's comments hold the
evidence and the human decisions. A comment is append-only. A newer comment never replaces an older
one. It supersedes it.

## Where a record lives

- Post every sprint record on the **sprint note**: `POST /sprint` with `comments`
  (`board.md` § Write). Send `_rev`, `role` and one comment object `{stage, kind, body}` for each
  record. A closed sprint still accepts comments.
- Copy two records to the **closure task** with `POST /ticket` (`local-tracker.md`): the
  `report-artifact` record and the `release` record. The task then links the path, the commit and
  the digests.
- Start the body with the marker line `<!-- sdlc:stage=<stage> kind=<kind> -->`. Then write a short
  text for people. End with one fenced `json` block that holds the record fields. Tools read the
  block. People read the text.

| Record | `stage` / `kind` | Posted by |
|---|---|---|
| Approved brief, and every amendment | `sprint` / `sprint-brief` | `sdlc-sprint` |
| Owner priority approval for the exact scope | `sprint` / `sprint-priority` | `sdlc-sprint` |
| The human's start request | `sprint` / `sprint-start` | `sdlc-sprint` |
| The human's close approval | `sprint` / `close-approval` | `sdlc-sprint` |
| Tag attempts, tag result, close result | `sprint` / `release` | `sdlc-sprint` |
| Progress observation | `sprint` / `summary` | `sdlc-sprint` |
| Cross-story retrospective, and the human's selections | `sprint` / `sprint-retro` | `sdlc-sprint-review` |
| Release packet (candidate and material inputs) | `sprint-review` / `summary` | `sdlc-sprint-review` |
| One area's review round (both lenses) | `sprint-review` / `review` | `sdlc-sprint-review` |
| Below-threshold correction verification | `sprint-review` / `correction` | `sdlc-sprint-review` |
| Later report fact-check receipt | `sprint-review` / `report-check` | `sdlc-sprint-review` |
| Round blocker | `sprint-review` / `blocker` | `sdlc-sprint-review` |
| Report publication or replacement | `sprint-review` / `report-artifact` | `sdlc-sprint-review` |

Every record names its sprint (`sprint`). A review record also names its area, its round, the exact
packet, the actual model and the disposition evidence. Keep earlier refutation checks and human
steering as history. Session changes and area changes never reset the review cap.

## Reference by key and digest

A board comment has no ID. Refer to a comment by the key of its note and the SHA-256 of its body.
Never write "comment ID". The body is the text the board returns, in UTF-8, with the marker line.

Save the response of `GET /sprint?key=S-3` to a file. Print the digest of every comment:

```sh
python3 -c 'import hashlib, json, sys; s = json.load(open(sys.argv[1]))["sprint"]; [print(c.get("stage"), c.get("kind"), hashlib.sha256(c["body"].encode("utf-8")).hexdigest()) for c in s["comments"]]' /tmp/sprint-S-3.json
```

`GET /sprint` answers `{"sprint": {…}}`. `comments` is a list of objects with `stage`, `kind`,
`author` (when the writer sent one) and `body`, in the order they were added; the body round-trips
byte for byte.

Write a reference in prose as `S-3 sha256:<64 hex>`. In the report data it is
`{"key": "S-3", "digest": "<64 hex>"}`. Hash the comment after the board accepted it. Do not hash
the payload you sent.

## Record fields

Each JSON block holds these fields. `sprint_state.py` reads them from the snapshot.

**`sprint-brief`.** The human approved this brief. Post it only after the approval. A later brief
restates the whole brief and names the digest it supersedes. The newest `sprint-brief` is the
current brief.

```json
{"sprint": "S-3", "review": "full", "continuation": "autonomous", "release": true, "closureTask": "TKT-512",
 "members": ["TKT-510", "TKT-511", "TKT-512"], "scopeDigest": "<64 hex>",
 "approvedBy": "<name of the human>", "approvedAt": "2026-10-19T08:55:00-04:00",
 "reportLanguage": "en", "supersedes": null}
```

`members` is the approved member list, sorted. `scopeDigest` is the digest the human approved. It
is the `scopeDigest` that the board reported, and that `sprint_state.py progress` matched. `review: "full"` requires a member closure task even with `release: false`. For the explicit `review: "none"` exception, set `closureTask: null`, `release: false` and a nonempty owner-approved `reviewReason`. New briefs always name a review policy and `continuation: "autonomous"|"manual"`. Only an approved autonomous continuation starts final validation automatically.

**`sprint-start`.** The human asked to start the sprint and named it and the digest.

```json
{"sprint": "S-3", "briefDigest": "<digest of the current brief comment>",
 "scopeDigest": "<64 hex>", "requestedBy": "<name>", "requestedAt": "<time>"}
```

**`close-approval`.** The human approved the close. With `review: "full"`, it also names the candidate and report. Set `tag: null` when `release: false`; only the tag is omitted from publication. With `review: "none"`, leave all three target fields out.

```json
{"sprint": "S-3", "briefDigest": "<64 hex>", "scopeDigest": "<64 hex>", "closeSprint": true,
 "approvedBy": "<name>", "approvedAt": "<time>",
 "candidate": "<full commit ID>", "reportDigest": "<HTML digest>", "tag": "v1.4.0"}
```

**`release`.** The result of a tag attempt, or of the close. Post one record for each event.

```json
{"sprint": "S-3", "event": "tag-published", "candidate": "<full commit ID>", "tag": "v1.4.0",
 "tagObject": "<annotated tag object ID>", "verified": true, "verifiedAt": "<time>"}
```

`event` is `tag-attempt`, `tag-published` or `closed`.

**`report-artifact`.** `sdlc-sprint-review` posts it. See its `references/publication.md`.

**`review`.** One record for each area and round. The shape is in
`sdlc-ticket/references/reviews.md`.

## Rules for approvals

- An approval names a real human and the exact targets. A marker, a `role` value and an agent
  comment are not an approval. `role` is chosen by the caller. The board never treats `role=human`
  or a metrics comment as an authenticated approval.
- Record what the human said, in the human's terms. If the human gave no name, ask for it. Never
  write `human` or a model name as the approver.
- A request to start or to close names the sprint. A request to start also names the digest. Reuse a
  request that already names the target. Ask only for what is missing.
- Keep these facts apart: review result, technical readiness, human approval, tag publication and
  board close. One never proves another.
- A ticket's Done is the pipeline's acceptance in this project. The human decision that covers a
  sprint is the close approval. It names the exact `scopeDigest`.
- Store the source-data digest and the HTML digest of a report outside the HTML. Keep one current
  report pointer: the newest `report-artifact` record. Keep the report that a human approval names.
  Never rewrite a commit that holds it. A later status edition has a new digest. Nobody approved
  that digest.

## The snapshot

`scripts/sprint_state.py` checks one JSON file. The file is a normalized copy of what you read. It is
a contract input. It is not a tracker and not a source of approvals. Use the examples:
[start](../assets/snapshot.example.json) and [close](../assets/snapshot.close.example.json).

| Snapshot field | Where it comes from |
|---|---|
| `vault` | `vault` in `GET /sprint` |
| `sprint` | `key`, `name`, `goal`, `state`, `startDate`, `endDate`, and `startScope` and `endScope` (`null` until set) |
| `scopeDigest` | `scopeDigest` in `GET /sprint`. The script recomputes it. A difference is `FAIL` |
| `observedAt`, `membershipComplete`, `unreadable`, `historyGaps` | the same names in `GET /sprint` |
| `members` | One object per member: `key`, `status`, and `blocked`. `blocked` is `true` when the status is `BLOCKED` or the ticket has an open `blocked-by` link (read `GET /ticket?key=K`) |
| `brief` | The JSON block of the newest `sprint-brief`, plus `digest` (its reference digest) |
| `startRequest` | The JSON block of the newest `sprint-start`, plus `digest` |
| `closeApproval` | The JSON block of the newest `close-approval`, plus `digest` |
| `release` | With `review: "full"`: `ready` (the report says `ready`), `candidate`, `reportDigest`, `tag`, `tagObject`, and `publication` (`verified`, `candidate`, `tag`, `tagObject`) from the `release` record; omit `publication` and `tagObject` and set `tag: null` for no tag. Also capture `review` and `report` below |

A member is accepted when its status is the done status. There is no per-ticket acceptance field.
Take the done status from the vault's status list, not from a response.

```sh
python3 .claude/skills/sdlc-sprint/scripts/sprint_state.py progress /tmp/sprint-S-3-snapshot.json --vault sdlc --sprint S-3 --done-status Done
python3 .claude/skills/sdlc-sprint/scripts/sprint_state.py start    /tmp/sprint-S-3-snapshot.json --vault sdlc --sprint S-3 --done-status Done
python3 .claude/skills/sdlc-sprint/scripts/sprint_state.py close    /tmp/sprint-S-3-snapshot.json --vault sdlc --sprint S-3 --done-status Done
```

Take `--vault` from `sprints.vault` in the config and `--sprint` from the human's request. Never take
them from the response you check.

| Output | Exit | Meaning |
|---|---|---|
| a JSON object (`progress`) | 0 | Counts, blockers, scope changes, history-gap count, `scopeDigest` |
| `START` or `CLOSE` | 0 | The request can go on. Nothing was started or closed |
| `STOP` | 1 | The request cannot go on. Each reason is a line `STOP: ...` on stderr |
| `FAIL: <reason>` | 1 | The snapshot is invalid, or a field is missing |
| usage text | 2 | The arguments are wrong |

`progress` prints: `accepted`, `total`, `remaining`, `blocked`, `added` and `removed` (against the
approved brief, `null` without a brief), `sinceStart` (against `startScope`), `functional` (the
members that are not the closure task), `closure` (the status of the closure task), `historyGaps`
(a count), `endScopeMismatch` and `scopeDigest`. Counts describe tickets. They are not effort.

What the script checks, and what it does not:

- It recomputes the scope digest exactly as the design defines it. It refuses a snapshot whose
  digest differs from the board's.
- It checks the order of timestamps. It does not check that a response is current or that an
  approval is real. Read the sprint again just before a write. Check the result after it.
- `historyGaps` never changes a decision. The script counts them. You report them.
- `STOP` is not a failure of the tool. Fix the cause and run it again. Never edit the snapshot to
  get `START` or `CLOSE`.
- A closed sprint returns `STOP` from `close`. After an ambiguous close, read the closed state and
  compare `endScope` with the approved members. Do not close again, and do not start again.

## Full-review closure evidence

The normalized `release` object keeps that legacy name for both tag choices. It contains:

- `candidate`, `reportDigest`, `ready` from the verified report. `reportDigest` is the HTML digest.
- `review`: `candidate` from the frozen reviewed packet, `packetDigest`, positive integer `round`, combined `decision: "HUMAN_REVIEW"`, and
  `areas` with exactly `implementation`, `verification`, `documentation`, `retrospective`.
  Each area has the same `packetDigest`, `round`, `decision: "HUMAN_REVIEW"` and `recordDigest`.
  Derive these from accepted board records after `review_round.py sprint-round` succeeds. Never
  infer them from member Done states or from an agent's summary. Keep author/model independence,
  cap authorization, dispositions and verification in those referenced round records.
- `report`: take `candidate` and `packetDigest` from `release.candidate.sha` and `report.packetDigest` in the committed JSON, after verifying its `sourceDigest`. Do not fill these from the review record. Then capture `commit`, `path`, `sourcePath`, `htmlDigest`, `sourceDigest`,
  `verified: true` and `pointerVerified: true`. Set the booleans only after the committed bytes
  match the render receipt and both board pointers name that artifact. Match candidate, packet
  and HTML digest to the review and approved target. A temporary HTML file is insufficient.

The report candidate must equal `review.candidate`. The only exception is a permitted correction
below the repeat threshold, captured as `review.correction`: `fromCandidate`, `toCandidate`,
`diffDigest`, `recordDigest`, `verificationPassed: true`, `materialChange: false`,
`repeatThresholdFinding: false`, `gatePassed: true`, and `gateCandidate` equal to the final candidate.
Read this append-only correction record and fresh final-candidate gate evidence before filling the
snapshot. Compare the complete intervening diff with the authorized corrections. A material change
or Critical/High fix needs a fresh full round. The original packet and four area records stay intact.
A changed final candidate always needs a new owner approval.

The checker validates captured facts, not authenticity. Independently read the referenced records,
verify their digests and run the round contract before filling the snapshot. The board close API
does not enforce this review contract. Do not bypass the local check with a direct UI or API close.

The closure checker does not re-evaluate the review cap or author independence. The referenced
`review_round.py sprint-round` receipt must validate both, including any owner round authorization.

## `sprint-priority`

Post only after the owner explicitly prioritizes the named members. A start request alone is not
this decision. It can be given in the same prompt. Store `sprint`, `briefDigest`, `scopeDigest`,
sorted `members`, `approvedBy` and `approvedAt`. Reference the returned body by key and SHA-256.
The snapshot field `priorityApproval` carries this block plus its `digest`.

An autonomous start with Backlog or BLOCKED members requires this exact approval so later Ready
transitions do not stop the run. Do not transition them until all DoR questions and blockers pass.
Every transition still uses the board's normal checks and logs the actual agent actor. The owner
remains the priority decision-maker. Changed scope or brief needs a new owner approval.


## Verification supplements

A `sprint-review` / `correction` is a new append-only comment, not an edit of a review record. Name `sprint`, `packetDigest`, `round` and the existing area record digests. Include the correction fields defined above, except `recordDigest`, which is the digest of this comment's returned body and is added only to the normalized snapshot. This avoids a self-digest. It does not add a review round or authorize a scope change.

A `sprint-review` / `report-check` is a new append-only comment for a later bounded fact check. Name `sprint`, `area: "documentation"`, `packetDigest`, `round`, the documentation record digest, checked source and HTML digests, actual reviewer, result and evidence. The earlier documentation record remains unchanged. Neither supplement enters the four `review` records passed to `sprint-round`; neither replaces a lens or changes the cap count.
