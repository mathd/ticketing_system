# Plan: move the SDLC board from git to a Fast Note Sync vault

> **Historical record — this migration shipped.** Ticket state now lives in an FNS vault served by
> `~/sources/sdlc-board`, and `.sdlc/` is a superseded rollback stub that must not be run (AGENTS.md).
> Kept for the reasoning behind the move, not as live instructions. Retained under `docs/` in
> TKT-312, having sat untracked inside the stub it replaced.

Implementation plan. Replaces the `sdlc-state` git branch as the board's source of truth with a
Fast Note Sync (FNS) vault, keeps a local per-dev board server, and adds a multi-project vault
switcher.

Every protocol claim carries a source reference. Claims marked *unverified* were not executed
against a running server; confirm them before relying on them.

The companion exploration note this plan was drafted alongside was a working file and was never
committed to this repository, so the trade-off discussion behind these choices is not recoverable
here. §1 records the goal and what was ruled out of scope; the decisions themselves are visible in
the plan's own sections.

---

## 1. Goal

Ticket state moves to FNS notes. A local `server.py` mirrors the vault over WebSocket, serves
`board.html`, and writes through to FNS. One local install serves several projects. Stage timing
moves from git commit history to an append-only transition log.

The `sdlc-state` branch survives as an archive that a scheduled job writes to. Nothing in the live
path reads it.

Out of scope: bidirectional git reconciliation, a centrally hosted board, changes to the
per-project `sdlc-ticket` skill beyond repointing its reads and writes.

---

## 2. Architecture

```
Board repo (own lifecycle, cloned once per dev, git pull for UI updates)
  board.html      vault switcher, columns rendered from _board/config.md
  server.py       localhost only: static files + mirror + WS client + REST write-through
  vault.py        CLI: migrate, mirror, pull, log, board. Keeps git; server.py does not.

Project repo (one per project)
  .claude/skills/sdlc-ticket/    per-project flow rules
  .claude/sdlc.config.json       names the vault this project uses

FNS vault (one per project, on 10.99.0.31)
  tickets/TKT-N.md      ticket state: frontmatter + body
  _board/config.md      statuses, transitions, WIP limits, schema version
  _log/transitions.md   append-only timing log
  archive/TKT-N.md      archived tickets
```

Writes go from `board.html` or an agent to `server.py`, then to FNS over REST. Changes return to
every connected client as a WebSocket broadcast, including the client that made the write. Reads
are served from the in-memory mirror that the WS client keeps current. Agents grep the on-disk
mirror instead of making tool calls.

The board is replicated by git because it is a file. The state is not replicated; it lives in FNS
and every board reads the same copy.

---

## 3. What changes

| Piece | Location today | After |
|---|---|---|
| `board.html` | `.sdlc/board.html` | Board repo. Gains a vault switcher. |
| `server.py` | `.sdlc/server.py` | Board repo. Loses all git code, gains a WS client. |
| `vault.py` | `.sdlc/vault.py` | Board repo. Prototype of the REST write path. |
| Ticket JSON | `ticketing_system.sdlc-state/.sdlc/tickets/*.json` | Migration source, then archive only. |
| `config.json` | `.sdlc/config.json` | Board config to the vault; flow config stays per project. |
| `sdlc.config.json` | `.claude/sdlc.config.json` | Unchanged, in the project repo. |

`server.py` serves five routes, all needing a new data source:

| Route | Today | After |
|---|---|---|
| `GET /board` | Ticket JSON from the state worktree | The in-memory mirror |
| `POST /ticket` | Write a file and commit; `_rev` guards staleness | `POST /api/note` as a compare-and-swap (Phase 3) |
| `GET /history?key=K` | `git log` for one ticket file | `_log/transitions.md`, filtered by key |
| `GET /metrics` | Stage durations from commit timestamps | Stage durations from `_log/transitions.md` |
| `POST /archive` | Move Done tickets, one commit | `POST /api/note/move` into `archive/` |

Two existing behaviours carry over usefully. `board.html` polls `GET /board` every 1500ms
(`board.html:904`) and diffs a signature before repainting (`board.html:886-890`), so it tolerates
an asynchronously updated mirror. And `POST /ticket` already carries an optimistic-concurrency
token, `_rev` (`board.html:614-615`, `server.py:317-319`), so the browser already round-trips a
staleness check. Its *meaning* changes: it stops being a git blob hash and becomes the status the
card is moving from (Phase 3).

