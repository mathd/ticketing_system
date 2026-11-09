# Public-read cache controls

Local Compose procedures. Run commands from the repository root. Configuration comes from
[configuration](../configuration.md); safety and approval rules remain in [AGENTS.md](../../AGENTS.md).
Load only the section needed for the operation. Historical examples retain their ticket IDs.

## Cache kill-switch (TKT-210)

Both in-memory read caches — inventory's availability cache (ADR-044) and catalog's public-read
cache (ADR-045) — can be **bypassed on a running process, without a restart or redeploy**. This is
ADR-004's incident switch: use it when a cache is suspected of serving something wrong, or to
compare behaviour with it out of the way.

The surface is `/internal/cache-control` on each service, reached **directly on its loopback port**,
not through the gateway — the edge denies `/api/<svc>/internal/*` with 404 by construction. Auth is
the shared `X-Internal-Token`; a wrong or missing token gets 401.

```bash
# inspect
curl -s -H "X-Internal-Token: $INTERNAL_SERVICE_TOKEN" localhost:8091/internal/cache-control
curl -s -H "X-Internal-Token: $INTERNAL_SERVICE_TOKEN" localhost:8090/internal/cache-control
# -> {"enabled":true,"entries":42}

# disable (purges immediately; every read then goes to the database)
curl -s -X PUT -H "X-Internal-Token: $INTERNAL_SERVICE_TOKEN" \
     -H 'Content-Type: application/json' -d '{"enabled":false}' \
     localhost:8091/internal/cache-control

# restore
curl -s -X PUT -H "X-Internal-Token: $INTERNAL_SERVICE_TOKEN" \
     -H 'Content-Type: application/json' -d '{"enabled":true}' \
     localhost:8091/internal/cache-control
```

Ports: inventory `8091`, catalog `8090` (`INVENTORY_PORT` / `CATALOG_PORT`; the smoke suite shifts
both per checkout).

**Read this before using it in an incident:**

- **It is not durable. A restart comes back ENABLED.** If you disable a cache and then roll the
  service, the cache is back and nothing will tell you. Re-check the state after any restart.
- **It is process-local.** The response describes *the process you asked*, and says nothing about
  another replica. With more than one, address each.
- **It is not a rate limiter, and disabling makes database load go UP, not down.** Every read that
  was being served from memory now reaches Postgres. Each cache still bounds its concurrent source
  calls, but that is a ceiling on concurrency, not on load. Disabling a cache during a traffic spike
  is a way to make an incident worse.
- **It does not purge anything downstream** — not the storefront's SSR cache, not a browser, not a
  future CDN. Those expire on their own tiers (`Age` keeps them from stacking; ADR-045).
- **Re-enabling starts cold**, so expect a reload wave proportional to how hot the data was.
- `entries` is **cardinality, not bytes**. A small count can hold a large payload — catalog's event
  list is one entry.
- The token authenticates a **shared machine credential**. There is no operator identity here and no
  durable record of who toggled what.
