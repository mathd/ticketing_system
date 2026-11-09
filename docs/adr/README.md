# Architecture decisions

Accepted decisions bind only their stated scope and amendments. A proposed decision grants no
implementation authority. Accepted requirements can remain unimplemented. Read each status and
delivery note, then verify current behavior in code. Numbers identify decisions, not release order.
ADR-073 is absent in this baseline. Check the board and open work before assigning a missing
number; a gap does not imply a missing implementation.

## Read by change

| Change | Decisions to load |
|---|---|
| Stack, service ownership, persistence, APIs | ADR-001, 002, 006, 007, 009 |
| Migrations or catalog reads/indexes | ADR-008 with 022, 019, 020 |
| Event schema or durable consumer | ADR-017, 033, 034, 072, 077 |
| Slot lifecycle, admission claims, channels, seats | ADR-005, 010, 014, 018, 023–027, 029, 031, 041, 054, 060, 061, 064, 080, 082 |
| Checkout, recovery, refund, exchange | ADR-011, 016, 032, 037–040, 062, 063, 065, 067, 069–071 |
| Prices, fees, splits, settlement | ADR-036, 046–048; ADR-084 is proposed |
| Staff, customer, partner, or credential boundary | ADR-042, 043, 049–053, 056–059, 068 |
| Ticket issuance, lifecycle, scanner | ADR-003, 012, 021, 025, 066, 074, 083, 085 |
| Read cache or image cache | ADR-004, 044, 045, 079 |
| Dependency/gate/security maintenance | ADR-028, 030, 035, 081, 086 |
| Future waitlist, rotating credentials, mail transport | ADR-075/076 are proposed; ADR-078 is accepted requirements, adapter unimplemented |

## Status register

This is a retrieval index. The linked status section and amendments govern any nuance. Update
this register when adding an ADR or changing its status. Preserve existing decision IDs.

