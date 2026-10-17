# The sdlc board — the ticket tracker

Tickets are notes in a Fast Note Sync (FNS) vault, rendered by a live HTML board. This is the only
tracker this skill drives (`config.tracker: "local"`). Read this file before the first board read
or write of a run; SKILL.md says *what* each stage writes, this file says *how*.

## Architecture — state lives in an FNS vault

**Ticket state is metadata about the work, not the work.** It is orthogonal to feature branches —
it lives outside git entirely.

- **Tooling:** its own repo, `~/sources/sdlc-board` (`board.html`, `server.py`, `vault.py`,
  `fnsclient.py`, `translog.py`, `boardconfig.py`). **Not** in this repo, and no longer under
  `.sdlc/`.
- **State in the vault:** `tickets/<KEY>.md` — one note per ticket, frontmatter plus body.
  `archive/<KEY>.md` once archived. Feature branches and PRs never contain ticket state.
- **Timing:** `_log/transitions.md` in the vault, one append-only line per transition. This is the
  only timing source; `kind=metrics` durations come from it, **not** from `git log` any more.
- **Board config:** `_board/config.md` in the vault carries the status list, transversal statuses
  and WIP limits. The board renders its columns from it, so the skill's rules and the board's
  columns cannot drift.

> **This replaced a git-backed board** (state on an `sdlc-state` branch, `.sdlc/server.py`, timing
> from `git log`). If you find instructions describing that, they are stale. `sdlc-state` still
> exists as a **read-only archive**, refreshed by a scheduled `vault.py pull`; nothing in the live
> path reads it, and you must never write to it.

```
cd ~/sources/sdlc-board
FNS_API=http://10.99.0.31:9000 FNS_VAULT=sdlc FNS_CLIENT=sdlcBoard \
  FNS_TOKEN=$(cat ~/.config/sdlc-board/token) python3 server.py 8787
# board on http://localhost:8787/board.html
```

Endpoints: `GET /board` (config + tickets, each with `_since`) · `GET /ticket?key=K` (one full
ticket, archive included) · `GET /history?key=K` · `GET /metrics[?key=K]` · `GET /vaults` ·
`POST /ticket` (compare-and-swap one ticket) · `POST /ticket/new` (create, key allocated by the
server) · `POST /archive` (move Done tickets to `archive/`; `?before=YYYY-MM-DD` for a cutoff).

All routes take an optional `?vault=<name>`; omit it for the default vault.

**Optimistic concurrency — `_rev` is the status you are moving FROM.** It is no longer a content
hash. The server re-reads the note, checks that field still holds `_rev`, and writes; it answers
**409** with both values if someone moved the ticket first — re-fetch `/board`, re-apply, retry.
`_rev` is **required**: a write without it is refused with 400, precisely so a stale write cannot
silently clobber. Always read-then-write.

## Mutating tickets — one write path

**`POST /ticket`** is the only write path for a ticket that already exists (creation is different —
see § Operations). Send **only the fields you are changing** plus `key`, `_rev` **and `status`** —
not the whole ticket object. `status` is required on every write even when it is not changing: omit it
and the server answers `400 status must be a string`, which reads like a payload bug rather than a
missing field.

```bash
curl -s -X POST http://localhost:8787/ticket -H 'Content-Type: application/json' -d @- <<'EOF'
{"key":"TKT-42","status":"Planning","_rev":"Ready","role":"implement","model":"claude-opus-5-5","effort":"high"}
EOF
```

- **Writable fields:** `status`, `labels`, `assignee`, `pr`, `classification`, `parent`, `type`.
  Anything else you send is ignored — deliberately. `summary`, `description`, `readiness` and
  `context` are authored in Obsidian or at shaping time, and a board write must never carry a stale
  copy of them back over a fresher one.
- **Comments are append-only.** Send `comments: [{stage,kind,body}]` with the new comment; the
  server merges by identity, so an existing comment is never duplicated and never deleted. You do
  not need to send the existing ones.
- **`role` / `model` / `effort`** identify the actor and land in `_log/transitions.md`. Set
  `role` to your pipeline role (`plan`, `implement`, `ai-review`, …); the browser sends
  `role=human`. **`role` is required on every write**, creation included: the server cannot infer
  who wrote, and refuses a write without it with `400 missing role`.
