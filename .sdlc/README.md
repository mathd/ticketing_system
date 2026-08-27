# `.sdlc/` — superseded, kept for rollback only

**Do not run `server.py` from this directory.** It is the *old* git-backed board and it is no
longer the source of truth.

The live board is a separate repo:

```sh
cd ~/sources/sdlc-board
FNS_API=http://10.99.0.31:9000 FNS_VAULT=sdlc FNS_CLIENT=sdlcBoard \
  FNS_TOKEN=$(cat ~/.config/sdlc-board/token) python3 server.py 8787
```

Ticket state lives in a Fast Note Sync vault (one note per ticket), not on the `sdlc-state` branch.
See `.claude/skills/sdlc-ticket/references/local-tracker.md` for how to read and write it, and
`~/sources/sdlc-board/CUTOVER.md` for the cutover and rollback procedure.

| File here | Status |
|---|---|
| `server.py` | Superseded by `~/sources/sdlc-board/server.py`. Kept as the rollback target (tag `pre-vault-migration`). |
| `board.html` | Superseded by `~/sources/sdlc-board/board.html`. |
| `config.json` | Superseded by `_board/config.md` **in the vault**. |
| `vault-migration-plan.md` | The plan this migration followed. Historical. |

`sdlc-state` is now a **read-only archive**, refreshed by a scheduled `vault.py pull`. Never write
to it: the next pull overwrites whatever you put there.