| ID | Decision | Status and qualification |
|---|---|---|
| ADR-001 | [Go backend services, TypeScript/React frontends](ADR-001-go-typescript-stack.md) | Accepted; amended by ADR-006 |
| ADR-002 | [Services from day one, in a coarse five-service cut](ADR-002-services-from-day-one.md) | Accepted |
| ADR-003 | [Append-only audit trail for money and ticket lifecycles (NF525-ready)](ADR-003-append-only-audit-trail.md) | Accepted |
| ADR-004 | [Cache-first read path with volatility-tiered TTLs](ADR-004-cache-first-read-path.md) | Accepted; read-cache amendments ADR-044/045 and image-cache change ADR-079 |
| ADR-005 | [Unified dated-slot admission model](ADR-005-unified-dated-slot-admission.md) | Accepted |
| ADR-006 | [Astro 7 as the storefront shell (React islands + SPA checkout)](ADR-006-astro-storefront-shell.md) | Accepted; amends ADR-001 frontend shell |
| ADR-007 | [PostgreSQL per service, NATS JetStream as the event bus](ADR-007-postgres-nats.md) | Accepted |
| ADR-008 | [Embedded goose migrations, applied at service startup](ADR-008-embedded-migrations.md) | Accepted; placement superseded by ADR-022 |
| ADR-009 | [Contract-first service APIs (OpenAPI + oapi-codegen) and the domain-event envelope](ADR-009-contract-first-apis.md) | Accepted |
| ADR-010 | [PostgreSQL claim transaction and hold lifecycle](ADR-010-postgres-claim-transaction.md) | Accepted |
| ADR-011 | [Checkout finalization and canonical money journal](ADR-011-checkout-journal-protocol.md) | Accepted |
| ADR-012 | [Ticket issuance, lifecycle and QR credential boundary](ADR-012-ticket-issuance-and-qr-credentials.md) | Accepted |
| ADR-013 | [Concurrent-occupancy capping is out of the reservation claim core](ADR-013-occupancy-capping-scoped-exception.md) | Accepted scoped exception to ADR-005 |
| ADR-014 | [Typed dated-slot implementation choices](ADR-014-typed-dated-slot-implementation.md) | Accepted |
| ADR-015 | [Series and season grouping with slot-derived lifecycle](ADR-015-series-season-grouping-and-lifecycle.md) | Accepted |
| ADR-016 | [Checkout recovery state machine and journal tamper-detection boundary](ADR-016-checkout-recovery-state-machine.md) | Accepted |
| ADR-017 | [Domain-event schema evolution — when additive stops being safe](ADR-017-domain-event-schema-evolution.md) | Accepted |
| ADR-018 | [Catalog slot transition concurrency and grouped-member lifecycle ownership](ADR-018-catalog-slot-transition-concurrency.md) | Accepted |
| ADR-019 | [Catalog read-path scoping and the evidence a scoping fix owes](ADR-019-catalog-read-path-scoping.md) | Accepted |
| ADR-020 | [Catalog index builds stay non-concurrent until migrations leave startup](ADR-020-catalog-index-build-concurrency.md) | Accepted; TKT-249 amendment; ADR-022 alone does not permit concurrent builds |
| ADR-021 | [The ticket lifecycle trail is chained per ticket and checkpointed per organizer](ADR-021-ticket-lifecycle-trail-integrity.md) | Accepted; implemented by TKT-67; rollback residual remains |
| ADR-022 | [Service migrations run out-of-band, as one-shot jobs](ADR-022-out-of-band-service-migrations.md) | Accepted |
| ADR-023 | [Operational holds are claims; conversion swaps under the pool lock](ADR-023-operational-holds-and-conversion.md) | Accepted |
| ADR-024 | [Channel Allocations as Derived-Usage Rows Inside the Pool Lock](ADR-024-channel-allocations.md) | Accepted |
| ADR-025 | [Admission lifecycle events and offline reconciliation](ADR-025-admission-events-and-offline-reconciliation.md) | Accepted; amended by ADR-085 |
| ADR-026 | [Inventory Capacity Adjustment — Derived Clamp with a Pending Target](ADR-026-inventory-capacity-adjustment-clamp.md) | Accepted |
| ADR-027 | [Group Reservations as Expiring Claims; Draw-Down Reuses the Conversion Carve](ADR-027-group-agency-reservations.md) | Accepted |
| ADR-028 | [Response-schema drift fails closed with 500](ADR-028-response-drift-fail-closed.md) | Accepted |
| ADR-029 | [Seat-identity pinning contract for safe published-seat-map edits](ADR-029-seat-identity-pinning-contract.md) | Accepted; amended by TKT-318 |
| ADR-030 | [Catalog stays out of the smoke happy-path coverage gate](ADR-030-catalog-coverage-gate-scope.md) | Accepted |
| ADR-031 | [Seat-claim ↔ catalog pin coordination (hold-then-pin)](ADR-031-seat-claim-pin-coordination.md) | Accepted |
| ADR-032 | [Stripe behind a provider-agnostic PSP port](ADR-032-stripe-behind-the-psp-port.md) | Accepted |
| ADR-033 | [One domain-event envelope in the shared kernel](ADR-033-shared-domain-event-envelope.md) | Accepted |
| ADR-034 | [A shared durable-consumer lifecycle primitive — and where it stops](ADR-034-shared-durable-consumer-lifecycle.md) | Accepted |
| ADR-035 | [Eight Go modules stay; a gate enforces one version per shared dependency](ADR-035-go-module-dependency-declarations.md) | Accepted |
| ADR-036 | [Pricing rules are typed declarative rows, not a custom DSL](ADR-036-pricing-rules-representation.md) | Accepted |
| ADR-037 | [Post-purchase refunds are their own dimension, and their own payments identity](ADR-037-post-purchase-refund-money-protocol.md) | Accepted |
| ADR-038 | [A refund's reversal is a set of independent obligations, and voiding tickets is the first](ADR-038-refund-reversal-ticket-voiding.md) | Accepted |
| ADR-039 | [An exchange settles the difference — one provider movement, two gross facts](ADR-039-exchange-settles-the-difference.md) | Accepted |
| ADR-040 | [A cancellation refund is a durable run with a per-order ledger, and the refund key does not know about the run](ADR-040-event-cancellation-bulk-refund-runs.md) | Accepted |
| ADR-041 | [Where an orphan-seat rule is arbitrated](ADR-041-orphan-seat-arbitration.md) | Accepted |
| ADR-042 | [Staff identity lives in catalog; the back office holds the session](ADR-042-staff-identity-and-backoffice-sessions.md) | Accepted; TKT-191/194/197 amendments |
| ADR-043 | [Where a service's auth guard lives — declared security vs inline check](ADR-043-where-a-service-auth-guard-lives.md) | Accepted |
| ADR-044 | [Inventory serves public availability from memory](ADR-044-inventory-availability-cache.md) | Accepted |
| ADR-045 | [Catalog serves its public reads from memory](ADR-045-catalog-public-read-cache.md) | Accepted |
| ADR-046 | [Fee rules are typed declarative rows, resolved per fee code](ADR-046-fee-rules-representation.md) | Accepted |
| ADR-047 | [Payees are rows, splits are basis points, and the cents are allocated](ADR-047-payees-and-split-schedules.md) | Accepted |
| ADR-048 | [The settlement ledger balances against the capture, in the capture's transaction](ADR-048-settlement-ledger.md) | Accepted |
| ADR-049 | [Customer identity lives in commerce; the storefront holds the session](ADR-049-customer-identity-and-storefront-sessions.md) | Accepted; amended by ADR-052 |
| ADR-050 | [Transactional mail behind a port; password recovery through a queued message](ADR-050-transactional-mail-and-password-recovery.md) | Accepted |
| ADR-051 | [The public customer surface is rate limited in commerce, per subject and per source](ADR-051-public-customer-surface-rate-limiting.md) | Accepted |
| ADR-052 | [Undoing a guest-order claim](ADR-052-order-attribution-detachment.md) | Accepted; amends ADR-049 |
| ADR-053 | [The back office reads the channel registry with its own catalog credential](ADR-053-channel-operator-read-credential.md) | Accepted |
| ADR-054 | [Per-channel sales windows as DB-time predicates on the allocation row](ADR-054-per-channel-sales-windows.md) | Accepted |
| ADR-055 | [The on-sale write path is rate limited per source, and that is not a queue](ADR-055-on-sale-write-rate-limiting.md) | Accepted |
| ADR-056 | [A reseller is a third identity class, and its scope lives in the credential](ADR-056-partner-credential-identity.md) | Accepted |
| ADR-057 | [The back office edits channel allocations with its own inventory credential](ADR-057-inventory-staff-write-credential.md) | Accepted |
| ADR-058 | [Catalog verifies which organizer a back-office write is for](ADR-058-catalog-organizer-assertion.md) | Accepted |
| ADR-059 | [Every credential has a length floor, and the journal's is not the same number](ADR-059-credential-length-floor.md) | Accepted |
| ADR-060 | [A public reservation may not name a sales channel](ADR-060-public-reservation-channel-entitlement.md) | Accepted |
| ADR-061 | [How best-available seats are selected](ADR-061-best-available-seat-selection.md) | Accepted |
| ADR-062 | [Outstanding refund reversals are reconciled by a leased runner, and a missing ACCESS_URL fails readiness](ADR-062-refund-reversal-reconciliation.md) | Accepted; missing ACCESS_URL fails readiness |
| ADR-063 | [Outstanding exchange obligations are swept by a second leased runner, and the sweep never invents the switch](ADR-063-exchange-reversal-reconciliation.md) | Accepted |
| ADR-064 | [Presale unlock codes](ADR-064-presale-unlock-codes.md) | Accepted |
| ADR-065 | [Parked recovery orders are a gauge, not an alarm](ADR-065-parked-recovery-order-observability.md) | Accepted |
| ADR-066 | [Scanners pull a voided-ticket feed, and the staleness ceiling is the scanner's to enforce](ADR-066-scanner-voided-ticket-feed.md) | Accepted; amended by ADR-085 |
| ADR-067 | [A wedged exchange is unwound by an operator, and the refusal is decided by payments](ADR-067-wedged-exchange-operator-unwind.md) | Accepted |
| ADR-068 | [Staff re-delivery of a completed order's tickets](ADR-068-staff-ticket-redelivery.md) | Accepted |
| ADR-069 | [The payment token is opaque to commerce, and an exchange upgrade needs a real one](ADR-069-opaque-payment-tokens-and-exchange-upgrade-instruments.md) | Accepted |
| ADR-070 | [The reversal claims' organizer check is a column comparison, not a correlated subquery](ADR-070-indexable-reversal-claim-scoping.md) | Accepted |
| ADR-071 | [Cross-service ordering is an honest-caller guarantee, enforced in the caller and declared at the boundary](ADR-071-internal-mutation-ordering-honest-caller.md) | Accepted |
| ADR-072 | [NATS JetStream publisher access control lists](ADR-072-nats-publisher-acls.md) | Accepted |
| ADR-074 | [Associate seats with tickets at issuance](ADR-074-ticket-seat-association-at-issuance.md) | Accepted |
| ADR-075 | [Proposed waitlist capacity capture and ADR-023 amendment](ADR-075-proposed-waitlist-capacity-capture.md) | Proposed; no active policy |
| ADR-076 | [Proposed rotating ticket credentials and offline clock trust](ADR-076-proposed-rotating-ticket-credentials.md) | Proposed; no rotating-credential policy accepted |
| ADR-077 | [Moot tombstones and bounded parking for catalog offering events](ADR-077-moot-tombstones-and-bounded-parking.md) | Accepted |
| ADR-078 | [Optional Resend transport and bounded delivery retries](ADR-078-optional-resend-and-bounded-delivery-retries.md) | Accepted requirements; adapter unimplemented |
| ADR-079 | [Disable persisted Astro image stale-if-error reuse](ADR-079-persisted-image-cache-without-stale-if-error.md) | Superseded; TKT-504 upgraded Astro and accepted the residual |
| ADR-080 | [Optimistic revision on the catalog channel PUT](ADR-080-channel-optimistic-revision.md) | Accepted |
| ADR-081 | [Reviewed, expiring vulnerability scan exceptions](ADR-081-reviewed-expiring-vulnerability-scan-exceptions.md) | Accepted |
| ADR-082 | [A coded refusal for a GA sellout](ADR-082-ga-sellout-refusal-code.md) | Accepted |
| ADR-083 | [Access reads performance time zones from Catalog](ADR-083-access-reads-performance-time-zones-from-catalog.md) | Accepted |
| ADR-084 | [Promotion storage and redemption contract](ADR-084-promotion-storage-and-redemption-contract.md) | Proposed; no implementation authority |
| ADR-085 | [Scanner local outcome model](ADR-085-scanner-local-outcome-model.md) | Accepted model; staged delivery and venue qualification limits |
| ADR-086 | [Parallel CI gate parts](ADR-086-parallel-ci-gate-parts.md) | Accepted |

Use [the ADR template](<[template].md>) for a new decision. Record current implementation and
future requirements separately. Historical context inside an ADR is not an operator procedure.
