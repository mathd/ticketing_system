# Workflow entry points

These are repository-owned workflow instructions. The active session determines which tools are
available; Claude slash-command names do not create Codex capabilities. Load one skill and only
the references needed for its current stage. Project bindings are in [sdlc.config.json](../sdlc.config.json).

| Work | Entry |
|---|---|
| One ticket from intake through delivery | [sdlc-ticket](sdlc-ticket/SKILL.md) |
| Plan or run a bounded sprint | [sdlc-sprint](sdlc-sprint/SKILL.md) |
| Review and close a sprint | [sdlc-sprint-review](sdlc-sprint-review/SKILL.md) |

Ticket state lives on the Fast Note Sync board, not in git. Read the
[tracker protocol](sdlc-ticket/references/local-tracker.md) before the first board call.
The `sdlc-state` branch is read-only history. Skills own stage contracts and configured model
routing; general prose in a contribution guide does not replace those rules. Current session
instructions and explicit owner scope still take precedence. [Hook behavior and limits](../../docs/technical-delivery-standards.md#tool-hooks-and-limits)
describes the Claude-specific guard wiring, not a universal permission boundary.
