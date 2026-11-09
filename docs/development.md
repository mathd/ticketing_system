# Development

Local Docker Compose testbed. Run commands from the repo root.

## Toolchain

Use the versions selected in [go.work](../go.work), [package.json](../package.json), and
[CI](../.github/workflows/check.yaml). At this revision, Go requires 1.27.3; CI selects
Node 24.22.2, and the package manager is pnpm 12.18.0. The gate requires the active Go minor version to match the declared
1.27 line and the pinned linter to read its export data. Patch compatibility still needs the lint
stage. See [toolchain check](../scripts/check-go-toolchain.sh). See [dependency policy](conventions/dependencies-and-versions.md)
for holds and tool upgrades. For Node 24 with Corepack, use `corepack enable pnpm` and `corepack install`. The web images
use the Node 26 pins in their Dockerfiles and install pnpm with npm. If the host has no Corepack,
install the pinned pnpm with `npm install -g pnpm@12.18.0` or your approved package-manager method.
Docker with Compose v2 is required. Browser checks also need real
host Chrome. `make lint-go` installs the pinned linter into `bin/`.

## Everyday loop

```bash
make deps
make up
```

`make up` creates missing local credentials in ignored `.env`, then builds and starts Compose
with the loopback diagnostic-port overlay. It waits for health. Open the gateway at
<http://localhost:8080/> or the `GATEWAY_PORT` you configured. An empty catalog or missing staff
account is not a boot failure. See [staff provisioning](runbooks/staff-accounts.md).

Run focused stages during work, then the full gate as a standalone command:

```bash
make lint
make test
make build
make check
```

Expect `.gate.verdict` to report `PASS` for the unchanged tree. On failure, inspect `.gate.log`
and fix the reported cause. Never edit tracked files while `.gate.lock` exists. Never call
`make check-all` directly. Commit authorized generated outputs before checking because generation
is compared with HEAD. See [testing](testing.md) for stages, smoke isolation, and browser checks.

To stop the dev stack while keeping data:

```bash
docker compose -f compose.yaml -f compose.direct-ports.yaml down
```

`make down` runs `down -v` and deletes the dev volumes. Use it only when a data reset is intended.
The base Compose file publishes only the gateway; `make up` adds diagnostic ports on loopback.
Do not omit the overlay when diagnosing published ports.

## Context by task

Use [runbooks](runbooks/README.md) for recovery and administration,
[configuration](configuration.md) for environment and key rules, and
[architecture](architecture.md) for the eight Go modules and three web apps.
Cross-service clients use `obs.Client()` for trace propagation. Grafana's provisioned checkout
view is documented with its [queries, evidence, and limits](verification/checkout-dashboard/README.md).
The [engineering rules](conventions/engineering-rules.md) apply to source, tests, and review.

## Moved procedures

The headings below preserve existing links. Each points to the current task file.

## Access failed-event recovery

See [Access failed-event recovery](runbooks/domain-events.md).

## Inventory catalog-event quarantine operations

See [Inventory catalog-event quarantine operations](runbooks/domain-events.md).

## Inventory moot slots and parked catalog events (TKT-317)

See [Inventory moot slots and parked catalog events (TKT-317)](runbooks/domain-events.md).

## Restoring `claim_history` (TKT-295)

See [Restoring `claim_history` (TKT-295)](runbooks/inventory.md).

## Seat-pin reconciliation (TKT-112)

See [Seat-pin reconciliation (TKT-112)](runbooks/inventory.md).

## Orphan-prevention correction wave (TKT-183)

See [Orphan-prevention correction wave (TKT-183)](runbooks/inventory.md).

## Best-available ordering correction wave (TKT-258)

See [Best-available ordering correction wave (TKT-258)](runbooks/inventory.md).

## Parked recovery orders (TKT-146)

See [Parked recovery orders (TKT-146)](runbooks/order-recovery.md).

## Refund reversal reconciliation

See [Refund reversal reconciliation](runbooks/order-recovery.md).

## Exchange obligation sweep

See [Exchange obligation sweep](runbooks/order-recovery.md).

## Wedged exchange operator unwind

See [Wedged exchange operator unwind](runbooks/order-recovery.md).

## Access ticket lifecycle trail operations

See [Access ticket lifecycle trail operations](runbooks/ticket-lifecycle.md).

## Event-cancellation bulk refunds (TKT-159)

See [Event-cancellation bulk refunds (TKT-159)](runbooks/payments.md).

## Journal signing key rotation

See [Journal signing key rotation](runbooks/payments.md).

## Back-office sign-in (TKT-190)

See [Back-office sign-in (TKT-190)](runbooks/staff-accounts.md).

## NATS publisher ACLs and credential operations (ADR-072)

See [NATS publisher ACLs and credential operations (ADR-072)](runbooks/nats.md).

### Pre-provisioned service durables (TKT-327)

For a durable-configuration drift error, use [durable reconciliation](runbooks/nats.md#pre-provisioned-service-durables-tkt-327).

## Customer accounts (TKT-220)

See [Customer accounts (TKT-220)](runbooks/customer-accounts.md).

### Password recovery

See [password recovery](runbooks/customer-accounts.md#password-recovery-and-the-mail-path-tkt-226).

## Cache kill-switch (TKT-210)

See [Cache kill-switch (TKT-210)](runbooks/caches.md).

## Scanner device enrolment (ai-review S1)

See [Scanner device enrolment (ai-review S1)](runbooks/scanner.md).

### Scanner Entry outcomes and local records

TKT-559 checks both wall and monotonic elapsed time from submission. Mint and send must finish
before 3 s; an admission decision must finish before 30 s. Equality is late.
A gateway-hidden refusal cannot select the offline fallback at or after 30 s.
A failed terminal write followed by actual page loss can still recover as a false admission.
Reconciliation records facts and does not decide permission. A local reservation does not prove
physical passage. Keep the page open and retry storage when a repair warning appears.
Read [the scanner runbook](runbooks/scanner.md#scanner-entry-outcomes-and-local-records) for the
complete outcome table, deadline behavior and residual limits.

### Entry rejection instructions

See [current Entry refusal instructions](runbooks/scanner.md#entry-rejection-instructions).

## Finding misconfigured rule currencies (TKT-243)

See [Finding misconfigured rule currencies (TKT-243)](runbooks/catalog-rules.md).

## When a scheduled workflow fails (TKT-213)

See [When a scheduled workflow fails (TKT-213)](runbooks/scheduled-workflows.md).

## The browser-submit gate (TKT-228)

See [The browser-submit gate (TKT-228)](runbooks/browser-forms.md).

## Conventions

See [engineering rules](conventions/engineering-rules.md) and [PR conventions](conventions/pull-requests.md).
