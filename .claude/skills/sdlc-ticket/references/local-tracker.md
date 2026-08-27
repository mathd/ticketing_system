# Local tracker — vault-backed backend (no Jira)

A first-class **backend**, not a demo hack: tickets are notes in a Fast Note Sync (FNS) vault,
rendered by a live HTML board. Use it for projects without Jira (internal tools, small mandates,
demos). Client mandates with Jira keep the Jira backend; same skill, same rules, same board — only
the storage adapter changes.

## When
`config.tracker == "local"` in `.claude/sdlc.config.json`, the user asks for the local/fake
board or a demo, or Jira isn't available yet. Say which backend you're driving at the start of a run.

## Architecture — state lives in an FNS vault

**Ticket state is metadata about the work, not the work.** Like Jira, it is orthogonal to feature
branches — it simply lives outside git entirely now.

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

Endpoints: `GET /board` (config + tickets, each with `_since`) · `GET /history?key=K` ·
`GET /metrics` · `GET /vaults` · `POST /ticket` (compare-and-swap one ticket) ·
`POST /archive` (move Done tickets to `archive/`; `?before=YYYY-MM-DD` for a cutoff).

All routes take an optional `?vault=<name>`; omit it for the default vault.

**Optimistic concurrency — `_rev` is the status you are moving FROM.** It is no longer a content
hash. The server re-reads the note, checks that field still holds `_rev`, and writes; it answers
**409** with both values if someone moved the ticket first — re-fetch `/board`, re-apply, retry.
`_rev` is **required**: a write without it is refused with 400, precisely so a stale write cannot
silently clobber. Always read-then-write.

## Mutating tickets — one write path

**`POST /ticket`** is the only write path for a ticket that already exists (creation is different —
see § Substrate swap). Send **only the fields you are changing** plus `key`, `_rev` **and `status`** —
not the whole ticket object. `status` is required on every write even when it is not changing: omit it
and the server answers `400 status must be a string`, which reads like a payload bug rather than a
missing field.

```bash
curl -s -X POST http://localhost:8787/ticket -H 'Content-Type: application/json' -d @- <<'EOF'
{"key":"TKT-42","status":"Planning","_rev":"Ready","role":"implement","model":"claude-opus-5","effort":"high"}
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
  `role=human`. The server cannot infer these — if you omit them the transition is logged as
  `human`.
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

## Substrate swap
Same labels, same markers, same invariants, same gates as the Jira backend — only storage changes.

| Jira backend | Local backend |
|---|---|
| `createJiraIssue` | **Not `POST /ticket`** — that is a compare-and-swap over an existing note and refuses one that does not exist (`cannot read tickets/<KEY>.md`), with or without `_rev`. Create the note directly, reusing the board's own renderer so the format cannot drift: import `note_from_ticket` + `put_note` from `vault.py` (env: `FNS_API`, `FNS_VAULT`, `FNS_CLIENT`, `FNS_TOKEN`), render `{key,type,summary,description,status:"Backlog",labels:[],assignee:null,pr:null,comments:[]}` and `put_note(f"tickets/{key}.md", …)`. Next key = max existing + 1. Then drive it with `POST /ticket` as usual. (TKT-209 needed this for a follow-up ticket; the old row said to POST it and that does not work.) |
| `editJiraIssue` (labels/assignee) | POST `{key, _rev, labels}` (keep the one-pipeline-label invariant) |
| `transitionJiraIssue` | POST `{key, _rev:<current status>, status:<new>}` |
| `addCommentToJiraIssue` | POST `{key, _rev, comments:[{stage,kind,body}]}` (append-only, merged by identity) |
| `createIssueLink` | `links: [{type:"blocks", key}]` — authored in the note, not a board write |
| entity-property context-mémo | a `context` object on the ticket — authored in the note, not a board write |
| git / gh / codex | **real** — branch, PR, codex review as in the Jira flow (set `pr` on the ticket). In pure demo runs, simulate and narrate |

The board polls `/board` (~1.5s) — every mutation appears live, including one made in Obsidian.
Drag-and-drop enforces the workflow graph and writes via the same `POST /ticket`.

## Movement
- **Chat-driven (primary)** — the agent narrates and posts mutations per the skill's rules.
- **Drag (human gates)** — a human dragging a card = the human transition (Gate 1–4). Drag changes
  **only `status`**; labels and comments remain the agent's job.
- **Gate buttons** — the ⛔ buttons in the ticket modal do apply the gate's label and comment
  changes. A drag does not: it moves the status only.

## The one-ticket run (identical to Jira mode)

> In `config.gates: "autonomous"` the ⛔ Gate 2–4 steps below are agent transitions per the skill's
> § Autonomous mode: don't set `needs:human` at those points; Gate 1 still waits for the human.

1. **Backlog** — shape per `shaping.md`: COS + the 8-item `readiness` object on the ticket (incl. `context_memo`),
   spikes (`type:"Spike"`, parent `blocked-by` them) for investigations, `owner:"human"` items for
   pending decisions; `kind=readiness` verdict comment; suggest `risk:low` if trivial.
2. ⛔ Gate 1 — human prioritizes → `Ready`. The board **hard-blocks** the drag while any
   `readiness` item is `open` or a blocker is open (`deferred` passes).
3. **Ready** — verify no open blockers, claim: `assignee`, `kind=claim`, → `Planning` + `agent:planning`.
4. **Planning** — `kind=plan` (drafted by `config.models.plan`) → `agent:plan-review` (critiqued by
   `config.models.planReview`) → `kind=plan-final` → `needs:human` (skip if `risk:low`).
5. ⛔ Gate 2 — human approves → `Building`, swap to `agent:coding`.
6. **Building** — TDD, PR open (`pr` field) → `agent:ai-review` → `kind=summary` → `needs:human`.
7. ⛔ Gate 3 — human merges → `pr.state:"merged"`, → `PO Review`.
8. **PO Review** — `kind=summary` validation note; stop.
9. ⛔ Gate 4 — PO accepts → `Done`; remove `needs:human`; `kind=metrics` (with `learnings:` + `retro:`).
   The board **refuses** moving a non-spike ticket to Done without a `kind=metrics` comment — post the
   closeout *before* the PO clears the gate.

`BLOCKED` transversal: status `BLOCKED`, keep the prior pipeline label, `kind=blocker` comment.

## Rules
- Same invariants as Jira mode: one pipeline label in flight, stop at `needs:human`, thread = memory.
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
wip), set `"tracker": "local"` in `.claude/sdlc.config.json`, start the server, POST the first
tickets. One board install serves several projects — `?vault=` selects between them and the
switcher appears once there is more than one.
