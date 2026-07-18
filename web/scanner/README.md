# Gate scanner

React gate-scanner app: camera/paste QR validation against the Access service
(`POST /api/access/scans`), with a durable offline occurrence queue
(`src/occurrences.ts`, ADR-025 §D3/§D6). Vite + vitest; `pnpm dev`, `pnpm test`.

## Occurrence protocol

Every scan mints a UUIDv4 **occurrence id**, committed to IndexedDB **before**
the request leaves the device. Transport retries reuse the id; a new physical
decision (a new scan) mints a new one. A response — first-time or replay — may
actuate (render the accepted screen) **only** while this device holds a
durably pending, never-actuated record for that occurrence id, and the record
is marked actuated **before** the accepted screen renders (fail-closed). A
`(qr_payload, occurrence_id)` pair copied to another device or browser profile
holds no pending record there and does not actuate. This is a per-profile
store property, not an authenticated-identity guarantee: nothing binds the
queue to a gate identity yet — ADR-025 §D3 defers occurrence↔scanner identity
binding to the hardware-gate work (TKT-19).

Offline scans queue locally and sync through
`POST /api/access/scans/reconciliations` on reconnect (or the manual sync
button). `conflict` results are surfaced to the operator and never act on the
gate — reconciliation is recording, not deciding.

## Named irreducible lost-admission window (ADR-025 §D6)

Browser storage is not a hardware guarantee. If the browser process or device
dies **after** the physical admission decision but **before** the PENDING
IndexedDB transaction commits (sub-millisecond on committed-storage browsers,
unbounded on a device that never recovers), the occurrence is lost: never
sent, never reconciled, invisible to the trace (ADR-025 §Claims: "a gate that
never syncs"). Additionally, IndexedDB evicted under browser storage pressure
before a sync loses QUEUED records. Hardware gates (TKT-16/TKT-19) must
provide write-ahead durable storage to narrow this window; the web scanner
names it rather than pretending it away.

The other half of §D3's irreducible window: a crash between mark-actuated and
the accepted render strands one admission on the "already processed" screen —
an operator resolves it at the gate. Never a double actuation.

## Version skew (ADR-025 §D10)

- **Old scanner → new server**: no `occurrence_id` sent; the server keeps
  today's semantics indefinitely (expand phase — the field is optional).
- **New scanner → old server**: the old contract rejects `occurrence_id`
  (422). The scanner does **not** auto-retry without the id — an auto-retry
  would be a new occurrence-identity decision the operator didn't make, and
  against a server that recorded the first attempt it can double-admit. The
  failure is surfaced; the operator re-scans (which mints a new id).
