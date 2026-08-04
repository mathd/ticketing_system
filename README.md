# Ticketing System

Event ticketing platform — five Go services behind a gateway, TypeScript/React frontends,
built spec-first with AI-assisted development. See `docs/product/` for the brief/PRD and
`docs/adr/` for the binding decisions.

## Quickstart

```bash
make up      # first run generates a local credential (.env), then compose up --build --wait
```

`make up` is needed once per clone: no service credential is checked in, so it generates two
independent random credentials into a gitignored `.env` — `INTERNAL_SERVICE_TOKEN`
(service-to-service) and `CATALOG_STAFF_WRITE_TOKEN` (catalog writes, TKT-191). After that, plain
`docker compose up` works as usual.

Then:

- Storefront through the gateway: <http://localhost:8080/>
- Scanner shell: <http://localhost:8080/scanner/>
- Aggregated health: <http://localhost:8080/healthz/all>
- Grafana (traces/logs/metrics): <http://localhost:3000/>

## Local gate

```bash
make check                 # lint + test + build + smoke — exactly what CI runs
```

`make smoke` boots an isolated copy of the stack (compose project `ticketing-smoke`,
shifted ports) and runs the integration suite in `smoke/`; it never touches your dev stack.
`scripts/gate-selftest.sh` proves the gate fails on seeded lint/test/build errors.

Prereqs: Go 1.26+, Node 24+ (pnpm via corepack), Docker with Compose v2.

## Layout

| Path | What |
|---|---|
| `services/{catalog,inventory,commerce,payments,access}` | the five Go services (ADR-002) |
| `gateway/` | public entry point, explicit route table |
| `web/storefront` | Astro 7 SSR storefront with React components (ADR-006) |
| `web/scanner` | React/Vite gate scanner served under `/scanner/` |
| `shared/go/` | shared kernel: healthz contract + observability (`httpx`, `obs`) |
| `smoke/` | black-box integration suite through the gateway |
| `docs/` | PRD, ADRs, architecture, conventions |

See `docs/architecture.md` for the system diagram and service ownership,
`docs/development.md` for the day-to-day workflow.
