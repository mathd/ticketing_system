# Sprint operations on the board

The board keeps all sprint state. This file says how to read it and how to write it.

Every board fact below comes from the board's sprint design (`docs/sprints.md` in the board
repository, `~/sources/sdlc-board`). Where this file is silent, the design is silent. Do not guess a
board behavior. If you need one that is not here, stop and ask.

Read `sdlc-ticket/references/local-tracker.md` first. Its rules apply to every write in this file:
set `role`, never delete a comment, and build the payload in a file.

## Before any sprint operation

1. Read `.claude/sdlc.config.json`. `sprints.vault` names the vault. If it is missing, stop and
   report.
2. Read `GET /board?vault=<vault>`. `sprintMode` must be `true`. If it is not, stop and report that
   sprint mode is off for this vault. The human turns it on in the vault's `_board/config.md`
   (`sprints: true`, schema 2). Do not edit that file.
3. Put `?vault=<vault>` on every sprint route. A sprint route refuses an unknown vault name with
   400. It does not fall back to the default vault.
4. Read the sprint, never a summary of it. Counts, digests and member lists come from
   `GET /sprint`, not from a description or an old comment.

## Read

| Read | Route | What it gives |
|---|---|---|
| Sprints | `GET /sprints` | For every sprint: key, name, state, goal, dates, `rev`, member count |
| One sprint | `GET /sprint?key=S-3` | The sprint, its members, `membershipComplete`, `unreadable`, `historyGaps`, `scopeDigest`, `observedAt`, `vault` |
| Board | `GET /board` | `sprintMode`, `sprints` (the list), and the `sprint` of every ticket |
| Metrics | `GET /metrics?sprint=S-3` | Counts: `committed`, `added`, `removed`, `done`, `open`, and `carriedOut`, which is a **list** of ticket keys (its length is the count). A row for every ticket ever in the sprint, with `member` and the time counted only while it was in this sprint |
| History | `GET /history?key=K` | The membership lines of one ticket |

A member entry holds the ticket's status, its type and an archived flag. The members come from
`tickets/` and from `archive/`. Only a Done ticket is archived.

Read these fields with care:

- `membershipComplete` must be `true`, and `unreadable` must be empty. `unreadable` lists the notes
  whose sprint cannot be established: a read that failed, a frontmatter line that is not in the
  board's one-line shape (`key: <JSON value>`; an Obsidian edit can write `status: Done` unquoted),
  or a note that names the sprint without a usable key. The status of such a member is unknown. Do
  not decide anything from a list that is incomplete or has an unreadable note. Ask the human to
  rewrite the line in the board's shape.
- `scopeDigest` is the board's digest of the sprint identity, text, planned dates and member keys.
  `scripts/sprint_state.py` recomputes it and refuses a snapshot where the two differ.
- `historyGaps` reports a member whose last membership line does not name the sprint it is in, or
  that has no membership line. A gap makes history and metrics unreliable. It never decides a start
  or a close. Report it to the human.
- In metrics, a member with `attributed: null` has unknown time. It does not have zero time.
- Write every date with an offset (`2026-10-20T09:00:00-04:00`). The board refuses a date without
  one. `startDate` and `endDate` are the dates agreed with the human. They never change the state
  by themselves. `startedAt` and `closedAt` are the actual instants. The server sets them.

## Write

The board's view does not create or edit a sprint; this skill does, through the API below, at the
human's request. The view only moves tickets in and out, and starts and closes a sprint behind a
confirm.

All sprint writes use `POST` with a JSON body and `?vault=<vault>`. Send `role` on every write. Use
`sprint` for the `sdlc-sprint` skill and `sprint-review` for the `sdlc-sprint-review` skill. The
board logs `role`. It does not check it.

| Operation | Route and body | Rules that can refuse it |
|---|---|---|
| Create a sprint | `POST /sprint/new` `{name, goal?, startDate?, endDate?, role}`. Answers `201 {key}`. The state is `planned` | `one-planned` |
| Edit a sprint, or add comments | `POST /sprint` `{key, _rev, role, name?, goal?, startDate?, endDate?, comments?}` | `sprint-rev`, `closed-immutable` |
| Start | `POST /sprint/start` `{key, _rev, scopeDigest, role}` | `sprint-rev`, `one-active`, `start-state`, `start-scope` |
| Close | `POST /sprint/close` `{key, _rev, scopeDigest, role}` | `sprint-rev`, `close-state`, `close-members`, `close-scope` |
| Change membership | `POST /ticket/sprint` `{key, sprint, expect, role, reason?}` | `member-rev`, `member-type`, `member-done`, `member-target`, `closed-immutable` |

