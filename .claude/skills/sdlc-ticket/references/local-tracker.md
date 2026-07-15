# Local tracker — repo-contained backend (no Jira)

A first-class **backend**, not a demo hack: tickets are JSON files in the repo, rendered by a live
HTML board. Use it for projects without Jira (internal tools, small mandates, demos). Client
mandates with Jira keep the Jira backend; same skill, same rules, same board — only the storage
adapter changes.

## When
`config.tracker == "local"` in `.claude/sdlc.config.json`, the user asks for the local/fake
board or a demo, or Jira isn't available yet. Say which backend you're driving at the start of a run.

## Architecture — state lives on a dedicated branch

**Ticket state is metadata about the work, not the work.** Like Jira, it must be orthogonal to
feature branches:

- **Tooling on the default branch:** `.sdlc/{config.json, board.html, server.py}`.
- **State on the `sdlc-state` branch (only):** `.sdlc/tickets/<KEY>.json` — one file per ticket.
  Feature branches and PRs never contain ticket-state changes.
- **Why:** a ticket's lifecycle straddles its own PR (Backlog→Planning happen before the branch,
  PO Review→Done after the merge), the default branch is protected (every label swap would need a
  PR), and a single-writer state branch makes merge conflicts impossible by construction.
- Every transition = one commit on `sdlc-state` (`chore(<KEY>): Building → PO Review`) — a free
  audit trail; the `kind=metrics` durations come from this log (`git log -- .sdlc/tickets/KEY.json`).
- **Never merge `sdlc-state` into the default branch.** Push it to share state with the team.
- **State commits never trigger CI:** auto-commits carry `[skip ci]` (honored by GitHub Actions,
  GitLab, CircleCI). Belt-and-suspenders for new repos: scope workflow triggers to the default
  branch (`on: push: branches: [main]`) rather than bare `on: push`.

`server.py` self-bootstraps everything with **plain `git worktree`** (no extra tooling): on start it
creates the `sdlc-state` branch (tracking origin's if present) and a worktree at
`../<repo>.sdlc-state`, then serves and writes tickets there.

```
python3 .sdlc/server.py      # background; board on http://localhost:8787/board.html
```

Endpoints: `GET /board` (assembled config + tickets, each with `_rev`) · `POST /ticket` (upsert one
ticket file, auto-committed) · `POST /archive` (move all Done tickets to `.sdlc/tickets/archive/`,
one commit — no UI over the archive; read/grep the files directly). Requires git ≥ 2.5.

**Optimistic concurrency:** every ticket from `GET /board` carries `_rev` (content hash). POST the
object back *with* `_rev`: the server answers **409** if the ticket changed since your read — re-fetch
`/board`, re-apply your change, retry. Always read-then-write; never POST from a stale copy. If bootstrap fails with "already checked out", someone has the
state branch checked out in another worktree — point them back to their normal branch.

## Mutating tickets — one write path

**Prefer `POST /ticket`** (curl) — validation + atomic write + auto-commit in one step:

```bash
curl -s -X POST http://localhost:8787/ticket -H 'Content-Type: application/json' -d @- <<'EOF'
{ ...full ticket object... }
EOF
```

**Markdown bodies are shell-hostile.** Comment bodies full of backticks/`$( )` get command-substituted
the moment they pass through a double-quoted shell string (e.g. inline `python3 -c "…"`) — the write
"succeeds" with a corrupted body. Build the payload in a **script file** (python via `urllib`, or a
`.json` file POSTed with `curl -d @file`); the single-quoted `<<'EOF'` heredoc above is safe, but only
as long as the JSON itself is written literally, not assembled from interpolated shell variables.

Fallback (server down): edit `../<repo>.sdlc-state/.sdlc/tickets/<KEY>.json` directly, then
`git -C ../<repo>.sdlc-state add -A && git -C ../<repo>.sdlc-state commit -m "chore(<KEY>): <what>"`.
Never edit ticket files in the main checkout — they don't exist there.

## Substrate swap
Same labels, same markers, same invariants, same gates as the Jira backend — only storage changes.

| Jira backend | Local backend |
|---|---|
| `createJiraIssue` | POST a new ticket — `{key,type,summary,status:"Backlog",labels:[],assignee:null,pr:null,comments:[]}`; next key = max existing + 1 |
| `editJiraIssue` (labels/assignee) | POST the ticket with edited fields (keep the one-pipeline-label invariant) |
| `transitionJiraIssue` | POST with new `status` |
| `addCommentToJiraIssue` | POST with `{stage,kind,body}` appended to `comments` (append-only) |
| `createIssueLink` | `links: [{type:"blocks", key}]` on the ticket |
| entity-property context-mémo | a `context` object on the ticket (same payload) |
| git / gh / codex | **real** — branch, PR, codex review as in the Jira flow (set `pr` on the ticket). In pure demo runs, simulate and narrate |

The board polls `/board` (~1.5s) — every mutation appears live. Drag-and-drop enforces the workflow
graph and writes via the same `POST /ticket`.

## Movement
- **Chat-driven (primary)** — the agent narrates and posts mutations per the skill's rules.
- **Drag (human gates)** — a human dragging a card = the human transition (Gate 1–4). Drag changes
  **only `status`**; labels and comments remain the agent's job.

## The one-ticket run (identical to Jira mode)
1. **Backlog** — shape per `shaping.md`: COS + the 7-item `readiness` object on the ticket,
   spikes (`type:"Spike"`, parent `blocked-by` them) for investigations, `owner:"human"` items for
   pending decisions; `kind=readiness` verdict comment; suggest `risk:low` if trivial.
2. ⛔ Gate 1 — human prioritizes → `Ready`. The board **hard-blocks** the drag while any
   `readiness` item is `open` or a blocker is open (`deferred` passes).
3. **Ready** — verify no open blockers, claim: `assignee`, `kind=claim`, → `Planning` + `agent:planning`.
4. **Planning** — `kind=plan` (codex drafts) → `agent:plan-review` (main agent critiques) → `kind=plan-final` → `needs:human` (skip if `risk:low`).
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
- State only on `sdlc-state`; never bundle ticket state into a feature branch or PR.
- **Reset a demo:** `git -C ../<repo>.sdlc-state revert` or `git -C ../<repo>.sdlc-state reset --hard <sha>`
  on the state branch — git is the undo, and it never touches code branches.

## Bootstrapping a new project
Copy `.sdlc/{config.json, board.html, server.py}` into the repo, edit `config.json` (project name,
statuses, `state.branch`), set `"tracker": "local"` in `.claude/sdlc.config.json`, start the server
(it creates the state branch + worktree), POST the first tickets.
