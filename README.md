# Ticketing system

Event ticketing platform — five Go services behind a gateway, TypeScript/React frontends,
built spec-first with AI-assisted development. See `docs/product/` for the brief/PRD and
`docs/adr/` for the binding decisions.

## Quickstart

```bash
make up      # first run generates a local credential (.env), then compose up --build --wait
```

`make up` is needed once per clone. No working service credential, database password, HMAC key, or
signing seed is checked in, so the command generates the required local values in a gitignored
`.env`. After that, plain `docker compose up` works as usual.

Then:

- Storefront through the gateway: <http://localhost:8080/>
- Back office: <http://localhost:8080/admin/>
- Scanner shell: <http://localhost:8080/scanner/>
- Aggregated health: <http://localhost:8080/healthz/all>
- Grafana (traces/logs/metrics): <http://localhost:3000/>

## Local gate

```bash
make check                 # drift + contract checks, then lint + test + build + smoke — exactly what CI runs
```

`make smoke` boots an isolated copy of the stack under a path-derived Compose project and
deterministic shifted ports. It runs the integration suite in `smoke/`; its default cleanup scope
is separate from your dev stack.
`scripts/gate-selftest.sh` proves the gate fails on seeded lint/test/build errors.

Prerequisites and pinned versions: [development](docs/development.md#toolchain).
Run `make deps` before the gate on a clean clone. Use [the documentation map](docs/README.md)
to load only the files needed for your task. `make down` deletes local volumes; see
[stopping without data loss](docs/development.md#everyday-loop).

## Layout

| Path | What |
|---|---|
| `services/{catalog,inventory,commerce,payments,access}` | the five Go services (ADR-002) |
| `gateway/` | public entry point, explicit route table |
| `web/storefront` | Astro 7 SSR storefront with React components (ADR-006) |
| `web/backoffice` | Astro 7 SSR staff back office with role-gated sessions (ADR-042) |
| `web/scanner` | React/Vite gate scanner served under `/scanner/` |
| `shared/go/` | shared kernel: cross-cutting packages; see the package map in docs/architecture.md; additions require an ADR |
| `smoke/` | black-box integration suite through the gateway |
| `docs/` | PRD, ADRs, architecture, conventions |

See `docs/architecture.md` for the system diagram and service ownership,
`docs/development.md` for the day-to-day workflow.