Details that matter:

- For a sprint write, `_rev` is the `rev` number that `GET /sprint` returned. It is not a status
  name. A write that changes the sprint adds 1 to `rev`; a comment write that adds a comment does
  too. A write that changes nothing (the same values, or a comment that is already there) answers
  `noop: true` with the same `rev`. So a new `rev` proves a change, and an unchanged one does not
  prove a failure. Read the sprint again before each write.
- A closed sprint accepts comments only.
- `key` in `POST /ticket/sprint` is the ticket key. `sprint` is the sprint key, or `null` for the
  backlog. `expect` is the ticket's current `sprint` value, or `null`. Read it from the ticket just
  before the write.
- `POST /ticket` ignores a `sprint` field. `POST /ticket/new` refuses a body that carries one.
  Create the ticket first, then assign it.
- A member is a Story, Task, Bug or Spike. An Epic cannot be a member. Assign its children.
  Membership does not depend on status. A Backlog ticket can be a member.
- A change into or out of an **active** sprint is a scope change. It needs a `reason` of at least
  10 characters. The board logs it. Write a real reason, for example
  `carry-over from S-2: blocked by DEMO-7`. For a planned sprint, `reason` is optional.
- Carry-over keeps the status, the labels and the assignee of the ticket. Nothing moves by itself.
  To close a sprint with unfinished work, move each unfinished member out first, to the planned
  sprint or to the backlog.

## Refusals

No sprint rule has an override. The ticket `override` field does not apply to them; a body that
carries one is refused with 400. Do not send it.

| Status | It means | Do this |
|---|---|---|
| 409 `conflict` | `sprint-rev` or `member-rev`: the sprint or the ticket changed after you read it | Read again, check your change is still right, send again with the new value |
| 422 `sprint` | A sprint rule refused it (table below) | Follow the row of the named rule |
| 409 other | Sprint mode is off, or the vault's config is newer than the board | Stop and report |
| 400 | A malformed request: bad key, missing `role`, `expect` or `sprint`, an `override`, an unknown vault | Fix the request. Do not retry it as is |
| 404 | No such sprint or ticket | Check the key |
| 503 | The board's mirror is disconnected (start or close) | Wait, then retry |
| 502 | The vault failed: **the outcome is unknown** | Treat it as an ambiguous result (§ Consistency and ambiguous results): read back before any retry |

A `409 conflict` and a `422 sprint` name the broken rule in `violations[].check`; the other 409
responses carry no `violations` (use the status table above):

| Rule | It means | Do this |
|---|---|---|
| `sprint-rev` | The sprint changed after you read it | Read it again. Check that your change is still right. Send it again with the new `rev` |
| `one-planned` | A planned sprint exists | Use that sprint, or ask the human. Never make a second one |
| `one-active` | Another sprint is active | Report it. The human decides how to finish that sprint first |
| `start-state` | The sprint is not planned | Read it again. If it is already active, read the section on ambiguous results |
| `start-scope` | The digest is stale, the sprint has no member, or its dates are missing or out of order | Read the digest again. Do not start. Fix the sprint data. The human approves the new digest |
| `sprint-dates` | A date has no offset, or `endDate` is not after `startDate` | Send dates with an offset, in order |
| `sprint-name` | The name is empty | Send a name |
| `scope-reason` | A change into or out of an active sprint without a reason of 10 characters or more | Ask the human for the reason, send it in `reason` |
| `close-state` | The sprint is not active | Read it again |
| `close-members` | A member is not Done, or a member note cannot be read | Do not close. Finish the work, or move it out with a reason |
| `close-scope` | The members changed after the human approved the close | Do not close. Read the digest again. The human approves the new scope |
| `closed-immutable` | The sprint is closed | Append comments only |
| `member-type` | The ticket is an Epic | Assign its children |
| `member-done` | A Done ticket cannot leave a sprint or join one | Leave it where it is |
| `member-target` | The sprint does not exist or is closed | Check the key. Use the planned sprint |
| `member-rev` | The ticket's `sprint` is not the `expect` you sent | Read the ticket again. Send again with the current value |