---

## 4. Data model

### Ticket note, `tickets/TKT-N.md`

```markdown
---
key: "TKT-272"
status: "Backlog"
type: "Story"
parent: "TKT-19"
classification: "simple"
assignee: null
pr: "https://github.com/.../pull/327"
labels: ["risk: low"]
---

# TKT-272: Rate-limit the scanner polling surface

> found-in [[TKT-162]]

<description>

## Readiness
- **objective** `met` ...
```

Frontmatter holds flat scalars and string lists only, so Obsidian's properties panel can edit it.
Board writes go through `POST /api/note` as a compare-and-swap (Phase 3), not through
`PATCH /api/note/frontmatter`, which has no concurrency token.

`readiness` is seven nested objects and stays in the body. It is authored during shaping and is
not mutated over a ticket's life, so the cost of a body rewrite never arises.

`pr` is an object in the source JSON and flattens to its URL. `vault.py selfcheck` covers this.

### Board config, `_board/config.md`

```yaml
---
schema: 1
statuses: ["Backlog", "Ready", "Planning", "Building", "PO Review", "Done"]
transversal: ["BLOCKED"]
wip: {"Building": 3}
---
```

One per vault. The board renders columns from it; the project's `sdlc-ticket` skill reads the same
file so the skill's rules and the board's columns cannot drift.

This frontmatter may nest, unlike a ticket's. Only the board and the skill parse it; nobody edits
it through Obsidian's properties panel, which is what forces tickets to stay flat.

`schema` gates rendering. The board supports the current version and the one before it. On a
higher number it refuses to render that vault and says so, rather than guessing at a config it
does not understand.

### Transition log, `_log/transitions.md`

```
- 2026-08-26T20:45:47Z TKT-272 Backlog -> Ready (role=implement model=claude-opus-5 effort=high host=mbp)
```

Written with `POST /api/note/append`. Its DTO carries no `baseHash` (`note_dto.go:87-92`), so a
writer never has to read the note first, which is why the log is a note and not a field.

**Unverified: whether concurrent appends are atomic server-side.** The server necessarily reads
and rewrites the note somewhere, and nothing was checked about the transaction or locking around
it. Two simultaneous appends may lose one. Phase 0 tests this; `/metrics` and `/history` both rest
on it.

Trailing fields are `key=value` so `/metrics` can gain a dimension without a format migration and
an older parser keeps working. An actor is a role, a model and an effort level. There is one
shared token; separate tokens would only buy separate revocation or scoping, which nothing needs.

The same three fields also go on the wire, in the shorter form the header expects:
`X-Client: sdlcAgent` and `X-Client-Name: <role>/<model>/<effort>` on REST, the same values in
`ClientInfo` on WebSocket. Use `sdlcBoard` as the client type for the mirror connection so agent
writes and mirror connections are distinguishable. The log's `key=value` form is the parseable
one; the header is a convenience and nothing should parse it.

The caller supplies these fields; `server.py` cannot infer them. `POST /ticket` therefore carries
`role`, `model` and `effort` in its body, and the browser sends `role=human` with the other two
empty.

**A note's `clientName` is not an audit trail.** `internal/dto/note_dto.go:235-237` holds only the
most recent writer and is overwritten by every subsequent write. `_log/transitions.md` is the
historical record and the only source `/metrics` and `/history` may read.

### Archive

Archived tickets stay in the vault indefinitely. At this SDLC's scale the reconciliation cost does
not justify a pruning mechanism, and keeping them means search still finds closed work. Revisit if
a project runs for years and full reconciliation gets slow.

---

## 5. Protocol reference

Repository `haierkeys/fast-note-sync-service`, branch `master`, unless stated otherwise.

### 5.1 Endpoints