- **Status must exist** in the vault's `_board/config.md`. A typo is refused with 400 and the
  allowed list, rather than persisting into a state no column renders.

**Markdown bodies are shell-hostile.** Comment bodies full of backticks/`$( )` get
command-substituted the moment they pass through a double-quoted shell string (e.g. inline
`python3 -c "…"`) — the write "succeeds" with a corrupted body. Build the payload in a **script
file** (python via `urllib`, or a `.json` file POSTed with `curl -d @file`); the single-quoted
`<<'EOF'` heredoc above is safe, but only as long as the JSON itself is written literally, not
assembled from interpolated shell variables.

**Fallback (server down):** use `vault.py` against the vault directly, or edit the note in
Obsidian. Never write to the `sdlc-state` worktree — it is a generated archive, and a scheduled
pull will overwrite whatever you put there.

```sh
cd ~/sources/sdlc-board
python3 vault.py pull            # refresh the git archive from the vault (read-only direction)
python3 vault.py log KEY FROM TO --role human   # repair a transition line the server failed to write
```

`vault.py log` is a **repair tool only**. Agents must not use it: you set `role`/`model`/`effort` on
`POST /ticket` and the server logs the transition for you, so appending separately double-logs it.

## Operations

Every board operation the pipeline uses, and how to do it:

