# NATS credentials and consumers

Local Compose procedures. Run commands from the repository root. Configuration comes from
[configuration](../configuration.md); safety and approval rules remain in [AGENTS.md](../../AGENTS.md).
Load only the section needed for the operation. Historical examples retain their ticket IDs.

## NATS publisher ACLs and credential operations (ADR-072)

NATS JetStream uses per-user publish and subscribe permissions configured in `deploy/nats/nats-server.conf`
under the default `$G` account ([ADR-072](../adr/ADR-072-nats-publisher-acls.md)).

### Credential generation

Stack credentials generate automatically during `make up` via `scripts/env-bootstrap.sh`. Each of the
six principals receives an independent `/dev/urandom` draw stored in `.env`:
- `NATS_ADMIN_PASSWORD`: full broker rights, used by `nats-init`, smoke tests, and an operator's
  manual re-drive of a parked catalog event.
- `NATS_CATALOG_PASSWORD`: publish rights for catalog events.
- `NATS_COMMERCE_PASSWORD`: publish rights for commerce order events.
- `NATS_ACCESS_PASSWORD`: publish rights for access alarms and subscription rights for order/performance events.
- `NATS_INVENTORY_PASSWORD`: consumer rights only; zero platform publish rights.
- `NATS_PAYMENTS_PASSWORD`: zero publish/subscribe rights; used for connection healthcheck only.

### Quarantine reprocess needs no NATS credential

Since TKT-326, `inventory reprocess-quarantine` applies quarantined events in-process and does not
connect to NATS (see the quarantine operations section above). TKT-326 removed the operator
principal and its password from the broker configuration, Compose and both credential scripts. A
`NATS_INVENTORY_REPROCESS_PASSWORD=` line left in an existing `.env` is read by nothing and can be
deleted.

### Pre-provisioned service durables (TKT-327)

`nats-init` creates `inventory-catalog-offering` and `access-ticket-issuer` from the committed files
in `deploy/nats/consumers/`, under `platform-admin`. inventory and access cannot create them: their
filters are plural, so a create request carries the filters in its body, where no subject permission
can constrain them (ADR-072 §5, §6(a)). Each service binds to its durable at startup and compares it
with its `consumerConfig()`. A missing or different durable stops the service before it consumes,
with an error that names the durable and each field that differs (ADR-034 §Amendment:
pre-provisioned plural-filter durables).

**Migrating an existing stack.**

1. Run `make up`. It re-runs `nats-init`, which creates a durable only when INFO does not find it.
   The durables the old service code created are kept with their cursors. They carry the same
   explicitly set fields, so they pass the startup comparison (this was executed against the
   pinned broker).
2. Run `docker compose restart nats`. The removed CREATE grants stay in force until the broker
   re-reads `deploy/nats/nats-server.conf`. Compose does not recreate a container when only the
   content of a mounted file changes, so `make up` leaves `nats` on the old ACL. The services
   reconnect by themselves.
3. **Never use `make down` for this.** It runs `docker compose down -v`, which deletes every volume:
   the JetStream store and all service databases.

**When a service refuses its durable.** Its log names the durable and the differing fields. The
service never repairs the durable. An operator does, under `platform-admin`. Set up a client on the
stack's network once:

```sh
export NATS_ADMIN_PASSWORD=<the value from .env>
nats_admin() {
  docker run --rm --network ticketing_default -v "$PWD/deploy/nats/consumers:/consumers:ro" \
    natsio/nats-box:0.19.7@sha256:ffce8bd103383f179f8c7f11cf645726acf5d17280706c530c3b342dbe16334c \
    nats --server "nats://platform-admin:${NATS_ADMIN_PASSWORD}@nats:4222" "$@"
}
```

- **`inventory-catalog-offering`, a field NATS can change in place** (for example a filter or
  MaxAckPending): check the edit, then apply it, and the cursor is kept:
  `nats_admin consumer edit PLATFORM inventory-catalog-offering --config /consumers/inventory-catalog-offering.json --dry-run`,
  then the same command with `-f` in place of `--dry-run`. Executed on the pinned CLI and broker
  for a dropped filter.
- **`access-ticket-issuer`:** the pinned CLI refuses `consumer edit --config` for this durable. It
  reports that a consumer with a BackOff does not support editing Ack Wait, and it refused even
  with `ack_wait` added to the file. Delete and re-provision it, as in the next item. Use the file
  that agrees with `ACCESS_EVENT_RETRY_BACKOFF` (`access-ticket-issuer.json` for the default
  schedule).
- **Any other difference** (for example the deliver policy): delete the durable and let `nats-init`
  create it again: `nats_admin consumer rm PLATFORM <durable> -f`, then `make up`.
  **This replays.** A new durable starts at `deliver_policy: all` and redelivers every event the
  stream retains. Deduplication makes that safe for the schemas the binary knows. A retained schema
  it does not know parks the consumer unready, which is the recreation case of ADR-017 §5(b).

**A deleted durable no longer comes back by itself.** Before TKT-327 the service recreated it at
restart. Now the service exits, and the restarted container fails its bind and restart-loops until
`nats-init` runs again (`make up`). The recreation replays, as above.

**Changing a consumer's config** means changing the service's `consumerConfig()` and its file in
`deploy/nats/consumers/` together. A unit test in each consumer package compares the default file
with the code. For access, the smoke stacks use `access-ticket-issuer.smoke.json`, which must agree
with the `ACCESS_EVENT_RETRY_BACKOFF` that `scripts/stack-env.sh` sets; a smoke stack whose two
values disagree fails to start. `nats-init` does not update a durable that exists, so an existing
stack also needs the reconciliation above.

### Credential rotation

To rotate a service principal credential:
1. Generate a new 32-byte hex secret: `od -An -tx1 -N32 /dev/urandom | tr -d ' \n'`.
2. Update the corresponding `NATS_<SERVICE>_PASSWORD` in `.env`.
3. Restart NATS to pick up the new environment variable: `docker compose up -d nats`.
4. Restart the affected service container: `docker compose up -d <service>`.
5. Verify health: `docker compose ps` (confirm all services report healthy).

### Rollback

To revert to unauthenticated NATS operation:
1. In `compose.yaml`, restore `command: ["-js", "-sd", "/data", "-m", "8222"]` on the `nats` service and remove the `deploy/nats/nats-server.conf` volume mount.
2. Restore `NATS_URL: nats://nats:4222` in the `&go-env` anchor and remove per-service `NATS_URL` overrides.
3. Update `nats-init` command to remove user credentials from `nats --server`.
4. Run `docker compose up -d nats` followed by restarting all services.