| Purpose | Endpoint | Source |
|---|---|---|
| WebSocket sync | `GET /api/user/sync` | `internal/routers/router_api.go:70` |
| MCP over SSE | `GET /api/mcp/sse`, `POST /api/mcp/message` | `internal/routers/router_mcp.go:22-23` |
| Note read | `GET /api/note?vault=&path=` | `docs/REST_API.md` |
| Note write | `POST /api/note` | `internal/dto/note_dto.go:23` |
| Frontmatter patch | `PATCH /api/note/frontmatter` (no concurrency token) | `internal/dto/note_dto.go:77-83` |
| Append | `POST /api/note/append` | `internal/dto/note_dto.go:87` |
| Note list | `GET /api/notes?vault=&page=&pageSize=` | `docs/REST_API.md` |
| Vault list | `GET /api/vault` | `docs/REST_API.md` |
| Token management | `/api/tokens`, `/api/token` (RequireWebGUI) | `internal/routers/router_api.go:178-179, 243-247` |

Note list ordering defaults to `mtime desc` with no parameter. `getSortField` accepts only `ctime`
and `path` as alternatives and silently falls back to `mtime` for anything else
(`internal/dao/note_repository.go:752-761`).

### 5.2 The broadcast this design depends on

Every REST note write broadcasts to all of that user's connected WebSocket clients:

```go
h.WSS.BroadcastToUser(uid, code.Success.WithData(noteNew).WithVault(params.Vault), "NoteSyncModify")
```

`internal/routers/api_router/handler_note.go`, lines 263, 410, 469, 528, 587 and 647 for
modify-shaped writes, 336 for `NoteSyncDelete`, 714 for `NoteSyncRename`. The MCP router holds the
same `wss` handle (`internal/routers/router_mcp.go:13`), so MCP writes broadcast too.

The broadcast is **per user, not per vault**, and each message is tagged with `.WithVault(...)`.
One connection therefore serves every project, and the vault switcher is a filter over messages
already arriving. `WithData(noteNew)` carries the note itself, so an update needs no follow-up
fetch.

### 5.3 WebSocket protocol

Text frames are `Action|JSON` (`docs/ws_api.md` §1.2). The JSON envelope is
`{code, status, message, data, details, vault, context}` (§1.3).

Handshake:

1. `Authorization|"<token>"`. Response carries `version`, `gitTag`, `buildTime` (ws_api.md §2.1).
2. `ClientInfo|{"name":"sdlc-board","version":"1.0.0","type":"sdlcBoard","offlineSyncStrategy":"newTimeMerge"}` (ws_api.md §2.2).
3. `NoteSync|{"vault":"<name>","lastTime":<watermark>,"notes":[...],"delNotes":[],"missingNotes":[]}`.

Server-to-client messages this client handles:

| Action | Payload | Source |
|---|---|---|
| `NoteSyncModify` | `path, pathHash, content, contentHash, ctime, mtime, lastTime` | `internal/dto/note_dto_ws.go:19` |
| `NoteSyncDelete` | `path, pathHash, ctime, mtime, size, lastTime` | `note_dto_ws.go:57` |
| `NoteSyncRename` | rename pair | `note_dto_ws.go:5` |
| `NoteSyncMtime` | timestamp only, no content change | `note_dto_ws.go:48` |
| `NoteSyncEnd` | `lastTime, needUploadCount, needModifyCount, needSyncMtimeCount, needDeleteCount, messages` | `note_dto_ws.go:31` |
| `NoteSyncNeedPush` | server wants the client to upload | `note_dto_ws.go:41` |

`NoteSyncEnd.lastTime` is the watermark for the next `NoteSync`; persist it across restarts.
`messages` is an array of `{action, data}` items in the same vocabulary (ws_api.md §7).

`NoteSyncRequest` (`internal/dto/note_dto.go:153`) is a two-way reconciliation, not "give me
everything since T". The client submits its own inventory as `notes[]` of `NoteSyncCheckRequest`
(`note_dto.go:136`: `path, pathHash, contentHash, mtime, ctime`), batched via `batchIndex` and
`totalBatches`, and the server replies with what to change. Send the mirror's inventory so the
server computes a real delta. An empty `notes[]` claims the client holds nothing, which is right
for a first sync and wasteful on every reconnect.

Status codes: `1` success, `6` already current, `441` content conflict, `490` sync conflict (ws_api.md §8).

Reference implementation, repository `haierkeys/obsidian-fast-note-sync`:
`src/lib/sync/websocket_client.ts`, `websocket_manager.ts`, `websocket_action.ts`,
`operator_note.ts`. Read these before writing the client.

