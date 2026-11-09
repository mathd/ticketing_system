# Operator runbooks

Local Compose only. Run commands from the repo root. Read [configuration](../configuration.md)
and the applicable [engineering rules](../conventions/engineering-rules.md) before repairs.
Commands can change durable state; documentation is not permission to execute them.

| Task | Procedure |
|---|---|
| Browser form verification | [browser-forms](browser-forms.md) |
| Public-read cache controls | [caches](caches.md) |
| Catalog rule validation | [catalog-rules](catalog-rules.md) |
| Customer accounts and recovery | [customer-accounts](customer-accounts.md) |
| Domain-event recovery | [domain-events](domain-events.md) |
| Inventory repair and seat pins | [inventory](inventory.md) |
| NATS credentials and consumers | [nats](nats.md) |
| Order, refund, and exchange recovery | [order-recovery](order-recovery.md) |
| Payment operations and journal keys | [payments](payments.md) |
| Scanner operations and outcomes | [scanner](scanner.md) |
| Scheduled workflow failures | [scheduled-workflows](scheduled-workflows.md) |
| Staff accounts and sessions | [staff-accounts](staff-accounts.md) |
| Ticket lifecycle integrity | [ticket-lifecycle](ticket-lifecycle.md) |

Start with [development](../development.md) for setup and checks. For a demo, follow the
[demo preparation steps](demo.md). Browser form changes require the submitting path in
[browser verification](browser-forms.md). Historical performance measurements and screenshots
are indexed in [records](../records.md), not live readiness guarantees.
