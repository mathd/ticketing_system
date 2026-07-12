# ADR-008: Embedded goose migrations, applied at service startup

Date: 2026-07-12

## Status

Accepted (approved at the TKT-26 plan gate, 2026-07-12)

## Context

US-002 (TKT-26) creates the first database schema in the platform (catalog). No migration
tooling exists yet; whatever this story picks becomes the convention for the other four
services. Constraints: one Postgres cluster with one database + one role per service
(ADR-007), Docker Compose only, a gate (`make check`) that must work from a clean clone,
and services that must own their schema without a shared migration coordinator crossing
service boundaries (ADR-002).

## Possible Solutions

- **Option 1 — `pressly/goose` v3 as a library, SQL files embedded (`embed.FS`), applied at startup before listen (chosen):**
    - Pros: zero extra containers or CLI steps — `docker compose up` and the smoke stack migrate themselves; migrations ship inside the service binary (distroless-friendly); per-service ownership falls out of the ADR-007 one-DB-per-service cut; plain SQL files, reviewable in PRs.
    - Cons: startup-time migration couples deploy and migrate (fine for Compose-scale; revisit if rollout orchestration ever appears); concurrent replicas racing migrations needs goose's locking (single-instance services today).
- **Option 2 — `golang-migrate` CLI in a one-shot compose container per service:**
    - Pros: migration decoupled from service start.
    - Cons: five extra one-shot containers + image plumbing; the smoke stack and CI must sequence them; more moving parts for no current benefit.
- **Option 3 — hand-rolled `schema.sql` applied by an init script:**
    - Pros: no dependency.
    - Cons: no versioning/idempotence story; diverges immediately once two stories touch the same schema.

## Decision

We adopt **goose v3 as a library**: each Go service embeds its `migrations/*.sql` via
`embed.FS` and runs `goose.Up` against its own database at startup, before listening.
Seed data that is product-mandated (the single v1 organizer, AC5 of US-002) is a
migration like any other. The service fails fast if migration fails.

## Consequences

- **Positive:**
    - Clean clone → `make check` → migrated stack, with no new lifecycle steps anywhere.
    - Schema changes travel in the same PR and binary as the code that needs them.
- **Negative:**
    - Deploy = migrate; a bad migration blocks service start (accepted: fail-fast is the
      desired behavior at this scale).
    - Startup ordering with future multi-replica services needs goose's advisory-lock
      support — to be revisited when a second replica exists.

## References

- TKT-26 (US-002) · [ADR-002](./ADR-002-services-from-day-one.md) · [ADR-007](./ADR-007-postgres-nats.md)
- [pressly/goose](https://github.com/pressly/goose)