### 5.4 Hashes

`pathHash` and `contentHash` are client-computed. `docs/REST_API.md` describes "a 32-bit hash
algorithm (e.g., FNV-1a)". The implementation is Java's `String.hashCode`, not FNV-1a
(plugin repo, `src/lib/utils/helpers.ts:423-439`):

```js
hash = (hash << 5) - hash + char   // hash * 31 + char
hash &= hash                        // coerce to signed int32
return String(hash)                 // decimal string, may be negative
```

A Python port must iterate **UTF-16 code units**, not Python code points. Encode with `utf-16-le`
and unpack to 16-bit units first. Otherwise any ticket containing an emoji or other non-BMP
character hashes differently and every sync comparison for that note fails.

### 5.5 Timestamps

Two documents disagree. `docs/REST_API.md` §Timestamp Format says `ctime`, `mtime`,
`updatedTimestamp` and `lastTime` are all milliseconds. `docs/ws_api.md` closing note says every
`int64` timestamp is seconds except `lastTime`, which is milliseconds. The DTO examples
(`note_dto.go:31-32`) show `1700000000`, which is seconds. Resolve empirically in Phase 0 by
writing one note and reading it back. A 1000x error here breaks delta sync silently.

`mtime` is client-supplied (`note_dto.go:32`); `lastTime` is server-assigned (`note_dto_ws.go:26`).
Key the **watermark** on `lastTime`, never on `mtime`: a watermark keyed on `mtime` trusts the
writing client to be honest and fails open when it is not.

This cannot extend to the whole reconciliation. `NoteSyncCheckRequest` compares on `mtime` and
`contentHash` by protocol design (§5.3), so the inventory has no choice. `contentHash` is at least
derived from content the server also holds, so it is checkable in a way `mtime` is not.

---

## 6. Prerequisite: a correctly scoped token

Scopes are `p:<protocol> c:<clientType> f:<function>`. The middleware reads `dbToken.Scope` from
the database on every request with no cache (`internal/middleware/user_auth_token.go:132`), so a
scope change applies immediately and a token that still fails has not been granted. Tokens also
carry a vault allowlist, enforced separately (`user_auth_token.go:149`).

This work needs `p:` covering `rest` and `ws`, and a vault list covering every project vault.

No phase uses MCP. Every write goes through `server.py` over REST, and reads come from the mirror.
Add `mcp` to the scope only if you later want an agent talking to FNS directly, which means
bypassing the Phase 3 compare-and-swap and should be a deliberate decision.

Mint it from the WebGUI at `http://10.99.0.31:9000` with "Copy API config".

`POST /api/token` can also mint tokens programmatically. The token routes sit on `webguiGroup`,
which is `auth.Group("")` with no path prefix (`router_api.go:178-179`), so the paths are
`/api/token` and `/api/tokens`. `RequireWebGUI` (`internal/middleware/webgui_auth.go`) gates them
on an `x-client: webgui` header and, when a token context exists, `IssueType == 1` and
`ClientType` matching `webgui`. Manual API tokens are `IssueType == 2` and refused by name, so an
existing API token cannot mint another; a scripted `POST /api/user/login` yields a login token that
can. `gin.DebugMode` bypasses the middleware entirely.

Verify before building:

```
curl -s -H "Authorization: Bearer $T" "http://10.99.0.31:9000/api/vault"
curl -s -H "Authorization: Bearer $T" "http://10.99.0.31:9000/api/notes?vault=sdlc&pageSize=1"
```

`315 Auth token Scope restricted` means the scope is still wrong. Do not proceed; every later step
fails in a way that looks like a bug in your own code.

---

## 7. Build phases

Each phase ends in something runnable. Do not start the next until the current one is proven.

### Phase 0: token and reconnaissance

Mint the token per §6. Resolve §5.5 by writing one throwaway note and reading it back. Record the
answer at the top of the board repo's README; everything downstream depends on it.

Also settle the open question in §4: fire fifty concurrent `POST /api/note/append` calls at one
throwaway note and count the resulting lines. Fifty means appends are atomic and the transition log
is safe as designed. Fewer means the log needs a single writer, which changes Phase 4.

