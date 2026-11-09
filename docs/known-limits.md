# Known implementation limits

Current code and accepted decisions establish these boundaries. This list is not a live backlog or
a full audit. Check the board for ticket status and owner priority.

| Area | Implemented boundary or remaining requirement | Canonical reference |
|---|---|---|
| Currency | Catalog stores ISO currencies; Commerce currently sells in EUR only. Multi-currency selling is TKT-10. | `services/commerce/internal/api/catalog_pricing.go`, resolved-price validation |
| Payments | Fake and Stripe test-mode adapters exist. Live keys are refused. No live PSP operation. | [ADR-032](adr/ADR-032-stripe-behind-the-psp-port.md) |
| Identity and operations | Single-replica sessions and abuse controls, bearer service credentials, local Compose only. No cloud release pipeline or production recovery estate. | [Security](conventions/security.md), [CI](conventions/continuous-delivery.md) |
| Integrity | Lifecycle modification/insertion detection has a defined trust boundary. Targeted rollback and current-key compromise remain open. No fiscal certification follows from chaining. | [ADR-021](adr/ADR-021-ticket-lifecycle-trail-integrity.md), [French-scope investigation](spikes/TKT-374-fr-cash-scope.md) |
| Scanner | Host-Chrome evidence and provisional pre-decision bounds do not qualify venue hardware. TKT-560 and owner bound/offline-spill choices remain venue-use prerequisites. Browser storage and page-loss residuals remain. | [Scanner runbook](runbooks/scanner.md), [ADR-085](adr/ADR-085-scanner-local-outcome-model.md) |
| Product proposals | Waitlist capture, rotating credentials, and promotion storage proposals authorize no implementation. The accepted Resend requirements do not mean an adapter exists. | ADR-075/076/084 and ADR-078 in the [decision index](adr/README.md) |
| Demo | No `demo-seed` target exists. Create catalog content through current UI/API and publish it before a purchase demo. | [Demo preparation](runbooks/demo.md) |
| Performance evidence | Historical one-host throughput is not production capacity. The full load target overwrites its tracked evidence path. | [Load proof](verification/on-sale-load/README.md) |
| Checks | Link checks omit external URLs and heading fragments. Claude-specific hooks do not protect every tool or runtime. | [Documentation checks](documentation/maintenance.md), [hook limits](technical-delivery-standards.md#tool-hooks-and-limits) |
| Historical assets | Screenshot-only folders have no adjacent baseline/procedure. Their contents alone do not prove current behavior. | [Record index](records.md) |

Read requirements and implementation together. A gap does not cancel a requirement, and an
accepted future design does not prove delivery. Do not turn this list into completion claims.