## Consistency and ambiguous results

One board server process is the only writer of sprint state. Two servers on different machines are
not protected.

- A start and a close do not trust the board's mirror. They list every ticket note in the vault
  and read each one (a few seconds; the scan stops at its first failed read and after 30 s). A
  member that the mirror has not seen yet still counts. A note whose sprint cannot be read makes
  the read incomplete, and the operation is refused. Retry it.
- A start checks `rev` and the digest of what it read, and then writes `active`, `startedAt`,
  `startScope`, `startDigest`, `startedFromRev` and the new `rev` in one note write.
- A close refuses when the board's mirror is disconnected. It checks `rev`, that every member is
  Done, and the digest of what it read. It writes `closed`, `closedAt`, `endScope`,
  `closedFromRev` and the new `rev` in one note write.
- A membership change re-reads the ticket, checks `expect`, reads the target sprint's state and
  writes the ticket.

A timeout after you sent a write is an ambiguous result. Do this:

1. Do not send the write again at once.
2. Read the sprint again with `GET /sprint` (a membership change: `GET /ticket?key=K`).
3. If the change is present, it happened. Check that it is **your** change, not another one with
   the same members: for a start, `startedFromRev` is the `_rev` you sent and `startDigest` is the
   `scopeDigest` you sent; for a close, `closedFromRev` is your `_rev` and `closeDigest` your
   `scopeDigest`; for a membership change, the ticket's `sprint` is your target. Then continue.
4. If the change is absent, send it again once. Use the new `rev` or the current `expect`.

A start sent again answers `noop` only when it is the same start: the sprint is active, `_rev`
equals `startedFromRev` and `scopeDigest` equals `startDigest`. A close sent again answers `noop`
only when the sprint is closed, `_rev` equals `closedFromRev` and `scopeDigest` equals
`closeDigest` (the digest the successful close saved). Anything else is refused with
`sprint-rev`. Still read back first: a read tells you more than a `noop`. A close that shows
`closed` after a timeout is final. Do not close again.

A `sprint` field edited by hand in Obsidian bypasses these rules when it is made. The start and
the close see it, because they read the vault. Between them it shows as a history gap, in both
directions: a member with no matching line, and a ticket whose last line names the sprint but whose
note does not. A hand-edit to the members of a closed sprint shows in `GET /sprint` as
`endScopeMismatch`: a **list** of the keys that differ from `endScope`, empty when they agree.

After a write, the server appends a line to the transition log. A failed append is reported as
`logged: false`. Report it to the human. Agents do not repair the log with `vault.py log`.

## Payload files

Comment bodies hold backticks and `$( )`. A shell runs them. Build every payload in a file:

1. Write the JSON with the Write tool to a file outside the repository, for example
   `/tmp/sprint-S-3-start.json`. A file inside the repository can reach a commit.
2. Post it with `curl`:

```sh
curl -s -X POST "http://localhost:8787/sprint/start?vault=sdlc" \
  -H 'Content-Type: application/json' -d @/tmp/sprint-S-3-start.json
```

3. Read the sprint again. Confirm the change. Delete only the files you made.

The server address is the one in `local-tracker.md`. Use the vault name from the config, not the
example above.

## Responses and identity

The board's design lists every response field (`docs/sprints.md` § Responses, in the board
repository). The points the skills depend on:

- `GET /sprint` returns every field of the sprint note, including `comments`, `startScope`,
  `endScope`, `startedAt`, `closedAt`, `rev`, `startDigest`, `startedFromRev` and `closedFromRev`.
  Each member has `key`, `summary`, `status`, `type`, `parent`, `labels`, `assignee`, `sprint` and
  `archived`.
- Its `members`, `membershipComplete` and `scopeDigest` come from the board's mirror. The start and
  the close compare the digest you send with one computed from the vault itself. A digest read from
  a lagging mirror is refused, never applied to a different scope.
- Write responses are listed in the design. Still read the sprint again after every write.
- Sprint comments merge like ticket comments: append-only, identity = `{stage, kind, author, body}`.
  The body round-trips byte for byte. The board does not restrict `stage` or `kind`; use the values
  in `records.md`. A reference to a comment is the sprint key and the SHA-256 of its body.
- Without `?vault=`, the board uses its default vault. These skills always send `?vault=`.