**Done when:** `GET /api/vault` lists your vaults, the timestamp unit is a stated fact, and the
append count is known.

### Phase 1: board repo and migration

Create the board repo and move `board.html`, `server.py` and `vault.py` into it unchanged. Add
`vault.py migrate`, reading `sdlc-state` ticket JSON and writing one note per ticket in the §4
format.

**Done when:** all 266 tickets exist as notes and a spot check of ten confirms bodies survived
colons, backticks, quotes and newlines intact.

### Phase 2: read path over WebSocket

Add the WS client to `server.py`. Connect, authenticate, `ClientInfo`, then one `NoteSync` **per
vault** with that vault's inventory and watermark. Apply `NoteSyncModify`, `NoteSyncDelete` and
`NoteSyncRename` to both the in-memory board and the on-disk mirror. Persist `lastTime` from each
`NoteSyncEnd`.

Broadcasts arrive for every vault on one connection (§5.2), but reconciliation does not:
`NoteSync` names a single vault, so connecting means looping over every mirrored vault, and
`lastTime` is **per vault**, not one global watermark. Getting this wrong looks like one project
syncing correctly while another silently never catches up after a reconnect.

Repoint `GET /board` at the in-memory mirror. Delete `bootstrap_state_worktree` and every other
git call **from `server.py`**. `vault.py` keeps its git code; the cutover archive in §8 needs it.

**The board is read-only for the whole of Phase 2.** Removing the git write path strands
`POST /ticket`, which is not rewritten until Phase 3, so have it return 503 and disable dragging.
This is the same banner the offline case uses. Do not leave a write path pointing at a store that
no longer exists.

Make the client structurally read-only: implement `Authorization`, `ClientInfo` and `NoteSync` as
the only client-to-server actions, and do not implement `NoteModify`, `NoteDelete` or `NoteRename`
at all. Writes go through the REST path in Phase 3. A mirror bug then cannot corrupt the shared
vault because the code to do so does not exist.

Reconnect with backoff and re-run `NoteSync` on every reconnect. A dropped connection means missed
broadcasts, and the delta sync is the only thing that repairs it. Reconnect is the normal case,
not an error path.

Offline read comes free here: when the socket is down, serve `GET /board` from the on-disk mirror
and mark the board read-only with a banner naming the age of the data. That is a connection-state
flag and a disabled drag handler.

**Done when:** a note edited in Obsidian on a phone reaches the board in about a second; killing
the connection for a minute and restoring it converges without a restart; and starting `server.py`
with the server unreachable still renders yesterday's board read-only.

### Phase 3: write path over REST

Rewrite `POST /ticket` as a compare-and-swap on the status field.

`PATCH /api/note/frontmatter` cannot carry board writes. Its DTO has no concurrency token
(`note_dto.go:77-83`), so it is a server-side read-modify-write that always wins, and two people
moving one card produce one silent loser. Only `POST /api/note` accepts `baseHash`
(`note_dto.go:27`).

The write:

1. Read the note. Confirm the guarded field still holds the value the client is moving *from*.
2. Write the whole note with `baseHash` set to the hash you just read.
3. On `441`, re-read and re-check that field:
   - **unchanged** (someone edited the body): re-apply the field change and retry automatically.
   - **changed**: a real conflict. Stop and return both values to the browser.

Bound the automatic retry at three attempts so a hot note cannot spin.

Whole-note `baseHash` on its own rejects unrelated body edits, which trains people to click
through conflict dialogs. The field check on its own misses lost updates. Together they detect
real conflicts without false alarms.

`_rev` in `board.html` (`board.html:614-615`) becomes the status the card is moving from, not a
git blob hash.

Writes broadcast back over the WS connection (§5.2), so the mirror updates through the same path
as a remote change. Do not also apply the write locally: one path is easier to reason about, and
it exercises the read path on every write.

The same rule applies to offline replay (Phase 7), so there is one policy and one code path.

**Done when:** dragging a card on machine A moves it on machine B with neither browser reloading;
editing a description in Obsidian and then moving that card succeeds without a conflict prompt;
and two racing drags from the same status produce one winner and one visible conflict.

### Phase 4: transition log, history and metrics

