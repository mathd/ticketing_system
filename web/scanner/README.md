# Gate scanner

React/Vite browser scanner served at `/scanner/`. Access verifies QR credentials through
`POST /api/access/scans`; queued facts sync through `POST /api/access/scans/reconciliations`.
Run `pnpm dev` or `pnpm test` in this directory. Root [testing](../../docs/testing.md) describes
integration and real-Chrome checks.

## Implementation map

| File | Owns |
|---|---|
| [App.tsx](src/App.tsx) | Device pairing, Entry/Exit selection, scan and sync UI |
| [entry-scan.ts](src/entry-scan.ts) | Scan execution and first-decision lifecycle for both directions |
| [scan-clock.ts](src/scan-clock.ts) | Wall and monotonic clock checks |
| [scan-result.ts](src/scan-result.ts) | Operator result presentation |
| [occurrence-model.ts](src/occurrence-model.ts) | Typed outcome and direction records |
| [occurrence-coordinator.ts](src/occurrence-coordinator.ts) | First outcome, persistence, and repair decisions |
| [occurrence-store.ts](src/occurrence-store.ts) | IndexedDB v3 storage and migration |
| [occurrence-owner.ts](src/occurrence-owner.ts) | Page owner identity |
| [reconcile-batch.ts](src/reconcile-batch.ts) | Sync batches and result counts |

## Decision boundaries

A scan can stop before mint or send. A request that proceeds uses an occurrence ID committed
locally before send; retries reuse it. Rendering an accepted result needs this page's durable
reservation, with the mark committed before render. Copying a QR/occurrence pair to another
profile gives it no local record and no actuation right. This is honest per-profile storage
behavior, not a hardware or hostile-device guarantee.

The scanner checks wall and monotonic clocks. It does not mint or send at or after 3 seconds,
or choose a new admission decision at or after 30 seconds. A first decision that was in time
can need a later persistence repair. Entry and Exit preserve their captured direction.
Use the [operator runbook](../../docs/runbooks/scanner.md) for exact outcomes, stored-record
handling, native network-error limits, and page-loss residuals. A software reservation does not
prove physical passage.

Exactly 502/503/504 and the selected native network-rejection shape can choose an offline
outcome. Other ambiguous responses do not grant admission. Reconciliation records facts and
never opens the gate. The UI reports conflicts and rejections separately.

Browser storage can fail, be evicted, or disappear with the page. A committed local mark can
outlive a missing render; an offline instruction can precede a late queue commit. These windows
need operator handling and are not guarantees of hardware durability. Venue use still requires
the qualification and owner choices recorded under TKT-560.

## Governing decisions

Read [ADR-025](../../docs/adr/ADR-025-admission-events-and-offline-reconciliation.md),
[ADR-066](../../docs/adr/ADR-066-scanner-voided-ticket-feed.md), and
[ADR-085](../../docs/adr/ADR-085-scanner-local-outcome-model.md), including staged-delivery notes.
The old no-ID server compatibility path does not authorize a new scanner to retry without its
occurrence ID. An old server's refusal needs operator action.
