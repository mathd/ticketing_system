# Demo readiness — findings

Assessment run on 2026-08-08 against `main` @ `4c995e8`, on this WSL2 box, by driving the real
stack (`make up`) rather than reading tests. Investigation only: no source was changed.

## Verdict

**The product demos.** The whole v1 narrative works end to end today — browse → reserve → pay →
tickets with QR → gate scan → double-scan refused. What is missing is not capability; it is
**presentation and bootstrap**. Two blockers below stand between "it works" and "it demos well",
and both are content/config, not code.

## What was verified live

Full stack up on `make up` (all 13 containers healthy), then exercised through the gateway:

| Step | Result |
|---|---|
| Storefront event list (EN) | 200, renders events with prices |
| Storefront event list (FR) | 200, fully localized — "À partir de 45,50 €" |
| Reserve 2 GA tickets | `201`, hold with 10-min expiry, €91.00 (integer minor units) |
| Checkout via `/checkout` bridge, `fake-ok` | `200 completed`, guest order ref issued |
| Ticket delivery page | 200, both tickets `issued` → `delivered`, QR present |
| Ticket QR payload | signed EdDSA JWT (`alg:EdDSA`, `kid:access-qr/local-v1`) |
| Gate scan | `200 accepted` |
| Same ticket scanned twice | `409 already_redeemed` with original scan time |
| Back-office sign-in | `303` + session cookie, venue list renders |
| Back-office event create / order lookup | 200 |
| Grafana | 200 on :3000 |
| `make check` | **green, EXIT=0** (with no competing stack on :18080) |

Notable demo material already present: the fake PSP exposes **Success / Decline / Timeout**, so
the failure paths are demoable, not just the happy path.

## Blocker 1 — the storefront shows test litter, not a catalog

This is the one that will actually hurt in front of an audience. Every event in the database is
smoke-test residue from 2026-07-13:

```
Electric Night 38813418      <- random suffix in the display name
Electric Night fc065ec6      <- near-duplicate
Draft 38813418
Checkout flow                <- x2, an internal test name shown as an event
Contention test              <- x2, ditto
Browser Ticket Delivery
```

Venues are the same story: the three seeded ones (`La Grande Salle`, `Le Petit Théâtre`,
`Parc des Festivals`) sit among `Contention Hall` x2, `Checkout flow` x2, `TKT-105 Demo Hall`.

Two independent causes, worth separating:

1. **There is no demo seed.** `0002` seeds one organizer and `0008` seeds three venues — and that
   is all. No events, no performances, no ticket types, no inventory pools. A fresh
   `make down -v && make up` yields an **empty storefront**, which is arguably a worse demo than
   the litter.
2. **The litter is durable.** It lives in the `ticketing_pgdata` volume and survives restarts; it
   only appears because smoke suites have been run against this stack over time.

**What's missing:** a demo seed producing a small, plausible catalog — a few real-looking events
across the three seeded venues, with performances, ticket types, and inventory pools, at prices
that make the money path legible. Note `inventory_pools` is currently **empty**, so seeding
catalog rows alone is not sufficient for the buy path on seeded events.

## Blocker 2 — the back-office is locked out of the box

`staff_accounts` is **empty on a fresh stack**, and there is no seeded credential — deliberately
so (`provision_staff.go` says a seeded credential is exactly what TKT-83 removed). The bootstrap
is a CLI:

```
printf 'YourPassword\n' | docker compose exec -T catalog /app provision-staff \
  --organizer-id 00000000-0000-0000-0000-000000000001 \
  --identifier demo@example.com --role admin
```

Verified working: that command returns an account id, and sign-in then succeeds (303 + session).
Two snags a demo would hit cold:

- The binary is `/app`, not `/app/catalog` — the obvious guess fails with a confusing OCI error.
- The password must come from **stdin**; there is no `--password` flag (by design).

**What's missing:** this sequence is not written down anywhere a presenter would find it.
`docs/development.md` should carry it verbatim, or a `make demo-seed` target should do it.

## Environment note — port binding on this box, not TKT-227

`make up` failed twice before succeeding, and the reason is **not** what the board says.

- **TKT-227 did not reproduce.** The scanner image built cleanly (`Image ticketing-scanner Built`)
  with the pinned `typescript@7.0.2`. That ticket describes an arm64-macOS-shaped failure; on this
  box the in-Docker scanner build is fine. Worth updating the ticket — `scripts/browser.sh` is
  currently routed around `make up` on that ticket's authority.
- **The real failure was Docker Desktop's WSL2 port forwarding** returning HTTP 500 on
  `127.0.0.1:5432`, then `127.0.0.1:8080`, while `ss` showed both ports free and no process held
  them. Every published port is already env-overridable, so the workaround is config only:

```
GATEWAY_PORT=18080 POSTGRES_PORT=55432 make up
```

Everything in this document was verified against a stack started that way.

**Demo-day trap:** `18080` is also the smoke suite's gateway port
(`smoke: project=ticketing-smoke-0 gateway=18080`). A demo stack left up on 18080 makes
`make check` fail with `port is already allocated` — which is exactly what happened here on the
first run, and it looks like a real gate failure until you read the line. Pick a demo port that is
not 18080, or tear the demo stack down before gating.

## Not blockers, but demo-adjacent

- **`make browser` cannot run here.** The repo's own web-UI gate needs a real host Chrome; this
  box has none (`Chromium distribution 'chrome' is not found at /opt/google/chrome/chrome`), and
  `playwright-core` resolves only from the repo root. Anything demoed through a browser form is
  currently unverified by that gate on this machine. TKT-196 is the open ticket for making this
  runnable.
- **The scanner is a client-rendered SPA.** It serves an empty shell to curl, so it can only be
  demoed in a real browser with a camera or a pasted payload. Worth rehearsing before the demo —
  it is the one surface I could not exercise headlessly.
- **Known-flaky tests** that could embarrass a live gate run, all already ticketed and none
  implicated in the green run above: TKT-149 (on-sale load, 1 x 5xx in 250), TKT-198
  (`AlreadyRefunded` vs `Refunded`), TKT-218 (storefront call budget, one case fails *open*),
  TKT-213 (hermetic smoke red on main — a different path than `make check`).

## Suggested order of work

1. **Demo seed** — catalog + performances + ticket types + **inventory pools**, idempotent, kept
   out of the default migration path so it never seeds production-shaped stacks. Biggest visible
   payoff; also removes the "empty storefront on a fresh volume" failure.
2. **Write down the staff bootstrap** (and ideally a `make demo-seed` that does both 1 and 2).
3. **Pick and document a demo port pair**, away from 18080, with the `make check` conflict called
   out.
4. **Rehearse the scanner in a browser** — the one leg not verifiable from the shell.
5. Optionally correct TKT-227 with the evidence above, since it is currently steering the browser
   gate away from `make up`.