**`server.py` is the only writer of the transition log**, and it appends from inside the same
`POST /ticket` handler that performs the Phase 3 write. Agents do not append; they set `role`,
`model` and `effort` on the `POST /ticket` body and are logged by the server like any other
caller. A Claude Code hook calling `vault.py log` separately would double-log every agent
transition, and an agent writing straight to FNS would skip the compare-and-swap. Neither is
wanted. `vault.py log` stays as a manual repair tool for a line the server failed to write.

**Order: append only after the write is confirmed.** The two operations have no transaction
between them, so pick the failure you prefer. Appending first records transitions that never
happened and corrupts `/metrics` with no way to tell. Appending second loses a line when the
append fails, which leaves `/metrics` incomplete but never wrong. Take the second, and log the
failure locally so the gap is visible and `vault.py log` can repair it.

If Phase 0 found appends are not atomic, serialise them behind a lock in `server.py`. That is
sufficient because `server.py` is the only writer.

Rebuild `GET /history?key=K` as a filter over that log and `GET /metrics` as stage durations
derived from it. Write both from the log's contents, not from the old git shapes.

**Done when:** `/metrics` returns stage durations for a ticket that has moved at least three
times, and the numbers match what the log shows by eye.

### Phase 5: multi-project switcher

Populate the switcher from `GET /api/vault`. Render columns from the selected vault's
`_board/config.md` rather than from constants in `board.html`. The WS connection already receives
every vault; filter on the `vault` envelope field.

Point the per-project `sdlc-ticket` skill at the same `_board/config.md`.

**Done when:** two vaults with deliberately different status lists both render correctly, and
switching between them needs no reconnect.

### Phase 6: archive and backup

Rewrite `POST /archive` onto `POST /api/note/move` into `archive/`. Configure FNS GitSync for
backup with a low `Delay`.

GitSync is a backup, not an audit trail and not a timing source, which is why Phase 4 exists.
`NotifyUpdated` stops and re-arms the timer on every change
(`internal/service/git_sync_service.go:1119-1160`), so `Delay` is a debounce and continuous
activity postpones the commit indefinitely. Every commit carries a fixed service identity and the
hardcoded message `"Update from Sync Service"` (`git_sync_service.go:801`). Its `retentionDays`
prunes `GitSyncHistory` rows, the sync job's own status log in the database
(`git_sync_service.go:651-670`); it never touches notes. `-1` keeps only the latest record, `0`
disables cleanup, positive is a day count.

**Done when:** a transition reaches the GitHub mirror within a couple of minutes, and reading a
commit confirms it tells you nothing about who changed what.

### Phase 7: offline writes

After cutover, and only if Phase 2's read-only offline proves insufficient.

The protocol supports this. `NoteSyncRequest` is a reconciliation taking the client's full
inventory with hashes and mtimes (§5.3). Read `operator_note.ts` and `file_hash_manager.ts` in the
plugin before designing anything.

The policy is Phase 3's compare-and-swap, not an `offlineSyncStrategy` setting. That field is
declared in `ClientInfo` but the branching on it lives in the *plugin*
(`websocket_manager.ts:592`), so it governs how the Obsidian client resolves its own conflicts and
not what the server does to ours. Its documented values are `newTimeMerge` and `ignoreTimeMerge`;
a third, `manualMerge`, exists in the plugin and writes base and remote copies into a
`conflict-notes/` directory. Useful as a reference for presenting a conflict. Not a lever we pull.

Build:

1. **A durable outbox.** An append-only local file of pending writes, surviving restart, or a
   transition is lost by closing a laptop.
2. **Optimistic local application.** Apply to the mirror immediately and mark the ticket pending.
3. **Replay on reconnect**, after `NoteSync` converges, never before. Replaying against a stale
   mirror re-creates the conflict you are resolving.
4. **The Phase 3 compare-and-swap, deferred.** Replay re-reads, checks the field still holds the
   expected value, applies if so and surfaces both versions if not. Identical rule, later.
5. **A TTL on the outbox**, about a day. A transition queued five minutes ago is almost certainly
   still right; one queued three days ago is a guess about a world that has moved, and applying it
   is worse than dropping it with a notice.

The binding constraint is semantic, not technical. Two people moving the same ticket offline is
not a synchronization bug, it is two incompatible decisions, and no merge strategy makes both
correct. Scope this to one person, their own tickets, few transitions. Do not build general
multi-writer offline merge.

