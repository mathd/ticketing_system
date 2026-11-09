# Staff accounts and sessions

Local Compose procedures. Run commands from the repository root. Configuration comes from
[configuration](../configuration.md); safety and approval rules remain in [AGENTS.md](../../AGENTS.md).
Load only the section needed for the operation. Historical examples retain their ticket IDs.

## Back-office sign-in (TKT-190)

`/admin/` is behind a staff session. Three paths stay anonymous and nothing else does:
`/admin/login`, `/admin/healthz` (Compose probes it **directly on the container**, so gating
it would make the back office unhealthy and the whole stack would fail to start) and
`/admin/_astro/*` (the hashed assets the login page itself needs). An unknown admin path is
gated exactly like a real one, so an anonymous caller cannot map the surface.

Roles are `admin`, `box_office`, and `finance`, from Catalog's `StaffRole` enum.
All three can reach the admin landing page, sign-out, and language selection. Authoring,
channels, and slot allocation pages are admin-only. The order console allows `admin` and
`box_office`; action-level checks can further restrict writes. See the current
[route-role matrix](../../web/backoffice/src/lib/authorization.ts) and
[order actions](../../web/backoffice/src/pages/orders.astro). Do not widen a route to give a
role more work. A new operation needs an explicit policy.

The route→role table is `web/backoffice/src/lib/authorization.ts`, read by both the gate and the
navigation. A page added under `src/pages/` without a row there **fails the build**; so does a row
naming a page that no longer exists.

**If one account suddenly gets a 500 at sign-in while everyone else works**, its stored `role` is
outside the vocabulary. That is deliberate: an unrecognised role must not authenticate, and it is
reported as a server-side problem rather than "wrong password" so nobody resets a password that was
never wrong. Check `staff_accounts.role` against the enum.

Provision an account with the command below. No seeded default credential exists. TKT-83 removed
the last checked-in default. The password is read from stdin only; a `--password` flag would
put it in shell history, the process table and any log that captures argv:

```bash
printf '%s' "$PASSWORD" | docker compose exec -T catalog /app provision-staff \
  --organizer-id 00000000-0000-0000-0000-000000000001 \
  --identifier ada@example.test --role admin
```

`--role` is validated against the contract vocabulary, so a typo fails here rather than at that
person's first sign-in days later.

It prints the new staff id and nothing else. Provisioning is **create-only**: a colliding
identifier fails rather than resetting a live account's password. The back office enforces the stored role through its route and action checks.

**Sessions are in-process and are not persisted.** A back-office restart signs everyone out,
and a second replica would not share them. This is accepted for a single-replica Compose staff
tool and reversible without moving the enforcement point (ADR-042). The absolute lifetime is eight
hours; it does not slide, so a stolen cookie cannot be kept alive by using it. One staff member
holds at most **five** concurrent sessions: signing in on a sixth device ends the oldest. That
cap is what bounds the session map, not the expiry sweep.

**Signing out invalidates server-side.** Replaying the captured cookie afterwards fails. The smoke suite asserts server-side refusal, not only browser cookie removal.

Catalog authenticates the back-office service credential and verifies the organizer assertion
for scoped operations. The back office enforces the individual staff member's role through
its route and action checks. These are separate controls. The route-role matrix linked above
is the current authority; the earlier TKT-191 description predates role enforcement.

`make up` generates these credentials independently. `INTERNAL_SERVICE_TOKEN` authenticates
shared internal routes in Catalog, Inventory, Commerce and Access. It does not authenticate
Payments operations; Commerce uses `PAYMENTS_INTERNAL_TOKEN` for those. The separate
`CATALOG_STAFF_WRITE_TOKEN` permits Catalog staff writes and channel-registry reads and is held
by Catalog and the back office. `INVENTORY_STAFF_WRITE_TOKEN` permits the allocation editor's
scoped availability read and allocation replacement. Commerce refunds and Access redelivery
have their own staff credentials. See [configuration](../configuration.md) and
`scripts/env-bootstrap.sh` for the full credential set. Independent random draws keep these
credentials distinct. Inventory refuses startup if its staff token equals the shared token.

**If `make up` fails on a missing credential, the bootstrap has fallen behind Compose.** That is
what TKT-227 fixed: TKT-244 made `INVENTORY_STAFF_WRITE_TOKEN` mandatory in `compose.yaml` without
adding it to `scripts/env-bootstrap.sh`, so interpolation failed with a message telling the
developer to run `make up`, the command that had just failed. `make check` never saw it, because
the smoke stage builds its environment from `scripts/stack-env.sh` instead.

```
make check-required-env    # every required var in compose.yaml is generated, non-empty
```

It reads **both** of Compose's required forms, which do not mean the same thing: `${VAR:?msg}`
fails when the variable is unset **or empty**, while `${VAR?msg}` fails only when it is unset. So
the check asks whether Compose would accept the value. A name-only comparison passes on `VAR=`
while `make up` still fails.

It bootstraps into a throwaway sandbox (never your `.env`), prints names and reasons but never
values. It runs in CI inside the `gate-selftest` job. Seeded mutations delete a name, generate an
empty value, and use the alternate `${VAR?}` requirement form. Each proves the check can fail. It is deliberately **not** in `make check`, which never starts the stack it
protects. When you add a required variable to `compose.yaml`, add it to `env-bootstrap.sh` in the
same change.

**An existing `.env` is preserved.** A run adds only what is missing; it never rotates a value you
already have. So a developer who once set `INVENTORY_STAFF_WRITE_TOKEN` equal to
`INTERNAL_SERVICE_TOKEN` by hand keeps that value, so Inventory will still refuse startup.
Delete the line from `.env` and re-run `make up` to get a fresh independent draw.

**The organizer assertion is signed with an Ed25519 pair (TKT-287, ADR-058 § Amendment).** `make up`
writes three values together: `CATALOG_ORGANIZER_ASSERTION_SIGNING_KEY` (the private seed, catalog
only), `CATALOG_ORGANIZER_ASSERTION_KID` (default `catalog-org/local-v1`) and
`COMMERCE_ORGANIZER_ASSERTION_PUBLIC_KEYS` (`kid=<public key>`, for commerce to verify with). An
`.env` from before TKT-287 keeps its values and gains the pair. The retired
`CATALOG_ORGANIZER_ASSERTION_KEY` (HMAC) is no longer read, and you can delete it. Catalog refuses to
start when the seed is not canonical raw-standard base64, or when another credential *is* the seed
in a common text form (raw bytes, any base64 spelling, hex).

To rotate, delete the seed and keyring lines from `.env` and run `make up`. **Every live assertion
then fails, and staff must sign in again.** The same is true of the deploy that first ships v2:
there is no v1 window, and catalog and the back office must be redeployed together. The back office
holds assertions only in its in-process session map, so restarting it clears them.

**Verifying a change here needs a real browser.** The smoke suite submits the login and logout
forms through the real gateway and Astro SSR layer. It sets `Origin` itself, so it cannot
prove a browser sends it on a same-origin POST, nor that a browser honours `SameSite`. Run
`make browser` and add a spec to `test/browser/` (see the ticket DoD and
`learnings/2026-07-20-browser-submit-is-the-only-checkorigin-catch.md`).