| Operation | How |
|---|---|
| Create a ticket | **`POST /ticket/new`** with `summary` (required), `role` (required), and any of `type` (default `Story`), `description`, `parent`, `labels`, `links`, `readiness`, `context`, `model`, `effort`, `prefix`. The server allocates the key under its write lock from `tickets/` **and** `archive/`, starts the ticket in the first status, logs a `created` line, and answers `201 {key, path, status}`. Use the returned key. Never pick `max key + 1` yourself and `put_note` it — that was last-writer-wins and could overwrite a live ticket (the server's `_create_ticket` docstring). `POST /ticket` refuses a key that does not exist. |
| Read a ticket | `GET /ticket?key=K` (or the ticket in `GET /board`); `GET /history?key=K` for its transitions |
| Set labels / assignee / `pr` | POST `{key, _rev, status, role, labels}` (keep the one-pipeline-label invariant) |
| Transition | POST `{key, _rev:<current status>, status:<new>, role, model, effort}` |
| Comment | POST `{key, _rev, status, role, comments:[{stage,kind,body}]}` (append-only, merged by identity) |
| Close an epic | See § Epics |
| Dependency | On the **blocked** ticket: `links: [{type:"blocked-by", key:"<blocker>"}]`. The board and its Gate 1 check read only `blocked-by` (`workflow.py`, `board.html`); a `blocks` link on the blocker is invisible to them. Authored in the note (below), not a board write |
| Context-mémo, `readiness`, description | A `context` / `readiness` object or the body on the note — authored in the note (below), not a board write |
| git / gh | **real** — branch, PR, review (set `pr` on the ticket). In pure demo runs, simulate and narrate |

**Authoring a note field the board does not write** (`links`, `context`, `readiness`, description):
edit the note in Obsidian, or read-modify-write it with `vault.py` — `get_note` →
`ticket_from_note` → change the field → `note_from_ticket` → `put_note`. `put_note` has **no**
compare-and-swap: write only when no other run is moving the ticket, then read the ticket back from
`GET /ticket` and confirm both your field and the current `status`/`labels`/`comments` survived.

The board polls `/board` (~1.5s) — every mutation appears live, including one made in Obsidian.
Drag-and-drop enforces the workflow graph and writes via the same `POST /ticket`.

## Workflow checks and `override`

The server checks every status change against `workflow.py`: the transition graph, the DoR and open
blockers on the way to Ready/Planning, a merged PR on the way to PO Review, and a `kind=metrics`
comment on the way to Done (spikes excepted). A refused move answers with the broken checks. A
POST may carry `override: "<reason>"`; the move then goes through and the server records the
bypass as a `kind=override` comment and on the transition log line.

**An agent sends `override` only for the epic roll-up below.** Any other refusal means a pipeline
step is missing — do the step. An override you sent for another reason goes in the closeout's
`overrides:` list.

## Epics

An epic is closed when **all its children are Done** — nothing else: no metrics comment, no
outcome check. The agent that moves an epic's last open child to Done closes the epic in the same
run.

The board has no epic transition: an epic can only leave Backlog for Ready, and Done requires a
`kind=metrics` comment (verified 2026-10-16). So the close uses `override`, which bypasses both
checks:

1. Find the children: tickets whose `parent` is the epic's key in `GET /board`. A child missing
   from the board was archived, and only Done tickets are archived.
2. Every child is Done → POST `{key, _rev:<epic status>, status:"Done", role,
   override:"epic roll-up: all children Done (<keys>)"}`.
3. Read it back.

## Movement
- **Chat-driven (primary)** — the agent narrates and posts mutations per the skill's rules.
- **Drag (human gates)** — a human dragging a card = the human transition (Gate 1–4). Drag changes
  **only `status`**; labels and comments remain the agent's job.
- **Gate buttons** — the ⛔ buttons in the ticket modal do apply the gate's label and comment
  changes. A drag does not: it moves the status only.

## The one-ticket run

> In `config.gates: "autonomous"` the ⛔ Gate 2–4 steps below are agent transitions per the skill's
> § Autonomous mode: don't set `needs:human` at those points; Gate 1 still waits for the human.

1. **Backlog** — shape per `shaping.md`: COS + the 8-item `readiness` object on the ticket (incl. `context_memo`),
   spikes (`type:"Spike"`, parent `blocked-by` them) for investigations, `owner:"human"` items for
   pending decisions; append a `kind=decision` comment for each settled material choice;
   `kind=readiness` verdict comment with the shaping decision IDs; suggest `risk:low` if trivial.
2. ⛔ Gate 1 — human prioritizes → `Ready`. The board **hard-blocks** the drag while any
   `readiness` item is `open` or a blocker is open (`deferred` passes).
3. **Ready** — verify no open blockers, claim: `assignee`, `kind=claim`, → `Planning` + `agent:planning`.
4. **Planning** — `kind=plan` (drafted by `config.models.plan`) + append-only `kind=decision`
   entries → `agent:plan-review` (review rounds per `reviews.md`: blind plan critique, then plan +
   decision-log audit by `config.models.planReview`, one `kind=review` comment per round) →
   `kind=plan-final` with the decision IDs → `needs:human` (skip if `risk:low`).
5. ⛔ Gate 2 — human approves → `Building`, swap to `agent:coding`.
6. **Building** — TDD + append-only `kind=decision` entries, PR open (`pr` field) →
   `agent:ai-review` (review rounds per `reviews.md`: blind diff review, then approved-plan +
   decision-log audit, one `kind=review` comment per round) → `kind=summary`
   with the development decision IDs plus the decision-audit yield line →
   `needs:human`.
7. ⛔ Gate 3 — human merges → `pr.state:"merged"`, → `PO Review`.
8. **PO Review** — `kind=summary` validation note; stop.
9. ⛔ Gate 4 — PO accepts → `Done`; remove `needs:human`; `kind=metrics` (with `learnings:` +
   `retro:`, copying the required decision-audit yield and adding the carried `audit-streak`).
   The board **refuses** moving a non-spike ticket to Done without a `kind=metrics` comment — post the
   closeout *before* the PO clears the gate.

`BLOCKED` transversal: status `BLOCKED`, keep the prior pipeline label, `kind=blocker` comment.

## Rules
- The invariants of SKILL.md § Labels: one pipeline label in flight, stop at `needs:human`, thread = memory.
- `kind=decision` comments are the ticket's shaping, planning and development decision logs. The
  board shows their phase counts on cards and renders the full log in the ticket's Decisions tab.
- State lives in the vault. Never write ticket state into a feature branch, a PR, or the
  `sdlc-state` worktree.
- **Always send `_rev`** and treat a 409 as a real conflict: re-read, re-apply, retry. Never retry
  by dropping `_rev`.
- **Reset a demo:** restore the notes from the `sdlc-state` archive with
  `python3 vault.py migrate --force`, or use Obsidian's file history. Note that `migrate` without
  `--force` refuses to touch a populated vault, on purpose.

## Bootstrapping a new project
Clone `~/sources/sdlc-board` once per machine. Create the project's vault in FNS, add it to the
API token's vault allowlist, write `_board/config.md` (schema, project name, statuses, transversal,
wip), set `"tracker": "local"` in `.claude/sdlc.config.json`, start the server, create the first
tickets (§ Operations). One board install serves several projects — `?vault=` selects between them and the
switcher appears once there is more than one.