**Done when:** a transition made with the server unreachable survives a restart of `server.py`,
lands when connectivity returns, and one that raced a remote change is shown to its author rather
than silently applied or dropped.

---

## 8. Cutover

Run both boards against the same work. The vault board is read-write. The git board is read-only,
fed by a scheduled `vault.py pull` that writes ticket JSON into the `sdlc-state` worktree and
commits it. Phase 2 strips git from `server.py`, not from `vault.py`, which is what keeps this
possible.

Compare daily by diffing the two: `vault.py pull` then `git -C <state-worktree> diff --stat`. An
empty diff after a day of work means the boards agree. After five consecutive clean days, stop
reading the git board.

Keep the scheduled pull permanently. It costs one cron line and it is what makes the archive real.

**Rollback is a checkout, not a switch.** The old board is `.sdlc/server.py` reading the state
worktree, and Phase 2 rewrote it. Rolling back means running the project repo at its
pre-migration commit, against a `sdlc-state` branch the pull job has kept current. Tag that commit
before starting Phase 2 so the rollback target is named rather than hunted for.

---

## 9. Traps

Ordered by cost if missed.

1. **Timestamp units** (§5.5). Wrong by 1000x, silent, breaks delta sync.
2. **Hash algorithm** (§5.4). The documented algorithm is not the implemented one, and the UTF-16
   detail bites only on tickets containing emoji, which is the worst failure profile.
3. **`mtime` versus `lastTime`** (§5.5). Keying on `mtime` builds a guard whose unknown case is
   "no change". It fails open and looks fine.
4. **Reconnect is the repair mechanism, not an error path.** Push alone loses every event that
   occurs while disconnected; without `NoteSync` on reconnect the board is silently stale until a
   restart. `NoteSync` and its `lastTime` are **per vault** while broadcasts are per connection, so
   a single global watermark leaves one project permanently behind while another looks fine.
5. **`PATCH /api/note/frontmatter` has no concurrency token** (`note_dto.go:77-83`). It is the
   obvious endpoint for a single-field board write and it silently drops the loser of any race.
   Use the Phase 3 compare-and-swap instead.
6. **No client-to-server write actions in the WS client** (Phase 2).
7. **A read-only client may receive `NoteSyncNeedPush`.** The server believes the client holds a
   newer copy. Log it and ignore it; do not implement the upload it asks for.
8. **The vault is a single point of failure.** Between cutover and Phase 7 a disconnected dev can
   look and cannot move a card. Confirm that is acceptable before Phase 3.
9. **`server.py` is unauthenticated because it binds `127.0.0.1`** (`server.py:387`). That
   assumption is load-bearing. Do not change the bind address without adding authentication.
10. **A green test against the in-memory mirror proves nothing about the vault.** Put the assertion
   at the tier the mechanism lives at.

---

## 10. Verification

Each of these must be shown to fail before it is trusted.

| Claim | Test | Mutation that must turn it red |
|---|---|---|
| Round trip is lossless | `vault.py selfcheck` | Emit raw scalars instead of JSON-encoded |
| Hash matches the server | Hash a known note, compare to the server's `contentHash` | Iterate code points instead of UTF-16 units |
| Delta sync repairs a gap | Disconnect, write from another client, reconnect | Skip `NoteSync` on reconnect |
| Concurrent appends do not lose lines | 50 parallel appends to one note, count lines | Remove the serialising lock, if Phase 0 showed one is needed |
| Each vault catches up after a reconnect | Disconnect, write to vault B, reconnect | Share one `lastTime` across vaults |
| A real conflict is surfaced | Two clients move one card from the same status | Skip the field re-check on `441` and always retry |
| An unrelated edit does not conflict | Edit the body, then move the card | Remove the automatic retry on `441` |
| Metrics match the log | Compute for a 3-transition ticket | Remove one line from `_log/transitions.md` |
| Board is scoped to a vault | Two vaults, distinct tickets | Ignore the `vault` envelope field |

The board is a web UI with write forms, so `AGENTS.md`'s browser rule applies: a drag that moves a
card is not verified until a browser has performed it and the vault has been re-read to confirm
the write landed.

